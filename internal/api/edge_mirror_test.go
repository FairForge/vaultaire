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
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/crypto"
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
	assert.Equal(t, "edgemirror", f.srv.mirrorLegFor("edgefake"), "unregistered and the placement itself are skipped")
	assert.Equal(t, "edgefake", f.srv.mirrorLegFor("edgemirror"))
	f.srv.mirrorLegs = []string{"edgefake"}
	assert.Empty(t, f.srv.mirrorLegFor("edgefake"), "no other leg: no mirror (never a refusal)")

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
