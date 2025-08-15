package tenant

import (
    "context"
    "sync"
)

// Store provides tenant lookup and management
type Store interface {
    GetByAPIKey(ctx context.Context, apiKey string) (*Tenant, error)
    GetByID(ctx context.Context, id string) (*Tenant, error)
}

// MemoryStore is an in-memory tenant store for MVP
type MemoryStore struct {
    mu      sync.RWMutex
    byKey   map[string]*Tenant  // apiKey -> tenant
    byID    map[string]*Tenant  // id -> tenant
}

// NewMemoryStore creates a new in-memory store
func NewMemoryStore() *MemoryStore {
    return &MemoryStore{
        byKey: make(map[string]*Tenant),
        byID:  make(map[string]*Tenant),
    }
}

// AddTenant adds a tenant to the store (for testing/MVP)
func (s *MemoryStore) AddTenant(apiKey string, t *Tenant) {
    s.mu.Lock()
    defer s.mu.Unlock()
    
    s.byKey[apiKey] = t
    s.byID[t.ID] = t
}

// GetByAPIKey looks up tenant by API key
func (s *MemoryStore) GetByAPIKey(ctx context.Context, apiKey string) (*Tenant, error) {
    s.mu.RLock()
    defer s.mu.RUnlock()
    
    tenant, ok := s.byKey[apiKey]
    if !ok {
        return nil, ErrNoTenant
    }
    return tenant, nil
}

// GetByID looks up tenant by ID
func (s *MemoryStore) GetByID(ctx context.Context, id string) (*Tenant, error) {
    s.mu.RLock()
    defer s.mu.RUnlock()
    
    tenant, ok := s.byID[id]
    if !ok {
        return nil, ErrNoTenant
    }
    return tenant, nil
}
