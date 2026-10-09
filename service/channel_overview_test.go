package service

import (
	"context"
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
			now := time.Now().UTC()
			base, ratio, zero := 100.0, 0.5, 0.0
			// A paid request, a free group with actual procurement cost, a retry
			// whose failed first attempt has unknown cost, and an unconfirmed
			// failed/refunding request on a zero-cost channel.
			requests := []*relaycommon.RelayInfo{
				{OverviewAttempts: []*relaycommon.OverviewAttempt{{EventID: "paid:0", RequestID: "paid", StartedAt: now, QuotaPerUnit: common.QuotaPerUnit, ChannelID: 1, ChannelName: "primary", GroupName: "paid", ModelName: "model-a", CredentialID: "key-a", CredentialName: "A", SupplierID: "supplier-a", SupplierName: "Supplier A", CostVersionID: "cost-v1", CostRatio: &ratio, Sent: true, Finished: true, Success: true, LatencyMS: 200, Billing: &relaycommon.OverviewBillingSnapshot{BaseQuota: &base, RevenueQuota: 200, RevenueConfirmed: true}}}},
				{OverviewAttempts: []*relaycommon.OverviewAttempt{{EventID: "free:0", RequestID: "free", StartedAt: now, QuotaPerUnit: common.QuotaPerUnit, ChannelID: 1, ChannelName: "primary", GroupName: "free", ModelName: "model-a", CredentialID: "key-a", CredentialName: "A", SupplierID: "supplier-a", SupplierName: "Supplier A", CostVersionID: "cost-v1", CostRatio: &ratio, Sent: true, Finished: true, Success: true, LatencyMS: 400, Billing: &relaycommon.OverviewBillingSnapshot{BaseQuota: &base, RevenueConfirmed: true}}}},
				{OverviewAttempts: []*relaycommon.OverviewAttempt{
					{EventID: "retry:0", RequestID: "retry", StartedAt: now, QuotaPerUnit: common.QuotaPerUnit, ChannelID: 2, ChannelName: "fallback", GroupName: "paid", ModelName: "model-b", CredentialID: "key-b", CredentialName: "B", SupplierID: "supplier-b", SupplierName: "Supplier B", CostRatio: &ratio, Sent: true, Finished: true, Failure: true, LatencyMS: 100},
					{EventID: "retry:1", RequestID: "retry", StartedAt: now, QuotaPerUnit: common.QuotaPerUnit, ChannelID: 1, ChannelName: "primary", GroupName: "paid", ModelName: "model-b", CredentialID: "key-a", CredentialName: "A", SupplierID: "supplier-a", SupplierName: "Supplier A", CostRatio: &ratio, Sent: true, Finished: true, Success: true, LatencyMS: 300, Billing: &relaycommon.OverviewBillingSnapshot{BaseQuota: &base, RevenueQuota: 200, RevenueConfirmed: true}},
				}},
				{OverviewAttempts: []*relaycommon.OverviewAttempt{{EventID: "unconfirmed:0", RequestID: "unconfirmed", StartedAt: now, QuotaPerUnit: common.QuotaPerUnit, ChannelID: 3, ChannelName: "free supplier", GroupName: "paid", ModelName: "model-a", CredentialID: "key-c", CostRatio: &zero, Sent: true, Finished: true, Failure: true, LatencyMS: 500}}},
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
				assert.Equal(t, 6, recorder.queries, "one report has six bounded business queries")
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
			absent, err := model.GetChannelOverview(context.Background(), "key", 7, "absent")
			require.NoError(t, err)
			assert.Empty(t, absent.Daily)
			assert.Nil(t, absent.Summary.Health)
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
			t.Logf("%s: migration twice, request/attempt attribution, fractional currency, unknown/null/zero, request-wide completeness, six-query budget, cancellation and durable projection recovery passed", fmt.Sprint(dialect))
		})
	}
}
