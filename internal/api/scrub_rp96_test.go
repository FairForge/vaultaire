package api

// RP96 — the review of WP-STRIPE-10 (Prompt 58, the blob scrub). The
// probes that became the review's fixes.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
)

// RP96-1: a sealed blob is longer than its row's size_bytes (the header and
// a tag per segment). The scrub compared the stored length to the PLAINTEXT
// size and wrote `mismatch` for a healthy SSE-C object — prod's one finding
// (72 bytes, the row says 27). The stored length of a streaming blob is a
// function of the plaintext size and the header the blob carries.
func TestScrub_ASealedBlobIsMeasuredByItsSealedLength(t *testing.T) {
	f := setupBlobScrubFixture(t, 24*time.Hour)
	f.job.Every = 24 * time.Hour // one run reads the whole footprint
	key := []byte("0123456789abcdef0123456789abcdef")
	plain := []byte("twenty-seven bytes of text!")
	require.Len(t, plain, 27)
	h := f.putObject("ssec.txt", plain, ssecHeaders(key))
	require.Equal(t, int64(27), h.size, "the row records the plaintext size")
	stored := f.blobOf("ssec.txt")
	require.Equal(t, crypto.SSECSealedLength(27), int64(len(stored)), "the fixture seals as prod does")

	// Healthy: the stored bytes' MD5 is the row's backend_etag, the length
	// the sealed one. No finding.
	res := f.run()
	st := res.Backends["edgefake"]
	require.NotNil(t, st)
	assert.Equal(t, 1, st.Checked[scrubKindWhole])
	assert.Equal(t, 0, st.Failed, "a healthy sealed blob is not a mismatch")
	assert.Equal(t, 0, st.Unverifiable, "its stored MD5 is on the row (backend_etag)")
	assert.Empty(t, f.findings())

	// Truncated by one byte: a mismatch (the length), as for a plaintext blob.
	f.drv.mu.Lock()
	for k, b := range f.drv.objects {
		if len(b) == len(stored) {
			f.drv.objects[k] = b[:len(b)-1]
		}
	}
	f.drv.mu.Unlock()
	f.job.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	res = f.run()
	st = res.Backends["edgefake"]
	assert.Equal(t, 1, st.Failed)
	rows := f.findings()
	require.Len(t, rows, 1)
	assert.Equal(t, scrubMismatch, rows[0].verdict)
	assert.Equal(t, "ssec.txt", rows[0].key)
}

// RP96-1 (the version half): a retained sealed version records no backend
// ETag (RP72-1), so it is `unverifiable` — never a mismatch for its length.
func TestScrub_ASealedVersionIsUnverifiableNotAMismatch(t *testing.T) {
	f := setupBlobScrubFixture(t, 24*time.Hour)
	f.job.Every = 24 * time.Hour
	f.versioned()
	key := []byte("0123456789abcdef0123456789abcdef")
	f.putObject("v.txt", []byte("the first body, sealed"), ssecHeaders(key))
	f.putObject("v.txt", []byte("the second body, sealed, longer than the first"), ssecHeaders(key))
	var retained int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM object_versions WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'v.txt' AND NOT is_latest AND storage_key IS NOT NULL`,
		f.tenantID, f.bucket).Scan(&retained))
	require.Equal(t, 1, retained, "the first body is a retained version")

	// The slice is sized in plaintext bytes and the reads are sealed ones,
	// so a run may end a blob early: two runs cover the whole and the version.
	first := f.run().Backends["edgefake"]
	second := f.run().Backends["edgefake"]
	require.NotNil(t, first)
	require.NotNil(t, second)
	assert.Equal(t, 1, first.Checked[scrubKindWhole]+second.Checked[scrubKindWhole])
	assert.Equal(t, 1, first.Checked[scrubKindVersion]+second.Checked[scrubKindVersion])
	assert.Equal(t, 0, first.Failed+second.Failed, "a sealed version's length is the sealed one")
	assert.Equal(t, 1, first.Unverifiable+second.Unverifiable, "no stored MD5 on a version row")
	assert.Empty(t, f.findings())
}

// RP96-B-1 / B-4: the bucket's line is true of the bucket's own bytes: every
// backend they are on has a finished cycle that is not stale (within two
// cycles). The scrub never reads tape (geyser), so a bucket holding a tape
// object gets no line; a bucket with no bytes claims nothing.
func TestDashboard_BucketLineNeedsEveryBackendOfTheBucketsBytes(t *testing.T) {
	f := setupBlobScrubFixture(t, 24*time.Hour)
	prior := handlers.ScrubJobName
	handlers.ScrubJobName = f.job.JobName
	t.Cleanup(func() { handlers.ScrubJobName = prior })
	line := func(b string) string { return handlers.BucketVerifiedLine(context.Background(), f.db, f.tenantID, b) }

	f.putObject("a.bin", testBytes(1000), nil)
	f.exec(`UPDATE feature_flags SET updated_at = NOW() - interval '1 hour' WHERE flag_key = $1 AND tenant_id = $2`, flagBlobScrub, f.tenantID)
	f.job.Every = 24 * time.Hour
	res := f.run()
	st := res.Backends["edgefake"]
	require.NotNil(t, st)
	require.False(t, st.CycleCompletedAt.IsZero())
	date := st.CycleCompletedAt.UTC().Format("2 January 2006")
	require.Equal(t, "Every file here was read back and checked on "+date+".", line(f.bucket))

	// A bucket with no bytes: nothing to claim.
	assert.Empty(t, line("no-such-bucket"), "an empty or unknown bucket claims nothing")

	// The bucket's one object is on tape: this scrub never reads it.
	f.exec(`UPDATE object_head_cache SET backend_name = 'geyser' WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'a.bin'`, f.tenantID, f.bucket)
	assert.Empty(t, line(f.bucket), "a bucket holding a tape object gets no line")
	f.exec(`UPDATE object_head_cache SET backend_name = 'edgefake' WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'a.bin'`, f.tenantID, f.bucket)
	require.NotEmpty(t, line(f.bucket))

	// A second object on a backend that never finished a cycle: no line.
	f.exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, is_chunked, created_at, updated_at, last_accessed)
		VALUES ($1, $2, 'elsewhere.bin', 10, 'd41d8cd98f00b204e9800998ecf8427e', 'edgemirror', false, NOW(), NOW(), NOW())`, f.tenantID, f.bucket)
	assert.Empty(t, line(f.bucket), "a backend of the bucket's bytes with no finished cycle")
	f.exec(`DELETE FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = 'elsewhere.bin'`, f.tenantID, f.bucket)
	require.NotEmpty(t, line(f.bucket))

	// The finished cycle is older than two cycles: stale, nothing claimed.
	f.exec(`UPDATE job_runs SET result = jsonb_set(result, '{backends,edgefake,cycle_completed_at}', to_jsonb((NOW() - interval '3 days')::timestamptz)) WHERE job = $1`, f.job.JobName)
	assert.Empty(t, line(f.bucket), "a cycle that finished more than two cycles ago is stale")
}

