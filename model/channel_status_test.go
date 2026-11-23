package model

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// channelsRevision is the id of the newest channels sync task, 0 without one.
func channelsRevision(t *testing.T) int64 {
	t.Helper()
	revisions, err := GetLatestSyncRevisions()
	require.NoError(t, err)
	return revisions[SyncTopicChannels]
}

func setupChannelStatusTest(t *testing.T) {
	t.Helper()
	truncateTables(t)
	require.NoError(t, DB.Exec("DELETE FROM abilities").Error)
	require.NoError(t, DB.Exec("DELETE FROM channels").Error)

	memoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = memoryCacheEnabled
	})
}

func TestUpdateChannelStatusPersistsMultiKeyState(t *testing.T) {
	setupChannelStatusTest(t)

	channel := Channel{
		Name:   "multi-key-status",
		Key:    "key-a\nkey-b",
		Status: common.ChannelStatusEnabled,
		ChannelInfo: ChannelInfo{
			IsMultiKey:           true,
			MultiKeySize:         2,
			MultiKeyMode:         constant.MultiKeyModePolling,
			MultiKeyPollingIndex: 1,
		},
	}
	require.NoError(t, DB.Create(&channel).Error)

	changed := UpdateChannelStatus(channel.Id, "key-a", common.ChannelStatusAutoDisabled, "provider rejected key")
	require.True(t, changed)

	var stored Channel
	require.NoError(t, DB.First(&stored, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[0])
	assert.Equal(t, "provider rejected key", stored.ChannelInfo.MultiKeyDisabledReason[0])
	assert.NotZero(t, stored.ChannelInfo.MultiKeyDisabledTime[0])
	assert.Equal(t, 1, stored.ChannelInfo.MultiKeyPollingIndex)
}

// writeAfterNextChannelRead stands for another node: right after the next
// read of the channel, before the writer under test writes, it stores the
// columns write returns for the channel it reads.
func writeAfterNextChannelRead(t *testing.T, channelID int, write func(stored *Channel) map[string]any) {
	t.Helper()
	var done atomic.Bool
	require.NoError(t, DB.Callback().Query().After("gorm:query").Register("test:other_node_write", func(tx *gorm.DB) {
		if !strings.HasSuffix(tx.Statement.Table, "channels") || !done.CompareAndSwap(false, true) {
			return
		}
		var stored Channel
		require.NoError(t, DB.First(&stored, "id = ?", channelID).Error)
		require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channelID).Updates(write(&stored)).Error)
	}))
	t.Cleanup(func() { _ = DB.Callback().Query().Remove("test:other_node_write") })
}

// A writer of other_info writes back only while no other writer, on any node,
// changed it since the read; otherwise it reads again, so both changes are
// kept. It writes its own columns only, so a stale read never puts back the
// key, counters, models or channel_info another writer stored.
func TestChannelOtherInfoWritesKeepConcurrentChanges(t *testing.T) {
	useScheduledChannelDatabase(t)
	common.MemoryCacheEnabled = false

	t.Run("a status change and a balance query of another node", func(t *testing.T) {
		channel := Channel{Name: "status-race", Key: "original-key", Status: common.ChannelStatusEnabled, Models: "original-model", Group: "default", UsedQuota: 100}
		require.NoError(t, channel.Insert())
		concurrentInfo := ChannelInfo{MultiKeyMode: constant.MultiKeyModePolling, MultiKeyPollingIndex: 1}
		// The status path locks and reloads the row in its transaction. Another
		// node's committed balance and credential changes must survive, even
		// though the caller still holds its older channel snapshot. Injecting
		// a synchronous second write from the locked read would deadlock.
		var concurrent Channel
		require.NoError(t, DB.First(&concurrent, channel.Id).Error)
		info := concurrent.GetOtherInfo()
		info[channelBalanceDetailKey] = map[string]any{"amount": 7, "currency": "USD"}
		concurrent.SetOtherInfo(info)
		require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).Updates(map[string]any{
			"other_info":   concurrent.OtherInfo,
			"key":          "rotated-key",
			"used_quota":   gorm.Expr("used_quota + ?", 250),
			"models":       "concurrent-model",
			"channel_info": concurrentInfo,
		}).Error)

		require.True(t, UpdateChannelStatus(channel.Id, "", common.ChannelStatusManuallyDisabled, "manual operation"))

		var stored Channel
		require.NoError(t, DB.First(&stored, channel.Id).Error)
		assert.Equal(t, common.ChannelStatusManuallyDisabled, stored.Status)
		assert.Equal(t, "manual operation", stored.GetOtherInfo()["status_reason"])
		detail := stored.GetBalanceDetail()
		require.NotNil(t, detail, stored.OtherInfo)
		require.NotNil(t, detail.Amount)
		assert.Equal(t, 7.0, *detail.Amount)
		assert.Equal(t, "rotated-key", stored.Key)
		assert.Equal(t, int64(350), stored.UsedQuota)
		assert.Equal(t, "concurrent-model", stored.Models)
		assert.Equal(t, concurrentInfo, stored.ChannelInfo)
	})

	t.Run("a balance query and a status change of another node", func(t *testing.T) {
		channel := Channel{Name: "balance-race", Key: "key", Status: common.ChannelStatusEnabled, Models: "balance-model", Group: "default"}
		require.NoError(t, channel.Insert())
		// A channel stored before other_info was written has it NULL.
		require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).Update("other_info", gorm.Expr("NULL")).Error)
		writeAfterNextChannelRead(t, channel.Id, func(stored *Channel) map[string]any {
			info := stored.GetOtherInfo()
			info["status_reason"] = "other node"
			stored.SetOtherInfo(info)
			return map[string]any{"other_info": stored.OtherInfo, "status": common.ChannelStatusManuallyDisabled}
		})
		amount := 3.5
		detail, err := UpdateChannelBalanceDetail(channel.Id, func(*ChannelBalanceDetail) (*ChannelBalanceDetail, *float64) {
			return &ChannelBalanceDetail{BalanceResult: jsplugin.BalanceResult{Amount: &amount, Currency: "USD"}}, &amount
		})
		require.NoError(t, err)
		require.NotNil(t, detail)

		var stored Channel
		require.NoError(t, DB.First(&stored, channel.Id).Error)
		assert.Equal(t, common.ChannelStatusManuallyDisabled, stored.Status)
		assert.Equal(t, "other node", stored.GetOtherInfo()["status_reason"])
		assert.Equal(t, 3.5, stored.Balance)
		assert.NotZero(t, stored.BalanceUpdatedTime)
		require.NotNil(t, stored.GetBalanceDetail())
		assert.Equal(t, 3.5, *stored.GetBalanceDetail().Amount)

		// Storing the same failure twice writes the same values again, which
		// MySQL reports as no changed row.
		for range 2 {
			_, err = UpdateChannelBalanceDetail(channel.Id, func(stored *ChannelBalanceDetail) (*ChannelBalanceDetail, *float64) {
				stored.Error = "upstream failed"
				return stored, nil
			})
			require.NoError(t, err)
		}
		require.NoError(t, DB.First(&stored, channel.Id).Error)
		assert.Equal(t, "upstream failed", stored.GetBalanceDetail().Error)
		assert.Equal(t, "other node", stored.GetOtherInfo()["status_reason"])
	})
}

