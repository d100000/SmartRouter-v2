package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/scheduler"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayBudgetSharedAcrossRetries(t *testing.T) {
	old := common.RelayTotalTimeout
	common.RelayTotalTimeout = 10
	t.Cleanup(func() { common.RelayTotalTimeout = old })
	synctest.Test(t, func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		cancel := BeginRelayRequestBudget(c)
		defer cancel()
		deadline, ok := c.Request.Context().Deadline()
		require.True(t, ok)
		start := time.Now()
		attempts := 0
		for SchedulingRetryAllowed(c) {
			attempts++
			// A retry must retain the original deadline, even if a caller
			// repeats initialization. synctest advances time without sleeping.
			BeginRelayRequestBudget(c)()
			actual, _ := c.Request.Context().Deadline()
			assert.Equal(t, deadline, actual)
			upstream := time.NewTimer(4 * time.Second)
			select {
			case <-upstream.C:
			case <-c.Request.Context().Done():
			}
			upstream.Stop()
		}
		assert.Equal(t, 3, attempts)
		assert.Equal(t, 10*time.Second, time.Since(start))
		assert.Equal(t, "request_deadline_exceeded", SchedulingRetryStopReason(c))
	})
}

func TestRelayBudgetScopeAndParentCancellation(t *testing.T) {
	oldHTTP, oldStream := common.RelayTotalTimeout, common.RelayStreamTotalTimeout
	common.RelayTotalTimeout, common.RelayStreamTotalTimeout = 10, 0
	t.Cleanup(func() { common.RelayTotalTimeout, common.RelayStreamTotalTimeout = oldHTTP, oldStream })
	for _, tc := range []struct {
		name, upgrade string
		stream        bool
		seconds       int
		wantDeadline  bool
	}{
		{"http", "", false, 0, true},
		{"upgrade header on ordinary HTTP", "websocket", false, 0, true},
		{"stream default unlimited", "", true, 0, false},
		{"stream opt in", "", true, 60, true},
		{"realtime excluded", "websocket", true, 60, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			common.RelayStreamTotalTimeout = tc.seconds
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if tc.name == "realtime excluded" {
				c.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
			}
			c.Request.Header.Set("Upgrade", tc.upgrade)
			common.SetContextKey(c, constant.ContextKeyIsStream, tc.stream)
			cancel := BeginRelayRequestBudget(c)
			defer cancel()
			_, ok := c.Request.Context().Deadline()
			assert.Equal(t, tc.wantDeadline, ok)
		})
	}
	synctest.Test(t, func(t *testing.T) {
		parent, cancelParent := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelParent()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(parent)
		cancel := BeginRelayRequestBudget(c)
		defer cancel()
		got, _ := c.Request.Context().Deadline()
		want, _ := parent.Deadline()
		assert.Equal(t, want, got)
		cancelParent()
		assert.False(t, SchedulingRetryAllowed(c))
		assert.Equal(t, "client_cancelled", SchedulingRetryStopReason(c))
	})
}

func TestStreamBudgetReleasesReservationWithoutFailureSample(t *testing.T) {
	previousEngine, previousTimeout := scheduler.Default, common.RelayStreamTotalTimeout
	scheduler.Default, common.RelayStreamTotalTimeout = scheduler.New(), 5
	t.Cleanup(func() { scheduler.Default, common.RelayStreamTotalTimeout = previousEngine, previousTimeout })
	synctest.Test(t, func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		common.SetContextKey(c, constant.ContextKeyIsStream, true)
		cancel := BeginRelayRequestBudget(c)
		defer cancel()
		state, request := schedulerRequestForGroup(c, "default", "budget-stream")
		candidate := scheduler.Candidate{ID: 1, Status: common.ChannelStatusEnabled, Capacity: 1}
		attempt, err := scheduler.Default.ReserveCandidate(request, candidate, true)
		require.NoError(t, err)
		state.currentRequest, state.attempt = request, attempt
		info := &relaycommon.RelayInfo{IsStream: true}
		StartSchedulingAttempt(c, info)
		before := scheduler.Default.Snapshot(request.Key, []scheduler.Candidate{candidate}, true)
		assert.Equal(t, 1, before.Summary.InFlight)
		<-c.Request.Context().Done()
		FinishSchedulingAttempt(c, info, types.NewOpenAIError(context.DeadlineExceeded, types.ErrorCodeDoRequestFailed, 504))
		EndSchedulingRequest(c)
		EndSchedulingRequest(c)
		after := scheduler.Default.Snapshot(request.Key, []scheduler.Candidate{candidate}, true)
		assert.Zero(t, after.Summary.InFlight)
		assert.Zero(t, after.Summary.Attempts30m)
		assert.Zero(t, after.Summary.Requests30m)
		assert.False(t, SchedulingRetryAllowed(c))
	})
}

func TestRelayMediaDownloadCancellationClosesUpstream(t *testing.T) {
	configureSSRFTestFetchSetting(t)
	fetch := system_setting.GetFetchSetting()
	fetch.EnableSSRFProtection = false
	oldWorker := system_setting.WorkerUrl
	system_setting.WorkerUrl = ""
	t.Cleanup(func() { system_setting.WorkerUrl = oldWorker })
	upstreamEnded := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamEnded)
	}))
	t.Cleanup(server.Close)
	previousClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = previousClient })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := DoDownloadRequestWithContext(ctx, server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	cancel()
	_, err = io.ReadAll(resp.Body)
	assert.ErrorIs(t, err, context.Canceled)
	select {
	case <-upstreamEnded:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled download left the upstream connection running")
	}
}
