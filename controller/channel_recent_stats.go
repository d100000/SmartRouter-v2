package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/pkg/scheduler"
	"github.com/gin-gonic/gin"
)

func GetChannelRecentStats(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"success": true, "data": scheduler.Default.RecentChannelStats()})
}