func TestAutoDisableKeepsManualDisable(t *testing.T) {
	for _, cacheEnabled := range []bool{false, true} {
		for _, tc := range []struct {
			name          string
			multiKey      bool
			initialStatus int
			wantChanged   bool
			wantStatus    int
		}{
			{name: "single key manual disable is kept", initialStatus: common.ChannelStatusManuallyDisabled, wantStatus: common.ChannelStatusManuallyDisabled},
			{name: "multi key manual disable is kept", multiKey: true, initialStatus: common.ChannelStatusManuallyDisabled, wantStatus: common.ChannelStatusManuallyDisabled},
			{name: "single key enabled is auto disabled", initialStatus: common.ChannelStatusEnabled, wantChanged: true, wantStatus: common.ChannelStatusAutoDisabled},
			{name: "multi key enabled is auto disabled", multiKey: true, initialStatus: common.ChannelStatusEnabled, wantChanged: true, wantStatus: common.ChannelStatusAutoDisabled},
		} {
			t.Run(fmt.Sprintf("cache=%t/%s", cacheEnabled, tc.name), func(t *testing.T) {
				setupChannelStatusTest(t)
				common.MemoryCacheEnabled = cacheEnabled

				channel := Channel{Name: t.Name(), Key: "key-a", Status: tc.initialStatus, Models: "test-model", Group: "default"}
				usingKey := "key-a"
				if tc.multiKey {
					// key-b already failed, so a failure on key-a disables every key.
					channel.Key = "key-a\nkey-b"
					channel.ChannelInfo = ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyStatusList: map[int]int{1: common.ChannelStatusAutoDisabled}}
				}
				channel.SetOtherInfo(map[string]any{"status_reason": "manual operation", "status_time": int64(1234)})
				require.NoError(t, channel.Insert())
				InitChannelCache()

				changed := UpdateChannelStatus(channel.Id, usingKey, common.ChannelStatusAutoDisabled, "provider rejected key")
				assert.Equal(t, tc.wantChanged, changed)

				stored, err := GetChannelById(channel.Id, true)
				require.NoError(t, err)
				assert.Equal(t, tc.wantStatus, stored.Status)
				if !tc.wantChanged {
					assert.Equal(t, "manual operation", stored.GetOtherInfo()["status_reason"])
					assert.Equal(t, float64(1234), stored.GetOtherInfo()["status_time"])
					assert.Equal(t, channel.ChannelInfo.MultiKeyStatusList, stored.ChannelInfo.MultiKeyStatusList)
				}
				var abilities []Ability
				require.NoError(t, DB.Where("channel_id = ?", channel.Id).Find(&abilities).Error)
				require.NotEmpty(t, abilities)
				for _, ability := range abilities {
					assert.False(t, ability.Enabled)
				}
				if cacheEnabled {
					cached, err := CacheGetChannel(channel.Id)
					require.NoError(t, err)
					assert.Equal(t, tc.wantStatus, cached.Status)
					assert.Equal(t, stored.ChannelInfo.MultiKeyStatusList, cached.ChannelInfo.MultiKeyStatusList)
				}
			})
		}
	}
}

