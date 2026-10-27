package api

// WP-EDGE-2 Part A: the codec's control plane proven by a Go harness that
// PLAYS THE WORKER — it chunks with the Go chunker, hashes, asks
// /api/v1/edge/index, seals the misses with the Go 0x06 sealer under the
// admit's public key, puts them where the presigned links point
// (edgeFakeDriver) and commits; then the ORIGIN reads the object back
// MD5-equal through its own chunked GET. This harness is Part B's oracle:
// the Worker 0.3.0 must produce the same rows.

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- the S3 content digest
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/sdk/go/e2ee"
)

// codecTestMaster is the LAB master key of the harness (the prompt's rule:
// slice 2 is proven on a lab key; prod stays dark without one).
const codecTestMaster = "7f3a1c5e9b2d4f6a8c0e2a4c6e8f0b1d3f5a7c9e1b3d5f7a9c0e2b4d6f8a1c3e"

type codecFixture struct {
	*edgeFixture
	keys *crypto.TenantKeys
	logs *observer.ObservedLogs
}

// setupCodecFixture is the edge fixture with the codec's dependencies: the
// content index, the tenant keys on the lab master, a small chunk threshold
// and the chunking + edge_codec flags for the tenant. The server's logs are
// observed (TestIndexHoldsNoPlainHash reads them).
func setupCodecFixture(t *testing.T) *codecFixture {
	t.Helper()
	f := setupEdgeFixture(t)
	core, logs := observer.New(zap.DebugLevel)
	f.srv.logger = zap.New(core)
	keys, err := crypto.NewTenantKeys(f.db, codecTestMaster, "")
	require.NoError(t, err)
	f.srv.tenantKeys = keys
	f.srv.gci = crypto.NewGlobalContentIndex(f.db)
	f.srv.chunkEncSvc = crypto.NewChunkEncryptionService(keys)
	f.srv.sseService = crypto.NewSSEService(keys)
	f.srv.chunkingThreshold = 256 << 10
	// EDGE_MIN_BYTES is the codec's threshold at the edge (RP68-1): a small
	// one here, so the tests move a few MiB.
	f.srv.edge.minBytes = 256 << 10
	// A hit's wait on a young claim (RP68-2): 60 s in production; the
	// harness's dead-host ladder would wait it out.
	f.srv.edge.codecHitWait = 3 * time.Second
	require.NoError(t, f.fl.Set(context.Background(), flagEdgeCodec, f.tenantID, true, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM tenant_chunk_refs WHERE tenant_id = $1`,
			`DELETE FROM object_metadata WHERE tenant_id = $1`,
			`DELETE FROM global_content_index WHERE dedup_scope = $1`,
			`DELETE FROM multipart_parts WHERE upload_id IN (SELECT upload_id FROM multipart_uploads WHERE tenant_id = $1)`,
			`DELETE FROM tenant_key_versions WHERE tenant_id = $1`,
		} {
			_, _ = f.db.Exec(q, f.tenantID)
		}
	})
	return &codecFixture{edgeFixture: f, keys: keys, logs: logs}
}

// index calls POST /api/v1/edge/index as the host would.
func (f *codecFixture) index(admitID string, chunks []crypto.Chunk) edgeIndexResponse {
	f.t.Helper()
	req := edgeIndexRequest{AdmitID: admitID}
	seen := map[string]bool{}
	for _, c := range chunks {
		if seen[c.Hash] {
			continue // one ask per distinct hash, as the pipeline would
		}
		seen[c.Hash] = true
		req.Chunks = append(req.Chunks, struct {
			Hash string `json:"hash"`
			Size int64  `json:"size"`
		}{Hash: c.Hash, Size: int64(c.Size)})
	}
	b, err := json.Marshal(req)
	require.NoError(f.t, err)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/edge/index", bytes.NewReader(b))
	r.Header.Set(edgeTokenHeader, f.token)
	w := httptest.NewRecorder()
	f.srv.handleEdgeIndex(w, r)
	require.Equal(f.t, http.StatusOK, w.Code, w.Body.String())
	var resp edgeIndexResponse
	require.NoError(f.t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp
}

// storeMisses seals each missed chunk as a 0x06 blob under the grant's
// public key and puts it where its presigned link points (the fake
// backend); returns the commit entries for every chunk in body order.
func (f *codecFixture) storeMisses(grant *edgeCodecGrant, chunks []crypto.Chunk, idx edgeIndexResponse) []edgeCommitChunk {
	f.t.Helper()
	return f.storeMissesWith(grant, chunks, idx, func(int) []byte { return grant.KEMPublicKey })
}

// storeMissesWith is storeMisses with the public key each stored chunk is
// sealed under chosen by its position among the stored ones (n = 0 is the
// first stored chunk) — a host that seals under the wrong key (RP70-3).
func (f *codecFixture) storeMissesWith(grant *edgeCodecGrant, chunks []crypto.Chunk, idx edgeIndexResponse, pubFor func(n int) []byte) []edgeCommitChunk {
	f.t.Helper()
	missURL := map[string]edgeIndexMiss{}
	for _, m := range idx.Misses {
		missURL[m.Hash] = m
	}
	stored := map[string]string{} // hash → backend MD5 ETag of the sealed bytes
	var out []edgeCommitChunk
	for _, c := range chunks {
		if etag, done := stored[c.Hash]; done {
			_ = etag
			out = append(out, edgeCommitChunk{Hash: c.Hash, Size: int64(c.Size), Status: "hit"})
			continue
		}
		m, miss := missURL[c.Hash]
		if !miss {
			out = append(out, edgeCommitChunk{Hash: c.Hash, Size: int64(c.Size), Status: "hit"})
			continue
		}
		sealer, err := crypto.SealWrappedChunk(pubFor(len(stored)), grant.Version, bytes.NewReader(c.Data), int64(c.Size))
		require.NoError(f.t, err)
		var sealed bytes.Buffer
		_, err = sealed.ReadFrom(sealer)
		require.NoError(f.t, err)
		require.Equal(f.t, m.SealedLength, int64(sealed.Len()), "the sealed length the link signed")
		// The link's key: everything after /vaultaire/t-_global/_global/
		// (the fake's URL shape).
		u, err := url.Parse(m.URL)
		require.NoError(f.t, err)
		key := strings.TrimPrefix(u.Path, "/vaultaire/t-_global/_global/")
		require.True(f.t, strings.HasPrefix(key, "_chunks/"+f.tenantID+"/"), "the one address under the tenant's keyed prefix: %s", u.Path)
		etag := strings.Trim(f.drv.workerPut("_global", "_global", key, sealed.Bytes()), `"`)
		stored[c.Hash] = etag
		out = append(out, edgeCommitChunk{Hash: c.Hash, Size: int64(c.Size), SealedSize: int64(sealed.Len()), BackendETag: etag, Status: "stored"})
	}
	return out
}

// uploadCodec runs the whole host pipeline for one PUT and returns the
// commit's relay and the index response.
func (f *codecFixture) uploadCodec(key string, body []byte) (*edgeRelay, edgeIndexResponse, edgeAdmitResponse) {
	f.t.Helper()
	resp := f.admit(f.signed("/"+f.bucket+"/"+key, nil, body, nil, ""))
	require.True(f.t, resp.Edge, "%+v", resp)
	require.NotNil(f.t, resp.Codec, "a codec admit")
	require.Empty(f.t, resp.URL, "no presigned body PUT: the host chunks and seals")
	chunks := f.chunksOf(body)
	idx := f.index(resp.AdmitID, chunks)
	entries := f.storeMisses(resp.Codec, chunks, idx)
	sum := md5.Sum(body) // #nosec G401
	relay, code := f.commit(edgeCommitRequest{AdmitID: resp.AdmitID, Outcome: edgeOutcomeOK,
		Chunks: entries, BodyMD5: hex.EncodeToString(sum[:])})
	require.Equal(f.t, http.StatusOK, code)
	return relay, idx, resp
}

func (f *codecFixture) chunksOf(body []byte) []crypto.Chunk {
	f.t.Helper()
	ch, err := crypto.DefaultChunker()
	require.NoError(f.t, err)
	chunks, err := ch.ChunkBytes(body)
	require.NoError(f.t, err)
	require.NotEmpty(f.t, chunks)
	return chunks
}

func TestEdgeCodecAdmit_Gates(t *testing.T) {
	f := setupCodecFixture(t)
	body := crypto.GoldenBody(1, 1<<20)
	path := "/" + f.bucket + "/gate.bin"

	// The flag off: the pass-through Part A gives a chunked placement.
	require.NoError(t, f.fl.Set(context.Background(), flagEdgeCodec, f.tenantID, false, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	resp := f.admit(f.signed(path, nil, body, nil, ""))
	assert.False(t, resp.Edge)
	assert.Equal(t, "placement_chunked", resp.Reason)
	require.NoError(t, f.fl.Set(context.Background(), flagEdgeCodec, f.tenantID, true, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))

	// The flag on with no key version on this process (prod without the
	// master key): Part A's whole PUT, never a refusal, never a plaintext
	// chunked write at the edge.
	saved := f.srv.tenantKeys
	f.srv.tenantKeys = nil
	resp = f.admit(f.signed(path, nil, body, nil, ""))
	assert.True(t, resp.Edge, "%+v", resp)
	assert.Nil(t, resp.Codec)
	assert.NotEmpty(t, resp.URL, "the whole PUT's presigned link")
	// Expire it through the reaper, so its reservation goes back.
	f.exec(`UPDATE edge_admits SET expires_at = NOW() - interval '1 minute' WHERE tenant_id = $1`, f.tenantID)
	_, rerr := f.srv.edgeReaper.RunOnce(context.Background())
	require.NoError(t, rerr)
	require.Zero(t, f.used())
	f.srv.tenantKeys = saved

	// The flag on with keys: the codec admit.
	resp = f.admit(f.signed(path, nil, body, nil, ""))
	require.True(t, resp.Edge, "%+v", resp)
	require.NotNil(t, resp.Codec)
	assert.Equal(t, 1, resp.Codec.Version)
	assert.Len(t, resp.Codec.KEMPublicKey, 1216, "the X-Wing public key")
	assert.Equal(t, int64(256<<10), resp.Codec.ChunkThreshold)
	assert.Equal(t, crypto.StreamSegmentSize, resp.Codec.SegmentSize)
	assert.Empty(t, resp.URL)
	assert.Equal(t, int64(len(body)), f.used(), "the write is reserved as a whole PUT's would be")

	// A body under EDGE_MIN_BYTES is the origin's, codec or not.
	small := f.admit(f.signed("/"+f.bucket+"/small.bin", nil, testBytes(8192), nil, ""))
	assert.False(t, small.Edge)
	assert.Equal(t, edgeReasonSmall, small.Reason)
}

func TestEdgeCodecHarness_EndToEnd(t *testing.T) {
	f := setupCodecFixture(t)
	body := crypto.GoldenBody(42, 3*(1<<20)+123456)
	chunks := f.chunksOf(body)
	sum := md5.Sum(body) // #nosec G401
	wantETag := hex.EncodeToString(sum[:])

	relay, idx, admitResp := f.uploadCodec("e2e.bin", body)
	assert.Equal(t, http.StatusOK, relay.Status, relay.Body)
	assert.Equal(t, `"`+wantETag+`"`, relay.Headers["Etag"])
	assert.Len(t, idx.Misses, len(chunks), "a first upload stores every chunk")
	assert.Empty(t, idx.Hits)

	// The rows are storeChunkedObject's for the same body (the control: the
	// SAME bytes through the origin's own chunked path at another key).
	ctl := f.originPut("control.bin", body, nil)
	require.Equal(t, http.StatusOK, ctl.Code, ctl.Body.String())
	h, ok := f.head("e2e.bin")
	require.True(t, ok)
	hc, ok := f.head("control.bin")
	require.True(t, ok)
	assert.Equal(t, hc.size, h.size)
	assert.Equal(t, hc.etag, h.etag, "both are the body's MD5")
	assert.Equal(t, chunkedBackendSentinel, h.backend)
	assert.Equal(t, hc.backend, h.backend)
	assert.Equal(t, hc.floor, h.floor)
	assert.Equal(t, admitResp.AdmitID, admitResp.AdmitID)
	assert.True(t, strings.HasPrefix(h.sk, "e2e.bin@"), "the manifest at its own address: %s", h.sk)
	var isChunked bool
	var encAlgo string
	require.NoError(t, f.db.QueryRow(`SELECT is_chunked, COALESCE(encryption_algorithm,'') FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'e2e.bin'`, f.tenantID).Scan(&isChunked, &encAlgo))
	assert.True(t, isChunked)
	assert.Equal(t, crypto.EdgeChunkEncryptionAlgo, encAlgo)
	// The manifests agree chunk for chunk: same count, offsets and the SAME
	// plaintext sizes (the frozen chunker), different identities (keyed
	// against plain) and seals (AES256-EW against AES256-CE) — on purpose.
	type refRow struct {
		idx    int
		offset int64
		hash   string
	}
	loadRefs := func(objectKey string) []refRow {
		var addr string
		require.NoError(t, f.db.QueryRow(`SELECT storage_key FROM object_head_cache WHERE tenant_id = $1 AND object_key = $2`, f.tenantID, objectKey).Scan(&addr))
		rows, err := f.db.Query(`SELECT chunk_index, chunk_offset, plaintext_hash FROM tenant_chunk_refs WHERE tenant_id = $1 AND object_key = $2 ORDER BY chunk_index`, f.tenantID, addr)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		var out []refRow
		for rows.Next() {
			var r refRow
			require.NoError(t, rows.Scan(&r.idx, &r.offset, &r.hash))
			out = append(out, r)
		}
		require.NoError(t, rows.Err())
		return out
	}
	edgeRefs, ctlRefs := loadRefs("e2e.bin"), loadRefs("control.bin")
	require.Equal(t, len(ctlRefs), len(edgeRefs), "the same chunk boundaries")
	for i := range edgeRefs {
		assert.Equal(t, ctlRefs[i].offset, edgeRefs[i].offset, "chunk %d", i)
		assert.NotEqual(t, ctlRefs[i].hash, edgeRefs[i].hash, "the codec row's identity is the KEYED hash, never the plain one")
	}

	// The origin reads it back, MD5-equal, through its own chunked GET.
	w := f.originGet("e2e.bin", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := md5.Sum(w.Body.Bytes()) // #nosec G401
	assert.Equal(t, wantETag, hex.EncodeToString(got[:]))
	assert.Equal(t, len(body), w.Body.Len())

	// A second upload of the same bytes: 100 % hits, 0 PUTs.
	puts := f.drv.presigns.Load()
	before := f.objectCount()
	indexHits := promtestutil.ToFloat64(edgeCodecChunks.WithLabelValues("hit"))
	referenced := promtestutil.ToFloat64(edgeCodecChunks.WithLabelValues("referenced"))
	relay2, idx2, _ := f.uploadCodec("e2e-again.bin", body)
	// RP68-4: hit is the INDEX's answer (once per distinct hash asked),
	// referenced the COMMIT's (once per chunk of the body) — never both
	// under one name.
	assert.Equal(t, indexHits+float64(len(idx2.Hits)), promtestutil.ToFloat64(edgeCodecChunks.WithLabelValues("hit")))
	assert.Equal(t, referenced+float64(len(chunks)), promtestutil.ToFloat64(edgeCodecChunks.WithLabelValues("referenced")))
	assert.Equal(t, http.StatusOK, relay2.Status)
	assert.Empty(t, idx2.Misses, "every chunk deduplicated")
	assert.Len(t, idx2.Hits, len(idx.Misses))
	assert.Equal(t, before, f.objectCount(), "no new blob on the backend")
	_ = puts
	var refCount int
	require.NoError(t, f.db.QueryRow(`SELECT MIN(ref_count) FROM global_content_index WHERE dedup_scope = $1 AND keyed_hash IS NOT NULL`, f.tenantID).Scan(&refCount))
	assert.Equal(t, 2, refCount, "both manifests hold their references")
	w2 := f.originGet("e2e-again.bin", nil)
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, len(body), w2.Body.Len())
}

// objectCount is the fake backend's blob count.
func (f *codecFixture) objectCount() int {
	f.drv.mu.Lock()
	defer f.drv.mu.Unlock()
	return len(f.drv.objects)
}

// TestIndexHoldsNoPlainHash: after a codec upload, sha256(chunk) appears in
// NO row of the index, the refs, the admit — and in no log line the server
// wrote (the prompt's grep of the schema, the zap fields and the metric
// labels; the metrics' labels are the fixed outcome words by construction).
func TestIndexHoldsNoPlainHash(t *testing.T) {
	f := setupCodecFixture(t)
	body := crypto.GoldenBody(7, 2*(1<<20)+999)
	chunks := f.chunksOf(body)
	relay, _, _ := f.uploadCodec("noplain.bin", body)
	require.Equal(t, http.StatusOK, relay.Status)

	rowsHold := func(table, column, scopeCol, scope string) []string {
		rows, err := f.db.Query(fmt.Sprintf(`SELECT %s FROM %s WHERE %s = $1`, column, table, scopeCol), scope) // #nosec G201 -- test-fixed identifiers
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var v string
			require.NoError(t, rows.Scan(&v))
			out = append(out, v)
		}
		require.NoError(t, rows.Err())
		return out
	}
	held := strings.Join(rowsHold("global_content_index", "plaintext_hash || '|' || COALESCE(keyed_hash,'') || '|' || COALESCE(storage_key,'')", "dedup_scope", f.tenantID), "\n") +
		strings.Join(rowsHold("tenant_chunk_refs", "plaintext_hash", "tenant_id", f.tenantID), "\n") +
		strings.Join(rowsHold("edge_admits", "attrs::text || COALESCE(result::text,'')", "tenant_id", f.tenantID), "\n")
	var logged strings.Builder
	for _, e := range f.logs.All() {
		logged.WriteString(e.Message)
		for _, field := range e.Context {
			fmt.Fprint(&logged, field.Key, "=", field.String, field.Interface)
		}
	}
	for _, c := range chunks {
		assert.NotContains(t, held, c.Hash, "a plain chunk hash stored")
		assert.NotContains(t, held, c.Hash[:16], "a plain chunk hash prefix stored")
		assert.NotContains(t, logged.String(), c.Hash[:16], "a plain chunk hash logged")
	}
	// And the keyed hashes ARE there: the index works.
	var keyedRows int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE dedup_scope = $1 AND keyed_hash = plaintext_hash AND keyed_hash IS NOT NULL`, f.tenantID).Scan(&keyedRows))
	assert.Equal(t, len(chunks), keyedRows)
}

// The index call's four answers (hit, miss/claim, a second host racing the
// same miss, the takeover) and Step 0.3: a host that dies mid-part leaves
// claims the next writer takes over after the window.
func TestEdgeCodecIndex_HitMissClaimTakeover(t *testing.T) {
	f := setupCodecFixture(t)
	body := crypto.GoldenBody(5, 1<<20+77)
	chunks := f.chunksOf(body)
	sum := md5.Sum(body) // #nosec G401
	bodyMD5 := hex.EncodeToString(sum[:])

	// Host 1 admits and indexes — then DIES (no commit): claims pending.
	a1 := f.admit(f.signed("/"+f.bucket+"/race.bin", nil, body, nil, ""))
	require.True(t, a1.Edge && a1.Codec != nil, "%+v", a1)
	idx1 := f.index(a1.AdmitID, chunks)
	require.Len(t, idx1.Misses, len(chunks))
	var pending int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE dedup_scope = $1 AND state = 'pending'`, f.tenantID).Scan(&pending))
	require.Equal(t, len(chunks), pending)

	// Host 2 (a second admit of the same bytes) races the same misses while
	// the claims are young: answered as HITS, nothing referenced.
	a2 := f.admit(f.signed("/"+f.bucket+"/race2.bin", nil, body, nil, ""))
	require.True(t, a2.Edge, "%+v", a2)
	idx2 := f.index(a2.AdmitID, chunks)
	assert.Empty(t, idx2.Misses, "a young claim is a hit: the commit validates")
	// Its commit's IncrementRef finds no ready row: the host retries.
	entries := make([]edgeCommitChunk, 0, len(chunks))
	for _, c := range chunks {
		entries = append(entries, edgeCommitChunk{Hash: c.Hash, Size: int64(c.Size), Status: "hit"})
	}
	relay, code := f.commit(edgeCommitRequest{AdmitID: a2.AdmitID, Outcome: edgeOutcomeOK, Chunks: entries, BodyMD5: bodyMD5})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, http.StatusServiceUnavailable, relay.Status, "a vanished hit is the commit's error: the host retries")
	assert.Equal(t, "failed", f.admitState(a2.AdmitID))

	// The takeover: the dead host's claims age past the window; a third
	// admit's index takes them over and stores.
	f.exec(`UPDATE global_content_index SET pending_since = pending_since - interval '21 minutes' WHERE dedup_scope = $1 AND state = 'pending'`, f.tenantID)
	a3 := f.admit(f.signed("/"+f.bucket+"/race3.bin", nil, body, nil, ""))
	require.True(t, a3.Edge, "%+v", a3)
	idx3 := f.index(a3.AdmitID, chunks)
	require.Len(t, idx3.Misses, len(chunks), "abandoned claims are taken over")
	entries3 := f.storeMisses(a3.Codec, chunks, idx3)
	relay3, _ := f.commit(edgeCommitRequest{AdmitID: a3.AdmitID, Outcome: edgeOutcomeOK, Chunks: entries3, BodyMD5: bodyMD5})
	require.Equal(t, http.StatusOK, relay3.Status, relay3.Body)
	w := f.originGet("race3.bin", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, len(body), w.Body.Len())

	// And now the chunks are READY: a fourth upload hits for real.
	a4 := f.admit(f.signed("/"+f.bucket+"/race4.bin", nil, body, nil, ""))
	require.True(t, a4.Edge)
	idx4 := f.index(a4.AdmitID, chunks)
	assert.Empty(t, idx4.Misses)
	relay4, _ := f.commit(edgeCommitRequest{AdmitID: a4.AdmitID, Outcome: edgeOutcomeOK, Chunks: entries, BodyMD5: bodyMD5})
	assert.Equal(t, http.StatusOK, relay4.Status, relay4.Body)
}

