package controller

import (
	"bytes"
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPerformanceHealthConfigurationContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, dialect := range []struct{ kind, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.kind, func(t *testing.T) {
			if dialect.env != "" && os.Getenv(dialect.env) == "" {
				t.Skip("set " + dialect.env + " to run this database")
			}
			db, _ := newAuditTestDatabase(t, dialect.kind, os.Getenv(dialect.env))
			previousDB, previousLog := model.DB, model.LOG_DB
			previousMainType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
			previousRedis, previousMemory := common.RedisEnabled, common.MemoryCacheEnabled
			common.RedisEnabled, common.MemoryCacheEnabled = false, false
			common.OptionMapRWMutex.Lock()
			previousOptions := maps.Clone(common.OptionMap)
			common.OptionMap = make(map[string]string)
			common.OptionMapRWMutex.Unlock()
			model.DB, model.LOG_DB = db, db
			common.SetDatabaseTypes(common.DatabaseType(dialect.kind), common.DatabaseType(dialect.kind))
			t.Cleanup(func() {
				model.DB, model.LOG_DB = previousDB, previousLog
				common.SetDatabaseTypes(previousMainType, previousLogType)
				common.RedisEnabled, common.MemoryCacheEnabled = previousRedis, previousMemory
				common.OptionMapRWMutex.Lock()
				common.OptionMap = previousOptions
				common.OptionMapRWMutex.Unlock()
			})
			require.NoError(t, db.AutoMigrate(&model.Option{}, &model.AuditLog{}, &model.User{}, &model.PerfMetric{}, &model.PerfMetricEvent{}, &model.PerfMetricCollector{}, &model.Task{}, &model.Channel{}))
			admin := model.User{Username: "config-owner", Password: "test-placeholder", AffCode: "config-owner", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, Group: "default"}
			require.NoError(t, db.Create(&admin).Error)
			confirmed := false
			initial := model.PerformanceHealthConfig{Version: 8, Companies: []model.PerformanceCompany{{
				ID: "legacy-openai", Name: "OpenAI", Status: "active", SLO: 97, Owner: "operator", Note: "manual note", PresetKey: "openai", SLOConfirmed: &confirmed,
				Groups: []model.PerformanceModelGroup{{ID: "manual-group", Name: "Manual", Models: []string{"private-model"}, Enabled: true, SLO: 97, SLOConfirmed: &confirmed, PresetKey: "openai", MinSamples: 250}},
			}}}
			raw, err := common.Marshal(initial)
			require.NoError(t, err)
			require.NoError(t, db.Create(&model.Option{Key: model.PerformanceHealthConfigOption, Value: string(raw)}).Error)

			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set("id", admin.Id)
				c.Set("role", common.RoleAdminUser)
				c.Next()
			})
			router.PATCH("/companies/:company_id", UpdatePerformanceHealthCompany)
			router.PATCH("/groups/:group_id", UpdatePerformanceHealthGroup)
			router.GET("/summary", GetPerformanceHealthSummary)
			router.GET("/timeseries", GetPerformanceHealthTimeseries)
			router.PUT("/config", PutPerformanceHealthConfig)
			router.POST("/presets/apply", ApplyPerformanceHealthPresets)
			request := func(method, path, body string, wantStatus int) {
				t.Helper()
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, httptest.NewRequest(method, path, bytes.NewBufferString(body)))
				require.Equal(t, wantStatus, recorder.Code, recorder.Body.String())
			}

			request(http.MethodPut, "/config", `{"companies":[]}`, http.StatusBadRequest)
			request(http.MethodPost, "/presets/apply", `{}`, http.StatusBadRequest)
			current, err := model.GetPerformanceHealthConfig()
			require.NoError(t, err)
			assert.Equal(t, int64(8), current.Version)
			assert.Equal(t, initial.Companies, current.Companies)

			request(http.MethodPatch, "/companies/legacy-openai", `{"name":"My OpenAI","id":"cannot-change-id"}`, http.StatusOK)
			current, err = model.GetPerformanceHealthConfig()
			require.NoError(t, err)
			wantCompany := initial.Companies[0]
			wantCompany.Name = "My OpenAI"
			assert.Equal(t, wantCompany, current.Companies[0], "name-only patches keep manual values and target confirmation")

			request(http.MethodPatch, "/groups/manual-group", `{"name":"Renamed group","id":"cannot-change-id"}`, http.StatusOK)
			current, err = model.GetPerformanceHealthConfig()
			require.NoError(t, err)
			wantGroup := initial.Companies[0].Groups[0]
			wantGroup.Name = "Renamed group"
			assert.Equal(t, wantGroup, current.Companies[0].Groups[0], "name-only patches keep monitoring, members and SLO")

			request(http.MethodPatch, "/groups/manual-group", `{"enabled":false,"slo":0,"min_samples":0}`, http.StatusOK)
			current, err = model.GetPerformanceHealthConfig()
			require.NoError(t, err)
			group := current.Companies[0].Groups[0]
			assert.False(t, group.Enabled)
			assert.Zero(t, group.SLO)
			assert.Zero(t, group.MinSamples)
			assert.Equal(t, []string{"private-model"}, group.Models)
			request(http.MethodPut, "/config", `{"expected_config_version":8,"companies":[]}`, http.StatusConflict)
			afterConflict, err := model.GetPerformanceHealthConfig()
			require.NoError(t, err)
			assert.Equal(t, current, afterConflict)

			// Admin history remains visible after a traffic group is removed.
			now := time.Now().Unix()
			require.NoError(t, db.Create(&[]model.PerfMetric{
				{ModelName: "private-model", Group: "removed-traffic-group", BucketTs: now - now%3600, RequestCount: 4, SuccessCount: 3, TotalLatencyMs: 4000},
				{ModelName: "zero-output", Group: "removed-traffic-group", BucketTs: now - now%3600, RequestCount: 1, SuccessCount: 1, LastSampleAt: now - 10, GenerationMs: 1000},
			}).Error)
			require.NoError(t, db.Create(&model.Channel{Name: "configured", Models: "waiting-model"}).Error)
			require.NoError(t, model.SavePerfMetricCollector(context.Background(), db, model.PerfMetricCollector{ID: "other-node", HeartbeatAt: now}))
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/summary", nil))
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			var response struct {
				Success bool
				Data    performanceHealthSummary
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			require.True(t, response.Success)
			require.NotNil(t, response.Data.Summary)
			assert.Equal(t, 80.0, response.Data.Summary.SuccessRate)
			require.NotNil(t, response.Data.LastSampleAt)
			assert.Equal(t, int64(now-10), *response.Data.LastSampleAt)
			assert.Nil(t, response.Data.PersistedThrough, "latest sample time is not an all-events coverage watermark")
			require.Len(t, response.Data.Models, 3)
			for _, item := range response.Data.Models {
				switch item.ModelName {
				case "private-model":
					assert.Equal(t, int64(4), item.RequestCount)
					assert.Equal(t, "legacy-openai", item.CompanyID, "disabled monitoring does not erase ownership or history")
					assert.Nil(t, item.AvgTps, "legacy missing output duration remains unknown")
				case "zero-output":
					require.NotNil(t, item.AvgTps)
					assert.Zero(t, *item.AvgTps)
					assert.True(t, item.OutputMetricsAvailable)
				case "waiting-model":
					assert.Zero(t, item.RequestCount)
					assert.Nil(t, item.SuccessRate)
					assert.Nil(t, item.AvgLatencyMs)
					assert.Nil(t, item.AvgTps)
				}
			}
			recorder = httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/timeseries?model=private-model", nil))
			require.Equal(t, http.StatusOK, recorder.Code)
			assert.Contains(t, recorder.Body.String(), `"success_rate":75`)
			retained, err := model.GetPerformanceHealthConfig()
			require.NoError(t, err)
			assert.Equal(t, afterConflict, retained, "read endpoints never rewrite manual configuration")

		})
	}
}

