package api

// WP-EDGE-3 step 11: host=slc — with data_plane_slc ON for a tenant, the
// origin's own PutObject / UploadPart / GetObject of an edge-eligible
// request go through internal/dataplane in-process (admit and commit as
// function calls, the bytes through the same mover on this box): the rows
// are the Worker path's, the admit row says host = slc, and the bytes the
// origin streams are metered ONCE (by the origin's own writer — the commit
// leaves them). OFF = the handlers as today.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// originSigned sends a signed client request through the origin's S3
// handler (its own authentication, not the test mode).
func (f *edgeFixture) originSigned(r *http.Request) *httptest.ResponseRecorder {
	f.t.Helper()
	r.RemoteAddr = "203.0.113.9:4000"
	w := httptest.NewRecorder()
	f.srv.handleS3Request(w, r)
	return w
}

func (f *edgeFixture) admitHostOf(key, op string) string {
	f.t.Helper()
	var h string
	require.NoError(f.t, f.db.QueryRow(`SELECT COALESCE(host, '') FROM edge_admits WHERE tenant_id = $1 AND object_key = $2 AND op = $3 ORDER BY created_at DESC LIMIT 1`,
		f.tenantID, key, op).Scan(&h))
	return h
}

func TestDataPlaneSLC_PutGetRowsAreTheWorkerPathsAndMeteredOnce(t *testing.T) {
	f := setupEdgeFixture(t)
	f.wireEgress(1<<40, 0, nil)
	f.srv.dataPlaneTransport = f.drv
	require.NoError(t, f.fl.Set(context.Background(), flagDataPlaneSLC, f.tenantID, true, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	body := testBytes(12 << 20)

	// The same bytes through the Worker's path (the control).
	usedBefore := f.used()
	ctl := f.admit(f.signed("/"+f.bucket+"/worker.bin", nil, body, nil, ""))
	require.True(t, ctl.Edge)
	etag := f.drv.workerPut(f.tenantID, f.container(), ctl.StorageKey, body)
	relay, _ := f.commit(okCommit(ctl.AdmitID, body, etag))
	require.Equal(t, http.StatusOK, relay.Status)
	workerBill := f.used() - usedBefore

	// host=slc: the origin's own PUT handler.
	usedBefore = f.used()
	w := f.originSigned(f.signedHTTP(http.MethodPut, "/"+f.bucket+"/slc.bin", nil, body, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, workerBill, f.used()-usedBefore, "the same bill")
	assert.Equal(t, "slc", f.admitHostOf("slc.bin", edgeOpPut))
	hw, ok := f.head("worker.bin")
	require.True(t, ok)
	hs, ok := f.head("slc.bin")
	require.True(t, ok)
	assert.Equal(t, hw.size, hs.size)
	assert.Equal(t, hw.etag, hs.etag)
	assert.Equal(t, hw.backend, hs.backend)
	assert.Equal(t, hw.floor, hs.floor)
	assert.Equal(t, hw.ct, hs.ct)
	assert.Equal(t, `"`+hs.etag+`"`, w.Header().Get("ETag"))

	// A GET through the origin: the backend's bytes on the presigned link,
	// metered once (the origin's writer; the commit leaves them).
	before := f.egressRecorded()
	w = f.originSigned(f.signedHTTP(http.MethodGet, "/"+f.bucket+"/slc.bin", nil, nil, nil))
	require.Equal(t, http.StatusOK, w.Code)
	got, _ := io.ReadAll(w.Body)
	assert.True(t, bytes.Equal(body, got))
	assert.Equal(t, "slc", f.admitHostOf("slc.bin", edgeOpGet))
	assert.Equal(t, before+int64(len(body)), f.egressRecorded(), "metered once")

	// The direct path with the flag OFF meters the same.
	require.NoError(t, f.fl.Set(context.Background(), flagDataPlaneSLC, f.tenantID, false, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	before = f.egressRecorded()
	w = f.originSigned(f.signedHTTP(http.MethodGet, "/"+f.bucket+"/slc.bin", nil, nil, nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, before+int64(len(body)), f.egressRecorded())
}

func TestDataPlaneSLC_UploadPartThroughTheOriginIsAnEdgePart(t *testing.T) {
	f := setupEdgeFixture(t)
	f.srv.dataPlaneTransport = f.drv
	require.NoError(t, f.fl.Set(context.Background(), flagDataPlaneSLC, f.tenantID, true, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	id := f.createUpload("slc.zip")
	var etags []string
	for n, pb := range [][]byte{testBytes(9000), testBytes(4000)} {
		q := url.Values{"uploadId": {id}, "partNumber": {[]string{"1", "2"}[n]}}
		w := f.originSigned(f.signedHTTP(http.MethodPut, "/"+f.bucket+"/slc.zip", q, pb, nil))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		etags = append(etags, w.Header().Get("ETag"))
	}
	var edge bool
	require.NoError(t, f.db.QueryRow(`SELECT edge FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&edge))
	assert.True(t, edge, "one upload, one path: the edge's")
	assert.Equal(t, "slc", f.admitHostOf("slc.zip", edgeOpPart))
	wc := f.s3(http.MethodPost, "/"+f.bucket+"/slc.zip?uploadId="+id,
		[]byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>`+etags[0]+`</ETag></Part><Part><PartNumber>2</PartNumber><ETag>`+etags[1]+`</ETag></Part></CompleteMultipartUpload>`), nil)
	require.Equal(t, http.StatusOK, wc.Code, wc.Body.String())
}

// The flag OFF: no code path change — the handler writes its rows once,
// and no admit row exists.
func TestDataPlaneSLC_FlagOffIsTheHandlerAsToday(t *testing.T) {
	f := setupEdgeFixture(t)
	f.srv.dataPlaneTransport = f.drv
	body := testBytes(9000)
	w := f.originSigned(f.signedHTTP(http.MethodPut, "/"+f.bucket+"/off.bin", nil, body, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM edge_admits WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	assert.Zero(t, n, "no admit: the data plane was not asked")
	h, ok := f.head("off.bin")
	require.True(t, ok)
	assert.Equal(t, int64(len(body)), h.size)
}
