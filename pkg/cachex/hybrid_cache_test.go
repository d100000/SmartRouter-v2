package cachex

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConditionalBindingAcrossRedisClients(t *testing.T) {
	server := miniredis.RunT(t)
	newClient := func() *HybridCache[int] {
		client := redis.NewClient(&redis.Options{Addr: server.Addr()})
		t.Cleanup(func() { assert.NoError(t, client.Close()) })
		return NewHybridCache(HybridCacheConfig[int]{Namespace: "binding", Redis: client, RedisCodec: IntCodec{}})
	}
	a, b := newClient(), newClient()
	channelA, channelB := 11, 22
	var winners atomic.Int32
	var writes sync.WaitGroup
	for _, cache := range []*HybridCache[int]{a, b} {
		writes.Go(func() {
			changed, err := CompareAndSwapWithTTL(cache, "session", nil, &channelA, time.Hour)
			assert.NoError(t, err)
			if changed {
				winners.Add(1)
			}
		})
	}
	writes.Wait()
	require.Equal(t, int32(1), winners.Load())
	changed, err := CompareAndSwapWithTTL(a, "session", &channelA, &channelB, time.Hour)
	require.NoError(t, err)
	require.True(t, changed)

	server.FastForward(10 * time.Minute)
	for _, late := range []*int{&channelA, nil} {
		changed, err = CompareAndSwapWithTTL(b, "session", &channelA, late, time.Hour)
		require.NoError(t, err)
		assert.False(t, changed, "a stale request cannot renew or delete the replacement")
	}
	assert.Equal(t, 50*time.Minute, server.TTL(a.FullKey("session")))
	current, found, err := b.Get("session")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, channelB, current)
	changed, err = CompareAndSwapWithTTL(b, "session", &channelB, nil, 0)
	require.NoError(t, err)
	assert.True(t, changed)
	_, found, err = a.Get("session")
	require.NoError(t, err)
	assert.False(t, found)
}
