package controller

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type supplierChannelResponse struct {
	ID                 int      `json:"id"`
	Name               string   `json:"name"`
	Type               int      `json:"type"`
	TemplateID         string   `json:"template_id"`
	Protocols          []string `json:"protocols"`
	TPMApplicable      bool     `json:"tpm_applicable"`
	BaseURL            string   `json:"base_url"`
	Models             []string `json:"models"`
	Remark             string   `json:"remark"`
	Priority           int64    `json:"priority"`
	Version            int64    `json:"version"`
	Paused             bool     `json:"paused"`
	Status             string   `json:"status"`
	BlockingReasons    []string `json:"blocking_reasons"`
	ValidationStatus   string   `json:"validation_status"`
	HasKey             bool     `json:"has_key"`
	HasValidatedConfig bool     `json:"has_validated_config"`
	SupplierID         string   `json:"supplier_id"`
	SupplierName       string   `json:"supplier_name"`
	PlatformPriority   int64    `json:"platform_priority,omitempty"`
	Weight             int      `json:"weight,omitempty"`
	Group              string   `json:"group,omitempty"`
}

type supplierAccountRequest struct {
	Name             string   `json:"name"`
	Revision         int64    `json:"revision"`
	UserID           int      `json:"user_id"`
	SupplyEnabled    bool     `json:"supply_enabled"`
	AllowedTemplates []string `json:"allowed_templates"`
	AllowedTypes     []int    `json:"allowed_types"`
	AllowedModels    []string `json:"allowed_models"`
	Group            string   `json:"group"`
	MaxChannels      int      `json:"max_channels"`
	MaxInputTokens   int64    `json:"max_input_tokens"`
	MaxOutputTokens  int64    `json:"max_output_tokens"`
	CostRatio        *float64 `json:"cost_ratio"`
	PoolPriority     int64    `json:"pool_priority"`
	PoolWeight       int      `json:"pool_weight"`
	PoolCapacity     int      `json:"pool_capacity"`
}

type supplierLimitRequest struct {
	Revision                 int64   `json:"revision"`
	RPM                      *int64  `json:"rpm"`
	TPM                      *int64  `json:"tpm"`
	DailyUSD                 *string `json:"daily_usd"`
	ReconcileSeconds         *int64  `json:"reconcile_seconds"`
	rpmSet, tpmSet, dailySet bool
}

func (request *supplierLimitRequest) UnmarshalJSON(data []byte) error {
	type plain supplierLimitRequest
	var value plain
	if err := common.Unmarshal(data, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(data, &fields); err != nil {
		return err
	}
	for key := range fields {
		if !slices.Contains([]string{"revision", "rpm", "tpm", "daily_usd", "reconcile_seconds"}, key) {
			return fmt.Errorf("invalid limit field")
		}
	}
	*request = supplierLimitRequest(value)
	_, request.rpmSet = fields["rpm"]
	_, request.tpmSet = fields["tpm"]
	_, request.dailySet = fields["daily_usd"]
	return nil
}

type supplierChannelRequest struct {
	Name       string                `json:"name"`
	TemplateID string                `json:"template_id"`
	BaseURL    string                `json:"base_url"`
	Key        string                `json:"key"`
	Models     []string              `json:"models"`
	Remark     string                `json:"remark"`
	Priority   int64                 `json:"priority"`
	Version    int64                 `json:"version"`
	Limits     *supplierLimitRequest `json:"limits"`
}

// These endpoints deliberately reject unknown fields instead of binding an
// administrator's Channel model and trying to remove sensitive fields later.
func decodeSupplierRequest(c *gin.Context, value any, allowed ...string) bool {
	data, err := io.ReadAll(io.LimitReader(c.Request.Body, 256*1024+1))
	if err != nil || len(data) > 256*1024 {
		common.ApiErrorT(c, "Invalid parameters")
		return false
	}
	var fields map[string]any
	if common.Unmarshal(data, &fields) != nil || fields == nil {
		common.ApiErrorT(c, "Invalid parameters")
		return false
	}
	for key := range fields {
		if !slices.Contains(allowed, key) {
			common.ApiErrorT(c, "Invalid parameters")
			return false
		}
	}
	if common.Unmarshal(data, value) != nil {
		common.ApiErrorT(c, "Invalid parameters")
		return false
	}
	return true
}

func supplierChannelScope(c *gin.Context) *gorm.DB {
	query := model.DB.Model(&model.Channel{})
	if c.GetInt("role") == common.RoleSupplierUser {
		return query.Where("owner_supplier_id = ?", c.GetString("supplier_id"))
	}
	return query
}

func supplierChannelForRequest(c *gin.Context, includeKey bool) (*model.Channel, bool) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		common.ApiErrorT(c, "Channel does not exist")
		return nil, false
	}
	query := supplierChannelScope(c)
	if !includeKey {
		query = query.Omit("key", "upstream_credential_snapshot_data")
	}
	var channel model.Channel
	if err := query.First(&channel, "id = ?", id).Error; err != nil {
		common.ApiErrorT(c, "Channel does not exist")
		return nil, false
	}
	return &channel, true
}

func supplierChannelDTO(channel *model.Channel, draft *model.SupplierChannelDraft, supplierName string) supplierChannelResponse {
	baseURL := channel.GetBaseURL()
	if parsed, err := url.Parse(baseURL); err == nil {
		parsed.User, parsed.RawQuery, parsed.Fragment = nil, "", ""
		baseURL = parsed.String()
	} else {
		baseURL = ""
	}
	result := supplierChannelResponse{ID: channel.Id, Name: channel.Name, Type: channel.Type, TemplateID: model.SupplierChannelTemplateID(channel), BaseURL: baseURL, Models: channel.GetModels(), Priority: channel.SupplierPriority, Version: channel.SupplierVersion, Paused: channel.SupplierPaused, Status: "available", ValidationStatus: "validated", HasKey: channel.SupplierValidated, SupplierID: channel.OwnerSupplierID, SupplierName: supplierName, PlatformPriority: channel.GetPriority(), Weight: channel.GetWeight(), Group: channel.Group}
	result.Protocols, result.TPMApplicable = service.SupplierChannelProtocols(channel)
	result.BlockingReasons = []string{}
	result.HasValidatedConfig = channel.SupplierValidated || channel.OwnerSupplierID == ""
	if channel.Remark != nil {
		result.Remark = *channel.Remark
	}
	if !channel.SupplierValidated && channel.OwnerSupplierID != "" {
		result.Status, result.ValidationStatus = "pending", "pending"
		result.BlockingReasons = append(result.BlockingReasons, "pending")
	}
	if channel.Status != common.ChannelStatusEnabled {
		result.Status = "paused"
		reason := "manual_paused"
		if channel.Status == common.ChannelStatusAutoDisabled {
			reason = "health_disabled"
		}
		result.BlockingReasons = append(result.BlockingReasons, reason)
	}
	if channel.SupplierPaused {
		result.Status = "paused"
		result.BlockingReasons = append(result.BlockingReasons, "paused")
	}
	if draft != nil {
		result.ValidationStatus = draft.Status
		result.HasKey = result.HasKey || draft.Secret != ""
		if !channel.SupplierValidated || draft.Status != "validated" {
			result.BaseURL, result.Models, result.Type, result.TemplateID = draft.BaseURL, strings.Split(draft.Models, ","), draft.Type, draft.Template()
			if template, ok := service.SupplierTemplateByID(draft.Template()); ok {
				result.Protocols, result.TPMApplicable = template.Protocols, template.TPMApplicable
			}
		}
	}
	if channel.OwnerSupplierID != "" {
		account, err := model.GetSupplierAccount(channel.OwnerSupplierID)
		if err != nil {
			result.Status = "authorization_removed"
			result.BlockingReasons = append(result.BlockingReasons, "authorization_removed")
		} else {
			result.PlatformPriority, result.Weight = account.PoolPriority, account.PoolWeight
			if !account.SupplyEnabled {
				result.Status = "supply_paused"
				result.BlockingReasons = append(result.BlockingReasons, "supply_paused")
			}
			if _, available := service.SupplierTemplateByID(result.TemplateID); !available {
				result.Status = "template_unavailable"
				result.BlockingReasons = append(result.BlockingReasons, "template_unavailable")
			}
			if !account.AllowsTemplate(result.TemplateID, result.Models) {
				result.Status = "authorization_removed"
				result.BlockingReasons = append(result.BlockingReasons, "authorization_removed")
			}
		}
	}
	if state, err := model.GetChannelLimitState(channel.Id, time.Now()); err != nil {
		result.Status = "accounting_unavailable"
		result.BlockingReasons = append(result.BlockingReasons, "accounting_unavailable")
	} else if state.Reason != "" {
		result.Status = state.Reason
		result.BlockingReasons = append(result.BlockingReasons, state.Reason)
	}
	if !channel.ScheduleAllowsAt(time.Now()) {
		result.Status = "outside_schedule"
		result.BlockingReasons = append(result.BlockingReasons, "outside_schedule")
	}
	return result
}

