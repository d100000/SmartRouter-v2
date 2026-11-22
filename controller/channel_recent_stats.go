package controller

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/scheduler"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type channelRPMUsage struct {
	Limit     *int64 `json:"limit"`
	Used      int64  `json:"used"`
	Reserved  int64  `json:"reserved"`
	Supported bool   `json:"supported"`
	Observed  bool   `json:"observed"`
}

// Only the displayed channels are queried. The rolling window is shared by
// every gateway instance, including ones that have no local scheduler samples.
func channelRPMForPage(ctx context.Context, ids []int, now time.Time) (map[int]*channelRPMUsage, error) {
	db := model.DB.WithContext(ctx)
	if now.IsZero() {
		var err error
		now, err = model.ChannelLimitDatabaseTime(db)
		if err != nil {
			return nil, err
		}
	}
	var channels []model.Channel
	if err := db.Select("id", "type", "setting").Where("id IN ?", ids).Find(&channels).Error; err != nil {
		return nil, err
	}
	result := make(map[int]*channelRPMUsage, len(channels))
	for i := range channels {
		result[channels[i].Id] = &channelRPMUsage{Supported: service.ValidateChannelLimitSupport(&channels[i], model.ChannelLimitSettings{RPM: 1}) == nil}
	}
	start := now.Add(-time.Minute).UnixMilli()
	var configs []model.ChannelLimitConfig
	if err := db.Select("channel_id", "rpm", "history_complete", "observed_since").Where("channel_id IN ?", ids).Find(&configs).Error; err != nil {
		return nil, err
	}
	for _, config := range configs {
		if usage := result[config.ChannelID]; usage != nil {
			if config.RPM > 0 {
				usage.Limit = &config.RPM
			}
			usage.Observed = config.HistoryComplete || config.ObservedSince > 0 && config.ObservedSince <= start
		}
	}
	var counts []struct {
		ChannelID      int
		Used, Reserved int64
	}
	// Separate index ranges keep expired reservations and historical sends
	// outside the scan. Both counts use the same database statement snapshot.
	window := db.Model(&model.ChannelLimitAttempt{}).Where("channel_id IN ? AND released = ?", ids, false).
		Group("channel_id").Session(&gorm.Session{})
	sent := window.Select("channel_id, COUNT(*) AS used, 0 AS reserved").Where("sent_at_ms > ?", start)
	pending := window.Select("channel_id, 0 AS used, COUNT(*) AS reserved").Where("sent_at_ms = 0 AND created_at_ms > ?", start)
	err := db.Raw("? UNION ALL ?", sent, pending).Scan(&counts).Error
	if err != nil {
		return nil, err
	}
	for _, count := range counts {
		if usage := result[count.ChannelID]; usage != nil {
			usage.Used += count.Used
			usage.Reserved += count.Reserved
		}
	}
	return result, nil
}

func GetChannelRecentStats(c *gin.Context) {
	var channelIDs []int
	if raw := c.Query("channel_ids"); raw != "" {
		if len(raw) > 8192 || strings.Count(raw, ",") >= 500 {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "channel_ids must contain at most 500 positive integers"})
			return
		}
		seen := make(map[int]bool)
		for part := range strings.SplitSeq(raw, ",") {
			id, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || id <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "channel_ids must contain positive integers"})
				return
			}
			if !seen[id] {
				seen[id] = true
				channelIDs = append(channelIDs, id)
			}
		}
	}
	data := struct {
		scheduler.RecentChannelStats
		RPM map[int]*channelRPMUsage `json:"rpm_by_channel,omitempty"`
	}{RecentChannelStats: scheduler.Default.FreshRecentChannelStats(channelIDs...)}
	if len(channelIDs) > 0 {
		var err error
		data.RPM, err = channelRPMForPage(c.Request.Context(), channelIDs, time.Time{})
		if err != nil {
			logger.LogError(c, "failed to load channel RPM: "+err.Error())
		}
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}
