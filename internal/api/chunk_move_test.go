package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R8-7 — the chunk move: blobs written before the one address existed are
// copied to it, verified, and only then removed from where they were.
//
// Every test runs on a backend name of its own (the mover works on one
// backend, so its row pass sees this test's rows only) and names the tenants
// its listing pass may list (tenantsOverride): the test database is shared.

type chunkMoveFixture struct {
	*chunkAddrFixture
	mover *ChunkMover
}

// setupChunkMoveFixture: tenant A uploaded three objects with a build from
// before WP-R8-7 — their blobs are under A's prefix.
func setupChunkMoveFixture(t *testing.T) (*chunkMoveFixture, map[string][]byte, []scopedChunk) {
	t.Helper()
	f := &chunkMoveFixture{chunkAddrFixture: setupChunkAddrFixture(t)}
	f.mover = NewChunkMover(f.db, f.eng, zap.NewNop())
	f.mover.tenantsOverride = []string{f.a.ID, f.b.ID}
	objects := map[string][]byte{}
	var chunks []scopedChunk
	for _, key := range []string{"one.bin", "two.bin", "three.bin"} {
		objects[key] = generateTestData(8 * 1024)
		chunks = append(chunks, f.put(f.a, key, objects[key])...)
	}
	f.makeLegacy(f.a, chunks)
	return f, objects, chunks
}

func (f *chunkMoveFixture) run(dry bool, extra ...string) ChunkMoveResult {
	f.t.Helper()
	res, err := f.mover.Run(context.Background(), ChunkMoveOptions{Backend: f.backend, DryRun: dry, ExtraTenants: extra})
	require.NoError(f.t, err)
	return res
}

func (f *chunkMoveFixture) readsAll(tn *tenant.Tenant, objects map[string][]byte) {
	f.t.Helper()
	for key, want := range objects {
		got := f.do(http.MethodGet, tn, key, nil)
		require.Equal(f.t, http.StatusOK, got.Code, key)
		require.Equal(f.t, want, got.Body.Bytes(), key)
	}
}

func keysAt(chunks []scopedChunk, key func(scopedChunk) string) []string {
	var out []string
	for _, c := range chunks {
		out = append(out, key(c))
	}
	return out
}

func TestChunkMove_DryRunReadsOnlyAndSaysWhatItWouldDo(t *testing.T) {
	// Arrange
	f, objects, chunks := setupChunkMoveFixture(t)
	before := f.fixed.blobs()

	// Act
	res := f.run(true)

	// Assert: the plan, and not one byte changed.
	assert.True(t, res.DryRun)
	assert.Equal(t, len(chunks), res.Rows)
	assert.Equal(t, len(chunks), res.Moved, "would move every chunk")
	assert.Equal(t, len(chunks), res.LegacyDeleted, "would delete every old copy — each counted once, by the row pass")
	assert.Zero(t, res.AtAddress)
	assert.Zero(t, res.Missing)
	assert.Zero(t, res.Failed, "%v", res.Errors)
	assert.Zero(t, res.Orphans)
	assert.Equal(t, int64(len(chunks)*8*1024), res.BytesMoved)
	assert.Equal(t, before, f.fixed.blobs())
	f.readsAll(f.a, objects)
}

func TestChunkMove_MovesVerifiesThenDeletes_AndASecondRunFindsItDone(t *testing.T) {
	// Arrange
	f, objects, chunks := setupChunkMoveFixture(t)

	// Act
	res := f.run(false)

	// Assert: every blob is at the one address and nowhere else; the objects
	// read without the fallback.
	assert.Equal(t, len(chunks), res.Rows)
	assert.Equal(t, len(chunks), res.Moved)
	assert.Equal(t, len(chunks), res.LegacyDeleted)
	assert.Zero(t, res.Failed, "%v", res.Errors)
	assert.ElementsMatch(t, keysAt(chunks, oneAddressKey), f.chunkBlobs())
	legacyReads := testutil.ToFloat64(chunkLegacyReads)
	f.readsAll(f.a, objects)
	assert.Equal(t, legacyReads, testutil.ToFloat64(chunkLegacyReads), "read at the one address: the fallback is not used")

	// Act 2 + Assert 2: idempotent — done is found done.
	again := f.run(false)
	assert.Equal(t, len(chunks), again.Rows)
	assert.Equal(t, len(chunks), again.AtAddress)
	assert.Zero(t, again.Moved)
	assert.Zero(t, again.LegacyDeleted)
	assert.Zero(t, again.Failed, "%v", again.Errors)
	assert.ElementsMatch(t, keysAt(chunks, oneAddressKey), f.chunkBlobs())
}

