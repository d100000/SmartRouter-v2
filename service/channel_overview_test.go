package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
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

func TestChannelOverviewMediaRemainsIndependentOfTokenEstimates(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/v1/audio/speech", nil)
	attempt := &relaycommon.OverviewAttempt{Sent: true, DispatchedAt: time.Now()}
	info := &relaycommon.RelayInfo{
		OverviewAttempt:  attempt,
		AnalyticsBilling: &relaycommon.OverviewBillingSnapshot{UsageSource: "local_estimate", InputTokens: 1000},
	}
	info.ObserveAudioSeconds(1.25, "media_metadata")
	FinishOverviewAttempt(ctx, info, nil)
	require.NotNil(t, attempt.Billing)
	require.NotNil(t, attempt.Billing.AudioSeconds)
	assert.Equal(t, 1.25, *attempt.Billing.AudioSeconds)
	assert.Equal(t, "media_metadata", attempt.Billing.AudioUsageSource)
	assert.Equal(t, "local_estimate", attempt.Billing.UsageSource)
	assert.Nil(t, attempt.Billing.ImageCount)
	assert.Nil(t, attempt.Billing.BaseQuota, "measured media duration does not manufacture procurement cost")
	info.ObserveAudioSeconds(9, "upstream_usage")
	assert.Equal(t, 1.25, *attempt.Billing.AudioSeconds, "finished attempts retain their own observations")

	mediaOnly := &relaycommon.RelayInfo{OverviewAttempt: &relaycommon.OverviewAttempt{Sent: true, DispatchedAt: time.Now()}}
	mediaOnly.ObserveImageCount(2)
	FinishOverviewAttempt(ctx, mediaOnly, nil)
	require.NotNil(t, mediaOnly.OverviewAttempt.Billing)
	assert.EqualValues(t, 2, *mediaOnly.OverviewAttempt.Billing.ImageCount)
	assert.Empty(t, mediaOnly.OverviewAttempt.Billing.UsageSource)
}

