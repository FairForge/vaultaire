package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// WP-R8-7 — one address for chunk blobs.
//
// The chunk container is shared and tenant-independent. On a fixed-bucket
// backend (prod's primary) it was not: the driver keys every object
// `t-<tenant>/<container>/<artifact>` with the tenant from the request
// context, so a chunk was stored under the prefix of whoever uploaded it
// first. A second tenant whose upload deduplicated against it got 200 on PUT
// and 500 on GET; dedup GC (no tenant in its context) deleted the index row
// and "deleted" a key under `t-default/`. No test saw it: every chunk test ran
// on the local driver, which keys by container only.
//
// These tests run on fixedBucketDriver with the request context as s3.go
// builds it (s3Ctx), under a backend name of their own — the index rows of a
// test are then the only rows on "its" backend in the shared test database.

type chunkAddrFixture struct {
	*adapterTestFixture
	t       *testing.T
	backend string // the fixed-bucket primary's name in this test
	a, b    *tenant.Tenant
	tracked map[chunkPair]bool
	tenants []string
}

func setupChunkAddrFixture(t *testing.T) *chunkAddrFixture {
	t.Helper()
	f := &chunkAddrFixture{adapterTestFixture: setupChunkingFixture(t), t: t, tracked: map[chunkPair]bool{}}
	f.backend = "fixed-" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
	f.eng.AddDriver(f.backend, f.fixed)
	f.eng.SetPrimary(f.backend)
	f.a = f.tenant
	f.tenants = []string{f.a.ID}
	f.b = f.newTenant("B")
	t.Cleanup(func() {
		for _, id := range f.tenants {
			for _, p := range tenantChunkPairs(t, f.db, id) {
				f.tracked[p] = true
			}
			cleanupTenantChunkRows(f.db, id, id)
			_, _ = f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, id)
			_, _ = f.db.Exec(`DELETE FROM object_locations WHERE tenant_id = $1`, id)
		}
		for p := range f.tracked {
			_, _ = f.db.Exec(`DELETE FROM global_content_index g
				WHERE g.dedup_scope = $1 AND g.plaintext_hash = $2 AND g.backend_id = $3
				  AND NOT EXISTS (SELECT 1 FROM tenant_chunk_refs r
					WHERE r.dedup_scope = g.dedup_scope AND r.plaintext_hash = g.plaintext_hash)`, p.scope, p.hash, f.backend)
		}
		for _, id := range f.tenants[1:] {
			_, _ = f.db.Exec(`DELETE FROM tenants WHERE id = $1`, id)
		}
	})
	return f
}

