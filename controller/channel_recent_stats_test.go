package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestChannelRecentStatsSayWhichRequestsAreLogged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousLogs, previousErrors := common.LogConsumeEnabled, constant.ErrorLogEnabled
	t.Cleanup(func() {
		common.LogConsumeEnabled, constant.ErrorLogEnabled = previousLogs, previousErrors
	})

	for _, logged := range []struct{ successes, failures bool }{{true, false}, {false, true}} {
		common.LogConsumeEnabled, constant.ErrorLogEnabled = logged.successes, logged.failures
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/channel/recent_stats", nil)

		GetChannelRecentStats(c)

		require.Equal(t, http.StatusOK, recorder.Code)
		var body struct {
			Success bool `json:"success"`
			Data    struct {
				LogRecordingEnabled   *bool `json:"log_recording_enabled"`
				ErrorRecordingEnabled *bool `json:"error_recording_enabled"`
			} `json:"data"`
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &body))
		require.True(t, body.Success)
		require.NotNil(t, body.Data.LogRecordingEnabled)
		require.NotNil(t, body.Data.ErrorRecordingEnabled)
		require.Equal(t, logged.successes, *body.Data.LogRecordingEnabled)
		require.Equal(t, logged.failures, *body.Data.ErrorRecordingEnabled)
	}
}
