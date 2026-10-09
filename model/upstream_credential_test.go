package model

import (
	"math"
	"os"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func TestUpstreamCredentialIdentityAndCostSnapshots(t *testing.T) {
	truncateTables(t)
	channelRatio := 0.6
	channel := Channel{Name: "procurement", Type: constant.ChannelTypeOpenAI, Key: "test-credential-a\ntest-credential-b\ntest-credential-c", Models: "test-model", Group: "default", CostRatio: &channelRatio, ChannelInfo: ChannelInfo{IsMultiKey: true, MultiKeyMode: constant.MultiKeyModePolling}}
	require.NoError(t, channel.Insert())
	original := []UpstreamCredentialSnapshot{channel.GetUpstreamCredentialSnapshot(0), channel.GetUpstreamCredentialSnapshot(1), channel.GetUpstreamCredentialSnapshot(2)}
	for _, snapshot := range original {
		assert.NotEmpty(t, snapshot.CredentialID)
		assert.NotEmpty(t, snapshot.CredentialVersionID)
		assert.NotEmpty(t, snapshot.BindingID)
		assert.Equal(t, "channel", snapshot.CostSource)
		require.NotNil(t, snapshot.EffectiveCostRatio)
		assert.Equal(t, 0.6, *snapshot.EffectiveCostRatio)
	}
	supplier, err := CreateUpstreamSupplier("  Procurement A  ")
	require.NoError(t, err)
	zero := 0.0
	credential, err := UpdateUpstreamCredentialMetadata(original[1].CredentialID, UpstreamCredentialMetadata{Alias: "primary", SupplierID: supplier.ID, Tags: []string{"production", "shared", "production"}, CostRatio: &zero})
	require.NoError(t, err)
	assert.Equal(t, []string{"production", "shared"}, credential.Tags)
	current, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	free := current.GetUpstreamCredentialSnapshot(1)
	assert.Equal(t, "credential", free.CostSource)
	require.NotNil(t, free.EffectiveCostRatio)
	assert.Zero(t, *free.EffectiveCostRatio)
	assert.Equal(t, supplier.ID, free.SupplierID)
	assert.Equal(t, "Procurement A", free.SupplierName)
	assert.NotEqual(t, original[1].OwnershipVersionID, free.OwnershipVersionID)
	assert.NotEqual(t, original[1].CostVersionID, free.CostVersionID)
	assert.Empty(t, original[1].SupplierID)
	assert.Equal(t, 0.6, *original[1].EffectiveCostRatio)

	// The same material in the same provider namespace has one procurement ID,
	// while each selected channel keeps its own default-cost inheritance.
	otherRatio := 0.9
	other := Channel{Name: "shared", Type: channel.Type, Key: "test-credential-a", Models: "test-model", Group: "default", CostRatio: &otherRatio}
	require.NoError(t, other.Insert())
	shared := other.GetUpstreamCredentialSnapshot(0)
	assert.Equal(t, original[0].CredentialID, shared.CredentialID)
	assert.NotEqual(t, original[0].BindingID, shared.BindingID)
	assert.Equal(t, 0.9, *shared.EffectiveCostRatio)

	// Reordering and deleting keys changes slots without transferring ownership.
	current.Key = "test-credential-c\ntest-credential-b"
	require.NoError(t, current.Update())
	assert.Equal(t, original[2].CredentialID, current.GetUpstreamCredentialSnapshot(0).CredentialID)
	assert.Equal(t, original[2].BindingID, current.GetUpstreamCredentialSnapshot(0).BindingID)
	assert.Equal(t, free.BindingID, current.GetUpstreamCredentialSnapshot(1).BindingID)
	var archived ChannelCredentialBinding
	require.NoError(t, DB.First(&archived, "id = ?", original[0].BindingID).Error)
	assert.False(t, archived.Active)
	assert.Nil(t, archived.ActiveSlot)

	// An unrelated replacement gets a new ID. Reintroducing a known material
	// recovers its permanent binding; an instance secret change cannot rename it.
	cryptoSecret := common.CryptoSecret
	common.CryptoSecret = "different-process-secret"
	t.Cleanup(func() { common.CryptoSecret = cryptoSecret })
	current.Key = "test-credential-a\ntest-credential-new"
	require.NoError(t, current.Update())
	assert.Equal(t, original[0].BindingID, current.GetUpstreamCredentialSnapshot(0).BindingID)
	assert.NotEqual(t, free.CredentialID, current.GetUpstreamCredentialSnapshot(1).CredentialID)
	assert.Empty(t, current.GetUpstreamCredentialSnapshot(1).SupplierID)
	assert.Equal(t, "channel", current.GetUpstreamCredentialSnapshot(1).CostSource)

	// Clearing a channel cost retains unknown; overriding with zero stays known.
	current.CostRatio, current.CostRatioSet = nil, true
	require.NoError(t, current.Update())
	unknown := current.GetUpstreamCredentialSnapshot(1)
	assert.Nil(t, unknown.EffectiveCostRatio)
	assert.Equal(t, "unknown", unknown.CostSource)
	assert.NotEqual(t, original[0].CostVersionID, unknown.CostVersionID)
	current.CostRatio = &zero
	require.NoError(t, current.Update())
	require.NotNil(t, current.GetUpstreamCredentialSnapshot(1).EffectiveCostRatio)
	assert.Zero(t, *current.GetUpstreamCredentialSnapshot(1).EffectiveCostRatio)

	// Credential overrides can be cleared to inherit, without touching user quota.
	_, err = UpdateUpstreamCredentialMetadata(free.CredentialID, UpstreamCredentialMetadata{Alias: "primary", SupplierID: supplier.ID, Tags: credential.Tags, CostRatio: nil})
	require.NoError(t, err)
	assert.Equal(t, int64(0), current.UsedQuota)
	assert.Equal(t, supplier.ID, free.SupplierID)
	assert.Zero(t, *free.EffectiveCostRatio)

	privateVersion := UpstreamCredentialVersion{}
	require.NoError(t, DB.First(&privateVersion, "id = ?", free.CredentialVersionID).Error)
	serialized, err := common.Marshal([]any{free, credential, privateVersion, UpstreamCredentialFingerprintSecret{ID: 1, Secret: "private-pepper"}})
	require.NoError(t, err)
	assert.NotContains(t, string(serialized), privateVersion.Fingerprint)
	assert.NotContains(t, string(serialized), "test-credential")
	assert.NotContains(t, string(serialized), "private-pepper")

	require.NoError(t, other.Delete())
	archived = ChannelCredentialBinding{}
	require.NoError(t, DB.First(&archived, "id = ?", shared.BindingID).Error)
	assert.False(t, archived.Active)
	var surviving UpstreamCredential
	require.NoError(t, DB.First(&surviving, "id = ?", shared.CredentialID).Error)
}

func TestUpstreamCredentialValidationAndAtomicConfiguration(t *testing.T) {
	truncateTables(t)
	for _, value := range []float64{-1, 100.1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		assert.Error(t, ValidateUpstreamCostRatio(&value))
	}
	assert.NoError(t, ValidateUpstreamCostRatio(nil))
	for _, value := range []float64{0, 0.2, 100} {
		assert.NoError(t, ValidateUpstreamCostRatio(&value))
	}
	channel := Channel{Name: "atomic", Type: constant.ChannelTypeOpenAI, Key: "old-material", Models: "model-a", Group: "default"}
	require.NoError(t, channel.Insert())
	original := channel.GetUpstreamCredentialSnapshot(0)
	callback := "test:upstream_binding_failure"
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "channel_credential_bindings" {
			tx.AddError(gorm.ErrInvalidData)
		}
	}))
	channel.Key, channel.Models = "replacement-material", "model-b"
	assert.Error(t, channel.Update())
	require.NoError(t, DB.Callback().Update().Remove(callback))
	t.Cleanup(func() { _ = DB.Callback().Update().Remove(callback) })
	stored, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "old-material", stored.Key)
	assert.Equal(t, "model-a", stored.Models)
	assert.Equal(t, original.CredentialID, stored.GetUpstreamCredentialSnapshot(0).CredentialID)
	var ability Ability
	require.NoError(t, DB.First(&ability, "channel_id = ?", channel.Id).Error)
	assert.Equal(t, "model-a", ability.Model)
	_, err = UpdateUpstreamCredentialMetadata(original.CredentialID, UpstreamCredentialMetadata{SupplierID: "missing"})
	assert.Error(t, err)
	tooManyTags := make([]string, 21)
	_, err = UpdateUpstreamCredentialMetadata(original.CredentialID, UpstreamCredentialMetadata{Tags: tooManyTags})
	assert.Error(t, err)
}

