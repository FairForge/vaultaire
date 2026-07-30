// internal/common/context.go
package common

import (
	"context"
	"sync"
)

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

// backendNoteKey holds a *BackendNote in request context.
const backendNoteKey contextKey = "backend-note"

// BackendNote is a mutable request-scoped slot the engine fills with the name
// of the storage backend that actually served the request. The API layer
// installs it before dispatch and reads it after, so per-backend bandwidth can
// be attributed without threading a return value through every handler.
type BackendNote struct {
	mu   sync.Mutex
	name string
}

// WithBackendNote installs a fresh BackendNote holder in the context.
func WithBackendNote(ctx context.Context) (context.Context, *BackendNote) {
	n := &BackendNote{}
	return context.WithValue(ctx, backendNoteKey, n), n
}

// SetBackendUsed records the backend that served this request. Last writer
// wins — under failover the final successful driver is the one that counts.
// No-op when no holder is installed (background jobs, tests).
func SetBackendUsed(ctx context.Context, backend string) {
	n, ok := ctx.Value(backendNoteKey).(*BackendNote)
	if !ok || n == nil {
		return
	}
	n.mu.Lock()
	n.name = backend
	n.mu.Unlock()
}

// BackendUsed returns the backend recorded for this request, or "".
func BackendUsed(ctx context.Context) string {
	n, ok := ctx.Value(backendNoteKey).(*BackendNote)
	if !ok || n == nil {
		return ""
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.name
}
