package model

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

// Procurement identity is independent of a channel's protocol vendor and tag.
// None of these records contains usable upstream credentials.
type UpstreamSupplier struct {
	ID          string `json:"id" gorm:"type:varchar(36);primaryKey"`
	Name        string `json:"name" gorm:"type:varchar(100);not null"`
	CreatedTime int64  `json:"created_time" gorm:"bigint"`
}

type UpstreamCredential struct {
	ID                 string   `json:"id" gorm:"type:varchar(36);primaryKey"`
	Alias              string   `json:"alias" gorm:"type:varchar(100)"`
	SupplierID         string   `json:"supplier_id" gorm:"type:varchar(36);index"`
	TagsJSON           string   `json:"-" gorm:"type:text"`
	Tags               []string `json:"tags" gorm:"-"`
	CostRatio          *float64 `json:"cost_ratio"`
	CostVersionID      string   `json:"cost_version_id" gorm:"type:varchar(36)"`
	OwnershipVersionID string   `json:"ownership_version_id" gorm:"type:varchar(36)"`
	CreatedTime        int64    `json:"created_time" gorm:"bigint"`
	UpdatedTime        int64    `json:"updated_time" gorm:"bigint"`
}

type UpstreamCredentialVersion struct {
	ID           string `json:"id" gorm:"type:varchar(36);primaryKey"`
	CredentialID string `json:"credential_id" gorm:"type:varchar(36);index"`
	Fingerprint  string `json:"-" gorm:"type:varchar(64);uniqueIndex"`
	CreatedTime  int64  `json:"created_time" gorm:"bigint"`
}

type ChannelCredentialBinding struct {
	ID                  string `json:"id" gorm:"type:varchar(36);primaryKey"`
	ChannelID           int    `json:"channel_id" gorm:"uniqueIndex:idx_channel_credential_slot;index"`
	KeyIndex            int    `json:"key_index"`
	ActiveSlot          *int   `json:"-" gorm:"uniqueIndex:idx_channel_credential_slot"`
	CredentialID        string `json:"credential_id" gorm:"type:varchar(36);index"`
	CredentialVersionID string `json:"credential_version_id" gorm:"type:varchar(36);index"`
	Active              bool   `json:"active" gorm:"index"`
	CreatedTime         int64  `json:"created_time" gorm:"bigint"`
}

// The database-backed HMAC pepper must survive restarts and be shared by all
// gateway instances. CryptoSecret may be randomly generated at each startup.
// The private table is never read by an API and secret-bearing SQL is not logged.
type UpstreamCredentialFingerprintSecret struct {
	ID     int    `json:"-" gorm:"primaryKey"`
	Secret string `json:"-" gorm:"type:varchar(64);not null"`
}

// UpstreamCredentialSnapshot is copied before an attempt. Its versions and
// values remain valid after a key is removed, reordered, or reassigned.
type UpstreamCredentialSnapshot struct {
	BindingID               string   `json:"binding_id"`
	ChannelID               int      `json:"channel_id"`
	KeyIndex                int      `json:"key_index"`
	CredentialID            string   `json:"credential_id"`
	CredentialVersionID     string   `json:"credential_version_id"`
	Alias                   string   `json:"alias"`
	SupplierID              string   `json:"supplier_id"`
	SupplierName            string   `json:"supplier_name"`
	Tags                    []string `json:"tags"`
	CostRatio               *float64 `json:"cost_ratio"`
	EffectiveCostRatio      *float64 `json:"effective_cost_ratio"`
	CostSource              string   `json:"cost_source"`
	CostVersionID           string   `json:"cost_version_id"`
	CredentialCostVersionID string   `json:"credential_cost_version_id"`
	ChannelCostVersionID    string   `json:"channel_cost_version_id"`
	OwnershipVersionID      string   `json:"ownership_version_id"`
	Active                  bool     `json:"active"`
}

type UpstreamCredentialMetadata struct {
	Alias      string   `json:"alias"`
	SupplierID string   `json:"supplier_id"`
	Tags       []string `json:"tags"`
	CostRatio  *float64 `json:"cost_ratio"`
}

var upstreamCredentialWriteMutex sync.Mutex

func AutoMigrateUpstreamCredentialSchema(db *gorm.DB) error {
	return db.AutoMigrate(&UpstreamSupplier{}, &UpstreamCredential{}, &UpstreamCredentialVersion{}, &ChannelCredentialBinding{}, &UpstreamCredentialFingerprintSecret{})
}

