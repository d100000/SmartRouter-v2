package model

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/QuantumNous/new-api/common"
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
	errChannelLimitNotTracked  = errors.New("channel limit tracking not initialized")
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

// Disabled reports that no RPM, TPM or daily budget is configured.
func (s ChannelLimitSettings) Disabled() bool {
	return s.RPM == 0 && s.TPM == 0 && s.DailyUSDNano == 0
}

type ChannelLimitConfig struct {
	sampledAt       time.Time
	databaseClockMS int64
	ChannelID       int `json:"channel_id" gorm:"primaryKey;autoIncrement:false"`
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
	CreatedAtMS        int64    `json:"created_at" gorm:"bigint;index:idx_cl_pending_due,priority:3"`
	SentAtMS           int64    `json:"sent_at" gorm:"bigint;index:idx_cl_attempt_window,priority:2;index:idx_cl_pending_due,priority:1"`
	Released           bool     `json:"-" gorm:"index:idx_cl_pending_due,priority:2"`
	Completed          bool     `json:"completed" gorm:"index:idx_cl_active_due,priority:1"`
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
	TPMClosedAtMS      int64    `json:"tpm_closed_at" gorm:"bigint;index:idx_cl_active_due,priority:2"`
	ReconcileSeconds   int64    `json:"-" gorm:"bigint"`
	ReconcileStartedMS int64    `json:"-" gorm:"bigint"`
	ReconcileUntilMS   int64    `json:"reconcile_until" gorm:"bigint;index:idx_cl_attempt_reconcile,priority:2;index:idx_cl_active_due,priority:3"`
	ExecutionLeaseMS   int64    `json:"-" gorm:"bigint;index;index:idx_cl_active_due,priority:4"`
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
	// FollowUp marks a later physical request of a logical request whose
	// first request was already admitted and sent. It is recorded and counted
	// like any send but never refused for RPM, TPM or daily-budget capacity:
	// the upstream work of the first request has already run.
	FollowUp bool
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
	Config         ChannelLimitConfig `json:"config"`
	RPMUsed        int64              `json:"rpm_used"`
	RPMReserved    int64              `json:"rpm_reserved"`
	RPMRetryAt     int64              `json:"rpm_retry_at_ms,omitempty"`
	TPMUsed        int64              `json:"tpm_used"`
	TPMReserved    int64              `json:"tpm_reserved"`
	DailyUsedNano  int64              `json:"-"`
	DailyHeldNano  int64              `json:"-"`
	DailyRemaining int64              `json:"-"`
	BudgetDay      string             `json:"budget_day"`
	ResetAt        int64              `json:"reset_at"`
	// DailyActiveAt is the Unix second at which a configured daily budget
	// starts to apply. It is zero when the budget already applies.
	DailyActiveAt   int64  `json:"daily_active_at,omitempty"`
	Reason          string `json:"reason"`
	UnknownTokens   bool   `json:"-"`
	UnknownCosts    int64  `json:"-"`
	BudgetExhausted bool   `json:"-"`
}

type ChannelLimitError struct {
	Reason        string
	RetryAt       int64 // Database Unix milliseconds; zero if the retry time is unknown.
	localDeadline time.Time
}

// RetryDeadline is the equivalent local deadline for relative response headers.
// The database and gateway machines need not have the same wall-clock time.
func (e *ChannelLimitError) RetryDeadline() time.Time {
	if e.RetryAt == 0 {
		return time.Time{}
	}
	if !e.localDeadline.IsZero() {
		return e.localDeadline
	}
	return time.UnixMilli(e.RetryAt)
}

func (config ChannelLimitConfig) limitError(reason string, retryAt int64) *ChannelLimitError {
	refusal := &ChannelLimitError{Reason: reason, RetryAt: retryAt}
	if !config.sampledAt.IsZero() && retryAt > 0 {
		refusal.localDeadline = config.sampledAt.Add(time.Duration(retryAt-config.databaseClockMS) * time.Millisecond)
	}
	return refusal
}

func (config ChannelLimitConfig) windowTime(explicit time.Time) time.Time {
	if !explicit.IsZero() {
		return explicit
	}
	if config.sampledAt.IsZero() {
		return time.Now() // no persisted window yet
	}
	return time.UnixMilli(config.databaseClockMS).Add(time.Since(config.sampledAt))
}

