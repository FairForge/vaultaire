package api

import (
	"bytes"
	"encoding/xml"
	"io"
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

// setupCopyTestServer creates a test server with a local driver and returns
// the server, tenant, temp dir, and cleanup function.
func setupCopyTestServer(t *testing.T) (*Server, *tenant.Tenant, string, func()) {
	t.Helper()

	logger, _ := zap.NewDevelopment()
	eng := engine.NewEngine(nil, logger, &engine.Config{
		EnableCaching:  false,
		EnableML:       false,
		DefaultBackend: "local",
	})

	tempDir, err := os.MkdirTemp("", "vaultaire-copy-test-*")
	require.NoError(t, err)

	driver := drivers.NewLocalDriver(tempDir, logger)
	eng.AddDriver("local", driver)

	server := &Server{
		logger:   logger,
		router:   chi.NewRouter(),
		engine:   eng,
		testMode: true,
	}

	testTenant := &tenant.Tenant{
		ID:        "copy-tenant",
		Namespace: "tenant/copy-tenant/",
		APIKey:    "test-key",
	}

	cleanup := func() { _ = os.RemoveAll(tempDir) }
	return server, testTenant, tempDir, cleanup
}

// putObject is a helper that PUTs an object and asserts success.
func putObject(t *testing.T, server *Server, tnt *tenant.Tenant, tempDir, bucket, key, content string) {
	t.Helper()

	// Ensure bucket directory exists.
	bucketPath := filepath.Join(tempDir, tnt.NamespaceContainer(bucket))
	require.NoError(t, os.MkdirAll(bucketPath, 0755))

	req := httptest.NewRequest("PUT", "/"+bucket+"/"+key, bytes.NewReader([]byte(content)))
	ctx := tenant.WithTenant(req.Context(), tnt)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	server.handleS3Request(w, req)
	require.Equal(t, 200, w.Code, "PUT %s/%s should succeed: %s", bucket, key, w.Body.String())
}

// getObject is a helper that GETs an object and returns its body.
func getObject(t *testing.T, server *Server, tnt *tenant.Tenant, bucket, key string) (int, string) {
	t.Helper()

	req := httptest.NewRequest("GET", "/"+bucket+"/"+key, nil)
	ctx := tenant.WithTenant(req.Context(), tnt)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	server.handleS3Request(w, req)

	body, err := io.ReadAll(w.Body)
	require.NoError(t, err)
	return w.Code, string(body)
}

func TestCopyObject_SameBucket(t *testing.T) {
	server, tnt, tempDir, cleanup := setupCopyTestServer(t)
	defer cleanup()

	// Arrange: put source object.
	putObject(t, server, tnt, tempDir, "bucket1", "src.txt", "hello copy")

	// Act: copy src.txt → dest.txt within same bucket.
	req := httptest.NewRequest("PUT", "/bucket1/dest.txt", nil)
	req.Header.Set("x-amz-copy-source", "/bucket1/src.txt")
	ctx := tenant.WithTenant(req.Context(), tnt)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	server.handleS3Request(w, req)

	// Assert: 200 with CopyObjectResult XML.
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/xml")

	var result CopyObjectResult
	err := xml.Unmarshal(w.Body.Bytes(), &result)
	require.NoError(t, err)
	assert.NotEmpty(t, result.ETag)
	assert.NotEmpty(t, result.LastModified)

	// Verify the copied object has the same content.
	code, body := getObject(t, server, tnt, "bucket1", "dest.txt")
	assert.Equal(t, 200, code)
	assert.Equal(t, "hello copy", body)
}

func TestCopyObject_CrossBucket(t *testing.T) {
	server, tnt, tempDir, cleanup := setupCopyTestServer(t)
	defer cleanup()

	// Arrange: create source in bucket-a.
	putObject(t, server, tnt, tempDir, "bucket-a", "file.bin", "cross bucket data")

	// Create dest bucket directory.
	destBucketPath := filepath.Join(tempDir, tnt.NamespaceContainer("bucket-b"))
	require.NoError(t, os.MkdirAll(destBucketPath, 0755))

	// Act: copy bucket-a/file.bin → bucket-b/file.bin.
	req := httptest.NewRequest("PUT", "/bucket-b/file.bin", nil)
	req.Header.Set("x-amz-copy-source", "/bucket-a/file.bin")
	ctx := tenant.WithTenant(req.Context(), tnt)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	server.handleS3Request(w, req)

	// Assert.
	assert.Equal(t, 200, w.Code)

	code, body := getObject(t, server, tnt, "bucket-b", "file.bin")
	assert.Equal(t, 200, code)
	assert.Equal(t, "cross bucket data", body)
}

func TestCopyObject_NonexistentSource(t *testing.T) {
	server, tnt, tempDir, cleanup := setupCopyTestServer(t)
	defer cleanup()

	// Create dest bucket so the PUT has somewhere to go.
	bucketPath := filepath.Join(tempDir, tnt.NamespaceContainer("bucket1"))
	require.NoError(t, os.MkdirAll(bucketPath, 0755))

	// Act: copy from a key that doesn't exist.
	req := httptest.NewRequest("PUT", "/bucket1/dest.txt", nil)
	req.Header.Set("x-amz-copy-source", "/bucket1/ghost.txt")
	ctx := tenant.WithTenant(req.Context(), tnt)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	server.handleS3Request(w, req)

	// Assert: should get NoSuchKey error.
	assert.Equal(t, 404, w.Code)
	assert.Contains(t, w.Body.String(), "NoSuchKey")
}

func TestCopyObject_SelfOverwrite(t *testing.T) {
	server, tnt, tempDir, cleanup := setupCopyTestServer(t)
	defer cleanup()

	// Arrange: put an object.
	putObject(t, server, tnt, tempDir, "bucket1", "same.txt", "original content")

	// Act: copy to itself (same bucket, same key).
	req := httptest.NewRequest("PUT", "/bucket1/same.txt", nil)
	req.Header.Set("x-amz-copy-source", "/bucket1/same.txt")
	ctx := tenant.WithTenant(req.Context(), tnt)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	server.handleS3Request(w, req)

	// Assert: should succeed — S3 allows self-copy.
	assert.Equal(t, 200, w.Code)

	code, body := getObject(t, server, tnt, "bucket1", "same.txt")
	assert.Equal(t, 200, code)
	assert.Equal(t, "original content", body)
}

func TestCopyObject_ETagMatches(t *testing.T) {
	server, tnt, tempDir, cleanup := setupCopyTestServer(t)
	defer cleanup()

	// Arrange.
	putObject(t, server, tnt, tempDir, "bucket1", "src.txt", "etag test data")

	// Act.
	req := httptest.NewRequest("PUT", "/bucket1/copy.txt", nil)
	req.Header.Set("x-amz-copy-source", "/bucket1/src.txt")
	ctx := tenant.WithTenant(req.Context(), tnt)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	server.handleS3Request(w, req)

	// Assert: ETag in response should be a valid MD5 hex string.
	require.Equal(t, 200, w.Code)

	var result CopyObjectResult
	err := xml.Unmarshal(w.Body.Bytes(), &result)
	require.NoError(t, err)

	// ETag should be quoted 32-char hex.
	etag := result.ETag
	assert.True(t, len(etag) == 34, "ETag should be 34 chars (quoted md5): got %q", etag)
	assert.Equal(t, byte('"'), etag[0])
	assert.Equal(t, byte('"'), etag[len(etag)-1])
}

func TestCopyObject_InvalidCopySource(t *testing.T) {
	server, tnt, _, cleanup := setupCopyTestServer(t)
	defer cleanup()

	tests := []struct {
		name       string
		copySource string
	}{
		{"empty header", ""},
		{"just bucket", "/bucket1"},
		{"just slash", "/"},
		{"no key", "bucket1/"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("PUT", "/bucket1/dest.txt", nil)
			if tc.copySource != "" {
				req.Header.Set("x-amz-copy-source", tc.copySource)
			}
			ctx := tenant.WithTenant(req.Context(), tnt)
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()
			server.handleS3Request(w, req)

			// Without a valid copy source header, this should either be a
			// regular PutObject (empty header) or InvalidRequest.
			if tc.copySource == "" {
				// No copy-source header → falls through to PutObject path.
				// PutObject with no body succeeds with 200.
				return
			}
			assert.Equal(t, 400, w.Code, "copy source %q should fail", tc.copySource)
		})
	}
}