func TestEdgeCodecCommit_FailureAndExpiryDropEverything(t *testing.T) {
	f := setupCodecFixture(t)
	body := crypto.GoldenBody(9, 1<<20+5)
	chunks := f.chunksOf(body)

	// A failed host (edge_error): the reservation back, the claims dropped.
	a := f.admit(f.signed("/"+f.bucket+"/fail.bin", nil, body, nil, ""))
	require.True(t, a.Edge && a.Codec != nil)
	_ = f.index(a.AdmitID, chunks)
	used := f.used()
	require.Equal(t, int64(len(body)), used)
	relay, _ := f.commit(edgeCommitRequest{AdmitID: a.AdmitID, Outcome: edgeOutcomeEdgeError})
	assert.Equal(t, http.StatusServiceUnavailable, relay.Status)
	assert.Zero(t, f.used(), "the reservation given back")
	var left int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE dedup_scope = $1`, f.tenantID).Scan(&left))
	assert.Zero(t, left, "every claim dropped")

	// An expired admit: the reaper's rule — reservation back, claims
	// dropped, nothing queued for the blob collector.
	a2 := f.admit(f.signed("/"+f.bucket+"/expire.bin", nil, body, nil, ""))
	require.True(t, a2.Edge && a2.Codec != nil)
	_ = f.index(a2.AdmitID, chunks)
	f.exec(`UPDATE edge_admits SET expires_at = NOW() - interval '1 minute' WHERE id = $1`, a2.AdmitID)
	dropped := promtestutil.ToFloat64(edgeAdmitReaperTotal.WithLabelValues("claims_dropped"))
	res, err := f.srv.edgeReaper.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Expired)
	assert.Equal(t, 1, res.ClaimsDropped, "a codec admit's expiry drops claims (RP68-4)")
	assert.Zero(t, res.Retired, "nothing queued for the blob collector, so nothing retired")
	assert.Equal(t, dropped+1, promtestutil.ToFloat64(edgeAdmitReaperTotal.WithLabelValues("claims_dropped")))
	assert.Zero(t, f.used())
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE dedup_scope = $1`, f.tenantID).Scan(&left))
	assert.Zero(t, left)
	var queued int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM retired_blobs WHERE tenant_id = $1`, f.tenantID).Scan(&queued))
	assert.Zero(t, queued, "a codec admit holds no whole blob")
	// A late commit: 410, as every expired admit.
	_, code := f.commit(edgeCommitRequest{AdmitID: a2.AdmitID, Outcome: edgeOutcomeOK, BodyMD5: strings.Repeat("0", 32)})
	assert.Equal(t, http.StatusGone, code)
}

func TestEdgeCodecMultipart_ConcatenationAndOnePath(t *testing.T) {
	f := setupCodecFixture(t)
	uploadID := f.createUpload("mp-codec.zip")
	part1 := crypto.GoldenBody(21, 1<<20+333)
	part2 := crypto.GoldenBody(22, 1<<20+77)

	partCommit := func(n int, body []byte) string {
		f.t.Helper()
		q := url.Values{"uploadId": {uploadID}, "partNumber": {fmt.Sprint(n)}}
		resp := f.admit(f.signed("/"+f.bucket+"/mp-codec.zip", q, body, nil, ""))
		require.True(t, resp.Edge && resp.Codec != nil, "part %d: %+v", n, resp)
		require.Empty(t, resp.URL)
		chunks := f.chunksOf(body)
		idx := f.index(resp.AdmitID, chunks)
		entries := f.storeMisses(resp.Codec, chunks, idx)
		sum := md5.Sum(body) // #nosec G401
		relay, code := f.commit(edgeCommitRequest{AdmitID: resp.AdmitID, Outcome: edgeOutcomeOK, Chunks: entries, BodyMD5: hex.EncodeToString(sum[:])})
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, http.StatusOK, relay.Status, relay.Body)
		return relay.Headers["Etag"]
	}
	etag1 := partCommit(1, part1)
	etag2 := partCommit(2, part2)
	var codec bool
	require.NoError(t, f.db.QueryRow(`SELECT codec FROM multipart_uploads WHERE upload_id = $1`, uploadID).Scan(&codec))
	assert.True(t, codec)
	var partChunksSet int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM multipart_parts WHERE upload_id = $1 AND part_chunks IS NOT NULL`, uploadID).Scan(&partChunksSet))
	assert.Equal(t, 2, partChunksSet)

	// One upload is one path: a staged part is 400 with the codec sentence;
	// an edge-whole part admit is 400 too.
	w := f.s3(http.MethodPut, "/"+f.bucket+"/mp-codec.zip?uploadId="+uploadID+"&partNumber=3", testBytes(4096), nil)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "edge codec")
	// An edge-whole part admit on a codec upload (a process with the
	// edge_codec flag OFF — the generic part path): 400, one upload is one
	// path.
	require.NoError(t, f.fl.Set(context.Background(), flagEdgeCodec, f.tenantID, false, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	small := f.admit(f.signed("/"+f.bucket+"/mp-codec.zip", url.Values{"uploadId": {uploadID}, "partNumber": {"4"}}, testBytes(8<<20), nil, ""))
	require.NoError(t, f.fl.Set(context.Background(), flagEdgeCodec, f.tenantID, true, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	require.NotNil(t, small.Refusal, "%+v", small)
	assert.Contains(t, small.Refusal.Body, "edge codec")

	// Complete: the parts' chunk lists concatenated into one manifest; the
	// ETag is the parts' -N.
	completeXML := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, etag1, etag2)
	wc := f.s3(http.MethodPost, "/"+f.bucket+"/mp-codec.zip?uploadId="+uploadID, []byte(completeXML), nil)
	require.Equal(t, http.StatusOK, wc.Code, wc.Body.String())
	assert.Contains(t, wc.Body.String(), "-2", "the parts' -N ETag")
	h, ok := f.head("mp-codec.zip")
	require.True(t, ok)
	assert.Equal(t, int64(len(part1)+len(part2)), h.size)
	assert.Equal(t, chunkedBackendSentinel, h.backend)

	// Read back: the concatenation, byte for byte.
	rb := f.originGet("mp-codec.zip", nil)
	require.Equal(t, http.StatusOK, rb.Code, rb.Body.String())
	require.Equal(t, len(part1)+len(part2), rb.Body.Len())
	assert.True(t, bytes.Equal(part1, rb.Body.Bytes()[:len(part1)]))
	assert.True(t, bytes.Equal(part2, rb.Body.Bytes()[len(part1):]))

	// An ABORTED codec upload gives its parts' references back.
	upload2 := f.createUpload("mp-abort.zip")
	q := url.Values{"uploadId": {upload2}, "partNumber": {"1"}}
	resp := f.admit(f.signed("/"+f.bucket+"/mp-abort.zip", q, part1, nil, ""))
	require.True(t, resp.Edge && resp.Codec != nil, "%+v", resp)
	chunks := f.chunksOf(part1)
	idx := f.index(resp.AdmitID, chunks)
	entries := f.storeMisses(resp.Codec, chunks, idx)
	sum := md5.Sum(part1) // #nosec G401
	relay, _ := f.commit(edgeCommitRequest{AdmitID: resp.AdmitID, Outcome: edgeOutcomeOK, Chunks: entries, BodyMD5: hex.EncodeToString(sum[:])})
	require.Equal(t, http.StatusOK, relay.Status)
	refsOf := func() map[string]int {
		rows, err := f.db.Query(`SELECT plaintext_hash, ref_count FROM global_content_index WHERE dedup_scope = $1`, f.tenantID)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		out := map[string]int{}
		for rows.Next() {
			var h string
			var n int
			require.NoError(t, rows.Scan(&h, &n))
			out[h] = n
		}
		require.NoError(t, rows.Err())
		return out
	}
	before := refsOf()
	wa := f.s3(http.MethodDelete, "/"+f.bucket+"/mp-abort.zip?uploadId="+upload2, nil, nil)
	require.Equal(t, http.StatusNoContent, wa.Code, wa.Body.String())
	// The abort released inline (or the reaper would): every ref the part
	// held is back where it was before the part.
	time.Sleep(50 * time.Millisecond)
	after := refsOf()
	for h, n := range after {
		assert.LessOrEqual(t, n, before[h], "no ref gained by an abort: %s", h)
	}
	var withChunks int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM multipart_parts p JOIN multipart_uploads u ON u.upload_id = p.upload_id
		WHERE u.upload_id = $1 AND p.part_chunks IS NOT NULL`, upload2).Scan(&withChunks))
	assert.Zero(t, withChunks, "the aborted parts' lists are cleared with their references")
}

// --- Prompt 46b Part 0: the RP68 fixes ---

// fixedChunks cuts body into size-byte chunks: the control plane trusts the
// host's boundaries (only their sum is checked), so a test may cut many
// small distinct chunks without running the chunker over a large body.
func fixedChunks(body []byte, size int) []crypto.Chunk {
	var out []crypto.Chunk
	for off := 0; off < len(body); off += size {
		end := min(off+size, len(body))
		sum := sha256.Sum256(body[off:end])
		out = append(out, crypto.Chunk{Data: body[off:end], Hash: hex.EncodeToString(sum[:]), Size: end - off, Offset: int64(off), Index: len(out)})
	}
	return out
}

// codecCommit runs index → store → commit for one admit over the given
// chunks (the harness's pipeline with the caller's boundaries).
func (f *codecFixture) codecCommit(resp edgeAdmitResponse, body []byte, chunks []crypto.Chunk) *edgeRelay {
	f.t.Helper()
	idx := f.index(resp.AdmitID, chunks)
	entries := f.storeMisses(resp.Codec, chunks, idx)
	sum := md5.Sum(body) // #nosec G401
	relay, code := f.commit(edgeCommitRequest{AdmitID: resp.AdmitID, Outcome: edgeOutcomeOK, Chunks: entries, BodyMD5: hex.EncodeToString(sum[:])})
	require.Equal(f.t, http.StatusOK, code)
	return relay
}

// RP68-1: at the edge every write >= EDGE_MIN_BYTES of a codec tenant is the
// codec's — a 4–64 MiB whole PUT and every part of an aws-cli multipart
// (8 MiB parts), which the ORIGIN would store whole.
func TestEdgeCodecAdmit_EdgeRuleCoversWhatTheOriginStoresWhole(t *testing.T) {
	f := setupCodecFixture(t)
	f.srv.chunkingThreshold = 64 << 20 // the origin's default: these bodies are below it
	f.srv.edge.minBytes = 4 << 20

	// (a) a 12 MiB whole PUT: admitted codec, read back MD5-equal.
	body := crypto.GoldenBody(1201, 12<<20)
	relay, _, resp := f.uploadCodec("twelve.bin", body)
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	assert.Equal(t, int64(4<<20), resp.Codec.ChunkThreshold, "the grant's threshold is EDGE_MIN_BYTES")
	h, ok := f.head("twelve.bin")
	require.True(t, ok)
	assert.Equal(t, chunkedBackendSentinel, h.backend, "a chunked row at the edge where the origin would store it whole")
	w := f.originGet("twelve.bin", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, bytes.Equal(body, w.Body.Bytes()))

	// (b) a multipart upload whose first part is 8 MiB: admitted codec;
	// its Complete reads back.
	uploadID := f.createUpload("eight.zip")
	part1 := crypto.GoldenBody(1202, 8<<20)
	part2 := crypto.GoldenBody(1203, 6<<20)
	var etags []string
	for n, pb := range [][]byte{part1, part2} {
		q := url.Values{"uploadId": {uploadID}, "partNumber": {fmt.Sprint(n + 1)}}
		pr := f.admit(f.signed("/"+f.bucket+"/eight.zip", q, pb, nil, ""))
		require.True(t, pr.Edge, "%+v", pr)
		require.NotNil(t, pr.Codec, "part %d is the codec's: %+v", n+1, pr)
		r := f.codecCommit(pr, pb, f.chunksOf(pb))
		require.Equal(t, http.StatusOK, r.Status, r.Body)
		etags = append(etags, r.Headers["Etag"])
	}
	completeXML := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, etags[0], etags[1])
	wc := f.s3(http.MethodPost, "/"+f.bucket+"/eight.zip?uploadId="+uploadID, []byte(completeXML), nil)
	require.Equal(t, http.StatusOK, wc.Code, wc.Body.String())
	rb := f.originGet("eight.zip", nil)
	require.Equal(t, http.StatusOK, rb.Code, rb.Body.String())
	assert.True(t, bytes.Equal(append(append([]byte{}, part1...), part2...), rb.Body.Bytes()))

	// (c) a 2 MiB PUT (under EDGE_MIN_BYTES): pass_through_small as before.
	small := f.admit(f.signed("/"+f.bucket+"/two.bin", nil, crypto.GoldenBody(1204, 2<<20), nil, ""))
	assert.False(t, small.Edge)
	assert.Equal(t, edgeReasonSmall, small.Reason)
}

// A codec (or edge) upload's LAST part may be under EDGE_MIN_BYTES (S3 has
// no minimum for it): it must still be the edge's — a small part passed
// through to the origin's stager is refused on a one-path upload.
func TestEdgeCodecMultipart_SmallLastPartStaysOnTheEdge(t *testing.T) {
	f := setupCodecFixture(t)
	f.srv.chunkingThreshold = 64 << 20
	f.srv.edge.minBytes = 4 << 20
	uploadID := f.createUpload("tail.zip")
	part1 := crypto.GoldenBody(1301, 8<<20)
	part2 := crypto.GoldenBody(1302, 1<<20+5)
	var etags []string
	for n, pb := range [][]byte{part1, part2} {
		q := url.Values{"uploadId": {uploadID}, "partNumber": {fmt.Sprint(n + 1)}}
		pr := f.admit(f.signed("/"+f.bucket+"/tail.zip", q, pb, nil, ""))
		require.True(t, pr.Edge, "part %d: %+v", n+1, pr)
		require.NotNil(t, pr.Codec, "part %d: %+v", n+1, pr)
		r := f.codecCommit(pr, pb, f.chunksOf(pb))
		require.Equal(t, http.StatusOK, r.Status, r.Body)
		etags = append(etags, r.Headers["Etag"])
	}
	completeXML := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, etags[0], etags[1])
	wc := f.s3(http.MethodPost, "/"+f.bucket+"/tail.zip?uploadId="+uploadID, []byte(completeXML), nil)
	require.Equal(t, http.StatusOK, wc.Code, wc.Body.String())
	rb := f.originGet("tail.zip", nil)
	require.Equal(t, http.StatusOK, rb.Code)
	assert.Equal(t, len(part1)+len(part2), rb.Body.Len())
}

// RP68-2: parallel parts of ONE upload sharing a chunk — the part holding
// the hit commits FIRST, while the claimer has not; it waits for the claim
// to flip ready and succeeds, never a 503.
func TestEdgeCodecCommit_HitWaitsForAYoungClaim(t *testing.T) {
	f := setupCodecFixture(t)
	uploadID := f.createUpload("shared.zip")
	body := crypto.GoldenBody(1401, 2<<20+999)
	chunks := f.chunksOf(body)
	sum := md5.Sum(body) // #nosec G401
	bodyMD5 := hex.EncodeToString(sum[:])

	admitPart := func(n int) edgeAdmitResponse {
		q := url.Values{"uploadId": {uploadID}, "partNumber": {fmt.Sprint(n)}}
		pr := f.admit(f.signed("/"+f.bucket+"/shared.zip", q, body, nil, ""))
		require.True(t, pr.Edge && pr.Codec != nil, "part %d: %+v", n, pr)
		return pr
	}
	p1 := admitPart(1)
	idx1 := f.index(p1.AdmitID, chunks)
	require.Len(t, idx1.Misses, len(chunks), "part 1 claims every chunk")
	p2 := admitPart(2)
	idx2 := f.index(p2.AdmitID, chunks)
	require.Empty(t, idx2.Misses, "part 2 sees part 1's young claims as hits")

	hits := make([]edgeCommitChunk, 0, len(chunks))
	for _, c := range chunks {
		hits = append(hits, edgeCommitChunk{Hash: c.Hash, Size: int64(c.Size), Status: "hit"})
	}
	type result struct {
		relay *edgeRelay
		code  int
	}
	done := make(chan result, 1)
	go func() {
		w := f.call("/api/v1/edge/commit", edgeCommitRequest{AdmitID: p2.AdmitID, Outcome: edgeOutcomeOK, Chunks: hits, BodyMD5: bodyMD5}, f.token)
		var relay edgeRelay
		_ = json.Unmarshal(w.Body.Bytes(), &relay)
		done <- result{&relay, w.Code}
	}()
	release := make(chan struct{})
	go func() { time.Sleep(200 * time.Millisecond); close(release) }()
	<-release
	select {
	case r := <-done:
		t.Fatalf("part 2's commit answered before part 1 stored its chunks: %d %+v", r.code, r.relay)
	default:
	}
	entries := f.storeMisses(p1.Codec, chunks, idx1)
	relay1, code1 := f.commit(edgeCommitRequest{AdmitID: p1.AdmitID, Outcome: edgeOutcomeOK, Chunks: entries, BodyMD5: bodyMD5})
	require.Equal(t, http.StatusOK, code1)
	require.Equal(t, http.StatusOK, relay1.Status, relay1.Body)
	select {
	case r := <-done:
		require.Equal(t, http.StatusOK, r.code)
		assert.Equal(t, http.StatusOK, r.relay.Status, "the hit waited for the claim, never a 503: %s", r.relay.Body)
	case <-time.After(30 * time.Second):
		t.Fatal("part 2's commit never answered")
	}
}

// RP68-2: a hit whose claim is DROPPED (the claimer failed) fails at once.
func TestEdgeCodecCommit_HitOfADroppedClaimFailsAtOnce(t *testing.T) {
	f := setupCodecFixture(t)
	body := crypto.GoldenBody(1402, 1<<20+77)
	chunks := f.chunksOf(body)
	sum := md5.Sum(body) // #nosec G401
	a1 := f.admit(f.signed("/"+f.bucket+"/drop1.bin", nil, body, nil, ""))
	_ = f.index(a1.AdmitID, chunks)
	a2 := f.admit(f.signed("/"+f.bucket+"/drop2.bin", nil, body, nil, ""))
	require.Empty(t, f.index(a2.AdmitID, chunks).Misses)
	relay, _ := f.commit(edgeCommitRequest{AdmitID: a1.AdmitID, Outcome: edgeOutcomeEdgeError})
	require.Equal(t, http.StatusServiceUnavailable, relay.Status)
	hits := make([]edgeCommitChunk, 0, len(chunks))
	for _, c := range chunks {
		hits = append(hits, edgeCommitChunk{Hash: c.Hash, Size: int64(c.Size), Status: "hit"})
	}
	start := time.Now()
	relay2, _ := f.commit(edgeCommitRequest{AdmitID: a2.AdmitID, Outcome: edgeOutcomeOK, Chunks: hits, BodyMD5: hex.EncodeToString(sum[:])})
	assert.Equal(t, http.StatusServiceUnavailable, relay2.Status)
	assert.Less(t, time.Since(start), 5*time.Second, "a row that is gone is no wait")
}

// RP68-6: a hit whose size is not the row's is refused (it would shift every
// later chunk's offset in the manifest).
func TestEdgeCodecCommit_HitSizeMustBeTheRows(t *testing.T) {
	f := setupCodecFixture(t)
	body := crypto.GoldenBody(1501, 3<<20+11)
	chunks := fixedChunks(body, 1<<20)
	first := f.admit(f.signed("/"+f.bucket+"/size1.bin", nil, body, nil, ""))
	require.Equal(t, http.StatusOK, f.codecCommit(first, body, chunks).Status)

	a := f.admit(f.signed("/"+f.bucket+"/size2.bin", nil, body, nil, ""))
	require.Empty(t, f.index(a.AdmitID, chunks).Misses)
	hits := make([]edgeCommitChunk, 0, len(chunks))
	for _, c := range chunks {
		hits = append(hits, edgeCommitChunk{Hash: c.Hash, Size: int64(c.Size), Status: "hit"})
	}
	hits[0].Size++ // the total still adds up
	hits[1].Size--
	sum := md5.Sum(body) // #nosec G401
	relay, _ := f.commit(edgeCommitRequest{AdmitID: a.AdmitID, Outcome: edgeOutcomeOK, Chunks: hits, BodyMD5: hex.EncodeToString(sum[:])})
	assert.Equal(t, http.StatusBadRequest, relay.Status, relay.Body)
	assert.Contains(t, relay.Body, "InvalidRequest")
	_, ok := f.head("size2.bin")
	assert.False(t, ok)
}

// RP68-3 (a): the commit HEADs the first stored chunk and every 32nd after
// it on the backend; a mismatch refuses the commit and gives back
// everything.
func TestEdgeCodecCommit_HeadSampleOfStoredChunks(t *testing.T) {
	f := setupCodecFixture(t)
	body := crypto.GoldenBody(1601, 40<<20)
	chunks := fixedChunks(body, 1<<20)
	require.Len(t, chunks, 40)
	a := f.admit(f.signed("/"+f.bucket+"/forty.bin", nil, body, nil, ""))
	idx := f.index(a.AdmitID, chunks)
	entries := f.storeMisses(a.Codec, chunks, idx)
	f.drv.heads.Store(0)
	sum := md5.Sum(body) // #nosec G401
	f.drv.ranges.Store(0)
	f.drv.mu.Lock()
	f.drv.rangeLens = nil
	f.drv.mu.Unlock()
	relay, _ := f.commit(edgeCommitRequest{AdmitID: a.AdmitID, Outcome: edgeOutcomeOK, Chunks: entries, BodyMD5: hex.EncodeToString(sum[:])})
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	// RP70-3: the FIRST stored chunk is opened (one ranged read of its
	// header and first segment — these 1 MiB chunks are one segment each),
	// the 33rd is HEADed.
	assert.Equal(t, int32(1), f.drv.ranges.Load(), "one ranged read: the first stored chunk's prefix")
	f.drv.mu.Lock()
	assert.Equal(t, []int64{int64(crypto.StreamHeaderWrapped) + 1<<20 + 16}, f.drv.rangeLens, "the header and the first segment")
	f.drv.mu.Unlock()
	assert.Equal(t, int32(1), f.drv.heads.Load(), "the 33rd stored chunk is HEADed")

	// A host that reports another ETag than the backend holds.
	body2 := crypto.GoldenBody(1602, 3<<20)
	chunks2 := fixedChunks(body2, 1<<20)
	before := promtestutil.ToFloat64(edgeCodecChunks.WithLabelValues("head_mismatch"))
	usedBefore := f.used()
	a2 := f.admit(f.signed("/"+f.bucket+"/lie.bin", nil, body2, nil, ""))
	idx2 := f.index(a2.AdmitID, chunks2)
	entries2 := f.storeMisses(a2.Codec, chunks2, idx2)
	entries2[0].BackendETag = strings.Repeat("ab", 16)
	sum2 := md5.Sum(body2) // #nosec G401
	relay2, _ := f.commit(edgeCommitRequest{AdmitID: a2.AdmitID, Outcome: edgeOutcomeOK, Chunks: entries2, BodyMD5: hex.EncodeToString(sum2[:])})
	assert.Equal(t, http.StatusBadRequest, relay2.Status, relay2.Body)
	assert.Contains(t, relay2.Body, "InvalidRequest")
	assert.Equal(t, before+1, promtestutil.ToFloat64(edgeCodecChunks.WithLabelValues("head_mismatch")))
	assert.Equal(t, usedBefore, f.used(), "the reservation given back")
	var refsLeft int
	keyed := func(c crypto.Chunk) string {
		ik, _, err := f.keys.IndexKey(context.Background(), f.tenantID, a2.Codec.Version)
		require.NoError(t, err)
		plain, _ := hex.DecodeString(c.Hash)
		k, err := crypto.KeyedChunkHash(ik, plain)
		require.NoError(t, err)
		return k
	}
	for _, c := range chunks2 {
		var n int
		require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`, f.tenantID, keyed(c)).Scan(&n))
		refsLeft += n
	}
	assert.Zero(t, refsLeft, "every claim of the refused commit dropped, nothing referenced")
	_, ok := f.head("lie.bin")
	assert.False(t, ok)
}