func TestUpstreamCredentialSnapshotFailsClosedAndRemainsIndependent(t *testing.T) {
	truncateTables(t)
	zero := 0.0
	channel := Channel{Name: "safe-snapshot", Type: constant.ChannelTypeOpenAI, Key: "snapshot-material", Models: "model", Group: "default", CostRatio: &zero}
	require.NoError(t, channel.Insert())
	initial := channel.GetUpstreamCredentialSnapshot(0)
	assert.Equal(t, "channel", initial.CostSource)
	require.NotNil(t, initial.EffectiveCostRatio)
	assert.Zero(t, *initial.EffectiveCostRatio)
	for _, index := range []int{-1, 1} {
		unknown := channel.GetUpstreamCredentialSnapshot(index)
		assert.Nil(t, unknown.EffectiveCostRatio, "an invalid slot cannot inherit zero channel cost")
		assert.Equal(t, "unknown", unknown.CostSource)
		assert.Empty(t, unknown.CredentialID)
	}

	ratio := 0.7
	_, err := UpdateUpstreamCredentialMetadata(initial.CredentialID, UpstreamCredentialMetadata{Alias: "snapshot", Tags: []string{"production"}, CostRatio: &ratio})
	require.NoError(t, err)
	loaded, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	copy := *loaded
	returned := copy.GetUpstreamCredentialSnapshot(0)
	require.NotNil(t, returned.CostRatio)
	require.NotNil(t, returned.EffectiveCostRatio)
	require.Len(t, returned.Tags, 1)
	*returned.CostRatio, *returned.EffectiveCostRatio, returned.Tags[0] = 99, 98, "changed"
	for _, current := range []*Channel{loaded, &copy} {
		unchanged := current.GetUpstreamCredentialSnapshot(0)
		assert.Equal(t, 0.7, *unchanged.CostRatio)
		assert.Equal(t, 0.7, *unchanged.EffectiveCostRatio)
		assert.Equal(t, []string{"production"}, unchanged.Tags)
	}

	// A changed/corrupt mirror must not reuse the older cached override or
	// substitute the channel's explicit zero when credential metadata is absent.
	copy.UpstreamCredentialSnapshotData = "invalid-json"
	unknown := copy.GetUpstreamCredentialSnapshot(0)
	assert.Nil(t, unknown.EffectiveCostRatio)
	assert.Empty(t, unknown.CredentialID)
	assert.Equal(t, "unknown", unknown.CostSource)
	require.NoError(t, DB.Model(&Channel{}).Where("id = ?", channel.Id).Update("upstream_credential_snapshot_data", "invalid-json").Error)
	corrupt, err := GetChannelById(channel.Id, true)
	require.NoError(t, err, "reporting metadata must not prevent a usable channel from loading")
	unknown = corrupt.GetUpstreamCredentialSnapshot(0)
	assert.Nil(t, unknown.EffectiveCostRatio)
	assert.Empty(t, unknown.CredentialID)
	assert.Equal(t, "unknown", unknown.CostSource)
	corrupt.UpstreamCredentialSnapshotData = ""
	assert.Nil(t, corrupt.GetUpstreamCredentialSnapshot(0).EffectiveCostRatio)
}

