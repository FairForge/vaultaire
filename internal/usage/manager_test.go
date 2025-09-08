// internal/usage/manager_test.go
package usage

import (
	"context"
	"database/sql"
	"testing"

	"github.com/FairForge/vaultaire/internal/database"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestDB(t *testing.T) *sql.DB {
	// Use your existing test helper
	dsn := database.GetTestDSN()

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("Cannot open test database: %v", err)
	}

	// Clean slate
	_, _ = db.Exec("DROP TABLE IF EXISTS quota_usage_events")
	_, _ = db.Exec("DROP TABLE IF EXISTS tenant_quotas")

	return db
}

func TestQuotaManager_InitializeSchema(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	m := NewQuotaManager(db)

	// Act
	err := m.InitializeSchema(context.Background())

	// Assert
	require.NoError(t, err)

	// Verify tables exist
	var exists bool
	err = db.QueryRow("SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'tenant_quotas')").Scan(&exists)
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestQuotaManager_CheckAndUpdateQuota(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	m := NewQuotaManager(db)
	require.NoError(t, m.InitializeSchema(context.Background()))
	require.NoError(t, m.CreateTenant(context.Background(), "tenant-123", "starter", 1000))

	t.Run("within quota", func(t *testing.T) {
		allowed, err := m.CheckAndReserve(context.Background(), "tenant-123", 500)
		require.NoError(t, err)
		assert.True(t, allowed)
	})

	t.Run("exceeds quota", func(t *testing.T) {
		allowed, err := m.CheckAndReserve(context.Background(), "tenant-123", 600)
		require.NoError(t, err)
		assert.False(t, allowed)
	})
}
