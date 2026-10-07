package api

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CopyObject writes the destination through ONE backend PUT before it can
// answer: on the sync tier (~30 MB/s per object) a 2 GiB copy outlasts
// Cloudflare's 100 s and the client's disconnect aborted the write. It runs
// under the long-operation keep-alive (s3_long_op.go) like
// CompleteMultipartUpload.

// seedSource puts the source object (the slow driver makes the PUT itself
// slow; plain PUT is bounded by the client's own upload and not wrapped).
func seedSource(t *testing.T, srv *Server, tnt *tenant.Tenant, key, body string) {
	t.Helper()
	w := doS3Request(srv, tnt, "PUT", "/test-bucket/"+key, strings.NewReader(body))
	require.Equal(t, http.StatusOK, w.Code)
}

func copyRequest(ctx context.Context, tnt *tenant.Tenant, src, dst string) *http.Request {
	r := httptest.NewRequest("PUT", "/test-bucket/"+dst, nil)
	r.Header.Set("x-amz-copy-source", "/test-bucket/"+src)
	return r.WithContext(s3Ctx(ctx, tnt))
}

func TestCopyObject_FastCopyAnswersAsBefore(t *testing.T) {
	// Arrange: a backend fast enough to finish inside the threshold
	srv, tnt, slow := newSlowMultipartServer(t, 0)
	srv.longOpThreshold = 5 * time.Second
	seedSource(t, srv, tnt, "src.bin", "copy me")
	_ = slow

	// Act
	w := httptest.NewRecorder()
	srv.handleS3Request(w, copyRequest(context.Background(), tnt, "src.bin", "dst.bin"))

	// Assert: status, headers and body exactly as an unwrapped copy writes them
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/xml", w.Header().Get("Content-Type"))
	assert.NotEmpty(t, w.Header().Get("x-amz-request-id"))
	assert.True(t, strings.HasPrefix(w.Body.String(), "<?xml"), "the declaration leads the fast-path body")
	assert.Contains(t, w.Body.String(), "<CopyObjectResult>")
}

func TestCopyObject_RefusalsKeepTheirStatusOnASlowBackend(t *testing.T) {
	// Arrange: validation fails long before the threshold
	srv, tnt, _ := newSlowMultipartServer(t, 400*time.Millisecond)

	// Act: the source does not exist
	w := httptest.NewRecorder()
	srv.handleS3Request(w, copyRequest(context.Background(), tnt, "missing.bin", "dst.bin"))

	// Assert
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "NoSuchKey")
}

func TestCopyObject_SlowCopyCommitsEarlyAndKeepsTheSDKClientAttached(t *testing.T) {
	// Arrange
	srv, tnt, _ := newSlowMultipartServer(t, 400*time.Millisecond)
	seedSource(t, srv, tnt, "src.bin", "a slow copy")
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.handleS3Request(w, r.WithContext(s3Ctx(r.Context(), tnt)))
	}))
	t.Cleanup(hs.Close)

	// Act 1: raw — the status line arrives long before the 400 ms write ends
	req, err := http.NewRequest("PUT", hs.URL+"/test-bucket/raw.bin", nil)
	require.NoError(t, err)
	req.Header.Set("x-amz-copy-source", "/test-bucket/src.bin")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	headersAfter := time.Since(start)
	body, _ := io.ReadAll(bufio.NewReader(resp.Body))
	_ = resp.Body.Close()

	// Assert 1
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Less(t, headersAfter, 300*time.Millisecond, "committed at the threshold, not after the copy")
	assert.True(t, strings.HasPrefix(string(body), " "), "keep-alive whitespace before the document")
	assert.Contains(t, string(body), "<CopyObjectResult>")
	assert.NotContains(t, string(body), "<?xml", "no declaration after the whitespace")

	// Act 2: the official SDK parses the committed answer
	out, err := sdkAgainst(t, srv, tnt).CopyObject(context.Background(), &s3.CopyObjectInput{
		Bucket: aws.String("test-bucket"), Key: aws.String("sdk.bin"), CopySource: aws.String("test-bucket/src.bin"),
	})

	// Assert 2
	require.NoError(t, err)
	require.NotNil(t, out.CopyObjectResult)
	assert.NotEmpty(t, aws.ToString(out.CopyObjectResult.ETag))
	getW := doS3Request(srv, tnt, "GET", "/test-bucket/sdk.bin", nil)
	require.Equal(t, http.StatusOK, getW.Code)
	assert.Equal(t, "a slow copy", getW.Body.String())
}

func TestCopyObject_SlowFailureReachesTheSDKAsAnError(t *testing.T) {
	// Arrange
	srv, tnt, slow := newSlowMultipartServer(t, 300*time.Millisecond)
	seedSource(t, srv, tnt, "src.bin", "never lands")
	slow.failPut = true

	// Act
	_, err := sdkAgainst(t, srv, tnt).CopyObject(context.Background(), &s3.CopyObjectInput{
		Bucket: aws.String("test-bucket"), Key: aws.String("fails.bin"), CopySource: aws.String("test-bucket/src.bin"),
	})

	// Assert: a 200 carrying <Error> is an error to the SDK, never a success
	require.Error(t, err)
	getW := doS3Request(srv, tnt, "GET", "/test-bucket/fails.bin", nil)
	assert.Equal(t, http.StatusNotFound, getW.Code)
}

func TestCopyObject_ClientGoneAfterTheCommitStillCompletes(t *testing.T) {
	// Arrange
	srv, tnt, _ := newSlowMultipartServer(t, 300*time.Millisecond)
	seedSource(t, srv, tnt, "src.bin", "outlives its client")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }() // after the 50 ms commit

	// Act
	w := httptest.NewRecorder()
	srv.handleS3Request(w, copyRequest(ctx, tnt, "src.bin", "gone.bin"))

	// Assert: the destination exists, whole
	getW := doS3Request(srv, tnt, "GET", "/test-bucket/gone.bin", nil)
	require.Equal(t, http.StatusOK, getW.Code)
	assert.Equal(t, "outlives its client", getW.Body.String())
}