// A status set by hand reaches every node at once through the channels topic.
// An automatic change, made for the key a request or a channel test used,
// bumps it too, coalesced with the other automatic changes of a burst.
func TestUpdateChannelStatusBumpsChannels(t *testing.T) {
	for _, cacheEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t", cacheEnabled), func(t *testing.T) {
			setupChannelStatusTest(t)
			common.MemoryCacheEnabled = cacheEnabled
			revision := func() int64 { return channelsRevision(t) }
			single := Channel{Name: "status-single", Key: "key-a", Status: common.ChannelStatusEnabled, Models: "status-model", Group: "default"}
			multi := Channel{Name: "status-multi", Key: "key-a\nkey-b", Status: common.ChannelStatusEnabled, Models: "status-model", Group: "default",
				ChannelInfo: ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}}
			require.NoError(t, single.Insert())
			require.NoError(t, multi.Insert())
			InitChannelCache()

			for _, step := range []struct {
				name     string
				id       int
				usingKey string
				status   int
				bump     bool
			}{
				{name: "a request disables a key", id: multi.Id, usingKey: "key-a", status: common.ChannelStatusAutoDisabled},
				{name: "a request disables a single-key channel", id: single.Id, usingKey: "key-a", status: common.ChannelStatusAutoDisabled},
				{name: "a test re-enables the channel", id: single.Id, usingKey: "key-a", status: common.ChannelStatusEnabled},
				{name: "an admin disables a channel", id: single.Id, status: common.ChannelStatusManuallyDisabled, bump: true},
				{name: "an admin disables it again", id: single.Id, status: common.ChannelStatusManuallyDisabled},
				{name: "an admin enables a channel", id: single.Id, status: common.ChannelStatusEnabled, bump: true},
				{name: "an admin disables a multi-key channel", id: multi.Id, status: common.ChannelStatusManuallyDisabled, bump: true},
			} {
				before := revision()
				changed := UpdateChannelStatus(step.id, step.usingKey, step.status, step.name)
				assert.Equal(t, step.bump || step.usingKey != "", changed, step.name)
				assert.Equal(t, step.bump, before != revision(), step.name)
				automatic := step.usingKey != ""
				before = revision()
				flushChannelStatusBump()
				assert.Equal(t, automatic, before != revision(), "%s: coalesced bump", step.name)
			}
			// A burst of automatic changes adds one sync task.
			other := Channel{Name: "status-other", Key: "key-c", Status: common.ChannelStatusEnabled, Models: "status-model", Group: "default"}
			require.NoError(t, other.Insert())
			before := revision()
			require.True(t, UpdateChannelStatus(other.Id, "key-c", common.ChannelStatusAutoDisabled, "burst"))
			require.True(t, UpdateChannelStatus(single.Id, "key-a", common.ChannelStatusAutoDisabled, "burst"))
			flushChannelStatusBump()
			flushChannelStatusBump()
			var tasks int64
			require.NoError(t, DB.Model(&SystemTask{}).Where("type = ? AND id > ?", SyncTaskType(SyncTopicChannels), before).Count(&tasks).Error)
			assert.EqualValues(t, 1, tasks)
		})
	}
}

// The master runs a plugin's scheduled hook when a key is disabled
// automatically, so a follower's automatic disable of such a key bumps the
// channels topic; the master's own, or one for a plugin without the hook,
// does not.
func TestAutoDisableOfAScheduledPluginKeyBumpsChannelsOnFollowers(t *testing.T) {
	setupChannelStatusTest(t)
	master := common.IsMasterNode
	t.Cleanup(func() { common.IsMasterNode = master })
	for key, hooks := range map[string]string{
		"status-scheduled": `export function scheduled() { return {next: 3600}; }`,
		"status-plain":     "",
	} {
		_, err := jsplugin.DefaultRegistry.Register(`export const meta = {apiVersion: 1, key: "`+key+`", name: "Status", version: "1.0.0", author: {name: "Test"}, relay: [{incoming_path: "/v1/chat/completions", upstream_path: "/chat"}]};`+hooks, jsplugin.Options{})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, jsplugin.DefaultRegistry.Unregister(key)) })
	}
	revision := func() int64 { return channelsRevision(t) }

	for _, tc := range []struct {
		plugin string
		master bool
		bump   bool
	}{
		{plugin: "status-scheduled", bump: true},
		{plugin: "status-scheduled", master: true},
		{plugin: "status-plain"},
	} {
		t.Run(fmt.Sprintf("%s/master=%t", tc.plugin, tc.master), func(t *testing.T) {
			common.IsMasterNode = tc.master
			channel := Channel{Type: constant.ChannelTypeTaskPlugin, Name: t.Name(), Key: "key-a", Status: common.ChannelStatusEnabled, Models: "status-model", Group: "default"}
			channel.SetSetting(dto.ChannelSettings{TaskPluginKey: tc.plugin})
			require.NoError(t, channel.Insert())

			before := revision()
			require.True(t, UpdateChannelStatus(channel.Id, "key-a", common.ChannelStatusAutoDisabled, "quota"))
			assert.Equal(t, tc.bump, before != revision())
		})
	}
}

func TestBatchSetChannelTagDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "channel-tags.db"))
			case "mysql":
				dsn := os.Getenv("TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("TEST_MYSQL_DSN is not configured")
				}
				driver = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TEST_POSTGRES_DSN is not configured")
				}
				driver = postgres.Open(dsn)
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "channel_tag_test_"}})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(2)
			previousDB, previousType := DB, common.MainDatabaseType()
			DB = db
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			initCol()
			t.Cleanup(func() {
				DB = previousDB
				common.SetMainDatabaseType(previousType)
				initCol()
				assert.NoError(t, db.Migrator().DropTable(&Ability{}, &Channel{}, &SystemTask{}))
				assert.NoError(t, sqlDB.Close())
			})
			require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}, &SystemTask{}))
			var version string
			versionQuery := "SELECT version()"
			if dialect == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("database version: %s", version)

			oldTag, newTag := "old-tag", "new-tag"
			priority, weight := int64(7), uint(23)
			channels := []Channel{
				{Name: "enabled", Status: common.ChannelStatusEnabled},
				{Name: "disabled", Status: common.ChannelStatusAutoDisabled},
				{Name: "unselected", Status: common.ChannelStatusEnabled},
			}
			for i := range channels {
				channel := &channels[i]
				channel.Models, channel.Group = "model-a,model-b", "default,second"
				channel.Tag, channel.Priority, channel.Weight = &oldTag, &priority, &weight
				require.NoError(t, db.Create(channel).Error)
				require.NoError(t, channel.AddAbilities(db))
			}
			ids := []int{channels[0].Id, channels[1].Id}
			for _, tc := range []struct {
				name string
				tag  *string
			}{
				{name: "set", tag: &newTag},
				{name: "clear", tag: nil},
			} {
				t.Run(tc.name, func(t *testing.T) {
					require.NoError(t, BatchSetChannelTag(ids, tc.tag))
					for i, channel := range channels {
						wantTag := tc.tag
						if i == 2 {
							wantTag = &oldTag
						}
						var stored Channel
						require.NoError(t, db.First(&stored, channel.Id).Error)
						assert.Equal(t, wantTag, stored.Tag, channel.Name)
						assert.Equal(t, channel.Status, stored.Status, channel.Name)
						var abilities []Ability
						require.NoError(t, db.Where("channel_id = ?", channel.Id).Find(&abilities).Error)
						require.Len(t, abilities, 4)
						for _, ability := range abilities {
							assert.Equal(t, wantTag, ability.Tag, channel.Name)
							assert.Equal(t, channel.Status == common.ChannelStatusEnabled, ability.Enabled, channel.Name)
							assert.Equal(t, channel.Priority, ability.Priority, channel.Name)
							assert.Equal(t, weight, ability.Weight, channel.Name)
						}
					}
				})
			}
		})
	}
}

