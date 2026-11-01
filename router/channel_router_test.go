package router

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestChannelReadEndpointsRequireReadPermission(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodGet, "/default_base_urls", authz.ChannelRead, controller.GetChannelDefaultBaseURLs)
	assertChannelRoutePermission(t, http.MethodGet, "/recent_stats", authz.ChannelRead, controller.GetChannelRecentStats)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	registerChannelRoutes(engine.Group("/api"))
	for _, path := range []string{"/api/channel/1/funding", "/api/channel/default_base_urls", "/api/channel/recent_stats", "/api/channel-overview", "/api/upstream-supplier", "/api/channel/1/upstream-credentials"} {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	}
	for _, request := range []struct{ method, path string }{{http.MethodPost, "/api/channel/1/funding/probe"}, {http.MethodPost, "/api/channel/1/funding/dismiss"}, {http.MethodPost, "/api/upstream-supplier"}, {http.MethodPut, "/api/upstream-credential/credential-id"}} {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(request.method, request.path, nil))
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	}
	assertChannelRoutePermission(t, http.MethodGet, "/:id/upstream-credentials", authz.ChannelRead, controller.GetChannelUpstreamCredentials)
}

func TestChannelStatusRoutesUseExpectedPermissions(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodGet, "/:id/funding", authz.ChannelRead, controller.GetChannelFunding)
	assertChannelRoutePermission(t, http.MethodPost, "/:id/funding/probe", authz.ChannelOperate, controller.OperateChannelFunding)
	assertChannelRoutePermission(t, http.MethodPost, "/:id/funding/dismiss", authz.ChannelOperate, controller.OperateChannelFunding)
	assertChannelRoutePermission(t, http.MethodGet, "/priority/order", authz.ChannelRead, controller.GetChannelPriorityOrder)
	assertChannelRoutePermission(t, http.MethodPost, "/priority/reorder", authz.ChannelWrite, controller.ReorderChannelPriority)
	assertChannelRoutePermission(t, http.MethodGet, "/:id/vllm/status", authz.ChannelRead, controller.GetVLLMChannelStatus)
	assertChannelRoutePermission(t, http.MethodGet, "/:id/sglang/status", authz.ChannelRead, controller.GetSGLangChannelStatus)
	assertChannelRoutePermission(t, http.MethodPost, "/:id/status", authz.ChannelOperate, controller.UpdateChannelStatus)
	assertChannelRoutePermission(t, http.MethodPost, "/status/batch", authz.ChannelOperate, controller.BatchUpdateChannelStatus)
	assertChannelRoutePermission(t, http.MethodPut, "/", authz.ChannelWrite, controller.UpdateChannel)
}

func TestChannelDeleteRoutesUseSensitiveWritePermission(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodDelete, "/:id", authz.ChannelSensitiveWrite, controller.DeleteChannel)
	assertChannelRoutePermission(t, http.MethodPost, "/batch", authz.ChannelSensitiveWrite, controller.DeleteChannelBatch)
	assertChannelRoutePermission(t, http.MethodDelete, "/disabled", authz.ChannelSensitiveWrite, controller.DeleteDisabledChannel)
	assertChannelRoutePermission(t, http.MethodPut, "/", authz.ChannelWrite, controller.UpdateChannel)
	assertChannelRoutePermission(t, http.MethodPut, "/tag", authz.ChannelWrite, controller.EditTagChannels)
	assertChannelRoutePermission(t, http.MethodPost, "/batch/tag", authz.ChannelWrite, controller.BatchSetChannelTag)
}

func TestChannelStatusRoutesRegisterWithoutConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api := engine.Group("/api")

	require.NotPanics(t, func() {
		registerChannelRoutes(api)
	})
}

func TestChannelFundingRouteAuthorization(t *testing.T) {
	previousDB, previousLog := model.DB, model.LOG_DB
	previousMain, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousRedis, previousMaster := common.RedisEnabled, common.IsMasterNode
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	connection, err := db.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	model.DB, model.LOG_DB, common.RedisEnabled = db, db, false
	common.IsMasterNode = true
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	t.Cleanup(func() {
		model.DB, model.LOG_DB, common.RedisEnabled = previousDB, previousLog, previousRedis
		common.IsMasterNode = previousMaster
		common.SetDatabaseTypes(previousMain, previousLogType)
		assert.NoError(t, connection.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserAccessToken{}, &model.AuditLog{}, &model.CasbinRule{}, &model.AuthzRole{}, &model.ChannelFundingIncident{}, &model.ChannelFundingDelivery{}, &model.ChannelCredentialBinding{}))
	require.NoError(t, authz.Init(db))
	require.NoError(t, db.Create(&model.ChannelFundingIncident{ID: 1, ChannelID: 1, Evidence: "administrator-only funding evidence"}).Error)
	engine := gin.New()
	registerChannelRoutes(engine.Group("/api"))
	for index, tc := range []struct {
		name, method, path, scope string
		role, status              int
	}{
		{"regular user cannot read", http.MethodGet, "/api/channel/1/funding", "channel:read", common.RoleCommonUser, http.StatusForbidden},
		{"regular user cannot operate", http.MethodPost, "/api/channel/1/funding/probe", "channel:operate", common.RoleCommonUser, http.StatusForbidden},
		{"admin read admitted", http.MethodGet, "/api/channel/1/funding", "channel:read", common.RoleAdminUser, http.StatusOK},
		{"read scope cannot probe", http.MethodPost, "/api/channel/1/funding/probe", "channel:read", common.RoleAdminUser, http.StatusForbidden},
		{"read scope cannot dismiss", http.MethodPost, "/api/channel/1/funding/dismiss", "channel:read", common.RoleAdminUser, http.StatusForbidden},
		{"operate scope admitted", http.MethodPost, "/api/channel/1/funding/probe", "channel:operate", common.RoleAdminUser, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := model.User{Username: tc.name, Role: tc.role, Status: common.UserStatusEnabled, Group: "default", AffCode: tc.name, AuthVersion: 1}
			require.NoError(t, db.Create(&user).Error)
			raw := model.AccessTokenPrefix + strings.Repeat(string(rune('k'+index)), 43)
			token := model.UserAccessToken{UserId: user.Id, Name: tc.name, TokenHash: model.AccessTokenFingerprint(raw), TokenHint: model.AccessTokenHint(raw), ExpiresAt: time.Now().Add(time.Hour).Unix()}
			require.NoError(t, token.SetScopes([]string{tc.scope}))
			require.NoError(t, db.Create(&token).Error)
			request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			request.Header.Set("Authorization", "Bearer "+raw)
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)
			assert.Equal(t, tc.status, recorder.Code, recorder.Body.String())
			if tc.status == http.StatusOK {
				assert.Contains(t, recorder.Body.String(), "administrator-only funding evidence")
			} else {
				assert.NotContains(t, recorder.Body.String(), "administrator-only funding evidence")
			}
			assert.NotContains(t, recorder.Body.String(), raw)
		})
	}
}

func assertChannelRoutePermission(t *testing.T, method string, path string, permission authz.Permission, handler any) {
	t.Helper()
	for _, route := range channelPermissionRoutes {
		if route.method == method && route.path == path {
			assert.Equal(t, permission, route.permission)
			assert.Equal(t, reflect.ValueOf(handler).Pointer(), reflect.ValueOf(route.handler).Pointer())
			return
		}
	}
	t.Fatalf("route %s %s not found", method, path)
}
