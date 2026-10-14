package api

// WP-R8-5 Part 0: the worker's post-merge probe of WP-R3-1 (#703), driven at
// the handlers (docs/reviews/WP-R3-1.md § "Post-merge review", worker probe).

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// probeBody is n random bytes with the plaintext marker every 512 bytes:
// unique chunk hashes, so the content index's ref counts are this test's own.
func probeBody(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	for i := 0; i+len(mpMarker) <= n; i += 512 {
		copy(b[i:], mpMarker)
	}
	return b
}

// keyRefCounts is the key's manifest rows and the sum of the content index's
// ref_count over the hashes the manifest names.
func (f *mpChunkFixture) keyRefCounts(t *testing.T, key string) (rows, refSum int) {
	t.Helper()
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id = $1 AND object_key = $2`,
		f.tenantID, key).Scan(&rows))
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(SUM(g.ref_count), 0) FROM global_content_index g
		WHERE (g.dedup_scope, g.plaintext_hash) IN (SELECT dedup_scope, plaintext_hash FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND object_key = $2)`, f.tenantID, key).Scan(&refSum))
	return rows, refSum
}

// Probe 1: two Completes of the same chunked upload in flight at once, both
// past every refusal and reading the parts together.
func TestProbe_TwoConcurrentChunkedCompletes_RefsAndBillExact(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	body := probeBody(t, 3<<20)
	u := f.upload(t, "k.bin", splitParts(body, 3), nil)

	// Both requests open the last part before either finishes: the first
	// to commit removes the staging directory while the other holds the
	// last file open.
	var mu sync.Mutex
	arrived := 0
	release := make(chan struct{})
	stagedPartReadHook = func(part int) {
		if part != 3 {
			return
		}
		mu.Lock()
		arrived++
		if arrived == 2 {
			close(release)
		}
		mu.Unlock()
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
	}
	t.Cleanup(func() { stagedPartReadHook = func(int) {} })

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = f.complete(t, "k.bin", u).Code
		}(i)
	}
	wg.Wait()
	stagedPartReadHook = func(int) {}

	assert.Equal(t, []int{http.StatusOK, http.StatusOK}, codes)
	row := f.row(t, "k.bin")
	assert.True(t, row.chunked)
	rows, refSum := f.keyRefCounts(t, "k.bin")
	assert.Positive(t, rows)
	assert.Equal(t, rows, refSum, "one object's chunks, ref_count 1 each")
	assert.Equal(t, int64(len(body)), f.used(t), "billed once")
	assert.Equal(t, body, f.get(t, "k.bin", "").Body.Bytes())
}

// Probe 1b: the same, without the barrier — whatever order the two take,
// the refs and the bill end exact and a client retry answers the object.
func TestProbe_TwoConcurrentChunkedCompletes_Unsynchronised(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	body := probeBody(t, 3<<20)
	u := f.upload(t, "k.bin", splitParts(body, 3), nil)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = f.complete(t, "k.bin", u).Code
		}(i)
	}
	wg.Wait()
	t.Logf("codes %v", codes)

	assert.Contains(t, codes, http.StatusOK)
	rows, refSum := f.keyRefCounts(t, "k.bin")
	assert.Equal(t, rows, refSum)
	assert.Equal(t, int64(len(body)), f.used(t))
	retry := f.complete(t, "k.bin", u)
	assert.Equal(t, http.StatusOK, retry.Code, "a retry of the loser is a replay")
	assert.Equal(t, int64(len(body)), f.used(t))
}

