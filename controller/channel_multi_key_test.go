package controller

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestMultiKeyEnableRestoresOnlyExhaustedChannels(t *testing.T) {
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousMaster, previousCache, previousRedis, previousSQLite := common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled, common.SQLitePath
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousType, previousLogType)
		common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled, common.SQLitePath = previousMaster, previousCache, previousRedis, previousSQLite
	})
	t.Setenv("SQL_DSN", os.Getenv("TEST_CHANNEL_SQL_DSN"))
	t.Setenv("LOG_SQL_DSN", "")
	common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled = false, false, false
	common.SQLitePath = filepath.Join(t.TempDir(), "channel.db")
	require.NoError(t, model.InitDB())
	database := model.DB
	sqlDB, err := database.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	model.LOG_DB = database
	common.SetLogDatabaseType(common.MainDatabaseType())
	require.NoError(t, database.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.User{}, &model.Log{}, &model.AuditLog{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(database))
	root := &model.User{Username: "multi-key-review-root", Role: common.RoleRootUser, Status: common.UserStatusEnabled}
	require.NoError(t, database.Create(root).Error)
	t.Cleanup(func() { require.NoError(t, database.Unscoped().Delete(root).Error) })
	versionQuery := "SELECT VERSION()"
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		versionQuery = "SELECT sqlite_version()"
	}
	var version string
	require.NoError(t, database.Raw(versionQuery).Scan(&version).Error)
	t.Logf("database=%s version=%s", common.MainDatabaseType(), version)

	for _, cacheEnabled := range []bool{false, true} {
		for _, action := range []string{"enable_key", "enable_all_keys"} {
			for _, tc := range []struct {
				name           string
				initialStatus  int
				manualOverride string
				wantStatus     int
			}{
				{name: "key exhaustion restores", initialStatus: common.ChannelStatusEnabled, wantStatus: common.ChannelStatusEnabled},
				{name: "manual disable is preserved", initialStatus: common.ChannelStatusManuallyDisabled, wantStatus: common.ChannelStatusManuallyDisabled},
				{name: "manual disable after exhaustion is preserved", initialStatus: common.ChannelStatusEnabled, manualOverride: "status", wantStatus: common.ChannelStatusManuallyDisabled},
				{name: "tag disable after exhaustion is preserved", initialStatus: common.ChannelStatusEnabled, manualOverride: "tag", wantStatus: common.ChannelStatusManuallyDisabled},
			} {
				t.Run(fmt.Sprintf("cache=%t/%s/%s", cacheEnabled, action, tc.name), func(t *testing.T) {
					common.MemoryCacheEnabled = cacheEnabled
					tag := t.Name()
					channel := &model.Channel{Name: t.Name(), Type: 1, Key: "key-one\nkey-two", Status: tc.initialStatus, Models: "test-model", Group: "default", Tag: &tag,
						ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyStatusList: map[int]int{1: common.ChannelStatusManuallyDisabled}},
					}
					require.NoError(t, channel.Insert())
					t.Cleanup(func() {
						require.NoError(t, channel.Delete())
						model.InitChannelCache()
					})
					for _, operation := range []string{"disable_key", action} {
						if operation == action {
							if tc.manualOverride == "status" {
								model.UpdateChannelStatus(channel.Id, "", common.ChannelStatusManuallyDisabled, "manual operation")
							} else if tc.manualOverride == "tag" {
								require.NoError(t, model.DisableChannelByTag(tag))
							}
						}
						payload, err := common.Marshal(MultiKeyManageRequest{ChannelId: channel.Id, Action: operation, KeyIndex: common.GetPointer(0)})
						require.NoError(t, err)
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Set("id", root.Id)
						c.Set("role", common.RoleRootUser)
						c.Request = httptest.NewRequest(http.MethodPost, "/api/channel/multi_key", bytes.NewReader(payload))
						c.Request.Header.Set("Content-Type", "application/json")
						ManageMultiKeys(c)
						var result struct {
							Success bool `json:"success"`
						}
						require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result))
						require.True(t, result.Success, recorder.Body.String())
					}
					loaded, err := model.GetChannelById(channel.Id, true)
					require.NoError(t, err)
					assert.Equal(t, tc.wantStatus, loaded.Status)
					assert.NotContains(t, loaded.ChannelInfo.MultiKeyStatusList, 0)
					assert.NotContains(t, loaded.ChannelInfo.MultiKeyDisabledReason, 0)
					assert.NotContains(t, loaded.ChannelInfo.MultiKeyDisabledTime, 0)
					var ability model.Ability
					require.NoError(t, database.Where("channel_id = ?", channel.Id).First(&ability).Error)
					assert.Equal(t, tc.wantStatus == common.ChannelStatusEnabled, ability.Enabled)
				})
			}
		}
	}
	for _, action := range []string{"delete_key", "delete_disabled_keys"} {
		t.Run("procurement identity/"+action, func(t *testing.T) {
			channel := &model.Channel{Name: t.Name(), Type: 1, Key: "procurement-first\nprocurement-disabled\nprocurement-last", Models: "test-model", Group: "default", ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 3, MultiKeyStatusList: map[int]int{1: common.ChannelStatusAutoDisabled}}}
			require.NoError(t, channel.Insert())
			t.Cleanup(func() { require.NoError(t, channel.Delete()) })
			original := []model.UpstreamCredentialSnapshot{channel.GetUpstreamCredentialSnapshot(0), channel.GetUpstreamCredentialSnapshot(1), channel.GetUpstreamCredentialSnapshot(2)}
			payload, err := common.Marshal(MultiKeyManageRequest{ChannelId: channel.Id, Action: action, KeyIndex: common.GetPointer(0)})
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Set("id", root.Id)
			ctx.Set("role", common.RoleRootUser)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/multi_key", bytes.NewReader(payload))
			ctx.Request.Header.Set("Content-Type", "application/json")
			ManageMultiKeys(ctx)
			var response struct {
				Success bool `json:"success"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			require.True(t, response.Success, recorder.Body.String())
			loaded, err := model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			if action == "delete_key" {
				assert.Equal(t, original[1].CredentialID, loaded.GetUpstreamCredentialSnapshot(0).CredentialID)
				assert.Equal(t, original[1].BindingID, loaded.GetUpstreamCredentialSnapshot(0).BindingID)
			} else {
				assert.Equal(t, original[0].CredentialID, loaded.GetUpstreamCredentialSnapshot(0).CredentialID)
				assert.Equal(t, original[0].BindingID, loaded.GetUpstreamCredentialSnapshot(0).BindingID)
			}
			assert.Equal(t, original[2].CredentialID, loaded.GetUpstreamCredentialSnapshot(1).CredentialID)
			assert.Equal(t, original[2].BindingID, loaded.GetUpstreamCredentialSnapshot(1).BindingID)
		})
	}
	t.Run("procurement metadata and nullable channel cost API", func(t *testing.T) {
		ratio := 0.6
		channel := &model.Channel{Name: t.Name(), Type: 1, Key: "private-api-test-material", Models: "test-model", Group: "default", CostRatio: &ratio}
		require.NoError(t, channel.Insert())
		t.Cleanup(func() { require.NoError(t, channel.Delete()) })
		frozen := channel.GetUpstreamCredentialSnapshot(0)
		for _, operation := range []struct {
			handler gin.HandlerFunc
			body    map[string]any
			id      string
		}{
			{handler: UpdateChannel, body: map[string]any{"id": channel.Id, "cost_ratio": nil}},
			{handler: UpdateUpstreamCredential, body: map[string]any{"alias": "safe alias", "supplier_id": "", "tags": []string{"test"}, "cost_ratio": 0, "key": "forbidden-secret-update"}, id: frozen.CredentialID},
		} {
			payload, err := common.Marshal(operation.body)
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Set("id", root.Id)
			ctx.Set("role", common.RoleRootUser)
			ctx.Params = gin.Params{{Key: "id", Value: operation.id}}
			ctx.Request = httptest.NewRequest(http.MethodPut, "/api/channel", bytes.NewReader(payload))
			ctx.Request.Header.Set("Content-Type", "application/json")
			operation.handler(ctx)
			var response struct {
				Success bool `json:"success"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			require.True(t, response.Success, recorder.Body.String())
			assert.NotContains(t, recorder.Body.String(), "private-api-test-material")
			assert.NotContains(t, recorder.Body.String(), "fingerprint")
			loaded, err := model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			assert.Nil(t, loaded.CostRatio)
			assert.Equal(t, "private-api-test-material", loaded.Key)
			if operation.id == "" {
				assert.Nil(t, loaded.GetUpstreamCredentialSnapshot(0).EffectiveCostRatio)
			} else {
				require.NotNil(t, loaded.GetUpstreamCredentialSnapshot(0).EffectiveCostRatio)
				assert.Zero(t, *loaded.GetUpstreamCredentialSnapshot(0).EffectiveCostRatio)
			}
		}
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Set("id", root.Id)
		ctx.Set("role", common.RoleRootUser)
		ctx.Params = gin.Params{{Key: "id", Value: strconv.Itoa(channel.Id)}}
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/channel/upstream-credentials", nil)
		GetChannelUpstreamCredentials(ctx)
		var response struct {
			Success bool                               `json:"success"`
			Data    []model.UpstreamCredentialSnapshot `json:"data"`
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		require.True(t, response.Success, recorder.Body.String())
		require.Len(t, response.Data, 1)
		assert.Equal(t, "safe alias", response.Data[0].Alias)
		assert.NotContains(t, recorder.Body.String(), "private-api-test-material")
		assert.NotContains(t, recorder.Body.String(), "fingerprint")
		assert.Equal(t, 0.6, *frozen.EffectiveCostRatio)
	})
	t.Run("channel cost changes have versioned safe audits", func(t *testing.T) {
		ratio := 0.6
		channel := &model.Channel{Name: t.Name(), Type: 1, Key: "private-cost-audit-material", Models: "test-model", Group: "default", CostRatio: &ratio}
		require.NoError(t, channel.Insert())
		t.Cleanup(func() { require.NoError(t, channel.Delete()) })
		for _, tc := range []struct {
			name       string
			value      *float64
			provided   bool
			wantChange bool
			concurrent bool
		}{
			{name: "explicit zero", value: common.GetPointer(0.0), provided: true, wantChange: true},
			{name: "same zero", value: common.GetPointer(0.0), provided: true},
			{name: "omitted cost"},
			{name: "clear cost", provided: true, wantChange: true},
			{name: "same unknown", provided: true},
			{name: "known cost", value: common.GetPointer(0.7), provided: true, wantChange: true},
			{name: "transaction captures concurrent cost", value: common.GetPointer(0.7), provided: true, wantChange: true, concurrent: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				before, err := model.GetChannelById(channel.Id, true)
				require.NoError(t, err)
				if tc.concurrent {
					callback := "test:concurrent_channel_cost_audit"
					require.NoError(t, model.DB.Callback().Query().After("gorm:after_query").Register(callback, func(tx *gorm.DB) {
						loaded, ok := tx.Statement.Dest.(*model.Channel)
						if !ok || loaded.Id != channel.Id {
							return
						}
						// The controller has read the old configuration. Commit another
						// cost before its update starts, without scheduling or sleeps.
						require.NoError(t, model.DB.Callback().Query().Remove(callback))
						before.CostRatio = common.GetPointer(0.9)
						before.CostVersionID = uuid.NewString()
						require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).Updates(map[string]any{"cost_ratio": before.CostRatio, "cost_version_id": before.CostVersionID}).Error)
					}))
					t.Cleanup(func() { require.NoError(t, model.DB.Callback().Query().Remove(callback)) })
				}
				body := map[string]any{"id": channel.Id}
				if tc.provided {
					body["cost_ratio"] = tc.value
				}
				payload, err := common.Marshal(body)
				require.NoError(t, err)
				requestID := common.NewRequestId()
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Set("id", root.Id)
				ctx.Set("role", common.RoleRootUser)
				ctx.Set(common.RequestIdKey, requestID)
				ctx.Request = httptest.NewRequest(http.MethodPut, "/api/channel", bytes.NewReader(payload))
				ctx.Request.Header.Set("Content-Type", "application/json")
				UpdateChannel(ctx)
				var response struct {
					Success bool `json:"success"`
				}
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				require.True(t, response.Success, recorder.Body.String())
				after, err := model.GetChannelById(channel.Id, true)
				require.NoError(t, err)
				var audits []model.AuditLog
				require.NoError(t, model.LOG_DB.Where("request_id = ? AND action = ?", requestID, "channel.update").Find(&audits).Error)
				require.Len(t, audits, 1)
				require.NotNil(t, audits[0].Other.Op)
				expected := map[string]any{"id": channel.Id, "name": after.Name, "changed_fields": []string{}}
				if tc.wantChange {
					expected["changed_fields"] = []string{"cost_ratio"}
					expected["cost_ratio_from"] = before.CostRatio
					expected["cost_ratio"] = after.CostRatio
					expected["previous_cost_version_id"] = before.CostVersionID
					expected["cost_version_id"] = after.CostVersionID
					assert.NotEqual(t, before.CostVersionID, after.CostVersionID)
				} else {
					assert.Equal(t, before.CostVersionID, after.CostVersionID)
				}
				wanted, err := common.Marshal(expected)
				require.NoError(t, err)
				actual, err := common.Marshal(audits[0].Other.Op.Params)
				require.NoError(t, err)
				assert.JSONEq(t, string(wanted), string(actual))
				assert.NotContains(t, string(actual), "private-cost-audit-material")
				assert.NotContains(t, string(actual), "fingerprint")
				assert.NotContains(t, string(actual), "snapshot")
			})
		}
	})
}
