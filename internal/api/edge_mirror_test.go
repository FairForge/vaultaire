package api

// WP-EDGE-4 Part A (Stage 1a Prompt 48): the second copy at the edge. With
// edge_mirror ON for a tenant, the write admit answers TWO links — the
// placement's backend and the first registered leg of MIRROR_LEGS that is
// not it — the host sends the body to both, and the commit records both:
// one blob_copies row per mirrored blob, ready when the leg answered,
// pending (the blob_copies job's) when it did not. A codec write mirrors
// its STORED chunks the same way (two links per miss).

import (
	"context"
	"crypto/md5" // #nosec G501 -- the S3 ETag
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/dataplane"
)

// edgeFakeMux routes a link to the fake backend whose host it names.
type edgeFakeMux []*edgeFakeDriver

func (m edgeFakeMux) RoundTrip(r *http.Request) (*http.Response, error) {
	for _, d := range m {
		if r.URL.Host == d.urlHost {
			return d.RoundTrip(r)
		}
	}
	return nil, fmt.Errorf("no fake backend at %s", r.URL.Host)
}

// addMirrorLeg registers a second fake EdgeWriter as the tenant's mirror
// leg (MIRROR_LEGS = it) and turns edge_mirror on.
func (f *edgeFixture) addMirrorLeg() *edgeFakeDriver {
	f.t.Helper()
	m := newEdgeFakeDriver()
	m.name, m.urlHost = "edgemirror", "mirror.backend.invalid"
	f.srv.engine.AddDriver(m.name, m)
	f.srv.mirrorLegs = []string{"edgemirror"}
	require.NoError(f.t, f.fl.Set(context.Background(), flagEdgeMirror, f.tenantID, true, "test"))
	require.NoError(f.t, f.fl.Refresh(context.Background()))
	f.t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM blob_copies WHERE tenant_id = $1`, f.tenantID) })
	return m
}

type copyRow struct {
	storageKey, leg, state, etag, source string
	size                                 int64
}

func (f *edgeFixture) copies() []copyRow {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT storage_key, leg, state, etag, source_backend, size FROM blob_copies WHERE tenant_id = $1 ORDER BY storage_key`, f.tenantID)
	require.NoError(f.t, err)
	defer func() { _ = rows.Close() }()
	var out []copyRow
	for rows.Next() {
		var c copyRow
		require.NoError(f.t, rows.Scan(&c.storageKey, &c.leg, &c.state, &c.etag, &c.source, &c.size))
		out = append(out, c)
	}
	require.NoError(f.t, rows.Err())
	return out
}

func TestMirrorLegFor_TheFirstRegisteredWriterThatIsNotThePlacement(t *testing.T) {
	f := setupEdgeFixture(t)
	f.addMirrorLeg()
	f.srv.mirrorLegs = []string{"nosuch", "edgefake", "edgemirror"}
	assert.Equal(t, "edgemirror", f.srv.mirrorLegFor("edgefake", bucketScope{}), "unregistered and the placement itself are skipped")
	assert.Equal(t, "edgefake", f.srv.mirrorLegFor("edgemirror", bucketScope{}))
	f.srv.mirrorLegs = []string{"edgefake"}
	assert.Empty(t, f.srv.mirrorLegFor("edgefake", bucketScope{}), "no other leg: no mirror (never a refusal)")

	assert.Equal(t, []string{"lyve", "idrive"}, mirrorLegsFromEnv(func(string) string { return "" }, nil))
	assert.Equal(t, []string{"idrive", "lyve"}, mirrorLegsFromEnv(func(string) string { return " idrive, lyve ,idrive" }, nil), "trimmed, deduplicated")
	assert.Equal(t, []string{"lyve", "idrive"}, mirrorLegsFromEnv(func(string) string { return "Bad Name!" }, nil), "a list that does not validate: the default")
}

