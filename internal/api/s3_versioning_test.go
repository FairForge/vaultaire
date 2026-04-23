package api

import (
	"bytes"
	"database/sql"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
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

	_ "github.com/lib/pq"
)

type versioningFixture struct {
	server   *Server
	adapter  *S3ToEngine
	db       *sql.DB
	eng      *engine.CoreEngine
	tenantID string
	tenant   *tenant.Tenant
	tempDir  string
	bucket   string
}

func setupVersioningFixture(t *testing.T) *versioningFixture {
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

	tempDir, err := os.MkdirTemp("", "vaultaire-versioning-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	eng := engine.NewEngine(nil, logger, nil)
	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	tenantID := fmt.Sprintf("ver-%s-%d", t.Name(), os.Getpid())
	bucket := "ver-bucket"
	email := fmt.Sprintf("ver-%s-%d@test.local", t.Name(), os.Getpid())

	_, err = db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING
	`, tenantID, "Versioning Test", email, "AK-"+tenantID, "SK-"+tenantID)
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO buckets (tenant_id, name, visibility, versioning_status)
		VALUES ($1, $2, 'private', 'disabled')
		ON CONFLICT (tenant_id, name) DO UPDATE SET versioning_status = 'disabled'
	`, tenantID, bucket)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM object_versions WHERE tenant_id = $1", tenantID)
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

	return &versioningFixture{
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

func (f *versioningFixture) setVersioning(t *testing.T, status string) {
	t.Helper()
	_, err := f.db.Exec(`UPDATE buckets SET versioning_status = $1 WHERE tenant_id = $2 AND name = $3`,
		status, f.tenantID, f.bucket)
	require.NoError(t, err)
}

func (f *versioningFixture) putObject(t *testing.T, key, content string) *httptest.ResponseRecorder {
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

func (f *versioningFixture) getObject(t *testing.T, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/"+f.bucket+"/"+key, nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, f.bucket, key)
	return w
}

func (f *versioningFixture) getObjectVersion(t *testing.T, key, versionID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/"+f.bucket+"/"+key+"?versionId="+versionID, nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, f.bucket, key)
	return w
}

func (f *versioningFixture) deleteObject(t *testing.T, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("DELETE", "/"+f.bucket+"/"+key, nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandleDelete(w, req, f.bucket, key)
	return w
}

func (f *versioningFixture) deleteObjectVersion(t *testing.T, key, versionID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("DELETE", "/"+f.bucket+"/"+key+"?versionId="+versionID, nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	f.adapter.HandleDelete(w, req, f.bucket, key)
	return w
}

func (f *versioningFixture) countVersions(t *testing.T, key string) int {
	t.Helper()
	var count int
	err := f.db.QueryRow(`
		SELECT COUNT(*) FROM object_versions
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, f.bucket, key).Scan(&count)
	require.NoError(t, err)
	return count
}

func TestVersioning_EnableSuspend(t *testing.T) {
	f := setupVersioningFixture(t)

	s3Req := &S3Request{Bucket: f.bucket, TenantID: f.tenantID}

	// Initially disabled — GET should return empty Status
	req := httptest.NewRequest("GET", "/"+f.bucket+"?versioning", nil)
	ctx := tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.handleGetBucketVersioning(w, req, s3Req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp VersioningConfiguration
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Status, "disabled bucket should have empty Status")

	// Enable versioning
	body := `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`
	req = httptest.NewRequest("PUT", "/"+f.bucket+"?versioning", bytes.NewReader([]byte(body)))
	ctx = tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w = httptest.NewRecorder()
	f.server.handlePutBucketVersioning(w, req, s3Req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify enabled
	req = httptest.NewRequest("GET", "/"+f.bucket+"?versioning", nil)
	ctx = tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w = httptest.NewRecorder()
	f.server.handleGetBucketVersioning(w, req, s3Req)
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "Enabled", resp.Status)

	// Suspend versioning
	body = `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Suspended</Status></VersioningConfiguration>`
	req = httptest.NewRequest("PUT", "/"+f.bucket+"?versioning", bytes.NewReader([]byte(body)))
	ctx = tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w = httptest.NewRecorder()
	f.server.handlePutBucketVersioning(w, req, s3Req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify suspended
	req = httptest.NewRequest("GET", "/"+f.bucket+"?versioning", nil)
	ctx = tenant.WithTenant(req.Context(), f.tenant)
	req = req.WithContext(ctx)
	w = httptest.NewRecorder()
	f.server.handleGetBucketVersioning(w, req, s3Req)
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "Suspended", resp.Status)
}

func TestVersioning_PutCreatesVersions(t *testing.T) {
	f := setupVersioningFixture(t)
	f.setVersioning(t, "Enabled")

	key := "multi-ver.txt"
	versionIDs := make([]string, 3)

	for i := 0; i < 3; i++ {
		w := f.putObject(t, key, fmt.Sprintf("content v%d", i+1))
		vid := w.Header().Get("x-amz-version-id")
		assert.NotEmpty(t, vid, "PUT should return version ID")
		assert.NotEqual(t, "null", vid, "Enabled versioning should not use 'null' version ID")
		versionIDs[i] = vid
	}

	assert.Equal(t, 3, f.countVersions(t, key))

	// All version IDs should be unique
	assert.NotEqual(t, versionIDs[0], versionIDs[1])
	assert.NotEqual(t, versionIDs[1], versionIDs[2])

	// Latest version should be the most recent
	var latestVID string
	err := f.db.QueryRow(`
		SELECT version_id FROM object_versions
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND is_latest = TRUE`,
		f.tenantID, f.bucket, key).Scan(&latestVID)
	require.NoError(t, err)
	assert.Equal(t, versionIDs[2], latestVID)

	// GET should return the latest content
	w := f.getObject(t, key)
	assert.Equal(t, http.StatusOK, w.Code)
	body, _ := io.ReadAll(w.Body)
	assert.Equal(t, "content v3", string(body))
}

func TestVersioning_GetSpecificVersion(t *testing.T) {
	f := setupVersioningFixture(t)
	f.setVersioning(t, "Enabled")

	key := "specific-ver.txt"
	w1 := f.putObject(t, key, "first version")
	vid1 := w1.Header().Get("x-amz-version-id")

	_ = f.putObject(t, key, "second version")

	// GET with first versionId should return first content's metadata
	w := f.getObjectVersion(t, key, vid1)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, vid1, w.Header().Get("x-amz-version-id"))
}

func TestVersioning_GetNonexistentVersion(t *testing.T) {
	f := setupVersioningFixture(t)
	f.setVersioning(t, "Enabled")

	key := "exists.txt"
	f.putObject(t, key, "some content")

	w := f.getObjectVersion(t, key, "nonexistent-version-id")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestVersioning_DeleteMarker(t *testing.T) {
	f := setupVersioningFixture(t)
	f.setVersioning(t, "Enabled")

	key := "to-delete.txt"
	f.putObject(t, key, "before delete")

	// DELETE without versionId creates a delete marker
	delW := f.deleteObject(t, key)
	assert.Equal(t, http.StatusNoContent, delW.Code)
	assert.Equal(t, "true", delW.Header().Get("x-amz-delete-marker"))
	assert.NotEmpty(t, delW.Header().Get("x-amz-version-id"))

	// GET now returns 404 with delete marker headers
	getW := f.getObject(t, key)
	assert.Equal(t, http.StatusNotFound, getW.Code)
	assert.Equal(t, "true", getW.Header().Get("x-amz-delete-marker"))
}

func TestVersioning_DeleteSpecificVersion(t *testing.T) {
	f := setupVersioningFixture(t)
	f.setVersioning(t, "Enabled")

	key := "perm-delete.txt"
	w1 := f.putObject(t, key, "v1")
	vid1 := w1.Header().Get("x-amz-version-id")

	w2 := f.putObject(t, key, "v2")
	vid2 := w2.Header().Get("x-amz-version-id")

	assert.Equal(t, 2, f.countVersions(t, key))

	// Permanently delete v1
	delW := f.deleteObjectVersion(t, key, vid1)
	assert.Equal(t, http.StatusNoContent, delW.Code)
	assert.Equal(t, vid1, delW.Header().Get("x-amz-version-id"))

	assert.Equal(t, 1, f.countVersions(t, key))

	// v2 is still accessible
	getW := f.getObjectVersion(t, key, vid2)
	assert.Equal(t, http.StatusOK, getW.Code)
}

func TestVersioning_DisabledBucketNoVersions(t *testing.T) {
	f := setupVersioningFixture(t)

	key := "no-versioning.txt"
	w := f.putObject(t, key, "disabled versioning content")

	vid := w.Header().Get("x-amz-version-id")
	assert.Empty(t, vid, "disabled bucket should not return version ID")
	assert.Equal(t, 0, f.countVersions(t, key), "no rows in object_versions when disabled")
}

func TestVersioning_SuspendedUsesNullVersionID(t *testing.T) {
	f := setupVersioningFixture(t)
	f.setVersioning(t, "Suspended")

	key := "suspended.txt"
	w := f.putObject(t, key, "suspended content")

	vid := w.Header().Get("x-amz-version-id")
	assert.Equal(t, "null", vid, "suspended versioning should use 'null' version ID")
	assert.Equal(t, 1, f.countVersions(t, key))
}
