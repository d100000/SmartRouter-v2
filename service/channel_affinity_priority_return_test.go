package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A preferred session that failed over to a lower priority tier keeps its
// channel while it is in use and returns to a healthy higher tier once idle.
func TestDemotedSessionReturnsToHigherPriorityAfterIdle(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const name, primary, backup = "priority-return", 6201, 6202
	setSchedulingTestConfig(t, nil)
	for _, id := range []int{primary, backup} {
		createChannelSelectAutoGroupsChannel(t, db, id, "default", name)
	}
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", primary).Update("priority", 10).Error)
	require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", primary).Update("priority", 10).Error)
	model.InitChannelCache()
	require.NoError(t, SyncSchedulingConfig())

	succeed := func(id int) {
		channel, err := model.CacheGetChannelForRouting(id)
		require.NoError(t, err)
		c := routingTestContext(t)
		require.NoError(t, ReserveSchedulingChannel(c, channel, "default", name))
		info := &relaycommon.RelayInfo{OriginModelName: name, UsingGroup: "default"}
		StartSchedulingAttempt(c, info)
		FinishSchedulingAttempt(c, info, nil)
		EndSchedulingRequest(c)
	}
	route := func(newRequest func() *gin.Context) int {
		c := newRequest()
		selected, _, selectErr := SelectChannelForRequest(c, name, &RetryParam{Ctx: c, TokenGroup: "default", ModelName: name})
		require.Nil(t, selectErr)
		require.NotNil(t, selected)
		RecordChannelAffinity(c, selected.Id)
		EndSchedulingRequest(c)
		return selected.Id
	}
	bound := func(newRequest func() *gin.Context) int {
		c := newRequest()
		id, found := GetPreferredChannelByAffinity(c, name, "default")
		require.True(t, found)
		return id
	}
	// Moves the recorded last use of the session's binding into the past.
	age := func(newRequest func() *gin.Context, by time.Duration) {
		c := newRequest()
		_, found := GetPreferredChannelByAffinity(c, name, "default")
		require.True(t, found)
		cacheKey, _, ok := getChannelAffinityContext(c)
		require.True(t, ok)
		require.NoError(t, getChannelAffinityCache().SetWithTTL(channelAffinitySeenKey(cacheKey, backup), int(time.Now().Add(-by).Unix()), time.Hour))
	}

	t.Run("a session in use through a higher-tier outage is not idle when it ends", func(t *testing.T) {
		// The primary has no recent success yet, as after an outage.
		newRequest := bindRoutingTestSession(t, name, "prefer", backup)
		assert.Equal(t, backup, route(newRequest))
		age(newRequest, 6*time.Minute)
		assert.Equal(t, backup, route(newRequest), "nothing healthy to return to")
		// The primary recovers. The session was used a moment ago, so it
		// keeps the channel that holds its cache.
		succeed(primary)
		assert.Equal(t, backup, route(newRequest), "the request during the outage restarted the idle clock")
		age(newRequest, 6*time.Minute)
		assert.Equal(t, primary, route(newRequest), "once really idle it returns")
	})

	t.Run("an active session keeps its channel and an idle one returns", func(t *testing.T) {
		newRequest := bindRoutingTestSession(t, name, "prefer", backup)
		assert.Equal(t, backup, route(newRequest), "the first request only starts the idle clock")
		assert.Equal(t, backup, route(newRequest), "a session in use keeps the channel that holds its cache")
		age(newRequest, 4*time.Minute)
		assert.Equal(t, backup, route(newRequest), "four idle minutes are below the return interval")
		age(newRequest, 6*time.Minute)
		assert.Equal(t, primary, route(newRequest), "an idle session returns to the healthy higher tier")
		assert.Equal(t, primary, bound(newRequest), "and is bound there")
		assert.Equal(t, primary, route(newRequest))

		// A later failover demotes the session again: it must stay on the
		// backup while in use, without inheriting the earlier idle clock.
		failover := newRequest()
		_, found := GetPreferredChannelByAffinity(failover, name, "default")
		require.True(t, found)
		failover.Set("channel_id", backup)
		RecordChannelAffinity(failover, backup)
		require.Equal(t, backup, bound(newRequest))
		assert.Equal(t, backup, route(newRequest), "a new demotion starts a new idle clock")
		assert.Equal(t, backup, route(newRequest))
	})

	t.Run("a return that ends on the old channel waits another interval", func(t *testing.T) {
		newRequest := bindRoutingTestSession(t, name, "prefer", backup)
		assert.Equal(t, backup, route(newRequest))
		age(newRequest, 6*time.Minute)
		// The returning request is sent to the primary, fails there and is
		// finally served by the backup again.
		c := newRequest()
		selected, _, selectErr := SelectChannelForRequest(c, name, &RetryParam{Ctx: c, TokenGroup: "default", ModelName: name})
		require.Nil(t, selectErr)
		require.Equal(t, primary, selected.Id)
		c.Set("channel_id", backup)
		RecordChannelAffinity(c, backup)
		EndSchedulingRequest(c)
		require.Equal(t, backup, bound(newRequest))
		assert.Equal(t, backup, route(newRequest), "the next request does not try the return again at once")
	})

	t.Run("returns even when bindings never switch on success", func(t *testing.T) {
		newRequest := bindRoutingTestSession(t, name, "prefer", backup)
		operation_setting.GetChannelAffinitySetting().SwitchOnSuccess = false
		assert.Equal(t, backup, route(newRequest))
		age(newRequest, 6*time.Minute)
		assert.Equal(t, primary, route(newRequest))
		assert.Equal(t, primary, bound(newRequest))
	})

	t.Run("a negative interval keeps demoted sessions in place", func(t *testing.T) {
		newRequest := bindRoutingTestSession(t, name, "prefer", backup)
		operation_setting.GetChannelAffinitySetting().ReturnToPriorityIdleSeconds = -1
		assert.Equal(t, backup, route(newRequest))
		age(newRequest, time.Hour)
		assert.Equal(t, backup, route(newRequest))
	})

	t.Run("a strict session never moves for priority", func(t *testing.T) {
		newRequest := bindRoutingTestSession(t, name, "strict", backup)
		assert.Equal(t, backup, route(newRequest))
		age(newRequest, time.Hour)
		assert.Equal(t, backup, route(newRequest))
	})

	t.Run("a session on the highest tier is left alone", func(t *testing.T) {
		newRequest := bindRoutingTestSession(t, name, "prefer", primary)
		succeed(backup)
		assert.Equal(t, primary, route(newRequest))
		c := newRequest()
		_, found := GetPreferredChannelByAffinity(c, name, "default")
		require.True(t, found)
		cacheKey, _, _ := getChannelAffinityContext(c)
		_, seen, err := getChannelAffinityCache().Get(channelAffinitySeenKey(cacheKey, primary))
		require.NoError(t, err)
		assert.False(t, seen, "no bookkeeping for sessions that are where priority wants them")
	})
}
