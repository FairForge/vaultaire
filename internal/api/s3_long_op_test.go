package api

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slowDriver is a backend whose writes and deletes take `delay` — Sync's
// bridges take ~30 MB/s per object and ~1 s per delete. failPut makes the
// slow Put fail after the delay.
type slowDriver struct {
	engine.Driver
	delay     time.Duration
	failPut   bool
	deletes   atomic.Int32
	inFlight  atomic.Int32
	maxFlight atomic.Int32
}

func (d *slowDriver) Put(ctx context.Context, c, a string, r io.Reader, opts ...engine.PutOption) error {
	time.Sleep(d.delay)
	if d.failPut {
		_, _ = io.Copy(io.Discard, r)
		return errors.New("slow backend: write refused")
	}
	return d.Driver.Put(ctx, c, a, r, opts...)
}

func (d *slowDriver) Delete(ctx context.Context, c, a string) error {
	n := d.inFlight.Add(1)
	for {
		m := d.maxFlight.Load()
		if n <= m || d.maxFlight.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(d.delay)
	d.inFlight.Add(-1)
	d.deletes.Add(1)
	return d.Driver.Delete(ctx, c, a)
}

// newSlowMultipartServer is newTestMultipartServer with the primary wrapped
// in a slowDriver and the keep-alive timings shortened.
func newSlowMultipartServer(t *testing.T, delay time.Duration) (*Server, *tenant.Tenant, *slowDriver) {
	t.Helper()
	srv, tnt, _ := newTestMultipartServer(t)
	local, ok := srv.engine.GetDriver("local")
	require.True(t, ok)
	slow := &slowDriver{Driver: local, delay: delay}
	srv.engine.AddDriver("local", slow)
	srv.engine.SetPrimary("local")
	srv.longOpThreshold = 50 * time.Millisecond
	srv.longOpInterval = 20 * time.Millisecond
	return srv, tnt, slow
}

// uploadTwoParts initiates an upload of key and sends two parts in-process;
// it returns the upload id and the CompleteMultipartUpload body.
func uploadTwoParts(t *testing.T, srv *Server, tnt *tenant.Tenant, key string) (string, string) {
	t.Helper()
	initW := doS3Request(srv, tnt, "POST", "/test-bucket/"+key+"?uploads", nil)
	require.Equal(t, http.StatusOK, initW.Code)
	var init InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(initW.Body.Bytes(), &init))
	var parts strings.Builder
	for i, data := range []string{"hello, ", "slow world"} {
		pw := doS3Request(srv, tnt, "PUT", fmt.Sprintf("/test-bucket/%s?uploadId=%s&partNumber=%d", key, init.UploadID, i+1), strings.NewReader(data))
		require.Equal(t, http.StatusOK, pw.Code)
		fmt.Fprintf(&parts, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", i+1, pw.Header().Get("ETag"))
	}
	return init.UploadID, "<CompleteMultipartUpload>" + parts.String() + "</CompleteMultipartUpload>"
}

// sdkAgainst serves srv over HTTP (tenant injected, as the auth middleware
// would) and returns an official aws-sdk-go-v2 client pointed at it.
func sdkAgainst(t *testing.T, srv *Server, tnt *tenant.Tenant) *s3.Client {
	t.Helper()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.handleS3Request(w, r.WithContext(s3Ctx(r.Context(), tnt)))
	}))
	t.Cleanup(hs.Close)
	return s3.New(s3.Options{
		BaseEndpoint:     aws.String(hs.URL),
		Region:           "us-east-1",
		UsePathStyle:     true,
		Credentials:      credentials.NewStaticCredentialsProvider("AK", "SK", ""),
		RetryMaxAttempts: 1,
	})
}

func completeParts(body string) *s3types.CompletedMultipartUpload {
	var req CompleteMultipartUploadRequest
	if err := xml.Unmarshal([]byte(body), &req); err != nil {
		panic(err)
	}
	out := &s3types.CompletedMultipartUpload{}
	for _, p := range req.Parts {
		out.Parts = append(out.Parts, s3types.CompletedPart{PartNumber: aws.Int32(int32(p.PartNumber)), ETag: aws.String(p.ETag)})
	}
	return out
}

// --- the helper itself -------------------------------------------------------