func TestUpstreamCredentialReorderPreservesKeyAvailability(t *testing.T) {
	truncateTables(t)
	memoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = memoryCacheEnabled })
	channel := Channel{Name: "key-state", Type: constant.ChannelTypeOpenAI, Key: "state-key-a\nstate-key-b", Models: "model", Group: "default", ChannelInfo: ChannelInfo{IsMultiKey: true, MultiKeyMode: constant.MultiKeyModePolling}}
	require.NoError(t, channel.Insert())
	originalA := channel.GetUpstreamCredentialSnapshot(0)
	originalB := channel.GetUpstreamCredentialSnapshot(1)
	stale, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	staleEnable, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	staleEnable.ExpectKeyState(staleEnable)
	require.True(t, UpdateChannelStatus(channel.Id, "state-key-b", common.ChannelStatusAutoDisabled, "upstream key rejected"))
	assert.ErrorContains(t, staleEnable.Update(), "channel key state changed")
	disabled, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	disabledTime := disabled.ChannelInfo.MultiKeyDisabledTime[1]
	require.NotZero(t, disabledTime)

	// A reorder without an expected availability edit preserves the latest
	// disabled material instead of saving the older availability map.
	stale.Key = "state-key-b\nstate-key-a"
	require.NoError(t, stale.Update())
	assert.Equal(t, originalB.CredentialID, stale.GetUpstreamCredentialSnapshot(0).CredentialID)
	assert.Equal(t, originalA.CredentialID, stale.GetUpstreamCredentialSnapshot(1).CredentialID)
	assert.Equal(t, map[int]int{0: common.ChannelStatusAutoDisabled}, stale.ChannelInfo.MultiKeyStatusList)
	assert.Equal(t, map[int]string{0: "upstream key rejected"}, stale.ChannelInfo.MultiKeyDisabledReason)
	assert.Equal(t, map[int]int64{0: disabledTime}, stale.ChannelInfo.MultiKeyDisabledTime)
	selected, index, keyErr := stale.GetNextEnabledKey()
	require.Nil(t, keyErr)
	assert.Equal(t, "state-key-a", selected)
	assert.Equal(t, 1, index)

	// Replacing the disabled material gives the new credential its default
	// enabled state, with no previous credential's reason or failure time.
	stale.Key = "state-key-c\nstate-key-a"
	require.NoError(t, stale.Update())
	assert.NotEqual(t, originalB.CredentialID, stale.GetUpstreamCredentialSnapshot(0).CredentialID)
	assert.Empty(t, stale.ChannelInfo.MultiKeyStatusList)
	assert.Empty(t, stale.ChannelInfo.MultiKeyDisabledReason)
	assert.Empty(t, stale.ChannelInfo.MultiKeyDisabledTime)
	assert.False(t, UpdateChannelStatus(channel.Id, "state-key-b", common.ChannelStatusAutoDisabled, "late removed-key failure"))

	// An explicit same-list enable/disable continues to work. A polling request
	// holding an older snapshot can change the cursor without undoing the ban.
	pollingSnapshot, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	stale.ChannelInfo.MultiKeyStatusList = map[int]int{1: common.ChannelStatusManuallyDisabled}
	stale.ChannelInfo.MultiKeyDisabledReason = map[int]string{1: "manual operation"}
	stale.ChannelInfo.MultiKeyDisabledTime = map[int]int64{1: 1234}
	require.NoError(t, stale.Update())
	pollingSnapshot.ChannelInfo.MultiKeyPollingIndex = 1
	require.NoError(t, pollingSnapshot.SaveChannelInfo())
	current, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, map[int]int{1: common.ChannelStatusManuallyDisabled}, current.ChannelInfo.MultiKeyStatusList)
	assert.Equal(t, map[int]string{1: "manual operation"}, current.ChannelInfo.MultiKeyDisabledReason)
	assert.Equal(t, map[int]int64{1: 1234}, current.ChannelInfo.MultiKeyDisabledTime)
	assert.Equal(t, 1, current.ChannelInfo.MultiKeyPollingIndex)
	current.ChannelInfo.MultiKeyStatusList = map[int]int{}
	current.ChannelInfo.MultiKeyDisabledReason = map[int]string{}
	current.ChannelInfo.MultiKeyDisabledTime = map[int]int64{}
	require.NoError(t, current.Update())
	assert.Empty(t, current.ChannelInfo.MultiKeyStatusList)
	assert.Empty(t, current.ChannelInfo.MultiKeyDisabledReason)
	assert.Empty(t, current.ChannelInfo.MultiKeyDisabledTime)
}

