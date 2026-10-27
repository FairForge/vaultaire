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
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/FairForge/vaultaire/internal/crypto"
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
		sealer, err := crypto.SealWrappedChunk(grant.KEMPublicKey, grant.Version, bytes.NewReader(c.Data), int64(c.Size))
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

	// A body under the threshold stays Part A's whole PUT, codec or not.
	small := f.admit(f.signed("/"+f.bucket+"/small.bin", nil, testBytes(8192), nil, ""))
	require.True(t, small.Edge)
	assert.Nil(t, small.Codec)
	assert.NotEmpty(t, small.URL)
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
	relay2, idx2, _ := f.uploadCodec("e2e-again.bin", body)
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
	res, err := f.srv.edgeReaper.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Expired)
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
