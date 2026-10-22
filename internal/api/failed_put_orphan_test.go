package api

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// A backend that keeps a body and answers an error (Blomp's proxy answered
// 503 to two PUTs on 2026-10-21 and the store kept both, 64 MiB each): the
// write fails and no row names the blob, so before this nothing ever
// removed it. Now the address the failed write used is queued for the
// collector on every backend that was sent the body.

// orphanWrites lists the storage keys queued as orphan_write on the fixed
// backend for key's addresses.
func (f *mpChunkFixture) orphanWrites(t *testing.T, key string) []string {
	t.Helper()
	rows, err := f.db.Query(`SELECT storage_key FROM retired_blobs
		WHERE tenant_id = $1 AND backend_id = 'fixed' AND reason = 'orphan_write' AND storage_key LIKE $2 ORDER BY storage_key`,
		f.tenantID, key+"@%")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

// orphanCollector is the collector scoped to the fixture's tenant (the test
// database is shared: a run must never collect another test's rows), its
// clock a minute ahead so a row queued at the database's NOW() is due.
func (f *mpChunkFixture) orphanCollector() *RetiredBlobCollector {
	c := NewRetiredBlobCollector(f.db, f.server.engine, f.server.gci, zap.NewNop())
	c.onlyTenant = f.tenantID
	c.JobName = "test-retired-" + f.tenantID
	c.now = func() time.Time { return time.Now().Add(time.Minute) }
	return c
}

func (f *mpChunkFixture) blobFile(addr string) string {
	return f.fixed.path(f.tenantID, f.tenantID+"_test-bucket", addr)
}

func TestFailedPut_StoredBehindAnError_IsQueuedAndCollected(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	f.fixed.storeThenFail.Store(true)

	w := f.req(t, "PUT", "/test-bucket/leak.bin", []byte("stored behind a 503"), nil)

	require.GreaterOrEqual(t, w.Code, 500, w.Body.String())
	assert.Zero(t, f.headRows(t, "leak.bin"))
	queued := f.orphanWrites(t, "leak.bin")
	require.Len(t, queued, 1, "the failed write's address is queued")
	assert.FileExists(t, f.blobFile(queued[0]), "the backend kept the body")

	f.fixed.storeThenFail.Store(false)
	_, err := f.orphanCollector().RunOnce(context.Background())
	require.NoError(t, err)
	assert.NoFileExists(t, f.blobFile(queued[0]), "the collector removed it")
	assert.Empty(t, f.orphanWrites(t, "leak.bin"))
}

func TestFailedPut_NothingSent_QueuesNothing(t *testing.T) {
	// A backend that answered before taking anything (the plain 5xx double
	// drains and answers; an open breaker is never asked) still gets the
	// address queued — harmless, the collector's delete is a miss — but a
	// refusal before any request (a quota 403) queues nothing.
	f := setupMPChunkFixture(t, false)
	_, err := f.db.Exec(`UPDATE tenant_quotas SET storage_limit_bytes = 1 WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)

	w := f.req(t, "PUT", "/test-bucket/over.bin", []byte("over quota"), nil)

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Empty(t, f.orphanWrites(t, "over.bin"))
}

func TestFailedComplete_StoredBehindAnError_KeptForTheRetry(t *testing.T) {
	// Complete reuses its recorded address on a retry: the queued blob is
	// kept while the upload is active and the row is dropped once the retry
	// commits it — never the committed object's bytes.
	f := setupMPChunkFixture(t, false)
	body := mpBody(900, 3)
	u := f.upload(t, "mp.bin", splitParts(body, 2), nil)
	f.fixed.storeThenFail.Store(true)

	w := f.complete(t, "mp.bin", u)

	require.GreaterOrEqual(t, w.Code, 500, w.Body.String())
	queued := f.orphanWrites(t, "mp.bin")
	require.Len(t, queued, 1)
	collector := f.orphanCollector()
	f.fixed.storeThenFail.Store(false)
	_, err := collector.RunOnce(context.Background())
	require.NoError(t, err)
	assert.FileExists(t, f.blobFile(queued[0]), "the active upload owns the address")

	w = f.complete(t, "mp.bin", u)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, queued[0], f.row(t, "mp.bin").storageKey, "the retry committed the same address")
	collector.now = func() time.Time { return time.Now().Add(retiredBlobUploadRecheck + time.Minute) } // the re-check is due
	_, err = collector.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Empty(t, f.orphanWrites(t, "mp.bin"), "a named blob's queue row is dropped")
	got, err := os.ReadFile(f.blobFile(queued[0]))
	require.NoError(t, err)
	assert.Equal(t, body, got)
	assert.Equal(t, body, f.getBytes(t, "mp.bin", ""))
}