func TestUpstreamCredentialStaleKeyManagementCannotOverrideAvailability(t *testing.T) {
	truncateTables(t)
	memoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = memoryCacheEnabled })
	for _, tc := range []struct {
		name          string
		initialStatus int
		latestStatus  int
	}{
		{name: "remaining key enabled after deletion read", initialStatus: common.ChannelStatusAutoDisabled, latestStatus: common.ChannelStatusEnabled},
		{name: "remaining key disabled after deletion read", initialStatus: common.ChannelStatusEnabled, latestStatus: common.ChannelStatusAutoDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := Channel{Name: t.Name(), Type: constant.ChannelTypeOpenAI, Key: "delete-a\ndelete-b", Models: "model", Group: "default", ChannelInfo: ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyStatusList: map[int]int{1: tc.initialStatus}}}
			require.NoError(t, channel.Insert())
			original := []UpstreamCredentialSnapshot{channel.GetUpstreamCredentialSnapshot(0), channel.GetUpstreamCredentialSnapshot(1)}
			stale, err := GetChannelById(channel.Id, true)
			require.NoError(t, err)
			stale.ExpectKeyState(stale)
			require.True(t, UpdateChannelStatus(channel.Id, "delete-b", tc.latestStatus, "concurrent upstream decision"))
			stale.Key = "delete-b"
			stale.ChannelInfo.MultiKeySize = 1
			stale.ChannelInfo.MultiKeyStatusList = map[int]int{0: tc.initialStatus}
			if tc.initialStatus != common.ChannelStatusEnabled {
				stale.Status = common.ChannelStatusManuallyDisabled
				stale.SetOtherInfo(map[string]any{"status_reason": ChannelStatusReasonAllKeysDisabled})
			}
			assert.ErrorContains(t, stale.Update(), "channel key state changed")
			current, err := GetChannelById(channel.Id, true)
			require.NoError(t, err)
			assert.Equal(t, "delete-a\ndelete-b", current.Key)
			assert.Equal(t, common.ChannelStatusEnabled, current.Status)
			assert.Equal(t, original, []UpstreamCredentialSnapshot{current.GetUpstreamCredentialSnapshot(0), current.GetUpstreamCredentialSnapshot(1)})
			var ability Ability
			require.NoError(t, DB.Where("channel_id = ?", channel.Id).First(&ability).Error)
			assert.True(t, ability.Enabled)
			assert.Empty(t, current.GetOtherInfo()["status_reason"])
			if tc.latestStatus == common.ChannelStatusEnabled {
				assert.NotContains(t, current.ChannelInfo.MultiKeyStatusList, 1)
			} else {
				assert.Equal(t, tc.latestStatus, current.ChannelInfo.MultiKeyStatusList[1])
			}
		})
	}
	t.Run("manual override blocks stale exhaustion recovery", func(t *testing.T) {
		channel := Channel{Name: t.Name(), Type: constant.ChannelTypeOpenAI, Key: "manual-a\nmanual-b", Status: common.ChannelStatusManuallyDisabled, Models: "model", Group: "default", ChannelInfo: ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyStatusList: map[int]int{0: common.ChannelStatusAutoDisabled, 1: common.ChannelStatusAutoDisabled}}}
		channel.SetOtherInfo(map[string]any{"status_reason": ChannelStatusReasonAllKeysDisabled})
		require.NoError(t, channel.Insert())
		stale, err := GetChannelById(channel.Id, true)
		require.NoError(t, err)
		stale.ExpectKeyState(stale)
		require.True(t, UpdateChannelStatus(channel.Id, "", common.ChannelStatusManuallyDisabled, "manual maintenance"))
		stale.ChannelInfo.MultiKeyStatusList = map[int]int{}
		stale.Status = common.ChannelStatusEnabled
		stale.SetOtherInfo(map[string]any{"status_reason": ""})
		assert.ErrorContains(t, stale.Update(), "channel key state changed")
		current, err := GetChannelById(channel.Id, true)
		require.NoError(t, err)
		assert.Equal(t, common.ChannelStatusManuallyDisabled, current.Status)
		assert.Equal(t, "manual maintenance", current.GetOtherInfo()["status_reason"])
		assert.Equal(t, map[int]int{0: common.ChannelStatusAutoDisabled, 1: common.ChannelStatusAutoDisabled}, current.ChannelInfo.MultiKeyStatusList)
	})
}

