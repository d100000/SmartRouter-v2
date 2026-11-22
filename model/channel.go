package model

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/google/uuid"
	"github.com/samber/lo"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

type Channel struct {
	OwnerSupplierID          string                 `json:"owner_supplier_id" gorm:"type:varchar(36);index"`
	CreatedBy                int                    `json:"created_by"`
	SupplierPriority         int64                  `json:"supplier_priority" gorm:"bigint"`
	SupplierPaused           bool                   `json:"supplier_paused"`
	SupplierVersion          int64                  `json:"supplier_version" gorm:"bigint"`
	SupplierValidated        bool                   `json:"supplier_validated"`
	SupplierValidationStatus string                 `json:"supplier_validation_status,omitempty" gorm:"-"`
	SecretsWriteOnly         bool                   `json:"secrets_write_only"`
	Funding                  *ChannelFundingSummary `json:"funding_status,omitempty" gorm:"-"`
	Id                       int                    `json:"id"`
	Type                     int                    `json:"type" gorm:"default:0"`
	Key                      string                 `json:"key" gorm:"not null"`
	OpenAIOrganization       *string                `json:"openai_organization"`
	TestModel                *string                `json:"test_model"`
	Status                   int                    `json:"status" gorm:"default:1"`
	Name                     string                 `json:"name" gorm:"index"`
	Weight                   *uint                  `json:"weight" gorm:"default:0"`
	CreatedTime              int64                  `json:"created_time" gorm:"bigint"`
	TestTime                 int64                  `json:"test_time" gorm:"bigint"`
	ResponseTime             int                    `json:"response_time"` // in milliseconds
	BaseURL                  *string                `json:"base_url" gorm:"column:base_url;default:''"`
	Other                    string                 `json:"other"`
	Balance                  float64                `json:"balance"` // in USD
	BalanceUpdatedTime       int64                  `json:"balance_updated_time" gorm:"bigint"`
	Models                   string                 `json:"models"`
	Group                    string                 `json:"group" gorm:"type:varchar(64);default:'default'"`
	UsedQuota                int64                  `json:"used_quota" gorm:"bigint;default:0"`
	CostRatio                *float64               `json:"cost_ratio"`
	CostVersionID            string                 `json:"cost_version_id" gorm:"type:varchar(36)"`
	ModelMapping             *string                `json:"model_mapping" gorm:"type:text"`
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

	Availability         string                    `json:"availability" gorm:"type:text"`
	EffectiveStatus      int                       `json:"effective_status" gorm:"-"`
	AvailabilityState    *ChannelAvailabilityState `json:"availability_state,omitempty" gorm:"-"`
	compiledAvailability *channelAvailabilityPlan

	// cache info
	BalanceDetail  *ChannelBalanceDetail  `json:"balance_detail,omitempty" gorm:"-"`
	Keys           []string               `json:"-" gorm:"-"`
	CostRatioSet   bool                   `json:"-" gorm:"-"`
	CostRuleChange *ChannelCostRuleChange `json:"-" gorm:"-"`
	// A validation candidate uses a known credential ID only when its material
	// and namespace match. A non-nil empty value means newly submitted material.
	ProcurementCredentialID        *string `json:"-" gorm:"-"`
	UpstreamCredentialSnapshotData string  `json:"-" gorm:"type:text"`
	keyStateExpected               *channelKeyState
	upstreamCredentialSnapshots    map[int]UpstreamCredentialSnapshot `json:"-" gorm:"-"`
	upstreamCredentialMirror       string                             `json:"-" gorm:"-"`
	// A credential this node cannot open: Key is empty, sealedKey holds the
	// stored value so a save writes it back unchanged. Never persisted.
	credentialUnavailable bool
	sealedKey             string
}

// CostRuleChange freezes the committed rule transition for safe administrative
// auditing. It is request-local metadata and never enters the channel schema.
type ChannelCostRuleChange struct {
	BeforeRatio     *float64
	AfterRatio      *float64
	BeforeVersionID string
	AfterVersionID  string
}

type channelKeyState struct {
	key         string
	status      int
	otherInfo   string
	channelInfo ChannelInfo
}

// ExpectKeyState protects an administrator's edit from overwriting a newer
// credential list or availability decision. Polling cursor changes are allowed.
func (channel *Channel) ExpectKeyState(current *Channel) {
	info := current.ChannelInfo
	info.MultiKeyStatusList = maps.Clone(info.MultiKeyStatusList)
	info.MultiKeyDisabledReason = maps.Clone(info.MultiKeyDisabledReason)
	info.MultiKeyDisabledTime = maps.Clone(info.MultiKeyDisabledTime)
	channel.keyStateExpected = &channelKeyState{key: current.Key, status: current.Status, otherInfo: current.OtherInfo, channelInfo: info}
}

const ChannelStatusReasonAllKeysDisabled = "All keys are disabled"

type ChannelInfo struct {
	IsMultiKey             bool                  `json:"is_multi_key"`                        // 是否多Key模式
	MultiKeySize           int                   `json:"multi_key_size"`                      // 多Key模式下的Key数量
	MultiKeyStatusList     map[int]int           `json:"multi_key_status_list"`               // key状态列表，key index -> status
	MultiKeyDisabledReason map[int]string        `json:"multi_key_disabled_reason,omitempty"` // key禁用原因列表，key index -> reason
	MultiKeyDisabledTime   map[int]int64         `json:"multi_key_disabled_time,omitempty"`   // key禁用时间列表，key index -> time
	MultiKeyPollingIndex   int                   `json:"multi_key_polling_index"`             // 多Key模式下轮询的key索引
	MultiKeyMode           constant.MultiKeyMode `json:"multi_key_mode"`
}

type ChannelSortOptions struct {
	SortBy    string
	SortOrder string
	IDSort    bool
}

var channelSortColumns = map[string]string{
	"id":            "id",
	"name":          "name",
	"priority":      "priority",
	"balance":       "balance",
	"response_time": "response_time",
	"test_time":     "test_time",
}

func NewChannelSortOptions(sortBy string, sortOrder string, idSort bool) ChannelSortOptions {
	normalizedSortBy := strings.ToLower(strings.TrimSpace(sortBy))
	normalizedSortOrder := strings.ToLower(strings.TrimSpace(sortOrder))
	if _, ok := channelSortColumns[normalizedSortBy]; !ok {
		normalizedSortBy = ""
		normalizedSortOrder = ""
	} else if normalizedSortOrder != "asc" {
		normalizedSortOrder = "desc"
	}

	return ChannelSortOptions{
		SortBy:    normalizedSortBy,
		SortOrder: normalizedSortOrder,
		IDSort:    idSort,
	}
}

func (options ChannelSortOptions) Apply(query *gorm.DB) *gorm.DB {
	if columnName, ok := channelSortColumns[options.SortBy]; ok {
		column := clause.Column{Name: columnName}
		if columnName == "priority" {
			// Routing treats a missing priority as zero on every database.
			column = clause.Column{Name: "COALESCE(priority, 0)", Raw: true}
		}
		return query.Order(clause.OrderByColumn{
			Column: column,
			Desc:   options.SortOrder != "asc",
		}).Order(clause.OrderByColumn{Column: clause.Column{Name: "id"}, Desc: true})
	}
	if options.IDSort {
		return query.Order(clause.OrderByColumn{
			Column: clause.Column{Name: "id"},
			Desc:   true,
		})
	}
	return query.Order(clause.OrderByColumn{
		Column: clause.Column{Name: "COALESCE(priority, 0)", Raw: true},
		Desc:   true,
	}).Order(clause.OrderByColumn{Column: clause.Column{Name: "id"}, Desc: true})
}

func resolveChannelSortOptions(idSort bool, sortOptions []ChannelSortOptions) ChannelSortOptions {
	if len(sortOptions) == 0 {
		return NewChannelSortOptions("", "", idSort)
	}
	options := sortOptions[0]
	options.IDSort = options.IDSort || idSort
	return options
}

func NormalizeChannelGroupFilter(group string) string {
	group = strings.TrimSpace(group)
	if group == "" || strings.EqualFold(group, "all") || strings.EqualFold(group, "null") {
		return ""
	}
	return group
}

func channelGroupFilterCondition() string {
	if common.UsingMainDatabase(common.DatabaseTypeMySQL) {
		return `CONCAT(',', ` + commonGroupCol + `, ',') LIKE ? ESCAPE '!'`
	}
	return `(',' || ` + commonGroupCol + ` || ',') LIKE ? ESCAPE '!'`
}

func channelGroupFilterPattern(group string) string {
	group = strings.NewReplacer(
		"!", "!!",
		"%", "!%",
		"_", "!_",
	).Replace(group)
	return "%," + group + ",%"
}

func ApplyChannelGroupFilter(query *gorm.DB, group string) *gorm.DB {
	group = NormalizeChannelGroupFilter(group)
	if group == "" {
		return query
	}
	return query.Where(channelGroupFilterCondition(), channelGroupFilterPattern(group))
}