func TestEdgeMirror_AWholePutLandsOnBothLegsInOneCommit(t *testing.T) {
	f := setupEdgeFixture(t)
	m := f.addMirrorLeg()
	body := testBytes(9000)
	resp := f.admit(f.signed("/"+f.bucket+"/two.bin", nil, body, nil, ""))
	require.True(t, resp.Edge, "%+v", resp)
	require.Len(t, resp.URLs, 2, "the placement's link and the mirror's")
	assert.Equal(t, resp.URL, resp.URLs[0], "url stays the primary's (the 0.4 Worker reads only it)")
	assert.Contains(t, resp.URLs[1], "mirror.backend.invalid")
	assert.Contains(t, resp.URLs[1], "/"+resp.StorageKey, "the same address on the leg")

	a := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	b := m.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	c := okCommit(resp.AdmitID, body, a)
	c.BackendETags = []string{a, b}
	relay, _ := f.commit(c)
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	sum := md5.Sum(body) // #nosec G401
	got := f.copies()
	require.Len(t, got, 1)
	assert.Equal(t, copyRow{storageKey: resp.StorageKey, leg: "edgemirror", state: "ready", etag: hex.EncodeToString(sum[:]), source: "edgefake", size: int64(len(body))}, got[0])

	// The mirror's PUT failed: the object commits on one leg, the copy is
	// owed (pending) — never a failed PUT for the client.
	resp = f.admit(f.signed("/"+f.bucket+"/one.bin", nil, body, nil, ""))
	require.Len(t, resp.URLs, 2)
	a = f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	c = okCommit(resp.AdmitID, body, a)
	c.BackendETags = []string{a}
	relay, _ = f.commit(c)
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	var state string
	require.NoError(t, f.db.QueryRow(`SELECT state FROM blob_copies WHERE tenant_id = $1 AND storage_key = $2`, f.tenantID, resp.StorageKey).Scan(&state))
	assert.Equal(t, "pending", state)

	// A mirror ETag that is not the body's: never a ready copy.
	resp = f.admit(f.signed("/"+f.bucket+"/bad.bin", nil, body, nil, ""))
	a = f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	c = okCommit(resp.AdmitID, body, a)
	c.BackendETags = []string{a, `"00000000000000000000000000000000"`}
	relay, _ = f.commit(c)
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	require.NoError(t, f.db.QueryRow(`SELECT state FROM blob_copies WHERE tenant_id = $1 AND storage_key = $2`, f.tenantID, resp.StorageKey).Scan(&state))
	assert.Equal(t, "pending", state, "the job rewrites it")

	// Flag OFF: one link, no copy row.
	require.NoError(t, f.fl.Set(context.Background(), flagEdgeMirror, f.tenantID, false, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	resp = f.admit(f.signed("/"+f.bucket+"/off.bin", nil, body, nil, ""))
	assert.Len(t, resp.URLs, 0)
}

// The Go host tees the body into both PUTs (io.Pipe) and commits both
// ETags; a failing mirror still commits the object.
func TestEdgeMirror_TheGoHostSendsTheBodyToBothLegs(t *testing.T) {
	f := setupEdgeFixture(t)
	m := f.addMirrorLeg()
	host := f.dataHost()
	host.Client = &http.Client{Transport: edgeFakeMux{f.drv, m}}
	body := testBytes(3 << 20)
	res, err := host.Write(context.Background(), f.signedHTTP(http.MethodPut, "/"+f.bucket+"/tee.bin", nil, body, nil), strings.NewReader(string(body)), int64(len(body)))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.Relay.Status, res.Relay.Body)
	host.WaitMirrors() // the settle runs after the answer (RP78-2)
	sk := res.Admit.StorageKey
	assert.True(t, f.drv.has(f.tenantID, f.container(), sk))
	assert.True(t, m.has(f.tenantID, f.container(), sk), "the same bytes on the leg")
	got := f.copies()
	require.Len(t, got, 1)
	assert.Equal(t, "ready", got[0].state)

	m.putFails.Store(true)
	res, err = host.Write(context.Background(), f.signedHTTP(http.MethodPut, "/"+f.bucket+"/tee2.bin", nil, body, nil), strings.NewReader(string(body)), int64(len(body)))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.Relay.Status, "a failing mirror never fails the client's PUT")
	host.WaitMirrors() // the settle runs after the answer (RP78-2)
	_, ok := f.head("tee2.bin")
	assert.True(t, ok)
	var state string
	require.NoError(t, f.db.QueryRow(`SELECT state FROM blob_copies WHERE tenant_id = $1 AND storage_key = $2`, f.tenantID, res.Admit.StorageKey).Scan(&state))
	assert.Equal(t, "pending", state)
}

// A codec write mirrors its STORED chunks: two links per miss, two ETags
// per stored chunk, one copy row per chunk at the chunk's one address.
func TestEdgeMirror_ACodecWriteMirrorsItsStoredChunks(t *testing.T) {
	f := setupCodecFixture(t)
	m := f.addMirrorLeg()
	host := f.dataHost()
	host.Client = &http.Client{Transport: edgeFakeMux{f.drv, m}}
	body := crypto.GoldenBody(4801, 12<<20)
	res, err := host.Write(context.Background(), f.signedHTTP(http.MethodPut, "/"+f.bucket+"/codec.bin", nil, body, nil), strings.NewReader(string(body)), int64(len(body)))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.Relay.Status, res.Relay.Body)
	require.NotEmpty(t, res.Index.Misses)
	for _, miss := range res.Index.Misses {
		require.Len(t, miss.URLs, 2, "two links per miss")
		assert.Equal(t, miss.URL, miss.URLs[0])
		u, _ := url.Parse(miss.URLs[1])
		assert.Equal(t, "mirror.backend.invalid", u.Host)
	}
	got := f.copies()
	require.Len(t, got, len(res.Index.Misses), "one copy row per stored chunk")
	for _, c := range got {
		assert.Equal(t, "ready", c.state)
		assert.True(t, strings.HasPrefix(c.storageKey, "_chunks/"+f.tenantID+"/"), c.storageKey)
		assert.True(t, m.has("_global", "_global", c.storageKey), "the sealed chunk on the leg")
	}
	w := f.originGet("codec.bin", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, body, w.Body.Bytes())
}

// --- the blob_copies job (step 6) ---

func (f *edgeFixture) copiesJob() *BlobCopiesJob {
	j := newBlobCopiesJob(f.srv)
	j.JobName, j.onlyTenant = "blob_copies_test_"+f.tenantID, f.tenantID
	return j
}

func (f *edgeFixture) copyState(storageKey string) string {
	f.t.Helper()
	var st string
	err := f.db.QueryRow(`SELECT state FROM blob_copies WHERE tenant_id = $1 AND storage_key = $2`, f.tenantID, storageKey).Scan(&st)
	if err != nil {
		return ""
	}
	return st
}

// A copy the edge could not write is written from the primary by the job
// (once past its 5 min of grace), checked by MD5, then ready.
func TestBlobCopiesJob_APendingCopyIsWrittenFromThePrimary(t *testing.T) {
	f := setupEdgeFixture(t)
	m := f.addMirrorLeg()
	body := testBytes(70_000)
	resp := f.admit(f.signed("/"+f.bucket+"/p.bin", nil, body, nil, ""))
	a := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	relay, _ := f.commit(okCommit(resp.AdmitID, body, a)) // the mirror's ETag never came
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	require.Equal(t, "pending", f.copyState(resp.StorageKey))

	j := f.copiesJob()
	res, err := j.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Zero(t, res.Copied, "younger than 5 min: the edge may still be writing it")

	f.exec(`UPDATE blob_copies SET created_at = NOW() - interval '6 minutes' WHERE tenant_id = $1`, f.tenantID)
	res, err = j.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Copied, "%+v", res)
	assert.Equal(t, "ready", f.copyState(resp.StorageKey))
	assert.True(t, m.has(f.tenantID, f.container(), resp.StorageKey), "the bytes on the leg")

	// A primary that holds other bytes than the row says: never ready.
	resp = f.admit(f.signed("/"+f.bucket+"/q.bin", nil, body, nil, ""))
	a = f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	relay, _ = f.commit(okCommit(resp.AdmitID, body, a))
	require.Equal(t, http.StatusOK, relay.Status)
	f.drv.tamper(t, "q.bin")
	f.exec(`UPDATE blob_copies SET created_at = NOW() - interval '6 minutes' WHERE tenant_id = $1`, f.tenantID)
	res, err = j.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Failed)
	assert.Equal(t, "pending", f.copyState(resp.StorageKey))
	assert.False(t, m.has(f.tenantID, f.container(), resp.StorageKey), "the wrong copy removed")
}

