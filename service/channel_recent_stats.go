package service

import (
	"sync"
	"time"

	"github.com/QuantumNous/new-api/pkg/scheduler"
)

var channelRecentStatsOnce sync.Once

// StartChannelRecentStatsTask publishes instance-local statistics asynchronously.
// Neither channel listing nor the statistics endpoint performs aggregation.
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
