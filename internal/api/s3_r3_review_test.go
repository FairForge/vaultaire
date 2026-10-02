package api

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 — asserting S3 ETag semantics
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/drivers"
)

// Review R3 (docs/reviews/R3-multipart-copy-batch.md): multipart complete,
// CopyObject and batch delete must honour the same invariants as plain PUT
// and DELETE — Object Lock, the head row as routing truth (backend_name),
// the head row as the ONLY description of the bytes (stale SSE/metadata
// columns cleared on overwrite), region pinning, versioning ledger rows,
// and typed miss detection on S3-class backends.

// ---- helpers on the DB-backed versioning fixture ----------------------------

func (f *versioningFixture) s3(t *testing.T, method, path string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, req)
	return w
}

func (f *versioningFixture) mpInitiate(t *testing.T, key string, hdr map[string]string) string {
	t.Helper()
	w := f.s3(t, "POST", "/"+f.bucket+"/"+key+"?uploads", nil, hdr)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res))
	return res.UploadID
}

func (f *versioningFixture) mpUploadPart(t *testing.T, key, uploadID string, n int, body []byte) string {
	t.Helper()
	w := f.s3(t, "PUT", fmt.Sprintf("/%s/%s?partNumber=%d&uploadId=%s", f.bucket, key, n, uploadID), bytes.NewReader(body), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return w.Header().Get("ETag")
}

func completeBody(parts ...string) io.Reader {
	var b strings.Builder
	b.WriteString("<CompleteMultipartUpload>")
	for i, etag := range parts {
		fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", i+1, etag)
	}
	b.WriteString("</CompleteMultipartUpload>")
	return strings.NewReader(b.String())
}

func (f *versioningFixture) mpComplete(t *testing.T, key, uploadID string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	return f.s3(t, "POST", fmt.Sprintf("/%s/%s?uploadId=%s", f.bucket, key, uploadID), body,
		map[string]string{"Content-Type": "application/xml"})
}

// mpUpload runs initiate → one part → complete and returns the complete response.
func (f *versioningFixture) mpUpload(t *testing.T, key string, content []byte, initiateHdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	id := f.mpInitiate(t, key, initiateHdr)
	etag := f.mpUploadPart(t, key, id, 1, content)
	return f.mpComplete(t, key, id, completeBody(etag))
}

func (f *versioningFixture) copyObject(t *testing.T, srcKey, destKey string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	h := map[string]string{"x-amz-copy-source": "/" + f.bucket + "/" + srcKey}
	for k, v := range hdr {
		h[k] = v
	}
	return f.s3(t, "PUT", "/"+f.bucket+"/"+destKey, nil, h)
}

type r3HeadRow struct {
	Size        int64
	ETag        string
	ContentType string
	Backend     string
	Metadata    string
	EncAlgo     string
	Disposition string
	CacheCtl    string
	Chunked     bool
}

func (f *versioningFixture) headRow(t *testing.T, key string) r3HeadRow {
	t.Helper()
	var row r3HeadRow
	require.NoError(t, f.db.QueryRow(`
		SELECT size_bytes, etag, content_type, COALESCE(backend_name, ''), metadata::text,
		       encryption_algorithm, content_disposition, cache_control, is_chunked
		FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		f.tenantID, f.bucket, key).Scan(&row.Size, &row.ETag, &row.ContentType, &row.Backend,
		&row.Metadata, &row.EncAlgo, &row.Disposition, &row.CacheCtl, &row.Chunked))
	return row
}

func (f *versioningFixture) pinRegion(t *testing.T, region string) {
	t.Helper()
	_, err := f.db.Exec(`UPDATE buckets SET region = $1 WHERE tenant_id = $2 AND name = $3`, region, f.tenantID, f.bucket)
	require.NoError(t, err)
}

// ---- R3-01 (P0): Object Lock on multipart complete and CopyObject --------------

func TestMultipartComplete_RefusedOnLockedKey(t *testing.T) {
	f := setupVersioningFixture(t)
	key := "retained.bin"
	f.putObject(t, key, "RETAINED")
	lockKey(t, f, key, "COMPLIANCE", time.Now().Add(24*time.Hour))

	w := f.mpUpload(t, key, bytes.NewBufferString("OVERWRITTEN-BY-MULTIPART").Bytes(), nil)

	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, "RETAINED", onDisk(t, f, key), "locked bytes must survive a multipart complete")
	assert.Equal(t, int64(8), f.headRow(t, key).Size)
}

func TestCopyObject_RefusedOnLockedKey(t *testing.T) {
	f := setupVersioningFixture(t)
	f.putObject(t, "src.bin", "SOURCE-BYTES")
	key := "retained.bin"
	f.putObject(t, key, "RETAINED")
	lockKey(t, f, key, "COMPLIANCE", time.Now().Add(24*time.Hour))

	w := f.copyObject(t, "src.bin", key, nil)

	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, "RETAINED", onDisk(t, f, key), "locked bytes must survive a CopyObject")
}

// ---- R3-02 (P1): UploadPartCopy is not implemented — never a silent empty part

func TestUploadPartCopy_RefusedNotImplemented(t *testing.T) {
	f := setupVersioningFixture(t)
	f.putObject(t, "src.bin", "SOURCE-BYTES")
	id := f.mpInitiate(t, "dest.bin", nil)

	w := f.s3(t, "PUT", fmt.Sprintf("/%s/dest.bin?partNumber=1&uploadId=%s", f.bucket, id), nil,
		map[string]string{"x-amz-copy-source": "/" + f.bucket + "/src.bin"})

	assert.Equal(t, http.StatusNotImplemented, w.Code, w.Body.String())
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM multipart_parts WHERE upload_id = $1`, id).Scan(&n))
	assert.Equal(t, 0, n, "a refused UploadPartCopy must not record a part")
}

// ---- R3-03 / R3-04 (P1): the head row carries backend_name and the attributes
// the client sent on CreateMultipartUpload -----------------------------------

func TestMultipartComplete_HeadRowCarriesBackendAndInitiateAttributes(t *testing.T) {
	f := setupVersioningFixture(t)
	key := "video.mp4"
	w := f.mpUpload(t, key, []byte("MP4-BYTES"), map[string]string{
		"Content-Type":        "video/mp4",
		"X-Amz-Meta-Camera":   "sony",
		"Cache-Control":       "max-age=60",
		"Content-Disposition": `attachment; filename="v.mp4"`,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	row := f.headRow(t, key)
	assert.Equal(t, "local", row.Backend, "backend_name is the routing truth for GET/DELETE")
	assert.Equal(t, "video/mp4", row.ContentType, "content type comes from CreateMultipartUpload, not from the Complete request")
	assert.JSONEq(t, `{"camera":"sony"}`, row.Metadata)
	assert.Equal(t, "max-age=60", row.CacheCtl)
	assert.Equal(t, `attachment; filename="v.mp4"`, row.Disposition)
	assert.Equal(t, int64(9), row.Size)
	assert.Regexp(t, `^[0-9a-f]{32}-1$`, row.ETag)
}

func TestMultipartInitiate_InvalidMetadataRejectedBeforeAnyState(t *testing.T) {
	f := setupVersioningFixture(t)
	hdr := map[string]string{}
	for i := 0; i < metadataMaxKeys+1; i++ {
		hdr[fmt.Sprintf("X-Amz-Meta-K%d", i)] = "v"
	}
	w := f.s3(t, "POST", "/"+f.bucket+"/too-much-meta?uploads", nil, hdr)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM multipart_uploads WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	assert.Equal(t, 0, n)
}

// ---- R3-05 (P1): overwriting via multipart / copy clears every column the
// previous object set — a stale encryption_algorithm made the new plaintext
// object unreadable (live: GET 500) ---------------------------------------------

func (f *versioningFixture) seedStaleColumns(t *testing.T, key string) {
	t.Helper()
	_, err := f.db.Exec(`UPDATE object_head_cache
		SET encryption_algorithm = 'AES256', metadata = '{"old":"x"}', content_disposition = 'inline',
		    cache_control = 'no-store'
		WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, f.tenantID, f.bucket, key)
	require.NoError(t, err)
}

func TestMultipartComplete_OverwriteClearsStaleColumns(t *testing.T) {
	f := setupVersioningFixture(t)
	key := "was-encrypted.bin"
	f.putObject(t, key, "OLD")
	f.seedStaleColumns(t, key)

	w := f.mpUpload(t, key, []byte("NEW-PLAINTEXT"), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	row := f.headRow(t, key)
	assert.Empty(t, row.EncAlgo, "multipart objects are stored as plaintext; the row must say so")
	assert.JSONEq(t, `{}`, row.Metadata)
	assert.Empty(t, row.Disposition)
	assert.Empty(t, row.CacheCtl)

	g := f.getObject(t, key)
	assert.Equal(t, http.StatusOK, g.Code)
	assert.Equal(t, "NEW-PLAINTEXT", g.Body.String())
}

func TestCopyObject_OverwriteClearsStaleColumns(t *testing.T) {
	f := setupVersioningFixture(t)
	f.putObject(t, "src.bin", "SOURCE")
	key := "was-encrypted.bin"
	f.putObject(t, key, "OLD")
	f.seedStaleColumns(t, key)

	w := f.copyObject(t, "src.bin", key, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	row := f.headRow(t, key)
	assert.Empty(t, row.EncAlgo)
	assert.Equal(t, "local", row.Backend)
	g := f.getObject(t, key)
	assert.Equal(t, http.StatusOK, g.Code)
	assert.Equal(t, "SOURCE", g.Body.String())
}

// ---- R3-06 (P1): x-amz-metadata-directive ------------------------------------

func TestCopyObject_MetadataDirectiveCopyAndReplace(t *testing.T) {
	f := setupVersioningFixture(t)
	req := httptest.NewRequest("PUT", "/"+f.bucket+"/src.txt", bytes.NewReader([]byte("SRC")))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Amz-Meta-Foo", "bar")
	req.Header.Set("Content-Disposition", `attachment; filename="s.txt"`)
	req.Header.Set("Cache-Control", "max-age=60")
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, f.bucket, "src.txt")
	require.Equal(t, http.StatusOK, w.Code)

	t.Run("COPY (default) carries the source attributes", func(t *testing.T) {
		w := f.copyObject(t, "src.txt", "copied.txt", nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		row := f.headRow(t, "copied.txt")
		assert.Equal(t, "text/plain", row.ContentType)
		assert.JSONEq(t, `{"foo":"bar"}`, row.Metadata)
		assert.Equal(t, `attachment; filename="s.txt"`, row.Disposition)
		assert.Equal(t, "max-age=60", row.CacheCtl)
	})

	t.Run("REPLACE takes the request's attributes", func(t *testing.T) {
		w := f.copyObject(t, "src.txt", "replaced.txt", map[string]string{
			"x-amz-metadata-directive": "REPLACE",
			"Content-Type":             "application/json",
			"X-Amz-Meta-New":           "val",
		})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		row := f.headRow(t, "replaced.txt")
		assert.Equal(t, "application/json", row.ContentType)
		assert.JSONEq(t, `{"new":"val"}`, row.Metadata)
		assert.Empty(t, row.Disposition, "REPLACE does not inherit the source's disposition")
		assert.Empty(t, row.CacheCtl)
	})

	t.Run("self-copy with REPLACE rewrites the attributes in place", func(t *testing.T) {
		w := f.copyObject(t, "src.txt", "src.txt", map[string]string{
			"x-amz-metadata-directive": "REPLACE",
			"Content-Type":             "text/csv",
			"X-Amz-Meta-Changed":       "yes",
		})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		row := f.headRow(t, "src.txt")
		assert.Equal(t, "text/csv", row.ContentType)
		assert.JSONEq(t, `{"changed":"yes"}`, row.Metadata)
		assert.Equal(t, "SRC", onDisk(t, f, "src.txt"))
	})

	t.Run("REPLACE with invalid metadata is refused before any write", func(t *testing.T) {
		hdr := map[string]string{"x-amz-metadata-directive": "REPLACE"}
		for i := 0; i < metadataMaxKeys+1; i++ {
			hdr[fmt.Sprintf("X-Amz-Meta-K%d", i)] = "v"
		}
		w := f.copyObject(t, "src.txt", "never.txt", hdr)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		_, err := os.Stat(filepath.Join(f.tempDir, f.tenant.NamespaceContainer(f.bucket), "never.txt"))
		assert.True(t, os.IsNotExist(err))
	})
}

// ---- R3-09 (P1): versioned buckets get a ledger row and a version id ----------

func TestMultipartComplete_VersionedBucketRecordsVersion(t *testing.T) {
	f := setupVersioningFixture(t)
	f.setVersioning(t, "Enabled")
	key := "v.bin"
	f.putObject(t, key, "V1")

	w := f.mpUpload(t, key, []byte("V2-VIA-MULTIPART"), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotEmpty(t, w.Header().Get("x-amz-version-id"))
	assert.Equal(t, 2, f.countVersions(t, key))

	var latestSize int64
	require.NoError(t, f.db.QueryRow(`SELECT size_bytes FROM object_versions
		WHERE tenant_id=$1 AND bucket=$2 AND object_key=$3 AND is_latest`, f.tenantID, f.bucket, key).Scan(&latestSize))
	assert.Equal(t, int64(len("V2-VIA-MULTIPART")), latestSize)
}

func TestCopyObject_VersionedBucketRecordsVersion(t *testing.T) {
	f := setupVersioningFixture(t)
	f.setVersioning(t, "Enabled")
	f.putObject(t, "src.bin", "SOURCE")
	key := "v.bin"
	f.putObject(t, key, "V1")

	w := f.copyObject(t, "src.bin", key, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotEmpty(t, w.Header().Get("x-amz-version-id"))
	assert.Equal(t, 2, f.countVersions(t, key))
}

// ---- R3-08 (P1): region pinning applies to multipart complete and copy --------

func TestMultipartComplete_RegionPinnedBucket(t *testing.T) {
	f := setupVersioningFixture(t)
	f.pinRegion(t, "eu-west-1")

	t.Run("no driver for the region → refused, nothing on the primary", func(t *testing.T) {
		w := f.mpUpload(t, "eu.bin", []byte("EU-BYTES"), nil)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		_, err := os.Stat(filepath.Join(f.tempDir, f.tenant.NamespaceContainer(f.bucket), "eu.bin"))
		assert.True(t, os.IsNotExist(err), "a region-pinned object must never land on the primary")
	})

	euDir := t.TempDir()
	f.eng.AddDriver("idrive-eu-west-1", drivers.NewLocalDriver(euDir, zap.NewNop()))

	t.Run("driver present → object lands in the region and the row says so", func(t *testing.T) {
		w := f.mpUpload(t, "eu.bin", []byte("EU-BYTES"), nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, "idrive-eu-west-1", f.headRow(t, "eu.bin").Backend)
		b, err := os.ReadFile(filepath.Join(euDir, f.tenant.NamespaceContainer(f.bucket), "eu.bin"))
		require.NoError(t, err)
		assert.Equal(t, "EU-BYTES", string(b))
	})
}

func TestCopyObject_RegionPinnedBucket(t *testing.T) {
	f := setupVersioningFixture(t)
	f.putObject(t, "src.bin", "SOURCE")
	f.pinRegion(t, "eu-west-1")

	w := f.copyObject(t, "src.bin", "eu-copy.bin", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	_, err := os.Stat(filepath.Join(f.tempDir, f.tenant.NamespaceContainer(f.bucket), "eu-copy.bin"))
	assert.True(t, os.IsNotExist(err))

	euDir := t.TempDir()
	f.eng.AddDriver("idrive-eu-west-1", drivers.NewLocalDriver(euDir, zap.NewNop()))
	w = f.copyObject(t, "src.bin", "eu-copy.bin", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "idrive-eu-west-1", f.headRow(t, "eu-copy.bin").Backend)
}

// ---- Item 1.11 / PR #334 coverage: aws-chunked part bodies are decoded --------

func awsChunkedBody(payload []byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%x;chunk-signature=deadbeef\r\n", len(payload))
	b.Write(payload)
	b.WriteString("\r\n0;chunk-signature=deadbeef\r\n")
	b.WriteString("x-amz-checksum-crc32:AAAAAA==\r\n\r\n")
	return b.Bytes()
}

func TestUploadPart_AwsChunkedFramingIsStripped(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)
	w := doS3Request(srv, tnt, "POST", "/test-bucket/f.bin?uploads", nil)
	require.Equal(t, http.StatusOK, w.Code)
	var init InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &init))
	t.Cleanup(func() { _ = os.RemoveAll(multipartDir(init.UploadID)) })

	payload := bytes.Repeat([]byte("part-data-"), 1000)
	framed := awsChunkedBody(payload)
	req := httptest.NewRequest("PUT", "/test-bucket/f.bin?partNumber=1&uploadId="+init.UploadID, bytes.NewReader(framed))
	req.Header.Set("x-amz-content-sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("x-amz-decoded-content-length", fmt.Sprint(len(payload)))
	req.ContentLength = int64(len(framed))
	req = req.WithContext(s3Ctx(req.Context(), tnt))
	rec := httptest.NewRecorder()
	srv.handleS3Request(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	wantETag := fmt.Sprintf(`"%x"`, md5.Sum(payload)) // #nosec G401
	assert.Equal(t, wantETag, rec.Header().Get("ETag"), "ETag is the MD5 of the DECODED part")
	onDisk, err := os.ReadFile(partFilePath(init.UploadID, 1))
	require.NoError(t, err)
	assert.Equal(t, payload, onDisk, "the wire framing must not be stored")

	memUploadsMu.RLock()
	p := memUploads[init.UploadID].Parts[1]
	memUploadsMu.RUnlock()
	assert.Equal(t, int64(len(payload)), p.Size)
}

// ---- R3-11 (P2): a failed part re-upload never destroys the part it retries --

type errAfterReader struct {
	data []byte
	n    int
}

func (e *errAfterReader) Read(p []byte) (int, error) {
	if e.n >= len(e.data) {
		return 0, errors.New("simulated client disconnect")
	}
	c := copy(p, e.data[e.n:])
	e.n += c
	return c, nil
}

func TestUploadPart_FailedRetryKeepsPreviousPart(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)
	w := doS3Request(srv, tnt, "POST", "/test-bucket/f.bin?uploads", nil)
	require.Equal(t, http.StatusOK, w.Code)
	var init InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &init))
	t.Cleanup(func() { _ = os.RemoveAll(multipartDir(init.UploadID)) })
	path := "/test-bucket/f.bin?partNumber=1&uploadId=" + init.UploadID

	w = doS3Request(srv, tnt, "PUT", path, bytes.NewReader([]byte("GOOD-PART")))
	require.Equal(t, http.StatusOK, w.Code)
	goodETag := w.Header().Get("ETag")

	w = doS3Request(srv, tnt, "PUT", path, &errAfterReader{data: []byte("PARTIAL")})
	require.NotEqual(t, http.StatusOK, w.Code, "a body that fails mid-stream is not a stored part")

	b, err := os.ReadFile(partFilePath(init.UploadID, 1))
	require.NoError(t, err, "the previously uploaded part must still be on disk")
	assert.Equal(t, "GOOD-PART", string(b))
	memUploadsMu.RLock()
	p := memUploads[init.UploadID].Parts[1]
	memUploadsMu.RUnlock()
	assert.Equal(t, goodETag, p.ETag)

	entries, err := os.ReadDir(multipartDir(init.UploadID))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temp file left behind: %v", entries)
}

// ---- R3-12 (P2): CompleteMultipartUpload body is bounded and validated -------

func TestCompleteMultipart_BodyLimitAndMalformedXML(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)
	w := doS3Request(srv, tnt, "POST", "/test-bucket/f.bin?uploads", nil)
	require.Equal(t, http.StatusOK, w.Code)
	var init InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &init))
	t.Cleanup(func() { _ = os.RemoveAll(multipartDir(init.UploadID)) })
	w = doS3Request(srv, tnt, "PUT", "/test-bucket/f.bin?partNumber=1&uploadId="+init.UploadID, bytes.NewReader([]byte("x")))
	require.Equal(t, http.StatusOK, w.Code)
	path := "/test-bucket/f.bin?uploadId=" + init.UploadID

	t.Run("oversized body", func(t *testing.T) {
		w := doS3Request(srv, tnt, "POST", path, io.LimitReader(zeroReader{}, maxCompleteMultipartBodyBytes+1))
		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		assert.Contains(t, w.Body.String(), ErrEntityTooLarge)
	})
	t.Run("malformed body", func(t *testing.T) {
		w := doS3Request(srv, tnt, "POST", path, strings.NewReader("<CompleteMultipartUpload><Part>"))
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), ErrMalformedXML)
	})
	t.Run("part list that names an ETag we never saw", func(t *testing.T) {
		w := doS3Request(srv, tnt, "POST", path, completeBody(`"00000000000000000000000000000000"`))
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), ErrInvalidPart)
	})
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

// ---- R5-28 (P3): upload ids are random --------------------------------------

func TestMultipart_UploadIDsAreRandom(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)
	ids := map[string]bool{}
	for i := 0; i < 3; i++ {
		w := doS3Request(srv, tnt, "POST", "/test-bucket/f.bin?uploads", nil)
		require.Equal(t, http.StatusOK, w.Code)
		var init InitiateMultipartUploadResult
		require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &init))
		t.Cleanup(func() { _ = os.RemoveAll(multipartDir(init.UploadID)) })
		assert.Regexp(t, regexp.MustCompile(`^upload-[0-9a-f]{32}$`), init.UploadID, "not derived from the clock")
		ids[init.UploadID] = true
	}
	assert.Len(t, ids, 3)
}

// ---- R6-25 / WP-R6-1 hand-offs: typed misses and the recorded backend --------

// sdkMissDriver answers every Get/Delete with the shape an aws-sdk-go-v2
// driver produces for a 404 (types.NoSuchKey is a smithy.APIError).
type sdkMissDriver struct {
	*drivers.LocalDriver
	deletes []string
}

func (d *sdkMissDriver) Get(context.Context, string, string) (io.ReadCloser, error) {
	return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "The specified key does not exist."}
}

func (d *sdkMissDriver) Delete(_ context.Context, container, artifact string) error {
	d.deletes = append(d.deletes, container+"/"+artifact)
	return &smithy.GenericAPIError{Code: "NoSuchKey", Message: "The specified key does not exist."}
}

func TestDeleteObjects_MissOnSDKBackendIsIdempotent(t *testing.T) {
	f := setupVersioningFixture(t)
	miss := &sdkMissDriver{LocalDriver: drivers.NewLocalDriver(t.TempDir(), zap.NewNop())}
	f.eng.AddDriver("sdk", miss)
	f.eng.SetPrimary("sdk")

	w := f.s3(t, "POST", "/"+f.bucket+"?delete", strings.NewReader(`<Delete><Object><Key>gone.txt</Key></Object></Delete>`), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res DeleteResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res))
	assert.Empty(t, res.Errors, "an SDK-shaped NoSuchKey is a miss, not a failure: %+v", res.Errors)
	assert.Len(t, res.Deleted, 1)
}

func TestDeleteObjects_HintsRecordedBackend(t *testing.T) {
	f := setupVersioningFixture(t)
	farDir := t.TempDir()
	far := drivers.NewLocalDriver(farDir, zap.NewNop())
	f.eng.AddDriver("far", far)
	container := f.tenant.NamespaceContainer(f.bucket)
	require.NoError(t, far.Put(context.Background(), container, "off.bin", bytes.NewReader([]byte("FAR"))))
	_, err := f.db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type, backend_name)
		VALUES ($1, $2, 'off.bin', 3, 'e', 'application/octet-stream', 'far')`, f.tenantID, f.bucket)
	require.NoError(t, err)

	w := f.s3(t, "POST", "/"+f.bucket+"?delete", strings.NewReader(`<Delete><Object><Key>off.bin</Key></Object></Delete>`), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res DeleteResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res))
	assert.Empty(t, res.Errors)
	_, statErr := os.Stat(filepath.Join(farDir, container, "off.bin"))
	assert.True(t, os.IsNotExist(statErr), "the bytes on the recorded backend must be gone, not orphaned")
}

func TestCopyObject_SourceMissOnSDKBackendIs404(t *testing.T) {
	f := setupVersioningFixture(t)
	miss := &sdkMissDriver{LocalDriver: drivers.NewLocalDriver(t.TempDir(), zap.NewNop())}
	f.eng.AddDriver("sdk", miss)
	f.eng.SetPrimary("sdk")

	w := f.copyObject(t, "absent.bin", "dest.bin", nil)
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), ErrNoSuchKey)
}

