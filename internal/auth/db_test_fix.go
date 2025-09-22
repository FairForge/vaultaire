//go:build integration
// +build integration

package auth

import (
	"context"
	"database/sql"
	"testing"

	"github.com/FairForge/vaultaire/internal/database"
	"go.uber.org/zap"
)

func setupTestDB(t *testing.T) *sql.DB {
	// Use your local PostgreSQL setup
	cfg := database.Config{
		Host:     "localhost",
		Port:     5432,
		Database: "vaultaire",
		User:     "viera",
		Password: "",
		SSLMode:  "disable",
	}

	logger := zap.NewNop()
	postgres, err := database.NewPostgres(cfg, logger)
	if err != nil {
		t.Skip("PostgreSQL not available:", err)
	}

	return postgres.DB()
}
