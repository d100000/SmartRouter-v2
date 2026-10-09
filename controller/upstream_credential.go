package controller

import (
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
)

func GetChannelUpstreamCredentials(c *gin.Context) {
	if !authz.Can(c.GetInt("id"), c.GetInt("role"), authz.ChannelRead) {
		common.ApiErrorI18n(c, i18n.MsgAuthInsufficientPrivilege)
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		common.ApiErrorI18n(c, i18n.MsgInvalidParams)
		return
	}
	snapshots, err := model.GetChannelUpstreamCredentials(id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, snapshots)
}

func UpdateUpstreamCredential(c *gin.Context) {
	if !authz.Can(c.GetInt("id"), c.GetInt("role"), authz.ChannelWrite) {
		common.ApiErrorI18n(c, i18n.MsgAuthInsufficientPrivilege)
		return
	}
	var metadata model.UpstreamCredentialMetadata
	if err := c.ShouldBindJSON(&metadata); err != nil {
		common.ApiError(c, err)
		return
	}
	credential, err := model.UpdateUpstreamCredentialMetadata(c.Param("id"), metadata)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	model.InitChannelCache()
	recordManageAudit(c, "upstream_credential.update", map[string]any{"id": credential.ID, "supplier_id": credential.SupplierID, "cost_version_id": credential.CostVersionID, "ownership_version_id": credential.OwnershipVersionID})
	common.ApiSuccess(c, credential)
}

func GetUpstreamSuppliers(c *gin.Context) {
	if !authz.Can(c.GetInt("id"), c.GetInt("role"), authz.ChannelRead) {
		common.ApiErrorI18n(c, i18n.MsgAuthInsufficientPrivilege)
		return
	}
	suppliers, err := model.ListUpstreamSuppliers()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, suppliers)
}

func AddUpstreamSupplier(c *gin.Context) {
	if !authz.Can(c.GetInt("id"), c.GetInt("role"), authz.ChannelWrite) {
		common.ApiErrorI18n(c, i18n.MsgAuthInsufficientPrivilege)
		return
	}
	var request struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		common.ApiError(c, err)
		return
	}
	supplier, err := model.CreateUpstreamSupplier(request.Name)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "upstream_supplier.create", map[string]any{"id": supplier.ID})
	common.ApiSuccess(c, supplier)
}
