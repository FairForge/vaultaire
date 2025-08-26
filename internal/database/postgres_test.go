package database

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgres_Connect(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping database tests in short mode")
	}

	db, err := NewPostgres(Config{
		Host:     "localhost",
		Port:     5432,
		Database: "vaultaire_dev",
		User:     "vaultaire",
		Password: "vaultaire_dev",
	})
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()
}

func TestPostgres_CreateTables(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping database tests in short mode")
	}

	db, err := NewPostgres(Config{
		Host:     "localhost",
		Port:     5432,
		Database: "vaultaire_dev",
		User:     "vaultaire",
		Password: "vaultaire_dev",
	})
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()

	if err := db.db.Ping(); err != nil {
		t.Errorf("Failed to ping database: %v", err)
	}
}

func TestPostgres_TenantOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping database tests in short mode")
	}

	db, err := NewPostgres(Config{
		Host:     "localhost",
		Port:     5432,
		Database: "vaultaire_dev",
		User:     "vaultaire",
		Password: "vaultaire_dev",
	})
	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// Create a tenant with unique email using UUID
	tenantID := uuid.New().String()
	tenant := &Tenant{
		ID:        tenantID,
		Name:      "Test Tenant",
		Email:     fmt.Sprintf("test-%s@example.com", tenantID[:8]),
		CreatedAt: time.Now(),
	}

	if err := db.CreateTenant(ctx, tenant); err != nil {
		t.Fatalf("Failed to create tenant: %v", err)
	}

	// Get the tenant back
	retrieved, err := db.GetTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("Failed to get tenant: %v", err)
	}

	if retrieved.Name != tenant.Name {
		t.Errorf("Name mismatch: got %s, want %s", retrieved.Name, tenant.Name)
	}
	if retrieved.Email != tenant.Email {
		t.Errorf("Email mismatch: got %s, want %s", retrieved.Email, tenant.Email)
	}

	// Clean up - delete the test tenant
	_, err = db.db.ExecContext(ctx, "DELETE FROM tenants WHERE id = $1", tenantID)
	if err != nil {
		t.Logf("Warning: failed to clean up test tenant: %v", err)
	}
}

func TestPostgres_ArtifactOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping database tests in short mode")
	}

	t.Skip("Artifact operations not yet implemented")
}
