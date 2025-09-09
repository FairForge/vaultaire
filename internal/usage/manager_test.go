// internal/usage/manager_test.go
package usage

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/database"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestDB(t *testing.T) *sql.DB {
	dsn := database.GetTestDSN()

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("Cannot open test database: %v", err)
	}

	// Clean slate - add report tables
	_, _ = db.Exec("DROP TABLE IF EXISTS report_schedules")
	_, _ = db.Exec("DROP TABLE IF EXISTS usage_daily_snapshots")
	_, _ = db.Exec("DROP TABLE IF EXISTS usage_reports")
	_, _ = db.Exec("DROP TABLE IF EXISTS upgrade_suggestions")
	_, _ = db.Exec("DROP TABLE IF EXISTS upgrade_triggers")
	_, _ = db.Exec("DROP TABLE IF EXISTS grace_periods")
	_, _ = db.Exec("DROP TABLE IF EXISTS quota_usage_events")
	_, _ = db.Exec("DROP TABLE IF EXISTS tenant_quotas")

	return db
}

func TestQuotaManager_InitializeSchema(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	m := NewQuotaManager(db)

	err := m.InitializeSchema(context.Background())

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

	// Use unique tenant ID to avoid conflicts
	tenantID := "tenant-manager-" + time.Now().Format("20060102150405")
	require.NoError(t, m.CreateTenant(context.Background(), tenantID, "starter", 1000000000)) // 1GB

	t.Run("within quota", func(t *testing.T) {
		allowed, err := m.CheckAndReserve(context.Background(), tenantID, 500000000) // 500MB
		require.NoError(t, err)
		assert.True(t, allowed)
	})

	t.Run("exceeds quota", func(t *testing.T) {
		allowed, err := m.CheckAndReserve(context.Background(), tenantID, 600000000) // 600MB more
		require.NoError(t, err)
		assert.False(t, allowed)
	})
}
