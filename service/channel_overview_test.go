package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type overviewQueryRecorder struct {
	logger.Interface
	queries      int
	onFirstQuery func()
}

func (r *overviewQueryRecorder) Trace(ctx context.Context, began time.Time, sql func() (string, int64), err error) {
	r.queries++
	r.Interface.Trace(ctx, began, sql, err)
	if r.queries == 1 && r.onFirstQuery != nil {
		r.onFirstQuery()
	}
}

func TestChannelOverviewDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "overview.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(0)&_txlock=immediate")
			case "mysql":
				dsn := os.Getenv("TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("TEST_MYSQL_DSN is not configured")
				}
				driver = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TEST_POSTGRES_DSN is not configured")
				}
				driver = postgres.Open(dsn)
			}
			db, err := gorm.Open(driver, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(2)
			originalDB, originalType := model.DB, common.MainDatabaseType()
			model.DB = db
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(&model.OverviewAttemptEvent{}, &model.OverviewDaily{}, &model.OverviewCollectionState{}))
				model.DB = originalDB
				common.SetMainDatabaseType(originalType)
				require.NoError(t, sqlDB.Close())
			})
			var version string
			query := "SELECT version()"
			if dialect == "sqlite" {
				query = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(query).Scan(&version).Error)
			t.Logf("%s version: %s", dialect, version)
			for range 2 {
				require.NoError(t, db.AutoMigrate(&model.OverviewAttemptEvent{}, &model.OverviewDaily{}, &model.OverviewCollectionState{}))
				require.NoError(t, model.InitializeOverviewCollection(db))
			}
			empty, err := model.GetChannelOverview(context.Background(), "channel", 7, "")
			require.NoError(t, err)
			assert.Empty(t, empty.Daily)
			assert.Nil(t, empty.Summary.Health)
			assert.Nil(t, empty.Summary.CostCoverage)
			assert.Nil(t, empty.Summary.CacheHitRate)
			assert.Nil(t, empty.Summary.CacheUsageCoverage)
			now := time.Now().UTC()
			base, ratio, zero := 100.0, 0.5, 0.0
			// A paid request, a free group with actual procurement cost, a retry
			// whose failed first attempt has unknown cost, and an unconfirmed
			// failed/refunding request on a zero-cost channel.
			requests := []*relaycommon.RelayInfo{
				{OverviewAttempts: []*relaycommon.OverviewAttempt{{EventID: "paid:0", RequestID: "paid", StartedAt: now, QuotaPerUnit: common.QuotaPerUnit, ChannelID: 1, ChannelName: "primary", GroupName: "paid", ModelName: "model-a", CredentialID: "key-a", CredentialName: "A", SupplierID: "supplier-a", SupplierName: "Supplier A", CostVersionID: "cost-v1", CostRatio: &ratio, Sent: true, Finished: true, Success: true, LatencyMS: 200, Billing: &relaycommon.OverviewBillingSnapshot{BaseQuota: &base, RevenueQuota: 200, RevenueConfirmed: true, CacheUsageKnown: true, CacheInputTokens: 1000, CacheReadTokens: 800, CacheWriteTokens: 100}}}},
				{OverviewAttempts: []*relaycommon.OverviewAttempt{{EventID: "free:0", RequestID: "free", StartedAt: now, QuotaPerUnit: common.QuotaPerUnit, ChannelID: 1, ChannelName: "primary", GroupName: "free", ModelName: "model-a", CredentialID: "key-a", CredentialName: "A", SupplierID: "supplier-a", SupplierName: "Supplier A", CostVersionID: "cost-v1", CostRatio: &ratio, Sent: true, Finished: true, Success: true, LatencyMS: 400, Billing: &relaycommon.OverviewBillingSnapshot{BaseQuota: &base, RevenueConfirmed: true, CacheUsageKnown: true, CacheInputTokens: 9000, CacheReadTokens: 900, CacheWriteTokens: 200}}}},
				{OverviewAttempts: []*relaycommon.OverviewAttempt{
					{EventID: "retry:0", RequestID: "retry", StartedAt: now, QuotaPerUnit: common.QuotaPerUnit, ChannelID: 2, ChannelName: "fallback", GroupName: "paid", ModelName: "model-b", CredentialID: "key-b", CredentialName: "B", SupplierID: "supplier-b", SupplierName: "Supplier B", CostRatio: &ratio, Sent: true, Finished: true, Failure: true, LatencyMS: 100},
					{EventID: "retry:1", RequestID: "retry", StartedAt: now, QuotaPerUnit: common.QuotaPerUnit, ChannelID: 1, ChannelName: "primary", GroupName: "paid", ModelName: "model-b", CredentialID: "key-a", CredentialName: "A", SupplierID: "supplier-a", SupplierName: "Supplier A", CostRatio: &ratio, Sent: true, Finished: true, Success: true, LatencyMS: 300, Billing: &relaycommon.OverviewBillingSnapshot{BaseQuota: &base, RevenueQuota: 200, RevenueConfirmed: true, CacheUsageKnown: true, CacheInputTokens: 2000}},
				}},
				{OverviewAttempts: []*relaycommon.OverviewAttempt{{EventID: "unconfirmed:0", RequestID: "unconfirmed", StartedAt: now, QuotaPerUnit: common.QuotaPerUnit, ChannelID: 3, ChannelName: "free supplier", GroupName: "paid", ModelName: "model-a", CredentialID: "key-c", CostRatio: &zero, Sent: true, Finished: true, Failure: true, LatencyMS: 500, Billing: &relaycommon.OverviewBillingSnapshot{UsageSource: "local_estimate", CacheInputTokens: 1000000, CacheReadTokens: 900000}}}},
			}
			for _, info := range requests {
				CompleteOverviewRelay(info)
			}
			pending, err := model.GetChannelOverview(context.Background(), "channel", 7, "")
			require.NoError(t, err)
			assert.True(t, pending.Partial, "durable unprojected events are visible as incomplete")
			count, err := model.ProjectOverviewEvents(context.Background(), db)
			require.NoError(t, err)
			assert.Equal(t, 5, count)
			for _, info := range requests {
				CompleteOverviewRelay(info)
			}
			count, err = model.ProjectOverviewEvents(context.Background(), db)
			require.NoError(t, err)
			assert.Zero(t, count, "replay must not double project")
			unit := common.QuotaPerUnit
			report, err := model.GetChannelOverview(context.Background(), "channel", 7, "")
			require.NoError(t, err)
			originalUnit := common.QuotaPerUnit
			common.QuotaPerUnit = originalUnit * 2
			historical, err := model.GetChannelOverview(context.Background(), "channel", 7, "")
			common.QuotaPerUnit = originalUnit
			require.NoError(t, err)
			assert.InDelta(t, report.Summary.Revenue, historical.Summary.Revenue, 1e-12, "configuration changes must not recalculate historical money")
			assert.InDelta(t, report.Summary.Cost, historical.Summary.Cost, 1e-12)
			assert.InDelta(t, 400.0/unit, report.Summary.Revenue, 1e-12)
			assert.InDelta(t, 150.0/unit, report.Summary.Cost, 1e-12)
			assert.InDelta(t, 100.0/unit, report.Summary.Margin, 1e-12, "unknown retry cost cannot inflate margin")
			assert.EqualValues(t, 4, report.Summary.Requests)
			assert.EqualValues(t, 5, report.Summary.Attempts)
			assert.EqualValues(t, 2, report.Summary.CompleteRequests)
			assert.EqualValues(t, 1, report.Summary.UnconfirmedRevenueRequests)
			require.NotNil(t, report.Summary.Health)
			assert.InDelta(t, 60.0, *report.Summary.Health, 1e-12)
			require.NotNil(t, report.Summary.CostCoverage)
			assert.InDelta(t, 80.0, *report.Summary.CostCoverage, 1e-12)
			assert.False(t, report.Partial)
			assert.EqualValues(t, 12000, report.Summary.CacheInputTokens)
			assert.EqualValues(t, 1700, report.Summary.CacheReadTokens)
			assert.EqualValues(t, 300, report.Summary.CacheWriteTokens)
			assert.EqualValues(t, 3, report.Summary.CacheUsageAttempts)
			assert.EqualValues(t, 2, report.Summary.CacheHitAttempts)
			require.NotNil(t, report.Summary.CacheHitRate)
			assert.InDelta(t, 1700.0/12000*100, *report.Summary.CacheHitRate, 1e-9, "cache rate is token weighted, never an average of per-request percentages")
			require.NotNil(t, report.Summary.CacheUsageCoverage)
			assert.InDelta(t, 60, *report.Summary.CacheUsageCoverage, 1e-9)
			require.Len(t, report.Daily, 1)
			assert.Equal(t, report.Summary.CacheReadTokens, report.Daily[0].CacheReadTokens)
			assert.Equal(t, report.Summary.CacheHitRate, report.Daily[0].CacheHitRate)
			require.Len(t, report.Channels, 3)
			for _, channel := range report.Channels {
				if channel.ID == "1" {
					assert.EqualValues(t, 12000, channel.CacheInputTokens)
					assert.Equal(t, report.Summary.CacheHitRate, channel.CacheHitRate)
				} else {
					assert.Nil(t, channel.CacheHitRate, "missing upstream usage is not a measured zero cache rate")
				}
			}
			require.Len(t, report.Models, 2)
			for _, row := range report.Models {
				require.NotNil(t, row.CacheHitRate)
				if row.ID == "model-a" {
					assert.InDelta(t, 17, *row.CacheHitRate, 1e-9)
				} else {
					assert.Zero(t, *row.CacheHitRate, "upstream explicitly reporting no cache reads is a measured 0%")
				}
			}
			for _, dimension := range []string{"channel", "group", "key", "supplier"} {
				recorder := &overviewQueryRecorder{Interface: logger.Default.LogMode(logger.Silent)}
				if dialect == "sqlite" {
					// Commit a write while the report's read snapshot is open.
					// Ordinary BEGIN IMMEDIATE would reject this with SQLITE_BUSY.
					recorder.onFirstQuery = func() {
						assert.NoError(t, db.Model(&model.OverviewCollectionState{}).Where("id = ?", 1).Update("collected_since", now.Unix()).Error, "a report must not reserve SQLite's WAL writer lock")
					}
				}
				model.DB = db.Session(&gorm.Session{Logger: recorder})
				value, err := model.GetChannelOverview(context.Background(), dimension, 7, "")
				require.NoError(t, err)
				assert.Equal(t, 7, recorder.queries, "one report has seven bounded business queries")
				assert.InDelta(t, report.Summary.Revenue, value.Summary.Revenue, 1e-12)
				assert.InDelta(t, report.Summary.Margin, value.Summary.Margin, 1e-12)
			}
			model.DB = db
			selected, err := model.GetChannelOverview(context.Background(), "channel", 7, "1")
			require.NoError(t, err)
			assert.EqualValues(t, 3, selected.Summary.Attempts)
			assert.EqualValues(t, 2, selected.Summary.CompleteRequests)
			assert.InDelta(t, 100.0/unit, selected.Summary.Margin, 1e-12, "filtering out failed channel cannot make its request complete")
			assert.Len(t, selected.Ranking, 1)
			assert.Len(t, selected.Options, 3)
			require.Len(t, selected.Channels, 1)
			assert.Equal(t, "1", selected.Channels[0].ID)
			assert.Equal(t, report.Summary.CacheReadTokens, selected.Summary.CacheReadTokens)
			paid, err := model.GetChannelOverview(context.Background(), "group", 7, "paid")
			require.NoError(t, err)
			assert.EqualValues(t, 3000, paid.Summary.CacheInputTokens)
			assert.EqualValues(t, 800, paid.Summary.CacheReadTokens)
			require.NotNil(t, paid.Summary.CacheHitRate)
			assert.InDelta(t, 800.0/3000*100, *paid.Summary.CacheHitRate, 1e-9)
			require.Len(t, paid.Channels, 3)
			for _, channel := range paid.Channels {
				if channel.ID == "1" {
					assert.EqualValues(t, 2, channel.CacheUsageAttempts)
					assert.EqualValues(t, 3000, channel.CacheInputTokens)
				}
			}
			absent, err := model.GetChannelOverview(context.Background(), "key", 7, "absent")
			require.NoError(t, err)
			assert.Empty(t, absent.Daily)
			assert.Nil(t, absent.Summary.Health)
			assert.Nil(t, absent.Summary.CacheHitRate)
			assert.Empty(t, absent.Channels)
			_, err = model.GetChannelOverview(context.Background(), "channel", 365, "")
			assert.Error(t, err)
			_, err = model.GetChannelOverview(context.Background(), "injected", 7, "")
			assert.Error(t, err)
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			_, err = model.GetChannelOverview(cancelled, "channel", 7, "")
			assert.Error(t, err)
			// A purchase multiplier can yield less than one quota unit. Analysis
			// preserves that expense instead of applying the user-charge rounding.
			fractionalBase, fractionalRatio := 0.4, 1.0
			for i := range 20 {
				requestID := fmt.Sprintf("fractional-%d", i)
				CompleteOverviewRelay(&relaycommon.RelayInfo{OverviewAttempts: []*relaycommon.OverviewAttempt{{EventID: requestID + ":0", RequestID: requestID, StartedAt: now, QuotaPerUnit: unit, ChannelID: 5, GroupName: "fractional", ModelName: "small-model", CostRatio: &fractionalRatio, Sent: true, Finished: true, Success: true, Billing: &relaycommon.OverviewBillingSnapshot{BaseQuota: &fractionalBase, RevenueQuota: 1, RevenueConfirmed: true}}}})
			}
			count, err = model.ProjectOverviewEvents(context.Background(), db)
			require.NoError(t, err)
			assert.Equal(t, 20, count)
			fractional, err := model.GetChannelOverview(context.Background(), "channel", 7, "5")
			require.NoError(t, err)
			assert.InDelta(t, 8.0/unit, fractional.Summary.Cost, 1e-12)
			assert.InDelta(t, 12.0/unit, fractional.Summary.Margin, 1e-12)
			assert.EqualValues(t, 20, fractional.Summary.CompleteRequests)
			var fractionalFact model.OverviewAttemptEvent
			require.NoError(t, db.First(&fractionalFact, "id = ?", "fractional-0:0").Error)
			assert.InDelta(t, 0.4, fractionalFact.CostQuota, 1e-12)
			// Preserve the existing per-attempt safety boundary without rounding
			// an oversized expense into an apparently complete purchase cost.
			oversizedBase := float64(common.MaxQuota) + 1
			CompleteOverviewRelay(&relaycommon.RelayInfo{OverviewAttempts: []*relaycommon.OverviewAttempt{{EventID: "oversized:0", RequestID: "oversized", StartedAt: now, QuotaPerUnit: unit, ChannelID: 6, CostRatio: &fractionalRatio, Sent: true, Finished: true, Success: true, Billing: &relaycommon.OverviewBillingSnapshot{BaseQuota: &oversizedBase, RevenueQuota: 1, RevenueConfirmed: true}}}})
			count, err = model.ProjectOverviewEvents(context.Background(), db)
			require.NoError(t, err)
			assert.Equal(t, 1, count)
			oversized, err := model.GetChannelOverview(context.Background(), "channel", 7, "6")
			require.NoError(t, err)
			assert.EqualValues(t, 1, oversized.Summary.UnknownCostAttempts)
			assert.Zero(t, oversized.Summary.CompleteAttempts)
			assert.Zero(t, oversized.Summary.Margin)
			// Persisted events survive a projection failure and can be replayed
			// after the aggregate table is recreated, without a hot-path queue.
			newEvent := model.OverviewAttemptEvent{ID: "recovery:0", RequestID: "recovery", QuotaPerUnit: common.QuotaPerUnit, Day: now.Format("2006-01-02"), StartedAt: now.Unix(), FinishedAt: now.Unix(), ChannelID: 4, GroupName: "paid", Status: "ready", CostKnown: true, CostQuota: 25, RevenueQuota: 75, RevenueConfirmed: true, IsFinal: true, RequestComplete: true, Success: true}
			require.NoError(t, model.CompleteOverviewRequest([]model.OverviewAttemptEvent{newEvent}))
			require.NoError(t, db.Migrator().DropTable(&model.OverviewDaily{}))
			_, err = model.ProjectOverviewEvents(context.Background(), db)
			require.Error(t, err)
			var stored model.OverviewAttemptEvent
			require.NoError(t, db.First(&stored, "id = ?", newEvent.ID).Error)
			assert.Equal(t, "ready", stored.Status)
			require.NoError(t, db.AutoMigrate(&model.OverviewDaily{}))
			count, err = model.ProjectOverviewEvents(context.Background(), db)
			require.NoError(t, err)
			assert.Equal(t, 1, count)
			var row model.OverviewDaily
			require.NoError(t, db.First(&row).Error)
			assert.EqualValues(t, 75, row.Revenue)
			assert.EqualValues(t, 25, row.Cost)
			count, err = model.ProjectOverviewEvents(context.Background(), db)
			require.NoError(t, err)
			assert.Zero(t, count)

			t.Run("released_schema_upgrade", func(t *testing.T) {
				require.NoError(t, db.Migrator().DropTable(&model.OverviewAttemptEvent{}, &model.OverviewDaily{}, &model.OverviewCollectionState{}))
				require.NoError(t, db.AutoMigrate(&overviewAttemptBeforeCache{}, &overviewDailyBeforeCache{}, &model.OverviewCollectionState{}))
				require.NoError(t, model.InitializeOverviewCollection(db))
				legacy := overviewDailyBeforeCache{Day: now.Format("2006-01-02"), ChannelID: 41, ChannelName: "preserved channel", GroupName: "manual", ModelName: "legacy-model", CredentialID: "manual-key", SupplierID: "manual-supplier", CostVersionID: "manual-cost", QuotaPerUnit: unit, Revenue: 80, Cost: 23, CompleteRevenue: 80, CompleteCost: 23, Requests: 1, Attempts: 1, Successes: 1, CompleteRequests: 1, CompleteAttempts: 1}
				identity, err := common.Marshal([]any{legacy.Day, legacy.ChannelID, legacy.GroupName, legacy.ModelName, legacy.CredentialID, legacy.SupplierID, legacy.OwnershipVersionID, legacy.CostVersionID, legacy.QuotaPerUnit})
				require.NoError(t, err)
				hash := sha256.Sum256(identity)
				legacy.ID = hex.EncodeToString(hash[:])
				require.NoError(t, db.Create(&legacy).Error)
				oldEvent := overviewAttemptBeforeCache{ID: "legacy-ready:0", RequestID: "legacy-ready", Day: legacy.Day, StartedAt: now.Unix(), Status: "ready", ChannelID: 42, ChannelName: "preserved pending channel", GroupName: "manual", ModelName: "legacy-model", CostRatio: &ratio, BaseQuota: &base, CostVersionID: "manual-cost", QuotaPerUnit: unit, RevenueQuota: 75, CostQuota: 25, CostKnown: true, RevenueConfirmed: true, IsFinal: true, RequestComplete: true, Success: true}
				require.NoError(t, db.Create(&oldEvent).Error)
				for range 2 {
					require.NoError(t, db.AutoMigrate(&model.OverviewAttemptEvent{}, &model.OverviewDaily{}, &model.OverviewCollectionState{}))
					require.NoError(t, model.InitializeOverviewCollection(db))
					var preserved model.OverviewDaily
					require.NoError(t, db.First(&preserved, "id = ?", legacy.ID).Error)
					assert.Equal(t, legacy.Revenue, preserved.Revenue)
					assert.Equal(t, legacy.Cost, preserved.Cost)
					assert.Equal(t, legacy.CostVersionID, preserved.CostVersionID)
					assert.Equal(t, legacy.CredentialID, preserved.CredentialID)
					var pending model.OverviewAttemptEvent
					require.NoError(t, db.First(&pending, "id = ?", oldEvent.ID).Error)
					assert.Equal(t, oldEvent.Status, pending.Status)
					assert.Equal(t, oldEvent.BaseQuota, pending.BaseQuota)
					assert.Equal(t, oldEvent.CostRatio, pending.CostRatio)
					assert.False(t, pending.CacheUsageKnown)
				}
				for _, index := range []string{"idx_overview_day", "idx_overview_channel", "idx_overview_group", "idx_overview_key", "idx_overview_supplier"} {
					assert.True(t, db.Migrator().HasIndex(&model.OverviewDaily{}, index), "upgrade preserves aggregate index %s", index)
				}
				assert.True(t, db.Migrator().HasIndex(&model.OverviewAttemptEvent{}, "idx_overview_projection"))
				oldReport, err := model.GetChannelOverview(context.Background(), "channel", 7, "41")
				require.NoError(t, err)
				assert.Nil(t, oldReport.Summary.CacheHitRate, "historical aggregates without telemetry stay unknown after upgrade")
				assert.EqualValues(t, 0, oldReport.Summary.CacheUsageAttempts)
				assert.InDelta(t, 80.0/unit, oldReport.Summary.Revenue, 1e-12)
				count, err := model.ProjectOverviewEvents(context.Background(), db)
				require.NoError(t, err)
				assert.Equal(t, 1, count, "legacy ready events remain recoverable after upgrade")
				newEvent := model.OverviewAttemptEvent{ID: "post-upgrade:0", RequestID: "post-upgrade", Day: legacy.Day, StartedAt: now.Unix(), Status: "ready", ChannelID: legacy.ChannelID, ChannelName: legacy.ChannelName, GroupName: legacy.GroupName, ModelName: legacy.ModelName, CredentialID: legacy.CredentialID, SupplierID: legacy.SupplierID, CostVersionID: legacy.CostVersionID, QuotaPerUnit: unit, RevenueQuota: 10, CostQuota: 5, CostKnown: true, RevenueConfirmed: true, IsFinal: true, RequestComplete: true, Success: true, CacheUsageKnown: true, CacheInputTokens: 100, CacheReadTokens: 75, CacheWriteTokens: 25}
				require.NoError(t, model.CompleteOverviewRequest([]model.OverviewAttemptEvent{newEvent}))
				count, err = model.ProjectOverviewEvents(context.Background(), db)
				require.NoError(t, err)
				assert.Equal(t, 1, count)
				require.NoError(t, model.CompleteOverviewRequest([]model.OverviewAttemptEvent{newEvent}))
				count, err = model.ProjectOverviewEvents(context.Background(), db)
				require.NoError(t, err)
				assert.Zero(t, count, "upgrade replay cannot duplicate financial or cache totals")
				upgraded, err := model.GetChannelOverview(context.Background(), "channel", 7, "41")
				require.NoError(t, err)
				assert.EqualValues(t, 2, upgraded.Summary.Attempts)
				assert.EqualValues(t, 100, upgraded.Summary.CacheInputTokens)
				assert.EqualValues(t, 75, upgraded.Summary.CacheReadTokens)
				assert.EqualValues(t, 25, upgraded.Summary.CacheWriteTokens)
				require.NotNil(t, upgraded.Summary.CacheHitRate)
				assert.Equal(t, 75.0, *upgraded.Summary.CacheHitRate)
				require.NotNil(t, upgraded.Summary.CacheUsageCoverage)
				assert.Equal(t, 50.0, *upgraded.Summary.CacheUsageCoverage)
				assert.InDelta(t, 90.0/unit, upgraded.Summary.Revenue, 1e-12)
				assert.InDelta(t, 28.0/unit, upgraded.Summary.Cost, 1e-12)
				var cohorts int64
				require.NoError(t, db.Model(&model.OverviewDaily{}).Where("channel_id = ?", 41).Count(&cohorts).Error)
				assert.EqualValues(t, 1, cohorts, "the existing primary-key cohort is updated rather than duplicated")
			})
			t.Logf("%s: migration twice, request/attempt attribution, fractional currency, unknown/null/zero, request-wide completeness, seven-query budget, cancellation and durable projection recovery passed", fmt.Sprint(dialect))
		})
	}
}

