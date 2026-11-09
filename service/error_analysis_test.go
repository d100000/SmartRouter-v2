package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"gorm.io/driver/clickhouse"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestErrorAnalysisOutputEvidence(t *testing.T) {
	// Reported token usage cannot override observed empty output. This is a
	// diagnostic classification only; the reported usage remains unchanged.
	emptyWithUsage := model.ClassifyFailureLog(model.Log{Type: model.LogTypeConsume, CompletionTokens: 12, Other: `{"admin_info":{"failure_diagnostic":{"path":"/v1/chat/completions","has_output":false}}}`})
	assert.Contains(t, emptyWithUsage.Categories, "zero_output")
	assert.Equal(t, 12, emptyWithUsage.CompletionTokens)
	for _, tc := range []struct {
		name, body string
		output     bool
		category   string
	}{
		{"empty completion", `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`, false, "zero_output"},
		{"tool call without text", `{"choices":[{"message":{"tool_calls":[{"function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, true, ""},
		{"reasoning without text", `{"choices":[{"message":{"reasoning_content":"thinking"}}]}`, true, ""},
		{"role-only stream", "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: [DONE]\n\n", false, "zero_output"},
		{"gemini function", `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"search","args":{}}}]}}]}`, true, ""},
		{"anthropic tool", "data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"tool_use\",\"name\":\"search\"}}\n\n", true, ""},
		{"responses tool", "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"search\"}}\n\n", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
			ObserveSchedulingResponse(c)
			_, err := c.Writer.WriteString(tc.body)
			require.NoError(t, err)
			other := model.NewLogOther()
			AppendRelayLogAdminInfo(c, &relaycommon.RelayInfo{StartTime: time.Now()}, other)
			entry := model.ClassifyFailureLog(model.Log{Type: model.LogTypeConsume, Other: other.JSONString()})
			require.NotNil(t, entry.Diagnostic.HasOutput)
			assert.Equal(t, tc.output, *entry.Diagnostic.HasOutput)
			if tc.category != "" {
				assert.Contains(t, entry.Categories, tc.category)
			} else {
				assert.Empty(t, entry.Categories)
			}
		})
	}
	for _, tc := range []struct {
		name, other string
		stream      bool
		want        []string
	}{
		{"empty JSON rejected before body write", `{"request_path":"/v1/chat/completions","error_code":"empty_response"}`, false, []string{"zero_output"}},
		{"legacy zero tokens", `{"request_path":"/v1/chat/completions"}`, false, []string{"unknown_output"}},
		{"embedding zero tokens", `{"request_path":"/v1/embeddings"}`, false, []string{}},
		{"client cancel", `{"stream_status":{"end_reason":"client_gone","response_status":"cancelled"}}`, true, []string{"client_cancelled"}},
		{"missing required terminal", `{"admin_info":{"failure_diagnostic":{"expects_terminal":true,"end_reason":"eof"}}}`, true, []string{"interrupted"}},
		{"persisted missing terminal failure", `{"admin_info":{"failure_diagnostic":{"expects_terminal":true,"end_reason":"eof","response_status":"failed","error_code":"bad_response","has_output":true}}}`, true, []string{"error", "interrupted"}},
		{"explicit upstream rejection", `{"admin_info":{"failure_diagnostic":{"expects_terminal":true,"end_reason":"eof","response_status":"failed","error_code":"invalid_api_key"}}}`, true, []string{"error"}},
		{"completed protocol before EOF", `{"admin_info":{"failure_diagnostic":{"expects_terminal":true,"end_reason":"eof","response_status":"completed"}}}`, true, []string{}},
		{"ordinary EOF", `{"stream_status":{"end_reason":"eof"}}`, true, []string{}},
		{"length limit", `{"stream_status":{"end_reason":"done","response_status":"incomplete"}}`, true, []string{}},
		{"transport timeout", `{"stream_status":{"end_reason":"timeout"}}`, true, []string{"interrupted"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := model.ClassifyFailureLog(model.Log{Type: model.LogTypeConsume, Other: tc.other, IsStream: tc.stream})
			assert.Equal(t, tc.want, entry.Categories)
		})
	}
}

func TestErrorAnalysisDatabaseMatrix(t *testing.T) {
	for _, kind := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			var driver gorm.Dialector
			switch kind {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "analysis.db"))
			case "mysql":
				if os.Getenv("TEST_ANALYSIS_MYSQL_DSN") == "" {
					t.Skip("TEST_ANALYSIS_MYSQL_DSN not set")
				}
				driver = mysql.Open(os.Getenv("TEST_ANALYSIS_MYSQL_DSN"))
			case "postgres":
				if os.Getenv("TEST_ANALYSIS_POSTGRES_DSN") == "" {
					t.Skip("TEST_ANALYSIS_POSTGRES_DSN not set")
				}
				driver = postgres.Open(os.Getenv("TEST_ANALYSIS_POSTGRES_DSN"))
			}
			db, err := gorm.Open(driver, &gorm.Config{})
			require.NoError(t, err)
			var version string
			versionQuery := "SELECT version()"
			if kind == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("database version: %s", version)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { sqlDB.Close() })
			oldDB, oldLog := model.DB, model.LOG_DB
			oldMainType, oldLogType := common.MainDatabaseType(), common.LogDatabaseType()
			model.DB, model.LOG_DB = db, db
			common.SetDatabaseTypes(common.DatabaseType(kind), common.DatabaseType(kind))
			t.Cleanup(func() { model.DB, model.LOG_DB = oldDB, oldLog; common.SetDatabaseTypes(oldMainType, oldLogType) })
			// Fresh schemas and the released rc.47 TEXT schema must both retain
			// reports across repeated startup migrations. DSNs are disposable.
			require.NoError(t, db.Migrator().DropTable(&model.SystemTaskLock{}, &model.SystemTask{}))
			var migrationSQL strings.Builder
			migrationDB := db.Session(&gorm.Session{Logger: gormlogger.New(log.New(&migrationSQL, "", 0), gormlogger.Config{LogLevel: gormlogger.Info})})
			for restart := range 2 {
				migrationSQL.Reset()
				require.NoError(t, migrationDB.AutoMigrate(&model.Log{}, &model.Option{}, &model.SystemTask{}, &model.SystemTaskLock{}, &model.ErrorAnalysisArtifact{}))
				if restart == 1 {
					assert.NotContains(t, strings.ToUpper(migrationSQL.String()), "ALTER TABLE", "fresh schema is stable on restart")
				}
			}
			for _, row := range []any{&model.Log{}, &model.Option{}} {
				require.NoError(t, db.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(row).Error)
			}
			now := time.Now().Unix()
			largeReport := strings.Repeat("错误分析报告：上游响应已完成。", 5000)
			require.Greater(t, len(largeReport), 65535)
			fresh, err := model.CreateSystemTask(model.ErrorAnalysisTaskType, nil, nil)
			require.NoError(t, err)
			_, claimedFresh, err := model.ClaimSystemTask(fresh.ID, model.ErrorAnalysisTaskType, "fresh-schema", now+60)
			require.NoError(t, err)
			require.True(t, claimedFresh)
			require.NoError(t, model.UpdateSystemTaskState(fresh.TaskID, "fresh-schema", AnalysisState{Report: largeReport}))
			require.NoError(t, model.FinishSystemTask(fresh.TaskID, "fresh-schema", model.SystemTaskStatusSucceeded, AnalysisResult{Report: largeReport}, ""))
			freshStored, err := model.GetSystemTaskByTaskID(fresh.TaskID)
			require.NoError(t, err)
			var freshState AnalysisState
			require.NoError(t, freshStored.DecodeState(&freshState))
			assert.Equal(t, largeReport, freshState.Report)
			var freshResult AnalysisResult
			require.NoError(t, common.UnmarshalJsonStr(string(freshStored.Result), &freshResult))
			assert.Equal(t, largeReport, freshResult.Report)
			artifact := model.ErrorAnalysisArtifact{TaskID: fresh.TaskID, Kind: "completion", Name: "saved-result", Data: model.LongText(largeReport)}
			require.NoError(t, db.Create(&artifact).Error)
			require.NoError(t, db.AutoMigrate(&model.ErrorAnalysisArtifact{}))
			var retainedArtifact model.ErrorAnalysisArtifact
			require.NoError(t, db.Where("id = ?", artifact.ID).Take(&retainedArtifact).Error)
			assert.Equal(t, largeReport, string(retainedArtifact.Data), "checkpoint results survive repeated migration")
			assert.Error(t, db.Create(&model.ErrorAnalysisArtifact{TaskID: fresh.TaskID, Kind: "completion", Name: "saved-result", Data: "duplicate"}).Error, "a completed step has one durable identity")

			require.NoError(t, db.Migrator().DropTable(&model.SystemTaskLock{}, &model.SystemTask{}))
			type releasedSystemTask struct {
				ID        int64                  `gorm:"primary_key"`
				TaskID    string                 `gorm:"type:varchar(64);uniqueIndex"`
				Type      string                 `gorm:"type:varchar(64);index"`
				Status    model.SystemTaskStatus `gorm:"type:varchar(32);index"`
				ActiveKey *string                `gorm:"type:varchar(64);uniqueIndex"`
				Payload   string                 `gorm:"type:text"`
				State     string                 `gorm:"type:text"`
				Result    string                 `gorm:"type:text"`
				Error     string                 `gorm:"type:text"`
				LockedBy  string                 `gorm:"type:varchar(128);index"`
				CreatedAt int64                  `gorm:"bigint;index"`
				UpdatedAt int64                  `gorm:"bigint;index"`
			}
			require.NoError(t, db.Table("system_tasks").AutoMigrate(&releasedSystemTask{}))
			require.NoError(t, db.AutoMigrate(&model.SystemTaskLock{}))
			legacy := releasedSystemTask{TaskID: "released-analysis", Type: model.ErrorAnalysisTaskType, Status: model.SystemTaskStatusRunning, ActiveKey: common.GetPointer(model.ErrorAnalysisTaskType), Payload: `{"start":1,"end":2}`, State: `{"report":"历史分析进度"}`, Result: `{"report":"历史分析报告"}`, LockedBy: "released-worker", CreatedAt: now - 10, UpdatedAt: now - 5}
			require.NoError(t, db.Table("system_tasks").Create(&legacy).Error)
			require.NoError(t, db.Create(&model.SystemTaskLock{Type: legacy.Type, TaskID: legacy.TaskID, LockedBy: legacy.LockedBy, LockedUntil: now + 60}).Error)
			for restart := range 2 {
				migrationSQL.Reset()
				require.NoError(t, migrationDB.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}, &model.ErrorAnalysisArtifact{}))
				if restart == 1 {
					assert.NotContains(t, strings.ToUpper(migrationSQL.String()), "ALTER TABLE", "upgraded schema is stable on restart")
				}
			}
			upgraded, err := model.GetSystemTaskByTaskID(legacy.TaskID)
			require.NoError(t, err)
			require.NotNil(t, upgraded)
			assert.Equal(t, legacy.ID, upgraded.ID)
			assert.Equal(t, legacy.Payload, upgraded.Payload)
			assert.Equal(t, legacy.State, string(upgraded.State))
			assert.Equal(t, legacy.Result, string(upgraded.Result))
			assert.Equal(t, legacy.LockedBy, upgraded.LockedBy)
			assert.Equal(t, legacy.CreatedAt, upgraded.CreatedAt)
			assert.Equal(t, legacy.UpdatedAt, upgraded.UpdatedAt)
			for _, index := range []string{"TaskID", "ActiveKey", "Type", "Status", "LockedBy", "CreatedAt", "UpdatedAt"} {
				assert.True(t, db.Migrator().HasIndex(&model.SystemTask{}, index), "retained index %s", index)
			}
			assert.Error(t, db.Create(&model.SystemTask{TaskID: legacy.TaskID, Type: "duplicate-id", Status: model.SystemTaskStatusSucceeded}).Error)
			_, err = model.CreateSystemTask(legacy.Type, nil, nil)
			assert.Error(t, err, "active-task uniqueness survives migration")
			require.NoError(t, model.RenewSystemTaskLock(legacy.TaskID, legacy.LockedBy, now+120))
			require.NoError(t, model.UpdateSystemTaskState(legacy.TaskID, legacy.LockedBy, AnalysisState{Report: largeReport}))
			require.NoError(t, model.FinishSystemTask(legacy.TaskID, legacy.LockedBy, model.SystemTaskStatusSucceeded, AnalysisResult{Report: largeReport}, ""))
			upgraded, err = model.GetSystemTaskByTaskID(legacy.TaskID)
			require.NoError(t, err)
			var upgradedState AnalysisState
			require.NoError(t, upgraded.DecodeState(&upgradedState))
			assert.Equal(t, largeReport, upgradedState.Report)
			var upgradedResult AnalysisResult
			require.NoError(t, common.UnmarshalJsonStr(string(upgraded.Result), &upgradedResult))
			assert.Equal(t, largeReport, upgradedResult.Report)
			assert.Equal(t, model.SystemTaskStatusSucceeded, upgraded.Status)
			assert.Nil(t, upgraded.ActiveKey)
			var remainingLocks int64
			require.NoError(t, db.Model(&model.SystemTaskLock{}).Count(&remainingLocks).Error)
			assert.Zero(t, remainingLocks)
			logs := []model.Log{
				{CreatedAt: now - 10, RequestId: "recovered", Type: model.LogTypeError, ModelName: "model-a", ChannelId: 1, Other: `{"error_code":"rate_limit","status_code":429}`},
				{CreatedAt: now - 9, RequestId: "recovered", Type: model.LogTypeConsume, ModelName: "model-a", ChannelId: 2, CompletionTokens: 3},
				{CreatedAt: now - 8, RequestId: "empty", Type: model.LogTypeConsume, ModelName: "model-a", ChannelId: 1, Other: `{"admin_info":{"failure_diagnostic":{"path":"/v1/chat/completions","has_output":false}},"root_info":{"key":"do-not-send"}}`},
				{CreatedAt: now - 7, RequestId: "cut", Type: model.LogTypeConsume, ModelName: "model-b", ChannelId: 3, IsStream: true, CompletionTokens: 5, Other: `{"stream_status":{"end_reason":"timeout"}}`},
				{CreatedAt: now - 6, RequestId: "error", Type: model.LogTypeError, ModelName: "model-b", ChannelId: 3, Content: `{"error":{"message":"upstream failed"},"prompt":"secret prompt must not leave"}`, Other: `{"error_code":"sk-secret-value","status_code":500}`},
				{CreatedAt: now, RequestId: "boundary", Type: model.LogTypeConsume, ModelName: "excluded"},
			}
			require.NoError(t, db.Create(&logs).Error)
			// A second migration of the unchanged schema retains existing evidence.
			require.NoError(t, db.AutoMigrate(&model.Log{}, &model.Option{}, &model.SystemTask{}, &model.SystemTaskLock{}, &model.ErrorAnalysisArtifact{}))
			snapshot, err := model.LoadErrorAnalysis(context.Background(), model.ErrorAnalysisFilter{Start: now - 20, End: now})
			require.NoError(t, err)
			assert.Equal(t, 4, snapshot.Requests)
			assert.Equal(t, 3, snapshot.Affected)
			assert.Equal(t, 1, snapshot.ZeroOutput)
			assert.Equal(t, 1, snapshot.Interrupted)
			assert.Equal(t, 1, snapshot.Errors)
			assert.Equal(t, 1, snapshot.Recovered)
			assert.Equal(t, int64(5), snapshot.TotalLogRows)
			encoded, err := common.Marshal(snapshot)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "secret prompt")
			assert.NotContains(t, string(encoded), "sk-secret-value")
			assert.NotContains(t, string(encoded), "do-not-send")
			// Exact IDs search retained history across time and channel filters,
			// include successful calls, and retain both sides of a retry.
			exact := model.ErrorAnalysisFilter{RequestID: " recovered ", Start: now - 1, End: now, Channel: 99}
			require.NoError(t, exact.Normalize(time.Now()))
			chain, err := model.LoadErrorAnalysis(context.Background(), exact)
			require.NoError(t, err)
			require.Len(t, chain.Logs, 1)
			require.Len(t, chain.Logs[0].Records, 2)
			assert.Equal(t, 2, chain.Logs[0].Records[0].Channel)
			assert.Equal(t, 1, chain.Logs[0].Records[1].Channel)
			successful, err := model.LoadErrorAnalysis(context.Background(), model.ErrorAnalysisFilter{RequestID: "boundary"})
			require.NoError(t, err)
			require.Len(t, successful.Logs, 1)
			assert.Empty(t, successful.Logs[0].Categories)
			missing, err := model.LoadErrorAnalysis(context.Background(), model.ErrorAnalysisFilter{RequestID: "recovered' OR 1=1 --"})
			require.NoError(t, err)
			assert.Empty(t, missing.Logs)
			finance := []model.Log{
				{RequestId: "FINANCIAL-RETRY", CreatedAt: now - 3, Type: model.LogTypeConsume, ChannelId: 9, ModelName: "finance"},
				{RequestId: "financial-retry", CreatedAt: now - 2, Type: model.LogTypeError, ChannelId: 4, ModelName: "finance", Other: `{"admin_info":{"settlement_evidence":{"version":1,"upstream":[{"attempt":1,"channel":4,"usage":{"usage":{"input_tokens":10}}}]}}}`},
				{RequestId: "financial-retry", CreatedAt: now - 1, Type: model.LogTypeConsume, ChannelId: 6, ModelName: "finance", Quota: 200, CompletionTokens: 2},
			}
			require.NoError(t, db.Create(&finance).Error)
			financial, err := model.LoadErrorAnalysis(context.Background(), model.ErrorAnalysisFilter{RequestID: "financial-retry"})
			require.NoError(t, err)
			require.Len(t, financial.Logs, 1)
			assert.Equal(t, 1, financial.UpstreamUsageUnbilled)
			assert.Equal(t, 1, financial.Recovered)
			assert.Equal(t, 1, financial.Affected)
			assert.EqualValues(t, 200, financial.Logs[0].RecordedQuota)
			assert.Contains(t, financial.Logs[0].Categories, "upstream_usage_unbilled")
			// The reader must use LOG_DB, independently of the primary database.
			model.DB = oldDB
			separate, err := model.LoadErrorAnalysis(context.Background(), model.ErrorAnalysisFilter{Start: now - 20, End: now, Model: "model-b"})
			require.NoError(t, err)
			assert.Equal(t, 2, separate.Requests)
			model.DB = db
			t.Setenv("CRYPTO_SECRET", "deterministic-test-secret-not-for-production")
			config, err := GetErrorAnalysisConfig()
			require.NoError(t, err)
			require.NoError(t, SaveErrorAnalysisConfig(config, "test-provider-credential", false))
			assert.Error(t, SaveErrorAnalysisConfig(config, "stale-overwrite", false))
			config, err = GetErrorAnalysisConfig()
			require.NoError(t, err)
			assert.True(t, config.APIKeyConfigured)
			assert.NotContains(t, config.EncryptedKey, "test-provider-credential")
			key, err := analysisAPIKey(config)
			require.NoError(t, err)
			assert.Equal(t, "test-provider-credential", key)
			config.Enabled = true
			require.NoError(t, SaveErrorAnalysisConfig(config, "", false))
			current, err := GetErrorAnalysisConfig()
			require.NoError(t, err)
			assert.Equal(t, config.EncryptedKey, current.EncryptedKey)
			task, created, err := StartErrorAnalysis(model.ErrorAnalysisFilter{Start: now - 20, End: now})
			require.NoError(t, err)
			require.True(t, created)
			duplicate, created, err := StartErrorAnalysis(model.ErrorAnalysisFilter{Start: now - 20, End: now})
			require.NoError(t, err)
			assert.False(t, created)
			assert.Equal(t, task.TaskID, duplicate.TaskID)
			claimed, ok, err := model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, "analysis-test", now+60)
			require.NoError(t, err)
			require.True(t, ok)
			state := AnalysisState{Phase: "retrying", Attempt: 1, Attempts: []AnalysisAttempt{{Number: 1, Code: "analysis_rate_limited", StatusCode: 429, Retryable: true}}}
			require.NoError(t, model.UpdateSystemTaskState(claimed.TaskID, "analysis-test", state))
			require.NoError(t, model.FinishSystemTask(claimed.TaskID, "analysis-test", model.SystemTaskStatusSucceeded, AnalysisResult{Report: "verified report", Snapshot: snapshot}, ""))
			stored, err := model.GetSystemTaskByTaskID(task.TaskID)
			require.NoError(t, err)
			assert.Contains(t, stored.Result, "verified report")
			assert.Nil(t, stored.ActiveKey)
			second, err := model.CreateSystemTask(model.ErrorAnalysisTaskType, model.ErrorAnalysisFilter{Start: now - 20, End: now}, nil)
			require.NoError(t, err)
			assert.NotEqual(t, task.TaskID, second.TaskID)
			_, ok, err = model.ClaimSystemTask(second.ID, model.ErrorAnalysisTaskType, "lost-worker", now+60)
			require.NoError(t, err)
			require.True(t, ok)
			require.NoError(t, model.ExpireStaleSystemTaskLocks(now+61))
			stale, err := model.GetSystemTaskByTaskID(second.TaskID)
			require.NoError(t, err)
			assert.Equal(t, model.SystemTaskStatusFailed, stale.Status)
			assert.Equal(t, "task lease expired", stale.Error)
			assert.ErrorIs(t, model.UpdateSystemTaskState(second.TaskID, "lost-worker", state), model.ErrSystemTaskLockLost)
			loc, err := time.LoadLocation("Asia/Shanghai")
			require.NoError(t, err)
			morning := time.Date(2026, 10, 29, 3, 0, 0, 0, loc)
			assert.True(t, (errorAnalysisHandler{}).Due(morning, &model.SystemTask{CreatedAt: morning.AddDate(0, 0, -1).Unix()}))
			assert.False(t, (errorAnalysisHandler{}).Due(morning, &model.SystemTask{CreatedAt: morning.Unix()}))
			assert.False(t, (errorAnalysisHandler{}).Due(morning.Add(-time.Second), nil))
		})
	}
}

type analysisTestTransport func(*http.Request) (*http.Response, error)

func TestErrorAnalysisEndpointDNSOverride(t *testing.T) {
	fallback := staticSSRFResolver{"other.example": {{IP: net.ParseIP("1.1.1.1")}}}
	resolver := analysisEndpointResolver{override: "analysis.example=8.8.8.8", fallback: fallback}
	resolved, err := resolver.LookupIPAddr(context.Background(), "ANALYSIS.EXAMPLE.")
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	assert.Equal(t, "8.8.8.8", resolved[0].IP.String())
	resolved, err = resolver.LookupIPAddr(context.Background(), "other.example")
	require.NoError(t, err)
	assert.Equal(t, "1.1.1.1", resolved[0].IP.String())
	for _, address := range []string{"127.0.0.1", "169.254.169.254", "198.18.0.1", "::1", "invalid"} {
		t.Run(address, func(t *testing.T) {
			t.Setenv("ERROR_ANALYSIS_HOST_OVERRIDE", "analysis.example="+address)
			client := newAnalysisHTTPClient()
			defer client.CloseIdleConnections()
			_, err := client.Get("https://analysis.example/v1/chat/completions")
			require.Error(t, err, "the override must not bypass address protection")
		})
	}
}

func TestErrorAnalysisWorkerRetryAndHistory(t *testing.T) {
	for _, tc := range []struct {
		name           string
		firstStatus    int
		firstBody      string
		wantStatus     model.SystemTaskStatus
		wantAttempts   int
		alwaysFail     bool
		wantError      string
		maxAttempts    int
		timeoutSeconds int
		wantLimits     []int
		persistFailure string
	}{
		{name: "an explicitly saved timeout is preserved", firstStatus: 200, firstBody: "data: {\"choices\":[{\"delta\":{\"content\":\"complete report\"},\"finish_reason\":\"stop\"}]}\n\n", wantStatus: model.SystemTaskStatusSucceeded, wantAttempts: 1, timeoutSeconds: 240},
		{name: "rate limit recovers", firstStatus: 429, wantStatus: model.SystemTaskStatusSucceeded, wantAttempts: 2, wantLimits: []int{6000, 6000}},
		{name: "interrupted draft is replaced", firstStatus: 200, firstBody: "data: {\"choices\":[{\"delta\":{\"content\":\"abandoned draft\"}}]}\n\n", wantStatus: model.SystemTaskStatusSucceeded, wantAttempts: 2, wantLimits: []int{6000, 6000}},
		{name: "invalid credentials stop immediately", firstStatus: 401, wantStatus: model.SystemTaskStatusFailed, wantAttempts: 1, wantError: "analysis_authentication_failed"},
		{name: "repeated failures exhaust the retry budget", firstStatus: 503, wantStatus: model.SystemTaskStatusFailed, wantAttempts: 2, alwaysFail: true, wantError: "analysis_http_error"},
		{name: "truncated report receives a larger output budget", firstStatus: 200, firstBody: "data: {\"choices\":[{\"delta\":{\"content\":\"abandoned draft\"},\"finish_reason\":\"length\"}]}\n\n", wantStatus: model.SystemTaskStatusSucceeded, wantAttempts: 2, wantLimits: []int{6000, 12000}},
		{name: "repeated truncation stops at the output ceiling", firstStatus: 200, firstBody: "data: {\"choices\":[{\"finish_reason\":\"length\"}]}\n\n", wantStatus: model.SystemTaskStatusFailed, wantAttempts: 3, alwaysFail: true, wantError: "analysis_incomplete_report", maxAttempts: 5, wantLimits: []int{6000, 12000, 24000}},
		{name: "filtered output is not retried", firstStatus: 200, firstBody: "data: {\"choices\":[{\"finish_reason\":\"content_filter\"}]}\n\n", wantStatus: model.SystemTaskStatusFailed, wantAttempts: 1, wantError: "analysis_incomplete_report"},
		{name: "oversized snapshot retains the completed report", firstStatus: 200, firstBody: "data: {\"choices\":[{\"delta\":{\"content\":\"complete report\"},\"finish_reason\":\"stop\"}]}\n\n", wantStatus: model.SystemTaskStatusSucceeded, wantAttempts: 1, persistFailure: "snapshot"},
		{name: "result write failure finishes with a persistence error", firstStatus: 200, firstBody: "data: {\"choices\":[{\"delta\":{\"content\":\"complete report\"},\"finish_reason\":\"stop\"}]}\n\n", wantStatus: model.SystemTaskStatusFailed, wantAttempts: 1, wantError: "analysis_persistence_error", persistFailure: "report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncate(t)
			require.NoError(t, model.DB.AutoMigrate(&model.Option{}))
			require.NoError(t, model.DB.Where(map[string]any{"key": errorAnalysisConfigOption}).Delete(&model.Option{}).Error)
			t.Cleanup(func() {
				require.NoError(t, model.DB.Where(map[string]any{"key": errorAnalysisConfigOption}).Delete(&model.Option{}).Error)
			})
			t.Setenv("CRYPTO_SECRET", "worker-test-secret-not-for-production")
			config, err := GetErrorAnalysisConfig()
			require.NoError(t, err)
			assert.Equal(t, 600, config.TimeoutSeconds)
			assert.Equal(t, 60, config.IdleSeconds)
			if tc.timeoutSeconds > 0 {
				config.TimeoutSeconds = tc.timeoutSeconds
			}
			config.MaxAttempts = 2
			if tc.maxAttempts > 0 {
				config.MaxAttempts = tc.maxAttempts
			}
			require.NoError(t, SaveErrorAnalysisConfig(config, "worker-key", false))
			storedConfig, err := GetErrorAnalysisConfig()
			require.NoError(t, err)
			assert.Equal(t, config.TimeoutSeconds, storedConfig.TimeoutSeconds)
			now := time.Now().Unix()
			require.NoError(t, model.LOG_DB.Create(&model.Log{RequestId: "worker-evidence", Type: model.LogTypeError, CreatedAt: now - 1, ModelName: "test-model", Other: `{"error_code":"upstream_error"}`}).Error)
			task, err := model.CreateSystemTask(model.ErrorAnalysisTaskType, model.ErrorAnalysisFilter{Start: now - 2, End: now}, nil)
			require.NoError(t, err)
			claimed, ok, err := model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, "worker-test", now+60)
			require.NoError(t, err)
			require.True(t, ok)
			if tc.persistFailure != "" {
				const callback = "analysis_result_write_failure"
				require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table != "system_tasks" {
						return
					}
					updates, ok := tx.Statement.Dest.(map[string]any)
					if !ok {
						return
					}
					resultText, ok := updates["result"].(string)
					if !ok {
						return
					}
					root := gjson.Parse(resultText)
					if tc.persistFailure == "snapshot" && root.Get("snapshot").Type == gjson.JSON || tc.persistFailure == "report" && root.Get("report").String() != "" {
						tx.AddError(errors.New("analysis test result write failure"))
					}
				}))
				t.Cleanup(func() { require.NoError(t, model.DB.Callback().Update().Remove(callback)) })
			}
			calls := 0
			var limits []int
			client := &http.Client{Transport: analysisTestTransport(func(request *http.Request) (*http.Response, error) {
				calls++
				var payload struct {
					MaxTokens int `json:"max_tokens"`
				}
				require.NoError(t, common.DecodeJson(request.Body, &payload))
				limits = append(limits, payload.MaxTokens)
				status, body := http.StatusOK, "data: {\"choices\":[{\"delta\":{\"content\":\"complete report\"},\"finish_reason\":\"stop\"}]}\n\n"
				if calls == 1 || tc.alwaysFail {
					status, body = tc.firstStatus, tc.firstBody
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			})}
			(errorAnalysisHandler{client: client}).Run(context.Background(), claimed, "worker-test")
			stored, err := model.GetSystemTaskByTaskID(task.TaskID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, stored.Status, stored.Error)
			assert.Equal(t, tc.wantAttempts, calls)
			if tc.wantLimits != nil {
				assert.Equal(t, tc.wantLimits, limits)
			}
			assert.Nil(t, stored.ActiveKey)
			var held int64
			require.NoError(t, model.DB.Model(&model.SystemTaskLock{}).Where("task_id = ?", task.TaskID).Count(&held).Error)
			assert.Zero(t, held, "terminal tasks must release their lease")
			assert.Equal(t, tc.wantError, stored.Error)
			if tc.persistFailure == "report" {
				assert.Empty(t, stored.Result)
				return
			}
			var result AnalysisResult
			require.NoError(t, common.UnmarshalJsonStr(string(stored.Result), &result))
			require.Len(t, result.Attempts, tc.wantAttempts)
			for i, attempt := range result.Attempts {
				assert.Equal(t, limits[i], attempt.MaxOutputTokens)
				assert.Equal(t, config.TimeoutSeconds, attempt.TimeoutSeconds)
				assert.Equal(t, config.IdleSeconds, attempt.IdleSeconds)
				assert.GreaterOrEqual(t, attempt.DurationMs, int64(0))
			}
			if tc.wantStatus == model.SystemTaskStatusSucceeded {
				assert.Equal(t, "complete report", result.Report)
				assert.Equal(t, "stop", result.Attempts[len(result.Attempts)-1].FinishReason)
				assert.Equal(t, http.StatusOK, result.Attempts[len(result.Attempts)-1].StatusCode)
			}
			if tc.persistFailure == "snapshot" {
				assert.True(t, result.SnapshotOmitted)
				assert.Nil(t, result.Snapshot)
			}
			assert.NotContains(t, string(stored.Result), "worker-key")
			assert.NotContains(t, string(stored.Result), "abandoned draft")
		})
	}
}

func (f analysisTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestErrorAnalysisStreamFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		status           int
		retry            bool
		finish           string
	}{
		{"complete", "data: {\"choices\":[{\"delta\":{\"content\":\"# Report\\nEvidence\"}}]}\n\ndata: [DONE]\n\n", "", 200, false, ""},
		{"reasoning is activity but not report content", "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"private reasoning\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"Report\"},\"finish_reason\":\"stop\"}]}\n\n", "", 200, false, "stop"},
		{"stop without done", "data: {\"choices\":[{\"delta\":{\"content\":\"Report\"},\"finish_reason\":\"stop\"}]}\n\n", "", 200, false, "stop"},
		{"interrupted", "data: {\"choices\":[{\"delta\":{\"content\":\"Partial\"}}]}\n\n", "analysis_stream_interrupted", 200, true, ""},
		{"empty", "data: [DONE]\n\n", "analysis_empty_report", 200, true, ""},
		{"in stream error", "data: {\"error\":{\"message\":\"secret token\"}}\n\n", "analysis_stream_error", 200, true, ""},
		{"truncated", "data: {\"choices\":[{\"finish_reason\":\"length\"}]}\n\n", "analysis_incomplete_report", 200, true, "length"},
		{"filtered", "data: {\"choices\":[{\"finish_reason\":\"content_filter\"}]}\n\n", "analysis_incomplete_report", 200, false, "content_filter"},
		{"tool call cannot complete a report", "data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\n", "analysis_incomplete_report", 200, false, "tool_calls"},
		{"legacy function call cannot complete a report", "data: {\"choices\":[{\"finish_reason\":\"function_call\"}]}\n\n", "analysis_incomplete_report", 200, false, "function_call"},
		{"provider resource interruption", "data: {\"choices\":[{\"finish_reason\":\"insufficient_system_resource\"}]}\n\n", "analysis_incomplete_report", 200, true, "insufficient_system_resource"},
		{"provider aborted", "data: {\"choices\":[{\"finish_reason\":\"aborted\"}]}\n\n", "analysis_incomplete_report", 200, true, "aborted"},
		{"unknown finish reason is sanitized", "data: {\"choices\":[{\"finish_reason\":\"secret token\"}]}\n\n", "analysis_incomplete_report", 200, false, "unknown"},
		{"unauthorized", "private raw error", "analysis_authentication_failed", 401, false, ""},
		{"rate limited", "private raw error", "analysis_rate_limited", 429, true, ""},
		{"server error", "private raw error", "analysis_http_error", 503, true, ""},
		{"malformed", "data: {bad json}\n\n", "analysis_invalid_sse", 200, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
				assert.Equal(t, "POST", r.Method)
				var request map[string]any
				require.NoError(t, common.DecodeJson(r.Body, &request))
				assert.Equal(t, true, request["stream"])
				assert.Equal(t, float64(6000), request["max_tokens"])
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			var report string
			attempt := &AnalysisAttempt{}
			err := streamErrorAnalysis(context.Background(), server.Client(), ErrorAnalysisConfig{Endpoint: server.URL, Model: "test", TimeoutSeconds: 30, IdleSeconds: 10}, "test-key", "synthetic evidence", attempt, func(value string) error { report = value; return nil })
			assert.Equal(t, tc.finish, attempt.FinishReason)
			assert.Equal(t, tc.status, attempt.StatusCode)
			assert.Equal(t, 6000, attempt.MaxOutputTokens)
			assert.Equal(t, 30, attempt.TimeoutSeconds)
			assert.Equal(t, 10, attempt.IdleSeconds)
			if tc.code == "" {
				require.NoError(t, err)
				assert.NotEmpty(t, report)
				assert.NotContains(t, report, "private reasoning")
				return
			}
			var failure *analysisFailure
			require.ErrorAs(t, err, &failure)
			assert.Equal(t, tc.code, failure.Code)
			assert.Equal(t, tc.retry, failure.Retryable)
			assert.NotContains(t, err.Error(), "secret")
		})
	}
	for _, tc := range []struct {
		name        string
		idle, total int
		code        string
		elapsed     time.Duration
	}{{"idle", 10, 30, "analysis_idle_timeout", 10 * time.Second}, {"deadline", 60, 30, "analysis_timeout", 30 * time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &http.Client{Transport: analysisTestTransport(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}
				attempt := &AnalysisAttempt{}
				err := streamErrorAnalysis(context.Background(), client, ErrorAnalysisConfig{Endpoint: "https://example.com/v1/chat/completions", IdleSeconds: tc.idle, TimeoutSeconds: tc.total}, "test", "test", attempt, func(string) error { return nil })
				require.EqualError(t, err, tc.code)
				assert.Equal(t, tc.elapsed.Milliseconds(), attempt.DurationMs)
			})
		})
	}
	t.Run("reasoning can continue past the old total timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			client := &http.Client{Transport: analysisTestTransport(func(request *http.Request) (*http.Response, error) {
				reader, writer := io.Pipe()
				go func() {
					defer writer.Close()
					for range 6 {
						time.Sleep(45 * time.Second)
						_, err := io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"private reasoning\"}}]}\n\n")
						if err != nil {
							return
						}
					}
					_, _ = io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"完整中文报告\"},\"finish_reason\":\"stop\"}]}\n\n")
				}()
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader, Request: request}, nil
			})}
			attempt := &AnalysisAttempt{}
			var report string
			err := streamErrorAnalysis(context.Background(), client, ErrorAnalysisConfig{Endpoint: "https://example.com/v1/chat/completions", IdleSeconds: 60, TimeoutSeconds: 600}, "test", "test", attempt, func(value string) error { report = value; return nil })
			require.NoError(t, err)
			assert.Equal(t, "完整中文报告", report)
			assert.Equal(t, int64(270000), attempt.DurationMs)
			assert.Equal(t, "stop", attempt.FinishReason)
		})
	})
	client := newAnalysisHTTPClient()
	for _, target := range []string{"http://127.0.0.1/", "http://169.254.169.254/"} {
		request, err := http.NewRequestWithContext(context.Background(), "GET", target, nil)
		require.NoError(t, err)
		_, err = client.Do(request)
		require.Error(t, err)
	}
}

// Opt-in live verification uses a credential file outside the repository and
// an isolated database containing synthetic diagnostic facts only. Credentials
// are never written to the report or capture files, or printed in test output.
func TestErrorAnalysisLiveProvider(t *testing.T) {
	keyFile := os.Getenv("TEST_ANALYSIS_KEY_FILE")
	if keyFile == "" {
		t.Skip("live provider test not configured")
	}
	keyBytes, err := os.ReadFile(keyFile)
	require.NoError(t, err)
	key := strings.TrimSpace(string(keyBytes))
	require.NotEmpty(t, key)
	endpoint, modelName := os.Getenv("TEST_ANALYSIS_ENDPOINT"), os.Getenv("TEST_ANALYSIS_MODEL")
	require.NotEmpty(t, endpoint, "TEST_ANALYSIS_ENDPOINT must name the explicitly authorized provider")
	require.NotEmpty(t, modelName, "TEST_ANALYSIS_MODEL must name an available model")
	endpointURL, err := url.Parse(endpoint)
	require.NoError(t, err)

	db := setupErrorAnalysisWorkflowDB(t)
	db.Logger = gormlogger.Default.LogMode(gormlogger.Silent)
	t.Setenv("CRYPTO_SECRET", common.GetRandomString(48))
	config, err := GetErrorAnalysisConfig()
	require.NoError(t, err)
	config.Endpoint, config.Model = endpoint, modelName
	config.Enabled, config.ReportLanguage, config.Timezone = false, "zh", "Asia/Shanghai"
	config.TimeoutSeconds, config.MaxAttempts, config.MaxCalls = 90, 2, 14
	config.ContextWindowTokens, config.ChunkInputTokens, config.MaxOutputTokens, config.SafetyMarginTokens = 32768, 8000, 2048, 4096
	require.NoError(t, SaveErrorAnalysisConfig(config, key, false))
	location, err := time.LoadLocation(config.Timezone)
	require.NoError(t, err)
	now := time.Now().In(location)
	end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	start := end.AddDate(0, 0, -7)
	filter := model.ErrorAnalysisFilter{Start: start.Unix(), End: end.Unix()}
	logs := make([]model.Log, 0, 37)
	for _, period := range []struct {
		Name        string
		Start       time.Time
		TimeoutDays []int
	}{
		{"previous", start.AddDate(0, 0, -7), []int{3}},
		{"current", start, []int{3, 4, 5, 6}},
	} {
		for day := range 7 {
			stamp := period.Start.AddDate(0, 0, day).Add(12 * time.Hour).Unix()
			for sample := range 2 {
				logs = append(logs, model.Log{RequestId: fmt.Sprintf("synthetic-%s-success-%d-%d", period.Name, day, sample), Type: model.LogTypeConsume, CreatedAt: stamp + int64(sample), ModelName: "synthetic-chat-model", ChannelId: 7, CompletionTokens: 8, Other: `{"admin_info":{"failure_diagnostic":{"path":"/v1/chat/completions","has_output":true,"response_status":"completed"}}}`})
			}
			if slices.Contains(period.TimeoutDays, day) {
				logs = append(logs, model.Log{RequestId: fmt.Sprintf("synthetic-%s-timeout-%d", period.Name, day), Type: model.LogTypeError, CreatedAt: stamp + 10, ModelName: "synthetic-chat-model", ChannelId: 7, Content: "Synthetic fixture: upstream request timed out", Other: `{"error_code":"upstream_timeout","status_code":504,"admin_info":{"failure_diagnostic":{"path":"/v1/chat/completions","has_output":false,"end_reason":"timeout"}}}`})
			}
			if day == 2 || day == 5 {
				logs = append(logs, model.Log{RequestId: fmt.Sprintf("synthetic-%s-limited-%d", period.Name, day), Type: model.LogTypeError, CreatedAt: stamp + 20, ModelName: "synthetic-chat-model", ChannelId: 8, Content: "Synthetic fixture: upstream rate limit exceeded", Other: `{"error_code":"rate_limit_exceeded","status_code":429,"admin_info":{"failure_diagnostic":{"path":"/v1/chat/completions","has_output":false}}}`})
			}
		}
	}
	require.NoError(t, db.Create(&logs).Error)
	client := newAnalysisCompletionClient(time.Duration(config.TimeoutSeconds) * time.Second)
	defer client.CloseIdleConnections()
	if publicIP := os.Getenv("TEST_ANALYSIS_PUBLIC_IP"); publicIP != "" {
		// An independently verified public address may bypass local fake-IP DNS;
		// the original HTTPS hostname still controls certificate validation.
		ip := net.ParseIP(publicIP)
		require.NotNil(t, ip)
		require.True(t, (&common.SSRFProtection{}).IsIPAccessAllowed(ip))
		client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil || host != endpointURL.Hostname() {
				return nil, fmt.Errorf("unexpected live-test target")
			}
			return (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, net.JoinHostPort(publicIP, port))
		}
	}
	transport := client.Transport
	defer transport.(*http.Transport).CloseIdleConnections()
	calls := 0
	client.Transport = analysisTestTransport(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		request.Body.Close()
		request.Body = io.NopCloser(strings.NewReader(string(body)))
		if gjson.GetBytes(body, "stream").Bool() || !gjson.GetBytes(body, "stream").Exists() {
			return nil, errors.New("live workflow attempted a streaming request")
		}
		if request.URL.Host != endpointURL.Host || request.URL.Scheme != "https" {
			return nil, errors.New("unexpected live-test target")
		}
		calls++
		started := time.Now()
		t.Logf("live request %d started (non-streaming)", calls)
		response, err := transport.RoundTrip(request)
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Logf("live request %d response headers: http=%d duration_ms=%d transport_error=%t", calls, status, time.Since(started).Milliseconds(), err != nil)
		return response, err
	})
	task, created, err := StartErrorAnalysis(filter)
	require.NoError(t, err)
	require.True(t, created)
	claimed, ok, err := model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, "live-workflow", time.Now().Add(8*time.Minute).Unix())
	require.NoError(t, err)
	require.True(t, ok)
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute+30*time.Second)
	defer cancel()
	(errorAnalysisHandler{client: client}).Run(ctx, claimed, "live-workflow")
	stored, err := model.GetSystemTaskByTaskID(task.TaskID)
	require.NoError(t, err)
	var state AnalysisState
	require.NoError(t, stored.DecodeState(&state))
	var result AnalysisResult
	if len(stored.Result) > 0 {
		require.NoError(t, common.UnmarshalJsonStr(string(stored.Result), &result))
	}
	// Save the observable state before asserting success so a failed live run
	// leaves its safe status, attempts and partial findings available for review.
	if output := os.Getenv("TEST_ANALYSIS_REPORT_FILE"); output != "" {
		report := "合成数据试跑报告（非生产数据）\n\n" + result.Report
		require.NoError(t, os.WriteFile(output, []byte(strings.ReplaceAll(report, key, "[redacted]")), 0600))
	}
	if output := os.Getenv("TEST_ANALYSIS_CAPTURE_FILE"); output != "" {
		capture, err := common.Marshal(map[string]any{"synthetic_test": true, "endpoint": endpoint, "model": modelName, "status": stored.Status, "error": stored.Error, "state": state, "result": result})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(output, []byte(strings.ReplaceAll(string(capture), key, "[redacted]")), 0600))
	}
	t.Logf("live workflow status=%s error=%s calls=%d chunks=%d/%d scanned=%d", stored.Status, stored.Error, state.CallsUsed, state.CompletedChunks, state.TotalChunks, state.ScannedRows)
	for _, attempt := range state.Attempts {
		t.Logf("attempt=%d step=%s http=%d finish=%s code=%s duration_ms=%d input_tokens_estimate=%d", attempt.Number, attempt.Step, attempt.StatusCode, attempt.FinishReason, attempt.Code, attempt.DurationMs, attempt.InputTokens)
	}
	require.Equal(t, model.SystemTaskStatusSucceeded, stored.Status, stored.Error)
	require.True(t, result.Complete)
	require.NotNil(t, result.Coverage)
	assert.True(t, result.Coverage.Complete)
	require.NotNil(t, result.Snapshot)
	require.NotNil(t, result.Comparison)
	assert.Equal(t, 20, result.Snapshot.Requests)
	assert.Equal(t, 6, result.Snapshot.Errors)
	assert.Equal(t, 17, result.Comparison.Requests)
	assert.Equal(t, 3, result.Comparison.Errors)
	assert.True(t, result.ComparisonAvailable)
	assert.GreaterOrEqual(t, state.CompletedChunks, 2)
	assert.Equal(t, state.TotalChunks, state.CompletedChunks)
	assert.GreaterOrEqual(t, state.CallsUsed, 6)
	assert.LessOrEqual(t, state.CallsUsed, config.MaxCalls)
	assert.NotEmpty(t, result.Summary)
	assert.Contains(t, result.Report, result.Summary)
	assert.False(t, strings.Contains(result.Report, key), "report must not contain the provider credential")
	t.Logf("synthetic requests current=%d previous=%d; report length=%d bytes", result.Snapshot.Requests, result.Comparison.Requests, len(result.Report))
}

func TestErrorAnalysisRetainsUpstreamEvidenceWithoutChangingResponse(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{StartTime: time.Now(), ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 7}}
	raw := "data: {\"usage\":{\"prompt_tokens\":118353,\"completion_tokens\":27,\"prompt_tokens_details\":{\"cached_tokens\":100}}}\n\ndata: {\"error\":{\"code\":\"bad_response\",\"message\":\"tool transport correction changed an operation; no tool was executed\",\"api_key\":\"private-provider-key\"},\"prompt\":\"private input\"}\n\n"
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, common.RequestIdKey: {"upstream-1"}}, Body: io.NopCloser(strings.NewReader(raw))}
	ObserveUpstreamResponse(c, info, response)
	received, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	assert.Equal(t, raw, string(received), "diagnostic capture must not change forwarded bytes")
	other := model.NewLogOther()
	AppendRelayLogAdminInfo(c, info, other)
	row := model.ClassifyFailureLog(model.Log{Type: model.LogTypeError, Other: other.JSONString()})
	require.NotNil(t, row.Evidence)
	require.Len(t, row.Evidence.Upstream, 1)
	attempt := row.Evidence.Upstream[0]
	assert.Equal(t, "upstream-1", attempt.RequestID)
	assert.Contains(t, string(attempt.Usage["usage"]), `"prompt_tokens":118353`)
	assert.Contains(t, attempt.ErrorBody, "tool transport correction changed an operation; no tool was executed")
	assert.NotContains(t, attempt.ErrorBody, "private-provider-key")
	assert.NotContains(t, attempt.ErrorBody, "private input")
	assert.False(t, attempt.CaptureIncomplete)
	assert.Contains(t, row.Categories, "upstream_usage_unbilled")
	// A retry without a provider ID must not inherit the preceding attempt's ID.
	next := &http.Response{StatusCode: 502, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("gateway unavailable"))}
	ObserveUpstreamResponse(c, info, next)
	_, err = io.ReadAll(next.Body)
	require.NoError(t, err)
	assert.Empty(t, c.GetString(common.UpstreamRequestIdKey))
	other = model.NewLogOther()
	AppendRelayLogAdminInfo(c, info, other)
	row = model.ClassifyFailureLog(model.Log{Type: model.LogTypeError, Other: other.JSONString()})
	require.Len(t, row.Evidence.Upstream, 2)
	assert.Equal(t, "gateway unavailable", row.Evidence.Upstream[1].ErrorBody)
	assert.Equal(t, "upstream-1", row.Evidence.Upstream[0].RequestID)
}

func TestErrorAnalysisDeliveryAndSettlementClassification(t *testing.T) {
	for _, tc := range []struct {
		name, body                         string
		risk, completed, usage, incomplete bool
	}{
		{name: "content followed by disconnect", body: "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n", risk: true},
		{name: "usage permits settlement without terminal", body: "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n", usage: true},
		{name: "complete terminal permits settlement without usage", body: "data: [DONE]\n\n", completed: true},
		{name: "partial terminal is unknown", body: "data: [DONE]\n", incomplete: true},
		{name: "error after usage remains a risk", body: "data: {\"usage\":{\"completion_tokens\":2}}\n\ndata: {\"error\":{\"message\":\"failed\"}}\n\n", risk: true, usage: true},
		{name: "claude terminal", body: "data: {\"type\":\"message_stop\"}\n\n", completed: true},
		{name: "responses terminal", body: "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10}}}\n\n", completed: true, usage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
			c.Writer.Header().Set("Content-Type", "text/event-stream")
			ObserveSchedulingResponse(c)
			// Arbitrary transport boundaries must not turn partial terminal bytes into completion.
			split := len(tc.body) / 2
			_, err := c.Writer.WriteString(tc.body[:split])
			require.NoError(t, err)
			_, err = c.Writer.WriteString(tc.body[split:])
			require.NoError(t, err)
			other := model.NewLogOther()
			AppendRelayLogAdminInfo(c, &relaycommon.RelayInfo{StartTime: time.Now()}, other)
			entry := model.ClassifyFailureLog(model.Log{Type: model.LogTypeConsume, Quota: 500, Other: other.JSONString()})
			require.NotNil(t, entry.Evidence.Downstream)
			assert.Equal(t, tc.completed, entry.Evidence.Downstream.Completed)
			assert.Equal(t, tc.usage, len(entry.Evidence.Downstream.Usage) > 0)
			assert.Equal(t, tc.incomplete, entry.Evidence.Downstream.CaptureIncomplete)
			assert.Equal(t, tc.risk, slices.Contains(entry.Categories, "settlement_risk"))
		})
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/v1/realtime", nil)
	ObserveSchedulingResponse(c)
	other := model.NewLogOther()
	AppendRelayLogAdminInfo(c, &relaycommon.RelayInfo{StartTime: time.Now(), RelayFormat: types.RelayFormatOpenAIRealtime}, other)
	realtime := model.ClassifyFailureLog(model.Log{Type: model.LogTypeConsume, Quota: 500, Other: other.JSONString()})
	assert.Nil(t, realtime.Evidence.Downstream, "HTTP observation cannot establish WebSocket delivery")
	assert.NotContains(t, realtime.Categories, "settlement_risk")
	legacy := model.ClassifyFailureLog(model.Log{Type: model.LogTypeConsume, Quota: 500, Other: `{}`})
	assert.NotContains(t, legacy.Categories, "settlement_risk", "missing history is not proof of a settlement gap")
	zero := model.ClassifyFailureLog(model.Log{Type: model.LogTypeConsume, Quota: 500, Other: `{"admin_info":{"failure_diagnostic":{"has_output":false}}}`})
	assert.Contains(t, zero.Categories, "charged_zero_output")
}

func TestErrorAnalysisCaptureBoundsAndSplitUsage(t *testing.T) {
	var parser responseEvidenceParser
	parser.feed([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":8}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n"), true)
	assert.JSONEq(t, `{"input_tokens":10,"cache_read_input_tokens":8}`, string(parser.usage["message.usage"]))
	assert.JSONEq(t, `{"output_tokens":3}`, string(parser.usage["usage"]))
	var precise responseEvidenceParser
	precise.feed([]byte(`{"usage":{"input_tokens":1234567890123456789,"cost":0.1234567890123456789}}`), false)
	assert.Contains(t, string(precise.usage["usage"]), `1234567890123456789`)
	assert.Contains(t, string(precise.usage["usage"]), `0.1234567890123456789`)
	snapshot := cloneEvidenceUsage(parser.usage)
	parser.feed([]byte("data: {\"usage\":{\"output_tokens\":4}}\n\n"), true)
	assert.JSONEq(t, `{"output_tokens":3}`, string(snapshot["usage"]), "persisted snapshots must not alias mutable usage")
	parser.feed([]byte("data: "+strings.Repeat("x", evidenceFrameLimit)+"\n\ndata: {\"usage\":{\"output_tokens\":5}}\n\ndata: [DONE]\n\n"), true)
	assert.True(t, parser.incomplete)
	assert.True(t, parser.completed)
	assert.JSONEq(t, `{"output_tokens":5}`, string(parser.usage["usage"]), "a large output frame must not hide later usage")
	safe, truncated := model.SafeFailureDetail(`{"error":{"message":"see https://example.com/private?q=key"},"headers":{"Authorization":"private-key"}}`, 2048)
	assert.True(t, gjson.Valid(safe), "redaction must preserve JSON structure")
	assert.False(t, truncated)
	assert.NotContains(t, safe, "private-key")
	safe, truncated = model.SafeFailureDetail(strings.Repeat("错", evidenceErrorLimit+1), evidenceErrorLimit)
	assert.True(t, truncated)
	assert.True(t, utf8.ValidString(safe))
	assert.Len(t, []rune(safe), evidenceErrorLimit)
	assert.False(t, model.HasReportedUsage(map[string]json.RawMessage{"usage": []byte(`{"input_tokens":0,"output_tokens":0,"service_tier":"default","unknown":null}`)}))
}

func TestErrorAnalysisPartialWriteDoesNotProveDelivery(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	c.Writer = &analysisFailWriter{ResponseWriter: c.Writer}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	ObserveSchedulingResponse(c)
	_, err := c.Writer.WriteString("data: [DONE]\n\n")
	require.Error(t, err)
	other := model.NewLogOther()
	AppendRelayLogAdminInfo(c, &relaycommon.RelayInfo{StartTime: time.Now()}, other)
	row := model.ClassifyFailureLog(model.Log{Type: model.LogTypeConsume, Quota: 100, Other: other.JSONString()})
	assert.True(t, row.Evidence.Downstream.WriteFailed)
	assert.Contains(t, row.Categories, "settlement_risk")
}

type analysisFailWriter struct{ gin.ResponseWriter }

func (w *analysisFailWriter) Write(body []byte) (int, error) { return len(body) - 1, io.ErrClosedPipe }

func TestErrorAnalysisSeparatesRecordedChargeFromConfirmedSettlement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		failed bool
		state  string
	}{
		{name: "pre-consumed amount is finalized", state: "confirmed"},
		{name: "settlement failed despite consumption log", failed: true, state: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
			info := &relaycommon.RelayInfo{StartTime: time.Now(), FinalPreConsumedQuota: 500}
			if tc.failed {
				info.Billing = &analysisFailedSettlement{}
			}
			err := SettleBilling(c, info, 500)
			if tc.failed {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			other := model.NewLogOther()
			AppendRelayLogAdminInfo(c, info, other)
			entry := model.ClassifyFailureLog(model.Log{Type: model.LogTypeConsume, Quota: 500, Other: other.JSONString()})
			require.NotNil(t, entry.Evidence.Local)
			assert.Equal(t, tc.state, entry.Evidence.Local.State)
			if tc.failed {
				assert.Zero(t, entry.Evidence.Local.ConfirmedQuota)
				assert.Contains(t, entry.Categories, "settlement_risk")
			} else {
				assert.EqualValues(t, 500, entry.Evidence.Local.ConfirmedQuota)
			}
		})
	}
}

func TestErrorAnalysisNonStreamingCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, body, code, content string
		status                    int
		retryable                 bool
	}{
		{"complete JSON", `{"choices":[{"message":{"content":"完整结论 test-key"},"finish_reason":"stop"}]}`, "", "完整结论 [redacted]", 200, false},
		{"empty content", `{"choices":[{"message":{"content":" "},"finish_reason":"stop"}]}`, "analysis_empty_report", "", 200, true},
		{"truncated output", `{"choices":[{"message":{"content":"unfinished"},"finish_reason":"length"}]}`, "analysis_incomplete_report", "", 200, false},
		{"missing completion reason", `{"choices":[{"message":{"content":"unfinished"}}]}`, "analysis_incomplete_report", "", 200, false},
		{"invalid JSON", `{"choices":[`, "analysis_invalid_response", "", 200, true},
		{"explicit context overflow", `{"error":{"code":"context_length_exceeded","message":"private provider error"}}`, "analysis_context_limit", "", 400, false},
		{"other invalid request", `{"error":{"code":"invalid_model","message":"private provider error"}}`, "analysis_invalid_request", "", 400, false},
		{"authentication failure", `{"error":{"message":"private provider error"}}`, "analysis_authentication_failed", "", 401, false},
		{"temporary upstream failure", `{"error":{"message":"private provider error"}}`, "analysis_http_error", "", 503, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: analysisTestTransport(func(request *http.Request) (*http.Response, error) {
				calls++
				var payload struct {
					Stream    bool `json:"stream"`
					MaxTokens int  `json:"max_tokens"`
				}
				require.NoError(t, common.DecodeJson(request.Body, &payload))
				assert.False(t, payload.Stream)
				assert.Equal(t, 2048, payload.MaxTokens)
				assert.Equal(t, "application/json", request.Header.Get("Accept"))
				assert.Equal(t, "Bearer test-key", request.Header.Get("Authorization"))
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: request}, nil
			})}
			attempt := &AnalysisAttempt{}
			content, err := completeErrorAnalysis(context.Background(), client, ErrorAnalysisConfig{Endpoint: "https://analysis.example/v1/chat/completions", Model: "test", TimeoutSeconds: 30}, "test-key", "Use supplied evidence.", "One failed request.", attempt)
			assert.Equal(t, 1, calls)
			assert.Equal(t, tc.status, attempt.StatusCode)
			assert.Positive(t, attempt.InputTokens)
			assert.Equal(t, tc.content, content)
			if tc.code == "" {
				require.NoError(t, err)
				assert.Equal(t, "stop", attempt.FinishReason)
				return
			}
			var failure *analysisFailure
			require.ErrorAs(t, err, &failure)
			assert.Equal(t, tc.code, failure.Code)
			assert.Equal(t, tc.retryable, failure.Retryable)
			assert.NotContains(t, err.Error(), "private provider error")
		})
	}
	t.Run("input budget is enforced before sending", func(t *testing.T) {
		calls := 0
		client := &http.Client{Transport: analysisTestTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("must not send an oversized request")
		})}
		config := normalizeAnalysisBudgets(ErrorAnalysisConfig{Endpoint: "https://analysis.example/v1/chat/completions", Model: "test", TimeoutSeconds: 30, ChunkInputTokens: 256})
		_, err := completeErrorAnalysis(context.Background(), client, config, "key", "Use supplied facts.", strings.Repeat("错误证据", 200), &AnalysisAttempt{})
		require.EqualError(t, err, "analysis_context_limit")
		assert.Zero(t, calls)
	})
	for _, tc := range []struct {
		name     string
		delay    time.Duration
		wantCode string
	}{
		{"silent generation may exceed legacy idle timeout", 20 * time.Second, ""},
		{"whole request deadline still applies", 40 * time.Second, "analysis_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &http.Client{Transport: analysisTestTransport(func(request *http.Request) (*http.Response, error) {
					timer := time.NewTimer(tc.delay)
					defer timer.Stop()
					select {
					case <-request.Context().Done():
						return nil, request.Context().Err()
					case <-timer.C:
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"complete"},"finish_reason":"stop"}]}`)), Request: request}, nil
					}
				})}
				content, err := completeErrorAnalysis(context.Background(), client, ErrorAnalysisConfig{Endpoint: "https://analysis.example/v1/chat/completions", Model: "test", TimeoutSeconds: 30, IdleSeconds: 10}, "key", "system", "input", &AnalysisAttempt{})
				if tc.wantCode == "" {
					require.NoError(t, err)
					assert.Equal(t, "complete", content)
				} else {
					require.EqualError(t, err, tc.wantCode)
					assert.Empty(t, content)
				}
			})
		})
	}
}