func TestChannelOverviewVendorRequestIDIsSafeAndAttemptLocal(t *testing.T) {
	for _, tc := range []struct{ name, value, want string }{
		{"vendor identifier", "req_01AZ-2.b:3", "req_01AZ-2.b:3"},
		{"maximum length", strings.Repeat("a", 128), strings.Repeat("a", 128)},
		{"missing", "", ""},
		{"too long", strings.Repeat("a", 129), ""},
		{"header injection", "vendor\r\nsecret", ""},
		{"non ASCII", "vendor-编号", ""},
		{"raw JSON", `{"id":"vendor"}`, ""},
		{"URL", "https://example.com/id", ""},
		{"CSV formula", "=vendor", ""},
		{"upstream secret", "req-upstream-secret-suffix", ""},
		{"consumer secret", "req-consumer-secret-suffix", ""},
		{"request authorization", "req-header-secret-suffix", ""},
		{"alternate authorization", "req-alternate-secret-suffix", ""},
		{"global platform ID", "platform-request-id", ""},
		{"wrapped platform ID", "req-platform-request-id-suffix", ""},
		{"relay request ID", "relay-request-id", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
			ctx.Request.Header.Set("Authorization", "Bearer header-secret")
			ctx.Request.Header.Set("X-Api-Key", "alternate-secret")
			ctx.Set(common.UpstreamRequestIdKey, tc.value)
			ctx.Set(common.RequestIdKey, "platform-request-id")
			attempt := &relaycommon.OverviewAttempt{Sent: true, DispatchedAt: time.Now()}
			info := &relaycommon.RelayInfo{OverviewAttempt: attempt, ChannelMeta: &relaycommon.ChannelMeta{ApiKey: "upstream-secret"}, TokenKey: "consumer-secret", RequestId: "relay-request-id"}
			FinishOverviewAttempt(ctx, info, nil)
			assert.Equal(t, tc.want, attempt.UpstreamRequestID)
			ctx.Set(common.UpstreamRequestIdKey, "another-attempt-id")
			FinishOverviewAttempt(ctx, info, nil)
			assert.Equal(t, tc.want, attempt.UpstreamRequestID, "finished observations cannot be replaced by a later attempt")
		})
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
				// Match the primary database configuration: this matrix recreates
				// released tables on one connection before testing startup upgrades.
				driver = postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
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
			assert.Nil(t, empty.Summary.CacheAttemptHitRate)
			assert.Empty(t, empty.ChannelModels)
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
				if info.OverviewAttempts[0].EventID == "paid:0" {
					info.OverviewAttempts[0].Billing.OverviewMediaUsage = relaycommon.OverviewMediaUsage{ImageCount: common.GetPointer(int64(2)), ImageUsageSource: "upstream_payload"}
					info.OverviewAttempts[0].UpstreamRequestID = "req_vendor-01"
				}
				if info.OverviewAttempts[0].EventID == "free:0" {
					info.OverviewAttempts[0].Billing.OverviewMediaUsage = relaycommon.OverviewMediaUsage{AudioSeconds: common.GetPointer(1.25), AudioUsageSource: "media_metadata"}
				}
				CompleteOverviewRelay(info)
			}
			var imageEvent, audioEvent, unknownEvent model.OverviewAttemptEvent
			require.NoError(t, db.First(&imageEvent, "id = ?", "paid:0").Error)
			require.NoError(t, db.First(&audioEvent, "id = ?", "free:0").Error)
			require.NoError(t, db.First(&unknownEvent, "id = ?", "retry:0").Error)
			require.NotNil(t, imageEvent.ImageCount)
			assert.EqualValues(t, 2, *imageEvent.ImageCount)
			assert.Equal(t, "upstream_payload", imageEvent.ImageUsageSource)
			assert.Equal(t, "req_vendor-01", imageEvent.UpstreamRequestID)
			require.NotNil(t, audioEvent.AudioSeconds)
			assert.Equal(t, 1.25, *audioEvent.AudioSeconds)
			assert.Equal(t, "media_metadata", audioEvent.AudioUsageSource)
			assert.Nil(t, imageEvent.AudioSeconds)
			assert.Nil(t, audioEvent.ImageCount)
			assert.Nil(t, unknownEvent.ImageCount)
			assert.Nil(t, unknownEvent.AudioSeconds)
			assert.Empty(t, unknownEvent.UpstreamRequestID)
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
			require.NotNil(t, report.Summary.CacheAttemptHitRate)
			assert.InDelta(t, 2.0/3*100, *report.Summary.CacheAttemptHitRate, 1e-9, "unknown usage must not count as a cache miss")
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
				assert.Equal(t, 8, recorder.queries, "cross-channel model comparisons use one bounded query, not one query per channel")
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
			require.Len(t, selected.ChannelModels, 2)
			for _, row := range selected.ChannelModels {
				assert.Equal(t, 1, row.ChannelID)
				assert.Equal(t, "primary", row.ChannelName)
			}
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
			assert.Empty(t, absent.ChannelModels)
			_, err = model.GetChannelOverview(context.Background(), "channel", 365, "")
			assert.Error(t, err)
			_, err = model.GetChannelOverview(context.Background(), "injected", 7, "")
			assert.Error(t, err)
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			_, err = model.GetChannelOverview(cancelled, "channel", 7, "")
			assert.Error(t, err)
			t.Run("channel_model_cache_comparison", func(t *testing.T) {
				rows := []model.OverviewDaily{
					{ID: "cache-a-today", Day: now.Format("2006-01-02"), ChannelID: 21, ChannelName: "same name", ModelName: "shared-model", Attempts: 2, CacheUsageAttempts: 1, CacheHitAttempts: 1, CacheInputTokens: 1000, CacheReadTokens: 900},
					{ID: "cache-a-yesterday", Day: now.AddDate(0, 0, -1).Format("2006-01-02"), ChannelID: 21, ChannelName: "same name", ModelName: "shared-model", Attempts: 1, CacheUsageAttempts: 1, CacheHitAttempts: 1, CacheInputTokens: 9000, CacheReadTokens: 100},
					{ID: "cache-b-miss", Day: now.Format("2006-01-02"), ChannelID: 22, ChannelName: "same name", ModelName: "shared-model", Attempts: 2, CacheUsageAttempts: 2, CacheInputTokens: 1000},
					{ID: "cache-a-other", Day: now.Format("2006-01-02"), ChannelID: 21, ChannelName: "same name", ModelName: "other-model", Attempts: 1, CacheUsageAttempts: 1, CacheHitAttempts: 1, CacheInputTokens: 1000, CacheReadTokens: 1000},
					{ID: "cache-b-unknown", Day: now.Format("2006-01-02"), ChannelID: 22, ChannelName: "same name", ModelName: "other-model", Attempts: 1},
					{ID: "cache-old", Day: now.AddDate(0, 0, -8).Format("2006-01-02"), ChannelID: 21, ChannelName: "same name", ModelName: "shared-model", Attempts: 1, CacheUsageAttempts: 1, CacheInputTokens: 10000},
				}
				for i := range rows {
					rows[i].GroupName, rows[i].QuotaPerUnit = "cache-comparison", unit
				}
				require.NoError(t, db.Create(&rows).Error)
				comparison, err := model.GetChannelOverview(context.Background(), "group", 7, "cache-comparison")
				require.NoError(t, err)
				require.Len(t, comparison.ChannelModels, 4)
				assert.False(t, comparison.ChannelModelsTruncated)
				byID := make(map[string]model.OverviewChannelModelRow)
				for _, row := range comparison.ChannelModels {
					byID[row.ID] = row
				}
				require.Len(t, byID, 4, "same model and same channel names must keep distinct channel/model identities")
				a := byID["21:shared-model"]
				assert.Equal(t, "shared-model", a.Name)
				assert.EqualValues(t, 10000, a.CacheInputTokens)
				require.NotNil(t, a.CacheHitRate)
				assert.Equal(t, 10.0, *a.CacheHitRate, "daily ratios must be weighted by measured input tokens")
				require.NotNil(t, a.CacheAttemptHitRate)
				assert.Equal(t, 100.0, *a.CacheAttemptHitRate)
				require.NotNil(t, a.CacheUsageCoverage)
				assert.InDelta(t, 200.0/3, *a.CacheUsageCoverage, 1e-9)
				miss := byID["22:shared-model"]
				require.NotNil(t, miss.CacheHitRate)
				assert.Zero(t, *miss.CacheHitRate)
				require.NotNil(t, miss.CacheAttemptHitRate)
				assert.Zero(t, *miss.CacheAttemptHitRate)
				unknown := byID["22:other-model"]
				assert.Nil(t, unknown.CacheHitRate)
				assert.Nil(t, unknown.CacheAttemptHitRate)
				month, err := model.GetChannelOverview(context.Background(), "group", 30, "cache-comparison")
				require.NoError(t, err)
				require.NotEmpty(t, month.ChannelModels)
				assert.Equal(t, "21:shared-model", month.ChannelModels[0].ID)
				require.NotNil(t, month.ChannelModels[0].CacheHitRate)
				assert.Equal(t, 5.0, *month.ChannelModels[0].CacheHitRate)
			})
			t.Run("bounded_channel_model_results", func(t *testing.T) {
				rows := make([]model.OverviewDaily, 101)
				for i := range rows {
					rows[i] = model.OverviewDaily{ID: fmt.Sprintf("bounded-cache-%03d", i), Day: now.Format("2006-01-02"), ChannelID: 99, ModelName: fmt.Sprintf("model-%03d", i), GroupName: "cache-limit", QuotaPerUnit: unit, Attempts: 1, CacheUsageAttempts: 1, CacheInputTokens: 10}
				}
				rows[100].ChannelID = 100
				require.NoError(t, db.Create(&rows).Error)
				limited, err := model.GetChannelOverview(context.Background(), "group", 7, "cache-limit")
				require.NoError(t, err)
				require.Len(t, limited.ChannelModels, 100)
				assert.True(t, limited.ChannelModelsTruncated)
				assert.EqualValues(t, 101, limited.Summary.CacheUsageAttempts, "summary must include rows beyond the detail limit")
				assert.Equal(t, "model-000", limited.ChannelModels[0].Name)
				assert.Equal(t, "model-099", limited.ChannelModels[99].Name)
				selected, err := model.GetChannelOverview(context.Background(), "channel", 7, "99")
				require.NoError(t, err)
				assert.Len(t, selected.ChannelModels, 100)
				assert.False(t, selected.ChannelModelsTruncated, "exactly 100 cohorts are complete")
				tail, err := model.GetChannelOverview(context.Background(), "channel", 7, "100")
				require.NoError(t, err)
				require.Len(t, tail.ChannelModels, 1)
				assert.Equal(t, "model-100", tail.ChannelModels[0].Name, "filtering happens before limiting")
			})
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
					assert.Nil(t, pending.ImageCount, "historical media quantity is unknown")
					assert.Nil(t, pending.AudioSeconds)
					assert.Empty(t, pending.UpstreamRequestID)
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
			t.Logf("%s: migration twice, request/attempt attribution, fractional currency, unknown/null/zero, channel/model cache comparisons, request-wide completeness, eight-query budget, cancellation and durable projection recovery passed", fmt.Sprint(dialect))
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

