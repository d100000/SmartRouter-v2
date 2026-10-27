package service

import (
	"math/rand/v2"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// PrepareStreamOverloadRetry can only narrow the existing routing policy. It
// never overrides strict sessions, channel pins, status rules or RetryTimes.
// Both ordinary attempts and a stream retry share the controller's budget;
// once a stream retry starts, no third attempt of any kind is allowed.
func PrepareStreamOverloadRetry(c *gin.Context, info *relaycommon.RelayInfo, apiErr *types.NewAPIError, decision PolicyDecision) PolicyDecision {
	failure := info.StreamFailure
	if failure == nil && info.StreamRetryCount > 0 {
		// A retried stream may fail before SSE starts (HTTP/transport errors).
		// It is still an unbilled attempt and needs the same durable diagnostics.
		failure = &relaycommon.StreamFailureInfo{Error: apiErr, NoCharge: true}
		info.StreamFailure = failure
	}
	candidate := failure != nil && failure.RetryableOverload && failure.NoCharge && !failure.HasActualUsage && !failure.HasEffectiveOutput
	if !candidate && info.StreamRetryCount == 0 {
		return decision
	}
	if info.StreamRetryCount > 0 || info.RetryIndex > 0 {
		decision = PolicyDecision{Action: "stop", Reason: "stream_retry_limit", Source: "system"}
	}
	if candidate && decision.Action == "retry" {
		delay := failure.RetryAfter
		if delay <= 0 {
			delay = time.Duration(250+rand.IntN(251)) * time.Millisecond
		}
		// Bound the extra wait and the period in which an invisible stream may
		// be restarted. Never extend an administrator/client total deadline.
		deadline := info.StartTime.Add(30 * time.Second)
		if requestDeadline, ok := c.Request.Context().Deadline(); ok && requestDeadline.Before(deadline) {
			deadline = requestDeadline
		}
		if delay > 10*time.Second || !time.Now().Add(delay).Before(deadline) {
			decision = PolicyDecision{Action: "stop", Reason: "stream_retry_deadline", Source: "system"}
		} else {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-c.Request.Context().Done():
			}
			timer.Stop()
			if reason := SchedulingRetryStopReason(c); reason != "" {
				decision = PolicyDecision{Action: "stop", Reason: reason, Source: "scheduler"}
			}
		}
	}
	message := apiErr.MaskSensitiveErrorWithStatusCode(common.UpstreamAddresses(c)...)
	if len(message) > common.LocalLogContentLimit {
		message = strings.ToValidUTF8(message[:common.LocalLogContentLimit], "")
	}
	if len(info.StreamRetryHistory) < 2 {
		info.StreamRetryHistory = append(info.StreamRetryHistory, relaycommon.StreamRetryAttempt{
			Attempt: info.RetryIndex + 1, ChannelID: info.ChannelId, Code: string(apiErr.GetErrorCode()), Message: message,
			Decision: decision.Action, Reason: decision.Reason, UpstreamRequestID: common.LocalLogPreview(c.GetString(common.UpstreamRequestIdKey)),
		})
	}
	return decision
}