func TestUpstreamCredentialExplicitRotationPreservesProcurementIdentity(t *testing.T) {
	truncateTables(t)
	channel := Channel{Name: "rotating", Type: constant.ChannelTypeCodex, Key: "old-oauth-material", Models: "model", Group: "default"}
	require.NoError(t, channel.Insert())
	original := channel.GetUpstreamCredentialSnapshot(0)
	supplier, err := CreateUpstreamSupplier("Rotation supplier")
	require.NoError(t, err)
	ratio := 0.4
	_, err = UpdateUpstreamCredentialMetadata(original.CredentialID, UpstreamCredentialMetadata{Alias: "subscription key", SupplierID: supplier.ID, Tags: []string{"subscription"}, CostRatio: &ratio})
	require.NoError(t, err)
	loaded, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	frozen := loaded.GetUpstreamCredentialSnapshot(0)
	require.NoError(t, RotateChannelUpstreamCredential(channel.Id, "old-oauth-material", "new-oauth-material"))
	loaded, err = GetChannelById(channel.Id, true)
	require.NoError(t, err)
	rotated := loaded.GetUpstreamCredentialSnapshot(0)
	assert.Equal(t, frozen.CredentialID, rotated.CredentialID)
	assert.Equal(t, frozen.BindingID, rotated.BindingID)
	assert.Equal(t, frozen.SupplierID, rotated.SupplierID)
	assert.Equal(t, frozen.Alias, rotated.Alias)
	assert.Equal(t, frozen.Tags, rotated.Tags)
	assert.Equal(t, frozen.OwnershipVersionID, rotated.OwnershipVersionID)
	assert.Equal(t, frozen.CostVersionID, rotated.CostVersionID)
	assert.Equal(t, *frozen.EffectiveCostRatio, *rotated.EffectiveCostRatio)
	assert.NotEqual(t, frozen.CredentialVersionID, rotated.CredentialVersionID)
	assert.Equal(t, "new-oauth-material", loaded.Key)
	assert.Error(t, RotateChannelUpstreamCredential(channel.Id, "old-oauth-material", "stale-refresh-material"))
	loaded, err = GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "new-oauth-material", loaded.Key)
	assert.Equal(t, rotated, loaded.GetUpstreamCredentialSnapshot(0))
	var oldVersion UpstreamCredentialVersion
	require.NoError(t, DB.First(&oldVersion, "id = ?", frozen.CredentialVersionID).Error)
	assert.Equal(t, rotated.CredentialID, oldVersion.CredentialID)
	// General editing continues to mean an unrelated replacement.
	loaded.Key = "unrelated-account-material"
	require.NoError(t, loaded.Update())
	assert.NotEqual(t, rotated.CredentialID, loaded.GetUpstreamCredentialSnapshot(0).CredentialID)
	assert.Empty(t, loaded.GetUpstreamCredentialSnapshot(0).SupplierID)
	assert.Nil(t, loaded.GetUpstreamCredentialSnapshot(0).CostRatio)
}

