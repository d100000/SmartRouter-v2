package model

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	ChannelLimitUSDScale         int64 = 1_000_000_000
	ChannelLimitMaxValue         int64 = 1<<53 - 1
	ChannelLimitDefaultReconcile       = 300
	channelLimitMinuteMS         int64 = 60_000
)

var (
	ErrChannelLimitInvalid     = errors.New("invalid channel limit value")
	ErrChannelLimitRevision    = errors.New("channel limits changed; reload before saving")
	ErrChannelLimitAccounting  = errors.New("channel limit accounting is incomplete")
	ErrChannelAttemptState     = errors.New("invalid channel attempt state")
	ErrChannelAttemptDuplicate = errors.New("channel attempt already exists")
	channelBudgetLocation      = func() *time.Location {
		location, err := time.LoadLocation("Asia/Shanghai")
		if err != nil {
			panic("embedded Asia/Shanghai timezone is unavailable")
		}
		return location
	}()
)

// Zero is the internal disabled representation. Public management APIs must use
// null for disabled limits and reject an explicitly supplied numerical zero.
type ChannelLimitSettings struct {
	RPM              int64 `json:"rpm" gorm:"bigint"`
	TPM              int64 `json:"tpm" gorm:"bigint"`
	DailyUSDNano     int64 `json:"-" gorm:"bigint"`
	ReconcileSeconds int64 `json:"reconcile_seconds" gorm:"bigint"`
}

type ChannelLimitConfig struct {
	ChannelID int `json:"channel_id" gorm:"primaryKey;autoIncrement:false"`
	ChannelLimitSettings
	Revision        int64  `json:"revision" gorm:"bigint"`
	BudgetReady     bool   `json:"budget_ready"`
	HistoryComplete bool   `json:"-"`
	ObservedSince   int64  `json:"-" gorm:"bigint"`
	LockVersion     int64  `json:"-" gorm:"bigint"`
	BudgetDay       string `json:"-" gorm:"type:varchar(10)"`
}

// This is a procurement ledger, never the consumer's wallet or refund ledger.
type ChannelDailyBudget struct {
	ID           string `json:"-" gorm:"type:varchar(48);primaryKey"`
	ChannelID    int    `json:"channel_id" gorm:"index"`
	BudgetDay    string `json:"budget_day" gorm:"type:varchar(10);index"`
	UsedUSDNano  int64  `json:"-" gorm:"bigint"`
	HeldUSDNano  int64  `json:"-" gorm:"bigint"`
	UnknownCosts int64  `json:"-" gorm:"bigint"`
	Exhausted    bool   `json:"exhausted"`
}

// A separate record is required for every actual upstream attempt, including
// failed attempts. It intentionally contains no consumer identity or secrets.
type ChannelLimitAttempt struct {
	ID                 string   `json:"id" gorm:"type:varchar(128);primaryKey"`
	ChannelID          int      `json:"channel_id" gorm:"index:idx_cl_attempt_window,priority:1;index:idx_cl_attempt_reconcile,priority:1"`
	BudgetDay          string   `json:"budget_day" gorm:"type:varchar(10)"`
	CreatedAtMS        int64    `json:"created_at" gorm:"bigint"`
	SentAtMS           int64    `json:"sent_at" gorm:"bigint;index:idx_cl_attempt_window,priority:2"`
	Released           bool     `json:"-"`
	Completed          bool     `json:"completed"`
	TPMApplicable      bool     `json:"-"`
	TokenBudgetKnown   bool     `json:"-"`
	ReservedTokens     int64    `json:"-" gorm:"bigint"`
	ObservedTokens     int64    `json:"-" gorm:"bigint"`
	ObservedCostNano   int64    `json:"-" gorm:"bigint"`
	TokenActualKnown   bool     `json:"-"`
	ActualTokens       int64    `json:"-" gorm:"bigint"`
	TPMActiveTokens    int64    `json:"-" gorm:"bigint"`
	TPMChargedTokens   int64    `json:"-" gorm:"bigint"`
	TPMExpiresAtMS     int64    `json:"-" gorm:"bigint;index"`
	TPMEstimated       bool     `json:"tpm_estimated"`
	TPMClosedAtMS      int64    `json:"tpm_closed_at" gorm:"bigint"`
	ReconcileSeconds   int64    `json:"-" gorm:"bigint"`
	ReconcileStartedMS int64    `json:"-" gorm:"bigint"`
	ReconcileUntilMS   int64    `json:"reconcile_until" gorm:"bigint;index:idx_cl_attempt_reconcile,priority:2"`
	ExecutionLeaseMS   int64    `json:"-" gorm:"bigint;index"`
	CostBudgetKnown    bool     `json:"-"`
	HeldUSDNano        int64    `json:"-" gorm:"bigint"`
	CostActualKnown    bool     `json:"-"`
	ActualUSDNano      int64    `json:"-" gorm:"bigint"`
	ResultVersion      int64    `json:"-" gorm:"bigint"`
	CostRatio          *float64 `json:"-"`
	QuotaPerUnit       float64  `json:"-"`
	PricingSnapshot    string   `json:"-" gorm:"type:text"`
}

