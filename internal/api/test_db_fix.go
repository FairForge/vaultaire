package api

import (
	"database/sql"
	"testing"

	"github.com/FairForge/vaultaire/internal/database"
	_ "github.com/lib/pq"
	"go.uber.org/zap"
)

func setupTestDBFixed(t *testing.T) *sql.DB {
	cfg := database.GetTestConfig()

	postgres, err := database.NewPostgres(cfg, zap.NewNop())
	if err != nil {
		t.Skip("PostgreSQL not available:", err)
		return nil
	}

	return postgres.DB()
}
