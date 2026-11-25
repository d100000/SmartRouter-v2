package channel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type relayRequestTransport func(*http.Request) (*http.Response, error)

func (transport relayRequestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return transport(req)
}

func TestDoRequestWaitsForAcceptedResponseBeforeHeartbeat(t *testing.T) {
	service.InitHttpClient()
	client := service.GetHttpClient()
	originalTransport, originalTimeout := client.Transport, client.Timeout
	settings := operation_setting.GetGeneralSetting()
	originalEnabled, originalSeconds := settings.PingIntervalEnabled, settings.PingIntervalSeconds
	settings.PingIntervalEnabled, settings.PingIntervalSeconds = true, 1
	client.Timeout = 0
	t.Cleanup(func() {
		client.Transport, client.Timeout = originalTransport, originalTimeout
		settings.PingIntervalEnabled, settings.PingIntervalSeconds = originalEnabled, originalSeconds
	})

	synctest.Test(t, func(t *testing.T) {
		client.Transport = relayRequestTransport(func(req *http.Request) (*http.Response, error) {
			// Model headers arriving after two heartbeat intervals using the test
			// clock; no upstream connection or wall-clock sleep is involved.
			<-time.After(2 * time.Second)
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":"upstream unavailable"}`)),
				Request:    req,
			}, nil
		})
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req, err := http.NewRequest(http.MethodPost, "https://provider.invalid/chat", strings.NewReader("{}"))
		require.NoError(t, err)
		info := &relaycommon.RelayInfo{IsStream: true, ChannelMeta: &relaycommon.ChannelMeta{}}

		resp, err := doRequest(c, req, info)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
		assert.False(t, c.Writer.Written(), "unaccepted response must leave the retry window open")
		assert.Empty(t, recorder.Body.String())
		assert.True(t, service.SchedulingRetryAllowed(c))

		c.JSON(resp.StatusCode, gin.H{"error": "upstream unavailable"})
		assert.Equal(t, http.StatusBadGateway, recorder.Code)
		assert.Equal(t, "application/json; charset=utf-8", recorder.Header().Get("Content-Type"))
		assert.NotContains(t, recorder.Body.String(), ": PING")
	})
}

func TestSchedulingRejectsReplayAfterHeartbeatOrBusinessOutput(t *testing.T) {
	for _, heartbeat := range []bool{true, false} {
		t.Run(map[bool]string{true: "heartbeat", false: "business content"}[heartbeat], func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			service.ObserveSchedulingResponse(c)
			require.True(t, service.SchedulingRetryAllowed(c))
			if heartbeat {
				require.NoError(t, helper.PingData(c))
			} else {
				require.NoError(t, helper.StringData(c, `{"choices":[{"delta":{"content":"answer"}}]}`))
			}
			assert.True(t, c.Writer.Written())
			assert.False(t, service.SchedulingRetryAllowed(c))
		})
	}
}

func TestDoRequestClientCancellationStopsUpstream(t *testing.T) {
	service.InitHttpClient()
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "waiting for headers", true: "reading accepted stream"}[accepted], func(t *testing.T) {
			started, stopped := make(chan struct{}), make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if accepted {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				close(started)
				<-r.Context().Done()
				close(stopped)
			}))
			defer upstream.Close()
			requestContext, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(requestContext)
			req, err := http.NewRequest(http.MethodPost, upstream.URL, strings.NewReader("{}"))
			require.NoError(t, err)
			info := &relaycommon.RelayInfo{IsStream: true, ChannelMeta: &relaycommon.ChannelMeta{}}
			type requestResult struct {
				response *http.Response
				err      error
			}
			result := make(chan requestResult, 1)
			go func() {
				resp, requestErr := doRequest(c, req, info)
				result <- requestResult{resp, requestErr}
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream request did not start")
			}
			var outcome requestResult
			if accepted {
				select {
				case outcome = <-result:
				case <-time.After(5 * time.Second):
					t.Fatal("accepted response headers were not returned")
				}
				require.NoError(t, outcome.err)
				require.NotNil(t, outcome.response)
				defer outcome.response.Body.Close()
			}
			cancel()
			if accepted {
				_, err = io.ReadAll(outcome.response.Body)
				require.ErrorIs(t, err, context.Canceled)
			} else {
				select {
				case outcome = <-result:
				case <-time.After(5 * time.Second):
					t.Fatal("request did not return after client cancellation")
				}
				require.Error(t, outcome.err, "the relay intentionally hides the provider transport error")
				require.ErrorIs(t, requestContext.Err(), context.Canceled)
				assert.Nil(t, outcome.response)
			}
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream connection remained open after cancellation")
			}
			assert.False(t, c.Writer.Written(), "neither waiting phase may emit an early heartbeat")
			assert.False(t, service.SchedulingRetryAllowed(c), "canceled requests cannot retry")
		})
	}
}

func TestNewTaskAPIRequestInheritsClientCancellation(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)

	upstream, err := newTaskAPIRequest(c, "https://provider.example/tasks", nil)
	require.NoError(t, err)
	cancel()

	require.ErrorIs(t, upstream.Context().Err(), context.Canceled)
}

