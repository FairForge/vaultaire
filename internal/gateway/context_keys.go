package gateway

// ContextKey is a type for context keys to avoid collisions
type ContextKey string

const (
    // ContextKeyTenant is the context key for tenant ID
    ContextKeyTenant ContextKey = "tenant"
    // ContextKeyAPIKey is the context key for API key
    ContextKeyAPIKey ContextKey = "api_key"
)
