package controller

import (
	"errors"
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShouldRetryHonorsPinRetryMode(t *testing.T) {
	openaiErr := types.NewOpenAIError(errors.New("upstream"), types.ErrorCodeBadResponseStatusCode, http.StatusInternalServerError)

	c := newPinRetryContext()
	assert.True(t, service.ShouldRetryRelayError(c, openaiErr, 1))

	origin := newPinRetryContext()
	service.GetChannelConstraints(origin).AddPin(dto.ChannelPin{
		ChannelId: 2,
		Source:    dto.PinSourceOriginTask,
		Rank:      dto.PinRankOriginTask,
		RetryMode: dto.PinRetrySameChannel,
	})
	assert.True(t, service.ShouldRetryRelayError(origin, openaiErr, 1), "origin pin retries on the same channel")

	token := newPinRetryContext()
	service.GetChannelConstraints(token).AddPin(dto.ChannelPin{
		ChannelId: 1,
		Source:    dto.PinSourceToken,
		Rank:      dto.PinRankToken,
		RetryMode: dto.PinRetrySingleAttempt,
	})
	assert.Equal(t, service.PolicyDecision{Action: "stop", Reason: "pinned_channel", Source: "channel_constraint"}, service.DecideRelayRetry(token, openaiErr, 1), "token pin suppresses retry")
}

func TestShouldRetryTaskRelayHonorsPinRetryMode(t *testing.T) {
	taskErr := &dto.TaskError{StatusCode: http.StatusInternalServerError}

	c := newPinRetryContext()
	assert.Equal(t, "retry", decideTaskRetry(c, taskErr, 1).Action)

	origin := newPinRetryContext()
	service.GetChannelConstraints(origin).AddPin(dto.ChannelPin{
		ChannelId: 2,
		Source:    dto.PinSourceOriginTask,
		Rank:      dto.PinRankOriginTask,
		RetryMode: dto.PinRetrySameChannel,
	})
	assert.Equal(t, "retry", decideTaskRetry(origin, taskErr, 1).Action)

	token := newPinRetryContext()
	service.GetChannelConstraints(token).AddPin(dto.ChannelPin{
		ChannelId: 1,
		Source:    dto.PinSourceToken,
		Rank:      dto.PinRankToken,
		RetryMode: dto.PinRetrySingleAttempt,
	})
	assert.Equal(t, service.PolicyDecision{Action: "stop", Reason: "pinned_channel", Source: "channel_constraint"}, decideTaskRetry(token, taskErr, 1))
}

func TestSameChannelPinsMergeToStricterRetryMode(t *testing.T) {
	c := newPinRetryContext()
	constraints := service.GetChannelConstraints(c)
	constraints.AddPin(dto.ChannelPin{
		ChannelId: 7,
		Source:    dto.PinSourceOriginTask,
		Rank:      dto.PinRankOriginTask,
		RetryMode: dto.PinRetrySameChannel,
	})
	constraints.AddPin(dto.ChannelPin{
		ChannelId: 7,
		Source:    dto.PinSourceToken,
		Rank:      dto.PinRankToken,
		RetryMode: dto.PinRetrySingleAttempt,
	})
	pin, found, overridden := constraints.ResolvedPin()
	require.True(t, found)
	assert.Equal(t, 7, pin.ChannelId)
	assert.Equal(t, dto.PinRetrySingleAttempt, pin.RetryMode)
	assert.Empty(t, overridden)
	assert.False(t, service.ShouldRetryRelayError(c, types.NewOpenAIError(errors.New("upstream"), types.ErrorCodeBadResponseStatusCode, http.StatusInternalServerError), 1))
}

func newPinRetryContext() *gin.Context {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c
}

func TestRequestPolicyConfigReturnsSettingsWithoutMigration(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	GetRequestPolicy(ctx)
	assert.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.Contains(t, response.Data, "options")
	assert.NotContains(t, response.Data, "migration")
	assert.NotContains(t, response.Data, "differences")
}

func TestRequestPolicyRoutingDatabaseMatrix(t *testing.T) {
	require.NoError(t, i18n.Init())
	for _, dialect := range []struct{ kind, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.kind, func(t *testing.T) {
			dsn := ""
			if dialect.env != "" {
				dsn = os.Getenv(dialect.env)
				if dsn == "" {
					t.Skipf("%s not configured", dialect.env)
				}
			}
			db := modelManagementDB(t, dialect.kind, dsn)
			previousGroups := setting.UserUsableGroups2JSONString()
			previousRatios, err := common.Marshal(ratio_setting.GetGroupRatioCopy())
			require.NoError(t, err)
			require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default"}`))
			require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`))
			t.Cleanup(func() {
				require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(previousGroups))
				require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(string(previousRatios)))
			})
			// Channel 1 holds the session binding but is no longer usable.
			channels := []model.Channel{
				{Id: 1, Name: "A", Type: 1, Key: "test-only", Status: common.ChannelStatusAutoDisabled, Models: "policy-test", Group: "default", Priority: common.GetPointer(int64(10))},
				{Id: 2, Name: "B", Type: 1, Key: "test-only", Status: common.ChannelStatusEnabled, Models: "policy-test", Group: "default", Priority: common.GetPointer(int64(5))},
			}
			for i := range channels {
				require.NoError(t, db.Create(&channels[i]).Error)
				require.NoError(t, channels[i].AddAbilities(db))
			}
			affinity := operation_setting.GetChannelAffinitySetting()
			previousAffinity := *affinity
			t.Cleanup(func() { *affinity = previousAffinity })
			for _, cached := range []bool{false, true} {
				for _, keep := range []bool{false, true} {
					for _, tc := range []struct {
						globalMode, ruleMode string
						blocked              bool
					}{
						{"strict", "inherit", true},
						{"prefer", "strict", true},
						{"prefer", "inherit", false},
						{"strict", "prefer", false},
					} {
						t.Run(fmt.Sprintf("cache=%t/keep=%t/global=%s/rule=%s", cached, keep, tc.globalMode, tc.ruleMode), func(t *testing.T) {
							common.MemoryCacheEnabled = cached
							model.InitChannelCache()
							snapshot, err := model.BuildRequestPolicy(map[string]string{
								"channel_affinity_setting.enabled":                  "true",
								"channel_affinity_setting.session_mode":             tc.globalMode,
								"channel_affinity_setting.keep_on_channel_disabled": fmt.Sprint(keep),
								"channel_affinity_setting.rules":                    fmt.Sprintf(`[{"name":"session","model_regex":[".*"],"key_sources":[{"type":"request_header","key":"X-Session"}],"session_mode":%q}]`, tc.ruleMode),
							})
							require.NoError(t, err)
							*affinity = snapshot.Affinity
							seed := newPinRetryContext()
							seed.Request.Header.Set("X-Session", t.Name())
							_, found := service.GetPreferredChannelByAffinity(seed, "policy-test", "default")
							require.False(t, found)
							seed.Set("channel_id", 1)
							service.RecordChannelAffinity(seed, 1)
							t.Cleanup(func() { service.ClearCurrentChannelAffinityCache(seed) })
							bound, found := service.GetPreferredChannelByAffinity(seed, "policy-test", "default")
							require.True(t, found)
							require.Equal(t, 1, bound)

							request := newPinRetryContext()
							request.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"policy-test"}`))
							request.Request.Header.Set("Content-Type", "application/json")
							request.Request.Header.Set("X-Session", t.Name())
							common.SetContextKey(request, constant.ContextKeyUsingGroup, "default")
							middleware.Distribute()(request)
							events := service.RequestPolicy(request).Events()
							assert.True(t, slices.ContainsFunc(events, func(event service.PolicyEvent) bool { return event.Decision.Reason == "session_rule_matched" }), "decision events are recorded for every request")
							bound, found = service.GetPreferredChannelByAffinity(seed, "policy-test", "default")
							if tc.blocked {
								assert.Equal(t, http.StatusServiceUnavailable, request.Writer.Status())
								assert.True(t, request.IsAborted())
								assert.Equal(t, keep, found, "binding retention is independent of strict request handling")
								if found {
									assert.Equal(t, 1, bound)
								}
								return
							}
							assert.False(t, request.IsAborted())
							assert.Equal(t, 2, common.GetContextKeyInt(request, constant.ContextKeyChannelId), "prefer falls back to the next eligible channel")
							require.True(t, found)
							assert.Equal(t, 2, bound, "a successful fallback rebinds the session")
						})
					}
				}
			}
		})
	}
}