func setupErrorAnalysisWorkflowDB(t *testing.T, dialects ...gorm.Dialector) *gorm.DB {
	t.Helper()
	var dialect gorm.Dialector = sqlite.Open(filepath.Join(t.TempDir(), "workflow.db"))
	if len(dialects) > 0 {
		dialect = dialects[0]
	}
	db, err := gorm.Open(dialect, &gorm.Config{})
	require.NoError(t, err)
	// Explicit TEST_ANALYSIS_* DSNs are disposable databases, never application DSNs.
	require.NoError(t, db.Migrator().DropTable(&model.ErrorAnalysisArtifact{}, &model.SystemTaskLock{}, &model.SystemTask{}, &model.Log{}, &model.Option{}))
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.Option{}, &model.SystemTask{}, &model.SystemTaskLock{}, &model.ErrorAnalysisArtifact{}))
	oldDB, oldLog := model.DB, model.LOG_DB
	oldMainType, oldLogType := common.MainDatabaseType(), common.LogDatabaseType()
	oldConsume, oldErrors := common.LogConsumeEnabled, constant.ErrorLogEnabled
	model.DB, model.LOG_DB = db, db
	common.SetDatabaseTypes(common.DatabaseType(db.Dialector.Name()), common.DatabaseType(db.Dialector.Name()))
	common.LogConsumeEnabled, constant.ErrorLogEnabled = true, true
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLog
		common.SetDatabaseTypes(oldMainType, oldLogType)
		common.LogConsumeEnabled, constant.ErrorLogEnabled = oldConsume, oldErrors
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	})
	return db
}

