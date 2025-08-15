package tenant

import (
    "context"
    "errors"
    "fmt"
)

// contextKey is a custom type to prevent context key collisions
type contextKey string

const (
    // ContextKeyTenant is the key for storing tenant in context
    ContextKeyTenant contextKey = "tenant"
)

// Tenant represents a customer/user of the platform
type Tenant struct {
    ID        string  // Unique identifier (from auth)
    Namespace string  // Data isolation prefix: "tenant/{id}/"
    APIKey    string  // The API key used (for logging)
    
    // Quotas and limits
    StorageQuota int64  // Max storage in bytes (0 = unlimited for MVP)
    BandwidthQuota int64  // Max bandwidth per month
    RequestsPerSecond int  // Rate limit
    
    // Billing info
    Plan    string  // "free", "starter", "pro"
    Status  string  // "active", "suspended", "trial"
}

// Errors
var (
    ErrNoTenant = errors.New("no tenant in context")
    ErrQuotaExceeded = errors.New("storage quota exceeded")
    ErrRateLimitExceeded = errors.New("rate limit exceeded")
)

// WithTenant adds tenant to context
func WithTenant(ctx context.Context, t *Tenant) context.Context {
    return context.WithValue(ctx, ContextKeyTenant, t)
}

// FromContext extracts tenant from context
func FromContext(ctx context.Context) (*Tenant, error) {
    tenant, ok := ctx.Value(ContextKeyTenant).(*Tenant)
    if !ok || tenant == nil {
        return nil, ErrNoTenant
    }
    return tenant, nil
}

// MustFromContext extracts tenant or panics (use in handlers after middleware)
func MustFromContext(ctx context.Context) *Tenant {
    tenant, err := FromContext(ctx)
    if err != nil {
        panic("tenant middleware not applied: " + err.Error())
    }
    return tenant
}

// NamespaceKey prefixes a key with the tenant's namespace
func (t *Tenant) NamespaceKey(key string) string {
    if t.Namespace == "" {
        // Generate namespace from ID if not set
        t.Namespace = fmt.Sprintf("tenant/%s/", t.ID)
    }
    return t.Namespace + key
}

// NamespaceContainer prefixes container name with tenant ID
func (t *Tenant) NamespaceContainer(container string) string {
    // For MVP, just prefix with tenant ID
    // Later can do more complex mapping
    return fmt.Sprintf("%s_%s", t.ID, container)
}