// Value implements driver.Valuer interface
// 必须返回 string 而非 []byte:PG simple protocol 下 []byte 参数按 bytea
// 编码,写 json 列会触发 SQLSTATE 22P02。
func (c ChannelInfo) Value() (driver.Value, error) {
	b, err := common.Marshal(&c)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// Scan implements sql.Scanner interface
func (c *ChannelInfo) Scan(value any) error {
	return common.Unmarshal(jsonScanBytes(value), c)
}

// ApplyChannelSupplierFilter scopes list, count, search, and tag expansion by
// immutable channel owner, independently of procurement labels on old keys.
func ApplyChannelSupplierFilter(query *gorm.DB, owner string) *gorm.DB {
	switch strings.TrimSpace(owner) {
	case "":
		return query
	case "unassigned":
		return query.Where("owner_supplier_id IS NULL OR owner_supplier_id = ?", "")
	default:
		return query.Where("owner_supplier_id = ?", strings.TrimSpace(owner))
	}
}

func (channel *Channel) HasPublishedConfiguration() bool {
	return channel.OwnerSupplierID == "" || channel.SupplierValidated
}

// publishedChannelQuery keeps supplier drafts out of generic status operations.
func publishedChannelQuery(query *gorm.DB) *gorm.DB {
	return query.Where("owner_supplier_id IS NULL OR owner_supplier_id = ? OR supplier_validated = ?", "", true)
}

func (channel *Channel) GetKeys() []string {
	if strings.HasPrefix(channel.Key, supplierSecretPrefix) {
		copy := *channel
		if copy.openSupplierCredential() != nil {
			return nil
		}
		copy.Keys = nil
		return copy.GetKeys()
	}
	if channel.Key == "" {
		return []string{}
	}
	if len(channel.Keys) > 0 {
		return channel.Keys
	}
	trimmed := strings.TrimSpace(channel.Key)
	// If the key starts with '[', try to parse it as a JSON array (e.g., for Vertex AI scenarios)
	if strings.HasPrefix(trimmed, "[") {
		var arr []json.RawMessage
		if err := common.Unmarshal([]byte(trimmed), &arr); err == nil {
			res := make([]string, len(arr))
			for i, v := range arr {
				res[i] = string(v)
			}
			return res
		}
	}
	// Otherwise, fall back to splitting by newline
	keys := strings.Split(strings.Trim(channel.Key, "\n"), "\n")
	return keys
}

func (channel *Channel) GetNextEnabledKey() (string, int, *types.NewAPIError) {
	if channel.credentialUnavailable {
		// A node-local key ring problem: retry elsewhere, never disable the
		// channel for every node.
		return "", 0, types.NewError(errChannelCredentialUnavailable, types.ErrorCodeGetChannelFailed)
	}
	// If not in multi-key mode, return the original key string directly.
	if !channel.ChannelInfo.IsMultiKey {
		if strings.HasPrefix(channel.Key, supplierSecretPrefix) {
			secret, err := DecryptSupplierSecret(channel.OwnerSupplierID, channel.Key)
			if err != nil {
				return "", 0, types.NewError(err, types.ErrorCodeChannelNoAvailableKey)
			}
			return secret, 0, nil
		}
		return channel.Key, 0, nil
	}

	// Obtain all keys (split by \n)
	keys := channel.GetKeys()
	if len(keys) == 0 {
		// No keys available, return error, should disable the channel
		return "", 0, types.NewError(errors.New("no keys available"), types.ErrorCodeChannelNoAvailableKey)
	}

	lock := GetChannelPollingLock(channel.Id)
	lock.Lock()
	defer lock.Unlock()

	statusList := channel.ChannelInfo.MultiKeyStatusList
	// helper to get key status, default to enabled when missing
	getStatus := func(idx int) int {
		if statusList == nil {
			return common.ChannelStatusEnabled
		}
		if status, ok := statusList[idx]; ok {
			return status
		}
		return common.ChannelStatusEnabled
	}

	// Collect indexes of enabled keys
	enabledIdx := make([]int, 0, len(keys))
	for i := range keys {
		if getStatus(i) == common.ChannelStatusEnabled {
			enabledIdx = append(enabledIdx, i)
		}
	}
	// If no specific status list or none enabled, return an explicit error so caller can
	// properly handle a channel with no available keys (e.g. mark channel disabled).
	// Returning the first key here caused requests to keep using an already-disabled key.
	if len(enabledIdx) == 0 {
		return "", 0, types.NewError(errors.New("no enabled keys"), types.ErrorCodeChannelNoAvailableKey)
	}

	switch channel.ChannelInfo.MultiKeyMode {
	case constant.MultiKeyModeRandom:
		// Randomly pick one enabled key
		selectedIdx := enabledIdx[rand.Intn(len(enabledIdx))]
		return keys[selectedIdx], selectedIdx, nil
	case constant.MultiKeyModePolling:
		// The next enabled key at or after the saved cursor.
		pick := func(start int) int {
			if start < 0 || start >= len(keys) {
				start = 0
			}
			for i := range keys {
				if idx := (start + i) % len(keys); getStatus(idx) == common.ChannelStatusEnabled {
					return idx
				}
			}
			return enabledIdx[0]
		}
		var selected int
		if common.MemoryCacheEnabled {
			// Routing hands out copies of the cached channel, so the cursor
			// advances on the cached entry that every copy reads.
			if !advanceCachedPollingIndex(channel.Id, func(start int) int {
				selected = pick(start)
				return (selected + 1) % len(keys)
			}) {
				return "", 0, types.NewError(fmt.Errorf("channel #%d no longer exists", channel.Id), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
			}
			// The receiver may be the shared cache entry itself; the cursor was
			// already advanced there under the cache lock.
			return keys[selected], selected, nil
		}
		channelInfo, err := CacheGetChannelInfo(channel.Id)
		if err != nil {
			return "", 0, types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
		}
		selected = pick(channelInfo.MultiKeyPollingIndex)
		channel.ChannelInfo.MultiKeyPollingIndex = (selected + 1) % len(keys)
		if common.DebugEnabled {
			logger.LogDebug(nil, "channel %d polling index: %d", channel.Id, channel.ChannelInfo.MultiKeyPollingIndex)
		}
		_ = channel.SaveChannelInfo()
		return keys[selected], selected, nil
	default:
		// Unknown mode, default to first enabled key (or original key string)
		return keys[enabledIdx[0]], enabledIdx[0], nil
	}
}

func (channel *Channel) SaveChannelInfo() error {
	// Polling owns only the cursor. A request can hold an older channel snapshot
	// after another instance has edited keys or disabled one of them.
	return DB.Transaction(func(tx *gorm.DB) error {
		var current Channel
		if err := lockForUpdate(tx).Select("id", "key", "channel_info").First(&current, channel.Id).Error; err != nil {
			return err
		}
		if current.Key != channel.Key || !current.ChannelInfo.IsMultiKey || current.ChannelInfo.MultiKeyMode != channel.ChannelInfo.MultiKeyMode {
			return nil
		}
		current.ChannelInfo.MultiKeyPollingIndex = channel.ChannelInfo.MultiKeyPollingIndex
		return tx.Model(&Channel{}).Where("id = ?", channel.Id).Update("channel_info", current.ChannelInfo).Error
	})
}

func (channel *Channel) GetModels() []string {
	if channel.Models == "" {
		return []string{}
	}
	return strings.Split(strings.Trim(channel.Models, ","), ",")
}

func (channel *Channel) GetGroups() []string {
	if channel.Group == "" {
		return []string{}
	}
	groups := strings.Split(strings.Trim(channel.Group, ","), ",")
	for i, group := range groups {
		groups[i] = strings.TrimSpace(group)
	}
	return groups
}

func (channel *Channel) GetOtherInfo() map[string]any {
	otherInfo := make(map[string]any)
	if channel.OtherInfo != "" {
		err := common.Unmarshal([]byte(channel.OtherInfo), &otherInfo)
		if err != nil {
			common.SysLog(common.LogText("failed to unmarshal other info: channel_id=%d, tag=%s, name=%s, error=%v", channel.Id, channel.GetTag(), channel.Name, err))
		}
	}
	return otherInfo
}

func (channel *Channel) SetOtherInfo(otherInfo map[string]any) {
	otherInfoBytes, err := common.Marshal(otherInfo)
	if err != nil {
		common.SysLog(common.LogText("failed to marshal other info: channel_id=%d, tag=%s, name=%s, error=%v", channel.Id, channel.GetTag(), channel.Name, err))
		return
	}
	channel.OtherInfo = string(otherInfoBytes)
}

func (channel *Channel) GetTag() string {
	if channel.Tag == nil {
		return ""
	}
	return *channel.Tag
}

func (channel *Channel) SetTag(tag string) {
	channel.Tag = &tag
}

func (channel *Channel) GetAutoBan() bool {
	if channel.AutoBan == nil {
		return false
	}
	return *channel.AutoBan == 1
}

// ErrChannelOtherInfoBusy reports a channel whose other_info other writers
// kept changing through every attempt of UpdateChannelOtherInfo.
var ErrChannelOtherInfoBusy = errors.New("channel other_info kept changing during the update")

func (channel *Channel) saveStatusState(transactions ...*gorm.DB) error {
	if channel.Id == 0 {
		return errors.New("channel ID is 0")
	}
	updates := map[string]any{
		"status":     channel.Status,
		"other_info": channel.OtherInfo,
	}
	if channel.ChannelInfo.IsMultiKey {
		updates["channel_info"] = channel.ChannelInfo
	}
	db := DB
	if len(transactions) > 0 && transactions[0] != nil {
		db = transactions[0]
	}
	return db.Model(&Channel{}).Where("id = ?", channel.Id).Updates(updates).Error
}

const channelOtherInfoAttempts = 5

// UpdateChannelKeyStatus runs change on the stored multi-key channel and
// writes the status, other_info and channel_info it leaves, all in one
// transaction under the channel's row lock (a process lock and an early write
// lock on SQLite). A key status another node writes meanwhile, such as a
// per-key auto-disable, is read before change runs, never overwritten.
// Abilities follow a status change. The caller holds the polling lock and
// reloads the channel cache.
func UpdateChannelKeyStatus(channelID int, change func(stored *Channel) error) error {
	defer invalidateRoutingMetadata()
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		keyLock := sqliteChannelKeyLock(channelID)
		keyLock.Lock()
		defer keyLock.Unlock()
	}
	err := DB.Transaction(func(tx *gorm.DB) error {
		if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
			if err := tx.Model(&Channel{}).Where("id = ?", channelID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
				return err
			}
		}
		var stored Channel
		if err := lockForUpdate(tx).First(&stored, "id = ?", channelID).Error; err != nil {
			return err
		}
		if stored.credentialUnavailable {
			return errChannelCredentialUnavailable
		}
		status := stored.Status
		if err := change(&stored); err != nil {
			return err
		}
		if err := tx.Model(&Channel{}).Where("id = ?", channelID).Updates(map[string]any{
			"status":       stored.Status,
			"other_info":   stored.OtherInfo,
			"channel_info": stored.ChannelInfo,
		}).Error; err != nil {
			return err
		}
		enabled := stored.Status == common.ChannelStatusEnabled
		if stored.Status == status || enabled && !stored.HasPublishedConfiguration() {
			return nil
		}
		return tx.Model(&Ability{}).Where("channel_id = ?", channelID).Update("enabled", enabled).Error
	})
	if err != nil {
		return err
	}
	BumpSyncTopic(SyncTopicChannels)
	return nil
}

