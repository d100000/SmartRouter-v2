package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
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

func setupChannelLimitRelayDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "limits.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := db.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	previousDB, previousType, previousCache := model.DB, common.MainDatabaseType(), common.MemoryCacheEnabled
	model.DB, common.MemoryCacheEnabled = db, false
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		model.DB, common.MemoryCacheEnabled = previousDB, previousCache
		common.SetMainDatabaseType(previousType)
		_ = connection.Close()
	})
	require.NoError(t, db.AutoMigrate(&model.Channel{}))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(db))
	return db
}

func channelLimitRelayContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c
}

// The ledger is the channel's procurement cost evidence, so a channel without
// any configured limit still records every upstream request.
func TestChannelWithoutLimitsStillRecordsEachRequest(t *testing.T) {
	db := setupChannelLimitRelayDB(t)
	platform := &model.Channel{Name: "platform", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled}
	require.NoError(t, db.Create(platform).Error)
	require.NoError(t, model.EnsureChannelLimitTracking(platform.Id, true, time.Now()))
	newInfo := func(channel *model.Channel) *relaycommon.RelayInfo {
		return &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: types.RelayFormatOpenAI,
			ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id, ChannelType: channel.Type}}
	}

	c := channelLimitRelayContext()
	info := newInfo(platform)
	require.NoError(t, PrepareChannelLimitAttempt(info, platform))
	require.NotNil(t, info.ChannelLimitAttempt)
	require.NoError(t, BeforeChannelUpstreamRequest(c, info, nil))
	assert.True(t, info.ChannelLimitAttempt.Sent)
	assert.ErrorIs(t, BeforeChannelUpstreamRequest(c, info, nil), model.ErrChannelAttemptDuplicate, "one send per recorded attempt")
	require.NoError(t, FinishChannelLimitAttempt(c, info, nil))

	var recorded model.ChannelLimitAttempt
	require.NoError(t, db.First(&recorded, "id = ?", info.ChannelLimitAttempt.ID).Error)
	assert.Equal(t, platform.Id, recorded.ChannelID)
	assert.NotZero(t, recorded.SentAtMS)
	assert.True(t, recorded.Completed, "the request is settled in the ledger")

	// A type with no relay adaptor sends nothing upstream and records nothing.
	other := &model.Channel{Name: "other-transport", Type: constant.ChannelTypeAIProxyLibrary, Status: common.ChannelStatusEnabled}
	require.NoError(t, db.Create(other).Error)
	info = newInfo(other)
	require.NoError(t, PrepareChannelLimitAttempt(info, other))
	assert.Nil(t, info.ChannelLimitAttempt)
	require.NoError(t, BeforeChannelUpstreamRequest(c, info, nil))
}

func TestChannelLimitRelayErrorSeparatesInfrastructureFailures(t *testing.T) {
	c := channelLimitRelayContext()
	c.Set("channel_id", 7)
	failure := ChannelLimitRelayError(errors.New("dial tcp 10.0.0.5:5432: connection refused"), c)
	assert.Nil(t, LocalChannelLimitError(failure), "a database failure is not the channel's refusal")
	assert.True(t, IsChannelLimitInfrastructureError(failure))
	assert.Equal(t, http.StatusServiceUnavailable, failure.StatusCode)
	assert.True(t, types.IsSkipRetryError(failure))
	assert.NotContains(t, failure.Error(), "10.0.0.5")

	for _, tc := range []struct {
		err    error
		status int
	}{
		{&model.ChannelLimitError{Reason: "rpm_limited"}, http.StatusTooManyRequests},
		{&model.ChannelLimitError{Reason: "channel_unavailable"}, http.StatusServiceUnavailable},
		{model.ErrChannelAttemptDuplicate, http.StatusServiceUnavailable},
		{model.ErrChannelLimitAccounting, http.StatusServiceUnavailable},
	} {
		refusal := ChannelLimitRelayError(tc.err, c)
		require.NotNil(t, LocalChannelLimitError(refusal), tc.err.Error())
		assert.False(t, IsChannelLimitInfrastructureError(refusal))
		assert.Equal(t, tc.status, refusal.StatusCode)
	}

	// A failing ledger read during preparation surfaces as an infrastructure error.
	db := setupChannelLimitRelayDB(t)
	channel := &model.Channel{Name: "platform", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled}
	require.NoError(t, db.Create(channel).Error)
	connection, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, connection.Close())
	info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: types.RelayFormatOpenAI, ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id}}
	prepareErr := PrepareChannelLimitAttempt(info, channel)
	require.Error(t, prepareErr)
	assert.True(t, IsChannelLimitInfrastructureError(ChannelLimitRelayError(prepareErr, c)))
}