// A deploy between the copy and the delete: the run stops with both copies in
// place; reads work; the next run deletes the old copies and copies nothing.
func TestChunkMove_StoppedBetweenTheCopyAndTheDelete_IsFinishedByTheNextRun(t *testing.T) {
	// Arrange: the process is stopped right after the first verified copy.
	f, objects, chunks := setupChunkMoveFixture(t)
	ctx, stop := context.WithCancel(context.Background())
	f.mover.afterCopy = stop

	// Act 1
	res, err := f.mover.Run(ctx, ChunkMoveOptions{Backend: f.backend})
	_ = res
	f.mover.afterCopy = nil

	// Assert 1: interrupted; one chunk has two copies, nothing was lost.
	require.ErrorIs(t, err, context.Canceled)
	assert.Len(t, f.chunkBlobs(), len(chunks)+1, "one chunk is at both addresses, the others still at the old one")
	f.readsAll(f.a, objects)

	// Act 2
	next := f.run(false)

	// Assert 2
	assert.Zero(t, next.Failed, "%v", next.Errors)
	assert.Equal(t, len(chunks)-1, next.Moved, "the chunk copied before the stop is not copied again")
	assert.Equal(t, len(chunks), next.LegacyDeleted)
	assert.ElementsMatch(t, keysAt(chunks, oneAddressKey), f.chunkBlobs())
	f.readsAll(f.a, objects)
}

func TestChunkMove_AnOldCopyThatDoesNotVerifyIsNeitherCopiedNorDeleted(t *testing.T) {
	// Arrange: one old copy is damaged.
	f, _, chunks := setupChunkMoveFixture(t)
	bad := f.fixed.path(f.a.ID, chunkContainer, chunks[0].storageKey)
	require.NoError(t, os.WriteFile(bad, []byte("not the chunk"), 0o600))

	// Act
	res := f.run(false)

	// Assert: reported missing (no copy that verifies), left exactly where
	// it was, and nothing put at the one address in its name.
	assert.Equal(t, 1, res.Missing)
	assert.Equal(t, len(chunks)-1, res.Moved)
	assert.FileExists(t, bad)
	assert.NoFileExists(t, f.fixed.path(engine.ChunkAddressTenant, chunkContainer, chunks[0].storageKey))
}

func TestChunkMove_ABlobAtTheOneAddressThatDoesNotVerifyIsReplacedFromAGoodCopy(t *testing.T) {
	// Arrange: a damaged blob at the one address, the good copy under A.
	f, objects, chunks := setupChunkMoveFixture(t)
	target := f.fixed.path(engine.ChunkAddressTenant, chunkContainer, chunks[0].storageKey)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o750))
	require.NoError(t, os.WriteFile(target, []byte("half a write"), 0o600))

	// Act
	res := f.run(false)

	// Assert
	assert.Zero(t, res.Failed, "%v", res.Errors)
	assert.Equal(t, len(chunks), res.Moved)
	f.readsAll(f.a, objects)
	assert.ElementsMatch(t, keysAt(chunks, oneAddressKey), f.chunkBlobs())
}

// A flaky backend: an error is not "not there" — nothing is deleted, nothing
// is declared missing, the next run retries. (One failure mode per test: five
// backend failures in a minute open the engine's breaker, as they should.)
func TestChunkMove_OldCopiesThatCannotBeReadAreNotMissing(t *testing.T) {
	f, _, chunks := setupChunkMoveFixture(t)
	before := f.fixed.blobs()
	f.fixed.failGetUnder.Store(f.a.ID)

	res := f.run(false)

	assert.Equal(t, len(chunks), res.Failed)
	assert.Zero(t, res.Missing, "unreadable is not missing")
	assert.Zero(t, res.Moved)
	assert.Zero(t, res.LegacyDeleted)
	assert.Equal(t, before, f.fixed.blobs())
}