// The clock is sampled in the statement following lock acquisition. PostgreSQL
// transaction timestamps would otherwise retain the instant before lock wait.
func channelLimitClockSQL() string {
	switch {
	case common.UsingMainDatabase(common.DatabaseTypeMySQL):
		return "TIMESTAMPDIFF(MICROSECOND, '1970-01-01 00:00:00', UTC_TIMESTAMP(3)) DIV 1000"
	case common.UsingMainDatabase(common.DatabaseTypePostgreSQL):
		return "CAST(FLOOR(EXTRACT(EPOCH FROM clock_timestamp()) * 1000) AS BIGINT)"
	default: // SQLite, including releases without the subsec modifier
		return "(CAST(strftime('%s','now') AS INTEGER) * 1000 + CAST(substr(strftime('%f','now'),4,3) AS INTEGER))"
	}
}

// ChannelLimitDatabaseTime is the time basis shared by observers and dispatchers.
func ChannelLimitDatabaseTime(db *gorm.DB) (time.Time, error) {
	var ms int64
	err := db.Raw("SELECT " + channelLimitClockSQL()).Scan(&ms).Error
	return time.UnixMilli(ms), err
}

func (e *ChannelLimitError) Error() string { return "channel limit: " + e.Reason }

func (e *ChannelLimitError) Is(target error) bool {
	return target == ErrChannelLimitAccounting && e.Reason == "minute_window_warming"
}

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
	db := DB
	if len(transactions) > 0 && transactions[0] != nil {
		db = transactions[0]
	}
	if now.IsZero() {
		var err error
		now, err = ChannelLimitDatabaseTime(db)
		if err != nil {
			return err
		}
	}
	config := ChannelLimitConfig{ChannelID: channelID, Revision: 1, ObservedSince: now.UnixMilli(), HistoryComplete: newChannel,
		ChannelLimitSettings: ChannelLimitSettings{ReconcileSeconds: ChannelLimitDefaultReconcile}}
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&config).Error
}

// The first write takes SQLite's writer lock before any reads; MySQL and
// PostgreSQL serialize the same channel on its configuration row. No local
// mutex, cache, log database or asynchronous projection grants permissions.
func channelLimitTransaction(channelID int, now time.Time, fn func(*gorm.DB, *ChannelLimitConfig, time.Time) error, transactions ...*gorm.DB) error {
	db := DB
	if len(transactions) > 0 {
		db = transactions[0]
	}
	if channelID <= 0 {
		return ErrChannelLimitInvalid
	}
	locked := func(tx *gorm.DB) error {
		update := tx.Model(&ChannelLimitConfig{}).Where("channel_id = ?", channelID).
			UpdateColumn("lock_version", gorm.Expr("lock_version + 1"))
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected == 0 {
			return errChannelLimitNotTracked
		}
		config, err := readChannelLimitConfig(lockForUpdate(tx), channelID, now.IsZero())
		if err != nil {
			return err
		}
		at := config.windowTime(now)
		day, dayStart, _ := channelBudgetDay(at)
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
		return fn(tx, &config, at)
	}
	err := db.Transaction(locked)
	if !errors.Is(err, errChannelLimitNotTracked) {
		return err
	}
	// Keep the start of a legacy channel's observation window even when
	// activating a limit immediately afterwards fails its history check.
	if err := EnsureChannelLimitTracking(channelID, false, now, db); err != nil {
		return err
	}
	return db.Transaction(locked)
}

