package service

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const upstreamEvidenceKey = "upstream_response_evidence"
const localSettlementEvidenceKey = "local_settlement_evidence"
const evidenceFrameLimit = 1024 * 1024
const evidenceErrorLimit = 32 * 1024

// The parser retains usage and error envelopes, never successful output. SSE
// terminal events are committed only after the complete event delimiter.
type responseEvidenceParser struct {
	pending, event []byte
	dropping       bool
	incomplete     bool
	completed      bool
	failed         bool
	usage          map[string]json.RawMessage
	errorBody      string
	errorTruncated bool
}

func (p *responseEvidenceParser) feed(body []byte, stream bool) {
	if !stream {
		if len(p.pending)+len(body) > evidenceFrameLimit {
			p.pending = nil
			p.incomplete, p.dropping = true, true
		}
		if p.dropping {
			return
		}
		p.pending = append(p.pending, body...)
		if gjson.ValidBytes(p.pending) {
			p.payload(p.pending, false)
			p.pending = nil
		}
		return
	}
	for len(body) > 0 {
		line, rest, found := bytes.Cut(body, []byte{'\n'})
		if len(p.pending)+len(line) > evidenceFrameLimit {
			p.pending, p.event = nil, nil
			p.incomplete, p.dropping = true, true
		}
		if !p.dropping {
			p.pending = append(p.pending, line...)
		}
		if !found {
			return
		}
		body = rest
		if p.dropping {
			p.dropping = false
			continue
		}
		line = bytes.TrimSuffix(p.pending, []byte{'\r'})
		if len(line) == 0 {
			p.payload(bytes.TrimSpace(p.event), true)
			p.event = nil
		} else if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if len(p.event)+len(data)+1 > evidenceFrameLimit {
				p.event = nil
				p.incomplete = true
			} else {
				p.event = append(p.event, bytes.TrimSpace(data)...)
				p.event = append(p.event, '\n')
			}
		}
		p.pending = nil
	}
}

func (p *responseEvidenceParser) payload(body []byte, stream bool) {
	if bytes.Equal(body, []byte("[DONE]")) {
		p.completed = true
		return
	}
	if len(body) == 0 {
		return
	}
	if !gjson.ValidBytes(body) {
		p.incomplete = true
		return
	}
	root := gjson.ParseBytes(body)
	for _, path := range []string{"usage", "response.usage", "message.usage", "usageMetadata"} {
		usage := root.Get(path)
		if !usage.IsObject() {
			continue
		}
		if len(usage.Raw) > 16*1024 {
			p.incomplete = true
			continue
		}
		var values map[string]json.RawMessage
		if common.UnmarshalJsonStr(usage.Raw, &values) != nil {
			continue
		}
		// Claude reports input usage at message_start and output at message_delta.
		// Keep their original paths and merge successive updates at the same path.
		if p.usage == nil {
			p.usage = map[string]json.RawMessage{}
		}
		previous := map[string]json.RawMessage{}
		_ = common.Unmarshal(p.usage[path], &previous)
		if previous == nil {
			previous = map[string]json.RawMessage{}
		}
		for key, value := range values {
			previous[key] = value
		}
		encoded, err := common.Marshal(previous)
		if err == nil {
			safe, truncated := model.SafeFailureDetail(string(encoded), 16*1024)
			if truncated || !gjson.Valid(safe) {
				p.incomplete = true
				continue
			}
			encoded = []byte(safe)
		}
		if err == nil && len(encoded) <= 16*1024 {
			p.usage[path] = encoded
		} else {
			p.incomplete = true
		}
	}
	kind := root.Get("type").String()
	failed := root.Get("error").Exists() && root.Get("error").Type != gjson.Null || root.Get("response.error").Exists() && root.Get("response.error").Type != gjson.Null || kind == "response.failed" || kind == "error" || kind == "response.error"
	if failed {
		p.failed = true
		p.errorBody, p.errorTruncated = model.SafeFailureDetail(string(body), evidenceErrorLimit)
	}
	if failed {
		return
	}
	if !stream {
		p.completed = true
		return
	}
	status := root.Get("response.status").String()
	if kind == "message_stop" || (kind == "response.completed" || kind == "response.done") && (status == "" || status == "completed") || root.Get("candidates.0.finishReason").String() != "" {
		p.completed = true
	}
	// Chat Completions finish_reason can precede the usage chunk. Only [DONE]
	// confirms delivery of the complete stream, including any requested usage.
}

type upstreamEvidenceCapture struct {
	mu       sync.Mutex
	attempts []*upstreamEvidenceBody
	count    int
}

type upstreamEvidenceBody struct {
	io.ReadCloser
	mu        sync.Mutex
	parser    responseEvidenceParser
	evidence  model.UpstreamResponseEvidence
	stream    bool
	errorBody []byte
	finished  bool
	secrets   []string
}

