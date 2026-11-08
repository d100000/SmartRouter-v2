package common

import (
	"github.com/QuantumNous/new-api/pkg/billingexpr"
)

// TaskChannelSnapshot binds asynchronous work to the channel and procurement
// rules that actually submitted it. Connection is encrypted for protected
// credentials; it never enters public task or supplier log projections.
type TaskChannelSnapshot struct {
	TemplateID            string                       `json:"template_id,omitempty"`
	IsChannelTest         bool                         `json:"is_channel_test,omitempty"`
	AttemptID             string                       `json:"attempt_id"`
	ChannelID             int                          `json:"channel_id"`
	SupplierID            string                       `json:"supplier_id,omitempty"`
	Connection            string                       `json:"connection"`
	Encrypted             bool                         `json:"encrypted"`
	CredentialID          string                       `json:"credential_id,omitempty"`
	CredentialVersionID   string                       `json:"credential_version_id,omitempty"`
	BindingID             string                       `json:"binding_id,omitempty"`
	Overview              *OverviewAttempt             `json:"overview,omitempty"`
	CostRatio             *float64                     `json:"cost_ratio"`
	QuotaPerUnit          float64                      `json:"quota_per_unit"`
	ModelPrice            float64                      `json:"model_price"`
	UsePrice              bool                         `json:"use_price"`
	OtherRatios           map[string]float64           `json:"other_ratios,omitempty"`
	Expression            *billingexpr.BillingSnapshot `json:"expression,omitempty"`
	UsageUnits            map[string]string            `json:"usage_units,omitempty"`
	TPMApplicable         bool                         `json:"tpm_applicable"`
	MediaKind             string                       `json:"media_kind,omitempty"`
	LastConfirmedStatus   string                       `json:"last_confirmed_status,omitempty"`
	LastConfirmedProgress string                       `json:"last_confirmed_progress,omitempty"`
	ModelRatio            float64                      `json:"model_ratio"`
	Observation           TaskChannelObservation       `json:"observation"`
}

// TaskChannelObservation is a durable, credential-free completion outbox.
// A gateway timeout/refund leaves procurement amounts unknown.
type TaskChannelObservation struct {
	Status        string   `json:"status"`
	ObservedAt    int64    `json:"observed_at"`
	CompletedAt   *int64   `json:"completed_at,omitempty"`
	Authoritative bool     `json:"authoritative"`
	CostUSDNano   *int64   `json:"cost_usd_nano,omitempty"`
	Tokens        *int64   `json:"tokens,omitempty"`
	ImageCount    *int64   `json:"image_count,omitempty"`
	VideoSeconds  *float64 `json:"video_seconds,omitempty"`
	AudioSeconds  *float64 `json:"audio_seconds,omitempty"`
	Credits       *string  `json:"credits,omitempty"`
	Progress      string   `json:"progress,omitempty"`
}

// NeedsReconciliation includes completed jobs whose billable usage arrives
// later than the terminal status. Customer completion must not stop those
// procurement corrections or clear their unknown amounts.
func (snapshot *TaskChannelSnapshot) NeedsReconciliation() bool {
	if snapshot == nil {
		return false
	}
	observation := snapshot.Observation
	if observation.Status == "unknown" {
		return true
	}
	if observation.Status != "succeeded" && observation.Status != "failed" {
		return false
	}
	return snapshot.CostRatio != nil && observation.CostUSDNano == nil || snapshot.TPMApplicable && observation.Tokens == nil
}
