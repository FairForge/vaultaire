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
	"strings"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/dataplane"
	"github.com/FairForge/vaultaire/internal/engine"
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

// RP74-5: one slc host per Server — one *http.Client and one Transport, so
// connections to the backends are reused and no idle pool outlives a
// request.
func TestDataPlaneSLC_TwoWritesShareOneClient(t *testing.T) {
	s := &Server{}
	a, b := s.slcHost(), s.slcHost()
	require.Same(t, a, b, "built once")
	assert.Same(t, a.Client, b.Client)
	assert.NotNil(t, a.Client.Transport)
}

// RP74-6: host=slc's codec writes take their bytes from the origin's own
// chunk budget (CHUNK_PUT_BUDGET_BYTES): the same gauge moves.
func TestDataPlaneSLC_TheHostsBudgetIsTheOriginsChunkBudget(t *testing.T) {
	s := &Server{chunkPutBudget: newChunkPutBudget(256 << 20)}
	h := s.slcHost()
	require.NotNil(t, h.Budget)
	release, err := h.Budget.Acquire(context.Background(), chunkPutReserveBytes)
	require.NoError(t, err)
	assert.Equal(t, chunkPutReserveBytes, s.chunkPutBudget.inUse())
	assert.Equal(t, float64(chunkPutReserveBytes), promtestutil.ToFloat64(chunkPutBudgetInUse))
	release()
	assert.Zero(t, s.chunkPutBudget.inUse())
	assert.Zero(t, promtestutil.ToFloat64(chunkPutBudgetInUse))

	// Spent: the host's ErrBusy (the client's 503).
	hold, err := s.chunkPutBudget.reserve(context.Background(), 256<<20, time.Second)
	require.NoError(t, err)
	defer hold.close()
	_, err = h.Budget.Acquire(context.Background(), 1<<20)
	assert.ErrorIs(t, err, dataplane.ErrBusy)
}

// tamper flips one byte of the blob whose address contains name.
func (d *edgeFakeDriver) tamper(t *testing.T, name string) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, b := range d.objects {
		if strings.Contains(k, name+"@") {
			c := bytes.Clone(b)
			c[len(c)/2] ^= 0xff
			d.objects[k] = c
			return
		}
	}
	t.Fatalf("no blob for %s", name)
}

// RP74-4: host=slc touches every byte, so its whole read runs the origin's
// integrity reader against the row's digest (the admit's body_md5): a
// backend serving other bytes is cut before the last one, and a verified
// read is never counted unverified.
func TestDataPlaneSLC_AWholeReadIsVerified(t *testing.T) {
	f := setupEdgeFixture(t)
	f.wireEgress(1<<40, 0, nil)
	f.srv.dataPlaneTransport = f.drv
	host := f.srv.slcHost()
	require.NotNil(t, host.Verify)
	body := testBytes(9000)
	res, err := f.dataHost().Write(context.Background(), f.signedHTTP(http.MethodPut, "/"+f.bucket+"/v.bin", nil, body, nil), bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.Relay.Status, res.Relay.Body)

	unverified := promtestutil.ToFloat64(objectIntegrityUnverified.WithLabelValues(integrityPathEdge, "unverified"))
	rr, err := host.Read(context.Background(), f.signedHTTP(http.MethodGet, "/"+f.bucket+"/v.bin", nil, nil, nil))
	require.NoError(t, err)
	require.False(t, rr.PassThrough, "%+v", rr.Admit)
	assert.NotEmpty(t, rr.Admit.BodyMD5, "the admit carries the row's digest")
	got, err := io.ReadAll(rr.Body)
	_ = rr.Body.Close()
	require.NoError(t, err)
	assert.True(t, bytes.Equal(body, got))
	assert.Equal(t, unverified, promtestutil.ToFloat64(objectIntegrityUnverified.WithLabelValues(integrityPathEdge, "unverified")),
		"a verified slc read is not counted unverified")

	failures := promtestutil.ToFloat64(objectIntegrityFailures.WithLabelValues(integrityPathEdge, "md5"))
	f.drv.tamper(t, "v.bin")
	rr, err = host.Read(context.Background(), f.signedHTTP(http.MethodGet, "/"+f.bucket+"/v.bin", nil, nil, nil))
	require.NoError(t, err)
	require.False(t, rr.PassThrough)
	got, err = io.ReadAll(rr.Body)
	_ = rr.Body.Close()
	assert.Error(t, err, "the response is cut")
	assert.Less(t, len(got), len(body), "the last byte is withheld")
	assert.Equal(t, failures+1, promtestutil.ToFloat64(objectIntegrityFailures.WithLabelValues(integrityPathEdge, "md5")))

	// A node verifies nothing of its own: its host has no Verify.
	assert.Nil(t, f.dataHost().Verify)
}

