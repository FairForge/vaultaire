package common

// ContextKey is the type for context keys
type ContextKey string

// Context keys used across the application
const (
	TenantIDKey ContextKey = "tenant_id"
)
