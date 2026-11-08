package service

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestChannelFundingClassification(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		provider, status          int
		code, kind, message, want string
	}{
		{"DeepSeek original status", constant.ChannelTypeDeepSeek, 402, "", "", "", "balance_insufficient"},
		{"unknown payment required", 1, 402, "", "", "Payment required", "suspected"},
		{"known balance", 1, 429, "credit_balance_exhausted", "", "", "balance_insufficient"},
		{"known spend limit", 1, 429, "org_spend_limit_exceeded", "", "", "quota_limited"},
		{"Claude account spend", 14, 400, "enforced_spend_limit_reached", "invalid_request_error", "", "quota_limited"},
		{"Claude balance", 14, 400, "", "invalid_request_error", "Your credit balance is too low to access the API", "balance_insufficient"},
		{"explicit monthly limit", 14, 429, "", "rate_limit_error", "Your account has reached its monthly spending limit", "quota_limited"},
		{"limit guidance is ambiguous", 1, 429, "", "", "Quota exceeded, review your monthly spending limit settings", "suspected"},
		{"Gemini rate quota", 24, 429, "resource_exhausted", "", "Quota exceeded: requests per minute", ""},
		{"Gemini unspecified quota", 24, 429, "resource_exhausted", "", "Quota exceeded", "suspected"},
		{"rate limited", 1, 429, "rate_limit_exceeded", "", "quota reached", ""},
		{"authentication", 1, 401, "", "", "billing account rejected", ""},
		{"local balance", 1, 403, "insufficient_user_quota", "new_api_error", "余额不足", ""},
		{"local network", 1, 502, "do_request_failed", "", "billing upstream timeout", ""},
		{"ambiguous quota", 1, 429, "insufficient_quota", "", "quota exceeded", "suspected"},
		{"request budget", 1, 402, "insufficient_balance", "", "This request requires more credits; reduce max_tokens", "suspected"},
		{"model budget", 1, 429, "", "", "quota for model exceeded", "suspected"},
		{"ordinary server failure", 1, 503, "", "", "Upstream unavailable", ""},
		{"unrelated request validation", 1, 400, "invalid_request", "", "Unsupported parameter for this request", ""},
		{"rate limit with billing guidance", 1, 429, "", "", "Rate limit reached. Check for insufficient balance in billing settings", ""},
		{"rate limit type with billing guidance", 1, 429, "", "rate_limit_error", "Check for insufficient balance in billing settings", ""},
		{"model spending limit", 1, 429, "billing_hard_limit_reached", "", "Model spending limit reached", "suspected"},
		{"negated funding failure", 1, 500, "", "", "This failure is not due to insufficient balance", "suspected"},
		{"requested token budget", 1, 402, "insufficient_balance", "", "Balance is insufficient for requested tokens", "suspected"},
		{"authentication error type", 1, 403, "", "authentication_error", "Cannot check billing or insufficient balance until authenticated", ""},
		{"gateway timeout", 1, 504, "", "", "Unable to determine if there is insufficient balance", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ClassifyChannelFundingError(tc.provider, tc.status, tc.code, tc.kind, tc.message).Category)
		})
	}
}

func fundingDatabase(t *testing.T, kind, dsn string) *gorm.DB {
	t.Helper()
	var driver gorm.Dialector
	switch kind {
	case "sqlite":
		driver = sqlite.Open(dsn)
	case "mysql":
		driver = mysql.Open(dsn)
	case "postgres":
		driver = postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
	}
	db, err := gorm.Open(driver, &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	beforeDB, beforeLog, beforeType, beforeCache := model.DB, model.LOG_DB, common.MainDatabaseType(), common.MemoryCacheEnabled
	model.DB = db
	common.SetMainDatabaseType(common.DatabaseType(kind))
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		model.DB, model.LOG_DB = beforeDB, beforeLog
		common.SetMainDatabaseType(beforeType)
		common.MemoryCacheEnabled = beforeCache
		require.NoError(t, sqlDB.Close())
	})
	return db
}

func fundingSchema(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.User{}, &model.Task{}, &model.Midjourney{}, &model.ChannelFundingIncident{}, &model.ChannelFundingDelivery{}, &model.SystemTask{}, &model.SystemTaskLock{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(db))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(db))
}

func fundingIdentity(channel *model.Channel, index int) hosttypes.ChannelFundingAttempt {
	snapshot := channel.GetUpstreamCredentialSnapshot(index)
	return hosttypes.ChannelFundingAttempt{ChannelID: channel.Id, ChannelType: channel.Type, BindingID: snapshot.BindingID, VersionID: snapshot.CredentialVersionID, BindingActivatedAt: snapshot.BindingActivatedAt, KeyIndex: index, Model: "test-model", Endpoint: "/v1/chat/completions", StartedAt: time.Now().UnixMilli(), Source: "http"}
}

func fundingEnabled(t *testing.T) {
	t.Helper()
	original := operation_setting.GetChannelFundingSetting()
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"channel_funding_setting.enabled": "true"}))
	beforeDisable, beforeEnable := common.AutomaticDisableChannelEnabled, common.AutomaticEnableChannelEnabled
	common.AutomaticDisableChannelEnabled, common.AutomaticEnableChannelEnabled = true, true
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled, common.AutomaticEnableChannelEnabled = beforeDisable, beforeEnable
		require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"channel_funding_setting.enabled": strconv.FormatBool(original.Enabled)}))
	})
}