// ---- R3-10: a failed head-row write is a 500, never a 200 without a row ------
// (a dropped connection is simulated by closing the pool under the handler)

func TestMultipartComplete_HeadRowFailureIsNot200(t *testing.T) {
	f := setupVersioningFixture(t)
	key := "unrecorded.bin"
	id := f.mpInitiate(t, key, nil)
	etag := f.mpUploadPart(t, key, id, 1, []byte("BYTES"))

	// Break the head-cache upsert only: a trigger that refuses inserts to
	// object_head_cache for this tenant. Dropped in cleanup.
	_, err := f.db.Exec(`CREATE OR REPLACE FUNCTION r3_refuse_head() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'r3: simulated head-cache failure'; END $$ LANGUAGE plpgsql`)
	require.NoError(t, err)
	_, err = f.db.Exec(fmt.Sprintf(`CREATE TRIGGER r3_refuse_head_%d BEFORE INSERT ON object_head_cache
		FOR EACH ROW WHEN (NEW.tenant_id = '%s') EXECUTE FUNCTION r3_refuse_head()`, os.Getpid(), f.tenantID))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS r3_refuse_head_%d ON object_head_cache`, os.Getpid()))
	})

	w := f.mpComplete(t, key, id, completeBody(etag))
	assert.Equal(t, http.StatusInternalServerError, w.Code, "no head row means HEAD/GET 404 forever; the client must retry")

	var status string
	require.NoError(t, f.db.QueryRow(`SELECT status FROM multipart_uploads WHERE upload_id = $1`, id).Scan(&status))
	assert.Equal(t, "active", status, "the upload stays completable so the retry is not NoSuchUpload")
}

// ---- upload ids are validated syntactically before any path is built --------

func TestMultipart_MalformedUploadIDNeverTouchesTheFilesystem(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)
	for _, id := range []string{"", "..", "../../etc", "upload-../x", "upload-zz", "upload-" + strings.Repeat("a", 33), "upload-1-2-3"} {
		q := "?uploadId=" + id
		for _, c := range []struct{ method, path string }{
			{"PUT", "/test-bucket/f.bin" + q + "&partNumber=1"},
			{"POST", "/test-bucket/f.bin" + q},
			{"DELETE", "/test-bucket/f.bin" + q},
			{"GET", "/test-bucket/f.bin" + q},
		} {
			w := doS3Request(srv, tnt, c.method, c.path, bytes.NewReader([]byte("x")))
			assert.Equal(t, http.StatusNotFound, w.Code, "%s %s", c.method, c.path)
			assert.Contains(t, w.Body.String(), ErrNoSuchUpload, "%s %s", c.method, c.path)
		}
	}
	for _, id := range []string{"upload-zz", "upload-1-2-3", "x"} {
		_, err := os.Stat(filepath.Join(multipartTempBase, id))
		assert.True(t, os.IsNotExist(err), "no directory for %q", id)
	}
	assert.True(t, validUploadID("upload-"+strings.Repeat("0", 32)))
	assert.True(t, validUploadID("upload-1790649661-802876000"), "pre-R3 ids still resolve")
}
