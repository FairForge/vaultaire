package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/engine"
)

// exactLengthDriver writes the way the sized writers do — the S3-class
// single part (≤ 16 MiB, putSinglePartIfSmall) and a stripe's pieces: it
// reads exactly ContentLength bytes and never reads on. The copy fixture's
// local driver reads to EOF, which is why a source longer than its row was
// never seen there.
type exactLengthDriver struct{ engine.Driver }

func (d *exactLengthDriver) Put(ctx context.Context, c, a string, r io.Reader, opts ...engine.PutOption) error {
	n := engine.ApplyPutOptions(opts...).ContentLength
	if n <= 0 {
		return d.Driver.Put(ctx, c, a, r, opts...)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return d.Driver.Put(ctx, c, a, bytes.NewReader(buf), opts...)
}

func sizedCopyRequest(f *adapterTestFixture, srv *Server, src, dst string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("PUT", "/test-bucket/"+dst, nil)
	r.Header.Set("x-amz-copy-source", "/test-bucket/"+src)
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	srv.handleS3Request(w, r)
	return w
}

// Prompt 2b.3 D2.1: a source LONGER than its head row's size was silently
// truncated by a sized copy — the writer read exactly the recorded size and
// stopped, so exactSizeReader never saw the excess: the copy stored a
// prefix under its own (self-consistent) MD5 and answered 200. Before
// (9bc9d1e): 200, dst = "0123456789", size 10.
func TestCopyObject_ASourceLongerThanItsRecordedSizeIsA503AndStoresNothing(t *testing.T) {
	// Arrange: a 20-byte source whose row says 10.
	f, srv, spy := spyFixture(t)
	f.eng.AddDriver("local", &exactLengthDriver{Driver: spy})
	putW := doS3Request(srv, f.tenant, "PUT", "/test-bucket/long.bin", strings.NewReader("0123456789abcdefghij"))
	require.Equal(t, http.StatusOK, putW.Code)
	_, err := f.db.Exec(`UPDATE object_head_cache SET size_bytes = 10 WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'long.bin'`, f.tenantID)
	require.NoError(t, err)
	before := testutil.ToFloat64(copySourceSizeMismatch)

	// Act
	w := sizedCopyRequest(f, srv, "long.bin", "long-copy.bin")

	// Assert: a retryable 503 (the long-op <Error> document: the keep-alive
	// has committed the status here), no object, counted.
	assert.Contains(t, w.Body.String(), "<Code>ServiceUnavailable</Code>", w.Body.String())
	assert.NotContains(t, w.Body.String(), "<CopyObjectResult>")
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'long-copy.bin'`, f.tenantID).Scan(&n))
	assert.Zero(t, n, "no head row for a prefix of the source")
	assert.Equal(t, before+1, testutil.ToFloat64(copySourceSizeMismatch))

	// The row repaired: the copy succeeds, whole.
	_, err = f.db.Exec(`UPDATE object_head_cache SET size_bytes = 20 WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'long.bin'`, f.tenantID)
	require.NoError(t, err)
	w = sizedCopyRequest(f, srv, "long.bin", "long-copy.bin")
	require.Contains(t, w.Body.String(), "<CopyObjectResult>", w.Body.String())
	getW := doS3Request(srv, f.tenant, "GET", "/test-bucket/long-copy.bin", nil)
	assert.Equal(t, "0123456789abcdefghij", getW.Body.String())
}

// The short case through the same exact-length writer: still refused, now
// as the same 503.
func TestCopyObject_ASourceShorterThanItsRecordedSizeIsA503WithAnExactWriter(t *testing.T) {
	f, srv, spy := spyFixture(t)
	f.eng.AddDriver("local", &exactLengthDriver{Driver: spy})
	putW := doS3Request(srv, f.tenant, "PUT", "/test-bucket/short.bin", strings.NewReader("0123456789"))
	require.Equal(t, http.StatusOK, putW.Code)
	_, err := f.db.Exec(`UPDATE object_head_cache SET size_bytes = 20 WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'short.bin'`, f.tenantID)
	require.NoError(t, err)

	w := sizedCopyRequest(f, srv, "short.bin", "short-copy.bin")

	assert.Contains(t, w.Body.String(), "<Code>ServiceUnavailable</Code>", w.Body.String())
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = 'short-copy.bin'`, f.tenantID).Scan(&n))
	assert.Zero(t, n)
}
