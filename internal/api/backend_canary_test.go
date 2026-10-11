package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
)

// Prompt 2b.5 F2. On 2026-10-11 the iDrive key answered HeadBucket 200 and
// every object call 403 for ≥ 9 h; the probe was the HeadBucket, so /health
// and vaultaire_backend_health{idrive} stayed healthy. Before: the probe of
// the 403 fake passed (health 1).

// memS3 is a minimal bucket: HeadBucket 200, objects kept in memory.
type memS3 struct {
	srv  *httptest.Server
	mu   sync.Mutex
	objs map[string][]byte
	puts atomic.Int32
	// noList: the credential lacks ListBucket — S3 then answers a HEAD of
	// an ABSENT key 403, not 404.
	noList bool
}

func newMemS3(t *testing.T) (*drivers.IDriveDriver, *memS3) {
	t.Helper()
	m := &memS3{objs: map[string][]byte{}}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "vaultaire" || path == "vaultaire/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		key := strings.TrimPrefix(path, "vaultaire/")
		m.mu.Lock()
		defer m.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			m.objs[key] = b
			m.puts.Add(1)
			w.Header().Set("ETag", `"x"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			if b, ok := m.objs[key]; ok {
				w.Header().Set("Content-Length", itoa(len(b)))
				w.WriteHeader(http.StatusOK)
				return
			}
			if m.noList {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(m.srv.Close)
	d, err := drivers.NewFixedBucketS3Driver("idrive", "AKIAMEM", "secret", m.srv.URL, "us-central-1", "vaultaire", zap.NewNop())
	require.NoError(t, err)
	return d, m
}

func (m *memS3) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.objs))
	for k := range m.objs {
		out = append(out, k)
	}
	return out
}

func probeServer() *Server {
	return &Server{startTime: time.Now(), healthChecker: NewBackendHealthChecker(), logger: zap.NewNop()}
}

func TestObjectCanary_ObjectCallsForbidden_HealthZeroWithinOneProbe(t *testing.T) {
	d, fi := newForbiddenIDrive(t, "idrive")
	require.NoError(t, d.HealthCheck(context.Background()), "HeadBucket alone stays green — the 2026-10-11 shape")
	s := probeServer()
	s.healthChecker.RegisterBackend("idrive")
	check := backendCheck{name: "idrive", probe: withObjectCanary(d.HealthCheck, d, "box-1")}

	s.probeBackendOnce(context.Background(), check)

	st := s.healthChecker.GetBackendStates()["idrive"]
	assert.False(t, st.Healthy, "one probe sees the object-level 403")
	assert.Equal(t, int64(1), st.Failures, "vaultaire_backend_probe_failures_total +1")
	assert.Contains(t, st.LastError, "object canary HEAD")
	assert.Contains(t, st.LastError, "403", "the SDK error is in the log and last_error")
	assert.Contains(t, st.LastError, "and its PUT", "a 403 HEAD is followed by one PUT, refused too")
	assert.Equal(t, int32(2), fi.objectCalls.Load(), "one HEAD, one PUT")
}

func TestObjectCanary_AbsentIsWrittenOnceThenHealthy(t *testing.T) {
	d, m := newMemS3(t)
	s := probeServer()
	s.healthChecker.RegisterBackend("idrive")
	check := backendCheck{name: "idrive", probe: withObjectCanary(d.HealthCheck, d, "box-1")}

	s.probeBackendOnce(context.Background(), check) // boot: absent → PUT
	s.probeBackendOnce(context.Background(), check) // present: HEAD only

	assert.True(t, s.healthChecker.GetBackendStates()["idrive"].Healthy)
	assert.Equal(t, []string{"t-_probe/_probe/box-1"}, m.keys(), "the reserved tenant's key")
	assert.Equal(t, int32(1), m.puts.Load(), "written once, skipped when it exists")

	// A cleanup removed it: re-created by the next probe, still healthy.
	m.mu.Lock()
	delete(m.objs, "t-_probe/_probe/box-1")
	m.mu.Unlock()
	s.probeBackendOnce(context.Background(), check)
	assert.True(t, s.healthChecker.GetBackendStates()["idrive"].Healthy)
	assert.Equal(t, int32(2), m.puts.Load())
}

// fakeCanaryEngine is a driverChecker that also hands out drivers.
type fakeCanaryEngine struct {
	fakeDriverChecker
	drvs map[string]engine.Driver
}

func (f *fakeCanaryEngine) GetDriver(name string) (engine.Driver, bool) {
	d, ok := f.drvs[name]
	return d, ok
}

func TestBuildBackendProbes_CanaryOnS3ClassOnly(t *testing.T) {
	forbidden, _ := newForbiddenIDrive(t, "idrive")
	eng := &fakeCanaryEngine{
		fakeDriverChecker: fakeDriverChecker{drivers: map[string]error{"idrive": nil, "geyser": nil, "permafrost": nil, "wasabi": nil}},
		drvs:              map[string]engine.Driver{"idrive": forbidden, "geyser": forbidden, "permafrost": forbidden, "wasabi": forbidden},
	}
	env := map[string]string{"IDRIVE_ACCESS_KEY": "a", "GEYSER_ACCESS_KEY": "a", "TENANT_1_ID": "a", "WASABI_ACCESS_KEY": "a"}

	checks := buildBackendProbes(envOf(env), eng)

	for _, name := range []string{"idrive", "wasabi"} {
		err := findCheck(t, checks, name).probe(context.Background())
		require.Error(t, err, "%s: HeadBucket passes, the canary HEAD is refused", name)
		assert.Contains(t, err.Error(), "object canary")
	}
	for _, name := range []string{"geyser", "permafrost"} {
		assert.NoError(t, findCheck(t, checks, name).probe(context.Background()), "%s keeps its bucket-level probe: never a PUT on tape or the fleet", name)
	}
}

// F2.2: an open breaker on the PRIMARY is /health degraded, with the reason.
// Before: status "healthy", backends_healthy = total, while every write fell
// over and every read of the primary's objects answered 503 (8.7 h).
func TestHealth_PrimaryBreakerOpenIsDegraded(t *testing.T) {
	f := setupID403(t)
	f.eng.SetPrimary("idrive")
	s := probeServer()
	s.engine = f.eng
	for _, b := range []string{"idrive", "lyve", "geyser"} {
		s.healthChecker.RegisterBackend(b)
		s.healthChecker.UpdateHealth(b, true, time.Millisecond, nil)
	}
	health := func() map[string]any {
		w := httptest.NewRecorder()
		s.handleHealthEnhanced(w, httptest.NewRequest(http.MethodGet, "/health", nil))
		require.Equal(t, http.StatusOK, w.Code, "never a 503: HAProxy would evict the instance")
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		return body
	}
	require.Equal(t, "healthy", health()["status"])

	openBreaker(t, f.eng, "idrive", f.tenantID)
	body := health()
	assert.Equal(t, "degraded", body["status"])
	assert.Equal(t, float64(2), body["backends_healthy"])
	assert.Equal(t, float64(3), body["backends_total"])
	assert.Equal(t, "circuit breaker open on the primary backend idrive", body["reason"])

	// A failing probe is named in the reason too.
	s.healthChecker.UpdateHealth("geyser", false, time.Millisecond, nil)
	body = health()
	assert.Equal(t, "probe failing on geyser; circuit breaker open on the primary backend idrive", body["reason"])
	s.healthChecker.UpdateHealth("geyser", true, time.Millisecond, nil)

	// A non-primary breaker open is not the primary's reason.
	f.eng.SetPrimary("lyve")
	body = health()
	assert.Equal(t, "healthy", body["status"])
	assert.Nil(t, body["reason"])
}

func TestMetrics_PrimaryBackendGauge(t *testing.T) {
	eng := engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "lyve"})
	eng.AddDriver("lyve", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	eng.AddDriver("idrive", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	eng.SetPrimary("lyve")
	c := newBackendCollector(NewBackendHealthChecker(), eng)
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	n, err := promtestutil.GatherAndCount(reg, "vaultaire_primary_backend")
	require.NoError(t, err)
	assert.Equal(t, 1, n, "one series: the primary")
	expected := `
# HELP vaultaire_primary_backend 1 for the engine's write primary (STORAGE_MODE); the rules join on it to page on the primary's breaker (Prompt 2b.5 F2.2).
# TYPE vaultaire_primary_backend gauge
vaultaire_primary_backend{backend="lyve"} 1
`
	require.NoError(t, promtestutil.GatherAndCompare(reg, strings.NewReader(expected), "vaultaire_primary_backend"))
}

// A scoped key without ListBucket gets a 403 for the absent canary: the PUT
// tells it from a dead key, and the backend stays healthy.
func TestObjectCanary_ScopedKeyWithoutListBucketIsHealthy(t *testing.T) {
	d, m := newMemS3(t)
	m.noList = true
	s := probeServer()
	s.healthChecker.RegisterBackend("wasabi")
	check := backendCheck{name: "wasabi", probe: withObjectCanary(d.HealthCheck, d, "box-1")}

	s.probeBackendOnce(context.Background(), check)

	st := s.healthChecker.GetBackendStates()["wasabi"]
	assert.True(t, st.Healthy, st.LastError)
	assert.Equal(t, int32(1), m.puts.Load())
}
