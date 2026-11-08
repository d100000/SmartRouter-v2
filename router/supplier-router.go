package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"net/http"
)

func registerSupplierRoutes(api *gin.RouterGroup) {
	supplier := api.Group("/supplier", middleware.SupplierAuth(), middleware.DisableCache())
	supplier.GET("/self", controller.GetSupplierSelf)
	supplier.GET("/channels", controller.ListSupplierChannels)
	supplier.GET("/channels/:id", controller.GetSupplierChannel)
	supplier.POST("/channels", middleware.CriticalRateLimit(), controller.SaveSupplierChannel)
	supplier.PUT("/channels/:id", middleware.CriticalRateLimit(), controller.SaveSupplierChannel)
	supplier.POST("/channels/:id/status", controller.SetSupplierChannelStatus)
	supplier.POST("/channels/:id/copy", middleware.CriticalRateLimit(), controller.CopySupplierChannel)
	supplier.DELETE("/channels/:id", controller.DiscardSupplierChannel)
	supplier.POST("/channels/:id/test", middleware.CriticalRateLimit(), controller.TestSupplierChannel)
	supplier.GET("/channels/:id/limits", controller.GetSupplierChannelLimits)
	supplier.GET("/channels/:id/budget-events", controller.GetSupplierBudgetEvents)
	supplier.GET("/channels/:id/limit-events", controller.GetSupplierLimitEvents)
	supplier.PUT("/channels/:id/limits", controller.UpdateSupplierChannelLimits)
	supplier.GET("/overview", controller.GetSupplierOverview)
	supplier.GET("/logs", controller.GetSupplierLogs)
	supplier.GET("/logs/export", controller.ExportSupplierLogs)

	reader := api.Group("/channel/read-only", middleware.ChannelReadAuth(), middleware.DisableCache())
	reader.GET("/", controller.ListSupplierChannels)
	reader.GET("/overview", controller.GetSupplierOverview)
	reader.GET("/:id", controller.GetSupplierChannel)
	reader.GET("/:id/limits", controller.GetSupplierChannelLimits)

	admin := api.Group("/suppliers", middleware.AdminAuth(), middleware.DisableCache())
	handlePermissionRoute(admin, http.MethodPost, "/owners/:supplier_id/channels", authz.ChannelSensitiveWrite, middleware.CriticalRateLimit(), controller.SaveSupplierChannel)
	handlePermissionRoute(admin, http.MethodPut, "/channels/:id", authz.ChannelSensitiveWrite, middleware.CriticalRateLimit(), controller.SaveSupplierChannel)
	handlePermissionRoute(admin, http.MethodGet, "/templates", authz.ChannelRead, controller.GetSupplierTemplates)
	handlePermissionRoute(admin, http.MethodGet, "/", authz.ChannelRead, controller.ListSupplierAccounts)
	handlePermissionRoute(admin, http.MethodPost, "/", authz.ChannelSensitiveWrite, middleware.CriticalRateLimit(), controller.SaveSupplierAccount)
	handlePermissionRoute(admin, http.MethodPut, "/:id", authz.ChannelSensitiveWrite, middleware.CriticalRateLimit(), controller.SaveSupplierAccount)
	channel := api.Group("/channel", middleware.AdminAuth(), middleware.DisableCache())
	handlePermissionRoute(channel, http.MethodGet, "/:id/limits", authz.ChannelRead, controller.GetSupplierChannelLimits)
	handlePermissionRoute(channel, http.MethodGet, "/:id/limit-events", authz.ChannelRead, controller.GetSupplierLimitEvents)
	handlePermissionRoute(channel, http.MethodPut, "/:id/limits", authz.ChannelWrite, controller.UpdateSupplierChannelLimits)
}