func TestRunLongS3Op_FastOperationKeepsItsStatus(t *testing.T) {
	// Arrange
	srv := &Server{longOpThreshold: time.Second, longOpInterval: time.Second}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/b/k?uploadId=x", nil)

	// Act
	srv.runLongS3Op(w, r, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"abc"`)
		WriteS3Error(w, ErrNoSuchUpload, "/b/k", "req-1")
	})

	// Assert: exactly what the operation wrote
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, `"abc"`, w.Header().Get("ETag"))
	assert.True(t, strings.HasPrefix(w.Body.String(), "<?xml"))
	assert.Contains(t, w.Body.String(), "<Code>NoSuchUpload</Code>")
}

func TestRunLongS3Op_SlowOperationCommits200WithWhitespaceThenTheResult(t *testing.T) {
	// Arrange
	srv := &Server{longOpThreshold: 30 * time.Millisecond, longOpInterval: 10 * time.Millisecond}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/b/k?uploadId=x", nil)

	// Act
	srv.runLongS3Op(w, r, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(xml.Header + "<CompleteMultipartUploadResult><ETag>\"e-2\"</ETag></CompleteMultipartUploadResult>"))
	})

	// Assert
	body := w.Body.String()
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/xml", w.Header().Get("Content-Type"))
	trimmed := strings.TrimLeft(body, " ")
	assert.Greater(t, len(body)-len(trimmed), 1, "keep-alive spaces before the document")
	assert.True(t, strings.HasPrefix(trimmed, "<CompleteMultipartUploadResult>"), "no XML declaration after whitespace: %q", trimmed)
	var res CompleteMultipartUploadResult
	require.NoError(t, xml.Unmarshal([]byte(body), &res))
	assert.Equal(t, `"e-2"`, res.ETag)
}

func TestRunLongS3Op_SlowFailureIsAnErrorDocumentIn200(t *testing.T) {
	// Arrange
	srv := &Server{longOpThreshold: 30 * time.Millisecond, longOpInterval: 10 * time.Millisecond}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/b/k?uploadId=x", nil)

	// Act
	srv.runLongS3Op(w, r, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		WriteS3Error(w, ErrServiceUnavailable, "/b/k", "req-2")
	})

	// Assert
	assert.Equal(t, http.StatusOK, w.Code)
	var e S3Error
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &e))
	assert.Equal(t, ErrServiceUnavailable, e.Code)
	assert.Equal(t, "req-2", e.RequestID)
}

func TestRunLongS3Op_APanicIsAnInternalErrorNotACrash(t *testing.T) {
	srv := &Server{longOpThreshold: time.Second, longOpInterval: time.Second}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/b?delete", nil)

	srv.runLongS3Op(w, r, func(http.ResponseWriter, *http.Request) { panic("boom") })

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "<Code>InternalError</Code>")
}

func TestRunLongS3Op_OperationContextOutlivesTheClient(t *testing.T) {
	// Arrange: the client's context is cancelled while the operation runs
	srv := &Server{longOpThreshold: 20 * time.Millisecond, longOpInterval: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/b/k?uploadId=x", nil).WithContext(ctx)
	var opErr error
	go func() { time.Sleep(40 * time.Millisecond); cancel() }()

	// Act
	srv.runLongS3Op(httptest.NewRecorder(), r, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		opErr = r.Context().Err()
		w.WriteHeader(http.StatusOK)
	})

	// Assert
	assert.NoError(t, opErr, "the backend write must not be cancelled by the client going away")
}

// --- CompleteMultipartUpload through the official SDK -------------------------

func TestCompleteMultipartUpload_SlowBackendKeepsTheSDKClientAttached(t *testing.T) {
	// Arrange
	srv, tnt, _ := newSlowMultipartServer(t, 250*time.Millisecond)
	uploadID, body := uploadTwoParts(t, srv, tnt, "slow.bin")
	client := sdkAgainst(t, srv, tnt)

	// Act
	out, err := client.CompleteMultipartUpload(context.Background(), &s3.CompleteMultipartUploadInput{
		Bucket: aws.String("test-bucket"), Key: aws.String("slow.bin"), UploadId: aws.String(uploadID),
		MultipartUpload: completeParts(body),
	})

	// Assert
	require.NoError(t, err)
	assert.Contains(t, aws.ToString(out.ETag), "-2")
	getW := doS3Request(srv, tnt, "GET", "/test-bucket/slow.bin", nil)
	require.Equal(t, http.StatusOK, getW.Code)
	assert.Equal(t, "hello, slow world", getW.Body.String())
}

func TestCompleteMultipartUpload_SlowBackendFailureReachesTheSDKAsAnError(t *testing.T) {
	// Arrange
	srv, tnt, slow := newSlowMultipartServer(t, 250*time.Millisecond)
	uploadID, body := uploadTwoParts(t, srv, tnt, "fails.bin")
	slow.failPut = true
	client := sdkAgainst(t, srv, tnt)

	// Act
	_, err := client.CompleteMultipartUpload(context.Background(), &s3.CompleteMultipartUploadInput{
		Bucket: aws.String("test-bucket"), Key: aws.String("fails.bin"), UploadId: aws.String(uploadID),
		MultipartUpload: completeParts(body),
	})

	// Assert: a 200 carrying <Error> is an error to the SDK, never a success
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ServiceUnavailable", "the operation's own error code, from the body of the 200")
	getW := doS3Request(srv, tnt, "GET", "/test-bucket/fails.bin", nil)
	assert.Equal(t, http.StatusNotFound, getW.Code)
}

func TestCompleteMultipartUpload_ClientGoneAfterTheCommitStillCompletes(t *testing.T) {
	// Arrange
	srv, tnt, _ := newSlowMultipartServer(t, 300*time.Millisecond)
	uploadID, body := uploadTwoParts(t, srv, tnt, "gone.bin")
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/test-bucket/gone.bin?uploadId="+uploadID, strings.NewReader(body))
	r = r.WithContext(s3Ctx(ctx, tnt))
	go func() { time.Sleep(100 * time.Millisecond); cancel() }() // after the 50 ms commit

	// Act
	w := httptest.NewRecorder()
	srv.handleS3Request(w, r)

	// Assert: the object exists, whole, and the upload is completed
	getW := doS3Request(srv, tnt, "GET", "/test-bucket/gone.bin", nil)
	require.Equal(t, http.StatusOK, getW.Code)
	assert.Equal(t, "hello, slow world", getW.Body.String())
	memUploadsMu.RLock()
	assert.Equal(t, "completed", memUploads[uploadID].Status)
	memUploadsMu.RUnlock()
}

// --- DeleteObjects ------------------------------------------------------------

func TestDeleteObjects_DeletesKeysInParallelAndAnswersInRequestOrder(t *testing.T) {
	// Arrange: 32 objects; every delete takes 100 ms
	srv, tnt, slow := newSlowMultipartServer(t, 0)
	var keys []string
	for i := 0; i < 32; i++ {
		k := fmt.Sprintf("k%02d", 31-i) // request order is not sorted order
		keys = append(keys, k)
		require.Equal(t, http.StatusOK, doS3Request(srv, tnt, "PUT", "/test-bucket/"+k, strings.NewReader("x")).Code)
	}
	slow.delay = 100 * time.Millisecond
	var b strings.Builder
	b.WriteString("<Delete>")
	for _, k := range keys {
		fmt.Fprintf(&b, "<Object><Key>%s</Key></Object>", k)
	}
	b.WriteString("<Object><Key>k05</Key></Object></Delete>") // a duplicate key

	// Act
	start := time.Now()
	w := doS3Request(srv, tnt, "POST", "/test-bucket?delete", strings.NewReader(b.String()))
	took := time.Since(start)

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res DeleteResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res))
	require.Empty(t, res.Errors)
	require.Len(t, res.Deleted, 33)
	for i, k := range keys {
		assert.Equal(t, k, res.Deleted[i].Key, "request order")
	}
	assert.Equal(t, "k05", res.Deleted[32].Key)
	assert.Less(t, took, 1500*time.Millisecond, "32 × 100 ms deletes must run in parallel (sequential = 3.2 s)")
	assert.Greater(t, slow.maxFlight.Load(), int32(4))
	assert.LessOrEqual(t, slow.maxFlight.Load(), int32(batchDeleteConcurrency))
	assert.Equal(t, int32(32), slow.deletes.Load(), "a duplicate key is deleted once")
	for _, k := range keys {
		assert.Equal(t, http.StatusNotFound, doS3Request(srv, tnt, "GET", "/test-bucket/"+k, nil).Code)
	}
}

func TestDeleteObjects_SlowBatchKeepsTheSDKClientAttached(t *testing.T) {
	// Arrange: 20 deletes × 200 ms at 16-way = 2 rounds ≈ 400 ms > the 50 ms threshold
	srv, tnt, slow := newSlowMultipartServer(t, 0)
	var ids []s3types.ObjectIdentifier
	for i := 0; i < 20; i++ {
		k := fmt.Sprintf("s%02d", i)
		require.Equal(t, http.StatusOK, doS3Request(srv, tnt, "PUT", "/test-bucket/"+k, strings.NewReader("x")).Code)
		ids = append(ids, s3types.ObjectIdentifier{Key: aws.String(k)})
	}
	slow.delay = 200 * time.Millisecond
	client := sdkAgainst(t, srv, tnt)

	// Act
	out, err := client.DeleteObjects(context.Background(), &s3.DeleteObjectsInput{
		Bucket: aws.String("test-bucket"), Delete: &s3types.Delete{Objects: ids},
	})

	// Assert
	require.NoError(t, err)
	assert.Len(t, out.Deleted, 20)
	assert.Empty(t, out.Errors)
}