func UpdateChannelLimits(channelID int, revision int64, settings ChannelLimitSettings, costReady bool, now time.Time, transactions ...*gorm.DB) (ChannelLimitConfig, error) {
	var updated ChannelLimitConfig
	if settings.RPM < 0 || settings.TPM < 0 || settings.DailyUSDNano < 0 ||
		settings.RPM > ChannelLimitMaxValue || settings.TPM > ChannelLimitMaxValue || settings.DailyUSDNano > ChannelLimitMaxValue ||
		settings.ReconcileSeconds <= 0 || settings.ReconcileSeconds > 86400 {
		return updated, ErrChannelLimitInvalid
	}
	err := channelLimitTransaction(channelID, now, func(tx *gorm.DB, config *ChannelLimitConfig, now time.Time) error {
		if config.Revision != revision {
			return ErrChannelLimitRevision
		}
		before := *config
		state, err := readChannelLimitState(tx, *config, now, true, "", nil)
		if err != nil {
			return err
		}
		_, dayStart, _ := channelBudgetDay(now)
		// A day observed only in part is accepted and applies from the next day.
		dailyDeferred := !config.HistoryComplete && config.ObservedSince > dayStart
		if settings.DailyUSDNano > 0 && config.DailyUSDNano == 0 && !dailyDeferred && state.UnknownCosts > 0 {
			return ErrChannelLimitAccounting
		}
		if (settings.RPM > 0 && config.RPM == 0 || settings.TPM > 0 && config.TPM == 0) &&
			!config.HistoryComplete && config.ObservedSince > now.UnixMilli()-channelLimitMinuteMS {
			return config.limitError("minute_window_warming", config.ObservedSince+channelLimitMinuteMS)
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
		if settings.DailyUSDNano > 0 && !dailyDeferred && account.UsedUSDNano >= settings.DailyUSDNano {
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

func readChannelLimitConfig(db *gorm.DB, channelID int, sampleClock bool) (ChannelLimitConfig, error) {
	var row struct {
		ChannelLimitConfig
		ClockMS int64
	}
	columns := "*"
	if sampleClock {
		columns += ", " + channelLimitClockSQL() + " AS clock_ms"
	}
	err := db.Model(&ChannelLimitConfig{}).Select(columns).Take(&row, "channel_id = ?", channelID).Error
	if err == nil && sampleClock {
		row.sampledAt, row.databaseClockMS = time.Now(), row.ClockMS
	}
	return row.ChannelLimitConfig, err
}

func GetChannelLimitConfig(channelID int, transactions ...*gorm.DB) (ChannelLimitConfig, error) {
	db := DB
	if len(transactions) > 0 {
		db = transactions[0]
	}
	config, err := readChannelLimitConfig(db, channelID, true)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ChannelLimitConfig{ChannelID: channelID, Revision: 1, ChannelLimitSettings: ChannelLimitSettings{ReconcileSeconds: ChannelLimitDefaultReconcile}}, nil
	}
	return config, err
}

func GetChannelLimitState(channelID int, now time.Time, transactions ...*gorm.DB) (ChannelLimitState, error) {
	db := DB
	if len(transactions) > 0 {
		db = transactions[0]
	}
	config, err := GetChannelLimitConfig(channelID, db)
	if err != nil {
		return ChannelLimitState{}, err
	}
	return readChannelLimitState(db, config, config.windowTime(now), true, "", nil)
}

func (s *ChannelLimitState) rpmRefusal() error {
	if s.Config.RPM > 0 && s.RPMUsed+s.RPMReserved >= s.Config.RPM {
		return s.Config.limitError("rpm_limited", s.RPMRetryAt)
	}
	return nil
}

func (s *ChannelLimitState) readRPM(tx *gorm.DB, now time.Time, exclude string) error {
	query := tx.Model(&ChannelLimitAttempt{}).Where("channel_id = ? AND released = ?", s.Config.ChannelID, false)
	if exclude != "" {
		query = query.Where("id <> ?", exclude)
	}
	// Separate index ranges keep a hot channel's settled history out of the
	// count. Both subqueries share a single statement snapshot.
	cutoff := now.UnixMilli() - channelLimitMinuteMS
	query = query.Session(&gorm.Session{})
	sent := query.Where("sent_at_ms > ?", cutoff).Session(&gorm.Session{})
	pending := query.Where("sent_at_ms = 0 AND created_at_ms > ?", cutoff).Session(&gorm.Session{})
	var counts struct{ Used, Reserved int64 }
	if err := tx.Raw("SELECT (?) AS used, (?) AS reserved", sent.Select("COUNT(*)"), pending.Select("COUNT(*)")).Scan(&counts).Error; err != nil {
		return err
	}
	s.RPMUsed, s.RPMReserved = counts.Used, counts.Reserved
	if s.Config.RPM <= 0 || counts.Used+counts.Reserved < s.Config.RPM {
		return nil
	}
	// A lowered limit can require more than one slot to expire. Looking up
	// either end of the same ordered window gives the same release instant.
	// The smaller offset avoids walking the longer side of an overloaded window.
	offset := counts.Used + counts.Reserved - s.Config.RPM
	order := " ASC"
	if offset > s.Config.RPM-1 {
		offset, order = s.Config.RPM-1, " DESC"
	}
	if offset <= math.MaxInt {
		var start int64
		expiry := sent.Select("sent_at_ms AS started").Order("sent_at_ms" + order)
		if counts.Reserved > 0 {
			window := tx.Raw("? UNION ALL ?", sent.Select("sent_at_ms AS started"), pending.Select("created_at_ms AS started"))
			expiry = tx.Table("(?) AS rpm_window", window).Select("started").Order("started" + order)
		}
		row := expiry.Offset(int(offset)).Limit(1).Scan(&start)
		if row.Error != nil {
			return row.Error
		}
		if row.RowsAffected > 0 {
			s.RPMRetryAt = start + channelLimitMinuteMS
		}
	}
	return nil
}

// Management needs all observed quantities, even with limits disabled. A
// dispatch reads only the quantities required by its enabled limits. The
// confirming send excludes its own outstanding reservation.
func readChannelLimitState(tx *gorm.DB, config ChannelLimitConfig, now time.Time, all bool, exclude string, rpmUpperBound *int64) (ChannelLimitState, error) {
	day, dayStart, reset := channelBudgetDay(now)
	s := ChannelLimitState{Config: config, BudgetDay: day, ResetAt: reset}
	// A daily budget set during a partly observed day applies from the next one.
	dailyActive := config.DailyUSDNano > 0
	if dailyActive && !config.HistoryComplete && config.ObservedSince > dayStart {
		dailyActive, s.DailyActiveAt = false, reset
	}
	if rpmUpperBound != nil {
		s.RPMUsed = *rpmUpperBound
	} else if all || config.RPM > 0 {
		if err := s.readRPM(tx, now, exclude); err != nil {
			return s, err
		}
	}
	if all || config.TPM > 0 {
		var attempts []ChannelLimitAttempt
		query := tx.Select("tpm_expires_at_ms", "tpm_active_tokens", "token_budget_known", "token_actual_known").
			Where("channel_id = ? AND released = ? AND tpm_applicable = ?", config.ChannelID, false, true).
			Where("(tpm_expires_at_ms > ? OR tpm_expires_at_ms = 0) AND (sent_at_ms > 0 OR created_at_ms > ?)", now.UnixMilli(), now.UnixMilli()-channelLimitMinuteMS)
		if exclude != "" {
			query = query.Where("id <> ?", exclude)
		}
		if err := query.Find(&attempts).Error; err != nil {
			return s, err
		}
		for _, attempt := range attempts {
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
	}
	if all || dailyActive {
		var account ChannelDailyBudget
		err := tx.First(&account, "id = ?", fmt.Sprintf("%d:%s", config.ChannelID, day)).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return s, err
		}
		s.DailyUsedNano, s.DailyHeldNano = account.UsedUSDNano, account.HeldUSDNano
		s.UnknownCosts, s.BudgetExhausted = account.UnknownCosts, account.Exhausted
		if dailyActive {
			s.DailyRemaining = max(0, config.DailyUSDNano-account.UsedUSDNano-account.HeldUSDNano)
		}
	}
	switch {
	case dailyActive && !config.BudgetReady:
		s.Reason = "cost_config_missing"
	case dailyActive && s.UnknownCosts > 0 || config.TPM > 0 && s.UnknownTokens:
		s.Reason = "accounting_unavailable"
	case dailyActive && (s.BudgetExhausted || s.DailyUsedNano >= config.DailyUSDNano):
		s.Reason = "daily_exhausted"
	case dailyActive && s.DailyRemaining == 0:
		s.Reason = "budget_reserved"
	case config.RPM > 0 && s.RPMUsed+s.RPMReserved >= config.RPM:
		s.Reason = "rpm_limited"
	case config.TPM > 0 && s.TPMUsed+s.TPMReserved >= config.TPM:
		s.Reason = "tpm_limited"
	case s.DailyActiveAt > 0:
		// Informational: the channel is usable, its daily budget is not active yet.
		s.Reason = "daily_pending"
	}
	return s, nil
}

func ReserveChannelAttempt(input ChannelAttemptReservation, now time.Time) (*ChannelLimitAttempt, error) {
	return recordChannelAttempt(input, now, false)
}

// DispatchChannelAttempt admits and records one immediate send in a single
// transaction. The outgoing request is ready before the channel lock is taken.
func DispatchChannelAttempt(input ChannelAttemptReservation) error {
	_, err := recordChannelAttempt(input, time.Time{}, true)
	return err
}

func recordChannelAttempt(input ChannelAttemptReservation, now time.Time, dispatch bool) (*ChannelLimitAttempt, error) {
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
	var nextWindow *committedRPMWindow
	db := DB
	err := channelLimitTransaction(input.ChannelID, now, func(tx *gorm.DB, config *ChannelLimitConfig, now time.Time) error {
		if config.DailyUSDNano > 0 || config.TPM > 0 {
			if err := reconcileChannelAttempts(tx, config, now, nil); err != nil {
				return err
			}
		}
		var proof *committedRPMWindow
		if dispatch && config.RPM > 0 {
			proof = committedRPMWindows[config.ChannelID%len(committedRPMWindows)].Load()
		}
		upper, proven := proof.upperBound(db, config, now.UnixMilli())
		existing := tx.Model(&ChannelLimitAttempt{}).Where("id = ?", input.ID)
		if proven {
			existing = existing.Or("id = ? AND channel_id = ? AND sent_at_ms = ?", proof.attemptID, input.ChannelID, proof.atMS)
		}
		var found []string
		if err := existing.Pluck("id", &found).Error; err != nil {
			return err
		}
		if slices.Contains(found, input.ID) {
			return ErrChannelAttemptDuplicate
		}
		var bound *int64
		if proven && slices.Contains(found, proof.attemptID) {
			bound = &upper
		}
		state, err := readChannelLimitState(tx, *config, now, false, "", bound)
		if err != nil {
			return err
		}
		if !input.FollowUp {
			if err := admitChannelAttempt(*config, state, input.TokenBudget, input.TPMApplicable, input.CostBudgetNano); err != nil {
				return err
			}
		}
		result = ChannelLimitAttempt{ID: input.ID, ChannelID: input.ChannelID, BudgetDay: state.BudgetDay,
			CreatedAtMS: now.UnixMilli(), TPMApplicable: input.TPMApplicable,
			TokenBudgetKnown: input.TokenBudget != nil, CostBudgetKnown: input.CostBudgetNano != nil,
			ReconcileSeconds: config.ReconcileSeconds, CostRatio: input.CostRatio, QuotaPerUnit: input.QuotaPerUnit, PricingSnapshot: input.PricingSnapshot}
		if dispatch {
			result.SentAtMS = now.UnixMilli()
			result.ExecutionLeaseMS = result.SentAtMS + channelLimitMinuteMS
			if config.RPM > 0 {
				nextWindow = &committedRPMWindow{
					db: db, channelID: config.ChannelID, revision: config.Revision, observedSince: config.ObservedSince,
					rpm: config.RPM, atMS: result.SentAtMS, lockVersion: config.LockVersion,
					occupied: state.RPMUsed + state.RPMReserved + 1, attemptID: result.ID,
				}
			}
		}
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
	}, db)
	if err == nil && nextWindow != nil {
		committedRPMWindows[input.ChannelID%len(committedRPMWindows)].Store(nextWindow)
	}
	return &result, err
}

func admitChannelAttempt(config ChannelLimitConfig, state ChannelLimitState, tokens *int64, applicable bool, cost *int64) error {
	if err := state.rpmRefusal(); err != nil {
		return err
	}
	if config.TPM > 0 && applicable {
		if tokens == nil || state.UnknownTokens {
			return &ChannelLimitError{Reason: "token_budget_unknown"}
		}
		if state.TPMUsed+state.TPMReserved > config.TPM || *tokens > config.TPM-state.TPMUsed-state.TPMReserved {
			return &ChannelLimitError{Reason: "tpm_limited"}
		}
	}
	if config.DailyUSDNano > 0 && state.DailyActiveAt == 0 {
		if !config.BudgetReady || cost == nil {
			return &ChannelLimitError{Reason: "cost_config_missing"}
		}
		if state.UnknownCosts > 0 {
			return &ChannelLimitError{Reason: "accounting_unavailable"}
		}
		if state.BudgetExhausted || state.DailyUsedNano >= config.DailyUSDNano {
			return config.limitError("daily_exhausted", state.ResetAt*1000)
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
	err := channelLimitTransaction(channelID, now, func(tx *gorm.DB, config *ChannelLimitConfig, now time.Time) error {
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
		state, err := readChannelLimitState(tx, *config, now, false, id, nil)
		if err != nil {
			return err
		}
		sameDay := state.BudgetDay == attempt.BudgetDay
		if sameDay && config.DailyUSDNano > 0 && state.DailyActiveAt == 0 {
			if state.DailyHeldNano < attempt.HeldUSDNano || !attempt.CostBudgetKnown && state.UnknownCosts <= 0 {
				return ErrChannelLimitAccounting
			}
			state.DailyHeldNano -= attempt.HeldUSDNano
			if !attempt.CostBudgetKnown {
				state.UnknownCosts--
			}
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
			return releaseChannelAttempt(tx, &attempt)
		}
		if !sameDay {
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
		}
		return tx.Model(&attempt).Updates(map[string]any{
			"budget_day": state.BudgetDay, "sent_at_ms": now.UnixMilli(), "execution_lease_ms": now.UnixMilli() + channelLimitMinuteMS,
		}).Error
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

// countUnknownChannelCost makes a sent attempt whose actual cost is not known
// count in its day's unknown_costs, whatever its dispatch-time estimate was.
// Its hold stays as a lower bound. The attempt then reads as one whose cost
// budget is unknown, so a later settlement with a cost removes it from the
// count exactly once through removeChannelBudgetHold.
func countUnknownChannelCost(tx *gorm.DB, attempt *ChannelLimitAttempt) error {
	if attempt.CostActualKnown || !attempt.CostBudgetKnown {
		return nil
	}
	account, err := getChannelDailyBudget(tx, attempt.ChannelID, attempt.BudgetDay)
	if err != nil {
		return err
	}
	account.UnknownCosts++
	if err := tx.Save(&account).Error; err != nil {
		return err
	}
	attempt.CostBudgetKnown = false
	return nil
}

func ReleaseChannelAttempt(channelID int, id string, now time.Time) error {
	return channelLimitTransaction(channelID, now, func(tx *gorm.DB, _ *ChannelLimitConfig, now time.Time) error {
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
	return channelLimitTransaction(input.ChannelID, now, func(tx *gorm.DB, config *ChannelLimitConfig, now time.Time) error {
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
		} else if err := countUnknownChannelCost(tx, &attempt); err != nil {
			// A request completed without a computable cost: it ran, so its day
			// must not read as complete.
			return err
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
		attempt.ExecutionLeaseMS = 0
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
	return channelLimitTransaction(channelID, now, func(tx *gorm.DB, config *ChannelLimitConfig, now time.Time) error {
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
		if err := countUnknownChannelCost(tx, &attempt); err != nil {
			return err
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
	return channelLimitTransaction(channelID, now, func(tx *gorm.DB, _ *ChannelLimitConfig, now time.Time) error {
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
	return RenewChannelAttemptLeases(channelID, []string{id}, now)
}

// RenewChannelAttemptLeases serializes with reconciliation on the channel row.
// Batching leaves completed attempts and attempts already under review alone.
func RenewChannelAttemptLeases(channelID int, ids []string, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	return channelLimitTransaction(channelID, now, func(tx *gorm.DB, _ *ChannelLimitConfig, now time.Time) error {
		return tx.Model(&ChannelLimitAttempt{}).Where("channel_id = ? AND id IN ? AND sent_at_ms > 0 AND released = ? AND completed = ? AND reconcile_started_ms = 0 AND tpm_closed_at_ms = 0", channelID, ids, false, false).
			Update("execution_lease_ms", now.UnixMilli()+channelLimitMinuteMS).Error
	})
}

// ReconcileChannelAttempts is safe to run on every instance, including after
// process crashes. Healthy long-running requests renew their execution lease.
// A failing channel or record is logged and backed off; it does not stop the
// pass for other channels.
func ReconcileChannelAttempts(now time.Time) error {
	if now.IsZero() {
		var err error
		now, err = ChannelLimitDatabaseTime(DB)
		if err != nil {
			return err
		}
	}
	day, _, _ := channelBudgetDay(now)
	var rolloverIDs []int
	if err := DB.Model(&ChannelLimitConfig{}).Where("daily_usd_nano > ? AND (budget_day < ? OR budget_day IS NULL)", 0, day).Pluck("channel_id", &rolloverIDs).Error; err != nil {
		return err
	}
	for _, id := range rolloverIDs {
		if err := channelLimitTransaction(id, now, func(*gorm.DB, *ChannelLimitConfig, time.Time) error { return nil }); err != nil {
			common.SysError(common.LogText("channel limit budget rollover failed: channel #%d: %s", id, err.Error()))
		}
	}
	// Each query seeks to its own due range. A DISTINCT over an OR of all
	// deadlines instead walks the channel index including settled history.
	minuteAgo := now.UnixMilli() - channelLimitMinuteMS
	due := []struct {
		filter, order string
		args          []any
	}{
		{"sent_at_ms = 0 AND created_at_ms <= ?", "created_at_ms", []any{minuteAgo}},
		{"completed = ? AND tpm_closed_at_ms = 0 AND reconcile_until_ms > 0 AND reconcile_until_ms <= ?", "reconcile_until_ms", []any{false, now.UnixMilli()}},
		{"completed = ? AND tpm_closed_at_ms = 0 AND reconcile_until_ms = 0 AND execution_lease_ms > 0 AND execution_lease_ms <= ?", "execution_lease_ms", []any{false, now.UnixMilli()}},
	}
	excluded := channelReconcileBackoff.excluded()
	byChannel := make(map[int][]string)
	for _, deadline := range due {
		var attempts []ChannelLimitAttempt
		query := DB.Select("id", "channel_id").Where("released = ?", false).Where(deadline.filter, deadline.args...)
		if len(excluded) > 0 {
			query = query.Where("id NOT IN ?", excluded)
		}
		if err := query.Order(deadline.order).Limit(128).Find(&attempts).Error; err != nil {
			return err
		}
		for _, attempt := range attempts {
			byChannel[attempt.ChannelID] = append(byChannel[attempt.ChannelID], attempt.ID)
		}
	}
	reconcile := func(channelID int, ids []string) error {
		return channelLimitTransaction(channelID, now, func(tx *gorm.DB, config *ChannelLimitConfig, now time.Time) error {
			return reconcileChannelAttempts(tx, config, now, ids)
		})
	}
	for channelID, ids := range byChannel {
		if err := reconcile(channelID, ids); err == nil {
			channelReconcileBackoff.succeed(ids)
			continue
		}
		// Retry one by one so only the failing records back off.
		var failed []string
		var failure error
		for _, id := range ids {
			if err := reconcile(channelID, []string{id}); err != nil {
				channelReconcileBackoff.fail(id)
				failed, failure = append(failed, id), err
				continue
			}
			channelReconcileBackoff.succeed([]string{id})
		}
		if failure != nil {
			common.SysError(common.LogText("channel limit reconciliation failed: channel #%d, attempts %s: %s", channelID, strings.Join(failed, ","), failure.Error()))
		}
	}
	return nil
}

const (
	channelReconcileBackoffCap      = 4096
	channelReconcileExcludedMax     = 1024
	channelReconcileBackoffBase     = 5 * time.Second
	channelReconcileBackoffMaxDelay = 10 * time.Minute
)

type channelReconcileFailure struct {
	failures int
	retryAt  time.Time
}

// channelReconcileBackoff is process-local. Another instance may still try
// a backed-off record; each one backs off independently.
var channelReconcileBackoff = &reconcileBackoff{entries: make(map[string]channelReconcileFailure)}

type reconcileBackoff struct {
	sync.Mutex
	entries map[string]channelReconcileFailure
}

// excluded lists records still waiting for their next retry. Entries that
// have not been seen again long after their retry time are forgotten.
func (b *reconcileBackoff) excluded() []string {
	now := time.Now()
	b.Lock()
	defer b.Unlock()
	var ids []string
	for id, entry := range b.entries {
		if now.Sub(entry.retryAt) > time.Hour {
			delete(b.entries, id)
			continue
		}
		if entry.retryAt.After(now) && len(ids) < channelReconcileExcludedMax {
			ids = append(ids, id)
		}
	}
	return ids
}

func (b *reconcileBackoff) fail(id string) {
	now := time.Now()
	b.Lock()
	defer b.Unlock()
	entry, found := b.entries[id]
	if !found && len(b.entries) >= channelReconcileBackoffCap {
		return
	}
	entry.failures++
	delay := channelReconcileBackoffMaxDelay
	if entry.failures < 8 {
		delay = min(channelReconcileBackoffMaxDelay, channelReconcileBackoffBase<<(entry.failures-1))
	}
	entry.retryAt = now.Add(delay)
	b.entries[id] = entry
}

func (b *reconcileBackoff) succeed(ids []string) {
	b.Lock()
	defer b.Unlock()
	for _, id := range ids {
		delete(b.entries, id)
	}
}

const (
	channelLimitAttemptCleanupInterval = 10 * time.Minute
	channelLimitAttemptCleanupBatch    = 500
	channelLimitAttemptCleanupMaxRows  = 100_000
)

var channelLimitAttemptCleanupOnce sync.Once

// StartChannelLimitAttemptCleanup prunes ledger rows. Reservations that were
// never sent carry no cost and are always removed after a day. Records of sent
// requests are the per-request procurement cost evidence: they are kept forever
// unless CHANNEL_LIMIT_ATTEMPT_RETENTION_DAYS is set to a positive number.
func StartChannelLimitAttemptCleanup() {
	retentionDays := common.GetEnvOrDefault("CHANNEL_LIMIT_ATTEMPT_RETENTION_DAYS", 0)
	channelLimitAttemptCleanupOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(channelLimitAttemptCleanupInterval)
			defer ticker.Stop()
			for range ticker.C {
				if _, err := CleanupChannelLimitAttempts(time.Time{}, retentionDays); err != nil {
					common.SysError(common.LogText("channel limit attempt cleanup failed: %s", err.Error()))
				}
			}
		}()
	})
}

// CleanupChannelLimitAttempts deletes, in small batches, released reservations
// that were never sent (older than a day). Only with a positive retention does
// it also delete completed sends whose TPM window and budget day are past the
// retention window and whose procurement cost is settled or has no cost rule;
// the daily cost totals in channel_daily_budgets are never deleted. Sent
// attempts without a result are always kept: tasks can settle days later. Every
// statement repeats the eligibility filter, so instances can run concurrently.
func CleanupChannelLimitAttempts(now time.Time, retentionDays int) (int64, error) {
	if now.IsZero() {
		var err error
		now, err = ChannelLimitDatabaseTime(DB)
		if err != nil {
			return 0, err
		}
	}
	releasedCutoffMS := now.AddDate(0, 0, -1).UnixMilli()
	eligible := []func(*gorm.DB) *gorm.DB{
		func(db *gorm.DB) *gorm.DB {
			return db.Where("sent_at_ms = ? AND released = ? AND created_at_ms < ?", 0, true, releasedCutoffMS)
		},
	}
	if retentionDays > 0 {
		cutoff := now.AddDate(0, 0, -retentionDays)
		cutoffDay, _, _ := channelBudgetDay(cutoff)
		cutoffMS := cutoff.UnixMilli()
		eligible = append(eligible, func(db *gorm.DB) *gorm.DB {
			return db.Where("tpm_expires_at_ms > ? AND tpm_expires_at_ms < ? AND completed = ? AND budget_day < ? AND (cost_actual_known = ? OR cost_ratio IS NULL)",
				0, cutoffMS, true, cutoffDay, true)
		})
	}
	var deleted int64
	for _, filter := range eligible {
		for deleted < channelLimitAttemptCleanupMaxRows {
			var ids []string
			if err := filter(DB.Model(&ChannelLimitAttempt{})).Limit(channelLimitAttemptCleanupBatch).Pluck("id", &ids).Error; err != nil {
				return deleted, err
			}
			if len(ids) == 0 {
				break
			}
			result := filter(DB.Where("id IN ?", ids)).Delete(&ChannelLimitAttempt{})
			if result.Error != nil {
				return deleted, result.Error
			}
			deleted += result.RowsAffected
			if len(ids) < channelLimitAttemptCleanupBatch {
				break
			}
		}
	}
	return deleted, nil
}

func reconcileChannelAttempts(tx *gorm.DB, config *ChannelLimitConfig, now time.Time, ids []string) error {
	var attempts []ChannelLimitAttempt
	query := tx.Where("channel_id = ? AND released = ? AND ((sent_at_ms = ? AND created_at_ms <= ?) OR (completed = ? AND tpm_closed_at_ms = ? AND ((reconcile_until_ms > ? AND reconcile_until_ms <= ?) OR (reconcile_until_ms = ? AND execution_lease_ms > ? AND execution_lease_ms <= ?))))",
		config.ChannelID, false, 0, now.UnixMilli()-channelLimitMinuteMS, false, 0, 0, now.UnixMilli(), 0, 0, now.UnixMilli())
	if len(ids) != 0 {
		query = query.Where("id IN ?", ids)
	} else if excluded := channelReconcileBackoff.excluded(); len(excluded) > 0 {
		// Leave records that keep failing to the background pass.
		query = query.Where("id NOT IN ?", excluded)
	}
	if err := query.Find(&attempts).Error; err != nil {
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
			// The executing process stopped renewing: it may have crashed before
			// settlement, so the cost of this request is now unknown.
			if err := countUnknownChannelCost(tx, attempt); err != nil {
				return err
			}
			attempt.ReconcileStartedMS = attempt.ExecutionLeaseMS
			attempt.ReconcileUntilMS = attempt.ExecutionLeaseMS + attempt.ReconcileSeconds*1000
		}
		attempt.ExecutionLeaseMS = 0
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
