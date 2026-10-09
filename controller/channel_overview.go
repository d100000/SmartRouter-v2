package controller

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"
)

type overviewCacheEntry struct {
	result  *model.OverviewResult
	expires time.Time
}

var overviewCache = struct {
	sync.Mutex
	entries map[string]overviewCacheEntry
}{entries: make(map[string]overviewCacheEntry)}
var overviewQueries singleflight.Group
var overviewQuerySlots = make(chan struct{}, 4)
var errOverviewBusy = errors.New("channel overview query capacity reached")

func GetChannelOverview(c *gin.Context) {
	dimension := c.DefaultQuery("dimension", "channel")
	if dimension != "channel" && dimension != "group" && dimension != "key" && dimension != "supplier" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid analysis dimension"})
		return
	}
	days, err := strconv.Atoi(c.DefaultQuery("days", "7"))
	if err != nil || days != 7 && days != 30 && days != 90 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Analysis period must be 7, 30 or 90 days"})
		return
	}
	objectID := c.Query("object_id")
	if len(objectID) > 191 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid analysis object"})
		return
	}
	if dimension == "channel" && objectID != "" {
		id, err := strconv.Atoi(objectID)
		if err != nil || id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid channel ID"})
			return
		}
		objectID = strconv.Itoa(id)
	}
	// Delimited, validated dimensions/period; the last component is not parsed.
	key := time.Now().UTC().Format("2006-01-02") + "/" + dimension + "/" + strconv.Itoa(days) + "/" + objectID
	overviewCache.Lock()
	cached, ok := overviewCache.entries[key]
	overviewCache.Unlock()
	if ok && time.Now().Before(cached.expires) {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": cached.result})
		return
	}
	query := overviewQueries.DoChan(key, func() (any, error) {
		overviewCache.Lock()
		cached, ok := overviewCache.entries[key]
		overviewCache.Unlock()
		if ok && time.Now().Before(cached.expires) {
			return cached.result, nil
		}
		select {
		case overviewQuerySlots <- struct{}{}:
		default:
			return nil, errOverviewBusy
		}
		defer func() { <-overviewQuerySlots }()
		// One disconnected browser must not cancel the shared query for others;
		// the query itself always has a strict independent two-second deadline.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		result, err := model.GetChannelOverview(ctx, dimension, days, objectID)
		if err != nil {
			return nil, err
		}
		overviewCache.Lock()
		if len(overviewCache.entries) >= 128 {
			for existing, entry := range overviewCache.entries {
				if time.Now().After(entry.expires) {
					delete(overviewCache.entries, existing)
				}
			}
			if len(overviewCache.entries) >= 128 {
				for existing := range overviewCache.entries {
					delete(overviewCache.entries, existing)
					break
				}
			}
		}
		overviewCache.entries[key] = overviewCacheEntry{result: result, expires: time.Now().Add(30 * time.Second)}
		overviewCache.Unlock()
		return result, nil
	})
	select {
	case <-c.Request.Context().Done():
		return
	case answer := <-query:
		if answer.Err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(answer.Err, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			c.JSON(status, gin.H{"success": false, "message": "Analysis is temporarily unavailable; retry later"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "data": answer.Val})
	}
}
