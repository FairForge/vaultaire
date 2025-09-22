package api

import (
	"database/sql"
	"os"
	"testing"

	"github.com/FairForge/vaultaire/internal/database"
	"go.uber.org/zap"
)

func setupTestDB(t *testing.T) *sql.DB {
	// Use environment variable or default to local setup
	user := os.Getenv("DB_USER")
	if user == "" {
		user = "viera" // Your local user
	}

	cfg := database.Config{
		Host:     "localhost",
		Port:     5432,
		Database: "vaultaire",
		User:     user,
		Password: "",
		SSLMode:  "disable",
	}

	postgres, err := database.NewPostgres(cfg, zap.NewNop())
	if err != nil {
		t.Skip("PostgreSQL not available:", err)
		return nil
	}

	return postgres.DB()
}