func TestErrorAnalysisWorkflowResumesSavedResults(t *testing.T) {
	db := setupErrorAnalysisWorkflowDB(t)
	t.Setenv("CRYPTO_SECRET", "workflow-test-secret-not-for-production")
	config, err := GetErrorAnalysisConfig()
	require.NoError(t, err)
	config.MaxCalls, config.MaxAttempts, config.Timezone = 2, 1, "UTC"
	require.NoError(t, SaveErrorAnalysisConfig(config, "workflow-key", false))
	start := time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC).Unix()
	filter := model.ErrorAnalysisFilter{Start: start, End: start + 7*86400}
	require.NoError(t, db.Create(&[]model.Log{
		{RequestId: "previous-success", Type: model.LogTypeConsume, CreatedAt: start - 3600, ModelName: "test-model", ChannelId: 4, CompletionTokens: 1},
		{RequestId: "current-error", Type: model.LogTypeError, CreatedAt: start + 3600, ModelName: "test-model", ChannelId: 4, Other: `{"error_code":"upstream_timeout"}`, Content: "original upstream timeout"},
		{RequestId: "current-success", Type: model.LogTypeConsume, CreatedAt: start + 7200, ModelName: "test-model", ChannelId: 4, CompletionTokens: 1},
	}).Error)
	task, created, err := StartErrorAnalysis(filter)
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(t, int64(2), gjson.Get(task.Payload, "workflow_version").Int())
	claimed, ok, err := model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, "workflow-first", time.Now().Unix()+600)
	require.NoError(t, err)
	require.True(t, ok)
	calls := 0
	client := &http.Client{Transport: analysisTestTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		var payload struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, common.DecodeJson(request.Body, &payload))
		assert.False(t, payload.Stream)
		require.Len(t, payload.Messages, 2)
		content := "根据证据检查渠道响应耗时，当前原因尚未确认。"
		input := gjson.Parse(payload.Messages[1].Content)
		channel := input.Get("channel_comparisons.0")
		if strings.Contains(payload.Messages[0].Content, "Return ONLY a JSON object") {
			assert.Equal(t, "2024-01-08 00:00", input.Get("current_window.start").String())
			assert.Equal(t, "2024-01-15 00:00", input.Get("current_window.end").String())
			assert.Equal(t, "2024-01-01 00:00", input.Get("previous_window.start").String())
			assert.Equal(t, "2024-01-08 00:00", input.Get("previous_window.end").String())
			assert.Equal(t, "UTC", input.Get("timezone").String())
			assert.NotEmpty(t, input.Get("first_seen_local").String())
			assert.NotEmpty(t, input.Get("count_scope").String())
			assert.Equal(t, int64(1), input.Get("current.requests").Int(), "the error-group count must remain distinct from channel traffic")
			channel = input.Get("channel_comparison")
			finding, err := common.Marshal(analysisFinding{Title: "渠道超时", Facts: []string{"请求发生上游超时"}, Causes: []string{"上游响应可能较慢"}, Checks: []string{"检查渠道响应耗时"}, Missing: []string{"缺少上游服务状态"}, EvidenceIDs: []string{"current-error"}})
			require.NoError(t, err)
			content = string(finding)
		}
		assert.Equal(t, "4", channel.Get("name").String())
		assert.Equal(t, int64(2), channel.Get("current.requests").Int())
		assert.Equal(t, int64(1), channel.Get("current.affected").Int())
		assert.Equal(t, float64(50), channel.Get("current.affected_percent").Float())
		assert.Equal(t, int64(1), channel.Get("previous.requests").Int())
		assert.True(t, channel.Get("previous.affected_percent").Exists(), "a known zero rate must not be omitted")
		assert.Equal(t, float64(0), channel.Get("previous.affected_percent").Float())
		body, err := common.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}, "finish_reason": "stop"}}})
		require.NoError(t, err)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
	})}
	(errorAnalysisHandler{client: client}).Run(context.Background(), claimed, "workflow-first")
	paused, err := model.GetSystemTaskByTaskID(task.TaskID)
	require.NoError(t, err)
	assert.Equal(t, model.SystemTaskStatusFailed, paused.Status)
	assert.Equal(t, "analysis_call_budget_exhausted", paused.Error)
	var pausedState AnalysisState
	require.NoError(t, paused.DecodeState(&pausedState))
	assert.True(t, pausedState.CanResume)
	assert.Equal(t, 2, pausedState.CallsUsed)
	assert.Equal(t, 1, pausedState.CompletedChunks)
	assert.Equal(t, 2, calls)
	before, err := model.GetErrorAnalysisRequestEvidence(task.TaskID, "current-error")
	require.NoError(t, err)
	require.NotNil(t, before)
	require.NoError(t, db.Model(&model.Log{}).Where("request_id = ?", "current-error").Update("content", "changed live log").Error)
	require.NoError(t, db.Create(&model.Log{RequestId: "late-arrival", Type: model.LogTypeError, CreatedAt: start + 8000, ModelName: "test-model"}).Error)
	resumed, err := ResumeErrorAnalysis(task.TaskID)
	require.NoError(t, err)
	assert.Equal(t, task.TaskID, resumed.TaskID)
	var resumedState AnalysisState
	require.NoError(t, resumed.DecodeState(&resumedState))
	assert.Greater(t, resumedState.MaxCalls, pausedState.MaxCalls)
	assert.Equal(t, pausedState.CallsUsed, resumedState.CallsUsed)
	claimed, ok, err = model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, "workflow-second", time.Now().Unix()+600)
	require.NoError(t, err)
	require.True(t, ok)
	(errorAnalysisHandler{client: client}).Run(context.Background(), claimed, "workflow-second")
	completed, err := model.GetSystemTaskByTaskID(task.TaskID)
	require.NoError(t, err)
	assert.Equal(t, model.SystemTaskStatusSucceeded, completed.Status, completed.Error)
	assert.Equal(t, 4, calls, "the successful finding and section must not call the model again")
	var result AnalysisResult
	require.NoError(t, common.UnmarshalJsonStr(string(completed.Result), &result))
	assert.True(t, result.Complete)
	require.NotNil(t, result.Coverage)
	assert.True(t, result.Coverage.Complete)
	assert.Equal(t, 2, result.Snapshot.Requests)
	assert.Equal(t, 1, result.Snapshot.Errors)
	require.NotNil(t, result.Comparison)
	assert.Equal(t, 1, result.Comparison.Requests)
	assert.True(t, result.ComparisonAvailable)
	assert.NotEmpty(t, result.Summary)
	assert.Contains(t, result.Report, result.Summary)
	assert.NotContains(t, string(completed.Result), "workflow-key")
	after, err := model.GetErrorAnalysisRequestEvidence(task.TaskID, "current-error")
	require.NoError(t, err)
	assert.Equal(t, before, after, "report evidence must come from the saved snapshot")
	assert.NotContains(t, after.ErrorMessage, "changed live log")
}