// UpdateChannelOtherInfo runs update on the stored channel and writes the
// other_info it leaves, with the columns update returns, only while the
// stored other_info is still the one read. When another writer, on any node,
// changed it in between, the channel is read again and update runs on that,
// so neither change is lost: update may run more than once and must not wait
// on the network. It returns nil to write nothing, and written reports a
// write. Only the returned columns and other_info are written, so a stale
// read never puts back the key, counters or settings. On SQLite, which serves
// one node, the channel's key lock also keeps the locked updates of this
// process out.
func UpdateChannelOtherInfo(channelID int, update func(channel *Channel) (map[string]any, error)) (written bool, err error) {
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		lock := sqliteChannelKeyLock(channelID)
		lock.Lock()
		defer lock.Unlock()
	}
	for range channelOtherInfoAttempts {
		var channel Channel
		if err := DB.First(&channel, "id = ?", channelID).Error; err != nil {
			return false, err
		}
		read := channel.OtherInfo
		updates, err := update(&channel)
		if err != nil || updates == nil {
			return false, err
		}
		updates["other_info"] = channel.OtherInfo
		query := DB.Model(&Channel{}).Where("id = ?", channelID)
		if read == "" {
			query = query.Where("(other_info IS NULL OR other_info = ?)", "")
		} else {
			query = query.Where("other_info = ?", read)
		}
		result := query.Updates(updates)
		if result.Error != nil {
			return false, result.Error
		}
		if result.RowsAffected > 0 {
			return true, nil
		}
		// MySQL counts changed rows only: when other_info is still the one
		// read, the row matched and already held every value written.
		if common.UsingMainDatabase(common.DatabaseTypeMySQL) && channel.OtherInfo == read {
			var stored Channel
			if err := DB.Select("other_info").First(&stored, "id = ?", channelID).Error; err != nil {
				return false, err
			}
			if stored.OtherInfo == read {
				return true, nil
			}
		}
	}
	return false, ErrChannelOtherInfoBusy
}

func GetAllChannels(startIdx int, num int, selectAll bool, idSort bool, sortOptions ...ChannelSortOptions) ([]*Channel, error) {
	var channels []*Channel
	var err error
	order := resolveChannelSortOptions(idSort, sortOptions)
	if selectAll {
		err = order.Apply(DB).Find(&channels).Error
	} else {
		err = order.Apply(DB).Limit(num).Offset(startIdx).Omit("key", "upstream_credential_snapshot_data").Find(&channels).Error
	}
	return channels, err
}

func GetChannelsByTag(tag string, idSort bool, selectAll bool, sortOptions ...ChannelSortOptions) ([]*Channel, error) {
	var channels []*Channel
	order := resolveChannelSortOptions(idSort, sortOptions)
	query := order.Apply(DB.Where("tag = ?", tag))
	if !selectAll {
		query = query.Omit("key", "upstream_credential_snapshot_data")
	}
	err := query.Find(&channels).Error
	return channels, err
}

func SearchChannels(keyword string, group string, model string, idSort bool, sortOptions ...ChannelSortOptions) ([]*Channel, error) {
	return SearchChannelsBySupplier(keyword, group, model, idSort, "", sortOptions...)
}

func SearchChannelsBySupplier(keyword, group, model string, idSort bool, owner string, sortOptions ...ChannelSortOptions) ([]*Channel, error) {
	var channels []*Channel

	order := resolveChannelSortOptions(idSort, sortOptions)

	// 构造基础查询
	baseQuery := ApplyChannelSupplierFilter(DB.Model(&Channel{}), owner).Omit("key", "upstream_credential_snapshot_data")

	search := DB.Where("id = ?", common.String2Int(keyword)).
		Or("name LIKE ?", "%"+keyword+"%").
		Or(clause.Eq{Column: clause.Column{Name: "key"}, Value: keyword}).
		Or("base_url LIKE ?", "%"+keyword+"%")
	baseQuery = ApplyChannelGroupFilter(baseQuery.Where(search).Where("models LIKE ?", "%"+model+"%"), group)

	// 执行查询
	err := order.Apply(baseQuery).Find(&channels).Error
	if err != nil {
		return nil, err
	}
	return channels, nil
}

// GetChannelById loads a channel directly from the database, bypassing the
// in-memory channel cache.
//
// WARNING: do NOT call this on request hot paths (middleware, distribution,
// relay submit/retry, polling). Every call is a synchronous DB query and will
// not see cache-only state. Use CacheGetChannel instead: it serves from the
// in-memory cache and falls back to this function automatically when
// MemoryCacheEnabled is false. Direct use is appropriate only where fresh DB
// state is required, e.g. admin CRUD, channel testing, or cache (re)building.
func GetChannelById(id int, selectAll bool) (*Channel, error) {
	channel := &Channel{Id: id}
	var err error = nil
	if selectAll {
		err = DB.First(channel, "id = ?", id).Error
	} else {
		err = DB.Omit("key", "upstream_credential_snapshot_data").First(channel, "id = ?", id).Error
	}
	if err != nil {
		return nil, err
	}
	return channel, nil
}

func BatchInsertChannels(channels []Channel) error {
	defer invalidateRoutingMetadata()
	if len(channels) == 0 {
		return nil
	}
	for i := range channels {
		if err := ValidateUpstreamCostRatio(channels[i].CostRatio); err != nil {
			return err
		}
		// Same rule as Insert: an invalid schedule would silently never route.
		if err := channels[i].ValidateAvailability(); err != nil {
			return err
		}
		channels[i].CostVersionID = uuid.NewString()
		channels[i].UpstreamCredentialSnapshotData = ""
	}
	upstreamCredentialWriteMutex.Lock()
	defer upstreamCredentialWriteMutex.Unlock()
	tx := DB.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
			panic(r)
		}
	}()

	for chunk := range slices.Chunk(channels, 50) {
		if err := tx.Session(&gorm.Session{Logger: gormlogger.Discard}).Create(&chunk).Error; err != nil {
			tx.Rollback()
			return err
		}
		// A reused ID must not inherit abilities left by an earlier channel.
		chunkIDs := lo.Map(chunk, func(channel Channel, _ int) int { return channel.Id })
		if err := tx.Where("channel_id IN ?", chunkIDs).Delete(&Ability{}).Error; err != nil {
			tx.Rollback()
			return err
		}
		for _, channel_ := range chunk {
			if err := EnsureChannelLimitTracking(channel_.Id, true, time.Time{}, tx); err != nil {
				tx.Rollback()
				return err
			}
			if err := syncChannelUpstreamCredentials(tx, &channel_); err != nil {
				tx.Rollback()
				return err
			}
			if err := channel_.AddAbilities(tx); err != nil {
				tx.Rollback()
				return err
			}
		}
	}
	if err := tx.Commit().Error; err != nil {
		return err
	}
	BumpSyncTopic(SyncTopicChannels)
	return nil
}

