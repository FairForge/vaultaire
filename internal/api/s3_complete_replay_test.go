package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Prompt 2a.3 H3 — the post-merge review of #644. "Before" numbers are the
// same scenarios run against 7bd5c5c.

func (f *quotaAccountingFixture) complete(t *testing.T, key string) (uploadID, body string) {
	t.Helper()
	uploadID, body = uploadTwoParts(t, f.server, f.tenant, key)
	w := doS3Request(f.server, f.tenant, "POST", "/test-bucket/"+key+"?uploadId="+uploadID, strings.NewReader(body))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return uploadID, body
}

func (f *quotaAccountingFixture) replay(t *testing.T, key, uploadID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doS3Request(f.server, f.tenant, "POST", "/test-bucket/"+key+"?uploadId="+uploadID, strings.NewReader(body))
}

func TestCompleteMultipartUpload_AReplayNeverAddsARetentionTheObjectNeverHad(t *testing.T) {
	// Arrange: completed in a bucket without Object Lock; then a 3650-day
	// COMPLIANCE default is enabled. Before: the replay wrote the bucket's
	// CURRENT default — 1 lock row, DELETE 403, irrevocably.
	f := longOpFixture(t, 0)
	uploadID, body := f.complete(t, "plain.bin")
	_, err := f.db.Exec(`UPDATE buckets SET object_lock_enabled = TRUE, default_retention_mode = 'COMPLIANCE',
		default_retention_days = 3650, updated_at = NOW() WHERE tenant_id = $1 AND name = 'test-bucket'`, f.tenantID)
	require.NoError(t, err)

	// Act
	w := f.replay(t, "plain.bin", uploadID, body)

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 0, f.lockRows(t, "plain.bin"))
	assert.Equal(t, http.StatusNoContent, doS3Request(f.server, f.tenant, "DELETE", "/test-bucket/plain.bin", nil).Code)
}

func TestCompleteMultipartUpload_AReplayNeverReDatesAnExpiredRetention(t *testing.T) {
	// Arrange: the object's retention has expired. Before: the replay
	// re-dated it (retain-until moved into the future again).
	f := longOpFixture(t, 1)
	uploadID, body := f.complete(t, "expired.bin")
	past := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	_, err := f.db.Exec(`UPDATE object_locks SET retain_until_date = $2 WHERE tenant_id = $1 AND object_key = 'expired.bin'`, f.tenantID, past)
	require.NoError(t, err)

	// Act
	require.Equal(t, http.StatusOK, f.replay(t, "expired.bin", uploadID, body).Code)

	// Assert
	var until time.Time
	require.NoError(t, f.db.QueryRow(`SELECT retain_until_date FROM object_locks WHERE tenant_id = $1 AND object_key = 'expired.bin'`, f.tenantID).Scan(&until))
	assert.True(t, until.Equal(past), "re-dated to %v", until)
}

func TestCompleteMultipartUpload_AReplayWhoseLockCannotBeDeterminedWritesNoneAndCountsIt(t *testing.T) {
	// Arrange: the lock row is missing (an earlier cut) and the bucket's
	// configuration changed since the upload began: what the default was at
	// completion is unknown. Before: the current default was written.
	f := longOpFixture(t, 30)
	uploadID, body := f.complete(t, "unsure.bin")
	_, err := f.db.Exec(`DELETE FROM object_locks WHERE tenant_id = $1 AND object_key = 'unsure.bin'`, f.tenantID)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE buckets SET default_retention_days = 3650, updated_at = NOW() WHERE tenant_id = $1 AND name = 'test-bucket'`, f.tenantID)
	require.NoError(t, err)
	before := promtestutil.ToFloat64(completeReplayLockUndetermined)

	// Act
	require.Equal(t, http.StatusOK, f.replay(t, "unsure.bin", uploadID, body).Code)

	// Assert
	assert.Equal(t, 0, f.lockRows(t, "unsure.bin"))
	assert.Equal(t, before+1, promtestutil.ToFloat64(completeReplayLockUndetermined))
}

func TestCompleteMultipartUpload_AnUploadCompletesOnlyAtItsOwnBucketAndKey(t *testing.T) {
	// Before: an upload for a.bin completed at /test-bucket/b.bin created
	// b.bin (200), and at /other-bucket/a.bin too. AWS: NoSuchUpload.
	f := longOpFixture(t, 0)
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, 'other-bucket', 'private')`, f.tenantID)
	require.NoError(t, err)
	uploadID, body := uploadTwoParts(t, f.server, f.tenant, "a.bin")

	otherKey := doS3Request(f.server, f.tenant, "POST", "/test-bucket/b.bin?uploadId="+uploadID, strings.NewReader(body))
	otherBucket := doS3Request(f.server, f.tenant, "POST", "/other-bucket/a.bin?uploadId="+uploadID, strings.NewReader(body))
	part := doS3Request(f.server, f.tenant, "PUT", "/test-bucket/b.bin?uploadId="+uploadID+"&partNumber=3", strings.NewReader("x"))

	assert.Equal(t, http.StatusNotFound, otherKey.Code, otherKey.Body.String())
	assert.Contains(t, otherKey.Body.String(), "NoSuchUpload")
	assert.Equal(t, http.StatusNotFound, otherBucket.Code, otherBucket.Body.String())
	assert.Equal(t, http.StatusNotFound, part.Code, part.Body.String())
	_, _, exists := f.headRow(t, "b.bin")
	assert.False(t, exists, "b.bin must not exist")
	// The upload still completes at its own key.
	assert.Equal(t, http.StatusOK, doS3Request(f.server, f.tenant, "POST", "/test-bucket/a.bin?uploadId="+uploadID, strings.NewReader(body)).Code)
}

