package controller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/scheduler"
	"github.com/QuantumNous/new-api/relay/channel/baidu"
	"github.com/QuantumNous/new-api/relay/channel/vertex"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type schedulingTokenTransport func(*http.Request) (*http.Response, error)

func (transport schedulingTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return transport(req)
}

func TestRelayBudgetCancelsProviderTokenAcquisition(t *testing.T) {
	service.InitHttpClient()
	client := service.GetHttpClient()
	previousTransport, previousTimeout := client.Transport, client.Timeout
	previousBudget := common.RelayTotalTimeout
	common.RelayTotalTimeout = 1
	client.Timeout = 0
	t.Cleanup(func() {
		client.Transport, client.Timeout = previousTransport, previousTimeout
		common.RelayTotalTimeout = previousBudget
	})
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	encodedKey, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	creds := vertex.Credentials{
		ClientEmail: "budget-test@example.invalid",
		PrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey})),
	}
	for _, tc := range []struct {
		name, host string
		acquire    func(*gin.Context) error
	}{
		{"Baidu cold cache", "aip.baidubce.com", func(c *gin.Context) error {
			adaptor := &baidu.Adaptor{}
			_, err := adaptor.DoRequest(c, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
				ApiKey: "budget-test-client|budget-test-secret", ChannelBaseUrl: "https://provider.invalid", UpstreamModelName: "ERNIE-Bot",
			}}, strings.NewReader("{}"))
			return err
		}},
		{"Vertex cold cache", "www.googleapis.com", func(c *gin.Context) error {
			adaptor := &vertex.Adaptor{AccountCredentials: creds}
			headers := make(http.Header)
			return adaptor.SetupRequestHeader(c, &headers, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelId: -9917}})
		}},
		{"Vertex plugin credentials", "www.googleapis.com", func(c *gin.Context) error {
			_, err := vertex.AcquireAccessTokenWithContext(c.Request.Context(), creds, "")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				defer service.BeginRelayRequestBudget(c)()
				deadline, ok := c.Request.Context().Deadline()
				require.True(t, ok)
				calls := 0
				client.Transport = schedulingTokenTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					assert.Equal(t, tc.host, req.URL.Host)
					assert.Equal(t, "https", req.URL.Scheme)
					requestDeadline, bounded := req.Context().Deadline()
					require.True(t, bounded, "provider credential acquisition must inherit the relay budget")
					assert.Equal(t, deadline, requestDeadline)
					if req.Body != nil {
						_, err := io.Copy(io.Discard, req.Body)
						require.NoError(t, err)
						require.NoError(t, req.Body.Close())
					}
					<-req.Context().Done()
					return nil, req.Context().Err()
				})
				err := tc.acquire(c)
				require.ErrorIs(t, err, context.DeadlineExceeded)
				assert.NotContains(t, err.Error(), "budget-test-secret")
				assert.Equal(t, 1, calls)
				assert.False(t, c.Writer.Written())
				assert.False(t, service.SchedulingRetryAllowed(c))
			})
		})
	}
}