// RP74-4: an aws-chunked move on host=slc is decoded beside the backend and
// its trailer verified; a trailer that is not the body's is the client's
// 400 XAmzContentChecksumMismatch — no row, the blob removed, everything
// given back — even on a backend that does not check it (Lyve).
func TestDataPlaneSLC_AWrongTrailerOnAMoveIsTheClients400(t *testing.T) {
	f := setupEdgeFixture(t)
	f.srv.dataPlaneTransport = f.drv
	f.drv.streaming = engine.EdgeStreamingProps{Supported: true, SignatureMaxAge: 15 * time.Minute} // Lyve's: no trailer check
	host := f.srv.slcHost()
	body := testBytes(3 << 20)
	framed := dataplane.FrameAWSChunked(body, dataplane.TrailerCRC64NVME, 1<<20)
	put := func(key string, payload []byte) *dataplane.Result {
		t.Helper()
		req := f.signedStreamingHTTP("/"+f.bucket+"/"+key, int64(len(body)), dataplane.TrailerCRC64NVME)
		res, err := host.Write(context.Background(), req, bytes.NewReader(payload), -1)
		require.NoError(t, err)
		require.True(t, res.Admit.Edge && res.Admit.Codec == nil, "%+v", res.Admit)
		return res
	}
	res := put("good.bin", framed)
	require.Equal(t, http.StatusOK, res.Relay.Status, res.Relay.Body)

	bad := bytes.Clone(framed)
	bad[100] ^= 1 // a data byte: the trailer no longer matches
	used := f.used()
	res = put("bad.bin", bad)
	assert.Equal(t, http.StatusBadRequest, res.Relay.Status)
	assert.Contains(t, res.Relay.Body, "XAmzContentChecksumMismatch")
	assert.Equal(t, used, f.used(), "everything given back")
	_, ok := f.head("bad.bin")
	assert.False(t, ok, "no row")
	f.drv.mu.Lock()
	for k := range f.drv.objects {
		assert.NotContains(t, k, "bad.bin@", "the blob removed")
	}
	f.drv.mu.Unlock()
}

// RP74-7: the registry's slc row steers host=slc — the flag AND the row up
// (read through the 15 s cache; one read): draining, down or no row and
// the handler takes every request as today.
func TestDataPlaneSLC_TheSLCRowMustBeUp(t *testing.T) {
	f := setupEdgeFixture(t)
	require.NoError(t, f.fl.Set(context.Background(), flagDataPlaneSLC, f.tenantID, true, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	c := f.srv.dataPlaneHosts()
	for health, want := range map[string]bool{"up": true, "draining": false, "down": false} {
		c.putName(dataplane.HostSLC, dataPlaneHost{Host: dataplane.HostSLC, Kind: dataplane.KindSLC, Health: health}, true)
		assert.Equal(t, want, f.srv.slcEnabled(context.Background(), f.tenantID), health)
	}
	c.putName(dataplane.HostSLC, dataPlaneHost{}, false)
	assert.False(t, f.srv.slcEnabled(context.Background(), f.tenantID), "no row")

	// The seeded row (migration 112: up) read from the database.
	c.invalidate()
	assert.True(t, f.srv.slcEnabled(context.Background(), f.tenantID))
	require.NoError(t, f.fl.Set(context.Background(), flagDataPlaneSLC, f.tenantID, false, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	assert.False(t, f.srv.slcEnabled(context.Background(), f.tenantID), "the flag first")
}