func ValidateUpstreamCostRatio(ratio *float64) error {
	if ratio != nil && (math.IsNaN(*ratio) || math.IsInf(*ratio, 0) || *ratio < 0 || *ratio > 100) {
		return errors.New("cost ratio must be finite and between 0 and 100")
	}
	return nil
}

func equalUpstreamCostRatio(a, b *float64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func (credential *UpstreamCredential) AfterFind(_ *gorm.DB) error {
	credential.Tags = []string{}
	if credential.TagsJSON != "" {
		return common.UnmarshalJsonStr(credential.TagsJSON, &credential.Tags)
	}
	return nil
}

func (channel *Channel) AfterFind(_ *gorm.DB) error {
	var snapshots []UpstreamCredentialSnapshot
	if channel.UpstreamCredentialSnapshotData != "" {
		// Reporting metadata must never make an otherwise usable channel fail
		// to load. Invalid metadata remains unknown in the reporting snapshot.
		if err := common.UnmarshalJsonStr(channel.UpstreamCredentialSnapshotData, &snapshots); err != nil {
			snapshots = nil
		}
	}
	channel.cacheUpstreamCredentialSnapshots(snapshots)
	return nil
}

// Publish a new immutable lookup with the same lifetime as its mirror. Channel
// routing snapshots can share this map safely when copying the Channel value.
func (channel *Channel) cacheUpstreamCredentialSnapshots(snapshots []UpstreamCredentialSnapshot) {
	channel.upstreamCredentialMirror = channel.UpstreamCredentialSnapshotData
	channel.upstreamCredentialSnapshots = nil
	if len(snapshots) == 0 {
		return
	}
	channel.upstreamCredentialSnapshots = make(map[int]UpstreamCredentialSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.Active {
			channel.upstreamCredentialSnapshots[snapshot.KeyIndex] = snapshot
		}
	}
}

func upstreamFingerprintSecret(tx *gorm.DB) ([]byte, error) {
	privateDB := tx.Session(&gorm.Session{Logger: gormlogger.Discard})
	var secret UpstreamCredentialFingerprintSecret
	err := privateDB.First(&secret, "id = ?", 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		material := make([]byte, 32)
		if _, err := rand.Read(material); err != nil {
			return nil, errors.New("failed to generate upstream identity secret")
		}
		secret = UpstreamCredentialFingerprintSecret{ID: 1, Secret: hex.EncodeToString(material)}
		if err := privateDB.Clauses(clause.OnConflict{DoNothing: true}).Create(&secret).Error; err != nil {
			return nil, errors.New("failed to initialize upstream identity secret")
		}
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errors.New("failed to read upstream identity secret")
	}
	// Serialize administrative identity mutations across gateway instances too.
	if err := lockForUpdate(privateDB).First(&secret, "id = ?", 1).Error; err != nil {
		return nil, errors.New("failed to lock upstream identity secret")
	}
	material, err := hex.DecodeString(secret.Secret)
	if err != nil || len(material) != 32 {
		return nil, errors.New("invalid upstream identity secret")
	}
	return material, nil
}

func channelCredentialKeys(channel *Channel) []string {
	if !channel.ChannelInfo.IsMultiKey {
		if channel.Key == "" {
			return nil
		}
		return []string{channel.Key}
	}
	return channel.GetKeys()
}

func upstreamCredentialNamespace(channel *Channel) (string, error) {
	organization := ""
	if channel.OpenAIOrganization != nil {
		organization = *channel.OpenAIOrganization
	}
	baseURL := channel.GetBaseURL()
	if baseURL == "" {
		baseURL = constant.GetChannelBaseURL(channel.Type)
	}
	namespace, err := common.Marshal([]any{channel.Type, strings.TrimRight(baseURL, "/"), organization})
	return string(namespace), err
}

// Sync at configuration time, never by querying keys on a relay hot path.
func syncChannelUpstreamCredentials(tx *gorm.DB, channel *Channel) error {
	material, err := upstreamFingerprintSecret(tx)
	if err != nil {
		return err
	}
	namespace, err := upstreamCredentialNamespace(channel)
	if err != nil {
		return err
	}
	var bindings []ChannelCredentialBinding
	if err := tx.Where("channel_id = ?", channel.Id).Order("created_time, id").Find(&bindings).Error; err != nil {
		return err
	}
	if err := tx.Model(&ChannelCredentialBinding{}).Where("channel_id = ?", channel.Id).Updates(map[string]any{"active": false, "active_slot": nil}).Error; err != nil {
		return err
	}
	usedBindings := make(map[string]bool)
	snapshots := make([]UpstreamCredentialSnapshot, 0)
	for index, key := range channelCredentialKeys(channel) {
		fingerprint := common.GenerateHMACWithKey(material, namespace+"\x00"+key)
		var version UpstreamCredentialVersion
		err := tx.Session(&gorm.Session{Logger: gormlogger.Discard}).Where("fingerprint = ?", fingerprint).First(&version).Error
		var credential UpstreamCredential
		if errors.Is(err, gorm.ErrRecordNotFound) {
			now := common.GetTimestamp()
			credential = UpstreamCredential{ID: uuid.NewString(), TagsJSON: "[]", Tags: []string{}, CostVersionID: uuid.NewString(), OwnershipVersionID: uuid.NewString(), CreatedTime: now, UpdatedTime: now}
			if err := tx.Create(&credential).Error; err != nil {
				return err
			}
			version = UpstreamCredentialVersion{ID: uuid.NewString(), CredentialID: credential.ID, Fingerprint: fingerprint, CreatedTime: now}
			// Fingerprints are also private metadata, so do not log their SQL values.
			if err := tx.Session(&gorm.Session{Logger: gormlogger.Discard}).Create(&version).Error; err != nil {
				return errors.New("failed to register upstream credential")
			}
		} else if err != nil {
			return err
		} else if err := tx.First(&credential, "id = ?", version.CredentialID).Error; err != nil {
			return err
		}
		binding := ChannelCredentialBinding{}
		for _, candidate := range bindings {
			if candidate.CredentialVersionID == version.ID && !usedBindings[candidate.ID] {
				binding = candidate
				break
			}
		}
		if binding.ID == "" {
			binding = ChannelCredentialBinding{ID: uuid.NewString(), ChannelID: channel.Id, CredentialID: credential.ID, CredentialVersionID: version.ID, CreatedTime: common.GetTimestamp()}
		}
		usedBindings[binding.ID] = true
		binding.KeyIndex, binding.ActiveSlot, binding.Active = index, &index, true
		if err := tx.Save(&binding).Error; err != nil {
			return err
		}
		snapshot := UpstreamCredentialSnapshot{BindingID: binding.ID, ChannelID: channel.Id, KeyIndex: index, CredentialID: credential.ID, CredentialVersionID: version.ID, Alias: credential.Alias, SupplierID: credential.SupplierID, Tags: slices.Clone(credential.Tags), CostRatio: credential.CostRatio, OwnershipVersionID: credential.OwnershipVersionID, Active: true, CostSource: "unknown", CostVersionID: channel.CostVersionID}
		snapshot.CredentialCostVersionID, snapshot.ChannelCostVersionID = credential.CostVersionID, channel.CostVersionID
		if credential.SupplierID != "" {
			var supplier UpstreamSupplier
			if err := tx.First(&supplier, "id = ?", credential.SupplierID).Error; err != nil {
				return err
			}
			snapshot.SupplierName = supplier.Name
		}
		if credential.CostRatio != nil {
			snapshot.EffectiveCostRatio, snapshot.CostSource, snapshot.CostVersionID = credential.CostRatio, "credential", credential.CostVersionID
		} else if channel.CostRatio != nil {
			snapshot.EffectiveCostRatio, snapshot.CostSource = channel.CostRatio, "channel"
		}
		snapshots = append(snapshots, snapshot)
	}
	data, err := common.Marshal(snapshots)
	if err != nil {
		return err
	}
	channel.UpstreamCredentialSnapshotData = string(data)
	channel.cacheUpstreamCredentialSnapshots(snapshots)
	return tx.Model(&Channel{}).Where("id = ?", channel.Id).Update("upstream_credential_snapshot_data", channel.UpstreamCredentialSnapshotData).Error
}

// Rotation is an explicit trusted operation, separate from replacing an
// unrelated key through channel editing. A stale refresh cannot overwrite a
// concurrent replacement, and the procurement identity and metadata survive.
func RotateChannelUpstreamCredential(channelID int, expectedKey, newKey string) error {
	if expectedKey == "" || newKey == "" {
		return errors.New("credential rotation requires existing and replacement material")
	}
	upstreamCredentialWriteMutex.Lock()
	defer upstreamCredentialWriteMutex.Unlock()
	err := DB.Transaction(func(tx *gorm.DB) error {
		material, err := upstreamFingerprintSecret(tx)
		if err != nil {
			return err
		}
		var channel Channel
		if err := lockForUpdate(tx).First(&channel, "id = ?", channelID).Error; err != nil {
			return err
		}
		if channel.ChannelInfo.IsMultiKey {
			return errors.New("credential rotation requires a single-key channel")
		}
		if channel.Key != expectedKey {
			return errors.New("channel credential changed during rotation")
		}
		if channel.UpstreamCredentialSnapshotData == "" {
			if err := syncChannelUpstreamCredentials(tx, &channel); err != nil {
				return err
			}
		}
		var binding ChannelCredentialBinding
		if err := tx.Where("channel_id = ? AND active_slot = ? AND active = ?", channelID, 0, true).First(&binding).Error; err != nil {
			return err
		}
		namespace, err := upstreamCredentialNamespace(&channel)
		if err != nil {
			return err
		}
		fingerprint := common.GenerateHMACWithKey(material, namespace+"\x00"+newKey)
		privateDB := tx.Session(&gorm.Session{Logger: gormlogger.Discard})
		var version UpstreamCredentialVersion
		err = privateDB.Where("fingerprint = ?", fingerprint).First(&version).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			version = UpstreamCredentialVersion{ID: uuid.NewString(), CredentialID: binding.CredentialID, Fingerprint: fingerprint, CreatedTime: common.GetTimestamp()}
			if err := privateDB.Create(&version).Error; err != nil {
				return errors.New("failed to register rotated upstream credential")
			}
		} else if err != nil {
			return errors.New("failed to inspect rotated upstream credential")
		} else if version.CredentialID != binding.CredentialID {
			return errors.New("replacement material already belongs to another procurement identity")
		}
		if err := tx.Model(&binding).Update("credential_version_id", version.ID).Error; err != nil {
			return err
		}
		if err := privateDB.Model(&Channel{}).Where("id = ?", channelID).Update("key", newKey).Error; err != nil {
			return errors.New("failed to persist rotated upstream credential")
		}
		channel.Key = newKey
		return syncChannelUpstreamCredentials(tx, &channel)
	})
	if err == nil {
		invalidateRoutingMetadata()
	}
	return err
}