func TestSchedulingConfigRejectsUnsafeCapacityAndWeights(t *testing.T) {
	valid := service.SchedulingConfig{Group: "default", Model: "model-a", Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 100}
	require.NoError(t, service.ValidateSchedulingConfig(valid))
	for _, tc := range []struct {
		name   string
		change func(*service.SchedulingConfig)
	}{
		{"auto is not a physical group", func(c *service.SchedulingConfig) { c.Group = "auto" }},
		{"missing model", func(c *service.SchedulingConfig) { c.Model = "" }},
		{"zero capacity", func(c *service.SchedulingConfig) { c.DefaultCapacity = 0 }},
		{"oversized capacity", func(c *service.SchedulingConfig) { c.DefaultCapacity = 100001 }},
		{"invalid success target", func(c *service.SchedulingConfig) { c.TargetSuccessRate = 1.1 }},
		{"non-finite target", func(c *service.SchedulingConfig) { c.TargetSuccessRate = math.NaN() }},
		{"zero latency target", func(c *service.SchedulingConfig) { c.TargetTTFTMS = 0 }},
		{"negative weight", func(c *service.SchedulingConfig) {
			c.ChannelOverrides = []service.SchedulingChannelOverride{{ChannelID: 1, Weight: common.GetPointer(-1.0)}}
		}},
		{"non-finite weight", func(c *service.SchedulingConfig) {
			c.ChannelOverrides = []service.SchedulingChannelOverride{{ChannelID: 1, Weight: common.GetPointer(math.Inf(1))}}
		}},
		{"negative override capacity", func(c *service.SchedulingConfig) {
			c.ChannelOverrides = []service.SchedulingChannelOverride{{ChannelID: 1, Capacity: common.GetPointer(-1)}}
		}},
		{"duplicate channel", func(c *service.SchedulingConfig) {
			c.ChannelOverrides = []service.SchedulingChannelOverride{{ChannelID: 1}, {ChannelID: 1}}
		}},
		{"invalid pool name", func(c *service.SchedulingConfig) {
			c.ChannelOverrides = []service.SchedulingChannelOverride{{ChannelID: 1, CapacityKey: "pool\nsecond"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := valid
			tc.change(&config)
			assert.Error(t, service.ValidateSchedulingConfig(config))
		})
	}
	valid.ChannelOverrides = []service.SchedulingChannelOverride{{ChannelID: 1, Weight: common.GetPointer(0.0)}, {ChannelID: 2}}
	assert.NoError(t, service.ValidateSchedulingConfig(valid), "zero static weight and inherited overrides are valid")
}

func TestSchedulingDashboardJSONKeepsConfigurationOverridesAndMissingSamples(t *testing.T) {
	encoded, err := common.Marshal(schedulingDashboard{
		Snapshot: scheduler.Snapshot{Scope: "instance", Config: scheduler.DefaultConfig(), Channels: []scheduler.ChannelSnapshot{{ChannelID: 9}}},
		Config:   service.SchedulingConfig{Enabled: true, ChannelOverrides: []service.SchedulingChannelOverride{{ChannelID: 9, Weight: common.GetPointer(25.0)}}},
	})
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, common.Unmarshal(encoded, &payload))
	config := payload["config"].(map[string]any)
	require.Len(t, config["channel_overrides"], 1)
	channel := payload["channels"].([]any)[0].(map[string]any)
	assert.Nil(t, channel["avg_ttft_ms_5m"], "unknown latency must never become zero")
	assert.Nil(t, channel["success_rate_5m"], "no attempts must not imply success")
	assert.Equal(t, "instance", payload["scope"])
}

func TestSchedulingHandlersRejectInvalidInputBeforeDatabaseAccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, method, target, body string
		handler                    gin.HandlerFunc
	}{
		{"bad stream", http.MethodGet, "/?stream=maybe", "", GetScheduling},
		{"bad config", http.MethodPost, "/", `{"group":"default","model":"m","default_capacity":-1}`, UpdateSchedulingConfig},
		{"bad recovery", http.MethodPost, "/", `{"group":"default","model":"m","channel_id":0}`, RecoverSchedulingChannel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			tc.handler(ctx)
			assert.Equal(t, http.StatusBadRequest, recorder.Code)
		})
	}
}

func TestSchedulingChannelMembership(t *testing.T) {
	channel := &model.Channel{Id: 7, Group: "default,premium", Models: "model-a,model-b"}
	assert.True(t, schedulingChannelMatches(channel, scheduler.Key{Group: "premium", Model: "model-a"}))
	assert.False(t, schedulingChannelMatches(channel, scheduler.Key{Group: "other", Model: "model-a"}))
	assert.False(t, schedulingChannelMatches(channel, scheduler.Key{Group: "premium", Model: "other"}))
	assert.False(t, schedulingChannelMatches(nil, scheduler.Key{}))
}

