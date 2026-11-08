package model

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
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
				require.NoError(t, db.Migrator().DropTable(&ChannelLimitEvent{}, &ChannelLimitAttempt{}, &ChannelDailyBudget{}, &ChannelLimitConfig{}))
				DB = oldDB
				common.SetMainDatabaseType(oldType)
				require.NoError(t, sqlDB.Close())
			})
			require.NoError(t, AutoMigrateChannelLimitSchema(db))
			require.NoError(t, AutoMigrateChannelLimitSchema(db))
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

			// A deployment starting mid-day cannot pretend historical usage is zero.
			require.NoError(t, EnsureChannelLimitTracking(8, false, now))
			_, err = UpdateChannelLimits(8, 1, ChannelLimitSettings{DailyUSDNano: cost, ReconcileSeconds: 300}, true, now)
			assert.ErrorIs(t, err, ErrChannelLimitAccounting)
			_, err = UpdateChannelLimits(8, 1, ChannelLimitSettings{DailyUSDNano: cost, ReconcileSeconds: 300}, true, midnight)
			require.NoError(t, err)
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
			require.NoError(t, RenewChannelAttemptLease(9, "live-process", now.Add(350*time.Second)))
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
