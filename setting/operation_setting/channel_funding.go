package operation_setting

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/setting/config"
)

type ChannelFundingSetting struct {
	Enabled        bool `json:"enabled"`
	ProbeMinutes   int  `json:"probe_minutes"`
	ReminderHours  int  `json:"reminder_hours"`
	RecoveryNotify bool `json:"recovery_notify"`
}

var channelFundingSetting = ChannelFundingSetting{ProbeMinutes: 10, ReminderHours: 24, RecoveryNotify: true}

func init() { config.GlobalConfig.Register("channel_funding_setting", &channelFundingSetting) }

func GetChannelFundingSetting() ChannelFundingSetting {
	return config.Snapshot[ChannelFundingSetting](config.GlobalConfig, "channel_funding_setting")
}

func ValidateChannelFundingOption(key, value string) error {
	field, ok := strings.CutPrefix(key, "channel_funding_setting.")
	if !ok {
		return nil
	}
	switch field {
	case "enabled", "recovery_notify":
		if _, err := strconv.ParseBool(value); err == nil {
			return nil
		}
	case "probe_minutes":
		if n, err := strconv.Atoi(value); err == nil && n >= 1 && n <= 60 {
			return nil
		}
	case "reminder_hours":
		if n, err := strconv.Atoi(value); err == nil && n >= 1 && n <= 720 {
			return nil
		}
	}
	return fmt.Errorf("Invalid channel funding monitoring setting")
}
