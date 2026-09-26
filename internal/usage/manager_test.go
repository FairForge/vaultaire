// internal/usage/manager_test.go
//
// Review R9 (WP-R0-7): these tests run against the MIGRATED schema. The
// migration set is the only schema owner — there is no Go DDL to rebuild
// from, and nothing here drops tables (a DROP on the shared CI database raced
// every other package's quota tests). Each test creates uniquely-named tenants
// and removes them on cleanup; quota_usage_events rows go with them via the
// ON DELETE CASCADE that migration 056 declares.
package usage

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/testutil"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	if err != nil {
		t.Fatalf("Cannot open test database: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newTestTenant registers a uniquely-named quota row and deletes it (and its
// usage events, by cascade) when the test ends.
func newTestTenant(t *testing.T, db *sql.DB, m *QuotaManager, prefix, tier string, limit int64) string {
	t.Helper()
	tenantID := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	require.NoError(t, m.CreateTenant(context.Background(), tenantID, tier, limit))
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, tenantID)
	})
	return tenantID
}

// The migrations own the quota schema: the table must exist with the column
// the Go DDL used to lack (spending_cap_cents, read by billing/metered.go)
// and the cascade FK from quota_usage_events.
func TestQuotaSchema_OwnedByMigrations(t *testing.T) {
	db := setupTestDB(t)

	var hasCap bool
	require.NoError(t, db.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_name = 'tenant_quotas' AND column_name = 'spending_cap_cents')`).Scan(&hasCap))
	assert.True(t, hasCap, "tenant_quotas.spending_cap_cents must exist (migration 043/056)")

	var cascade bool
	require.NoError(t, db.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM pg_constraint
		               WHERE conrelid = 'quota_usage_events'::regclass
		                 AND confrelid = 'tenant_quotas'::regclass AND confdeltype = 'c')`).Scan(&cascade))
	assert.True(t, cascade, "quota_usage_events → tenant_quotas must be ON DELETE CASCADE (migration 056)")
}

func TestQuotaManager_CheckAndUpdateQuota(t *testing.T) {
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	tenantID := newTestTenant(t, db, m, "tenant-manager", "starter", 1000000000) // 1GB

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

func TestUpdateTier_Free(t *testing.T) {
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	tenantID := newTestTenant(t, db, m, "tenant-free", "starter", 1099511627776)

	err := m.UpdateTier(context.Background(), tenantID, "free")
	require.NoError(t, err)

	tier, err := m.GetTier(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, "free", tier)

	used, limit, err := m.GetUsage(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), used)
	assert.Equal(t, int64(5368709120), limit) // 5 GB
}

// R9-03: GetUsageHistory embedded a literal `INTERVAL '%d days'` (never
// formatted) so GET /api/v1/quota/history always failed.
func TestQuotaManager_GetUsageHistory(t *testing.T) {
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	tenantID := newTestTenant(t, db, m, "tenant-history", "starter", 1000000000)

	ok, err := m.CheckAndReserve(context.Background(), tenantID, 4096)
	require.NoError(t, err)
	require.True(t, ok)

	history, err := m.GetUsageHistory(context.Background(), tenantID, 30)
	require.NoError(t, err, "usage history must be a valid query")
	require.Len(t, history, 1, "one day of activity → one row")
	assert.Equal(t, int64(4096), history[0]["peak_usage"])

	// A window that excludes today's event returns nothing, proving the
	// day count is actually bound.
	none, err := m.GetUsageHistory(context.Background(), tenantID, 0)
	require.NoError(t, err)
	assert.Empty(t, none)
}
