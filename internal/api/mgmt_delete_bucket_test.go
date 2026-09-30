package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Post-merge review of R4 (R4-22): the management API's DELETE
// /api/v1/manage/buckets/{name} decided emptiness from a DATA_PATH directory
// listing that object writes never touch, then deleted the registry row —
// the same R4-04 class #515 fixed on the S3 path only. Both entry points now
// share deleteBucketRegistry.

func (f *versioningFixture) mgmtDeleteBucket(t *testing.T, bucket string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("DELETE", "/api/v1/manage/buckets/"+bucket, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", bucket)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = context.WithValue(ctx, tenantIDKey, f.tenantID)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.server.handleMgmtDeleteBucket(w, req)
	return w
}

func TestMgmtDeleteBucket_IsRegistryDriven(t *testing.T) {
	f := setupVersioningFixture(t)
	f.putObject(t, "obj.bin", "BYTES")

	// Non-empty (a head row, no data directory anywhere): refused, row kept.
	w := f.mgmtDeleteBucket(t, f.bucket)
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "bucket_not_empty")
	var rows int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM buckets WHERE tenant_id=$1 AND name=$2`, f.tenantID, f.bucket).Scan(&rows))
	assert.Equal(t, 1, rows, "the bucket row survives a refused delete")

	// Empty: deleted from the registry with no directory involved, and the
	// bucket's own notification targets go with it (a re-created bucket must
	// not inherit them).
	_, err := f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id=$1`, f.tenantID)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO bucket_notifications (tenant_id, bucket, event_filter, target_type, target_url) VALUES ($1, $2, 's3:ObjectCreated:*', 'webhook', 'https://example.com/hook')`, f.tenantID, f.bucket)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM bucket_notifications WHERE tenant_id=$1`, f.tenantID) })

	w = f.mgmtDeleteBucket(t, f.bucket)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM buckets WHERE tenant_id=$1 AND name=$2`, f.tenantID, f.bucket).Scan(&rows))
	assert.Equal(t, 0, rows)
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM bucket_notifications WHERE tenant_id=$1 AND bucket=$2`, f.tenantID, f.bucket).Scan(&rows))
	assert.Equal(t, 0, rows, "notification targets do not outlive the bucket")

	// Gone: 404, not 500 and not 200.
	w = f.mgmtDeleteBucket(t, f.bucket)
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}