func TestBackfillUpstreamCredentialsDoesNotReidentifyCurrentKeys(t *testing.T) {
	truncateTables(t)
	legacy := Channel{Name: "legacy", Type: constant.ChannelTypeOpenAI, Key: "legacy-material", UsedQuota: 123, Models: "model", Group: "default"}
	require.NoError(t, DB.Create(&legacy).Error)
	require.NoError(t, BackfillUpstreamCredentials())
	loaded, err := GetChannelById(legacy.Id, true)
	require.NoError(t, err)
	first := loaded.GetUpstreamCredentialSnapshot(0)
	require.NotEmpty(t, first.CredentialID)
	assert.Nil(t, first.EffectiveCostRatio)
	assert.Equal(t, int64(123), loaded.UsedQuota)
	require.NoError(t, BackfillUpstreamCredentials())
	loaded, err = GetChannelById(legacy.Id, true)
	require.NoError(t, err)
	assert.Equal(t, first, loaded.GetUpstreamCredentialSnapshot(0))

	// Identical secrets at different upstream origins are separate identities.
	baseURL := "https://other-provider.invalid/v1"
	other := Channel{Name: "other-provider", Type: legacy.Type, Key: legacy.Key, BaseURL: &baseURL}
	require.NoError(t, other.Insert())
	assert.NotEqual(t, first.CredentialID, other.GetUpstreamCredentialSnapshot(0).CredentialID)
}

