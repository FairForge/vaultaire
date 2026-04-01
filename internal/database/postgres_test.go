package database

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

func TestPostgres_Connect(t *testing.T) {
	cfg := GetTestConfig()
	logger := zap.NewNop()

	db, err := NewPostgres(cfg, logger)
	if err != nil {
		t.Skip("PostgreSQL not available:", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("failed to close database: %v", err)
		}
	}()
}

func TestPostgres_CreateTables(t *testing.T) {
	cfg := GetTestConfig()
	logger := zap.NewNop()

	db, err := NewPostgres(cfg, logger)
	if err != nil {
		t.Skip("PostgreSQL not available:", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("failed to close database: %v", err)
		}
	}()
}

func TestPostgres_TenantOperations(t *testing.T) {
	cfg := GetTestConfig()
	logger := zap.NewNop()

	db, err := NewPostgres(cfg, logger)
	if err != nil {
		t.Skip("PostgreSQL not available:", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("failed to close database: %v", err)
		}
	}()

	ctx := context.Background()

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

	_, err = db.db.ExecContext(ctx, "DELETE FROM tenants WHERE id = $1", tenantID)
	if err != nil {
		t.Logf("Warning: failed to clean up test tenant: %v", err)
	}
}

func TestPostgres_ArtifactOperations(t *testing.T) {
	t.Skip("Artifact operations not yet implemented")
}