func TestChunkMove_AOneAddressThatCannotBeReadMovesNothing(t *testing.T) {
	f, _, chunks := setupChunkMoveFixture(t)
	before := f.fixed.blobs()
	f.fixed.failGetUnder.Store(engine.ChunkAddressTenant)

	res := f.run(false)

	assert.Equal(t, len(chunks), res.Failed)
	assert.Zero(t, res.Moved)
	assert.Zero(t, res.LegacyDeleted)
	assert.Equal(t, before, f.fixed.blobs())
}

func TestChunkMove_AnOldCopyThatCannotBeAskedAboutIsKept_TheNextRunFinishes(t *testing.T) {
	// Arrange: Exists fails — after the copy, the old copies cannot be found.
	f, objects, chunks := setupChunkMoveFixture(t)
	f.fixed.failExists.Store(true)

	// Act 1
	res := f.run(false)

	// Assert 1: copied and verified, every old copy kept.
	assert.Equal(t, len(chunks), res.Moved)
	assert.Zero(t, res.LegacyDeleted)
	assert.Equal(t, len(chunks), res.Failed)
	assert.Len(t, f.chunkBlobs(), 2*len(chunks))

	// Act 2: the backend recovers.
	f.fixed.failExists.Store(false)
	res = f.run(false)

	// Assert 2
	assert.Zero(t, res.Failed, "%v", res.Errors)
	assert.Zero(t, res.Moved)
	assert.Equal(t, len(chunks), res.LegacyDeleted)
	assert.ElementsMatch(t, keysAt(chunks, oneAddressKey), f.chunkBlobs())
	f.readsAll(f.a, objects)
}

// What is deleted next must be replaceable by what is THERE, not by what was
// sent: a copy that does not read back keeps the old copy.
func TestChunkMove_ACopyThatDoesNotReadBackKeepsTheOldCopy(t *testing.T) {
	f, objects, chunks := setupChunkMoveFixture(t)
	before := f.fixed.blobs()
	f.fixed.dropPut.Store(true)

	res := f.run(false)

	assert.Equal(t, len(chunks), res.Failed)
	assert.Zero(t, res.Moved)
	assert.Zero(t, res.LegacyDeleted)
	assert.Equal(t, before, f.fixed.blobs())
	f.readsAll(f.a, objects)
}

func TestChunkMove_RefusesToRunWhileTheBackendsBreakerIsOpen(t *testing.T) {
	// Arrange: the backend failed five times in a row.
	f, _, _ := setupChunkMoveFixture(t)
	before := f.fixed.blobs()
	f.fixed.failGet.Store(true)
	for i := 0; i < 5; i++ {
		_, _ = f.eng.GetOn(engine.ChunkContext(context.Background()), f.backend, chunkContainer, "_chunks/x")
	}
	f.fixed.failGet.Store(false)
	require.Equal(t, engine.StateOpen.String(), f.eng.GetFailoverStatus()[f.backend])

	// Act
	_, err := f.mover.Run(context.Background(), ChunkMoveOptions{Backend: f.backend})

	// Assert
	require.Error(t, err)
	assert.Contains(t, err.Error(), "circuit breaker is open")
	assert.Equal(t, before, f.fixed.blobs())
}