// RP68-3 (c): routing_truth's chunked sample OPENS a codec chunk — a blob
// overwritten with other bytes of the same length is unreadable.
func TestRoutingTruth_CodecChunkOpenedNotJustRead(t *testing.T) {
	f := setupCodecFixture(t)
	body := crypto.GoldenBody(1701, 2<<20+3)
	relay, _, _ := f.uploadCodec("probe.bin", body)
	require.Equal(t, http.StatusOK, relay.Status)
	c := NewRoutingTruthChecker(f.db, f.srv.engine, f.srv.gci, zap.NewNop())
	require.NotNil(t, c)
	c.scopeTenant = f.tenantID
	c.JobName = "routing_truth_codec_test"
	c.tenantKeys = f.keys
	res, err := c.RunOnce(context.Background())
	require.NoError(t, err)
	require.Positive(t, res.Chunks.Chunks)
	assert.Zero(t, res.Chunks.Unreadable, "an intact codec chunk opens")

	var key string
	require.NoError(t, f.db.QueryRow(`SELECT storage_key FROM global_content_index WHERE dedup_scope = $1 AND keyed_hash IS NOT NULL ORDER BY storage_key LIMIT 1`, f.tenantID).Scan(&key))
	f.drv.mu.Lock()
	k := "t-_global/_global/" + key
	orig := f.drv.objects[k]
	require.NotEmpty(t, orig)
	f.drv.objects[k] = crypto.GoldenBody(99, len(orig))
	f.drv.mu.Unlock()
	res, err = c.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Chunks.Unreadable, "the poisoned codec chunk is found by the daily sample")
}

