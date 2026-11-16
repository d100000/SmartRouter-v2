package model

import (
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/request_plugin_setting"
	"gorm.io/gorm"
)

// retiredSensitiveOptionKeys are the options of the retired sensitive word
// check. Its words now live in a request-filter instance.
var retiredSensitiveOptionKeys = []string{
	"CheckSensitiveEnabled",
	"CheckSensitiveOnPromptEnabled",
	"StopOnSensitiveEnabled",
	"SensitiveWords",
	"StreamCacheQueueLength",
}

// The receipt retains the exact retired values for an operator-directed
// rollback and identifies the migrated instance on a later re-upgrade.
const sensitiveWordMigrationBackupOption = "sensitive_word_migration.backup"

type sensitiveWordMigrationBackup struct {
	Values     map[string]string `json:"values"`
	InstanceID string            `json:"instance_id"`
}

// CheckSensitiveWordsMigration keeps followers from accepting requests with
// retired filters before a master has migrated their shared configuration.
// It performs no writes, including when the migration is still pending.
func CheckSensitiveWordsMigration() error {
	if DB == nil {
		return errors.New("database is not initialized")
	}
	var pending int64
	if err := DB.Model(&Option{}).Where(commonKeyCol+" IN ?", retiredSensitiveOptionKeys).Count(&pending).Error; err != nil {
		return fmt.Errorf("check sensitive word migration: %w", err)
	}
	if pending > 0 {
		return errors.New("sensitive word options are pending migration; start a master node before this follower")
	}
	return nil
}

// MigrateSensitiveWords moves the words of the retired sensitive word check
// into one global request-filter instance and deletes the check's options.
// The instance is enabled when the check ran on prompts: both switches on.
// One transaction does both, so a later start finds nothing to move.
func MigrateSensitiveWords() error {
	if DB == nil {
		return errors.New("database is not initialized")
	}
	changed := false
	name := ""
	err := DB.Transaction(func(tx *gorm.DB) error {
		values := make(map[string]string, len(retiredSensitiveOptionKeys))
		var found []string
		for _, key := range retiredSensitiveOptionKeys {
			var option Option
			err := tx.Where(&Option{Key: key}).First(&option).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			if err != nil {
				return fmt.Errorf("read option %s: %w", key, err)
			}
			values[key] = option.Value
			found = append(found, key)
		}
		if len(found) == 0 {
			return nil
		}
		var saved Option
		backup := sensitiveWordMigrationBackup{}
		err := tx.Where(&Option{Key: sensitiveWordMigrationBackupOption}).First(&saved).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if saved.Value != "" {
			if err := common.UnmarshalJsonStr(saved.Value, &backup); err != nil {
				return fmt.Errorf("read sensitive word migration receipt: %w", err)
			}
		}
		// Restoring the same old options must not duplicate the instance or
		// overwrite edits made in the new request-filter UI.
		if !maps.Equal(values, backup.Values) {
			words := sensitiveWordLines(values["SensitiveWords"])
			enabled := values["CheckSensitiveEnabled"] == "true" && values["CheckSensitiveOnPromptEnabled"] == "true"
			name = sensitiveWordInstanceName()
			id, err := addSensitiveWordInstance(tx, name, words, enabled, backup.InstanceID)
			if err != nil {
				return err
			}
			backup = sensitiveWordMigrationBackup{Values: values, InstanceID: id}
			encoded, err := common.Marshal(backup)
			if err != nil {
				return err
			}
			if err := tx.Save(&Option{Key: sensitiveWordMigrationBackupOption, Value: string(encoded)}).Error; err != nil {
				return err
			}
			if id == "" {
				name = ""
			}
		}
		for _, key := range found {
			if err := tx.Delete(&Option{Key: key}).Error; err != nil {
				return fmt.Errorf("delete option %s: %w", key, err)
			}
		}
		changed = true
		return nil
	})
	if err != nil {
		return err
	}
	if changed {
		BumpSyncTopic(SyncTopicOptions)
	}
	if name != "" {
		common.SysLog(common.LogText("moved the sensitive words to the global request filter instance %s", name))
	}
	return nil
}

// sensitiveWordLines is the old word list, one word per line, as the check
// read it: lines trimmed, empty lines dropped.
func sensitiveWordLines(value string) string {
	var words []string
	for line := range strings.SplitSeq(value, "\n") {
		if word := strings.TrimSpace(line); word != "" {
			words = append(words, word)
		}
	}
	return strings.Join(words, "\n")
}

func sensitiveWordInstanceName() string {
	if language := strings.ToLower(strings.TrimSpace(common.DefaultLanguage)); language != "" && !strings.HasPrefix(language, "zh") {
		return "Sensitive words"
	}
	return "敏感词"
}

// addSensitiveWordInstance appends the instance to the saved instance list.
// The old check matched words in any letter case.
func addSensitiveWordInstance(tx *gorm.DB, name, words string, enabled bool, migratedID string) (string, error) {
	var option Option
	err := tx.Where(&Option{Key: request_plugin_setting.InstancesOption}).First(&option).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", fmt.Errorf("read option %s: %w", request_plugin_setting.InstancesOption, err)
	}
	var instances []request_plugin_setting.Instance
	if strings.TrimSpace(option.Value) != "" {
		if err := common.UnmarshalJsonStr(option.Value, &instances); err != nil {
			return "", fmt.Errorf("read option %s: %w", request_plugin_setting.InstancesOption, err)
		}
	}
	ids := make(map[string]bool, len(instances))
	index := -1
	for i, instance := range instances {
		ids[instance.ID] = true
		if migratedID != "" && instance.ID == migratedID && instance.Plugin == "request-filter" {
			index = i
		}
	}
	if index < 0 && words == "" {
		return "", nil
	}
	id := migratedID
	if index < 0 {
		for id == "" || ids[id] {
			id = "rp_" + strings.ToLower(rand.Text()[:8])
		}
	}
	config, err := common.Marshal(map[string]any{
		"rules":                 []any{},
		"keywords":              words,
		"keywordsCaseSensitive": false,
		"message":               "",
		"replies":               false,
	})
	if err != nil {
		return "", err
	}
	if index >= 0 {
		var savedConfig map[string]any
		if err := common.Unmarshal(instances[index].Config, &savedConfig); err != nil {
			return "", err
		}
		if savedConfig == nil {
			savedConfig = make(map[string]any)
		}
		savedConfig["keywords"] = words
		config, err = common.Marshal(savedConfig)
		if err != nil {
			return "", err
		}
		instances[index].Config = config
		instances[index].Enabled = enabled
	} else {
		instances = append(instances, request_plugin_setting.Instance{
			ID:      id,
			Name:    name,
			Plugin:  "request-filter",
			Enabled: enabled,
			Global:  true,
			Groups:  []string{},
			Models:  []string{},
			Config:  config,
		})
	}
	encoded, err := common.Marshal(instances)
	if err != nil {
		return "", err
	}
	option.Key = request_plugin_setting.InstancesOption
	option.Value = string(encoded)
	if err := tx.Save(&option).Error; err != nil {
		return "", fmt.Errorf("write option %s: %w", request_plugin_setting.InstancesOption, err)
	}
	return id, nil
}