func TestSettleTaskChannelSnapshotToleratesClockSkew(t *testing.T) {
	db := setupChannelLimitRelayDB(t)
	channel := &model.Channel{Name: "task", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, OwnerSupplierID: "supplier"}
	require.NoError(t, db.Create(channel).Error)
	require.NoError(t, model.EnsureChannelLimitTracking(channel.Id, true, time.Time{}))
	require.NoError(t, model.DispatchChannelAttempt(model.ChannelAttemptReservation{ID: "task-attempt", ChannelID: channel.Id}))
	databaseNow, err := model.ChannelLimitDatabaseTime(db)
	require.NoError(t, err)

	// The gateway clock runs three seconds ahead of the database.
	observedAt := databaseNow.Unix() + 3
	snapshot := &relaycommon.TaskChannelSnapshot{AttemptID: "task-attempt", ChannelID: channel.Id,
		Observation: relaycommon.TaskChannelObservation{Status: "running", Authoritative: true, ObservedAt: observedAt}}
	require.NoError(t, SettleTaskChannelSnapshot(context.Background(), snapshot, 2, databaseNow))
	var attempt model.ChannelLimitAttempt
	require.NoError(t, db.First(&attempt, "id = ?", "task-attempt").Error)
	assert.Equal(t, observedAt*1000+60_000, attempt.ExecutionLeaseMS, "the running confirmation renews the lease")

	// A correction for an attempt pruned after its retention window is dropped.
	pruned := &relaycommon.TaskChannelSnapshot{AttemptID: "pruned-attempt", ChannelID: channel.Id,
		Observation: relaycommon.TaskChannelObservation{Status: "succeeded", Authoritative: true, ObservedAt: observedAt}}
	assert.NoError(t, SettleTaskChannelSnapshot(context.Background(), pruned, 3, databaseNow))
}

// setupChannelLimitLedgerDB opens the procurement ledger on one dialect of the
// matrix; MySQL and PostgreSQL run when TEST_MYSQL_DSN / TEST_POSTGRES_DSN are set.
func setupChannelLimitLedgerDB(t *testing.T, dialect string) *gorm.DB {
	t.Helper()
	var driver gorm.Dialector
	switch dialect {
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
	default:
		driver = sqlite.Open(filepath.Join(t.TempDir(), "ledger.db") + "?_pragma=busy_timeout(5000)")
	}
	db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "cledger_test_"}, Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	connection, err := db.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	previousDB, previousType, previousCache := model.DB, common.MainDatabaseType(), common.MemoryCacheEnabled
	model.DB, common.MemoryCacheEnabled = db, false
	common.SetMainDatabaseType(common.DatabaseType(dialect))
	t.Cleanup(func() {
		_ = db.Migrator().DropTable(&model.ChannelLimitEvent{}, &model.ChannelLimitAttempt{}, &model.ChannelDailyBudget{}, &model.ChannelLimitConfig{}, &model.Channel{})
		model.DB, common.MemoryCacheEnabled = previousDB, previousCache
		common.SetMainDatabaseType(previousType)
		_ = connection.Close()
	})
	_ = db.Migrator().DropTable(&model.ChannelLimitEvent{}, &model.ChannelLimitAttempt{}, &model.ChannelDailyBudget{}, &model.ChannelLimitConfig{}, &model.Channel{})
	require.NoError(t, db.AutoMigrate(&model.Channel{}))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(db))
	return db
}

