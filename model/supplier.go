package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var ErrSupplierChannelOperation = errors.New("This operation is not available for supplier channels")

const (
	SupplierDraftReady        = "ready_for_review"
	SupplierDraftClosed       = "closed"
	SupplierDraftQueued       = "review_queued"
	SupplierDraftTesting      = "review_testing"
	SupplierDraftWaiting      = "review_waiting"
	SupplierDraftUnknown      = "review_unknown"
	SupplierDraftCostMissing  = "review_cost_missing"
	SupplierDraftPriceMissing = "review_model_price_missing"
)

// SupplierAccount grants access to a procurement identity explicitly. Existing
// procurement labels never grant access to a login account or historical data.
type SupplierAccount struct {
	SupplierID        string   `json:"supplier_id" gorm:"type:varchar(36);primaryKey"`
	SupplyEnabled     bool     `json:"supply_enabled"`
	AllowAllTemplates bool     `json:"allow_all_templates"`
	AllowAllModels    bool     `json:"allow_all_models"`
	AllowedTemplates  string   `json:"-" gorm:"type:text"`
	AllowedTypes      string   `json:"-" gorm:"type:text"`
	AllowedModels     string   `json:"-" gorm:"type:text"`
	Group             string   `json:"group" gorm:"type:varchar(64)"`
	MaxChannels       int      `json:"max_channels"`
	MaxInputTokens    int64    `json:"max_input_tokens" gorm:"bigint"`
	MaxOutputTokens   int64    `json:"max_output_tokens" gorm:"bigint"`
	CostRatio         *float64 `json:"cost_ratio"`
	PoolPriority      int64    `json:"pool_priority" gorm:"bigint"`
	PoolWeight        int      `json:"pool_weight"`
	PoolCapacity      int      `json:"pool_capacity"`
	Revision          int64    `json:"revision" gorm:"bigint"`
	CreatedAt         int64    `json:"created_at" gorm:"bigint"`
	UpdatedAt         int64    `json:"updated_at" gorm:"bigint"`
}

