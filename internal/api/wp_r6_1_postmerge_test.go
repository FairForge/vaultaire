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
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
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
	assert.Equal(t, "Did you mean 'k.bin'?", keySuggestion(context.Background(), f.db, f.tenantID, "test-bucket", "k.binn"),
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
	// Since WP-VM-1 (R61-5) a Suspended DELETE is ONE transaction under the
	// key's locks (the null marker, the head row, the bytes' fate, the
	// bill): a PUT racing it is wholly before or wholly after. Run the race
	// for real, many times: the outcome is always a serial one.
	f := setupLocFixture(t)
	f.bucketSet(t, "versioning_status = 'Suspended'")
	require.NoError(t, f.qm.SetHouse(context.Background(), f.tenantID, usage.House{StdBytes: 1 << 30}))
	newBody := mpBody(5000, 2)
	for i := 0; i < 10; i++ {
		require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, "k.bin", mpBody(3000, 1), nil).Code)
		var wg sync.WaitGroup
		var putCode int
		var d *http.Response
		wg.Go(func() { putCode = httptestPut(f.mpChunkFixture, "k.bin", newBody, nil).Code })
		wg.Go(func() { d = f.del(t, "k.bin") })
		wg.Wait()

		require.Equal(t, http.StatusOK, putCode)
		assert.Equal(t, http.StatusNoContent, d.StatusCode)
		g := f.get(t, "k.bin", "")
		switch g.Code {
		case http.StatusOK: // the DELETE first, then the PUT
			assert.Equal(t, newBody, g.Body.Bytes())
			f.ledgerAgrees(t, 5000, "PUT after the DELETE")
		case http.StatusNotFound: // the PUT first, then the DELETE
			f.ledgerAgrees(t, 0, "DELETE after the PUT")
		default:
			t.Fatalf("GET %d", g.Code)
		}
	}
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

// --- the silent-primary hole (Part 0 item 2) -------------------------------