// Every billable channel type must reach the procurement ledger. A type is
// either admitted through the metered transport (doRequest or an explicit
// BeforeChannelUpstream in its adaptor) or listed here with the reason it
// never sends billable work upstream.
func TestEveryBillableChannelTypeRecordsLedgerAttempt(t *testing.T) {
	nonBillable := map[int]string{
		constant.ChannelTypeUnknown:        "placeholder type; no channel is created with it",
		constant.ChannelTypeAIProxyLibrary: "no relay adaptor: requests fail before any upstream send",
	}
	db := setupChannelLimitRelayDB(t)
	for channelType := range constant.ChannelTypeNames {
		name := constant.GetChannelTypeName(channelType)
		if reason, ok := nonBillable[channelType]; ok {
			assert.False(t, channelUsesMeteredHTTP(channelType), "%s is classified non-billable: %s", name, reason)
			continue
		}
		require.True(t, channelUsesMeteredHTTP(channelType), "billable channel type %d (%s) records no ledger attempt", channelType, name)
		channel := &model.Channel{Name: name, Type: channelType, Status: common.ChannelStatusEnabled}
		require.NoError(t, db.Create(channel).Error)
		info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: types.RelayFormatOpenAI,
			ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id, ChannelType: channelType}}
		require.NoError(t, PrepareChannelLimitAttempt(info, channel), name)
		assert.NotNil(t, info.ChannelLimitAttempt, "%s prepares a ledger attempt", name)
	}
}

// The ledger records what ran: the amount the customer settlement computed,
// zero for an upstream rejection, and an unknown (counted) cost otherwise.
func TestFinishChannelLimitAttemptRecordsWhatRan(t *testing.T) {
	ratio := 0.5
	base := 1000.0
	const quotaPerUnit = 500000.0
	// 1000 quota at a 0.5 cost ratio and 500000 quota per USD.
	const settledCost = int64(1_000_000)
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := setupChannelLimitLedgerDB(t, dialect)
			for index, tc := range []struct {
				name        string
				status      int
				apiErr      *types.NewAPIError
				billing     *relaycommon.OverviewBillingSnapshot
				disconnect  bool
				wantCost    *int64
				wantUnknown bool
			}{
				{name: "fixed price with local estimate usage", status: http.StatusOK,
					billing:  &relaycommon.OverviewBillingSnapshot{BaseQuota: &base, QuotaPerUnit: quotaPerUnit, UsageSource: "local_estimate"},
					wantCost: common.GetPointer(settledCost)},
				{name: "non-network error after 2xx with a computed amount", status: http.StatusOK,
					apiErr:   types.NewError(errors.New("response rejected"), types.ErrorCodeBadResponse),
					billing:  &relaycommon.OverviewBillingSnapshot{BaseQuota: &base, QuotaPerUnit: quotaPerUnit, UsageSource: "upstream", InputTokens: 10, OutputTokens: 5},
					wantCost: common.GetPointer(settledCost)},
				{name: "client disconnect after a settled response", status: http.StatusOK, disconnect: true,
					billing:  &relaycommon.OverviewBillingSnapshot{BaseQuota: &base, QuotaPerUnit: quotaPerUnit, UsageSource: "upstream", InputTokens: 10, OutputTokens: 5},
					wantCost: common.GetPointer(settledCost)},
				{name: "upstream rejection without usage", status: http.StatusTooManyRequests,
					apiErr:   types.NewErrorWithStatusCode(errors.New("rate limited"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests),
					wantCost: common.GetPointer(int64(0))},
				{name: "error after 2xx without any computed amount", status: http.StatusOK,
					apiErr:      types.NewError(errors.New("broken body"), types.ErrorCodeBadResponseBody),
					wantUnknown: true},
				{name: "success without any computed amount", status: http.StatusOK, wantUnknown: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					channel := &model.Channel{Name: tc.name, Type: constant.ChannelTypeAli, Status: common.ChannelStatusEnabled}
					require.NoError(t, db.Create(channel).Error)
					require.NoError(t, model.EnsureChannelLimitTracking(channel.Id, true, time.Time{}))
					c := channelLimitRelayContext()
					requestContext, cancel := context.WithCancel(context.Background())
					defer cancel()
					c.Request = c.Request.WithContext(requestContext)
					info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: types.RelayFormatOpenAI,
						ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id, ChannelType: channel.Type}}
					// A per-call price gives the dispatch a known hold to release.
					info.PriceData.UsePrice, info.PriceData.ModelPrice = true, 0.002
					require.NoError(t, PrepareChannelLimitAttempt(info, channel))
					info.ChannelLimitAttempt.CostRatio = &ratio
					require.NoError(t, BeforeChannelUpstreamRequest(c, info, nil))
					ObserveChannelLimitUpstreamStatus(c, info, tc.status)
					state, err := model.GetChannelLimitState(channel.Id, time.Time{})
					require.NoError(t, err)
					require.Equal(t, settledCost, state.DailyHeldNano, "case %d holds its estimate", index)

					info.AnalyticsBilling = tc.billing
					if tc.disconnect {
						cancel()
					}
					require.NoError(t, FinishChannelLimitAttempt(c, info, tc.apiErr))

					var attempt model.ChannelLimitAttempt
					require.NoError(t, db.First(&attempt, "id = ?", info.ChannelLimitAttempt.ID).Error)
					state, err = model.GetChannelLimitState(channel.Id, time.Time{})
					require.NoError(t, err)
					if tc.wantUnknown {
						assert.False(t, attempt.CostActualKnown)
						assert.EqualValues(t, 1, state.UnknownCosts, "an unknown cost never reads as a complete day")
						assert.Equal(t, settledCost, state.DailyHeldNano, "its estimate stays held as a lower bound")
						return
					}
					assert.True(t, attempt.Completed)
					assert.True(t, attempt.CostActualKnown)
					assert.Equal(t, *tc.wantCost, attempt.ActualUSDNano)
					assert.Equal(t, *tc.wantCost, state.DailyUsedNano)
					assert.Zero(t, state.DailyHeldNano, "the hold is released")
					assert.Zero(t, state.UnknownCosts)
				})
			}
		})
	}
}

