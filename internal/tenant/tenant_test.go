package tenant

import (
    "context"
    "testing"
)

func TestTenantIsolation(t *testing.T) {
    // Create two tenants
    tenant1 := &Tenant{
        ID:        "customer-1",
        Namespace: "tenant/customer-1/",
    }
    
    tenant2 := &Tenant{
        ID:        "customer-2", 
        Namespace: "tenant/customer-2/",
    }
    
    // Test namespace isolation
    key := "data.txt"
    ns1 := tenant1.NamespaceKey(key)
    ns2 := tenant2.NamespaceKey(key)
    
    if ns1 == ns2 {
        t.Errorf("Tenants should have different namespaces: %s == %s", ns1, ns2)
    }
    
    // Verify expected namespaces
    expectedNs1 := "tenant/customer-1/data.txt"
    if ns1 != expectedNs1 {
        t.Errorf("Wrong namespace for tenant1: got %s, want %s", ns1, expectedNs1)
    }
    
    // Test context propagation
    ctx := context.Background()
    ctx = WithTenant(ctx, tenant1)
    
    retrieved, err := FromContext(ctx)
    if err != nil {
        t.Fatalf("Failed to retrieve tenant: %v", err)
    }
    
    if retrieved.ID != tenant1.ID {
        t.Errorf("Wrong tenant retrieved: got %s, want %s", retrieved.ID, tenant1.ID)
    }
}

func TestMemoryStore(t *testing.T) {
    store := NewMemoryStore()
    
    // Add a test tenant
    tenant := &Tenant{
        ID:        "test-tenant",
        Namespace: "tenant/test-tenant/",
        APIKey:    "test-key-123",
    }
    
    store.AddTenant("test-key-123", tenant)
    
    // Test lookup by API key
    ctx := context.Background()
    found, err := store.GetByAPIKey(ctx, "test-key-123")
    if err != nil {
        t.Fatalf("Failed to find tenant by API key: %v", err)
    }
    
    if found.ID != tenant.ID {
        t.Errorf("Wrong tenant found: got %s, want %s", found.ID, tenant.ID)
    }
    
    // Test lookup by ID
    found, err = store.GetByID(ctx, "test-tenant")
    if err != nil {
        t.Fatalf("Failed to find tenant by ID: %v", err)
    }
    
    if found.ID != tenant.ID {
        t.Errorf("Wrong tenant found: got %s, want %s", found.ID, tenant.ID)
    }
    
    // Test missing tenant
    _, err = store.GetByAPIKey(ctx, "nonexistent")
    if err != ErrNoTenant {
        t.Errorf("Expected ErrNoTenant, got: %v", err)
    }
}
