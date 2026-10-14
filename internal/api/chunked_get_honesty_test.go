package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Stage 1a Part A: the chunked GET tells the truth. A chunk backend that
// cannot be asked is a 503 before any header whichever chunk it holds, and
// a failure after the status line cuts the connection instead of ending a
// short body cleanly.

// chunkedObjectChunks returns the object's chunk rows in chunk order.
func chunkedObjectChunks(t *testing.T, f *adapterTestFixture, key string) []honestChunk {
	t.Helper()
	rows, err := f.db.Query(`
		SELECT r.dedup_scope, r.plaintext_hash, COALESCE(g.storage_key, '')
		FROM tenant_chunk_refs r
		JOIN global_content_index g ON g.dedup_scope = r.dedup_scope AND g.plaintext_hash = r.plaintext_hash
		WHERE r.tenant_id = $1 AND r.bucket_name = 'test-bucket' AND r.object_key = `+manifestOfKeySQL+`
		ORDER BY r.chunk_index`, f.tenantID, key)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []honestChunk
	for rows.Next() {
		var c honestChunk
		require.NoError(t, rows.Scan(&c.scope, &c.hash, &c.storageKey))
		out = append(out, c)
	}
	require.NoError(t, rows.Err())
	return out
}

type honestChunk struct{ scope, hash, storageKey string }

// rehomeChunk points one chunk's index row at another backend and drops the
// cached copy of the row, as the chunk move does.
func rehomeChunk(t *testing.T, f *adapterTestFixture, c honestChunk, backend string) {
	t.Helper()
	_, err := f.db.Exec(`UPDATE global_content_index SET backend_id = $3 WHERE dedup_scope = $1 AND plaintext_hash = $2`,
		c.scope, c.hash, backend)
	require.NoError(t, err)
	f.adapter.gci.InvalidateCache(c.scope, c.hash)
}

// putMultiChunk stores an object of at least two chunks.
func putMultiChunk(t *testing.T, f *adapterTestFixture, key string) ([]byte, []honestChunk) {
	t.Helper()
	content := generateTestData(20 << 20) // > the 16 MiB max chunk: two chunks or more
	putChunkedObject(t, f, key, content, "application/octet-stream")
	chunks := chunkedObjectChunks(t, f, key)
	require.GreaterOrEqual(t, len(chunks), 2, "the object must span two chunks")
	return content, chunks
}

func TestChunkedGet_LaterChunkOnDeadBackend_Is503BeforeHeaders(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *adapterTestFixture) string
	}{
		{"breaker open", func(t *testing.T, f *adapterTestFixture) string {
			dead := &fixedBucketDriver{dir: t.TempDir()}
			dead.failGet.Store(true)
			f.eng.AddDriver("dead", dead)
			openBreaker(t, f.eng, "dead", f.tenantID)
			return "dead"
		}},
		{"unregistered name", func(t *testing.T, f *adapterTestFixture) string {
			return "onedrive" // a name prod's rows carried after the fleet was renamed
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setupChunkingFixture(t)
			_, chunks := putMultiChunk(t, f, "later.bin")
			backend := tc.setup(t, f)
			rehomeChunk(t, f, chunks[len(chunks)-1], backend)

			w := getChunkedAs(t, f, f.tenant, "test-bucket", "later.bin")

			t.Logf("GET with chunk %d of %d on %q: %d, %d body bytes, Retry-After=%q",
				len(chunks), len(chunks), backend, w.Code, w.Body.Len(), w.Header().Get("Retry-After"))
			assert.Equal(t, http.StatusServiceUnavailable, w.Code, "no 200 for a body that cannot be finished")
			assert.Equal(t, "30", w.Header().Get("Retry-After"))
			assert.Empty(t, w.Header().Get("ETag"), "no identity header before the refusal")
			assert.True(t, strings.Contains(w.Body.String(), "ServiceUnavailable"), "an S3 ServiceUnavailable error body")
		})
	}
}

func TestChunkedGet_UnregisteredChunkBackend_Is503NotInternalError(t *testing.T) {
	f := setupChunkingFixture(t)
	putChunkedObject(t, f, "one.bin", generateTestData(8<<10), "application/octet-stream")
	chunks := chunkedObjectChunks(t, f, "one.bin")
	require.Len(t, chunks, 1)
	rehomeChunk(t, f, chunks[0], "onedrive")

	w := getChunkedAs(t, f, f.tenant, "test-bucket", "one.bin")

	t.Logf("GET with its only chunk on an unregistered backend: %d %s", w.Code, w.Body.String())
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "the blob may well exist: a backend we cannot ask is 503, not 500")
	assert.Equal(t, "30", w.Header().Get("Retry-After"))
}

// dropContentLength removes Content-Length from the response, as a proxy
// that re-chunks a body does: the client then has nothing to count against.
type dropContentLength struct{ http.ResponseWriter }

