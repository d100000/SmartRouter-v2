package service

import (
	"bytes"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const failureObservationKey = "failure_output_observation"

// Only the current incomplete frame is buffered, never persisted. This observer
// does not alter forwarding, token counts, retry decisions or settlement.
type failureOutputObservation struct {
	mu          sync.Mutex
	pending     []byte
	known       bool
	output      bool
	finish      string
	overflow    bool
	delivery    responseEvidenceParser
	writeFailed bool
}

func observeFailureOutput(c *gin.Context, body []byte) {
	value, exists := c.Get(failureObservationKey)
	if !exists {
		return
	}
	state := value.(*failureOutputObservation)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.delivery.feed(body, strings.Contains(c.Writer.Header().Get("Content-Type"), "text/event-stream") || bytes.HasPrefix(bytes.TrimSpace(body), []byte("data:")) || bytes.HasPrefix(bytes.TrimSpace(body), []byte("event:")))
	if state.overflow {
		return
	}
	if len(state.pending)+len(body) > 256*1024 {
		state.pending = nil
		state.overflow = true
		return
	}
	state.pending = append(state.pending, body...)
	if gjson.ValidBytes(state.pending) {
		state.observePayload(state.pending)
		state.pending = nil
		return
	}
	for {
		line, rest, found := bytes.Cut(state.pending, []byte{'\n'})
		if !found {
			break
		}
		state.pending = rest
		if data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
			data = bytes.TrimSpace(data)
			if bytes.Equal(data, []byte("[DONE]")) {
				state.known = true
			} else {
				state.observePayload(data)
			}
		}
	}
}

func (s *failureOutputObservation) observePayload(body []byte) {
	if !gjson.ValidBytes(body) {
		return
	}
	root := gjson.ParseBytes(body)
	// An error envelope proves no successful output only if no earlier output
	// was observed. Role-only and terminal frames are not useful output.
	if root.Get("choices").Exists() || root.Get("candidates").Exists() || root.Get("output").Exists() || root.Get("content").Exists() || root.Get("type").Exists() || root.Get("error").Exists() {
		s.known = true
	}
	s.output = s.output || meaningfulSchedulingPayload(body)
	for _, path := range []string{"choices.#.message.content", "choices.#.message.reasoning_content", "choices.#.message.refusal", "choices.#.message.tool_calls.#.function.name", "choices.#.delta.tool_calls.#.function.name", "choices.#.delta.refusal", "choices.#.message.function_call.name", "choices.#.delta.function_call.name", "choices.#.message.audio.data", "content.#.text", "content.#.thinking", "content_block.text", "content_block.name", "output.#.content.#.text", "output.#.name", "response.output.#.content.#.text", "response.output.#.name", "item.name", "candidates.#.content.parts.#.functionCall.name"} {
		s.output = s.output || nonemptySchedulingValue(root.Get(path))
	}
	for _, path := range []string{"choices.0.finish_reason", "delta.stop_reason", "stop_reason", "candidates.0.finishReason", "response.incomplete_details.reason"} {
		if reason := root.Get(path).String(); reason != "" {
			s.finish = model.SafeFailureFact(reason, 80)
		}
	}
}

func appendFailureDiagnostic(c *gin.Context, info *relaycommon.RelayInfo, other *model.LogOther) {
	if info == nil {
		return
	}
	d := model.FailureDiagnostic{Version: 1, Protocol: string(info.RelayFormat), Node: common.NodeName, ElapsedMS: max(0, time.Since(info.StartTime).Milliseconds()), ReceivedChunks: info.ReceivedResponseCount, SentChunks: info.SendResponseCount, RetryCount: max(0, len(c.GetStringSlice("use_channel"))-1) + info.StreamRetryCount}
	if c.Request != nil {
		d.Method = c.Request.Method
		d.Path = c.Request.URL.Path
	}
	if info.ChannelMeta != nil {
		d.ChannelType = info.ChannelType
		d.UpstreamModel = info.UpstreamModelName
	}
	if info.FirstResponseTime.After(info.StartTime) {
		d.FirstResponseMS = info.FirstResponseTime.Sub(info.StartTime).Milliseconds()
	}
	if value, exists := c.Get(failureObservationKey); exists {
		state := value.(*failureOutputObservation)
		state.mu.Lock()
		if state.output || state.known && !state.overflow {
			output := state.output
			d.HasOutput = &output
		}
		d.FinishReason = state.finish
		state.mu.Unlock()
	}
	if ss := info.StreamStatus; ss != nil {
		outcome := ss.OutcomeSnapshot()
		d.EndReason = string(outcome.EndReason)
		d.ResponseStatus = string(outcome.Response)
		d.ExpectsTerminal = outcome.ExpectsTerminal
		d.ErrorCode, d.ErrorType, d.StatusCode = outcome.ErrorCode, outcome.ErrorType, outcome.ErrorStatus
		d.StreamErrors = ss.TotalErrorCount()
	}
	d.UpstreamModel = model.SafeFailureFact(d.UpstreamModel, 200)
	d.Node = model.SafeFailureFact(d.Node, 100)
	d.Path, _, _ = strings.Cut(d.Path, "?")
	other.SetAdmin("failure_diagnostic", d)
}