func (f *quotaAccountingFixture) latestVersions(t *testing.T, key string) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM object_versions WHERE tenant_id = $1 AND object_key = $2 AND is_latest`,
		f.tenantID, key).Scan(&n))
	return n
}

func TestCompleteMultipartUpload_ConcurrentReplaysWriteOneLatestVersion(t *testing.T) {
	// Arrange: a versioned bucket; the version row was lost to a cut. Two
	// retries are held after their write, before their commit, until both
	// got there (or 300 ms passed: with the key locked the second waits for
	// the first's commit, then finds the row and writes nothing). Before:
	// each checked "no latest row" and inserted one — 2 is_latest rows (the
	// review: 19 of 20 runs under real concurrency).
	f := longOpFixture(t, 0)
	_, err := f.db.Exec(`UPDATE buckets SET versioning_status = 'Enabled' WHERE tenant_id = $1 AND name = 'test-bucket'`, f.tenantID)
	require.NoError(t, err)
	uploadID, body := f.complete(t, "v.bin")
	_, err = f.db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1 AND object_key = 'v.bin'`, f.tenantID)
	require.NoError(t, err)
	var mu sync.Mutex
	arrived := 0
	reassertVersionWrittenHook = func() {
		mu.Lock()
		arrived++
		mu.Unlock()
		deadline := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := arrived
			mu.Unlock()
			if n >= 2 {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Cleanup(func() { reassertVersionWrittenHook = func() {} })

	// Act
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.replay(t, "v.bin", uploadID, body)
		}()
	}
	wg.Wait()

	// Assert
	assert.Equal(t, 1, f.latestVersions(t, "v.bin"))
}

func TestCompleteMultipartUpload_AReplayAfterVersioningWasEnabledMintsNoVersion(t *testing.T) {
	// Before: the replay minted a random version id for an object that was
	// written before versioning existed on the bucket (AWS: its version is "null").
	f := longOpFixture(t, 0)
	uploadID, body := f.complete(t, "pre.bin")
	_, err := f.db.Exec(`UPDATE buckets SET versioning_status = 'Enabled', updated_at = NOW() WHERE tenant_id = $1 AND name = 'test-bucket'`, f.tenantID)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, f.replay(t, "pre.bin", uploadID, body).Code)

	assert.Equal(t, 0, f.latestVersions(t, "pre.bin"))
}