func TestErrorAnalysisFindingRejectsUnrelatedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value analysisFinding
	}{
		{"unrelated citation", analysisFinding{Title: "Timeout", Facts: []string{"Observed timeout"}, EvidenceIDs: []string{"outside-snapshot"}}},
		{"unrelated follow-up", analysisFinding{Title: "Timeout", Facts: []string{"Observed timeout"}, FollowUpIDs: []string{"outside-snapshot"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, err := common.Marshal(tc.value)
			require.NoError(t, err)
			_, err = decodeAnalysisFinding(string(text), []string{"allowed-request"})
			require.EqualError(t, err, "analysis_invalid_finding")
		})
	}
}

func TestErrorAnalysisWorkflowCollectionDatabaseMatrix(t *testing.T) {
	for _, kind := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			var dialect gorm.Dialector = sqlite.Open(filepath.Join(t.TempDir(), "collection.db"))
			if kind != "sqlite" {
				dsn := os.Getenv("TEST_ANALYSIS_" + strings.ToUpper(kind) + "_DSN")
				if dsn == "" {
					t.Skip("disposable database DSN is not configured")
				}
				if kind == "mysql" {
					dialect = mysql.Open(dsn)
				} else {
					dialect = postgres.Open(dsn)
				}
			}
			db := setupErrorAnalysisWorkflowDB(t, dialect)
			var version string
			versionQuery := "SELECT version()"
			if kind == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("database version: %s", version)
			start := time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC).Unix()
			filter := model.ErrorAnalysisFilter{Start: start, End: start + 7*86400}
			require.NoError(t, db.Create(&[]model.Log{
				{RequestId: "previous-only", Type: model.LogTypeError, CreatedAt: start - 3600, ModelName: "test-model", Other: `{"error_code":"timeout"}`},
				{RequestId: "shared", Type: model.LogTypeError, CreatedAt: start - 1, ModelName: "test-model", ChannelId: 1, Other: `{"error_code":"timeout"}`},
				{RequestId: "independent", Type: model.LogTypeConsume, CreatedAt: start + 200, ModelName: "test-model", CompletionTokens: 1},
				{RequestId: "shared", Type: model.LogTypeConsume, CreatedAt: start + 100, ModelName: "test-model", ChannelId: 2, CompletionTokens: 1},
				{RequestId: "SHARED", Type: model.LogTypeError, CreatedAt: start + 300, ModelName: "test-model", ChannelId: 3, Other: `{"error_code":"timeout"}`},
			}).Error)
			task, err := model.CreateSystemTask(model.ErrorAnalysisTaskType, errorAnalysisPayload{ErrorAnalysisFilter: filter, WorkflowVersion: 2}, AnalysisState{Version: 2, CallsUsed: 1, MaxCalls: 10})
			require.NoError(t, err)
			_, ok, err := model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, "collection-first", time.Now().Unix()+600)
			require.NoError(t, err)
			require.True(t, ok)
			require.NoError(t, model.SaveErrorAnalysisArtifact(task.TaskID, "collection-first", "config", "snapshot", map[string]any{"timezone": "Asia/Shanghai", "max_calls": 10}))
			progress, err := model.CollectErrorAnalysisPage(context.Background(), task.TaskID, "collection-first", filter, 2)
			require.NoError(t, err)
			assert.False(t, progress.Done)
			assert.EqualValues(t, 2, progress.ScannedRows)
			assert.EqualValues(t, 5, progress.TotalRows)
			originalSource := os.Getenv("LOG_SQL_DSN")
			t.Setenv("LOG_SQL_DSN", "postgres://analysis.invalid/changed-source")
			_, err = model.CollectErrorAnalysisPage(context.Background(), task.TaskID, "collection-first", filter, 2)
			require.ErrorIs(t, err, model.ErrErrorAnalysisSourceChanged)
			var unchanged model.ErrorAnalysisScanProgress
			found, err := model.GetErrorAnalysisArtifact(task.TaskID, "scan", "checkpoint", &unchanged)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, progress.ScannedRows, unchanged.ScannedRows)
			assert.Equal(t, progress.CursorID, unchanged.CursorID)
			t.Setenv("LOG_SQL_DSN", originalSource)
			// A later arrival is outside the frozen ID watermark even if its event time matches.
			require.NoError(t, db.Create(&model.Log{RequestId: "late-arrival", Type: model.LogTypeError, CreatedAt: start + 400}).Error)
			require.NoError(t, db.Model(&model.SystemTaskLock{}).Where("task_id = ?", task.TaskID).Update("locked_until", time.Now().Unix()-1).Error)
			require.NoError(t, model.ExpireStaleSystemTaskLocks(time.Now().Unix()))
			recovered, err := model.GetSystemTaskByTaskID(task.TaskID)
			require.NoError(t, err)
			assert.Equal(t, model.SystemTaskStatusPending, recovered.Status)
			var recoveredState AnalysisState
			require.NoError(t, recovered.DecodeState(&recoveredState))
			assert.Equal(t, 1, recoveredState.CallsUsed)
			assert.Equal(t, 10, recoveredState.MaxCalls, "automatic recovery must not grant more model calls")
			_, ok, err = model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, "collection-second", time.Now().Unix()+600)
			require.NoError(t, err)
			require.True(t, ok)
			assert.ErrorIs(t, model.SaveErrorAnalysisArtifact(task.TaskID, "collection-first", "finding", "stale", map[string]string{"content": "stale worker"}), model.ErrSystemTaskLockLost)
			for !progress.Done {
				progress, err = model.CollectErrorAnalysisPage(context.Background(), task.TaskID, "collection-second", filter, 2)
				require.NoError(t, err)
			}
			assert.EqualValues(t, 5, progress.ScannedRows)
			t.Setenv("LOG_SQL_DSN", "postgres://analysis.invalid/changed-source")
			completedScan, err := model.CollectErrorAnalysisPage(context.Background(), task.TaskID, "collection-second", filter, 2)
			require.NoError(t, err)
			assert.True(t, completedScan.Done, "a completed snapshot no longer needs the live source")
			t.Setenv("LOG_SQL_DSN", originalSource)
			for {
				aggregation, err := model.AggregateErrorAnalysisPage(context.Background(), task.TaskID, "collection-second", 1, 3)
				require.NoError(t, err)
				if aggregation.Done {
					break
				}
			}
			snapshot, err := model.LoadErrorAnalysisWorkflowSnapshot(task.TaskID)
			require.NoError(t, err)
			require.NotNil(t, snapshot)
			assert.True(t, snapshot.Coverage.Complete)
			assert.Equal(t, 3, snapshot.Current.Requests)
			assert.Equal(t, 1, snapshot.Current.Errors)
			assert.Equal(t, 1, snapshot.Current.Recovered)
			require.NotNil(t, snapshot.Previous)
			assert.Equal(t, 1, snapshot.Previous.Requests, "a cross-period retry belongs to its representative final request only")
			assert.Equal(t, 1, snapshot.Previous.Errors)
			assert.True(t, snapshot.ComparisonAvailable)
			entry, err := model.GetErrorAnalysisRequestEvidence(task.TaskID, "shared")
			require.NoError(t, err)
			require.NotNil(t, entry)
			assert.Equal(t, 2, entry.Attempts)
			assert.Equal(t, 2, entry.Channel)
			assert.Contains(t, entry.Categories, "recovered")
			require.Len(t, entry.Records, 2)
			assert.Equal(t, 1, entry.Records[1].Channel)
			assert.Equal(t, "Asia/Shanghai", snapshot.Timezone)
			require.NotEmpty(t, snapshot.Current.Timeline)
			location, err := time.LoadLocation("Asia/Shanghai")
			require.NoError(t, err)
			day, err := time.ParseInLocation("2006-01-02", "2024-01-08", location)
			require.NoError(t, err)
			assert.Equal(t, fmt.Sprint(day.Unix()), snapshot.Current.Timeline[0].Name)
			require.NoError(t, model.FinishSystemTask(task.TaskID, "collection-second", model.SystemTaskStatusFailed, nil, "analysis_budget_too_small"))
			_, err = model.ResumeErrorAnalysisTask(task.TaskID)
			assert.ErrorIs(t, err, model.ErrErrorAnalysisResumeUnavailable, "a failed task without resumable state cannot receive more calls")
			for _, timezone := range []string{"UTC", "America/New_York"} {
				location, err := time.LoadLocation(timezone)
				require.NoError(t, err)
				currentStart := time.Date(2024, 3, 10, 0, 0, 0, 0, location)
				filter := model.ErrorAnalysisFilter{Start: currentStart.Unix(), End: currentStart.AddDate(0, 0, 7).Unix()}
				task, err := model.CreateSystemTask(model.ErrorAnalysisTaskType, errorAnalysisPayload{ErrorAnalysisFilter: filter, WorkflowVersion: 2}, AnalysisState{Version: 2})
				require.NoError(t, err)
				_, claimed, err := model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, "calendar-window", time.Now().Unix()+600)
				require.NoError(t, err)
				require.True(t, claimed)
				require.NoError(t, model.SaveErrorAnalysisArtifact(task.TaskID, "calendar-window", "config", "snapshot", map[string]any{"timezone": timezone, "max_calls": 10}))
				progress, err := model.CollectErrorAnalysisPage(context.Background(), task.TaskID, "calendar-window", filter, 2)
				require.NoError(t, err)
				assert.Equal(t, time.Date(2024, 3, 3, 0, 0, 0, 0, location).Unix(), progress.PreviousFilter.Start, "previous calendar week must begin at midnight across DST")
				assert.Equal(t, currentStart.Unix(), progress.PreviousFilter.End)
				require.NoError(t, model.FinishSystemTask(task.TaskID, "calendar-window", model.SystemTaskStatusSucceeded, nil, ""))
			}
		})
	}
}

