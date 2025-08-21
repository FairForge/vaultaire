package database

import (
	"context"
	"testing"
	"time"
)

func TestPostgres_Connect(t *testing.T) {
	// Skip in CI for now
	if testing.Short() {
		t.Skip("Skipping database tests in short mode")
	}

	db, err := NewPostgres(Config{
		Host:     "localhost",
		Port:     5432,
		Database: "vaultaire_test",
		User:     "vaultaire",
		Password: "vaultaire",
	})

	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}

	defer db.Close()

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.Ping(ctx); err != nil {
		t.Errorf("Failed to ping database: %v", err)
	}
}

func TestPostgres_CreateTables(t *testing.T) {
	// Skip in CI for now
	if testing.Short() {
		t.Skip("Skipping database tests in short mode")
	}

	db, err := NewPostgres(Config{
		Host:     "localhost",
		Port:     5432,
		Database: "vaultaire_test",
		User:     "vaultaire",
		Password: "vaultaire",
	})

	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}

	defer db.Close()

	// Create tables
	if err := db.CreateTables(context.Background()); err != nil {
		t.Errorf("Failed to create tables: %v", err)
	}
}

func TestPostgres_TenantOperations(t *testing.T) {
	// Skip in CI for now
	if testing.Short() {
		t.Skip("Skipping database tests in short mode")
	}

	db, err := NewPostgres(Config{
		Host:     "localhost",
		Port:     5432,
		Database: "vaultaire_test",
		User:     "vaultaire",
		Password: "vaultaire",
	})

	if err != nil {
		t.Fatalf("Failed to create database: %v", err)
	}

	defer db.Close()

	ctx := context.Background()

	// Create a tenant
	tenant := &Tenant{
		ID:        "test-tenant-1",
		Name:      "Test Tenant",
		CreatedAt: time.Now(),
	}

	if err := db.CreateTenant(ctx, tenant); err != nil {
		t.Errorf("Failed to create tenant: %v", err)
	}

	// Get the tenant
	retrieved, err := db.GetTenant(ctx, tenant.ID)
	if err != nil {
		t.Errorf("Failed to get tenant: %v", err)
	}

	if retrieved.Name != tenant.Name {
		t.Errorf("Expected name %s, got %s", tenant.Name, retrieved.Name)
	}
}
