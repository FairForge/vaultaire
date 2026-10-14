package api

// Stage 1a Prompt 6, Part 0: the post-merge probe of WP-R6-1 (#714, #715).
// The row is the one authority for where an object's bytes are; these tests
// probe it where the review asked: a delete on a backend whose key is dead
// (AccessDenied with the breaker closed — prod's iDrive since 2026-10-11),
// a batch mixing such keys with healthy ones, PM-6 under versioning, and
// R61-1 (a miss suggested the very key asked for).

import (
	"context"
	"net/http"
	"os"
	"sync/atomic"
	"testing"

	"github.com/FairForge/vaultaire/internal/usage"
	smithy "github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// accessDenied is the error a fixed-bucket S3 driver returns when its key
// sees the bucket but not its objects (iDrive, 2026-10-11).
var accessDenied = &smithy.GenericAPIError{Code: "AccessDenied", Message: "Access Denied"}

// onOther moves key's head row and blob to the named other backend.
func (f *locFixture) onOther(t *testing.T, key, name string) *countingFixed {
	t.Helper()
	d := f.others[name]
	src := f.blobPath(t, f.primary, key)
	f.plant(t, d, key, mustRead(t, src))
	require.NoError(t, os.Remove(src))
	_, err := f.db.Exec(`UPDATE object_head_cache SET backend_name = $1 WHERE tenant_id = $2 AND bucket = 'test-bucket' AND object_key = $3`,
		name, f.tenantID, key)
	require.NoError(t, err)
	return d
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p) // #nosec G304 — test temp dir
	require.NoError(t, err)
	return b
}

func TestKeySuggestion_NeverProposesTheKeyAsked(t *testing.T) {
	// R61-1. Before: a GET of a key whose row exists but whose backend
	// missed answered "Did you mean 'k.bin'?" — the key the client asked for.
	f := setupLocFixture(t)
	require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, "k.bin", mpBody(3000, 1), nil).Code)
	require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, "k.bim", mpBody(3000, 2), nil).Code)

	assert.Empty(t, keySuggestion(context.Background(), f.db, f.tenantID, "test-bucket", "k.bin"),
		"the key exists: nothing to suggest, least of all itself")
	assert.Equal(t, "Did you mean 'k.bin'?", keySuggestion(context.Background(), f.db, f.tenantID, "test-bucket", "k.bix"),
		"a near miss still gets its suggestion")

	require.NoError(t, os.Remove(f.blobPath(t, f.primary, "k.bin"))) // the backend lost it
	w := f.get(t, "k.bin", "")
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.NotContains(t, w.Body.String(), "Did you mean 'k.bin'")
}

func TestDelete_DeadKeyBackend_503RowAndBillKept_RetryConverges(t *testing.T) {
	// The 2026-10-11 shape: the backend's key answers AccessDenied to object
	// calls while its breaker is closed (HeadBucket passes).
	f := setupLocFixture(t)
	body := mpBody(3000, 1)
	require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, "k.bin", body, nil).Code)
	dead := f.onOther(t, "k.bin", "other03")
	dead.deleteErr = accessDenied

	// Act: two DELETEs (below the breaker's threshold).
	for i := 0; i < 2; i++ {
		d := f.del(t, "k.bin")
		// Assert: 503 + Retry-After; the row, the bytes and the bill stay.
		assert.Equal(t, http.StatusServiceUnavailable, d.StatusCode)
		assert.NotEmpty(t, d.Header.Get("Retry-After"))
		assert.Equal(t, int64(len(body)), f.row(t, "k.bin").size, "the row is kept")
		assert.Equal(t, int64(len(body)), f.used(t), "the bill is kept")
	}
	assert.Equal(t, int32(2), dead.deleteSeen.Load(), "one delete per request, on the recorded backend only")
	assert.Zero(t, f.primary.deleteSeen.Load(), "never the primary")

	// The key is repaired: the client's retry converges.
	dead.deleteErr = nil
	d := f.del(t, "k.bin")
	assert.Equal(t, http.StatusNoContent, d.StatusCode)
	assert.Equal(t, 0, f.headRows(t, "k.bin"))
	assert.Zero(t, f.used(t))
	assert.Empty(t, listFiles(t, f.others["other03"].dir), "the bytes are gone from the dead backend")
}

func TestDelete_DeadKeyBackend_BreakerOpens_ThenNoCall(t *testing.T) {
	f := setupLocFixture(t)
	require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, "k.bin", mpBody(3000, 1), nil).Code)
	dead := f.onOther(t, "k.bin", "other03")
	dead.deleteErr = accessDenied
	for i := 0; i < 5; i++ {
		_ = f.del(t, "k.bin")
	}
	seen := dead.deleteSeen.Load()

	d := f.del(t, "k.bin")
	assert.Equal(t, http.StatusServiceUnavailable, d.StatusCode)
	assert.Equal(t, seen, dead.deleteSeen.Load(), "the open breaker answers; the dead backend is not asked")
	assert.Equal(t, 1, f.headRows(t, "k.bin"))
}

