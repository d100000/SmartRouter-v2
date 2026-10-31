package model

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func TestChannelAvailabilityBoundaries(t *testing.T) {
	daily := `{"enabled":true,"timezone":"Asia/Shanghai","mode":"daily","rules":[{"start":"09:00","end":"12:00"},{"start":"14:00","end":"18:00"}]}`
	weekly := `{"enabled":true,"timezone":"UTC","mode":"weekly","rules":[{"weekdays":[7],"start":"22:00","end":"02:00"}]}`
	exception := `{"enabled":true,"timezone":"UTC","mode":"daily","rules":[{"start":"22:00","end":"02:00"}],"exceptions":[{"date":"2026-11-02","windows":[{"start":"10:00","end":"11:00"}]},{"date":"2026-11-04","windows":[]}]}`
	dst := `{"enabled":true,"timezone":"America/New_York","mode":"daily","rules":[{"start":"01:00","end":"02:30"}]}`
	for _, tc := range []struct {
		name, config, at string
		open             bool
	}{
		{"legacy is unrestricted", "", "2026-11-01T00:00:00Z", true},
		{"timezone before start", daily, "2026-11-01T00:59:59Z", false},
		{"inclusive start", daily, "2026-11-01T01:00:00Z", true},
		{"exclusive end", daily, "2026-11-01T04:00:00Z", false},
		{"second daily window", daily, "2026-11-01T06:00:00Z", true},
		{"weekly start", weekly, "2026-11-01T22:00:00Z", true},
		{"sunday spills to monday", weekly, "2026-11-02T01:59:59Z", true},
		{"overnight end", weekly, "2026-11-02T02:00:00Z", false},
		{"unselected weekday", weekly, "2026-11-02T23:00:00Z", false},
		{"exception overrides yesterday spill", exception, "2026-11-02T01:00:00Z", false},
		{"exception opens own hours", exception, "2026-11-02T10:00:00Z", true},
		{"exception removes own overnight spill", exception, "2026-11-03T01:00:00Z", false},
		{"closed exception overrides overnight", exception, "2026-11-04T01:00:00Z", false},
		{"spring forward before gap", dst, "2026-03-08T06:59:00Z", true},
		{"spring forward skipped hours are closed", dst, "2026-03-08T07:00:00Z", false},
		{"fall back first local hour", dst, "2026-11-01T05:30:00Z", true},
		{"fall back repeated local hour", dst, "2026-11-01T06:30:00Z", true},
		{"damaged configuration fails closed", "{bad", "2026-11-01T00:00:00Z", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tc.at)
			require.NoError(t, err)
			ch := Channel{Status: common.ChannelStatusEnabled, Availability: tc.config}
			assert.Equal(t, tc.open, ch.ScheduleAllowsAt(now))
			ch.PopulateAvailabilityState(now)
			assert.Equal(t, tc.open, ch.AvailabilityState.Open)
			if tc.open {
				assert.Equal(t, common.ChannelStatusEnabled, ch.EffectiveStatus)
			} else {
				assert.Equal(t, common.ChannelStatusOutsideSchedule, ch.EffectiveStatus)
			}
			for _, status := range []int{common.ChannelStatusManuallyDisabled, common.ChannelStatusAutoDisabled} {
				ch.Status = status
				assert.Equal(t, status, ch.EffectiveStatusAt(now), "schedule cannot reopen a disabled channel")
			}
		})
	}
	now, err := time.Parse(time.RFC3339, "2026-03-08T06:59:30Z")
	require.NoError(t, err)
	ch := Channel{Status: common.ChannelStatusEnabled, Availability: dst}
	ch.PopulateAvailabilityState(now)
	assert.Equal(t, now.Add(30*time.Second).Unix(), ch.AvailabilityState.NextChangeAt, "next change follows real time over DST")
}

