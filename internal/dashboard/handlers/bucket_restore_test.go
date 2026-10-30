package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The Restore button and the restore-status probe recall the object's BLOB,
// not its key: since WP-R2-1 Part C (087) every whole write puts its bytes at
// a new address <key>@<26 base32> (object_head_cache.storage_key), and the
// archive backend holds nothing at the bare key. A row written before 087
// records no storage_key and its bytes are at the key (api.objectBlobAddress).

// recordingArchive is an archive backend that records the address of every
// restore and status call.
type recordingArchive struct {
	fakeArchive
	restored []string
	probed   []string
}

func (d *recordingArchive) RestoreObject(_ context.Context, container, artifact string, _ int32) error {
	d.restored = append(d.restored, container+"/"+artifact)
	return nil
}

func (d *recordingArchive) RestoreStatus(_ context.Context, container, artifact string) (*engine.RestoreStatus, error) {
	d.probed = append(d.probed, container+"/"+artifact)
	return &engine.RestoreStatus{Restore: `ongoing-request="true"`}, nil
}

const addressedBlob = "addressed.zip@ABCDEFGHIJKLMNOPQRSTUVWXYZ"

func restoreFixtureRows(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("no test database: %v", err)
	}
	tenantID := "tenant-" + uuid.New().String()[:8]
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, tenantID) })
	_, err = db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor, storage_key)
		VALUES ($1, 'b', 'addressed.zip', 9, 'e1', 'geyser', 'vault', $2),
		       ($1, 'b', 'legacy.zip', 9, 'e2', 'geyser', 'vault', NULL)`, tenantID, addressedBlob)
	require.NoError(t, err)
	return db, tenantID
}

func restoreRequest(method, target, tenantID string, form url.Values) *http.Request {
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "b")
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = dashauth.WithSession(ctx, &dashauth.SessionData{TenantID: tenantID})
	return req.WithContext(ctx)
}

func TestHandleRestoreObject_RecallsTheRowsBlobAddress(t *testing.T) {
	// Arrange
	db, tenantID := restoreFixtureRows(t)
	archive := &recordingArchive{}
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver("geyser", archive)
	handler := HandleRestoreObject(eng, db, zap.NewNop())

	// Act
	for _, key := range []string{"addressed.zip", "legacy.zip"} {
		rec := httptest.NewRecorder()
		handler(rec, restoreRequest(http.MethodPost, "/dashboard/buckets/b/restore", tenantID, url.Values{"key": {key}}))
		require.Equal(t, http.StatusSeeOther, rec.Code)
	}

	// Assert
	container := tenantID + "_b"
	assert.Equal(t, []string{container + "/" + addressedBlob, container + "/legacy.zip"}, archive.restored,
		"an addressed object is recalled at its storage_key; a pre-087 row at its key")
}

func TestHandleObjectRestoreStatus_ProbesTheRowsBlobAddress(t *testing.T) {
	// Arrange
	db, tenantID := restoreFixtureRows(t)
	archive := &recordingArchive{}
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver("geyser", archive)
	handler := HandleObjectRestoreStatus(eng, db, zap.NewNop())

	// Act
	rec := httptest.NewRecorder()
	handler(rec, restoreRequest(http.MethodGet, "/dashboard/buckets/b/restore-status?key=addressed.zip", tenantID, nil))

	// Assert
	assert.Equal(t, []string{tenantID + "_b/" + addressedBlob}, archive.probed)
	assert.Contains(t, rec.Body.String(), "coming down")
}