// RP68-5: the chunk move verifies a codec row by the MD5 its row records.
func TestChunkMove_CodecRowVerifiesByMD5(t *testing.T) {
	f := setupCodecFixture(t)
	body := crypto.GoldenBody(1801, 1<<20+9)
	relay, _, _ := f.uploadCodec("move.bin", body)
	require.Equal(t, http.StatusOK, relay.Status)
	m := NewChunkMover(f.db, f.srv.engine, zap.NewNop())
	require.NotNil(t, m)
	m.tenantsOverride = []string{f.tenantID} // the shared test DB: this tenant's prefix only
	res, err := m.Run(context.Background(), ChunkMoveOptions{Backend: "edgefake", DryRun: true})
	require.NoError(t, err)
	require.Positive(t, res.Rows)
	assert.Zero(t, res.Missing, "%+v", res)
	assert.Equal(t, res.Rows, res.AtAddress, "%+v", res)
}

// The same rule for an edge-WHOLE upload (Part A's): its small last part is
// the edge's — the origin's stager refuses a part of an edge upload.
func TestEdgeMultipart_SmallLastPartOfAnEdgeUploadStaysOnTheEdge(t *testing.T) {
	f := setupEdgeFixture(t)
	f.srv.edge.minBytes = 4096
	id := f.createUpload("whole-tail.zip")
	_, e1 := f.edgePart("whole-tail.zip", id, 1, testBytes(8192))
	_, e2 := f.edgePart("whole-tail.zip", id, 2, testBytes(100)) // under EDGE_MIN_BYTES
	completeXML := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`, e1, e2)
	wc := f.s3(http.MethodPost, "/"+f.bucket+"/whole-tail.zip?uploadId="+id, []byte(completeXML), nil)
	require.Equal(t, http.StatusOK, wc.Code, wc.Body.String())
}

// The lab's finding (Prompt 46b step 12 d): aws-cli sends every part of an
// upload at once, so the small TAIL (under EDGE_MIN_BYTES: the origin's,
// D-EDGE-5) may reach the origin's stager before any part marked the upload.
// Its reservation is on the upload row before its row exists: the first
// codec part must see it and pass through (staged_parts) — the upload stays
// ONE staged path, never codec parts beside a staged one.
func TestEdgeCodecMultipart_StagedTailInFlightKeepsTheUploadStaged(t *testing.T) {
	f := setupCodecFixture(t)
	f.srv.chunkingThreshold = 64 << 20
	f.srv.edge.minBytes = 4 << 20
	uploadID := f.createUpload("tail-first.zip")
	// The tail's staged reservation, taken (its body still arriving).
	reserved, err := reserveUploadPart(context.Background(), f.db, f.tenantID, uploadID, 2, 2<<20)
	require.NoError(t, err)
	require.Positive(t, reserved)
	head := crypto.GoldenBody(1352, 8<<20)
	q := url.Values{"uploadId": {uploadID}, "partNumber": {"1"}}
	pr := f.admit(f.signed("/"+f.bucket+"/tail-first.zip", q, head, nil, ""))
	assert.False(t, pr.Edge, "a staged part is in flight: the upload is the stager's: %+v", pr)
	assert.Equal(t, "staged_parts", pr.Reason)
	var codec bool
	require.NoError(t, f.db.QueryRow(`SELECT codec FROM multipart_uploads WHERE upload_id = $1`, uploadID).Scan(&codec))
	assert.False(t, codec, "never marked codec")
}

// One upload is one path, under the lock: a staged part that passed
// stageUploadPart's pre-check before the upload became codec is refused by
// its reservation (and, defensively, by its install) — never a staged part
// beside chunk lists.
func TestStagedPart_RefusedUnderTheLockOnceTheUploadIsCodec(t *testing.T) {
	f := setupCodecFixture(t)
	uploadID := f.createUpload("mixed.zip")
	body := crypto.GoldenBody(1361, 1<<20+5)
	q := url.Values{"uploadId": {uploadID}, "partNumber": {"1"}}
	pr := f.admit(f.signed("/"+f.bucket+"/mixed.zip", q, body, nil, ""))
	require.NotNil(t, pr.Codec, "%+v", pr)
	usedBefore := f.used()

	_, err := reserveUploadPart(context.Background(), f.db, f.tenantID, uploadID, 2, 4096)
	assert.ErrorIs(t, err, errUploadCodec, "the reservation is refused under the lock")
	installed := false
	err = settleUploadPart(context.Background(), f.db, f.tenantID, uploadID, 2, 0, 4096, `"`+strings.Repeat("0", 32)+`"`, "",
		func() error { installed = true; return nil })
	assert.ErrorIs(t, err, errUploadCodec, "and so is an install")
	assert.False(t, installed)
	assert.Equal(t, usedBefore, f.used(), "nothing reserved, nothing held")

	// Through the S3 API: 400 with the codec sentence.
	w := f.s3(http.MethodPut, "/"+f.bucket+"/mixed.zip?uploadId="+uploadID+"&partNumber=3", testBytes(4096), nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "edge codec")
}

// --- Prompt 46c Part 0: the RP70 fixes ---

// RP70-2 / D-EDGE-29: every UploadPart of an edge tenant is the edge's
// whatever its size — EDGE_MIN_BYTES gates PutObject only. A 2 MiB first
// part of a NEW upload (no part has marked it yet: the small-tail race of
// every ≤ 10-part aws-cli upload) is admitted codec, and an EMPTY last part
// (S3 allows one; RB3-5) is admitted and commits with an empty chunk list.
func TestEdgeCodecMultipart_EveryPartIsTheEdges(t *testing.T) {
	f := setupCodecFixture(t)
	f.srv.chunkingThreshold = 64 << 20
	f.srv.edge.minBytes = 4 << 20
	uploadID := f.createUpload("small-first.zip")
	part1 := crypto.GoldenBody(2901, 2<<20)

	q1 := url.Values{"uploadId": {uploadID}, "partNumber": {"1"}}
	p1 := f.admit(f.signed("/"+f.bucket+"/small-first.zip", q1, part1, nil, ""))
	require.True(t, p1.Edge, "a 2 MiB part of a new upload is the edge's (D-EDGE-29): %+v", p1)
	require.NotNil(t, p1.Codec, "admitted codec: %+v", p1)
	r1 := f.codecCommit(p1, part1, f.chunksOf(part1))
	require.Equal(t, http.StatusOK, r1.Status, r1.Body)

	q2 := url.Values{"uploadId": {uploadID}, "partNumber": {"2"}}
	p2 := f.admit(f.signed("/"+f.bucket+"/small-first.zip", q2, nil, nil, ""))
	require.True(t, p2.Edge, "an empty part of a codec upload is the edge's: %+v", p2)
	require.NotNil(t, p2.Codec, "%+v", p2)
	empty := md5.Sum(nil) // #nosec G401
	r2, code := f.commit(edgeCommitRequest{AdmitID: p2.AdmitID, Outcome: edgeOutcomeOK, Chunks: []edgeCommitChunk{}, BodyMD5: hex.EncodeToString(empty[:])})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, http.StatusOK, r2.Status, r2.Body)
	var list string
	var size int64
	require.NoError(t, f.db.QueryRow(`SELECT part_chunks::text, size_bytes FROM multipart_parts WHERE upload_id = $1 AND part_number = 2`, uploadID).Scan(&list, &size))
	assert.Equal(t, "[]", list, "an empty part records an empty chunk list")
	assert.Zero(t, size)

	completeXML := fmt.Sprintf(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part><Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>`,
		r1.Headers["Etag"], r2.Headers["Etag"])
	wc := f.s3(http.MethodPost, "/"+f.bucket+"/small-first.zip?uploadId="+uploadID, []byte(completeXML), nil)
	require.Equal(t, http.StatusOK, wc.Code, wc.Body.String())
	rb := f.originGet("small-first.zip", nil)
	require.Equal(t, http.StatusOK, rb.Code, rb.Body.String())
	assert.True(t, bytes.Equal(part1, rb.Body.Bytes()), "read back: part 1 then nothing")

	// A PutObject under EDGE_MIN_BYTES stays the origin's.
	small := f.admit(f.signed("/"+f.bucket+"/small.bin", nil, part1, nil, ""))
	assert.False(t, small.Edge)
	assert.Equal(t, edgeReasonSmall, small.Reason)
}

