package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// /metrics was a hand-written two-line page and vaultaire_errors_total never
// incremented, so the prod VaultaireHighErrorRate rule could never fire. It is
// now a real Prometheus registry that keeps the two legacy series (same names,
// same meaning — the live rules reference them) and adds backend health,
// circuit state and durable-write failures so Alertmanager can page on a dead
// backend instead of us noticing two weeks later in journalctl.

type fakeEngineMetrics struct {
	statuses      map[string]string
	writeFailures int64
}

func (f *fakeEngineMetrics) GetFailoverStatus() map[string]string { return f.statuses }
func (f *fakeEngineMetrics) WriteFailures() int64                 { return f.writeFailures }

func scrape(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	rr := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	return rr.Body.String()
}

func TestMetricsEndpoint_KeepsLegacySeries(t *testing.T) {
	// Arrange
	s := &Server{startTime: time.Now(), healthChecker: NewBackendHealthChecker(), logger: zap.NewNop()}
	atomic.StoreInt64(&s.requestCount, 5)
	atomic.StoreInt64(&s.errorCount, 2)
	s.initMetrics()

	// Act
	rr := httptest.NewRecorder()
	s.handleMetrics(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	// Assert
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Contains(t, body, "# TYPE vaultaire_requests_total counter")
	assert.Contains(t, body, "\nvaultaire_requests_total 5\n")
	assert.Contains(t, body, "\nvaultaire_errors_total 2\n")
}

func TestBackendCollector_HealthCircuitAndWriteFailures(t *testing.T) {
	// Arrange
	hc := NewBackendHealthChecker()
	hc.RegisterBackend("lyve")
	hc.RegisterBackend("idrive")
	hc.UpdateHealth("idrive", false, 40*time.Millisecond, errors.New("InvalidAccessKeyId"))
	hc.UpdateHealth("idrive", false, 40*time.Millisecond, errors.New("InvalidAccessKeyId"))
	hc.UpdateHealth("lyve", true, 70*time.Millisecond, nil)
	eng := &fakeEngineMetrics{
		statuses:      map[string]string{"idrive": "open", "lyve": "closed", "geyser": "half-open"},
		writeFailures: 7,
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(newBackendCollector(hc, eng))

	// Act
	body := scrape(t, reg)

	// Assert
	assert.Contains(t, body, `vaultaire_backend_health{backend="idrive"} 0`)
	assert.Contains(t, body, `vaultaire_backend_health{backend="lyve"} 1`)
	assert.Contains(t, body, `vaultaire_backend_probe_failures_total{backend="idrive"} 2`)
	assert.Contains(t, body, `vaultaire_backend_probe_failures_total{backend="lyve"} 0`)
	assert.Contains(t, body, `vaultaire_backend_probe_latency_seconds{backend="lyve"} 0.07`)
	assert.Contains(t, body, `vaultaire_backend_circuit_open{backend="idrive"} 1`)
	assert.Contains(t, body, `vaultaire_backend_circuit_open{backend="lyve"} 0`)
	assert.Contains(t, body, `vaultaire_backend_circuit_open{backend="geyser"} 1`, "half-open is not closed")
	assert.Contains(t, body, "\nvaultaire_backend_write_failures_total 7\n")
	assert.Contains(t, body, "# TYPE vaultaire_backend_write_failures_total counter")
}

func TestBackendCollector_NilEngineStillExportsHealth(t *testing.T) {
	hc := NewBackendHealthChecker()
	hc.RegisterBackend("lyve")
	reg := prometheus.NewRegistry()
	reg.MustRegister(newBackendCollector(hc, nil))

	body := scrape(t, reg)

	assert.Contains(t, body, `vaultaire_backend_health{backend="lyve"} 1`)
	assert.NotContains(t, body, "vaultaire_backend_write_failures_total")
}

func TestLoggingMiddleware_CountsServerErrorsOnly(t *testing.T) {
	// Arrange
	s := &Server{startTime: time.Now(), logger: zap.NewNop()}
	serve := func(status int) {
		h := s.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("x"))
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	}

	// Act
	serve(http.StatusOK)
	serve(http.StatusNotFound)
	serve(http.StatusInternalServerError)
	serve(http.StatusServiceUnavailable)

	// Assert
	assert.Equal(t, int64(4), atomic.LoadInt64(&s.requestCount))
	assert.Equal(t, int64(2), atomic.LoadInt64(&s.errorCount), "only 5xx are server errors")
}

func TestLoggingMiddleware_ImplicitOKIsNotAnError(t *testing.T) {
	s := &Server{startTime: time.Now(), logger: zap.NewNop()}
	h := s.loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("no explicit WriteHeader"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	assert.Equal(t, int64(0), atomic.LoadInt64(&s.errorCount))
}
