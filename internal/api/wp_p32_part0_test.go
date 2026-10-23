package api

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
)

// Stage 1a Prompt 32 Part 0b (RP38-1, P4): a new slot exported
// vaultaire_backend_health 1 for every backend before its first probe —
// RegisterBackend seeds Healthy, so an unknown state read as healthy. Now a
// backend never probed has no health sample (and no latency sample): an
// absent series, never a false 1. /health is untouched (RP9-9).
func TestBackendHealth_NoSampleBeforeFirstProbe(t *testing.T) {
	// Arrange
	s := newTestServerWithHealthChecker(t)
	s.healthChecker.RegisterBackend("idrive")
	reg := prometheus.NewRegistry()
	reg.MustRegister(newBackendCollector(s.healthChecker, nil))

	// Act + Assert: registered only — no health, no latency.
	body := scrape(t, reg)
	assert.NotContains(t, body, `vaultaire_backend_health{backend="idrive"}`, "an unprobed backend is unknown, never healthy")
	assert.NotContains(t, body, `vaultaire_backend_probe_latency_seconds{backend="idrive"}`)
	assert.Contains(t, body, `vaultaire_backend_probe_failures_total{backend="idrive"} 0`)
	assert.Equal(t, "healthy", healthOf(t, s)["status"], "/health keeps RP9-9's rule")

	// Its first probe fails: one sample, 0 (never answered — RP9-9).
	s.healthChecker.UpdateHealth("idrive", false, 15*time.Second, errors.New("AccessDenied"))
	body = scrape(t, reg)
	assert.Contains(t, body, `vaultaire_backend_health{backend="idrive"} 0`)
	assert.Contains(t, body, `vaultaire_backend_probe_latency_seconds{backend="idrive"} 15`)

	// It answers: 1.
	s.healthChecker.UpdateHealth("idrive", true, 10*time.Millisecond, nil)
	assert.Contains(t, scrape(t, reg), `vaultaire_backend_health{backend="idrive"} 1`)
}