// Database reads and configuration sync prepare the immutable lookup. Normal
// relay snapshots need only a lookup, with no JSON decode or database request.
func (channel *Channel) GetUpstreamCredentialSnapshot(index int) UpstreamCredentialSnapshot {
	var snapshot UpstreamCredentialSnapshot
	found := false
	if channel != nil {
		if channel.upstreamCredentialMirror == channel.UpstreamCredentialSnapshotData {
			snapshot, found = channel.upstreamCredentialSnapshots[index]
		} else {
			// Keep hand-built or externally modified snapshots compatible without
			// lazily mutating a Channel that another relay may share.
			var snapshots []UpstreamCredentialSnapshot
			if common.UnmarshalJsonStr(channel.UpstreamCredentialSnapshotData, &snapshots) == nil {
				for _, candidate := range snapshots {
					if candidate.KeyIndex == index && candidate.Active {
						snapshot, found = candidate, true
						break
					}
				}
			}
		}
	}
	if found {
		// A caller may retain or mutate a returned snapshot. Keep its mutable
		// values independent of the channel's shared lookup and future attempts.
		snapshot.Tags = slices.Clone(snapshot.Tags)
		if snapshot.CostRatio != nil {
			value := *snapshot.CostRatio
			snapshot.CostRatio = &value
		}
		if snapshot.EffectiveCostRatio != nil {
			value := *snapshot.EffectiveCostRatio
			snapshot.EffectiveCostRatio = &value
		}
		return snapshot
	}
	snapshot = UpstreamCredentialSnapshot{CostSource: "unknown"}
	if channel != nil {
		snapshot.ChannelID, snapshot.KeyIndex, snapshot.CostVersionID = channel.Id, index, channel.CostVersionID
	}
	return snapshot
}