func BatchDeleteChannels(ids []int) (int64, error) {
	defer invalidateRoutingMetadata()
	if len(ids) == 0 {
		return 0, nil
	}
	upstreamCredentialWriteMutex.Lock()
	defer upstreamCredentialWriteMutex.Unlock()
	// 使用事务 分批删除channel表和abilities表
	tx := DB.Begin()
	if tx.Error != nil {
		return 0, tx.Error
	}
	if _, err := upstreamFingerprintSecret(tx); err != nil {
		tx.Rollback()
		return 0, err
	}
	var deletedCount int64
	for _, chunk := range lo.Chunk(ids, 200) {
		var owned int64
		if err := tx.Model(&Channel{}).Where("id IN ? AND owner_supplier_id <> ?", chunk, "").Count(&owned).Error; err != nil {
			tx.Rollback()
			return 0, err
		}
		if owned > 0 {
			tx.Rollback()
			return 0, ErrSupplierChannelOperation
		}
		if err := tx.Model(&ChannelCredentialBinding{}).Where("channel_id IN ?", chunk).Updates(map[string]any{"active": false, "active_slot": nil}).Error; err != nil {
			tx.Rollback()
			return 0, err
		}
		result := tx.Where("id in (?)", chunk).Delete(&Channel{})
		if result.Error != nil {
			tx.Rollback()
			return 0, result.Error
		}
		deletedCount += result.RowsAffected
		if err := tx.Where("channel_id in (?)", chunk).Delete(&Ability{}).Error; err != nil {
			tx.Rollback()
			return 0, err
		}
	}
	if err := tx.Commit().Error; err != nil {
		return 0, err
	}
	BumpSyncTopic(SyncTopicChannels)
	return deletedCount, nil
}

func (channel *Channel) GetPriority() int64 {
	if channel.Priority == nil {
		return 0
	}
	return *channel.Priority
}

func (channel *Channel) GetWeight() int {
	if channel.Weight == nil {
		return 0
	}
	return int(*channel.Weight)
}

func (channel *Channel) GetBaseURL() string {
	if channel.BaseURL == nil {
		return ""
	}
	url := *channel.BaseURL
	if url == "" {
		url = constant.GetChannelBaseURL(channel.Type)
	}
	return url
}

func (channel *Channel) GetModelMapping() string {
	if channel.ModelMapping == nil {
		return ""
	}
	return *channel.ModelMapping
}

func (channel *Channel) GetStatusCodeMapping() string {
	if channel.StatusCodeMapping == nil {
		return ""
	}
	return *channel.StatusCodeMapping
}

func (channel *Channel) Insert() error {
	if err := channel.ValidateAvailability(); err != nil {
		return err
	}
	defer invalidateRoutingMetadata()
	if err := ValidateUpstreamCostRatio(channel.CostRatio); err != nil {
		return err
	}
	upstreamCredentialWriteMutex.Lock()
	defer upstreamCredentialWriteMutex.Unlock()
	channel.CostVersionID = uuid.NewString()
	channel.UpstreamCredentialSnapshotData = ""
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Session(&gorm.Session{Logger: gormlogger.Discard}).Create(channel).Error; err != nil {
			return err
		}
		if err := EnsureChannelLimitTracking(channel.Id, true, time.Time{}, tx); err != nil {
			return err
		}
		if err := syncChannelUpstreamCredentials(tx, channel); err != nil {
			return err
		}
		// A reused ID must not inherit abilities left by an earlier channel.
		if err := tx.Where("channel_id = ?", channel.Id).Delete(&Ability{}).Error; err != nil {
			return err
		}
		return channel.AddAbilities(tx)
	})
	if err == nil {
		BumpSyncTopic(SyncTopicChannels)
	}
	return err
}

// Update writes an edited channel, all but its key: a key changes only through
// UpdateChannelLocked, so an edit never writes back a key a refresh replaced
// after the edit read it.
func (channel *Channel) Update() error {
	return channel.UpdateWithKey(nil)
}

// UpdateWithKey commits an explicit key edit and the channel configuration in
// one transaction. The callback sees the current key under the identity and
// channel row locks; nil preserves a key refreshed after the edit was loaded.
func (channel *Channel) UpdateWithKey(updateKey func(*Channel) error) error {
	if err := channel.ValidateAvailability(); err != nil {
		return err
	}
	defer invalidateRoutingMetadata()
	channel.CostRuleChange = nil
	if err := ValidateUpstreamCostRatio(channel.CostRatio); err != nil {
		return err
	}
	pollingLock := GetChannelPollingLock(channel.Id)
	pollingLock.Lock()
	defer pollingLock.Unlock()
	upstreamCredentialWriteMutex.Lock()
	defer upstreamCredentialWriteMutex.Unlock()
	var costRuleChange *ChannelCostRuleChange
	err := DB.Transaction(func(tx *gorm.DB) error {
		if _, err := upstreamFingerprintSecret(tx); err != nil {
			return err
		}
		var existing Channel
		if err := lockForUpdate(tx).First(&existing, "id = ?", channel.Id).Error; err != nil {
			return err
		}
		if existing.OwnerSupplierID != "" && channel.OwnerSupplierID != "" && channel.OwnerSupplierID != existing.OwnerSupplierID {
			return errors.New("supplier channel ownership cannot be transferred")
		}
		channel.OwnerSupplierID = existing.OwnerSupplierID
		channel.SecretsWriteOnly = existing.SecretsWriteOnly || existing.OwnerSupplierID != ""
		channel.CreatedBy = existing.CreatedBy
		channel.SupplierPriority = existing.SupplierPriority
		channel.SupplierPaused = existing.SupplierPaused
		channel.SupplierValidated = existing.SupplierValidated
		channel.SupplierVersion = existing.SupplierVersion
		if expected := channel.keyStateExpected; expected != nil {
			keysMatch := existing.Key == expected.key
			if !keysMatch && existing.Type == constant.ChannelTypeTaskPlugin {
				// A refresh may change opaque material while each slot still
				// belongs to the same verified plugin account. Let independent
				// edits merge with that rotation, never with an account swap.
				previous := Channel{Key: expected.key, ChannelInfo: expected.channelInfo}
				previousKeys, currentKeys := channelCredentialKeys(&previous), channelCredentialKeys(&existing)
				keysMatch = len(previousKeys) == len(currentKeys)
				if keysMatch {
					for index, oldKey := range previousKeys {
						if oldKey == currentKeys[index] {
							continue
						}
						oldCredential, oldOK := jsplugin.ParseCredential(oldKey)
						newCredential, newOK := jsplugin.ParseCredential(currentKeys[index])
						if !oldOK || !newOK || oldCredential.AccountID == "" || oldCredential.AccountID != newCredential.AccountID || oldCredential.Plugin != newCredential.Plugin {
							keysMatch = false
							break
						}
					}
				}
			}
			if !keysMatch || existing.ChannelInfo.IsMultiKey != expected.channelInfo.IsMultiKey {
				return errors.New("channel key state changed; reload and retry")
			}
			if existing.Status != expected.status || existing.OtherInfo != expected.otherInfo ||
				existing.ChannelInfo.MultiKeyMode != expected.channelInfo.MultiKeyMode ||
				!reflect.DeepEqual(existing.ChannelInfo.MultiKeyStatusList, expected.channelInfo.MultiKeyStatusList) ||
				!reflect.DeepEqual(existing.ChannelInfo.MultiKeyDisabledReason, expected.channelInfo.MultiKeyDisabledReason) ||
				!reflect.DeepEqual(existing.ChannelInfo.MultiKeyDisabledTime, expected.channelInfo.MultiKeyDisabledTime) {
				return errors.New("channel key state changed; reload and retry")
			}
			if channel.Status == 0 || channel.Status == expected.status {
				channel.Status = existing.Status
			}
			if channel.OtherInfo == "" || channel.OtherInfo == expected.otherInfo {
				channel.OtherInfo = existing.OtherInfo
			}
		}
		previousKeys := existing.GetKeys()
		if updateKey != nil {
			if err := updateKey(&existing); err != nil {
				return err
			}
		}
		channel.Key = existing.Key
		if existing.credentialUnavailable && updateKey == nil && existing.ChannelInfo.IsMultiKey {
			// This node cannot read the stored keys, so it cannot map key
			// statuses. Keep the stored per-key state exactly as it is.
			channel.ChannelInfo.IsMultiKey = existing.ChannelInfo.IsMultiKey
			channel.ChannelInfo.MultiKeySize = existing.ChannelInfo.MultiKeySize
			channel.ChannelInfo.MultiKeyStatusList = existing.ChannelInfo.MultiKeyStatusList
			channel.ChannelInfo.MultiKeyDisabledReason = existing.ChannelInfo.MultiKeyDisabledReason
			channel.ChannelInfo.MultiKeyDisabledTime = existing.ChannelInfo.MultiKeyDisabledTime
			channel.ChannelInfo.MultiKeyPollingIndex = existing.ChannelInfo.MultiKeyPollingIndex
		} else if channel.ChannelInfo.IsMultiKey {
			keyList := Channel{Key: channel.Key}
			if keyList.Key == "" {
				keyList.Key = existing.Key
			}
			newKeys := keyList.GetKeys()
			oldKeys := previousKeys
			channel.ChannelInfo.MultiKeySize = len(newKeys)
			if existing.ChannelInfo.IsMultiKey && !slices.Equal(oldKeys, newKeys) {
				// Status belongs to the actual credential material, never its old
				// slot. Read the latest state under the same row lock as the edit.
				statuses := make(map[int]int)
				reasons := make(map[int]string)
				times := make(map[int]int64)
				oldSlots := make(map[string][]int, len(oldKeys))
				for oldIndex, oldKey := range oldKeys {
					oldSlots[oldKey] = append(oldSlots[oldKey], oldIndex)
				}
				for newIndex, newKey := range newKeys {
					indices := oldSlots[newKey]
					if len(indices) == 0 {
						continue
					}
					oldIndex := indices[0]
					oldSlots[newKey] = indices[1:]
					if status, ok := existing.ChannelInfo.MultiKeyStatusList[oldIndex]; ok {
						statuses[newIndex] = status
					}
					if reason, ok := existing.ChannelInfo.MultiKeyDisabledReason[oldIndex]; ok {
						reasons[newIndex] = reason
					}
					if disabledTime, ok := existing.ChannelInfo.MultiKeyDisabledTime[oldIndex]; ok {
						times[newIndex] = disabledTime
					}
				}
				channel.ChannelInfo.MultiKeyStatusList = statuses
				channel.ChannelInfo.MultiKeyDisabledReason = reasons
				channel.ChannelInfo.MultiKeyDisabledTime = times
			}
			for index := range channel.ChannelInfo.MultiKeyStatusList {
				if index < 0 || index >= len(newKeys) {
					delete(channel.ChannelInfo.MultiKeyStatusList, index)
					delete(channel.ChannelInfo.MultiKeyDisabledReason, index)
					delete(channel.ChannelInfo.MultiKeyDisabledTime, index)
				}
			}
			if channel.ChannelInfo.MultiKeyPollingIndex < 0 || channel.ChannelInfo.MultiKeyPollingIndex >= len(newKeys) {
				channel.ChannelInfo.MultiKeyPollingIndex = 0
			}
		}
		costProvided := channel.CostRatioSet || channel.CostRatio != nil
		channel.CostVersionID = existing.CostVersionID
		if channel.CostVersionID == "" || costProvided && !equalUpstreamCostRatio(channel.CostRatio, existing.CostRatio) {
			channel.CostVersionID = uuid.NewString()
		}
		if costProvided && !equalUpstreamCostRatio(channel.CostRatio, existing.CostRatio) {
			costRuleChange = &ChannelCostRuleChange{BeforeVersionID: existing.CostVersionID, AfterVersionID: channel.CostVersionID}
			if existing.CostRatio != nil {
				costRuleChange.BeforeRatio = common.GetPointer(*existing.CostRatio)
			}
			if channel.CostRatio != nil {
				costRuleChange.AfterRatio = common.GetPointer(*channel.CostRatio)
			}
		}
		// Never let a stale or client-provided mirror overwrite current identity.
		channel.UpstreamCredentialSnapshotData = ""
		query := tx.Session(&gorm.Session{Logger: gormlogger.Discard}).Model(channel).Omit("upstream_credential_snapshot_data")
		if updateKey == nil {
			query = query.Omit("key", "upstream_credential_snapshot_data")
		}
		if err := query.Updates(channel).Error; err != nil {
			return err
		}
		if costProvided {
			if err := tx.Model(&Channel{}).Where("id = ?", channel.Id).Update("cost_ratio", channel.CostRatio).Error; err != nil {
				return err
			}
		}
		if err := tx.First(channel, "id = ?", channel.Id).Error; err != nil {
			return err
		}
		channel.Keys = nil
		channel.keyStateExpected = nil
		if err := syncChannelUpstreamCredentials(tx, channel); err != nil {
			return err
		}
		return channel.UpdateAbilities(tx)
	})
	if err == nil {
		channel.CostRuleChange = costRuleChange
		CacheUpdateChannel(channel)
		BumpSyncTopic(SyncTopicChannels)
	}
	return err
}

