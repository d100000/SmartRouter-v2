package model

import (
	"errors"
	"strings"
	"time"
	_ "time/tzdata" // Availability must work in containers without system zoneinfo.

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var ErrChannelOutsideSchedule = errors.New("Outside availability hours")

// ChannelAvailability is a wall-clock schedule in an explicit IANA time zone.
// Weekdays use ISO numbering (Monday=1, Sunday=7). Exceptions replace the whole
// local date, including overnight windows carried over from the previous day.
type ChannelAvailability struct {
	Enabled    bool                           `json:"enabled"`
	Timezone   string                         `json:"timezone"`
	Mode       string                         `json:"mode"`
	Rules      []ChannelAvailabilityRule      `json:"rules"`
	Exceptions []ChannelAvailabilityException `json:"exceptions,omitempty"`
}

type ChannelAvailabilityWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type ChannelAvailabilityRule struct {
	ChannelAvailabilityWindow
	Weekdays []int `json:"weekdays,omitempty"`
}

type ChannelAvailabilityException struct {
	Date    string                      `json:"date"`
	Windows []ChannelAvailabilityWindow `json:"windows"`
}

type availabilityWindow struct{ start, end int }

type channelAvailabilityPlan struct {
	raw        string
	location   *time.Location
	weekly     [7][]availabilityWindow
	exceptions map[string][]availabilityWindow
	err        error
}

type ChannelAvailabilityState struct {
	Enabled      bool   `json:"enabled"`
	Open         bool   `json:"open"`
	Timezone     string `json:"timezone,omitempty"`
	Invalid      bool   `json:"invalid,omitempty"`
	EvaluatedAt  int64  `json:"evaluated_at"`
	NextChangeAt int64  `json:"next_change_at,omitempty"`
	// The preview is bounded; no transition here does not mean never available.
	PreviewDays int `json:"preview_days"`
}

func availabilityMinute(value string, allowMidnightEnd bool) (int, error) {
	if value == "24:00" && allowMidnightEnd {
		return 1440, nil
	}
	parsed, err := time.Parse("15:04", value)
	if err != nil || len(value) != 5 {
		return 0, errors.New("Availability times must use HH:MM; only an end time may be 24:00")
	}
	return parsed.Hour()*60 + parsed.Minute(), nil
}

func compileChannelAvailability(raw string) (*channelAvailabilityPlan, error) {
	plan := &channelAvailabilityPlan{raw: raw}
	if strings.TrimSpace(raw) == "" {
		return plan, nil
	}
	if len(raw) > 32768 || strings.TrimSpace(raw)[0] != '{' {
		return nil, errors.New("Invalid channel availability configuration")
	}
	var config ChannelAvailability
	if err := common.UnmarshalJsonStr(raw, &config); err != nil {
		return nil, errors.New("Invalid channel availability configuration")
	}
	if !config.Enabled {
		return plan, nil
	}
	if config.Timezone == "" || config.Timezone == "Local" {
		return nil, errors.New("Select a valid IANA time zone")
	}
	location, err := time.LoadLocation(config.Timezone)
	if err != nil {
		return nil, errors.New("Select a valid IANA time zone")
	}
	if config.Mode != "daily" && config.Mode != "weekly" {
		return nil, errors.New("Availability mode must be daily or weekly")
	}
	if len(config.Rules) == 0 || len(config.Rules) > 32 || len(config.Exceptions) > 100 {
		return nil, errors.New("Availability requires 1 to 32 rules and at most 100 date exceptions")
	}
	plan.location = location
	plan.exceptions = make(map[string][]availabilityWindow, len(config.Exceptions))
	for _, rule := range config.Rules {
		start, err := availabilityMinute(rule.Start, false)
		if err != nil {
			return nil, err
		}
		end, err := availabilityMinute(rule.End, true)
		if err != nil {
			return nil, err
		}
		if start == end {
			return nil, errors.New("Availability start and end must differ; use 00:00 to 24:00 for all day")
		}
		days := rule.Weekdays
		if config.Mode == "daily" {
			days = []int{1, 2, 3, 4, 5, 6, 7}
		}
		if len(days) == 0 || len(days) > 7 {
			return nil, errors.New("Select at least one weekday for each availability rule")
		}
		var seen [7]bool
		for _, day := range days {
			if day < 1 || day > 7 || seen[day-1] {
				return nil, errors.New("Availability weekdays must be unique values from 1 to 7")
			}
			seen[day-1] = true
			plan.weekly[day-1] = append(plan.weekly[day-1], availabilityWindow{start, end})
		}
	}
	for _, exception := range config.Exceptions {
		date, err := time.Parse("2006-01-02", exception.Date)
		if err != nil || date.Format("2006-01-02") != exception.Date || len(exception.Windows) > 8 {
			return nil, errors.New("Date exceptions require a valid date and at most 8 time windows")
		}
		if _, exists := plan.exceptions[exception.Date]; exists {
			return nil, errors.New("Availability date exceptions must have unique dates")
		}
		windows := make([]availabilityWindow, 0, len(exception.Windows))
		for _, window := range exception.Windows {
			start, err := availabilityMinute(window.Start, false)
			if err != nil {
				return nil, err
			}
			end, err := availabilityMinute(window.End, true)
			if err != nil {
				return nil, err
			}
			if start >= end {
				return nil, errors.New("Date exception windows must end later on the same day")
			}
			windows = append(windows, availabilityWindow{start, end})
		}
		plan.exceptions[exception.Date] = windows
	}
	return plan, nil
}

func (plan *channelAvailabilityPlan) allows(now time.Time) bool {
	if plan.err != nil {
		return false
	}
	if plan.location == nil {
		return true
	}
	local := now.In(plan.location)
	minute := local.Hour()*60 + local.Minute()
	if windows, exists := plan.exceptions[local.Format("2006-01-02")]; exists {
		for _, window := range windows {
			if minute >= window.start && minute < window.end {
				return true
			}
		}
		return false
	}
	day := (int(local.Weekday()) + 6) % 7
	for _, window := range plan.weekly[day] {
		if minute >= window.start && (window.end < window.start || minute < window.end) {
			return true
		}
	}
	// An exception on the start date replaces that date's overnight rule too.
	if _, overridden := plan.exceptions[local.AddDate(0, 0, -1).Format("2006-01-02")]; !overridden {
		for _, window := range plan.weekly[(day+6)%7] {
			if window.end < window.start && minute < window.end {
				return true
			}
		}
	}
	return false
}

func (channel *Channel) availabilityPlan() *channelAvailabilityPlan {
	if channel.compiledAvailability != nil && channel.compiledAvailability.raw == channel.Availability {
		return channel.compiledAvailability
	}
	plan, err := compileChannelAvailability(channel.Availability)
	if err != nil {
		return &channelAvailabilityPlan{raw: channel.Availability, err: err}
	}
	return plan
}

func (channel *Channel) ValidateAvailability() error {
	_, err := compileChannelAvailability(channel.Availability)
	return err
}

func (channel *Channel) ScheduleAllowsAt(now time.Time) bool {
	return channel != nil && channel.availabilityPlan().allows(now)
}

func (channel *Channel) EffectiveStatusAt(now time.Time) int {
	if channel.Status == common.ChannelStatusEnabled && !channel.ScheduleAllowsAt(now) {
		return common.ChannelStatusOutsideSchedule
	}
	return channel.Status
}

// PopulateAvailabilityState only operates on management response copies. Routing
// evaluates the same immutable rules without computing the bounded UI preview.
func (channel *Channel) PopulateAvailabilityState(now time.Time) {
	plan := channel.availabilityPlan()
	state := &ChannelAvailabilityState{Enabled: plan.location != nil || plan.err != nil,
		Open: plan.allows(now), Invalid: plan.err != nil, EvaluatedAt: now.Unix(), PreviewDays: 8}
	if plan.location != nil {
		state.Timezone = plan.location.String()
		for next, end := now.Truncate(time.Minute).Add(time.Minute), now.Add(8*24*time.Hour); next.Before(end); next = next.Add(time.Minute) {
			if plan.allows(next) != state.Open {
				state.NextChangeAt = next.Unix()
				break
			}
		}
	}
	channel.AvailabilityState = state
	channel.EffectiveStatus = channel.Status
	if channel.Status == common.ChannelStatusEnabled && !state.Open {
		channel.EffectiveStatus = common.ChannelStatusOutsideSchedule
	}
}

// ApplyChannelEffectiveStatusFilter evaluates schedules before pagination and
// aggregation, keeping list totals and tag/type counts consistent with routing.
func ApplyChannelEffectiveStatusFilter(query *gorm.DB, status int, now time.Time) *gorm.DB {
	if status != common.ChannelStatusEnabled && status != common.ChannelStatusOutsideSchedule && status != 0 {
		return query
	}
	var channels []Channel
	if err := query.Session(&gorm.Session{}).Select("id", "availability").Where("status = ? AND availability <> ?", common.ChannelStatusEnabled, "").Find(&channels).Error; err != nil {
		query.AddError(err)
		return query
	}
	closed := make([]int, 0, len(channels))
	for _, channel := range channels {
		if !channel.ScheduleAllowsAt(now) {
			closed = append(closed, channel.Id)
		}
	}
	switch status {
	case common.ChannelStatusEnabled:
		query = query.Where("status = ?", common.ChannelStatusEnabled)
		if len(closed) > 0 {
			query = query.Where("id NOT IN ?", closed)
		}
	case common.ChannelStatusOutsideSchedule:
		if len(closed) == 0 {
			return query.Where("1 = 0")
		}
		query = query.Where("status = ? AND id IN ?", common.ChannelStatusEnabled, closed)
	case 0:
		if len(closed) == 0 {
			return query.Where("status <> ?", common.ChannelStatusEnabled)
		}
		query = query.Where("status <> ? OR id IN ?", common.ChannelStatusEnabled, closed)
	}
	return query
}
