package service

import (
	"sync"
	"time"

	"github.com/QuantumNous/new-api/pkg/scheduler"
)

var channelRecentStatsOnce sync.Once

// StartChannelRecentStatsTask prewarms observations and prunes idle history.
// Explicit statistics reads also refresh without visiting database or routing state.
func StartChannelRecentStatsTask() {
	channelRecentStatsOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				scheduler.Default.RefreshRecentChannelStats()
				<-ticker.C
			}
		}()
	})
}
