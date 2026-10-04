package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/engine"
)

// countingRangeDriver wraps the fixture's local driver and counts whole-object
// Gets against native range Gets.
type countingRangeDriver struct {
	engine.Driver
	gets, ranges atomic.Int32
}

func (c *countingRangeDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	c.gets.Add(1)
	return c.Driver.Get(ctx, container, artifact)
}

// GetRange stands in for a backend-native range read (the local driver has
// none): it opens the inner driver directly, so the wrapper's Get counter only
// ever counts the handler's own whole-object reads.
func (c *countingRangeDriver) GetRange(ctx context.Context, container, artifact string, offset, length int64) (io.ReadCloser, error) {
	c.ranges.Add(1)
	rc, err := c.Driver.Get(ctx, container, artifact)
	if err != nil {
		return nil, err
	}
	if _, err := io.CopyN(io.Discard, rc, offset); err != nil {
		_ = rc.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(rc, length), rc}, nil
}

// A ranged read of a cached, unencrypted whole object must not open the full
// object first. The 2026-10-04 bench measured every 1 MB range read paying a
// whole-object GET's first byte (~200 ms) on top of the range itself, because
// the handler opened engine.Get and abandoned it once the Range branch used
// GetRange. Now the native range is the only backend call.
func TestHandleGet_Range_UsesNativeRangeOnly(t *testing.T) {
	f := setupAdapterFixture(t)
	local, ok := f.eng.GetDriver("local")
	require.True(t, ok)
	counting := &countingRangeDriver{Driver: local}
	f.eng.AddDriver("local", counting)

	body := ssePlaintext()
	req := httptest.NewRequest("PUT", "/test-bucket/plain.bin", bytes.NewReader(body))
	req = req.WithContext(s3Ctx(req.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.adapter.HandlePut(w, req, "test-bucket", "plain.bin")
	require.Equal(t, http.StatusOK, w.Code)
	counting.gets.Store(0)
	counting.ranges.Store(0)

	w = rangeGet(t, f, "plain.bin", "bytes=1000-1999", nil)
	require.Equal(t, http.StatusPartialContent, w.Code)
	assert.Equal(t, body[1000:2000], w.Body.Bytes())
	assert.Equal(t, "bytes 1000-1999/4096", w.Header().Get("Content-Range"))
	assert.Equal(t, int32(1), counting.ranges.Load(), "one native range read")
	assert.Equal(t, int32(0), counting.gets.Load(), "no whole-object GET for a ranged read")

	// A whole read still uses Get exactly once.
	w = rangeGet(t, f, "plain.bin", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, int32(1), counting.gets.Load())
	assert.Equal(t, int32(1), counting.ranges.Load())

	// An unsatisfiable range answers 416 without touching the backend.
	w = rangeGet(t, f, "plain.bin", "bytes=99999-", nil)
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, w.Code)
	assert.Equal(t, int32(1), counting.gets.Load())
	assert.Equal(t, int32(1), counting.ranges.Load())
}