// DELETE removes the copy with the object (the known copies); the job
// then finds the row orphaned and drops it.
func TestBlobCopiesJob_ADeleteRemovesBothAndTheRowGoes(t *testing.T) {
	f := setupEdgeFixture(t)
	m := f.addMirrorLeg()
	body := testBytes(9000)
	resp := f.admit(f.signed("/"+f.bucket+"/d.bin", nil, body, nil, ""))
	a := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	b := m.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	c := okCommit(resp.AdmitID, body, a)
	c.BackendETags = []string{a, b}
	relay, _ := f.commit(c)
	require.Equal(t, http.StatusOK, relay.Status)
	require.Equal(t, "ready", f.copyState(resp.StorageKey))

	w := f.s3(http.MethodDelete, "/"+f.bucket+"/d.bin", nil, nil)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	assert.False(t, f.drv.has(f.tenantID, f.container(), resp.StorageKey))
	assert.False(t, m.has(f.tenantID, f.container(), resp.StorageKey), "DELETE removes the leg's copy too")

	f.exec(`UPDATE blob_copies SET updated_at = NOW() - interval '2 hours' WHERE tenant_id = $1`, f.tenantID)
	res, err := f.copiesJob().RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Orphaned)
	assert.Equal(t, 1, res.Deleted)
	assert.Empty(t, f.copyState(resp.StorageKey), "the row gone")
}

// Turning edge_mirror on backfills: every whole object of the tenant gets
// its copy (origin writes, everything written before), vault never.
func TestBlobCopiesJob_BackfillCopiesWhatTheTenantHolds(t *testing.T) {
	f := setupEdgeFixture(t)
	m := f.addMirrorLeg()
	body := testBytes(12_000)
	f.exec(`DELETE FROM feature_flags WHERE tenant_id = $1 AND flag_key = $2`, f.tenantID, flagEdgeMirror)
	require.NoError(t, f.fl.Refresh(context.Background()))
	resp := f.admit(f.signed("/"+f.bucket+"/old.bin", nil, body, nil, ""))
	a := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	relay, _ := f.commit(okCommit(resp.AdmitID, body, a))
	require.Equal(t, http.StatusOK, relay.Status)
	require.Empty(t, f.copies(), "written while the flag was off: no copy")

	j := f.copiesJob()
	res, err := j.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Zero(t, res.Enqueued, "the flag is off")

	require.NoError(t, f.fl.Set(context.Background(), flagEdgeMirror, f.tenantID, true, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	res, err = j.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Enqueued)
	assert.Equal(t, 1, res.Copied, "due at once")
	assert.Equal(t, "ready", f.copyState(resp.StorageKey))
	assert.True(t, m.has(f.tenantID, f.container(), resp.StorageKey))

	res, err = j.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Zero(t, res.Enqueued, "one copy per blob")
}

// A codec chunk's copy whose index row is gone (the dedup GC, a commit that
// failed after it) is orphaned and removed from the leg.
func TestBlobCopiesJob_AChunkCopyNoIndexRowNamesIsRemoved(t *testing.T) {
	f := setupEdgeFixture(t)
	m := f.addMirrorLeg()
	sk := "_chunks/" + f.tenantID + "/" + strings.Repeat("ab", 32) + ".k1"
	m.workerPut("_global", "_global", sk, []byte("sealed"))
	require.NoError(t, recordBlobCopy(context.Background(), f.db, blobCopy{TenantID: f.tenantID, StorageKey: sk, Source: "edgefake", Leg: "edgemirror", State: copyReady, Size: 6}))
	f.exec(`UPDATE blob_copies SET updated_at = NOW() - interval '2 hours' WHERE tenant_id = $1`, f.tenantID)
	res, err := f.copiesJob().RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Orphaned)
	assert.Equal(t, 1, res.Deleted)
	assert.False(t, m.has("_global", "_global", sk))
	assert.Equal(t, "0123", fmt.Sprint(codecChunkKeyedOK(f.tenantID, "_chunks/"+f.tenantID+"/0123.k4")))
}

func codecChunkKeyedOK(tenantID, sk string) string {
	k, _ := codecChunkKeyed(tenantID, sk)
	return k
}

// --- the read (step 7) ---

