package api

// WP-EDGE-4 PR D (plan 10.3): the public catalogue. A `_public` chunk is the
// operator's seeded copy of public content: no tenant's reference keeps it,
// so dedup GC must never collect it, whatever its ref_count says.

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- the S3 ETag
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/catalogue"
	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/dataplane"
	"github.com/FairForge/vaultaire/internal/drivers"
)

func TestDedupGC_NeverCollectsAPublicChunk(t *testing.T) {
	runner, f := setupGCFixture(t)
	sum := sha256.Sum256([]byte("public chunk " + uuid.New().String()))
	hash := hex.EncodeToString(sum[:])
	_, err := f.db.Exec(`INSERT INTO global_content_index (dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes, encrypted, ref_count, state, marked_for_deletion, marked_at, last_accessed_at)
		VALUES ($1, $2, 'local', $3, 13, FALSE, 0, 'ready', TRUE, NOW() - interval '30 days', NOW() - interval '30 days')`,
		crypto.PublicDedupScope, hash, "_chunks/_public/"+hash)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`, crypto.PublicDedupScope, hash)
	})

	_, err = runner.RunOnce(context.Background())
	require.NoError(t, err)
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`,
		crypto.PublicDedupScope, hash).Scan(&n))
	assert.Equal(t, 1, n, "a seeded public chunk outlives every reference")
}

// The catalogue_filter job builds the filter from public_chunks and
// publishes catalogue/<version>.bloom and catalogue/latest on the r2
// backend under the reserved tenant _catalogue; without r2 it is a note.
func TestCatalogueFilterJob_PublishesTheFilter(t *testing.T) {
	f := setupEdgeFixture(t)
	r2 := drivers.NewLocalDriver(t.TempDir(), zap.NewNop())
	f.srv.engine.AddDriver("r2", r2)
	object := func(artifact string) []byte {
		rc, err := r2.Get(common.WithTenantID(context.Background(), catalogueTenant), catalogueContainer, artifact)
		if err != nil {
			return nil
		}
		defer func() { _ = rc.Close() }()
		b, _ := io.ReadAll(rc)
		return b
	}

	var id int64
	require.NoError(t, f.db.QueryRow(`INSERT INTO public_artefacts (name, sha256) VALUES ('test artefact', $1) RETURNING id`, uuid.New().String()).Scan(&id))
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM public_artefacts WHERE id = $1`, id) })
	var hashes []string
	for i := range 50 {
		s := sha256.Sum256([]byte(fmt.Sprintf("%d-%d", id, i)))
		hashes = append(hashes, hex.EncodeToString(s[:]))
		f.exec(`INSERT INTO public_chunks (artefact_id, chunk_index, plain_hash, size_bytes) VALUES ($1, $2, $3, 100)`, id, i, hashes[i])
	}

	j := newCatalogueFilterJob(f.srv)
	j.JobName = "catalogue_filter_test_" + f.tenantID
	res, err := j.RunOnce(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.Chunks, int64(50))
	require.NotZero(t, res.Version)

	latest := object("catalogue/latest")
	require.NotNil(t, latest)
	assert.Equal(t, fmt.Sprint(res.Version), string(latest))
	raw := object(fmt.Sprintf("catalogue/%d.bloom", res.Version))
	require.NotNil(t, raw)
	filter, err := catalogue.Unmarshal(raw)
	require.NoError(t, err)
	assert.Equal(t, res.Version, filter.Version())
	for _, h := range hashes {
		ok, err := filter.Contains(h)
		require.NoError(t, err)
		assert.True(t, ok)
	}
	assert.Equal(t, float64(res.Version), promtestutil.ToFloat64(catalogueFilterVersion))
}

// seedPublic seeds body's chunks as a public artefact the way
// cmd/tools/catalogue-seed does: rows, plaintext blobs at the one address
// on the fixture's backend, ready _public index rows.
func (f *codecFixture) seedPublic(body []byte) []crypto.Chunk {
	f.t.Helper()
	chunks := f.chunksOf(body)
	whole := sha256.Sum256(body)
	var id int64
	require.NoError(f.t, f.db.QueryRow(`INSERT INTO public_artefacts (name, sha256, size_bytes) VALUES ('lab artefact', $1, $2) RETURNING id`,
		hex.EncodeToString(whole[:]), len(body)).Scan(&id))
	f.t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM public_artefacts WHERE id = $1`, id)
		for _, c := range chunks {
			_, _ = f.db.Exec(`DELETE FROM global_content_index WHERE dedup_scope = '_public' AND plaintext_hash = $1`, c.Hash)
		}
	})
	for i, c := range chunks {
		f.exec(`INSERT INTO public_chunks (artefact_id, chunk_index, plain_hash, size_bytes) VALUES ($1, $2, $3, $4)`, id, i, c.Hash, c.Size)
		key := "_chunks/_public/" + c.Hash
		f.drv.workerPut("_global", "_global", key, c.Data)
		f.exec(`INSERT INTO global_content_index (dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes, encrypted, ref_count, state)
			VALUES ('_public', $1, 'edgefake', $2, $3, FALSE, 0, 'ready') ON CONFLICT DO NOTHING`, c.Hash, key, c.Size)
	}
	return chunks
}

