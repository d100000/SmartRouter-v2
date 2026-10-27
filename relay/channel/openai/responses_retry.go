package openai

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Only known initialization envelopes with no execution/usage facts can remain
// invisible to the client. A new event or unknown field closes the retry window.
func responsesEventHasNoActivity(data string) bool {
	root := gjson.Parse(data)
	kind := root.Get("type").Str
	initialization := kind == "response.created" || kind == "response.queued" || kind == "response.in_progress"
	safe := root.IsObject()
	seen := make(map[string]bool)
	root.ForEach(func(key, value gjson.Result) bool {
		// Duplicate fields can disagree between the streaming and DTO parsers.
		// Ambiguous envelopes must never hide usage or execution evidence.
		if seen[key.Str] {
			safe = false
			return false
		}
		seen[key.Str] = true
		switch key.Str {
		case "error":
			safe = !initialization || value.Type == gjson.Null
		case "type", "sequence_number", "response", "code", "message", "param", "event_id":
		default:
			safe = false
		}
		return safe
	})
	response := root.Get("response")
	if !safe || response.Exists() && !response.IsObject() {
		return false
	}
	clear(seen)
	response.ForEach(func(key, value gjson.Result) bool {
		if seen[key.Str] {
			safe = false
			return false
		}
		seen[key.Str] = true
		switch key.Str {
		case "output":
			safe = value.IsArray() && len(value.Array()) == 0
		case "usage", "conversation", "previous_response_id", "completed_at", "incomplete_details":
			safe = value.Type == gjson.Null
		case "status":
			safe = value.Type == gjson.String && (initialization && (value.Str == "queued" || value.Str == "in_progress") || !initialization && value.Str == "failed")
		case "error":
			safe = !initialization || value.Type == gjson.Null
		case "background":
			safe = value.Type == gjson.False || value.Type == gjson.Null
		case "tools":
			safe = value.Type == gjson.Null || value.IsArray()
			for _, tool := range value.Array() {
				if tool.Get("type").Str != "function" {
					safe = false
					break
				}
			}
		case "id", "object", "created_at",
			"model", "instructions", "max_output_tokens", "max_tool_calls", "metadata", "parallel_tool_calls",
			"reasoning", "service_tier", "store", "temperature", "text", "tool_choice", "top_p", "top_logprobs",
			"truncation", "user", "safety_identifier", "prompt_cache_key", "prompt_cache_retention":
		default:
			safe = false
		}
		return safe
	})
	return safe
}

func responsesStreamError(c *gin.Context, data, event string) *types.NewAPIError {
	root := gjson.Parse(data)
	kind := root.Get("type").Str
	if kind != "error" && kind != "response.error" && kind != "response.failed" &&
		event != "error" && root.Get("response.status").Str != "failed" {
		return nil
	}
	errData := root.Get("response.error")
	if !errData.IsObject() {
		errData = root.Get("error")
	}
	if !errData.IsObject() {
		errData = root
	}
	var upstream types.OpenAIError
	_ = common.UnmarshalJsonStr(errData.Raw, &upstream)
	upstream.Code = streamErrorIdentifier(upstream.Code, string(types.ErrorCodeBadResponse))
	upstream.Type = streamErrorIdentifier(upstream.Type, "upstream_error")
	if strings.TrimSpace(upstream.Message) == "" {
		upstream.Message = i18n.T(c, i18n.MsgRelayStreamUpstreamError)
	}
	status := http.StatusBadGateway
	if upstream.Code == "server_is_overloaded" {
		status = http.StatusServiceUnavailable
	}
	apiErr := types.WithOpenAIError(upstream, status, types.ErrOptionWithSkipRetry())
	sanitizeStreamError(c, apiErr)
	return apiErr
}

// Output evidence is independent of tool prices: an unpriced function call is
// still output and must never be replayed. Empty message shells are not output.
func responsesEventHasOutput(event *dto.ResponsesStreamResponse) bool {
	if strings.TrimSpace(event.Delta) != "" || event.Text != nil && strings.TrimSpace(*event.Text) != "" ||
		event.Arguments != nil && *event.Arguments != "" || event.Input != nil && *event.Input != "" {
		return true
	}
	var items []dto.ResponsesOutput
	if event.Response != nil {
		items = event.Response.Output
	}
	if event.Item != nil {
		items = append(items[:len(items):len(items)], *event.Item)
	}
	for _, item := range items {
		if item.Name != "" || item.Result != "" || strings.HasSuffix(item.Type, "_call") && item.Status == "completed" {
			return true
		}
		for _, content := range item.Content {
			if strings.TrimSpace(content.Text) != "" {
				return true
			}
		}
		for _, summary := range item.Summary {
			if strings.TrimSpace(summary.Text) != "" {
				return true
			}
		}
	}
	return false
}

// A valid Retry-After is a minimum. Large values are retained as a stop signal
// for the controller, never shortened into an earlier retry.
func responsesRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		if err != nil || seconds > uint64((1<<63-1)/int64(time.Second)) {
			return time.Duration(1<<63 - 1)
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(0, time.Until(date))
	}
	return 0
}
