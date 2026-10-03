package api

// A system bucket (tenant.ExportsBucket) does not exist to the S3 API
// (NoSuchBucket, except GetObject/HeadObject) and the dashboard's bucket
// pages 404 it — but the management API's per-bucket routes did not know it:
// PATCH could change its metadata, PUT …/tier could re-tier the export
// objects onto tape, PUT …/residency could pin them, DELETE could remove the
// registry row under a completed export. Plan-driver review of WP-R10-3b
// (#563): the same rule on the third entry point.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (f *versioningFixture) mgmtBucketCall(t *testing.T, method, bucket, suffix, body string, h http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1/manage/buckets/"+bucket+suffix, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", bucket)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = context.WithValue(ctx, tenantIDKey, f.tenantID)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

func TestMgmt_SystemBucketDoesNotExistOnThePerBucketRoutes(t *testing.T) {
	f := setupVersioningFixture(t)
	// The export bucket's registry row, as the export service creates it.
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, versioning_status, object_lock_enabled, metadata)
		VALUES ($1, $2, 'private', 'disabled', FALSE, '{"system": true}'::jsonb) ON CONFLICT (tenant_id, name) DO NOTHING`,
		f.tenantID, tenant.ExportsBucket)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1 AND name = $2`, f.tenantID, tenant.ExportsBucket)
	})

	calls := []struct {
		name, method, suffix, body string
		h                          http.HandlerFunc
	}{
		{"get", "GET", "", "", f.server.handleMgmtGetBucket},
		{"patch", "PATCH", "", `{"metadata":{"owner":"x"}}`, f.server.handleMgmtPatchBucket},
		{"delete", "DELETE", "", "", f.server.handleMgmtDeleteBucket},
		{"objects", "GET", "/objects", "", f.server.handleMgmtListObjects},
		{"tier", "PUT", "/tier", `{"tier":"archive"}`, f.server.handleMgmtSetBucketTier},
		{"residency", "PUT", "/residency", `{"region":"eu-west-1"}`, f.server.handleMgmtSetBucketResidency},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			w := f.mgmtBucketCall(t, c.method, tenant.ExportsBucket, c.suffix, c.body, c.h)
			assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), "bucket_not_found")
		})
	}

	// The row is untouched: still there, still private, no tier, no pin, no metadata.
	var n int
	var tierPref, region, visibility string
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(tier_preference), ''), COALESCE(MAX(region), ''), COALESCE(MAX(visibility), '')
		FROM buckets WHERE tenant_id = $1 AND name = $2`, f.tenantID, tenant.ExportsBucket).Scan(&n, &tierPref, &region, &visibility))
	assert.Equal(t, 1, n, "the registry row survives")
	assert.NotEqual(t, "archive", tierPref)
	assert.NotEqual(t, "eu-west-1", region)
	assert.Equal(t, "private", visibility)

	// A customer bucket is unaffected.
	w := f.mgmtBucketCall(t, "GET", f.bucket, "", "", f.server.handleMgmtGetBucket)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}