// A read whose primary does not answer as admitted (the blob gone: a 404)
// is served from the ready mirror copy before the origin, and counted.
func TestEdgeMirror_AReadThePrimaryCannotServeComesFromTheMirror(t *testing.T) {
	f := setupEdgeFixture(t)
	f.wireEgress(1<<40, 0, nil)
	m := f.addMirrorLeg()
	host := f.dataHost()
	host.Client = &http.Client{Transport: edgeFakeMux{f.drv, m}}
	body := testBytes(40_000)
	res, err := host.Write(context.Background(), f.signedHTTP(http.MethodPut, "/"+f.bucket+"/r.bin", nil, body, nil), strings.NewReader(string(body)), int64(len(body)))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.Relay.Status)
	host.WaitMirrors() // the settle runs after the answer (RP78-2)
	sk := res.Admit.StorageKey
	require.Equal(t, "ready", f.copyState(sk))

	rr, err := host.Read(context.Background(), f.signedHTTP(http.MethodGet, "/"+f.bucket+"/r.bin", nil, nil, nil))
	require.NoError(t, err)
	require.Len(t, rr.Admit.URLs, 2, "the mirror's link beside the primary's")
	assert.Equal(t, map[string]string{"range": "bytes=0-39999"}, rr.Admit.MirrorSendHeaders, "the exact span, no If-Match")
	_ = rr.Body.Close()

	f.drv.mu.Lock()
	delete(f.drv.objects, "t-"+f.tenantID+"/"+f.container()+"/"+sk)
	f.drv.mu.Unlock()
	served := promtestutil.ToFloat64(blobCopiesReads.WithLabelValues("served_mirror"))
	rr, err = host.Read(context.Background(), f.signedHTTP(http.MethodGet, "/"+f.bucket+"/r.bin", nil, nil, nil))
	require.NoError(t, err)
	require.False(t, rr.PassThrough, "served from the mirror, not the origin")
	got, err := io.ReadAll(rr.Body)
	_ = rr.Body.Close()
	require.NoError(t, err)
	assert.Equal(t, body, got)
	assert.Equal(t, served+1, promtestutil.ToFloat64(blobCopiesReads.WithLabelValues("served_mirror")))

	// RP76-8: the lookup is the mirrored tenant's only — flag off, no link
	// (and no query), though the ready copy is still there.
	require.NoError(t, f.fl.Set(context.Background(), flagEdgeMirror, f.tenantID, false, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	f.drv.mu.Lock()
	f.drv.objects["t-"+f.tenantID+"/"+f.container()+"/"+sk] = body
	f.drv.mu.Unlock()
	rr, err = host.Read(context.Background(), f.signedHTTP(http.MethodGet, "/"+f.bucket+"/r.bin", nil, nil, nil))
	require.NoError(t, err)
	assert.Len(t, rr.Admit.URLs, 0, "edge_mirror off: no mirror link")
	if rr.Body != nil {
		_ = rr.Body.Close()
	}
	require.NoError(t, f.fl.Set(context.Background(), flagEdgeMirror, f.tenantID, true, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	f.drv.mu.Lock()
	delete(f.drv.objects, "t-"+f.tenantID+"/"+f.container()+"/"+sk)
	f.drv.mu.Unlock()

	// No ready copy: one link, and the origin takes a primary that fails.
	f.exec(`UPDATE blob_copies SET state = 'pending' WHERE tenant_id = $1`, f.tenantID)
	rr, err = host.Read(context.Background(), f.signedHTTP(http.MethodGet, "/"+f.bucket+"/r.bin", nil, nil, nil))
	require.NoError(t, err)
	assert.Len(t, rr.Admit.URLs, 0)
	assert.True(t, rr.PassThrough)
}

// routing_truth samples the mirror copies like head rows (kind="copy"):
// asked of their LEG, present / missing by the same rules.
func TestRoutingTruth_SamplesMirrorCopies(t *testing.T) {
	f := setupEdgeFixture(t)
	m := f.addMirrorLeg()
	host := f.dataHost()
	host.Client = &http.Client{Transport: edgeFakeMux{f.drv, m}}
	body := testBytes(9000)
	res, err := host.Write(context.Background(), f.signedHTTP(http.MethodPut, "/"+f.bucket+"/rt.bin", nil, body, nil), strings.NewReader(string(body)), int64(len(body)))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.Relay.Status)
	host.WaitMirrors() // the settle runs after the answer (RP78-2)

	c := NewRoutingTruthChecker(f.db, f.srv.engine, nil, zap.NewNop())
	c.scopeTenant, c.JobName = f.tenantID, "rt_copies_"+f.tenantID
	present := promtestutil.ToFloat64(routingCopyChecks.WithLabelValues("edgemirror", "present"))
	out, err := c.RunOnce(context.Background())
	require.NoError(t, err)
	require.NotNil(t, out.Copies["edgemirror"], "%+v", out)
	assert.Equal(t, 1, out.Copies["edgemirror"].Present)
	assert.Equal(t, present+1, promtestutil.ToFloat64(routingCopyChecks.WithLabelValues("edgemirror", "present")))

	m.mu.Lock()
	m.objects = map[string][]byte{}
	m.mu.Unlock()
	out, err = c.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, out.Copies["edgemirror"].Missing, "a ready copy the leg does not hold")
}

// A run whose time runs out in the copy pass (prod's first backfill run:
// the copies filled the 9 minutes and the orphan pass hit the deadline) is
// a note on an ok run, never an error — and the copy pass leaves the
// other passes their time.
func TestBlobCopiesJob_ARunOutOfTimeIsANoteNotAFailure(t *testing.T) {
	f := setupEdgeFixture(t)
	f.addMirrorLeg()
	body := testBytes(9000)
	resp := f.admit(f.signed("/"+f.bucket+"/late.bin", nil, body, nil, ""))
	a := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	relay, _ := f.commit(okCommit(resp.AdmitID, body, a))
	require.Equal(t, http.StatusOK, relay.Status)
	f.exec(`UPDATE blob_copies SET created_at = NOW() - interval '6 minutes' WHERE tenant_id = $1`, f.tenantID)

	// Less than the tail reserve left: the copy pass copies nothing.
	ctx, cancel := context.WithTimeout(context.Background(), blobCopiesTailReserve/2)
	defer cancel()
	res, err := f.copiesJob().RunOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, res.Copied, "no copy started inside the tail reserve")

	// The deadline already past: still no error.
	done, cancel2 := context.WithCancel(context.Background())
	cancel2()
	res, err = f.copiesJob().RunOnce(done)
	require.NoError(t, err)
	assert.NotEmpty(t, res.Stopped, "the run says it stopped")
}

