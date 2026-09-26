package database

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

func TestPostgres_Connect(t *testing.T) {
	cfg := testConfig()
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
	cfg := testConfig()
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
	cfg := testConfig()
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
		AccessKey: "AK-" + tenantID[:8],
		SecretKey: "SK-" + tenantID[:8],
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

// testConfig mirrors testutil.DBConfig; duplicated here because testutil imports
// this package and an in-package _test.go cannot import it back.
func testConfig() Config {
	env := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return def
	}
	return Config{
		Host:     env("TEST_DB_HOST", "localhost"),
		Port:     5432,
		Database: env("TEST_DB_NAME", "vaultaire"),
		User:     env("TEST_DB_USER", "viera"),
		Password: env("TEST_DB_PASSWORD", ""),
		SSLMode:  "disable",
	}
}
