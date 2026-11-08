package model

import (
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ChannelLimitEvent records facts in the same primary-database transaction as
// the corresponding ledger change. It contains no credentials or end-user data.
type ChannelLimitEvent struct {
	ID             string `json:"id" gorm:"type:varchar(36);primaryKey"`
	ChannelID      int    `json:"channel_id" gorm:"index:idx_channel_limit_event,priority:1"`
	Kind           string `json:"kind" gorm:"type:varchar(32)"`
	BudgetDay      string `json:"budget_day" gorm:"type:varchar(10)"`
	AttemptID      string `json:"attempt_id,omitempty" gorm:"type:varchar(128)"`
	ConfigRevision int64  `json:"config_revision"`
	ResultVersion  int64  `json:"result_version,omitempty"`
	OccurredAtMS   int64  `json:"occurred_at_ms" gorm:"index:idx_channel_limit_event,priority:2"`
	Data           string `json:"data" gorm:"type:text"`
}

func recordChannelLimitEvent(tx *gorm.DB, config *ChannelLimitConfig, kind, subject, day, attemptID string, resultVersion int64, now time.Time, data any) error {
	encoded, err := common.Marshal(data)
	if err != nil {
		return err
	}
	event := ChannelLimitEvent{
		ID:        uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("channel-limit:%d:%s:%s", config.ChannelID, kind, subject))).String(),
		ChannelID: config.ChannelID, Kind: kind, BudgetDay: day, AttemptID: attemptID,
		ConfigRevision: config.Revision, ResultVersion: resultVersion, OccurredAtMS: now.UnixMilli(), Data: string(encoded),
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&event).Error
}

func channelLimitConfigAudit(config ChannelLimitConfig) map[string]any {
	var rpm, tpm *int64
	var daily *string
	if config.RPM > 0 {
		rpm = &config.RPM
	}
	if config.TPM > 0 {
		tpm = &config.TPM
	}
	if config.DailyUSDNano > 0 {
		amount := FormatChannelLimitUSD(config.DailyUSDNano)
		daily = &amount
	}
	return map[string]any{"rpm": rpm, "tpm": tpm, "daily_usd": daily, "revision": config.Revision, "budget_ready": config.BudgetReady, "reconcile_seconds": config.ReconcileSeconds}
}

func recordChannelDailyTransition(tx *gorm.DB, config *ChannelLimitConfig, account ChannelDailyBudget, wasExhausted bool, now time.Time) error {
	if wasExhausted == account.Exhausted {
		return nil
	}
	kind := "daily_restored"
	if account.Exhausted {
		kind = "daily_exhausted"
	}
	return recordChannelLimitEvent(tx, config, kind, fmt.Sprintf("%s:%d", account.BudgetDay, config.Revision), account.BudgetDay, "", 0, now,
		map[string]any{"daily_used_usd": FormatChannelLimitUSD(account.UsedUSDNano), "daily_reserved_usd": FormatChannelLimitUSD(account.HeldUSDNano), "daily_limit_usd": FormatChannelLimitUSD(config.DailyUSDNano)})
}