// The backend a chunk test always ran on: its keys do not depend on the
// tenant, so the "old copy" IS the blob. The move must not touch it.
func TestChunkMove_DoesNothingWhereTheAddressDoesNotDependOnTheTenant(t *testing.T) {
	// Arrange: the same chunks on a local (container-keyed) backend.
	f := &chunkMoveFixture{chunkAddrFixture: setupChunkAddrFixture(t)}
	dir := t.TempDir()
	f.eng.AddDriver(f.backend, drivers.NewLocalDriver(dir, zap.NewNop()))
	f.mover = NewChunkMover(f.db, f.eng, zap.NewNop())
	f.mover.tenantsOverride = []string{f.a.ID}
	content := generateTestData(8 * 1024)
	chunks := f.put(f.a, "a.bin", content)
	blob := filepath.Join(dir, chunkContainer, filepath.FromSlash(chunks[0].storageKey))
	require.FileExists(t, blob)

	// Act
	res := f.run(false)

	// Assert
	assert.Contains(t, res.Note, "does not depend on the tenant")
	assert.Zero(t, res.LegacyDeleted)
	assert.Zero(t, res.OrphansDeleted)
	assert.FileExists(t, blob)
	got := f.do(http.MethodGet, f.a, "a.bin", nil)
	require.Equal(t, http.StatusOK, got.Code)
	assert.Equal(t, content, got.Body.Bytes())
}

func TestChunkMove_AChunkTwoTenantsShareIsReadableByBothAfterwards(t *testing.T) {
	// Arrange: B's object deduplicated against A's legacy chunks.
	f, objects, chunks := setupChunkMoveFixture(t)
	for key, content := range objects {
		f.put(f.b, key, content)
	}

	// Act
	res := f.run(false)

	// Assert
	assert.Zero(t, res.Failed, "%v", res.Errors)
	assert.Equal(t, len(chunks), res.Moved)
	assert.ElementsMatch(t, keysAt(chunks, oneAddressKey), f.chunkBlobs())
	f.readsAll(f.a, objects)
	f.readsAll(f.b, objects)
}

// The listing pass: a copy under a tenant the database no longer connects to
// the chunk (its uploader deleted the object, no write was recorded) is found
// by listing that tenant's chunk container; a blob with no index row at all
// is an orphan and is deleted; anything else there is left alone.
func TestChunkMove_ListingFindsWhatTheRowsDoNotName(t *testing.T) {
	// Arrange
	f, objects, chunks := setupChunkMoveFixture(t)
	// (1) B references A's legacy chunks; A deleted its objects.
	for key, content := range objects {
		f.put(f.b, key, content)
		require.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, f.a, key, nil).Code)
	}
	// (2) an orphan under A: a chunk key with no index row.
	sum := sha256.Sum256([]byte("orphan"))
	orphan := f.fixed.path(f.a.ID, chunkContainer, "_chunks/"+hex.EncodeToString(sum[:]))
	require.NoError(t, os.WriteFile(orphan, []byte("orphan"), 0o600))
	// (3) a name that is not a chunk key.
	stranger := f.fixed.path(f.a.ID, chunkContainer, "notes.txt")
	require.NoError(t, os.WriteFile(stranger, []byte("x"), 0o600))

	// Act 1: the dry run counts and touches nothing.
	dry := f.run(true)

	// Assert 1
	assert.Equal(t, len(chunks), dry.Moved)
	assert.Equal(t, 1, dry.Orphans)
	assert.Zero(t, dry.OrphansDeleted)
	assert.Equal(t, 1, dry.Unrecognized)
	assert.FileExists(t, orphan)

	// Act 2
	res := f.run(false)

	// Assert 2
	assert.Zero(t, res.Failed, "%v", res.Errors)
	assert.Equal(t, len(chunks), res.Moved, "found under A by listing A's chunk container")
	assert.Zero(t, res.Missing, "a copy found by the listing pass is not missing")
	assert.Equal(t, 1, res.OrphansDeleted)
	assert.NoFileExists(t, orphan)
	assert.FileExists(t, stranger, "not a chunk key: not the move's to delete")
	f.readsAll(f.b, objects)
}