func GetSupplierTemplates(c *gin.Context) {
	common.ApiSuccess(c, gin.H{"items": service.SupplierTemplates()})
}

func GetSupplierSelf(c *gin.Context) {
	id := c.GetString("supplier_id")
	account, err := model.GetSupplierAccount(id)
	if err != nil {
		common.ApiErrorT(c, "Unauthorized, insufficient privileges")
		return
	}
	var supplier model.UpstreamSupplier
	if model.DB.First(&supplier, "id = ?", id).Error != nil {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}
	templates := []service.SupplierTemplate{}
	for _, template := range service.SupplierTemplates() {
		if !slices.Contains(account.TemplateIDs(), template.ID) {
			continue
		}
		models := []string{}
		for _, name := range account.Models() {
			if len(template.Models) == 0 || slices.Contains(template.Models, name) {
				models = append(models, name)
			}
		}
		if len(models) == 0 {
			continue
		}
		template.Models = models
		templates = append(templates, template)
	}
	common.ApiSuccess(c, gin.H{"supplier": gin.H{"id": id, "name": supplier.Name, "supply_enabled": account.SupplyEnabled}, "templates": templates})
}

func ListSupplierChannels(c *gin.Context) {
	page := common.GetPageQuery(c)
	query := supplierChannelScope(c)
	if c.GetInt("role") != common.RoleSupplierUser {
		query = model.ApplyChannelSupplierFilter(query, c.Query("owner_supplier_id"))
	}
	if keyword := strings.TrimSpace(c.Query("keyword")); keyword != "" {
		query = query.Where("name LIKE ?", "%"+keyword+"%")
	}
	var total int64
	if query.Count(&total).Error != nil {
		common.ApiErrorT(c, "Please try again later.")
		return
	}
	var channels []model.Channel
	if query.Omit("key", "upstream_credential_snapshot_data").Order("id DESC").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&channels).Error != nil {
		common.ApiErrorT(c, "Please try again later.")
		return
	}
	items := make([]supplierChannelResponse, 0, len(channels))
	ids := make([]int, 0, len(channels))
	for _, channel := range channels {
		ids = append(ids, channel.Id)
	}
	configured := []int{}
	if len(ids) > 0 {
		if model.DB.Model(&model.Channel{}).Where("id IN ?", ids).Where(clause.Neq{Column: "key", Value: ""}).Pluck("id", &configured).Error != nil {
			common.ApiErrorT(c, "Please try again later.")
			return
		}
	}
	for i := range channels {
		channel := &channels[i]
		var draft model.SupplierChannelDraft
		var draftPtr *model.SupplierChannelDraft
		if model.DB.First(&draft, "channel_id = ? AND supplier_id = ?", channel.Id, channel.OwnerSupplierID).Error == nil {
			draftPtr = &draft
		}
		var supplier model.UpstreamSupplier
		if channel.OwnerSupplierID != "" {
			_ = model.DB.First(&supplier, "id = ?", channel.OwnerSupplierID).Error
		}
		item := supplierChannelDTO(channel, draftPtr, supplier.Name)
		item.HasKey = item.HasKey || slices.Contains(configured, channel.Id)
		if c.GetInt("role") == common.RoleCommonUser {
			if parsed, err := url.Parse(item.BaseURL); err == nil {
				parsed.Path, parsed.RawPath = "", ""
				item.BaseURL = parsed.String()
			}
		}
		if c.GetInt("role") == common.RoleSupplierUser {
			item.PlatformPriority, item.Weight, item.Group = 0, 0, ""
		}
		items = append(items, item)
	}
	common.ApiSuccess(c, gin.H{"items": items, "total": total})
}

func GetSupplierChannel(c *gin.Context) {
	channel, ok := supplierChannelForRequest(c, false)
	if !ok {
		return
	}
	var draft model.SupplierChannelDraft
	var draftPtr *model.SupplierChannelDraft
	if model.DB.First(&draft, "channel_id = ? AND supplier_id = ?", channel.Id, channel.OwnerSupplierID).Error == nil {
		draftPtr = &draft
	}
	var supplier model.UpstreamSupplier
	if channel.OwnerSupplierID != "" {
		_ = model.DB.First(&supplier, "id = ?", channel.OwnerSupplierID).Error
	}
	item := supplierChannelDTO(channel, draftPtr, supplier.Name)
	var configured int64
	if model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).Where(clause.Neq{Column: "key", Value: ""}).Count(&configured).Error != nil {
		common.ApiErrorT(c, "Please try again later.")
		return
	}
	item.HasKey = item.HasKey || configured > 0
	if c.GetInt("role") == common.RoleCommonUser {
		if parsed, err := url.Parse(item.BaseURL); err == nil {
			parsed.Path, parsed.RawPath = "", ""
			item.BaseURL = parsed.String()
		}
	}
	if c.GetInt("role") == common.RoleSupplierUser {
		item.PlatformPriority, item.Weight, item.Group = 0, 0, ""
	}
	common.ApiSuccess(c, item)
}

func SaveSupplierChannel(c *gin.Context) {
	if !common.SessionCookieSecure || !common.RequestUsesTrustedHTTPS(c.Request) {
		common.ApiErrorT(c, "Supplier credentials require HTTPS and secure session cookies")
		return
	}
	var request supplierChannelRequest
	if !decodeSupplierRequest(c, &request, "name", "template_id", "base_url", "key", "models", "remark", "priority", "version", "limits") {
		return
	}
	owner := c.GetString("supplier_id")
	if c.GetInt("role") >= common.RoleAdminUser {
		owner = c.Param("supplier_id")
	}
	var origin *model.Channel
	if c.Param("id") != "" {
		var ok bool
		origin, ok = supplierChannelForRequest(c, true)
		if !ok {
			return
		}
		owner = origin.OwnerSupplierID
	}
	account, err := model.GetSupplierAccount(owner)
	if err != nil {
		common.ApiErrorT(c, "Unauthorized, insufficient privileges")
		return
	}
	template, knownTemplate := service.SupplierTemplateByID(request.TemplateID)
	kind := template.Type
	if !knownTemplate || !account.AllowsTemplate(request.TemplateID, request.Models) || strings.TrimSpace(request.Name) == "" || utf8.RuneCountInString(request.Name) > 100 || utf8.RuneCountInString(request.Remark) > 255 || len(request.Models) > 100 || request.Priority < -1000 || request.Priority > 1000 {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}
	for _, name := range request.Models {
		if len(template.Models) > 0 && !slices.Contains(template.Models, name) {
			common.ApiErrorT(c, "Invalid parameters")
			return
		}
	}
	if err := service.ValidateSupplierURL(request.BaseURL); err != nil {
		common.ApiError(c, err)
		return
	}
	channelID := 0
	sensitive := true
	var pendingDraft *model.SupplierChannelDraft
	if origin != nil {
		channelID = origin.Id
		if request.Version != origin.SupplierVersion {
			common.ApiErrorT(c, "Conflict: the resource was updated. Refresh and try again.")
			return
		}
		previousURL, previousTemplate, previousKey := origin.GetBaseURL(), model.SupplierChannelTemplateID(origin), origin.Key
		var saved model.SupplierChannelDraft
		savedErr := model.DB.First(&saved, "channel_id = ? AND supplier_id = ? AND version = ?", origin.Id, owner, origin.SupplierVersion).Error
		if savedErr != nil && (!origin.SupplierValidated || !errors.Is(savedErr, gorm.ErrRecordNotFound)) {
			common.ApiErrorT(c, "Conflict: the resource was updated. Refresh and try again.")
			return
		}
		if savedErr == nil && (!origin.SupplierValidated || saved.Status != "validated") {
			pendingDraft = &saved
			previousURL, previousTemplate = saved.BaseURL, saved.Template()
			if request.Key == "" {
				previousKey, err = model.DecryptSupplierSecret(owner, saved.Secret)
				if err != nil {
					common.ApiError(c, err)
					return
				}
			}
		}
		sensitive = request.Key != "" || request.BaseURL != previousURL || request.TemplateID != previousTemplate
		if (request.BaseURL != previousURL || request.TemplateID != previousTemplate) && request.Key == "" {
			common.ApiErrorT(c, "A new key is required when changing the upstream URL")
			return
		}
		if request.Key == "" {
			request.Key = previousKey
		}
	}
	if request.Key == "" || len(request.Key) > 128*1024 || len(strings.Split(strings.TrimSpace(request.Key), "\n")) > 20 {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}
	if sensitive {
		context, _ := common.Marshal(service.SupplierChannelCredentialsContext{ChannelID: channelID, Version: request.Version})
		if middleware.RequireSecurityProof(c, service.VerificationOperation{Scope: service.VerificationScopeSupplierChannelCredentials, Context: context}) == nil {
			return
		}
	}
	secret, err := model.EncryptSupplierSecret(owner, request.Key)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	draft := model.SupplierChannelDraft{TemplateID: request.TemplateID, ChannelID: channelID, SupplierID: owner, Type: kind, Name: strings.TrimSpace(request.Name), BaseURL: request.BaseURL, Secret: secret, Models: strings.Join(request.Models, ","), Remark: request.Remark}
	metadataOnly := pendingDraft == nil && origin != nil && origin.SupplierValidated && !sensitive && request.BaseURL == origin.GetBaseURL() && request.TemplateID == model.SupplierChannelTemplateID(origin) && request.Key == origin.Key && slices.Equal(request.Models, origin.GetModels())
	if pendingDraft != nil && !sensitive && request.BaseURL == pendingDraft.BaseURL && request.TemplateID == pendingDraft.Template() && draft.Models == pendingDraft.Models {
		// Cosmetic edits keep the exact same submitted connection and probes.
		// The new version still fences publication, without another paid task.
		draft.ProbeTaskIDs = pendingDraft.ProbeTaskIDs
	}
	reusedProbes := pendingDraft != nil && draft.ProbeTaskIDs != ""
	var channel *model.Channel
	err = model.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		channel, err = model.SaveSupplierChannelDraft(&draft, c.GetInt("id"), request.Version, request.Priority, tx)
		if err != nil {
			return err
		}
		if err := model.EnsureChannelLimitTracking(channel.Id, origin == nil, time.Now(), tx); err != nil {
			return err
		}
		if request.Limits != nil {
			if origin == nil {
				request.Limits.Revision = 1
			}
			if _, err := applySupplierLimits(c, channel, *request.Limits, tx); err != nil {
				return err
			}
		}
		status := "pending"
		if metadataOnly {
			status = "validated"
		} else if reusedProbes {
			status = pendingDraft.Status
			if status == "testing" {
				status = "waiting"
			}
		}
		return tx.Model(&model.SupplierChannelDraft{}).Where("channel_id = ? AND version = ? AND status = ?", channel.Id, draft.Version, "configuring").Update("status", status).Error
	})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "supplier.channel.save", map[string]any{"id": channel.Id, "supplier_id": owner, "version": channel.SupplierVersion})
	// Metadata-only edits preserve the validated connection and do not depend
	// on a test that could be blocked by a daily budget or a manual pause.
	if metadataOnly {
		model.InitChannelCache()
		GetSupplierChannel(c)
		return
	}
	if reusedProbes {
		GetSupplierChannel(c)
		return
	}
	common.ApiSuccess(c, gin.H{"id": channel.Id, "version": channel.SupplierVersion, "validation_status": "pending"})
	go validateSupplierChannelDraft(channel.Id, owner, draft.Version, c.GetInt("id"))
}

