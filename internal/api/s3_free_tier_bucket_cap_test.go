package api

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CreateBucket enforces the free tier's one bucket, but a PUT (or copy, or
// multipart) into a bucket that has no row used to auto-create it — the cap
// was one upload away from meaningless (R2-19 = R5-27, Review R10-12).
func TestFreeTier_ObjectWritesCannotGrowASecondBucket(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 100<<20)
	_, err := f.db.Exec(`UPDATE tenant_quotas SET tier = 'free' WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, 'test-bucket') ON CONFLICT DO NOTHING`, f.tenantID)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.tenantID) })

	// The one bucket still works.
	require.Equal(t, 200, f.put(t, "ok.bin", testBytes(4<<10)))

	// A second, never-created bucket: refused like CreateBucket refuses it,
	// and nothing is reserved.
	req := httptest.NewRequest("PUT", "/second-bucket/k.bin", bytes.NewReader(testBytes(4<<10)))
	req.ContentLength = 4 << 10
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handlePutObject(w, req, f.s3Req("second-bucket", "k.bin"))
	assert.Equal(t, 403, w.Code)
	assert.Contains(t, w.Body.String(), "QuotaExceeded")
	assert.Equal(t, int64(4<<10), f.used(t), "the refused write reserved nothing")

	// Copy into it and a multipart start are refused the same way.
	cReq := httptest.NewRequest("PUT", "/second-bucket/copy.bin", nil)
	cReq.Header.Set("x-amz-copy-source", "/test-bucket/ok.bin")
	cReq = cReq.WithContext(f.ctx(cReq.Context()))
	cw := httptest.NewRecorder()
	f.server.handleCopyObject(cw, cReq, f.s3Req("second-bucket", "copy.bin"))
	assert.Equal(t, 403, cw.Code)

	mReq := httptest.NewRequest("POST", "/second-bucket/mp.bin?uploads", nil)
	mReq = mReq.WithContext(f.ctx(mReq.Context()))
	mw := httptest.NewRecorder()
	f.server.handleInitiateMultipartUpload(mw, mReq, "second-bucket", "mp.bin")
	assert.Equal(t, 403, mw.Code)

	// A paying tenant is not affected.
	_, err = f.db.Exec(`UPDATE tenant_quotas SET tier = 'standard' WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	w = httptest.NewRecorder()
	req = httptest.NewRequest("PUT", "/second-bucket/k.bin", bytes.NewReader(testBytes(4<<10)))
	req.ContentLength = 4 << 10
	req = req.WithContext(f.ctx(req.Context()))
	f.server.handlePutObject(w, req, f.s3Req("second-bucket", "k.bin"))
	assert.Equal(t, 200, w.Code)
}
