package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestSupplierOnboardingUploadReviewAndClose(t *testing.T) {
	admin, _ := setupSecurityEnrollmentTest(t)
	require.NoError(t, model.DB.Model(admin).Update("role", common.RoleRootUser).Error)
	require.NoError(t, model.PublishUserAuthCache(admin.Id))
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.UpstreamSupplier{}, &model.SupplierAccount{}, &model.SupplierChannelDraft{}, &model.OverviewAttemptEvent{}, &model.SystemTask{}, &model.ChannelFundingIncident{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(model.DB))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(model.DB))
	t.Setenv("SUPPLIER_CREDENTIAL_KEYS", `{"v1":"`+base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))+`"}`)
	t.Setenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY", "v1")
	previousSecure, previousPasswordLogin, previousCache := common.SessionCookieSecure, common.PasswordLoginEnabled, common.MemoryCacheEnabled
	common.SessionCookieSecure, common.PasswordLoginEnabled, common.MemoryCacheEnabled = true, true, false
	t.Cleanup(func() {
		common.SessionCookieSecure, common.PasswordLoginEnabled, common.MemoryCacheEnabled = previousSecure, previousPasswordLogin, previousCache
	})
	previousRatios := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-4o-mini":1}`))
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousRatios)) })
	previousGroups := ratio_setting.GroupRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"vip":1}`))
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousGroups)) })
	upstream := service.GetSupplierHTTPClient()
	previousTransport := upstream.Transport
	t.Cleanup(func() { upstream.Transport = previousTransport })
	upstream.Transport = schedulingTokenTransport(func(request *http.Request) (*http.Response, error) {
		assert.Equal(t, "Bearer upstream-test-secret", request.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chatcmpl-supplier","object":"chat.completion","model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`))}, nil
	})

	api := gin.New()
	api.POST("/api/user/login", Login)
	api.POST("/api/verify", middleware.UserAuth(), UniversalVerify)
	api.PUT("/api/user/self", middleware.UserAuth(), UpdateSelf)
	api.GET("/api/user/", middleware.AdminAuth(), GetAllUsers)
	api.POST("/api/suppliers/", middleware.AdminAuth(), middleware.RequirePermission(authz.ChannelSensitiveWrite), SaveSupplierAccount)
	api.GET("/api/supplier/self", middleware.SupplierAuth(), GetSupplierSelf)
	api.POST("/api/supplier/channels", middleware.SupplierAuth(), SaveSupplierChannel)
	api.POST("/api/supplier/channels/:id/test", middleware.SupplierAuth(), TestSupplierChannel)
	api.GET("/api/suppliers/channels/:id", middleware.AdminAuth(), middleware.RequirePermission(authz.ChannelRead), GetSupplierChannel)
	api.POST("/api/suppliers/channels/:id/publish", middleware.AdminAuth(), middleware.RequirePermission(authz.ChannelSensitiveWrite), PublishSupplierChannel)
	api.PUT("/api/suppliers/channels/:id/routing", middleware.AdminAuth(), middleware.RequirePermission(authz.ChannelWrite), UpdateSupplierChannelRouting)
	api.POST("/api/suppliers/channels/:id/close", middleware.AdminAuth(), middleware.RequirePermission(authz.ChannelSensitiveWrite), CloseSupplierChannel)
	api.GET("/api/channel/", middleware.AdminAuth(), middleware.RequirePermission(authz.ChannelRead), GetAllChannels)
	api.POST("/api/channel/:id/status", middleware.AdminAuth(), middleware.RequirePermission(authz.ChannelOperate), UpdateChannelStatus)
	request := func(method, path, token, proof string, input any) (*httptest.ResponseRecorder, securityEnrollmentResponse) {
		t.Helper()
		data, err := common.Marshal(input)
		require.NoError(t, err)
		req := httptest.NewRequest(method, "https://example.com"+path, bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if proof != "" {
			req.Header.Set("X-Security-Proof", proof)
		}
		response := httptest.NewRecorder()
		api.ServeHTTP(response, req)
		var output securityEnrollmentResponse
		if response.Code != http.StatusNotFound {
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &output), response.Body.String())
		}
		return response, output
	}
	ok := func(method, path, token, proof string, input any) map[string]any {
		t.Helper()
		response, output := request(method, path, token, proof, input)
		require.True(t, output.Success, "%s %s: %s", method, path, response.Body.String())
		var data map[string]any
		if common.GetJsonType(output.Data) == "object" {
			require.NoError(t, common.Unmarshal(output.Data, &data))
		}
		return data
	}
	login := func(username, password string) string {
		t.Helper()
		data := ok(http.MethodPost, "/api/user/login", "", "", gin.H{"username": username, "password": password})
		token, _ := data["access_token"].(string)
		require.NotEmpty(t, token)
		return token
	}
	verify := func(token, password, scope string, details any) string {
		t.Helper()
		input := gin.H{"method": "password", "password": password, "scope": scope}
		if details != nil {
			input["context"] = details
		}
		data := ok(http.MethodPost, "/api/verify", token, "", input)
		proof, _ := data["proof_token"].(string)
		require.NotEmpty(t, proof)
		return proof
	}

	const initialPassword = "supplier-temporary-2026!"
	const supplierPassword = "supplier-updated-2026!"
	const username = "supplier-flow"
	adminToken := login(admin.Username, "enrollment-password")
	provision := verify(adminToken, "enrollment-password", service.VerificationScopeSupplierProvision, service.SupplierProvisionContext{Username: username})
	supplier := ok(http.MethodPost, "/api/suppliers/", adminToken, provision, gin.H{"name": "Workflow supplier", "username": username, "password": initialPassword, "max_channels": 1})
	assert.Positive(t, supplier["user_id"])
	users := ok(http.MethodGet, "/api/user/", adminToken, "", nil)
	assert.Contains(t, fmt.Sprint(users["items"]), username)
	token := login(username, initialPassword)
	_, uninitialized := request(http.MethodGet, "/api/supplier/self", token, "", nil)
	assert.Equal(t, "AUTH_PASSWORD_CHANGE_REQUIRED", uninitialized.Code)
	passwordProof := verify(token, initialPassword, service.VerificationScopePasswordChange, nil)
	ok(http.MethodPut, "/api/user/self", token, passwordProof, gin.H{"password": supplierPassword, "original_password": initialPassword})
	token = login(username, supplierPassword)
	ok(http.MethodGet, "/api/supplier/self", token, "", nil)

	payload := gin.H{"name": "Discarded draft", "template_id": "1", "base_url": "https://1.1.1.1", "key": "upstream-test-secret", "models": []string{"supplier-unpriced-model"}}
	upload := func() map[string]any {
		t.Helper()
		proof := verify(token, supplierPassword, service.VerificationScopeSupplierChannelCredentials, service.SupplierChannelCredentialsContext{})
		return ok(http.MethodPost, "/api/supplier/channels", token, proof, payload)
	}
	discarded := upload()
	ok(http.MethodPost, fmt.Sprintf("/api/supplier/channels/%.0f/test", discarded["id"]), token, "", gin.H{"version": discarded["version"]})
	require.Eventually(t, func() bool {
		var draft model.SupplierChannelDraft
		return model.DB.Select("status").First(&draft, "channel_id = ?", int(discarded["id"].(float64))).Error == nil &&
			draft.Status == model.SupplierDraftPriceMissing
	}, 12*time.Second, 25*time.Millisecond, "submitted but unpriced model should report missing platform pricing")
	discardedPath := fmt.Sprintf("/api/suppliers/channels/%.0f/close", discarded["id"])
	refused, _ := request(http.MethodPost, discardedPath, token, "", gin.H{"version": discarded["version"]})
	assert.Equal(t, http.StatusForbidden, refused.Code)
	ok(http.MethodPost, discardedPath, adminToken, "", gin.H{"version": discarded["version"]})
	payload["name"] = "Reviewed channel"
	payload["models"] = []string{"gpt-4o-mini"}
	submitted := upload()
	id := int(submitted["id"].(float64))
	version := int64(submitted["version"].(float64))
	detailPath := fmt.Sprintf("/api/suppliers/channels/%d", id)
	statusPath := fmt.Sprintf("/api/channel/%d/status", id)
	request(http.MethodPost, statusPath, adminToken, "", gin.H{"status": common.ChannelStatusEnabled})
	channel, err := model.GetChannelById(id, false)
	require.NoError(t, err)
	assert.NotEqual(t, common.ChannelStatusEnabled, channel.Status)
	require.NoError(t, model.DB.Model(channel).UpdateColumn("tag", "supplier-draft-test").Error)
	require.NoError(t, model.EnableChannelByTag("supplier-draft-test"))
	_, _, err = model.FixAbility()
	require.NoError(t, err)
	var draftAbilities int64
	require.NoError(t, model.DB.Model(&model.Ability{}).Where("channel_id = ?", id).Count(&draftAbilities).Error)
	assert.Zero(t, draftAbilities)
	ok(http.MethodPost, fmt.Sprintf("/api/supplier/channels/%d/test", id), token, "", gin.H{"version": version})
	require.Eventually(t, func() bool {
		var draft model.SupplierChannelDraft
		return model.DB.Select("status").First(&draft, "channel_id = ?", id).Error == nil && draft.Status == model.SupplierDraftReady
	}, 12*time.Second, 25*time.Millisecond, "successful probe must enter review")
	candidates, err := model.GetSatisfiedChannelCandidates("default", "gpt-4o-mini", nil)
	require.NoError(t, err)
	assert.Empty(t, candidates, "a test must not activate a channel")
	review := ok(http.MethodGet, "/api/channel/?supplier_review=true", adminToken, "", nil)
	assert.EqualValues(t, 1, review["total"])
	detail := ok(http.MethodGet, detailPath, adminToken, "", nil)
	revision, _ := detail["publication_revision"].(string)
	require.NotEmpty(t, revision)
	publication := service.SupplierChannelPublishContext{ChannelID: id, Version: version, Revision: revision, Models: []string{"gpt-4o-mini"}, Groups: []string{"default"}, Weight: 9}
	proof := verify(adminToken, "enrollment-password", service.VerificationScopeSupplierChannelPublish, publication)
	publish := gin.H{"version": version, "revision": revision, "models": publication.Models, "groups": publication.Groups, "priority": publication.Priority, "weight": publication.Weight}
	published := ok(http.MethodPost, detailPath+"/publish", adminToken, proof, publish)
	candidates, err = model.GetSatisfiedChannelCandidates("default", "gpt-4o-mini", nil)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, id, candidates[0].Id)
	assert.EqualValues(t, 9, candidates[0].GetWeight())
	_, replay := request(http.MethodPost, detailPath+"/publish", adminToken, proof, publish)
	assert.Equal(t, "SECURITY_PROOF_CONSUMED", replay.Code)
	routing := gin.H{"revision": published["publication_revision"], "groups": []string{"vip"}, "priority": 0, "weight": 11}
	ok(http.MethodPut, detailPath+"/routing", adminToken, "", routing)
	candidates, err = model.GetSatisfiedChannelCandidates("default", "gpt-4o-mini", nil)
	require.NoError(t, err)
	assert.Empty(t, candidates)
	candidates, err = model.GetSatisfiedChannelCandidates("vip", "gpt-4o-mini", nil)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.EqualValues(t, 11, candidates[0].GetWeight())
	request(http.MethodPost, statusPath, adminToken, "", gin.H{"status": common.ChannelStatusManuallyDisabled})
	candidates, err = model.GetSatisfiedChannelCandidates("vip", "gpt-4o-mini", nil)
	require.NoError(t, err)
	assert.Empty(t, candidates)
}