// RP70-3: a host that sealed the first stored chunk under ANOTHER public key
// passes a HEAD sample (the wrong-key blob has an ETag and size of its own)
// — the commit OPENS that chunk's header and first segment under the
// tenant's codec seed and refuses it (head_mismatch, everything given back).
// A well-formed commit reads exactly one prefix: the header, one segment
// and the opener's one look-ahead byte when the chunk has more segments.
func TestEdgeCodecCommit_FirstStoredChunkIsOpened(t *testing.T) {
	f := setupCodecFixture(t)
	body := crypto.GoldenBody(2911, 3<<20+77)
	chunks := fixedChunks(body, 3<<19) // 1.5 MiB chunks: two segments each
	sum := md5.Sum(body)               // #nosec G401
	bodyMD5 := hex.EncodeToString(sum[:])

	other, err := e2ee.PublicKey(bytes.Repeat([]byte{9}, 32))
	require.NoError(t, err)
	a := f.admit(f.signed("/"+f.bucket+"/wrongkey.bin", nil, body, nil, ""))
	require.NotNil(t, a.Codec, "%+v", a)
	idx := f.index(a.AdmitID, chunks)
	entries := f.storeMissesWith(a.Codec, chunks, idx, func(n int) []byte {
		if n == 0 {
			return other
		}
		return a.Codec.KEMPublicKey
	})
	before := promtestutil.ToFloat64(edgeCodecChunks.WithLabelValues("head_mismatch"))
	usedBefore := f.used()
	relay, _ := f.commit(edgeCommitRequest{AdmitID: a.AdmitID, Outcome: edgeOutcomeOK, Chunks: entries, BodyMD5: bodyMD5})
	assert.Equal(t, http.StatusBadRequest, relay.Status, relay.Body)
	assert.Contains(t, relay.Body, "head_mismatch")
	assert.Equal(t, before+1, promtestutil.ToFloat64(edgeCodecChunks.WithLabelValues("head_mismatch")))
	assert.Less(t, f.used(), usedBefore, "the reservation given back")
	_, ok := f.head("wrongkey.bin")
	assert.False(t, ok, "nothing committed")

	// The same bytes from an honest host: one ranged read of the prefix.
	f.drv.ranges.Store(0)
	f.drv.mu.Lock()
	f.drv.rangeLens = nil
	f.drv.mu.Unlock()
	a2 := f.admit(f.signed("/"+f.bucket+"/rightkey.bin", nil, body, nil, ""))
	require.NotNil(t, a2.Codec)
	r2 := f.codecCommit(a2, body, chunks)
	require.Equal(t, http.StatusOK, r2.Status, r2.Body)
	assert.Equal(t, int32(1), f.drv.ranges.Load())
	f.drv.mu.Lock()
	assert.Equal(t, []int64{int64(crypto.StreamHeaderWrapped) + 1<<20 + 16 + 1}, f.drv.rangeLens,
		"the header, the first segment and the opener's look-ahead byte (the chunk has a second segment)")
	f.drv.mu.Unlock()
	w := f.originGet("rightkey.bin", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, bytes.Equal(body, w.Body.Bytes()))
}

