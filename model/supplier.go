package model

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var ErrSupplierChannelOperation = errors.New("This operation is not available for supplier channels")

// SupplierAccount grants access to a procurement identity explicitly. Existing
// procurement labels never grant access to a login account or historical data.
type SupplierAccount struct {
	SupplierID       string   `json:"supplier_id" gorm:"type:varchar(36);primaryKey"`
	SupplyEnabled    bool     `json:"supply_enabled"`
	AllowedTemplates string   `json:"-" gorm:"type:text"`
	AllowedTypes     string   `json:"-" gorm:"type:text"`
	AllowedModels    string   `json:"-" gorm:"type:text"`
	Group            string   `json:"group" gorm:"type:varchar(64)"`
	MaxChannels      int      `json:"max_channels"`
	MaxInputTokens   int64    `json:"max_input_tokens" gorm:"bigint"`
	MaxOutputTokens  int64    `json:"max_output_tokens" gorm:"bigint"`
	CostRatio        *float64 `json:"cost_ratio"`
	PoolPriority     int64    `json:"pool_priority" gorm:"bigint"`
	PoolWeight       int      `json:"pool_weight"`
	PoolCapacity     int      `json:"pool_capacity"`
	Revision         int64    `json:"revision" gorm:"bigint"`
	CreatedAt        int64    `json:"created_at" gorm:"bigint"`
	UpdatedAt        int64    `json:"updated_at" gorm:"bigint"`
}

// Drafts preserve the last verified configuration while a replacement is
// tested. Secrets are encrypted and are never serialized in a management API.
type SupplierChannelDraft struct {
	ProbeTaskIDs string `json:"-" gorm:"type:text"`
	ChannelID    int    `gorm:"primaryKey"`
	TemplateID   string `gorm:"type:varchar(96)"`
	SupplierID   string `gorm:"type:varchar(36);index"`
	Version      int64  `gorm:"bigint"`
	Type         int
	Name         string `gorm:"type:varchar(100)"`
	BaseURL      string `gorm:"type:text"`
	Secret       string `json:"-" gorm:"type:text"`
	Models       string `gorm:"type:text"`
	Remark       string `gorm:"type:varchar(255)"`
	Status       string `gorm:"type:varchar(32)"`
	ErrorCode    string `gorm:"type:varchar(64)"`
	CreatedAt    int64  `gorm:"bigint"`
	LeaseUntil   int64  `gorm:"bigint"`
}

func GetSupplierAccount(id string) (*SupplierAccount, error) {
	if id == "" {
		return nil, gorm.ErrRecordNotFound
	}
	var account SupplierAccount
	err := DB.First(&account, "supplier_id = ?", id).Error
	return &account, err
}

// DefaultCostVersionID distinguishes an inherited supplier rule from an
// administrator's explicit channel override without storing another flag.
func (account *SupplierAccount) DefaultCostVersionID() string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(account.SupplierID+":"+strconv.FormatInt(account.Revision, 10))).String()
}

func (account *SupplierAccount) Types() []int {
	var values []int
	_ = common.UnmarshalJsonStr(account.AllowedTypes, &values)
	return values
}

func (account *SupplierAccount) Models() []string {
	return strings.FieldsFunc(account.AllowedModels, func(r rune) bool { return r == ',' || r == '\n' })
}

// TemplateIDs preserves legacy numeric authorizations without granting every
// installed plugin through the generic type-61 channel type.
func (account *SupplierAccount) TemplateIDs() []string {
	var values []string
	if account.AllowedTemplates != "" {
		_ = common.UnmarshalJsonStr(account.AllowedTemplates, &values)
		return values
	}
	for _, kind := range account.Types() {
		if kind != constant.ChannelTypeTaskPlugin {
			values = append(values, strconv.Itoa(kind))
		}
	}
	return values
}

func (account *SupplierAccount) Allows(channelType int, models []string) bool {
	return account.AllowsTemplate(strconv.Itoa(channelType), models)
}

