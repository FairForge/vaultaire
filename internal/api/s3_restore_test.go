package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// stubArchiveDriver simulates Geyser's Glacier semantics at the engine.Driver
// level: once archived, Get fails with engine.ErrArchived (as geyser.go does
// for Vail's 403 InvalidObjectState) while metadata/restore calls keep working.
type stubArchiveDriver struct {
	mu           sync.Mutex
	data         map[string][]byte
	archived     bool
	inProgress   bool
	restoreCalls int
	lastDays     int32
	restoreState string
}

func newStubArchiveDriver() *stubArchiveDriver {
	return &stubArchiveDriver{data: map[string][]byte{}}
}

func (d *stubArchiveDriver) Name() string { return "geyser" }

func (d *stubArchiveDriver) Get(_ context.Context, container, artifact string) (io.ReadCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.archived {
		return nil, fmt.Errorf("geyser get %s/%s: %w", container, artifact, engine.ErrArchived)
	}
	b, ok := d.data[container+"/"+artifact]
	if !ok {
		return nil, engine.ErrNotFound(container, artifact)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (d *stubArchiveDriver) Put(_ context.Context, container, artifact string, data io.Reader, _ ...engine.PutOption) error {
	b, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data[container+"/"+artifact] = b
	return nil
}

func (d *stubArchiveDriver) Delete(_ context.Context, container, artifact string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.data, container+"/"+artifact)
	return nil
}

func (d *stubArchiveDriver) List(_ context.Context, _ string, _ string) ([]string, error) {
	return nil, nil
}

func (d *stubArchiveDriver) Exists(_ context.Context, container, artifact string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.data[container+"/"+artifact]
	return ok, nil
}

func (d *stubArchiveDriver) HealthCheck(_ context.Context) error { return nil }

func (d *stubArchiveDriver) RestoreObject(_ context.Context, _, _ string, days int32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inProgress {
		return fmt.Errorf("geyser restore: %w", engine.ErrRestoreAlreadyInProgress)
	}
	d.restoreCalls++
	d.lastDays = days
	return nil
}

func (d *stubArchiveDriver) RestoreStatus(_ context.Context, _, _ string) (*engine.RestoreStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return &engine.RestoreStatus{Restore: d.restoreState, StorageClass: "GLACIER"}, nil
}

var _ engine.Driver = (*stubArchiveDriver)(nil)
var _ engine.Restorer = (*stubArchiveDriver)(nil)

// restoreFixture: chunking fixture + the stub archive driver registered as
// "geyser" + an archive-tier bucket + a Server wired for the handlers that
// live at Server level (RestoreObject, HeadObject).
func restoreFixture(t *testing.T) (*adapterTestFixture, *stubArchiveDriver, *Server) {
	t.Helper()
	f := setupChunkingFixture(t)

	stub := newStubArchiveDriver()
	f.eng.AddDriver("geyser", stub)

	_, err := f.db.Exec(`
		INSERT INTO buckets (tenant_id, name, tier_preference)
		VALUES ($1, 'test-bucket', 'archive')
		ON CONFLICT (tenant_id, name) DO UPDATE SET tier_preference = 'archive'`,
		f.tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1 AND name = 'test-bucket'`, f.tenantID)
	})

	s := &Server{engine: f.eng, db: f.db, logger: zap.NewNop()}
	return f, stub, s
}

func putArchiveObject(t *testing.T, f *adapterTestFixture, key string, content []byte) {
	t.Helper()
	req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", key)
	require.Equal(t, http.StatusOK, w.Code)
}

// TestGetArchivedObject_403InvalidObjectState: a GET on an object past the
// staging window must answer AWS Glacier's exact wire error — 403 with
// <Code>InvalidObjectState</Code> — never a raw 500, and never a 404 from
// failover trying hot backends that don't hold the object.
func TestGetArchivedObject_403InvalidObjectState(t *testing.T) {
	f, stub, _ := restoreFixture(t)

	putArchiveObject(t, f, "cold.bin", generateTestData(512))
	stub.archived = true

	req := httptest.NewRequest("GET", "/test-bucket/cold.bin", nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleGet(w, req, "test-bucket", "cold.bin")

	assert.Equal(t, http.StatusForbidden, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "<Code>InvalidObjectState</Code>",
		"archived GET must be S3 InvalidObjectState XML, got: %s", body)
}

// TestRestoreObject_Passthrough: POST ?restore reaches the Restorer with the
// requested Days and answers 202.
func TestRestoreObject_Passthrough(t *testing.T) {
	f, stub, s := restoreFixture(t)
	putArchiveObject(t, f, "cold.bin", generateTestData(512))
	stub.archived = true

	body := strings.NewReader(`<RestoreRequest><Days>3</Days></RestoreRequest>`)
	req := httptest.NewRequest("POST", "/test-bucket/cold.bin?restore", body)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	s.handleRestoreObject(w, req, &S3Request{Bucket: "test-bucket", Object: "cold.bin", TenantID: f.tenantID})

	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, 1, stub.restoreCalls)
	assert.Equal(t, int32(3), stub.lastDays)
}

// TestRestoreObject_AlreadyInProgress409: a running recall answers AWS's 409.
func TestRestoreObject_AlreadyInProgress409(t *testing.T) {
	f, stub, s := restoreFixture(t)
	putArchiveObject(t, f, "cold.bin", generateTestData(512))
	stub.inProgress = true

	req := httptest.NewRequest("POST", "/test-bucket/cold.bin?restore", nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	s.handleRestoreObject(w, req, &S3Request{Bucket: "test-bucket", Object: "cold.bin", TenantID: f.tenantID})

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "<Code>RestoreAlreadyInProgress</Code>")
}

// TestRestoreObject_HotBackend403: restore on a hot-tier object is
// InvalidObjectState (matching AWS for STANDARD objects).
func TestRestoreObject_HotBackend403(t *testing.T) {
	f, _, s := restoreFixture(t)

	// Head-cache row pointing at the hot primary, no restore concept there.
	_, err := f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name)
		VALUES ($1, 'test-bucket', 'hot.bin', 10, 'e', 'application/octet-stream', 'local')`,
		f.tenantID)
	require.NoError(t, err)

	req := httptest.NewRequest("POST", "/test-bucket/hot.bin?restore", nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	s.handleRestoreObject(w, req, &S3Request{Bucket: "test-bucket", Object: "hot.bin", TenantID: f.tenantID})

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "<Code>InvalidObjectState</Code>")
}

// TestRestoreObject_MissingKey404: restore of an absent key is NoSuchKey.
func TestRestoreObject_MissingKey404(t *testing.T) {
	f, _, s := restoreFixture(t)

	req := httptest.NewRequest("POST", "/test-bucket/nope.bin?restore", nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	s.handleRestoreObject(w, req, &S3Request{Bucket: "test-bucket", Object: "nope.bin", TenantID: f.tenantID})

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestHeadArchivedObject_XAmzRestore: HEAD surfaces the backend's live
// restore state so `aws s3api head-object` pollers work unmodified.
func TestHeadArchivedObject_XAmzRestore(t *testing.T) {
	f, stub, s := restoreFixture(t)
	putArchiveObject(t, f, "cold.bin", generateTestData(512))
	stub.archived = true
	stub.restoreState = `ongoing-request="true"`

	req := httptest.NewRequest("HEAD", "/test-bucket/cold.bin", nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	s.handleHeadObject(w, req, &S3Request{Bucket: "test-bucket", Object: "cold.bin", TenantID: f.tenantID})

	require.Equal(t, http.StatusOK, w.Code, "HEAD on archived must still work")
	assert.Equal(t, `ongoing-request="true"`, w.Header().Get("x-amz-restore"))
	assert.Equal(t, "GLACIER", w.Header().Get("x-amz-storage-class"))

	// No restore requested → header absent (AWS semantics).
	stub.restoreState = ""
	w = httptest.NewRecorder()
	s.handleHeadObject(w, req, &S3Request{Bucket: "test-bucket", Object: "cold.bin", TenantID: f.tenantID})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("x-amz-restore"))
}