func TestChannelLimitActualRequestBudget(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	common.SetContextKey(ctx, constant.ContextKeyOriginalModel, "gpt-4")
	info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: types.RelayFormatOpenAI,
		ChannelLimitAttempt: &relaycommon.ChannelLimitRuntime{TPMApplicable: true, MaxOutputTokens: 128}}
	for _, tc := range []struct {
		name  string
		body  string
		want  int64
		known bool
		mode  int
	}{
		{"final outbound limit and choices", `{"messages":[{"role":"user","content":"hello"}],"max_completion_tokens":100,"n":3}`, 300, true, relayconstant.RelayModeChatCompletions},
		{"trusted template fallback", `{"messages":[{"role":"user","content":"hello"}]}`, 128, true, relayconstant.RelayModeChatCompletions},
		{"explicit zero remains zero", `{"messages":[{"role":"user","content":"hello"}],"max_tokens":0}`, 0, true, relayconstant.RelayModeChatCompletions},
		{"fractional bound is unknown", `{"messages":[{"role":"user","content":"hello"}],"max_tokens":1.5}`, 0, false, relayconstant.RelayModeChatCompletions},
		{"completion strings batch", `{"prompt":["hello","world"],"max_tokens":10,"n":2,"best_of":3}`, 60, true, relayconstant.RelayModeCompletions},
		{"completion flat token IDs", `{"prompt":[10,11,12,13,14],"max_tokens":10,"n":2}`, 20, true, relayconstant.RelayModeCompletions},
		{"completion nested token IDs", `{"prompt":[[10,11,12],[20,21]],"max_tokens":10,"n":2}`, 40, true, relayconstant.RelayModeCompletions},
		{"embedding token IDs", `{"input":[[10,11,12],[20,21]]}`, 0, true, relayconstant.RelayModeEmbeddings},
		{"mixed prompt batch", `{"prompt":["hello",[10,11]],"max_tokens":10}`, 0, false, relayconstant.RelayModeCompletions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info.RelayMode = tc.mode
			input, output, total := channelLimitTokenBudget(ctx, info, []byte(tc.body))
			if !tc.known {
				assert.Nil(t, output)
				assert.Nil(t, total)
				return
			}
			require.NotNil(t, input)
			assert.Positive(t, *input)
			require.NotNil(t, output)
			require.NotNil(t, total)
			assert.Equal(t, tc.want, *output)
			assert.Equal(t, *input+tc.want, *total)
			if strings.Contains(tc.name, "token IDs") {
				assert.GreaterOrEqual(t, *input, int64(5), "token-ID inputs must not disappear from the request estimate")
			}
		})
	}
	info.RelayMode = relayconstant.RelayModeChatCompletions
	info.ChannelLimitAttempt.MaxOutputTokens = 0
	_, output, total := channelLimitTokenBudget(ctx, info, []byte(`{"messages":[{"role":"user","content":"hello"}]}`))
	assert.Nil(t, output)
	assert.Nil(t, total)
}