func TestErrorAnalysisWorkflowClickHouseSameKeyRows(t *testing.T) {
	dsn := os.Getenv("TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		t.Skip("TEST_CLICKHOUSE_DSN disposable log database is not configured")
	}
	setupErrorAnalysisWorkflowDB(t)
	logDB, err := gorm.Open(clickhouse.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := logDB.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	var version string
	require.NoError(t, logDB.Raw("SELECT version()").Scan(&version).Error)
	t.Logf("ClickHouse version: %s", version)
	require.NoError(t, logDB.Exec("DROP TABLE IF EXISTS logs").Error)
	require.NoError(t, logDB.Exec(`CREATE TABLE logs (
		id Int64 DEFAULT 0, created_at Int64, type Int32, model_name String DEFAULT '',
		channel_id Int32 DEFAULT 0, request_id String DEFAULT '', upstream_request_id String DEFAULT '',
		prompt_tokens Int32 DEFAULT 0, completion_tokens Int32 DEFAULT 0, quota Int32 DEFAULT 0,
		content String DEFAULT '', use_time Int32 DEFAULT 0, is_stream UInt8 DEFAULT 0, other String DEFAULT ''
	) ENGINE = MergeTree ORDER BY (created_at, request_id)`).Error)
	start := time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC).Unix()
	for _, record := range []struct {
		request string
		kind    int
		output  int
	}{
		{"same-key", model.LogTypeError, 0},
		{"same-key", model.LogTypeConsume, 1},
		{"", model.LogTypeError, 0},
		{"", model.LogTypeError, 0},
	} {
		require.NoError(t, logDB.Exec("INSERT INTO logs (created_at, type, request_id, completion_tokens, other) VALUES (?, ?, ?, ?, ?)", start+1, record.kind, record.request, record.output, `{"error_code":"upstream_error"}`).Error)
	}
	model.LOG_DB = logDB
	common.SetLogDatabaseType(common.DatabaseTypeClickHouse)
	filter := model.ErrorAnalysisFilter{Start: start, End: start + 86400}
	task, err := model.CreateSystemTask(model.ErrorAnalysisTaskType, errorAnalysisPayload{ErrorAnalysisFilter: filter, WorkflowVersion: 2}, AnalysisState{Version: 2})
	require.NoError(t, err)
	_, ok, err := model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, "clickhouse-test", time.Now().Unix()+600)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, model.SaveErrorAnalysisArtifact(task.TaskID, "clickhouse-test", "config", "snapshot", map[string]string{"timezone": "UTC"}))
	progress, err := model.CollectErrorAnalysisPage(context.Background(), task.TaskID, "clickhouse-test", filter, 2)
	require.NoError(t, err)
	assert.True(t, progress.Done)
	assert.EqualValues(t, 4, progress.ScannedRows)
	assert.EqualValues(t, 4, progress.TotalRows)
	for {
		aggregation, err := model.AggregateErrorAnalysisPage(context.Background(), task.TaskID, "clickhouse-test", 1, 2)
		require.NoError(t, err)
		if aggregation.Done {
			break
		}
	}
	snapshot, err := model.LoadErrorAnalysisWorkflowSnapshot(task.TaskID)
	require.NoError(t, err)
	assert.True(t, snapshot.Coverage.Complete)
	assert.Equal(t, 3, snapshot.Current.Requests, "same-key retries merge, but two anonymous events remain distinct")
	assert.Equal(t, 2, snapshot.Current.Errors)
	assert.Equal(t, 1, snapshot.Current.Recovered)
	assert.False(t, snapshot.ComparisonAvailable)
	entry, err := model.GetErrorAnalysisRequestEvidence(task.TaskID, "same-key")
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, 2, entry.Attempts)
}

