package database

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
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
// this package and an in-package _test.go cannot import it back. Resolution:
// DATABASE_URL (CI) > TEST_DB_* > localhost/viera/vaultaire_test.
func testConfig() Config {
	if raw := os.Getenv("DATABASE_URL"); raw != "" {
		if u, err := url.Parse(raw); err == nil && u.Path != "" {
			port := 5432
			if p := u.Port(); p != "" {
				port, _ = strconv.Atoi(p)
			}
			cfg := Config{Host: u.Hostname(), Port: port, Database: u.Path[1:], SSLMode: u.Query().Get("sslmode")}
			if u.User != nil {
				cfg.User = u.User.Username()
				cfg.Password, _ = u.User.Password()
			}
			if cfg.SSLMode == "" {
				cfg.SSLMode = "disable"
			}
			return cfg
		}
	}
	env := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return def
	}
	return Config{
		Host:     env("TEST_DB_HOST", "localhost"),
		Port:     5432,
		Database: env("TEST_DB_NAME", "vaultaire_test"),
		User:     env("TEST_DB_USER", "viera"),
		Password: env("TEST_DB_PASSWORD", ""),
		SSLMode:  "disable",
	}
}
