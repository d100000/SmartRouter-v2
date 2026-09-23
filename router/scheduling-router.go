package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
)

func registerSchedulingRoutes(apiRouter *gin.RouterGroup) {
	routes := apiRouter.Group("/scheduling")
	routes.Use(middleware.AdminAuth(), middleware.DisableCache())
	routes.GET("", middleware.RequirePermission(authz.ChannelRead), controller.GetScheduling)
	routes.POST("/config", middleware.RequirePermission(authz.ChannelWrite), controller.UpdateSchedulingConfig)
	routes.POST("/recover", middleware.RequirePermission(authz.ChannelOperate), controller.RecoverSchedulingChannel)
}
