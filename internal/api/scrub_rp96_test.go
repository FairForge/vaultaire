package api

// RP96 — the review of WP-STRIPE-10 (Prompt 58, the blob scrub). The
// probes that became the review's fixes.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/crypto"
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
