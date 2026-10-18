package api

// WP-R3-2 (R3-02): UploadPartCopy — an UploadPart whose bytes are a range of
// an existing object (x-amz-copy-source + x-amz-copy-source-range). aws-cli
// and rclone copy every object above their multipart threshold (8 MB for
// aws-cli) server-side through it; it answered 501 since #514. The source is
// resolved and opened as CopyObject's (one resolver, one opener), the part
// staged as UploadPart's (one stager).

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 — S3 ETag semantics
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/crypto"
	"go.uber.org/zap"
)

// upcDriver is the fixture's primary: the fixed-bucket test driver whose
// reads can be made to fail after n bytes (a backend GET body that breaks).
type upcDriver struct {
	*fixedBucketDriver
	failAfter atomic.Int64 // > 0: a read fails after this many bytes
}

type upcBrokenBody struct {
	r    io.ReadCloser
	left int64
}

func (b *upcBrokenBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errors.New("backend body broke off")
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *upcBrokenBody) Close() error { return b.r.Close() }

func (d *upcDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	rc, err := d.fixedBucketDriver.Get(ctx, container, artifact)
	if err != nil || d.failAfter.Load() <= 0 {
		return rc, err
	}
	return &upcBrokenBody{r: rc, left: d.failAfter.Load()}, nil
}

type upcFixture struct {
	*mpChunkFixture
	drv *upcDriver
}

func setupUPCFixture(t *testing.T, withKey bool) *upcFixture {
	t.Helper()
	f := setupMPChunkFixture(t, withKey)
	drv := &upcDriver{fixedBucketDriver: f.fixed}
	require.NoError(t, f.server.engine.AddDriver("fixed", drv))
	return &upcFixture{mpChunkFixture: f, drv: drv}
}

func (f *upcFixture) do(t *testing.T, method, target string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	if body != nil {
		r.ContentLength = int64(len(body))
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)
	return w
}

func (f *upcFixture) putObj(t *testing.T, key string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	w := f.do(t, "PUT", "/test-bucket/"+key, body, hdr)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return w
}

