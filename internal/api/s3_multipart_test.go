package api

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// newTestMultipartServer creates a test server with local driver for multipart tests.
func newTestMultipartServer(t *testing.T) (*Server, *tenant.Tenant, string) {
	t.Helper()
	logger := zap.NewNop()
	eng := engine.NewEngine(nil, logger, nil)

	tempDir, err := os.MkdirTemp("", "vaultaire-multipart-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)
	eng.SetPrimary("local")

	testTenant := &tenant.Tenant{
		ID:        "test-tenant",
		Namespace: "tenant/test-tenant/",
	}

	// Pre-create the namespaced bucket directory
	bucketDir := filepath.Join(tempDir, "test-tenant_test-bucket")
	require.NoError(t, os.MkdirAll(bucketDir, 0755))

	srv := &Server{
		logger:   logger,
		router:   chi.NewRouter(),
		engine:   eng,
		testMode: true,
	}
	srv.router.HandleFunc("/*", srv.handleS3Request)

	// Clean in-memory state between tests
	memUploadsMu.Lock()
	memUploads = make(map[string]*memUpload)
	memUploadsMu.Unlock()

	return srv, testTenant, tempDir
}

// doS3Request executes an S3 request against the test server with tenant context.
func doS3Request(srv *Server, t *tenant.Tenant, method, path string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	ctx := tenant.WithTenant(req.Context(), t)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	srv.handleS3Request(w, req)
	return w
}

func TestMultipart_InitiateUpload(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)

	// Act
	w := doS3Request(srv, tnt, "POST", "/test-bucket/large-file.bin?uploads", nil)

	// Assert
	require.Equal(t, http.StatusOK, w.Code)

	var result InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &result))
	assert.Equal(t, "test-bucket", result.Bucket)
	assert.Equal(t, "large-file.bin", result.Key)
	assert.NotEmpty(t, result.UploadID)
	assert.True(t, strings.HasPrefix(result.UploadID, "upload-"))
}