// A provider chain records each physical request. Intermediate steps cost
// nothing, the final step carries the logical request's full cost, and a
// follow-up step is never refused for capacity once the first step ran.
func TestProviderPluginChainSettlesWhatRan(t *testing.T) {
	ratio := 0.5
	base := 1000.0
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := setupChannelLimitLedgerDB(t, dialect)
			channel := &model.Channel{Name: "plugin", Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled}
			require.NoError(t, db.Create(channel).Error)
			require.NoError(t, model.EnsureChannelLimitTracking(channel.Id, true, time.Time{}))
			_, err := model.UpdateChannelLimits(channel.Id, 1, model.ChannelLimitSettings{RPM: 1, ReconcileSeconds: 300}, false, time.Time{})
			require.NoError(t, err)
			c := channelLimitRelayContext()
			info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: types.RelayFormatOpenAI,
				ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id, ChannelType: channel.Type}}
			require.NoError(t, PrepareChannelEventAttempt(info, channel))
			info.ChannelLimitAttempt.CostRatio = &ratio
			require.NoError(t, BeforeChannelUpstreamRequest(c, info, nil))
			first := info.ChannelLimitAttempt.ID
			ObserveChannelLimitUpstreamStatus(c, info, http.StatusAccepted)

			// An unrelated request on the saturated channel is still limited.
			other := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: types.RelayFormatOpenAI,
				ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id, ChannelType: channel.Type}}
			require.NoError(t, PrepareChannelEventAttempt(other, channel))
			var limited *model.ChannelLimitError
			require.ErrorAs(t, BeforeChannelUpstreamRequest(channelLimitRelayContext(), other, nil), &limited)
			assert.Equal(t, "rpm_limited", limited.Reason)

			// Polling and a refreshed-credential resend follow the first step.
			for range 2 {
				require.NoError(t, PrepareNextPluginRelayAttempt(info))
				require.NoError(t, BeforeChannelUpstreamRequest(c, info, nil), "a follow-up step is not refused for capacity")
			}
			info.AnalyticsBilling = &relaycommon.OverviewBillingSnapshot{BaseQuota: &base, QuotaPerUnit: 500000, UsageSource: "upstream", InputTokens: 10, OutputTokens: 5}
			require.NoError(t, FinishChannelLimitAttempt(c, info, nil))

			var attempts []model.ChannelLimitAttempt
			require.NoError(t, db.Where("channel_id = ?", channel.Id).Order("sent_at_ms, id").Find(&attempts).Error)
			require.Len(t, attempts, 3, "every physical request is recorded")
			var total int64
			for _, attempt := range attempts {
				assert.True(t, attempt.Completed)
				assert.True(t, attempt.CostActualKnown)
				if attempt.ID == first {
					assert.Zero(t, attempt.ActualUSDNano)
				}
				total += attempt.ActualUSDNano
			}
			assert.EqualValues(t, 1_000_000, total, "the records sum to the logical request's cost")
			state, err := model.GetChannelLimitState(channel.Id, time.Time{})
			require.NoError(t, err)
			assert.EqualValues(t, 1_000_000, state.DailyUsedNano)
			assert.EqualValues(t, 3, state.RPMUsed)
			assert.Zero(t, state.UnknownCosts)
		})
	}
}