func TestChannelRPMListMatchesAdmissionWindow(t *testing.T) {
	for _, dialect := range []struct{ name, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.name, func(t *testing.T) {
			dsn := os.Getenv(dialect.env)
			if dialect.env != "" && dsn == "" {
				t.Skipf("%s is not configured", dialect.env)
			}
			db := modelManagementDB(t, dialect.name, dsn)
			channel := model.Channel{Name: "RPM list sample", Type: constant.ChannelTypeOpenAI}
			require.NoError(t, db.Create(&channel).Error)
			now := time.Now()
			require.NoError(t, model.EnsureChannelLimitTracking(channel.Id, true, now))
			_, err := model.UpdateChannelLimits(channel.Id, 1, model.ChannelLimitSettings{RPM: 3, ReconcileSeconds: 300}, false, now)
			require.NoError(t, err)
			for _, attempt := range []model.ChannelLimitAttempt{
				{ID: "sent", SentAtMS: now.UnixMilli(), Completed: true},
				{ID: "reserved", CreatedAtMS: now.UnixMilli()},
				{ID: "released", CreatedAtMS: now.UnixMilli(), Released: true},
				{ID: "expired", SentAtMS: now.Add(-time.Minute).UnixMilli(), Completed: true},
			} {
				attempt.ChannelID = channel.Id
				require.NoError(t, db.Create(&attempt).Error)
			}
			page, err := channelRPMForPage(t.Context(), []int{channel.Id}, now)
			require.NoError(t, err)
			require.NotNil(t, page[channel.Id])
			assert.Equal(t, channelRPMUsage{Limit: common.GetPointer(int64(3)), Used: 1, Reserved: 1, Observed: true, Supported: true}, *page[channel.Id])
		})
	}
}

func TestSupplierAsyncFactsRemainOwnerScoped(t *testing.T) {
	_, _ = setupSecurityEnrollmentTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.SupplierAccount{}, &model.OverviewAttemptEvent{}, &model.OverviewCollectionState{}))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(model.DB))
	now := time.Now().Unix()
	for i, event := range []model.OverviewAttemptEvent{
		{CallStage: "task_submit", Success: true, TaskStatus: "running", LatencyMS: 20},
		{CallStage: "task_submit", Success: true, TaskStatus: "failed", LatencyMS: 40, TaskDurationMS: common.GetPointer(int64(800)), Credits: common.GetPointer("1234567890.123456789"), CreditUsageSource: "upstream_usage"},
		{CallStage: "task_submit", Failure: true, UpstreamStatusCode: 429, LatencyMS: 30},
		{CallStage: "task_submit", TaskStatus: "unknown", LatencyMS: 50},
		{CallStage: "realtime_generation", Success: true, LatencyMS: 100, Credits: common.GetPointer("0.000000001")},
	} {
		event.ID, event.SupplierReference, event.AuthorizedSupplierID = fmt.Sprint("async-fact-", i), fmt.Sprint("local-", i), "owner-a"
		event.ChannelID, event.ModelName, event.StartedAt, event.FinishedAt = 1, "model-a", now, now
		require.NoError(t, model.DB.Create(&event).Error)
	}
	require.NoError(t, model.DB.Create(&model.OverviewAttemptEvent{ID: "foreign-task", SupplierReference: "foreign-reference", AuthorizedSupplierID: "owner-b", ChannelID: 2, CallStage: "task_submit", TaskStatus: "succeeded", Success: true, StartedAt: now, Credits: common.GetPointer("999")}).Error)
	response := supplierTestRequest(http.MethodGet, "/api/supplier/overview", "", "owner-a", 0, GetSupplierOverview)
	var overview struct {
		Success bool
		Data    supplierOverviewMetrics
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &overview))
	require.True(t, overview.Success, response.Body.String())
	assert.EqualValues(t, 5, overview.Data.RequestCount)
	assert.EqualValues(t, 1, overview.Data.SuccessCount, "accepted submissions are not successful generations")
	assert.Zero(t, overview.Data.FailureCount)
	assert.EqualValues(t, 1, overview.Data.RealtimeGenerationCount)
	require.NotNil(t, overview.Data.AverageLatency)
	assert.Equal(t, float64(100), *overview.Data.AverageLatency)
	assert.EqualValues(t, 4, overview.Data.Async.SubmittedCount)
	assert.EqualValues(t, 2, overview.Data.Async.AcceptedCount)
	assert.EqualValues(t, 1, overview.Data.Async.RejectedCount)
	assert.EqualValues(t, 1, overview.Data.Async.UnknownSubmissionCount)
	assert.EqualValues(t, 1, overview.Data.Async.RunningCount)
	assert.EqualValues(t, 1, overview.Data.Async.FailedCount)
	assert.EqualValues(t, 1, overview.Data.Async.UnknownCount)
	assert.Zero(t, overview.Data.Async.SucceededCount)
	require.NotNil(t, overview.Data.Credits)
	assert.Equal(t, "1234567890.12345679", *overview.Data.Credits)
	assert.EqualValues(t, 2, overview.Data.CreditUsageKnownCount)
	logs := supplierTestRequest(http.MethodGet, "/api/supplier/logs?task_status=failed", "", "owner-a", 0, GetSupplierLogs)
	var envelope struct {
		Success bool
		Data    struct {
			Items []supplierLogResponse
			Total int
		}
	}
	require.NoError(t, common.Unmarshal(logs.Body.Bytes(), &envelope))
	require.True(t, envelope.Success, logs.Body.String())
	require.Len(t, envelope.Data.Items, 1)
	assert.Equal(t, "success", envelope.Data.Items[0].Status)
	assert.Equal(t, "failed", envelope.Data.Items[0].TaskStatus)
	assert.Equal(t, "async_task", envelope.Data.Items[0].Protocol)
	exported := supplierTestRequest(http.MethodGet, "/api/supplier/logs/export?stage=task_submit", "", "owner-a", 0, ExportSupplierLogs)
	assert.Contains(t, exported.Body.String(), "task_completed_at")
	assert.Contains(t, exported.Body.String(), "1234567890.123456789")
	assert.NotContains(t, exported.Body.String(), "foreign-reference")
}

func TestSupplierPluginAuthorizationCannotExpandFromChannelType(t *testing.T) {
	account := model.SupplierAccount{AllowedTypes: "[1,61]", AllowedModels: "test"}
	assert.True(t, account.Allows(constant.ChannelTypeOpenAI, []string{"test"}))
	assert.False(t, account.Allows(constant.ChannelTypeTaskPlugin, []string{"test"}))
	assert.False(t, account.AllowsTemplate("plugin:kling", []string{"test"}))
	account.AllowedTemplates = `["plugin:kling"]`
	assert.True(t, account.AllowsTemplate("plugin:kling", []string{"test"}))
	assert.False(t, account.AllowsTemplate("plugin:sora", []string{"test"}))
	assert.False(t, account.AllowsTemplate("plugin:kling", []string{"foreign-model"}))
	assert.False(t, account.Allows(constant.ChannelTypeOpenAI, []string{"test"}))
}