func (f *chunkAddrFixture) newTenant(name string) *tenant.Tenant {
	f.t.Helper()
	id := uuid.New().String()
	_, err := f.db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1, $2, $3, $4, $5)`,
		id, name, "chunk-"+id[:8]+"@test.local", "AKC"+id[:8], "SKC"+id[:8])
	require.NoError(f.t, err)
	f.tenants = append(f.tenants, id)
	return &tenant.Tenant{ID: id, Namespace: "tenant/" + id + "/"}
}

// do is one S3 request of a tenant, with the context s3.go builds.
func (f *chunkAddrFixture) do(method string, tn *tenant.Tenant, key string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/test-bucket/"+key, bytes.NewReader(body)).WithContext(s3Ctx(context.Background(), tn))
	req.ContentLength = int64(len(body))
	w := httptest.NewRecorder()
	switch method {
	case http.MethodPut:
		f.adapter.HandlePut(w, req, "test-bucket", key)
	case http.MethodGet:
		f.adapter.HandleGet(w, req, "test-bucket", key)
	default:
		f.adapter.HandleDelete(w, req, "test-bucket", key)
	}
	return w
}

// put uploads and returns the chunks of the object (tracked for cleanup).
func (f *chunkAddrFixture) put(tn *tenant.Tenant, key string, body []byte) []scopedChunk {
	f.t.Helper()
	require.Equal(f.t, http.StatusOK, f.do(http.MethodPut, tn, key, body).Code)
	chunks := tenantChunks(f.t, f.db, tn.ID, key)
	require.NotEmpty(f.t, chunks)
	for _, c := range chunks {
		f.tracked[chunkPair{c.scope, c.hash}] = true
	}
	return chunks
}

func (f *chunkAddrFixture) addr(c scopedChunk) chunkAddr {
	return chunkAddr{scope: c.scope, hash: c.hash, backend: f.backend, key: c.storageKey}
}

// chunkBlobs lists the chunk blobs on the backend, as the driver keys them.
func (f *chunkAddrFixture) chunkBlobs() []string {
	var out []string
	for _, b := range f.fixed.blobs() {
		if strings.Contains(b, "/"+chunkContainer+"/") {
			out = append(out, b)
		}
	}
	return out
}

func oneAddressKey(c scopedChunk) string {
	return "t-" + engine.ChunkAddressTenant + "/" + chunkContainer + "/" + c.storageKey
}

func legacyKey(tn *tenant.Tenant, c scopedChunk) string {
	return "t-" + tn.ID + "/" + chunkContainer + "/" + c.storageKey
}

// makeLegacy puts the blobs of the chunks where a build before WP-R8-7 wrote
// them: under the uploading tenant's prefix, and nowhere else.
func (f *chunkAddrFixture) makeLegacy(uploader *tenant.Tenant, chunks []scopedChunk) {
	f.t.Helper()
	for _, c := range chunks {
		from := f.fixed.path(engine.ChunkAddressTenant, chunkContainer, c.storageKey)
		to := f.fixed.path(uploader.ID, chunkContainer, c.storageKey)
		require.NoError(f.t, os.MkdirAll(filepath.Dir(to), 0o750))
		require.NoError(f.t, os.Rename(from, to))
	}
}

// recordWrites inserts the rows the engine writes for a Put (object_locations,
// keyed by the tenant in the context of the write). The fixture's engine has
// no database, so it records none itself.
func (f *chunkAddrFixture) recordWrites(writer *tenant.Tenant, chunks []scopedChunk) {
	f.t.Helper()
	for _, c := range chunks {
		_, err := f.db.Exec(`INSERT INTO object_locations (tenant_id, bucket, object_key, backend_name) VALUES ($1, $2, $3, $4)`,
			writer.ID, chunkContainer, c.storageKey, f.backend)
		require.NoError(f.t, err)
	}
}

func (f *chunkAddrFixture) gc() *DedupGCRunner {
	return NewDedupGCRunner(f.db, f.eng, f.adapter.gci, zap.NewNop())
}

// sweep runs dedup GC's sweep of exactly these chunks (never RunOnce: the
// reconcile and the candidate scan are table-wide on a shared database).
func (f *chunkAddrFixture) sweep(chunks []scopedChunk) []sweepOutcome {
	gc := f.gc()
	var out []sweepOutcome
	for _, c := range chunks {
		o, err := gc.sweepOne(context.Background(), c.scope, c.hash, f.backend, c.storageKey)
		assert.NoError(f.t, err)
		out = append(out, o)
	}
	return out
}

func pairsOf(chunks []scopedChunk) []chunkPair {
	var out []chunkPair
	for _, c := range chunks {
		out = append(out, chunkPair{c.scope, c.hash})
	}
	return out
}

// The red test of WP-R8-7 (kept skipped by WP-R10-3c; seen failing on main
// @ #555 with the Skip removed: tenant B's GET answered 500), extended.
func TestChunkBlobs_HaveOneAddressOnAFixedBucketBackend(t *testing.T) {
	// Arrange: a fixed-bucket primary, two tenants, the same bytes.
	f := setupChunkAddrFixture(t)
	content := generateTestData(8 * 1024)

	// Act 1: both upload; the second upload is a dedup hit.
	chunks := f.put(f.a, "a.bin", content)
	f.put(f.b, "b.bin", content)

	// Assert 1: one blob per chunk, at the one address — under neither
	// tenant's prefix — and BOTH tenants read what their PUT said was stored.
	var want []string
	for _, c := range chunks {
		want = append(want, oneAddressKey(c))
	}
	assert.ElementsMatch(t, want, f.chunkBlobs())
	for _, r := range []struct {
		tn  *tenant.Tenant
		key string
	}{{f.a, "a.bin"}, {f.b, "b.bin"}} {
		got := f.do(http.MethodGet, r.tn, r.key, nil)
		require.Equal(t, http.StatusOK, got.Code, "main @ #555: 500 for the second tenant — the chunk was under the first uploader's prefix")
		assert.Equal(t, content, got.Body.Bytes())
	}

	// Act 2: both delete. Inside the grace nothing is a sweep candidate (the
	// scan's `marked_at < now - grace`); past it, GC sweeps.
	require.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, f.a, "a.bin", nil).Code)
	require.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, f.b, "b.bin", nil).Code)
	var candidates int
	require.NoError(t, f.db.QueryRow(`
		SELECT COUNT(*) FROM global_content_index
		 WHERE backend_id = $1 AND marked_for_deletion AND ref_count = 0
		   AND marked_at < NOW() - make_interval(secs => $2)`, f.backend, int(f.gc().GracePeriod.Seconds())).Scan(&candidates))
	require.Zero(t, candidates, "released a moment ago: not a candidate inside the 7-day grace")
	require.Len(t, f.chunkBlobs(), len(chunks), "nothing is deleted before the grace has passed")
	backdateGCIRows(t, f.db, pairsOf(chunks))
	outcomes := f.sweep(chunks)

	// Assert 2: the blob is gone FROM THE BACKEND, not only from the index
	// (main @ #555: rows deleted, blob still under t-<uploader>/_global/).
	for _, o := range outcomes {
		assert.Equal(t, sweepDeleted, o)
	}
	assert.Zero(t, gciRowsFor(t, f.db, pairsOf(chunks)))
	assert.Empty(t, f.chunkBlobs(), "the last reference went and the grace passed: the blob is deleted on the backend")

	// Act 3 + Assert 3: a third upload of the same bytes stores it again.
	c := f.newTenant("C")
	again := f.put(c, "c.bin", content)
	assert.ElementsMatch(t, want, f.chunkBlobs())
	assert.Equal(t, len(chunks), len(again))
	got := f.do(http.MethodGet, c, "c.bin", nil)
	require.Equal(t, http.StatusOK, got.Code)
	assert.Equal(t, content, got.Body.Bytes())
}

func TestDedupGC_NeverDeletesAChunkAnotherTenantStillReferences(t *testing.T) {
	// Arrange: A and B share the chunks; A deletes its object.
	f := setupChunkAddrFixture(t)
	content := generateTestData(8 * 1024)
	chunks := f.put(f.a, "a.bin", content)
	f.put(f.b, "b.bin", content)
	require.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, f.a, "a.bin", nil).Code)
	backdateGCIRows(t, f.db, pairsOf(chunks)) // as old as a row can be

	// Act: the sweep is pointed straight at them.
	outcomes := f.sweep(chunks)

	// Assert: kept — row and blob — and B still reads.
	for _, o := range outcomes {
		assert.Equal(t, sweepKept, o)
	}
	assert.Equal(t, len(chunks), gciRowsFor(t, f.db, pairsOf(chunks)))
	assert.Len(t, f.chunkBlobs(), len(chunks))
	got := f.do(http.MethodGet, f.b, "b.bin", nil)
	require.Equal(t, http.StatusOK, got.Code)
	assert.Equal(t, content, got.Body.Bytes())
}

// A sweep racing a new reference, on the one address. The row delete and the
// blob delete happen under the chunk's advisory lock; a first store takes the
// same lock. Whatever the interleaving, an upload that answered 200 reads.
func TestDedupGC_SweepRacingANewReferenceNeverLeavesAnUnreadableObject(t *testing.T) {
	f := setupChunkAddrFixture(t)
	for round := 0; round < 12; round++ {
		// Arrange: a chunk nobody references, past its grace.
		content := generateTestData(8 * 1024)
		chunks := f.put(f.a, "old.bin", content)
		require.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, f.a, "old.bin", nil).Code)
		backdateGCIRows(t, f.db, pairsOf(chunks))

		// Act: the sweep and a new upload of the same bytes, at once.
		var wg sync.WaitGroup
		var putCode int
		wg.Add(2)
		go func() { defer wg.Done(); _ = f.sweep(chunks) }()
		go func() {
			defer wg.Done()
			putCode = f.do(http.MethodPut, f.b, "new.bin", content).Code
		}()
		wg.Wait()

		// Assert
		require.Equal(t, http.StatusOK, putCode)
		got := f.do(http.MethodGet, f.b, "new.bin", nil)
		require.Equal(t, http.StatusOK, got.Code, "round %d: the upload answered 200 and its chunk is gone", round)
		require.Equal(t, content, got.Body.Bytes())
		require.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, f.b, "new.bin", nil).Code)
	}
}

func TestDedupGC_ABackendThatCannotBeAskedKeepsTheRow(t *testing.T) {
	// Arrange: a collectable chunk, and a backend that answers 503 to Exists.
	f := setupChunkAddrFixture(t)
	chunks := f.put(f.a, "a.bin", generateTestData(8*1024))
	require.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, f.a, "a.bin", nil).Code)
	backdateGCIRows(t, f.db, pairsOf(chunks))
	f.fixed.failExists.Store(true)

	// Act
	_, err := f.gc().sweepOne(context.Background(), chunks[0].scope, chunks[0].hash, f.backend, chunks[0].storageKey)

	// Assert: an error, and the row — the only record of where the blob is —
	// is still there.
	require.Error(t, err)
	assert.Equal(t, len(chunks), gciRowsFor(t, f.db, pairsOf(chunks)))
	assert.Len(t, f.chunkBlobs(), len(chunks))

	// A row that names a backend this server has no driver for: the same.
	f.fixed.failExists.Store(false)
	_, err = f.gc().sweepOne(context.Background(), chunks[0].scope, chunks[0].hash, "no-such-backend", chunks[0].storageKey)
	require.ErrorIs(t, err, engine.ErrBackendNotRegistered)
	assert.Equal(t, len(chunks), gciRowsFor(t, f.db, pairsOf(chunks)))
}

// --- blobs written before WP-R8-7: the read fallback ----------------------------

func TestChunkRead_FallsBackToTheUploadersPrefixAndCountsIt(t *testing.T) {
	// Arrange: A's object, its blobs where an older build wrote them.
	f := setupChunkAddrFixture(t)
	core, logs := observer.New(zap.WarnLevel)
	f.adapter.logger = zap.New(core)
	content := generateTestData(8 * 1024)
	chunks := f.put(f.a, "a.bin", content)
	f.makeLegacy(f.a, chunks)
	before := testutil.ToFloat64(chunkLegacyReads)

	// Act: the uploader reads twice; then B uploads the same bytes (a dedup
	// hit on a chunk whose blob is under A's prefix) and reads.
	first := f.do(http.MethodGet, f.a, "a.bin", nil)
	second := f.do(http.MethodGet, f.a, "a.bin", nil)
	f.put(f.b, "b.bin", content)
	other := f.do(http.MethodGet, f.b, "b.bin", nil)

	// Assert: all three read; each chunk read is counted; logged once per
	// chunk, not once per read; nothing was written or moved by a read.
	for _, got := range []*httptest.ResponseRecorder{first, second, other} {
		require.Equal(t, http.StatusOK, got.Code)
		assert.Equal(t, content, got.Body.Bytes())
	}
	assert.Equal(t, float64(3*len(chunks)), testutil.ToFloat64(chunkLegacyReads)-before)
	assert.Equal(t, len(chunks), logs.FilterMessageSnippet("legacy address").Len(), "one line per chunk")
	var want []string
	for _, c := range chunks {
		want = append(want, legacyKey(f.a, c))
	}
	assert.ElementsMatch(t, want, f.chunkBlobs(), "a read never writes: the blob is still only where it was")
}

func TestChunkRead_FindsALegacyBlobWhoseUploaderDeletedItsObject(t *testing.T) {
	// Arrange: A uploaded first (blob under A's prefix), B references the
	// same chunk, A deleted its object. No manifest of A names the chunk any
	// more; the engine's record of the write still does.
	f := setupChunkAddrFixture(t)
	content := generateTestData(8 * 1024)
	chunks := f.put(f.a, "a.bin", content)
	f.makeLegacy(f.a, chunks)
	f.put(f.b, "b.bin", content)
	require.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, f.a, "a.bin", nil).Code)
	f.recordWrites(f.a, chunks)

	// Act
	got := f.do(http.MethodGet, f.b, "b.bin", nil)

	// Assert
	require.Equal(t, http.StatusOK, got.Code)
	assert.Equal(t, content, got.Body.Bytes())
}

// An error is never read as "not there".
func TestChunkRead_AnErrorAtTheOneAddressIsNotAMiss(t *testing.T) {
	// Arrange: the blob IS at a legacy address — a fallback would find it —
	// and the one address answers 503.
	f := setupChunkAddrFixture(t)
	chunks := f.put(f.a, "a.bin", generateTestData(8*1024))
	f.makeLegacy(f.a, chunks)
	f.fixed.failGetUnder.Store(engine.ChunkAddressTenant)
	before := testutil.ToFloat64(chunkLegacyReads)

	// Act
	_, err := f.adapter.chunks().get(context.Background(), f.addr(chunks[0]))
	got := f.do(http.MethodGet, f.a, "a.bin", nil)

	// Assert: the error comes back as an error; the legacy address was not
	// consulted on the strength of it.
	require.Error(t, err)
	assert.False(t, isObjectMissingErr(err), "a 503 must not look like a miss: %v", err)
	assert.GreaterOrEqual(t, got.Code, 500)
	assert.Equal(t, before, testutil.ToFloat64(chunkLegacyReads))
}

func TestChunkRead_ALegacyAddressThatErrorsIsNotAMissEither(t *testing.T) {
	// Arrange: nothing at the one address (a true miss), and the only place
	// the blob could be — under A — cannot be read.
	f := setupChunkAddrFixture(t)
	chunks := f.put(f.a, "a.bin", generateTestData(8*1024))
	f.makeLegacy(f.a, chunks)
	f.fixed.failGetUnder.Store(f.a.ID)

	// Act
	_, err := f.adapter.chunks().get(context.Background(), f.addr(chunks[0]))

	// Assert: "one place could not be asked", not "the chunk does not exist".
	require.Error(t, err)
	assert.False(t, isObjectMissingErr(err), "got a miss: %v", err)

	// And when every place answers "not there", it IS a miss.
	f.fixed.failGetUnder.Store("")
	require.NoError(t, os.Remove(f.fixed.path(f.a.ID, chunkContainer, chunks[0].storageKey)))
	_, err = f.adapter.chunks().get(context.Background(), f.addr(chunks[0]))
	require.Error(t, err)
	assert.True(t, isObjectMissingErr(err), "every address missed: %v", err)
}

// No fallback on deletes: GC never touches a legacy address — and it does
// not drop the row of a chunk whose only copy is at one.
func TestDedupGC_KeepsTheRowOfAChunkStillAtALegacyAddress(t *testing.T) {
	// Arrange: a collectable chunk whose blob is under A's prefix; the engine
	// recorded A's write (no manifest references the chunk any more).
	f := setupChunkAddrFixture(t)
	chunks := f.put(f.a, "a.bin", generateTestData(8*1024))
	f.makeLegacy(f.a, chunks)
	require.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, f.a, "a.bin", nil).Code)
	backdateGCIRows(t, f.db, pairsOf(chunks))
	f.recordWrites(f.a, chunks)

	// Act
	outcomes := f.sweep(chunks)

	// Assert: row kept for the chunk move, blob untouched.
	for _, o := range outcomes {
		assert.Equal(t, sweepLegacyPending, o)
	}
	assert.Equal(t, len(chunks), gciRowsFor(t, f.db, pairsOf(chunks)))
	assert.FileExists(t, f.fixed.path(f.a.ID, chunkContainer, chunks[0].storageKey))

	// A chunk that is nowhere at all: the row goes (there is nothing to delete).
	require.NoError(t, os.Remove(f.fixed.path(f.a.ID, chunkContainer, chunks[0].storageKey)))
	o, err := f.gc().sweepOne(context.Background(), chunks[0].scope, chunks[0].hash, f.backend, chunks[0].storageKey)
	require.NoError(t, err)
	assert.Equal(t, sweepDeleted, o)
}

// No fallback on writes: the engine refuses a chunk blob write that is not
// addressed through engine.ChunkContext, whoever makes it.
func TestChunkWrite_OnlyAtTheOneAddress(t *testing.T) {
	f := setupChunkAddrFixture(t)
	body := []byte("chunk")

	for name, ctx := range map[string]context.Context{
		"a tenant's request":   s3Ctx(context.Background(), f.a),
		"a job with no tenant": context.Background(),
		"the legacy context":   engine.LegacyChunkContext(context.Background(), f.a.ID),
	} {
		_, err := f.eng.Put(ctx, chunkContainer, "_chunks/x", bytes.NewReader(body))
		assert.ErrorIs(t, err, engine.ErrChunkAddress, name)
		err = f.eng.PutOn(ctx, f.backend, chunkContainer, "_chunks/x", bytes.NewReader(body))
		assert.ErrorIs(t, err, engine.ErrChunkAddress, name)
	}
	assert.Empty(t, f.chunkBlobs())

	_, err := f.eng.Put(engine.ChunkContext(context.Background()), chunkContainer, "_chunks/x", bytes.NewReader(body))
	require.NoError(t, err)
	assert.Equal(t, []string{"t-" + engine.ChunkAddressTenant + "/" + chunkContainer + "/_chunks/x"}, f.chunkBlobs())
}

// A driver call for a tenant's container that carries NO tenant used to land
// under `t-default/` without a word. It is refused now — and a chunk job that
// forgot the helper would be exactly that call.
func TestDriverCallWithoutATenant_IsRefusedNotFiledUnderDefault(t *testing.T) {
	f := setupChunkAddrFixture(t)
	ctx := context.Background() // a job that forgot its tenant
	container := f.a.NamespaceContainer("test-bucket")

	_, getErr := f.eng.GetOn(ctx, f.backend, container, "k")
	delErr := f.eng.DeleteOn(ctx, f.backend, container, "k")
	_, exErr := f.eng.ExistsOn(ctx, f.backend, container, "k")

	for _, err := range []error{getErr, delErr, exErr} {
		require.ErrorIs(t, err, drivers.ErrNoTenant)
		assert.ErrorIs(t, err, engine.ErrInvalidInput, "a caller's bug: no charge on the backend's breaker")
		assert.False(t, isObjectMissingErr(err), "and never a miss — a delete that 'succeeded' on t-default is how GC leaked every blob")
	}
	assert.Equal(t, "closed", f.eng.GetFailoverStatus()[f.backend])
	assert.Empty(t, f.fixed.blobs())
}

// The S3 front door refuses a credential that resolves to a reserved tenant
// id: nobody is the tenant the chunk blobs live under.
func TestReservedTenantID_IsNobodys(t *testing.T) {
	assert.True(t, engine.IsReservedTenantID(engine.ChunkAddressTenant))
	assert.True(t, engine.IsReservedTenantID(""))
	assert.True(t, engine.IsReservedTenantID("_anything"))
	assert.False(t, engine.IsReservedTenantID("tenant-0123abcd"))
	assert.False(t, engine.IsReservedTenantID(uuid.New().String()))
	// The account-erasure sweep can never list under it.
	assert.Error(t, sweepableTenantID(engine.ChunkAddressTenant))

	s := &Server{logger: zap.NewNop(), testMode: true}
	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	req = req.WithContext(tenant.WithTenant(req.Context(), &tenant.Tenant{ID: engine.ChunkAddressTenant}))
	w := httptest.NewRecorder()
	s.handleS3Request(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// The legal pages say what the collector does with the blocks of a deleted
// account's large objects (WP-R8-7): released on the date, deleted once the
// seven-day safety period has passed — the GC grace. Change both together.
func TestErasureCopy_SaysWhatDedupGCDoes(t *testing.T) {
	gc := NewDedupGCRunner(nil, nil, nil, nil)
	require.Nil(t, gc, "no runner without a database")
	f := setupChunkAddrFixture(t)
	assert.Equal(t, 7*24*time.Hour, f.gc().GracePeriod)
	assert.True(t, f.gc().spec().daily(), "the collector runs daily, so 'once the period has passed' is the next night")

	for _, page := range []string{"privacy", "terms", "dpa", "baa", "gdpr"} {
		body, err := os.ReadFile(filepath.Join("..", "dashboard", "templates", "legal", page+".html"))
		require.NoError(t, err)
		assert.Contains(t, string(body), "deleted by the storage collector once its seven-day safety period has passed", page)
		assert.Contains(t, string(body), "byte-for-byte identical", page)
	}
}
