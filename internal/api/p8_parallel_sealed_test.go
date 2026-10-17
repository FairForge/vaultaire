package api

// Stage 1a Prompt 8, Part B: `parallel_get` for sealed objects and for
// region-pinned buckets. A sealed (0x03) whole object — SSE-S3 or SSE-C —
// is read as parallel ranges aligned to its segments, each part opened as
// it arrives; a bucket pinned to a region reads from its idrive-<region>
// driver in parallel like the primary. "Before" = these tests on
// origin/main (5c383e8): a sealed object was always one stream.

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/parfetch"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sealedParallelFixture: the parallel fixture with an SSE service, parts of
// two segments (2 MiB) and a 4 MiB threshold.
func setupSealedParallelFixture(t *testing.T, flagOn bool) *parallelFixture {
	t.Helper()
	f := setupParallelFixture(t, flagOn)
	f.adapter.sseService = crypto.NewSSEService(testTenantKeys(t, f.db, keyTestMasterA))
	f.adapter.largeGet.partBytes = 2 << 20
	f.adapter.largeGet.minBytes = 4 << 20
	f.adapter.largeGet.budget = parfetch.NewBudget(256 << 20)
	return f
}

// putSealed stores content sealed: SSE-S3 (key nil) or SSE-C (key set).
func (f *parallelFixture) putSealed(t *testing.T, key string, content, ssecKey []byte) string {
	t.Helper()
	req := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(content))
	req.ContentLength = int64(len(content))
	if ssecKey != nil {
		setSSECHeaders(t, req, ssecKey)
	} else {
		req.Header.Set("x-amz-server-side-encryption", "AES256")
	}
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", key)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var alg string
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(encryption_algorithm, '') FROM object_head_cache
		WHERE tenant_id = $1 AND object_key = $2`, f.tenant.ID, key).Scan(&alg))
	require.NotEmpty(t, alg, "stored sealed")
	return w.Header().Get("ETag")
}

func ssecHdr(key []byte) map[string]string { return ssecHeaders(key) }

func TestParallelSealed_SSES3_WholeObjectInParallelByteForByte(t *testing.T) {
	f := setupSealedParallelFixture(t, true)
	f.adapter.largeGet.hedgeAfter = 0     // the range count below is exact: no hedged second request on a slow runner
	content := randomBytes(9<<20 + 77777) // 10 segments, the last one short
	etag := f.putSealed(t, "sealed.bin", content, nil)
	f.drv.delay = func(off int64, _ int32) time.Duration { return time.Duration((off/(1<<20))%5) * time.Millisecond }
	started := testutil.ToFloat64(largeGetParallelReads.WithLabelValues("ranged", "sealed"))

	w := f.get(t, "sealed.bin", nil)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.False(t, f.cut)
	assert.True(t, bytes.Equal(content, w.Body.Bytes()), "the plaintext, in order")
	assert.Equal(t, strconv.Itoa(len(content)), w.Header().Get("Content-Length"))
	assert.Equal(t, etag, w.Header().Get("ETag"))
	assert.Equal(t, "AES256", w.Header().Get("x-amz-server-side-encryption"))
	assert.Equal(t, int32(0), f.drv.gets.Load(), "no single-stream GET")
	assert.Equal(t, int32(1+5), f.drv.ranges.Load(), "the header, then one range per two segments")
	assert.Equal(t, started+1, testutil.ToFloat64(largeGetParallelReads.WithLabelValues("ranged", "sealed")))
	assert.Equal(t, "closed", f.eng.GetFailoverStatus()["ranged"])
}

func TestParallelSealed_RangeInsideSegmentsIsTheExactSlice(t *testing.T) {
	f := setupSealedParallelFixture(t, true)
	content := randomBytes(12 << 20)
	etag := f.putSealed(t, "sealed.bin", content, nil)
	start, end := int64(1<<20+12345), int64(9<<20+999) // starts and ends mid-segment

	w := f.get(t, "sealed.bin", map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", start, end)})

	require.Equal(t, http.StatusPartialContent, w.Code, w.Body.String())
	assert.True(t, bytes.Equal(content[start:end+1], w.Body.Bytes()), "the exact slice")
	assert.Equal(t, fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)), w.Header().Get("Content-Range"))
	assert.Equal(t, etag, w.Header().Get("ETag"))
	assert.Greater(t, f.drv.ranges.Load(), int32(2), "parallel within the range")
	assert.Equal(t, int32(0), f.drv.gets.Load())
}

func TestParallelSealed_SSEC_InParallel_AndAWrongKeyIsStill403(t *testing.T) {
	f := setupSealedParallelFixture(t, true)
	key := bytes.Repeat([]byte{7}, 32)
	content := randomBytes(6<<20 + 3)
	f.putSealed(t, "ssec.bin", content, key)

	w := f.get(t, "ssec.bin", ssecHdr(key))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, bytes.Equal(content, w.Body.Bytes()))
	assert.Equal(t, "AES256", w.Header().Get("x-amz-server-side-encryption-customer-algorithm"))
	assert.Equal(t, int32(0), f.drv.gets.Load(), "parallel")

	wrong := f.get(t, "ssec.bin", ssecHdr(bytes.Repeat([]byte{8}, 32)))
	assert.Equal(t, http.StatusForbidden, wrong.Code, "decided before the status line, as on the single stream")
	assert.NotContains(t, wrong.Body.String(), string(content[:64]))
}

func TestParallelSealed_FlagOffSmallAndVaultAreTheSingleStream(t *testing.T) {
	t.Run("flag off", func(t *testing.T) {
		f := setupSealedParallelFixture(t, false)
		content := randomBytes(9 << 20)
		f.putSealed(t, "s.bin", content, nil)
		w := f.get(t, "s.bin", nil)
		require.Equal(t, http.StatusOK, w.Code)
		assert.True(t, bytes.Equal(content, w.Body.Bytes()))
		assert.Equal(t, int32(1), f.drv.gets.Load())
	})
	t.Run("under the threshold", func(t *testing.T) {
		f := setupSealedParallelFixture(t, true)
		content := randomBytes(3 << 20)
		f.putSealed(t, "s.bin", content, nil)
		w := f.get(t, "s.bin", nil)
		require.Equal(t, http.StatusOK, w.Code)
		assert.True(t, bytes.Equal(content, w.Body.Bytes()))
		assert.Equal(t, int32(1), f.drv.gets.Load())
	})
	t.Run("vault floor", func(t *testing.T) {
		f := setupSealedParallelFixture(t, true)
		content := randomBytes(9 << 20)
		f.putSealed(t, "s.bin", content, nil)
		_, err := f.db.Exec(`UPDATE object_head_cache SET floor = 'vault' WHERE tenant_id = $1 AND object_key = 's.bin'`, f.tenant.ID)
		require.NoError(t, err)
		w := f.get(t, "s.bin", nil)
		require.Equal(t, http.StatusOK, w.Code)
		assert.True(t, bytes.Equal(content, w.Body.Bytes()))
		assert.Equal(t, int32(1), f.drv.gets.Load(), "tape semantics stay on the single stream")
	})
}

func TestParallelSealed_ATamperedLaterSegmentCutsTheBodyAndChargesNoBreaker(t *testing.T) {
	f := setupSealedParallelFixture(t, true)
	content := randomBytes(9 << 20)
	f.putSealed(t, "sealed.bin", content, nil)
	f.drv.mu.Lock()
	for k, b := range f.drv.objects {
		b[len(b)-40] ^= 0xFF // inside the last segment
		f.drv.objects[k] = b
	}
	f.drv.mu.Unlock()

	w := f.get(t, "sealed.bin", nil)

	assert.True(t, f.cut, "the connection is cut — never a clean short body")
	assert.Less(t, w.Body.Len(), len(content))
	assert.Equal(t, "closed", f.eng.GetFailoverStatus()["ranged"], "bytes that do not open are not the backend's health")
}

func TestParallelSealed_AnOverwriteMidReadCutsTheBody(t *testing.T) {
	f := setupSealedParallelFixture(t, true)
	f.putSealed(t, "sealed.bin", randomBytes(9<<20), nil)
	f.drv.etag = func(off int64) string {
		if off >= 6<<20 {
			return "v2"
		}
		return "v1"
	}

	f.get(t, "sealed.bin", nil)

	assert.True(t, f.cut, "never spliced from two objects")
}

func TestParallelSealed_BudgetSpentFallsBackAndIsCounted(t *testing.T) {
	f := setupSealedParallelFixture(t, true)
	content := randomBytes(9 << 20)
	f.putSealed(t, "sealed.bin", content, nil)
	f.adapter.largeGet.budget = parfetch.NewBudget(1)
	before := testutil.ToFloat64(largeGetFallbacks.WithLabelValues("budget"))

	w := f.get(t, "sealed.bin", nil)

	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, bytes.Equal(content, w.Body.Bytes()))
	assert.Equal(t, int32(1), f.drv.gets.Load())
	assert.Equal(t, before+1, testutil.ToFloat64(largeGetFallbacks.WithLabelValues("budget")))
}

func TestParallelGet_ABucketPinnedToARegionReadsFromItsDriverInParallel(t *testing.T) {
	// The 2026-10-14 bench read a Frankfurt-pinned bucket at 4.1 MB/s and
	// concluded the parallel reader did not cover region drivers. The row
	// names idrive-<region>, its driver is a VersionedRangeGetter like the
	// primary (one *IDriveDriver type), and ParallelRangeSource reads the
	// row's backend: the reader does cover it — this pins that it stays so.
	for _, sealed := range []bool{false, true} {
		t.Run("sealed="+strconv.FormatBool(sealed), func(t *testing.T) {
			f := setupSealedParallelFixture(t, true)
			region := newRangedDriver()
			f.eng.AddDriver("idrive-eu-central-1", region)
			_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, region) VALUES ($1, 'test-bucket', 'private', 'eu-central-1')
				ON CONFLICT (tenant_id, name) DO UPDATE SET region = 'eu-central-1'`, f.tenant.ID)
			require.NoError(t, err)
			t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.tenant.ID) })
			content := randomBytes(9 << 20)
			path := "ranged"
			if sealed {
				f.putSealed(t, "eu.bin", content, nil)
				path = "sealed"
			} else {
				f.put(t, "eu.bin", content)
			}
			var backend string
			require.NoError(t, f.db.QueryRow(`SELECT backend_name FROM object_head_cache WHERE tenant_id = $1 AND object_key = 'eu.bin'`,
				f.tenant.ID).Scan(&backend))
			require.Equal(t, "idrive-eu-central-1", backend, "placed on the region driver")
			started := testutil.ToFloat64(largeGetParallelReads.WithLabelValues("idrive-eu-central-1", path))

			w := f.get(t, "eu.bin", nil)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.True(t, bytes.Equal(content, w.Body.Bytes()))
			assert.Greater(t, region.ranges.Load(), int32(2), "parallel ranges on the region driver")
			assert.Equal(t, int32(0), region.gets.Load())
			assert.Equal(t, int32(0), f.drv.ranges.Load()+f.drv.gets.Load(), "the primary is never asked")
			assert.Equal(t, started+1, testutil.ToFloat64(largeGetParallelReads.WithLabelValues("idrive-eu-central-1", path)))
		})
	}
}

var _ engine.VersionedRangeGetter = (*rangedDriver)(nil)