type ChannelAttemptReservation struct {
	ID              string
	ChannelID       int
	TPMApplicable   bool
	TokenBudget     *int64
	CostBudgetNano  *int64
	CostRatio       *float64
	QuotaPerUnit    float64
	PricingSnapshot string
}

// Nil usage means unknown, not zero. Values must come from the selected
// attempt's verified usage and frozen procurement-price snapshot.
type ChannelAttemptSettlement struct {
	ID          string
	ChannelID   int
	Version     int64
	Tokens      *int64
	CostUSDNano *int64
}

type ChannelLimitState struct {
	Config          ChannelLimitConfig `json:"config"`
	RPMUsed         int64              `json:"rpm_used"`
	RPMReserved     int64              `json:"rpm_reserved"`
	TPMUsed         int64              `json:"tpm_used"`
	TPMReserved     int64              `json:"tpm_reserved"`
	DailyUsedNano   int64              `json:"-"`
	DailyHeldNano   int64              `json:"-"`
	DailyRemaining  int64              `json:"-"`
	BudgetDay       string             `json:"budget_day"`
	ResetAt         int64              `json:"reset_at"`
	Reason          string             `json:"reason"`
	UnknownTokens   bool               `json:"-"`
	UnknownCosts    int64              `json:"-"`
	BudgetExhausted bool               `json:"-"`
}

type ChannelLimitError struct {
	Reason  string
	RetryAt int64 // Unix milliseconds; zero when a retry time cannot be known.
}

func (e *ChannelLimitError) Error() string { return "channel limit: " + e.Reason }

func AutoMigrateChannelLimitSchema(db *gorm.DB) error {
	return db.AutoMigrate(&ChannelLimitConfig{}, &ChannelDailyBudget{}, &ChannelLimitAttempt{}, &ChannelLimitEvent{})
}

func ParseChannelLimitUSD(value string) (int64, error) {
	v, err := decimal.NewFromString(value)
	if err != nil || v.IsNegative() {
		return 0, ErrChannelLimitInvalid
	}
	nano := v.Mul(decimal.NewFromInt(ChannelLimitUSDScale))
	if !nano.Equal(nano.Truncate(0)) || nano.GreaterThan(decimal.NewFromInt(ChannelLimitMaxValue)) {
		return 0, ErrChannelLimitInvalid
	}
	return nano.IntPart(), nil
}

func FormatChannelLimitUSD(nano int64) string {
	return decimal.NewFromInt(nano).Div(decimal.NewFromInt(ChannelLimitUSDScale)).String()
}

// ProcurementCostUSDNano converts a frozen base-quota cost into fixed-point
// USD. Round upwards only at the nine-decimal accounting boundary, never from
// an interface display value. Customer revenue must not be supplied here.
func ProcurementCostUSDNano(baseQuota, costRatio, quotaPerUnit float64) (int64, error) {
	if math.IsNaN(baseQuota) || math.IsInf(baseQuota, 0) || baseQuota < 0 ||
		math.IsNaN(costRatio) || math.IsInf(costRatio, 0) || costRatio < 0 || costRatio > 100 ||
		math.IsNaN(quotaPerUnit) || math.IsInf(quotaPerUnit, 0) || quotaPerUnit <= 0 {
		return 0, ErrChannelLimitAccounting
	}
	v := decimal.NewFromFloat(baseQuota).Mul(decimal.NewFromFloat(costRatio)).
		Mul(decimal.NewFromInt(ChannelLimitUSDScale)).Div(decimal.NewFromFloat(quotaPerUnit)).Ceil()
	if v.GreaterThan(decimal.NewFromInt(ChannelLimitMaxValue)) {
		return 0, ErrChannelLimitAccounting
	}
	return v.IntPart(), nil
}

func channelBudgetDay(now time.Time) (string, int64, int64) {
	now = now.In(channelBudgetLocation)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, channelBudgetLocation)
	return start.Format("2006-01-02"), start.UnixMilli(), start.AddDate(0, 0, 1).Unix()
}

func EnsureChannelLimitTracking(channelID int, newChannel bool, now time.Time, transactions ...*gorm.DB) error {
	if channelID <= 0 {
		return ErrChannelLimitInvalid
	}
	config := ChannelLimitConfig{ChannelID: channelID, Revision: 1, ObservedSince: now.UnixMilli(), HistoryComplete: newChannel,
		ChannelLimitSettings: ChannelLimitSettings{ReconcileSeconds: ChannelLimitDefaultReconcile}}
	db := DB
	if len(transactions) > 0 && transactions[0] != nil {
		db = transactions[0]
	}
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&config).Error
}