func (account *SupplierAccount) AllowsTemplate(id string, models []string) bool {
	if !slices.Contains(account.TemplateIDs(), id) || len(models) == 0 {
		return false
	}
	allowed := account.Models()
	for _, name := range models {
		if name == "" || !slices.Contains(allowed, name) {
			return false
		}
	}
	return true
}

func SupplierChannelTemplateID(channel *Channel) string {
	if channel.Type == constant.ChannelTypeTaskPlugin {
		var settings dto.ChannelSettings
		if channel.Setting != nil {
			_ = common.UnmarshalJsonStr(*channel.Setting, &settings)
		}
		return "plugin:" + settings.TaskPluginKey
	}
	return strconv.Itoa(channel.Type)
}

func (draft *SupplierChannelDraft) Template() string {
	if draft.TemplateID != "" {
		return draft.TemplateID
	}
	return strconv.Itoa(draft.Type)
}

func (draft *SupplierChannelDraft) ApplyTemplate(channel *Channel) {
	if key, ok := strings.CutPrefix(draft.Template(), "plugin:"); ok && channel.Type == constant.ChannelTypeTaskPlugin {
		channel.SetSetting(dto.ChannelSettings{TaskPluginKey: key})
	} else if channel.Type == constant.ChannelTypeOpenAI {
		channel.SetSetting(dto.ChannelSettings{ResponsesWebSocketEnabled: true})
	} else {
		channel.Setting = nil
	}
}

func SaveSupplierAccount(account *SupplierAccount, expected int64, transactions ...*gorm.DB) error {
	if len(transactions) == 0 {
		err := DB.Transaction(func(tx *gorm.DB) error { return SaveSupplierAccount(account, expected, tx) })
		if err == nil {
			invalidateRoutingMetadata()
		}
		return err
	}
	db := DB
	if len(transactions) > 0 {
		db = transactions[0]
	}
	if account.SupplierID == "" || account.MaxChannels < 1 || account.MaxChannels > 10000 || account.PoolWeight < 1 || account.PoolWeight > 1000 || account.Group == "" {
		return errors.New("invalid supplier configuration")
	}
	if err := ValidateUpstreamCostRatio(account.CostRatio); err != nil {
		return err
	}
	account.UpdatedAt = time.Now().Unix()
	account.Revision = expected + 1
	if expected == 0 {
		account.CreatedAt = account.UpdatedAt
		return db.Create(account).Error
	}
	var previous SupplierAccount
	if err := lockForUpdate(db).First(&previous, "supplier_id = ? AND revision = ?", account.SupplierID, expected).Error; err != nil {
		return err
	}
	result := db.Model(&SupplierAccount{}).Where("supplier_id = ? AND revision = ?", account.SupplierID, expected).Select("supply_enabled", "allowed_types", "allowed_templates", "allowed_models", "group", "max_channels", "max_input_tokens", "max_output_tokens", "cost_ratio", "pool_priority", "pool_weight", "pool_capacity", "revision", "updated_at").Updates(account)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("supplier configuration changed; reload and retry")
	}
	var channels []Channel
	if err := lockForUpdate(db.Omit("key")).Where("owner_supplier_id = ?", account.SupplierID).Find(&channels).Error; err != nil {
		return err
	}
	for i := range channels {
		channel := &channels[i]
		channel.Group = account.Group
		if channel.CostRatio == nil || channel.CostVersionID == previous.DefaultCostVersionID() {
			channel.CostRatio, channel.CostVersionID = account.CostRatio, account.DefaultCostVersionID()
		}
		if err := channel.refreshProcurementCostSnapshots(); err != nil {
			return err
		}
		if err := db.Model(channel).Select("group", "cost_ratio", "cost_version_id", "upstream_credential_snapshot_data").Updates(channel).Error; err != nil {
			return err
		}
		if err := channel.UpdateAbilities(db); err != nil {
			return err
		}
		if err := db.Model(&ChannelLimitConfig{}).Where("channel_id = ?", channel.Id).Update("budget_ready", channel.ProcurementCostReady()).Error; err != nil {
			return err
		}
	}
	return nil
}