// A periodic reload that finds the same routing rows keeps the scheduling
// metadata version, and a failed read keeps the previously published cache.
// During an outage every failing in-flight request reports the same channel.
// Only the first report changes anything and republishes routing membership.
func TestRepeatedStatusReportsPublishRoutingMetadataOnce(t *testing.T) {
	for _, cached := range []bool{true, false} {
		t.Run(fmt.Sprintf("memory-cache=%t", cached), func(t *testing.T) {
			setupChannelStatusTest(t)
			common.MemoryCacheEnabled = cached
			channel := &Channel{Id: 9111, Name: "repeated", Type: 1, Key: "key", Status: common.ChannelStatusEnabled, Group: "default", Models: "repeated-model"}
			require.NoError(t, DB.Create(channel).Error)
			require.NoError(t, channel.AddAbilities(DB))
			InitChannelCache()
			version := RoutingMetadataVersion()

			require.True(t, UpdateChannelStatus(channel.Id, "", common.ChannelStatusAutoDisabled, "upstream down"))
			changed := RoutingMetadataVersion()
			assert.Greater(t, changed, version, "the status change is published")
			for range 20 {
				assert.False(t, UpdateChannelStatus(channel.Id, "", common.ChannelStatusAutoDisabled, "upstream down"))
			}
			assert.Equal(t, changed, RoutingMetadataVersion(), "repeated reports of a known status publish nothing")
			if cached {
				stored, err := CacheGetChannel(channel.Id)
				require.NoError(t, err)
				assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)

				// Another node re-enabled the channel: this node's cache is
				// stale, so a report that changes nothing in the database
				// must still bring the cache in line.
				require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).Update("status", common.ChannelStatusEnabled).Error)
				require.False(t, EnableChannelAutomatically(channel.Id, ""))
				stored, err = CacheGetChannel(channel.Id)
				require.NoError(t, err)
				assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
				assert.Greater(t, RoutingMetadataVersion(), changed, "bringing a stale cache in line is published")
			}
		})
	}
}

func TestChannelCacheReloadPublishesOnlyChangedRoutingMetadata(t *testing.T) {
	setupChannelStatusTest(t)
	common.MemoryCacheEnabled = true
	channel := &Channel{Id: 9101, Name: "reload", Type: 1, Key: "key", Status: common.ChannelStatusEnabled, Group: "default", Models: "reload-model"}
	require.NoError(t, DB.Create(channel).Error)
	require.NoError(t, channel.AddAbilities(DB))
	InitChannelCache()
	version := RoutingMetadataVersion()

	InitChannelCache()
	assert.Equal(t, version, RoutingMetadataVersion(), "an unchanged reload keeps the version")

	require.NoError(t, DB.Model(&Ability{}).Where("channel_id = ?", channel.Id).Update("enabled", false).Error)
	InitChannelCache()
	assert.Greater(t, RoutingMetadataVersion(), version, "an ability change is published")
	require.NoError(t, DB.Model(&Ability{}).Where("channel_id = ?", channel.Id).Update("enabled", true).Error)
	InitChannelCache()
	version = RoutingMetadataVersion()

	require.NoError(t, DB.Migrator().RenameTable("abilities", "abilities_unavailable"))
	t.Cleanup(func() {
		if DB.Migrator().HasTable("abilities_unavailable") {
			require.NoError(t, DB.Migrator().RenameTable("abilities_unavailable", "abilities"))
		}
	})
	InitChannelCache()
	assert.Equal(t, version, RoutingMetadataVersion(), "a failed read publishes nothing")
	cached, err := CacheGetChannel(channel.Id)
	require.NoError(t, err, "a failed read keeps the previous cache")
	assert.Equal(t, channel.Name, cached.Name)
	candidates, err := GetSatisfiedChannelCandidates("default", "reload-model", nil)
	require.NoError(t, err)
	assert.Len(t, candidates, 1)
	require.NoError(t, DB.Migrator().RenameTable("abilities_unavailable", "abilities"))
	InitChannelCache()
	assert.Equal(t, version, RoutingMetadataVersion(), "the recovered rows are unchanged")
}

func init() {
	// Model tests flush coalesced channel bumps themselves; a timer firing
	// into a later test would add an unrelated sync task.
	channelStatusBumpDelay = time.Hour
}

