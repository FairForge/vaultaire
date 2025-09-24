// internal/common/context.go
package common

import "context"

// contextKey is the type for context keys
type contextKey string

// Context keys for request-scoped values
const (
	TenantIDKey contextKey = "tenant-id"
	UserIDKey   contextKey = "user_id"
)

// GetTenantID extracts tenant ID from context
func GetTenantID(ctx context.Context) string {
	if tenantID, ok := ctx.Value(TenantIDKey).(string); ok {
		return tenantID
	}
	return "default"
}

// WithTenantID adds tenant ID to context
func WithTenantID(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, TenantIDKey, tenantID)
}
