package middleware

import "strings"

// Supplier accounts are management-only identities. Deny by default so adding
// a new UserAuth consumer route cannot accidentally expose it to suppliers.
func supplierDashboardRouteAllowed(method, path string) bool {
	if strings.HasPrefix(path, "/api/supplier/") {
		return true
	}
	switch method + " " + path {
	case "GET /api/user/self", "PUT /api/user/self", "PUT /api/user/setting",
		"GET /api/user/sessions", "DELETE /api/user/sessions/:sid", "POST /api/user/sessions/revoke-others",
		"GET /api/verify/methods", "POST /api/verify", "GET /api/audit/self",
		"GET /api/user/passkey", "POST /api/user/passkey/register/begin", "POST /api/user/passkey/register/finish",
		"POST /api/user/passkey/verify/begin", "POST /api/user/passkey/verify/finish", "DELETE /api/user/passkey",
		"GET /api/user/2fa/status", "POST /api/user/2fa/setup", "POST /api/user/2fa/enable",
		"POST /api/user/2fa/disable", "POST /api/user/2fa/backup_codes",
		"GET /api/user/oauth/bindings", "DELETE /api/user/oauth/bindings/:provider_id",
		"POST /api/oauth/email/bind/start", "POST /api/oauth/email/bind/resend", "POST /api/oauth/email/bind",
		"POST /api/oauth/wechat/bind", "POST /api/oauth/telegram/bind/start":
		return true
	default:
		return false
	}
}