func (f *upcFixture) getObj(t *testing.T, key string) []byte {
	t.Helper()
	w := f.do(t, "GET", "/test-bucket/"+key, nil, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return w.Body.Bytes()
}

// copyPart is UploadPartCopy of part n of upload id from source (a header
// value, "test-bucket/src" style) with an optional range and extra headers.
func (f *upcFixture) copyPart(t *testing.T, key, id string, n int, source, rng string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	h := map[string]string{"x-amz-copy-source": source}
	if rng != "" {
		h["x-amz-copy-source-range"] = rng
	}
	for k, v := range hdr {
		h[k] = v
	}
	return f.do(t, "PUT", fmt.Sprintf("/test-bucket/%s?partNumber=%d&uploadId=%s", key, n, id), nil, h)
}

func (f *upcFixture) partRow(t *testing.T, id string, n int) (etag string, size int64, ok bool) {
	t.Helper()
	err := f.db.QueryRow(`SELECT etag, size_bytes FROM multipart_parts WHERE upload_id = $1 AND part_number = $2`, id, n).Scan(&etag, &size)
	if err != nil {
		return "", 0, false
	}
	return etag, size, true
}

func (f *upcFixture) partRows(t *testing.T, id string) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM multipart_parts WHERE upload_id = $1`, id).Scan(&n))
	return n
}

// stagedTemps lists the part temp files left in the upload's directory.
func stagedTemps(t *testing.T, id string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(multipartDir(id), ".part-*"))
	require.NoError(t, err)
	return m
}

func md5Hex(b []byte) string {
	h := md5.Sum(b) // #nosec G401 — S3 ETag semantics
	return hex.EncodeToString(h[:])
}

func (f *upcFixture) completeParts(t *testing.T, key, id string, etags ...string) *httptest.ResponseRecorder {
	t.Helper()
	var b strings.Builder
	b.WriteString("<CompleteMultipartUpload>")
	for i, e := range etags {
		fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", i+1, e)
	}
	b.WriteString("</CompleteMultipartUpload>")
	return f.do(t, "POST", fmt.Sprintf("/test-bucket/%s?uploadId=%s", key, id), []byte(b.String()), nil)
}

type upcResult struct {
	XMLName      xml.Name `xml:"CopyPartResult"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

func parseCopyPartResult(t *testing.T, w *httptest.ResponseRecorder) upcResult {
	t.Helper()
	var res upcResult
	require.NoError(t, xml.Unmarshal(bytes.TrimSpace(w.Body.Bytes()), &res), w.Body.String())
	return res
}

func TestUploadPartCopy_WholePlainSource(t *testing.T) {
	// Arrange: a plain (whole, unchunked) source.
	f := setupUPCFixture(t, false)
	src := mpBody(900, 1)
	f.putObj(t, "src.bin", src, nil)
	id := f.createUpload(t, "dst.bin", nil)

	// Act
	w := f.copyPart(t, "dst.bin", id, 1, "/test-bucket/src.bin", "", nil)

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	res := parseCopyPartResult(t, w)
	assert.Equal(t, `"`+md5Hex(src)+`"`, res.ETag)
	assert.NotEmpty(t, res.LastModified)
	staged, err := os.ReadFile(partFilePath(id, 1))
	require.NoError(t, err)
	assert.Equal(t, src, staged, "the part file holds the source's bytes")
	etag, size, ok := f.partRow(t, id, 1)
	require.True(t, ok)
	assert.Equal(t, `"`+md5Hex(src)+`"`, etag, "the row has the part's own MD5")
	assert.Equal(t, int64(900), size)
	assert.Empty(t, stagedTemps(t, id))
	assert.Equal(t, int64(900+900), f.used(t), "the source and the part in flight are both billed")
	f.ledgerAgrees(t, "after the copied part")
}

func TestUploadPartCopy_RangeIsExactlyThoseBytes(t *testing.T) {
	f := setupUPCFixture(t, false)
	src := mpBody(900, 2)
	f.putObj(t, "src.bin", src, nil)
	id := f.createUpload(t, "dst.bin", nil)

	w := f.copyPart(t, "dst.bin", id, 3, "test-bucket/src.bin", "bytes=100-399", nil)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	staged, err := os.ReadFile(partFilePath(id, 3))
	require.NoError(t, err)
	assert.Equal(t, src[100:400], staged)
	_, size, ok := f.partRow(t, id, 3)
	require.True(t, ok)
	assert.Equal(t, int64(300), size)
	assert.Equal(t, `"`+md5Hex(src[100:400])+`"`, parseCopyPartResult(t, w).ETag)
}

func TestUploadPartCopy_InvalidRanges_400NothingStaged(t *testing.T) {
	f := setupUPCFixture(t, false)
	f.putObj(t, "src.bin", mpBody(900, 3), nil)
	id := f.createUpload(t, "dst.bin", nil)

	for _, rng := range []string{
		"bytes=0-900",          // past the source's size
		"bytes=900-900",        // starts past it
		"bytes=10-5",           // reversed
		"bytes=5-",             // both ends are required
		"bytes=-5",             // a suffix range is not a copy range
		"items=0-5",            // not bytes
		"bytes=0-5,7-9",        // one range only
		"bytes=a-b",            // not numbers
		"bytes=0-5368709120",   // past the size and longer than 5 GiB
		"bytes=0-99999999999x", // trailing junk
	} {
		w := f.copyPart(t, "dst.bin", id, 1, "/test-bucket/src.bin", rng, nil)
		assert.Equal(t, http.StatusBadRequest, w.Code, rng)
		assert.Contains(t, w.Body.String(), "<Code>InvalidArgument</Code>", rng)
	}
	assert.Equal(t, 0, f.partRows(t, id))
	assert.Empty(t, stagedTemps(t, id))
	assert.Equal(t, int64(900), f.used(t), "nothing reserved")
}

func TestUploadPartCopy_SourceOver5GiB(t *testing.T) {
	// Arrange: a row recording a 6 GiB source (no bytes needed: refused first).
	f := setupUPCFixture(t, false)
	_, err := f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor, content_type)
		VALUES ($1, 'test-bucket', 'huge.bin', $2, 'e', 'fixed', 'standard', 'application/octet-stream')`, f.tenantID, int64(6)<<30)
	require.NoError(t, err)
	id := f.createUpload(t, "dst.bin", nil)

	// Act + Assert: no range = the whole 6 GiB, more than a part may be.
	w := f.copyPart(t, "dst.bin", id, 1, "/test-bucket/huge.bin", "", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "<Code>InvalidRequest</Code>")
	// A range within the size but longer than 5 GiB.
	w = f.copyPart(t, "dst.bin", id, 1, "/test-bucket/huge.bin", fmt.Sprintf("bytes=0-%d", int64(5)<<30), nil)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "<Code>InvalidArgument</Code>")
	assert.Equal(t, 0, f.partRows(t, id))
}

func TestUploadPartCopy_SealedSourceGivesPlaintext(t *testing.T) {
	// Arrange: an SSE-S3 whole source (under the chunking threshold).
	f := setupUPCFixture(t, true)
	src := mpBody(800, 4)
	f.putObj(t, "sealed.bin", src, map[string]string{"x-amz-server-side-encryption": "AES256"})
	var enc string
	require.NoError(t, f.db.QueryRow(`SELECT COALESCE(encryption_algorithm,'') FROM object_head_cache WHERE tenant_id=$1 AND object_key='sealed.bin'`, f.tenantID).Scan(&enc))
	require.NotEmpty(t, enc, "fixture: the source is sealed")
	id := f.createUpload(t, "dst.bin", nil)

	// Act: whole, then a range.
	w1 := f.copyPart(t, "dst.bin", id, 1, "/test-bucket/sealed.bin", "", nil)
	w2 := f.copyPart(t, "dst.bin", id, 2, "/test-bucket/sealed.bin", "bytes=10-509", nil)

	// Assert
	require.Equal(t, http.StatusOK, w1.Code, w1.Body.String())
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	p1, err := os.ReadFile(partFilePath(id, 1))
	require.NoError(t, err)
	p2, err := os.ReadFile(partFilePath(id, 2))
	require.NoError(t, err)
	assert.Equal(t, src, p1, "the part is the plaintext")
	assert.Equal(t, src[10:510], p2)
}

func TestUploadPartCopy_ChunkedSourceWholeAndRange(t *testing.T) {
	// Arrange: a chunked source (over the fixture's 1 KiB threshold).
	f := setupUPCFixture(t, false)
	src := mpBody(300*1024, 5)
	f.putObj(t, "chunked.bin", src, nil)
	var chunked bool
	require.NoError(t, f.db.QueryRow(`SELECT is_chunked FROM object_head_cache WHERE tenant_id=$1 AND object_key='chunked.bin'`, f.tenantID).Scan(&chunked))
	require.True(t, chunked, "fixture: the source is chunked")
	id := f.createUpload(t, "dst.bin", nil)

	// Act
	w1 := f.copyPart(t, "dst.bin", id, 1, "/test-bucket/chunked.bin", "", nil)
	w2 := f.copyPart(t, "dst.bin", id, 2, "/test-bucket/chunked.bin", "bytes=70000-250000", nil)

	// Assert
	require.Equal(t, http.StatusOK, w1.Code, w1.Body.String())
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	p1, err := os.ReadFile(partFilePath(id, 1))
	require.NoError(t, err)
	p2, err := os.ReadFile(partFilePath(id, 2))
	require.NoError(t, err)
	assert.Equal(t, src, p1)
	assert.Equal(t, src[70000:250001], p2)
}

func TestUploadPartCopy_SourceVersion(t *testing.T) {
	// Arrange: two versions of the source.
	f := setupUPCFixture(t, false)
	f.bucketSet(t, "versioning_status = 'Enabled'")
	v1 := mpBody(700, 6)
	w := f.putObj(t, "src.bin", v1, nil)
	vid := w.Header().Get("x-amz-version-id")
	require.NotEmpty(t, vid)
	f.putObj(t, "src.bin", mpBody(700, 7), nil)
	id := f.createUpload(t, "dst.bin", nil)

	// Act
	w = f.copyPart(t, "dst.bin", id, 1, "/test-bucket/src.bin?versionId="+vid, "bytes=0-99", nil)

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, vid, w.Header().Get("x-amz-copy-source-version-id"))
	staged, err := os.ReadFile(partFilePath(id, 1))
	require.NoError(t, err)
	assert.Equal(t, v1[:100], staged, "that version's bytes")

	// A version that does not exist.
	w = f.copyPart(t, "dst.bin", id, 2, "/test-bucket/src.bin?versionId=nope", "", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "NoSuchVersion")
}

func TestUploadPartCopy_SourceConditions(t *testing.T) {
	f := setupUPCFixture(t, false)
	src := mpBody(500, 8)
	f.putObj(t, "src.bin", src, nil)
	id := f.createUpload(t, "dst.bin", nil)

	w := f.copyPart(t, "dst.bin", id, 1, "/test-bucket/src.bin", "", map[string]string{"x-amz-copy-source-if-match": `"0123"`})
	assert.Equal(t, http.StatusPreconditionFailed, w.Code, w.Body.String())
	assert.Equal(t, 0, f.partRows(t, id))

	w = f.copyPart(t, "dst.bin", id, 1, "/test-bucket/src.bin", "", map[string]string{"x-amz-copy-source-if-match": `"` + md5Hex(src) + `"`})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestUploadPartCopy_SSECSource_501(t *testing.T) {
	f := setupUPCFixture(t, false)
	_, err := f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor, content_type, encryption_algorithm)
		VALUES ($1, 'test-bucket', 'ssec.bin', 100, 'e', 'fixed', 'standard', 'application/octet-stream', $2)`, f.tenantID, crypto.SSECAlgorithm)
	require.NoError(t, err)
	id := f.createUpload(t, "dst.bin", nil)

	w := f.copyPart(t, "dst.bin", id, 1, "/test-bucket/ssec.bin", "", nil)

	assert.Equal(t, http.StatusNotImplemented, w.Code, w.Body.String())
	assert.Equal(t, 0, f.partRows(t, id))
}