// Drafts preserve the last verified configuration while a replacement is
// tested. Secrets are encrypted and are never serialized in a management API.
type SupplierChannelDraft struct {
	ProbeTaskIDs string `json:"-" gorm:"type:text"`
	TestedModels string `json:"-" gorm:"type:text"`
	TestToken    string `json:"-" gorm:"type:varchar(36)"`
	TestOnly     bool   `json:"-"`
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

func (draft *SupplierChannelDraft) ValidationStatus() string {
	switch draft.Status {
	case SupplierDraftQueued:
		return "pending"
	case SupplierDraftTesting:
		return "testing"
	case SupplierDraftWaiting:
		return "waiting"
	case SupplierDraftUnknown:
		return "result_unknown"
	case SupplierDraftCostMissing:
		return "cost_config_missing"
	case SupplierDraftPriceMissing:
		return "model_price_error"
	default:
		return draft.Status
	}
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

// AllowsTemplateID checks the account's restriction. Callers selecting a
// template must also check that it exists in the platform's supplier catalog.
func (account *SupplierAccount) AllowsTemplateID(id string) bool {
	return id != "" && id != strconv.Itoa(constant.ChannelTypeTaskPlugin) &&
		(account.AllowAllTemplates || slices.Contains(account.TemplateIDs(), id))
}

func (account *SupplierAccount) AllowsTemplate(id string, models []string) bool {
	if !account.AllowsTemplateID(id) || len(models) == 0 {
		return false
	}
	allowed := account.Models()
	for _, name := range models {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, ",\r\n") ||
			!account.AllowAllModels && !slices.Contains(allowed, name) {
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
			BumpSyncTopic(SyncTopicChannels)
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
	result := db.Model(&SupplierAccount{}).Where("supplier_id = ? AND revision = ?", account.SupplierID, expected).Select("supply_enabled", "allow_all_templates", "allow_all_models", "allowed_types", "allowed_templates", "allowed_models", "group", "max_channels", "max_input_tokens", "max_output_tokens", "cost_ratio", "pool_priority", "pool_weight", "pool_capacity", "revision", "updated_at").Updates(account)
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
		if channel.CostRatio == nil || channel.CostVersionID == previous.DefaultCostVersionID() {
			channel.CostRatio, channel.CostVersionID = account.CostRatio, account.DefaultCostVersionID()
		}
		if err := channel.refreshProcurementCostSnapshots(); err != nil {
			return err
		}
		if err := db.Model(channel).Select("cost_ratio", "cost_version_id", "upstream_credential_snapshot_data").Updates(channel).Error; err != nil {
			return err
		}
		if err := db.Model(&ChannelLimitConfig{}).Where("channel_id = ?", channel.Id).Update("budget_ready", channel.ProcurementCostReady()).Error; err != nil {
			return err
		}
	}
	return nil
}

// SaveSupplierChannelDraft leaves the published configuration available until
// an administrator publishes the candidate.
func SaveSupplierChannelDraft(draft *SupplierChannelDraft, userID int, expected int64, transactions ...*gorm.DB) (*Channel, error) {
	db := DB
	if len(transactions) > 0 {
		db = transactions[0]
	}
	var channel Channel
	status := "configuring"
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
			closed := tx.Model(&SupplierChannelDraft{}).Select("channel_id").Where("supplier_id = ? AND status = ?", draft.SupplierID, SupplierDraftClosed)
			if err := tx.Model(&Channel{}).Where("owner_supplier_id = ?", draft.SupplierID).
				Where("supplier_validated = ? OR id NOT IN (?)", true, closed).Count(&count).Error; err != nil {
				return err
			}
			if count >= int64(account.MaxChannels) {
				return errors.New("supplier channel limit reached")
			}
			channel = Channel{OwnerSupplierID: draft.SupplierID, CreatedBy: userID, Name: draft.Name, Type: draft.Type, Status: common.ChannelStatusManuallyDisabled, CreatedTime: time.Now().Unix(), SecretsWriteOnly: true, SupplierVersion: 1, CostRatio: account.CostRatio, CostVersionID: account.DefaultCostVersionID()}
			channel.Remark = &draft.Remark
			if err := tx.Create(&channel).Error; err != nil {
				return err
			}
			// GORM applies the default group on insert. A new draft has no public group.
			if err := tx.Model(&channel).UpdateColumn("group", "").Error; err != nil {
				return err
			}
			channel.Group = ""
			draft.ChannelID = channel.Id
		} else {
			if err := lockForUpdate(tx).First(&channel, "id = ? AND owner_supplier_id = ?", draft.ChannelID, draft.SupplierID).Error; err != nil {
				return err
			}
			if channel.SupplierVersion != expected {
				return errors.New("supplier channel changed; reload and retry")
			}
			var current SupplierChannelDraft
			err := lockForUpdate(tx).First(&current, "channel_id = ? AND supplier_id = ? AND version = ?", draft.ChannelID, draft.SupplierID, expected).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if !channel.SupplierValidated && current.Status == SupplierDraftClosed {
				return errors.New("This channel draft is closed.")
			}
			currentStatus := current.ValidationStatus()
			if currentStatus == "testing" {
				return errors.New("A channel test is already in progress.")
			}
			if draft.ProbeTaskIDs != "" && currentStatus == "waiting" {
				status = SupplierDraftWaiting
			} else if draft.ProbeTaskIDs != "" && currentStatus == "result_unknown" {
				status = SupplierDraftUnknown
			}
			channel.SupplierVersion++
			if err := tx.Model(&Channel{}).Where("id = ? AND supplier_version = ?", channel.Id, expected).Update("supplier_version", channel.SupplierVersion).Error; err != nil {
				return err
			}
		}
		draft.Version, draft.Status, draft.CreatedAt = channel.SupplierVersion, status, time.Now().Unix()
		return tx.Save(draft).Error
	})
	return &channel, err
}