func TestMultipart_UploadPart_InvalidPartNumber(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)

	t.Run("zero", func(t *testing.T) {
		w := doS3Request(srv, tnt, "PUT", "/test-bucket/file?partNumber=0&uploadId=fake", bytes.NewReader([]byte("data")))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("too large", func(t *testing.T) {
		w := doS3Request(srv, tnt, "PUT", "/test-bucket/file?partNumber=10001&uploadId=fake", bytes.NewReader([]byte("data")))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("non-numeric", func(t *testing.T) {
		w := doS3Request(srv, tnt, "PUT", "/test-bucket/file?partNumber=abc&uploadId=fake", bytes.NewReader([]byte("data")))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestMultipart_UploadPart_NoSuchUpload(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)

	w := doS3Request(srv, tnt, "PUT", "/test-bucket/file?partNumber=1&uploadId=nonexistent", bytes.NewReader([]byte("data")))
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "NoSuchUpload")
}

func TestMultipart_FullLifecycle(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)

	// Arrange: initiate upload
	initW := doS3Request(srv, tnt, "POST", "/test-bucket/assembled.txt?uploads", nil)
	require.Equal(t, http.StatusOK, initW.Code)

	var initResult InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(initW.Body.Bytes(), &initResult))
	uploadID := initResult.UploadID

	// Act: upload 3 parts
	part1Data := []byte("Hello, ")
	part2Data := []byte("multipart ")
	part3Data := []byte("world!")

	part1W := doS3Request(srv, tnt, "PUT",
		fmt.Sprintf("/test-bucket/assembled.txt?uploadId=%s&partNumber=1", uploadID),
		bytes.NewReader(part1Data))
	require.Equal(t, http.StatusOK, part1W.Code)
	etag1 := part1W.Header().Get("ETag")
	assert.NotEmpty(t, etag1)

	part2W := doS3Request(srv, tnt, "PUT",
		fmt.Sprintf("/test-bucket/assembled.txt?uploadId=%s&partNumber=2", uploadID),
		bytes.NewReader(part2Data))
	require.Equal(t, http.StatusOK, part2W.Code)
	etag2 := part2W.Header().Get("ETag")

	part3W := doS3Request(srv, tnt, "PUT",
		fmt.Sprintf("/test-bucket/assembled.txt?uploadId=%s&partNumber=3", uploadID),
		bytes.NewReader(part3Data))
	require.Equal(t, http.StatusOK, part3W.Code)
	etag3 := part3W.Header().Get("ETag")

	// Act: list parts
	listW := doS3Request(srv, tnt, "GET",
		fmt.Sprintf("/test-bucket/assembled.txt?uploadId=%s", uploadID), nil)
	require.Equal(t, http.StatusOK, listW.Code)

	var listResult ListPartsResult
	require.NoError(t, xml.Unmarshal(listW.Body.Bytes(), &listResult))
	assert.Len(t, listResult.Parts, 3)
	assert.Equal(t, 1, listResult.Parts[0].PartNumber)
	assert.Equal(t, 2, listResult.Parts[1].PartNumber)
	assert.Equal(t, 3, listResult.Parts[2].PartNumber)

	// Act: complete upload with part manifest
	completeBody := fmt.Sprintf(`<CompleteMultipartUpload>
		<Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part>
		<Part><PartNumber>2</PartNumber><ETag>%s</ETag></Part>
		<Part><PartNumber>3</PartNumber><ETag>%s</ETag></Part>
	</CompleteMultipartUpload>`, etag1, etag2, etag3)

	completeW := doS3Request(srv, tnt, "POST",
		fmt.Sprintf("/test-bucket/assembled.txt?uploadId=%s", uploadID),
		strings.NewReader(completeBody))
	require.Equal(t, http.StatusOK, completeW.Code)

	var completeResult CompleteMultipartUploadResult
	require.NoError(t, xml.Unmarshal(completeW.Body.Bytes(), &completeResult))
	assert.Equal(t, "test-bucket", completeResult.Bucket)
	assert.Equal(t, "assembled.txt", completeResult.Key)
	assert.NotEmpty(t, completeResult.ETag)
	// Multipart ETag format: "hex-N"
	assert.Contains(t, completeResult.ETag, "-3")

	// Verify: GET the assembled object
	getW := doS3Request(srv, tnt, "GET", "/test-bucket/assembled.txt", nil)
	require.Equal(t, http.StatusOK, getW.Code)
	assert.Equal(t, "Hello, multipart world!", getW.Body.String())

	// Verify: temp files cleaned up
	_, err := os.Stat(multipartDir(uploadID))
	assert.True(t, os.IsNotExist(err), "temp dir should be cleaned up after complete")
}

func TestMultipart_Abort(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)

	// Arrange: initiate and upload a part
	initW := doS3Request(srv, tnt, "POST", "/test-bucket/aborted.txt?uploads", nil)
	require.Equal(t, http.StatusOK, initW.Code)

	var initResult InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(initW.Body.Bytes(), &initResult))
	uploadID := initResult.UploadID

	doS3Request(srv, tnt, "PUT",
		fmt.Sprintf("/test-bucket/aborted.txt?uploadId=%s&partNumber=1", uploadID),
		bytes.NewReader([]byte("some data")))

	// Act: abort
	abortW := doS3Request(srv, tnt, "DELETE",
		fmt.Sprintf("/test-bucket/aborted.txt?uploadId=%s", uploadID), nil)
	assert.Equal(t, http.StatusNoContent, abortW.Code)

	// Assert: upload part should fail after abort
	partW := doS3Request(srv, tnt, "PUT",
		fmt.Sprintf("/test-bucket/aborted.txt?uploadId=%s&partNumber=2", uploadID),
		bytes.NewReader([]byte("more data")))
	assert.Equal(t, http.StatusNotFound, partW.Code)
	assert.Contains(t, partW.Body.String(), "NoSuchUpload")

	// Assert: temp files cleaned up
	_, err := os.Stat(multipartDir(uploadID))
	assert.True(t, os.IsNotExist(err), "temp dir should be cleaned up after abort")
}

