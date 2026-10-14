package api

// WP-R6-1 Part A at the handlers, on prod's key shape (fixed-bucket
// drivers): a GET asks the one backend the head row names; a DELETE
// removes every copy the tables know of and fails while one may remain; a
// DELETE racing a PUT leaves the PUT's object whole (PM-6 of WP-R2-1).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingFixed is a fixed-bucket double that counts every call and can
// fail its deletes or run a hook inside them.
type countingFixed struct {
	*fixedBucketDriver
	calls      atomic.Int32
	deleteErr  error
	onDelete   func(artifact string)
	deleteSeen atomic.Int32
}

func (d *countingFixed) Get(ctx context.Context, c, a string) (io.ReadCloser, error) {
	d.calls.Add(1)
	return d.fixedBucketDriver.Get(ctx, c, a)
}

func (d *countingFixed) Exists(ctx context.Context, c, a string) (bool, error) {
	d.calls.Add(1)
	return d.fixedBucketDriver.Exists(ctx, c, a)
}

func (d *countingFixed) Delete(ctx context.Context, c, a string) error {
	d.calls.Add(1)
	d.deleteSeen.Add(1)
	if d.deleteErr != nil {
		return d.deleteErr
	}
	err := d.fixedBucketDriver.Delete(ctx, c, a)
	if d.onDelete != nil {
		d.onDelete(a)
	}
	return err
}

// locFixture: the multipart fixture's fixed-bucket primary, counted, and 17
// more registered fixed-bucket backends — prod's 18.
type locFixture struct {
	*mpChunkFixture
	primary *countingFixed
	others  map[string]*countingFixed
}

func setupLocFixture(t *testing.T) *locFixture {
	t.Helper()
	f := setupMPChunkFixture(t, false)
	f.server.chunkingThreshold = 1 << 40 // whole objects only: the blob is what is routed
	primary := &countingFixed{fixedBucketDriver: f.fixed}
	f.server.engine.AddDriver("fixed", primary)
	others := map[string]*countingFixed{}
	for i := 1; i < 18; i++ {
		name := fmt.Sprintf("other%02d", i)
		others[name] = &countingFixed{fixedBucketDriver: &fixedBucketDriver{dir: t.TempDir()}}
		f.server.engine.AddDriver(name, others[name])
	}
	return &locFixture{mpChunkFixture: f, primary: primary, others: others}
}

func (f *locFixture) calls() int32 {
	n := f.primary.calls.Load()
	for _, d := range f.others {
		n += d.calls.Load()
	}
	return n
}

func (f *locFixture) resetCalls() {
	f.primary.calls.Store(0)
	for _, d := range f.others {
		d.calls.Store(0)
	}
}

// blobPath is where driver d keeps key's current blob.
func (f *locFixture) blobPath(t *testing.T, d *countingFixed, key string) string {
	t.Helper()
	return d.path(f.tenantID, f.tenant.NamespaceContainer("test-bucket"), f.row(t, key).storageKey)
}

func (f *locFixture) plant(t *testing.T, d *countingFixed, key string, body []byte) {
	t.Helper()
	p := f.blobPath(t, d, key)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(t, os.WriteFile(p, body, 0o600))
}

func (f *locFixture) del(t *testing.T, key string) *http.Response {
	t.Helper()
	w := doS3Request(f.server, f.tenant, "DELETE", "/test-bucket/"+key, nil)
	return w.Result()
}

func TestGet_RecordedBackendMiss_IsOneCallNot18(t *testing.T) {
	// Before (origin/main 9f3d0c1): 18 backend calls — the recorded
	// backend, then every other registered one — and a 404.
	f := setupLocFixture(t)
	require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, "k.bin", mpBody(3000, 1), nil).Code)
	require.NoError(t, os.Remove(f.blobPath(t, f.primary, "k.bin"))) // the backend lost it
	f.resetCalls()

	w := f.get(t, "k.bin", "")
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "NoSuchKey")
	assert.Equal(t, int32(1), f.calls(), "one backend call")
}

