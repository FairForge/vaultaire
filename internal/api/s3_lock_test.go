package api

import (
	"bytes"
	"database/sql"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

type lockFixture struct {
	server   *Server
	adapter  *S3ToEngine
	db       *sql.DB
	eng      *engine.CoreEngine
	tenantID string
	tenant   *tenant.Tenant
	tempDir  string
	bucket   string
}

func setupLockFixture(t *testing.T) *lockFixture {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())

	logger := zap.NewNop()

	tempDir, err := os.MkdirTemp("", "vaultaire-lock-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	eng := engine.NewEngine(nil, logger, nil)
	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	tenantID := fmt.Sprintf("lock-%s-%d", t.Name(), os.Getpid())
	bucket := "lock-bucket"
	email := fmt.Sprintf("lock-%s-%d@test.local", t.Name(), os.Getpid())

	_, err = db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING
	`, tenantID, "Lock Test", email, "AK-"+tenantID, "SK-"+tenantID)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO buckets (tenant_id, name, visibility, object_lock_enabled, default_retention_mode, default_retention_days)
		VALUES ($1, $2, 'private', FALSE, '', 0)
		ON CONFLICT (tenant_id, name) DO UPDATE SET
			object_lock_enabled = FALSE, default_retention_mode = '', default_retention_days = 0
	`, tenantID, bucket)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM object_locks WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM buckets WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenants WHERE id = $1", tenantID)
	})

	tn := &tenant.Tenant{
		ID:        tenantID,
		Namespace: "tenant/" + tenantID + "/",
	}

	container := tn.NamespaceContainer(bucket)
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, container), 0755))

	srv := &Server{
		logger:   logger,
		router:   chi.NewRouter(),
		engine:   eng,
		db:       db,
		testMode: true,
	}

	return &lockFixture{
		server:   srv,
		adapter:  NewS3ToEngine(eng, db, logger),
		db:       db,
		eng:      eng,
		tenantID: tenantID,
		tenant:   tn,
		tempDir:  tempDir,
		bucket:   bucket,
	}
}

func (f *lockFixture) putObject(t *testing.T, key, content string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/"+f.bucket+"/"+key, bytes.NewReader([]byte(content)))
	req.Header.Set("Content-Type", "text/plain")
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, f.bucket, key)
	require.Equal(t, http.StatusOK, w.Code, "PUT should succeed")
	return w
}

func (f *lockFixture) deleteObject(t *testing.T, key string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("DELETE", "/"+f.bucket+"/"+key, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandleDelete(w, req, f.bucket, key)
	return w
}

// --- Test: bucket-level object lock configuration ---

func TestObjectLock_PutGetBucketConfig(t *testing.T) {
	f := setupLockFixture(t)

	s3Req := &S3Request{Bucket: f.bucket, TenantID: f.tenantID}

	// GET before enabling — should return empty config
	req := httptest.NewRequest("GET", "/"+f.bucket+"?object-lock", nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.handleGetObjectLockConfiguration(w, req, s3Req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp ObjectLockConfiguration
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.ObjectLockEnabled)
	assert.Nil(t, resp.Rule)

	// PUT to enable with default GOVERNANCE retention of 30 days
	body := `<ObjectLockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
		<ObjectLockEnabled>Enabled</ObjectLockEnabled>
		<Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>30</Days></DefaultRetention></Rule>
	</ObjectLockConfiguration>`
	req = httptest.NewRequest("PUT", "/"+f.bucket+"?object-lock", bytes.NewReader([]byte(body)))
	ctx = tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w = httptest.NewRecorder()
	f.server.handlePutObjectLockConfiguration(w, req, s3Req)
	assert.Equal(t, http.StatusOK, w.Code)

	// GET again — should return enabled config
	req = httptest.NewRequest("GET", "/"+f.bucket+"?object-lock", nil)
	ctx = tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w = httptest.NewRecorder()
	f.server.handleGetObjectLockConfiguration(w, req, s3Req)
	assert.Equal(t, http.StatusOK, w.Code)

	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "Enabled", resp.ObjectLockEnabled)
	require.NotNil(t, resp.Rule)
	require.NotNil(t, resp.Rule.DefaultRetention)
	assert.Equal(t, "GOVERNANCE", resp.Rule.DefaultRetention.Mode)
	assert.Equal(t, 30, resp.Rule.DefaultRetention.Days)
}

// --- Test: per-object retention ---

func TestObjectLock_PutGetRetention(t *testing.T) {
	f := setupLockFixture(t)

	key := "retained.txt"
	f.putObject(t, key, "data under retention")

	s3Req := &S3Request{Bucket: f.bucket, Object: key, TenantID: f.tenantID}

	retainUntil := time.Now().Add(365 * 24 * time.Hour).UTC().Format(time.RFC3339)
	body := fmt.Sprintf(`<Retention xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
		<Mode>GOVERNANCE</Mode>
		<RetainUntilDate>%s</RetainUntilDate>
	</Retention>`, retainUntil)

	req := httptest.NewRequest("PUT", "/"+f.bucket+"/"+key+"?retention", bytes.NewReader([]byte(body)))
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.handlePutObjectRetention(w, req, s3Req)
	assert.Equal(t, http.StatusOK, w.Code)

	// GET retention
	req = httptest.NewRequest("GET", "/"+f.bucket+"/"+key+"?retention", nil)
	ctx = tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w = httptest.NewRecorder()
	f.server.handleGetObjectRetention(w, req, s3Req)
	assert.Equal(t, http.StatusOK, w.Code)

	var ret RetentionConfig
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &ret))
	assert.Equal(t, "GOVERNANCE", ret.Mode)
	assert.NotEmpty(t, ret.RetainUntilDate)
}

// --- Test: per-object legal hold ---