func SetSupplierChannelStatus(c *gin.Context) {
	channel, ok := supplierChannelForRequest(c, false)
	if !ok {
		return
	}
	var request struct {
		Paused  bool  `json:"paused"`
		Version int64 `json:"version"`
	}
	if !decodeSupplierRequest(c, &request, "paused", "version") {
		return
	}
	result := model.DB.Model(&model.Channel{}).Where("id = ? AND owner_supplier_id = ? AND supplier_version = ?", channel.Id, channel.OwnerSupplierID, request.Version).Updates(map[string]any{"supplier_paused": request.Paused})
	if result.Error != nil || result.RowsAffected != 1 {
		common.ApiErrorT(c, "Conflict: the resource was updated. Refresh and try again.")
		return
	}
	model.InitChannelCache()
	recordManageAudit(c, "supplier.channel.pause", map[string]any{"id": channel.Id, "paused": request.Paused})
	common.ApiSuccess(c, gin.H{"version": request.Version, "paused": request.Paused})
}

func CopySupplierChannel(c *gin.Context) {
	channel, ok := supplierChannelForRequest(c, false)
	if !ok {
		return
	}
	var request struct {
		Version int64  `json:"version"`
		Name    string `json:"name"`
	}
	if !decodeSupplierRequest(c, &request, "version", "name") {
		return
	}
	if strings.TrimSpace(request.Name) == "" || utf8.RuneCountInString(request.Name) > 100 {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}
	context, _ := common.Marshal(service.SupplierChannelCredentialsContext{ChannelID: channel.Id, Version: request.Version})
	if middleware.RequireSecurityProof(c, service.VerificationOperation{Scope: service.VerificationScopeSupplierChannelCredentials, Context: context}) == nil {
		return
	}
	id, err := model.CopySupplierChannel(channel.Id, channel.OwnerSupplierID, request.Version, c.GetInt("id"), strings.TrimSpace(request.Name))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	model.InitChannelCache()
	recordManageAudit(c, "supplier.channel.copy", map[string]any{"id": id, "source_id": channel.Id, "supplier_id": channel.OwnerSupplierID})
	common.ApiSuccess(c, gin.H{"id": id})
}

func DiscardSupplierChannel(c *gin.Context) {
	channel, ok := supplierChannelForRequest(c, false)
	if !ok {
		return
	}
	var request struct {
		Version int64 `json:"version"`
	}
	if !decodeSupplierRequest(c, &request, "version") {
		return
	}
	if err := model.DiscardSupplierChannelDraft(channel.Id, channel.OwnerSupplierID, request.Version); err != nil {
		common.ApiError(c, err)
		return
	}
	model.InitChannelCache()
	recordManageAudit(c, "supplier.channel.discard", map[string]any{"id": channel.Id, "supplier_id": channel.OwnerSupplierID})
	common.ApiSuccess(c, nil)
}

func TestSupplierChannel(c *gin.Context) {
	channel, ok := supplierChannelForRequest(c, false)
	if !ok {
		return
	}
	var request struct {
		Version int64 `json:"version"`
	}
	if !decodeSupplierRequest(c, &request, "version") {
		return
	}
	status, err := model.QueueSupplierChannelValidation(channel.Id, channel.OwnerSupplierID, request.Version)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if status == "pending" {
		go validateSupplierChannelDraft(channel.Id, channel.OwnerSupplierID, request.Version, c.GetInt("id"))
	}
	common.ApiSuccess(c, gin.H{"validation_status": status})
}

func GetSupplierChannelLimits(c *gin.Context) {
	channel, ok := supplierChannelForRequest(c, false)
	if !ok {
		return
	}
	state, err := model.GetChannelLimitState(channel.Id, time.Now())
	if err != nil {
		common.ApiError(c, err)
		return
	}
	var rpm, tpm *int64
	var daily *string
	if state.Config.RPM > 0 {
		rpm = &state.Config.RPM
	}
	if state.Config.TPM > 0 {
		tpm = &state.Config.TPM
	}
	if state.Config.DailyUSDNano > 0 {
		value := model.FormatChannelLimitUSD(state.Config.DailyUSDNano)
		daily = &value
	}
	var used, reserved, remaining any
	location, _ := time.LoadLocation("Asia/Shanghai")
	dayStart, _ := time.ParseInLocation("2006-01-02", state.BudgetDay, location)
	complete := state.Config.HistoryComplete || state.Config.ObservedSince > 0 && state.Config.ObservedSince <= dayStart.UnixMilli()
	if state.UnknownCosts == 0 && complete {
		used = model.FormatChannelLimitUSD(state.DailyUsedNano)
		reserved = model.FormatChannelLimitUSD(state.DailyHeldNano)
		if state.Config.DailyUSDNano > 0 {
			remaining = model.FormatChannelLimitUSD(state.DailyRemaining)
		}
	}
	protocols, tpmApplicable := service.SupplierChannelProtocols(channel)
	common.ApiSuccess(c, gin.H{"config": gin.H{"protocols": protocols, "tpm_applicable": tpmApplicable, "channel_id": channel.Id, "revision": state.Config.Revision, "rpm": rpm, "tpm": tpm, "daily_usd": daily, "reconcile_seconds": state.Config.ReconcileSeconds, "budget_ready": state.Config.BudgetReady}, "usage": gin.H{"rpm_used": state.RPMUsed, "tpm_used": state.TPMUsed, "tpm_reserved": state.TPMReserved, "daily_used_usd": used, "daily_reserved_usd": reserved, "daily_remaining_usd": remaining, "budget_day": state.BudgetDay, "reset_at": state.ResetAt, "reason": state.Reason}})
}

