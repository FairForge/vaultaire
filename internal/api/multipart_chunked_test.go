package api

// WP-R3-1 (Stage 1a Prompt 3): CompleteMultipartUpload is placed exactly as
// a single PUT of the same bytes and class would be — chunked when the PUT
// would chunk, whole and SSE-sealed when the PUT would seal, 413 / 503 when
// the PUT would refuse — through ONE placement decision (decidePlacement)
// and ONE chunked core (storeChunkedObject). "Before" numbers are these
// tests on 3e4b95e (Part 0) with the Server.chunkingThreshold knob only.

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 — S3 multipart ETag semantics
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// mpChunkFixture is the quota fixture on prod's key shape: the primary is a
// fixed-bucket driver (t-<tenant>/<container>/<artifact>), the content index
// is on, and anything over 1 KiB is above the chunking threshold.
type mpChunkFixture struct {
	*quotaAccountingFixture
	fixed *fixedBucketDriver
}

// mpMarker is the plaintext marker every test body carries: found in a
// backend file = plaintext reached the backend.
const mpMarker = "VAULTAIRE-PLAINTEXT-MARKER-7f3a"

func setupMPChunkFixture(t *testing.T, withKey bool) *mpChunkFixture {
	t.Helper()
	f := setupQuotaAccountingFixture(t, 1<<30)
	fixed := &fixedBucketDriver{dir: t.TempDir()}
	ce := f.server.engine
	ce.AddDriver("fixed", fixed)
	ce.SetPrimary("fixed")
	f.server.gci = crypto.NewGlobalContentIndex(f.db)
	f.server.chunkingThreshold = 1024
	if withKey {
		keys := testTenantKeys(t, f.db, keyTestMasterA)
		f.server.sseService = crypto.NewSSEService(keys)
		f.server.chunkEncSvc = crypto.NewChunkEncryptionService(keys)
	}
	_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, 'test-bucket', 'private') ON CONFLICT (tenant_id, name) DO NOTHING`, f.tenantID)
	require.NoError(t, err)
	tid := f.tenantID
	t.Cleanup(func() {
		cleanupTenantChunkRows(f.db, tid, tid)
		_, _ = f.db.Exec(`DELETE FROM object_versions WHERE tenant_id = $1`, tid)
		_, _ = f.db.Exec(`DELETE FROM retired_blobs WHERE tenant_id = $1`, tid)
		_, _ = f.db.Exec(`DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, tid)
		_, _ = f.db.Exec(`DELETE FROM tenant_key_versions WHERE tenant_id = $1`, tid)
		_, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, tid)
	})
	return &mpChunkFixture{quotaAccountingFixture: f, fixed: fixed}
}

func (f *mpChunkFixture) bucketSet(t *testing.T, set string) {
	t.Helper()
	_, err := f.db.Exec(`UPDATE buckets SET `+set+` WHERE tenant_id = $1 AND name = 'test-bucket'`, f.tenantID)
	require.NoError(t, err)
}

// mpBody is a body of n bytes with the plaintext marker every 512 bytes.
func mpBody(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i%251) ^ seed
	}
	for i := 0; i+len(mpMarker) <= n; i += 512 {
		copy(b[i:], mpMarker)
	}
	return b
}

// awsMultipartETag is what aws-cli computes: hex(MD5(MD5_1 ‖ … ‖ MD5_N))-N.
func awsMultipartETag(parts [][]byte) string {
	h := md5.New() // #nosec G401
	for _, p := range parts {
		s := md5.Sum(p) // #nosec G401
		h.Write(s[:])
	}
	return fmt.Sprintf("%s-%d", hex.EncodeToString(h.Sum(nil)), len(parts))
}

func splitParts(body []byte, n int) [][]byte {
	var parts [][]byte
	size := (len(body) + n - 1) / n
	for off := 0; off < len(body); off += size {
		end := off + size
		if end > len(body) {
			end = len(body)
		}
		parts = append(parts, body[off:end])
	}
	return parts
}

type mpUpload struct {
	id    string
	parts [][]byte
	body  string // the Complete request body
}