// UpdateResponseTime writes only the test columns by ID. The receiver may be a
// shared cache entry: no save hook runs on it, so its sealed key stays sealed.
func (channel *Channel) UpdateResponseTime(responseTime int64) {
	channel.TestTime, channel.ResponseTime = common.GetTimestamp(), int(responseTime)
	err := DB.Model(&Channel{}).Where("id = ?", channel.Id).Updates(map[string]any{
		"test_time":     channel.TestTime,
		"response_time": channel.ResponseTime,
	}).Error
	if err != nil {
		common.SysLog(common.LogText("failed to update response time: channel_id=%d, error=%v", channel.Id, err))
	}
}

// UpdateBalance writes only the balance columns by ID, like UpdateResponseTime.
func (channel *Channel) UpdateBalance(balance float64) {
	channel.BalanceUpdatedTime, channel.Balance = common.GetTimestamp(), balance
	err := DB.Model(&Channel{}).Where("id = ?", channel.Id).Updates(map[string]any{
		"balance_updated_time": channel.BalanceUpdatedTime,
		"balance":              channel.Balance,
	}).Error
	if err != nil {
		common.SysLog(common.LogText("failed to update balance: channel_id=%d, error=%v", channel.Id, err))
	}
}

func (channel *Channel) Delete() error {
	defer invalidateRoutingMetadata()
	upstreamCredentialWriteMutex.Lock()
	defer upstreamCredentialWriteMutex.Unlock()
	err := DB.Transaction(func(tx *gorm.DB) error {
		var owned int64
		if err := tx.Model(&Channel{}).Where("id = ? AND owner_supplier_id <> ?", channel.Id, "").Count(&owned).Error; err != nil {
			return err
		}
		if owned > 0 {
			return ErrSupplierChannelOperation
		}
		if _, err := upstreamFingerprintSecret(tx); err != nil {
			return err
		}
		if err := tx.Model(&ChannelCredentialBinding{}).Where("channel_id = ?", channel.Id).Updates(map[string]any{"active": false, "active_slot": nil}).Error; err != nil {
			return err
		}
		if err := tx.Delete(channel).Error; err != nil {
			return err
		}
		return tx.Where("channel_id = ?", channel.Id).Delete(&Ability{}).Error
	})
	if err == nil {
		BumpSyncTopic(SyncTopicChannels)
	}
	return err
}

var channelStatusLock sync.Mutex

// channelPollingLocks stores locks for each channel.id to ensure thread-safe polling
var channelPollingLocks sync.Map

// GetChannelPollingLock returns or creates a mutex for the given channel ID
func GetChannelPollingLock(channelId int) *sync.Mutex {
	if lock, exists := channelPollingLocks.Load(channelId); exists {
		return lock.(*sync.Mutex)
	}
	// Create new lock for this channel
	newLock := &sync.Mutex{}
	actual, _ := channelPollingLocks.LoadOrStore(channelId, newLock)
	return actual.(*sync.Mutex)
}

// CleanupChannelPollingLocks removes locks for channels that no longer exist
// This is optional and can be called periodically to prevent memory leaks
func CleanupChannelPollingLocks() {
	var activeChannelIds []int
	DB.Model(&Channel{}).Pluck("id", &activeChannelIds)

	activeChannelSet := make(map[int]bool)
	for _, id := range activeChannelIds {
		activeChannelSet[id] = true
	}

	channelPollingLocks.Range(func(key, value any) bool {
		channelId := key.(int)
		if !activeChannelSet[channelId] {
			channelPollingLocks.Delete(channelId)
		}
		return true
	})
}

// sqlitePluginRefreshLocks serialize the credential refreshes of a channel on
// SQLite, which serves one node: channel id -> chan struct{} of capacity one.
var sqlitePluginRefreshLocks sync.Map

// pluginRefreshLockSpace is the first key of the PostgreSQL advisory locks of
// credential refreshes; the channel id is the second.
const pluginRefreshLockSpace = 0x6e617069

// WithPluginCredentialRefreshLock runs refresh while no other node or
// goroutine refreshes a credential of the channel, so a refresh token goes out
// once even when several nodes need a fresh credential at the same time. MySQL
// holds a named lock and PostgreSQL a transaction advisory lock, each on a
// connection of its own, and SQLite, which serves one node, a process lock.
// None of them locks the channel row: no writer of the channel waits for the
// network call refresh makes. ctx bounds the wait for the lock only.
func WithPluginCredentialRefreshLock(ctx context.Context, channelID int, refresh func() error) error {
	switch {
	case common.UsingMainDatabase(common.DatabaseTypeMySQL):
		name := fmt.Sprintf("new-api:plugin-refresh:%d", channelID)
		wait := 0
		if deadline, ok := ctx.Deadline(); ok {
			wait = max(int(time.Until(deadline)/time.Second), 0)
		}
		return DB.WithContext(ctx).Connection(func(conn *gorm.DB) error {
			var acquired sql.NullInt64
			if err := conn.Raw("SELECT GET_LOCK(?, ?)", name, wait).Row().Scan(&acquired); err != nil {
				return err
			}
			if acquired.Int64 != 1 {
				return ErrPluginRefreshLockTimeout
			}
			// The lock belongs to this connection until released, whatever
			// happened to ctx meanwhile.
			defer conn.WithContext(context.Background()).Exec("DO RELEASE_LOCK(?)", name)
			return refresh()
		})
	case common.UsingMainDatabase(common.DatabaseTypePostgreSQL):
		// The commit releases the lock; it must not fail because ctx ended
		// while refresh ran.
		return DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.WithContext(ctx).Exec("SELECT pg_advisory_xact_lock(?, ?)", pluginRefreshLockSpace, channelID).Error; err != nil {
				return err
			}
			return refresh()
		})
	default:
		value, _ := sqlitePluginRefreshLocks.LoadOrStore(channelID, make(chan struct{}, 1))
		lock := value.(chan struct{})
		select {
		case lock <- struct{}{}:
		case <-ctx.Done():
			return ErrPluginRefreshLockTimeout
		}
		defer func() { <-lock }()
		return refresh()
	}
}