func applySupplierLimits(c *gin.Context, channel *model.Channel, request supplierLimitRequest, transactions ...*gorm.DB) (model.ChannelLimitSettings, error) {
	db := model.DB
	if len(transactions) > 0 {
		db = transactions[0]
	}
	state, err := model.GetChannelLimitState(channel.Id, time.Now(), db)
	if err != nil {
		return model.ChannelLimitSettings{}, err
	}
	settings := state.Config.ChannelLimitSettings
	if request.rpmSet {
		settings.RPM = 0
	}
	if request.tpmSet {
		settings.TPM = 0
	}
	if request.dailySet {
		settings.DailyUSDNano = 0
	}
	if request.RPM != nil {
		if *request.RPM <= 0 {
			return model.ChannelLimitSettings{}, errors.New("Invalid parameters")
		}
		settings.RPM = *request.RPM
	}
	if request.TPM != nil {
		if *request.TPM <= 0 {
			return model.ChannelLimitSettings{}, errors.New("Invalid parameters")
		}
		settings.TPM = *request.TPM
	}
	if request.DailyUSD != nil && strings.TrimSpace(*request.DailyUSD) != "" {
		amount, err := model.ParseChannelLimitUSD(*request.DailyUSD)
		if err != nil || amount <= 0 {
			return model.ChannelLimitSettings{}, errors.New("Invalid parameters")
		}
		settings.DailyUSDNano = amount
	}
	if request.ReconcileSeconds != nil {
		if c.GetInt("role") < common.RoleAdminUser {
			return model.ChannelLimitSettings{}, errors.New("Unauthorized, insufficient privileges")
		}
		settings.ReconcileSeconds = *request.ReconcileSeconds
	}
	if err := service.ValidateChannelLimitSupport(channel, settings); err != nil {
		return model.ChannelLimitSettings{}, errors.New("Channel limits are not supported for this upstream protocol yet")
	}
	var pricing model.Channel
	if err := db.Select("id", "cost_ratio", "cost_version_id", "upstream_credential_snapshot_data").First(&pricing, "id = ?", channel.Id).Error; err != nil {
		return model.ChannelLimitSettings{}, err
	}
	if _, err := model.UpdateChannelLimits(channel.Id, request.Revision, settings, pricing.ProcurementCostReady(), time.Now(), db); err != nil {
		return model.ChannelLimitSettings{}, err
	}
	return settings, nil
}

func saveSupplierLimits(c *gin.Context, channel *model.Channel, request supplierLimitRequest) bool {
	settings, err := applySupplierLimits(c, channel, request)
	if err != nil {
		common.ApiError(c, err)
		return false
	}
	recordManageAudit(c, "channel.limits.update", map[string]any{"id": channel.Id, "rpm": settings.RPM, "tpm": settings.TPM, "daily_usd": request.DailyUSD})
	return true
}

func UpdateSupplierChannelLimits(c *gin.Context) {
	channel, ok := supplierChannelForRequest(c, false)
	if !ok {
		return
	}
	var request supplierLimitRequest
	if !decodeSupplierRequest(c, &request, "revision", "rpm", "tpm", "daily_usd", "reconcile_seconds") {
		return
	}
	if !saveSupplierLimits(c, channel, request) {
		return
	}
	GetSupplierChannelLimits(c)
}

func GetSupplierBudgetEvents(c *gin.Context) {
	channel, ok := supplierChannelForRequest(c, false)
	if !ok {
		return
	}
	query := model.DB.Model(&model.ChannelLimitAttempt{}).Where("channel_id = ? AND sent_at_ms > 0", channel.Id)
	if day := c.Query("day"); day != "" {
		if _, err := time.Parse("2006-01-02", day); err != nil {
			common.ApiErrorT(c, "Invalid parameters")
			return
		}
		query = query.Where("budget_day = ?", day)
	}
	var total int64
	if query.Count(&total).Error != nil {
		common.ApiErrorT(c, "Please try again later.")
		return
	}
	page := common.GetPageQuery(c)
	var attempts []model.ChannelLimitAttempt
	if query.Order("sent_at_ms DESC, id DESC").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&attempts).Error != nil {
		common.ApiErrorT(c, "Please try again later.")
		return
	}
	rows := make([]gin.H, 0, len(attempts))
	for _, attempt := range attempts {
		var cost, reserved *string
		if attempt.CostActualKnown {
			value := model.FormatChannelLimitUSD(attempt.ActualUSDNano)
			cost = &value
		}
		if attempt.CostBudgetKnown {
			value := model.FormatChannelLimitUSD(attempt.HeldUSDNano)
			reserved = &value
		}
		status := "pending"
		if attempt.Completed {
			status = "settled"
		}
		if attempt.ReconcileStartedMS > 0 {
			status = "reconciling"
		}
		if attempt.TPMEstimated {
			status = "estimated"
		}
		rows = append(rows, gin.H{"id": attempt.ID, "channel_id": attempt.ChannelID, "budget_day": attempt.BudgetDay, "sent_at": attempt.SentAtMS / 1000, "status": status, "cost_usd": cost, "reserved_usd": reserved, "tpm_estimated": attempt.TPMEstimated, "reconcile_until": attempt.ReconcileUntilMS / 1000})
	}
	common.ApiSuccess(c, gin.H{"items": rows, "total": total})
}

func GetSupplierLimitEvents(c *gin.Context) {
	channel, ok := supplierChannelForRequest(c, false)
	if !ok {
		return
	}
	query := model.DB.Model(&model.ChannelLimitEvent{}).Where("channel_id = ?", channel.Id)
	var total int64
	if query.Count(&total).Error != nil {
		common.ApiErrorT(c, "Please try again later.")
		return
	}
	page := common.GetPageQuery(c)
	var events []model.ChannelLimitEvent
	if query.Order("occurred_at_ms DESC, id DESC").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&events).Error != nil {
		common.ApiErrorT(c, "Please try again later.")
		return
	}
	items := make([]gin.H, 0, len(events))
	for _, event := range events {
		var details map[string]any
		_ = common.UnmarshalJsonStr(event.Data, &details)
		items = append(items, gin.H{"id": event.ID, "channel_id": event.ChannelID, "kind": event.Kind, "budget_day": event.BudgetDay, "occurred_at": event.OccurredAtMS / 1000, "details": details})
	}
	common.ApiSuccess(c, gin.H{"items": items, "total": total})
}

func ListSupplierAccounts(c *gin.Context) {
	var accounts []model.SupplierAccount
	if model.DB.Order("created_at DESC").Find(&accounts).Error != nil {
		common.ApiErrorT(c, "Please try again later.")
		return
	}
	items := make([]gin.H, 0, len(accounts))
	for _, account := range accounts {
		var supplier model.UpstreamSupplier
		_ = model.DB.First(&supplier, "id = ?", account.SupplierID).Error
		var user model.User
		_ = model.DB.Select("id", "username", "status").Where("supplier_id = ?", account.SupplierID).First(&user).Error
		items = append(items, gin.H{"id": account.SupplierID, "name": supplier.Name, "revision": account.Revision, "user_id": user.Id, "username": user.Username, "login_enabled": user.Status == common.UserStatusEnabled, "supply_enabled": account.SupplyEnabled, "allowed_types": account.Types(), "allowed_templates": account.TemplateIDs(), "allowed_models": account.Models(), "group": account.Group, "max_channels": account.MaxChannels, "max_output_tokens": account.MaxOutputTokens, "max_input_tokens": account.MaxInputTokens, "cost_ratio": account.CostRatio, "pool_priority": account.PoolPriority, "pool_weight": account.PoolWeight, "pool_capacity": account.PoolCapacity})
	}
	common.ApiSuccess(c, gin.H{"items": items, "total": len(items)})
}