// The first write takes SQLite's writer lock before any reads; MySQL and
// PostgreSQL serialize the same channel on its configuration row. No local
// mutex, cache, log database or asynchronous projection grants permissions.
func channelLimitTransaction(channelID int, now time.Time, fn func(*gorm.DB, *ChannelLimitConfig) error, transactions ...*gorm.DB) error {
	db := DB
	if len(transactions) > 0 {
		db = transactions[0]
	}
	if err := EnsureChannelLimitTracking(channelID, false, now, db); err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&ChannelLimitConfig{}).Where("channel_id = ?", channelID).
			UpdateColumn("lock_version", gorm.Expr("lock_version + 1")).Error; err != nil {
			return err
		}
		var config ChannelLimitConfig
		if err := lockForUpdate(tx).First(&config, "channel_id = ?", channelID).Error; err != nil {
			return err
		}
		day, dayStart, _ := channelBudgetDay(now)
		if config.BudgetDay < day {
			previousDay := config.BudgetDay
			config.BudgetDay = day
			if err := tx.Model(&config).Update("budget_day", day).Error; err != nil {
				return err
			}
			if previousDay != "" && config.DailyUSDNano > 0 {
				if err := recordChannelLimitEvent(tx, &config, "daily_reset", day, day, "", 0, time.UnixMilli(dayStart), map[string]any{"previous_day": previousDay, "timezone": "Asia/Shanghai", "reason": "new_budget_day"}); err != nil {
					return err
				}
			}
		}
		return fn(tx, &config)
	})
}

func UpdateChannelLimits(channelID int, revision int64, settings ChannelLimitSettings, costReady bool, now time.Time, transactions ...*gorm.DB) (ChannelLimitConfig, error) {
	var updated ChannelLimitConfig
	if settings.RPM < 0 || settings.TPM < 0 || settings.DailyUSDNano < 0 ||
		settings.RPM > ChannelLimitMaxValue || settings.TPM > ChannelLimitMaxValue || settings.DailyUSDNano > ChannelLimitMaxValue ||
		settings.ReconcileSeconds <= 0 || settings.ReconcileSeconds > 86400 {
		return updated, ErrChannelLimitInvalid
	}
	err := channelLimitTransaction(channelID, now, func(tx *gorm.DB, config *ChannelLimitConfig) error {
		if config.Revision != revision {
			return ErrChannelLimitRevision
		}
		before := *config
		state, err := readChannelLimitState(tx, *config, now)
		if err != nil {
			return err
		}
		_, dayStart, _ := channelBudgetDay(now)
		if settings.DailyUSDNano > 0 && config.DailyUSDNano == 0 &&
			(!config.HistoryComplete && config.ObservedSince > dayStart || state.UnknownCosts > 0) {
			return ErrChannelLimitAccounting
		}
		if (settings.RPM > 0 && config.RPM == 0 || settings.TPM > 0 && config.TPM == 0) &&
			!config.HistoryComplete && config.ObservedSince > now.UnixMilli()-channelLimitMinuteMS {
			return ErrChannelLimitAccounting
		}
		if settings.TPM > 0 && config.TPM == 0 && state.UnknownTokens {
			return ErrChannelLimitAccounting
		}
		account, err := getChannelDailyBudget(tx, channelID, state.BudgetDay)
		if err != nil {
			return err
		}
		wasExhausted := account.Exhausted
		if settings.DailyUSDNano == 0 || settings.DailyUSDNano > config.DailyUSDNano &&
			account.UsedUSDNano < settings.DailyUSDNano && account.HeldUSDNano <= settings.DailyUSDNano-account.UsedUSDNano {
			account.Exhausted = false
		}
		if settings.DailyUSDNano > 0 && account.UsedUSDNano >= settings.DailyUSDNano {
			account.Exhausted = true
		}
		if err := tx.Save(&account).Error; err != nil {
			return err
		}
		config.ChannelLimitSettings = settings
		config.BudgetReady = costReady
		config.Revision++
		if err := tx.Save(config).Error; err != nil {
			return err
		}
		if err := recordChannelLimitEvent(tx, config, "config_changed", fmt.Sprint(config.Revision), state.BudgetDay, "", 0, now, map[string]any{"before": channelLimitConfigAudit(before), "after": channelLimitConfigAudit(*config)}); err != nil {
			return err
		}
		if err := recordChannelDailyTransition(tx, config, account, wasExhausted, now); err != nil {
			return err
		}
		updated = *config
		return nil
	}, transactions...)
	return updated, err
}

func getChannelDailyBudget(tx *gorm.DB, channelID int, day string) (ChannelDailyBudget, error) {
	account := ChannelDailyBudget{ID: fmt.Sprintf("%d:%s", channelID, day), ChannelID: channelID, BudgetDay: day}
	err := tx.First(&account, "id = ?", account.ID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = tx.Create(&account).Error
	}
	return account, err
}

func GetChannelLimitState(channelID int, now time.Time, transactions ...*gorm.DB) (ChannelLimitState, error) {
	db := DB
	if len(transactions) > 0 {
		db = transactions[0]
	}
	var config ChannelLimitConfig
	err := db.First(&config, "channel_id = ?", channelID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		config = ChannelLimitConfig{ChannelID: channelID, Revision: 1, ChannelLimitSettings: ChannelLimitSettings{ReconcileSeconds: ChannelLimitDefaultReconcile}}
		day, _, reset := channelBudgetDay(now)
		return ChannelLimitState{Config: config, BudgetDay: day, ResetAt: reset}, nil
	}
	if err != nil {
		return ChannelLimitState{}, err
	}
	return readChannelLimitState(db, config, now)
}