// indexWith is an index call marking every chunk public (or not).
func (f *codecFixture) indexWith(admitID string, chunks []crypto.Chunk, public bool) edgeIndexResponse {
	f.t.Helper()
	req := edgeIndexRequest{AdmitID: admitID}
	seen := map[string]bool{}
	for _, c := range chunks {
		if seen[c.Hash] {
			continue
		}
		seen[c.Hash] = true
		req.Chunks = append(req.Chunks, dataplane.IndexChunk{Hash: c.Hash, Size: int64(c.Size), Public: public})
	}
	resp, code, err := f.srv.edgeIndex(context.Background(), req)
	require.NoError(f.t, err)
	require.Equal(f.t, http.StatusOK, code)
	return *resp
}

func TestCatalogue_ASeededChunkIsReferencedWithoutAnUpload(t *testing.T) {
	f := setupCodecFixture(t)
	body := crypto.GoldenBody(7001, 6<<20)
	chunks := f.seedPublic(body)
	resp := f.admit(f.signed("/"+f.bucket+"/public.img", nil, body, nil, ""))
	require.NotNil(t, resp.Codec, "%+v", resp)
	presigns := f.drv.presigns.Load()

	idx := f.indexWith(resp.AdmitID, chunks, true)
	assert.Empty(t, idx.Misses, "every chunk is the catalogue's")
	assert.Equal(t, presigns, f.drv.presigns.Load(), "no link minted: no upload")

	entries := make([]edgeCommitChunk, 0, len(chunks))
	for _, c := range chunks {
		entries = append(entries, edgeCommitChunk{Hash: c.Hash, Size: int64(c.Size), Status: "hit"})
	}
	sum := md5.Sum(body) // #nosec G401
	relay, code := f.commit(edgeCommitRequest{AdmitID: resp.AdmitID, Outcome: edgeOutcomeOK, Chunks: entries, BodyMD5: hex.EncodeToString(sum[:])})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)

	var public, refs int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id = $1 AND dedup_scope = '_public'`, f.tenantID).Scan(&public))
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(SUM(ref_count), 0) FROM global_content_index WHERE dedup_scope = '_public' AND plaintext_hash = $1`, chunks[0].Hash).Scan(&refs))
	assert.Equal(t, len(chunks), public, "the manifest references the one public copy")
	assert.GreaterOrEqual(t, refs, 1)
	var sealed int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE dedup_scope = '_public' AND plaintext_hash = $1 AND encrypted`, chunks[0].Hash).Scan(&sealed))
	assert.Zero(t, sealed, "a public chunk is never sealed")

	w := f.originGet("public.img", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, bytes.Equal(body, w.Body.Bytes()), "read back from the public copy")
}

// A false positive (the filter matched, the catalogue does not hold the
// chunk) is a miss: a claim in the TENANT's scope and a link — the chunk
// uploads, sealed, as any other; nothing enters _public.
func TestCatalogue_AFalsePositiveUploadsAndNothingEntersThePublicScope(t *testing.T) {
	f := setupCodecFixture(t)
	body := crypto.GoldenBody(7002, 6<<20)
	resp := f.admit(f.signed("/"+f.bucket+"/private.bin", nil, body, nil, ""))
	require.NotNil(t, resp.Codec)
	chunks := f.chunksOf(body)
	idx := f.indexWith(resp.AdmitID, chunks, true)
	assert.Len(t, idx.Misses, len(chunks), "not in the catalogue: every chunk a miss")
	for _, c := range chunks {
		var n int
		require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE dedup_scope = '_public' AND plaintext_hash = $1`, c.Hash).Scan(&n))
		assert.Zero(t, n, "a tenant never claims in _public")
	}
	entries := f.storeMisses(resp.Codec, chunks, idx)
	sum := md5.Sum(body) // #nosec G401
	relay, code := f.commit(edgeCommitRequest{AdmitID: resp.AdmitID, Outcome: edgeOutcomeOK, Chunks: entries, BodyMD5: hex.EncodeToString(sum[:])})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	w := f.originGet("private.bin", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, bytes.Equal(body, w.Body.Bytes()))

	// A host that says "hit" for a chunk it was never answered public
	// cannot reference the public copy.
	resp = f.admit(f.signed("/"+f.bucket+"/sneaky.bin", nil, body, nil, ""))
	other := f.seedPublic(crypto.GoldenBody(7003, 6<<20))
	bad := []edgeCommitChunk{{Hash: other[0].Hash, Size: int64(other[0].Size), Status: "hit"}}
	relay, _ = f.commit(edgeCommitRequest{AdmitID: resp.AdmitID, Outcome: edgeOutcomeOK, Chunks: bad, BodyMD5: hex.EncodeToString(sum[:])})
	assert.NotEqual(t, http.StatusOK, relay.Status, "a public reference only for a chunk the index answered public")
}
