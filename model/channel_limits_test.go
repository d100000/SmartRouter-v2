package model

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

func TestChannelLimitsDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "limits.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
			case "mysql":
				if os.Getenv("TEST_MYSQL_DSN") == "" {
					t.Skip("TEST_MYSQL_DSN not configured")
				}
				driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
			case "postgres":
				if os.Getenv("TEST_POSTGRES_DSN") == "" {
					t.Skip("TEST_POSTGRES_DSN not configured")
				}
				driver = postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "climit_test_"}, Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(4)
			oldDB, oldType := DB, common.MainDatabaseType()
			DB = db
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(&ChannelLimitEvent{}, &ChannelLimitAttempt{}, &ChannelDailyBudget{}, &ChannelLimitConfig{}, &Channel{}))
				DB = oldDB
				common.SetMainDatabaseType(oldType)
				require.NoError(t, sqlDB.Close())
			})
			require.NoError(t, AutoMigrateChannelLimitSchema(db))
			require.NoError(t, AutoMigrateChannelLimitSchema(db))
			require.NoError(t, db.AutoMigrate(&Channel{}))
			query := "SELECT version()"
			if dialect == "sqlite" {
				query = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(query).Scan(&version).Error)
			t.Logf("database version: %s", version)
			now := time.Date(2026, 11, 8, 12, 0, 0, 0, channelBudgetLocation)
			tokens, cost := int64(100), int64(20*ChannelLimitUSDScale)
			for _, channelID := range []int{1, 2, 3, 4, 5, 6, 7} {
				require.NoError(t, EnsureChannelLimitTracking(channelID, true, now))
			}
			// Supplier channels always record their attempts. Channel 8 and
			// the channels from 20 on are platform channels without limits,
			// whose requests skip the ledger until a limit is set.
			for channelID := 1; channelID <= 16; channelID++ {
				owner := "supplier"
				if channelID == 8 {
					owner = ""
				}
				require.NoError(t, db.Create(&Channel{Id: channelID, Name: fmt.Sprintf("limit-%d", channelID), OwnerSupplierID: owner}).Error)
			}

			// Disabled limits do not require unknown output or cost estimates.
			_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: "unlimited", ChannelID: 1, TPMApplicable: true}, now)
			require.NoError(t, err)
			require.NoError(t, MarkChannelAttemptSent(1, "unlimited", now))
			_, err = UpdateChannelLimits(1, 1, ChannelLimitSettings{TPM: 1000, ReconcileSeconds: 300}, false, now)
			assert.ErrorIs(t, err, ErrChannelLimitAccounting)
			config, err := UpdateChannelLimits(1, 1, ChannelLimitSettings{RPM: 2, ReconcileSeconds: 300}, false, now)
			require.NoError(t, err, "unknown cost cannot prevent RPM-only limits")
			assert.Equal(t, int64(0), config.TPM)
			_, err = UpdateChannelLimits(1, 1, config.ChannelLimitSettings, false, now)
			assert.ErrorIs(t, err, ErrChannelLimitRevision)

			// Exactly one process gets the last RPM permit; failures still count.
			_, err = UpdateChannelLimits(2, 1, ChannelLimitSettings{RPM: 1, ReconcileSeconds: 300}, false, now)
			require.NoError(t, err)
			results := make(chan error, 2)
			start := make(chan struct{})
			var workers sync.WaitGroup
			for _, id := range []string{"concurrent-a", "concurrent-b"} {
				workers.Go(func() {
					<-start
					_, err := ReserveChannelAttempt(ChannelAttemptReservation{ID: id, ChannelID: 2}, now)
					results <- err
				})
			}
			close(start)
			workers.Wait()
			close(results)
			successes := 0
			for err := range results {
				if err == nil {
					successes++
				} else {
					var limited *ChannelLimitError
					require.ErrorAs(t, err, &limited)
					assert.Equal(t, "rpm_limited", limited.Reason)
				}
			}
			assert.Equal(t, 1, successes)
			require.NoError(t, ReconcileChannelAttempts(now.Add(time.Minute)))
			state, err := GetChannelLimitState(2, now.Add(time.Minute))
			require.NoError(t, err)
			assert.Zero(t, state.RPMReserved)

			// Lowering a live cap keeps both sent attempts and outstanding
			// reservations. Enough slots must expire to admit one more.
			require.NoError(t, EnsureChannelLimitTracking(12, true, now))
			for i, id := range []string{"sent-first", "reserved-second", "sent-last"} {
				at := now.Add(time.Duration(i) * 10 * time.Second)
				_, err := ReserveChannelAttempt(ChannelAttemptReservation{ID: id, ChannelID: 12}, at)
				require.NoError(t, err)
				if id != "reserved-second" {
					require.NoError(t, MarkChannelAttemptSent(12, id, at.Add(time.Second)))
				}
			}
			current := now.Add(25 * time.Second)
			config, err = UpdateChannelLimits(12, 1, ChannelLimitSettings{RPM: 2, ReconcileSeconds: 300}, false, current)
			require.NoError(t, err)
			state, err = GetChannelLimitState(12, current)
			require.NoError(t, err)
			assert.EqualValues(t, 2, state.RPMUsed)
			assert.EqualValues(t, 1, state.RPMReserved)
			assert.Equal(t, now.Add(70*time.Second).UnixMilli(), state.RPMRetryAt)
			// The routing node's wall clock is ahead of the database. Its
			// relative Retry-After still lasts exactly the remaining 45 seconds.
			config.sampledAt, config.databaseClockMS = current.Add(8*time.Hour), current.UnixMilli()
			var refused *ChannelLimitError
			require.ErrorAs(t, CheckChannelRPM(config, current), &refused)
			assert.Equal(t, 45*time.Second, refused.RetryDeadline().Sub(config.sampledAt))
			_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: "still-full", ChannelID: 12}, current)
			require.ErrorAs(t, err, &refused)
			assert.Equal(t, state.RPMRetryAt, refused.RetryAt)
			require.NoError(t, ReleaseChannelAttempt(12, "reserved-second", current))
			state, err = GetChannelLimitState(12, current)
			require.NoError(t, err)
			assert.Equal(t, now.Add(61*time.Second).UnixMilli(), state.RPMRetryAt)
			_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: "at-boundary", ChannelID: 12}, now.Add(61*time.Second))
			require.NoError(t, err)

			t.Run("Redis routing across mixed nodes", func(t *testing.T) {
				server := useUserCacheMiniRedis(t)
				const id = 14
				require.NoError(t, EnsureChannelLimitTracking(id, true, now))
				settings := ChannelLimitSettings{RPM: 1, ReconcileSeconds: 300}
				config, err := UpdateChannelLimits(id, 1, settings, false, now)
				require.NoError(t, err)
				require.NoError(t, CheckChannelRPM(config, now))
				// A node which does not use the Redis hint takes the last slot.
				_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: "external-hold", ChannelID: id}, now)
				require.NoError(t, err)
				check := func(at time.Time) error {
					t.Helper()
					current, err := GetChannelLimitConfig(id)
					require.NoError(t, err)
					return CheckChannelRPM(current, at)
				}
				require.ErrorAs(t, check(now), &refused)
				require.NoError(t, ReleaseChannelAttempt(id, "external-hold", now))
				require.NoError(t, check(now), "releasing a reservation must restore availability immediately")
				_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: "external-send", ChannelID: id}, now)
				require.NoError(t, err)
				require.NoError(t, MarkChannelAttemptSent(id, "external-send", now))
				require.ErrorAs(t, check(now), &refused)
				assert.Equal(t, now.Add(time.Minute).UnixMilli(), refused.RetryAt)
				require.ErrorAs(t, check(now.Add(time.Second)), &refused)
				server.FlushAll()
				require.ErrorAs(t, check(now.Add(time.Second)), &refused, "an empty Redis cannot grant a slot")
				settings.RPM = 2
				config, err = UpdateChannelLimits(id, config.Revision, settings, false, now.Add(2*time.Second))
				require.NoError(t, err)
				require.NoError(t, check(now.Add(2*time.Second)), "raising the cap supersedes a full cached window")
				settings.RPM = 1
				_, err = UpdateChannelLimits(id, config.Revision, settings, false, now.Add(3*time.Second))
				require.NoError(t, err)
				require.ErrorAs(t, check(now.Add(3*time.Second)), &refused)
				require.NoError(t, check(now.Add(time.Minute)), "the sent slot expires at the exact boundary")
			})

			t.Run("dispatch proof and external writers", func(t *testing.T) {
				const id = 15
				at := time.Now()
				require.NoError(t, EnsureChannelLimitTracking(id, true, at))
				_, err := UpdateChannelLimits(id, 1, ChannelLimitSettings{RPM: 4, ReconcileSeconds: 300}, false, at)
				require.NoError(t, err)
				before, err := ChannelLimitDatabaseTime(db)
				require.NoError(t, err)
				require.NoError(t, DispatchChannelAttempt(ChannelAttemptReservation{ID: "local-dispatch", ChannelID: id}))
				after, err := ChannelLimitDatabaseTime(db)
				require.NoError(t, err)
				var dispatched ChannelLimitAttempt
				require.NoError(t, db.First(&dispatched, "id = ?", "local-dispatch").Error)
				assert.GreaterOrEqual(t, dispatched.SentAtMS, before.UnixMilli())
				assert.LessOrEqual(t, dispatched.SentAtMS, after.UnixMilli())
				_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: "older-node", ChannelID: id}, at)
				require.NoError(t, err)
				require.NoError(t, MarkChannelAttemptSent(id, "older-node", at))
				var workers sync.WaitGroup
				results := make(chan error, 4)
				for i := range cap(results) {
					workers.Go(func() {
						results <- DispatchChannelAttempt(ChannelAttemptReservation{ID: fmt.Sprintf("concurrent-send-%d", i), ChannelID: id})
					})
				}
				workers.Wait()
				close(results)
				allowed := 0
				for err := range results {
					if err == nil {
						allowed++
					} else {
						require.ErrorAs(t, err, &refused)
						assert.Equal(t, "rpm_limited", refused.Reason)
					}
				}
				assert.Equal(t, 2, allowed)
			})

			t.Run("restored ledger discards a proof", func(t *testing.T) {
				const id = 16
				at := time.Now()
				require.NoError(t, EnsureChannelLimitTracking(id, true, at))
				_, err := UpdateChannelLimits(id, 1, ChannelLimitSettings{RPM: 2, ReconcileSeconds: 300}, false, at)
				require.NoError(t, err)
				require.NoError(t, DispatchChannelAttempt(ChannelAttemptReservation{ID: "before-restore", ChannelID: id}))
				// Replace only this test channel's attempt history while preserving
				// channel configuration and lock_version, as on a restored primary.
				require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
					if err := tx.Where("channel_id = ?", id).Delete(&ChannelLimitAttempt{}).Error; err != nil {
						return err
					}
					return tx.Create(&[]ChannelLimitAttempt{
						{ID: "restored-first", ChannelID: id, SentAtMS: at.UnixMilli()},
						{ID: "restored-second", ChannelID: id, SentAtMS: at.UnixMilli()},
					}).Error
				}))
				err = DispatchChannelAttempt(ChannelAttemptReservation{ID: "after-restore", ChannelID: id})
				require.ErrorAs(t, err, &refused)
				assert.Equal(t, "rpm_limited", refused.Reason)
			})

			// The rejected first edit of a legacy channel starts its window.
			_, err = UpdateChannelLimits(13, 1, config.ChannelLimitSettings, false, now)
			require.ErrorAs(t, err, &refused)
			assert.ErrorIs(t, err, ErrChannelLimitAccounting)
			assert.Equal(t, "minute_window_warming", refused.Reason)
			assert.Equal(t, now.Add(time.Minute).UnixMilli(), refused.RetryAt)
			_, err = UpdateChannelLimits(13, 1, config.ChannelLimitSettings, false, now.Add(time.Minute))
			require.NoError(t, err)

			// Unknown termination closes only TPM, never the business task/cost.
			_, err = UpdateChannelLimits(3, 1, ChannelLimitSettings{TPM: 100, DailyUSDNano: 100 * ChannelLimitUSDScale, ReconcileSeconds: 300}, true, now)
			require.NoError(t, err)
			_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: "lost", ChannelID: 3, TPMApplicable: true, TokenBudget: &tokens, CostBudgetNano: &cost}, now)
			require.NoError(t, err)
			require.NoError(t, MarkChannelAttemptSent(3, "lost", now))
			require.NoError(t, MarkChannelAttemptUnknown(3, "lost", 110, now.Add(time.Second)))
			require.NoError(t, MarkChannelAttemptUnknown(3, "lost", 100, now.Add(2*time.Minute)))
			require.NoError(t, ReconcileChannelAttempts(now.Add(301*time.Second)))
			require.NoError(t, ReconcileChannelAttempts(now.Add(320*time.Second)))
			state, err = GetChannelLimitState(3, now.Add(350*time.Second))
			require.NoError(t, err)
			assert.Equal(t, int64(110), state.TPMUsed)
			assert.Zero(t, state.TPMReserved)
			assert.Equal(t, cost, state.DailyHeldNano)
			var lost ChannelLimitAttempt
			require.NoError(t, db.First(&lost, "id = ?", "lost").Error)
			assert.False(t, lost.Completed)
			assert.Zero(t, lost.ExecutionLeaseMS)
			assert.Equal(t, now.Add(301*time.Second).UnixMilli(), lost.TPMClosedAtMS)
			state, err = GetChannelLimitState(3, now.Add(361*time.Second))
			require.NoError(t, err)
			assert.Zero(t, state.TPMUsed)
			actualTokens := int64(130)
			settlement := ChannelAttemptSettlement{ID: "lost", ChannelID: 3, Version: 1, Tokens: &actualTokens, CostUSDNano: &cost}
			require.NoError(t, SettleChannelAttempt(settlement, now.Add(362*time.Second)))
			require.NoError(t, SettleChannelAttempt(settlement, now.Add(380*time.Second)))
			state, err = GetChannelLimitState(3, now.Add(381*time.Second))
			require.NoError(t, err)
			assert.Equal(t, int64(20), state.TPMUsed, "late result charges only the unaccounted positive delta")
			assert.Equal(t, cost, state.DailyUsedNano)
			assert.Zero(t, state.DailyHeldNano)
			lowerTokens := int64(50)
			settlement.Version, settlement.Tokens = 2, &lowerTokens
			require.NoError(t, SettleChannelAttempt(settlement, now.Add(400*time.Second)))
			state, err = GetChannelLimitState(3, now.Add(401*time.Second))
			require.NoError(t, err)
			assert.Zero(t, state.TPMUsed, "no credit against another request or an expired window")
			settlement.Version, settlement.Tokens = 1, &actualTokens
			require.NoError(t, SettleChannelAttempt(settlement, now.Add(402*time.Second)))
			state, err = GetChannelLimitState(3, now.Add(403*time.Second))
			require.NoError(t, err)
			assert.Zero(t, state.TPMUsed)

			// A supplier budget is procurement cost, sticky until the next Beijing
			// date or an explicit authorized increase. Token windows do not reset.
			midnight := time.Date(2026, 11, 9, 0, 0, 0, 0, channelBudgetLocation)
			_, err = UpdateChannelLimits(4, 1, ChannelLimitSettings{RPM: 1, DailyUSDNano: cost, ReconcileSeconds: 300}, true, now)
			require.NoError(t, err)
			_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: "old-day", ChannelID: 4, CostBudgetNano: &cost}, midnight.Add(-time.Second))
			require.NoError(t, err)
			require.NoError(t, MarkChannelAttemptSent(4, "old-day", midnight.Add(-time.Second)))
			require.NoError(t, SettleChannelAttempt(ChannelAttemptSettlement{ID: "old-day", ChannelID: 4, Version: 1, CostUSDNano: &cost}, midnight.Add(time.Second)))
			state, err = GetChannelLimitState(4, midnight.Add(time.Second))
			require.NoError(t, err)
			assert.Equal(t, "2026-11-09", state.BudgetDay)
			assert.Zero(t, state.DailyUsedNano)
			assert.Equal(t, int64(1), state.RPMUsed)
			var previous ChannelDailyBudget
			require.NoError(t, db.First(&previous, "id = ?", "4:2026-11-08").Error)
			assert.Equal(t, cost, previous.UsedUSDNano)
			assert.True(t, previous.Exhausted)

			_, err = UpdateChannelLimits(5, 1, ChannelLimitSettings{DailyUSDNano: cost, ReconcileSeconds: 300}, true, now)
			require.NoError(t, err)
			_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: "day-change", ChannelID: 5, CostBudgetNano: &cost}, midnight.Add(-time.Second))
			require.NoError(t, err)
			require.NoError(t, MarkChannelAttemptSent(5, "day-change", midnight.Add(time.Second)))
			previous = ChannelDailyBudget{}
			require.NoError(t, db.First(&previous, "id = ?", "5:2026-11-08").Error)
			assert.Zero(t, previous.HeldUSDNano)
			state, err = GetChannelLimitState(5, midnight.Add(time.Second))
			require.NoError(t, err)
			assert.Equal(t, cost, state.DailyHeldNano)
			require.NoError(t, SettleChannelAttempt(ChannelAttemptSettlement{ID: "day-change", ChannelID: 5, Version: 1, CostUSDNano: &cost}, midnight.Add(2*time.Second)))
			zero := int64(0)
			require.NoError(t, SettleChannelAttempt(ChannelAttemptSettlement{ID: "day-change", ChannelID: 5, Version: 2, CostUSDNano: &zero}, midnight.Add(3*time.Second)))
			state, err = GetChannelLimitState(5, midnight.Add(3*time.Second))
			require.NoError(t, err)
			assert.Zero(t, state.DailyUsedNano)
			assert.Equal(t, "daily_exhausted", state.Reason, "procurement correction does not reopen an exhausted day")
			_, err = UpdateChannelLimits(5, 2, ChannelLimitSettings{DailyUSDNano: cost * 2, ReconcileSeconds: 300}, true, midnight.Add(4*time.Second))
			require.NoError(t, err)
			state, err = GetChannelLimitState(5, midnight.Add(4*time.Second))
			require.NoError(t, err)
			assert.Empty(t, state.Reason)

			// Cost rule preparation is independent from RPM/TPM and can be saved
			// as pending, but an enabled daily cap never silently becomes unlimited.
			_, err = UpdateChannelLimits(6, 1, ChannelLimitSettings{DailyUSDNano: cost, ReconcileSeconds: 300}, false, now)
			require.NoError(t, err)
			_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: "pending-cost", ChannelID: 6, CostBudgetNano: &cost}, now)
			var limitErr *ChannelLimitError
			require.ErrorAs(t, err, &limitErr)
			assert.Equal(t, "cost_config_missing", limitErr.Reason)
			_, err = UpdateChannelLimits(7, 1, ChannelLimitSettings{TPM: 200, ReconcileSeconds: 300}, false, now)
			require.NoError(t, err)
			_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: "no-output-bound", ChannelID: 7, TPMApplicable: true}, now)
			require.ErrorAs(t, err, &limitErr)
			assert.Equal(t, "token_budget_unknown", limitErr.Reason)
			_, err = UpdateChannelLimits(7, 2, ChannelLimitSettings{ReconcileSeconds: 300}, false, now)
			require.NoError(t, err)
			_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: "off-again", ChannelID: 7, TPMApplicable: true}, now)
			require.NoError(t, err)
			require.NoError(t, ReleaseChannelAttempt(7, "off-again", now))
			require.NoError(t, ReleaseChannelAttempt(7, "off-again", now))

			// A deployment starting mid-day cannot pretend historical usage is
			// zero: the budget is accepted and applies from the next Beijing day.
			require.NoError(t, EnsureChannelLimitTracking(8, false, now))
			_, err = UpdateChannelLimits(8, 1, ChannelLimitSettings{DailyUSDNano: cost, ReconcileSeconds: 300}, true, now)
			require.NoError(t, err)
			state, err = GetChannelLimitState(8, now)
			require.NoError(t, err)
			assert.Equal(t, "daily_pending", state.Reason)
			assert.Equal(t, midnight.Unix(), state.DailyActiveAt)
			state, err = GetChannelLimitState(8, midnight)
			require.NoError(t, err)
			assert.Empty(t, state.Reason)
			assert.Zero(t, state.DailyActiveAt)
			require.NoError(t, AutoMigrateChannelLimitSchema(db), "restart preserves ledger and unique attempts")
			_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: "lost", ChannelID: 3}, midnight)
			assert.True(t, errors.Is(err, ErrChannelAttemptDuplicate))

			// Process loss starts reconciliation after its lease expires. A live
			// stream that renews its lease remains reserved beyond five minutes.
			require.NoError(t, EnsureChannelLimitTracking(9, true, now))
			_, err = UpdateChannelLimits(9, 1, ChannelLimitSettings{TPM: 200, ReconcileSeconds: 300}, false, now)
			require.NoError(t, err)
			for _, id := range []string{"crashed-process", "live-process"} {
				_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: id, ChannelID: 9, TPMApplicable: true, TokenBudget: &tokens}, now)
				require.NoError(t, err)
				require.NoError(t, MarkChannelAttemptSent(9, id, now))
			}
			for _, id := range []string{"second-live", "already-settled"} {
				_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: id, ChannelID: 9}, now)
				require.NoError(t, err)
				require.NoError(t, MarkChannelAttemptSent(9, id, now))
			}
			require.NoError(t, SettleChannelAttempt(ChannelAttemptSettlement{ID: "already-settled", ChannelID: 9, Version: 1}, now))
			// A process can finish between the worker snapshot and its batch.
			// The same renewal must extend both live sends and leave it settled.
			require.NoError(t, RenewChannelAttemptLeases(9, []string{"live-process", "second-live", "already-settled"}, now.Add(350*time.Second)))
			var renewed int64
			require.NoError(t, db.Model(&ChannelLimitAttempt{}).Where("id IN ? AND execution_lease_ms = ?", []string{"live-process", "second-live"}, now.Add(410*time.Second).UnixMilli()).Count(&renewed).Error)
			assert.EqualValues(t, 2, renewed)
			var settled ChannelLimitAttempt
			require.NoError(t, db.First(&settled, "id = ?", "already-settled").Error)
			assert.Zero(t, settled.ExecutionLeaseMS)
			require.NoError(t, ReconcileChannelAttempts(now.Add(360*time.Second)))
			state, err = GetChannelLimitState(9, now.Add(360*time.Second))
			require.NoError(t, err)
			assert.Equal(t, tokens, state.TPMReserved)
			assert.Equal(t, tokens, state.TPMUsed)
			state, err = GetChannelLimitState(9, now.Add(421*time.Second))
			require.NoError(t, err)
			assert.Equal(t, tokens, state.TPMReserved)
			assert.Zero(t, state.TPMUsed)
			for _, event := range []struct {
				kind, attempt string
				version       int64
			}{{"tpm_estimated", "lost", 0}, {"usage_corrected", "lost", 1}, {"cost_corrected", "lost", 1}} {
				var count int64
				require.NoError(t, db.Model(&ChannelLimitEvent{}).Where("kind = ? AND attempt_id = ? AND result_version = ?", event.kind, event.attempt, event.version).Count(&count).Error)
				assert.Equal(t, int64(1), count, "repeated jobs and settlement deliveries cannot duplicate the audit event")
			}
			var resetCount int64
			require.NoError(t, db.Model(&ChannelLimitEvent{}).Where("kind = ? AND channel_id = ? AND budget_day = ?", "daily_reset", 4, "2026-11-09").Count(&resetCount).Error)
			assert.Equal(t, int64(1), resetCount)
			require.NoError(t, EnsureChannelLimitTracking(10, true, now))
			auditFailure := errors.New("audit persistence unavailable")
			require.NoError(t, db.Callback().Create().Before("gorm:create").Register("limit:audit-failure", func(tx *gorm.DB) {
				if event, ok := tx.Statement.Dest.(*ChannelLimitEvent); ok && event.ChannelID == 10 {
					tx.AddError(auditFailure)
				}
			}))
			_, err = UpdateChannelLimits(10, 1, ChannelLimitSettings{RPM: 5, ReconcileSeconds: 300}, false, now)
			assert.ErrorIs(t, err, auditFailure)
			require.NoError(t, db.Callback().Create().Remove("limit:audit-failure"))
			state, err = GetChannelLimitState(10, now)
			require.NoError(t, err)
			assert.Equal(t, int64(1), state.Config.Revision)
			assert.Zero(t, state.Config.RPM, "a limit change cannot commit when its durable audit fails")

			// A disconnected stream's trusted cost is a lower bound, even when
			// it arrives after automatic TPM reconciliation. It is not a refund
			// or a terminal business result.
			require.NoError(t, EnsureChannelLimitTracking(11, true, now))
			_, err = UpdateChannelLimits(11, 1, ChannelLimitSettings{TPM: 1000, DailyUSDNano: 4 * cost, ReconcileSeconds: 300}, true, now)
			require.NoError(t, err)
			_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: "partial-cost", ChannelID: 11, TPMApplicable: true, TokenBudget: &tokens, CostBudgetNano: &cost}, now)
			require.NoError(t, err)
			require.NoError(t, MarkChannelAttemptSent(11, "partial-cost", now))
			require.NoError(t, MarkChannelAttemptUnknown(11, "partial-cost", 120, now.Add(time.Second), 3*cost))
			require.NoError(t, MarkChannelAttemptUnknown(11, "partial-cost", 100, now.Add(2*time.Second), 2*cost))
			state, err = GetChannelLimitState(11, now.Add(2*time.Second))
			require.NoError(t, err)
			assert.Equal(t, 3*cost, state.DailyHeldNano)
			assert.Equal(t, int64(120), state.TPMReserved)
			twoCosts := 2 * cost
			_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: "partial-cost-denied", ChannelID: 11, TPMApplicable: true, TokenBudget: &tokens, CostBudgetNano: &twoCosts}, now.Add(2*time.Second))
			require.Error(t, err, "new requests cannot spend a known cost overrun")
			require.NoError(t, ReconcileChannelAttempts(now.Add(301*time.Second)))
			require.NoError(t, MarkChannelAttemptUnknown(11, "partial-cost", 140, now.Add(362*time.Second), 4*cost))
			state, err = GetChannelLimitState(11, now.Add(362*time.Second))
			require.NoError(t, err)
			assert.Equal(t, 4*cost, state.DailyHeldNano)
			assert.Equal(t, int64(20), state.TPMUsed)
			var partial ChannelLimitAttempt
			require.NoError(t, db.First(&partial, "id = ?", "partial-cost").Error)
			assert.False(t, partial.Completed)
			assert.False(t, partial.CostActualKnown)
			require.NoError(t, SettleChannelAttempt(ChannelAttemptSettlement{ID: partial.ID, ChannelID: 11, Version: 1, Tokens: &tokens, CostUSDNano: &cost}, now.Add(363*time.Second)))
			state, err = GetChannelLimitState(11, now.Add(363*time.Second))
			require.NoError(t, err)
			assert.Zero(t, state.DailyHeldNano)
			assert.Equal(t, cost, state.DailyUsedNano)

			t.Run("platform channel without limits records cost and keeps history on activation", func(t *testing.T) {
				const id = 20
				require.NoError(t, db.Create(&Channel{Id: id, Name: "platform"}).Error)
				require.NoError(t, EnsureChannelLimitTracking(id, true, now.Add(-48*time.Hour)))
				// Without any limit the request is still recorded with its cost.
				ratio := 0.5
				_, err := ReserveChannelAttempt(ChannelAttemptReservation{ID: "platform-unlimited", ChannelID: id, CostBudgetNano: &cost, CostRatio: &ratio, QuotaPerUnit: 1}, now)
				require.NoError(t, err)
				require.NoError(t, MarkChannelAttemptSent(id, "platform-unlimited", now))
				require.NoError(t, SettleChannelAttempt(ChannelAttemptSettlement{ID: "platform-unlimited", ChannelID: id, Version: 1, CostUSDNano: &cost}, now.Add(time.Second)))
				state, err := GetChannelLimitState(id, now.Add(time.Second))
				require.NoError(t, err)
				assert.Equal(t, cost, state.DailyUsedNano, "an unlimited channel still records its procurement cost")
				assert.EqualValues(t, 1, state.RPMUsed)
				// Activation keeps the recorded history: no reset, no warming, and the
				// request already sent counts toward both the minute and the day.
				config, err := UpdateChannelLimits(id, 1, ChannelLimitSettings{RPM: 1, DailyUSDNano: 2 * cost, ReconcileSeconds: 300}, true, now.Add(2*time.Second))
				require.NoError(t, err)
				assert.True(t, config.HistoryComplete)
				state, err = GetChannelLimitState(id, now.Add(2*time.Second))
				require.NoError(t, err)
				assert.Zero(t, state.DailyActiveAt, "a fully observed day applies the budget at once")
				assert.Equal(t, cost, state.DailyRemaining)
				var limited *ChannelLimitError
				_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: "platform-second", ChannelID: id, CostBudgetNano: &cost, CostRatio: &ratio, QuotaPerUnit: 1}, now.Add(3*time.Second))
				require.ErrorAs(t, err, &limited)
				assert.Equal(t, "rpm_limited", limited.Reason)
			})

			t.Run("reconciliation skips a failing record and backs it off", func(t *testing.T) {
				const poisonChannel, healthyChannel = 22, 23
				for _, id := range []int{poisonChannel, healthyChannel} {
					require.NoError(t, EnsureChannelLimitTracking(id, true, now))
				}
				stale := now.Add(-2 * time.Minute)
				_, err := ReserveChannelAttempt(ChannelAttemptReservation{ID: "poison-reservation", ChannelID: poisonChannel}, stale)
				require.NoError(t, err)
				_, err = ReserveChannelAttempt(ChannelAttemptReservation{ID: "healthy-reservation", ChannelID: healthyChannel}, stale.Add(time.Second))
				require.NoError(t, err)
				var poisonWrites atomic.Int64
				broken := errors.New("broken attempt record")
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("limit:poison", func(tx *gorm.DB) {
					if attempt, ok := tx.Statement.Dest.(*ChannelLimitAttempt); ok && attempt.ChannelID == poisonChannel {
						poisonWrites.Add(1)
						tx.AddError(broken)
					}
				}))
				t.Cleanup(func() {
					_ = db.Callback().Update().Remove("limit:poison")
					channelReconcileBackoff.succeed([]string{"poison-reservation"})
				})
				require.NoError(t, ReconcileChannelAttempts(now))
				var healthy, poison ChannelLimitAttempt
				require.NoError(t, db.First(&healthy, "id = ?", "healthy-reservation").Error)
				assert.True(t, healthy.Released, "another channel's failure does not stop reconciliation")
				require.NoError(t, db.First(&poison, "id = ?", "poison-reservation").Error)
				assert.False(t, poison.Released)
				assert.Contains(t, channelReconcileBackoff.excluded(), "poison-reservation")
				writes := poisonWrites.Load()
				require.NoError(t, ReconcileChannelAttempts(now))
				assert.Equal(t, writes, poisonWrites.Load(), "a backed-off record is excluded from the next pass")
				require.NoError(t, db.Callback().Update().Remove("limit:poison"))
				channelReconcileBackoff.succeed([]string{"poison-reservation"})
				require.NoError(t, ReconcileChannelAttempts(now))
				require.NoError(t, db.First(&poison, "id = ?", "poison-reservation").Error)
				assert.True(t, poison.Released)
			})

			t.Run("cleanup deletes only rows that affect no limit", func(t *testing.T) {
				const id = 24
				old := now.AddDate(0, 0, -10)
				oldDay, _, _ := channelBudgetDay(old)
				recentDay, _, _ := channelBudgetDay(now)
				ratio := 0.5
				rows := []ChannelLimitAttempt{
					{ID: "gc-released-old", ChannelID: id, BudgetDay: oldDay, CreatedAtMS: old.UnixMilli(), Released: true},
					{ID: "gc-released-recent", ChannelID: id, BudgetDay: recentDay, CreatedAtMS: now.UnixMilli(), Released: true},
					{ID: "gc-settled-old", ChannelID: id, BudgetDay: oldDay, CreatedAtMS: old.UnixMilli(), SentAtMS: old.UnixMilli(), Completed: true, TPMExpiresAtMS: old.Add(time.Minute).UnixMilli(), CostRatio: &ratio, CostActualKnown: true},
					{ID: "gc-no-cost-rule-old", ChannelID: id, BudgetDay: oldDay, CreatedAtMS: old.UnixMilli(), SentAtMS: old.UnixMilli(), Completed: true, TPMExpiresAtMS: old.Add(time.Minute).UnixMilli()},
					{ID: "gc-cost-unknown-old", ChannelID: id, BudgetDay: oldDay, CreatedAtMS: old.UnixMilli(), SentAtMS: old.UnixMilli(), Completed: true, TPMExpiresAtMS: old.Add(time.Minute).UnixMilli(), CostRatio: &ratio},
					{ID: "gc-running-old", ChannelID: id, BudgetDay: oldDay, CreatedAtMS: old.UnixMilli(), SentAtMS: old.UnixMilli(), TPMClosedAtMS: old.UnixMilli(), TPMExpiresAtMS: old.Add(time.Minute).UnixMilli()},
					{ID: "gc-settled-recent", ChannelID: id, BudgetDay: recentDay, CreatedAtMS: now.UnixMilli(), SentAtMS: now.UnixMilli(), Completed: true, TPMExpiresAtMS: now.Add(time.Minute).UnixMilli(), CostActualKnown: true},
				}
				require.NoError(t, db.Create(&rows).Error)
				remaining := func() []string {
					var ids []string
					require.NoError(t, db.Model(&ChannelLimitAttempt{}).Where("channel_id = ?", id).Order("id").Pluck("id", &ids).Error)
					return ids
				}
				// By default every sent request keeps its cost record; only an
				// unsent, released reservation (no cost) is removed.
				removed, err := CleanupChannelLimitAttempts(now, 0)
				require.NoError(t, err)
				assert.Equal(t, int64(1), removed)
				assert.Equal(t, []string{"gc-cost-unknown-old", "gc-no-cost-rule-old", "gc-released-recent", "gc-running-old", "gc-settled-old", "gc-settled-recent"}, remaining())
				removed, err = CleanupChannelLimitAttempts(now, 7)
				require.NoError(t, err)
				assert.Equal(t, int64(2), removed, "an explicit retention removes settled records past it")
				assert.Equal(t, []string{"gc-cost-unknown-old", "gc-released-recent", "gc-running-old", "gc-settled-recent"}, remaining())
				removed, err = CleanupChannelLimitAttempts(now, 7)
				require.NoError(t, err)
				assert.Zero(t, removed)
			})

			t.Run("every sent request without a known cost counts as unknown", func(t *testing.T) {
				const id = 25
				require.NoError(t, EnsureChannelLimitTracking(id, true, now))
				ratio := 0.5
				held := int64(3 * ChannelLimitUSDScale)
				dispatch := func(attemptID string, at time.Time) {
					_, err := ReserveChannelAttempt(ChannelAttemptReservation{ID: attemptID, ChannelID: id, CostBudgetNano: &held, CostRatio: &ratio, QuotaPerUnit: 1}, at)
					require.NoError(t, err)
					require.NoError(t, MarkChannelAttemptSent(id, attemptID, at))
				}
				unknownCosts := func(at time.Time) int64 {
					state, err := GetChannelLimitState(id, at)
					require.NoError(t, err)
					return state.UnknownCosts
				}
				// A known dispatch estimate alone keeps the day complete.
				dispatch("unknown-marked", now)
				assert.Zero(t, unknownCosts(now))
				// Losing the outcome counts the row although its estimate was known.
				require.NoError(t, MarkChannelAttemptUnknown(id, "unknown-marked", 0, now.Add(time.Second)))
				require.NoError(t, MarkChannelAttemptUnknown(id, "unknown-marked", 0, now.Add(2*time.Second)))
				assert.EqualValues(t, 1, unknownCosts(now.Add(2*time.Second)), "repeated marks count once")
				// Completion without a computable cost is not a complete day either.
				dispatch("settled-without-cost", now.Add(3*time.Second))
				require.NoError(t, SettleChannelAttempt(ChannelAttemptSettlement{ID: "settled-without-cost", ChannelID: id, Version: 1}, now.Add(4*time.Second)))
				assert.EqualValues(t, 2, unknownCosts(now.Add(4*time.Second)))
				// A crashed executor stops renewing its lease; reconciliation counts it.
				dispatch("lease-lapsed", now.Add(5*time.Second))
				require.NoError(t, ReconcileChannelAttempts(now.Add(5*time.Second+2*time.Minute)))
				assert.EqualValues(t, 3, unknownCosts(now.Add(5*time.Second+2*time.Minute)))
				// A later settlement with the actual cost removes each row exactly once.
				actual := int64(2 * ChannelLimitUSDScale)
				later := now.Add(10 * time.Minute)
				for _, attemptID := range []string{"unknown-marked", "settled-without-cost", "lease-lapsed"} {
					require.NoError(t, SettleChannelAttempt(ChannelAttemptSettlement{ID: attemptID, ChannelID: id, Version: 2, CostUSDNano: &actual}, later))
				}
				state, err := GetChannelLimitState(id, later)
				require.NoError(t, err)
				assert.Zero(t, state.UnknownCosts)
				assert.Zero(t, state.DailyHeldNano, "every hold is released")
				assert.Equal(t, 3*actual, state.DailyUsedNano)
			})

			t.Run("follow-up of a sent request is recorded but never refused for capacity", func(t *testing.T) {
				const id = 26
				require.NoError(t, EnsureChannelLimitTracking(id, true, now))
				ratio := 0.5
				budget := int64(4 * ChannelLimitUSDScale)
				_, err := UpdateChannelLimits(id, 1, ChannelLimitSettings{RPM: 1, DailyUSDNano: budget, ReconcileSeconds: 300}, true, now)
				require.NoError(t, err)
				at := now.Add(time.Second)
				reservation := func(attemptID string, followUp bool) ChannelAttemptReservation {
					return ChannelAttemptReservation{ID: attemptID, ChannelID: id, CostBudgetNano: &budget, CostRatio: &ratio, QuotaPerUnit: 1, FollowUp: followUp}
				}
				_, err = recordChannelAttempt(reservation("chain-first", false), at, true)
				require.NoError(t, err)
				var limited *ChannelLimitError
				_, err = recordChannelAttempt(reservation("unrelated", false), at, true)
				require.ErrorAs(t, err, &limited, "an unrelated request is still limited")
				_, err = recordChannelAttempt(reservation("chain-second", true), at, true)
				require.NoError(t, err)
				state, err := GetChannelLimitState(id, at)
				require.NoError(t, err)
				assert.EqualValues(t, 2, state.RPMUsed, "the follow-up still counts")
				assert.Equal(t, 2*budget, state.DailyHeldNano)
			})
		})
	}
}