func TestChunkMove_ARowThatDedupGCSweptMeanwhileIsNotResurrected(t *testing.T) {
	// Arrange: the chunk's last reference went and GC's sweep already took
	// the row of one chunk (its legacy blob stayed — the old bug).
	f, _, chunks := setupChunkMoveFixture(t)
	for _, key := range []string{"one.bin", "two.bin", "three.bin"} {
		require.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, f.a, key, nil).Code)
	}
	_, err := f.db.Exec(`DELETE FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`, chunks[0].scope, chunks[0].hash)
	require.NoError(t, err)

	// Act
	res := f.run(false)

	// Assert: two rows moved; the third has no row — its blob is an orphan,
	// deleted, not copied to the one address.
	assert.Zero(t, res.Failed, "%v", res.Errors)
	assert.Equal(t, len(chunks)-1, res.Rows)
	assert.Equal(t, len(chunks)-1, res.Moved)
	assert.Equal(t, 1, res.OrphansDeleted)
	assert.ElementsMatch(t, keysAt(chunks[1:], oneAddressKey), f.chunkBlobs())
}

func TestChunkMove_AnErasedTenantsPrefixIsReachedByName(t *testing.T) {
	// Arrange: a tenant the database no longer knows (erased), whose prefix
	// still holds an orphan chunk blob.
	f, _, _ := setupChunkMoveFixture(t)
	gone := "tenant-erased00"
	sum := sha256.Sum256([]byte("left behind"))
	left := f.fixed.path(gone, chunkContainer, "_chunks/"+hex.EncodeToString(sum[:]))
	require.NoError(t, os.MkdirAll(filepath.Dir(left), 0o750))
	require.NoError(t, os.WriteFile(left, []byte("left behind"), 0o600))

	// Act + Assert: not listed unless named.
	res := f.run(false)
	assert.Zero(t, res.OrphansDeleted)
	assert.FileExists(t, left)
	res = f.run(false, gone)
	assert.Equal(t, 1, res.OrphansDeleted)
	assert.NoFileExists(t, left)

	// A name that is not a tenant id is refused before anything is listed.
	for _, bad := range []string{engine.ChunkAddressTenant, "a/b", ""} {
		_, err := f.mover.Run(context.Background(), ChunkMoveOptions{Backend: f.backend, ExtraTenants: []string{bad}})
		assert.ErrorIs(t, err, errChunkMoveRequest, "tenant %q", bad)
	}
}

func TestChunkMove_AdminEndpoint_DryRunByDefault(t *testing.T) {
	// Arrange
	f, _, chunks := setupChunkMoveFixture(t)
	s := &Server{logger: zap.NewNop(), db: f.db, chunkMover: f.mover}
	before := f.fixed.blobs()
	call := func(query string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleChunkMove(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/chunk-move"+query, nil))
		return w
	}

	// Act + Assert: no backend, an unknown backend, a bad flag.
	assert.Equal(t, http.StatusBadRequest, call("").Code)
	assert.Equal(t, http.StatusBadRequest, call("?backend=nope").Code)
	assert.Equal(t, http.StatusBadRequest, call("?backend="+f.backend+"&dry_run=maybe").Code)

	// No dry_run parameter: a dry run.
	w := call("?backend=" + f.backend)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"dry_run":true`)
	assert.Equal(t, before, f.fixed.blobs())

	// One at a time.
	require.True(t, s.chunkMoveGate.tryAcquire())
	assert.Equal(t, http.StatusConflict, call("?backend="+f.backend).Code)
	s.chunkMoveGate.release()

	// dry_run=false acts.
	w = call("?backend=" + f.backend + "&dry_run=false")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"dry_run":false`)
	assert.ElementsMatch(t, keysAt(chunks, oneAddressKey), f.chunkBlobs())
}

// After the move the account-erasure sweep finds nothing under the tenant's
// prefix: chunk_blobs_left reads 0 for a tenant whose blobs were moved.
func TestChunkMove_LeavesNothingForTheErasureSweepToCount(t *testing.T) {
	f, _, _ := setupChunkMoveFixture(t)
	count := func() int {
		n := 0
		require.NoError(t, f.fixed.WalkTenant(context.Background(), f.a.ID, func(o engine.TenantObject) error {
			if o.Container == chunkContainer {
				n++
			}
			return nil
		}))
		return n
	}
	require.Equal(t, 3, count(), "before the move the sweep sees the tenant's legacy chunk blobs")

	f.run(false)

	assert.Zero(t, count())
}