// The copy pass copies several blobs at once (prod's backfill ran one at a
// time: ~250 small Lightroom files per 9-minute run).
func TestBlobCopiesJob_CopiesSeveralAtOnce(t *testing.T) {
	f := setupEdgeFixture(t)
	m := f.addMirrorLeg()
	body := testBytes(5000)
	for i := range 12 {
		resp := f.admit(f.signed(fmt.Sprintf("/%s/many-%d.bin", f.bucket, i), nil, body, nil, ""))
		a := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
		relay, _ := f.commit(okCommit(resp.AdmitID, body, a))
		require.Equal(t, http.StatusOK, relay.Status)
	}
	f.exec(`UPDATE blob_copies SET created_at = NOW() - interval '6 minutes' WHERE tenant_id = $1`, f.tenantID)
	var inFlight, peak atomic.Int32
	m.putHook = func() func() {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		return func() { inFlight.Add(-1) }
	}
	res, err := f.copiesJob().RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 12, res.Copied)
	assert.Greater(t, peak.Load(), int32(1), "copies overlap")
	assert.LessOrEqual(t, peak.Load(), int32(blobCopiesConcurrency))
}

// RP76-2: a host that cut its mirror for falling behind says so on the
// commit; the control plane counts it (vaultaire_blob_copies_total{outcome
// ="cut"}) and the copy is owed, as for any mirror that did not store.
func TestEdgeMirror_ACutMirrorIsCountedAndTheCopyOwed(t *testing.T) {
	f := setupEdgeFixture(t)
	f.addMirrorLeg()
	body := testBytes(9000)
	before := promtestutil.ToFloat64(blobCopiesTotal.WithLabelValues(blobCopyCut))
	resp := f.admit(f.signed("/"+f.bucket+"/cut.bin", nil, body, nil, ""))
	require.Len(t, resp.URLs, 2)
	a := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	c := okCommit(resp.AdmitID, body, a)
	c.BackendETags = []string{a}
	c.MirrorCut = true
	relay, _ := f.commit(c)
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	assert.Equal(t, before+1, promtestutil.ToFloat64(blobCopiesTotal.WithLabelValues(blobCopyCut)))
	var state string
	require.NoError(t, f.db.QueryRow(`SELECT state FROM blob_copies WHERE tenant_id = $1 AND storage_key = $2`, f.tenantID, resp.StorageKey).Scan(&state))
	assert.Equal(t, "pending", state)
}

// RP76-1 + RP76-6: the mirror leg must satisfy the bucket. A region-pinned
// bucket mirrors only to a leg of its own jurisdiction (an EU bucket today
// has none: no mirror, never a refusal); a strict-target tier is never
// mirrored; an unpinned bucket mirrors as before.
func TestMirrorLegFor_RespectsResidencyAndTier(t *testing.T) {
	f := setupEdgeFixture(t)
	f.addMirrorLeg()
	ctx := context.Background()
	t.Setenv("LYVE_REGION", "us-west-1")
	t.Setenv("IDRIVE_REGION", "us-central-1")

	// The pure rule.
	f.srv.mirrorLegs = []string{"edgemirror"}
	assert.Empty(t, f.srv.mirrorLegFor("idrive-eu-west-1", bucketScope{Region: "eu-west-1"}), "a leg of no known region is never a pinned bucket's")
	assert.Equal(t, "edgemirror", f.srv.mirrorLegFor("edgefake", bucketScope{}), "unpinned: as before")
	assert.Equal(t, "edgemirror", f.srv.mirrorLegFor("edgefake", bucketScope{Region: "us-central-1"}), "the default region is unpinned")
	for _, tier := range []string{"sia", "blomp", "sync"} {
		assert.Empty(t, f.srv.mirrorLegFor("edgefake", bucketScope{Tier: tier}), "a strict tier is never mirrored: %s", tier)
	}
	assert.Empty(t, f.srv.mirrorLegFor("blomp", bucketScope{}), "a strict placement is never mirrored")
	assert.Empty(t, f.srv.mirrorLegFor(rsBackendSentinel, bucketScope{}), "an rs object is never mirrored")

	// Legs with a region: lyve (LYVE_REGION) and idrive-<region>.
	lyve := newEdgeFakeDriver()
	lyve.name, lyve.urlHost = "lyve", "lyve.backend.invalid"
	f.srv.engine.AddDriver("lyve", lyve)
	eu := newEdgeFakeDriver()
	eu.name, eu.urlHost = "idrive-eu-central-1", "eu.backend.invalid"
	f.srv.engine.AddDriver("idrive-eu-central-1", eu)
	idr := newEdgeFakeDriver()
	idr.name, idr.urlHost = "idrive", "idrive.backend.invalid"
	f.srv.engine.AddDriver("idrive", idr)
	f.srv.mirrorLegs = []string{"lyve", "idrive-eu-central-1", "idrive"}
	assert.Equal(t, "idrive-eu-central-1", f.srv.mirrorLegFor("idrive-eu-west-1", bucketScope{Region: "eu-west-1"}), "the EU's own leg")
	assert.Equal(t, "lyve", f.srv.mirrorLegFor("idrive-us-west-2", bucketScope{Region: "us-west-2"}), "a US pin: a US leg")
	assert.Equal(t, "idrive", f.srv.mirrorLegFor("lyve", bucketScope{Tier: "resilient"}), "a us-central-1 resilient bucket mirrors to idrive (never to the EU leg listed first)")
	assert.Equal(t, "idrive", f.srv.mirrorLegFor("lyve", bucketScope{Region: "us-central-1", Tier: "resilient"}))
	f.srv.mirrorLegs = []string{"lyve", "idrive"}
	assert.Empty(t, f.srv.mirrorLegFor("idrive-eu-west-1", bucketScope{Region: "eu-west-1"}), "no EU leg: a single copy")

	// The admit: an EU-pinned bucket's write gets one link and no copy row.
	f.srv.mirrorLegs = []string{"edgemirror"}
	f.srv.engine.AddDriver("idrive-eu-west-1", f.drv)
	f.exec(`INSERT INTO buckets (tenant_id, name, visibility, region) VALUES ($1, 'edge-eu', 'private', 'eu-west-1')`, f.tenantID)
	body := testBytes(9000)
	resp := f.admit(f.signed("/edge-eu/eu.bin", nil, body, nil, ""))
	require.True(t, resp.Edge, "%+v", resp)
	assert.Len(t, resp.URLs, 0, "no mirror link")
	a := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	relay, _ := f.commit(okCommit(resp.AdmitID, body, a))
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	assert.Empty(t, f.copies(), "an EU bucket is a single copy until an EU leg exists")
	assert.Equal(t, "", f.srv.mirrorFor(ctx, f.tenantID, "edge-eu", "idrive-eu-west-1"))
	assert.Equal(t, "edgemirror", f.srv.mirrorFor(ctx, f.tenantID, f.bucket, "edgefake"))
}

