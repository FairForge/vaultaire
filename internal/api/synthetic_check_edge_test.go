package api

// WP-LOG-2 (D-LOG-6): the canary's edge leg. The small cycle's 4 KiB never
// takes the edge path (EDGE_MIN_BYTES); every SYNTHETIC_CHECK_EDGE_EVERY the
// leg PUTs a body of SYNTHETIC_CHECK_EDGE_BYTES (4 MiB), GETs it whole —
// sha256 compared as it streams — and DELETEs it. Its failure never changes
// vaultaire_synthetic_check_success. Whether the edge took it is judged by
// the admit counter (DataPlaneEdgePathUnused), never by a response header.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// countingS3 counts requests and can corrupt an edge object's GET.
type countingS3 struct {
	*fakeS3
	n       atomic.Int64
	edge    atomic.Int64
	corrupt atomic.Bool
}

func (c *countingS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.n.Add(1)
	isEdge := strings.Contains(r.URL.Path, "/synthetic/edge-")
	if isEdge {
		c.edge.Add(1)
	}
	if isEdge && r.Method == http.MethodGet && c.corrupt.Load() {
		c.fakeS3.mu.Lock()
		b := append([]byte(nil), c.fakeS3.objects[r.URL.Path]...)
		c.fakeS3.mu.Unlock()
		if len(b) > 0 {
			b[len(b)/2] ^= 0xff
		}
		_, _ = w.Write(b)
		return
	}
	c.fakeS3.ServeHTTP(w, r)
}

func edgeChecker(t *testing.T, url string, every time.Duration) (*syntheticChecker, *time.Time) {
	t.Helper()
	c := newSyntheticChecker(url, "AK", "SK", "synthetic-check", "us-east-1", time.Minute, zap.NewNop())
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	c.edgeEvery, c.edgeBytes = every, 1<<20
	return c, &now
}

func TestSyntheticCheck_EdgeLegOffByDefault(t *testing.T) {
	s3 := &countingS3{fakeS3: newFakeS3()}
	srv := httptest.NewServer(s3)
	defer srv.Close()
	c := newSyntheticChecker(srv.URL, "AK", "SK", "b", "us-east-1", time.Minute, zap.NewNop())
	assert.Zero(t, c.edgeEvery, "off unless configured")
	c.runOnce(context.Background())
	assert.Equal(t, int64(5), s3.n.Load(), "the small cycle only")
	m := gatherSynthetic(t, c)
	assert.Equal(t, 0.0, m["vaultaire_synthetic_check_edge_enabled"])
	_, ok := m["vaultaire_synthetic_check_edge_success"]
	assert.False(t, ok, "no leg ran")
}

func TestSyntheticCheck_EdgeLegRunsOncePerEvery(t *testing.T) {
	s3 := &countingS3{fakeS3: newFakeS3()}
	srv := httptest.NewServer(s3)
	defer srv.Close()
	c, now := edgeChecker(t, srv.URL, 30*time.Minute)

	c.runOnce(context.Background())
	assert.Equal(t, int64(3), s3.edge.Load(), "PUT, GET, DELETE of the edge key after the first small cycle")
	c.runOnce(context.Background())
	assert.Equal(t, int64(3), s3.edge.Load(), "not due again yet")
	*now = now.Add(31 * time.Minute)
	c.runOnce(context.Background())
	assert.Equal(t, int64(6), s3.edge.Load(), "due again after EVERY")
	assert.Empty(t, s3.objects, "the leg deletes its key")

	m := gatherSynthetic(t, c)
	assert.Equal(t, 1.0, m["vaultaire_synthetic_check_edge_enabled"])
	assert.Equal(t, 1.0, m["vaultaire_synthetic_check_edge_success"])
	assert.Equal(t, float64(now.Unix()), m["vaultaire_synthetic_check_edge_last_success_timestamp_seconds"])
	for _, op := range []string{"edge_put", "edge_get", "edge_delete"} {
		assert.Contains(t, m, "vaultaire_synthetic_check_latency_seconds{op="+op+"}", op)
		assert.Equal(t, 0.0, m["vaultaire_synthetic_check_failures_total{op="+op+"}"], op)
	}
}

func TestSyntheticCheck_EdgeLegBodyMismatchFailsTheLegNotTheSmallCycle(t *testing.T) {
	s3 := &countingS3{fakeS3: newFakeS3()}
	s3.corrupt.Store(true)
	srv := httptest.NewServer(s3)
	defer srv.Close()
	c, _ := edgeChecker(t, srv.URL, 30*time.Minute)
	c.runOnce(context.Background())

	m := gatherSynthetic(t, c)
	assert.Equal(t, 1.0, m["vaultaire_synthetic_check_success"], "the small cycle is green")
	assert.Equal(t, 0.0, m["vaultaire_synthetic_check_edge_success"], "the leg failed")
	assert.Equal(t, 1.0, m["vaultaire_synthetic_check_failures_total{op=edge_get}"])
	assert.Equal(t, 0.0, m["vaultaire_synthetic_check_edge_last_success_timestamp_seconds"])
	assert.Empty(t, s3.objects, "the key is deleted even after a failed GET")
}

func TestSyntheticCheck_EdgeLegEnvParsing(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		base := map[string]string{"SYNTHETIC_CHECK_URL": "https://s3.stored.cloud", "SYNTHETIC_CHECK_ACCESS_KEY": "a", "SYNTHETIC_CHECK_SECRET_KEY": "s"}
		for k, v := range m {
			base[k] = v
		}
		return func(k string) string { return base[k] }
	}
	c := newSyntheticCheckerFromEnv(env(nil), nil)
	require.NotNil(t, c)
	assert.Zero(t, c.edgeEvery, "unset = no leg")
	assert.Equal(t, int64(syntheticEdgeDefaultBytes), c.edgeBytes)

	c = newSyntheticCheckerFromEnv(env(map[string]string{"SYNTHETIC_CHECK_EDGE_EVERY": "30m", "SYNTHETIC_CHECK_EDGE_BYTES": "8MiB"}), nil)
	assert.Equal(t, 30*time.Minute, c.edgeEvery)
	assert.Equal(t, int64(8<<20), c.edgeBytes)

	c = newSyntheticCheckerFromEnv(env(map[string]string{"SYNTHETIC_CHECK_EDGE_EVERY": "off"}), nil)
	assert.Zero(t, c.edgeEvery)
	c = newSyntheticCheckerFromEnv(env(map[string]string{"SYNTHETIC_CHECK_EDGE_EVERY": "1m"}), nil)
	assert.Zero(t, c.edgeEvery, "below 5m is rejected: the default (no leg)")
	c = newSyntheticCheckerFromEnv(env(map[string]string{"SYNTHETIC_CHECK_EDGE_EVERY": "30m", "SYNTHETIC_CHECK_EDGE_BYTES": "100MiB"}), nil)
	assert.Equal(t, int64(syntheticEdgeDefaultBytes), c.edgeBytes, "over 64 MiB is rejected")
	c = newSyntheticCheckerFromEnv(env(map[string]string{"SYNTHETIC_CHECK_EDGE_EVERY": "30m", "SYNTHETIC_CHECK_EDGE_BYTES": "512KiB"}), nil)
	assert.Equal(t, int64(syntheticEdgeDefaultBytes), c.edgeBytes, "under 1 MiB is rejected")
}
