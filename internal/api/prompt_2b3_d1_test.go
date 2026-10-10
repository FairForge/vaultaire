package api

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/FairForge/vaultaire/internal/engine"
)

// stallingSource fails a source stream after half its bytes, the way a
// backend's GET body does when it stalls (Sync's idle watchdog).
type stallingSource struct {
	*sizeSpy
	stall bool
}

func (s *stallingSource) Get(ctx context.Context, c, a string) (io.ReadCloser, error) {
	rc, err := s.sizeSpy.Get(ctx, c, a)
	if err != nil || !s.stall {
		return rc, err
	}
	return &brokenAfter{rc: rc, left: 8}, nil
}

type brokenAfter struct {
	rc   io.ReadCloser
	left int
}

func (b *brokenAfter) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, fmt.Errorf("GET: no byte from the server for 1m0s: %w", engine.ErrTimeout)
	}
	if len(p) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= n
	return n, err
}

func (b *brokenAfter) Close() error { return b.rc.Close() }

// Prompt 2b.3 D1.6: a copy whose SOURCE stream failed (the engine's own
// Get, not the client's body) was tagged ErrCallerAborted by engine.Put —
// "the request body could not be read" — and answered 500. Before
// (9bc9d1e): InternalError.
func TestCopyObject_ASourceThatStallsIsA503(t *testing.T) {
	// Arrange
	f, srv, spy := spyFixture(t)
	src := &stallingSource{sizeSpy: spy}
	f.eng.AddDriver("local", src)
	putW := doS3Request(srv, f.tenant, "PUT", "/test-bucket/stall-src.bin", strings.NewReader(strings.Repeat("x", 64)))
	require.Equal(t, http.StatusOK, putW.Code)
	src.stall = true

	// Act
	r := httptest.NewRequest("PUT", "/test-bucket/stall-dst.bin", nil)
	r.Header.Set("x-amz-copy-source", "/test-bucket/stall-src.bin")
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	srv.handleS3Request(w, r)

	// Assert: a retryable 503 (the keep-alive has committed the status
	// here: the long-op <Error> document carries the code).
	assert.Contains(t, w.Body.String(), "<Code>ServiceUnavailable</Code>")
	assert.NotContains(t, w.Body.String(), "InternalError")
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'stall-dst.bin'`, f.tenantID).Scan(&n))
	assert.Zero(t, n)
}

// Prompt 2b.3 D1.7: the 503s of a backend that is out are Warn lines once
// per request, never an Error (with its stack trace) per key. Before
// (9bc9d1e): batchDeleteKey logged Error per key; HandleDelete's 503 logged
// nothing.
func TestUnavailableBackendLogsWarnOncePerRequestNeverError(t *testing.T) {
	f := setupAdapterFixture(t)
	core, logs := observer.New(zap.DebugLevel)
	seedOnFlaky(t, f, "noisy-0.bin")
	seedOnFlaky(t, f, "noisy-1.bin")
	f.adapter.logger = zap.New(core)
	s := &Server{logger: zap.New(core), engine: f.eng, db: f.db, testMode: true}
	req := httptest.NewRequest("POST", "/test-bucket?delete", nil)
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	for i := 0; i < 3; i++ {
		de := s.batchDeleteKey(req, f.tenant, "test-bucket", f.tenant.NamespaceContainer("test-bucket"), "noisy-0.bin", s.objectDeleteAftermath())
		require.NotNil(t, de)
	}
	assert.Zero(t, logs.FilterLevelExact(zap.ErrorLevel).Len(), "no Error per key")

	dreq := httptest.NewRequest("DELETE", "/test-bucket/noisy-1.bin", nil)
	dreq = dreq.WithContext(s3Ctx(dreq.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandleDelete(w, dreq, "test-bucket", "noisy-1.bin")
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Zero(t, logs.FilterLevelExact(zap.ErrorLevel).Len())
	assert.Equal(t, 1, logs.FilterLevelExact(zap.WarnLevel).FilterMessageSnippet("delete: the object's backend is unavailable").Len(), "one Warn for the single DELETE's 503")
}

// Prompt 2b.3 D1.8: an UploadPart whose client goes away mid-body is the
// client's request — IncompleteBody 400, logged at Info — not a 500 with an
// Error and a stack trace ("failed to write part data: read: connection
// reset by peer", 3 on prod from the 10-09 tunnel tests). Before (9bc9d1e):
// 500 InternalError.
func TestUploadPart_AClientAbortIsIncompleteBody(t *testing.T) {
	srv, tnt, _ := newTestMultipartServer(t)
	core, logs := observer.New(zap.DebugLevel)
	srv.logger = zap.New(core)
	w := doS3Request(srv, tnt, "POST", "/test-bucket/abort.bin?uploads", nil)
	require.Equal(t, http.StatusOK, w.Code)
	var init InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &init))
	t.Cleanup(func() { _ = os.RemoveAll(multipartDir(init.UploadID)) })

	w = doS3Request(srv, tnt, "PUT", "/test-bucket/abort.bin?partNumber=1&uploadId="+init.UploadID, &errAfterReader{data: []byte("PARTIAL")})

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), ErrIncompleteBody)
	assert.Zero(t, logs.FilterLevelExact(zap.ErrorLevel).Len(), "never an Error for the client's abort")
}