// RP76-1 + RP76-6: the backfill never enqueues what cannot or must not be
// mirrored — an rs head row, a strict-target row, a strict-tier bucket's
// object, an EU-pinned bucket's object with no EU leg.
func TestBlobCopiesJob_BackfillSkipsRSStrictAndPinnedRows(t *testing.T) {
	f := setupEdgeFixture(t)
	f.addMirrorLeg()
	f.exec(`INSERT INTO buckets (tenant_id, name, visibility, tier_preference) VALUES ($1, 'edge-blomp', 'private', 'blomp')`, f.tenantID)
	f.exec(`INSERT INTO buckets (tenant_id, name, visibility, region) VALUES ($1, 'edge-eu2', 'private', 'eu-west-1')`, f.tenantID)
	for _, r := range []struct{ bucket, key, backend string }{
		{f.bucket, "rs.bin", rsBackendSentinel},
		{f.bucket, "strict.bin", "blomp"},
		{"edge-blomp", "tier.bin", "edgefake"},
		{"edge-eu2", "eu.bin", "edgefake"},
		{f.bucket, "plain.bin", "edgefake"},
	} {
		f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor)
			VALUES ($1, $2, $3, 10, 'e807f1fcf82d132f9bb018ca6738a19f', $4, 'standard')`, f.tenantID, r.bucket, r.key, r.backend)
	}
	// A pending row from before the fix whose source is no driver (an rs
	// object's sentinel): stale, never skipped for ever.
	f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor)
		VALUES ($1, $2, 'old-rs.bin', 10, 'e807f1fcf82d132f9bb018ca6738a19f', 'rs', 'standard')`, f.tenantID, f.bucket)
	f.exec(`INSERT INTO blob_copies (tenant_id, bucket, object_key, storage_key, source_backend, leg, kind, state, etag, size, created_at)
		VALUES ($1, $2, 'old-rs.bin', 'old-rs.bin', 'rs', 'edgemirror', 'mirror', 'pending', '', 10, NOW() - interval '1 hour')`, f.tenantID, f.bucket)
	res, err := f.copiesJob().RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Enqueued, "only plain.bin")
	assert.Zero(t, res.Skipped)
	got := f.copies()
	var keys []string
	for _, c := range got {
		if c.state != "stale" {
			keys = append(keys, c.storageKey)
		}
	}
	assert.Equal(t, []string{"plain.bin"}, keys, "the rs row's copy is stale (or gone), never pending")
}

// pendingCopies writes n mirrored objects whose mirror PUT failed: n
// pending copies, due at once.
func (f *edgeFixture) pendingCopies(n, size int, prefix string) {
	f.t.Helper()
	body := testBytes(size)
	for i := range n {
		resp := f.admit(f.signed(fmt.Sprintf("/%s/%s-%d.bin", f.bucket, prefix, i), nil, body, nil, ""))
		require.True(f.t, resp.Edge, "%+v", resp)
		a := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
		c := okCommit(resp.AdmitID, body, a)
		c.BackendETags = []string{a}
		relay, _ := f.commit(c)
		require.Equal(f.t, http.StatusOK, relay.Status, relay.Body)
	}
	f.exec(`UPDATE blob_copies SET created_at = NOW() - interval '6 minutes' WHERE tenant_id = $1`, f.tenantID)
}

// RP76-4: the copy pass spends a source's free egress ((backend_plans'
// egress_free_multiple − 1, RP78-5) × what the last backend_capacity run
// counted stored there) and no more within a UTC month; what it copied
// this month is carried in its own job_runs.result. (Scaled: 10 KiB stored
// × (3 − 1) → 20 KiB of 9,000-byte copies, two of them.)
func TestBlobCopiesJob_StopsAtTheVendorsEgressBudget(t *testing.T) {
	f := setupEdgeFixture(t)
	f.addMirrorLeg()
	f.pendingCopies(5, 9000, "eg")
	capJob := "test_cap_for_copies_" + f.tenantID
	raw, err := json.Marshal(BackendCapacityResult{Backends: map[string]*BackendBytes{"edgefake": {Objects: 10 << 10, Total: 10 << 10}}})
	require.NoError(t, err)
	f.exec(`INSERT INTO job_runs (job, last_outcome, result) VALUES ($1, 'ok', $2)`, capJob, raw)
	mult := 3.0
	require.NoError(t, upsertBackendPlan(context.Background(), f.db, BackendPlan{Backend: "edgefake", CapacityBytes: 1 << 40, EgressFreeMultiple: &mult,
		TermStart: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), TermEnd: time.Date(2027, 10, 20, 0, 0, 0, 0, time.UTC)}))
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM job_runs WHERE job = $1`, capJob)
		_, _ = f.db.Exec(`DELETE FROM backend_plans WHERE backend = 'edgefake'`)
	})

	j := f.copiesJob()
	j.capacityJob = capJob
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM job_runs WHERE job = $1`, j.JobName) })
	res, err := j.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, res.Copied, "2 × 9,000 fits in 20,480; a third would not")
	assert.Equal(t, 3, res.Skipped)
	require.NotNil(t, res.Egress)
	assert.Equal(t, int64(18_000), res.Egress.Spent["edgefake"])
	assert.Contains(t, strings.Join(res.Errors, " ")+res.Stopped+res.EgressNote, "egress")

	// The next run in the same month reads what this one spent.
	raw, err = json.Marshal(res)
	require.NoError(t, err)
	f.exec(`INSERT INTO job_runs (job, last_outcome, result) VALUES ($1, 'ok', $2) ON CONFLICT (job) DO UPDATE SET result = EXCLUDED.result`, j.JobName, raw)
	res, err = j.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Zero(t, res.Copied, "the month's budget is spent")

	// A new UTC month: the budget is whole again.
	j.now = func() time.Time { return time.Now().AddDate(0, 1, 0) }
	res, err = j.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, res.Copied)
}

