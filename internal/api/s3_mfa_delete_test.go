package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

type mfaDeleteFixture struct {
	server   *Server
	adapter  *S3ToEngine
	db       *sql.DB
	eng      *engine.CoreEngine
	authSvc  *auth.AuthService
	mfaSvc   *auth.MFAService
	tenantID string
	userID   string
	tenant   *tenant.Tenant
	tempDir  string
	bucket   string
}

func setupMFADeleteFixture(t *testing.T) *mfaDeleteFixture {
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

	tempDir, err := os.MkdirTemp("", "vaultaire-mfa-delete-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	eng := engine.NewEngine(nil, logger, nil)
	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	authSvc := auth.NewAuthService(nil, nil)
	mfaSvc := auth.NewMFAService("vaultaire-test")

	bucket := "mfa-bucket"
	email := fmt.Sprintf("mfa-%s-%d@test.local", t.Name(), os.Getpid())

	user, tn, _, err := authSvc.CreateUserWithTenant(context.TODO(), email, "testpass123", "MFA Test Co")
	require.NoError(t, err)

	tenantID := tn.ID
	userID := user.ID

	_, err = db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING
	`, tenantID, "MFA Test", email, "AK-"+tenantID, "SK-"+tenantID)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO buckets (tenant_id, name, visibility, object_lock_enabled, mfa_delete_enabled,
			default_retention_mode, default_retention_days)
		VALUES ($1, $2, 'private', TRUE, FALSE, '', 0)
		ON CONFLICT (tenant_id, name) DO UPDATE SET
			object_lock_enabled = TRUE, mfa_delete_enabled = FALSE,
			default_retention_mode = '', default_retention_days = 0
	`, tenantID, bucket)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM object_locks WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM object_head_cache WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM buckets WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenants WHERE id = $1", tenantID)
	})

	tnCtx := &tenant.Tenant{
		ID:        tenantID,
		Namespace: "tenant/" + tenantID + "/",
	}

	container := tnCtx.NamespaceContainer(bucket)
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, container), 0755))

	srv := &Server{
		logger:     logger,
		router:     chi.NewRouter(),
		engine:     eng,
		db:         db,
		auth:       authSvc,
		mfaService: mfaSvc,
		testMode:   true,
	}

	return &mfaDeleteFixture{
		server:   srv,
		adapter:  NewS3ToEngine(eng, db, logger),
		db:       db,
		eng:      eng,
		authSvc:  authSvc,
		mfaSvc:   mfaSvc,
		tenantID: tenantID,
		userID:   userID,
		tenant:   tnCtx,
		tempDir:  tempDir,
		bucket:   bucket,
	}
}

func (f *mfaDeleteFixture) putObject(t *testing.T, key, content string) {
	t.Helper()
	req := httptest.NewRequest("PUT", "/"+f.bucket+"/"+key, bytes.NewReader([]byte(content)))
	req.Header.Set("Content-Type", "text/plain")
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, f.bucket, key)
	require.Equal(t, http.StatusOK, w.Code, "PUT should succeed")
}

func (f *mfaDeleteFixture) enableMFADelete(t *testing.T) {
	t.Helper()
	_, err := f.db.Exec(
		`UPDATE buckets SET mfa_delete_enabled = TRUE WHERE tenant_id = $1 AND name = $2`,
		f.tenantID, f.bucket)
	require.NoError(t, err)
}

func (f *mfaDeleteFixture) enableUserMFA(t *testing.T) {
	t.Helper()
	err := f.authSvc.EnableMFA(context.TODO(), f.userID, "JBSWY3DPEHPK3PXP", []string{"backup1"})
	require.NoError(t, err)
}

