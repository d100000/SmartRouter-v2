package controller

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/pkg/scheduler"
	"github.com/gin-gonic/gin"
)

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
	c.JSON(http.StatusOK, gin.H{"success": true, "data": scheduler.Default.FreshRecentChannelStats(channelIDs...)})
}