func GetChannelUpstreamCredentials(channelID int) ([]UpstreamCredentialSnapshot, error) {
	var channel Channel
	err := DB.Select("id", "upstream_credential_snapshot_data").First(&channel, "id = ?", channelID).Error
	if err != nil {
		return nil, err
	}
	snapshots := []UpstreamCredentialSnapshot{}
	if channel.UpstreamCredentialSnapshotData != "" {
		err = common.UnmarshalJsonStr(channel.UpstreamCredentialSnapshotData, &snapshots)
	}
	return snapshots, err
}

// Bootstrap current configurations only; old logs are deliberately untouched.
func BackfillUpstreamCredentials() error {
	upstreamCredentialWriteMutex.Lock()
	defer upstreamCredentialWriteMutex.Unlock()
	var channelIDs []int
	if err := DB.Model(&Channel{}).Where("upstream_credential_snapshot_data IS NULL OR upstream_credential_snapshot_data = ?", "").Pluck("id", &channelIDs).Error; err != nil {
		return err
	}
	for _, id := range channelIDs {
		if err := DB.Transaction(func(tx *gorm.DB) error {
			if _, err := upstreamFingerprintSecret(tx); err != nil {
				return err
			}
			var channel Channel
			if err := lockForUpdate(tx).First(&channel, "id = ?", id).Error; err != nil {
				return err
			}
			if channel.CostVersionID == "" {
				channel.CostVersionID = uuid.NewString()
				if err := tx.Model(&Channel{}).Where("id = ?", id).Update("cost_version_id", channel.CostVersionID).Error; err != nil {
					return err
				}
			}
			return syncChannelUpstreamCredentials(tx, &channel)
		}); err != nil {
			return err
		}
	}
	return nil
}