// create runs CreateMultipartUpload with the given headers.
func (f *mpChunkFixture) create(t *testing.T, key string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/test-bucket/"+key+"?uploads", nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)
	return w
}

// upload creates the upload and PUTs the parts.
func (f *mpChunkFixture) upload(t *testing.T, key string, parts [][]byte, hdr map[string]string) mpUpload {
	t.Helper()
	w := f.create(t, key, hdr)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var init InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &init))
	var b strings.Builder
	b.WriteString("<CompleteMultipartUpload>")
	for i, p := range parts {
		pw := doS3Request(f.server, f.tenant, "PUT", fmt.Sprintf("/test-bucket/%s?uploadId=%s&partNumber=%d", key, init.UploadID, i+1), bytes.NewReader(p))
		require.Equal(t, http.StatusOK, pw.Code, pw.Body.String())
		fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", i+1, pw.Header().Get("ETag"))
	}
	b.WriteString("</CompleteMultipartUpload>")
	return mpUpload{id: init.UploadID, parts: parts, body: b.String()}
}

func (f *mpChunkFixture) complete(t *testing.T, key string, u mpUpload) *httptest.ResponseRecorder {
	t.Helper()
	return doS3Request(f.server, f.tenant, "POST", "/test-bucket/"+key+"?uploadId="+u.id, strings.NewReader(u.body))
}

// multipart uploads body under key in n parts and completes it (200 required).
func (f *mpChunkFixture) multipart(t *testing.T, key string, body []byte, n int, hdr map[string]string) mpUpload {
	t.Helper()
	u := f.upload(t, key, splitParts(body, n), hdr)
	w := f.complete(t, key, u)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return u
}

type headRowView struct {
	chunked                   bool
	etag, enc, backend, floor string
	storageKey                string
	size                      int64
}

func (f *mpChunkFixture) row(t *testing.T, key string) headRowView {
	t.Helper()
	var v headRowView
	require.NoError(t, f.db.QueryRow(`SELECT is_chunked, etag, COALESCE(encryption_algorithm, ''), COALESCE(backend_name, ''), floor,
		COALESCE(storage_key, ''), size_bytes FROM object_head_cache WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`,
		f.tenantID, key).Scan(&v.chunked, &v.etag, &v.enc, &v.backend, &v.floor, &v.storageKey, &v.size))
	return v
}

// plaintextOnBackend counts backend files that contain the marker.
func plaintextOnBackend(t *testing.T, dirs ...string) int {
	t.Helper()
	n := 0
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			b, rErr := os.ReadFile(p) // #nosec G304 — test temp dir
			if rErr == nil && bytes.Contains(b, []byte(mpMarker)) {
				n++
			}
			return nil
		})
	}
	return n
}

func (f *mpChunkFixture) get(t *testing.T, key, rng string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", "/test-bucket/"+key, nil)
	if rng != "" {
		r.Header.Set("Range", rng)
	}
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)
	return w
}

func (f *mpChunkFixture) chunkRefs(t *testing.T, key string) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM tenant_chunk_refs WHERE tenant_id::text = $1 AND object_key = $2`,
		f.tenantID, key).Scan(&n))
	return n
}

// chunkingOffFor is a flag service with the `chunking` flag off for the
// fixture's tenant only (a per-tenant row: a global one would reach every
// package sharing the test database).
func chunkingOffFor(t *testing.T, f *mpChunkFixture) *flags.Service {
	t.Helper()
	svc := flags.New(f.db, zap.NewNop())
	svc.Register(flagChunking, true)
	require.NoError(t, svc.Set(context.Background(), flagChunking, f.tenantID, false, "test"))
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM feature_flags WHERE flag_key = $1 AND tenant_id = $2`, flagChunking, f.tenantID)
	})
	return svc
}

