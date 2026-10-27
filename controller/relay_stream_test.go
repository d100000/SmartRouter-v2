package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/perf_metrics_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func disableRelayStreamTestMetrics(t *testing.T) {
	t.Helper()
	previous := strconv.FormatBool(perf_metrics_setting.GetSetting().Enabled)
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"perf_metrics_setting.enabled": "false"}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"perf_metrics_setting.enabled": previous}))
	})
}

func TestRelayDoesNotCommitDuringImagePreparation(t *testing.T) {
	disableRelayStreamTestMetrics(t)
	settings := operation_setting.GetGeneralSetting()
	previousEnabled, previousInterval := settings.PingIntervalEnabled, settings.PingIntervalSeconds
	previousCount, previousMedia := constant.CountToken, constant.GetMediaToken
	fetchSettings := system_setting.GetFetchSetting()
	previousFetchSettings := *fetchSettings
	fetchSettings.AllowPrivateIp = true
	fetchSettings.AllowedPorts = []string{"1-65535"}
	settings.PingIntervalEnabled, settings.PingIntervalSeconds = true, 1
	constant.CountToken, constant.GetMediaToken = true, true
	service.InitHttpClient()
	client := service.GetSSRFProtectedHTTPClient()
	previousTransport, previousTimeout := client.Transport, client.Timeout
	client.Timeout = 0
	t.Cleanup(func() {
		settings.PingIntervalEnabled, settings.PingIntervalSeconds = previousEnabled, previousInterval
		constant.CountToken, constant.GetMediaToken = previousCount, previousMedia
		*fetchSettings = previousFetchSettings
		client.Transport, client.Timeout = previousTransport, previousTimeout
	})

	synctest.Test(t, func(t *testing.T) {
		var fetched bool
		client.Transport = schedulingTokenTransport(func(request *http.Request) (*http.Response, error) {
			fetched = true
			<-time.After(2 * time.Second)
			return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("unavailable")), Request: request}, nil
		})
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		body := `{"model":"claude-sonnet-4-20250514","max_tokens":16,"stream":true,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"http://127.0.0.1/image.png"}}]}]}`
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		common.SetContextKey(c, constant.ContextKeyOriginalModel, "claude-sonnet-4-20250514")
		Relay(c, types.RelayFormatClaude)
		require.True(t, fetched, "the request must reach delayed image preparation")
		assert.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
		assert.NotContains(t, recorder.Body.String(), ": PING", "preparation must preserve the HTTP error and retry boundary")
	})
}

func TestRelayRetriesBeforeAcceptedStreamOutput(t *testing.T) {
	settings := operation_setting.GetGeneralSetting()
	previousEnabled, previousInterval := settings.PingIntervalEnabled, settings.PingIntervalSeconds
	settings.PingIntervalEnabled, settings.PingIntervalSeconds = true, 1
	service.InitHttpClient()
	client := service.GetHttpClient()
	previousTransport, previousTimeout := client.Transport, client.Timeout
	client.Timeout = 0
	t.Cleanup(func() {
		settings.PingIntervalEnabled, settings.PingIntervalSeconds = previousEnabled, previousInterval
		client.Transport, client.Timeout = previousTransport, previousTimeout
	})
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		client.Transport = schedulingTokenTransport(func(request *http.Request) (*http.Response, error) {
			attempts++
			<-time.After(2 * time.Second)
			status, body := http.StatusBadGateway, `{"error":"unavailable"}`
			if attempts == 2 {
				status, body = http.StatusOK, "data: ok\n\ndata: [DONE]\n\n"
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
		})
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		service.ObserveSchedulingResponse(c)
		info := &relaycommon.RelayInfo{IsStream: true, ChannelMeta: &relaycommon.ChannelMeta{}}
		for range 2 {
			request, err := http.NewRequest(http.MethodPost, "https://provider.invalid/messages", strings.NewReader("{}"))
			require.NoError(t, err)
			response, err := channel.DoRequest(c, request, info)
			require.NoError(t, err)
			require.True(t, service.SchedulingRetryAllowed(c), "waiting for either attempt's headers must not close the retry window")
			if response.StatusCode == http.StatusBadGateway {
				response.Body.Close()
				continue
			}
			helper.SetEventStreamHeaders(c)
			_, err = io.Copy(c.Writer, response.Body)
			response.Body.Close()
			require.NoError(t, err)
		}
		assert.Equal(t, 2, attempts)
		assert.Equal(t, "data: ok\n\ndata: [DONE]\n\n", recorder.Body.String())
		assert.False(t, service.SchedulingRetryAllowed(c), "accepted output must stop retries")
	})
}

func TestRelayKeepAlivePreservesValidationErrors(t *testing.T) {
	settings := operation_setting.GetGeneralSetting()
	previousEnabled := settings.PingIntervalEnabled
	settings.PingIntervalEnabled = true
	t.Cleanup(func() { settings.PingIntervalEnabled = previousEnabled })
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-20250514","stream":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	Relay(c, types.RelayFormatClaude)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
	require.NotContains(t, recorder.Body.String(), ": PING")
}