func readChannelLimitState(tx *gorm.DB, config ChannelLimitConfig, now time.Time) (ChannelLimitState, error) {
	day, _, reset := channelBudgetDay(now)
	s := ChannelLimitState{Config: config, BudgetDay: day, ResetAt: reset}
	var attempts []ChannelLimitAttempt
	err := tx.Where("channel_id = ? AND released = ? AND (sent_at_ms > ? OR tpm_expires_at_ms > ? OR tpm_expires_at_ms = ?)",
		config.ChannelID, false, now.UnixMilli()-channelLimitMinuteMS, now.UnixMilli(), 0).Find(&attempts).Error
	if err != nil {
		return s, err
	}
	for _, attempt := range attempts {
		if attempt.SentAtMS == 0 && attempt.CreatedAtMS+channelLimitMinuteMS <= now.UnixMilli() {
			continue
		}
		if attempt.SentAtMS == 0 {
			s.RPMReserved++
		} else if attempt.SentAtMS > now.UnixMilli()-channelLimitMinuteMS {
			s.RPMUsed++
		}
		if !attempt.TPMApplicable || attempt.TPMExpiresAtMS > 0 && attempt.TPMExpiresAtMS <= now.UnixMilli() {
			continue
		}
		if !attempt.TokenBudgetKnown && !attempt.TokenActualKnown {
			s.UnknownTokens = true
		}
		if attempt.TPMActiveTokens > ChannelLimitMaxValue-s.TPMUsed-s.TPMReserved {
			return s, ErrChannelLimitAccounting
		}
		if attempt.TPMExpiresAtMS == 0 {
			s.TPMReserved += attempt.TPMActiveTokens
		} else {
			s.TPMUsed += attempt.TPMActiveTokens
		}
	}
	var account ChannelDailyBudget
	err = tx.First(&account, "id = ?", fmt.Sprintf("%d:%s", config.ChannelID, day)).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return s, err
	}
	s.DailyUsedNano, s.DailyHeldNano = account.UsedUSDNano, account.HeldUSDNano
	s.UnknownCosts, s.BudgetExhausted = account.UnknownCosts, account.Exhausted
	if config.DailyUSDNano > 0 {
		s.DailyRemaining = max(0, config.DailyUSDNano-account.UsedUSDNano-account.HeldUSDNano)
	}
	switch {
	case config.DailyUSDNano > 0 && !config.BudgetReady:
		s.Reason = "cost_config_missing"
	case config.DailyUSDNano > 0 && account.UnknownCosts > 0 || config.TPM > 0 && s.UnknownTokens:
		s.Reason = "accounting_unavailable"
	case config.DailyUSDNano > 0 && (account.Exhausted || account.UsedUSDNano >= config.DailyUSDNano):
		s.Reason = "daily_exhausted"
	case config.DailyUSDNano > 0 && s.DailyRemaining == 0:
		s.Reason = "budget_reserved"
	case config.RPM > 0 && s.RPMUsed+s.RPMReserved >= config.RPM:
		s.Reason = "rpm_limited"
	case config.TPM > 0 && s.TPMUsed+s.TPMReserved >= config.TPM:
		s.Reason = "tpm_limited"
	}
	return s, nil
}

func ReserveChannelAttempt(input ChannelAttemptReservation, now time.Time) (*ChannelLimitAttempt, error) {
	if input.ChannelID <= 0 || input.ID == "" || len(input.ID) > 128 || strings.TrimSpace(input.ID) != input.ID ||
		input.TokenBudget != nil && (*input.TokenBudget < 0 || *input.TokenBudget > ChannelLimitMaxValue) ||
		input.CostBudgetNano != nil && (*input.CostBudgetNano < 0 || *input.CostBudgetNano > ChannelLimitMaxValue) {
		return nil, ErrChannelLimitInvalid
	}
	if input.CostRatio != nil {
		if _, err := ProcurementCostUSDNano(0, *input.CostRatio, 1); err != nil {
			return nil, err
		}
	}
	if math.IsNaN(input.QuotaPerUnit) || math.IsInf(input.QuotaPerUnit, 0) || input.QuotaPerUnit < 0 {
		return nil, ErrChannelLimitInvalid
	}
	var result ChannelLimitAttempt
	err := channelLimitTransaction(input.ChannelID, now, func(tx *gorm.DB, config *ChannelLimitConfig) error {
		if err := reconcileChannelAttempts(tx, config, now); err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&ChannelLimitAttempt{}).Where("id = ?", input.ID).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return ErrChannelAttemptDuplicate
		}
		state, err := readChannelLimitState(tx, *config, now)
		if err != nil {
			return err
		}
		if err := admitChannelAttempt(*config, state, input.TokenBudget, input.TPMApplicable, input.CostBudgetNano); err != nil {
			return err
		}
		result = ChannelLimitAttempt{ID: input.ID, ChannelID: input.ChannelID, BudgetDay: state.BudgetDay,
			CreatedAtMS: now.UnixMilli(), TPMApplicable: input.TPMApplicable,
			TokenBudgetKnown: input.TokenBudget != nil, CostBudgetKnown: input.CostBudgetNano != nil,
			ReconcileSeconds: config.ReconcileSeconds, CostRatio: input.CostRatio, QuotaPerUnit: input.QuotaPerUnit, PricingSnapshot: input.PricingSnapshot}
		if input.TokenBudget != nil && input.TPMApplicable {
			result.ReservedTokens, result.TPMActiveTokens, result.TPMChargedTokens = *input.TokenBudget, *input.TokenBudget, *input.TokenBudget
		}
		if input.CostBudgetNano != nil {
			result.HeldUSDNano = *input.CostBudgetNano
		}
		account, err := getChannelDailyBudget(tx, input.ChannelID, state.BudgetDay)
		if err != nil {
			return err
		}
		if result.HeldUSDNano > ChannelLimitMaxValue-account.HeldUSDNano {
			return ErrChannelLimitAccounting
		}
		account.HeldUSDNano += result.HeldUSDNano
		if !result.CostBudgetKnown {
			account.UnknownCosts++
		}
		if err := tx.Save(&account).Error; err != nil {
			return err
		}
		return tx.Create(&result).Error
	})
	return &result, err
}