// SaveSupplierChannelDraft fences each change by the caller's displayed
// version. Ownership and platform routing fields come from the account, never
// from the supplier's request body.
func SaveSupplierChannelDraft(draft *SupplierChannelDraft, userID int, expected, priority int64, transactions ...*gorm.DB) (*Channel, error) {
	db := DB
	if len(transactions) > 0 {
		db = transactions[0]
	}
	var channel Channel
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&SupplierAccount{}).Where("supplier_id = ?", draft.SupplierID).UpdateColumn("updated_at", gorm.Expr("updated_at")).Error; err != nil {
			return err
		}
		var account SupplierAccount
		if err := lockForUpdate(tx).First(&account, "supplier_id = ?", draft.SupplierID).Error; err != nil {
			return err
		}
		if !account.AllowsTemplate(draft.Template(), strings.Split(draft.Models, ",")) {
			return errors.New("supplier model or template is not authorized")
		}
		if draft.ChannelID == 0 {
			var count int64
			if err := tx.Model(&Channel{}).Where("owner_supplier_id = ?", draft.SupplierID).Count(&count).Error; err != nil {
				return err
			}
			if count >= int64(account.MaxChannels) {
				return errors.New("supplier channel limit reached")
			}
			channel = Channel{OwnerSupplierID: draft.SupplierID, CreatedBy: userID, Name: draft.Name, Type: draft.Type, Group: account.Group, Status: common.ChannelStatusEnabled, CreatedTime: time.Now().Unix(), SecretsWriteOnly: true, SupplierVersion: 1, SupplierPriority: priority, CostRatio: account.CostRatio, CostVersionID: account.DefaultCostVersionID()}
			draft.ApplyTemplate(&channel)
			channel.Remark = &draft.Remark
			if err := tx.Create(&channel).Error; err != nil {
				return err
			}
			draft.ChannelID = channel.Id
		} else {
			if err := lockForUpdate(tx).First(&channel, "id = ? AND owner_supplier_id = ?", draft.ChannelID, draft.SupplierID).Error; err != nil {
				return err
			}
			if channel.SupplierVersion != expected {
				return errors.New("supplier channel changed; reload and retry")
			}
			channel.SupplierVersion++
			if err := tx.Model(&Channel{}).Where("id = ? AND supplier_version = ?", channel.Id, expected).Updates(map[string]any{"supplier_version": channel.SupplierVersion, "name": draft.Name, "remark": draft.Remark, "supplier_priority": priority}).Error; err != nil {
				return err
			}
		}
		draft.Version, draft.Status, draft.CreatedAt = channel.SupplierVersion, "configuring", time.Now().Unix()
		return tx.Save(draft).Error
	})
	return &channel, err
}

// QueueSupplierChannelValidation re-tests the current version without changing
// the live connection. Copies have no candidate yet, so snapshot their active
// credentials under the same owner/version fence before scheduling a probe.
func QueueSupplierChannelValidation(id int, owner string, version int64) (string, error) {
	status := "pending"
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&SupplierAccount{}).Where("supplier_id = ?", owner).UpdateColumn("updated_at", gorm.Expr("updated_at")).Error; err != nil {
			return err
		}
		var channel Channel
		if err := lockForUpdate(tx).First(&channel, "id = ? AND owner_supplier_id = ?", id, owner).Error; err != nil {
			return err
		}
		if channel.SupplierVersion != version {
			return errors.New("supplier channel changed; reload and retry")
		}
		var draft SupplierChannelDraft
		err := lockForUpdate(tx).First(&draft, "channel_id = ? AND supplier_id = ? AND version = ?", id, owner, version).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if !channel.SupplierValidated || channel.Key == "" {
				return errors.New("supplier channel changed; reload and retry")
			}
			secret, err := EncryptSupplierSecret(owner, channel.Key)
			if err != nil {
				return err
			}
			draft = SupplierChannelDraft{TemplateID: SupplierChannelTemplateID(&channel), ChannelID: id, SupplierID: owner, Version: version, Type: channel.Type, Name: channel.Name, BaseURL: channel.GetBaseURL(), Secret: secret, Models: channel.Models, Status: status, CreatedAt: time.Now().Unix()}
			if channel.Remark != nil {
				draft.Remark = *channel.Remark
			}
			return tx.Create(&draft).Error
		}
		if err != nil {
			return err
		}
		if draft.Status == "testing" && draft.LeaseUntil > time.Now().Unix() {
			status = "testing"
			return nil
		}
		return tx.Model(&draft).Updates(map[string]any{"status": status, "error_code": "", "lease_until": 0}).Error
	})
	return status, err
}