func TestObjectLock_PutGetLegalHold(t *testing.T) {
	f := setupLockFixture(t)

	key := "held.txt"
	f.putObject(t, key, "data under legal hold")

	s3Req := &S3Request{Bucket: f.bucket, Object: key, TenantID: f.tenantID}

	body := `<LegalHold xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>ON</Status></LegalHold>`
	req := httptest.NewRequest("PUT", "/"+f.bucket+"/"+key+"?legal-hold", bytes.NewReader([]byte(body)))
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.handlePutObjectLegalHold(w, req, s3Req)
	assert.Equal(t, http.StatusOK, w.Code)

	// GET legal hold
	req = httptest.NewRequest("GET", "/"+f.bucket+"/"+key+"?legal-hold", nil)
	ctx = tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w = httptest.NewRecorder()
	f.server.handleGetObjectLegalHold(w, req, s3Req)
	assert.Equal(t, http.StatusOK, w.Code)

	var hold LegalHoldConfig
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &hold))
	assert.Equal(t, "ON", hold.Status)
}

// --- Test: COMPLIANCE retention blocks delete ---

func TestObjectLock_ComplianceBlocksDelete(t *testing.T) {
	f := setupLockFixture(t)

	key := "compliance-locked.txt"
	f.putObject(t, key, "compliance data")

	retainUntil := time.Now().Add(365 * 24 * time.Hour)
	_, err := f.db.Exec(`
		INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date)
		VALUES ($1, $2, $3, 'COMPLIANCE', $4)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			retention_mode = 'COMPLIANCE', retain_until_date = $4
	`, f.tenantID, f.bucket, key, retainUntil)
	require.NoError(t, err)

	w := f.deleteObject(t, key, nil)
	assert.Equal(t, http.StatusForbidden, w.Code)

	var s3Err S3Error
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &s3Err))
	assert.Equal(t, ErrObjectLocked, s3Err.Code)
}

// --- Test: GOVERNANCE bypass ---

func TestObjectLock_GovernanceBypass(t *testing.T) {
	f := setupLockFixture(t)

	key := "governance-locked.txt"
	f.putObject(t, key, "governance data")

	retainUntil := time.Now().Add(365 * 24 * time.Hour)
	_, err := f.db.Exec(`
		INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date)
		VALUES ($1, $2, $3, 'GOVERNANCE', $4)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			retention_mode = 'GOVERNANCE', retain_until_date = $4
	`, f.tenantID, f.bucket, key, retainUntil)
	require.NoError(t, err)

	// Without bypass header — should be blocked
	w := f.deleteObject(t, key, nil)
	assert.Equal(t, http.StatusForbidden, w.Code)

	// With bypass header — should succeed
	w = f.deleteObject(t, key, map[string]string{
		"x-amz-bypass-governance-retention": "true",
	})
	assert.Equal(t, http.StatusNoContent, w.Code)
}

// --- Test: legal hold blocks delete ---

func TestObjectLock_LegalHoldBlocksDelete(t *testing.T) {
	f := setupLockFixture(t)

	key := "legal-held.txt"
	f.putObject(t, key, "legal hold data")

	_, err := f.db.Exec(`
		INSERT INTO object_locks (tenant_id, bucket, object_key, legal_hold)
		VALUES ($1, $2, $3, TRUE)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET legal_hold = TRUE
	`, f.tenantID, f.bucket, key)
	require.NoError(t, err)

	w := f.deleteObject(t, key, nil)
	assert.Equal(t, http.StatusForbidden, w.Code)

	var s3Err S3Error
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &s3Err))
	assert.Equal(t, ErrObjectLocked, s3Err.Code)

	// Even with governance bypass, legal hold blocks
	w = f.deleteObject(t, key, map[string]string{
		"x-amz-bypass-governance-retention": "true",
	})
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- Test: expired retention allows delete ---

func TestObjectLock_ExpiredRetentionAllowsDelete(t *testing.T) {
	f := setupLockFixture(t)

	key := "expired-lock.txt"
	f.putObject(t, key, "expired data")

	pastDate := time.Now().Add(-24 * time.Hour)
	_, err := f.db.Exec(`
		INSERT INTO object_locks (tenant_id, bucket, object_key, retention_mode, retain_until_date)
		VALUES ($1, $2, $3, 'COMPLIANCE', $4)
		ON CONFLICT (tenant_id, bucket, object_key) DO UPDATE SET
			retention_mode = 'COMPLIANCE', retain_until_date = $4
	`, f.tenantID, f.bucket, key, pastDate)
	require.NoError(t, err)

	w := f.deleteObject(t, key, nil)
	assert.Equal(t, http.StatusNoContent, w.Code)
}

// --- Test: default retention applied on PUT ---

func TestObjectLock_DefaultRetentionApplied(t *testing.T) {
	f := setupLockFixture(t)

	// Enable object lock with GOVERNANCE default of 30 days
	_, err := f.db.Exec(`
		UPDATE buckets SET object_lock_enabled = TRUE,
			default_retention_mode = 'GOVERNANCE', default_retention_days = 30
		WHERE tenant_id = $1 AND name = $2
	`, f.tenantID, f.bucket)
	require.NoError(t, err)

	key := "auto-retained.txt"
	f.putObject(t, key, "auto-retention data")

	// Verify lock was applied
	var mode string
	var retainUntil time.Time
	err = f.db.QueryRow(`
		SELECT retention_mode, retain_until_date FROM object_locks
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, f.bucket, key).Scan(&mode, &retainUntil)
	require.NoError(t, err)
	assert.Equal(t, "GOVERNANCE", mode)
	assert.True(t, retainUntil.After(time.Now().Add(29*24*time.Hour)),
		"retain_until_date should be ~30 days from now")
}
