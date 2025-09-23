// internal/api/quota_management_test.go
package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/usage"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type testContextKey string

const (
	testAdminKey testContextKey = "is_admin"
)

func TestQuotaManagementAPI_CreateQuota(t *testing.T) {
	db := setupTestDBFixed(t)
	if db == nil {
		return // Test was skipped
	}
	defer func() { _ = db.Close() }()

	server := setupTestQuotaAPI(t, db)

	// Create quota request
	quotaReq := QuotaRequest{
		TenantID:       "tenant-456",
		Plan:           "professional",
		StorageLimit:   10737418240,  // 10GB
		BandwidthLimit: 107374182400, // 100GB
	}

	body, _ := json.Marshal(quotaReq)
	req := httptest.NewRequest("POST", "/api/v1/admin/quotas", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), testAdminKey, true))
	w := httptest.NewRecorder()

	server.handleCreateQuota(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)

	var response QuotaResponse
	err := json.NewDecoder(w.Body).Decode(&response)
	require.NoError(t, err)
	assert.Equal(t, "tenant-456", response.TenantID)
	assert.Equal(t, int64(10737418240), response.StorageLimit)
}

func TestQuotaManagementAPI_UpdateQuota(t *testing.T) {
	db := setupTestDBFixed(t)
	if db == nil {
		return // Test was skipped
	}
	defer func() { _ = db.Close() }()

	server := setupTestQuotaAPI(t, db)

	// First create a quota
	_ = server.quotaManager.(*usage.QuotaManager).CreateTenant(
		context.Background(), "tenant-789", "starter", 1073741824) // 1GB

	// Update request
	updateReq := QuotaUpdateRequest{
		StorageLimit: 5368709120, // 5GB
	}

	body, _ := json.Marshal(updateReq)
	req := httptest.NewRequest("PUT", "/api/v1/admin/quotas/tenant-789", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), testAdminKey, true))
	w := httptest.NewRecorder()

	server.handleUpdateQuota(w, req)

	// Debug: Print the actual error response
	if w.Code != http.StatusOK {
		t.Logf("UpdateQuota failed with status %d: %s", w.Code, w.Body.String())
	}

	assert.Equal(t, http.StatusOK, w.Code)

	// Verify the update
	used, limit, err := server.quotaManager.GetUsage(context.Background(), "tenant-789")
	require.NoError(t, err)
	assert.Equal(t, int64(5368709120), limit)
	assert.Equal(t, int64(0), used)
}

func TestQuotaManagementAPI_ListQuotas(t *testing.T) {
	db := setupTestDBFixed(t)
	if db == nil {
		return // Test was skipped
	}
	defer func() { _ = db.Close() }()

	server := setupTestQuotaAPI(t, db)

	// Create some test quotas
	ctx := context.Background()
	qm := server.quotaManager.(*usage.QuotaManager)
	_ = qm.CreateTenant(ctx, "tenant-1", "starter", 1073741824)
	_ = qm.CreateTenant(ctx, "tenant-2", "professional", 10737418240)

	req := httptest.NewRequest("GET", "/api/v1/admin/quotas", nil)
	req = req.WithContext(context.WithValue(req.Context(), testAdminKey, true))
	w := httptest.NewRecorder()

	server.handleListQuotas(w, req)

	if w.Code != http.StatusOK {
		t.Logf("ListQuotas failed with status %d: %s", w.Code, w.Body.String())
	}

	assert.Equal(t, http.StatusOK, w.Code)

	var quotas []QuotaResponse
	err := json.NewDecoder(w.Body).Decode(&quotas)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(quotas), 2)
}

func setupTestQuotaAPI(t *testing.T, db *sql.DB) *Server {
	// Clean and setup
	_, _ = db.Exec("DROP TABLE IF EXISTS quota_usage_events")
	_, _ = db.Exec("DROP TABLE IF EXISTS tenant_quotas")

	quotaMgr := usage.NewQuotaManager(db)
	require.NoError(t, quotaMgr.InitializeSchema(context.Background()))

	// Create test tenant with some usage
	require.NoError(t, quotaMgr.CreateTenant(context.Background(), "tenant-123", "starter", 1000000000))
	_, err := quotaMgr.CheckAndReserve(context.Background(), "tenant-123", 500000000)
	require.NoError(t, err)

	// Add a no-op logger for testing
	logger := zap.NewNop()

	server := &Server{
		quotaManager: quotaMgr,
		logger:       logger,
	}

	return server
}