func TestUploadPartCopy_QuotaAndStagingGuards(t *testing.T) {
	t.Run("past the quota: 403, nothing staged, no row", func(t *testing.T) {
		f := setupUPCFixture(t, false)
		f.putObj(t, "src.bin", mpBody(900, 9), nil)
		_, err := f.db.Exec(`UPDATE tenant_quotas SET storage_limit_bytes = 1000 WHERE tenant_id = $1`, f.tenantID)
		require.NoError(t, err)
		id := f.createUpload(t, "dst.bin", nil)

		w := f.copyPart(t, "dst.bin", id, 1, "/test-bucket/src.bin", "", nil)

		assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "QuotaExceeded")
		assert.Equal(t, 0, f.partRows(t, id))
		assert.Empty(t, stagedTemps(t, id))
		_, statErr := os.Stat(partFilePath(id, 1))
		assert.True(t, os.IsNotExist(statErr))
		assert.Equal(t, int64(900), f.used(t))
	})
	t.Run("in-flight cap: the range length is the declared size", func(t *testing.T) {
		f := setupUPCFixture(t, false)
		f.putObj(t, "src.bin", mpBody(900, 10), nil)
		f.server.multipartMaxUploadBytes = 500
		id := f.createUpload(t, "dst.bin", nil)

		w := f.copyPart(t, "dst.bin", id, 1, "/test-bucket/src.bin", "bytes=0-599", nil)
		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code, w.Body.String(), "as an UploadPart past the cap")
		assert.Contains(t, w.Body.String(), "EntityTooLarge")
		w = f.copyPart(t, "dst.bin", id, 1, "/test-bucket/src.bin", "bytes=0-399", nil)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})
	t.Run("staging filesystem guard: 503", func(t *testing.T) {
		f := setupUPCFixture(t, false)
		f.putObj(t, "src.bin", mpBody(900, 11), nil)
		f.server.multipartStagingMinFree = func(total uint64) uint64 { return total }
		id := f.createUpload(t, "dst.bin", nil)

		w := f.copyPart(t, "dst.bin", id, 1, "/test-bucket/src.bin", "", nil)

		assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		assert.NotEmpty(t, w.Header().Get("Retry-After"))
		assert.Equal(t, 0, f.partRows(t, id))
	})
}