func TestProcessHeaderOverride_ChannelTestSkipsPassthroughRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Parallel()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"*": "",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Empty(t, headers)
}

func TestProcessHeaderOverride_ChannelTestSkipsClientHeaderPlaceholder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Parallel()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Upstream-Trace": "{client_header:X-Trace-Id}",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	_, ok := headers["x-upstream-trace"]
	require.False(t, ok)
}

func TestProcessHeaderOverride_NonTestKeepsClientHeaderPlaceholder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Parallel()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Upstream-Trace": "{client_header:X-Trace-Id}",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "trace-123", headers["x-upstream-trace"])
}

func TestProcessHeaderOverride_RuntimeOverrideIsFinalHeaderMap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Parallel()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		IsChannelTest:             false,
		UseRuntimeHeadersOverride: true,
		RuntimeHeadersOverride: map[string]any{
			"x-static":  "runtime-value",
			"x-runtime": "runtime-only",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Static": "legacy-value",
				"X-Legacy": "legacy-only",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "runtime-value", headers["x-static"])
	require.Equal(t, "runtime-only", headers["x-runtime"])
	_, exists := headers["x-legacy"]
	require.False(t, exists)
}

func TestProcessHeaderOverride_PassthroughSkipsAcceptEncoding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Parallel()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")
	ctx.Request.Header.Set("Accept-Encoding", "gzip")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"*": "",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "trace-123", headers["x-trace-id"])

	_, hasAcceptEncoding := headers["accept-encoding"]
	require.False(t, hasAcceptEncoding)
}

func TestProcessHeaderOverride_PassHeadersTemplateSetsRuntimeHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Parallel()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("Originator", "Codex CLI")
	ctx.Request.Header.Set("Session_id", "sess-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		RequestHeaders: map[string]string{
			"Originator": "Codex CLI",
			"Session_id": "sess-123",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			ParamOverride: map[string]any{
				"operations": []any{
					map[string]any{
						"mode":  "pass_headers",
						"value": []any{"Originator", "Session_id", "X-Codex-Beta-Features"},
					},
				},
			},
			HeadersOverride: map[string]any{
				"X-Static": "legacy-value",
			},
		},
	}

	_, err := relaycommon.ApplyParamOverrideWithRelayInfo([]byte(`{"model":"gpt-4.1"}`), info)
	require.NoError(t, err)
	require.True(t, info.UseRuntimeHeadersOverride)
	require.Equal(t, "Codex CLI", info.RuntimeHeadersOverride["originator"])
	require.Equal(t, "sess-123", info.RuntimeHeadersOverride["session_id"])
	_, exists := info.RuntimeHeadersOverride["x-codex-beta-features"]
	require.False(t, exists)
	require.Equal(t, "legacy-value", info.RuntimeHeadersOverride["x-static"])

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "Codex CLI", headers["originator"])
	require.Equal(t, "sess-123", headers["session_id"])
	_, exists = headers["x-codex-beta-features"]
	require.False(t, exists)

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	applyHeaderOverrideToRequest(upstreamReq, headers)
	require.Equal(t, "Codex CLI", upstreamReq.Header.Get("Originator"))
	require.Equal(t, "sess-123", upstreamReq.Header.Get("Session_id"))
	require.Empty(t, upstreamReq.Header.Get("X-Codex-Beta-Features"))
}

func TestToWebSocketURL(t *testing.T) {
	for input, want := range map[string]string{
		"https://api.openai.com/v1/responses":             "wss://api.openai.com/v1/responses",
		"http://127.0.0.1:3000/v1/responses":              "ws://127.0.0.1:3000/v1/responses",
		"wss://chatgpt.com/backend-api/codex/responses":   "wss://chatgpt.com/backend-api/codex/responses",
		"ws://127.0.0.1:3000/backend-api/codex/responses": "ws://127.0.0.1:3000/backend-api/codex/responses",
	} {
		assert.Equal(t, want, toWebSocketURL(input), input)
	}
}

func TestDoRequestLeavesEarlyResponseBodyOwnedByTransport(t *testing.T) {
	service.InitHttpClient()
	client := service.GetHttpClient()
	previousTransport := client.Transport
	t.Cleanup(func() { client.Transport = previousTransport })
	client.Transport = relayRequestTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: req}, nil
	})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request.Body = nil
	body, writer := io.Pipe()
	t.Cleanup(func() { _ = body.Close(); _ = writer.Close() })
	req, err := http.NewRequest(http.MethodPost, "https://provider.invalid/chat", body)
	require.NoError(t, err)
	resp, err := doRequest(c, req, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}})
	require.NoError(t, err)
	defer resp.Body.Close()
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, "still uploading")
		_ = writer.Close()
		written <- err
	}()
	remaining, err := io.ReadAll(req.Body)
	require.NoError(t, err, "early response headers must not close the transport's ongoing upload")
	require.NoError(t, <-written)
	assert.Equal(t, "still uploading", string(remaining))
}