func TestChannelLimitProcurementUsesFullFrozenBudget(t *testing.T) {
	ratio := 0.5
	input, output := int64(1000), int64(100)
	expression := `tier("base", p * 2 + c * 10)`
	info := &relaycommon.RelayInfo{
		ChannelLimitAttempt: &relaycommon.ChannelLimitRuntime{CostRatio: &ratio},
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{ExprString: expression, ExprHash: billingexpr.ExprHashString(expression),
			GroupRatio: 0, QuotaPerUnit: 500000, ExprVersion: 1, EstimatedQuotaBeforeGroup: 1000},
	}
	budget, err := channelLimitProcurementBudget(info, &input, &output)
	require.NoError(t, err)
	assert.Equal(t, int64(2500000), budget, "input, long-TTL cache fallback, and output cost survive a free customer group")
	assert.Zero(t, info.TieredBillingSnapshot.GroupRatio, "budget estimation must not mutate consumer billing")
	_, err = channelLimitProcurementBudget(info, &input, nil)
	assert.ErrorIs(t, err, model.ErrChannelLimitAccounting)
	expression = `tier("cache", p * 2 + cr * 30 + c * 10)`
	info.TieredBillingSnapshot.ExprString, info.TieredBillingSnapshot.ExprHash = expression, billingexpr.ExprHashString(expression)
	budget, err = channelLimitProcurementBudget(info, &input, &output)
	require.NoError(t, err)
	assert.Equal(t, int64(17500000), budget, "all cache allocations are bounded conservatively, including omitted long-TTL prices")
	expression = `tier("request", fixed(0.01))`
	info.TieredBillingSnapshot.ExprString, info.TieredBillingSnapshot.ExprHash = expression, billingexpr.ExprHashString(expression)
	budget, err = channelLimitProcurementBudget(info, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(5000000), budget)

	for _, tc := range []struct {
		name       string
		expression string
		want       int64
		invalid    bool
	}{
		{"decreasing output price", `tier("decreasing", max(100-c,0)*1000000)`, 50000000000, false},
		{"mixed cache conditional", `p>0 && cr>0 ? tier("mixed",100000000) : tier("empty",0)`, 50000000000, false},
		{"interior maximum", `tier("interior",c*(100-c)*1000)`, 5000000000, false},
		{"long cache normalization", `tier("cache",cc*2)`, 2000000, false},
		{"request conditional", `tier("request",fixed(0.01))*(header("service-tier")=="fast" ? 2 : 1)`, 10000000, false},
		{"frozen request amount", `tier("request",param("amount")*1000000)`, 1500000000, false},
		{"explicit zero", `tier("request",fixed(0))`, 0, false},
		{"unbounded reciprocal", `tier("bad",100/c)`, 0, true},
		{"unsupported operator", `tier("unsupported",c^2)`, 0, true},
		{"mutable usage facts", `tier("usage",u("image_count"))`, 0, true},
		{"quota saturation", `tier("oversized",1000000000000000)`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info.TieredBillingSnapshot.ExprString = tc.expression
			info.TieredBillingSnapshot.ExprHash = billingexpr.ExprHashString(tc.expression)
			info.BillingRequestInput = &billingexpr.RequestInput{Body: []byte(`{"amount":3}`)}
			budget, err := channelLimitProcurementBudget(info, &input, &output)
			if tc.invalid {
				assert.ErrorIs(t, err, model.ErrChannelLimitAccounting)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, budget)
		})
	}
}

