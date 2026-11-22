package model

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

type channelRPMObservation struct {
	AtMS        int64 `json:"at_ms"`
	LockVersion int64 `json:"lock_version"`
	Occupied    int64 `json:"occupied"`
	FullUntilMS int64 `json:"full_until_ms,omitempty"`
}

// A committed send cannot be released. A window consisting of sent slots
// remains full until its required number of sends expires. For an open window,
// every new slot costs at least one committed lock_version increment, including
// writes from older nodes. Expirations and cancellations only reduce occupancy.
func (o channelRPMObservation) check(config ChannelLimitConfig, at int64) (decided bool, err error) {
	if o.AtMS <= at-channelLimitMinuteMS || o.AtMS > at || o.LockVersion < 0 || o.LockVersion > config.LockVersion || o.Occupied < 0 {
		return false, nil
	}
	if o.Occupied >= config.RPM && o.FullUntilMS > at && o.FullUntilMS <= o.AtMS+channelLimitMinuteMS {
		return true, config.limitError("rpm_limited", o.FullUntilMS)
	}
	if o.Occupied < config.RPM && config.LockVersion-o.LockVersion < config.RPM-o.Occupied {
		return true, nil
	}
	return false, nil
}

// CheckChannelRPM uses the current primary-database configuration from
// GetChannelLimitConfig for an early routing decision. Final admission always
// rechecks under the channel lock.
func CheckChannelRPM(config ChannelLimitConfig, now time.Time) error {
	if config.RPM == 0 {
		return nil
	}
	now = config.windowTime(now)
	state := ChannelLimitState{Config: config}
	cacheable := common.RedisEnabled && common.RDB != nil
	var key string
	const cacheTimeout = 25 * time.Millisecond
	if cacheable {
		key = fmt.Sprintf("new-api:channel_rpm:v1:%d:%d:%d:%d", config.ChannelID, config.ObservedSince, config.Revision, config.RPM)
		ctx, cancel := context.WithTimeout(context.Background(), cacheTimeout)
		raw, err := common.RDB.Get(ctx, key).Bytes()
		cancel()
		cacheable = err == nil || err == redis.Nil
		var previous channelRPMObservation
		if err == nil && common.Unmarshal(raw, &previous) == nil {
			if decided, refusal := previous.check(config, now.UnixMilli()); decided {
				return refusal
			}
		}
	}
	if err := state.readRPM(DB, now, ""); err != nil {
		return err
	}
	occupied := state.RPMUsed + state.RPMReserved
	if cacheable && (occupied < config.RPM || state.RPMReserved == 0) {
		observation := channelRPMObservation{
			AtMS: now.UnixMilli(), LockVersion: config.LockVersion, Occupied: occupied, FullUntilMS: state.RPMRetryAt,
		}
		if raw, err := common.Marshal(observation); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), cacheTimeout)
			_ = common.RDB.Set(ctx, key, raw, time.Minute).Err()
			cancel()
		}
	}
	return state.rpmRefusal()
}

// One immutable observation per slot bounds memory when channels are deleted.
// A collision, restart, configuration change or clock rollback falls back to
// SQL. Redis values never enter this table or authorize a dispatch.
var committedRPMWindows [1024]atomic.Pointer[committedRPMWindow]

type committedRPMWindow struct {
	db            *gorm.DB
	channelID     int
	revision      int64
	observedSince int64
	rpm           int64
	atMS          int64
	lockVersion   int64
	occupied      int64
	attemptID     string
}

// The caller already incremented lock_version for the current transaction,
// which has not inserted its send yet. Each previous committed transaction
// could have occupied at most one additional slot. Before using this bound,
// verify that our witness send still exists in the locked database history.
func (p *committedRPMWindow) upperBound(db *gorm.DB, config *ChannelLimitConfig, at int64) (int64, bool) {
	if p == nil || p.db != db || p.channelID != config.ChannelID || p.observedSince != config.ObservedSince ||
		p.revision != config.Revision || p.rpm != config.RPM || p.atMS > at || at-p.atMS >= channelLimitMinuteMS ||
		p.lockVersion < 0 || p.lockVersion >= config.LockVersion || p.occupied >= config.RPM {
		return 0, false
	}
	delta := config.LockVersion - p.lockVersion - 1
	if delta >= config.RPM-p.occupied {
		return 0, false
	}
	return p.occupied + delta, true
}
