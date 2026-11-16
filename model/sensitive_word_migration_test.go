package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/request_plugin_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func optionsRevision(t *testing.T) int64 {
	t.Helper()
	revisions, err := GetLatestSyncRevisions()
	require.NoError(t, err)
	return revisions[SyncTopicOptions]
}

// The old words become one global request-filter instance after the saved
// ones, enabled when both old switches were on; the old options go, and a
// second start changes nothing.
func TestMigrateSensitiveWords(t *testing.T) {
	keep := `{"id":"rp_keep","name":"Keep","plugin":"request-redact","enabled":true,"global":true,"groups":[],"models":[],"config":{"replacement":"[X]"}}`
	tests := map[string]struct {
		options   map[string]string
		language  string
		instances []string
		enabled   bool
		name      string
	}{
		"both switches on": {
			options:   map[string]string{"CheckSensitiveEnabled": "true", "CheckSensitiveOnPromptEnabled": "true", "StopOnSensitiveEnabled": "false", "SensitiveWords": " Bad \n\nworse\r\n", "StreamCacheQueueLength": "3", request_plugin_setting.InstancesOption: "[" + keep + "]"},
			instances: []string{keep},
			enabled:   true,
			name:      "敏感词",
		},
		"one switch on, no saved instances, English": {
			options:  map[string]string{"CheckSensitiveEnabled": "true", "SensitiveWords": "Bad\nworse"},
			language: "en",
			name:     "Sensitive words",
		},
		"Chinese by region": {
			options:  map[string]string{"CheckSensitiveOnPromptEnabled": "true", "SensitiveWords": "Bad\nworse"},
			language: "zh-TW",
			name:     "敏感词",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			db := useFrontendOptionMigrationDB(t)
			require.NoError(t, db.AutoMigrate(&SystemTask{}))
			previousLanguage := common.DefaultLanguage
			common.DefaultLanguage = tc.language
			t.Cleanup(func() { common.DefaultLanguage = previousLanguage })
			for key, value := range tc.options {
				require.NoError(t, db.Create(&Option{Key: key, Value: value}).Error)
			}

			require.NoError(t, MigrateSensitiveWords())
			for _, key := range retiredSensitiveOptionKeys {
				requireOptionMissing(t, db, key)
			}
			saved := requireOptionValue(t, db, request_plugin_setting.InstancesOption)
			instances := gjson.Parse(saved).Array()
			require.Len(t, instances, len(tc.instances)+1)
			for index, instance := range tc.instances {
				assert.JSONEq(t, instance, instances[index].Raw)
			}
			migrated := instances[len(tc.instances)]
			assert.Regexp(t, `^rp_[a-z2-7]{8}$`, migrated.Get("id").Str)
			withoutID, err := sjson.Delete(migrated.Raw, "id")
			require.NoError(t, err)
			assert.JSONEq(t, `{"name":"`+tc.name+`","plugin":"request-filter","enabled":`+map[bool]string{true: "true", false: "false"}[tc.enabled]+`,
				"global":true,"groups":[],"models":[],
				"config":{"rules":[],"keywords":"Bad\nworse","keywordsCaseSensitive":false,"message":"","replies":false}}`, withoutID)
			revision := optionsRevision(t)
			assert.NotZero(t, revision, "the other nodes reload the options")

			require.NoError(t, MigrateSensitiveWords())
			assert.Equal(t, saved, requireOptionValue(t, db, request_plugin_setting.InstancesOption))
			assert.Equal(t, revision, optionsRevision(t))
		})
	}
}

func TestMigrateSensitiveWordsWithoutWords(t *testing.T) {
	db := useFrontendOptionMigrationDB(t)
	require.NoError(t, db.AutoMigrate(&SystemTask{}))

	// Nothing to move: no write, no reload.
	require.NoError(t, MigrateSensitiveWords())
	assert.Zero(t, optionsRevision(t))

	// Switches without words leave no instance behind.
	require.NoError(t, db.Create(&[]Option{{Key: "CheckSensitiveEnabled", Value: "true"}, {Key: "SensitiveWords", Value: " \n "}}).Error)
	require.NoError(t, MigrateSensitiveWords())
	requireOptionMissing(t, db, "CheckSensitiveEnabled")
	requireOptionMissing(t, db, "SensitiveWords")
	requireOptionMissing(t, db, request_plugin_setting.InstancesOption)

	// An instance list it cannot read keeps everything as it was.
	require.NoError(t, db.Create(&[]Option{{Key: "SensitiveWords", Value: "bad"}, {Key: request_plugin_setting.InstancesOption, Value: "{"}}).Error)
	require.Error(t, MigrateSensitiveWords())
	assert.Equal(t, "bad", requireOptionValue(t, db, "SensitiveWords"))
	assert.Equal(t, "{", requireOptionValue(t, db, request_plugin_setting.InstancesOption))
}