func TestCopyObject_CopySourceWithQueryString(t *testing.T) {
	server, tnt, tempDir, cleanup := setupCopyTestServer(t)
	defer cleanup()

	// Arrange.
	putObject(t, server, tnt, tempDir, "bucket1", "src.txt", "versioned data")

	// Act: copy source with ?versionId= (should be stripped).
	req := httptest.NewRequest("PUT", "/bucket1/dest.txt", nil)
	req.Header.Set("x-amz-copy-source", "/bucket1/src.txt?versionId=null")
	ctx := tenant.WithTenant(req.Context(), tnt)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	server.handleS3Request(w, req)

	// Assert.
	assert.Equal(t, 200, w.Code)

	code, body := getObject(t, server, tnt, "bucket1", "dest.txt")
	assert.Equal(t, 200, code)
	assert.Equal(t, "versioned data", body)
}

func TestCopyObject_NoTenant(t *testing.T) {
	server, _, _, cleanup := setupCopyTestServer(t)
	defer cleanup()

	// Act: send copy request with no tenant context.
	req := httptest.NewRequest("PUT", "/bucket1/dest.txt", nil)
	req.Header.Set("x-amz-copy-source", "/bucket1/src.txt")

	w := httptest.NewRecorder()
	// Call handleCopyObject directly since handleS3Request creates a default tenant in test mode.
	s3Req := &S3Request{Bucket: "bucket1", Object: "dest.txt", Operation: "PutObject"}
	server.handleCopyObject(w, req, s3Req)

	// Assert: should get AccessDenied.
	assert.Equal(t, 403, w.Code)
	assert.Contains(t, w.Body.String(), "AccessDenied")
}