// Probe 2: the reaper aborts the upload while Complete is between parts
// (its UPDATE + RemoveAll, run for this upload only: the real RunOnce would
// abort other packages' uploads on the shared database).
func TestProbe_ChunkedCompleteRacingTheReaper(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	require.Equal(t, http.StatusOK, f.put(t, "k.bin", []byte("previous")))
	body := probeBody(t, 3<<20)
	u := f.upload(t, "k.bin", splitParts(body, 3), nil)
	reap := func() {
		// The reaper's abort: the status and the release of what the upload
		// holds, in one transaction (WP-R10-8 Part C).
		ended, err := endUploadReleasing(context.Background(), f.db, u.id, "", "", "")
		require.NoError(t, err)
		if ended {
			_ = os.RemoveAll(multipartDir(u.id))
		}
	}
	stagedPartReadHook = func(part int) {
		if part == 2 {
			reap()
		}
	}
	t.Cleanup(func() { stagedPartReadHook = func(int) {} })

	w := f.complete(t, "k.bin", u)
	stagedPartReadHook = func(int) {}
	t.Logf("complete: %d %s", w.Code, w.Body.String())

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, "previous", f.get(t, "k.bin", "").Body.String(), "nothing committed")
	assert.Equal(t, int64(len("previous")), f.used(t), "reservation released")
	rows, _ := f.keyRefCounts(t, "k.bin")
	assert.Zero(t, rows)
	var orphanRefs int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id = $1`, f.tenantID).Scan(&orphanRefs))
	assert.Zero(t, orphanRefs, "refs compensated")
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM multipart_uploads WHERE upload_id = $1`, u.id).Scan(&status))
	assert.Equal(t, "aborted", status)

	retry := f.complete(t, "k.bin", u)
	assert.Equal(t, http.StatusNotFound, retry.Code)
	assert.Contains(t, retry.Body.String(), "NoSuchUpload")
}

// Probe 3: a sealed Complete in a versioning-Enabled bucket.
func TestProbe_SealedCompleteInVersionedBucket(t *testing.T) {
	f := setupMPChunkFixture(t, true)
	f.bucketSet(t, "sse_enabled = TRUE, versioning_status = 'Enabled'")
	first := probeBody(t, 12<<10)
	f.multipart(t, "k.bin", first, 2, nil)
	var oldVersion, alg string
	var keyVersion *int
	require.NoError(t, f.db.QueryRow(`SELECT version_id, COALESCE(encryption_algorithm, ''), encryption_key_version FROM object_versions
		WHERE tenant_id = $1 AND object_key = 'k.bin' AND is_latest`, f.tenantID).Scan(&oldVersion, &alg, &keyVersion))
	assert.Equal(t, "ML-KEM-768+AES-256-GCM", alg)
	require.NotNil(t, keyVersion)
	assert.Equal(t, 1, *keyVersion)

	second := probeBody(t, 12<<10)
	f.multipart(t, "k.bin", second, 2, nil)
	assert.Zero(t, plaintextOnBackend(t, f.fixed.dir))

	g := doS3Request(f.server, f.tenant, "GET", "/test-bucket/k.bin?versionId="+oldVersion, nil)
	require.Equal(t, http.StatusOK, g.Code, g.Body.String())
	assert.Equal(t, first, g.Body.Bytes())
	assert.Equal(t, "AES256", g.Header().Get("x-amz-server-side-encryption"))
	h := doS3Request(f.server, f.tenant, "HEAD", "/test-bucket/k.bin?versionId="+oldVersion, nil)
	require.Equal(t, http.StatusOK, h.Code)
	assert.Equal(t, "AES256", h.Header().Get("x-amz-server-side-encryption"))
	assert.Equal(t, second, f.get(t, "k.bin", "").Body.Bytes())
}

// Probe 4: PUT asking for AES256 on a process with no master key, into a
// bucket that is not sse_enabled. On 59e2769: 200, stored plaintext, no
// header claiming encryption (W-4, P2). Since WP-R8-5 Part A: refused 503,
// as an sse_enabled bucket is (TestPut_SSEHeader_NoKey_Refused503).
func TestProbe_PutSSEHeader_NoKey_NonSSEBucket(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	body := probeBody(t, 900)
	r := httptestPut(f, "small.bin", body, map[string]string{"x-amz-server-side-encryption": "AES256"})
	assert.Equal(t, http.StatusServiceUnavailable, r.Code)
	assert.Empty(t, r.Header().Get("x-amz-server-side-encryption"), "never claims encryption it did not apply")
	assert.Zero(t, plaintextOnBackend(t, f.fixed.dir))
	c := f.create(t, "mp.bin", map[string]string{"x-amz-server-side-encryption": "AES256"})
	assert.Equal(t, http.StatusServiceUnavailable, c.Code)
}

func httptestPut(f *mpChunkFixture, key string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req = req.WithContext(f.ctx(req.Context()))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, req)
	return w
}
