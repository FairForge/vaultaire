// internal/api/context_keys.go
package api

// Context key types to avoid collisions
type contextKey string

const (
	userIDKey   contextKey = "user_id"
	emailKey    contextKey = "email"
	tenantIDKey contextKey = "tenant_id"
)