// useChannelMatrixDatabase points DB at an isolated database of dialect,
// skipping MySQL and PostgreSQL unless TEST_MYSQL_DSN / TEST_POSTGRES_DSN is set.
func useChannelMatrixDatabase(t *testing.T, dialect string) {
	t.Helper()
	var driver gorm.Dialector
	switch dialect {
	case "sqlite":
		driver = sqlite.Open(filepath.Join(t.TempDir(), "channel-writes.db"))
	case "mysql":
		dsn := os.Getenv("TEST_MYSQL_DSN")
		if dsn == "" {
			t.Skip("TEST_MYSQL_DSN is not configured")
		}
		driver = mysql.Open(dsn)
	case "postgres":
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN is not configured")
		}
		driver = postgres.Open(dsn)
	}
	db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: fmt.Sprintf("cw_%x_", time.Now().UnixNano())}})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(2)
	tables := []any{&Channel{}, &Ability{}, &SystemTask{}, &ChannelLimitConfig{}, &ChannelDailyBudget{}, &ChannelLimitAttempt{}, &ChannelLimitEvent{},
		&UpstreamSupplier{}, &UpstreamCredential{}, &UpstreamCredentialVersion{}, &ChannelCredentialBinding{}, &UpstreamCredentialFingerprintSecret{}}
	require.NoError(t, db.AutoMigrate(tables...))
	previousDB, previousType, previousMemory := DB, common.MainDatabaseType(), common.MemoryCacheEnabled
	DB = db
	common.SetMainDatabaseType(common.DatabaseType(dialect))
	common.MemoryCacheEnabled = false
	initCol()
	t.Cleanup(func() {
		DB = previousDB
		common.SetMainDatabaseType(previousType)
		common.MemoryCacheEnabled = previousMemory
		initCol()
		assert.NoError(t, db.Migrator().DropTable(tables...))
		assert.NoError(t, sqlDB.Close())
	})
	var version string
	versionQuery := "SELECT version()"
	if dialect == "sqlite" {
		versionQuery = "SELECT sqlite_version()"
	}
	require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
	t.Logf("database: %s %s", dialect, version)
}

// failTableWrites makes every write of kind ("create" or "update") to table
// fail, standing for a database error in the middle of a channel operation.
func failTableWrites(t *testing.T, kind, table string) {
	t.Helper()
	name := "test:fail_" + kind + "_" + table
	fail := func(tx *gorm.DB) {
		if strings.HasSuffix(tx.Statement.Table, table) {
			_ = tx.AddError(errors.New("injected " + table + " " + kind + " failure"))
		}
	}
	callbacks := DB.Callback()
	if kind == "create" {
		require.NoError(t, callbacks.Create().Before("gorm:create").Register(name, fail))
		t.Cleanup(func() { _ = DB.Callback().Create().Remove(name) })
		return
	}
	require.NoError(t, callbacks.Update().Before("gorm:update").Register(name, fail))
	t.Cleanup(func() { _ = DB.Callback().Update().Remove(name) })
}

func channelAbilities(t *testing.T, channelID int) []Ability {
	t.Helper()
	var abilities []Ability
	require.NoError(t, DB.Where("channel_id = ?", channelID).Order("model").Find(&abilities).Error)
	return abilities
}