func TestErrorAnalysisWorkflowCollectionPassesLegacyLimit(t *testing.T) {
	db := setupErrorAnalysisWorkflowDB(t)
	start := time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC).Unix()
	filter := model.ErrorAnalysisFilter{Start: start, End: start + 86400}
	// The exact 50,001-row boundary protects against restoring the old 50k cap.
	logs := make([]model.Log, 50001)
	for i := range logs {
		logs[i] = model.Log{RequestId: fmt.Sprintf("coverage-%05d", i), Type: model.LogTypeConsume, CreatedAt: start + 1, CompletionTokens: 1}
	}
	require.NoError(t, db.CreateInBatches(&logs, 250).Error)
	task, err := model.CreateSystemTask(model.ErrorAnalysisTaskType, errorAnalysisPayload{ErrorAnalysisFilter: filter, WorkflowVersion: 2}, AnalysisState{Version: 2})
	require.NoError(t, err)
	_, ok, err := model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, "coverage-test", time.Now().Unix()+600)
	require.NoError(t, err)
	require.True(t, ok)
	for {
		progress, err := model.CollectErrorAnalysisPage(context.Background(), task.TaskID, "coverage-test", filter, 1000)
		require.NoError(t, err)
		if progress.Done {
			assert.True(t, progress.Stable)
			assert.EqualValues(t, len(logs), progress.ScannedRows)
			assert.EqualValues(t, len(logs), progress.TotalRows)
			break
		}
	}
	last, err := model.GetErrorAnalysisRequestEvidence(task.TaskID, "coverage-50000")
	require.NoError(t, err)
	require.NotNil(t, last, "the request beyond the legacy limit must be retained")
	assert.Equal(t, "coverage-50000", last.RequestID)
}