// Released overview schemas before cache telemetry was persisted. These fixtures
// exercise an additive upgrade with occupied aggregates and an unprojected event.
type overviewAttemptBeforeCache struct {
	ID                      string `gorm:"type:varchar(96);primaryKey"`
	RequestID               string `gorm:"type:varchar(64);index"`
	Day                     string `gorm:"type:varchar(10);index"`
	StartedAt               int64  `gorm:"index:idx_overview_projection,priority:2"`
	FinishedAt              int64
	Status                  string `gorm:"type:varchar(16);index:idx_overview_projection,priority:1"`
	ChannelID               int
	ChannelName             string `gorm:"type:varchar(191)"`
	GroupName               string `gorm:"type:varchar(191)"`
	ModelName               string `gorm:"type:varchar(191)"`
	CredentialID            string `gorm:"type:varchar(64)"`
	CredentialName          string `gorm:"type:varchar(191)"`
	CredentialVersionID     string `gorm:"type:varchar(64)"`
	BindingID               string `gorm:"type:varchar(64)"`
	SupplierID              string `gorm:"type:varchar(64)"`
	SupplierName            string `gorm:"type:varchar(191)"`
	OwnershipVersionID      string `gorm:"type:varchar(64)"`
	CostVersionID           string `gorm:"type:varchar(64)"`
	CredentialCostVersionID string `gorm:"type:varchar(64)"`
	ChannelCostVersionID    string `gorm:"type:varchar(64)"`
	TagsJSON                string `gorm:"type:text"`
	CostSource              string `gorm:"type:varchar(16)"`
	CostRatio               *float64
	BaseQuota               *float64
	QuotaPerUnit            float64
	RevenueQuota            int64
	RawRevenueQuota         int64
	RevenueUnitAmbiguous    bool
	CostQuota               float64
	CostKnown               bool
	RevenueConfirmed        bool
	BillingSource           string `gorm:"type:varchar(16)"`
	RequestComplete         bool
	IsFinal                 bool
	Success                 bool
	Failure                 bool
	LatencyMS               int64
}