// Channel writes that touch channels, abilities and credential bindings
// commit together or not at all, and leave nothing behind a reused ID.
func TestChannelWriteTransactionsDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			useChannelMatrixDatabase(t, dialect)

			t.Run("deleting disabled channels removes their abilities and bindings", func(t *testing.T) {
				disabled := Channel{Name: "disabled", Key: "disabled-key", Status: common.ChannelStatusManuallyDisabled, Models: "m1,m2", Group: "default"}
				enabled := Channel{Name: "enabled", Key: "enabled-key", Status: common.ChannelStatusEnabled, Models: "m1", Group: "default"}
				owned := Channel{Name: "supplier", Key: "supplier-key", Status: common.ChannelStatusManuallyDisabled, Models: "m1", Group: "default"}
				for _, channel := range []*Channel{&disabled, &enabled, &owned} {
					require.NoError(t, channel.Insert())
				}
				require.NoError(t, DB.Model(&Channel{}).Where("id = ?", owned.Id).Update("owner_supplier_id", "supplier-a").Error)
				require.Len(t, channelAbilities(t, disabled.Id), 2)

				deleted, err := DeleteDisabledPlatformChannels()
				require.NoError(t, err)
				require.Len(t, deleted, 1)
				assert.Equal(t, disabled.Id, deleted[0].Id)
				assert.Equal(t, "disabled-key", deleted[0].Key)
				assert.Empty(t, channelAbilities(t, disabled.Id))
				var active int64
				require.NoError(t, DB.Model(&ChannelCredentialBinding{}).Where("channel_id = ? AND active = ?", disabled.Id, true).Count(&active).Error)
				assert.Zero(t, active)
				for _, kept := range []Channel{enabled, owned} {
					var count int64
					require.NoError(t, DB.Model(&Channel{}).Where("id = ?", kept.Id).Count(&count).Error)
					assert.EqualValues(t, 1, count, kept.Name)
					assert.NotEmpty(t, channelAbilities(t, kept.Id), kept.Name)
				}

				// A channel created again under the deleted ID routes.
				reused := Channel{Id: disabled.Id, Name: "reused", Key: "reused-key", Status: common.ChannelStatusEnabled, Models: "m1,m2", Group: "default"}
				require.NoError(t, reused.Insert())
				abilities := channelAbilities(t, reused.Id)
				require.Len(t, abilities, 2)
				for _, ability := range abilities {
					assert.True(t, ability.Enabled)
				}
			})

			t.Run("creating a channel replaces stale abilities of its ID", func(t *testing.T) {
				const id = 7001
				require.NoError(t, DB.Create(&Ability{Group: "default", Model: "stale", ChannelId: id, Enabled: false}).Error)
				require.NoError(t, DB.Create(&Ability{Group: "default", Model: "kept-name", ChannelId: id, Enabled: false}).Error)
				channel := Channel{Id: id, Name: "fresh", Key: "fresh-key", Status: common.ChannelStatusEnabled, Models: "kept-name", Group: "default"}
				require.NoError(t, BatchInsertChannels([]Channel{channel}))
				abilities := channelAbilities(t, id)
				require.Len(t, abilities, 1)
				assert.Equal(t, "kept-name", abilities[0].Model)
				assert.True(t, abilities[0].Enabled)
			})

			t.Run("tag disable is one transaction", func(t *testing.T) {
				tag := "disable-tag"
				multi := Channel{Name: "tag-multi", Key: "a\nb", Status: common.ChannelStatusAutoDisabled, Models: "tag-model", Group: "default", Tag: &tag,
					ChannelInfo: ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyStatusList: map[int]int{0: common.ChannelStatusAutoDisabled, 1: common.ChannelStatusAutoDisabled}}}
				multi.SetOtherInfo(map[string]any{"status_reason": ChannelStatusReasonAllKeysDisabled})
				single := Channel{Name: "tag-single", Key: "single", Status: common.ChannelStatusEnabled, Models: "tag-model", Group: "default", Tag: &tag}
				require.NoError(t, multi.Insert())
				require.NoError(t, single.Insert())

				failTableWrites(t, "update", "abilities")
				require.Error(t, DisableChannelByTag(tag))
				stored := func(id int) *Channel {
					var channel Channel
					require.NoError(t, DB.First(&channel, id).Error)
					return &channel
				}
				assert.Equal(t, common.ChannelStatusEnabled, stored(single.Id).Status, "a failed ability write rolls the status back")
				assert.Equal(t, ChannelStatusReasonAllKeysDisabled, stored(multi.Id).GetOtherInfo()["status_reason"])
				require.NoError(t, DB.Callback().Update().Remove("test:fail_update_abilities"))

				require.NoError(t, DisableChannelByTag(tag))
				for _, channel := range []Channel{multi, single} {
					assert.Equal(t, common.ChannelStatusManuallyDisabled, stored(channel.Id).Status, channel.Name)
					for _, ability := range channelAbilities(t, channel.Id) {
						assert.False(t, ability.Enabled, channel.Name)
					}
				}
				assert.Equal(t, "manual tag operation", stored(multi.Id).GetOtherInfo()["status_reason"], "automatic restoration is cancelled")
			})

			t.Run("tag edit is one transaction and reports ability errors", func(t *testing.T) {
				tag := "edit-tag"
				channel := Channel{Name: "tag-edit", Key: "edit-key", Status: common.ChannelStatusEnabled, Models: "old-model", Group: "default", Tag: &tag}
				require.NoError(t, channel.Insert())

				failTableWrites(t, "create", "abilities")
				models := "new-model"
				require.Error(t, EditChannelByTag(tag, nil, nil, &models, nil, nil, nil, nil, nil))
				var stored Channel
				require.NoError(t, DB.First(&stored, channel.Id).Error)
				assert.Equal(t, "old-model", stored.Models)
				abilities := channelAbilities(t, channel.Id)
				require.Len(t, abilities, 1)
				assert.Equal(t, "old-model", abilities[0].Model)
				require.NoError(t, DB.Callback().Create().Remove("test:fail_create_abilities"))

				priority := int64(9)
				require.NoError(t, EditChannelByTag(tag, nil, nil, &models, nil, &priority, nil, nil, nil))
				abilities = channelAbilities(t, channel.Id)
				require.Len(t, abilities, 1)
				assert.Equal(t, "new-model", abilities[0].Model)
				assert.Equal(t, &priority, abilities[0].Priority)
			})

			t.Run("a managed key status keeps a per-key change committed before it", func(t *testing.T) {
				channel := Channel{Name: "managed", Key: "k0\nk1", Status: common.ChannelStatusEnabled, Models: "managed-model", Group: "default",
					ChannelInfo: ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}}
				require.NoError(t, channel.Insert())
				// Another node disables k1 after the administrator's page loaded.
				require.True(t, UpdateChannelStatus(channel.Id, "k1", common.ChannelStatusAutoDisabled, "quota"))

				require.NoError(t, UpdateChannelKeyStatus(channel.Id, func(stored *Channel) error {
					stored.ChannelInfo.MultiKeyStatusList[0] = common.ChannelStatusManuallyDisabled
					stored.Status = common.ChannelStatusManuallyDisabled
					return nil
				}))
				var stored Channel
				require.NoError(t, DB.First(&stored, channel.Id).Error)
				assert.Equal(t, map[int]int{0: common.ChannelStatusManuallyDisabled, 1: common.ChannelStatusAutoDisabled}, stored.ChannelInfo.MultiKeyStatusList)
				assert.Equal(t, "quota", stored.ChannelInfo.MultiKeyDisabledReason[1])
				assert.Equal(t, common.ChannelStatusManuallyDisabled, stored.Status)
				for _, ability := range channelAbilities(t, channel.Id) {
					assert.False(t, ability.Enabled)
				}

				failTableWrites(t, "update", "abilities")
				require.Error(t, UpdateChannelKeyStatus(channel.Id, func(stored *Channel) error {
					stored.ChannelInfo.MultiKeyStatusList = map[int]int{}
					stored.Status = common.ChannelStatusEnabled
					return nil
				}))
				require.NoError(t, DB.First(&stored, channel.Id).Error)
				assert.Equal(t, common.ChannelStatusManuallyDisabled, stored.Status, "a failed ability write rolls the key status back")
				assert.Len(t, stored.ChannelInfo.MultiKeyStatusList, 2)
			})
		})
	}
}