func analysisWorkflowTestResponse(t *testing.T, request *http.Request, content any, finish string) (*http.Response, error) {
	t.Helper()
	text, ok := content.(string)
	if !ok {
		encoded, err := common.Marshal(content)
		require.NoError(t, err)
		text = string(encoded)
	}
	body, err := common.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": text}, "finish_reason": finish}}})
	require.NoError(t, err)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
}

func TestErrorAnalysisWorkflowResumesSizeFailureBySplitting(t *testing.T) {
	for _, sizeFailure := range []string{"context_limit", "length"} {
		t.Run(sizeFailure, func(t *testing.T) {
			db := setupErrorAnalysisWorkflowDB(t)
			t.Setenv("CRYPTO_SECRET", "split-workflow-test-secret-not-for-production")
			config, err := GetErrorAnalysisConfig()
			require.NoError(t, err)
			config.MaxCalls, config.MaxAttempts = 1, 1
			require.NoError(t, SaveErrorAnalysisConfig(config, "split-key", false))
			start := time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC).Unix()
			filter := model.ErrorAnalysisFilter{Start: start, End: start + 86400}
			require.NoError(t, db.Create(&[]model.Log{
				{RequestId: "split-A", Type: model.LogTypeError, CreatedAt: start + 1, ModelName: "test-model", Other: `{"error_code":"timeout"}`},
				{RequestId: "split-B", Type: model.LogTypeError, CreatedAt: start + 2, ModelName: "test-model", Other: `{"error_code":"timeout"}`},
			}).Error)
			parentCalls := 0
			childCalls := map[string]int{}
			client := &http.Client{Transport: analysisTestTransport(func(request *http.Request) (*http.Response, error) {
				var payload struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				require.NoError(t, common.DecodeJson(request.Body, &payload))
				require.Len(t, payload.Messages, 2)
				input := payload.Messages[1].Content
				if gjson.Get(input, "samples").IsArray() {
					var group model.ErrorAnalysisGroup
					require.NoError(t, common.UnmarshalJsonStr(input, &group))
					if len(group.Samples) == 2 {
						parentCalls++
						if sizeFailure == "length" {
							return analysisWorkflowTestResponse(t, request, "unfinished finding", "length")
						}
						return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"context_length_exceeded"}}`)), Request: request}, nil
					}
					require.Len(t, group.Samples, 1)
					id := group.Samples[0].RequestID
					childCalls[id]++
					return analysisWorkflowTestResponse(t, request, analysisFinding{Title: "Timeout", Facts: []string{id + " timed out"}, EvidenceIDs: []string{id}}, "stop")
				}
				if gjson.Parse(input).IsArray() {
					var findings []analysisFinding
					require.NoError(t, common.UnmarshalJsonStr(input, &findings))
					merged := analysisFinding{Title: "Timeouts"}
					for _, finding := range findings {
						merged.Facts = append(merged.Facts, finding.Facts...)
						merged.EvidenceIDs = append(merged.EvidenceIDs, finding.EvidenceIDs...)
					}
					return analysisWorkflowTestResponse(t, request, merged, "stop")
				}
				return analysisWorkflowTestResponse(t, request, "Review the two supplied timeout requests.", "stop")
			})}
			task, _, err := StartErrorAnalysis(filter)
			require.NoError(t, err)
			completed := false
			for run := range 10 {
				if run > 0 {
					task, err = ResumeErrorAnalysis(task.TaskID)
					require.NoError(t, err)
				}
				runner := fmt.Sprintf("split-worker-%d", run)
				claimed, ok, err := model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, runner, time.Now().Unix()+600)
				require.NoError(t, err)
				require.True(t, ok)
				(errorAnalysisHandler{client: client}).Run(context.Background(), claimed, runner)
				task, err = model.GetSystemTaskByTaskID(task.TaskID)
				require.NoError(t, err)
				assert.Equal(t, 1, parentCalls, "resuming must reuse the known size failure instead of sending the parent again")
				if task.Status == model.SystemTaskStatusSucceeded {
					completed = true
					break
				}
				require.Equal(t, "analysis_call_budget_exhausted", task.Error)
				if run == 1 {
					assert.Equal(t, 1, childCalls["split-A"]+childCalls["split-B"], "one granted call advances to a smaller child")
				}
			}
			require.True(t, completed, "repeated explicit one-call resumes must eventually finish the bounded workflow")
			assert.Equal(t, map[string]int{"split-A": 1, "split-B": 1}, childCalls)
			var result AnalysisResult
			require.NoError(t, common.UnmarshalJsonStr(string(task.Result), &result))
			assert.True(t, result.Complete)
			assert.Contains(t, result.Report, "split-A")
			assert.Contains(t, result.Report, "split-B")
		})
	}
}

func TestErrorAnalysisWorkflowFollowUpPreservesOtherFindings(t *testing.T) {
	db := setupErrorAnalysisWorkflowDB(t)
	t.Setenv("CRYPTO_SECRET", "follow-up-workflow-test-secret-not-for-production")
	config, err := GetErrorAnalysisConfig()
	require.NoError(t, err)
	config.MaxAttempts = 1
	require.NoError(t, SaveErrorAnalysisConfig(config, "follow-up-key", false))
	start := time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC).Unix()
	filter := model.ErrorAnalysisFilter{Start: start, End: start + 86400}
	logs := []model.Log{}
	for i := range 6 {
		logs = append(logs, model.Log{RequestId: "follow-A", Type: model.LogTypeError, CreatedAt: start + int64(i) + 1, ModelName: "test-model", Other: `{"error_code":"timeout"}`})
	}
	logs = append(logs, model.Log{RequestId: "follow-B", Type: model.LogTypeError, CreatedAt: start + 10, ModelName: "test-model", Other: `{"error_code":"timeout"}`})
	require.NoError(t, db.Create(&logs).Error)
	initialSeen, expandedSeen, mergedSeen := false, false, false
	client := &http.Client{Transport: analysisTestTransport(func(request *http.Request) (*http.Response, error) {
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, common.DecodeJson(request.Body, &payload))
		require.Len(t, payload.Messages, 2)
		input := payload.Messages[1].Content
		if gjson.Get(input, "samples").IsArray() {
			var group model.ErrorAnalysisGroup
			require.NoError(t, common.UnmarshalJsonStr(input, &group))
			if len(group.Samples) == 2 {
				initialSeen = true
				for _, sample := range group.Samples {
					if sample.RequestID == "follow-A" {
						assert.Less(t, len(sample.Records), sample.Attempts, "initial evidence is compacted")
					}
				}
				return analysisWorkflowTestResponse(t, request, analysisFinding{Title: "Two timeout requests", Facts: []string{"A timed out", "B independently failed"}, EvidenceIDs: []string{"follow-A", "follow-B"}, FollowUpIDs: []string{"follow-A"}}, "stop")
			}
			expandedSeen = true
			require.Len(t, group.Samples, 1)
			assert.Equal(t, "follow-A", group.Samples[0].RequestID)
			assert.Len(t, group.Samples[0].Records, 6, "follow-up must load the additional saved retry evidence")
			return analysisWorkflowTestResponse(t, request, analysisFinding{Title: "A retry evidence", Facts: []string{"A had six failed attempts"}, EvidenceIDs: []string{"follow-A"}}, "stop")
		}
		if gjson.Parse(input).IsArray() {
			mergedSeen = true
			var findings []analysisFinding
			require.NoError(t, common.UnmarshalJsonStr(input, &findings))
			merged := analysisFinding{Title: "Timeout investigation"}
			for _, finding := range findings {
				merged.Facts = append(merged.Facts, finding.Facts...)
				for _, id := range finding.EvidenceIDs {
					if !slices.Contains(merged.EvidenceIDs, id) {
						merged.EvidenceIDs = append(merged.EvidenceIDs, id)
					}
				}
			}
			assert.Contains(t, merged.Facts, "B independently failed")
			assert.Contains(t, merged.Facts, "A had six failed attempts")
			assert.ElementsMatch(t, []string{"follow-A", "follow-B"}, merged.EvidenceIDs)
			return analysisWorkflowTestResponse(t, request, merged, "stop")
		}
		return analysisWorkflowTestResponse(t, request, "Inspect the two retained request traces.", "stop")
	})}
	task, _, err := StartErrorAnalysis(filter)
	require.NoError(t, err)
	claimed, ok, err := model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, "follow-up-worker", time.Now().Unix()+600)
	require.NoError(t, err)
	require.True(t, ok)
	(errorAnalysisHandler{client: client}).Run(context.Background(), claimed, "follow-up-worker")
	stored, err := model.GetSystemTaskByTaskID(task.TaskID)
	require.NoError(t, err)
	require.Equal(t, model.SystemTaskStatusSucceeded, stored.Status, stored.Error)
	assert.True(t, initialSeen)
	assert.True(t, expandedSeen)
	assert.True(t, mergedSeen)
	var result AnalysisResult
	require.NoError(t, common.UnmarshalJsonStr(string(stored.Result), &result))
	assert.Contains(t, result.Report, "B independently failed")
	assert.Contains(t, result.Report, "A had six failed attempts")
	assert.Contains(t, result.Report, "follow-A")
	assert.Contains(t, result.Report, "follow-B")
}

func TestErrorAnalysisWorkflowNarrativeCompactsOversizedInput(t *testing.T) {
	for _, boundary := range []string{"local_budget", "provider_context"} {
		t.Run(boundary, func(t *testing.T) {
			setupErrorAnalysisWorkflowDB(t)
			config, err := GetErrorAnalysisConfig()
			require.NoError(t, err)
			config.MaxAttempts = 1
			if boundary == "local_budget" {
				config.ChunkInputTokens = 512
			}
			require.NoError(t, validateErrorAnalysisConfig(config))
			task, err := model.CreateSystemTask(model.ErrorAnalysisTaskType, errorAnalysisPayload{WorkflowVersion: 2}, AnalysisState{Version: 2})
			require.NoError(t, err)
			_, claimed, err := model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, "narrative-worker", time.Now().Unix()+600)
			require.NoError(t, err)
			require.True(t, claimed)
			facts := make([]string, 6)
			for i := range facts {
				facts[i] = strings.Repeat("The recorded request timed out upstream. ", 30)
			}
			input, err := common.Marshal(map[string]any{
				"current":                 map[string]int{"requests": 20, "affected": 5},
				"request_rate_comparison": []any{map[string]any{"category": "affected", "current_count": 5, "current_percent": 25.0, "previous_count": 2, "previous_percent": 10.0, "change_percentage_points": 15.0}},
				"findings":                analysisFinding{Title: "Timeouts", Facts: facts, EvidenceIDs: []string{"request-A", "request-B"}},
			})
			require.NoError(t, err)
			calls := 0
			client := &http.Client{Transport: analysisTestTransport(func(request *http.Request) (*http.Response, error) {
				calls++
				var payload struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				require.NoError(t, common.DecodeJson(request.Body, &payload))
				require.Len(t, payload.Messages, 2)
				content := payload.Messages[1].Content
				if boundary == "provider_context" && calls == 1 {
					assert.Equal(t, string(input), content)
					return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"context_length_exceeded"}}`)), Request: request}, nil
				}
				assert.Less(t, len(content), len(input), "the rejected overview must be compacted before another request")
				assert.True(t, gjson.Get(content, "overview_compacted").Bool())
				assert.JSONEq(t, gjson.GetBytes(input, "current").Raw, gjson.Get(content, "current").Raw)
				assert.JSONEq(t, gjson.GetBytes(input, "request_rate_comparison").Raw, gjson.Get(content, "request_rate_comparison").Raw, "compaction preserves host-calculated counts, percentages, and percentage-point changes")
				return analysisWorkflowTestResponse(t, request, "Five of twenty recorded requests were affected; check the timeout traces first.", "stop")
			})}
			workflow := &analysisWorkflow{ctx: context.Background(), task: task, runner: "narrative-worker", config: config, key: "narrative-key", client: client, state: AnalysisState{Version: 2, Phase: "summarizing", MaxCalls: config.MaxCalls}}
			content, err := workflow.writeNarrative("summary", "Summarize the supplied error evidence.", string(input))
			require.NoError(t, err)
			assert.Contains(t, content, "Five of twenty")
			if boundary == "local_budget" {
				assert.Equal(t, 1, calls, "an oversized local input must not reach the provider")
			} else {
				assert.Equal(t, 2, calls)
			}
		})
	}
}

