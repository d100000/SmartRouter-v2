package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
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
			// These are existing schemas; no new table or migration is introduced.
			require.NoError(t, db.AutoMigrate(&model.Log{}, &model.Option{}, &model.SystemTask{}, &model.SystemTaskLock{}))
			for _, row := range []any{&model.Log{}, &model.Option{}, &model.SystemTask{}, &model.SystemTaskLock{}} {
				require.NoError(t, db.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(row).Error)
			}
			now := time.Now().Unix()
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
			require.NoError(t, db.AutoMigrate(&model.Log{}, &model.Option{}, &model.SystemTask{}, &model.SystemTaskLock{}))
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
			second, _, err := StartErrorAnalysis(model.ErrorAnalysisFilter{Start: now - 20, End: now})
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
		name         string
		firstStatus  int
		firstBody    string
		wantStatus   model.SystemTaskStatus
		wantAttempts int
		alwaysFail   bool
		wantError    string
	}{
		{"rate limit recovers", 429, "", model.SystemTaskStatusSucceeded, 2, false, ""},
		{"interrupted draft is replaced", 200, "data: {\"choices\":[{\"delta\":{\"content\":\"abandoned draft\"}}]}\n\n", model.SystemTaskStatusSucceeded, 2, false, ""},
		{"invalid credentials stop immediately", 401, "", model.SystemTaskStatusFailed, 1, false, "analysis_authentication_failed"},
		{"repeated failures exhaust the retry budget", 503, "", model.SystemTaskStatusFailed, 2, true, "analysis_http_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncate(t)
			require.NoError(t, model.DB.AutoMigrate(&model.Option{}))
			t.Setenv("CRYPTO_SECRET", "worker-test-secret-not-for-production")
			config, err := GetErrorAnalysisConfig()
			require.NoError(t, err)
			config.MaxAttempts = 2
			require.NoError(t, SaveErrorAnalysisConfig(config, "worker-key", false))
			now := time.Now().Unix()
			require.NoError(t, model.LOG_DB.Create(&model.Log{RequestId: "worker-evidence", Type: model.LogTypeError, CreatedAt: now - 1, ModelName: "test-model", Other: `{"error_code":"upstream_error"}`}).Error)
			task, _, err := StartErrorAnalysis(model.ErrorAnalysisFilter{Start: now - 2, End: now})
			require.NoError(t, err)
			claimed, ok, err := model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, "worker-test", now+60)
			require.NoError(t, err)
			require.True(t, ok)
			calls := 0
			client := &http.Client{Transport: analysisTestTransport(func(request *http.Request) (*http.Response, error) {
				calls++
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
			var result AnalysisResult
			require.NoError(t, common.UnmarshalJsonStr(stored.Result, &result))
			require.Len(t, result.Attempts, tc.wantAttempts)
			if tc.wantStatus == model.SystemTaskStatusSucceeded {
				assert.Equal(t, "complete report", result.Report)
			} else {
				assert.Equal(t, tc.wantError, stored.Error)
			}
			assert.NotContains(t, stored.Result, "worker-key")
			assert.NotContains(t, stored.Result, "abandoned draft")
		})
	}
}