func admitChannelAttempt(config ChannelLimitConfig, state ChannelLimitState, tokens *int64, applicable bool, cost *int64) error {
	if config.RPM > 0 && state.RPMUsed+state.RPMReserved >= config.RPM {
		return &ChannelLimitError{Reason: "rpm_limited"}
	}
	if config.TPM > 0 && applicable {
		if tokens == nil || state.UnknownTokens {
			return &ChannelLimitError{Reason: "token_budget_unknown"}
		}
		if state.TPMUsed+state.TPMReserved > config.TPM || *tokens > config.TPM-state.TPMUsed-state.TPMReserved {
			return &ChannelLimitError{Reason: "tpm_limited"}
		}
	}
	if config.DailyUSDNano > 0 {
		if !config.BudgetReady || cost == nil {
			return &ChannelLimitError{Reason: "cost_config_missing"}
		}
		if state.UnknownCosts > 0 {
			return &ChannelLimitError{Reason: "accounting_unavailable"}
		}
		if state.BudgetExhausted || state.DailyUsedNano >= config.DailyUSDNano {
			return &ChannelLimitError{Reason: "daily_exhausted", RetryAt: state.ResetAt * 1000}
		}
		if state.DailyHeldNano > config.DailyUSDNano-state.DailyUsedNano || *cost > config.DailyUSDNano-state.DailyUsedNano-state.DailyHeldNano {
			return &ChannelLimitError{Reason: "budget_reserved"}
		}
	}
	return nil
}

// MarkChannelAttemptSent rechecks the current configuration and moves an unused
// reservation across the Beijing date boundary before recording dispatch.
func MarkChannelAttemptSent(channelID int, id string, now time.Time) error {
	var admissionErr error
	err := channelLimitTransaction(channelID, now, func(tx *gorm.DB, config *ChannelLimitConfig) error {
		var attempt ChannelLimitAttempt
		if err := tx.First(&attempt, "id = ? AND channel_id = ?", id, channelID).Error; err != nil {
			return err
		}
		if attempt.SentAtMS != 0 {
			return nil
		}
		if attempt.Released || attempt.CreatedAtMS+channelLimitMinuteMS <= now.UnixMilli() {
			return ErrChannelAttemptState
		}
		oldDay, err := getChannelDailyBudget(tx, channelID, attempt.BudgetDay)
		if err != nil {
			return err
		}
		if err := removeChannelBudgetHold(&oldDay, attempt); err != nil {
			return err
		}
		if err := tx.Save(&oldDay).Error; err != nil {
			return err
		}
		// Exclude this reservation when checking its own permission again.
		if err := tx.Model(&attempt).Update("released", true).Error; err != nil {
			return err
		}
		state, err := readChannelLimitState(tx, *config, now)
		if err != nil {
			return err
		}
		var tokens, cost *int64
		if attempt.TokenBudgetKnown {
			tokens = &attempt.ReservedTokens
		}
		if attempt.CostBudgetKnown {
			cost = &attempt.HeldUSDNano
		}
		if err := admitChannelAttempt(*config, state, tokens, attempt.TPMApplicable, cost); err != nil {
			admissionErr = err
			return nil // Commit the release; this attempt must not be sent.
		}
		account, err := getChannelDailyBudget(tx, channelID, state.BudgetDay)
		if err != nil {
			return err
		}
		if attempt.HeldUSDNano > ChannelLimitMaxValue-account.HeldUSDNano {
			return ErrChannelLimitAccounting
		}
		account.HeldUSDNano += attempt.HeldUSDNano
		if !attempt.CostBudgetKnown {
			account.UnknownCosts++
		}
		if err := tx.Save(&account).Error; err != nil {
			return err
		}
		attempt.BudgetDay, attempt.SentAtMS, attempt.Released = state.BudgetDay, now.UnixMilli(), false
		attempt.ExecutionLeaseMS = now.UnixMilli() + channelLimitMinuteMS
		return tx.Save(&attempt).Error
	})
	if err != nil {
		return err
	}
	return admissionErr
}

func removeChannelBudgetHold(account *ChannelDailyBudget, attempt ChannelLimitAttempt) error {
	if attempt.CostActualKnown {
		return nil
	}
	if account.HeldUSDNano < attempt.HeldUSDNano || !attempt.CostBudgetKnown && account.UnknownCosts <= 0 {
		return ErrChannelLimitAccounting
	}
	account.HeldUSDNano -= attempt.HeldUSDNano
	if !attempt.CostBudgetKnown {
		account.UnknownCosts--
	}
	return nil
}

