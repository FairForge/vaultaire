package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The origin LE cert expires 2026-10-29 (T-2 days to launch) and certbot's
// dry-run cannot prove the real renewal (it asks the staging CA about a
// production cert → "Certificate not found"). The app therefore watches the
// certificate HAProxy actually serves and exports NotAfter so Prometheus can
// page at <14d (deploy/monitoring/vaultaire-tls.yml).

func TestParseCertProbeTargets(t *testing.T) {
	got := parseCertProbeTargets(" stored.ge@127.0.0.1:443, stored.cloud , , api.stored.ge@10.0.0.1 ")
	assert.Equal(t, []certProbeTarget{
		{ServerName: "stored.ge", Addr: "127.0.0.1:443"},
		{ServerName: "stored.cloud", Addr: "stored.cloud:443"},
		{ServerName: "api.stored.ge", Addr: "10.0.0.1:443"},
	}, got)
	assert.Empty(t, parseCertProbeTargets(""))
	assert.Nil(t, newCertExpiryMonitorFromEnv(envOf(nil), zap.NewNop()), "unset env → no monitor")
}

func TestProbeCertNotAfter_ReadsLeafFromHandshake(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	// The httptest cert is self-signed and not for this SNI — verification
	// fails, and the probe must still report the leaf's NotAfter (taken from
	// the x509 error) without disabling verification.
	got, err := probeCertNotAfter(context.Background(), certProbeTarget{ServerName: "stored.ge", Addr: addr})
	require.NoError(t, err)
	assert.True(t, got.Equal(srv.Certificate().NotAfter), "must be the served leaf's NotAfter")

	_, err = probeCertNotAfter(context.Background(), certProbeTarget{ServerName: "x", Addr: "127.0.0.1:1"})
	assert.Error(t, err, "closed port must be an error, not a zero time")
}

func TestCertExpiryMonitor_ExportsExpiryAndKeepsLastGoodOnFailure(t *testing.T) {
	notAfter := time.Date(2026, 10, 29, 2, 43, 11, 0, time.UTC)
	fail := false
	m := newCertExpiryMonitor(parseCertProbeTargets("stored.ge@127.0.0.1:443,stored.cloud@127.0.0.1:443"), zap.NewNop())
	m.probe = func(_ context.Context, tgt certProbeTarget) (time.Time, error) {
		if fail && tgt.ServerName == "stored.ge" {
			return time.Time{}, errors.New("dial: connection refused")
		}
		return notAfter, nil
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(newCertExpiryCollector(m))
	// Exposition format renders the gauge as a float ("1.793241791e+09").
	want := strconv.FormatFloat(float64(notAfter.Unix()), 'g', -1, 64)

	// Before any probe: nothing exported (no fake zero timestamps).
	assert.NotContains(t, scrape(t, reg), "vaultaire_tls_cert_expiry_timestamp_seconds{")

	m.probeAll(context.Background())
	body := scrape(t, reg)
	assert.Contains(t, body, fmt.Sprintf("vaultaire_tls_cert_expiry_timestamp_seconds{sni=\"stored.ge\"} %s", want))
	assert.Contains(t, body, fmt.Sprintf("vaultaire_tls_cert_expiry_timestamp_seconds{sni=\"stored.cloud\"} %s", want))
	assert.Contains(t, body, "vaultaire_tls_cert_probe_ok{sni=\"stored.ge\"} 1")

	// A failed dial flips probe_ok to 0 but keeps the last known expiry, so
	// the expiry alert cannot silently un-fire because of a flaky probe.
	fail = true
	m.probeAll(context.Background())
	body = scrape(t, reg)
	assert.Contains(t, body, "vaultaire_tls_cert_probe_ok{sni=\"stored.ge\"} 0")
	assert.Contains(t, body, "vaultaire_tls_cert_probe_ok{sni=\"stored.cloud\"} 1")
	assert.Contains(t, body, fmt.Sprintf("vaultaire_tls_cert_expiry_timestamp_seconds{sni=\"stored.ge\"} %s", want))
	assert.Equal(t, "dial: connection refused", m.snapshot()["stored.ge"].Err)
}

func TestMetricsEndpoint_IncludesCertExpiryWhenConfigured(t *testing.T) {
	s := &Server{startTime: time.Now(), healthChecker: NewBackendHealthChecker(), logger: zap.NewNop()}
	s.certMonitor = newCertExpiryMonitor(parseCertProbeTargets("stored.ge"), zap.NewNop())
	s.certMonitor.probe = func(context.Context, certProbeTarget) (time.Time, error) {
		return time.Unix(1_800_000_000, 0), nil
	}
	s.certMonitor.probeAll(context.Background())
	s.initMetrics()

	rr := httptest.NewRecorder()
	s.handleMetrics(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "vaultaire_tls_cert_expiry_timestamp_seconds{sni=\"stored.ge\"} 1.8e+09")
}
