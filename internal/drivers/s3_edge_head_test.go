package drivers

// WP-EDGE-2 Part B Part 0 (RP68-3): the control plane HEADs a sample of the
// chunks an edge host stored — HeadObject answers the backend's ETag
// (unquoted) and size at exactly the key the driver's own Put would use.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/engine"
)

func TestEdgeHeadObject_ETagAndSizeAtTheDriversKey(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/gone") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", `"0123456789ABCDEF0123456789abcdef"`)
		w.Header().Set("Content-Length", "2097338")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	d, err := NewFixedBucketS3Driver("idrive", "AKIDTEST", "secret", srv.URL, "us-central-1", "vaultaire", zap.NewNop())
	require.NoError(t, err)
	var h engine.ObjectHeader = d
	ctx := engine.ChunkContext(context.Background())

	info, err := h.HeadObject(ctx, engine.ChunkContainer, "_chunks/t1/abc.k1")
	require.NoError(t, err)
	assert.Equal(t, "0123456789abcdef0123456789abcdef", info.ETag, "unquoted, lower-case")
	assert.Equal(t, int64(2097338), info.Size)
	mu.Lock()
	assert.Contains(t, paths, "HEAD /vaultaire/t-_global/_global/_chunks/t1/abc.k1", "the driver's own key")
	mu.Unlock()

	_, err = h.HeadObject(ctx, engine.ChunkContainer, "gone")
	var nf *engine.NotFoundError
	assert.True(t, errors.As(err, &nf), "a 404 is the engine's not-found: %v", err)

	_, err = h.HeadObject(context.Background(), "c", "a")
	assert.ErrorIs(t, err, ErrNoTenant)
}