func TestStreamKeepAlivePreservesFastHTTPFailures(t *testing.T) {
	settings := operation_setting.GetGeneralSetting()
	previousEnabled, previousInterval := settings.PingIntervalEnabled, settings.PingIntervalSeconds
	settings.PingIntervalEnabled, settings.PingIntervalSeconds = true, 60
	t.Cleanup(func() {
		settings.PingIntervalEnabled, settings.PingIntervalSeconds = previousEnabled, previousInterval
	})
	service.InitHttpClient()
	for _, status := range []int{http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"request rejected"}`)
			}))
			defer upstream.Close()
			engine := gin.New()
			engine.POST("/v1/messages", func(c *gin.Context) {
				stop := helper.StartStreamKeepAlive(c, time.Minute)
				defer stop()
				request, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, strings.NewReader("{}"))
				if !assert.NoError(t, err) {
					return
				}
				info := &relaycommon.RelayInfo{IsStream: true, ChannelMeta: &relaycommon.ChannelMeta{}}
				response, err := channel.DoRequest(c, request, info)
				if !assert.NoError(t, err) {
					return
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if assert.NoError(t, err) {
					c.Header("Content-Type", "application/json")
					c.Data(response.StatusCode, "application/json", body)
				}
			})
			gateway := httptest.NewServer(engine)
			defer gateway.Close()
			client := &http.Client{Timeout: 4 * time.Second}
			response, err := client.Post(gateway.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
			require.NoError(t, err)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, status, response.StatusCode)
			require.Equal(t, "application/json", response.Header.Get("Content-Type"))
			require.JSONEq(t, `{"error":"request rejected"}`, string(body))
		})
	}
}

func TestRelayKeepAliveHonorsStreamSettings(t *testing.T) {
	disableRelayStreamTestMetrics(t)
	settings := operation_setting.GetGeneralSetting()
	previousEnabled := settings.PingIntervalEnabled
	previousCount, previousMedia, previousNonStreamMedia := constant.CountToken, constant.GetMediaToken, constant.GetMediaTokenNotStream
	constant.CountToken, constant.GetMediaToken, constant.GetMediaTokenNotStream = true, true, true
	t.Cleanup(func() {
		settings.PingIntervalEnabled = previousEnabled
		constant.CountToken, constant.GetMediaToken, constant.GetMediaTokenNotStream = previousCount, previousMedia, previousNonStreamMedia
	})
	for _, test := range []struct {
		name    string
		enabled bool
		stream  bool
	}{
		{name: "ping disabled", enabled: false, stream: true},
		{name: "non-streaming", enabled: true, stream: false},
		{name: "fast streaming failure", enabled: true, stream: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings.PingIntervalEnabled = test.enabled
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			body := `{"model":"claude-sonnet-4-20250514","max_tokens":16,"stream":` + strconv.FormatBool(test.stream) + `,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"!"}}]}]}`
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "claude-sonnet-4-20250514")
			Relay(c, types.RelayFormatClaude)
			require.Equal(t, http.StatusInternalServerError, recorder.Code)
			require.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
			require.NotContains(t, recorder.Body.String(), ": PING")
			var response map[string]any
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			require.Equal(t, "error", response["type"])
		})
	}
}

func TestRelayStreamErrorAfterKeepAlive(t *testing.T) {
	for _, format := range []types.RelayFormat{types.RelayFormatClaude, types.RelayFormatOpenAI} {
		t.Run(string(format), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/stream", nil)
			helper.SetEventStreamHeaders(c)
			require.NoError(t, helper.PingData(c))
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://provider.example")
			common.SetContextKey(c, constant.ContextKeyUpstreamRequestOrigin, "https://provider.example")
			apiError := types.NewErrorWithStatusCode(errors.New("upstream rejected input at https://provider.example/v1/messages"), types.ErrorCodeBadResponseStatusCode, http.StatusBadRequest)
			writeRelayStreamError(c, format, apiError)
			require.Equal(t, http.StatusOK, recorder.Code)
			require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
			require.Contains(t, recorder.Body.String(), "upstream rejected input")
			require.NotContains(t, recorder.Body.String(), "provider.example")
			status, ok := common.GetContextKeyType[*relaycommon.StreamStatus](c, constant.ContextKeyResponseStreamStatus)
			require.True(t, ok)
			require.True(t, status.ResponseFailed(), "stream errors must not consume the successful-request rate limit")
			require.Equal(t, http.StatusBadRequest, status.OutcomeSnapshot().ErrorStatus)
			if format == types.RelayFormatClaude {
				require.Contains(t, recorder.Body.String(), "event: error\n")
			} else {
				require.Contains(t, recorder.Body.String(), "data: [DONE]")
			}
		})
	}
}

func TestStreamKeepAliveCancellation(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/stream", nil).WithContext(requestContext)
	stop := helper.StartStreamKeepAlive(c, time.Hour)
	cancel()
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("keepalive did not stop after client cancellation")
	}
	_, err := c.Writer.WriteString("data: done\n\n")
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(recorder.Body.String(), "data: done\n\n"))
}