func TestSchedulingDefaultsFollowCurrentBusinessTraffic(t *testing.T) {
	db := modelManagementDB(t, "sqlite", "")
	previousEngine := scheduler.Default
	scheduler.Default = scheduler.New()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap["SchedulingConfig"] = ""
		common.OptionMapRWMutex.Unlock()
		assert.NoError(t, service.SyncSchedulingConfig())
		scheduler.Default = previousEngine
	})
	common.OptionMapRWMutex.Lock()
	common.OptionMap["SchedulingConfig"] = "[]"
	common.OptionMapRWMutex.Unlock()
	require.NoError(t, service.SyncSchedulingConfig())
	for _, row := range []model.Channel{
		{Name: "idle channel", Type: 1, Key: "fixture-key", Status: common.ChannelStatusEnabled, Group: "a-group", Models: "alpha-model"},
		{Name: "live channel", Type: 1, Key: "fixture-key", Status: common.ChannelStatusEnabled, Group: "live-group", Models: "alpha-model,z-live"},
	} {
		require.NoError(t, db.Create(&row).Error)
		for modelName := range strings.SplitSeq(row.Models, ",") {
			require.NoError(t, db.Create(&model.Ability{Group: row.Group, Model: modelName, ChannelId: row.Id, Enabled: true}).Error)
		}
	}
	for _, key := range []scheduler.Key{
		{Group: "live-group", Model: "z-live"},
		{Group: "live-group", Model: "deleted-model"},
		{Group: "deleted-group", Model: "deleted-model"},
	} {
		request := scheduler.Default.BeginRequest(key, "", false)
		scheduler.Default.EndRequest(request)
	}
	// Simply visiting an idle model must not displace the actual live model.
	scheduler.Default.Snapshot(scheduler.Key{Group: "a-group", Model: "alpha-model"}, nil, true)
	for _, tc := range []struct {
		query, group, model string
	}{
		{"/", "live-group", "z-live"},
		{"/?group=live-group", "live-group", "z-live"},
		{"/?group=a-group", "a-group", "alpha-model"},
		{"/?group=live-group&model=alpha-model", "live-group", "alpha-model"},
	} {
		var result struct {
			Success bool                `json:"success"`
			Data    schedulingDashboard `json:"data"`
		}
		recorder := modelManagementRequest(t, GetScheduling, http.MethodGet, tc.query, nil, &result)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.True(t, result.Success)
		assert.Equal(t, tc.group, result.Data.Group)
		assert.Equal(t, tc.model, result.Data.Model)
		assert.NotContains(t, result.Data.Groups, "deleted-group")
		assert.NotContains(t, result.Data.Models, "deleted-model")
		require.Len(t, result.Data.Channels, 1)
	}
	for _, query := range []string{"/?group=deleted-group", "/?group=live-group&model=deleted-model"} {
		var result struct {
			Success bool   `json:"success"`
			Code    string `json:"code"`
		}
		recorder := modelManagementRequest(t, GetScheduling, http.MethodGet, query, nil, &result)
		assert.Equal(t, http.StatusBadRequest, recorder.Code)
		assert.False(t, result.Success)
		assert.Equal(t, "scheduling_selection_unavailable", result.Code, "the UI may reset only a selection rejected by the server")
	}
}

