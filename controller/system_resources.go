package controller

import (
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func GetSystemResources(c *gin.Context) {
	common.ApiSuccess(c, service.GetSystemResources())
}

func GetSystemResourceHistory(c *gin.Context) {
	hours, err := strconv.Atoi(c.DefaultQuery("hours", "12"))
	if err != nil || (hours != 1 && hours != 6 && hours != 12 && hours != 24) {
		common.ApiErrorMsg(c, "Invalid time range")
		return
	}
	node := c.DefaultQuery("node", common.GetNodeIdentity().Name)
	if strings.TrimSpace(node) == "" || len(node) > 128 {
		common.ApiErrorMsg(c, "Invalid node name")
		return
	}
	to := time.Now().Unix()
	from := to - int64(hours)*3600
	points, err := model.ListSystemResourceSamples(c.Request.Context(), node, from, to)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{
		"node": node, "from": from, "to": to, "interval_seconds": 60, "points": points,
	})
}