func ReleaseChannelAttempt(channelID int, id string, now time.Time) error {
	return channelLimitTransaction(channelID, now, func(tx *gorm.DB, _ *ChannelLimitConfig) error {
		var attempt ChannelLimitAttempt
		if err := tx.First(&attempt, "id = ? AND channel_id = ?", id, channelID).Error; err != nil {
			return err
		}
		if attempt.Released {
			return nil
		}
		if attempt.SentAtMS != 0 {
			return ErrChannelAttemptState
		}
		return releaseChannelAttempt(tx, &attempt)
	})
}

func releaseChannelAttempt(tx *gorm.DB, attempt *ChannelLimitAttempt) error {
	account, err := getChannelDailyBudget(tx, attempt.ChannelID, attempt.BudgetDay)
	if err != nil {
		return err
	}
	if err := removeChannelBudgetHold(&account, *attempt); err != nil {
		return err
	}
	if err := tx.Save(&account).Error; err != nil {
		return err
	}
	attempt.Released = true
	attempt.TPMActiveTokens = 0
	return tx.Save(attempt).Error
}

func SettleChannelAttempt(input ChannelAttemptSettlement, now time.Time) error {
	if input.Version <= 0 || input.Tokens != nil && (*input.Tokens < 0 || *input.Tokens > ChannelLimitMaxValue) ||
		input.CostUSDNano != nil && (*input.CostUSDNano < 0 || *input.CostUSDNano > ChannelLimitMaxValue) {
		return ErrChannelLimitInvalid
	}
	return channelLimitTransaction(input.ChannelID, now, func(tx *gorm.DB, config *ChannelLimitConfig) error {
		var attempt ChannelLimitAttempt
		if err := tx.First(&attempt, "id = ? AND channel_id = ?", input.ID, input.ChannelID).Error; err != nil {
			return err
		}
		if attempt.Released || attempt.SentAtMS == 0 {
			return ErrChannelAttemptState
		}
		if input.Version <= attempt.ResultVersion {
			return nil
		}
		previous := attempt
		if input.CostUSDNano != nil {
			account, err := getChannelDailyBudget(tx, input.ChannelID, attempt.BudgetDay)
			if err != nil {
				return err
			}
			if err := removeChannelBudgetHold(&account, attempt); err != nil {
				return err
			}
			if account.UsedUSDNano < attempt.ActualUSDNano || *input.CostUSDNano > ChannelLimitMaxValue-(account.UsedUSDNano-attempt.ActualUSDNano) {
				return ErrChannelLimitAccounting
			}
			account.UsedUSDNano = account.UsedUSDNano - attempt.ActualUSDNano + *input.CostUSDNano
			wasExhausted := account.Exhausted
			if config.DailyUSDNano > 0 && account.UsedUSDNano >= config.DailyUSDNano {
				account.Exhausted = true
			}
			if err := tx.Save(&account).Error; err != nil {
				return err
			}
			if err := recordChannelDailyTransition(tx, config, account, wasExhausted, now); err != nil {
				return err
			}
			attempt.ActualUSDNano, attempt.HeldUSDNano, attempt.CostActualKnown = *input.CostUSDNano, 0, true
		}
		if attempt.TPMClosedAtMS == 0 && !attempt.Completed {
			tokens := max(attempt.ReservedTokens, attempt.ObservedTokens)
			if input.Tokens != nil {
				tokens = *input.Tokens
			}
			attempt.TPMActiveTokens, attempt.TPMChargedTokens = tokens, tokens
			attempt.TPMExpiresAtMS = now.UnixMilli() + channelLimitMinuteMS
			attempt.TPMEstimated = input.Tokens == nil
		} else if input.Tokens != nil {
			if attempt.TPMExpiresAtMS <= now.UnixMilli() {
				attempt.TPMActiveTokens = 0
			}
			difference := *input.Tokens - attempt.TPMChargedTokens
			if difference > 0 {
				if attempt.TPMActiveTokens == 0 && attempt.TPMExpiresAtMS <= now.UnixMilli() {
					attempt.TPMExpiresAtMS = now.UnixMilli() + channelLimitMinuteMS
				}
				attempt.TPMActiveTokens += difference
				attempt.TPMChargedTokens += difference
			} else {
				reduction := min(-difference, attempt.TPMActiveTokens)
				attempt.TPMActiveTokens -= reduction
				attempt.TPMChargedTokens -= reduction
			}
			attempt.TPMEstimated = false
		}
		if input.Tokens != nil {
			attempt.ActualTokens, attempt.TokenActualKnown = *input.Tokens, true
		}
		attempt.Completed, attempt.ResultVersion = true, input.Version
		if err := tx.Save(&attempt).Error; err != nil {
			return err
		}
		if input.Tokens != nil && (previous.TPMEstimated || previous.ResultVersion > 0) {
			if err := recordChannelLimitEvent(tx, config, "usage_corrected", fmt.Sprintf("%s:%d", attempt.ID, input.Version), attempt.BudgetDay, attempt.ID, input.Version, now,
				map[string]any{"previous_tokens": previous.TPMChargedTokens, "actual_tokens": *input.Tokens, "previous_estimated": previous.TPMEstimated, "active_tokens": attempt.TPMActiveTokens}); err != nil {
				return err
			}
		}
		if input.CostUSDNano != nil && (previous.Completed || previous.TPMClosedAtMS > 0) {
			if err := recordChannelLimitEvent(tx, config, "cost_corrected", fmt.Sprintf("%s:%d", attempt.ID, input.Version), attempt.BudgetDay, attempt.ID, input.Version, now,
				map[string]any{"previous_cost_known": previous.CostActualKnown, "previous_cost_usd": FormatChannelLimitUSD(previous.ActualUSDNano), "actual_cost_usd": FormatChannelLimitUSD(*input.CostUSDNano), "cost_side": "procurement"}); err != nil {
				return err
			}
		}
		return nil
	})
}