func TestComplete_ChunkableClass_StoresChunked_KeepsMultipartETag(t *testing.T) {
	// Before: one whole blob, is_chunked = FALSE.
	f := setupMPChunkFixture(t, false)
	body := mpBody(12<<10, 1)
	parts := splitParts(body, 3)

	u := f.multipart(t, "big.bin", body, 3, map[string]string{"Content-Type": "video/mp4"})

	row := f.row(t, "big.bin")
	assert.True(t, row.chunked, "a chunkable multipart object is chunked")
	assert.Equal(t, awsMultipartETag(u.parts), row.etag, "the -N ETag aws-cli computes")
	assert.Equal(t, awsMultipartETag(parts), row.etag)
	assert.Equal(t, int64(len(body)), row.size)
	assert.Equal(t, "", row.storageKey, "a chunked row names no blob")
	assert.Equal(t, chunkedBackendSentinel, row.backend)
	assert.Equal(t, usage.FloorStandard, row.floor)
	assert.Greater(t, f.chunkRefs(t, "big.bin"), 0)
	g := f.get(t, "big.bin", "")
	require.Equal(t, http.StatusOK, g.Code, g.Body.String())
	assert.Equal(t, body, g.Body.Bytes())
	assert.Equal(t, `"`+row.etag+`"`, g.Header().Get("ETag"))
	assert.Equal(t, "video/mp4", g.Header().Get("Content-Type"))
	rg := f.get(t, "big.bin", "bytes=5000-9000")
	require.Equal(t, http.StatusPartialContent, rg.Code)
	assert.Equal(t, body[5000:9001], rg.Body.Bytes())
	assert.Equal(t, int64(len(body)), f.used(t), "billed once, logical bytes")
	_, statErr := os.Stat(multipartDir(u.id))
	assert.True(t, os.IsNotExist(statErr), "staged parts removed")
}

func TestComplete_SSEBucket_WithKey_NoPlaintextOnBackend(t *testing.T) {
	// Before: the marker found on the backend for every case; encryption ''.
	cases := []struct {
		name, setup, enc string
		size             int
		chunked          bool
	}{
		{"chunked (> threshold, chunkable)", "", "AES256-CE", 12 << 10, true},
		{"small (≤ threshold): sealed whole", "", crypto.SSEAlgorithm, 900, false},
		{"versioned bucket (non-chunkable): sealed whole", ", versioning_status = 'Enabled'", crypto.SSEAlgorithm, 12 << 10, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setupMPChunkFixture(t, true)
			f.bucketSet(t, "sse_enabled = TRUE"+c.setup)
			body := mpBody(c.size, 2)

			f.multipart(t, "k.bin", body, 2, nil)

			row := f.row(t, "k.bin")
			assert.Equal(t, c.chunked, row.chunked)
			assert.Equal(t, c.enc, row.enc)
			assert.Zero(t, plaintextOnBackend(t, f.fixed.dir, f.tempDir), "no plaintext byte reached a backend")
			g := f.get(t, "k.bin", "")
			require.Equal(t, http.StatusOK, g.Code, g.Body.String())
			assert.Equal(t, body, g.Body.Bytes())
			assert.Equal(t, `"`+row.etag+`"`, g.Header().Get("ETag"))
			assert.True(t, strings.HasSuffix(row.etag, "-2"), "the multipart ETag survives the seal: %s", row.etag)
			assert.Equal(t, int64(len(body)), f.used(t), "billed on the plaintext size")
		})
	}
}