// ObserveUpstreamResponse wraps the existing body without extra reads, buffering
// delays or protocol changes. SDK/WebSocket transports remain explicitly unknown.
func ObserveUpstreamResponse(c *gin.Context, info *relaycommon.RelayInfo, response *http.Response) {
	if c == nil || info == nil || response == nil || response.Body == nil {
		return
	}
	value, exists := c.Get(upstreamEvidenceKey)
	if !exists {
		value = &upstreamEvidenceCapture{}
		c.Set(upstreamEvidenceKey, value)
	}
	capture := value.(*upstreamEvidenceCapture)
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.count++
	upID := ""
	for _, key := range []string{common.RequestIdKey, "X-Request-Id", "Request-Id", "X-Goog-Request-Id", common.UpstreamRequestIdKey} {
		if upID = response.Header.Get(key); upID != "" {
			break
		}
	}
	// Clear a preceding attempt's ID when this upstream supplied none.
	c.Set(common.UpstreamRequestIdKey, upID)
	channel := 0
	if info.ChannelMeta != nil {
		channel = info.ChannelId
	}
	body := &upstreamEvidenceBody{ReadCloser: response.Body, stream: strings.Contains(response.Header.Get("Content-Type"), "text/event-stream"), evidence: model.UpstreamResponseEvidence{Attempt: capture.count, Channel: channel, RequestID: model.SafeFailureFact(upID, 128), StatusCode: response.StatusCode}}
	if response.Request != nil {
		for _, key := range []string{"Authorization", "X-Api-Key", "Api-Key", "X-Goog-Api-Key"} {
			secret := strings.TrimPrefix(response.Request.Header.Get(key), "Bearer ")
			if secret != "" {
				body.secrets = append(body.secrets, secret)
			}
		}
	}
	if len(capture.attempts) == 16 {
		capture.attempts = capture.attempts[1:]
	}
	capture.attempts = append(capture.attempts, body)
	response.Body = body
}

func (b *upstreamEvidenceBody) Read(body []byte) (int, error) {
	n, err := b.ReadCloser.Read(body)
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > 0 {
		if b.evidence.StatusCode >= 300 {
			remaining := evidenceErrorLimit - len(b.errorBody)
			b.errorBody = append(b.errorBody, body[:min(n, remaining)]...)
			b.evidence.ErrorTruncated = b.evidence.ErrorTruncated || n > remaining
		}
		b.parser.feed(body[:n], b.stream)
	}
	if err != nil {
		b.finished = err == io.EOF
		if err != io.EOF {
			b.parser.incomplete = true
		}
	}
	return n, err
}

func (b *upstreamEvidenceBody) Close() error {
	err := b.ReadCloser.Close()
	b.mu.Lock()
	// Failed retries can leave a large partial frame. Keep the incompleteness
	// fact, but release output bytes before another attempt starts.
	b.parser.incomplete = b.parser.incomplete || len(b.parser.pending) > 0 || len(b.parser.event) > 0
	b.parser.pending, b.parser.event = nil, nil
	b.mu.Unlock()
	return err
}

func (b *upstreamEvidenceBody) snapshot() model.UpstreamResponseEvidence {
	b.mu.Lock()
	defer b.mu.Unlock()
	evidence := b.evidence
	evidence.Usage = cloneEvidenceUsage(b.parser.usage)
	evidence.CaptureIncomplete = b.parser.incomplete || !b.finished && !b.parser.completed && !b.parser.failed || len(b.parser.pending) > 0 || len(b.parser.event) > 0
	errorBody := b.parser.errorBody
	if len(b.errorBody) > 0 {
		errorBody = string(b.errorBody)
	}
	for _, secret := range b.secrets {
		errorBody = strings.ReplaceAll(errorBody, secret, "[redacted]")
	}
	var truncated bool
	evidence.ErrorBody, truncated = model.SafeFailureDetail(errorBody, evidenceErrorLimit)
	evidence.ErrorTruncated = evidence.ErrorTruncated || b.parser.errorTruncated || truncated
	return evidence
}

func appendSettlementEvidence(c *gin.Context, info *relaycommon.RelayInfo, other *model.LogOther) {
	evidence := model.RelaySettlementEvidence{Version: 1}
	if info != nil && info.AnalyticsBilling != nil {
		evidence.UsageSource = info.AnalyticsBilling.UsageSource
	}
	if value, ok := c.Get(upstreamEvidenceKey); ok {
		capture := value.(*upstreamEvidenceCapture)
		capture.mu.Lock()
		for _, attempt := range capture.attempts {
			evidence.Upstream = append(evidence.Upstream, attempt.snapshot())
		}
		evidence.AttemptsTruncated = capture.count > len(capture.attempts)
		capture.mu.Unlock()
	}
	if value, ok := c.Get(failureObservationKey); ok && (info == nil || info.ClientWs == nil && info.RelayFormat != types.RelayFormatOpenAIRealtime) {
		state := value.(*failureOutputObservation)
		state.mu.Lock()
		status := 0
		if c.Writer.Written() {
			status = c.Writer.Status()
		}
		evidence.Downstream = &model.DownstreamEvidence{StatusCode: status, Usage: cloneEvidenceUsage(state.delivery.usage), Completed: state.delivery.completed, Failed: state.delivery.failed, WriteFailed: state.writeFailed, CaptureIncomplete: state.delivery.incomplete || len(state.delivery.pending) > 0 || len(state.delivery.event) > 0}
		state.mu.Unlock()
	}
	if value, ok := c.Get(localSettlementEvidenceKey); ok {
		evidence.Local = value.(*model.LocalSettlementEvidence)
	}
	other.SetAdmin("settlement_evidence", evidence)
}

// Snapshot maps must not alias observers still receiving bytes after a log.
func cloneEvidenceUsage(usage map[string]json.RawMessage) map[string]json.RawMessage {
	if usage == nil {
		return nil
	}
	copy := make(map[string]json.RawMessage, len(usage))
	for key, value := range usage {
		copy[key] = bytes.Clone(value)
	}
	return copy
}