func TestLongOpIncidents_AnAccountErasureNeverLowersTheExportedCount(t *testing.T) {
	// Before: EraseRows deleted the tenant's rows — the exported total fell
	// from 2 to 0, which increase() reads as a counter reset (a false alert
	// at the next incident).
	f := longOpFixture(t, 0)
	_, err := f.db.Exec(`INSERT INTO s3_long_op_incidents (outcome, op, tenant_id, bucket, object_key) VALUES
		('abandoned', 'CompleteMultipartUpload', $1, 'test-bucket', 'a'), ('abandoned', 'CompleteMultipartUpload', $1, 'test-bucket', 'b')`, f.tenantID)
	require.NoError(t, err)
	var before int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM s3_long_op_incidents`).Scan(&before))

	userID := uuid.NewString()
	_, err = f.db.Exec(`INSERT INTO users (id, email, password_hash, company, status, deletion_scheduled_at)
		VALUES ($1, $2, 'x', 'H3', 'pending_deletion', NOW() - INTERVAL '1 hour')`, userID, "h3-"+userID[:8]+"@test.local")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM users WHERE id::text = $1`, userID) })
	_, err = account.NewService(f.db, zap.NewNop()).EraseRows(context.Background(), userID, f.tenantID, "", time.Now())
	require.NoError(t, err)

	var after, personal int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM s3_long_op_incidents`).Scan(&after))
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM s3_long_op_incidents WHERE tenant_id = $1 OR object_key IN ('a', 'b') AND bucket = 'test-bucket'`, f.tenantID).Scan(&personal))
	assert.Equal(t, before, after, "the rows stay, counted")
	assert.Equal(t, 0, personal, "nothing of the account is left on them")
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM s3_long_op_incidents WHERE tenant_id = '' AND op = 'CompleteMultipartUpload' AND age_seconds = 0 AND slot = ''`)
	})
}

func collectIncidentSeries(t *testing.T, c prometheus.Collector) map[string]float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(c))
	mfs, err := reg.Gather()
	require.NoError(t, err)
	out := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			out[mf.GetName()+"/"+m.GetLabel()[0].GetValue()] = m.GetCounter().GetValue()
		}
	}
	return out
}

func TestLongOpIncidents_AnUnreadTableExportsNoSeriesAndAValueNeverFalls(t *testing.T) {
	// Before: a failed first read after a restart exported 0 for every op
	// (the next good read then looked like an increase of N), and a lower
	// count was exported as is.
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	if db.Ping() != nil {
		t.Skip("test database unreachable")
	}
	broken, err := sql.Open("postgres", "postgres://nobody@127.0.0.1:1/x?sslmode=disable&connect_timeout=1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close(); _ = broken.Close() })

	// Act 1: the table cannot be read.
	assert.Empty(t, collectIncidentSeries(t, newLongOpIncidentsCollector(broken, zap.NewNop())), "unknown: no series")

	// Act 2: read N, then the table shows fewer.
	c := newLongOpIncidentsCollector(db, zap.NewNop())
	first := collectIncidentSeries(t, c)
	key := "vaultaire_s3_long_ops_abandoned_total/" + longOpCopy
	c.mu.Lock()
	c.counts[[2]string{longOpAbandoned, longOpCopy}] += 5 // what an earlier read saw
	high := c.counts[[2]string{longOpAbandoned, longOpCopy}]
	c.at = time.Time{} // the next scrape reads again
	c.mu.Unlock()
	second := collectIncidentSeries(t, c)

	assert.Contains(t, first, key)
	assert.Equal(t, high, second[key], "never lower than what was exported")
}

// heldDB is a one-connection pool on the test database whose connection a
// transaction holds: every other statement waits for its context.
func heldDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	if db.Ping() != nil {
		t.Skip("test database unreachable")
	}
	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(); _ = db.Close() })
	return db
}

func TestLongOps_TheDrainCutsEveryDueOperationBeforeWritingItsIncident(t *testing.T) {
	// Arrange: two operations past their bound, a database that does not
	// answer. Before: each incident was written (5 s timeout) BEFORE its
	// operation was cancelled — the second cut came 5 s late, the drain
	// returned after ≥ 10 s.
	s := &Server{logger: zap.NewNop(), db: heldDB(t)}
	var mu sync.Mutex
	cancelled := map[string]time.Time{}
	reg := s.longOps()
	for _, k := range []string{"a.bin", "b.bin"} {
		e := &longOpEntry{longOpInfo: longOpInfo{Op: longOpComplete, Bucket: "b", Key: k}, tenant: "t", started: time.Now().Add(-time.Hour)}
		e.cancel = func() {
			mu.Lock()
			cancelled[k] = time.Now()
			mu.Unlock()
			reg.remove(e)
		}
		reg.add(e)
	}

	// Act
	start := time.Now()
	n := s.drainLongOps(time.Minute)

	// Assert
	assert.Equal(t, 2, n)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, cancelled, 2)
	for k, at := range cancelled {
		assert.Less(t, at.Sub(start), time.Second, "%s cut %v after the drain began", k, at.Sub(start))
	}
}
