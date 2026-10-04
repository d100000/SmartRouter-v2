package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestSchedulingRoutesRequireAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	require.NotPanics(t, func() { registerSchedulingRoutes(engine.Group("/api")) })
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/scheduling"},
		{http.MethodPost, "/api/scheduling/config"},
		{http.MethodPost, "/api/scheduling/recover"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
			assert.Equal(t, http.StatusUnauthorized, recorder.Code)
		})
	}
}

func TestSchedulingRoutesEnforceScopedTokensAndAccountPermissions(t *testing.T) {
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousType := common.MainDatabaseType()
	previousRedis, previousMaster := common.RedisEnabled, common.IsMasterNode
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	model.DB, model.LOG_DB = db, db
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	common.RedisEnabled, common.IsMasterNode = false, true
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetMainDatabaseType(previousType)
		common.RedisEnabled, common.IsMasterNode = previousRedis, previousMaster
		assert.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserAccessToken{}, &model.AuditLog{}, &model.CasbinRule{}, &model.AuthzRole{}))
	require.NoError(t, authz.Init(db))
	user := model.User{Username: "scheduling-scoped-admin", Password: "unused-test-password", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "scheduling-scoped-admin"}
	require.NoError(t, db.Create(&user).Error)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	registerSchedulingRoutes(engine.Group("/api"))

	// Invalid handler input lets us verify admission without changing scheduler
	// configuration or requiring channel data.
	for index, tc := range []struct {
		name, method, path, scope, code string
		status                          int
		expired, revoke                 bool
	}{
		{name: "read admitted", method: http.MethodGet, path: "/api/scheduling?stream=invalid", scope: "channel:read", status: http.StatusBadRequest},
		{name: "write admitted", method: http.MethodPost, path: "/api/scheduling/config", scope: "channel:write", status: http.StatusBadRequest},
		{name: "recover admitted", method: http.MethodPost, path: "/api/scheduling/recover", scope: "channel:operate", status: http.StatusBadRequest},
		{name: "read cannot write", method: http.MethodPost, path: "/api/scheduling/config", scope: "channel:read", status: http.StatusForbidden, code: "ACCESS_TOKEN_SCOPE_DENIED"},
		{name: "write cannot recover", method: http.MethodPost, path: "/api/scheduling/recover", scope: "channel:write", status: http.StatusForbidden, code: "ACCESS_TOKEN_SCOPE_DENIED"},
		{name: "unrelated scope cannot read", method: http.MethodGet, path: "/api/scheduling?stream=invalid", scope: "profile:read", status: http.StatusForbidden, code: "ACCESS_TOKEN_SCOPE_DENIED"},
		{name: "expired token", method: http.MethodGet, path: "/api/scheduling?stream=invalid", scope: "channel:read", status: http.StatusUnauthorized, code: "ACCESS_TOKEN_EXPIRED", expired: true},
		{name: "account permission revoked", method: http.MethodGet, path: "/api/scheduling?stream=invalid", scope: "channel:read", status: http.StatusForbidden, revoke: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := model.AccessTokenPrefix + strings.Repeat(string(rune('a'+index)), 43)
			token := model.UserAccessToken{UserId: user.Id, Name: tc.name, TokenHash: model.AccessTokenFingerprint(raw), TokenHint: model.AccessTokenHint(raw), ExpiresAt: time.Now().Add(time.Hour).Unix()}
			if tc.expired {
				token.ExpiresAt = time.Now().Add(-time.Hour).Unix()
			}
			require.NoError(t, token.SetScopes([]string{tc.scope}))
			require.NoError(t, db.Create(&token).Error)
			if tc.revoke {
				require.NoError(t, authz.SetUserPermissions(user.Id, authz.PermissionsMap{authz.ResourceChannel: {authz.ActionRead: false}}))
			}
			request := httptest.NewRequest(tc.method, tc.path, strings.NewReader("invalid-json"))
			request.Header.Set("Authorization", "Bearer "+raw)
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)
			assert.Equal(t, tc.status, recorder.Code, recorder.Body.String())
			if tc.code != "" {
				assert.Contains(t, recorder.Body.String(), tc.code)
			}
			if tc.revoke {
				assert.NotContains(t, recorder.Body.String(), "ACCESS_TOKEN_SCOPE_DENIED")
			}
			assert.NotContains(t, recorder.Body.String(), raw)
		})
	}
}
