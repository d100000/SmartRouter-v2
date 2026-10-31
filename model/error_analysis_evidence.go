package model

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/tidwall/gjson"
)

// Evidence is diagnostic only. Provider usage is not an invoice, and a
// successful socket write is not an acknowledgement of downstream settlement.
type RelaySettlementEvidence struct {
	Version           int                        `json:"version"`
	UsageSource       string                     `json:"usage_source,omitempty"`
	Upstream          []UpstreamResponseEvidence `json:"upstream,omitempty"`
	AttemptsTruncated bool                       `json:"attempts_truncated,omitempty"`
	Downstream        *DownstreamEvidence        `json:"downstream,omitempty"`
	Local             *LocalSettlementEvidence   `json:"local,omitempty"`
}

type UpstreamResponseEvidence struct {
	Attempt           int                        `json:"attempt"`
	Channel           int                        `json:"channel"`
	RequestID         string                     `json:"request_id,omitempty"`
	StatusCode        int                        `json:"status_code"`
	ErrorBody         string                     `json:"error_body,omitempty"`
	ErrorTruncated    bool                       `json:"error_truncated,omitempty"`
	Usage             map[string]json.RawMessage `json:"usage,omitempty"`
	CaptureIncomplete bool                       `json:"capture_incomplete,omitempty"`
}

type DownstreamEvidence struct {
	StatusCode        int                        `json:"status_code"`
	Usage             map[string]json.RawMessage `json:"usage,omitempty"`
	Completed         bool                       `json:"completed"`
	Failed            bool                       `json:"failed"`
	WriteFailed       bool                       `json:"write_failed"`
	CaptureIncomplete bool                       `json:"capture_incomplete"`
}

type LocalSettlementEvidence struct {
	State            string `json:"state"`
	RequestedQuota   int    `json:"requested_quota"`
	ConfirmedQuota   int64  `json:"confirmed_quota"`
	PreConsumedQuota int    `json:"pre_consumed_quota"`
	Error            string `json:"error,omitempty"`
}

var failureCredentialPattern = regexp.MustCompile(`(?i)(["']?(?:authorization|api[_-]?key|access[_-]?token|refresh[_-]?token|password|secret|cookie)["']?\s*[:=]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;}]+)`)

// SafeFailureDetail retains the error envelope while removing common secret
// and echoed request fields. Never pass request/response success bodies here.
func SafeFailureDetail(value string, limit int) (string, bool) {
	truncated := len([]rune(value)) > limit
	if gjson.Valid(value) {
		object := redactFailureValue(gjson.Parse(value), 0)
		if data, err := common.Marshal(object); err == nil {
			value = string(data)
		}
	} else {
		value = failureCredentialPattern.ReplaceAllString(value, "$1[redacted]")
		value = SafeFailureFact(value, limit+1)
	}
	characters := []rune(value)
	return string(characters[:min(len(characters), limit)]), truncated || len(characters) > limit
}

func redactFailureValue(value gjson.Result, depth int) any {
	if depth > 20 {
		return "[redacted]"
	}
	if value.IsObject() {
		result := map[string]any{}
		value.ForEach(func(keyValue, item gjson.Result) bool {
			key := keyValue.String()
			normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", ""))
			switch normalized {
			case "authorization", "apikey", "accesskey", "token", "accesstoken", "refreshtoken", "password", "secret", "cookie", "headers", "request", "messages", "prompt", "input", "output", "content":
				result[key] = "[redacted]"
			default:
				result[key] = redactFailureValue(item, depth+1)
			}
			return true
		})
		return result
	}
	if value.IsArray() {
		result := []any{}
		value.ForEach(func(_, item gjson.Result) bool {
			result = append(result, redactFailureValue(item, depth+1))
			return true
		})
		return result
	}
	if value.Type == gjson.String {
		text := value.String()
		return SafeFailureFact(failureCredentialPattern.ReplaceAllString(text, "$1[redacted]"), len([]rune(text)))
	}
	// Preserve the exact provider number, including integers beyond 2^53 and
	// decimal costs. Diagnostic sanitization must not round usage through float64.
	return json.RawMessage(value.Raw)
}

func HasReportedUsage(usage map[string]json.RawMessage) bool {
	for _, raw := range usage {
		if positiveUsageValue(gjson.ParseBytes(raw)) {
			return true
		}
	}
	return false
}

func positiveUsageValue(value gjson.Result) bool {
	if value.Type == gjson.Number {
		return value.Float() > 0
	}
	if !value.IsObject() && !value.IsArray() {
		return false
	}
	positive := false
	value.ForEach(func(_, item gjson.Result) bool { positive = positiveUsageValue(item); return !positive })
	return positive
}