func TestChannelLimitSupplierSnapshotAndNormalizedSettlement(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(":memory:")
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
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "supplier_cost_test_"}, Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			connection, err := db.DB()
			require.NoError(t, err)
			connection.SetMaxOpenConns(1)
			previousDB, previousType, previousQuota, previousCache := model.DB, common.MainDatabaseType(), common.QuotaPerUnit, common.MemoryCacheEnabled
			model.DB, common.QuotaPerUnit, common.MemoryCacheEnabled = db, 500000, false
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			schemas := []any{&model.Channel{}, &model.Ability{}, &model.SupplierAccount{}, &model.SupplierChannelDraft{}, &model.UpstreamSupplier{}, &model.UpstreamCredential{}, &model.UpstreamCredentialVersion{}, &model.ChannelCredentialBinding{}, &model.UpstreamCredentialFingerprintSecret{}, &model.ChannelLimitEvent{}, &model.ChannelLimitConfig{}, &model.ChannelLimitAttempt{}, &model.ChannelDailyBudget{}}
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(schemas...))
				model.DB, common.QuotaPerUnit, common.MemoryCacheEnabled = previousDB, previousQuota, previousCache
				common.SetMainDatabaseType(previousType)
				require.NoError(t, connection.Close())
			})
			for range 2 {
				require.NoError(t, db.AutoMigrate(schemas...))
			}
			t.Setenv("SUPPLIER_CREDENTIAL_KEYS", `{"v1":"`+base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))+`"}`)
			t.Setenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY", "v1")
			supplierRatio := 0.5
			supplier, err := model.CreateUpstreamSupplier("Supplier cost contract")
			require.NoError(t, err)
			account := model.SupplierAccount{SupplierID: supplier.ID, SupplyEnabled: true, AllowedTypes: "[1]", AllowedModels: "gpt-4", Group: "default", PoolWeight: 1, MaxChannels: 5, CostRatio: &supplierRatio, MaxOutputTokens: 128}
			require.NoError(t, model.SaveSupplierAccount(&account, 0))
			secret, err := model.EncryptSupplierSecret(account.SupplierID, "supplier-cost-test-secret")
			require.NoError(t, err)
			draft := model.SupplierChannelDraft{SupplierID: account.SupplierID, Name: "Supplier cost", Type: constant.ChannelTypeOpenAI, BaseURL: "https://upstream.invalid", Secret: secret, Models: "gpt-4"}
			channel, err := model.SaveSupplierChannelDraft(&draft, 99, 0, 0)
			require.NoError(t, err)
			require.NoError(t, model.EnsureChannelLimitTracking(channel.Id, true, time.Now()))
			require.NoError(t, model.PublishSupplierChannelDraft(channel.Id, account.SupplierID, channel.SupplierVersion, []string{"gpt-4"}))
			require.NoError(t, db.Omit("key").First(channel, "id = ?", channel.Id).Error)
			initial := channel.GetUpstreamCredentialSnapshot(0)
			assert.Equal(t, account.DefaultCostVersionID(), initial.CostVersionID)
			_, err = model.UpdateChannelLimits(channel.Id, 1, model.ChannelLimitSettings{RPM: 10, TPM: 2000, DailyUSDNano: model.ChannelLimitUSDScale, ReconcileSeconds: 300}, channel.ProcurementCostReady(), time.Now())
			require.NoError(t, err)
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
			common.SetContextKey(ctx, constant.ContextKeyOriginalModel, "gpt-4")
			info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: types.RelayFormatOpenAI, OriginModelName: "gpt-4", ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id, ChannelType: channel.Type}}
			info.PriceData.UsePrice, info.PriceData.ModelPrice = true, 0.002
			require.NoError(t, PrepareChannelLimitAttempt(info, channel))
			PrepareOverviewAttempt(ctx, info, channel)
			assert.Equal(t, 0.5, *info.ChannelLimitAttempt.CostRatio)
			assert.Equal(t, info.ChannelLimitAttempt.CostVersionID, info.OverviewAttempt.CostVersionID)
			request := httptest.NewRequest("POST", "https://upstream.invalid/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"private request text"}]}`))
			request.Header.Set("Content-Type", "application/json")
			require.NoError(t, BeforeChannelUpstreamRequest(ctx, info, request))
			var persisted model.ChannelLimitAttempt
			require.NoError(t, db.First(&persisted, "id = ?", info.ChannelLimitAttempt.ID).Error)
			assert.Equal(t, 0.5, *persisted.CostRatio)
			assert.Equal(t, float64(500000), persisted.QuotaPerUnit)
			assert.Contains(t, persisted.PricingSnapshot, initial.CostVersionID)
			assert.NotContains(t, persisted.PricingSnapshot, "private request text")
			assert.NotContains(t, persisted.PricingSnapshot, "supplier-cost-test-secret")
			account.CostRatio = common.GetPointer(0.25)
			require.NoError(t, model.SaveSupplierAccount(&account, account.Revision))
			base := float64(1000)
			info.AnalyticsBilling = &relaycommon.OverviewBillingSnapshot{BaseQuota: &base, QuotaPerUnit: 500000, UsageSource: "upstream", InputTokens: 20, OutputTokens: 10, CacheUsageKnown: true, CacheInputTokens: 100}
			require.NoError(t, FinishChannelLimitAttempt(ctx, info, nil))
			require.NoError(t, FinishChannelLimitAttempt(ctx, info, nil))
			state, err := model.GetChannelLimitState(channel.Id, time.Now())
			require.NoError(t, err)
			assert.Equal(t, int64(110), state.TPMUsed)
			assert.Equal(t, int64(1000000), state.DailyUsedNano, "in-flight cost remains frozen")
			assert.Zero(t, state.DailyHeldNano)
			assert.Equal(t, int64(1), state.RPMUsed)
			require.NoError(t, PrepareChannelLimitAttempt(info, channel)) // deliberately reuse stale routing metadata
			PrepareOverviewAttempt(ctx, info, channel)
			assert.Equal(t, 0.25, *info.ChannelLimitAttempt.CostRatio)
			assert.Equal(t, *info.ChannelLimitAttempt.CostRatio, *info.OverviewAttempt.CostRatio)
			assert.Equal(t, account.DefaultCostVersionID(), info.OverviewAttempt.CostVersionID)
			credential, err := model.UpdateUpstreamCredentialMetadata(initial.CredentialID, model.UpstreamCredentialMetadata{SupplierID: account.SupplierID, CostRatio: common.GetPointer(0.1)})
			require.NoError(t, err)
			account.CostRatio = common.GetPointer(0.75)
			require.NoError(t, model.SaveSupplierAccount(&account, account.Revision))
			require.NoError(t, PrepareChannelLimitAttempt(info, channel))
			PrepareOverviewAttempt(ctx, info, channel)
			assert.Equal(t, 0.1, *info.ChannelLimitAttempt.CostRatio, "credential override wins over a changed supplier default")
			assert.Equal(t, "credential", info.OverviewAttempt.CostSource)
			assert.Equal(t, credential.CostVersionID, info.ChannelLimitAttempt.CostVersionID)
			assert.Equal(t, info.ChannelLimitAttempt.CostVersionID, info.OverviewAttempt.CostVersionID)
			candidate := *channel
			candidate.ProcurementCredentialID = common.GetPointer("")
			require.NoError(t, PrepareChannelLimitAttempt(info, &candidate))
			assert.Equal(t, 0.75, *info.ChannelLimitAttempt.CostRatio, "new candidate material cannot inherit the previous Key's override")
			candidate.ProcurementCredentialID = common.GetPointer(credential.ID)
			require.NoError(t, PrepareChannelLimitAttempt(info, &candidate))
			assert.Equal(t, 0.1, *info.ChannelLimitAttempt.CostRatio, "a verified matching credential retains its override during a probe")
			_, err = model.UpdateUpstreamCredentialMetadata(initial.CredentialID, model.UpstreamCredentialMetadata{SupplierID: account.SupplierID})
			require.NoError(t, err)
			require.NoError(t, PrepareChannelLimitAttempt(info, channel))
			assert.Equal(t, 0.75, *info.ChannelLimitAttempt.CostRatio, "cleared credential override uses the latest inherited default")
			editable, err := model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			editable.CostRatio, editable.CostRatioSet = common.GetPointer(0.4), true
			require.NoError(t, editable.Update())
			account.CostRatio = nil
			require.NoError(t, model.SaveSupplierAccount(&account, account.Revision))
			require.NoError(t, PrepareChannelLimitAttempt(info, channel))
			PrepareOverviewAttempt(ctx, info, channel)
			assert.Equal(t, 0.4, *info.ChannelLimitAttempt.CostRatio, "supplier defaults cannot erase an administrator's channel override")
			assert.Equal(t, editable.CostVersionID, info.ChannelLimitAttempt.CostVersionID)
			assert.Equal(t, info.ChannelLimitAttempt.CostVersionID, info.OverviewAttempt.CostVersionID)
			state, err = model.GetChannelLimitState(channel.Id, time.Now())
			require.NoError(t, err)
			assert.True(t, state.Config.BudgetReady, "a known channel override remains ready without a supplier default")

			// A pause committed during request inspection still prevents dispatch
			// and releases the unsent reservation without spending RPM.
			pausedRequest := httptest.NewRequest("POST", "https://upstream.invalid/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`))
			pausedRequest.GetBody = func() (io.ReadCloser, error) {
				if err := db.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("supplier_paused", true).Error; err != nil {
					return nil, err
				}
				return io.NopCloser(strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`)), nil
			}
			assert.ErrorContains(t, BeforeChannelUpstreamRequest(ctx, info, pausedRequest), "channel_unavailable")
			assert.False(t, info.ChannelLimitAttempt.Sent)
			state, err = model.GetChannelLimitState(channel.Id, time.Now())
			require.NoError(t, err)
			assert.Equal(t, int64(1), state.RPMUsed)
			assert.Zero(t, state.RPMReserved)
			require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("supplier_paused", false).Error)

			// EOF without the terminal event required by the protocol remains
			// unknown, retaining both TPM and a trusted partial cost lower bound.
			ctx, _ = gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
			require.NoError(t, PrepareChannelLimitAttempt(info, channel))
			request = httptest.NewRequest("POST", "https://upstream.invalid/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`))
			require.NoError(t, BeforeChannelUpstreamRequest(ctx, info, request))
			info.StreamStatus = relaycommon.NewStreamStatus()
			info.StreamStatus.RequireTerminal()
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
			base = 2000
			info.AnalyticsBilling = &relaycommon.OverviewBillingSnapshot{BaseQuota: &base, QuotaPerUnit: 500000, UsageSource: "upstream", InputTokens: 100, OutputTokens: 20}
			require.NoError(t, FinishChannelLimitAttempt(ctx, info, types.NewError(fmt.Errorf("missing terminal event"), types.ErrorCodeBadResponse)))
			persisted = model.ChannelLimitAttempt{}
			require.NoError(t, db.First(&persisted, "id = ?", info.ChannelLimitAttempt.ID).Error)
			assert.False(t, persisted.Completed)
			assert.False(t, persisted.CostActualKnown)
			assert.NotZero(t, persisted.ReconcileUntilMS)
			assert.Equal(t, int64(1600000), persisted.HeldUSDNano)
			ctx.Request = httptest.NewRequest("POST", "/mj/submit/imagine", strings.NewReader(`{}`))
			ctx.Set("channel_id", channel.Id)
			mjResponse, _, err := DoMidjourneyHttpRequest(ctx, time.Second, "https://upstream.invalid/mj/submit/imagine")
			assert.ErrorContains(t, err, "protocol_limit_unsupported", "the legacy asynchronous transport cannot bypass supplier/limit admission")
			assert.Equal(t, 503, mjResponse.StatusCode)
		})
	}
}
