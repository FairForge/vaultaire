package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Review R13-13 (checklist item 4): the synthetic customer check drives a
// signed PUT/HEAD/GET/DELETE cycle and exports its outcome. The fake S3
// server below stands in for the public endpoint; the real path is proven
// on a local build in docs/reviews/R13-jobs.md.

type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	auths   []string
	failGet bool
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.objects[r.URL.Path] = b
		w.WriteHeader(http.StatusOK)
	case http.MethodHead:
		b, ok := f.objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if f.failGet {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		b, ok := f.objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(b)
	case http.MethodDelete:
		delete(f.objects, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func gatherSynthetic(t *testing.T, c *syntheticChecker) map[string]float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(newSyntheticCollector(c))
	fams, err := reg.Gather()
	require.NoError(t, err)
	out := map[string]float64{}
	for _, fam := range fams {
		for _, m := range fam.GetMetric() {
			name := fam.GetName()
			for _, l := range m.GetLabel() {
				name += "{" + l.GetName() + "=" + l.GetValue() + "}"
			}
			switch {
			case m.GetGauge() != nil:
				out[name] = m.GetGauge().GetValue()
			case m.GetCounter() != nil:
				out[name] = m.GetCounter().GetValue()
			}
		}
	}
	return out
}

func TestSyntheticCheck_CycleSignsAndReportsSuccess(t *testing.T) {
	s3 := newFakeS3()
	srv := httptest.NewServer(s3)
	defer srv.Close()

	c := newSyntheticChecker(srv.URL, "AKSYN", "secret", "synthetic-check", "us-east-1", time.Minute, zap.NewNop())
	c.runOnce(context.Background())

	m := gatherSynthetic(t, c)
	assert.Equal(t, 1.0, m["vaultaire_synthetic_check_success"], "lastError=%q", c.lastError)
	assert.Greater(t, m["vaultaire_synthetic_check_last_success_timestamp_seconds"], 0.0)
	for _, op := range syntheticOps {
		assert.Contains(t, m, "vaultaire_synthetic_check_latency_seconds{op="+op+"}", op)
		assert.Equal(t, 0.0, m["vaultaire_synthetic_check_failures_total{op="+op+"}"], op)
	}
	require.Len(t, s3.auths, 5, "PUT, HEAD, GET, DELETE, GET")
	for _, a := range s3.auths {
		assert.True(t, strings.HasPrefix(a, "AWS4-HMAC-SHA256 Credential=AKSYN/"), a)
		assert.Contains(t, a, "/us-east-1/s3/aws4_request")
	}
	assert.Empty(t, s3.objects, "the cycle deletes its own key")
}

func TestSyntheticCheck_FailureIsAttributedToTheOp(t *testing.T) {
	s3 := newFakeS3()
	s3.failGet = true
	srv := httptest.NewServer(s3)
	defer srv.Close()

	c := newSyntheticChecker(srv.URL, "AK", "SK", "b", "us-east-1", time.Minute, zap.NewNop())
	c.runOnce(context.Background())
	c.runOnce(context.Background())

	m := gatherSynthetic(t, c)
	assert.Equal(t, 0.0, m["vaultaire_synthetic_check_success"])
	assert.Equal(t, 0.0, m["vaultaire_synthetic_check_last_success_timestamp_seconds"], "never succeeded")
	assert.Equal(t, 2.0, m["vaultaire_synthetic_check_failures_total{op=get}"])
	assert.Equal(t, 0.0, m["vaultaire_synthetic_check_failures_total{op=put}"])
	_, ok := m["vaultaire_synthetic_check_latency_seconds{op=delete}"]
	assert.False(t, ok, "delete never ran")
	assert.Contains(t, c.lastError, "get: status 500")

	// Unreachable endpoint: put fails, nothing panics.
	srv.Close()
	c.runOnce(context.Background())
	m = gatherSynthetic(t, c)
	assert.Equal(t, 1.0, m["vaultaire_synthetic_check_failures_total{op=put}"])
}

func TestSyntheticCheck_BeforeFirstRunExportsNoSuccessSeries(t *testing.T) {
	c := newSyntheticChecker("http://127.0.0.1:1", "AK", "SK", "b", "us-east-1", time.Minute, zap.NewNop())
	m := gatherSynthetic(t, c)
	_, ok := m["vaultaire_synthetic_check_success"]
	assert.False(t, ok)
	assert.Equal(t, 0.0, m["vaultaire_synthetic_check_last_success_timestamp_seconds"])
}

func TestSyntheticCheck_EnvParsing(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	assert.Nil(t, newSyntheticCheckerFromEnv(env(map[string]string{}), nil), "off when the URL is unset")
	assert.Nil(t, newSyntheticCheckerFromEnv(env(map[string]string{"SYNTHETIC_CHECK_URL": "https://stored.ge"}), nil), "off without a key pair")

	c := newSyntheticCheckerFromEnv(env(map[string]string{
		"SYNTHETIC_CHECK_URL": "https://stored.ge/", "SYNTHETIC_CHECK_ACCESS_KEY": "a", "SYNTHETIC_CHECK_SECRET_KEY": "s",
		"SYNTHETIC_CHECK_INTERVAL": "5s", // below the floor → default
	}), nil)
	require.NotNil(t, c)
	assert.Equal(t, "https://stored.ge", c.url)
	assert.Equal(t, syntheticDefaultBucket, c.bucket)
	assert.Equal(t, syntheticDefaultRegion, c.region)
	assert.Equal(t, syntheticDefaultInterval, c.interval)

	c = newSyntheticCheckerFromEnv(env(map[string]string{
		"SYNTHETIC_CHECK_URL": "https://stored.ge", "SYNTHETIC_CHECK_ACCESS_KEY": "a", "SYNTHETIC_CHECK_SECRET_KEY": "s",
		"SYNTHETIC_CHECK_BUCKET": "canary", "SYNTHETIC_CHECK_INTERVAL": "10m", "SYNTHETIC_CHECK_REGION": "us-central-1",
	}), nil)
	require.NotNil(t, c)
	assert.Equal(t, "canary", c.bucket)
	assert.Equal(t, 10*time.Minute, c.interval)
	assert.Equal(t, "us-central-1", c.region)

	var nilChecker *syntheticChecker
	nilChecker.Start(context.Background()) // nil-safe
}