func TestChannelAvailabilityRejectsInvalidRules(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`,
		`{"enabled":true,"timezone":"Local","mode":"daily","rules":[{"start":"00:00","end":"24:00"}]}`,
		`{"enabled":true,"timezone":"Not/AZone","mode":"daily","rules":[{"start":"00:00","end":"24:00"}]}`,
		`{"enabled":true,"timezone":"UTC","mode":"daily","rules":[]}`,
		`{"enabled":true,"timezone":"UTC","mode":"daily","rules":[{"start":"09:00","end":"09:00"}]}`,
		`{"enabled":true,"timezone":"UTC","mode":"daily","rules":[{"start":"24:00","end":"09:00"}]}`,
		`{"enabled":true,"timezone":"UTC","mode":"weekly","rules":[{"start":"09:00","end":"18:00","weekdays":[]}]}`,
		`{"enabled":true,"timezone":"UTC","mode":"weekly","rules":[{"start":"09:00","end":"18:00","weekdays":[0]}]}`,
		`{"enabled":true,"timezone":"UTC","mode":"daily","rules":[{"start":"09:00","end":"18:00"}],"exceptions":[{"date":"2026-02-30","windows":[]}]}`,
		`{"enabled":true,"timezone":"UTC","mode":"daily","rules":[{"start":"09:00","end":"18:00"}],"exceptions":[{"date":"2026-11-01","windows":[]},{"date":"2026-11-01","windows":[]}]}`,
		`{"enabled":true,"timezone":"UTC","mode":"daily","rules":[{"start":"09:00","end":"18:00"}],"exceptions":[{"date":"2026-11-01","windows":[{"start":"23:00","end":"01:00"}]}]}`,
	} {
		t.Run(raw, func(t *testing.T) { ch := Channel{Availability: raw}; require.Error(t, ch.ValidateAvailability()) })
	}
}

// The released v1.0.0-rc.47 channel schema, including its indexes and defaults.
// Keep it independent of Channel so this fixture continues to exercise upgrades.
type availabilityReleasedChannel struct {
	Id                 int     `json:"id"`
	Type               int     `json:"type" gorm:"default:0"`
	Key                string  `json:"key" gorm:"not null"`
	OpenAIOrganization *string `json:"openai_organization"`
	TestModel          *string `json:"test_model"`
	Status             int     `json:"status" gorm:"default:1"`
	Name               string  `json:"name" gorm:"index"`
	Weight             *uint   `json:"weight" gorm:"default:0"`
	CreatedTime        int64   `json:"created_time" gorm:"bigint"`
	TestTime           int64   `json:"test_time" gorm:"bigint"`
	ResponseTime       int     `json:"response_time"` // in milliseconds
	BaseURL            *string `json:"base_url" gorm:"column:base_url;default:''"`
	Other              string  `json:"other"`
	Balance            float64 `json:"balance"` // in USD
	BalanceUpdatedTime int64   `json:"balance_updated_time" gorm:"bigint"`
	Models             string  `json:"models"`
	Group              string  `json:"group" gorm:"type:varchar(64);default:'default'"`
	UsedQuota          int64   `json:"used_quota" gorm:"bigint;default:0"`
	ModelMapping       *string `json:"model_mapping" gorm:"type:text"`
	//MaxInputTokens     *int    `json:"max_input_tokens" gorm:"default:0"`
	StatusCodeMapping *string `json:"status_code_mapping" gorm:"type:varchar(1024);default:''"`
	Priority          *int64  `json:"priority" gorm:"bigint;default:0"`
	AutoBan           *int    `json:"auto_ban" gorm:"default:1"`
	OtherInfo         string  `json:"other_info"`
	Tag               *string `json:"tag" gorm:"index"`
	Setting           *string `json:"setting" gorm:"type:text"` // 渠道额外设置
	ParamOverride     *string `json:"param_override" gorm:"type:text"`
	HeaderOverride    *string `json:"header_override" gorm:"type:text"`
	Remark            *string `json:"remark" gorm:"type:varchar(255)" validate:"max=255"`
	// add after v0.8.5
	ChannelInfo ChannelInfo `json:"channel_info" gorm:"type:json"`

	OtherSettings string `json:"settings" gorm:"column:settings"` // 其他设置，存储azure版本等不需要检索的信息，详见dto.ChannelOtherSettings

}

func TestChannelAvailabilityMigrationMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(":memory:")
			case "mysql":
				dsn := os.Getenv("TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("TEST_MYSQL_DSN not configured")
				}
				driver = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TEST_POSTGRES_DSN not configured")
				}
				driver = postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
			}
			prefix := fmt.Sprintf("availability_%d_", time.Now().UnixNano())
			recorder := &migrationSQLRecorder{}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: prefix}, Logger: recorder})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			var version string
			query := "SELECT version()"
			if dialect == "sqlite" {
				query = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(query).Scan(&version).Error)
			t.Logf("%s: %s", dialect, version)
			for _, upgrade := range []bool{false, true} {
				t.Run(fmt.Sprint("upgrade=", upgrade), func(t *testing.T) {
					require.NoError(t, db.Migrator().DropTable(&Channel{}))
					t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(&Channel{})) })
					var previous availabilityReleasedChannel
					if upgrade {
						require.NoError(t, db.Table(prefix+"channels").AutoMigrate(&availabilityReleasedChannel{}))
						previous = availabilityReleasedChannel{Name: "preserved-channel", Key: "fixture-key", Status: common.ChannelStatusManuallyDisabled, Models: "schedule-model", Group: "default", UsedQuota: 42, Tag: common.GetPointer("preserved-tag"), OtherSettings: `{"allow_service_tier":true}`}
						require.NoError(t, db.Table(prefix+"channels").Create(&previous).Error)
					}
					require.NoError(t, db.AutoMigrate(&Channel{}))
					recorder.reset()
					require.NoError(t, db.AutoMigrate(&Channel{}))
					assert.Empty(t, recorder.schemaMutations(), "second migration must not mutate the schema")
					assert.True(t, db.Migrator().HasIndex(&Channel{}, "Name"))
					assert.True(t, db.Migrator().HasIndex(&Channel{}, "Tag"))
					if upgrade {
						var stored Channel
						require.NoError(t, db.First(&stored, "id = ?", previous.Id).Error)
						assert.Equal(t, previous.Key, stored.Key)
						assert.Equal(t, previous.UsedQuota, stored.UsedQuota)
						assert.Equal(t, previous.OtherSettings, stored.OtherSettings)
						assert.Equal(t, previous.Status, stored.Status)
						assert.Empty(t, stored.Availability)
						assert.True(t, stored.ScheduleAllowsAt(time.Now()))
					}
					raw := `{"enabled":true,"timezone":"UTC","mode":"daily","rules":[{"start":"22:00","end":"02:00"}],"exceptions":[{"date":"2026-11-01","windows":[]}]}`
					channel := Channel{Name: "scheduled", Key: "fixture-key", Status: common.ChannelStatusEnabled, Availability: raw}
					require.NoError(t, db.Create(&channel).Error)
					var stored Channel
					require.NoError(t, db.First(&stored, "id = ?", channel.Id).Error)
					assert.Equal(t, raw, stored.Availability)
					require.NoError(t, db.Model(&stored).Update("availability", "{}").Error)
					require.NoError(t, db.First(&stored, "id = ?", channel.Id).Error)
					assert.True(t, stored.ScheduleAllowsAt(time.Now()))
				})
			}
		})
	}
}