func TestMFADelete_NotEnabled_AllowsDelete(t *testing.T) {
	f := setupMFADeleteFixture(t)

	key := "allowed.txt"
	f.putObject(t, key, "data to delete")

	req := httptest.NewRequest("DELETE", "/"+f.bucket+"/"+key, nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	s3Req := &S3Request{Bucket: f.bucket, Object: key, TenantID: f.tenantID}

	w := httptest.NewRecorder()
	f.server.handleDeleteObject(w, req, s3Req)
	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestMFADelete_Enabled_MissingHeader_Denied(t *testing.T) {
	f := setupMFADeleteFixture(t)
	f.enableMFADelete(t)
	f.enableUserMFA(t)

	key := "locked.txt"
	f.putObject(t, key, "protected data")

	req := httptest.NewRequest("DELETE", "/"+f.bucket+"/"+key, nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	s3Req := &S3Request{Bucket: f.bucket, Object: key, TenantID: f.tenantID}

	w := httptest.NewRecorder()
	f.server.handleDeleteObject(w, req, s3Req)
	assert.Equal(t, http.StatusForbidden, w.Code)

	var errResp S3Error
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &errResp))
	assert.Equal(t, "AccessDenied", errResp.Code)
}

func TestMFADelete_Enabled_ValidCode_Succeeds(t *testing.T) {
	f := setupMFADeleteFixture(t)
	f.enableMFADelete(t)
	f.enableUserMFA(t)

	key := "mfa-ok.txt"
	f.putObject(t, key, "data with MFA")

	req := httptest.NewRequest("DELETE", "/"+f.bucket+"/"+key, nil)
	req.Header.Set("x-amz-mfa", "arn:aws:iam::serial 123456")
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	s3Req := &S3Request{Bucket: f.bucket, Object: key, TenantID: f.tenantID}

	w := httptest.NewRecorder()
	f.server.handleDeleteObject(w, req, s3Req)
	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestMFADelete_Enabled_InvalidCode_Denied(t *testing.T) {
	f := setupMFADeleteFixture(t)
	f.enableMFADelete(t)
	f.enableUserMFA(t)

	key := "bad-code.txt"
	f.putObject(t, key, "protected data")

	req := httptest.NewRequest("DELETE", "/"+f.bucket+"/"+key, nil)
	req.Header.Set("x-amz-mfa", "arn:aws:iam::serial 999999")
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	s3Req := &S3Request{Bucket: f.bucket, Object: key, TenantID: f.tenantID}

	w := httptest.NewRecorder()
	f.server.handleDeleteObject(w, req, s3Req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestMFADelete_EnableRequiresObjectLock(t *testing.T) {
	f := setupMFADeleteFixture(t)
	f.enableUserMFA(t)

	noLockBucket := "no-lock-bucket"
	_, err := f.db.Exec(`
		INSERT INTO buckets (tenant_id, name, visibility, object_lock_enabled, mfa_delete_enabled,
			default_retention_mode, default_retention_days)
		VALUES ($1, $2, 'private', FALSE, FALSE, '', 0)
		ON CONFLICT (tenant_id, name) DO UPDATE SET object_lock_enabled = FALSE, mfa_delete_enabled = FALSE
	`, f.tenantID, noLockBucket)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec("DELETE FROM buckets WHERE tenant_id = $1 AND name = $2", f.tenantID, noLockBucket)
	})

	body := `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
		<Status>Enabled</Status>
		<MfaDelete>Enabled</MfaDelete>
	</VersioningConfiguration>`

	req := httptest.NewRequest("PUT", "/"+noLockBucket+"?versioning", bytes.NewReader([]byte(body)))
	req.Header.Set("x-amz-mfa", "arn:aws:iam::serial 123456")
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	s3Req := &S3Request{Bucket: noLockBucket, TenantID: f.tenantID}

	w := httptest.NewRecorder()
	f.server.handlePutBucketVersioning(w, req, s3Req)
	assert.Equal(t, http.StatusConflict, w.Code)

	var errResp S3Error
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &errResp))
	assert.Equal(t, "InvalidBucketState", errResp.Code)
}

func TestMFADelete_PutVersioning_SuspendRequiresMFA(t *testing.T) {
	f := setupMFADeleteFixture(t)
	f.enableMFADelete(t)
	f.enableUserMFA(t)

	_, err := f.db.Exec(
		`UPDATE buckets SET versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = $2`,
		f.tenantID, f.bucket)
	require.NoError(t, err)

	body := `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
		<Status>Suspended</Status>
	</VersioningConfiguration>`

	req := httptest.NewRequest("PUT", "/"+f.bucket+"?versioning", bytes.NewReader([]byte(body)))
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	s3Req := &S3Request{Bucket: f.bucket, TenantID: f.tenantID}

	w := httptest.NewRecorder()
	f.server.handlePutBucketVersioning(w, req, s3Req)
	assert.Equal(t, http.StatusForbidden, w.Code, "suspend without MFA should be denied")

	req = httptest.NewRequest("PUT", "/"+f.bucket+"?versioning", bytes.NewReader([]byte(body)))
	req.Header.Set("x-amz-mfa", "arn:aws:iam::serial 123456")
	ctx = tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w = httptest.NewRecorder()
	f.server.handlePutBucketVersioning(w, req, s3Req)
	assert.Equal(t, http.StatusOK, w.Code, "suspend with valid MFA should succeed")
}

func TestMFADelete_GetVersioning_ReturnsMfaDeleteStatus(t *testing.T) {
	f := setupMFADeleteFixture(t)

	s3Req := &S3Request{Bucket: f.bucket, TenantID: f.tenantID}

	// Before enabling — MfaDelete should be absent
	req := httptest.NewRequest("GET", "/"+f.bucket+"?versioning", nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.handleGetBucketVersioning(w, req, s3Req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp VersioningConfiguration
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.MfaDelete)

	// Enable MFA Delete
	f.enableMFADelete(t)

	req = httptest.NewRequest("GET", "/"+f.bucket+"?versioning", nil)
	ctx = tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w = httptest.NewRecorder()
	f.server.handleGetBucketVersioning(w, req, s3Req)
	assert.Equal(t, http.StatusOK, w.Code)

	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "Enabled", resp.MfaDelete)
}