// A failed task without vendor usage settles at zero and releases its hold.
// An accepted submission that never gets a task row is not left to lapse.
func TestTaskChannelAttemptsSettleWithoutDanglingCost(t *testing.T) {
	ratio := 0.5
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := setupChannelLimitLedgerDB(t, dialect)
			for _, tc := range []struct {
				name        string
				settle      func(c *gin.Context, info *relaycommon.RelayInfo)
				wantCost    int64
				wantUnknown bool
			}{
				{name: "failed task settles at zero", settle: func(c *gin.Context, info *relaycommon.RelayInfo) {
					ObserveTaskChannelSnapshot(info.TaskChannelSnapshot, &relaycommon.TaskInfo{Status: string(model.TaskStatusFailure)}, true)
					require.NoError(t, SettleTaskChannelSnapshot(context.Background(), info.TaskChannelSnapshot, 1, time.Time{}))
				}},
				{name: "immediate success without a task row settles its frozen price", settle: func(c *gin.Context, info *relaycommon.RelayInfo) {
					SettleUnpersistedTaskChannelAttempt(c, info, &relaycommon.TaskInfo{Status: string(model.TaskStatusSuccess)})
				}, wantCost: 2_000_000},
				{name: "accepted job without a task row holds its frozen price as unknown", settle: func(c *gin.Context, info *relaycommon.RelayInfo) {
					SettleUnpersistedTaskChannelAttempt(c, info, &relaycommon.TaskInfo{Status: string(model.TaskStatusSubmitted)})
				}, wantCost: 2_000_000, wantUnknown: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					channel := &model.Channel{Name: tc.name, Type: constant.ChannelTypeKling, Status: common.ChannelStatusEnabled}
					require.NoError(t, db.Create(channel).Error)
					require.NoError(t, model.EnsureChannelLimitTracking(channel.Id, true, time.Time{}))
					c := channelLimitRelayContext()
					info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: types.RelayFormatOpenAI,
						ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id, ChannelType: channel.Type}}
					info.PriceData.UsePrice, info.PriceData.ModelPrice = true, 0.004
					require.NoError(t, PrepareChannelEventAttempt(info, channel))
					info.ChannelLimitAttempt.CostRatio = &ratio
					require.NoError(t, AdmitChannelEvent(c, info, nil, nil, nil))
					info.TaskChannelSnapshot = &relaycommon.TaskChannelSnapshot{AttemptID: info.ChannelLimitAttempt.ID, ChannelID: channel.Id,
						CostRatio: &ratio, QuotaPerUnit: 500000, ModelPrice: 0.004, UsePrice: true}
					info.ChannelLimitAttempt.Finished = true // the submission hook already ran
					tc.settle(c, info)

					var attempt model.ChannelLimitAttempt
					require.NoError(t, db.First(&attempt, "id = ?", info.ChannelLimitAttempt.ID).Error)
					state, err := model.GetChannelLimitState(channel.Id, time.Time{})
					require.NoError(t, err)
					if tc.wantUnknown {
						assert.False(t, attempt.Completed)
						assert.NotZero(t, attempt.ReconcileStartedMS, "explicitly marked unknown, not left to lapse")
						assert.EqualValues(t, 1, state.UnknownCosts)
						assert.Equal(t, tc.wantCost, state.DailyHeldNano)
						return
					}
					assert.True(t, attempt.Completed)
					assert.True(t, attempt.CostActualKnown)
					assert.Equal(t, tc.wantCost, attempt.ActualUSDNano)
					assert.Zero(t, state.DailyHeldNano)
					assert.Zero(t, state.UnknownCosts)
				})
			}
		})
	}
}
