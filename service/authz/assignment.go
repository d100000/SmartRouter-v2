package authz

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// resolveSubjectRoles returns the role keys assigned to a subject. The mapping
// is derived from the caller's system role.
var resolveSubjectRoles = func(userID int, systemRole int) []string {
	switch {
	case systemRole >= common.RoleRootUser:
		return []string{BuiltInRoleRoot}
	case systemRole >= common.RoleAdminUser:
		return []string{BuiltInRoleAdmin}
	case systemRole == common.RoleCommonUser && model.DB != nil:
		user, err := model.GetUserCache(userID)
		if err == nil && user.Role == common.RoleCommonUser && user.Status == common.UserStatusEnabled && user.ChannelViewer && user.SupplierID == "" {
			return []string{BuiltInRoleChannelViewer}
		}
		return nil
	default:
		return nil
	}
}

// managedRoleKey is the role whose baseline per-user overrides are expressed
// relative to.
const managedRoleKey = BuiltInRoleAdmin