func TestSupplierProvisioningWithDedicatedAccount(t *testing.T) {
	admin, identity := setupSecurityEnrollmentTest(t)
	db := model.DB
	require.NoError(t, db.Model(admin).Update("role", common.RoleRootUser).Error)
	require.NoError(t, model.PublishUserAuthCache(admin.Id))
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.SupplierAccount{}, &model.SystemTask{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(db))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(db))
	secureCookies := common.SessionCookieSecure
	common.SessionCookieSecure = true
	t.Cleanup(func() { common.SessionCookieSecure = secureCookies })

	const username, password = "supplier-login", "a dedicated passphrase"
	const commonPassword = "123456789012345"
	submit := func(scheme, id, proof string, fields map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		body, err := common.Marshal(fields)
		require.NoError(t, err)
		method := http.MethodPost
		if id != "" {
			method = http.MethodPut
		}
		return securityEnrollmentRequest(method, scheme+"://console.example/api/suppliers/"+id, string(body), proof, identity, func(c *gin.Context) {
			c.Set("role", common.RoleRootUser)
			c.Params = gin.Params{{Key: "id", Value: id}}
			SaveSupplierAccount(c)
		})
	}
	provisionProof := func(id string, revision int64, login string) string {
		t.Helper()
		payload, err := common.Marshal(service.SupplierProvisionContext{SupplierID: id, Version: revision, Username: login})
		require.NoError(t, err)
		return issueSecurityEnrollmentProof(t, identity, service.VerificationOperation{Scope: service.VerificationScopeSupplierProvision, Context: payload}, service.VerificationMethodPassword)
	}
	fields := map[string]any{"name": "Supplier Co", "username": username, "password": password}
	proof := provisionProof("", 0, username)
	insecure := submit("http", "", proof, fields)
	assert.Contains(t, insecure.Body.String(), "Supplier credentials require HTTPS")
	weak := submit("https", "", proof, map[string]any{"name": "Supplier Co", "username": username, "password": commonPassword})
	assert.Contains(t, weak.Body.String(), model.ErrSupplierPasswordTooCommon.Error())
	assert.Contains(t, submit("https", "", "", fields).Body.String(), "SECURITY_PROOF_REQUIRED")
	assert.Contains(t, submit("https", "", provisionProof("", 0, "wrong-login"), fields).Body.String(), "SECURITY_PROOF_CONTEXT_MISMATCH")

	response := submit("https", "", proof, fields)
	var result struct {
		Success bool
		Data    struct {
			ID     string `json:"id"`
			UserID int    `json:"user_id"`
		}
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Success, response.Body.String())
	require.NotEmpty(t, result.Data.ID)
	require.Positive(t, result.Data.UserID)
	assert.NotContains(t, response.Body.String(), password)
	assert.Contains(t, submit("https", "", proof, fields).Body.String(), "SECURITY_PROOF_CONSUMED")

	var user model.User
	require.NoError(t, db.First(&user, result.Data.UserID).Error)
	assert.Equal(t, username, user.Username)
	assert.Equal(t, common.RoleSupplierUser, user.Role)
	assert.True(t, user.MustChangePassword)
	assert.Equal(t, result.Data.ID, user.GetSupplierID())
	assert.True(t, common.ValidatePasswordAndHash(password, user.Password))
	supplierSession, err := service.CreateLoginSession(user.Id, "password", "127.0.0.1", "supplier-provisioning-test")
	require.NoError(t, err)
	supplierIdentity, err := service.ParseAccessToken(supplierSession.AccessToken)
	require.NoError(t, err)
	assert.ErrorIs(t, model.ChangeUserPassword(supplierIdentity, &model.User{Password: commonPassword, OriginalPassword: password}, false), model.ErrSupplierPasswordTooCommon)
	assert.ErrorIs(t, (&model.User{Id: user.Id, Password: commonPassword}).Edit(true), model.ErrSupplierPasswordTooCommon)
	require.NoError(t, db.Model(&user).Update("email", "supplier@example.com").Error)
	assert.ErrorIs(t, model.ResetUserPasswordByEmail("supplier@example.com", "an unapproved passphrase"), model.ErrEmailNotFound)

	account, err := model.GetSupplierAccount(result.Data.ID)
	require.NoError(t, err)
	assert.True(t, account.SupplyEnabled)
	assert.True(t, account.Allows(constant.ChannelTypeOpenAI, []string{"new-upstream-model"}))
	assert.False(t, account.Allows(constant.ChannelTypeTaskPlugin, []string{"new-upstream-model"}))

	catalog := supplierTestRequest(http.MethodGet, "/api/supplier/self", "", account.SupplierID, 0, GetSupplierSelf)
	var self struct {
		Success bool
		Data    struct {
			Templates []service.SupplierTemplate
		}
	}
	require.NoError(t, common.Unmarshal(catalog.Body.Bytes(), &self))
	require.True(t, self.Success, catalog.Body.String())
	require.True(t, slices.ContainsFunc(self.Data.Templates, func(template service.SupplierTemplate) bool {
		return template.Type == constant.ChannelTypeOpenAI && template.AllowsCustomModels
	}))

	duplicate := submit("https", "", provisionProof("", 0, username), fields)
	assert.Contains(t, duplicate.Body.String(), "Username already exists or has been deleted")
	for _, table := range []any{&model.UpstreamSupplier{}, &model.SupplierAccount{}} {
		var count int64
		require.NoError(t, db.Model(table).Count(&count).Error)
		assert.EqualValues(t, 1, count, "the duplicate account must roll back both supplier writes")
	}

	legacy := model.UpstreamSupplier{ID: "legacy", Name: "Existing"}
	require.NoError(t, db.Create(&legacy).Error)
	legacyAccount := model.SupplierAccount{
		SupplierID: legacy.ID, SupplyEnabled: true, Group: "old-default", MaxChannels: 10, PoolWeight: 1,
		AllowedTypes: "[1]", AllowedModels: "allowed",
	}
	require.NoError(t, model.SaveSupplierAccount(&legacyAccount, 0))
	channel := model.Channel{OwnerSupplierID: legacy.ID, Group: "admin-selected", Name: "already live"}
	require.NoError(t, db.Create(&channel).Error)
	legacyUpdate := submit("https", legacy.ID, provisionProof(legacy.ID, legacyAccount.Revision, "legacy-login"),
		map[string]any{"revision": legacyAccount.Revision, "username": "legacy-login", "password": password})
	require.Contains(t, legacyUpdate.Body.String(), `"success":true`)
	updatedAccount, err := model.GetSupplierAccount(legacy.ID)
	require.NoError(t, err)
	assert.True(t, updatedAccount.Allows(constant.ChannelTypeOpenAI, []string{"allowed"}))
	assert.False(t, updatedAccount.Allows(constant.ChannelTypeOpenAI, []string{"not-allowed"}))
	require.NoError(t, db.First(&channel, channel.Id).Error)
	assert.Equal(t, "admin-selected", channel.Group)
}