// This pre-feature fixture protects existing credentials and accounting values
// when the nullable cost and safe metadata mirror columns are introduced.
type legacyUpstreamMatrixChannel struct {
	Id        int
	Type      int
	Key       string `gorm:"not null"`
	Name      string
	Models    string
	Group     string `gorm:"type:varchar(64)"`
	UsedQuota int64  `gorm:"bigint"`
}

func (legacyUpstreamMatrixChannel) TableName() string { return "procurement_matrix_channels" }

func TestUpstreamCredentialDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(":memory:")
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
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "procurement_matrix_"}})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			previousDB, previousType := DB, common.MainDatabaseType()
			previousMemoryCache := common.MemoryCacheEnabled
			DB = db
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			common.MemoryCacheEnabled = false
			initCol()
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(&Ability{}, &Channel{}, &ChannelCredentialBinding{}, &UpstreamCredentialVersion{}, &UpstreamCredential{}, &UpstreamSupplier{}, &UpstreamCredentialFingerprintSecret{}))
				DB = previousDB
				common.SetMainDatabaseType(previousType)
				common.MemoryCacheEnabled = previousMemoryCache
				initCol()
				require.NoError(t, sqlDB.Close())
			})
			var version string
			versionQuery := "SELECT version()"
			if dialect == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("%s version: %s", dialect, version)
			require.NoError(t, db.Migrator().CreateTable(&legacyUpstreamMatrixChannel{}))
			legacy := legacyUpstreamMatrixChannel{Type: constant.ChannelTypeOpenAI, Key: "matrix-legacy-material", Name: "legacy", Models: "test-model", Group: "default", UsedQuota: 1234}
			require.NoError(t, db.Create(&legacy).Error)
			for range 2 {
				require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}))
				require.NoError(t, AutoMigrateUpstreamCredentialSchema(db))
				require.NoError(t, BackfillUpstreamCredentials())
			}
			stored, err := GetChannelById(legacy.Id, true)
			require.NoError(t, err)
			assert.Equal(t, legacy.Key, stored.Key)
			assert.Equal(t, int64(1234), stored.UsedQuota)
			assert.Nil(t, stored.CostRatio)
			frozen := stored.GetUpstreamCredentialSnapshot(0)
			require.NotEmpty(t, frozen.CredentialID)
			assert.Nil(t, frozen.EffectiveCostRatio)
			supplier, err := CreateUpstreamSupplier("Matrix procurement")
			require.NoError(t, err)
			zero := 0.0
			_, err = UpdateUpstreamCredentialMetadata(frozen.CredentialID, UpstreamCredentialMetadata{SupplierID: supplier.ID, CostRatio: &zero, Tags: []string{"matrix"}})
			require.NoError(t, err)
			stored, err = GetChannelById(legacy.Id, true)
			require.NoError(t, err)
			current := stored.GetUpstreamCredentialSnapshot(0)
			assert.Equal(t, frozen.CredentialID, current.CredentialID)
			assert.Equal(t, supplier.ID, current.SupplierID)
			require.NotNil(t, current.EffectiveCostRatio)
			assert.Zero(t, *current.EffectiveCostRatio)
			assert.Nil(t, frozen.EffectiveCostRatio)
			// A fresh channel exercises nullable uniqueness and multi-key reordering
			// in the same upgraded database, then preserves archived identities.
			fresh := Channel{Name: "fresh", Type: constant.ChannelTypeOpenAI, Key: "matrix-legacy-material\nmatrix-other-material", Models: "test-model", Group: "default", ChannelInfo: ChannelInfo{IsMultiKey: true}}
			require.NoError(t, fresh.Insert())
			before := fresh.GetUpstreamCredentialSnapshot(0)
			assert.Equal(t, frozen.CredentialID, before.CredentialID)
			fresh.ExpectKeyState(&fresh)
			require.True(t, UpdateChannelStatus(fresh.Id, "matrix-other-material", common.ChannelStatusAutoDisabled, "matrix key rejected"))
			fresh.Key = "matrix-other-material\nmatrix-legacy-material"
			assert.ErrorContains(t, fresh.Update(), "channel key state changed")
			fresh.keyStateExpected = nil // An unconditional reorder still carries the latest key state.
			require.NoError(t, fresh.Update())
			assert.Equal(t, before.BindingID, fresh.GetUpstreamCredentialSnapshot(1).BindingID)
			assert.Equal(t, map[int]int{0: common.ChannelStatusAutoDisabled}, fresh.ChannelInfo.MultiKeyStatusList)
			assert.Equal(t, map[int]string{0: "matrix key rejected"}, fresh.ChannelInfo.MultiKeyDisabledReason)
			assert.NotZero(t, fresh.ChannelInfo.MultiKeyDisabledTime[0])
			info, err := CacheGetChannelInfo(fresh.Id)
			require.NoError(t, err)
			assert.Equal(t, fresh.ChannelInfo, *info)
			fresh.CostRatio, fresh.CostRatioSet = &zero, true
			require.NoError(t, fresh.Update())
			require.NotNil(t, fresh.CostRuleChange)
			assert.Nil(t, fresh.CostRuleChange.BeforeRatio)
			require.NotNil(t, fresh.CostRuleChange.AfterRatio)
			assert.Zero(t, *fresh.CostRuleChange.AfterRatio)
			assert.Equal(t, fresh.CostVersionID, fresh.CostRuleChange.AfterVersionID)
			assert.NotEqual(t, fresh.CostRuleChange.BeforeVersionID, fresh.CostRuleChange.AfterVersionID)
			fresh.CostRatio = nil
			require.NoError(t, fresh.Update())
			require.NotNil(t, fresh.CostRuleChange)
			require.NotNil(t, fresh.CostRuleChange.BeforeRatio)
			assert.Zero(t, *fresh.CostRuleChange.BeforeRatio)
			assert.Nil(t, fresh.CostRuleChange.AfterRatio)
			assert.Nil(t, fresh.CostRatio)
			assert.Equal(t, "unknown", fresh.GetUpstreamCredentialSnapshot(0).CostSource)
			assert.Equal(t, "credential", fresh.GetUpstreamCredentialSnapshot(1).CostSource)
			require.NoError(t, fresh.Delete())
			var active int64
			require.NoError(t, db.Model(&ChannelCredentialBinding{}).Where("channel_id = ? AND active = ?", fresh.Id, true).Count(&active).Error)
			assert.Zero(t, active)
		})
	}
}