func TestDeleteObjects_DeadKeyBackendMixedWithHealthy_PerKeyResults(t *testing.T) {
	f := setupLocFixture(t)
	for _, k := range []string{"a.bin", "b.bin", "c.bin"} {
		require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, k, mpBody(1000, 1), nil).Code)
	}
	dead := f.onOther(t, "b.bin", "other03")
	dead.deleteErr = accessDenied

	res := f.batchDelete(t, batchEntry{key: "a.bin"}, batchEntry{key: "b.bin"}, batchEntry{key: "c.bin"})

	var deleted []string
	for _, d := range res.Deleted {
		deleted = append(deleted, d.Key)
	}
	assert.ElementsMatch(t, []string{"a.bin", "c.bin"}, deleted)
	require.Len(t, res.Errors, 1)
	assert.Equal(t, "b.bin", res.Errors[0].Key)
	assert.Equal(t, ErrServiceUnavailable, res.Errors[0].Code)
	assert.Equal(t, 0, f.headRows(t, "a.bin"))
	assert.Equal(t, 1, f.headRows(t, "b.bin"), "the dead key's row stays")
	assert.Equal(t, 0, f.headRows(t, "c.bin"))
	assert.Equal(t, int64(1000), f.used(t), "billed for b.bin alone")

	dead.deleteErr = nil
	res = f.batchDelete(t, batchEntry{key: "b.bin"})
	assert.Len(t, res.Deleted, 1, "the retry converges")
	assert.Zero(t, f.used(t))
}

func TestDelete_RacingPut_VersioningEnabled_SerialOutcome(t *testing.T) {
	// PM-6 under versioning Enabled: the delete marker and the head-row
	// delete were two transactions. A PUT that committed between them lost
	// its head row: GET 404 while its version was the latest (no marker
	// after it), ListObjectVersions said the object was current.
	f := setupLocFixture(t)
	f.bucketSet(t, "versioning_status = 'Enabled'")
	require.NoError(t, f.qm.SetHouse(context.Background(), f.tenantID, usage.House{StdBytes: 1 << 30}))
	require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, "k.bin", mpBody(3000, 1), nil).Code)
	newBody := mpBody(5000, 2)
	var putCode atomic.Int32
	deleteMarkerWrittenHook = func(_, _, _ string) {
		deleteMarkerWrittenHook = func(_, _, _ string) {}
		putCode.Store(int32(httptestPut(f.mpChunkFixture, "k.bin", newBody, nil).Code))
	}
	t.Cleanup(func() { deleteMarkerWrittenHook = func(_, _, _ string) {} })

	d := f.del(t, "k.bin")

	require.Equal(t, int32(http.StatusOK), putCode.Load())
	assert.Equal(t, http.StatusNoContent, d.StatusCode)
	// The serial order the commits took: the marker, then the PUT.
	g := f.get(t, "k.bin", "")
	require.Equal(t, http.StatusOK, g.Code, "the PUT is the current object")
	assert.Equal(t, newBody, g.Body.Bytes())
	var latestMarker bool
	require.NoError(t, f.db.QueryRow(`SELECT is_delete_marker FROM object_versions WHERE tenant_id = $1 AND object_key = 'k.bin' AND is_latest`,
		f.tenantID).Scan(&latestMarker))
	assert.False(t, latestMarker, "the PUT's version is the latest, after the marker")
	f.ledgerAgrees(t, 3000+5000, "v1 retained + the PUT current")
}

func TestDelete_RacingPut_VersioningSuspended_SerialOutcome(t *testing.T) {
	f := setupLocFixture(t)
	f.bucketSet(t, "versioning_status = 'Suspended'")
	require.NoError(t, f.qm.SetHouse(context.Background(), f.tenantID, usage.House{StdBytes: 1 << 30}))
	require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, "k.bin", mpBody(3000, 1), nil).Code)
	newBody := mpBody(5000, 2)
	var putCode atomic.Int32
	deleteBeforeRowHook = func(_, _, _ string) {
		deleteBeforeRowHook = func(_, _, _ string) {}
		putCode.Store(int32(httptestPut(f.mpChunkFixture, "k.bin", newBody, nil).Code))
	}
	t.Cleanup(func() { deleteBeforeRowHook = func(_, _, _ string) {} })

	d := f.del(t, "k.bin")

	require.Equal(t, int32(http.StatusOK), putCode.Load())
	assert.Equal(t, http.StatusNoContent, d.StatusCode)
	g := f.get(t, "k.bin", "")
	require.Equal(t, http.StatusOK, g.Code, "the PUT is the current object")
	assert.Equal(t, newBody, g.Body.Bytes())
	f.ledgerAgrees(t, 5000, "the null version rewritten by the PUT")
}

// ledgerAgrees: the incremental ledgers (total, the standard floor) equal
// want and the reconcile agrees.
func (f *locFixture) ledgerAgrees(t *testing.T, want int64, step string) {
	t.Helper()
	f.quotaAccountingFixture.agrees(t, want, step)
}

func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, listFiles(t, dir+"/"+e.Name())...)
			continue
		}
		out = append(out, e.Name())
	}
	return out
}