func PublishSupplierChannelDraft(channelID int, owner string, version int64, models []string) error {
	upstreamCredentialWriteMutex.Lock()
	defer upstreamCredentialWriteMutex.Unlock()
	err := DB.Transaction(func(tx *gorm.DB) error {
		if _, err := upstreamFingerprintSecret(tx); err != nil {
			return err
		}
		var channel Channel
		if err := lockForUpdate(tx).First(&channel, "id = ? AND owner_supplier_id = ?", channelID, owner).Error; err != nil {
			return err
		}
		var draft SupplierChannelDraft
		if err := tx.First(&draft, "channel_id = ? AND supplier_id = ? AND version = ?", channelID, owner, version).Error; err != nil {
			return err
		}
		if channel.SupplierVersion != version || len(models) == 0 {
			return errors.New("supplier channel changed; reload and retry")
		}
		var account SupplierAccount
		if err := tx.First(&account, "supplier_id = ?", owner).Error; err != nil {
			return err
		}
		if !account.AllowsTemplate(draft.Template(), models) {
			return errors.New("supplier model or template is not authorized")
		}
		secret, err := DecryptSupplierSecret(owner, draft.Secret)
		if err != nil {
			return err
		}
		channel.Type, channel.Key, channel.BaseURL = draft.Type, secret, &draft.BaseURL
		draft.ApplyTemplate(&channel)
		channel.Models, channel.SupplierValidated = strings.Join(models, ","), true
		channel.Group = account.Group
		if channel.CostRatio == nil || channel.CostVersionID == account.DefaultCostVersionID() {
			channel.CostRatio, channel.CostVersionID = account.CostRatio, account.DefaultCostVersionID()
		}
		channel.Keys = nil
		channel.ChannelInfo.IsMultiKey = strings.Contains(secret, "\n")
		channel.ChannelInfo.MultiKeySize = len(channel.GetKeys())
		if err := tx.Model(&channel).Select("type", "setting", "key", "base_url", "models", "supplier_validated", "secrets_write_only", "cost_ratio", "cost_version_id", "group", "channel_info").Updates(&channel).Error; err != nil {
			return err
		}
		if err := syncChannelUpstreamCredentials(tx, &channel); err != nil {
			return err
		}
		if err := channel.UpdateAbilities(tx); err != nil {
			return err
		}
		return tx.Model(&draft).Updates(map[string]any{"status": "validated", "error_code": ""}).Error
	})
	if err == nil {
		invalidateRoutingMetadata()
	}
	return err
}

func SupplierChannelAvailable(channel *Channel) bool {
	if channel == nil {
		return false
	}
	if channel.OwnerSupplierID == "" {
		return true
	}
	if channel.SupplierPaused || !channel.SupplierValidated {
		return false
	}
	account, err := GetSupplierAccount(channel.OwnerSupplierID)
	return err == nil && account.SupplyEnabled && account.AllowsTemplate(SupplierChannelTemplateID(channel), channel.GetModels())
}