func SaveSupplierAccount(c *gin.Context) {
	var request supplierAccountRequest
	if !decodeSupplierRequest(c, &request, "name", "revision", "user_id", "supply_enabled", "allowed_types", "allowed_templates", "allowed_models", "group", "max_channels", "max_input_tokens", "max_output_tokens", "cost_ratio", "pool_priority", "pool_weight", "pool_capacity") {
		return
	}
	if strings.TrimSpace(request.Name) == "" || utf8.RuneCountInString(request.Name) > 100 || len(request.AllowedTypes) == 0 && len(request.AllowedTemplates) == 0 || len(request.AllowedModels) == 0 || request.PoolCapacity < 0 || request.PoolCapacity > 100000 || request.MaxOutputTokens < 0 || request.MaxOutputTokens > int64(common.MaxQuota) || request.MaxInputTokens < 0 || request.MaxInputTokens > int64(common.MaxQuota) {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}
	if request.AllowedTemplates == nil {
		for _, kind := range request.AllowedTypes {
			request.AllowedTemplates = append(request.AllowedTemplates, strconv.Itoa(kind))
		}
	}
	var previousTemplates []string
	if c.Param("id") != "" && request.Revision > 0 {
		previous, err := model.GetSupplierAccount(c.Param("id"))
		if err != nil {
			common.ApiErrorT(c, "Invalid parameters")
			return
		}
		previousTemplates = previous.TemplateIDs()
	}
	request.AllowedTypes = nil
	for _, id := range request.AllowedTemplates {
		template, ok := service.SupplierTemplateByID(id)
		if !ok {
			if slices.Contains(previousTemplates, id) {
				continue
			}
			common.ApiErrorT(c, "Invalid parameters")
			return
		}
		if template.Type != constant.ChannelTypeTaskPlugin && !slices.Contains(request.AllowedTypes, template.Type) {
			request.AllowedTypes = append(request.AllowedTypes, template.Type)
		}
	}
	for _, name := range request.AllowedModels {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, ",\r\n") {
			common.ApiErrorT(c, "Invalid parameters")
			return
		}
	}
	id := c.Param("id")
	existingSupplier := id != ""
	context, _ := common.Marshal(service.SupplierManageContext{SupplierID: id, Version: request.Revision})
	if middleware.RequireSecurityProof(c, service.VerificationOperation{Scope: service.VerificationScopeSupplierManage, Context: context}) == nil {
		return
	}
	if id == "" {
		id = uuid.NewString()
	}
	typesJSON, _ := common.Marshal(request.AllowedTypes)
	templatesJSON, _ := common.Marshal(request.AllowedTemplates)
	account := model.SupplierAccount{SupplierID: id, SupplyEnabled: request.SupplyEnabled, AllowedTypes: string(typesJSON), AllowedTemplates: string(templatesJSON), AllowedModels: strings.Join(request.AllowedModels, ","), Group: request.Group, MaxChannels: request.MaxChannels, MaxOutputTokens: request.MaxOutputTokens, MaxInputTokens: request.MaxInputTokens, CostRatio: request.CostRatio, PoolPriority: request.PoolPriority, PoolWeight: request.PoolWeight, PoolCapacity: request.PoolCapacity}
	err := model.DB.Transaction(func(tx *gorm.DB) error {
		if request.Revision == 0 {
			if existingSupplier {
				var supplier model.UpstreamSupplier
				if err := tx.First(&supplier, "id = ?", id).Error; err != nil {
					return err
				}
			} else {
				if err := tx.Create(&model.UpstreamSupplier{ID: id, Name: strings.TrimSpace(request.Name), CreatedTime: time.Now().Unix()}).Error; err != nil {
					return err
				}
			}
		} else {
			if err := tx.Model(&model.UpstreamSupplier{}).Where("id = ?", id).Update("name", strings.TrimSpace(request.Name)).Error; err != nil {
				return err
			}
		}
		if err := model.SaveSupplierAccount(&account, request.Revision, tx); err != nil {
			return err
		}
		if request.UserID > 0 {
			var user model.User
			if err := tx.First(&user, request.UserID).Error; err != nil {
				return err
			}
			if user.GetSupplierID() != id {
				return model.AssignSupplierIdentityWithTx(tx, request.UserID, id)
			}
		}
		return nil
	})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if request.UserID > 0 {
		_ = model.PublishUserAuthCache(request.UserID)
		_, _ = model.RevokeAllUserSessions(request.UserID, "supplier_configuration_changed")
	}
	model.InitChannelCache()
	recordManageAudit(c, "supplier.configure", map[string]any{"supplier_id": id, "revision": account.Revision, "supply_enabled": account.SupplyEnabled})
	if !account.SupplyEnabled {
		var ids []int
		if model.DB.Model(&model.Channel{}).Where("owner_supplier_id = ?", id).Pluck("id", &ids).Error == nil {
			closeActiveChannelWebSockets(ids)
		}
	}
	common.ApiSuccess(c, gin.H{"id": id, "revision": account.Revision})
}

// References keep only supplier-local opaque correlation IDs in exported
// records. This whitelist must never become the consumer request-log DTO.
func ExportSupplierLogs(c *gin.Context) {
	rows, _, err := supplierLogRows(c, true)
	if err != nil {
		if errors.Is(err, errSupplierExportLimit) {
			common.ApiErrorT(c, "Too many logs to export. Narrow the filters and try again.")
		} else {
			common.ApiErrorT(c, "Please try again later.")
		}
		return
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", "attachment; filename=supplier-usage.csv")
	writer := csv.NewWriter(c.Writer)
	_ = writer.Write([]string{"time", "channel_id", "model", "status", "error_category", "latency_ms", "input_tokens", "output_tokens", "image_count", "audio_seconds", "image_usage_source", "audio_usage_source", "upstream_request_id", "reference", "protocol", "task_status", "task_observed_at", "task_completed_at", "task_duration_ms", "video_seconds", "credits", "video_usage_source", "credit_usage_source"})
	for _, row := range rows {
		name := row.ModelName
		if strings.ContainsAny(name[:min(1, len(name))], "=+-@\t\r\n") {
			name = "'" + name
		}
		audioSeconds := ""
		if row.AudioSeconds != nil {
			audioSeconds = strconv.FormatFloat(*row.AudioSeconds, 'f', -1, 64)
		}
		videoSeconds, credits := "", ""
		if row.VideoSeconds != nil {
			videoSeconds = strconv.FormatFloat(*row.VideoSeconds, 'f', -1, 64)
		}
		if row.Credits != nil {
			credits = *row.Credits
			if strings.HasPrefix(credits, "-") || strings.HasPrefix(credits, "+") || strings.HasPrefix(credits, "=") {
				credits = "'" + credits
			}
		}
		_ = writer.Write([]string{strconv.FormatInt(row.StartedAt, 10), strconv.Itoa(row.ChannelID), name, row.Status, row.ErrorCategory, strconv.FormatInt(row.LatencyMS, 10), supplierTokenCSV(row.InputTokens), supplierTokenCSV(row.OutputTokens), supplierTokenCSV(row.ImageCount), audioSeconds, row.ImageUsageSource, row.AudioUsageSource, row.UpstreamRequestID, row.Reference, row.Protocol, row.TaskStatus, supplierTokenCSV(row.TaskObservedAt), supplierTokenCSV(row.TaskCompletedAt), supplierTokenCSV(row.TaskDurationMS), videoSeconds, credits, row.VideoUsageSource, row.CreditUsageSource})
	}
	writer.Flush()
}

type supplierLogResponse struct {
	Protocol          string   `json:"protocol"`
	TaskStatus        string   `json:"task_status"`
	TaskObservedAt    *int64   `json:"task_observed_at"`
	TaskCompletedAt   *int64   `json:"task_completed_at"`
	TaskDurationMS    *int64   `json:"task_duration_ms"`
	VideoSeconds      *float64 `json:"video_seconds"`
	Credits           *string  `json:"credits"`
	VideoUsageSource  string   `json:"video_usage_source"`
	CreditUsageSource string   `json:"credit_usage_source"`
	IsStream          bool     `json:"is_stream"`
	FirstTokenMS      *int64   `json:"first_token_ms"`
	CacheReadTokens   *int64   `json:"cache_read_tokens"`
	CacheWriteTokens  *int64   `json:"cache_write_tokens"`
	ImageCount        *int64   `json:"image_count"`
	AudioSeconds      *float64 `json:"audio_seconds"`
	ImageUsageSource  string   `json:"image_usage_source"`
	AudioUsageSource  string   `json:"audio_usage_source"`
	Reference         string   `json:"id"`
	ChannelID         int      `json:"channel_id"`
	ChannelName       string   `json:"channel_name"`
	StartedAt         int64    `json:"created_at"`
	ModelName         string   `json:"model"`
	Stage             string   `json:"stage"`
	Status            string   `json:"result"`
	ErrorCategory     string   `json:"error_category"`
	StatusCode        int      `json:"status_code"`
	LatencyMS         int64    `json:"duration_ms"`
	InputTokens       *int64   `json:"input_tokens"`
	OutputTokens      *int64   `json:"output_tokens"`
	UsageSource       string   `json:"usage_source"`
	CorrelationID     string   `json:"correlation_id"`
	UpstreamRequestID string   `json:"upstream_request_id"`
}

// Logs and aggregates share this classification. An incomplete response or a
// long latency is not evidence of a timeout; only a recorded timeout status is.
const supplierErrorCategorySQL = "CASE WHEN success THEN 'none' WHEN upstream_status_code IN (408, 504) THEN 'timeout' WHEN upstream_status_code = 429 THEN 'rate_limited' WHEN upstream_status_code IN (401, 403) THEN 'auth_error' WHEN upstream_status_code >= 500 AND upstream_status_code < 600 THEN 'upstream_error' WHEN upstream_status_code >= 400 AND upstream_status_code < 500 THEN 'request_error' ELSE 'unknown' END"

func supplierEventQuery(c *gin.Context) (*gorm.DB, error) {
	days, _ := strconv.Atoi(c.DefaultQuery("days", "7"))
	if days < 1 || days > 90 {
		return nil, fmt.Errorf("invalid date range")
	}
	from, to := time.Now().Add(-time.Duration(days)*24*time.Hour).Unix(), time.Now().Unix()
	if raw := c.Query("from"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid date range")
		}
		from = value
	}
	if raw := c.Query("to"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid date range")
		}
		to = value
	}
	if from < 0 || to <= from || to-from > 90*86400 {
		return nil, fmt.Errorf("invalid date range")
	}
	query := model.DB.Model(&model.OverviewAttemptEvent{}).Where("started_at >= ? AND started_at <= ?", from, to)
	if c.GetInt("role") == common.RoleSupplierUser {
		owner := c.GetString("supplier_id")
		if owner == "" {
			return nil, fmt.Errorf("invalid supplier")
		}
		query = query.Where("authorized_supplier_id = ?", owner)
	}
	if value := c.Query("channel_id"); value != "" {
		id, err := strconv.Atoi(value)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid channel")
		}
		query = query.Where("channel_id = ?", id)
	}
	if name := c.Query("model"); name != "" {
		query = query.Where("model_name = ?", name)
	}
	if stage := c.Query("stage"); stage != "" {
		if !slices.Contains([]string{"inference", "embedding", "rerank", "image", "audio", "task_submit", "realtime_generation", "realtime_transcription"}, stage) {
			return nil, fmt.Errorf("invalid stage")
		}
		query = query.Where("call_stage = ?", stage)
	}
	if status := c.Query("task_status"); status != "" {
		if !slices.Contains([]string{"accepted", "queued", "running", "succeeded", "failed", "unknown"}, status) {
			return nil, fmt.Errorf("invalid task status")
		}
		query = query.Where("task_status = ?", status)
	}
	if reference := c.Query("correlation_id"); reference != "" {
		query = query.Where("supplier_reference = ?", reference)
	}
	switch c.Query("result") {
	case "":
	case "success":
		query = query.Where("success = ?", true)
	case "failure":
		query = query.Where("failure = ?", true)
	case "unknown":
		query = query.Where("success = ? AND failure = ?", false, false)
	default:
		return nil, fmt.Errorf("invalid result")
	}
	return query, nil
}

