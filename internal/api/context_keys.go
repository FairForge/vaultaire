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
)
