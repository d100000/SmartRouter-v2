package service

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
)

// IsRelayWebSocketRequest identifies session routes, rather than trusting an
// Upgrade header on ordinary POST endpoints to opt out of HTTP time budgets.
func IsRelayWebSocketRequest(c *gin.Context) bool {
	if c.Request == nil || c.Request.Method != http.MethodGet || !strings.EqualFold(c.GetHeader("Upgrade"), "websocket") {
		return false
	}
	return c.Request.URL.Path == "/v1/realtime" || c.Request.URL.Path == "/v1/responses"
}

// BeginRelayRequestBudget runs once after model/stream parsing, before channel
// selection. All retries inherit this same context, including response reads.
// The caller owns cancellation until its full HTTP handler has returned.
// A zero setting preserves existing behavior (no application total deadline).
func BeginRelayRequestBudget(c *gin.Context) context.CancelFunc {
	const key = "relay_request_budget_started"
	if _, exists := c.Get(key); exists || c.Request == nil {
		return func() {}
	}
	c.Set(key, true)
	if IsRelayWebSocketRequest(c) {
		return func() {}
	}
	seconds := common.RelayTotalTimeout
	if common.GetContextKeyBool(c, constant.ContextKeyIsStream) {
		seconds = common.RelayStreamTotalTimeout
	}
	if seconds <= 0 {
		return func() {}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(min(seconds, maxTimeoutSeconds))*time.Second)
	c.Request = c.Request.WithContext(ctx)
	// A slow downstream reader must not keep a completed upstream attempt and
	// its capacity lease alive beyond the same budget. Streaming writes may
	// shorten this deadline, but must never extend it past the request deadline.
	if c.Writer == nil {
		return cancel
	}
	response := http.NewResponseController(c.Writer)
	deadline, _ := ctx.Deadline()
	_ = response.SetWriteDeadline(deadline)
	return func() {
		cancel()
		_ = response.SetWriteDeadline(time.Time{})
	}
}