func TestMultipart_Abort_NonexistentUpload(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)

	w := doS3Request(srv, tnt, "DELETE", "/test-bucket/file?uploadId=does-not-exist", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "NoSuchUpload")
}

func TestMultipart_Complete_InvalidPartOrder(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)

	// Arrange
	initW := doS3Request(srv, tnt, "POST", "/test-bucket/file?uploads", nil)
	require.Equal(t, http.StatusOK, initW.Code)
	var initResult InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(initW.Body.Bytes(), &initResult))
	uploadID := initResult.UploadID

	doS3Request(srv, tnt, "PUT",
		fmt.Sprintf("/test-bucket/file?uploadId=%s&partNumber=1", uploadID),
		bytes.NewReader([]byte("part1")))
	doS3Request(srv, tnt, "PUT",
		fmt.Sprintf("/test-bucket/file?uploadId=%s&partNumber=2", uploadID),
		bytes.NewReader([]byte("part2")))

	// Act: send parts in wrong order
	completeBody := `<CompleteMultipartUpload>
		<Part><PartNumber>2</PartNumber><ETag>"x"</ETag></Part>
		<Part><PartNumber>1</PartNumber><ETag>"y"</ETag></Part>
	</CompleteMultipartUpload>`

	w := doS3Request(srv, tnt, "POST",
		fmt.Sprintf("/test-bucket/file?uploadId=%s", uploadID),
		strings.NewReader(completeBody))

	// Assert
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "InvalidPartOrder")
}

func TestMultipart_Complete_MismatchedETag(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)

	// Arrange
	initW := doS3Request(srv, tnt, "POST", "/test-bucket/file?uploads", nil)
	require.Equal(t, http.StatusOK, initW.Code)
	var initResult InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(initW.Body.Bytes(), &initResult))
	uploadID := initResult.UploadID

	doS3Request(srv, tnt, "PUT",
		fmt.Sprintf("/test-bucket/file?uploadId=%s&partNumber=1", uploadID),
		bytes.NewReader([]byte("part1")))

	// Act: complete with wrong ETag
	completeBody := `<CompleteMultipartUpload>
		<Part><PartNumber>1</PartNumber><ETag>"wrong-etag"</ETag></Part>
	</CompleteMultipartUpload>`

	w := doS3Request(srv, tnt, "POST",
		fmt.Sprintf("/test-bucket/file?uploadId=%s", uploadID),
		strings.NewReader(completeBody))

	// Assert
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "InvalidPart")
}

func TestMultipart_ListMultipartUploads(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)

	// Arrange: create two uploads
	init1 := doS3Request(srv, tnt, "POST", "/test-bucket/file1.bin?uploads", nil)
	require.Equal(t, http.StatusOK, init1.Code)

	init2 := doS3Request(srv, tnt, "POST", "/test-bucket/file2.bin?uploads", nil)
	require.Equal(t, http.StatusOK, init2.Code)

	// Act: list uploads for bucket
	listW := doS3Request(srv, tnt, "GET", "/test-bucket?uploads", nil)
	require.Equal(t, http.StatusOK, listW.Code)

	var result ListMultipartUploadsResult
	require.NoError(t, xml.Unmarshal(listW.Body.Bytes(), &result))
	assert.Equal(t, "test-bucket", result.Bucket)
	assert.Len(t, result.Uploads, 2)
}