// QueueSupplierChannelValidation runs an unpublished candidate or takes a
// diagnostic snapshot of the published configuration.
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
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if draft.Status == SupplierDraftClosed {
			return errors.New("This channel draft is closed.")
		}
		if errors.Is(err, gorm.ErrRecordNotFound) || draft.Status == "validated" {
			if !channel.SupplierValidated || channel.Key == "" {
				return errors.New("supplier channel changed; reload and retry")
			}
			secret, err := EncryptSupplierSecret(owner, channel.Key)
			if err != nil {
				return err
			}
			draft = SupplierChannelDraft{TemplateID: SupplierChannelTemplateID(&channel), ChannelID: id, SupplierID: owner, Version: version, Type: channel.Type, Name: channel.Name, BaseURL: channel.GetBaseURL(), Secret: secret, Models: channel.Models, Status: SupplierDraftQueued, CreatedAt: time.Now().Unix(), TestOnly: true}
			if channel.Remark != nil {
				draft.Remark = *channel.Remark
			}
			return tx.Save(&draft).Error
		}
		switch draft.ValidationStatus() {
		case "testing", "waiting", "result_unknown", "pending":
			status = draft.ValidationStatus()
			return nil
		}
		if draft.Status == SupplierDraftReady || draft.Status == "failed" {
			draft.ProbeTaskIDs = ""
		}
		return tx.Model(&draft).Updates(map[string]any{"status": SupplierDraftQueued, "error_code": "", "lease_until": 0, "test_token": "", "tested_models": "", "probe_task_ids": draft.ProbeTaskIDs}).Error
	})
	return status, err
}