// ErrPluginRefreshLockTimeout reports a credential refresh that waited for
// another refresh of the channel longer than it may.
var ErrPluginRefreshLockTimeout = errors.New("timed out waiting for another credential refresh of the channel")

func handlerMultiKeyUpdate(channel *Channel, usingKey string, status int, reason string) {
	keys := channel.GetKeys()
	if len(keys) == 0 {
		channel.Status = status
	} else {
		keyIndex := -1
		for i, key := range keys {
			if key == usingKey {
				keyIndex = i
				break
			}
		}
		if keyIndex < 0 {
			if usingKey != "" {
				common.SysLog(common.LogText("failed to update multi-key status: channel_id=%d, using key not found", channel.Id))
				return
			}
			channel.Status = status
			info := channel.GetOtherInfo()
			info["status_reason"] = reason
			info["status_time"] = common.GetTimestamp()
			channel.SetOtherInfo(info)
			return
		}
		updateMultiKeyStatusAtIndex(channel, keyIndex, status, reason)
	}
}

// Funding incidents address a binding, not key material: duplicate key slots
// still have distinct bindings and must not update the first matching slot.
func updateMultiKeyStatusAtIndex(channel *Channel, keyIndex, status int, reason string) {
	keys := channel.GetKeys()
	if keyIndex < 0 || keyIndex >= len(keys) {
		return
	}
	if channel.ChannelInfo.MultiKeyStatusList == nil {
		channel.ChannelInfo.MultiKeyStatusList = make(map[int]int)
	}
	// An automatic disable that arrives late must not turn a key an
	// administrator disabled into an auto-disabled one, which "delete
	// auto-disabled keys" would remove.
	if status == common.ChannelStatusAutoDisabled && channel.ChannelInfo.MultiKeyStatusList[keyIndex] == common.ChannelStatusManuallyDisabled {
		return
	}
	if status == common.ChannelStatusEnabled {
		delete(channel.ChannelInfo.MultiKeyStatusList, keyIndex)
		delete(channel.ChannelInfo.MultiKeyDisabledReason, keyIndex)
		delete(channel.ChannelInfo.MultiKeyDisabledTime, keyIndex)
	} else {
		channel.ChannelInfo.MultiKeyStatusList[keyIndex] = status
		if channel.ChannelInfo.MultiKeyDisabledReason == nil {
			channel.ChannelInfo.MultiKeyDisabledReason = make(map[int]string)
		}
		if channel.ChannelInfo.MultiKeyDisabledTime == nil {
			channel.ChannelInfo.MultiKeyDisabledTime = make(map[int]int64)
		}
		channel.ChannelInfo.MultiKeyDisabledReason[keyIndex] = reason
		channel.ChannelInfo.MultiKeyDisabledTime[keyIndex] = common.GetTimestamp()
	}
	if !hasEnabledMultiKey(keys, channel.ChannelInfo.MultiKeyStatusList) {
		channel.Status = common.ChannelStatusAutoDisabled
		info := channel.GetOtherInfo()
		info["status_reason"] = ChannelStatusReasonAllKeysDisabled
		info["status_time"] = common.GetTimestamp()
		channel.SetOtherInfo(info)
	} else if status == common.ChannelStatusEnabled {
		channel.Status = common.ChannelStatusEnabled
	}
}

func hasEnabledMultiKey(keys []string, statusList map[int]int) bool {
	for i := range keys {
		if statusList == nil {
			return true
		}
		status, ok := statusList[i]
		if !ok || status == common.ChannelStatusEnabled {
			return true
		}
	}
	return false
}

func UpdateChannelStatus(channelId int, usingKey string, status int, reason string) bool {
	return updateChannelStatus(channelId, usingKey, status, reason, false, false)
}

// ManageChannelStatus is the explicit administrator operation. It also lets
// administrators resume published channels paused by suppliers before upgrade.
func ManageChannelStatus(channelID, status int, reason string) bool {
	return updateChannelStatus(channelID, "", status, reason, false, true)
}

// EnableChannelAutomatically checks funding ownership under the same database
// lock as the status update. A separate read would race a new funding failure.
func EnableChannelAutomatically(channelId int, usingKey string) bool {
	return updateChannelStatus(channelId, usingKey, common.ChannelStatusEnabled, "", true, false)
}

func updateChannelStatus(channelId int, usingKey string, status int, reason string, automatic, administrative bool) bool {
	defer invalidateRoutingMetadata()
	if common.MemoryCacheEnabled {
		channelStatusLock.Lock()
		defer channelStatusLock.Unlock()
	}

	// ChannelInfo stores both multi-key status and the polling cursor. Hold the
	// same per-channel lock from the first read through persistence so neither
	// writer can save a stale JSON snapshot over the other.
	pollingLock := GetChannelPollingLock(channelId)
	pollingLock.Lock()
	defer pollingLock.Unlock()

	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		keyLock := sqliteChannelKeyLock(channelId)
		keyLock.Lock()
		defer keyLock.Unlock()
	}

	channel := &Channel{}
	changed := false
	bump := usingKey == "" && status != common.ChannelStatusAutoDisabled
	err := DB.Transaction(func(tx *gorm.DB) error {
		if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
			if err := tx.Model(&Channel{}).Where("id = ?", channelId).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
				return err
			}
		}
		if err := lockForUpdate(tx).First(channel, channelId).Error; err != nil {
			return err
		}
		if status == common.ChannelStatusEnabled && !channel.HasPublishedConfiguration() {
			return nil
		}
		resumingLegacyPause := administrative && status == common.ChannelStatusEnabled && channel.SupplierPaused
		if status == common.ChannelStatusAutoDisabled && channel.Status == common.ChannelStatusManuallyDisabled {
			return nil
		}
		if status == common.ChannelStatusAutoDisabled && !common.IsMasterNode && channelPluginHasHook(channel, "scheduled") {
			bump = true
		}
		if automatic {
			owned, err := fundingOwnedDisable(tx, channel, usingKey)
			if err != nil || owned || channel.Status == common.ChannelStatusManuallyDisabled {
				return err
			}
			if channel.ChannelInfo.IsMultiKey {
				for index, key := range channel.GetKeys() {
					if key == usingKey && channel.ChannelInfo.MultiKeyStatusList[index] == common.ChannelStatusManuallyDisabled {
						return nil
					}
				}
			}
		}
		// A manual channel operation must replace the exhaustion reason even
		// when the status value is already manually disabled.
		overridesKeyExhaustion := channel.ChannelInfo.IsMultiKey && usingKey == "" &&
			status == common.ChannelStatusManuallyDisabled && reason != ChannelStatusReasonAllKeysDisabled &&
			channel.GetOtherInfo()["status_reason"] == ChannelStatusReasonAllKeysDisabled
		overridesFunding := status == common.ChannelStatusAutoDisabled && strings.HasPrefix(fmt.Sprint(channel.GetOtherInfo()["status_reason"]), "Upstream funding restriction (event #")
		if channel.Status == status && !resumingLegacyPause && !overridesKeyExhaustion && !overridesFunding && (!channel.ChannelInfo.IsMultiKey || usingKey == "") {
			return nil
		}
		beforeStatus := channel.Status
		if channel.ChannelInfo.IsMultiKey {
			keys := channel.GetKeys()
			if usingKey != "" && !slices.Contains(keys, usingKey) {
				return nil
			}
			if status == common.ChannelStatusAutoDisabled && usingKey != "" &&
				channel.ChannelInfo.MultiKeyStatusList[slices.Index(keys, usingKey)] == common.ChannelStatusManuallyDisabled {
				return nil
			}
			handlerMultiKeyUpdate(channel, usingKey, status, reason)
		} else {
			info := channel.GetOtherInfo()
			info["status_reason"] = reason
			info["status_time"] = common.GetTimestamp()
			channel.SetOtherInfo(info)
			channel.Status = status
		}
		if err := channel.saveStatusState(tx); err != nil {
			return err
		}
		if resumingLegacyPause {
			if err := tx.Model(channel).UpdateColumn("supplier_paused", false).Error; err != nil {
				return err
			}
			channel.SupplierPaused = false
		}
		if beforeStatus != channel.Status {
			if err := tx.Model(&Ability{}).Where("channel_id = ?", channelId).Update("enabled", channel.Status == common.ChannelStatusEnabled).Error; err != nil {
				return err
			}
		}
		changed = true
		return nil
	})
	if err != nil {
		common.SysLog(fmt.Sprintf("failed to update channel status: channel_id=%d, status=%d, error=%v", channelId, status, err))
		return false
	}
	if changed && bump {
		BumpSyncTopic(SyncTopicChannels)
	} else if changed {
		// Other nodes must stop routing to a disabled channel or key before
		// their own failures or the periodic sync find it.
		bumpChannelsCoalesced()
	}
	CacheUpdateChannel(channel)
	CacheUpdateChannelStatus(channelId, channel.Status)
	return changed
}

// channelStatusBumpDelay is how long automatic status changes wait for each
// other before they bump the channels topic: a burst of failures adds one
// sync task, and every node reloads once, instead of once per channel.
var channelStatusBumpDelay = time.Second

var channelStatusBump struct {
	sync.Mutex
	timer *time.Timer
}

