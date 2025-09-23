package api

import (
	"database/sql"
	"testing"

	"github.com/FairForge/vaultaire/internal/database"
	_ "github.com/lib/pq"
	"go.uber.org/zap"
)

func setupTestDBFixed(t *testing.T) *sql.DB {
	// Explicitly use the correct database
	cfg := database.Config{
		Host:     "localhost",
		Port:     5432,
		Database: "vaultaire", // EXPLICIT: always use vaultaire
		User:     "viera",
		Password: "",
		SSLMode:  "disable",
	}

	t.Logf("Using config: Host=%s, Port=%d, Database=%s, User=%s",
		cfg.Host, cfg.Port, cfg.Database, cfg.User)

	postgres, err := database.NewPostgres(cfg, zap.NewNop())
	if err != nil {
		t.Skip("PostgreSQL not available:", err)
		return nil
	}

	return postgres.DB()
}