func TestComplete_SSEBucket_NoKey_Refused503BeforeKeepAlive(t *testing.T) {
	// Before: Create 200 and Complete 200, the object stored plaintext.
	f := setupMPChunkFixture(t, false)
	instantCommit(f.server)
	f.bucketSet(t, "sse_enabled = TRUE")

	w := f.create(t, "k.bin", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.NotEmpty(t, w.Header().Get("Retry-After"))

	// Encryption required after the upload began: Complete refuses too,
	// with its own status, never a committed 200.
	f.bucketSet(t, "sse_enabled = FALSE")
	u := f.upload(t, "k.bin", splitParts(mpBody(12<<10, 3), 2), nil)
	f.bucketSet(t, "sse_enabled = TRUE")
	w = f.complete(t, "k.bin", u)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "ServiceUnavailable")
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
	assert.Zero(t, plaintextOnBackend(t, f.fixed.dir, f.tempDir))
	assert.Equal(t, int64(0), f.used(t), "nothing reserved")
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM multipart_uploads WHERE upload_id = $1`, u.id).Scan(&status))
	assert.Equal(t, "active", status, "the client can retry once the key is back")
}

// WP-R8-5 (= WP-R3-3): a non-chunkable Complete into an sse_enabled bucket is
// sealed as the parts stream, at any size. Before (WP-R3-1, the
// TestComplete_SSEBucket_NonChunkableOver256MiB_Refused413 this replaces):
// 413 over maxSSEObjectSize, and at or under it the assembled plaintext and
// its ciphertext held in memory. "Over the old cap" is many segments here:
// the service seals 1 KiB segments, so 12 KiB is 12 of them.
func TestComplete_SSEBucket_NonChunkableOver256MiB_SealedStreaming(t *testing.T) {
	for name, c := range map[string]struct {
		bucket string
		hdr    map[string]string
		floor  string
	}{
		"versioned bucket": {"sse_enabled = TRUE, versioning_status = 'Enabled'", nil, usage.FloorStandard},
		"GLACIER (Vault)":  {"sse_enabled = TRUE", map[string]string{"x-amz-storage-class": "GLACIER"}, usage.FloorVault},
		"DEEP_ARCHIVE":     {"sse_enabled = TRUE", map[string]string{"x-amz-storage-class": "DEEP_ARCHIVE"}, usage.FloorVault},
	} {
		t.Run(name, func(t *testing.T) {
			f := setupMPChunkFixture(t, true)
			f.server.sseService.SetSegmentSize(1 << 10)
			f.bucketSet(t, c.bucket)
			body := mpBody(12<<10, 4)

			u := f.upload(t, "k.bin", splitParts(body, 2), c.hdr)
			w := f.complete(t, "k.bin", u)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, "AES256", w.Header().Get("x-amz-server-side-encryption"), "the response says the object is encrypted")
			row := f.row(t, "k.bin")
			assert.False(t, row.chunked)
			assert.Equal(t, crypto.SSEAlgorithm, row.enc)
			assert.Equal(t, c.floor, row.floor)
			assert.Equal(t, awsMultipartETag(splitParts(body, 2)), row.etag, "the -N ETag of the parts")
			assert.Zero(t, plaintextOnBackend(t, f.fixed.dir, f.tempDir))
			assert.Equal(t, int64(len(body)), f.used(t), "billed the plaintext size")
			assert.Equal(t, body, f.get(t, "k.bin", "").Body.Bytes())
		})
	}
}

// TestPlacement_PutAndCompleteAgree drives a PUT and a Complete of the same
// bytes over the same matrix and asserts the same outcome: chunked or not,
// sealed or not, refused or not.
func TestPlacement_PutAndCompleteAgree(t *testing.T) {
	type outcome struct {
		code    int
		chunked bool
		enc     string
	}
	cases := []struct {
		name   string
		key    bool
		bucket string // the buckets row's SET clause
		hdr    map[string]string
		size   int
	}{
		{"plain bucket, large", false, "sse_enabled = FALSE", nil, 12 << 10},
		{"plain bucket, small", false, "sse_enabled = FALSE", nil, 900},
		{"plain, versioned", false, "versioning_status = 'Enabled'", nil, 12 << 10},
		{"plain, chunking flag off", false, "sse_enabled = FALSE", map[string]string{"x-test-flag-off": "1"}, 12 << 10},
		{"plain, RESILIENT class", false, "sse_enabled = FALSE", map[string]string{"x-amz-storage-class": "RESILIENT"}, 12 << 10},
		{"sse, no key", false, "sse_enabled = TRUE", nil, 12 << 10},
		{"sse, key, large", true, "sse_enabled = TRUE", nil, 12 << 10},
		{"sse, key, small", true, "sse_enabled = TRUE", nil, 900},
		// WP-R8-5: every non-chunkable class and a versioned bucket, sealed
		// at any size ("> cap" = many segments; this was a 413 both ways).
		{"sse, key, versioned, small", true, "sse_enabled = TRUE, versioning_status = 'Enabled'", nil, 3 << 10},
		{"sse, key, versioned > old cap", true, "sse_enabled = TRUE, versioning_status = 'Enabled'", nil, 12 << 10},
		{"sse, key, GLACIER > old cap", true, "sse_enabled = TRUE", map[string]string{"x-amz-storage-class": "GLACIER"}, 12 << 10},
		{"sse, key, DEEP_ARCHIVE > old cap", true, "sse_enabled = TRUE", map[string]string{"x-amz-storage-class": "DEEP_ARCHIVE"}, 12 << 10},
		{"no sse bucket, key, large", true, "sse_enabled = FALSE", nil, 12 << 10},
		// The header on Create is kept (089) and placed as the PUT's.
		{"AES256 header, key, versioned > old cap", true, "versioning_status = 'Enabled'", map[string]string{"x-amz-server-side-encryption": "AES256"}, 12 << 10},
		{"AES256 header, key, small", true, "sse_enabled = FALSE", map[string]string{"x-amz-server-side-encryption": "AES256"}, 900},
		{"AES256 header, no key", false, "sse_enabled = FALSE", map[string]string{"x-amz-server-side-encryption": "AES256"}, 900},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := func(viaComplete bool) outcome {
				f := setupMPChunkFixture(t, c.key)
				instantCommit(f.server)
				f.bucketSet(t, c.bucket)
				if f.server.sseService != nil {
					f.server.sseService.SetSegmentSize(1 << 10)
				}
				hdr := map[string]string{}
				for k, v := range c.hdr {
					hdr[k] = v
				}
				if hdr["x-test-flag-off"] != "" {
					delete(hdr, "x-test-flag-off")
					f.server.flags = chunkingOffFor(t, f)
				}
				body := mpBody(c.size, 5)
				var code int
				if viaComplete {
					w := f.create(t, "k.bin", hdr)
					if w.Code != http.StatusOK {
						return outcome{code: w.Code}
					}
					var init InitiateMultipartUploadResult
					require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &init))
					var b strings.Builder
					b.WriteString("<CompleteMultipartUpload>")
					for i, p := range splitParts(body, 2) {
						pw := doS3Request(f.server, f.tenant, "PUT", fmt.Sprintf("/test-bucket/k.bin?uploadId=%s&partNumber=%d", init.UploadID, i+1), bytes.NewReader(p))
						require.Equal(t, http.StatusOK, pw.Code)
						fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", i+1, pw.Header().Get("ETag"))
					}
					b.WriteString("</CompleteMultipartUpload>")
					code = doS3Request(f.server, f.tenant, "POST", "/test-bucket/k.bin?uploadId="+init.UploadID, strings.NewReader(b.String())).Code
				} else {
					r := httptest.NewRequest("PUT", "/test-bucket/k.bin", bytes.NewReader(body))
					r.ContentLength = int64(len(body))
					for k, v := range hdr {
						r.Header.Set(k, v)
					}
					r = r.WithContext(f.ctx(r.Context()))
					w := httptest.NewRecorder()
					f.server.handlePutObject(w, r, f.s3Req("test-bucket", "k.bin"))
					code = w.Code
				}
				o := outcome{code: code}
				if code == http.StatusOK {
					row := f.row(t, "k.bin")
					o.chunked, o.enc = row.chunked, row.enc
					if o.enc != "" {
						assert.Zero(t, plaintextOnBackend(t, f.fixed.dir, f.tempDir), "sealed or chunk-encrypted: no plaintext")
					}
				}
				return o
			}
			put, complete := run(false), run(true)
			assert.Equal(t, put, complete, "a Complete is placed as a PUT of the same bytes and class")
		})
	}
}

func TestComplete_StagedPartCorrupted_RefusedNothingCommitted(t *testing.T) {
	// Before: 200 and the corrupted bytes were stored under the ETag of the
	// bytes the client sent.
	for _, chunked := range []bool{true, false} {
		t.Run(fmt.Sprintf("chunked=%v", chunked), func(t *testing.T) {
			f := setupMPChunkFixture(t, false)
			require.Equal(t, http.StatusOK, f.put(t, "k.bin", []byte("the previous object")))
			if !chunked {
				f.bucketSet(t, "versioning_status = 'Enabled'")
			}
			u := f.upload(t, "k.bin", splitParts(mpBody(12<<10, 6), 3), nil)
			// One staged part flips a byte at rest (same size: the stat check passes).
			p := partFilePath(u.id, 2)
			b, err := os.ReadFile(p) // #nosec G304 — test
			require.NoError(t, err)
			b[100] ^= 0xFF
			require.NoError(t, os.WriteFile(p, b, 0o600))
			usedBefore := f.used(t)

			w := f.complete(t, "k.bin", u)

			assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), "InternalError")
			g := f.get(t, "k.bin", "")
			require.Equal(t, http.StatusOK, g.Code)
			assert.Equal(t, "the previous object", g.Body.String(), "nothing committed")
			assert.Equal(t, usedBefore, f.used(t), "the reservation released")
			assert.Equal(t, 0, f.chunkRefs(t, "k.bin"), "no manifest")
			var status string
			require.NoError(t, f.db.QueryRow(`SELECT status FROM multipart_uploads WHERE upload_id = $1`, u.id).Scan(&status))
			assert.Equal(t, "active", status)
		})
	}
}

func TestComplete_Chunked_ReplayReturnsSameResult(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	body := mpBody(12<<10, 7)
	u := f.upload(t, "k.bin", splitParts(body, 3), nil)
	first := f.complete(t, "k.bin", u)
	require.Equal(t, http.StatusOK, first.Code)
	used := f.used(t)

	again := f.complete(t, "k.bin", u)

	require.Equal(t, http.StatusOK, again.Code, again.Body.String())
	assert.Equal(t, first.Header().Get("ETag"), again.Header().Get("ETag"))
	assert.True(t, f.row(t, "k.bin").chunked)
	assert.Equal(t, used, f.used(t), "a replay charges nothing")
	g := f.get(t, "k.bin", "")
	assert.Equal(t, body, g.Body.Bytes())
}

func TestComplete_Chunked_OverWholeObject_RetiresBlob(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	require.Equal(t, http.StatusOK, f.put(t, "k.bin", []byte("a whole object")))
	old := f.row(t, "k.bin")
	require.False(t, old.chunked)
	require.NotEmpty(t, old.storageKey)

	body := mpBody(12<<10, 8)
	f.multipart(t, "k.bin", body, 2, nil)

	assert.True(t, f.row(t, "k.bin").chunked)
	var reason string
	require.NoError(t, f.db.QueryRow(`SELECT reason FROM retired_blobs WHERE tenant_id = $1 AND storage_key = $2`,
		f.tenantID, old.storageKey).Scan(&reason))
	assert.Equal(t, retiredOverwritten, reason)
	assert.Equal(t, int64(len(body)), f.used(t), "the whole object released, the chunked one billed")
	assert.Equal(t, body, f.get(t, "k.bin", "").Body.Bytes())
}

func TestComplete_Chunked_WholeOverChunked_ReleasesManifest(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	first := mpBody(12<<10, 9)
	f.multipart(t, "k.bin", first, 2, nil)
	require.True(t, f.row(t, "k.bin").chunked)
	require.Greater(t, f.chunkRefs(t, "k.bin"), 0)

	// The same key, now whole: under the threshold.
	second := mpBody(900, 10)
	f.multipart(t, "k.bin", second, 2, nil)

	row := f.row(t, "k.bin")
	assert.False(t, row.chunked)
	assert.Equal(t, 0, f.chunkRefs(t, "k.bin"), "the displaced manifest released")
	assert.Equal(t, int64(len(second)), f.used(t))
	assert.Equal(t, second, f.get(t, "k.bin", "").Body.Bytes())
}

func TestComplete_Chunked_LongOpCut_CompensatesRefs(t *testing.T) {
	// A shutdown's cut while the chunks are being stored: nothing commits,
	// every reference this Complete took is given back, the previous object
	// stays, the reservation is released, the upload stays active.
	f := setupMPChunkFixture(t, false)
	require.Equal(t, http.StatusOK, f.put(t, "k.bin", []byte("previous")))
	body := mpBody(12<<10, 11)
	u := f.upload(t, "k.bin", splitParts(body, 3), nil)
	ctx, cancel := context.WithCancel(context.Background())
	stagedPartReadHook = func(part int) {
		if part == 2 {
			cancel()
		}
	}
	t.Cleanup(func() { stagedPartReadHook = func(int) {} })
	r := httptest.NewRequest("POST", "/test-bucket/k.bin?uploadId="+u.id, strings.NewReader(u.body))
	r = r.WithContext(s3Ctx(ctx, f.tenant))
	w := httptest.NewRecorder()
	f.server.completeMultipartUpload(w, r, "test-bucket", "k.bin")

	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.Equal(t, "previous", f.get(t, "k.bin", "").Body.String())
	assert.Equal(t, int64(len("previous")), f.used(t))
	// 12 KiB is under the chunker's minimum: the whole body is one chunk,
	// its hash the body's SHA-256.
	sum := sha256.Sum256(body)
	var refs int
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(SUM(ref_count), 0) FROM global_content_index
		WHERE dedup_scope = $1 AND plaintext_hash = $2`, crypto.GlobalDedupScope, hex.EncodeToString(sum[:])).Scan(&refs))
	assert.Zero(t, refs, "no reference survives without a manifest")
	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM multipart_uploads WHERE upload_id = $1`, u.id).Scan(&status))
	assert.Equal(t, "active", status)
}

func TestComplete_VersionedBucket_StaysWhole(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	f.bucketSet(t, "versioning_status = 'Enabled'")
	body := mpBody(12<<10, 12)
	f.multipart(t, "k.bin", body, 2, nil)
	row := f.row(t, "k.bin")
	assert.False(t, row.chunked, "manifests carry no version id: versioned buckets stay whole")
	assert.NotEmpty(t, row.storageKey)
	assert.Equal(t, body, f.get(t, "k.bin", "").Body.Bytes())
}

func TestComplete_GlacierStaysWholeOnTheAttic(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	f.house(t, 10<<20, 10<<20)
	body := mpBody(12<<10, 13)
	f.multipart(t, "k.bin", body, 2, map[string]string{"x-amz-storage-class": "GLACIER"})
	row := f.row(t, "k.bin")
	assert.False(t, row.chunked, "the attic is never chunked")
	assert.Equal(t, usage.FloorVault, row.floor)
	std, vault := f.floors(t)
	assert.Equal(t, int64(0), std)
	assert.Equal(t, int64(len(body)), vault)
}

func TestComplete_Chunked_Dedup(t *testing.T) {
	f := setupMPChunkFixture(t, false)
	body := mpBody(12<<10, 14)
	f.multipart(t, "a.bin", body, 3, nil)
	var blobsAfterFirst int
	require.NoError(t, filepath.WalkDir(f.fixed.dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			blobsAfterFirst++
		}
		return nil
	}))

	f.multipart(t, "b.bin", body, 2, nil) // another part split: the same bytes

	var blobs int
	require.NoError(t, filepath.WalkDir(f.fixed.dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			blobs++
		}
		return nil
	}))
	assert.Equal(t, blobsAfterFirst, blobs, "the second Complete stored no new chunk")
	assert.Equal(t, f.row(t, "a.bin").size, f.row(t, "b.bin").size)
	assert.NotEqual(t, f.row(t, "a.bin").etag, f.row(t, "b.bin").etag, "each keeps its own -N ETag")
	assert.Equal(t, body, f.get(t, "b.bin", "").Body.Bytes())
	assert.Equal(t, int64(2*len(body)), f.used(t), "billed per object (logical bytes)")
}

func TestCDN_ChunkedPublicObject_Served(t *testing.T) {
	// Before: 404 for a row that exists (the CDN asked the engine for a whole
	// blob at <key>).
	f := setupMPChunkFixture(t, false)
	body := mpBody(12<<10, 15)
	f.multipart(t, "pub.bin", body, 2, map[string]string{"Content-Type": "application/pdf"})
	require.True(t, f.row(t, "pub.bin").chunked)
	slug := "mpchunk-" + f.tenantID[:8]
	_, err := f.db.Exec(`UPDATE tenants SET slug = $2, slug_locked = TRUE WHERE id = $1`, f.tenantID, slug)
	require.NoError(t, err)
	f.bucketSet(t, "visibility = 'public-read'")
	f.server.cdnRateLimiter = NewRateLimiter()
	f.server.router.Get("/cdn/{slug}/{bucket}/*", f.server.handleCDNRequest)

	r := httptest.NewRequest("GET", "/cdn/"+slug+"/test-bucket/pub.bin", nil)
	w := httptest.NewRecorder()
	f.server.router.ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, body, w.Body.Bytes())
	assert.Contains(t, w.Header().Get("Cache-Control"), "public")
	assert.Empty(t, w.Header().Get("x-amz-version-id"), "no S3 headers on the CDN")

	r = httptest.NewRequest("GET", "/cdn/"+slug+"/test-bucket/pub.bin", nil)
	r.Header.Set("Range", "bytes=100-199")
	w = httptest.NewRecorder()
	f.server.router.ServeHTTP(w, r)
	require.Equal(t, http.StatusPartialContent, w.Code)
	assert.Equal(t, body[100:200], w.Body.Bytes())

	// A request cannot override the CDN's own disposition.
	own := w.Header().Get("Content-Disposition")
	r = httptest.NewRequest("GET", "/cdn/"+slug+"/test-bucket/pub.bin?response-content-disposition=attachment%3B%20filename%3Devil.html", nil)
	w = httptest.NewRecorder()
	f.server.router.ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, own, w.Header().Get("Content-Disposition"))
	assert.NotContains(t, w.Header().Get("Content-Disposition"), "evil")
}

// PM-5 (the post-merge review of WP-R2-1): a chunked source is copied as a
// manifest only where a PUT of its bytes would be chunked. Before: a manifest
// copy into a versioned bucket (no version row) and into an archive-tier
// bucket (a chunked row on the primary, billed downstairs).
func TestCopy_ChunkedSource_IntoNonChunkableDestination_StoredWhole(t *testing.T) {
	cases := []struct {
		name, setup string
		floor       string
		version     bool
	}{
		{"versioned destination", "versioning_status = 'Enabled'", usage.FloorStandard, true},
		{"archive-tier destination", "tier_preference = 'archive'", usage.FloorVault, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setupMPChunkFixture(t, false)
			f.house(t, 10<<20, 10<<20)
			body := mpBody(12<<10, 16)
			f.multipart(t, "src.bin", body, 2, nil)
			require.True(t, f.row(t, "src.bin").chunked)
			_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, 'dest-bucket', 'private')
				ON CONFLICT (tenant_id, name) DO NOTHING`, f.tenantID)
			require.NoError(t, err)
			_, err = f.db.Exec(`UPDATE buckets SET `+c.setup+` WHERE tenant_id = $1 AND name = 'dest-bucket'`, f.tenantID)
			require.NoError(t, err)

			r := httptest.NewRequest("PUT", "/dest-bucket/copy.bin", nil)
			r.Header.Set("x-amz-copy-source", "/test-bucket/src.bin")
			r = r.WithContext(f.ctx(r.Context()))
			w := httptest.NewRecorder()
			f.server.handleS3Request(w, r)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())

			var chunked bool
			var etag, floor, sk string
			require.NoError(t, f.db.QueryRow(`SELECT is_chunked, etag, floor, COALESCE(storage_key, '') FROM object_head_cache
				WHERE tenant_id = $1 AND bucket = 'dest-bucket' AND object_key = 'copy.bin'`, f.tenantID).Scan(&chunked, &etag, &floor, &sk))
			assert.False(t, chunked, "re-stored whole")
			assert.NotEmpty(t, sk)
			sum := md5.Sum(body) // #nosec G401
			assert.Equal(t, hex.EncodeToString(sum[:]), etag, "a one-request copy reports a fresh single-part ETag")
			assert.Equal(t, c.floor, floor)
			var versions int
			require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_versions WHERE tenant_id = $1 AND bucket = 'dest-bucket'`,
				f.tenantID).Scan(&versions))
			assert.Equal(t, c.version, versions == 1)
			g := doS3Request(f.server, f.tenant, "GET", "/dest-bucket/copy.bin", nil)
			require.Equal(t, http.StatusOK, g.Code)
			assert.Equal(t, body, g.Body.Bytes())
		})
	}
}