// A credential this node cannot open disables only its own channel in the
// cache, never reaches a request as a usable key, and is never overwritten.
func TestUndecryptableCredentialAffectsOnlyItsChannel(t *testing.T) {
	setupChannelStatusTest(t)
	keyRing := func(id string) string {
		return `{"` + id + `":"` + base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901")) + `"}`
	}
	t.Setenv("SUPPLIER_CREDENTIAL_KEYS", keyRing("v1"))
	t.Setenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY", "v1")
	protected := Channel{Name: "write-only", Key: "protected-secret", SecretsWriteOnly: true, Status: common.ChannelStatusEnabled, Models: "shared-model", Group: "default"}
	plain := Channel{Name: "plain", Key: "plain-key", Status: common.ChannelStatusEnabled, Models: "shared-model", Group: "default"}
	require.NoError(t, protected.Insert())
	require.NoError(t, plain.Insert())
	storedKey := func() string {
		var raw struct{ Key string }
		require.NoError(t, DB.Model(&Channel{}).Select(commonKeyCol).Where("id = ?", protected.Id).Take(&raw).Error)
		return raw.Key
	}
	sealed := storedKey()
	require.True(t, strings.HasPrefix(sealed, supplierSecretPrefix))
	var bindings int64
	require.NoError(t, DB.Model(&ChannelCredentialBinding{}).Where("channel_id = ? AND active = ?", protected.Id, true).Count(&bindings).Error)
	require.EqualValues(t, 1, bindings)

	// The key that sealed the credential leaves this node's key ring.
	t.Setenv("SUPPLIER_CREDENTIAL_KEYS", keyRing("v2"))
	t.Setenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY", "v2")

	for _, cacheEnabled := range []bool{true, false} {
		common.MemoryCacheEnabled = cacheEnabled
		require.NoError(t, ReloadChannelCache())
		candidates, err := GetSatisfiedChannelCandidates("default", "shared-model", nil)
		require.NoError(t, err)
		require.Len(t, candidates, 1, "cache=%t", cacheEnabled)
		assert.Equal(t, plain.Id, candidates[0].Id)
		channel, err := CacheGetChannel(protected.Id)
		require.NoError(t, err)
		assert.Empty(t, channel.Key)
		assert.True(t, channel.CredentialUnavailable())
		key, _, apiErr := channel.GetNextEnabledKey()
		require.NotNil(t, apiErr)
		assert.Empty(t, key)
		assert.False(t, types.IsChannelError(apiErr), "a node-local key ring problem must not disable the channel everywhere")
		if cacheEnabled {
			assert.NotEqual(t, common.ChannelStatusEnabled, channel.Status)
		}
	}

	common.MemoryCacheEnabled = false
	loaded, err := GetChannelById(protected.Id, true)
	require.NoError(t, err)
	loaded.UpdateResponseTime(42)
	loaded.UpdateBalance(3)
	require.NoError(t, DB.Save(loaded).Error)
	assert.Empty(t, loaded.Key, "a save leaves the usable key empty")
	loaded.Name = "renamed"
	require.NoError(t, loaded.Update())
	require.True(t, UpdateChannelStatus(protected.Id, "", common.ChannelStatusManuallyDisabled, "manual operation"))
	copied := *loaded
	copied.Id = 0
	require.Error(t, DB.Create(&copied).Error, "an unreadable credential is never copied")

	assert.Equal(t, sealed, storedKey(), "no write replaces the sealed credential")
	require.NoError(t, DB.Model(&ChannelCredentialBinding{}).Where("channel_id = ? AND active = ?", protected.Id, true).Count(&bindings).Error)
	assert.EqualValues(t, 1, bindings, "an edit keeps the credential bindings")

	t.Setenv("SUPPLIER_CREDENTIAL_KEYS", keyRing("v1"))
	recovered, err := GetChannelById(protected.Id, true)
	require.NoError(t, err)
	assert.False(t, recovered.CredentialUnavailable())
	assert.Equal(t, "protected-secret", recovered.Key)
	assert.Equal(t, "renamed", recovered.Name)
}

// Routing hands out copies of the cached channel; polling still rotates.
func TestMultiKeyPollingRotatesAcrossCachedCopies(t *testing.T) {
	for _, cacheEnabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("cache=%t", cacheEnabled), func(t *testing.T) {
			setupChannelStatusTest(t)
			common.MemoryCacheEnabled = cacheEnabled
			channel := Channel{Name: "polling", Key: "k0\nk1\nk2", Status: common.ChannelStatusEnabled, Models: "polling-model", Group: "default",
				ChannelInfo: ChannelInfo{IsMultiKey: true, MultiKeySize: 3, MultiKeyMode: constant.MultiKeyModePolling, MultiKeyStatusList: map[int]int{1: common.ChannelStatusManuallyDisabled}}}
			require.NoError(t, channel.Insert())
			InitChannelCache()

			var got []string
			for range 4 {
				routed, err := CacheGetChannelForRouting(channel.Id)
				require.NoError(t, err)
				key, _, apiErr := routed.GetNextEnabledKey()
				require.Nil(t, apiErr)
				got = append(got, key)
			}
			assert.Equal(t, []string{"k0", "k2", "k0", "k2"}, got)
		})
	}
}

// The cursor lives only in the cache: a status write that republishes the
// entry from a database row must not rewind it, and the pinned path that
// receives the shared cache pointer must still rotate (run with -race).
func TestMultiKeyPollingCursorSurvivesCacheUpdates(t *testing.T) {
	setupChannelStatusTest(t)
	common.MemoryCacheEnabled = true
	channel := Channel{Name: "polling-cursor", Key: "k0\nk1\nk2\nk3", Status: common.ChannelStatusEnabled, Models: "cursor-model", Group: "default",
		ChannelInfo: ChannelInfo{IsMultiKey: true, MultiKeySize: 4, MultiKeyMode: constant.MultiKeyModePolling}}
	require.NoError(t, channel.Insert())
	InitChannelCache()
	next := func() string {
		shared, err := CacheGetChannel(channel.Id)
		require.NoError(t, err)
		key, _, apiErr := shared.GetNextEnabledKey()
		require.Nil(t, apiErr)
		return key
	}
	assert.Equal(t, "k0", next())
	assert.Equal(t, "k1", next())
	require.True(t, UpdateChannelStatus(channel.Id, "k3", common.ChannelStatusAutoDisabled, "provider rejected key"))
	assert.Equal(t, "k2", next(), "a key-level disable does not rewind the cursor")
	assert.Equal(t, "k0", next(), "the disabled key is skipped")
}

// A multi-key channel whose keys this node cannot decrypt keeps its per-key
// state when an administrator edits other fields.
func TestUnreadableMultiKeyEditKeepsKeyState(t *testing.T) {
	setupChannelStatusTest(t)
	keyRing := func(id string) string {
		return `{"` + id + `":"` + base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901")) + `"}`
	}
	t.Setenv("SUPPLIER_CREDENTIAL_KEYS", keyRing("v1"))
	t.Setenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY", "v1")
	channel := Channel{Name: "sealed-multi", Key: "a\nb\nc", Status: common.ChannelStatusEnabled, Models: "sealed-model", Group: "default", SecretsWriteOnly: true,
		ChannelInfo: ChannelInfo{IsMultiKey: true, MultiKeySize: 3, MultiKeyStatusList: map[int]int{1: common.ChannelStatusManuallyDisabled}, MultiKeyDisabledReason: map[int]string{1: "manual"}}}
	require.NoError(t, channel.Insert())
	// The key that sealed the credential leaves this node's key ring.
	t.Setenv("SUPPLIER_CREDENTIAL_KEYS", keyRing("v2"))

	var unreadable Channel
	require.NoError(t, DB.First(&unreadable, channel.Id).Error)
	require.True(t, unreadable.CredentialUnavailable())
	unreadable.Name = "renamed"
	require.NoError(t, unreadable.UpdateWithKey(nil))

	var stored Channel
	require.NoError(t, DB.Omit("key").First(&stored, channel.Id).Error)
	assert.Equal(t, "renamed", stored.Name)
	assert.Equal(t, 3, stored.ChannelInfo.MultiKeySize)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, stored.ChannelInfo.MultiKeyStatusList[1])
	assert.Equal(t, "manual", stored.ChannelInfo.MultiKeyDisabledReason[1])
}

