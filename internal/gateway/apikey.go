// internal/gateway/apikey.go
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

// APIKey represents an API key with metadata
type APIKey struct {
	Key         string
	TenantID    string
	Permissions []string
	CreatedAt   time.Time
	LastUsed    time.Time
}

// APIKeyManager handles API key validation and permissions
type APIKeyManager struct {
	mu   sync.RWMutex
	keys map[string]*APIKey
}

// NewAPIKeyManager creates a new API key manager
func NewAPIKeyManager() *APIKeyManager {
	return &APIKeyManager{
		keys: make(map[string]*APIKey),
	}
}

// GenerateKey creates a new API key for a tenant
func (m *APIKeyManager) GenerateKey(tenantID string, permissions []string) string {
	// Generate random key
	bytes := make([]byte, 32)
	_, _ = rand.Read(bytes)
	key := "vlt_" + hex.EncodeToString(bytes)

	// Store key
	m.mu.Lock()
	m.keys[key] = &APIKey{
		Key:         key,
		TenantID:    tenantID,
		Permissions: permissions,
		CreatedAt:   time.Now(),
	}
	m.mu.Unlock()

	return key
}

// ValidateKey checks if a key is valid and returns the tenant ID
func (m *APIKeyManager) ValidateKey(key string) (bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if apiKey, exists := m.keys[key]; exists {
		apiKey.LastUsed = time.Now()
		return true, apiKey.TenantID
	}

	return false, ""
}

// HasPermission checks if a key has a specific permission
func (m *APIKeyManager) HasPermission(key, permission string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	apiKey, exists := m.keys[key]
	if !exists {
		return false
	}

	for _, perm := range apiKey.Permissions {
		if perm == permission {
			return true
		}
	}

	return false
}

// Middleware validates API keys from request headers
func (m *APIKeyManager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check for API key in headers
		key := r.Header.Get("X-API-Key")
		if key == "" {
			// Also check Authorization header
			auth := r.Header.Get("Authorization")
			if strings.HasPrefix(auth, "Bearer ") {
				key = strings.TrimPrefix(auth, "Bearer ")
			}
		}

		if key == "" {
			http.Error(w, "API key required", http.StatusUnauthorized)
			return
		}

		// Validate key
		valid, tenantID := m.ValidateKey(key)
		if !valid {
			http.Error(w, "Invalid API key", http.StatusUnauthorized)
			return
		}

		// Add tenant to context
		ctx := context.WithValue(r.Context(), ContextKeyTenant, tenantID)
		ctx = context.WithValue(ctx, ContextKeyAPIKey, key)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RevokeKey removes an API key
func (m *APIKeyManager) RevokeKey(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.keys[key]; exists {
		delete(m.keys, key)
		return true
	}

	return false
}