// RP70-4: an index call's claims are ONE transaction — the admit row
// locked, then one advisory lock per chunk taken in keyed-hash order (two
// calls sharing chunks can never deadlock), the claims and their record on
// the admit row, one commit; the links are presigned after it (no database
// work). Was: a transaction per chunk plus one to record the claims.
func TestEdgeIndex_OneTransactionPerCall(t *testing.T) {
	f := setupCodecFixture(t)
	db, log := openCountingDB(t)
	body := crypto.GoldenBody(2921, 64*(64<<10))
	chunks := fixedChunks(body, 64<<10)
	require.Len(t, chunks, 64)
	a := f.admit(f.signed("/"+f.bucket+"/sixtyfour.bin", nil, body, nil, ""))
	require.NotNil(t, a.Codec, "%+v", a)

	f.srv.db = db // the index call alone through the counting driver
	log.reset()
	idx := f.index(a.AdmitID, chunks)
	f.srv.db = f.db
	require.Len(t, idx.Misses, 64)
	assert.Equal(t, 1, log.beginCount(), "one transaction for the call's 64 claims")
	locks := log.advisoryLockArgs()
	require.Len(t, locks, 64, "one advisory lock per chunk")
	assert.True(t, sort.StringsAreSorted(locks), "taken in keyed-hash order")

	// The claims are recorded on the admit: the commit flips them.
	entries := f.storeMisses(a.Codec, chunks, idx)
	sum := md5.Sum(body) // #nosec G401
	r, _ := f.commit(edgeCommitRequest{AdmitID: a.AdmitID, Outcome: edgeOutcomeOK, Chunks: entries, BodyMD5: hex.EncodeToString(sum[:])})
	assert.Equal(t, http.StatusOK, r.Status, r.Body)
}
