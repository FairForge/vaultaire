// internal/api/context_keys.go
package api

// Context key types to avoid collisions
type contextKey string

const (
	userIDKey   contextKey = "user_id"
	emailKey    contextKey = "email"
	tenantIDKey contextKey = "tenant_id"
	// jwtIDKey: the management JWT's id (jti), the request log's key_id (RP78-6).
	jwtIDKey contextKey = "jwt_id"
	// roleKey: the acting member's role on the JWT's tenant (WP-TEAM-1),
	// an auth.Role resolved by requireJWT from tenant_members.
	roleKey contextKey = "member_role"
)