func TestChannelAvailabilityAdmissionAndManagement(t *testing.T) {
	require.NoError(t, i18n.Init())
	db := modelManagementDB(t, "sqlite", "")
	config := model.ChannelAvailability{Enabled: true, Timezone: "UTC", Mode: "daily", Rules: []model.ChannelAvailabilityRule{{ChannelAvailabilityWindow: model.ChannelAvailabilityWindow{Start: "00:00", End: "24:00"}}}}
	for _, offset := range []int{-1, 0, 1} {
		config.Exceptions = append(config.Exceptions, model.ChannelAvailabilityException{Date: time.Now().UTC().AddDate(0, 0, offset).Format("2006-01-02")})
	}
	raw, err := common.Marshal(config)
	require.NoError(t, err)
	scheduled := model.Channel{Name: "scheduled", Type: 1, Key: "fixture-key", Status: 1, Models: "availability-model", Group: "default", Availability: string(raw), Tag: common.GetPointer("scheduled-tag")}
	enabled := model.Channel{Name: "always-open", Type: 1, Key: "fixture-key", Status: 1, Models: "availability-model", Group: "default", Tag: common.GetPointer("open-tag")}
	for _, ch := range []*model.Channel{&scheduled, &enabled} {
		require.NoError(t, db.Create(ch).Error)
		require.NoError(t, ch.AddAbilities(db))
	}
	for _, cached := range []bool{false, true} {
		common.MemoryCacheEnabled = cached
		model.InitChannelCache()
		c := newPinRetryContext()
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		service.GetChannelConstraints(c).AddPin(dto.ChannelPin{ChannelId: scheduled.Id, Source: dto.PinSourceToken, Rank: dto.PinRankToken})
		ch, _, selectErr := service.SelectChannelForRequest(c, "availability-model", &service.RetryParam{Ctx: c, TokenGroup: "default", ModelName: "availability-model", Retry: common.GetPointer(0)})
		require.Nil(t, ch)
		require.NotNil(t, selectErr)
		assert.Equal(t, http.StatusServiceUnavailable, selectErr.StatusCode)
		assert.Equal(t, types.ErrorCode(dto.FilterAvailability), selectErr.Code)
		apiErr := middleware.SetupContextForSelectedChannel(c, &scheduled, "availability-model")
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
		assert.True(t, types.IsSkipRetryError(apiErr))
		assert.False(t, service.ShouldDisableChannel(apiErr))
		service.EndSchedulingRequest(c)
		// Automatic probes cannot turn planned closure into success/failure samples.
		assert.Equal(t, channelTestSummary{}, testChannelForHealthCheck(c.Request.Context(), &scheduled, 0, true, 1))
	}
	// Reading an accepted task (including result callbacks) does not admit new
	// work. A token pin must not stop these requests when its channel closes.
	for _, request := range []struct{ method, path, body string }{
		{http.MethodGet, "/mj/task/task-1/fetch", ""},
		{http.MethodPost, "/mj/task/list-by-condition", `{}`},
		{http.MethodGet, "/mj/task/task-1/image-seed", ""},
		{http.MethodPost, "/mj/notify", `{}`},
		{http.MethodGet, "/v1/videos/task-1", ""},
		{http.MethodGet, "/v1/video/generations/task-1", ""},
		{http.MethodPost, "/v1/videos/task-1/remix", `{}`},
	} {
		t.Run(request.path, func(t *testing.T) {
			router := gin.New()
			router.Use(func(c *gin.Context) {
				common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
				service.GetChannelConstraints(c).AddPin(dto.ChannelPin{ChannelId: scheduled.Id, Source: dto.PinSourceToken, Rank: dto.PinRankToken})
			}, middleware.Distribute())
			router.Handle(request.method, request.path, func(c *gin.Context) { c.Status(http.StatusNoContent) })
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(request.method, request.path, strings.NewReader(request.body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, req)
			if strings.HasSuffix(request.path, "/remix") {
				assert.Equal(t, http.StatusServiceUnavailable, recorder.Code, "remixing starts new work and stays gated")
			} else {
				assert.Equal(t, http.StatusNoContent, recorder.Code, recorder.Body.String())
			}
		})
	}
	for _, tagMode := range []bool{false, true} {
		for _, search := range []bool{false, true} {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/channel?status=outside_schedule&p=1&page_size=1&tag_mode=%t", tagMode), nil)
			if search {
				SearchChannels(ctx)
			} else {
				GetAllChannels(ctx)
			}
			var response struct {
				Success bool
				Data    struct {
					Items      []model.Channel
					Total      int
					TypeCounts map[string]int `json:"type_counts"`
				}
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			require.True(t, response.Success, recorder.Body.String())
			require.Len(t, response.Data.Items, 1)
			assert.Equal(t, 1, response.Data.Total)
			assert.Equal(t, scheduled.Id, response.Data.Items[0].Id)
			assert.Equal(t, common.ChannelStatusEnabled, response.Data.Items[0].Status)
			assert.Equal(t, common.ChannelStatusOutsideSchedule, response.Data.Items[0].EffectiveStatus)
			assert.Equal(t, 1, response.Data.TypeCounts["1"])
			assert.Empty(t, response.Data.Items[0].Key)
		}
	}
	// Partial channel edits must preserve a configured schedule; explicit reset
	// must reopen it. Read-only effective status cannot be written back.
	patch := model.Channel{Id: scheduled.Id, Name: "renamed"}
	require.NoError(t, patch.Update())
	assert.Equal(t, string(raw), patch.Availability)
	patch.Availability = "{}"
	require.NoError(t, patch.Update())
	assert.True(t, patch.ScheduleAllowsAt(time.Now()))
}

func TestChannelAvailabilityStatusCannotBePersistedByCreate(t *testing.T) {
	channel := model.Channel{Name: "scheduled", Type: 1, Key: "fixture-key", Models: "fixture-model", Group: "default", Status: common.ChannelStatusEnabled}
	require.NoError(t, validateChannel(&channel, true))
	channel.Status = common.ChannelStatusOutsideSchedule
	require.Error(t, validateChannel(&channel, true))
}