// bumpChannelsCoalesced bumps the channels topic within channelStatusBumpDelay.
// The bump runs after every change committed before it, so none is missed.
func bumpChannelsCoalesced() {
	channelStatusBump.Lock()
	defer channelStatusBump.Unlock()
	if channelStatusBump.timer == nil {
		channelStatusBump.timer = time.AfterFunc(channelStatusBumpDelay, flushChannelStatusBump)
	}
}

// flushChannelStatusBump adds the pending coalesced bump now.
func flushChannelStatusBump() {
	channelStatusBump.Lock()
	pending := channelStatusBump.timer != nil
	if pending {
		channelStatusBump.timer.Stop()
		channelStatusBump.timer = nil
	}
	channelStatusBump.Unlock()
	if pending {
		BumpSyncTopic(SyncTopicChannels)
	}
}

// channelPluginHasHook reports a task plugin channel whose plugin exports
// hook. It reads the setting without GetSetting, which clears an invalid
// stored setting.
func channelPluginHasHook(ch *Channel, hook string) bool {
	var setting dto.ChannelSettings
	if ch.Type != constant.ChannelTypeTaskPlugin || ch.Setting == nil || common.UnmarshalJsonStr(*ch.Setting, &setting) != nil {
		return false
	}
	plugin, ok := jsplugin.DefaultRegistry.Get(setting.TaskPluginKey)
	return ok && plugin.HasProviderHook(hook)
}

func EnableChannelByTag(tag string) error {
	err := DB.Transaction(func(tx *gorm.DB) error {
		channels := publishedChannelQuery(tx.Model(&Channel{}).Where("tag = ?", tag))
		if err := channels.Session(&gorm.Session{}).Update("status", common.ChannelStatusEnabled).Error; err != nil {
			return err
		}
		return tx.Model(&Ability{}).Where("channel_id IN (?)", channels.Select("id")).Update("enabled", true).Error
	})
	if err == nil {
		BumpSyncTopic(SyncTopicChannels)
	}
	return err
}

func DisableChannelByTag(tag string) error {
	err := DB.Transaction(func(tx *gorm.DB) error {
		var channels []Channel
		// Lock rows in id order, like the other multi-channel writers.
		if err := lockForUpdate(tx).Omit("key", "upstream_credential_snapshot_data").Where("tag = ?", tag).Order("id").Find(&channels).Error; err != nil {
			return err
		}
		// Explicit tag-level disable also cancels automatic restoration for
		// channels that were already disabled because all keys were unavailable.
		for _, channel := range channels {
			info := channel.GetOtherInfo()
			if !channel.ChannelInfo.IsMultiKey || info["status_reason"] != ChannelStatusReasonAllKeysDisabled {
				continue
			}
			info["status_reason"] = "manual tag operation"
			info["status_time"] = common.GetTimestamp()
			channel.SetOtherInfo(info)
			if err := tx.Model(&Channel{}).Where("id = ?", channel.Id).Update("other_info", channel.OtherInfo).Error; err != nil {
				return err
			}
		}
		tagged := tx.Model(&Channel{}).Where("tag = ?", tag)
		if err := tagged.Session(&gorm.Session{}).Update("status", common.ChannelStatusManuallyDisabled).Error; err != nil {
			return err
		}
		return tx.Model(&Ability{}).Where("channel_id IN (?)", tagged.Session(&gorm.Session{}).Select("id")).Update("enabled", false).Error
	})
	if err != nil {
		return err
	}
	invalidateRoutingMetadata()
	BumpSyncTopic(SyncTopicChannels)
	return nil
}

func EditChannelByTag(tag string, newTag *string, modelMapping *string, models *string, group *string, priority *int64, weight *uint, paramOverride *string, headerOverride *string) error {
	updateData := Channel{}
	shouldReCreateAbilities := false
	updatedTag := tag
	// 如果 newTag 不为空且不等于 tag，则更新 tag
	if newTag != nil && *newTag != tag {
		updateData.Tag = newTag
		updatedTag = *newTag
	}
	if modelMapping != nil {
		updateData.ModelMapping = modelMapping
	}
	if models != nil && *models != "" {
		shouldReCreateAbilities = true
		updateData.Models = *models
	}
	if group != nil && *group != "" {
		shouldReCreateAbilities = true
		updateData.Group = *group
	}
	if priority != nil {
		updateData.Priority = priority
	}
	if weight != nil {
		updateData.Weight = weight
	}
	if paramOverride != nil {
		updateData.ParamOverride = paramOverride
	}
	if headerOverride != nil {
		updateData.HeaderOverride = headerOverride
	}

	err := DB.Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&Channel{}).Where("tag = ?", tag)
		if modelMapping != nil || models != nil || paramOverride != nil || headerOverride != nil {
			var owned int64
			if err := query.Session(&gorm.Session{}).Where("owner_supplier_id <> ?", "").Count(&owned).Error; err != nil {
				return err
			}
			if owned > 0 {
				return ErrSupplierChannelOperation
			}
			// A supplier moved into the tag after the check must remain protected.
			query = query.Where("owner_supplier_id IS NULL OR owner_supplier_id = ?", "")
		}
		if err := query.Updates(updateData).Error; err != nil {
			return err
		}
		if !shouldReCreateAbilities {
			return UpdateAbilityByTag(tx, tag, newTag, priority, weight)
		}
		var channels []*Channel
		if err := tx.Omit("key", "upstream_credential_snapshot_data").Where("tag = ?", updatedTag).Find(&channels).Error; err != nil {
			return err
		}
		for _, channel := range channels {
			if err := channel.UpdateAbilities(tx); err != nil {
				return fmt.Errorf("failed to update abilities of channel #%d: %w", channel.Id, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	invalidateRoutingMetadata()
	BumpSyncTopic(SyncTopicChannels)
	return nil
}

func UpdateChannelUsedQuota(id int, quota int) {
	if common.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeChannelUsedQuota, id, quota)
		return
	}
	updateChannelUsedQuota(id, quota)
}

func updateChannelUsedQuota(id int, quota int) {
	err := DB.Model(&Channel{}).Where("id = ?", id).Update("used_quota", gorm.Expr("used_quota + ?", quota)).Error
	if err != nil {
		common.SysLog(common.LogText("failed to update channel used quota: channel_id=%d, delta_quota=%d, error=%v", id, quota, err))
	}
}

func DeleteChannelByStatus(status int64) (int64, error) {
	var owned int64
	if err := DB.Model(&Channel{}).Where("status = ? AND owner_supplier_id <> ?", status, "").Count(&owned).Error; err != nil {
		return 0, err
	}
	if owned > 0 {
		return 0, ErrSupplierChannelOperation
	}
	deleted, err := deletePlatformChannels(func(query *gorm.DB) *gorm.DB { return query.Where("status = ?", status) })
	return int64(len(deleted)), err
}

func DeleteDisabledChannel() (int64, error) {
	deleted, err := DeleteDisabledPlatformChannels()
	return int64(len(deleted)), err
}

// DeleteDisabledPlatformChannels deletes every disabled channel not owned by
// a supplier and returns the rows it deleted, keys included, so the caller
// revokes exactly their credentials and closes exactly their connections.
func DeleteDisabledPlatformChannels() ([]*Channel, error) {
	return deletePlatformChannels(func(query *gorm.DB) *gorm.DB {
		return query.Where("status IN ?", []int{common.ChannelStatusAutoDisabled, common.ChannelStatusManuallyDisabled})
	})
}

