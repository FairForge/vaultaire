package api

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type quotaTestManager struct {
	allowReserve bool
	tier         string
}

func (m *quotaTestManager) GetUsage(_ context.Context, _ string) (int64, int64, error) {
	return 0, 0, nil
}
func (m *quotaTestManager) CheckAndReserve(_ context.Context, _ string, _ int64) (bool, error) {
	return m.allowReserve, nil
}
func (m *quotaTestManager) ReleaseQuota(_ context.Context, _ string, _ int64) error    { return nil }
func (m *quotaTestManager) CreateTenant(_ context.Context, _, _ string, _ int64) error { return nil }
func (m *quotaTestManager) UpdateQuota(_ context.Context, _ string, _ int64) error     { return nil }
func (m *quotaTestManager) ListQuotas(_ context.Context) ([]map[string]interface{}, error) {
	return nil, nil
}
func (m *quotaTestManager) DeleteQuota(_ context.Context, _ string) error { return nil }
func (m *quotaTestManager) GetTier(_ context.Context, _ string) (string, error) {
	return m.tier, nil
}
func (m *quotaTestManager) UpdateTier(_ context.Context, _, _ string) error { return nil }
func (m *quotaTestManager) GetUsageHistory(_ context.Context, _ string, _ int) ([]map[string]interface{}, error) {
	return nil, nil
}

func TestHandlePutObject_QuotaExceeded(t *testing.T) {
	logger := zap.NewNop()
	eng := engine.NewEngine(nil, logger, nil)

	tempDir, err := os.MkdirTemp("", "vaultaire-quota-test-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	qm := &quotaTestManager{allowReserve: false, tier: "free"}

	server := &Server{
		logger:       logger,
		router:       chi.NewRouter(),
		engine:       eng,
		quotaManager: qm,
		testMode:     true,
	}

	testTenant := &tenant.Tenant{
		ID:        "test-tenant",
		Namespace: "tenant/test-tenant/",
		APIKey:    "test-key",
	}

	body := []byte("hello world — this should be rejected")
	putReq := httptest.NewRequest("PUT", "/test-bucket/test-object.txt",
		bytes.NewReader(body))
	putReq.ContentLength = int64(len(body))
	ctx := tenant.WithTenant(putReq.Context(), testTenant)
	putReq = putReq.WithContext(ctx)

	s3Req := &S3Request{
		Bucket:   "test-bucket",
		Object:   "test-object.txt",
		TenantID: "test-tenant",
	}

	w := httptest.NewRecorder()
	server.handlePutObject(w, putReq, s3Req)

	assert.Equal(t, 403, w.Code)
	assert.Contains(t, w.Body.String(), "QuotaExceeded")
	assert.Contains(t, w.Body.String(), "Upgrade")
}

func TestHandlePutObject_QuotaAllowed(t *testing.T) {
	logger := zap.NewNop()
	eng := engine.NewEngine(nil, logger, nil)

	tempDir, err := os.MkdirTemp("", "vaultaire-quota-test-*")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	namespacedBucket := "test-tenant_test-bucket"
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, namespacedBucket), 0755))

	qm := &quotaTestManager{allowReserve: true, tier: "starter"}

	server := &Server{
		logger:       logger,
		router:       chi.NewRouter(),
		engine:       eng,
		quotaManager: qm,
		testMode:     true,
	}

	testTenant := &tenant.Tenant{
		ID:        "test-tenant",
		Namespace: "tenant/test-tenant/",
		APIKey:    "test-key",
	}

	body := []byte("allowed content")
	putReq := httptest.NewRequest("PUT", "/test-bucket/test-object.txt",
		bytes.NewReader(body))
	putReq.ContentLength = int64(len(body))
	ctx := tenant.WithTenant(putReq.Context(), testTenant)
	putReq = putReq.WithContext(ctx)

	s3Req := &S3Request{
		Bucket:   "test-bucket",
		Object:   "test-object.txt",
		TenantID: "test-tenant",
	}

	w := httptest.NewRecorder()
	server.handlePutObject(w, putReq, s3Req)

	assert.Equal(t, 200, w.Code)
}