func TestSupplierAsyncValidationWaitsForActualCompletion(t *testing.T) {
	user, _ := setupSecurityEnrollmentTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.SupplierAccount{}, &model.SupplierChannelDraft{}, &model.Task{}, &model.OverviewAttemptEvent{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(model.DB))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(model.DB))
	t.Setenv("SUPPLIER_CREDENTIAL_KEYS", `{"v1":"`+base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))+`"}`)
	t.Setenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY", "v1")
	previousRatios := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"kling-v3":1}`))
	previousAdaptor := service.GetTaskAdaptorFunc
	service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor { return relay.GetTaskAdaptor(platform) }
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousRatios))
		service.GetTaskAdaptorFunc = previousAdaptor
	})
	account := model.SupplierAccount{SupplierID: "probe-owner", SupplyEnabled: true, AllowedTemplates: `["plugin:kling"]`, AllowedModels: "kling-v3", Group: "default", MaxChannels: 10, PoolWeight: 1, CostRatio: common.GetPointer(0.0)}
	require.NoError(t, model.DB.Create(&model.UpstreamSupplier{ID: account.SupplierID, Name: "Probe owner"}).Error)
	require.NoError(t, model.SaveSupplierAccount(&account, 0))
	secret, err := model.EncryptSupplierSecret(account.SupplierID, "probe-private-key")
	require.NoError(t, err)
	draft := model.SupplierChannelDraft{SupplierID: account.SupplierID, Type: constant.ChannelTypeTaskPlugin, TemplateID: "plugin:kling", Name: "async probe", BaseURL: "https://1.1.1.1", Secret: secret, Models: "kling-v3"}
	channel, err := model.SaveSupplierChannelDraft(&draft, user.Id, 0)
	require.NoError(t, err)
	require.NoError(t, model.EnsureChannelLimitTracking(channel.Id, true, time.Now()))
	require.NoError(t, model.DB.Model(channel).Update("supplier_paused", true).Error)
	require.NoError(t, model.DB.Model(&draft).Update("status", "pending").Error)
	client := service.GetSupplierHTTPClient()
	transport := client.Transport
	t.Cleanup(func() { client.Transport = transport })
	submits, polls, completed := 0, 0, false
	client.Transport = schedulingTokenTransport(func(request *http.Request) (*http.Response, error) {
		body := `{"code":0,"data":{"task_id":"vendor-probe-id","task_status":"submitted"}}`
		if request.Method == http.MethodPost {
			submits++
		} else {
			polls++
			if completed {
				body = `{"code":0,"data":{"task_id":"vendor-probe-id","task_status":"succeed","task_result":{"videos":[{"url":"https://cdn.example/test.mp4","duration":"5"}]},"final_unit_deduction":"1"}}`
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	staleDraft := draft
	for iteration := range 2 {
		validateSupplierChannelDraft(channel.Id, account.SupplierID, draft.Version, user.Id)
		require.NoError(t, model.DB.First(&draft, "channel_id = ?", channel.Id).Error)
		assert.Equal(t, "waiting", draft.ValidationStatus())
		stored, err := model.GetChannelById(channel.Id, false)
		require.NoError(t, err)
		assert.False(t, stored.SupplierValidated, "acceptance does not prove generation capability")
		assert.True(t, stored.SupplierPaused)
		if iteration == 0 {
			// A worker can hold an old empty probe map when its lease expires.
			// Reclaiming the same version must not submit the paid task twice.
			require.NoError(t, model.DB.Model(&draft).Updates(map[string]any{"status": model.SupplierDraftTesting, "test_token": "replacement-worker", "lease_until": time.Now().Add(time.Minute).Unix()}).Error)
			candidate := *channel
			candidate.Key, candidate.BaseURL, candidate.Models = "probe-private-key", &draft.BaseURL, draft.Models
			_ = supplierTaskProbe(context.Background(), &staleDraft, &candidate, user.Id, "kling-v3", 0)
			assert.Equal(t, 1, submits)
			require.NoError(t, model.DB.Model(&draft).Update("status", model.SupplierDraftWaiting).Error)
		}
	}
	assert.Equal(t, 1, submits, "repeated validation resumes the accepted task")
	assert.Positive(t, polls)
	completed = true
	validateSupplierChannelDraft(channel.Id, account.SupplierID, draft.Version, user.Id)
	require.NoError(t, model.DB.First(&draft, "channel_id = ?", channel.Id).Error)
	assert.Equal(t, model.SupplierDraftReady, draft.Status)
	assert.Equal(t, "kling-v3", draft.TestedModels)
	stored, err := model.GetChannelById(channel.Id, false)
	require.NoError(t, err)
	assert.False(t, stored.SupplierValidated, "a passed test must leave the draft unpublished")
	assert.True(t, stored.SupplierPaused, "validation cannot clear a supplier pause")
	assert.Equal(t, 1, submits)
	var task model.Task
	require.NoError(t, model.DB.Where("channel_id = ?", channel.Id).First(&task).Error)
	assert.Zero(t, task.Quota)
	require.NotNil(t, task.PrivateData.ChannelAttempt)
	assert.True(t, task.PrivateData.ChannelAttempt.Encrypted)
	assert.NotContains(t, task.PrivateData.ChannelAttempt.Connection, "probe-private-key")
	assert.Empty(t, task.PrivateData.Key)
	var testUser model.User
	require.NoError(t, model.DB.First(&testUser, user.Id).Error)
	assert.Equal(t, user.Quota, testUser.Quota, "controlled probes never charge the supplier's consumer wallet")
}

func TestSupplierPendingConnectionSurvivesMetadataEdits(t *testing.T) {
	user, _ := setupSecurityEnrollmentTest(t)
	previousSecure := common.SessionCookieSecure
	common.SessionCookieSecure = true
	t.Cleanup(func() { common.SessionCookieSecure = previousSecure })
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.SupplierAccount{}, &model.SupplierChannelDraft{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(model.DB))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(model.DB))
	t.Setenv("SUPPLIER_CREDENTIAL_KEYS", `{"v1":"`+base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))+`"}`)
	t.Setenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY", "v1")
	account := model.SupplierAccount{SupplierID: "pending-owner", SupplyEnabled: true, AllowedTypes: "[1]", AllowedModels: "model-a", Group: "default", MaxChannels: 10, PoolWeight: 1}
	require.NoError(t, model.DB.Create(&model.UpstreamSupplier{ID: account.SupplierID, Name: "Pending owner"}).Error)
	require.NoError(t, model.SaveSupplierAccount(&account, 0))
	channel := model.Channel{OwnerSupplierID: account.SupplierID, Name: "live", Type: 1, Key: "live-private-key", BaseURL: common.GetPointer("https://1.1.1.1/live"), Models: "model-a", SupplierValidated: true, SupplierVersion: 1, Status: common.ChannelStatusEnabled, Group: "default"}
	require.NoError(t, channel.Insert())
	require.NoError(t, model.EnsureChannelLimitTracking(channel.Id, true, time.Now()))
	secret, err := model.EncryptSupplierSecret(account.SupplierID, "pending-private-key")
	require.NoError(t, err)
	draft := model.SupplierChannelDraft{ChannelID: channel.Id, SupplierID: account.SupplierID, Type: 1, TemplateID: "1", BaseURL: "https://1.1.1.1/pending", Models: "model-a", Name: "pending", Secret: secret, ProbeTaskIDs: `{"model-a:0":"existing-paid-probe"}`}
	updated, err := model.SaveSupplierChannelDraft(&draft, user.Id, 1)
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&draft).Update("status", "waiting").Error)
	draft.Status = "waiting"
	view := supplierChannelDTO(updated, &draft, "Pending owner")
	assert.Equal(t, draft.BaseURL, view.BaseURL)
	assert.True(t, view.HasValidatedConfig)
	body := fmt.Sprintf(`{"name":"renamed","template_id":"1","base_url":%q,"models":["model-a"],"version":%d}`, draft.BaseURL, updated.SupplierVersion)
	response := supplierTestRequest(http.MethodPut, "/channel", body, account.SupplierID, channel.Id, func(c *gin.Context) { c.Request.TLS = &tls.ConnectionState{}; SaveSupplierChannel(c) })
	assert.Contains(t, response.Body.String(), `"success":true`)
	assert.NotContains(t, response.Body.String(), "private-key")
	require.NoError(t, model.DB.First(&draft, "channel_id = ?", channel.Id).Error)
	assert.Equal(t, "waiting", draft.ValidationStatus())
	assert.EqualValues(t, 3, draft.Version)
	assert.Equal(t, `{"model-a:0":"existing-paid-probe"}`, draft.ProbeTaskIDs)
	decrypted, err := model.DecryptSupplierSecret(account.SupplierID, draft.Secret)
	require.NoError(t, err)
	assert.Equal(t, "pending-private-key", decrypted)
	live, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "live-private-key", live.Key)
	assert.Equal(t, "https://1.1.1.1/live", live.GetBaseURL())
	assert.Equal(t, "renamed", draft.Name)
	assert.Equal(t, "live", live.Name)
}

func TestSupplierConnectionChangesRequireValidatedEditor(t *testing.T) {
	_, _ = setupSecurityEnrollmentTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(model.DB))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(model.DB))
	t.Setenv("SUPPLIER_CREDENTIAL_KEYS", `{"v1":"`+base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))+`"}`)
	t.Setenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY", "v1")
	require.NoError(t, model.DB.Create(&model.UpstreamSupplier{ID: "supplier-a", Name: "A"}).Error)
	setting, header, mapping := `{"force_format":true}`, `{"X-Provider":"old"}`, `{"test-model":"old-model"}`
	channel := model.Channel{Name: "validated", Type: 1, OwnerSupplierID: "supplier-a", Key: "secret-one\nsecret-two", Models: "test-model", Group: "default", Tag: common.GetPointer("supplier-tag"), SupplierValidated: true, SupplierVersion: 1, Setting: &setting, HeaderOverride: &header, ModelMapping: &mapping, Other: "old", OtherSettings: `{"old":true}`, ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2}}
	require.NoError(t, channel.Insert())
	for _, change := range []string{`"settings":"{}"`, `"other":""`, `"setting":"{}"`, `"header_override":"{}"`, `"model_mapping":"{}"`, `"models":"another-model"`} {
		body := fmt.Sprintf(`{"id":%d,"type":1,"models":"test-model",%s}`, channel.Id, change)
		response := supplierTestRequest("PUT", "/api/channel/", body, "", 0, func(c *gin.Context) {
			c.Set("role", common.RoleRootUser)
			UpdateChannel(c)
		})
		assert.Contains(t, response.Body.String(), "Use the supplier channel editor", change)
	}
	for _, action := range []string{"delete_key", "delete_disabled_keys"} {
		response := supplierTestRequest("POST", "/api/channel/multi_key/manage", fmt.Sprintf(`{"channel_id":%d,"action":%q,"key_index":0}`, channel.Id, action), "", 0, func(c *gin.Context) {
			c.Set("role", common.RoleRootUser)
			ManageMultiKeys(c)
		})
		assert.Contains(t, response.Body.String(), "Use the supplier channel editor")
	}
	stored, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, channel.Key, stored.Key)
	assert.Equal(t, channel.OtherSettings, stored.OtherSettings)
	assert.Equal(t, channel.Setting, stored.Setting)
	assert.Equal(t, channel.HeaderOverride, stored.HeaderOverride)
	assert.Equal(t, channel.ModelMapping, stored.ModelMapping)
	assert.EqualValues(t, 1, stored.SupplierVersion)
	assert.ErrorIs(t, model.EditChannelByTag("supplier-tag", nil, nil, nil, nil, nil, nil, nil, common.GetPointer("{}")), model.ErrSupplierChannelOperation)
	_, err = fetchChannelUpstreamModelIDs(stored)
	assert.ErrorIs(t, err, model.ErrSupplierChannelOperation)
	_, err = updateChannelBalance(stored)
	assert.ErrorIs(t, err, model.ErrSupplierChannelOperation)
	_, err = GetResponseBody(http.MethodGet, "https://1.1.1.1", stored, nil)
	assert.ErrorIs(t, err, model.ErrSupplierChannelOperation)
	_, err = fetchInferenceStatus(context.Background(), stored)
	assert.ErrorIs(t, err, model.ErrSupplierChannelOperation)
	_, _, err = service.RefreshCodexChannelCredential(context.Background(), stored.Id, service.CodexCredentialRefreshOptions{})
	assert.ErrorIs(t, err, model.ErrSupplierChannelOperation)
	for _, handler := range []gin.HandlerFunc{OllamaPullModel, OllamaPullModelStream, OllamaDeleteModel, OllamaVersion, GetCodexChannelUsage} {
		response := supplierTestRequest("POST", "/utility", fmt.Sprintf(`{"channel_id":%d,"model_name":"test-model"}`, stored.Id), "", stored.Id, handler)
		assert.Contains(t, response.Body.String(), model.ErrSupplierChannelOperation.Error())
		assert.NotContains(t, response.Body.String(), "secret-one")
	}
	assert.ErrorIs(t, updateChannelUpstreamModelSettings(stored, stored.GetOtherSettings(), true), model.ErrSupplierChannelOperation)
	assert.ErrorIs(t, (&model.Channel{Id: channel.Id}).Delete(), model.ErrSupplierChannelOperation)
	_, err = model.BatchDeleteChannels([]int{channel.Id})
	assert.ErrorIs(t, err, model.ErrSupplierChannelOperation)
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("status", common.ChannelStatusManuallyDisabled).Error)
	require.NoError(t, model.DB.Create(&model.Channel{Name: "disabled platform channel", Status: common.ChannelStatusManuallyDisabled}).Error)
	deleted, err := model.DeleteDisabledChannel()
	require.NoError(t, err)
	assert.EqualValues(t, 1, deleted, "disabled supplier channels must not block platform-channel cleanup")
	_, err = model.DeleteChannelByStatus(common.ChannelStatusManuallyDisabled)
	assert.ErrorIs(t, err, model.ErrSupplierChannelOperation)
	stored, err = model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, header, *stored.HeaderOverride)
	assert.Equal(t, channel.Key, stored.Key)
}

func TestSupplierDraftValidationCanProbeNewPausedChannel(t *testing.T) {
	user, _ := setupSecurityEnrollmentTest(t)
	previousRatios := ratio_setting.ModelRatio2JSONString()
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousRatios)) })
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-4o-mini":1}`))
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.SupplierAccount{}, &model.SupplierChannelDraft{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(model.DB))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(model.DB))
	t.Setenv("SUPPLIER_CREDENTIAL_KEYS", `{"v1":"`+base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))+`"}`)
	t.Setenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY", "v1")
	require.NoError(t, model.DB.Create(&model.UpstreamSupplier{ID: "supplier-a", Name: "A"}).Error)
	account := model.SupplierAccount{SupplierID: "supplier-a", SupplyEnabled: true, AllowedTypes: "[1]", AllowedModels: "gpt-4o-mini", Group: "default", MaxChannels: 10, PoolWeight: 1}
	require.NoError(t, model.SaveSupplierAccount(&account, 0))
	secret, err := model.EncryptSupplierSecret(account.SupplierID, "draft-secret")
	require.NoError(t, err)
	draft := model.SupplierChannelDraft{SupplierID: account.SupplierID, Type: 1, Name: "new", BaseURL: "https://1.1.1.1/v1", Secret: secret, Models: "gpt-4o-mini"}
	channel, err := model.SaveSupplierChannelDraft(&draft, user.Id, 0)
	require.NoError(t, err)
	require.NoError(t, model.EnsureChannelLimitTracking(channel.Id, true, time.Now()))
	require.NoError(t, model.DB.Model(channel).Update("supplier_paused", true).Error)
	require.NoError(t, model.DB.Model(&draft).Update("status", "pending").Error)
	client := service.GetSupplierHTTPClient()
	previousTransport := client.Transport
	t.Cleanup(func() { client.Transport = previousTransport })
	sent := 0
	client.Transport = schedulingTokenTransport(func(request *http.Request) (*http.Response, error) {
		sent++
		assert.Equal(t, "Bearer draft-secret", request.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"invalid key draft-secret","type":"invalid_request_error","code":"invalid_api_key"}}`))}, nil
	})
	validateSupplierChannelDraft(channel.Id, account.SupplierID, draft.Version, user.Id)
	assert.Equal(t, 1, sent, "the draft probe must reach its upstream without enabling the live channel")
	stored, err := model.GetChannelById(channel.Id, false)
	require.NoError(t, err)
	assert.False(t, stored.SupplierValidated)
	assert.True(t, stored.SupplierPaused)
	require.NoError(t, model.DB.First(&draft, "channel_id = ?", channel.Id).Error)
	assert.Equal(t, "failed", draft.Status)
	// A published version still schedules a real diagnostic probe.
	draft.Status, draft.TestedModels = model.SupplierDraftReady, draft.Models
	require.NoError(t, model.DB.Save(&draft).Error)
	routing := model.SupplierChannelRouting{Groups: []string{"default"}}
	require.NoError(t, model.PublishSupplierChannelDraft(channel.Id, account.SupplierID, draft.Version,
		model.SupplierChannelRevision(stored, &draft), []string{"gpt-4o-mini"}, routing))
	status, err := model.QueueSupplierChannelValidation(channel.Id, account.SupplierID, draft.Version)
	require.NoError(t, err)
	assert.Equal(t, "pending", status)
	validateSupplierChannelDraft(channel.Id, account.SupplierID, draft.Version, user.Id)
	assert.Equal(t, 2, sent)
	_, err = model.QueueSupplierChannelValidation(channel.Id, account.SupplierID, draft.Version+1)
	assert.Error(t, err)
	stored, err = model.GetChannelById(channel.Id, false)
	require.NoError(t, err)
	assert.True(t, stored.SupplierValidated, "a failed re-test must retain the working version")
	assert.True(t, stored.SupplierPaused)
	require.NoError(t, model.DB.First(&draft, "channel_id = ?", channel.Id).Error)
	assert.True(t, draft.TestOnly)
	assert.Equal(t, "failed", draft.Status)

	t.Run("image model validation uses its image endpoint", func(t *testing.T) {
		previousPrices := ratio_setting.ModelPrice2JSONString()
		t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(previousPrices)) })
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"dall-e-3":0.04}`))
		account.AllowedModels = "gpt-4o-mini,dall-e-3"
		require.NoError(t, model.SaveSupplierAccount(&account, account.Revision))
		imageDraft := model.SupplierChannelDraft{SupplierID: account.SupplierID, Type: 1, Name: "image", BaseURL: "https://1.1.1.1", Secret: secret, Models: "dall-e-3"}
		imageChannel, err := model.SaveSupplierChannelDraft(&imageDraft, user.Id, 0)
		require.NoError(t, err)
		require.NoError(t, model.EnsureChannelLimitTracking(imageChannel.Id, true, time.Now()))
		require.NoError(t, model.DB.Model(&imageDraft).Update("status", "pending").Error)
		previousTransport := client.Transport
		t.Cleanup(func() { client.Transport = previousTransport })
		imageSent := false
		client.Transport = schedulingTokenTransport(func(request *http.Request) (*http.Response, error) {
			imageSent = true
			assert.Equal(t, "/v1/images/generations", request.URL.Path)
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"invalid key"}}`))}, nil
		})
		validateSupplierChannelDraft(imageChannel.Id, account.SupplierID, imageDraft.Version, user.Id)
		assert.True(t, imageSent)
	})

	probe := *channel
	probe.SupplierValidated, probe.SupplierPaused, probe.Models, probe.Key, probe.BaseURL = true, false, draft.Models, "draft-secret", &draft.BaseURL
	result := testChannel(nil, &probe, user.Id, draft.Models, "", false)
	require.Error(t, result.localErr)
	assert.Equal(t, "Channel test failed", result.localErr.Error())
	require.NotNil(t, result.newAPIError)
	assert.NotContains(t, result.newAPIError.Error(), "draft-secret")
	assert.Equal(t, http.StatusUnauthorized, result.newAPIError.StatusCode)
}

func TestSupplierIsolationReadModelsAndLimitUpdates(t *testing.T) {
	previousSecure := common.SessionCookieSecure
	common.SessionCookieSecure = true
	t.Cleanup(func() { common.SessionCookieSecure = previousSecure })
	_, _ = setupSecurityEnrollmentTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.SupplierAccount{}, &model.SupplierChannelDraft{}, &model.OverviewAttemptEvent{}, &model.OverviewCollectionState{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(model.DB))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(model.DB))
	t.Setenv("SUPPLIER_CREDENTIAL_KEYS", `{"v1":"`+base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))+`"}`)
	t.Setenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY", "v1")
	now := time.Now()
	channels := make([]model.Channel, 2)
	for i, owner := range []string{"supplier-a", "supplier-b"} {
		require.NoError(t, model.DB.Create(&model.UpstreamSupplier{ID: owner, Name: owner}).Error)
		account := model.SupplierAccount{SupplierID: owner, SupplyEnabled: true, AllowedTypes: "[1]", AllowedModels: "test-model", Group: "default", MaxChannels: 10, PoolWeight: 1}
		require.NoError(t, model.SaveSupplierAccount(&account, 0))
		channels[i] = model.Channel{Name: owner + "-channel", Type: 1, OwnerSupplierID: owner, Key: "never-return-upstream-secret", Models: "test-model", Group: "default", SupplierValidated: true, SupplierVersion: 1, Status: common.ChannelStatusEnabled}
		require.NoError(t, channels[i].Insert())
		require.NoError(t, model.EnsureChannelLimitTracking(channels[i].Id, true, now))
		require.NoError(t, model.DB.Create(&model.OverviewAttemptEvent{ID: fmt.Sprintf("request:%d", i), RequestID: "global-private-request-id", AuthorizedSupplierID: owner, SupplierReference: owner + "-reference", ChannelID: channels[i].Id, ChannelName: channels[i].Name, StartedAt: now.Unix(), FinishedAt: now.Unix(), ModelName: "test-model", Success: true, InputTokens: 10, UsageSource: "upstream", RevenueQuota: 999}).Error)
	}
	// A procurement label or a channel's current identity does not grant access
	// to historical facts that lack the immutable authorized-owner snapshot.
	require.NoError(t, model.DB.Create(&model.OverviewAttemptEvent{ID: "legacy", SupplierID: "supplier-a", ChannelID: channels[0].Id, StartedAt: now.Unix(), ModelName: "legacy-private"}).Error)
	for _, test := range []struct {
		name, path string
		handler    gin.HandlerFunc
	}{
		{"list", "/api/supplier/channels", ListSupplierChannels},
		{"logs", "/api/supplier/logs", GetSupplierLogs},
		{"export", "/api/supplier/logs/export", ExportSupplierLogs},
		{"overview", "/api/supplier/overview", GetSupplierOverview},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := supplierTestRequest("GET", test.path, "", "supplier-a", 0, test.handler)
			body := response.Body.String()
			assert.Equal(t, 200, response.Code)
			assert.NotContains(t, body, "supplier-b")
			assert.NotContains(t, body, "never-return")
			assert.NotContains(t, body, "global-private-request-id")
			assert.NotContains(t, body, "legacy-private")
			assert.NotContains(t, body, "revenue_quota")
			if test.name != "export" {
				assert.Contains(t, body, `"success":true`)
			}
		})
	}
	t.Run("supplier scoped request counts and error categories", func(t *testing.T) {
		bucket := now.Add(-time.Hour).Truncate(time.Hour).Unix()
		require.NoError(t, model.DB.Model(&model.OverviewAttemptEvent{}).Where("id = ?", "request:0").Update("started_at", bucket).Error)
		cases := []struct {
			reference, requestID, category string
			status                         int
			failure                        bool
		}{
			{"request-timeout", "global-private-request-id", "timeout", 408, true},
			{"gateway-timeout", "private-timeout-id", "timeout", 504, true},
			{"rate-limit", "private-rate-id", "rate_limited", 429, true},
			{"server-error", "private-retry-id", "upstream_error", 500, true},
			{"server-retry", "private-retry-id", "upstream_error", 503, true},
			{"bad-request", "private-client-id", "request_error", 400, true},
			{"unknown-failure", "private-unknown-id", "unknown", 0, true},
			{"unknown-outcome", "", "unknown", 0, false},
			{"unfinished-response", "private-unfinished-id", "unknown", 200, false},
			{"authentication-error", "private-auth-id", "auth_error", 401, true},
			{"authorization-error", "private-authorization-id", "auth_error", 403, true},
		}
		for i, tc := range cases {
			event := model.OverviewAttemptEvent{ID: "category:" + tc.reference, RequestID: tc.requestID, AuthorizedSupplierID: "supplier-a", SupplierReference: tc.reference,
				ChannelID: channels[0].Id, ChannelName: channels[0].Name, StartedAt: bucket + int64(i) + 1, ModelName: "test-model", Failure: tc.failure, UpstreamStatusCode: tc.status, LatencyMS: 3600000}
			require.NoError(t, model.DB.Create(&event).Error)
		}
		require.NoError(t, model.DB.Model(&model.OverviewAttemptEvent{}).Where("id = ?", "request:0").Updates(map[string]any{"image_count": 2, "image_usage_source": "upstream_payload", "call_stage": "image", "upstream_request_id": "vendor-rid-a"}).Error)
		require.NoError(t, model.DB.Model(&model.OverviewAttemptEvent{}).Where("id = ?", "request:1").Update("upstream_request_id", "vendor-rid-b").Error)
		require.NoError(t, model.DB.Model(&model.OverviewAttemptEvent{}).Where("id = ?", "category:server-error").Updates(map[string]any{"image_count": 3, "image_usage_source": "upstream_usage", "call_stage": "image"}).Error)
		require.NoError(t, model.DB.Model(&model.OverviewAttemptEvent{}).Where("id = ?", "category:bad-request").Updates(map[string]any{"audio_seconds": 1.25, "audio_usage_source": "upstream_usage", "call_stage": "audio"}).Error)
		require.NoError(t, model.DB.Model(&model.OverviewAttemptEvent{}).Where("id = ?", "category:unfinished-response").Updates(map[string]any{"audio_seconds": 2.5, "audio_usage_source": "media_metadata", "call_stage": "audio"}).Error)
		require.NoError(t, model.DB.Create(&model.OverviewAttemptEvent{ID: "foreign-category", RequestID: "private-foreign-id", AuthorizedSupplierID: "supplier-b", SupplierReference: "supplier-b-private-reference", ChannelID: channels[1].Id, StartedAt: now.Unix(), UpstreamStatusCode: 504, Failure: true}).Error)
		response := supplierTestRequest("GET", "/api/supplier/overview", "", "supplier-a", 0, GetSupplierOverview)
		var overview struct {
			Success bool
			Data    struct {
				supplierOverviewMetrics
				Channels []supplierOverviewMetrics `json:"channels"`
				Trend    []supplierOverviewMetrics `json:"trend"`
			}
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &overview))
		require.True(t, overview.Success, response.Body.String())
		require.Len(t, overview.Data.Channels, 1)
		require.Len(t, overview.Data.Trend, 1)
		for _, metrics := range []supplierOverviewMetrics{overview.Data.supplierOverviewMetrics, overview.Data.Channels[0], overview.Data.Trend[0]} {
			assert.EqualValues(t, 12, metrics.RequestCount)
			assert.EqualValues(t, 9, metrics.LogicalRequestCount, "retries collapse only inside this supplier's filtered facts; absent IDs remain unknown")
			require.NotNil(t, metrics.LastCalledAt)
			assert.Equal(t, bucket+11, *metrics.LastCalledAt, "another supplier's later call must not change this timestamp")
			assert.EqualValues(t, 2, metrics.TimeoutCount)
			assert.EqualValues(t, 1, metrics.RateLimitedCount)
			assert.EqualValues(t, 2, metrics.AuthErrorCount)
			assert.EqualValues(t, 2, metrics.UpstreamErrorCount)
			assert.EqualValues(t, 1, metrics.RequestErrorCount)
			assert.EqualValues(t, 1, metrics.UnknownErrorCount, "in-flight and unknown outcomes are not counted as failures")
			assert.EqualValues(t, 2, metrics.UnknownCount, "long latency and missing terminal outcome are not known timeouts")
			require.NotNil(t, metrics.ImageCount)
			assert.EqualValues(t, 5, *metrics.ImageCount)
			require.NotNil(t, metrics.AudioSeconds)
			assert.Equal(t, 3.75, *metrics.AudioSeconds)
			assert.EqualValues(t, 2, metrics.ImageUsageKnownCount)
			assert.EqualValues(t, 2, metrics.AudioUsageKnownCount)
		}
		assert.NotContains(t, response.Body.String(), "private-")
		response = supplierTestRequest("GET", "/api/supplier/logs?page_size=100", "", "supplier-a", 0, GetSupplierLogs)
		var logs struct {
			Success bool
			Data    struct {
				Items []supplierLogResponse
				Total int
			}
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &logs))
		require.True(t, logs.Success, response.Body.String())
		assert.Equal(t, 12, logs.Data.Total)
		categories := map[string]string{}
		for _, row := range logs.Data.Items {
			categories[row.Reference] = row.ErrorCategory
			switch row.Reference {
			case "supplier-a-reference":
				require.NotNil(t, row.ImageCount)
				assert.EqualValues(t, 2, *row.ImageCount)
				assert.Equal(t, "upstream_payload", row.ImageUsageSource)
				assert.Equal(t, "vendor-rid-a", row.UpstreamRequestID)
				assert.NotEqual(t, row.Reference, row.UpstreamRequestID, "vendor and supplier-local identifiers are independent")
			case "unfinished-response":
				require.NotNil(t, row.AudioSeconds)
				assert.Equal(t, 2.5, *row.AudioSeconds)
				assert.Equal(t, "media_metadata", row.AudioUsageSource)
			case "unknown-outcome":
				assert.Nil(t, row.ImageCount)
				assert.Nil(t, row.AudioSeconds)
				assert.Empty(t, row.UpstreamRequestID, "missing vendor identity must not be replaced by a platform request ID")
			}
		}
		assert.Equal(t, "none", categories["supplier-a-reference"])
		for _, tc := range cases {
			assert.Equal(t, tc.category, categories[tc.reference])
		}
		assert.NotContains(t, response.Body.String(), "private-")
		assert.NotContains(t, response.Body.String(), "global-private-request-id")
		assert.NotContains(t, response.Body.String(), "vendor-rid-b")
		exported := supplierTestRequest("GET", "/api/supplier/logs/export", "", "supplier-a", 0, ExportSupplierLogs)
		assert.Contains(t, exported.Body.String(), "error_category")
		assert.Contains(t, exported.Body.String(), ",rate_limited,")
		assert.NotContains(t, exported.Body.String(), "private-")
		assert.Contains(t, exported.Body.String(), "upstream_request_id")
		assert.Contains(t, exported.Body.String(), "vendor-rid-a")
		assert.NotContains(t, exported.Body.String(), "vendor-rid-b")
		empty := supplierTestRequest("GET", "/api/supplier/overview?model=no-activity", "", "supplier-a", 0, GetSupplierOverview)
		require.NoError(t, common.Unmarshal(empty.Body.Bytes(), &overview))
		require.True(t, overview.Success)
		assert.Zero(t, overview.Data.LogicalRequestCount)
		assert.Nil(t, overview.Data.LastCalledAt)
		assert.Nil(t, overview.Data.ImageCount)
		assert.Nil(t, overview.Data.AudioSeconds)
		assert.Zero(t, overview.Data.ImageUsageKnownCount)
		assert.Zero(t, overview.Data.AudioUsageKnownCount)
	})
	foreign := supplierTestRequest("GET", "/api/supplier/channels/2", "", "supplier-a", channels[1].Id, GetSupplierChannel)
	missing := supplierTestRequest("GET", "/api/supplier/channels/99999", "", "supplier-a", 99999, GetSupplierChannel)
	assert.JSONEq(t, missing.Body.String(), foreign.Body.String())
	viewer := supplierTestRequest("GET", "/api/channel/read-only/", "", "", 0, ListSupplierChannels)
	assert.Contains(t, viewer.Body.String(), "supplier-a")
	assert.Contains(t, viewer.Body.String(), "supplier-b")
	assert.NotContains(t, viewer.Body.String(), "never-return")
	saveLimits := func(id int, body string) *httptest.ResponseRecorder {
		return supplierTestRequest("PUT", "/limits", body, "", id, func(c *gin.Context) {
			c.Set("role", common.RoleRootUser)
			UpdateSupplierChannelLimits(c)
		})
	}
	for _, test := range []struct {
		body     string
		success  bool
		rpm, tpm int64
	}{
		{`{"revision":1,"rpm":5,"tpm":100}`, true, 5, 100},
		{`{"revision":2,"rpm":null}`, true, 0, 100},
		{`{"revision":3,"tpm":0}`, false, 0, 100},
		{`{"revision":3,"supplier_id":"supplier-b"}`, false, 0, 100},
		{`{"revision":2,"tpm":null}`, false, 0, 100},
	} {
		response := saveLimits(channels[0].Id, test.body)
		assert.Contains(t, response.Body.String(), fmt.Sprintf(`"success":%t`, test.success))
		state, err := model.GetChannelLimitState(channels[0].Id, now)
		require.NoError(t, err)
		assert.Equal(t, test.rpm, state.Config.RPM)
		assert.Equal(t, test.tpm, state.Config.TPM)
	}
	response := supplierTestRequest("PUT", "/limits", `{"revision":3,"rpm":1}`, "supplier-a", channels[0].Id, UpdateSupplierChannelLimits)
	assert.Contains(t, response.Body.String(), "Unauthorized, insufficient privileges")
	t.Run("legacy channel can enable RPM after its observation window warms", func(t *testing.T) {
		channel := &channels[1]
		require.NoError(t, model.DB.Delete(&model.ChannelLimitConfig{}, "channel_id = ?", channel.Id).Error)
		body := `{"revision":1,"rpm":12}`
		response := saveLimits(channel.Id, body)
		require.Contains(t, response.Body.String(), `"success":false`)
		config, err := model.GetChannelLimitConfig(channel.Id)
		require.NoError(t, err)
		require.Positive(t, config.ObservedSince, response.Body.String())
		assert.Zero(t, config.RPM)
		require.NoError(t, model.DB.Model(&config).Update("observed_since", time.Now().Add(-time.Minute).UnixMilli()).Error)
		response = saveLimits(channel.Id, body)
		require.Contains(t, response.Body.String(), `"success":true`)
		config, err = model.GetChannelLimitConfig(channel.Id)
		require.NoError(t, err)
		assert.EqualValues(t, 12, config.RPM)
		t.Run("limit edit preserves concurrent procurement settlement", func(t *testing.T) {
			if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
				t.Skip("SQLite serializes both writes on the database")
			}
			const id = "settled-during-limit-save"
			cost := int64(250)
			require.NoError(t, model.DispatchChannelAttempt(model.ChannelAttemptReservation{ID: id, ChannelID: channel.Id, CostBudgetNano: &cost}))
			// Commit the send's settlement on a second connection between the
			// editor's initial settings read and its admission-row lock.
			pending := true
			updates := model.DB.Callback().Update()
			const callback = "test:settle_before_supplier_limit_edit"
			require.NoError(t, updates.Before("gorm:update").Register(callback, func(tx *gorm.DB) {
				if pending && tx.Statement.Table == "channel_limit_configs" {
					pending = false
					tx.AddError(model.SettleChannelAttempt(model.ChannelAttemptSettlement{ID: id, ChannelID: channel.Id, Version: 1, CostUSDNano: &cost}, time.Time{}))
				}
			}))
			t.Cleanup(func() { require.NoError(t, updates.Remove(callback)) })
			response := saveLimits(channel.Id, `{"revision":2,"rpm":15}`)
			require.Contains(t, response.Body.String(), `"success":true`)
			require.False(t, pending)
			state, err := model.GetChannelLimitState(channel.Id, time.Time{})
			require.NoError(t, err)
			assert.Equal(t, cost, state.DailyUsedNano)
			assert.Zero(t, state.DailyHeldNano)
		})
	})
	t.Run("upload payload cannot change routing or channel limits", func(t *testing.T) {
		before, err := model.GetChannelById(channels[0].Id, true)
		require.NoError(t, err)
		for _, limits := range []string{`{"revision":3,"tpm":0}`, `{"revision":1,"tpm":200}`} {
			body := fmt.Sprintf(`{"name":"must-roll-back","template_id":"1","base_url":%q,"models":["test-model"],"remark":"must-roll-back","priority":10,"version":1,"limits":%s}`, before.GetBaseURL(), limits)
			response := supplierTestRequest("PUT", "/channel", body, "supplier-a", before.Id, func(c *gin.Context) {
				c.Request.TLS = &tls.ConnectionState{}
				SaveSupplierChannel(c)
			})
			assert.Contains(t, response.Body.String(), `"success":false`)
			after, err := model.GetChannelById(before.Id, true)
			require.NoError(t, err)
			assert.Equal(t, before.Name, after.Name)
			assert.Equal(t, before.SupplierVersion, after.SupplierVersion)
			assert.Equal(t, before.Key, after.Key)
			var drafts int64
			require.NoError(t, model.DB.Model(&model.SupplierChannelDraft{}).Where("channel_id = ?", before.Id).Count(&drafts).Error)
			assert.Zero(t, drafts)
			state, err := model.GetChannelLimitState(before.Id, now)
			require.NoError(t, err)
			assert.EqualValues(t, 3, state.Config.Revision)
			assert.EqualValues(t, 100, state.Config.TPM)
		}
	})

	t.Run("publishing cannot clear a pause or accept a stale test", func(t *testing.T) {
		secret, err := model.EncryptSupplierSecret("supplier-a", "replacement-secret")
		require.NoError(t, err)
		draft := model.SupplierChannelDraft{ChannelID: channels[0].Id, SupplierID: "supplier-a", Type: 1, Name: "revised", BaseURL: "https://example.com", Secret: secret, Models: "test-model"}
		updated, err := model.SaveSupplierChannelDraft(&draft, 1, 1)
		require.NoError(t, err)
		before, err := model.GetChannelById(updated.Id, true)
		require.NoError(t, err)
		assert.Equal(t, "never-return-upstream-secret", before.Key)
		routing := model.SupplierChannelRouting{Groups: []string{"default"}, Priority: 50, Weight: 2}
		models := []string{"test-model"}
		assert.Error(t, model.PublishSupplierChannelDraft(updated.Id, "supplier-a", draft.Version, model.SupplierChannelRevision(before, &draft), models, routing))
		draft.Status, draft.TestedModels = model.SupplierDraftReady, draft.Models
		require.NoError(t, model.DB.Save(&draft).Error)
		revision := model.SupplierChannelRevision(before, &draft)
		assert.Error(t, model.PublishSupplierChannelDraft(updated.Id, "supplier-a", 1, revision, models, routing))
		require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", updated.Id).Updates(map[string]any{"supplier_paused": true, "status": common.ChannelStatusManuallyDisabled}).Error)
		assert.Error(t, model.PublishSupplierChannelDraft(updated.Id, "supplier-a", updated.SupplierVersion, revision, models, routing))
		paused, err := model.GetChannelById(updated.Id, false)
		require.NoError(t, err)
		require.NoError(t, model.PublishSupplierChannelDraft(updated.Id, "supplier-a", updated.SupplierVersion, model.SupplierChannelRevision(paused, &draft), models, routing))
		after, err := model.GetChannelById(updated.Id, true)
		require.NoError(t, err)
		assert.True(t, after.SupplierPaused)
		assert.Equal(t, common.ChannelStatusManuallyDisabled, after.Status)
		assert.Equal(t, "replacement-secret", after.Key)
		assert.False(t, model.SupplierChannelAvailable(after))
		assert.True(t, after.SecretsWriteOnly)
		assert.EqualValues(t, 50, after.SupplierPriority)
		assert.EqualValues(t, 2, after.GetWeight())
		account, err := model.GetSupplierAccount("supplier-a")
		require.NoError(t, err)
		ratio := 0.5
		account.CostRatio = &ratio
		require.NoError(t, model.SaveSupplierAccount(account, account.Revision))
		ready, err := model.GetChannelLimitState(updated.Id, now)
		require.NoError(t, err)
		assert.True(t, ready.Config.BudgetReady)
		assert.EqualValues(t, 100, ready.Config.TPM)
	})
}

func supplierTestRequest(method, path, body, owner string, id int, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("id", 1)
	c.Set("role", common.RoleCommonUser)
	if owner != "" {
		c.Set("role", common.RoleSupplierUser)
		c.Set("supplier_id", owner)
	}
	if id > 0 {
		c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(id)}}
	}
	handler(c)
	return response
}

func TestSupplierReadModelReportsEveryBlockingReasonAndPoolSettings(t *testing.T) {
	_, _ = setupSecurityEnrollmentTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.SupplierAccount{}, &model.SupplierChannelDraft{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(model.DB))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(model.DB))
	account := model.SupplierAccount{SupplierID: "supplier-state", SupplyEnabled: true, AllowedTypes: "[1]", AllowedModels: "model-a", Group: "default", MaxChannels: 10, PoolPriority: 81, PoolWeight: 22}
	require.NoError(t, model.SaveSupplierAccount(&account, 0))
	channel := model.Channel{Name: "state", Type: 1, Models: "model-a", OwnerSupplierID: account.SupplierID, SupplierValidated: true, Status: common.ChannelStatusEnabled, Priority: common.GetPointer(int64(1)), Weight: common.GetPointer(uint(1))}
	require.NoError(t, model.DB.Create(&channel).Error)
	active := supplierChannelDTO(&channel, nil, "Supplier")
	assert.Equal(t, "available", active.Status)
	assert.Empty(t, active.BlockingReasons)
	assert.EqualValues(t, 81, active.PlatformPriority)
	assert.Equal(t, 22, active.Weight)

	now := time.Now()
	require.NoError(t, model.EnsureChannelLimitTracking(channel.Id, true, now))
	limit, err := model.ParseChannelLimitUSD("10")
	require.NoError(t, err)
	_, err = model.UpdateChannelLimits(channel.Id, 1, model.ChannelLimitSettings{DailyUSDNano: limit, ReconcileSeconds: model.ChannelLimitDefaultReconcile}, true, now)
	require.NoError(t, err)
	state, err := model.GetChannelLimitState(channel.Id, now)
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.ChannelDailyBudget{}).Where("channel_id = ? AND budget_day = ?", channel.Id, state.BudgetDay).Updates(map[string]any{"used_usd_nano": limit, "exhausted": true}).Error)
	require.NoError(t, model.DB.Model(&account).Updates(map[string]any{"allowed_models": "model-b", "supply_enabled": false}).Error)
	channel.Status, channel.SupplierPaused = common.ChannelStatusManuallyDisabled, true
	blocked := supplierChannelDTO(&channel, nil, "Supplier")
	assert.ElementsMatch(t, []string{"manual_paused", "paused", "supply_paused", "authorization_removed", "daily_exhausted"}, blocked.BlockingReasons)
	assert.Equal(t, "daily_exhausted", blocked.Status)

	// New authorized drafts must not be treated as revoked merely because the
	// active channel does not have a published model list yet.
	channel.SupplierValidated, channel.Models = false, ""
	draft := model.SupplierChannelDraft{Type: 1, Models: "model-b", Status: "pending"}
	pending := supplierChannelDTO(&channel, &draft, "Supplier")
	assert.Contains(t, pending.BlockingReasons, "pending")
	assert.NotContains(t, pending.BlockingReasons, "authorization_removed")
}

func TestSupplierCredentialTransportRequiresVerifiedHTTPS(t *testing.T) {
	previousSecure := common.SessionCookieSecure
	t.Cleanup(func() { common.SessionCookieSecure = previousSecure })
	for _, tc := range []struct {
		name, peer, proxies, proto string
		tls, secure, allowed       bool
	}{
		{"direct TLS", "198.51.100.1:443", "none", "", true, true, true},
		{"secure cookies required", "198.51.100.1:443", "none", "", true, false, false},
		{"cookie flag cannot allow plaintext", "127.0.0.1:80", "", "", false, true, false},
		{"public spoof", "198.51.100.1:80", "", "https", false, true, false},
		{"trusted proxy", "127.0.0.1:80", "127.0.0.1", "https", false, true, true},
		{"trust disabled", "127.0.0.1:80", "none", "https", false, true, false},
		{"mixed protocol chain", "127.0.0.1:80", "", "https,http", false, true, false},
		{"invalid trust configuration", "127.0.0.1:80", "127.0.0.1,bad", "https", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TRUSTED_PROXIES", tc.proxies)
			common.SessionCookieSecure = tc.secure
			response := supplierTestRequest("POST", "/channel", "{}", "supplier-a", 0, func(c *gin.Context) {
				c.Request.RemoteAddr = tc.peer
				c.Request.Header.Set("X-Forwarded-Proto", tc.proto)
				if tc.tls {
					c.Request.TLS = &tls.ConnectionState{}
				}
				// Invalid JSON stops after the transport guard, before any database access.
				c.Request.Body = io.NopCloser(strings.NewReader("{"))
				SaveSupplierChannel(c)
			})
			assert.Equal(t, !tc.allowed, strings.Contains(response.Body.String(), "Supplier credentials require HTTPS and secure session cookies"))
		})
	}
}

func TestSupplierChannelOwnerFilterBeforePagination(t *testing.T) {
	_, _ = setupSecurityEnrollmentTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.ChannelFundingIncident{}, &model.SupplierChannelDraft{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(model.DB))
	for i, owner := range []string{"supplier-a", "supplier-b", ""} {
		channel := model.Channel{Name: fmt.Sprintf("scope-%d", i), Type: 1, OwnerSupplierID: owner, Models: "test-model", Group: "default", Tag: common.GetPointer("shared-tag"), Status: common.ChannelStatusEnabled}
		require.NoError(t, model.DB.Create(&channel).Error)
	}
	for _, handler := range []gin.HandlerFunc{GetAllChannels, SearchChannels} {
		for _, tagMode := range []bool{false, true} {
			for _, owner := range []string{"supplier-a", "supplier-b", "unassigned"} {
				response := supplierTestRequest("GET", fmt.Sprintf("/api/channel/?owner_supplier_id=%s&tag_mode=%t&page_size=1", owner, tagMode), "", "", 0, handler)
				var envelope struct {
					Success bool `json:"success"`
					Data    struct {
						Items      []model.Channel  `json:"items"`
						Total      int64            `json:"total"`
						TypeCounts map[string]int64 `json:"type_counts"`
					} `json:"data"`
				}
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &envelope))
				require.True(t, envelope.Success, response.Body.String())
				require.Len(t, envelope.Data.Items, 1)
				want := owner
				if owner == "unassigned" {
					want = ""
				}
				assert.Equal(t, want, envelope.Data.Items[0].OwnerSupplierID)
				assert.EqualValues(t, 1, envelope.Data.Total)
				assert.EqualValues(t, 1, envelope.Data.TypeCounts["1"])
			}
		}
	}
}
