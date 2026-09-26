package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/usage"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestUsageAPI(t *testing.T) (*Server, *sql.DB) {
	db := setupTestDBFixed(t)
	if db == nil {
		return nil, nil
	}

	// Migrated schema only (R9 / WP-R0-7): no DROP TABLE, no Go DDL. Rows are
	// per-test and removed on cleanup (usage events cascade with the quota row).
	quotaMgr := usage.NewQuotaManager(db)

	// Create test tenant with some usage
	seed := uniqueQuotaTenant(t, db, "test-usage-seed")
	require.NoError(t, quotaMgr.CreateTenant(context.Background(), seed, "starter", 1073741824)) // 1GB
	_, err := quotaMgr.CheckAndReserve(context.Background(), seed, 524288000)
	require.NoError(t, err)

	server := &Server{
		quotaManager: quotaMgr,
	}

	return server, db
}

func TestUsageAPI_GetUsageStats(t *testing.T) {
	server, db := setupTestUsageAPI(t)
	if db == nil {
		return // Test was skipped
	}
	defer func() { _ = db.Close() }()

	// Use unique tenant ID
	tenantID := uniqueQuotaTenant(t, db, "test-tenant")

	// Create tenant with proper quota
	err := server.quotaManager.CreateTenant(context.Background(), tenantID, "starter", 1073741824) // 1GB
	require.NoError(t, err)

	// Reserve some storage
	ok, err := server.quotaManager.CheckAndReserve(context.Background(), tenantID, 524288000) // 500MB
	require.NoError(t, err)
	assert.True(t, ok)

	// Get usage stats - Add tenantID to context
	req := httptest.NewRequest("GET", "/api/v1/usage/stats?tenant_id="+tenantID, nil)
	ctx := context.WithValue(req.Context(), common.TenantIDKey, tenantID)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	server.handleGetUsageStats(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUsageAPI_GetUsageAlerts(t *testing.T) {
	server, db := setupTestUsageAPI(t)
	if db == nil {
		return // Test was skipped
	}
	defer func() { _ = db.Close() }()

	// Use unique tenant ID
	tenantID := uniqueQuotaTenant(t, db, "test-alert-tenant")

	// Create tenant near limit
	err := server.quotaManager.CreateTenant(context.Background(), tenantID, "starter", 100000) // 100KB limit
	require.NoError(t, err)

	// Use 95KB
	ok, err := server.quotaManager.CheckAndReserve(context.Background(), tenantID, 95000)
	require.NoError(t, err)
	assert.True(t, ok)

	// Get usage alerts - Add tenantID to context
	req := httptest.NewRequest("GET", "/api/v1/usage/alerts?tenant_id="+tenantID, nil)
	ctx := context.WithValue(req.Context(), common.TenantIDKey, tenantID)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	server.handleGetUsageAlerts(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// uniqueQuotaTenant returns a tenant ID that cannot collide with any other
// test or package sharing the database, and deletes its quota row (and, by
// cascade, its usage events) when the test ends.
func uniqueQuotaTenant(t *testing.T, db *sql.DB, prefix string) string {
	t.Helper()
	id := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, id) })
	return id
}