func (overviewAttemptBeforeCache) TableName() string { return "overview_attempt_events" }

type overviewDailyBeforeCache struct {
	ID                         string `gorm:"type:varchar(64);primaryKey"`
	Day                        string `gorm:"type:varchar(10);index:idx_overview_day;index:idx_overview_channel,priority:2;index:idx_overview_group,priority:2;index:idx_overview_key,priority:2;index:idx_overview_supplier,priority:2"`
	ChannelID                  int    `gorm:"index:idx_overview_channel,priority:1"`
	ChannelName                string `gorm:"type:varchar(191)"`
	GroupName                  string `gorm:"type:varchar(191);index:idx_overview_group,priority:1"`
	ModelName                  string `gorm:"type:varchar(191)"`
	CredentialID               string `gorm:"type:varchar(64);index:idx_overview_key,priority:1"`
	CredentialName             string `gorm:"type:varchar(191)"`
	SupplierID                 string `gorm:"type:varchar(64);index:idx_overview_supplier,priority:1"`
	SupplierName               string `gorm:"type:varchar(191)"`
	OwnershipVersionID         string `gorm:"type:varchar(64)"`
	CostVersionID              string `gorm:"type:varchar(64)"`
	QuotaPerUnit               float64
	Revenue                    int64
	Cost                       float64
	CompleteRevenue            int64
	CompleteCost               float64
	Requests                   int64
	Attempts                   int64
	Successes                  int64
	Failures                   int64
	UnconfirmedRevenueRequests int64
	SubscriptionRevenue        int64
	UnknownCostAttempts        int64
	CompleteRequests           int64
	CompleteAttempts           int64
	LatencyTotalMS             int64
	LatencySamples             int64
}

func (overviewDailyBeforeCache) TableName() string { return "overview_dailies" }