func TestParseCopySource(t *testing.T) {
	tests := []struct {
		input      string
		wantBucket string
		wantKey    string
		wantErr    bool
	}{
		{"/bucket/key.txt", "bucket", "key.txt", false},
		{"bucket/key.txt", "bucket", "key.txt", false},
		{"/bucket/path/to/key.txt", "bucket", "path/to/key.txt", false},
		{"/bucket/key?versionId=123", "bucket", "key", false},
		// URL-decoded keys (AWS spec compliance).
		{"/bucket/foo%20bar", "bucket", "foo bar", false},
		{"/bucket/with%2Fslash", "bucket", "with/slash", false},
		{"/bucket/path/sub%20dir/file.txt", "bucket", "path/sub dir/file.txt", false},
		// Invalid percent-encoding.
		{"/bucket/bad%ZZ", "", "", true},
		{"", "", "", true},
		{"/", "", "", true},
		{"/bucket", "", "", true},
		{"/bucket/", "", "", true},
		{"bucket/", "", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			bucket, key, err := parseCopySource(tc.input)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantBucket, bucket)
				assert.Equal(t, tc.wantKey, key)
			}
		})
	}
}

func TestResolveCopyContentType(t *testing.T) {
	tests := []struct {
		name      string
		directive string
		requestCT string
		sourceCT  string
		want      string
	}{
		{"COPY preserves source", "COPY", "text/plain", "image/png", "image/png"},
		{"default (empty) preserves source", "", "text/plain", "image/png", "image/png"},
		{"COPY falls back when source empty", "COPY", "text/plain", "", "application/octet-stream"},
		{"REPLACE uses request", "REPLACE", "image/jpeg", "text/plain", "image/jpeg"},
		{"REPLACE case-insensitive", "replace", "image/jpeg", "text/plain", "image/jpeg"},
		{"REPLACE falls back when request empty", "REPLACE", "", "text/plain", "application/octet-stream"},
		{"both empty falls back", "COPY", "", "", "application/octet-stream"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveCopyContentType(tc.directive, tc.requestCT, tc.sourceCT)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCountingReader_TracksBytes(t *testing.T) {
	src := strings.NewReader("hello world")
	c := &countingReader{r: src}
	buf := make([]byte, 4)

	n, err := c.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, int64(4), c.n)

	rest, err := io.ReadAll(c)
	require.NoError(t, err)
	assert.Equal(t, "o world", string(rest))
	assert.Equal(t, int64(11), c.n, "should match total bytes of source")
}

// TestCopyObject_SelfCopy_LargeFile guards the regression that motivated
// the self-copy short-circuit removal: with the atomic LocalDriver.Put,
// self-copying a multi-MB object via the regular Get→Put streaming path
// must preserve every byte. (Pre-fix, os.Create truncated the source
// mid-read, leaving the destination empty.)
func TestCopyObject_SelfCopy_LargeFile(t *testing.T) {
	server, tnt, tempDir, cleanup := setupCopyTestServer(t)
	defer cleanup()

	// 2 MiB of repeating bytes — large enough to ensure streaming, small
	// enough to keep the test fast.
	original := strings.Repeat("ABCDEFGH", 256*1024)
	putObject(t, server, tnt, tempDir, "bucket1", "self.bin", original)

	req := httptest.NewRequest("PUT", "/bucket1/self.bin", nil)
	req.Header.Set("x-amz-copy-source", "/bucket1/self.bin")
	ctx := tenant.WithTenant(req.Context(), tnt)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	server.handleS3Request(w, req)
	require.Equal(t, 200, w.Code, "self-copy must succeed: %s", w.Body.String())

	code, body := getObject(t, server, tnt, "bucket1", "self.bin")
	require.Equal(t, 200, code)
	assert.Equal(t, len(original), len(body),
		"self-copy must preserve exact byte count")
	assert.Equal(t, original, body)
}

// TestCopyObject_URLEncodedSourceKey verifies a copy source with
// percent-encoded characters resolves to the literal key on disk.
func TestCopyObject_URLEncodedSourceKey(t *testing.T) {
	server, tnt, tempDir, cleanup := setupCopyTestServer(t)
	defer cleanup()

	// Write the source object directly so we can use a key that contains
	// a literal space (HTTP request URLs can't carry one unescaped).
	bucketPath := filepath.Join(tempDir, tnt.NamespaceContainer("bucket1"))
	require.NoError(t, os.MkdirAll(bucketPath, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(bucketPath, "my file.txt"), []byte("spaced content"), 0644))

	req := httptest.NewRequest("PUT", "/bucket1/dest.txt", nil)
	req.Header.Set("x-amz-copy-source", "/bucket1/my%20file.txt")
	ctx := tenant.WithTenant(req.Context(), tnt)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	server.handleS3Request(w, req)
	require.Equal(t, 200, w.Code, "encoded copy-source must resolve: %s", w.Body.String())

	code, body := getObject(t, server, tnt, "bucket1", "dest.txt")
	assert.Equal(t, 200, code)
	assert.Equal(t, "spaced content", body)
}