// SupplierChannelRevision binds an administrator's confirmation to the latest
// test result and the routing configuration they reviewed.
func SupplierChannelRevision(channel *Channel, draft *SupplierChannelDraft) string {
	var status, testToken, testedModels string
	if draft != nil {
		status, testToken, testedModels = draft.Status, draft.TestToken, draft.TestedModels
	}
	data, _ := common.Marshal([]any{channel.Id, channel.SupplierVersion, channel.SupplierValidated,
		channel.Status, channel.SupplierPaused, channel.Group, channel.Models, channel.GetPriority(),
		channel.SupplierPriority, channel.GetWeight(), status, testToken, testedModels})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

type SupplierChannelRouting struct {
	Groups   []string `json:"groups"`
	Priority int64    `json:"priority"`
	Weight   uint     `json:"weight"`
}

func (routing SupplierChannelRouting) Validate() error {
	if len(routing.Groups) == 0 || utf8.RuneCountInString(strings.Join(routing.Groups, ",")) > 64 || routing.Priority < -1000 || routing.Priority > 1000 || routing.Weight > 1000 {
		return common.NewMessage("Invalid parameters")
	}
	seen := make(map[string]bool, len(routing.Groups))
	for _, group := range routing.Groups {
		if group == "" || strings.ContainsAny(group, ",\r\n") || !ratio_setting.ContainsGroupRatio(group) || seen[group] {
			return common.NewMessage("Invalid parameters")
		}
		seen[group] = true
	}
	return nil
}

func PublishSupplierChannelDraft(channelID int, owner string, version int64, revision string, models []string, routing SupplierChannelRouting) error {
	upstreamCredentialWriteMutex.Lock()
	defer upstreamCredentialWriteMutex.Unlock()
	err := DB.Transaction(func(tx *gorm.DB) error {
		if _, err := upstreamFingerprintSecret(tx); err != nil {
			return err
		}
		var account SupplierAccount
		if err := lockForUpdate(tx).First(&account, "supplier_id = ?", owner).Error; err != nil {
			return err
		}
		var channel Channel
		if err := lockForUpdate(tx).First(&channel, "id = ? AND owner_supplier_id = ?", channelID, owner).Error; err != nil {
			return err
		}
		var draft SupplierChannelDraft
		if err := lockForUpdate(tx).First(&draft, "channel_id = ? AND supplier_id = ? AND version = ?", channelID, owner, version).Error; err != nil {
			return err
		}
		if channel.SupplierVersion != version || draft.Status != SupplierDraftReady || draft.TestOnly || SupplierChannelRevision(&channel, &draft) != revision {
			return common.NewMessage("Conflict: the resource was updated. Refresh and try again.")
		}
		if err := routing.Validate(); err != nil {
			return err
		}
		testedModels := strings.Split(draft.TestedModels, ",")
		for _, name := range models {
			if !slices.Contains(testedModels, name) {
				return common.NewMessage("Select only models that passed testing.")
			}
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
		if !channel.SupplierValidated {
			channel.Status = common.ChannelStatusEnabled
		}
		channel.Name, channel.Remark = draft.Name, &draft.Remark
		channel.Models, channel.SupplierValidated = strings.Join(models, ","), true
		channel.Group, channel.Weight = strings.Join(routing.Groups, ","), &routing.Weight
		channel.Priority, channel.SupplierPriority = &routing.Priority, routing.Priority
		if channel.CostRatio == nil || channel.CostVersionID == account.DefaultCostVersionID() {
			channel.CostRatio, channel.CostVersionID = account.CostRatio, account.DefaultCostVersionID()
		}
		channel.Keys = nil
		channel.ChannelInfo.IsMultiKey = strings.Contains(secret, "\n")
		channel.ChannelInfo.MultiKeySize = len(channel.GetKeys())
		if err := tx.Model(&channel).Select("type", "setting", "name", "remark", "key", "base_url", "models", "supplier_validated", "secrets_write_only", "cost_ratio", "cost_version_id",
			"status", "group", "priority", "supplier_priority", "weight", "channel_info").Updates(&channel).Error; err != nil {
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
		BumpSyncTopic(SyncTopicChannels)
	}
	return err
}

func UpdateSupplierChannelRouting(id int, owner, revision string, routing SupplierChannelRouting) error {
	err := DB.Transaction(func(tx *gorm.DB) error {
		var channel Channel
		if err := lockForUpdate(tx).First(&channel, "id = ? AND owner_supplier_id = ?", id, owner).Error; err != nil {
			return err
		}
		var draft SupplierChannelDraft
		err := lockForUpdate(tx).First(&draft, "channel_id = ? AND supplier_id = ? AND version = ?", id, owner, channel.SupplierVersion).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if !channel.SupplierValidated || SupplierChannelRevision(&channel, &draft) != revision {
			return common.NewMessage("Conflict: the resource was updated. Refresh and try again.")
		}
		if err := routing.Validate(); err != nil {
			return err
		}
		channel.Group, channel.Weight = strings.Join(routing.Groups, ","), &routing.Weight
		channel.Priority, channel.SupplierPriority = &routing.Priority, routing.Priority
		if err := tx.Model(&channel).Select("group", "weight", "priority", "supplier_priority").Updates(&channel).Error; err != nil {
			return err
		}
		return channel.UpdateAbilities(tx)
	})
	if err == nil {
		invalidateRoutingMetadata()
		BumpSyncTopic(SyncTopicChannels)
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

// CloseSupplierChannelDraft preserves test and channel records. An unpublished
// channel's closed draft no longer counts toward the supplier's channel limit.
func CloseSupplierChannelDraft(id int, owner string, version int64) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		// Match draft creation's lock order so closure and allocation see the
		// same available channel slots even on SQLite.
		if err := tx.Model(&SupplierAccount{}).Where("supplier_id = ?", owner).UpdateColumn("updated_at", gorm.Expr("updated_at")).Error; err != nil {
			return err
		}
		var channel Channel
		if err := lockForUpdate(tx).Omit("key").First(&channel, "id = ? AND owner_supplier_id = ?", id, owner).Error; err != nil {
			return err
		}
		var draft SupplierChannelDraft
		if err := lockForUpdate(tx).First(&draft, "channel_id = ? AND supplier_id = ? AND version = ?", id, owner, version).Error; err != nil {
			return err
		}
		if channel.SupplierVersion != version || draft.TestOnly || draft.Status == "validated" || draft.Status == SupplierDraftClosed {
			return common.NewMessage("Conflict: the resource was updated. Refresh and try again.")
		}
		status := draft.ValidationStatus()
		if status == "testing" || status == "waiting" || status == "result_unknown" || status == "pending" && draft.ProbeTaskIDs != "" {
			return common.NewMessage("A channel test is already in progress.")
		}
		return tx.Model(&draft).Updates(map[string]any{"status": SupplierDraftClosed, "lease_until": 0}).Error
	})
}
