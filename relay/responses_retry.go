package relay

import (
	"strings"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Inspect the serialized outbound body AFTER conversion and parameter overrides.
// Unknown extensions and server-owned state/tools fail closed. Passthrough is
// deliberately outside v1: its body is not the validated serialized request.
func responsesRequestAllowsOverloadRetry(c *gin.Context, info *relaycommon.RelayInfo, body []byte) bool {
	if !info.ChannelSetting.ResponsesOverloadRetryEnabled || !info.IsStream ||
		info.RelayMode != relayconstant.RelayModeResponses || info.RelayFormat != types.RelayFormatOpenAIResponses ||
		c.Request.Method != "POST" || c.GetHeader("X-Codex-Turn-State") != "" {
		return false
	}
	switch info.ChannelType {
	case constant.ChannelTypeOpenAI, constant.ChannelTypeSub2API, constant.ChannelTypeNewAPI:
	default:
		return false
	}
	for key := range info.HeadersOverride {
		if strings.EqualFold(key, "X-Codex-Turn-State") {
			return false
		}
	}
	root := gjson.ParseBytes(body)
	if root.Get("stream").Type != gjson.True {
		return false
	}
	safe := true
	root.ForEach(func(key, value gjson.Result) bool {
		switch key.Str {
		case "conversation", "previous_response_id", "prompt":
			safe = value.Type == gjson.Null || value.Type == gjson.String && value.Str == ""
		case "background":
			safe = value.Type == gjson.Null || value.Type == gjson.False
		case "tools":
			if value.Type == gjson.Null {
				break
			}
			safe = value.IsArray()
			for _, tool := range value.Array() {
				// Client-executed function declarations are safe until any output
				// activity occurs. Hosted tools, MCP and unknown types are excluded.
				if tool.Get("type").Str != "function" {
					safe = false
					break
				}
			}
		case "input":
			// References to stored items or files cannot be replayed safely on
			// another upstream account. Self-contained history stays supported.
			for _, item := range value.Array() {
				if item.Get("type").Str == "item_reference" || item.Get("file_id").Exists() {
					safe = false
					break
				}
				for _, content := range item.Get("content").Array() {
					if content.Get("file_id").Exists() {
						safe = false
						break
					}
				}
			}
		case "model", "instructions", "include", "max_output_tokens", "top_logprobs",
			"metadata", "parallel_tool_calls", "reasoning", "service_tier", "store", "prompt_cache_key",
			"prompt_cache_options", "prompt_cache_retention", "safety_identifier", "stream", "stream_options",
			"temperature", "text", "tool_choice", "top_p", "truncation", "user", "max_tool_calls",
			"client_metadata", "frequency_penalty", "presence_penalty":
		default:
			safe = false
		}
		return safe
	})
	return safe
}