func TestGet_StaleCopyOnAnotherBackend_NeverServedAtTheHandler(t *testing.T) {
	// Before: the recorded backend's miss walked on, and the stale copy on
	// another backend was served under the head row's ETag (R6-07).
	f := setupLocFixture(t)
	require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, "k.bin", mpBody(3000, 1), nil).Code)
	f.plant(t, f.others["other05"], "k.bin", mpBody(3000, 9))
	require.NoError(t, os.Remove(f.blobPath(t, f.primary, "k.bin")))

	w := f.get(t, "k.bin", "")
	assert.Equal(t, http.StatusNotFound, w.Code, "never another backend's copy")
	w = f.get(t, "k.bin", "bytes=0-99")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGet_RecordedBackendBreakerOpen_503NoFanOut(t *testing.T) {
	f := setupLocFixture(t)
	require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, "k.bin", mpBody(3000, 1), nil).Code)
	f.plant(t, f.others["other03"], "k.bin", mpBody(3000, 9))
	// Five failures open the primary's breaker (the recorded backend).
	f.primary.failGet.Store(true)
	for i := 0; i < 5; i++ {
		_ = f.get(t, "k.bin", "")
	}
	f.primary.failGet.Store(false)
	f.resetCalls()

	w := f.get(t, "k.bin", "")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
	assert.Zero(t, f.calls(), "no backend asked: the open breaker is the answer")
}

func TestGet_UnregisteredRecordedBackend_503(t *testing.T) {
	// WP-R7-5's fallback (a row on an unregistered name read through the
	// fan-out) goes: prod had 0 such rows on 2026-10-13.
	f := setupLocFixture(t)
	require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, "k.bin", mpBody(3000, 1), nil).Code)
	_, err := f.db.Exec(`UPDATE object_head_cache SET backend_name = 'onedrive' WHERE tenant_id = $1 AND object_key = 'k.bin'`, f.tenantID)
	require.NoError(t, err)
	f.resetCalls()

	w := f.get(t, "k.bin", "")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Zero(t, f.calls(), "never the primary's copy in its place")
	d := f.del(t, "k.bin")
	assert.Equal(t, http.StatusServiceUnavailable, d.StatusCode)
	assert.Equal(t, int64(3000), f.row(t, "k.bin").size, "the row is kept")
}

