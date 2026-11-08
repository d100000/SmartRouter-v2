package model

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var ErrSupplierPasswordLength = errors.New("Supplier account passwords must contain at least 15 characters")

func (user *User) GetSupplierID() string {
	if user.SupplierID == nil {
		return ""
	}
	return *user.SupplierID
}

func (user *User) validateSupplyIdentity(tx *gorm.DB) error {
	supplierID := strings.TrimSpace(user.GetSupplierID())
	if user.ChannelViewer && user.Role != common.RoleCommonUser {
		return common.NewMessage("Invalid parameters")
	}
	if user.Role != common.RoleSupplierUser {
		if supplierID != "" {
			return common.NewMessage("Invalid parameters")
		}
		user.SupplierID = nil
		return nil
	}
	if supplierID == "" || len(supplierID) > 36 || user.ChannelViewer {
		return common.NewMessage("Invalid parameters")
	}
	if user.Password != "" && utf8.RuneCountInString(user.Password) < 15 {
		return ErrSupplierPasswordLength
	}
	var supplier UpstreamSupplier
	if err := tx.Select("id").Where("id = ?", supplierID).First(&supplier).Error; err != nil {
		return err
	}
	user.SupplierID = &supplierID
	return nil
}

// AssignSupplierIdentity binds a dedicated, unused account to one procurement
// supplier. The nullable unique supplier_id column also protects concurrent
// assignments on every supported database.
func AssignSupplierIdentity(userID int, supplierID string) error {
	if err := DB.Transaction(func(tx *gorm.DB) error {
		return AssignSupplierIdentityWithTx(tx, userID, supplierID)
	}); err != nil {
		return err
	}
	if err := PublishUserAuthCache(userID); err != nil {
		return err
	}
	_, err := RevokeAllUserSessions(userID, "supplier_identity_assigned")
	return err
}

// AssignSupplierIdentityWithTx requires the caller to publish the user auth
// cache and revoke sessions after its transaction commits.
func AssignSupplierIdentityWithTx(tx *gorm.DB, userID int, supplierID string) error {
	var user User
	if err := lockForUpdate(tx).First(&user, userID).Error; err != nil {
		return err
	}
	if user.Role != common.RoleCommonUser || user.ChannelViewer || user.GetSupplierID() != "" ||
		user.Quota != 0 || user.UsedQuota != 0 || user.RequestCount != 0 || user.LastLoginAt != 0 || user.AffCount != 0 || user.AffQuota != 0 || user.AffHistoryQuota != 0 || user.InviterId != 0 || user.GetAccessToken() != "" {
		return common.NewMessage("Invalid parameters")
	}
	for _, record := range []any{&Token{}, &UserAccessToken{}, &TopUp{}, &SubscriptionOrder{}, &UserSubscription{}, &UserSession{}} {
		var count int64
		if err := tx.Unscoped().Model(record).Where("user_id = ?", userID).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return common.NewMessage("Invalid parameters")
		}
	}
	user.Role = common.RoleSupplierUser
	user.SupplierID = &supplierID
	if err := user.validateSupplyIdentity(tx); err != nil {
		return err
	}
	if _, err := IncrementUserAuthVersionWithTx(tx, userID); err != nil {
		return err
	}
	return tx.Model(&User{}).Where("id = ?", userID).Updates(map[string]any{
		"role": common.RoleSupplierUser, "supplier_id": user.GetSupplierID(),
		"channel_viewer": false, "must_change_password": true,
	}).Error
}

// SetChannelViewer changes only the additive channel read grant. Consumer
// credentials and accounting remain intact; active browser sessions are revoked
// after the authentication version fence has been committed and published.
func SetChannelViewer(userID int, enabled bool) error {
	changed := false
	if err := DB.Transaction(func(tx *gorm.DB) error {
		var user User
		if err := lockForUpdate(tx).First(&user, userID).Error; err != nil {
			return err
		}
		if user.Role != common.RoleCommonUser || user.GetSupplierID() != "" {
			return common.NewMessage("Invalid parameters")
		}
		if user.ChannelViewer == enabled {
			return nil
		}
		if _, err := IncrementUserAuthVersionWithTx(tx, userID); err != nil {
			return err
		}
		changed = true
		return tx.Model(&User{}).Where("id = ?", userID).Update("channel_viewer", enabled).Error
	}); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := PublishUserAuthCache(userID); err != nil {
		return err
	}
	_, err := RevokeAllUserSessions(userID, "channel_viewer_changed")
	return err
}