func TestChannelFundingProtocolEvidence(t *testing.T) {
	db := fundingDatabase(t, "sqlite", filepath.Join(t.TempDir(), "protocol.db"))
	fundingSchema(t, db)
	fundingEnabled(t)
	for _, tc := range []struct {
		name, body, contentType string
		status                  int
		want                    string
	}{
		{"http_raw", `{"error":{"code":"credit_balance_exhausted","message":"insufficient balance secret-fixture"}}`, "application/json", 429, "balance_insufficient"},
		{"sse_200", "event: error\ndata: {\"error\":{\"type\":\"invalid_request_error\",\"message\":\"credit balance is too low\"}}\n\n", "text/event-stream", 200, "balance_insufficient"},
		{"ws_failed", `{"type":"response.failed","response":{"error":{"code":"project_spend_limit_exceeded","message":"spend limit"}}}`, "websocket", 0, "quota_limited"},
		{"realtime_done", `{"type":"response.done","response":{"status_details":{"error":{"code":"credit_balance_exhausted","message":"credits exhausted"}}}}`, "websocket", 0, "balance_insufficient"},
		{"model_text_is_not_evidence", `{"choices":[{"message":{"content":"insufficient balance"}}]}`, "application/json", 200, ""},
		{"rate_limit", `{"error":{"code":"rate_limit_exceeded","message":"quota per minute"}}`, "application/json", 429, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := model.Channel{Name: tc.name, Type: 1, Key: "secret-fixture", Status: 1, Models: "test-model", Group: "default"}
			require.NoError(t, channel.Insert())
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
			BeginChannelFundingAttempt(c, &channel, channel.Key, 0, "test-model")
			if tc.contentType == "websocket" {
				ObserveChannelFundingJSON(ChannelFundingAttempt(c), tc.status, []byte(tc.body))
			} else {
				response := &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{tc.contentType}}, Body: io.NopCloser(strings.NewReader(tc.body))}
				ObserveUpstreamResponse(c, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id}}, response)
				response.StatusCode = 502 // The observer must retain the pre-mapping status.
				_, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
			}
			var rows []model.ChannelFundingIncident
			require.NoError(t, db.Where("channel_id = ?", channel.Id).Find(&rows).Error)
			if tc.want == "" {
				assert.Empty(t, rows)
				return
			}
			require.Len(t, rows, 1)
			assert.Equal(t, tc.want, rows[0].Category)
			assert.Equal(t, tc.status, rows[0].StatusCode)
			assert.NotContains(t, rows[0].Evidence, channel.Key)
			assert.Equal(t, int64(1), rows[0].Hits)
		})
	}
	// An accepted task keeps the exact submission binding after key replacement.
	channel := model.Channel{Name: "task", Type: 1, Key: "task-key", Status: 1, Models: "test-model", Group: "default"}
	require.NoError(t, channel.Insert())
	identity := fundingIdentity(&channel, 0)
	task := model.Task{TaskID: "fixture-task", PrivateData: model.TaskPrivateData{FundingAttempt: &identity}}
	ObserveTaskFundingFailure(&task, "insufficient balance", "insufficient_balance")
	var row model.ChannelFundingIncident
	require.NoError(t, db.Where("channel_id = ?", channel.Id).First(&row).Error)
	channel.Key = "replacement-key"
	require.NoError(t, channel.Update())
	ObserveTaskFundingFailure(&task, "insufficient balance", "insufficient_balance")
	require.NoError(t, db.First(&row, row.ID).Error)
	assert.Equal(t, "dismissed", row.State)
	assert.Nil(t, row.ActiveKey)
}