func TestGet_NoRow_NoSuchKeyWithoutABackendCall(t *testing.T) {
	// Before: a key with no head row was asked of the primary at <key> (an
	// orphan blob there was served as the object).
	f := setupLocFixture(t)
	require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, "k.bin", mpBody(3000, 1), nil).Code)
	f.plant(t, f.primary, "k.bin", mpBody(3000, 1))
	p := f.blobPath(t, f.primary, "k.bin")
	_, err := f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'k.bin'`, f.tenantID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.primary.path(f.tenantID, f.tenant.NamespaceContainer("test-bucket"), "orphan.bin"), mustRead(t, p), 0o600))
	f.resetCalls()

	w := f.get(t, "orphan.bin", "")
	assert.Equal(t, http.StatusNotFound, w.Code, "no row, no object")
	assert.Zero(t, f.calls(), "no backend asked")
}

func TestDelete_NoRow_NoBackendCall(t *testing.T) {
	f := setupLocFixture(t)
	f.resetCalls()
	d := f.del(t, "never.bin")
	assert.Equal(t, http.StatusNoContent, d.StatusCode)
	assert.Zero(t, f.calls(), "no row names any bytes: nothing to delete anywhere")
	res := f.batchDelete(t, batchEntry{key: "never.bin"})
	assert.Len(t, res.Deleted, 1)
	assert.Zero(t, f.calls())
}

func TestGet_LegacyVersionBehindMarker_ReadFromItsRowsBackend(t *testing.T) {
	// A version written before C1 (no address: its bytes at <key>) behind a
	// delete marker — the one such version still served. Before: read with
	// no recorded backend, i.e. from the primary.
	f := setupLocFixture(t)
	f.bucketSet(t, "versioning_status = 'Enabled'")
	body := mpBody(3000, 7)
	legacy := f.others["other05"]
	p := legacy.path(f.tenantID, f.tenant.NamespaceContainer("test-bucket"), "old.bin")
	require.NoError(t, os.MkdirAll(dirOf(p), 0o750))
	require.NoError(t, os.WriteFile(p, body, 0o600))
	_, err := f.db.Exec(`INSERT INTO object_versions (tenant_id, bucket, object_key, version_id, size_bytes, etag, content_type, is_latest, is_delete_marker, backend_name, created_at)
		VALUES ($1, 'test-bucket', 'old.bin', 'v-legacy', $2, 'e', 'application/octet-stream', FALSE, FALSE, 'other05', NOW() - INTERVAL '1 hour'),
		       ($1, 'test-bucket', 'old.bin', 'v-marker', 0, '', 'application/octet-stream', TRUE, TRUE, NULL, NOW())`, f.tenantID, len(body))
	require.NoError(t, err)
	f.resetCalls()

	r := httptestGetVersion(f, "old.bin", "v-legacy")
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	assert.Equal(t, body, r.Body.Bytes())
	assert.Zero(t, f.primary.calls.Load(), "never the primary")
}

func TestCopy_SourceVersionId_CopiesThatVersion(t *testing.T) {
	// Restoring an earlier version the S3 way: copy it onto its own key.
	// Before: the versionId was dropped and the CURRENT object copied.
	f := setupLocFixture(t)
	f.bucketSet(t, "versioning_status = 'Enabled'")
	v1Body, v2Body := mpBody(3000, 1), mpBody(4000, 2)
	w1 := httptestPut(f.mpChunkFixture, "k.bin", v1Body, nil)
	require.Equal(t, http.StatusOK, w1.Code)
	require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, "k.bin", v2Body, nil).Code)
	v1 := w1.Header().Get("x-amz-version-id")
	require.NotEmpty(t, v1)

	c := httptestCopy(f, "k.bin", "test-bucket/k.bin?versionId="+v1)
	require.Equal(t, http.StatusOK, c.Code, c.Body.String())
	assert.Equal(t, v1, c.Header().Get("x-amz-copy-source-version-id"))
	g := f.get(t, "k.bin", "")
	require.Equal(t, http.StatusOK, g.Code)
	assert.Equal(t, v1Body, g.Body.Bytes(), "the restored version is current")

	c = httptestCopy(f, "x.bin", "test-bucket/k.bin?versionId=nope")
	assert.Equal(t, http.StatusNotFound, c.Code)
	assert.Contains(t, c.Body.String(), "NoSuchVersion")
}

func TestCopy_SourceWithoutRow_NoSuchKeyWithoutABackendCall(t *testing.T) {
	// Before: the row read's error was ignored: a key with no row was read
	// from the primary at <key> and an orphan blob there copied.
	f := setupLocFixture(t)
	p := f.primary.path(f.tenantID, f.tenant.NamespaceContainer("test-bucket"), "orphan.bin")
	require.NoError(t, os.MkdirAll(dirOf(p), 0o750))
	require.NoError(t, os.WriteFile(p, mpBody(3000, 1), 0o600))
	f.resetCalls()

	c := httptestCopy(f, "dst.bin", "test-bucket/orphan.bin")
	assert.Equal(t, http.StatusNotFound, c.Code, c.Body.String())
	assert.Contains(t, c.Body.String(), "NoSuchKey")
	assert.Zero(t, f.calls())
	assert.Equal(t, 0, f.headRows(t, "dst.bin"))
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

func httptestGetVersion(f *locFixture, key, vid string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/test-bucket/"+key+"?versionId="+vid, nil)
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)
	return w
}

func httptestCopy(f *locFixture, dstKey, source string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("PUT", "/test-bucket/"+dstKey, nil)
	r.Header.Set("x-amz-copy-source", source)
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)
	return w
}

// failDeleteLocal is the export fixture's local driver with every delete
// refused (a backend whose key cannot delete).
type failDeleteLocal struct{ engine.Driver }

func (d failDeleteLocal) Delete(context.Context, string, string) error { return accessDenied }

func TestAccountExport_PurgeWhoseBackendDeleteFails_KeepsTheRowAndTheBill(t *testing.T) {
	// Before: the export purge deleted the head row (and released the
	// bytes) whatever the backend answered: the blob stayed with no row.
	f := setupExportFixture(t)
	f.seed()
	id, err := f.svc.Request(context.Background(), f.userID, f.tenantID)
	require.NoError(t, err)
	f.run()
	f.exec(`UPDATE account_exports SET expires_at = NOW() - INTERVAL '1 minute' WHERE id = $1`, id)
	var usedBefore int64
	require.NoError(t, f.db.QueryRow(`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.tenantID).Scan(&usedBefore))
	local, ok := f.eng.GetDriver("local")
	require.True(t, ok)
	f.eng.AddDriver("local", failDeleteLocal{Driver: local})

	n, err := f.svc.PurgeExpired(context.Background())

	require.NoError(t, err)
	assert.Zero(t, n, "not expired: retried next run")
	var cnt int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2`, f.tenantID, tenant.ExportsBucket).Scan(&cnt))
	assert.Equal(t, 1, cnt, "the head row stays with its bytes")
	assert.NotEmpty(t, f.exportFiles())
	var usedAfter int64
	require.NoError(t, f.db.QueryRow(`SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1`, f.tenantID).Scan(&usedAfter))
	assert.Equal(t, usedBefore, usedAfter, "the bill stays")
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM account_exports WHERE id = $1`, id).Scan(&status))
	assert.Equal(t, "completed", status)

	f.eng.AddDriver("local", local)
	n, err = f.svc.PurgeExpired(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "the next run converges")
	assert.Empty(t, f.exportFiles())
}