// ledgerRow writes an open Smart-demotion ledger row: the cold copy is the
// recorded one, the hot copy (not yet reclaimed) on hot.
func (f *locFixture) ledgerRow(t *testing.T, key, hot, cold string) {
	t.Helper()
	r := f.row(t, key)
	_, err := f.db.Exec(`INSERT INTO smart_demotions (tenant_id, bucket, object_key, etag, size_bytes, hot_backend, cold_backend, storage_key)
		VALUES ($1, 'test-bucket', $2, $3, $4, $5, $6, $7)`, f.tenantID, key, r.etag, r.size, hot, cold, r.storageKey)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM smart_demotions WHERE tenant_id = $1`, f.tenantID) })
}

func TestDelete_RemovesEveryKnownCopy(t *testing.T) {
	// Before: DELETE removed the recorded copy only and answered 204; the
	// ledger's hot copy waited for the aftermath's best-effort settle (and,
	// for a generic second copy, nothing removed it — R6-24).
	f := setupLocFixture(t)
	require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, "k.bin", mpBody(3000, 1), nil).Code)
	hot := f.others["other07"]
	f.plant(t, hot, "k.bin", mpBody(3000, 1))
	f.ledgerRow(t, "k.bin", "other07", "fixed")
	hotPath, coldPath := f.blobPath(t, hot, "k.bin"), f.blobPath(t, f.primary, "k.bin")
	// A retired_blobs row naming the same address on a third backend.
	third := f.others["other11"]
	f.plant(t, third, "k.bin", mpBody(3000, 1))
	thirdPath := f.blobPath(t, third, "k.bin")
	_, err := f.db.Exec(`INSERT INTO retired_blobs (backend_id, container, storage_key, tenant_id, reason, retire_after)
		VALUES ('other11', $1, $2, $3, 'orphan_write', NOW() + interval '1 day')`,
		f.tenant.NamespaceContainer("test-bucket"), f.row(t, "k.bin").storageKey, f.tenantID)
	require.NoError(t, err)
	hot.deleteSeen.Store(0)

	d := f.del(t, "k.bin")
	assert.Equal(t, http.StatusNoContent, d.StatusCode)
	for name, p := range map[string]string{"recorded": coldPath, "ledger's hot copy": hotPath, "retired_blobs copy": thirdPath} {
		_, statErr := os.Stat(p)
		assert.True(t, errors.Is(statErr, os.ErrNotExist), "%s removed", name)
	}
	assert.GreaterOrEqual(t, hot.deleteSeen.Load(), int32(1), "the hot copy was deleted by the DELETE itself")
}

func TestDelete_OneCopyFails_IsAFailure(t *testing.T) {
	// Before: 204 and the head row gone while the hot copy stayed (the
	// aftermath's settle failed quietly, Warn, for the daily reclaim).
	f := setupLocFixture(t)
	require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, "k.bin", mpBody(3000, 1), nil).Code)
	hot := f.others["other07"]
	f.plant(t, hot, "k.bin", mpBody(3000, 1))
	f.ledgerRow(t, "k.bin", "other07", "fixed")
	hot.deleteErr = errors.New("503 Service Unavailable")

	d := f.del(t, "k.bin")
	assert.Equal(t, http.StatusServiceUnavailable, d.StatusCode, "never 'deleted' while a copy may remain")
	assert.Equal(t, int64(3000), f.row(t, "k.bin").size, "the row (and the bill) are kept: the client retries")

	// The retry, once the backend answers, deletes both.
	hotPath := f.blobPath(t, hot, "k.bin")
	hot.deleteErr = nil
	d = f.del(t, "k.bin")
	assert.Equal(t, http.StatusNoContent, d.StatusCode)
	_, statErr := os.Stat(hotPath)
	assert.True(t, errors.Is(statErr, os.ErrNotExist), "the hot copy is gone")
}

func TestDelete_RacingPut_KeepsThePutsBlob(t *testing.T) {
	// PM-6 of WP-R2-1. The DELETE reads the row (address A), deletes A's
	// blob; a PUT of the key commits (address B) before the DELETE's row
	// delete. Before: the DELETE removed whatever row was there — the PUT's
	// — and B's blob leaked (no row named it until the erasure sweep); GET
	// 404 after a PUT that answered 200.
	f := setupLocFixture(t)
	require.Equal(t, http.StatusOK, httptestPut(f.mpChunkFixture, "k.bin", mpBody(3000, 1), nil).Code)
	first := f.row(t, "k.bin").storageKey
	newBody := mpBody(5000, 2)
	var putCode int
	f.primary.onDelete = func(artifact string) {
		f.primary.onDelete = nil
		putCode = httptestPut(f.mpChunkFixture, "k.bin", newBody, nil).Code // commits between the blob delete and the row delete
	}

	d := f.del(t, "k.bin")
	require.Equal(t, http.StatusOK, putCode)
	assert.Equal(t, http.StatusNoContent, d.StatusCode, "the DELETE answers 204: it is ordered before the PUT")
	row := f.row(t, "k.bin")
	assert.NotEqual(t, first, row.storageKey)
	assert.Equal(t, int64(len(newBody)), row.size, "the PUT's row is current")
	g := f.get(t, "k.bin", "")
	require.Equal(t, http.StatusOK, g.Code)
	assert.Equal(t, newBody, g.Body.Bytes(), "and its bytes are there")
	assert.Equal(t, int64(len(newBody)), f.storedBytes(t), "billed for the PUT's object, exactly")
}
