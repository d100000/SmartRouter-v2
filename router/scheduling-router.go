package router

import (
	"net/http"

	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
)

func registerSchedulingRoutes(apiRouter *gin.RouterGroup) {
	routes := apiRouter.Group("/scheduling")
	routes.Use(middleware.AdminAuth(), middleware.DisableCache())
	handlePermissionRoute(routes, http.MethodGet, "", authz.ChannelRead, controller.GetScheduling)
	handlePermissionRoute(routes, http.MethodPost, "/config", authz.ChannelWrite, controller.UpdateSchedulingConfig)
	handlePermissionRoute(routes, http.MethodPost, "/recover", authz.ChannelOperate, controller.RecoverSchedulingChannel)
}