func (f analysisTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestErrorAnalysisStreamFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		status           int
		retry            bool
	}{
		{"complete", "data: {\"choices\":[{\"delta\":{\"content\":\"# Report\\nEvidence\"}}]}\n\ndata: [DONE]\n\n", "", 200, false},
		{"reasoning is activity but not report content", "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"private reasoning\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"Report\"},\"finish_reason\":\"stop\"}]}\n\n", "", 200, false},
		{"stop without done", "data: {\"choices\":[{\"delta\":{\"content\":\"Report\"},\"finish_reason\":\"stop\"}]}\n\n", "", 200, false},
		{"interrupted", "data: {\"choices\":[{\"delta\":{\"content\":\"Partial\"}}]}\n\n", "analysis_stream_interrupted", 200, true},
		{"empty", "data: [DONE]\n\n", "analysis_empty_report", 200, true},
		{"in stream error", "data: {\"error\":{\"message\":\"secret token\"}}\n\n", "analysis_stream_error", 200, true},
		{"truncated", "data: {\"choices\":[{\"finish_reason\":\"length\"}]}\n\n", "analysis_incomplete_report", 200, true},
		{"unauthorized", "private raw error", "analysis_authentication_failed", 401, false},
		{"rate limited", "private raw error", "analysis_rate_limited", 429, true},
		{"server error", "private raw error", "analysis_http_error", 503, true},
		{"malformed", "data: {bad json}\n\n", "analysis_invalid_sse", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
				assert.Equal(t, "POST", r.Method)
				var request map[string]any
				require.NoError(t, common.DecodeJson(r.Body, &request))
				assert.Equal(t, true, request["stream"])
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			var report string
			err := streamErrorAnalysis(context.Background(), server.Client(), ErrorAnalysisConfig{Endpoint: server.URL, Model: "test", TimeoutSeconds: 30, IdleSeconds: 10}, "test-key", "synthetic evidence", func(value string) error { report = value; return nil })
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
	}{{"idle", 10, 30, "analysis_idle_timeout"}, {"deadline", 60, 30, "analysis_timeout"}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &http.Client{Transport: analysisTestTransport(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}
				err := streamErrorAnalysis(context.Background(), client, ErrorAnalysisConfig{Endpoint: "https://example.com/v1/chat/completions", IdleSeconds: tc.idle, TimeoutSeconds: tc.total}, "test", "test", func(string) error { return nil })
				require.EqualError(t, err, tc.code)
			})
		})
	}
	client := newAnalysisHTTPClient()
	for _, target := range []string{"http://127.0.0.1/", "http://169.254.169.254/"} {
		request, err := http.NewRequestWithContext(context.Background(), "GET", target, nil)
		require.NoError(t, err)
		_, err = client.Do(request)
		require.Error(t, err)
	}
}

// Opt-in live verification uses a credential file outside the repository and
// sends synthetic diagnostic facts only. No credentials are printed or saved.
func TestErrorAnalysisLiveProvider(t *testing.T) {
	keyFile := os.Getenv("TEST_ANALYSIS_KEY_FILE")
	if keyFile == "" {
		t.Skip("live provider test not configured")
	}
	key, err := os.ReadFile(keyFile)
	require.NoError(t, err)
	config := ErrorAnalysisConfig{Endpoint: "https://api.bob-api.com/v1/chat/completions", Model: "deepseek-v4.1-flash", TimeoutSeconds: 180, IdleSeconds: 60, ReportLanguage: "zh"}
	var report string
	client := newAnalysisHTTPClient()
	defer client.CloseIdleConnections()
	if publicIP := os.Getenv("TEST_ANALYSIS_PUBLIC_IP"); publicIP != "" {
		// Resolve the authorized provider through independently verified public
		// DNS when a local VPN substitutes reserved fake-IP DNS responses.
		ip := net.ParseIP(publicIP)
		require.NotNil(t, ip)
		require.True(t, (&common.SSRFProtection{}).IsIPAccessAllowed(ip))
		client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil || host != "api.bob-api.com" {
				return nil, fmt.Errorf("unexpected live-test target")
			}
			return (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, net.JoinHostPort(publicIP, port))
		}
	}
	if capturePath := os.Getenv("TEST_ANALYSIS_CAPTURE_FILE"); capturePath != "" {
		capture, err := os.OpenFile(capturePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		require.NoError(t, err)
		defer capture.Close()
		transport := client.Transport
		client.Transport = analysisTestTransport(func(request *http.Request) (*http.Response, error) {
			response, err := transport.RoundTrip(request)
			if err == nil {
				response.Body = struct {
					io.Reader
					io.Closer
				}{io.TeeReader(response.Body, capture), response.Body}
			}
			return response, err
		})
	}
	err = streamErrorAnalysis(context.Background(), client, config, strings.TrimSpace(string(key)), `{"synthetic_test":true,"requests":12,"affected":2,"zero_output":1,"interrupted":1,"errors":0,"logs":[{"request_id":"synthetic-empty-1","model":"example-model","categories":["zero_output"],"diagnostic":{"has_output":false,"end_reason":"done"}},{"request_id":"synthetic-cut-1","categories":["interrupted"],"diagnostic":{"end_reason":"timeout","has_output":true}}]}`, func(value string) error { report = value; return nil })
	require.NoError(t, err)
	require.NotEmpty(t, report)
	if output := os.Getenv("TEST_ANALYSIS_REPORT_FILE"); output != "" {
		require.NoError(t, os.WriteFile(output, []byte(report), 0600))
	}
	t.Log(fmt.Sprintf("Chat Completions SSE succeeded; report length %d bytes", len(report)))
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

type analysisFailedSettlement struct{ relaycommon.BillingSettler }

func (*analysisFailedSettlement) Settle(int) error         { return io.ErrClosedPipe }
func (*analysisFailedSettlement) GetPreConsumedQuota() int { return 500 }