func TestSensitiveWordRollbackReupgradeUsesMigrationReceipt(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := useSystemTaskMatrixDB(t, dialect)
			require.NoError(t, db.AutoMigrate(&Option{}))
			t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(&Option{})) })
			original := map[string]string{"SensitiveWords": "original\nword", "CheckSensitiveEnabled": "true", "CheckSensitiveOnPromptEnabled": "true", "StopOnSensitiveEnabled": "false", "StreamCacheQueueLength": "3"}
			for key, value := range original {
				require.NoError(t, db.Save(&Option{Key: key, Value: value}).Error)
			}
			assert.ErrorContains(t, CheckSensitiveWordsMigration(), "start a master node")
			for key, value := range original {
				assert.Equal(t, value, requireOptionValue(t, db, key), "the follower gate is read-only")
			}
			require.NoError(t, MigrateSensitiveWords())
			require.NoError(t, CheckSensitiveWordsMigration())
			var receipt sensitiveWordMigrationBackup
			require.NoError(t, common.UnmarshalJsonStr(requireOptionValue(t, db, sensitiveWordMigrationBackupOption), &receipt))
			assert.Equal(t, original, receipt.Values, "exact legacy values remain available for an explicit rollback")
			var instances []request_plugin_setting.Instance
			require.NoError(t, common.UnmarshalJsonStr(requireOptionValue(t, db, request_plugin_setting.InstancesOption), &instances))
			require.Len(t, instances, 1)
			instances[0].Name = "edited after migration"
			instances[0].Config = []byte(`{"keywords":"new console words","rules":[],"message":"custom reply"}`)
			encoded, err := common.Marshal(instances)
			require.NoError(t, err)
			require.NoError(t, db.Save(&Option{Key: request_plugin_setting.InstancesOption, Value: string(encoded)}).Error)
			for key, value := range receipt.Values {
				require.NoError(t, db.Save(&Option{Key: key, Value: value}).Error)
			}
			require.NoError(t, MigrateSensitiveWords())
			assert.Equal(t, string(encoded), requireOptionValue(t, db, request_plugin_setting.InstancesOption), "an unchanged rollback/reupgrade preserves new UI edits and never appends a duplicate")
			for key, value := range receipt.Values {
				require.NoError(t, db.Save(&Option{Key: key, Value: value}).Error)
			}
			require.NoError(t, db.Save(&Option{Key: "SensitiveWords", Value: "changed while rolled back"}).Error)
			require.NoError(t, MigrateSensitiveWords())
			require.NoError(t, common.UnmarshalJsonStr(requireOptionValue(t, db, request_plugin_setting.InstancesOption), &instances))
			require.Len(t, instances, 1)
			assert.Equal(t, receipt.InstanceID, instances[0].ID)
			assert.Equal(t, "edited after migration", instances[0].Name)
			assert.Equal(t, "changed while rolled back", gjson.GetBytes(instances[0].Config, "keywords").String())
			assert.Equal(t, "custom reply", gjson.GetBytes(instances[0].Config, "message").String())
			savedReceipt := requireOptionValue(t, db, sensitiveWordMigrationBackupOption)
			require.NoError(t, updateOptionMap(sensitiveWordMigrationBackupOption, savedReceipt))
			common.OptionMapRWMutex.RLock()
			_, visible := common.OptionMap[sensitiveWordMigrationBackupOption]
			common.OptionMapRWMutex.RUnlock()
			assert.False(t, visible, "migration control data is excluded from console options")
			assert.Error(t, UpdateOption(sensitiveWordMigrationBackupOption, "{}"))
			assert.Error(t, UpdateOptionsBulk(map[string]string{sensitiveWordMigrationBackupOption: "{}"}))
			assert.Equal(t, savedReceipt, requireOptionValue(t, db, sensitiveWordMigrationBackupOption))
		})
	}
}