// CopySupplierChannel keeps the credential lineage inside its owner. It never
// accepts a target owner or returns a secret to the caller.
func CopySupplierChannel(id int, owner string, version int64, userID int, name string) (int, error) {
	upstreamCredentialWriteMutex.Lock()
	defer upstreamCredentialWriteMutex.Unlock()
	copyID := 0
	err := DB.Transaction(func(tx *gorm.DB) error {
		if _, err := upstreamFingerprintSecret(tx); err != nil {
			return err
		}
		if err := tx.Model(&SupplierAccount{}).Where("supplier_id = ?", owner).UpdateColumn("updated_at", gorm.Expr("updated_at")).Error; err != nil {
			return err
		}
		var account SupplierAccount
		if err := lockForUpdate(tx).First(&account, "supplier_id = ?", owner).Error; err != nil {
			return err
		}
		var source Channel
		if err := lockForUpdate(tx).First(&source, "id = ? AND owner_supplier_id = ?", id, owner).Error; err != nil {
			return err
		}
		if source.SupplierVersion != version || !source.SupplierValidated || !account.AllowsTemplate(SupplierChannelTemplateID(&source), source.GetModels()) {
			return errors.New("supplier channel changed; reload and retry")
		}
		var count int64
		if err := tx.Model(&Channel{}).Where("owner_supplier_id = ?", owner).Count(&count).Error; err != nil {
			return err
		}
		if count >= int64(account.MaxChannels) {
			return errors.New("supplier channel limit reached")
		}
		copy := source
		copy.Id, copy.CreatedBy, copy.CreatedTime, copy.SupplierVersion = 0, userID, time.Now().Unix(), 1
		copy.Name, copy.SecretsWriteOnly = name, true
		copy.Balance, copy.UsedQuota, copy.TestTime, copy.ResponseTime = 0, 0, 0, 0
		if err := tx.Create(&copy).Error; err != nil {
			return err
		}
		if err := syncChannelUpstreamCredentials(tx, &copy); err != nil {
			return err
		}
		if err := copy.AddAbilities(tx); err != nil {
			return err
		}
		config := ChannelLimitConfig{ChannelID: copy.Id, Revision: 1, HistoryComplete: true, ObservedSince: time.Now().UnixMilli(), BudgetReady: copy.ProcurementCostReady(), ChannelLimitSettings: ChannelLimitSettings{ReconcileSeconds: ChannelLimitDefaultReconcile}}
		var original ChannelLimitConfig
		if err := tx.First(&original, "channel_id = ?", id).Error; err == nil {
			config.ChannelLimitSettings = original.ChannelLimitSettings
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := tx.Create(&config).Error; err != nil {
			return err
		}
		copyID = copy.Id
		return nil
	})
	if err == nil {
		invalidateRoutingMetadata()
	}
	return copyID, err
}

func DiscardSupplierChannelDraft(id int, owner string, version int64) error {
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&SupplierAccount{}).Where("supplier_id = ?", owner).UpdateColumn("updated_at", gorm.Expr("updated_at")).Error; err != nil {
			return err
		}
		var channel Channel
		if err := lockForUpdate(tx).Omit("key").First(&channel, "id = ? AND owner_supplier_id = ?", id, owner).Error; err != nil {
			return err
		}
		var draft SupplierChannelDraft
		if err := lockForUpdate(tx).First(&draft, "channel_id = ? AND supplier_id = ?", id, owner).Error; err != nil {
			return err
		}
		if channel.SupplierVersion != version || channel.SupplierValidated || draft.Status == "testing" {
			return errors.New("Only unused, idle channel drafts can be discarded")
		}
		var used int64
		if err := tx.Model(&ChannelLimitAttempt{}).Where("channel_id = ? AND sent_at_ms > 0", id).Count(&used).Error; err != nil {
			return err
		}
		if used > 0 {
			return errors.New("Only unused, idle channel drafts can be discarded")
		}
		if err := tx.Model(&OverviewAttemptEvent{}).Where("channel_id = ?", id).Count(&used).Error; err != nil {
			return err
		}
		if used > 0 {
			return errors.New("Only unused, idle channel drafts can be discarded")
		}
		if err := tx.Where("channel_id = ?", id).Delete(&Ability{}).Error; err != nil {
			return err
		}
		if err := tx.Delete(&draft).Error; err != nil {
			return err
		}
		return tx.Delete(&channel).Error
	})
	if err == nil {
		invalidateRoutingMetadata()
	}
	return err
}