// Call only after losing trustworthy execution evidence, not simply because a
// healthy long-running request passed its ordinary minute window.
func MarkChannelAttemptUnknown(channelID int, id string, observedTokens int64, now time.Time, observedCostNano ...int64) error {
	if observedTokens < 0 || observedTokens > ChannelLimitMaxValue {
		return ErrChannelLimitInvalid
	}
	var observedCost int64
	if len(observedCostNano) > 0 {
		observedCost = observedCostNano[0]
		if observedCost < 0 || observedCost > ChannelLimitMaxValue {
			return ErrChannelLimitInvalid
		}
	}
	return channelLimitTransaction(channelID, now, func(tx *gorm.DB, config *ChannelLimitConfig) error {
		var attempt ChannelLimitAttempt
		if err := tx.First(&attempt, "id = ? AND channel_id = ?", id, channelID).Error; err != nil {
			return err
		}
		if attempt.Released || attempt.SentAtMS == 0 {
			return ErrChannelAttemptState
		}
		if attempt.Completed {
			return nil
		}
		attempt.ObservedTokens = max(attempt.ObservedTokens, observedTokens)
		if attempt.TPMClosedAtMS == 0 {
			attempt.TPMActiveTokens = max(attempt.TPMActiveTokens, attempt.ObservedTokens)
			attempt.TPMChargedTokens = max(attempt.TPMChargedTokens, attempt.ObservedTokens)
		} else if observedTokens > attempt.TPMChargedTokens {
			if attempt.TPMExpiresAtMS <= now.UnixMilli() {
				attempt.TPMActiveTokens = 0
				attempt.TPMExpiresAtMS = now.UnixMilli() + channelLimitMinuteMS
			}
			attempt.TPMActiveTokens += observedTokens - attempt.TPMChargedTokens
			attempt.TPMChargedTokens = observedTokens
		}
		if observedCost > attempt.ObservedCostNano {
			attempt.ObservedCostNano = observedCost
			if observedCost > attempt.HeldUSDNano {
				account, err := getChannelDailyBudget(tx, channelID, attempt.BudgetDay)
				if err != nil {
					return err
				}
				difference := observedCost - attempt.HeldUSDNano
				if difference > ChannelLimitMaxValue-account.HeldUSDNano {
					return ErrChannelLimitAccounting
				}
				account.HeldUSDNano += difference
				attempt.HeldUSDNano = observedCost
				if err := tx.Save(&account).Error; err != nil {
					return err
				}
			}
			if err := recordChannelLimitEvent(tx, config, "cost_corrected", fmt.Sprintf("pending:%s:%d", id, observedCost), attempt.BudgetDay, id, 0, now,
				map[string]any{"pending": true, "cost_side": "procurement", "observed_lower_bound_usd": FormatChannelLimitUSD(observedCost), "reserved_usd": FormatChannelLimitUSD(attempt.HeldUSDNano)}); err != nil {
				return err
			}
		}
		if attempt.ReconcileStartedMS == 0 {
			attempt.ReconcileStartedMS = now.UnixMilli()
			attempt.ReconcileUntilMS = now.UnixMilli() + attempt.ReconcileSeconds*1000
		}
		return tx.Save(&attempt).Error
	})
}

// ConfirmChannelAttemptRunning requires new, trustworthy execution evidence.
// Repeated retrieval of an unchanged local status is not such evidence.
func ConfirmChannelAttemptRunning(channelID int, id string, observedTokens int64, now time.Time) error {
	if observedTokens < 0 || observedTokens > ChannelLimitMaxValue {
		return ErrChannelLimitInvalid
	}
	return channelLimitTransaction(channelID, now, func(tx *gorm.DB, _ *ChannelLimitConfig) error {
		var attempt ChannelLimitAttempt
		if err := tx.First(&attempt, "id = ? AND channel_id = ?", id, channelID).Error; err != nil {
			return err
		}
		if attempt.Released || attempt.SentAtMS == 0 || attempt.Completed || attempt.TPMClosedAtMS > 0 {
			return ErrChannelAttemptState
		}
		attempt.ObservedTokens = max(attempt.ObservedTokens, observedTokens)
		attempt.TPMActiveTokens = max(attempt.TPMActiveTokens, attempt.ObservedTokens)
		attempt.TPMChargedTokens = max(attempt.TPMChargedTokens, attempt.ObservedTokens)
		attempt.ReconcileStartedMS, attempt.ReconcileUntilMS = 0, 0
		attempt.ExecutionLeaseMS = now.UnixMilli() + channelLimitMinuteMS
		return tx.Save(&attempt).Error
	})
}

