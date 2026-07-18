// caching_reader_test.go — WP-2 (CR-2): cachingReader must never buffer more
// than the cache-eligibility limit in memory. Before the fix it buffered the
// ENTIRE stream and only consulted the 10 MB limit at EOF — so a 5 GB GET
// held 5 GB in RAM per concurrent reader (the OOM).
package engine

import (
	"bytes"
	"io"
	"testing"

	"github.com/FairForge/vaultaire/internal/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// zeroReader yields an endless stream of zero bytes.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func newTestTieredCache() *cache.TieredCache {
	return cache.NewTieredCache(&cache.Config{MemorySize: 64 << 20}, zap.NewNop())
}

func drainCachingReader(t *testing.T, r *cachingReader) (total int64, peakBuffered int) {
	t.Helper()
	chunk := make([]byte, 256<<10)
	for {
		n, err := r.Read(chunk)
		total += int64(n)
		if r.buffer != nil && r.buffer.Len() > peakBuffered {
			peakBuffered = r.buffer.Len()
		}
		if err == io.EOF {
			return total, peakBuffered
		}
		require.NoError(t, err)
	}
}

func TestCachingReader_LargeObjectDoesNotBufferInMemory(t *testing.T) {
	// Arrange: a 50 MB stream through the caching wrapper.
	tc := newTestTieredCache()
	r := &cachingReader{
		ReadCloser: io.NopCloser(io.LimitReader(zeroReader{}, 50<<20)),
		cache:      tc,
		key:        "big-object",
		buffer:     &bytes.Buffer{},
	}

	// Act
	total, peakBuffered := drainCachingReader(t, r)

	// Assert: the full stream reached the client, but at no point did the
	// reader retain more than the 10 MB cache-eligibility limit.
	assert.Equal(t, int64(50<<20), total, "client must receive every byte")
	assert.LessOrEqual(t, peakBuffered, 10<<20,
		"cachingReader must stop buffering once the object exceeds the cache limit")
	_, err := tc.Get("big-object")
	assert.Error(t, err, "an over-limit object must not land in the cache")
}

func TestCachingReader_SmallObjectStillCached(t *testing.T) {
	// Arrange: 1 MB of 0xAB so cache content is verifiable.
	payload := bytes.Repeat([]byte{0xAB}, 1<<20)
	tc := newTestTieredCache()
	r := &cachingReader{
		ReadCloser: io.NopCloser(bytes.NewReader(payload)),
		cache:      tc,
		key:        "small-object",
		buffer:     &bytes.Buffer{},
	}

	// Act
	total, _ := drainCachingReader(t, r)

	// Assert
	assert.Equal(t, int64(len(payload)), total)
	got, err := tc.Get("small-object")
	require.NoError(t, err, "an under-limit object must be cached at EOF")
	assert.True(t, bytes.Equal(payload, got), "cached bytes must match the stream")
}