// RP96-A-1: a blob whose read never answers (a deadline — a blob larger than
// the run can read — a timeout, an archived answer from a hot backend) used
// to stop the slice at the same cursor every run, for ever: nothing behind
// it was read again, and the bytes read before each cut were egress every
// run. After three runs without an answer it is a finding (`unreadable`,
// the detail says why) and the walk goes on.
func TestScrub_ABlobWithNoAnswerThreeRunsIsAFindingAndTheWalkGoesOn(t *testing.T) {
	f := setupBlobScrubFixture(t, 24*time.Hour)
	f.job.Every = 24 * time.Hour
	f.putObject("a.bin", testBytes(100), nil)
	huge := f.putObject("huge.bin", testBytes(100), nil)
	f.putObject("z.bin", testBytes(100), nil)
	_, hugeBlob := objectBlobAddress(f.tenantID, f.bucket, "huge.bin", huge.sk)
	f.drv.getHook = func(artifact string) error {
		if artifact == hugeBlob {
			return context.DeadlineExceeded
		}
		return nil
	}

	first := f.run().Backends["edgefake"]
	assert.Equal(t, 1, first.Checked[scrubKindWhole], "a.bin, then the slice stops at huge.bin")
	assert.Equal(t, "a.bin", first.Cursor.Key)
	assert.Empty(t, f.findings())
	second := f.run().Backends["edgefake"]
	assert.Equal(t, "a.bin", second.Cursor.Key, "no answer twice: still before it")
	assert.Empty(t, f.findings())

	third := f.run().Backends["edgefake"]
	rows := f.findings()
	require.Len(t, rows, 1, "the third run without an answer is a finding")
	assert.Equal(t, "huge.bin", rows[0].key)
	assert.Equal(t, scrubUnreadable, rows[0].verdict)
	assert.Equal(t, 1, third.Failed)
	assert.Equal(t, 2, third.Checked[scrubKindWhole], "huge.bin (unreadable) and z.bin — the walk went on")
	assert.False(t, third.CycleCompletedAt.IsZero(), "the cycle finished")
	var detail string
	require.NoError(t, f.db.QueryRow(`SELECT detail FROM scrub_findings WHERE tenant_id = $1 AND object_key = 'huge.bin'`, f.tenantID).Scan(&detail))
	assert.Contains(t, detail, "no answer in 3 runs")
	assert.Contains(t, detail, "deadline exceeded")

	// The next cycle: the hook lifted, the blob reads fine, nothing new.
	f.drv.getHook = nil
	f.job.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	res := f.run().Backends["edgefake"]
	assert.Equal(t, 3, res.Checked[scrubKindWhole])
	assert.Equal(t, 0, res.Failed)
	assert.Len(t, f.findings(), 1)
}