func (d dropContentLength) WriteHeader(code int) {
	d.ResponseWriter.Header().Del("Content-Length")
	d.ResponseWriter.WriteHeader(code)
}

func (d dropContentLength) Write(b []byte) (int, error) {
	d.ResponseWriter.Header().Del("Content-Length")
	return d.ResponseWriter.Write(b)
}

func TestChunkedGet_MidStreamFailureCutsTheConnection(t *testing.T) {
	f := setupChunkingFixture(t)
	content, chunks := putMultiChunk(t, f, "cut.bin")
	// The last chunk's bytes are wrong on the backend: the preflight cannot
	// see that (no read per chunk), so the status line goes out first.
	last := chunks[len(chunks)-1]
	require.NoError(t, os.WriteFile(f.fixed.path(engine.ChunkAddressTenant, chunkContainer, last.storageKey),
		[]byte("not the chunk"), 0o600))

	srv := &Server{logger: zap.NewNop()}
	handler := func(wrap func(http.ResponseWriter) http.ResponseWriter) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(s3Ctx(r.Context(), f.tenant))
			f.adapter.HandleGet(wrap(w), r, "test-bucket", "cut.bin")
		})
	}
	same := func(w http.ResponseWriter) http.ResponseWriter { return w }
	noLength := func(w http.ResponseWriter) http.ResponseWriter { return dropContentLength{w} }

	cases := []struct {
		name    string
		handler http.Handler
		http2   bool
		rng     string
	}{
		{"http1 full", handler(same), false, ""},
		{"http1 no content-length", handler(noLength), false, ""},
		{"http1 range", handler(same), false, "bytes=100-"},
		{"http2 no content-length", handler(noLength), true, ""},
		{"behind the logging middleware", srv.loggingMiddleware(handler(noLength)), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewUnstartedServer(tc.handler)
			ts.Config.ErrorLog = nil
			if tc.http2 {
				ts.EnableHTTP2 = true
				ts.StartTLS()
			} else {
				ts.Start()
			}
			defer ts.Close()

			before := testutil.ToFloat64(chunkedGetTruncated.WithLabelValues(truncatedIntegrity))
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/test-bucket/cut.bin", nil)
			require.NoError(t, err)
			if tc.rng != "" {
				req.Header.Set("Range", tc.rng)
			}
			resp, err := ts.Client().Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			body, readErr := io.ReadAll(resp.Body)

			t.Logf("%s: status %d proto %s, %d of %d bytes, read error %v", tc.name, resp.StatusCode, resp.Proto, len(body), len(content), readErr)
			require.Contains(t, []int{http.StatusOK, http.StatusPartialContent}, resp.StatusCode, "the status line went out before the bad chunk")
			require.Error(t, readErr, "a body that stops short must never end cleanly")
			assert.True(t, errors.Is(readErr, io.ErrUnexpectedEOF) || isStreamReset(readErr), "a cut, not a clean end: %v", readErr)
			assert.Less(t, len(body), len(content))
			assert.Eventually(t, func() bool {
				return testutil.ToFloat64(chunkedGetTruncated.WithLabelValues(truncatedIntegrity)) == before+1
			}, 2*time.Second, 10*time.Millisecond, "the cut is counted once")
		})
	}
}

// isStreamReset is HTTP/2's shape of a cut: the stream is reset
// (INTERNAL_ERROR), the client's read fails with a stream error.
func isStreamReset(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "stream error") || strings.Contains(err.Error(), "INTERNAL_ERROR") || strings.Contains(err.Error(), "connection reset"))
}

// The same bug on the plain path (the audit): a whole object whose backend
// read breaks mid-body. With no Content-Length to count against, the body
// used to end cleanly.
func TestPlainGet_MidBodyBackendFailureCutsTheConnection(t *testing.T) {
	f := setupChunkingFixture(t)
	f.adapter.chunkingThreshold = 1 << 30 // stored whole
	content := generateTestData(4 << 20)
	putChunkedObject(t, f, "whole.bin", content, "application/octet-stream")
	f.fixed.failReadAfter.Store(1 << 20)

	srv := &Server{logger: zap.NewNop()}
	for _, tc := range []struct {
		name string
		rng  string
	}{{"full", ""}, {"range", "bytes=10-"}} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(srv.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r = r.WithContext(s3Ctx(r.Context(), f.tenant))
				f.adapter.HandleGet(dropContentLength{w}, r, "test-bucket", "whole.bin")
			})))
			defer ts.Close()
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/test-bucket/whole.bin", nil)
			require.NoError(t, err)
			if tc.rng != "" {
				req.Header.Set("Range", tc.rng)
			}
			resp, err := ts.Client().Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			body, readErr := io.ReadAll(resp.Body)
			t.Logf("%s: status %d, %d of %d bytes, read error %v", tc.name, resp.StatusCode, len(body), len(content), readErr)
			require.Error(t, readErr, "a body that stops short must never end cleanly")
			assert.ErrorIs(t, readErr, io.ErrUnexpectedEOF)
		})
	}
}