func TestUploadPartCopy_SourceReadFails_503NoPart(t *testing.T) {
	t.Run("the backend body breaks off", func(t *testing.T) {
		f := setupUPCFixture(t, false)
		f.putObj(t, "src.bin", mpBody(900, 12), nil)
		id := f.createUpload(t, "dst.bin", nil)
		f.drv.failAfter.Store(300)

		w := f.copyPart(t, "dst.bin", id, 1, "/test-bucket/src.bin", "", nil)

		assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		assert.NotEmpty(t, w.Header().Get("Retry-After"))
		assert.NotContains(t, w.Body.String(), "IncompleteBody", "the body is not the client's")
		assert.Equal(t, 0, f.partRows(t, id))
		assert.Empty(t, stagedTemps(t, id), "the staged temp file is gone")
		assert.Equal(t, int64(900), f.used(t), "the reservation is given back")
	})
	for _, tc := range []struct {
		name     string
		recorded int64
	}{
		{"fewer bytes than the row records", 1000},
		{"more bytes than the row records", 800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupUPCFixture(t, false)
			f.putObj(t, "src.bin", mpBody(900, 13), nil)
			_, err := f.db.Exec(`UPDATE object_head_cache SET size_bytes = $2 WHERE tenant_id = $1 AND object_key = 'src.bin'`, f.tenantID, tc.recorded)
			require.NoError(t, err)
			id := f.createUpload(t, "dst.bin", nil)

			w := f.copyPart(t, "dst.bin", id, 1, "/test-bucket/src.bin", "", nil)

			assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
			assert.NotEmpty(t, w.Header().Get("Retry-After"))
			assert.Equal(t, 0, f.partRows(t, id))
			assert.Empty(t, stagedTemps(t, id))
		})
	}
}