func supplierLogRows(c *gin.Context, export bool) ([]supplierLogResponse, int64, error) {
	query, err := supplierEventQuery(c)
	if err != nil {
		return nil, 0, err
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	page := common.GetPageQuery(c)
	limit, offset := page.GetPageSize(), page.GetStartIdx()
	if export {
		limit, offset = 10000, 0
		if total > 10000 {
			return nil, 0, errSupplierExportLimit
		}
	}
	var events []struct {
		model.OverviewAttemptEvent
		ErrorCategory string
	}
	if err := query.Select("supplier_reference", "channel_id", "channel_name", "started_at", "model_name", "success", "failure", "latency_ms", "input_tokens", "output_tokens", "usage_source", "upstream_status_code", "call_stage", "is_stream", "first_token_ms", "cache_usage_known", "cache_read_tokens", "cache_write_tokens", "image_count", "audio_seconds", "image_usage_source", "audio_usage_source", "upstream_request_id", "task_status", "task_observed_at", "task_completed_at", "task_duration_ms", "video_seconds", "credits", "video_usage_source", "credit_usage_source", supplierErrorCategorySQL+" AS error_category").Order("started_at DESC, id DESC").Offset(offset).Limit(limit).Find(&events).Error; err != nil {
		return nil, 0, err
	}
	rows := make([]supplierLogResponse, 0, len(events))
	for _, event := range events {
		row := supplierLogResponse{Reference: event.SupplierReference, ChannelID: event.ChannelID, ChannelName: event.ChannelName, StartedAt: event.StartedAt, ModelName: event.ModelName, Stage: event.CallStage, IsStream: event.IsStream, FirstTokenMS: event.FirstTokenMS, Status: "unknown", ErrorCategory: event.ErrorCategory, StatusCode: event.UpstreamStatusCode, LatencyMS: event.LatencyMS, UsageSource: event.UsageSource, CorrelationID: event.SupplierReference}
		row.Protocol = "http"
		if event.CallStage == "task_submit" {
			row.Protocol = "async_task"
		}
		if strings.HasPrefix(event.CallStage, "realtime_") {
			row.Protocol = "realtime"
		}
		row.TaskStatus, row.TaskObservedAt, row.TaskCompletedAt, row.TaskDurationMS = event.TaskStatus, event.TaskObservedAt, event.TaskCompletedAt, event.TaskDurationMS
		row.VideoSeconds, row.Credits, row.VideoUsageSource, row.CreditUsageSource = event.VideoSeconds, event.Credits, event.VideoUsageSource, event.CreditUsageSource
		row.ImageCount, row.AudioSeconds = event.ImageCount, event.AudioSeconds
		row.ImageUsageSource, row.AudioUsageSource = event.ImageUsageSource, event.AudioUsageSource
		row.UpstreamRequestID = event.UpstreamRequestID
		if row.Stage == "" {
			row.Stage = "unknown"
		}
		if event.CacheUsageKnown {
			read, write := event.CacheReadTokens, event.CacheWriteTokens
			row.CacheReadTokens, row.CacheWriteTokens = &read, &write
		}
		if event.Success {
			row.Status = "success"
		} else if event.Failure {
			row.Status = "failure"
		}
		if event.UsageSource != "" {
			input, output := event.InputTokens, event.OutputTokens
			row.InputTokens, row.OutputTokens = &input, &output
		} else {
			row.UsageSource = "unknown"
		}
		rows = append(rows, row)
	}
	return rows, total, nil
}

func GetSupplierLogs(c *gin.Context) {
	rows, total, err := supplierLogRows(c, false)
	if err != nil {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}
	common.ApiSuccess(c, gin.H{"items": rows, "total": total})
}

type supplierAsyncMetrics struct {
	SubmittedCount         int64    `json:"submitted_count"`
	AcceptedCount          int64    `json:"accepted_count"`
	RejectedCount          int64    `json:"rejected_count"`
	UnknownSubmissionCount int64    `json:"unknown_submission_count"`
	QueuedCount            int64    `json:"queued_count"`
	RunningCount           int64    `json:"running_count"`
	SucceededCount         int64    `json:"succeeded_count"`
	FailedCount            int64    `json:"failed_count"`
	UnknownCount           int64    `json:"unknown_count"`
	AvgSubmitLatencyMS     *float64 `json:"avg_submit_latency_ms"`
	AvgCompletionMS        *float64 `json:"avg_completion_ms"`
}

type supplierOverviewMetrics struct {
	Async                   supplierAsyncMetrics `json:"async" gorm:"embedded;embeddedPrefix:async_"`
	RealtimeGenerationCount int64                `json:"realtime_generation_count"`
	VideoSeconds            *float64             `json:"video_seconds"`
	Credits                 *string              `json:"credits" gorm:"-"`
	VideoUsageKnownCount    int64                `json:"video_usage_known_count"`
	CreditUsageKnownCount   int64                `json:"credit_usage_known_count"`
	Timestamp               int64                `json:"timestamp"`
	AverageFirstToken       *float64             `json:"avg_first_token_ms"`
	CacheReadTokens         int64                `json:"cache_read_tokens"`
	CacheWriteTokens        int64                `json:"cache_write_tokens"`
	UsageKnownCount         int64                `json:"usage_known_count"`
	ChannelID               int                  `json:"channel_id"`
	ChannelName             string               `json:"channel_name"`
	RequestCount            int64                `json:"request_count"`
	LogicalRequestCount     int64                `json:"logical_request_count"`
	LastCalledAt            *int64               `json:"last_called_at"`
	TimeoutCount            int64                `json:"timeout_count"`
	RateLimitedCount        int64                `json:"rate_limited_count"`
	AuthErrorCount          int64                `json:"auth_error_count"`
	UpstreamErrorCount      int64                `json:"upstream_error_count"`
	RequestErrorCount       int64                `json:"request_error_count"`
	UnknownErrorCount       int64                `json:"unknown_error_count"`
	ImageCount              *int64               `json:"image_count"`
	AudioSeconds            *float64             `json:"audio_seconds"`
	ImageUsageKnownCount    int64                `json:"image_usage_known_count"`
	AudioUsageKnownCount    int64                `json:"audio_usage_known_count"`
	SuccessCount            int64                `json:"success_count"`
	FailureCount            int64                `json:"failure_count"`
	UnknownCount            int64                `json:"unknown_count"`
	InputTokens             int64                `json:"input_tokens"`
	OutputTokens            int64                `json:"output_tokens"`
	AverageLatency          *float64             `json:"avg_latency_ms"`
}

func GetSupplierOverview(c *gin.Context) {
	query, err := supplierEventQuery(c)
	if err != nil {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}
	metrics := []string{"AVG(first_token_ms) AS average_first_token, SUM(cache_read_tokens) AS cache_read_tokens, SUM(cache_write_tokens) AS cache_write_tokens, SUM(CASE WHEN usage_source <> '' THEN 1 ELSE 0 END) AS usage_known_count, COUNT(*) AS request_count, SUM(CASE WHEN call_stage <> 'task_submit' AND success THEN 1 ELSE 0 END) AS success_count, SUM(CASE WHEN call_stage <> 'task_submit' AND failure THEN 1 ELSE 0 END) AS failure_count, SUM(CASE WHEN call_stage <> 'task_submit' AND NOT success AND NOT failure THEN 1 ELSE 0 END) AS unknown_count, SUM(input_tokens) AS input_tokens, SUM(output_tokens) AS output_tokens, AVG(CASE WHEN call_stage <> 'task_submit' AND finished_at > 0 THEN latency_ms ELSE NULL END) AS average_latency"}
	// The scoped query is formed before aggregation. Request IDs are used only
	// for counting and never leave this query or become cross-supplier links.
	metrics = append(metrics, "COUNT(DISTINCT NULLIF(request_id, '')) AS logical_request_count, MAX(started_at) AS last_called_at")
	metrics = append(metrics, "SUM(image_count) AS image_count, SUM(audio_seconds) AS audio_seconds, COUNT(image_count) AS image_usage_known_count, COUNT(audio_seconds) AS audio_usage_known_count")
	for _, category := range []struct{ value, column string }{
		{"timeout", "timeout_count"}, {"rate_limited", "rate_limited_count"}, {"auth_error", "auth_error_count"}, {"upstream_error", "upstream_error_count"}, {"request_error", "request_error_count"},
	} {
		metrics = append(metrics, "SUM(CASE WHEN ("+supplierErrorCategorySQL+") = '"+category.value+"' THEN 1 ELSE 0 END) AS "+category.column)
	}
	metrics = append(metrics, "SUM(CASE WHEN failure AND ("+supplierErrorCategorySQL+") = 'unknown' THEN 1 ELSE 0 END) AS unknown_error_count")
	metrics = append(metrics, "SUM(video_seconds) AS video_seconds, COUNT(video_seconds) AS video_usage_known_count, COUNT(credits) AS credit_usage_known_count, SUM(CASE WHEN call_stage = 'realtime_generation' THEN 1 ELSE 0 END) AS realtime_generation_count")
	for _, item := range []struct{ condition, name string }{
		{"1=1", "submitted_count"}, {"success", "accepted_count"}, {"failure", "rejected_count"}, {"NOT success AND NOT failure", "unknown_submission_count"},
		{"task_status IN ('accepted', 'queued')", "queued_count"}, {"task_status = 'running'", "running_count"}, {"task_status = 'succeeded'", "succeeded_count"}, {"task_status = 'failed'", "failed_count"}, {"task_status = 'unknown' OR (success AND (task_status IS NULL OR task_status = ''))", "unknown_count"},
	} {
		metrics = append(metrics, "SUM(CASE WHEN call_stage = 'task_submit' AND ("+item.condition+") THEN 1 ELSE 0 END) AS async_"+item.name)
	}
	metrics = append(metrics, "AVG(CASE WHEN call_stage = 'task_submit' AND finished_at > 0 THEN latency_ms ELSE NULL END) AS async_avg_submit_latency_ms, AVG(CASE WHEN call_stage = 'task_submit' AND task_status IN ('succeeded', 'failed') THEN task_duration_ms ELSE NULL END) AS async_avg_completion_ms")
	metricsSQL := strings.Join(metrics, ", ")
	var total supplierOverviewMetrics
	if query.Session(&gorm.Session{}).Select(metricsSQL).Scan(&total).Error != nil {
		common.ApiErrorT(c, "Please try again later.")
		return
	}
	rows := []supplierOverviewMetrics{}
	if query.Session(&gorm.Session{}).Select("channel_id, channel_name, "+metricsSQL).Group("channel_id, channel_name").Order("channel_id DESC").Scan(&rows).Error != nil {
		common.ApiErrorT(c, "Please try again later.")
		return
	}
	bucket := "CAST(started_at / 3600 AS BIGINT) * 3600"
	if common.UsingMainDatabase(common.DatabaseTypeMySQL) {
		bucket = "FLOOR(started_at / 3600) * 3600"
	}
	trend := []supplierOverviewMetrics{}
	if query.Session(&gorm.Session{}).Select(bucket+" AS timestamp, "+metricsSQL).Group(bucket).Order("timestamp ASC").Scan(&trend).Error != nil {
		common.ApiErrorT(c, "Please try again later.")
		return
	}
	// Credits are decimal vendor units, not money or tokens. Summing text via
	// SQL would either fail on PostgreSQL or lose precision on SQLite.
	creditRows, err := query.Session(&gorm.Session{}).Select("channel_id", "channel_name", "started_at", "credits").Where("credits IS NOT NULL").Rows()
	if err != nil {
		common.ApiErrorT(c, "Please try again later.")
		return
	}
	type creditChannel struct {
		id   int
		name string
	}
	byChannel := map[creditChannel]decimal.Decimal{}
	byHour := map[int64]decimal.Decimal{}
	creditTotal := decimal.Zero
	for creditRows.Next() {
		var id int
		var name, raw string
		var started int64
		if err = creditRows.Scan(&id, &name, &started, &raw); err != nil {
			break
		}
		var amount decimal.Decimal
		amount, err = decimal.NewFromString(raw)
		if err != nil || amount.IsNegative() {
			err = model.ErrChannelLimitAccounting
			break
		}
		creditTotal = creditTotal.Add(amount)
		key, hour := creditChannel{id, name}, started/3600*3600
		byChannel[key], byHour[hour] = byChannel[key].Add(amount), byHour[hour].Add(amount)
	}
	if err == nil {
		err = creditRows.Err()
	}
	_ = creditRows.Close()
	if err != nil {
		common.ApiErrorT(c, "Please try again later.")
		return
	}
	if total.CreditUsageKnownCount > 0 {
		total.Credits = common.GetPointer(creditTotal.String())
	}
	for i := range rows {
		if rows[i].CreditUsageKnownCount > 0 {
			rows[i].Credits = common.GetPointer(byChannel[creditChannel{rows[i].ChannelID, rows[i].ChannelName}].String())
		}
	}
	for i := range trend {
		if trend[i].CreditUsageKnownCount > 0 {
			trend[i].Credits = common.GetPointer(byHour[trend[i].Timestamp].String())
		}
	}
	var collection model.OverviewCollectionState
	_ = model.DB.First(&collection, 1).Error
	var channels []model.Channel
	channelQuery := supplierChannelScope(c)
	if id := c.Query("channel_id"); id != "" {
		channelQuery = channelQuery.Where("id = ?", id)
	}
	if channelQuery.Omit("key", "upstream_credential_snapshot_data").Find(&channels).Error != nil {
		common.ApiErrorT(c, "Please try again later.")
		return
	}
	ids := make([]int, 0, len(channels))
	available := 0
	location, _ := time.LoadLocation("Asia/Shanghai")
	day := time.Now().In(location).Format("2006-01-02")
	dayStart, _ := time.ParseInLocation("2006-01-02", day, location)
	costComplete := true
	for i := range channels {
		channel := &channels[i]
		ids = append(ids, channel.Id)
		state, err := model.GetChannelLimitState(channel.Id, time.Now())
		if err != nil || !(state.Config.HistoryComplete || state.Config.ObservedSince > 0 && state.Config.ObservedSince <= dayStart.UnixMilli()) {
			costComplete = false
		}
		templateAvailable := true
		if channel.OwnerSupplierID != "" {
			_, templateAvailable = service.SupplierTemplateByID(model.SupplierChannelTemplateID(channel))
		}
		if templateAvailable && channel.Status == common.ChannelStatusEnabled && model.SupplierChannelAvailable(channel) && channel.ScheduleAllowsAt(time.Now()) {
			if err == nil && state.Reason == "" {
				available++
			}
		}
	}
	var cost struct {
		Used    int64
		Unknown int64
	}
	if len(ids) > 0 {
		if model.DB.Model(&model.ChannelDailyBudget{}).Where("channel_id IN ? AND budget_day = ?", ids, day).Select("COALESCE(SUM(used_usd_nano),0) AS used, COALESCE(SUM(unknown_costs),0) AS unknown").Scan(&cost).Error != nil {
			common.ApiErrorT(c, "Please try again later.")
			return
		}
	}
	var daily *string
	if cost.Unknown == 0 && costComplete {
		value := model.FormatChannelLimitUSD(cost.Used)
		daily = &value
	}
	common.ApiSuccess(c, gin.H{"async": total.Async, "realtime_generation_count": total.RealtimeGenerationCount, "video_seconds": total.VideoSeconds, "credits": total.Credits, "video_usage_known_count": total.VideoUsageKnownCount, "credit_usage_known_count": total.CreditUsageKnownCount, "channel_count": len(channels), "available_channels": available, "request_count": total.RequestCount, "logical_request_count": total.LogicalRequestCount, "last_called_at": total.LastCalledAt, "timeout_count": total.TimeoutCount, "rate_limited_count": total.RateLimitedCount, "auth_error_count": total.AuthErrorCount, "upstream_error_count": total.UpstreamErrorCount, "request_error_count": total.RequestErrorCount, "unknown_error_count": total.UnknownErrorCount, "image_count": total.ImageCount, "audio_seconds": total.AudioSeconds, "image_usage_known_count": total.ImageUsageKnownCount, "audio_usage_known_count": total.AudioUsageKnownCount, "success_count": total.SuccessCount, "failure_count": total.FailureCount, "unknown_count": total.UnknownCount, "input_tokens": total.InputTokens, "output_tokens": total.OutputTokens, "avg_latency_ms": total.AverageLatency, "daily_used_usd": daily, "budget_day": day, "updated_at": time.Now().Unix(), "channels": rows, "trend": trend, "avg_first_token_ms": total.AverageFirstToken, "cache_read_tokens": total.CacheReadTokens, "cache_write_tokens": total.CacheWriteTokens, "usage_known_count": total.UsageKnownCount, "coverage": gin.H{"collected_since": collection.CollectedSince, "timezone": "UTC", "limitations": []string{}}})
}

// Test candidates on an isolated copy. The active channel remains unchanged
// until the complete version-fenced publish transaction succeeds.
func validateSupplierChannelDraft(id int, owner string, version int64, userID int) {
	select {
	case supplierTestSlots <- struct{}{}:
		defer func() { <-supplierTestSlots }()
	default:
		return
	}
	claimed := model.DB.Model(&model.SupplierChannelDraft{}).Where("channel_id = ? AND supplier_id = ? AND version = ? AND status IN ?", id, owner, version, []string{"pending", "failed", "cost_config_missing", "waiting", "result_unknown"}).Updates(map[string]any{"status": "testing", "lease_until": time.Now().Add(2 * time.Minute).Unix()})
	if claimed.Error != nil || claimed.RowsAffected != 1 {
		return
	}
	var draft model.SupplierChannelDraft
	var channel model.Channel
	if model.DB.First(&draft, "channel_id = ? AND version = ?", id, version).Error != nil || model.DB.First(&channel, "id = ? AND owner_supplier_id = ?", id, owner).Error != nil {
		return
	}
	status := "failed"
	stop := make(chan struct{})
	defer close(stop)
	_ = model.DB.Model(&model.SupplierChannelDraft{}).Where("channel_id = ? AND version = ?", id, version).Update("lease_until", time.Now().Add(2*time.Minute).Unix()).Error
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = model.DB.Model(&model.SupplierChannelDraft{}).Where("channel_id = ? AND version = ? AND status = ?", id, version, "testing").Update("lease_until", time.Now().Add(2*time.Minute).Unix()).Error
			}
		}
	}()
	defer func() {
		_ = model.DB.Model(&model.SupplierChannelDraft{}).Where("channel_id = ? AND version = ? AND status = ?", id, version, "testing").Updates(map[string]any{"status": status, "error_code": status}).Error
		model.InitChannelCache()
	}()
	account, err := model.GetSupplierAccount(owner)
	if err != nil || !account.SupplyEnabled || !account.AllowsTemplate(draft.Template(), strings.Split(draft.Models, ",")) {
		return
	}
	state, err := model.GetChannelLimitState(id, time.Now())
	if err != nil {
		return
	}
	var pricing model.Channel
	if err := model.DB.Select("id", "cost_ratio", "cost_version_id", "upstream_credential_snapshot_data").First(&pricing, "id = ?", id).Error; err != nil {
		return
	}
	if state.Config.BudgetReady != pricing.ProcurementCostReady() {
		config, err := model.UpdateChannelLimits(id, state.Config.Revision, state.Config.ChannelLimitSettings, pricing.ProcurementCostReady(), time.Now())
		if err != nil {
			return
		}
		state.Config = config
	}
	if state.Config.DailyUSDNano > 0 && !state.Config.BudgetReady {
		status = "cost_config_missing"
		return
	}
	secret, err := model.DecryptSupplierSecret(owner, draft.Secret)
	if err != nil {
		return
	}
	if service.ValidateSupplierURL(draft.BaseURL) != nil {
		return
	}
	existingCredentials := make(map[string]string)
	if channel.Type == draft.Type && strings.TrimRight(channel.GetBaseURL(), "/") == strings.TrimRight(draft.BaseURL, "/") {
		for index, key := range channel.GetKeys() {
			snapshot := channel.GetUpstreamCredentialSnapshot(index)
			if snapshot.Active {
				existingCredentials[key] = snapshot.CredentialID
			}
		}
	}
	channel.Type, channel.Key, channel.Keys, channel.BaseURL, channel.Models = draft.Type, secret, nil, &draft.BaseURL, draft.Models
	draft.ApplyTemplate(&channel)
	channel.CostRatio, channel.CostVersionID = pricing.CostRatio, pricing.CostVersionID
	channel.ChannelInfo = model.ChannelInfo{}
	// Validation probes test the isolated draft even when the live channel is
	// new or supplier-paused. Only PublishSupplierChannelDraft may activate it.
	channel.SupplierValidated, channel.SupplierPaused = true, false
	keys := strings.Split(strings.TrimSpace(secret), "\n")
	passed := []string{}
	pendingStatus := ""
	batchContext, batchCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer batchCancel()
	for _, name := range strings.Split(draft.Models, ",") {
		if batchContext.Err() != nil {
			break
		}
		endpoint := ""
		if common.IsImageGenerationModel(name) {
			endpoint = string(constant.EndpointTypeImageGeneration)
		} else if common.IsOpenAIResponseOnlyModel(name) {
			endpoint = string(constant.EndpointTypeOpenAIResponse)
		}
		valid := true
		for keyIndex, key := range keys {
			candidate := channel
			candidate.Key = strings.TrimSpace(key)
			candidate.ProcurementCredentialID = common.GetPointer(existingCredentials[candidate.Key])
			ctx, cancel := context.WithTimeout(batchContext, 2*time.Minute)
			probeStatus := "validated"
			if candidate.Type == constant.ChannelTypeTaskPlugin {
				probeStatus = supplierTaskProbe(ctx, &draft, &candidate, userID, name, keyIndex)
			} else if candidate.Type == constant.ChannelTypeMidjourney || candidate.Type == constant.ChannelTypeMidjourneyPlus {
				probeStatus = supplierMidjourneyProbe(ctx, &draft, &candidate, userID, name, keyIndex)
			} else if candidate.Type == constant.ChannelTypeOpenAI && (strings.Contains(strings.ToLower(name), "transcrib") || strings.HasPrefix(strings.ToLower(name), "whisper-")) {
				if testSupplierTranscriptionChannel(ctx, &candidate, userID, name) != nil {
					probeStatus = "failed"
				}
			} else if candidate.Type == constant.ChannelTypeOpenAI && strings.Contains(strings.ToLower(name), "realtime") {
				if testSupplierRealtimeChannel(ctx, &candidate, userID, name) != nil {
					probeStatus = "failed"
				}
			} else {
				result := testChannel(ctx, &candidate, userID, name, endpoint, false)
				if result.localErr != nil || result.newAPIError != nil {
					probeStatus = "failed"
				}
			}
			cancel()
			if probeStatus == "waiting" || probeStatus == "result_unknown" {
				pendingStatus = probeStatus
			}
			if probeStatus != "validated" {
				valid = false
				break
			}
		}
		if valid {
			passed = append(passed, name)
		}
	}
	if pendingStatus != "" {
		status = pendingStatus
		return
	}
	if len(passed) > 0 && model.PublishSupplierChannelDraft(id, owner, version, passed) == nil {
		status = "validated"
	}
}

var supplierTestSlots = make(chan struct{}, 4)
var supplierWorkerOnce sync.Once
var errSupplierExportLimit = errors.New("supplier export limit")

func StartSupplierValidationWorker() {
	supplierWorkerOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				_ = model.DB.Model(&model.SupplierChannelDraft{}).Where("status = ? AND lease_until < ?", "testing", time.Now().Unix()).Update("status", "pending").Error
				var drafts []model.SupplierChannelDraft
				if model.DB.Where("status IN ?", []string{"pending", "cost_config_missing", "waiting", "result_unknown"}).Order("created_at ASC").Limit(20).Find(&drafts).Error != nil {
					continue
				}
				for _, draft := range drafts {
					var channel model.Channel
					if model.DB.Select("id", "created_by").First(&channel, draft.ChannelID).Error == nil {
						go validateSupplierChannelDraft(draft.ChannelID, draft.SupplierID, draft.Version, channel.CreatedBy)
					}
				}
			}
		}()
	})
}

func supplierTokenCSV(value *int64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatInt(*value, 10)
}
