package model

import (
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const (
	schedulingConfigOption        = "SchedulingConfig"
	schedulingConfigVersionOption = "SchedulingConfigVersion"
	// Version 2 reads a zero default capacity as unlimited.
	schedulingConfigVersion = "2"
	// Version 1 saved its built-in default of 100 as an explicit capacity.
	legacySchedulingDefaultCapacity = "100"
)

// MigrateSchedulingDefaultCapacity runs once on a master node before options
// load. Saved group/model defaults of 100 were the former built-in value, so
// they become unlimited. Channel overrides and supplier pools keep their
// explicit limits. It returns the changed "group/model" dimensions.
func MigrateSchedulingDefaultCapacity() ([]string, error) {
	if DB == nil {
		return nil, errors.New("database is not initialized")
	}
	var changed []string
	migrated := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var version Option
		err := tx.Where(&Option{Key: schedulingConfigVersionOption}).First(&version).Error
		if err == nil && version.Value == schedulingConfigVersion {
			return nil
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var config Option
		err = tx.Where(&Option{Key: schedulingConfigOption}).First(&config).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if strings.TrimSpace(config.Value) != "" {
			// Preserve IDs and unknown fields without converting JSON numbers to floats.
			var entries []map[string]common.RawMessage
			if err := common.UnmarshalJsonStr(config.Value, &entries); err != nil {
				return fmt.Errorf("read scheduling configuration: %w", err)
			}
			for _, entry := range entries {
				if string(entry["default_capacity"]) == legacySchedulingDefaultCapacity {
					entry["default_capacity"] = common.RawMessage("0")
					changed = append(changed, common.JsonRawMessageToString(entry["group"])+"/"+common.JsonRawMessageToString(entry["model"]))
				}
			}
			if len(changed) > 0 {
				encoded, err := common.Marshal(entries)
				if err != nil {
					return err
				}
				if err := tx.Save(&Option{Key: schedulingConfigOption, Value: string(encoded)}).Error; err != nil {
					return err
				}
			}
		}
		if err := tx.Save(&Option{Key: schedulingConfigVersionOption, Value: schedulingConfigVersion}).Error; err != nil {
			return err
		}
		migrated = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if migrated {
		BumpSyncTopic(SyncTopicOptions)
	}
	if len(changed) > 0 {
		common.SysLog(common.LogText("scheduling default capacity changed from 100 to unlimited: %s", strings.Join(changed, ", ")))
	}
	return changed, nil
}

// RecordSchedulingCapacityMigration writes the manage log for a migration
// once the log database is available.
func RecordSchedulingCapacityMigration(changed []string) {
	if len(changed) == 0 {
		return
	}
	RecordLog(0, LogTypeManage, common.NewMessage("Scheduling default capacity changed from 100 to unlimited: {{dimensions}}", map[string]any{"dimensions": strings.Join(changed, ", ")}))
}