func TestChannelLimitMoneyConversion(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  int64
		valid bool
	}{{"0", 0, true}, {"0.000000001", 1, true}, {"100.25", 100250000000, true}, {"-1", 0, false}, {"0.0000000001", 0, false}, {"NaN", 0, false}, {"9007199.254740992", 0, false}} {
		t.Run(tc.input, func(t *testing.T) {
			value, err := ParseChannelLimitUSD(tc.input)
			if !tc.valid {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, value)
		})
	}
	cost, err := ProcurementCostUSDNano(500000, 0.25, 500000)
	require.NoError(t, err)
	assert.Equal(t, int64(250000000), cost)
	assert.Equal(t, "0.25", FormatChannelLimitUSD(cost))
}

// Measure the complete routing precheck (fresh config + RPM window) for a
// stable hot channel. Each run uses its own tables and Redis key generations.
func BenchmarkChannelRPMPreflight(b *testing.B) {
	dsn, redisURL := os.Getenv("TEST_RPM_BENCH_MYSQL_DSN"), os.Getenv("TEST_RPM_REDIS_URL")
	if dsn == "" || redisURL == "" {
		b.Skip("requires TEST_RPM_BENCH_MYSQL_DSN and TEST_RPM_REDIS_URL")
	}
	redisOptions, err := redis.ParseURL(redisURL)
	require.NoError(b, err)
	client := redis.NewClient(redisOptions)
	require.NoError(b, client.Ping(context.Background()).Err())
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{NamingStrategy: schema.NamingStrategy{
		TablePrefix: fmt.Sprintf("rpm%d_", time.Now().UnixNano()),
	}})
	require.NoError(b, err)
	conn, err := db.DB()
	require.NoError(b, err)
	oldDB, oldRedis, oldClient := DB, common.RedisEnabled, common.RDB
	oldType := common.MainDatabaseType()
	DB, common.RDB = db, client
	common.SetMainDatabaseType(common.DatabaseTypeMySQL)
	b.Cleanup(func() {
		DB, common.RedisEnabled, common.RDB = oldDB, oldRedis, oldClient
		common.SetMainDatabaseType(oldType)
		require.NoError(b, db.Migrator().DropTable(&ChannelLimitAttempt{}, &ChannelLimitConfig{}))
		require.NoError(b, conn.Close())
		require.NoError(b, client.Close())
	})
	require.NoError(b, db.AutoMigrate(&ChannelLimitConfig{}, &ChannelLimitAttempt{}))
	now := time.Now()
	for _, fixture := range []struct {
		name      string
		channelID int
		limit     int64
		full      bool
	}{{"full", 1, 512, true}, {"open", 2, 1024, false}} {
		config := ChannelLimitConfig{ChannelID: fixture.channelID, Revision: 1, ObservedSince: now.UnixMilli(), ChannelLimitSettings: ChannelLimitSettings{RPM: fixture.limit}}
		require.NoError(b, db.Create(&config).Error)
		attempts := make([]ChannelLimitAttempt, 4096+512)
		for i := range attempts {
			sent := now.Add(-2 * time.Minute)
			if i < 512 {
				sent = now.Add(-time.Second)
			}
			attempts[i] = ChannelLimitAttempt{ID: fmt.Sprintf("%d-%d", fixture.channelID, i), ChannelID: fixture.channelID, SentAtMS: sent.UnixMilli(), Completed: true}
		}
		require.NoError(b, db.CreateInBatches(attempts, 128).Error)
		for _, mode := range []string{"sql", "redis"} {
			b.Run(fixture.name+"/"+mode, func(b *testing.B) {
				common.RedisEnabled = mode == "redis"
				// Bound latency samples even for long benchmark runs.
				stride := max(1, (b.N+9999)/10000)
				latencies := make([]time.Duration, (b.N+stride-1)/stride)
				var calls atomic.Int64
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						i := int(calls.Add(1) - 1)
						start := time.Now()
						config, err := GetChannelLimitConfig(fixture.channelID)
						if err == nil {
							err = CheckChannelRPM(config, now)
						}
						if i%stride == 0 {
							latencies[i/stride] = time.Since(start)
						}
						var refusal *ChannelLimitError
						if fixture.full && errors.As(err, &refusal) && refusal.Reason == "rpm_limited" || !fixture.full && err == nil {
							continue
						}
						b.Errorf("unexpected admission result: %v", err)
						return
					}
				})
				b.StopTimer()
				slices.Sort(latencies)
				b.ReportMetric(float64(latencies[(len(latencies)*95-1)/100].Microseconds()), "p95-us")
				b.ReportMetric(float64(latencies[(len(latencies)*99-1)/100].Microseconds()), "p99-us")
			})
		}
	}
}