func ListUpstreamSuppliers() ([]UpstreamSupplier, error) {
	suppliers := []UpstreamSupplier{}
	err := DB.Order("created_time, id").Find(&suppliers).Error
	return suppliers, err
}

func CreateUpstreamSupplier(name string) (*UpstreamSupplier, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 100 {
		return nil, errors.New("supplier name must contain 1 to 100 characters")
	}
	supplier := UpstreamSupplier{ID: uuid.NewString(), Name: name, CreatedTime: common.GetTimestamp()}
	return &supplier, DB.Create(&supplier).Error
}

func UpdateUpstreamCredentialMetadata(id string, metadata UpstreamCredentialMetadata) (*UpstreamCredential, error) {
	if err := ValidateUpstreamCostRatio(metadata.CostRatio); err != nil {
		return nil, err
	}
	metadata.Alias = strings.TrimSpace(metadata.Alias)
	if utf8.RuneCountInString(metadata.Alias) > 100 || len(metadata.Tags) > 20 {
		return nil, errors.New("alias must not exceed 100 characters and tags must not exceed 20 items")
	}
	tags := make([]string, 0, len(metadata.Tags))
	for _, tag := range metadata.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || utf8.RuneCountInString(tag) > 40 {
			return nil, errors.New("each tag must contain 1 to 40 characters")
		}
		if !slices.Contains(tags, tag) {
			tags = append(tags, tag)
		}
	}
	slices.Sort(tags)
	metadata.SupplierID = strings.TrimSpace(metadata.SupplierID)
	upstreamCredentialWriteMutex.Lock()
	defer upstreamCredentialWriteMutex.Unlock()
	var credential UpstreamCredential
	err := DB.Transaction(func(tx *gorm.DB) error {
		if _, err := upstreamFingerprintSecret(tx); err != nil {
			return err
		}
		if err := lockForUpdate(tx).First(&credential, "id = ?", id).Error; err != nil {
			return err
		}
		if metadata.SupplierID != "" {
			var count int64
			if err := tx.Model(&UpstreamSupplier{}).Where("id = ?", metadata.SupplierID).Count(&count).Error; err != nil {
				return err
			}
			if count != 1 {
				return errors.New("supplier does not exist")
			}
		}
		if credential.SupplierID != metadata.SupplierID || credential.Alias != metadata.Alias || !slices.Equal(credential.Tags, tags) {
			credential.OwnershipVersionID = uuid.NewString()
		}
		if !equalUpstreamCostRatio(credential.CostRatio, metadata.CostRatio) {
			credential.CostVersionID = uuid.NewString()
		}
		data, err := common.Marshal(tags)
		if err != nil {
			return err
		}
		credential.Alias, credential.SupplierID, credential.Tags, credential.TagsJSON, credential.CostRatio = metadata.Alias, metadata.SupplierID, tags, string(data), metadata.CostRatio
		credential.UpdatedTime = common.GetTimestamp()
		if err := tx.Save(&credential).Error; err != nil {
			return err
		}
		var bindings []ChannelCredentialBinding
		if err := tx.Where("credential_id = ? AND active = ?", id, true).Find(&bindings).Error; err != nil {
			return err
		}
		seen := make(map[int]bool)
		for _, binding := range bindings {
			if seen[binding.ChannelID] {
				continue
			}
			seen[binding.ChannelID] = true
			var channel Channel
			if err := lockForUpdate(tx).First(&channel, "id = ?", binding.ChannelID).Error; err != nil {
				return fmt.Errorf("failed to refresh upstream credential channel: %w", err)
			}
			if err := syncChannelUpstreamCredentials(tx, &channel); err != nil {
				return err
			}
		}
		return nil
	})
	return &credential, err
}