// RenewChannelAttemptLease is liveness evidence from the process executing the
// request. Losing this lease starts reconciliation; it does not imply failure.
func RenewChannelAttemptLease(channelID int, id string, now time.Time) error {
	return channelLimitTransaction(channelID, now, func(tx *gorm.DB, _ *ChannelLimitConfig) error {
		return tx.Model(&ChannelLimitAttempt{}).Where("channel_id = ? AND id = ? AND released = ? AND completed = ? AND reconcile_started_ms = ? AND tpm_closed_at_ms = ?", channelID, id, false, false, 0, 0).
			Update("execution_lease_ms", now.UnixMilli()+channelLimitMinuteMS).Error
	})
}

// ReconcileChannelAttempts is safe to run on every instance, including after
// process crashes. Healthy long-running requests renew their execution lease.
func ReconcileChannelAttempts(now time.Time) error {
	day, _, _ := channelBudgetDay(now)
	var rolloverIDs []int
	if err := DB.Model(&ChannelLimitConfig{}).Where("daily_usd_nano > ? AND (budget_day < ? OR budget_day IS NULL)", 0, day).Pluck("channel_id", &rolloverIDs).Error; err != nil {
		return err
	}
	for _, id := range rolloverIDs {
		if err := channelLimitTransaction(id, now, func(*gorm.DB, *ChannelLimitConfig) error { return nil }); err != nil {
			return err
		}
	}
	var ids []int
	err := DB.Model(&ChannelLimitAttempt{}).Distinct("channel_id").Where("released = ? AND ((sent_at_ms = ? AND created_at_ms <= ?) OR (completed = ? AND tpm_closed_at_ms = ? AND ((reconcile_until_ms > ? AND reconcile_until_ms <= ?) OR (reconcile_until_ms = ? AND execution_lease_ms > ? AND execution_lease_ms <= ?))))",
		false, 0, now.UnixMilli()-channelLimitMinuteMS, false, 0, 0, now.UnixMilli(), 0, 0, now.UnixMilli()).Pluck("channel_id", &ids).Error
	if err != nil {
		return err
	}
	for _, channelID := range ids {
		if err := channelLimitTransaction(channelID, now, func(tx *gorm.DB, config *ChannelLimitConfig) error {
			return reconcileChannelAttempts(tx, config, now)
		}); err != nil {
			return err
		}
	}
	return nil
}

func reconcileChannelAttempts(tx *gorm.DB, config *ChannelLimitConfig, now time.Time) error {
	var attempts []ChannelLimitAttempt
	err := tx.Where("channel_id = ? AND released = ? AND ((sent_at_ms = ? AND created_at_ms <= ?) OR (completed = ? AND tpm_closed_at_ms = ? AND ((reconcile_until_ms > ? AND reconcile_until_ms <= ?) OR (reconcile_until_ms = ? AND execution_lease_ms > ? AND execution_lease_ms <= ?))))",
		config.ChannelID, false, 0, now.UnixMilli()-channelLimitMinuteMS, false, 0, 0, now.UnixMilli(), 0, 0, now.UnixMilli()).Find(&attempts).Error
	if err != nil {
		return err
	}
	for i := range attempts {
		attempt := &attempts[i]
		if attempt.SentAtMS == 0 {
			if err := releaseChannelAttempt(tx, attempt); err != nil {
				return err
			}
			continue
		}
		if attempt.ReconcileUntilMS == 0 {
			attempt.ReconcileStartedMS = attempt.ExecutionLeaseMS
			attempt.ReconcileUntilMS = attempt.ExecutionLeaseMS + attempt.ReconcileSeconds*1000
		}
		if attempt.ReconcileUntilMS > now.UnixMilli() {
			if err := tx.Save(attempt).Error; err != nil {
				return err
			}
			continue
		}
		attempt.TPMActiveTokens = max(attempt.ReservedTokens, attempt.ObservedTokens)
		attempt.TPMChargedTokens = attempt.TPMActiveTokens
		attempt.TPMEstimated, attempt.TPMClosedAtMS = true, now.UnixMilli()
		attempt.TPMExpiresAtMS = now.UnixMilli() + channelLimitMinuteMS
		if err := tx.Save(attempt).Error; err != nil {
			return err
		}
		if err := recordChannelLimitEvent(tx, config, "tpm_estimated", attempt.ID, attempt.BudgetDay, attempt.ID, attempt.ResultVersion, now,
			map[string]any{"reserved_tokens": attempt.ReservedTokens, "observed_tokens": attempt.ObservedTokens, "estimated_tokens": attempt.TPMActiveTokens, "expires_at_ms": attempt.TPMExpiresAtMS, "cost_remains_unknown": !attempt.CostActualKnown}); err != nil {
			return err
		}
	}
	return nil
}
