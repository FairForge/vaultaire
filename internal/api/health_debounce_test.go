package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R6-3: /health on N consecutive probe failures. The probe runs every
// 30 s with a 15 s deadline and one failed probe flipped /health to
// degraded: on 2026-10-17 Wasabi's edge lost ~2.5 % of flows a minute, and
// 199 single failures in 2.7 days each said "degraded". A backend is now
// unhealthy after two consecutive failed probes (≥ 30 s of silence across
// two probes) and healthy again after one success; /health's status and
// reason, vaultaire_backend_health (which the rules join with
// vaultaire_primary_backend) and the dashboard all read that state. Every
// failure is still counted (vaultaire_backend_probe_failures_total) and its
// error shown (/health/backends last_error). The breaker is not the
// probe's: it observes, never charges.

func healthOf(t *testing.T, s *Server) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleHealthEnhanced(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	var resp map[string]any
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	return resp
}

func TestHealth_OneFailedProbeIsNotAnOutage(t *testing.T) {
	s := newTestServerWithHealthChecker(t)
	s.healthChecker.RegisterBackend("lyve")
	s.healthChecker.RegisterBackend("wasabi")
	s.healthChecker.UpdateHealth("lyve", true, 10*time.Millisecond, nil)
	s.healthChecker.UpdateHealth("wasabi", true, 10*time.Millisecond, nil)
	reg := prometheus.NewRegistry()
	reg.MustRegister(newBackendCollector(s.healthChecker, nil))

	// One failed probe: still healthy, but counted and shown.
	s.healthChecker.UpdateHealth("wasabi", false, 15*time.Second, errors.New("context deadline exceeded"))
	resp := healthOf(t, s)
	assert.Equal(t, "healthy", resp["status"])
	assert.Nil(t, resp["reason"])
	body := scrape(t, reg)
	assert.Contains(t, body, `vaultaire_backend_health{backend="wasabi"} 1`)
	assert.Contains(t, body, `vaultaire_backend_probe_failures_total{backend="wasabi"} 1`)
	assert.Equal(t, "context deadline exceeded", s.healthChecker.GetBackendStates()["wasabi"].LastError)

	// A second in a row: degraded, with the reason.
	s.healthChecker.UpdateHealth("wasabi", false, 15*time.Second, errors.New("context deadline exceeded"))
	resp = healthOf(t, s)
	assert.Equal(t, "degraded", resp["status"])
	assert.Equal(t, "probe failing on wasabi", resp["reason"])
	assert.Contains(t, scrape(t, reg), `vaultaire_backend_health{backend="wasabi"} 0`)

	// One success: healthy again.
	s.healthChecker.UpdateHealth("wasabi", true, 10*time.Millisecond, nil)
	resp = healthOf(t, s)
	assert.Equal(t, "healthy", resp["status"])
	assert.Contains(t, scrape(t, reg), `vaultaire_backend_health{backend="wasabi"} 1`)
	assert.Contains(t, scrape(t, reg), `vaultaire_backend_probe_failures_total{backend="wasabi"} 2`)

	// A failure, a success, a failure: never two in a row, never degraded.
	s.healthChecker.UpdateHealth("wasabi", false, 0, errors.New("x"))
	s.healthChecker.UpdateHealth("wasabi", true, 0, nil)
	s.healthChecker.UpdateHealth("wasabi", false, 0, errors.New("x"))
	assert.Equal(t, "healthy", healthOf(t, s)["status"])
}

// RP9-9 (Prompt 10 Part 0.3): RegisterBackend seeds Healthy and two
// failures in a row used to be needed whatever the backend's past, so after
// every slot switch /health said "healthy" for ~30 s with eleven dead iDrive
// drivers and vaultaire_backend_health read 1 for them until their second
// probe. A backend that has never answered a probe has no record to give
// the benefit of the doubt to: it is unhealthy at its first failure. The
// two-in-a-row rule is for a backend that has succeeded at least once.
func TestHealth_ABackendWithNoSuccessYetIsUnhealthyAtItsFirstFailedProbe(t *testing.T) {
	s := newTestServerWithHealthChecker(t)
	s.healthChecker.RegisterBackend("idrive")
	s.healthChecker.RegisterBackend("lyve")
	s.healthChecker.UpdateHealth("lyve", true, 10*time.Millisecond, nil)
	reg := prometheus.NewRegistry()
	reg.MustRegister(newBackendCollector(s.healthChecker, nil))
	assert.Equal(t, "healthy", healthOf(t, s)["status"], "registered, not yet probed: nothing to report")

	s.healthChecker.UpdateHealth("idrive", false, 0, errors.New("AccessDenied"))

	resp := healthOf(t, s)
	assert.Equal(t, "degraded", resp["status"], "its first probe failed and it has never answered one")
	assert.Equal(t, "probe failing on idrive", resp["reason"])
	assert.Contains(t, scrape(t, reg), `vaultaire_backend_health{backend="idrive"} 0`)
	assert.False(t, s.healthChecker.GetBackendStates()["idrive"].Healthy)

	// Once it has answered, one failure is not an outage again.
	s.healthChecker.UpdateHealth("idrive", true, 10*time.Millisecond, nil)
	s.healthChecker.UpdateHealth("idrive", false, 0, errors.New("AccessDenied"))
	assert.Equal(t, "healthy", healthOf(t, s)["status"], "it has succeeded: the two-in-a-row rule applies")
	s.healthChecker.UpdateHealth("idrive", false, 0, errors.New("AccessDenied"))
	assert.Equal(t, "degraded", healthOf(t, s)["status"])
}