func TestChannelFundingCompletedResponse(t *testing.T) {
	db := fundingDatabase(t, "sqlite", filepath.Join(t.TempDir(), "completed.db"))
	fundingSchema(t, db)
	fundingEnabled(t)
	for _, tc := range []struct {
		name, body, contentType string
		recovered               bool
	}{
		{"empty envelope", `{}`, "application/json", false},
		{"unfinished response", `{"object":"response","status":"incomplete"}`, "application/json", false},
		{"complete response", `{"object":"response","status":"completed"}`, "application/json", true},
		{"truncated stream", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", "text/event-stream", false},
		{"empty done stream", "data: [DONE]\n\n", "text/event-stream", false},
		{"complete stream", "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n", "text/event-stream", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := model.Channel{Name: tc.name, Type: 1, Key: "completion-key", Status: 1, Models: "test-model", Group: "default"}
			require.NoError(t, channel.Insert())
			incident, err := model.RecordChannelFundingFailure(fundingIdentity(&channel, 0), "balance_insufficient", "test", "insufficient_balance", "fixture", 402, true, 10)
			require.NoError(t, err)
			require.NoError(t, db.Model(incident).Update("last_failure_at", time.Now().Add(-time.Second).UnixMilli()).Error)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			BeginChannelFundingAttempt(c, &channel, channel.Key, 0, "test-model")
			response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{tc.contentType}}, Body: io.NopCloser(strings.NewReader(tc.body))}
			ObserveUpstreamResponse(c, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id}}, response)
			_, err = io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			ObserveChannelFundingResponseSuccess(c)
			require.NoError(t, db.First(incident, incident.ID).Error)
			assert.Equal(t, tc.recovered, incident.State == "resolved")
			stored, err := model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			assert.Equal(t, tc.recovered, stored.Status == common.ChannelStatusEnabled)
		})
	}
	channel := model.Channel{Name: "mapped-deepseek", Type: constant.ChannelTypeDeepSeek, Key: "mapped-key", Status: 1, Models: "test-model", Group: "default"}
	require.NoError(t, channel.Insert())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	BeginChannelFundingAttempt(c, &channel, "wrong-key", 0, "test-model")
	assert.Nil(t, ChannelFundingAttempt(c), "a key/index mismatch must not bind the wrong credential")
	BeginChannelFundingAttempt(c, &channel, channel.Key, 0, "test-model")
	response := &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"Capacity unavailable"}}`))}
	ObserveUpstreamResponse(c, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id}}, response)
	_, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	mapped := relaytypes.WithOpenAIError(relaytypes.OpenAIError{Message: "Capacity unavailable", Type: "upstream_error"}, 402)
	assert.False(t, ProcessChannelFundingError(c, mapped))
	var count int64
	require.NoError(t, db.Model(&model.ChannelFundingIncident{}).Where("channel_id = ?", channel.Id).Count(&count).Error)
	assert.Zero(t, count, "a downstream 402 mapping cannot become DeepSeek balance evidence")
}

func TestChannelFundingDatabaseMatrix(t *testing.T) {
	fundingEnabled(t)
	for _, kind := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			dsn := os.Getenv("TEST_FUNDING_" + strings.ToUpper(kind) + "_DSN")
			if kind == "sqlite" {
				dsn = filepath.Join(t.TempDir(), "funding.db") + "?_pragma=busy_timeout(10000)"
			} else if dsn == "" {
				t.Skip("disposable database DSN not configured")
			}
			db := fundingDatabase(t, kind, dsn)
			var version string
			query := "SELECT version()"
			if kind == "sqlite" {
				query = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(query).Scan(&version).Error)
			t.Logf("%s: %s", kind, version)
			// The supplied DSNs must point to dedicated disposable databases.
			for _, mode := range []string{"fresh", "released_rc47"} {
				t.Run(mode, func(t *testing.T) {
					require.NoError(t, db.Migrator().DropTable(&model.ChannelLimitEvent{}, &model.ChannelLimitAttempt{}, &model.ChannelDailyBudget{}, &model.ChannelLimitConfig{}, &model.ChannelFundingDelivery{}, &model.ChannelFundingIncident{}, &model.Task{}, &model.Midjourney{}, &model.Ability{}, &model.Channel{}, &model.User{}, &model.SystemTask{}, &model.SystemTaskLock{}, &model.ChannelCredentialBinding{}, &model.UpstreamCredentialVersion{}, &model.UpstreamCredential{}, &model.UpstreamSupplier{}, &model.UpstreamCredentialFingerprintSecret{}))
					if mode == "released_rc47" {
						require.NoError(t, db.AutoMigrate(&fundingReleasedChannel{}, &fundingReleasedMidjourney{}))
						require.NoError(t, db.Create(&fundingReleasedChannel{Id: 900, Name: "preserved release channel", Type: 1, Key: "released-key", Models: "test-model", Group: "default", UsedQuota: 1234, Status: 1}).Error)
						require.NoError(t, db.Create(&fundingReleasedMidjourney{Id: 900, MjId: "released-task", Status: "SUCCESS", Progress: "100%", Quota: 456}).Error)
					}
					fundingSchema(t, db)
					require.NoError(t, model.BackfillUpstreamCredentials())
					var migrationSQL bytes.Buffer
					migrationDB := db.Session(&gorm.Session{Logger: gormlogger.New(log.New(&migrationSQL, "", 0), gormlogger.Config{LogLevel: gormlogger.Info})})
					require.NoError(t, migrationDB.AutoMigrate(&model.Channel{}, &model.Task{}, &model.Midjourney{}, &model.ChannelCredentialBinding{}, &model.ChannelFundingIncident{}, &model.ChannelFundingDelivery{}))
					for _, line := range strings.Split(migrationSQL.String(), "\n") {
						if strings.Contains(strings.ToUpper(line), "ALTER TABLE") {
							t.Errorf("second migration must be stable: %s", line)
						}
					}
					if mode == "released_rc47" {
						preserved, err := model.GetChannelById(900, true)
						require.NoError(t, err)
						assert.Equal(t, int64(1234), preserved.UsedQuota)
						assert.Equal(t, "released-key", preserved.Key)
						var mj model.Midjourney
						require.NoError(t, db.First(&mj, 900).Error)
						assert.Equal(t, 456, mj.Quota)
						assert.Empty(t, mj.FundingAttemptData)
					}
					root := model.User{Username: "funding-root", Role: common.RoleRootUser, Status: common.UserStatusEnabled, AffCode: "funding-root"}
					require.NoError(t, db.Create(&root).Error)
					channel := model.Channel{Name: "multi-key", Type: 1, Key: "matrix-key-a\nmatrix-key-b", Status: 1, Models: "test-model", Group: "default", ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeyMode: constant.MultiKeyModePolling}}
					require.NoError(t, channel.Insert())
					identity := fundingIdentity(&channel, 0)
					// Concurrent independent processes exercise the database lock, not the Go mutex.
					var workers [2]*exec.Cmd
					var output [2]bytes.Buffer
					for i := range workers {
						worker := exec.Command(os.Args[0], "-test.run=^TestChannelFundingWorker$")
						worker.Env = append(os.Environ(), "FUNDING_WORKER_KIND="+kind, "FUNDING_WORKER_DSN="+dsn, fmt.Sprintf("FUNDING_WORKER_CHANNEL=%d", channel.Id))
						worker.Stdout = &output[i]
						worker.Stderr = &output[i]
						workers[i] = worker
						require.NoError(t, worker.Start())
					}
					for i, worker := range workers {
						require.NoError(t, worker.Wait(), output[i].String())
					}
					var incidents []model.ChannelFundingIncident
					require.NoError(t, db.Where("channel_id = ? AND active_key IS NOT NULL", channel.Id).Find(&incidents).Error)
					require.Len(t, incidents, 1)
					row := incidents[0]
					assert.Equal(t, int64(2), row.Hits)
					var deliveries []model.ChannelFundingDelivery
					require.NoError(t, db.Where("incident_id = ?", row.ID).Find(&deliveries).Error)
					require.Len(t, deliveries, 1)
					require.NoError(t, db.Model(&deliveries[0]).Updates(map[string]any{"status": "sending", "attempts": 1}).Error)
					require.NoError(t, model.FinishFundingDelivery(deliveries[0].ID, 1, "sent", "", 0, true))
					require.NoError(t, db.First(&deliveries[0], deliveries[0].ID).Error)
					assert.Equal(t, "sent", deliveries[0].Status)
					expired := model.ChannelFundingDelivery{IncidentID: row.ID, ChannelID: channel.Id, UserID: root.Id, DedupKey: "expired-matrix-reminder", Kind: "reminder", Status: "sending", Attempts: 5, NextAttemptAt: common.GetTimestamp() - 1}
					require.NoError(t, db.Create(&expired).Error)
					require.NoError(t, RunChannelFundingNotifications(context.Background()))
					require.NoError(t, db.First(&expired, expired.ID).Error)
					assert.Equal(t, "failed", expired.Status)
					duplicate := row
					duplicate.ID = 0
					assert.Error(t, db.Create(&duplicate).Error, "database unique constraint must reject duplicate active events")
					current, err := model.GetChannelById(channel.Id, true)
					require.NoError(t, err)
					assert.Equal(t, 1, current.Status)
					assert.Equal(t, 3, current.ChannelInfo.MultiKeyStatusList[0])
					assert.Zero(t, current.ChannelInfo.MultiKeyStatusList[1])
					assert.ErrorIs(t, model.ResolveChannelFunding(row.ID, row.Revision, row.LastFailureAt, false, false, 0, "late success"), model.ErrFundingConflict)
					// Reordering must retain identity and disabled ownership.
					current.Key = "matrix-key-b\nmatrix-key-a"
					require.NoError(t, current.Update())
					require.NoError(t, model.ResolveChannelFunding(row.ID, row.Revision, row.LastFailureAt+1, true, true, 0, "probe"))
					var recovery []model.ChannelFundingDelivery
					require.NoError(t, db.Where("incident_id = ? AND kind = ?", row.ID, "recovery").Find(&recovery).Error)
					require.Len(t, recovery, 1, "the notified recipient must receive a persisted recovery delivery")
					current, err = model.GetChannelById(channel.Id, true)
					require.NoError(t, err)
					assert.Zero(t, current.ChannelInfo.MultiKeyStatusList[1])
					assert.Equal(t, 1, current.Status)
					_, err = model.RecordChannelFundingFailure(identity, "balance_insufficient", "test", "insufficient_balance", "late failure", 402, true, 10)
					assert.ErrorIs(t, err, model.ErrFundingConflict)
					require.NoError(t, db.First(&row, row.ID).Error)
					next := fundingIdentity(current, 1)
					next.StartedAt = row.ResolvedAfter + 1
					event, err := model.RecordChannelFundingFailure(next, "balance_insufficient", "test", "insufficient_balance", "new failure", 402, true, 10)
					require.NoError(t, err)
					require.True(t, model.UpdateChannelStatus(channel.Id, "", 2, "operator disabled"))
					require.NoError(t, model.ResolveChannelFunding(event.ID, event.Revision, event.LastFailureAt+1, true, false, 0, "recovered while manually disabled"))
					current, err = model.GetChannelById(channel.Id, true)
					require.NoError(t, err)
					assert.Equal(t, 2, current.Status)
					// Removing and reintroducing identical material changes its activation epoch.
					oldIdentity := fundingIdentity(current, 1)
					current.Key = "matrix-key-b"
					require.NoError(t, current.Update())
					current.Key = "matrix-key-b\nmatrix-key-a"
					require.NoError(t, current.Update())
					assert.Equal(t, oldIdentity.BindingID, current.GetUpstreamCredentialSnapshot(1).BindingID)
					_, err = model.RecordChannelFundingFailure(oldIdentity, "balance_insufficient", "test", "insufficient_balance", "detached response", 402, true, 10)
					assert.ErrorIs(t, err, model.ErrFundingConflict)
					require.NoError(t, model.AttachChannelFunding([]*model.Channel{current}))
					assert.Nil(t, current.Funding)
					var count int64
					require.NoError(t, model.ApplyChannelFundingFilter(db.Model(&model.Channel{}), "balance_insufficient").Count(&count).Error)
					assert.Zero(t, count)
					// Upgrading again retains resolved incidents and delivery history.
					fundingSchema(t, db)
					require.NoError(t, db.Model(&model.ChannelFundingIncident{}).Where("channel_id = ?", channel.Id).Count(&count).Error)
					assert.Equal(t, int64(2), count)
					fundingReviewDatabaseInvariants(t, db, kind, dsn, root.Id)
				})
			}
		})
	}
}

func TestChannelFundingWorker(t *testing.T) {
	kind := os.Getenv("FUNDING_WORKER_KIND")
	if kind == "" {
		t.Skip("subprocess fixture")
	}
	fundingDatabase(t, kind, os.Getenv("FUNDING_WORKER_DSN"))
	id, err := strconv.Atoi(os.Getenv("FUNDING_WORKER_CHANNEL"))
	require.NoError(t, err)
	channel, err := model.GetChannelById(id, true)
	require.NoError(t, err)
	switch os.Getenv("FUNDING_WORKER_ACTION") {
	case "enable":
		model.EnableChannelAutomatically(id, channel.Key)
		return
	case "claim":
		userID, err := strconv.Atoi(os.Getenv("FUNDING_WORKER_USER"))
		require.NoError(t, err)
		rows, err := model.ClaimFundingDeliveries(id, userID, "initial", common.GetTimestamp())
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(os.Getenv("FUNDING_WORKER_REPORT"), []byte(strconv.Itoa(len(rows))), 0600))
		return
	}
	_, err = model.RecordChannelFundingFailure(fundingIdentity(channel, 0), "balance_insufficient", "test", "insufficient_balance", "fixture", 402, true, 10)
	require.NoError(t, err)
}

func fundingReviewDatabaseInvariants(t *testing.T, db *gorm.DB, kind, dsn string, userID int) {
	t.Helper()
	channel := model.Channel{Name: "duplicate-key-slots", Type: 1, Key: "duplicate-key\nduplicate-key\nother-key", Status: 1, Models: "test-model", Group: "default", ChannelInfo: model.ChannelInfo{IsMultiKey: true}}
	require.NoError(t, channel.Insert())
	identity := fundingIdentity(&channel, 1)
	incident, err := model.RecordChannelFundingFailure(identity, "balance_insufficient", "test", "insufficient_balance", "fixture", 402, true, 10)
	require.NoError(t, err)
	stored, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Zero(t, stored.ChannelInfo.MultiKeyStatusList[0])
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[1])
	assert.False(t, model.EnableChannelAutomatically(channel.Id, "duplicate-key"))
	assert.Nil(t, FundingTaskAttempt(stored, "duplicate-key", nil), "duplicate material without an identity is ambiguous")
	poll := FundingTaskAttempt(stored, "duplicate-key", &model.Task{PrivateData: model.TaskPrivateData{FundingAttempt: &identity}})
	require.NotNil(t, poll)
	assert.Equal(t, identity.BindingID, poll.Identity.BindingID)
	_, _, err = model.ClaimChannelFundingProbe(incident.ID, incident.Revision, "first-probe")
	require.NoError(t, err)
	_, _, err = model.ClaimChannelFundingProbe(incident.ID, incident.Revision, "overlapping-probe")
	assert.ErrorIs(t, err, model.ErrFundingConflict)
	require.NoError(t, db.Model(incident).Update("probe_lease_until", 0).Error)
	_, _, err = model.ClaimChannelFundingProbe(incident.ID, incident.Revision, "replacement-probe")
	require.NoError(t, err)
	assert.ErrorIs(t, model.ResolveChannelFundingProbe(incident.ID, incident.Revision, incident.LastFailureAt+1, true, false, "first-probe"), model.ErrFundingConflict)
	require.NoError(t, model.ResolveChannelFundingProbe(incident.ID, incident.Revision, incident.LastFailureAt+1, true, false, "replacement-probe"))
	stored, err = model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Empty(t, stored.ChannelInfo.MultiKeyStatusList)

	// Two processes claim the same two-key notification batch. Exactly one
	// claims both rows; neither may split the group into separate emails.
	mailChannel := model.Channel{Name: "parallel-mail", Type: 1, Key: "mail-a\nmail-b", Status: 1, Models: "test-model", Group: "default", ChannelInfo: model.ChannelInfo{IsMultiKey: true}}
	require.NoError(t, mailChannel.Insert())
	for index := range 2 {
		_, err := model.RecordChannelFundingFailure(fundingIdentity(&mailChannel, index), "balance_insufficient", "test", "insufficient_balance", "fixture", 402, true, 10)
		require.NoError(t, err)
	}
	reports := runFundingReviewWorkers(t, kind, dsn, mailChannel.Id, userID, "claim", "claim")
	assert.ElementsMatch(t, []string{"0", "2"}, reports)
	var deliveries []model.ChannelFundingDelivery
	require.NoError(t, db.Where("channel_id = ?", mailChannel.Id).Order("id").Find(&deliveries).Error)
	require.Len(t, deliveries, 2)
	for _, row := range deliveries {
		assert.Equal(t, 1, row.Attempts)
		assert.Equal(t, "sending", row.Status)
	}
	// The first process lost its lease. Its late success or failure must not
	// acknowledge the retry now owned by another process.
	require.NoError(t, db.Model(&model.ChannelFundingDelivery{}).Where("channel_id = ?", mailChannel.Id).Update("next_attempt_at", 0).Error)
	reclaimed, err := model.ClaimFundingDeliveries(mailChannel.Id, userID, "initial", common.GetTimestamp())
	require.NoError(t, err)
	require.Len(t, reclaimed, 2)
	for _, status := range []string{"failed", "sent"} {
		require.NoError(t, model.FinishFundingDelivery(deliveries[0].ID, 1, status, "stale worker", 0, true))
	}
	var delivery model.ChannelFundingDelivery
	require.NoError(t, db.First(&delivery, deliveries[0].ID).Error)
	assert.Equal(t, "sending", delivery.Status)
	assert.Equal(t, 2, delivery.Attempts)
	incident = &model.ChannelFundingIncident{}
	require.NoError(t, db.First(incident, delivery.IncidentID).Error)
	require.NoError(t, model.ResolveChannelFunding(incident.ID, incident.Revision, incident.LastFailureAt+1, true, true, 0, "recovered during delivery"))
	require.NoError(t, model.FinishFundingDelivery(delivery.ID, 2, "sent", "", 0, true))
	require.NoError(t, model.FinishFundingDelivery(delivery.ID, 2, "failed", "late failure", 0, true))
	require.NoError(t, db.First(&delivery, delivery.ID).Error)
	assert.Equal(t, "sent", delivery.Status)
	var count int64
	require.NoError(t, db.Model(&model.ChannelFundingDelivery{}).Where("incident_id = ? AND kind = ?", incident.ID, "recovery").Count(&count).Error)
	assert.Equal(t, int64(1), count)
	blockedRecovery, err := model.ClaimFundingDeliveries(mailChannel.Id, userID, "recovery", common.GetTimestamp())
	require.NoError(t, err)
	assert.Empty(t, blockedRecovery, "recovery waits for every in-flight fault email in this group")
	require.NoError(t, model.FinishFundingDelivery(reclaimed[1].ID, reclaimed[1].Attempts, "sent", "", 0, true))
	recovery, err := model.ClaimFundingDeliveries(mailChannel.Id, userID, "recovery", common.GetTimestamp())
	require.NoError(t, err)
	require.Len(t, recovery, 1)
	require.NoError(t, db.First(incident, incident.ID).Error)
	identity = fundingIdentity(&mailChannel, 0)
	identity.StartedAt = incident.ResolvedAfter + 1
	_, err = model.RecordChannelFundingFailure(identity, "balance_insufficient", "test", "insufficient_balance", "a new funding failure", 402, true, 10)
	require.NoError(t, err)
	var obsolete model.ChannelFundingDelivery
	require.NoError(t, db.First(&obsolete, recovery[0].ID).Error)
	assert.Equal(t, "cancelled", obsolete.Status, "new debt invalidates a queued recovery notice")
	blockedInitial, err := model.ClaimFundingDeliveries(mailChannel.Id, userID, "initial", common.GetTimestamp())
	require.NoError(t, err)
	assert.Empty(t, blockedInitial, "a cancelled in-flight send still holds ordering until acknowledged")
	require.NoError(t, model.FinishFundingDelivery(obsolete.ID, obsolete.Attempts, "failed", "cancelled before sending", 0, true))
	newInitial, err := model.ClaimFundingDeliveries(mailChannel.Id, userID, "initial", common.GetTimestamp())
	require.NoError(t, err)
	require.Len(t, newInitial, 1)
	require.NoError(t, model.FinishFundingDelivery(newInitial[0].ID, newInitial[0].Attempts, "sent", "", 0, true))

	raceChannel := model.Channel{Name: "health-funding-race", Type: 1, Key: "race-key", Status: 1, Models: "test-model", Group: "default"}
	require.NoError(t, raceChannel.Insert())
	runFundingReviewWorkers(t, kind, dsn, raceChannel.Id, userID, "enable", "failure")
	stored, err = model.GetChannelById(raceChannel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
	// Deleting a channel archives its evidence and cancels its pending sends.
	require.NoError(t, db.Delete(&raceChannel).Error)
	require.NoError(t, model.QueueFundingMaintenance(24))
	var archived model.ChannelFundingIncident
	require.NoError(t, db.Where("channel_id = ?", raceChannel.Id).First(&archived).Error)
	assert.Nil(t, archived.ActiveKey)
	assert.Equal(t, "dismissed", archived.State)
	assert.Positive(t, archived.ResolvedAfter)
	require.NoError(t, db.Model(&model.ChannelFundingDelivery{}).Where("channel_id = ? AND status <> ?", raceChannel.Id, "cancelled").Count(&count).Error)
	assert.Zero(t, count)
}

func runFundingReviewWorkers(t *testing.T, kind, dsn string, channelID, userID int, actions ...string) []string {
	t.Helper()
	workers := make([]*exec.Cmd, len(actions))
	outputs := make([]bytes.Buffer, len(actions))
	reports := make([]string, len(actions))
	paths := make([]string, len(actions))
	for i, action := range actions {
		paths[i] = filepath.Join(t.TempDir(), "claim-count")
		worker := exec.Command(os.Args[0], "-test.run=^TestChannelFundingWorker$")
		worker.Env = append(os.Environ(), "FUNDING_WORKER_KIND="+kind, "FUNDING_WORKER_DSN="+dsn, fmt.Sprintf("FUNDING_WORKER_CHANNEL=%d", channelID), fmt.Sprintf("FUNDING_WORKER_USER=%d", userID), "FUNDING_WORKER_ACTION="+action, "FUNDING_WORKER_REPORT="+paths[i])
		worker.Stdout, worker.Stderr = &outputs[i], &outputs[i]
		workers[i] = worker
		require.NoError(t, worker.Start())
	}
	for i, worker := range workers {
		require.NoError(t, worker.Wait(), outputs[i].String())
		if actions[i] == "claim" {
			data, err := os.ReadFile(paths[i])
			require.NoError(t, err)
			reports[i] = string(data)
		}
	}
	return reports
}

// Schema fixtures copied from v1.0.0-rc.47, without unrelated methods.

type fundingReleasedChannel struct {
	Id                 int     `json:"id"`
	Type               int     `json:"type" gorm:"default:0"`
	Key                string  `json:"key" gorm:"not null"`
	OpenAIOrganization *string `json:"openai_organization"`
	TestModel          *string `json:"test_model"`
	Status             int     `json:"status" gorm:"default:1"`
	Name               string  `json:"name" gorm:"index"`
	Weight             *uint   `json:"weight" gorm:"default:0"`
	CreatedTime        int64   `json:"created_time" gorm:"bigint"`
	TestTime           int64   `json:"test_time" gorm:"bigint"`
	ResponseTime       int     `json:"response_time"` // in milliseconds
	BaseURL            *string `json:"base_url" gorm:"column:base_url;default:''"`
	Other              string  `json:"other"`
	Balance            float64 `json:"balance"` // in USD
	BalanceUpdatedTime int64   `json:"balance_updated_time" gorm:"bigint"`
	Models             string  `json:"models"`
	Group              string  `json:"group" gorm:"type:varchar(64);default:'default'"`
	UsedQuota          int64   `json:"used_quota" gorm:"bigint;default:0"`
	ModelMapping       *string `json:"model_mapping" gorm:"type:text"`
	//MaxInputTokens     *int    `json:"max_input_tokens" gorm:"default:0"`
	StatusCodeMapping *string `json:"status_code_mapping" gorm:"type:varchar(1024);default:''"`
	Priority          *int64  `json:"priority" gorm:"bigint;default:0"`
	AutoBan           *int    `json:"auto_ban" gorm:"default:1"`
	OtherInfo         string  `json:"other_info"`
	Tag               *string `json:"tag" gorm:"index"`
	Setting           *string `json:"setting" gorm:"type:text"` // 渠道额外设置
	ParamOverride     *string `json:"param_override" gorm:"type:text"`
	HeaderOverride    *string `json:"header_override" gorm:"type:text"`
	Remark            *string `json:"remark" gorm:"type:varchar(255)" validate:"max=255"`
	// add after v0.8.5
	ChannelInfo model.ChannelInfo `json:"channel_info" gorm:"type:json"`

	OtherSettings string `json:"settings" gorm:"column:settings"` // 其他设置，存储azure版本等不需要检索的信息，详见dto.ChannelOtherSettings

	// cache info
	Keys []string `json:"-" gorm:"-"`
}

func (fundingReleasedChannel) TableName() string { return "channels" }

type fundingReleasedMidjourney struct {
	Id          int    `json:"id"`
	Code        int    `json:"code"`
	UserId      int    `json:"user_id" gorm:"index"`
	Action      string `json:"action" gorm:"type:varchar(40);index"`
	MjId        string `json:"mj_id" gorm:"index"`
	Prompt      string `json:"prompt"`
	PromptEn    string `json:"prompt_en"`
	Description string `json:"description"`
	State       string `json:"state"`
	SubmitTime  int64  `json:"submit_time" gorm:"index"`
	StartTime   int64  `json:"start_time" gorm:"index"`
	FinishTime  int64  `json:"finish_time" gorm:"index"`
	ImageUrl    string `json:"image_url"`
	VideoUrl    string `json:"video_url"`
	VideoUrls   string `json:"video_urls"`
	Status      string `json:"status" gorm:"type:varchar(20);index"`
	Progress    string `json:"progress" gorm:"type:varchar(30);index"`
	FailReason  string `json:"fail_reason"`
	ChannelId   int    `json:"channel_id"`
	Quota       int    `json:"quota"`
	Buttons     string `json:"buttons"`
	Properties  string `json:"properties"`

	TokenId          int `json:"-" gorm:"default:0"`
	BillingChannelId int `json:"-" gorm:"default:0"`
}

func (fundingReleasedMidjourney) TableName() string { return "midjourneys" }

// Local SMTP fixture exercises the real mail transport without contacting any
// external mail service. It rejects the first DATA request to verify retries.
func fundingSMTP(t *testing.T) (<-chan string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	messages := make(chan string, 8)
	var attempts atomic.Int32
	var wg sync.WaitGroup
	oldServer, oldPort, oldAccount, oldToken, oldFrom := common.SMTPServer, common.SMTPPort, common.SMTPAccount, common.SMTPToken, common.SMTPFrom
	oldSSL, oldTLS := common.SMTPSSLEnabled, common.SMTPStartTLSEnabled
	common.SMTPServer = "127.0.0.1"
	common.SMTPPort = listener.Addr().(*net.TCPAddr).Port
	common.SMTPAccount = ""
	common.SMTPToken = ""
	common.SMTPFrom = "gateway@example.test"
	common.SMTPSSLEnabled = false
	common.SMTPStartTLSEnabled = false
	common.OptionMapRWMutex.Lock()
	oldProvider := common.OptionMap["EmailProvider"]
	if common.OptionMap == nil {
		common.OptionMap = map[string]string{}
	}
	common.OptionMap["EmailProvider"] = "smtp"
	common.OptionMapRWMutex.Unlock()
	wg.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				r := bufio.NewReader(conn)
				_, _ = io.WriteString(conn, "220 fixture ESMTP\r\n")
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					command := strings.TrimSpace(line)
					switch {
					case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
						_, _ = io.WriteString(conn, "250 fixture\r\n")
					case command == "DATA":
						if attempts.Add(1) == 1 {
							_, _ = io.WriteString(conn, "451 retry later\r\n")
							continue
						}
						_, _ = io.WriteString(conn, "354 send data\r\n")
						var body strings.Builder
						for {
							line, err = r.ReadString('\n')
							if err != nil {
								return
							}
							if line == ".\r\n" {
								break
							}
							body.WriteString(line)
						}
						messages <- body.String()
						_, _ = io.WriteString(conn, "250 accepted\r\n")
					case command == "QUIT":
						_, _ = io.WriteString(conn, "221 bye\r\n")
						return
					default:
						_, _ = io.WriteString(conn, "250 ok\r\n")
					}
				}
			})
		}
	})
	t.Cleanup(func() {
		listener.Close()
		wg.Wait()
		common.SMTPServer, common.SMTPPort, common.SMTPAccount, common.SMTPToken, common.SMTPFrom = oldServer, oldPort, oldAccount, oldToken, oldFrom
		common.SMTPSSLEnabled, common.SMTPStartTLSEnabled = oldSSL, oldTLS
		common.OptionMapRWMutex.Lock()
		common.OptionMap["EmailProvider"] = oldProvider
		common.OptionMapRWMutex.Unlock()
	})
	return messages, &attempts
}

func TestChannelFundingNotificationLifecycle(t *testing.T) {
	db := fundingDatabase(t, "sqlite", filepath.Join(t.TempDir(), "notification.db"))
	fundingSchema(t, db)
	fundingEnabled(t)
	messages, smtpAttempts := fundingSMTP(t)
	limit, redis := constant.NotifyLimitCount, common.RedisEnabled
	constant.NotifyLimitCount = 100
	common.RedisEnabled = false
	notifyLimitStore.Clear()
	t.Cleanup(func() { constant.NotifyLimitCount = limit; common.RedisEnabled = redis; notifyLimitStore.Clear() })
	user := model.User{Username: "mail-root", Role: common.RoleRootUser, Status: common.UserStatusEnabled, AffCode: "mail-root"}
	require.NoError(t, db.Create(&user).Error)
	channel := model.Channel{Name: "<mail-channel>", Type: 1, Key: "mail-key-a\nmail-key-b", Status: 1, Models: "test-model", Group: "default", ChannelInfo: model.ChannelInfo{IsMultiKey: true}}
	require.NoError(t, channel.Insert())
	for index := range 2 {
		_, err := model.RecordChannelFundingFailure(fundingIdentity(&channel, index), "balance_insufficient", "test", "insufficient_balance", "<script>alert(1)</script>", 402, true, 10)
		require.NoError(t, err)
	}
	require.NoError(t, RunChannelFundingNotifications(context.Background()))
	var deliveries []model.ChannelFundingDelivery
	require.NoError(t, db.Find(&deliveries).Error)
	require.Len(t, deliveries, 2)
	for _, row := range deliveries {
		assert.Equal(t, "missing_address", row.Status)
		assert.Equal(t, 1, row.Attempts)
	}
	assert.Zero(t, smtpAttempts.Load())
	setting, _ := common.Marshal(dto.UserSetting{NotificationEmail: "notifications@example.test", Language: "zh-CN"})
	require.NoError(t, db.Model(&user).Updates(map[string]any{"email": "fallback@example.test", "setting": string(setting)}).Error)
	require.NoError(t, db.Model(&model.ChannelFundingDelivery{}).Where("channel_id = ?", channel.Id).Update("next_attempt_at", 0).Error)
	constant.NotifyLimitCount = 0
	require.NoError(t, RunChannelFundingNotifications(context.Background()))
	require.NoError(t, db.Find(&deliveries).Error)
	assert.Equal(t, "limited", deliveries[0].Status)
	assert.Zero(t, smtpAttempts.Load())
	constant.NotifyLimitCount = 100
	require.NoError(t, db.Model(&model.ChannelFundingDelivery{}).Where("channel_id = ?", channel.Id).Update("next_attempt_at", 0).Error)
	require.NoError(t, RunChannelFundingNotifications(context.Background()))
	require.NoError(t, db.Find(&deliveries).Error)
	assert.Equal(t, "failed", deliveries[0].Status)
	assert.Equal(t, int32(1), smtpAttempts.Load())
	require.NoError(t, db.Model(&model.ChannelFundingDelivery{}).Where("channel_id = ?", channel.Id).Update("next_attempt_at", 0).Error)
	require.NoError(t, RunChannelFundingNotifications(context.Background()))
	require.NoError(t, db.Find(&deliveries).Error)
	for _, row := range deliveries {
		assert.Equal(t, "sent", row.Status)
		assert.Equal(t, 4, row.Attempts)
	}
	assert.Equal(t, int32(2), smtpAttempts.Load(), "two keys must share one mail")
	received := <-messages
	assert.Contains(t, received, "To: notifications@example.test")
	assert.Contains(t, received, "余额不足")
	assert.Contains(t, received, "&lt;script&gt;")
	assert.NotContains(t, received, "<script>")
	assert.NotContains(t, received, "funding.notice_entry")
	expired := model.ChannelFundingDelivery{IncidentID: deliveries[0].IncidentID, ChannelID: channel.Id, UserID: user.Id, DedupKey: "expired-reminder", Kind: "reminder", Status: "sending", Attempts: 5, NextAttemptAt: common.GetTimestamp() - 1}
	require.NoError(t, db.Create(&expired).Error)
	require.NoError(t, RunChannelFundingNotifications(context.Background()))
	require.NoError(t, db.First(&expired, expired.ID).Error)
	assert.Equal(t, "failed", expired.Status, "a worker crash on the last attempt must not leave a permanent sending state")
	assert.Equal(t, int32(2), smtpAttempts.Load(), "no sixth delivery attempt")
	var incident model.ChannelFundingIncident
	require.NoError(t, db.First(&incident, deliveries[0].IncidentID).Error)
	require.NoError(t, model.ResolveChannelFunding(incident.ID, incident.Revision, incident.LastFailureAt+1, true, true, 0, "recovered"))
	require.NoError(t, RunChannelFundingNotifications(context.Background()))
	received = <-messages
	assert.Contains(t, received, "已恢复")
	assert.Equal(t, int32(3), smtpAttempts.Load())
	require.NoError(t, RunChannelFundingNotifications(context.Background()))
	assert.Equal(t, int32(3), smtpAttempts.Load(), "restart/duplicate scan cannot resend completed deliveries")
	// A mail accepted after recovery must get a recovery notice as well.
	var other model.ChannelFundingIncident
	require.NoError(t, db.Where("id <> ?", incident.ID).First(&other).Error)
	require.NoError(t, db.Model(&model.ChannelFundingDelivery{}).Where("incident_id = ?", other.ID).Updates(map[string]any{"status": "sending", "sent_at": 0, "attempts": 1}).Error)
	require.NoError(t, model.ResolveChannelFunding(other.ID, other.Revision, other.LastFailureAt+1, true, true, 0, "recovered"))
	require.NoError(t, model.FinishFundingDelivery(deliveries[1].ID, 1, "sent", "", 0, true))
	var count int64
	require.NoError(t, db.Model(&model.ChannelFundingDelivery{}).Where("incident_id = ? AND kind = ?", other.ID, "recovery").Count(&count).Error)
	assert.Equal(t, int64(1), count)
	// Recheck subscription at send time, not just when the event is created.
	off := false
	setting, _ = common.Marshal(dto.UserSetting{ChannelNotifyEnabled: &off, NotificationEmail: "notifications@example.test"})
	require.NoError(t, db.Model(&user).Update("setting", string(setting)).Error)
	require.NoError(t, RunChannelFundingNotifications(context.Background()))
	assert.Equal(t, int32(3), smtpAttempts.Load())
	var recovery model.ChannelFundingDelivery
	require.NoError(t, db.Where("incident_id = ? AND kind = ?", other.ID, "recovery").First(&recovery).Error)
	assert.Equal(t, "cancelled", recovery.Status)
}

func TestChannelFundingSwitchesAndTaskAttribution(t *testing.T) {
	db := fundingDatabase(t, "sqlite", filepath.Join(t.TempDir(), "switches.db"))
	fundingSchema(t, db)
	fundingEnabled(t)
	for _, tc := range []struct {
		name                     string
		monitor, global, channel bool
		category                 string
		wantEvent, wantDisabled  bool
	}{
		{"all enabled", true, true, true, "balance_insufficient", true, true},
		{"monitor off", false, true, true, "balance_insufficient", false, false},
		{"global off", true, false, true, "balance_insufficient", true, false},
		{"channel off", true, true, false, "balance_insufficient", true, false},
		{"ambiguous", true, true, true, "suspected", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"channel_funding_setting.enabled": strconv.FormatBool(tc.monitor)}))
			common.AutomaticDisableChannelEnabled = tc.global
			auto := 0
			if tc.channel {
				auto = 1
			}
			channel := model.Channel{Name: tc.name, Type: 1, Key: "switch-key", Status: 1, Models: "test-model", Group: "default", AutoBan: &auto}
			require.NoError(t, channel.Insert())
			attempt := &FundingAttempt{Identity: fundingIdentity(&channel, 0), secret: channel.Key}
			code := "insufficient_balance"
			if tc.category == "suspected" {
				code = "insufficient_quota"
			}
			RecordChannelFundingError(attempt, 429, code, "", "quota")
			var events []model.ChannelFundingIncident
			require.NoError(t, db.Where("channel_id = ?", channel.Id).Find(&events).Error)
			assert.Equal(t, tc.wantEvent, len(events) == 1)
			current, err := model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			assert.Equal(t, tc.wantDisabled, current.Status == 3)
		})
	}
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"channel_funding_setting.enabled": "true"}))
	common.AutomaticDisableChannelEnabled = true
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.Token{}))
	// Batch polling may read several old tasks with the current channel key. Each
	// terminal failure must remain bound to its original submission credential.
	channel := model.Channel{Name: "batch", Type: 1, Key: "submit-key", Status: 1, Models: "test-model", Group: "default"}
	require.NoError(t, channel.Insert())
	identity := fundingIdentity(&channel, 0)
	task := model.Task{TaskID: "batch-old", ChannelId: channel.Id, Status: model.TaskStatusInProgress, PrivateData: model.TaskPrivateData{UpstreamTaskID: "old", FundingAttempt: &identity}}
	require.NoError(t, db.Create(&task).Error)
	channel.Key = "new-polling-key"
	require.NoError(t, channel.Update())
	currentIdentity := fundingIdentity(&channel, 0)
	currentTask := model.Task{TaskID: "batch-new", ChannelId: channel.Id, Status: model.TaskStatusInProgress, PrivateData: model.TaskPrivateData{UpstreamTaskID: "new", FundingAttempt: &currentIdentity}}
	require.NoError(t, db.Create(&currentTask).Error)
	adaptor := &scriptedBatchPollingAdaptor{results: map[string]*BatchTaskResult{
		"old": {TaskInfo: relaycommon.TaskInfo{Status: model.TaskStatusFailure, Reason: "insufficient balance"}},
		"new": {TaskInfo: relaycommon.TaskInfo{Status: model.TaskStatusInProgress}},
	}}
	require.NoError(t, updateBatchTasks(context.Background(), adaptor, channel.Id, []string{"old", "new"}, map[string]*model.Task{"old": &task, "new": &currentTask}))
	var count int64
	require.NoError(t, db.Model(&model.ChannelFundingIncident{}).Where("channel_id = ?", channel.Id).Count(&count).Error)
	assert.Zero(t, count)
	adaptor.results["new"].TaskInfo = relaycommon.TaskInfo{Status: model.TaskStatusFailure, Reason: "insufficient balance new-polling-key"}
	require.NoError(t, updateBatchTasks(context.Background(), adaptor, channel.Id, []string{"new"}, map[string]*model.Task{"new": &currentTask}))
	var incident model.ChannelFundingIncident
	require.NoError(t, db.Where("channel_id = ?", channel.Id).First(&incident).Error)
	assert.Equal(t, currentIdentity.BindingID, incident.BindingID)
	assert.NotContains(t, incident.Evidence, "new-polling-key")
}

type fundingBatchPollingAdaptor struct {
	scriptedBatchPollingAdaptor
	calls map[string][]string
}

func (a *fundingBatchPollingAdaptor) FetchBatchTasks(_ string, key string, tasks []*model.Task, _ string) (*http.Response, error) {
	for _, task := range tasks {
		a.calls[key] = append(a.calls[key], task.GetUpstreamTaskID())
	}
	body := `{}`
	if key == "batch-key-a" {
		// Some plugins parse this HTTP 200 error envelope into an unknown or
		// nonterminal result, rather than returning a Go parse error.
		body = `{"error":{"code":"insufficient_balance","message":"insufficient balance"}}`
		a.results = map[string]*BatchTaskResult{
			"task-a": {TaskInfo: relaycommon.TaskInfo{Status: model.TaskStatusInProgress}},
			"task-b": {TaskInfo: relaycommon.TaskInfo{Status: model.TaskStatusFailure, Reason: "insufficient balance"}},
		}
	} else {
		a.results = map[string]*BatchTaskResult{"task-b": {TaskInfo: relaycommon.TaskInfo{Status: model.TaskStatusInProgress}}}
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestChannelFundingBatchUsesSubmissionKeys(t *testing.T) {
	db := fundingDatabase(t, "sqlite", filepath.Join(t.TempDir(), "batch-keys.db"))
	fundingSchema(t, db)
	fundingEnabled(t)
	channel := model.Channel{Name: "batch-keys", Type: constant.ChannelTypeTaskPlugin, Key: "batch-key-a\nbatch-key-b", Status: 1, Models: "test-model", Group: "default", ChannelInfo: model.ChannelInfo{IsMultiKey: true}}
	require.NoError(t, channel.Insert())
	tasks := make(map[string]*model.Task)
	for index, key := range channel.GetKeys() {
		info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id, ChannelType: channel.Type, ChannelIsMultiKey: true, ApiKey: key}}
		task := model.InitTask("funding-test", info)
		assert.Equal(t, key, task.PrivateData.Key)
		identity := fundingIdentity(&channel, index)
		task.PrivateData.FundingAttempt = &identity
		task.PrivateData.UpstreamTaskID = []string{"task-a", "task-b"}[index]
		task.Status = model.TaskStatusInProgress
		require.NoError(t, db.Create(task).Error)
		tasks[task.GetUpstreamTaskID()] = task
	}
	adaptor := &fundingBatchPollingAdaptor{calls: make(map[string][]string)}
	require.NoError(t, updateBatchTasks(context.Background(), adaptor, channel.Id, []string{"task-a", "task-b"}, tasks))
	assert.Equal(t, map[string][]string{"batch-key-a": {"task-a"}, "batch-key-b": {"task-b"}}, adaptor.calls)
	var incidents []model.ChannelFundingIncident
	require.NoError(t, db.Where("channel_id = ?", channel.Id).Find(&incidents).Error)
	require.Len(t, incidents, 1)
	assert.Equal(t, channel.GetUpstreamCredentialSnapshot(0).BindingID, incidents[0].BindingID)
	assert.Equal(t, int64(1), incidents[0].Hits)
	for _, task := range tasks {
		require.NoError(t, db.First(task, task.ID).Error)
		assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), task.Status, "a foreign batch result must not fail or refund an unrelated task")
		encoded, err := common.Marshal(task)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "batch-key-")
	}
	stored, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[0])
	assert.Zero(t, stored.ChannelInfo.MultiKeyStatusList[1])
}