func TestUploadPartCopy_CompleteAssemblesTheSource(t *testing.T) {
	// Arrange: the aws-cli shape — a source copied in ranges.
	f := setupUPCFixture(t, false)
	src := mpBody(1000, 14)
	f.putObj(t, "src.bin", src, nil)
	id := f.createUpload(t, "dst.bin", nil)
	before := testutil.ToFloat64(longOpOutcomes.WithLabelValues(longOpUploadPartCopy, longOpOK))

	// Act: three parts; part 2 is copied twice (the second replaces it).
	var etags []string
	for i, rng := range []string{"bytes=0-399", "bytes=400-799", "bytes=800-999"} {
		w := f.copyPart(t, "dst.bin", id, i+1, "/test-bucket/src.bin", rng, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		etags = append(etags, parseCopyPartResult(t, w).ETag)
	}
	w := f.copyPart(t, "dst.bin", id, 2, "/test-bucket/src.bin", "bytes=400-799", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 3, f.partRows(t, id), "a part copied again replaces, never adds")
	cw := f.completeParts(t, "dst.bin", id, etags...)

	// Assert
	require.Equal(t, http.StatusOK, cw.Code, cw.Body.String())
	assert.Equal(t, src, f.getObj(t, "dst.bin"), "byte-equal to the source")
	var etag string
	require.NoError(t, f.db.QueryRow(`SELECT etag FROM object_head_cache WHERE tenant_id=$1 AND object_key='dst.bin'`, f.tenantID).Scan(&etag))
	assert.Regexp(t, `^[0-9a-f]{32}-3$`, etag)
	assert.Equal(t, before+4, testutil.ToFloat64(longOpOutcomes.WithLabelValues(longOpUploadPartCopy, longOpOK)))
	f.ledgerAgrees(t, "after the complete")
}

func TestUploadPartCopy_AnotherTenantsBucketOfTheSameName_404(t *testing.T) {
	// Arrange: the other tenant's test-bucket holds other.bin.
	f := setupUPCFixture(t, false)
	other := setupUPCFixture(t, false)
	other.putObj(t, "other.bin", []byte("NOT YOURS"), nil)
	id := f.createUpload(t, "dst.bin", nil)

	// Act
	w := f.copyPart(t, "dst.bin", id, 1, "/test-bucket/other.bin", "", nil)

	// Assert
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "NoSuchKey")
	assert.Equal(t, 0, f.partRows(t, id))
}