// deletePlatformChannels deletes the channels scope selects that no supplier
// owns, with their abilities and credential bindings, in one transaction: an
// ability row left behind would keep a reused channel ID from routing.
func deletePlatformChannels(scope func(*gorm.DB) *gorm.DB) ([]*Channel, error) {
	defer invalidateRoutingMetadata()
	upstreamCredentialWriteMutex.Lock()
	defer upstreamCredentialWriteMutex.Unlock()
	var deleted []*Channel
	err := DB.Transaction(func(tx *gorm.DB) error {
		if _, err := upstreamFingerprintSecret(tx); err != nil {
			return err
		}
		query := scope(lockForUpdate(tx).Model(&Channel{})).Where("owner_supplier_id IS NULL OR owner_supplier_id = ?", "")
		if err := query.Find(&deleted).Error; err != nil {
			return err
		}
		ids := lo.Map(deleted, func(channel *Channel, _ int) int { return channel.Id })
		for chunk := range slices.Chunk(ids, 200) {
			if err := tx.Model(&ChannelCredentialBinding{}).Where("channel_id IN ?", chunk).Updates(map[string]any{"active": false, "active_slot": nil}).Error; err != nil {
				return err
			}
			if err := tx.Where("id IN ?", chunk).Delete(&Channel{}).Error; err != nil {
				return err
			}
			if err := tx.Where("channel_id IN ?", chunk).Delete(&Ability{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(deleted) > 0 {
		BumpSyncTopic(SyncTopicChannels)
	}
	return deleted, nil
}

func GetPaginatedTags(offset int, limit int) ([]*string, error) {
	return GetPaginatedChannelTags(DB.Model(&Channel{}), offset, limit)
}

func GetPaginatedChannelTags(query *gorm.DB, offset int, limit int) ([]*string, error) {
	var tags []*string
	err := query.
		Select("DISTINCT tag").
		Where("tag is not null AND tag != ''").
		Order(clause.OrderByColumn{Column: clause.Column{Name: "tag"}}).
		Offset(offset).
		Limit(limit).
		Find(&tags).Error
	return tags, err
}

func SearchTags(keyword string, group string, model string, idSort bool, owners ...string) ([]*string, error) {
	var tags []*string

	order := "priority desc"
	if idSort {
		order = "id desc"
	}

	// 构造基础查询
	baseQuery := DB.Model(&Channel{}).Omit("key", "upstream_credential_snapshot_data")

	if len(owners) > 0 {
		baseQuery = ApplyChannelSupplierFilter(baseQuery, owners[0])
	}

	search := DB.Where("id = ?", common.String2Int(keyword)).
		Or("name LIKE ?", "%"+keyword+"%").
		Or(clause.Eq{Column: clause.Column{Name: "key"}, Value: keyword}).
		Or("base_url LIKE ?", "%"+keyword+"%")
	baseQuery = ApplyChannelGroupFilter(baseQuery.Where(search).Where("models LIKE ?", "%"+model+"%"), group)

	subQuery := baseQuery.
		Select("tag").
		Where("tag != ''").
		Order(order)

	err := DB.Table("(?) as sub", subQuery).
		Select("DISTINCT tag").
		Find(&tags).Error

	if err != nil {
		return nil, err
	}

	return tags, nil
}

func (channel *Channel) ValidateSettings() error {
	if err := channel.ValidateAvailability(); err != nil {
		return err
	}
	channelParams := &dto.ChannelSettings{}
	if channel.Setting != nil && *channel.Setting != "" {
		err := common.Unmarshal([]byte(*channel.Setting), channelParams)
		if err != nil {
			return err
		}
	}
	if _, err := common.ParseProxyURLStrict(channelParams.Proxy); err != nil {
		return fmt.Errorf("invalid channel proxy: %w", err)
	}
	if err := channelParams.ValidateHTTPTransport(); err != nil {
		return err
	}
	channelOtherSettings := &dto.ChannelOtherSettings{}
	if channel.OtherSettings != "" {
		err := common.UnmarshalJsonStr(channel.OtherSettings, channelOtherSettings)
		if err != nil {
			return err
		}
	}
	if err := channelOtherSettings.ValidateToolLossPolicy(); err != nil {
		return err
	}
	if preset := common.GetAdvancedCustomPreset(channel.Type); preset != nil {
		channelOtherSettings.AdvancedCustom = preset
	}
	if constant.IsAdvancedCustomChannel(channel.Type) {
		if channelOtherSettings.AdvancedCustom == nil {
			return fmt.Errorf("advanced_custom is required")
		}
	}
	if channelOtherSettings.AdvancedCustom != nil {
		if err := channelOtherSettings.AdvancedCustom.Validate(); err != nil {
			return err
		}
	}
	if constant.IsAdvancedCustomChannel(channel.Type) && channelOtherSettings.UpstreamModelUpdateCheckEnabled {
		if _, ok := channelOtherSettings.AdvancedCustom.ModelListRoute(); !ok {
			return fmt.Errorf("advanced custom channels require a %s route when upstream model update checks are enabled", dto.AdvancedCustomModelListPath)
		}
	}
	return nil
}

func (channel *Channel) GetSetting() dto.ChannelSettings {
	setting := dto.ChannelSettings{}
	if channel.Setting != nil && *channel.Setting != "" {
		err := common.Unmarshal([]byte(*channel.Setting), &setting)
		if err != nil {
			common.SysLog(common.LogText("failed to unmarshal setting: channel_id=%d, error=%v", channel.Id, err))
			channel.Setting = nil // 清空设置以避免后续错误
			// Only the cleared column is written: the rest of this copy, its key
			// included, may be older than the stored channel.
			_ = DB.Model(&Channel{}).Where("id = ?", channel.Id).Update("setting", nil).Error
		}
	}
	return setting
}

func (channel *Channel) SetSetting(setting dto.ChannelSettings) {
	settingBytes, err := common.Marshal(setting)
	if err != nil {
		common.SysLog(common.LogText("failed to marshal setting: channel_id=%d, error=%v", channel.Id, err))
		return
	}
	channel.Setting = common.GetPointer[string](string(settingBytes))
}

func (channel *Channel) GetOtherSettings() dto.ChannelOtherSettings {
	setting := dto.ChannelOtherSettings{}
	if channel.OtherSettings != "" {
		err := common.UnmarshalJsonStr(channel.OtherSettings, &setting)
		if err != nil {
			common.SysLog(common.LogText("failed to unmarshal setting: channel_id=%d, error=%v", channel.Id, err))
			channel.OtherSettings = "{}" // 清空设置以避免后续错误
			// Only the cleared column is written, as in GetSetting.
			_ = DB.Model(&Channel{}).Where("id = ?", channel.Id).Update("settings", channel.OtherSettings).Error
		}
	}
	if preset := common.GetAdvancedCustomPreset(channel.Type); preset != nil {
		setting.AdvancedCustom = preset
	}
	return setting
}

func (channel *Channel) SetOtherSettings(setting dto.ChannelOtherSettings) {
	settingBytes, err := common.Marshal(setting)
	if err != nil {
		common.SysLog(common.LogText("failed to marshal setting: channel_id=%d, error=%v", channel.Id, err))
		return
	}
	channel.OtherSettings = string(settingBytes)
}

func (channel *Channel) GetParamOverride() map[string]any {
	paramOverride := make(map[string]any)
	if channel.ParamOverride != nil && *channel.ParamOverride != "" {
		err := common.Unmarshal([]byte(*channel.ParamOverride), &paramOverride)
		if err != nil {
			common.SysLog(common.LogText("failed to unmarshal param override: channel_id=%d, error=%v", channel.Id, err))
		}
	}
	return paramOverride
}

func (channel *Channel) GetHeaderOverride() map[string]any {
	headerOverride := make(map[string]any)
	if channel.HeaderOverride != nil && *channel.HeaderOverride != "" {
		err := common.Unmarshal([]byte(*channel.HeaderOverride), &headerOverride)
		if err != nil {
			common.SysLog(common.LogText("failed to unmarshal header override: channel_id=%d, error=%v", channel.Id, err))
		}
	}
	return headerOverride
}

func GetChannelsByIds(ids []int) ([]*Channel, error) {
	var channels []*Channel
	err := DB.Where("id in (?)", ids).Find(&channels).Error
	return channels, err
}

// GetChannelNamesByIds fetches only the display fields needed by aggregate
// dashboards. It avoids loading channel credentials and routing configuration
// when a report only needs channel labels.
func GetChannelNamesByIds(ids []int) (map[int]string, error) {
	if len(ids) == 0 {
		return map[int]string{}, nil
	}
	type channelName struct {
		Id   int
		Name string
	}
	var rows []channelName
	if err := DB.Model(&Channel{}).
		Select("id, name").
		Where("id IN ?", ids).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	names := make(map[int]string, len(rows))
	for _, row := range rows {
		names[row.Id] = row.Name
	}
	return names, nil
}

func BatchSetChannelTag(ids []int, tag *string) error {
	// 开启事务
	tx := DB.Begin()
	if tx.Error != nil {
		return tx.Error
	}

	// 更新标签
	err := tx.Model(&Channel{}).Where("id in (?)", ids).Update("tag", tag).Error
	if err != nil {
		tx.Rollback()
		return err
	}

	// Read the new tag through this transaction before rebuilding abilities.
	var channels []*Channel
	err = tx.Where("id IN ?", ids).Find(&channels).Error
	if err != nil {
		tx.Rollback()
		return err
	}

	for _, channel := range channels {
		err = channel.UpdateAbilities(tx)
		if err != nil {
			tx.Rollback()
			return err
		}
	}

	// 提交事务
	if err = tx.Commit().Error; err != nil {
		return err
	}
	BumpSyncTopic(SyncTopicChannels)
	return nil
}

// CountAllChannels returns total channels in DB
func CountAllChannels() (int64, error) {
	var total int64
	err := DB.Model(&Channel{}).Count(&total).Error
	return total, err
}

// CountAllTags returns number of non-empty distinct tags
func CountAllTags() (int64, error) {
	return CountChannelTags(DB.Model(&Channel{}))
}

func CountChannelTags(query *gorm.DB) (int64, error) {
	var total int64
	err := query.Where("tag is not null AND tag != ''").Distinct("tag").Count(&total).Error
	return total, err
}

// Get channels of specified type with pagination
func GetChannelsByType(startIdx int, num int, idSort bool, channelType int) ([]*Channel, error) {
	var channels []*Channel
	order := "priority desc"
	if idSort {
		order = "id desc"
	}
	err := DB.Where("type = ?", channelType).Order(order).Limit(num).Offset(startIdx).Omit("key", "upstream_credential_snapshot_data").Find(&channels).Error
	return channels, err
}

// Count channels of specific type
func CountChannelsByType(channelType int) (int64, error) {
	var count int64
	err := DB.Model(&Channel{}).Where("type = ?", channelType).Count(&count).Error
	return count, err
}

// Return map[type]count for all channels
func CountChannelsGroupByType() (map[int64]int64, error) {
	type result struct {
		Type  int64 `gorm:"column:type"`
		Count int64 `gorm:"column:count"`
	}
	var results []result
	err := DB.Model(&Channel{}).Select("type, count(*) as count").Group("type").Find(&results).Error
	if err != nil {
		return nil, err
	}
	counts := make(map[int64]int64)
	for _, r := range results {
		counts[r.Type] = r.Count
	}
	return counts, nil
}