func TestProcessHeaderOverride_RequestPlaceholders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Parallel()

	tests := []struct {
		name          string
		userID        int
		channelID     int
		upstreamModel string
		originModel   string
		runtime       bool
		clientHeaders map[string]string
		overrides     map[string]any
		want          map[string]string
	}{
		{
			name:          "request values stand alone and inside text",
			userID:        12,
			channelID:     37,
			upstreamModel: "gpt-4o",
			originModel:   "my-gpt",
			overrides: map[string]any{
				"X-User-Id":        "{user_id}",
				"X-Channel-Id":     "{channel_id}",
				"X-Model":          "{model}",
				"X-Original-Model": "{original_model}",
				"X-Trace":          "u{user_id}-c{channel_id}-u{user_id}",
				"Authorization":    "Bearer {api_key}",
				"X-Unknown":        "{unknown}",
			},
			want: map[string]string{
				"x-user-id":        "12",
				"x-channel-id":     "37",
				"x-model":          "gpt-4o",
				"x-original-model": "my-gpt",
				"x-trace":          "u12-c37-u12",
				"authorization":    "Bearer sk-test",
				"x-unknown":        "{unknown}",
			},
		},
		{
			name:        "model is the requested model when none is mapped",
			userID:      12,
			channelID:   37,
			originModel: "my-gpt",
			overrides:   map[string]any{"X-Model": "{model}"},
			want:        map[string]string{"x-model": "my-gpt"},
		},
		{
			name: "a header naming a value the request lacks is left out",
			overrides: map[string]any{
				"X-User-Id":        "{user_id}",
				"X-Channel-Id":     "{channel_id}",
				"X-Model":          "{model}",
				"X-Original-Model": "{original_model}",
				"X-Mixed":          "u{user_id}-fixed",
				"X-Static":         "fixed",
			},
			want: map[string]string{"x-static": "fixed"},
		},
		{
			name:          "a model name with a control character is left out",
			userID:        12,
			channelID:     37,
			upstreamModel: "gpt-4o\r\nX-Injected: 1",
			originModel:   "gpt-4o\r\nX-Injected: 1",
			overrides: map[string]any{
				"X-User-Id":        "{user_id}",
				"X-Model":          "{model}",
				"X-Original-Model": "{original_model}",
			},
			want: map[string]string{"x-user-id": "12"},
		},
		{
			name:          "a value that reads like a placeholder is not resolved",
			userID:        12,
			channelID:     37,
			upstreamModel: "{user_id}",
			originModel:   "{api_key}",
			overrides: map[string]any{
				"X-Model":          "{model}",
				"X-Original-Model": "{original_model}",
			},
			want: map[string]string{
				"x-model":          "{user_id}",
				"x-original-model": "{api_key}",
			},
		},
		{
			name:          "client header values are not resolved",
			userID:        12,
			channelID:     37,
			clientHeaders: map[string]string{"X-Client-Tag": "{user_id}"},
			overrides: map[string]any{
				"*":        true,
				"X-Copied": "{client_header:X-Client-Tag}",
			},
			want: map[string]string{
				"x-client-tag": "{user_id}",
				"x-copied":     "{user_id}",
			},
		},
		{
			name:          "headers the parameter override sets are resolved",
			userID:        12,
			channelID:     37,
			upstreamModel: "gpt-4o",
			originModel:   "my-gpt",
			runtime:       true,
			overrides: map[string]any{
				"x-user-id": "{user_id}",
				"x-model":   "{model}",
			},
			want: map[string]string{
				"x-user-id": "12",
				"x-model":   "gpt-4o",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			for name, value := range tt.clientHeaders {
				ctx.Request.Header.Set(name, value)
			}
			info := &relaycommon.RelayInfo{
				UserId:          tt.userID,
				OriginModelName: tt.originModel,
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelId:         tt.channelID,
					ApiKey:            "sk-test",
					UpstreamModelName: tt.upstreamModel,
					HeadersOverride:   tt.overrides,
				},
			}
			if tt.runtime {
				info.ChannelMeta.HeadersOverride = nil
				info.UseRuntimeHeadersOverride = true
				info.RuntimeHeadersOverride = tt.overrides
			}

			headers, err := processHeaderOverride(info, ctx)
			require.NoError(t, err)
			assert.Equal(t, tt.want, headers)
		})
	}

	t.Run("both timestamps are the instant of the call", func(t *testing.T) {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		info := &relaycommon.RelayInfo{
			ChannelMeta: &relaycommon.ChannelMeta{
				HeadersOverride: map[string]any{
					"X-Timestamp":    "{timestamp}",
					"X-Timestamp-Ms": "{timestamp_ms}",
				},
			},
		}

		before := time.Now().UnixMilli()
		headers, err := processHeaderOverride(info, ctx)
		after := time.Now().UnixMilli()
		require.NoError(t, err)

		seconds, err := strconv.ParseInt(headers["x-timestamp"], 10, 64)
		require.NoError(t, err)
		millis, err := strconv.ParseInt(headers["x-timestamp-ms"], 10, 64)
		require.NoError(t, err)
		assert.Equal(t, millis/1000, seconds)
		assert.GreaterOrEqual(t, millis, before)
		assert.LessOrEqual(t, millis, after)
	})
}