// A late automatic disable of a key an administrator disabled keeps it
// manually disabled, so deleting auto-disabled keys keeps it.
func TestAutoDisableKeepsManuallyDisabledKey(t *testing.T) {
	setupChannelStatusTest(t)
	channel := Channel{Name: "manual-key", Key: "key-a\nkey-b", Status: common.ChannelStatusEnabled, Models: "manual-model", Group: "default",
		ChannelInfo: ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyStatusList: map[int]int{0: common.ChannelStatusManuallyDisabled}, MultiKeyDisabledReason: map[int]string{0: "manual"}}}
	require.NoError(t, channel.Insert())

	assert.False(t, UpdateChannelStatus(channel.Id, "key-a", common.ChannelStatusAutoDisabled, "provider rejected key"))
	var stored Channel
	require.NoError(t, DB.First(&stored, channel.Id).Error)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, stored.ChannelInfo.MultiKeyStatusList[0])
	assert.Equal(t, "manual", stored.ChannelInfo.MultiKeyDisabledReason[0])
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)

	updateMultiKeyStatusAtIndex(&stored, 0, common.ChannelStatusAutoDisabled, "funding")
	assert.Equal(t, common.ChannelStatusManuallyDisabled, stored.ChannelInfo.MultiKeyStatusList[0])
}

// Uncached routing retries with the routing model name, like the cache.
func TestUncachedCandidatesFallBackToRoutingModelName(t *testing.T) {
	setupChannelStatusTest(t)
	channel := Channel{Name: "gizmo", Key: "key", Status: common.ChannelStatusEnabled, Models: "gpt-4-gizmo-*", Group: "default"}
	require.NoError(t, channel.Insert())
	for _, cacheEnabled := range []bool{false, true} {
		common.MemoryCacheEnabled = cacheEnabled
		InitChannelCache()
		candidates, err := GetSatisfiedChannelCandidates("default", "gpt-4-gizmo-abc", nil)
		require.NoError(t, err)
		require.Len(t, candidates, 1, "cache=%t", cacheEnabled)
		assert.Equal(t, channel.Id, candidates[0].Id)
	}
}

// A reload that read its rows before a disable committed does not route to
// the channel again.
func TestChannelCacheReloadKeepsDisableCachedDuringTheRead(t *testing.T) {
	setupChannelStatusTest(t)
	common.MemoryCacheEnabled = true
	channel := Channel{Name: "reload-race", Key: "key", Status: common.ChannelStatusEnabled, Models: "race-model", Group: "default"}
	require.NoError(t, channel.Insert())
	InitChannelCache()

	var done atomic.Bool
	require.NoError(t, DB.Callback().Query().After("gorm:query").Register("test:disable_during_reload", func(tx *gorm.DB) {
		if strings.HasSuffix(tx.Statement.Table, "channels") && done.CompareAndSwap(false, true) {
			// The rows are read; the disable commits and is cached now.
			require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).Update("status", common.ChannelStatusAutoDisabled).Error)
			CacheUpdateChannelStatus(channel.Id, common.ChannelStatusAutoDisabled)
		}
	}))
	t.Cleanup(func() { _ = DB.Callback().Query().Remove("test:disable_during_reload") })
	require.NoError(t, ReloadChannelCache())
	require.True(t, done.Load())

	cached, err := CacheGetChannel(channel.Id)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, cached.Status)
	routed, err := GetRandomSatisfiedChannel("default", "race-model", 0, nil)
	require.NoError(t, err)
	assert.Nil(t, routed)

	// The next reload reads the committed disable itself.
	require.NoError(t, ReloadChannelCache())
	routed, err = GetRandomSatisfiedChannel("default", "race-model", 0, nil)
	require.NoError(t, err)
	assert.Nil(t, routed)
}