func TestErrorAnalysisWorkflowResumesFailedFinalSave(t *testing.T) {
	db := setupErrorAnalysisWorkflowDB(t)
	t.Setenv("CRYPTO_SECRET", "final-save-workflow-test-secret-not-for-production")
	config, err := GetErrorAnalysisConfig()
	require.NoError(t, err)
	config.MaxAttempts = 1
	require.NoError(t, SaveErrorAnalysisConfig(config, "final-save-key", false))
	start := time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC).Unix()
	require.NoError(t, db.Create(&model.Log{RequestId: "recorded-success", Type: model.LogTypeConsume, CreatedAt: start + 1, CompletionTokens: 1}).Error)
	task, _, err := StartErrorAnalysis(model.ErrorAnalysisFilter{Start: start, End: start + 86400})
	require.NoError(t, err)
	failedOnce := false
	const callback = "analysis_final_save_failure_once"
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if failedOnce || tx.Statement.Table != "system_tasks" {
			return
		}
		updates, ok := tx.Statement.Dest.(map[string]any)
		if !ok {
			return
		}
		result, ok := updates["result"].(string)
		if ok && gjson.Get(result, "complete").Bool() && gjson.Get(result, "report").String() != "" {
			failedOnce = true
			tx.AddError(errors.New("analysis test final result write failure"))
		}
	}))
	t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove(callback)) })
	calls := 0
	client := &http.Client{Transport: analysisTestTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		return analysisWorkflowTestResponse(t, request, "The recorded request succeeded; no error evidence was found.", "stop")
	})}
	for _, runner := range []string{"save-first", "save-resumed"} {
		claimed, ok, err := model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, runner, time.Now().Unix()+600)
		require.NoError(t, err)
		require.True(t, ok)
		(errorAnalysisHandler{client: client}).Run(context.Background(), claimed, runner)
		task, err = model.GetSystemTaskByTaskID(task.TaskID)
		require.NoError(t, err)
		if runner == "save-first" {
			require.True(t, failedOnce)
			require.Equal(t, model.SystemTaskStatusFailed, task.Status)
			assert.Equal(t, "analysis_persistence_error", task.Error)
			var state AnalysisState
			require.NoError(t, task.DecodeState(&state))
			assert.True(t, state.CanResume)
			assert.Equal(t, "paused", state.Phase)
			task, err = ResumeErrorAnalysis(task.TaskID)
			require.NoError(t, err)
		} else {
			require.Equal(t, model.SystemTaskStatusSucceeded, task.Status, task.Error)
			assert.True(t, gjson.Get(string(task.Result), "complete").Bool())
			assert.Equal(t, 3, calls, "retrying the final save reuses all completed narrative calls")
		}
	}
}

type analysisFailedSettlement struct{ relaycommon.BillingSettler }

func (*analysisFailedSettlement) Settle(int) error         { return io.ErrClosedPipe }
func (*analysisFailedSettlement) GetPreConsumedQuota() int { return 500 }