func TestUploadPartCopy_ParsesAsItsOwnOperation(t *testing.T) {
	p := NewS3Parser(zap.NewNop())
	r := httptest.NewRequest("PUT", "/b/k?partNumber=1&uploadId=u", nil)
	r.Header.Set("x-amz-copy-source", "/b/src")
	req, err := p.ParseRequest(r)
	require.NoError(t, err)
	assert.Equal(t, "UploadPartCopy", req.Operation)

	r = httptest.NewRequest("PUT", "/b/k?partNumber=1&uploadId=u", nil)
	req, err = p.ParseRequest(r)
	require.NoError(t, err)
	assert.Equal(t, "UploadPart", req.Operation)
}

// ---- the scope gate, through real SigV4 (R5-02's class, second entry point)

// scopedKey creates an API key of the fixture's user with perms and buckets.
func (f *bypassFixture) scopedKey(perms, buckets []string) (ak, sk string) {
	f.t.Helper()
	id := uuid.New().String()
	ak, sk = "VLT_"+strings.ToUpper(strings.ReplaceAll(id[:13], "-", "")), "sk-"+id
	pj, err := json.Marshal(perms)
	require.NoError(f.t, err)
	f.exec(`INSERT INTO api_keys (id, user_id, tenant_id, name, key_id, secret_hash, secret_key, permissions, bucket_scope) VALUES ($1, $2, $3, 'scoped', $4, 'h', $5, $6, $7)`,
		id, f.userID, f.tenantID, ak, sk, pj, pq.Array(buckets))
	return ak, sk
}

func TestUploadPartCopy_ScopedKeyNeedsGetObjectOnTheSource(t *testing.T) {
	// Arrange: a source in "locked", an upload in "locked-versions"; the
	// keys may upload parts into the destination only.
	f := setupBypassFixture(t)
	ctx := context.Background()
	_, err := f.root().PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(f.bucket), Key: aws.String("src.bin"),
		Body: strings.NewReader("SOURCE BYTES THE KEY MAY NOT READ")})
	require.NoError(t, err)
	mp, err := f.root().CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(f.vbucket), Key: aws.String("dst.bin")})
	require.NoError(t, err)
	partOnly := f.client(f.scopedKey([]string{"UploadPart"}, []string{f.vbucket}))
	withGet := f.client(f.scopedKey([]string{"UploadPart", "GetObject"}, []string{f.vbucket, f.bucket}))
	in := &s3.UploadPartCopyInput{Bucket: aws.String(f.vbucket), Key: aws.String("dst.bin"), UploadId: mp.UploadId,
		PartNumber: aws.Int32(1), CopySource: aws.String(f.bucket + "/src.bin")}

	// Act + Assert: no GetObject, source bucket out of scope → 403.
	_, err = partOnly.UploadPartCopy(ctx, in)
	requireAccessDenied(t, err)
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM multipart_parts WHERE upload_id = $1`, *mp.UploadId).Scan(&n))
	assert.Equal(t, 0, n)

	// A key that may read the source copies it (UploadPart is the
	// destination's permission; the SDK parses the CopyPartResult).
	out, err := withGet.UploadPartCopy(ctx, in)
	require.NoError(t, err)
	require.NotNil(t, out.CopyPartResult)
	assert.Equal(t, `"`+md5Hex([]byte("SOURCE BYTES THE KEY MAY NOT READ"))+`"`, aws.ToString(out.CopyPartResult.ETag))
	_, err = f.root().CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(f.vbucket), Key: aws.String("dst.bin"),
		UploadId: mp.UploadId, MultipartUpload: &s3types.CompletedMultipartUpload{Parts: []s3types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: out.CopyPartResult.ETag}}}})
	require.NoError(t, err)
	_ = os.RemoveAll(multipartDir(*mp.UploadId))
}