func TestMultipart_TenantIsolation(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)

	// Tenant A initiates upload
	initW := doS3Request(srv, tnt, "POST", "/test-bucket/secret.txt?uploads", nil)
	require.Equal(t, http.StatusOK, initW.Code)
	var initResult InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(initW.Body.Bytes(), &initResult))
	uploadID := initResult.UploadID

	// Tenant B tries to upload a part to tenant A's upload
	otherTenant := &tenant.Tenant{
		ID:        "other-tenant",
		Namespace: "tenant/other-tenant/",
	}

	partW := doS3Request(srv, otherTenant, "PUT",
		fmt.Sprintf("/test-bucket/secret.txt?uploadId=%s&partNumber=1", uploadID),
		bytes.NewReader([]byte("evil data")))
	assert.Equal(t, http.StatusNotFound, partW.Code)
	assert.Contains(t, partW.Body.String(), "NoSuchUpload")
}

func TestMultipart_PartOverwrite(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)

	// Arrange
	initW := doS3Request(srv, tnt, "POST", "/test-bucket/overwrite.txt?uploads", nil)
	require.Equal(t, http.StatusOK, initW.Code)
	var initResult InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(initW.Body.Bytes(), &initResult))
	uploadID := initResult.UploadID

	// Upload part 1 with initial data
	doS3Request(srv, tnt, "PUT",
		fmt.Sprintf("/test-bucket/overwrite.txt?uploadId=%s&partNumber=1", uploadID),
		bytes.NewReader([]byte("original")))

	// Overwrite part 1 with new data
	part1W := doS3Request(srv, tnt, "PUT",
		fmt.Sprintf("/test-bucket/overwrite.txt?uploadId=%s&partNumber=1", uploadID),
		bytes.NewReader([]byte("updated")))
	require.Equal(t, http.StatusOK, part1W.Code)
	etag1 := part1W.Header().Get("ETag")

	// Complete with the overwritten part
	completeBody := fmt.Sprintf(`<CompleteMultipartUpload>
		<Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part>
	</CompleteMultipartUpload>`, etag1)

	completeW := doS3Request(srv, tnt, "POST",
		fmt.Sprintf("/test-bucket/overwrite.txt?uploadId=%s", uploadID),
		strings.NewReader(completeBody))
	require.Equal(t, http.StatusOK, completeW.Code)

	// Verify the object has the updated content
	getW := doS3Request(srv, tnt, "GET", "/test-bucket/overwrite.txt", nil)
	require.Equal(t, http.StatusOK, getW.Code)
	assert.Equal(t, "updated", getW.Body.String())
}

func TestMultipart_ETag_S3Compatible(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)

	// Arrange
	initW := doS3Request(srv, tnt, "POST", "/test-bucket/etag-test.bin?uploads", nil)
	require.Equal(t, http.StatusOK, initW.Code)
	var initResult InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(initW.Body.Bytes(), &initResult))
	uploadID := initResult.UploadID

	// Upload 2 parts
	doS3Request(srv, tnt, "PUT",
		fmt.Sprintf("/test-bucket/etag-test.bin?uploadId=%s&partNumber=1", uploadID),
		bytes.NewReader([]byte("aaaa")))
	doS3Request(srv, tnt, "PUT",
		fmt.Sprintf("/test-bucket/etag-test.bin?uploadId=%s&partNumber=2", uploadID),
		bytes.NewReader([]byte("bbbb")))

	// Complete without explicit part list
	completeW := doS3Request(srv, tnt, "POST",
		fmt.Sprintf("/test-bucket/etag-test.bin?uploadId=%s", uploadID),
		nil)
	require.Equal(t, http.StatusOK, completeW.Code)

	var result CompleteMultipartUploadResult
	require.NoError(t, xml.Unmarshal(completeW.Body.Bytes(), &result))

	// S3 multipart ETag format: "hex-N" where N is part count
	assert.True(t, strings.HasPrefix(result.ETag, "\""), "ETag should start with quote")
	assert.True(t, strings.HasSuffix(result.ETag, "\""), "ETag should end with quote")
	assert.Contains(t, result.ETag, "-2", "ETag should contain -2 for 2 parts")
}
