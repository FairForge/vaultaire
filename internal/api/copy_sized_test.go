package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/engine"
)

// Prompt 2b.2 C3: CopyObject read its source with no size and wrote the
// destination with no length. On the multi-bridge `sync` backend a key can
// hold two versions after an interrupted commit (a plain file and a striped
// manifest) and only the recorded size tells them apart: the copy took the
// stale plain bytes and recorded their MD5 as the new object (repro:
// committed 49,152 B, read 100 B). And a destination write of unknown
// length is never striped: one plain file on one bridge at ~30 MB/s
// (~28 min for 50 GiB), breaking the rule that a plain file is always below
// the stripe minimum. Before (2b8adc3): the read carried no expected size,
// the write no length; the second range attempt of GET no expected size.

// sizeSpy wraps a driver and records what each call was told about sizes.
type sizeSpy struct {
	engine.Driver
	mu          sync.Mutex
	getExpected []int64 // -1 = none
	putLengths  []int64
}

func (s *sizeSpy) Get(ctx context.Context, c, a string) (io.ReadCloser, error) {
	s.note(ctx)
	return s.Driver.Get(ctx, c, a)
}

func (s *sizeSpy) GetRange(ctx context.Context, c, a string, off, n int64) (io.ReadCloser, error) {
	s.note(ctx)
	if rg, ok := s.Driver.(engine.RangeGetter); ok {
		return rg.GetRange(ctx, c, a, off, n)
	}
	return nil, io.ErrUnexpectedEOF
}

func (s *sizeSpy) note(ctx context.Context) {
	n, ok := engine.ExpectedSize(ctx)
	if !ok {
		n = -1
	}
	s.mu.Lock()
	s.getExpected = append(s.getExpected, n)
	s.mu.Unlock()
}

func (s *sizeSpy) Put(ctx context.Context, c, a string, r io.Reader, opts ...engine.PutOption) error {
	s.mu.Lock()
	s.putLengths = append(s.putLengths, engine.ApplyPutOptions(opts...).ContentLength)
	s.mu.Unlock()
	return s.Driver.Put(ctx, c, a, r, opts...)
}

func (s *sizeSpy) reset() {
	s.mu.Lock()
	s.getExpected, s.putLengths = nil, nil
	s.mu.Unlock()
}

func spyFixture(t *testing.T) (*adapterTestFixture, *Server, *sizeSpy) {
	t.Helper()
	f := setupAdapterFixture(t)
	local, ok := f.eng.GetDriver("local")
	require.True(t, ok)
	spy := &sizeSpy{Driver: local}
	f.eng.AddDriver("local", spy)
	f.eng.SetPrimary("local")
	srv := &Server{logger: f.adapter.logger, router: chi.NewRouter(), engine: f.eng, db: f.db, testMode: true}
	instantCommit(srv)
	return f, srv, spy
}

func TestCopyObject_ReadsAndWritesWithTheSourcesRecordedSize(t *testing.T) {
	// Arrange
	f, srv, spy := spyFixture(t)
	body := strings.Repeat("s", 49152)
	putW := doS3Request(srv, f.tenant, "PUT", "/test-bucket/src.bin", strings.NewReader(body))
	require.Equal(t, http.StatusOK, putW.Code, putW.Body.String())
	spy.reset()

	// Act
	r := httptest.NewRequest("PUT", "/test-bucket/dst.bin", nil)
	r.Header.Set("x-amz-copy-source", "/test-bucket/src.bin")
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	srv.handleS3Request(w, r)

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []int64{49152}, spy.getExpected, "the source is read as the committed version of its recorded size")
	assert.Equal(t, []int64{49152}, spy.putLengths, "the destination is written with its length (a large copy onto sync stripes)")
	getW := doS3Request(srv, f.tenant, "GET", "/test-bucket/dst.bin", nil)
	assert.True(t, bytes.Equal([]byte(body), getW.Body.Bytes()))
}

// A source whose bytes are not its recorded size: the copy fails, it never
// records other bytes under the source's identity.
func TestCopyObject_ASourceShorterThanItsRecordedSizeFails(t *testing.T) {
	f, srv, _ := spyFixture(t)
	putW := doS3Request(srv, f.tenant, "PUT", "/test-bucket/drift.bin", strings.NewReader("0123456789"))
	require.Equal(t, http.StatusOK, putW.Code)
	_, err := f.db.Exec(`UPDATE object_head_cache SET size_bytes = 20 WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'drift.bin'`, f.tenantID)
	require.NoError(t, err)

	r := httptest.NewRequest("PUT", "/test-bucket/drift-copy.bin", nil)
	r.Header.Set("x-amz-copy-source", "/test-bucket/drift.bin")
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	srv.handleS3Request(w, r)

	// The keep-alive (instant here) has committed the 200: the failure is
	// the <Error> document of the long-op contract.
	assert.Contains(t, w.Body.String(), "<Error>")
	assert.NotContains(t, w.Body.String(), "<CopyObjectResult>")
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'drift-copy.bin'`, f.tenantID).Scan(&n))
	assert.Zero(t, n, "no head row for bytes that are not the source")
}

// GET's second range attempt (when the first native range could not be
// opened) carries the recorded size too.
func TestGetObject_TheSecondRangeAttemptCarriesTheRecordedSize(t *testing.T) {
	f, srv, spy := spyFixture(t)
	putW := doS3Request(srv, f.tenant, "PUT", "/test-bucket/ranged.bin", strings.NewReader(strings.Repeat("r", 4096)))
	require.Equal(t, http.StatusOK, putW.Code)
	spy.reset()

	r := httptest.NewRequest("GET", "/test-bucket/ranged.bin", nil)
	r.Header.Set("Range", "bytes=10-19")
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	srv.handleS3Request(w, r)

	require.Equal(t, http.StatusPartialContent, w.Code)
	for i, n := range spy.getExpected {
		assert.Equal(t, int64(4096), n, "read %d", i)
	}
}