// RP76-4 / RB6-10: a run copies batch after batch while rows are pending
// and its time remains — it never idles at a fixed count.
func TestBlobCopiesJob_LoopsWhileTimeRemains(t *testing.T) {
	f := setupEdgeFixture(t)
	f.addMirrorLeg()
	f.pendingCopies(25, 1000, "loop")
	j := f.copiesJob()
	j.copyBatch = 10
	res, err := j.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 25, res.Copied, "three batches of 10 in one run")
	var pending int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM blob_copies WHERE tenant_id = $1 AND state = 'pending'`, f.tenantID).Scan(&pending))
	assert.Zero(t, pending)
}

// settle is POST /api/v1/edge/mirror.
func (f *edgeFixture) settle(req dataplane.MirrorSettle) (dataplane.MirrorSettleResponse, int) {
	f.t.Helper()
	w := f.call("/api/v1/edge/mirror", req, f.token)
	var resp dataplane.MirrorSettleResponse
	if w.Code == http.StatusOK {
		require.NoError(f.t, json.Unmarshal(w.Body.Bytes(), &resp))
	}
	return resp, w.Code
}

// RP78-2: a mirrored PUT commits at the primary's answer (mirror_settle),
// its copy pending; the mirror's settle after the commit makes it ready.
// A DELETE between the commit and the settle leaves no ready row: the
// leg's bytes are queued for the collector on the leg. A second settle is
// idempotent; an unknown admit is 404.
func TestEdgeMirror_SettleAfterCommit(t *testing.T) {
	f := setupEdgeFixture(t)
	m := f.addMirrorLeg()
	body := testBytes(9000)

	resp := f.admit(f.signed("/"+f.bucket+"/settle.bin", nil, body, nil, ""))
	require.Len(t, resp.URLs, 2)
	a := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	c := okCommit(resp.AdmitID, body, a)
	c.BackendETags = []string{a}
	c.MirrorSettle = true
	relay, _ := f.commit(c)
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	assert.Equal(t, "pending", f.copyState(resp.StorageKey), "owed until the settle")

	b := m.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	before := promtestutil.ToFloat64(blobCopiesTotal.WithLabelValues(blobCopySettled))
	got, code := f.settle(dataplane.MirrorSettle{AdmitID: resp.AdmitID, ETag: b})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "ready", got.State)
	assert.Equal(t, "ready", f.copyState(resp.StorageKey))
	assert.Equal(t, before+1, promtestutil.ToFloat64(blobCopiesTotal.WithLabelValues(blobCopySettled)))

	got, code = f.settle(dataplane.MirrorSettle{AdmitID: resp.AdmitID, ETag: b})
	require.Equal(t, http.StatusOK, code, "idempotent")
	assert.Equal(t, "ready", got.State)
	assert.Equal(t, before+1, promtestutil.ToFloat64(blobCopiesTotal.WithLabelValues(blobCopySettled)), "counted once")

	// A DELETE between the commit and the settle.
	resp = f.admit(f.signed("/"+f.bucket+"/gone.bin", nil, body, nil, ""))
	a = f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	c = okCommit(resp.AdmitID, body, a)
	c.BackendETags = []string{a}
	c.MirrorSettle = true
	relay, _ = f.commit(c)
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	w := f.s3(http.MethodDelete, "/"+f.bucket+"/gone.bin", nil, nil)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	b = m.workerPut(f.tenantID, f.container(), resp.StorageKey, body) // the mirror lands after
	got, code = f.settle(dataplane.MirrorSettle{AdmitID: resp.AdmitID, ETag: b})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "retired", got.State)
	assert.NotEqual(t, "ready", f.copyState(resp.StorageKey), "no ready copy of a deleted object")
	var reason string
	require.NoError(t, f.db.QueryRow(`SELECT reason FROM retired_blobs WHERE backend_id = 'edgemirror' AND storage_key = $1`, resp.StorageKey).Scan(&reason),
		"the leg's address queued on the leg")
	assert.Equal(t, retiredOrphanWrite, reason)

	// A cut mirror: nothing to settle, the copy owed.
	resp = f.admit(f.signed("/"+f.bucket+"/cut2.bin", nil, body, nil, ""))
	a = f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	c = okCommit(resp.AdmitID, body, a)
	c.BackendETags = []string{a}
	c.MirrorSettle = true
	relay, _ = f.commit(c)
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	cutBefore := promtestutil.ToFloat64(blobCopiesTotal.WithLabelValues(blobCopyCut))
	got, code = f.settle(dataplane.MirrorSettle{AdmitID: resp.AdmitID, Cut: true})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "pending", got.State)
	assert.Equal(t, cutBefore+1, promtestutil.ToFloat64(blobCopiesTotal.WithLabelValues(blobCopyCut)))

	_, code = f.settle(dataplane.MirrorSettle{AdmitID: "00000000-0000-0000-0000-000000000000", ETag: b})
	assert.Equal(t, http.StatusNotFound, code)
}

// RP78-1: a host that sent a mirrored PUT unmirrored for want of budget
// says so; the control plane counts it and the copy is owed.
func TestEdgeMirror_AnUnbudgetedMirrorIsCountedAndTheCopyOwed(t *testing.T) {
	f := setupEdgeFixture(t)
	f.addMirrorLeg()
	body := testBytes(9000)
	before := promtestutil.ToFloat64(blobCopiesTotal.WithLabelValues(blobCopyUnbudgeted))
	resp := f.admit(f.signed("/"+f.bucket+"/nb.bin", nil, body, nil, ""))
	a := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	c := okCommit(resp.AdmitID, body, a)
	c.BackendETags = []string{a}
	c.MirrorUnbudgeted = true
	relay, _ := f.commit(c)
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	assert.Equal(t, before+1, promtestutil.ToFloat64(blobCopiesTotal.WithLabelValues(blobCopyUnbudgeted)))
	assert.Equal(t, "pending", f.copyState(resp.StorageKey))
}

// RP78-2: the Go host answers at the primary's pace and settles the mirror
// after (WaitMirrors); the copy ends ready.
func TestEdgeMirror_TheGoHostSettlesAfterTheAnswer(t *testing.T) {
	f := setupEdgeFixture(t)
	m := f.addMirrorLeg()
	host := f.dataHost()
	release := make(chan struct{})
	m.answerHook = func() { <-release }
	host.Client = &http.Client{Transport: edgeFakeMux{f.drv, m}}
	body := testBytes(1 << 20)
	res, err := host.Write(context.Background(), f.signedHTTP(http.MethodPut, "/"+f.bucket+"/late.bin", nil, body, nil), strings.NewReader(string(body)), int64(len(body)))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.Relay.Status, res.Relay.Body)
	assert.Equal(t, "pending", f.copyState(res.Admit.StorageKey), "the client answered before the mirror")
	close(release)
	host.WaitMirrors()
	assert.Equal(t, "ready", f.copyState(res.Admit.StorageKey))
}

// RP78-4: a Paris bucket's copy never goes to a London leg (eu-west-3 is
// the UK); a London bucket mirrors only to a UK leg.
func TestMirrorLegFor_LondonIsNotTheEU(t *testing.T) {
	f := setupEdgeFixture(t)
	t.Setenv("IDRIVE_REGION", "us-central-1")
	ldn := newEdgeFakeDriver()
	ldn.name, ldn.urlHost = "idrive-eu-west-3", "ldn.backend.invalid"
	f.srv.engine.AddDriver(ldn.name, ldn)
	fra := newEdgeFakeDriver()
	fra.name, fra.urlHost = "idrive-eu-central-1", "fra.backend.invalid"
	f.srv.engine.AddDriver(fra.name, fra)

	f.srv.mirrorLegs = []string{"idrive-eu-west-3"}
	assert.Empty(t, f.srv.mirrorLegFor("idrive-eu-west-4", bucketScope{Region: "eu-west-4"}), "Paris never mirrors to London")
	f.srv.mirrorLegs = []string{"idrive-eu-west-3", "idrive-eu-central-1"}
	assert.Equal(t, "idrive-eu-central-1", f.srv.mirrorLegFor("idrive-eu-west-4", bucketScope{Region: "eu-west-4"}), "an EU leg")
	assert.Empty(t, f.srv.mirrorLegFor("idrive-eu-west-3", bucketScope{Region: "eu-west-3"}), "London has no other UK leg")
	assert.Equal(t, "uk", jurisdiction("eu-west-3"))
	t.Setenv("LYVE_REGION", "us-east-1")
	assert.Equal(t, "us", jurisdiction(legRegion("lyve")))
}

// RP78-5: the copy pass spends only the free egress above one multiple of
// what the source stores — the customers' own GETs keep 1× stored before
// the vendor's overage rate. Multiple 3, stored 10 KiB: the pass stops at
// 20 KiB of copies.
func TestBlobCopiesJob_LeavesTheCustomersTheirEgress(t *testing.T) {
	f := setupEdgeFixture(t)
	f.addMirrorLeg()
	f.pendingCopies(5, 5000, "cust")
	capJob := "test_cap_for_customers_" + f.tenantID
	raw, err := json.Marshal(BackendCapacityResult{Backends: map[string]*BackendBytes{"edgefake": {Objects: 10 << 10, Total: 10 << 10}}})
	require.NoError(t, err)
	f.exec(`INSERT INTO job_runs (job, last_outcome, result) VALUES ($1, 'ok', $2)`, capJob, raw)
	mult := 3.0
	require.NoError(t, upsertBackendPlan(context.Background(), f.db, BackendPlan{Backend: "edgefake", CapacityBytes: 1 << 40, EgressFreeMultiple: &mult,
		TermStart: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), TermEnd: time.Date(2027, 10, 20, 0, 0, 0, 0, time.UTC)}))
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM job_runs WHERE job = $1`, capJob)
		_, _ = f.db.Exec(`DELETE FROM backend_plans WHERE backend = 'edgefake'`)
	})
	j := f.copiesJob()
	j.capacityJob = capJob
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM job_runs WHERE job = $1`, j.JobName) })
	res, err := j.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 4, res.Copied, "4 × 5,000 = 20,000 fits in (3 − 1) × 10,240 = 20,480; a fifth would not")
	assert.Equal(t, 1, res.Skipped)
	assert.Equal(t, int64(20_000), res.Egress.Spent["edgefake"])
}
