// internal/common/context.go
package common

import "context"

// TenantIDKey is the context key for tenant ID
type contextKey string

const TenantIDKey = contextKey("tenant-id")

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