func TestSchedulingDatabaseMatrix(t *testing.T) {
	for _, dialect := range []struct{ kind, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.kind, func(t *testing.T) {
			if dialect.env != "" && os.Getenv(dialect.env) == "" {
				t.Skip("set " + dialect.env + " to run this database")
			}
			// InitDB in this fixture initializes the dialect-specific quoting for
			// reserved group/key columns as well as isolating the database.
			db := modelManagementDB(t, dialect.kind, os.Getenv(dialect.env))
			require.NoError(t, db.AutoMigrate(&model.Log{}))
			previousEngine := scheduler.Default
			scheduler.Default = scheduler.New()
			t.Cleanup(func() {
				common.OptionMapRWMutex.Lock()
				common.OptionMap["SchedulingConfig"] = ""
				common.OptionMapRWMutex.Unlock()
				// Clear the service's cached configuration while this database is
				// still open, before the fixture restores its previous OptionMap.
				assert.NoError(t, service.SyncSchedulingConfig())
				scheduler.Default = previousEngine
			})
			common.OptionMapRWMutex.Lock()
			common.OptionMap["SchedulingConfig"] = "[]"
			common.OptionMapRWMutex.Unlock()
			require.NoError(t, service.SyncSchedulingConfig())

			channel := model.Channel{Name: "shared upstream", Type: 1, Key: "fixture-key", Status: common.ChannelStatusEnabled, Group: "matrix-a,matrix-b", Models: "matrix-model,matrix-other", Weight: common.GetPointer(uint(10))}
			require.NoError(t, db.Create(&channel).Error)
			keyA := scheduler.Key{Group: "matrix-a", Model: "matrix-model"}
			keyB := scheduler.Key{Group: "matrix-b", Model: "matrix-model"}
			keyOther := scheduler.Key{Group: "matrix-a", Model: "matrix-other"}
			for _, key := range []scheduler.Key{keyA, keyB, keyOther} {
				require.NoError(t, db.Create(&model.Ability{Group: key.Group, Model: key.Model, ChannelId: channel.Id, Enabled: true, Weight: 10}).Error)
			}
			configA := service.SchedulingConfig{Group: keyA.Group, Model: keyA.Model, Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 8, ChannelOverrides: []service.SchedulingChannelOverride{{ChannelID: channel.Id, Weight: common.GetPointer(25.0), CapacityKey: "matrix-shared"}}}
			configB := service.SchedulingConfig{Group: keyB.Group, Model: keyB.Model, Enabled: true, TargetSuccessRate: .99, TargetTTFTMS: 1500, DefaultCapacity: 1, ChannelOverrides: []service.SchedulingChannelOverride{{ChannelID: channel.Id, Weight: common.GetPointer(75.0), CapacityKey: "matrix-shared"}}}
			configOther := service.SchedulingConfig{Group: keyOther.Group, Model: keyOther.Model, Enabled: false, TargetSuccessRate: .9, TargetTTFTMS: 4500, DefaultCapacity: 3, ChannelOverrides: []service.SchedulingChannelOverride{{ChannelID: channel.Id, Weight: common.GetPointer(0.0), CapacityKey: "matrix-other"}}}
			sentinel := model.Option{Key: "SchedulingMatrixUnrelated", Value: "preserve this value"}
			require.NoError(t, db.Create(&sentinel).Error)
			for _, config := range []service.SchedulingConfig{configA, configB, configOther} {
				require.NoError(t, service.SaveSchedulingConfig(config))
			}
			configA.TargetTTFTMS = 2200
			configA.ChannelOverrides[0].Weight = common.GetPointer(40.0)
			require.NoError(t, service.SaveSchedulingConfig(configA))
			require.NoError(t, service.SaveSchedulingConfig(configA), "saving again must update the same durable option")
			var stored []model.Option
			require.NoError(t, db.Where(&model.Option{Key: "SchedulingConfig"}).Find(&stored).Error)
			require.Len(t, stored, 1)
			var persisted []service.SchedulingConfig
			require.NoError(t, common.UnmarshalJsonStr(stored[0].Value, &persisted))
			assert.ElementsMatch(t, []service.SchedulingConfig{configA, configB, configOther}, persisted, "changing one group/model must preserve both other dimensions")
			var untouched model.Option
			require.NoError(t, db.Where(&model.Option{Key: sentinel.Key}).First(&untouched).Error)
			assert.Equal(t, sentinel.Value, untouched.Value)

			// Neither a request nor a dashboard visit has accessed group B. Its
			// lower shared capacity must nevertheless constrain group A immediately.
			candidate := service.SchedulerCandidate(&channel)
			first := scheduler.Default.BeginRequest(keyA, "first reservation", true)
			second := scheduler.Default.BeginRequest(keyA, "second reservation", true)
			attempt, err := scheduler.Default.SelectAndReserve(first, []scheduler.Candidate{candidate}, true, false)
			require.NoError(t, err)
			assert.Equal(t, 1, attempt.Candidate.Capacity)
			_, err = scheduler.Default.SelectAndReserve(second, []scheduler.Candidate{candidate}, true, false)
			assert.ErrorIs(t, err, scheduler.ErrNoEligibleChannel)
			scheduler.Default.EndRequest(first)
			_, err = scheduler.Default.SelectAndReserve(second, []scheduler.Candidate{candidate}, true, false)
			require.NoError(t, err, "releasing an unstarted reservation restores shared capacity")
			scheduler.Default.EndRequest(second)

			// Reload only the durable option into a fresh engine. This models a
			// restart, rather than reading back the service's previous cache.
			common.OptionMapRWMutex.Lock()
			common.OptionMap["SchedulingConfig"] = "[]"
			common.OptionMapRWMutex.Unlock()
			require.NoError(t, service.SyncSchedulingConfig())
			scheduler.Default = scheduler.New()
			common.OptionMapRWMutex.Lock()
			common.OptionMap["SchedulingConfig"] = stored[0].Value
			common.OptionMapRWMutex.Unlock()
			require.NoError(t, service.SyncSchedulingConfig())
			assert.Equal(t, configA, service.GetSchedulingConfig(keyA))
			assert.Equal(t, configB, service.GetSchedulingConfig(keyB))
			assert.Equal(t, configOther, service.GetSchedulingConfig(keyOther))

			channels := []model.Channel{channel}
			for _, name := range []string{"disabled ability", "missing ability", "disabled channel"} {
				row := model.Channel{Name: name, Type: 1, Key: "fixture-key", Status: common.ChannelStatusEnabled, Group: keyA.Group, Models: keyA.Model, Weight: common.GetPointer(uint(10))}
				require.NoError(t, db.Create(&row).Error)
				require.NoError(t, db.Create(&model.Ability{Group: keyA.Group, Model: keyA.Model, ChannelId: row.Id, Enabled: true, Weight: 10}).Error)
				channels = append(channels, row)
			}
			// Reach the real 50-original activation threshold and leave a failure
			// on each channel, so false recovery eligibility would be observable.
			model.InitChannelCache()
			require.NoError(t, service.SyncSchedulingConfig())
			for index := range 50 {
				request := scheduler.Default.BeginRequest(keyA, fmt.Sprintf("activation-%d", index), false)
				attempt, err := scheduler.Default.ReserveCandidate(request, candidate, true)
				require.NoError(t, err)
				scheduler.Default.StartAttempt(attempt)
				scheduler.Default.FinishAttempt(attempt, scheduler.Outcome{Success: true})
				scheduler.Default.EndRequest(request)
			}
			for _, row := range channels {
				request := scheduler.Default.BeginRequest(keyA, fmt.Sprintf("failure-%d", row.Id), false)
				attempt, err := scheduler.Default.ReserveCandidate(request, service.SchedulerCandidate(&row), true)
				require.NoError(t, err)
				scheduler.Default.StartAttempt(attempt)
				scheduler.Default.FinishAttempt(attempt, scheduler.Outcome{ChannelFailure: true})
				scheduler.Default.EndRequest(request)
			}
			require.NoError(t, db.Model(&model.Ability{}).Where(&model.Ability{Group: keyA.Group, Model: keyA.Model, ChannelId: channels[1].Id}).Update("enabled", false).Error)
			require.NoError(t, db.Where(&model.Ability{Group: keyA.Group, Model: keyA.Model, ChannelId: channels[2].Id}).Delete(&model.Ability{}).Error)
			require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channels[3].Id).Update("status", common.ChannelStatusManuallyDisabled).Error)
			model.InitChannelCache()

			var dashboard struct {
				Success bool                `json:"success"`
				Data    schedulingDashboard `json:"data"`
			}
			recorder := modelManagementRequest(t, GetScheduling, http.MethodGet, "/?group=matrix-a&model=matrix-model&stream=true", nil, &dashboard)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.True(t, dashboard.Success)
			require.True(t, dashboard.Data.Active)
			require.Len(t, dashboard.Data.Channels, 4)
			assert.Equal(t, 1, dashboard.Data.Summary.EligibleChannels)
			byID := make(map[int]scheduler.ChannelSnapshot, len(dashboard.Data.Channels))
			for _, row := range dashboard.Data.Channels {
				byID[row.ChannelID] = row
			}
			assert.Equal(t, 1.0, byID[channel.Id].SelectionProbability)
			assert.True(t, byID[channel.Id].CanRecover)
			assert.Equal(t, 40.0, byID[channel.Id].ConfiguredWeight)
			assert.Equal(t, 1, byID[channel.Id].Capacity)
			for _, row := range channels[1:] {
				assert.Zero(t, byID[row.Id].SelectionProbability, row.Name)
				assert.False(t, byID[row.Id].CanRecover, row.Name)
				var result struct {
					Success bool `json:"success"`
				}
				recorder := modelManagementRequest(t, RecoverSchedulingChannel, http.MethodPost, "/", map[string]any{"group": keyA.Group, "model": keyA.Model, "channel_id": row.Id}, &result)
				assert.Equal(t, http.StatusConflict, recorder.Code, row.Name)
				assert.False(t, result.Success, row.Name)
			}
			assert.Equal(t, "ineligible", byID[channels[1].Id].RouteState)
			assert.Equal(t, "ineligible", byID[channels[2].Id].RouteState)
			assert.Equal(t, "disabled", byID[channels[3].Id].RouteState)
			recorder = modelManagementRequest(t, RecoverSchedulingChannel, http.MethodPost, "/", map[string]any{"group": keyA.Group, "model": keyA.Model, "channel_id": channel.Id}, nil)
			assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		})
	}
}