func TestPerformanceHealthSLOInheritance(t *testing.T) {
	unconfirmed := false
	config := model.PerformanceHealthConfig{Companies: []model.PerformanceCompany{{
		ID: "original", Name: "Openai", Status: "active", SLO: 97,
		Groups: []model.PerformanceModelGroup{{ID: "default", Name: "Default", Models: []string{"model"}, Enabled: true, SLO: 99.9, SLOConfirmed: &unconfirmed, MinSamples: 100}},
	}}}
	owner := buildPerformanceHealthOwnership(config)["model"]
	require.NotNil(t, owner.EffectiveSLO)
	assert.Equal(t, float64(97), *owner.EffectiveSLO, "unconfirmed group templates inherit the legacy manual company target")
	assert.True(t, owner.MonitoringEnabled)
	assert.Equal(t, int64(100), owner.MinSamples)
	confirmed := true
	config.Companies[0].Groups[0].SLOConfirmed = &confirmed
	owner = buildPerformanceHealthOwnership(config)["model"]
	require.NotNil(t, owner.EffectiveSLO)
	assert.Equal(t, 99.9, *owner.EffectiveSLO)
	config.Companies[0].Groups[0].SLO = 0
	owner = buildPerformanceHealthOwnership(config)["model"]
	require.NotNil(t, owner.EffectiveSLO)
	assert.Zero(t, *owner.EffectiveSLO, "an explicitly confirmed zero is a valid target")
	config.Companies[0].Groups[0].SLOConfirmed = nil
	owner = buildPerformanceHealthOwnership(config)["model"]
	require.NotNil(t, owner.EffectiveSLO)
	assert.Equal(t, float64(97), *owner.EffectiveSLO, "an omitted legacy zero keeps inheriting the company target")
	config.Companies[0].Status = "disabled"
	owner = buildPerformanceHealthOwnership(config)["model"]
	assert.False(t, owner.MonitoringEnabled)
	assert.Nil(t, owner.EffectiveSLO)
	assert.Equal(t, "original", owner.CompanyID, "disabled ownership is retained for inspection")
}
