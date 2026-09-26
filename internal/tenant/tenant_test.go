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
