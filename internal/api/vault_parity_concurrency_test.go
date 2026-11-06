package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// WP-FLEET next chain Part 4 (D-F-PROT-1): one protect reads the object
// back from Geyser at ~20 MB/s and writes its four shards; a 1 GiB object
// took ~54 s on three drives AND on six — the protects ran one at a time,
// so the job, not the fleet, was the ingest bound. Now up to
// VAULT_PARITY_PROTECT_CONCURRENCY (default 4, 1–16) protects run at once,
// each started only when its own estimate fits before the run's deadline,
// the byte budget shared.

func TestVaultParity_ProtectsRunConcurrently(t *testing.T) {
	// Arrange: four objects; every tape read takes 400 ms.
	f := setupParityFixture(t)
	f.svc.ProtectConcurrency = 4
	for _, k := range []string{"a.bin", "b.bin", "c.bin", "d.bin"} {
		f.object(k, 256<<10)
	}
	f.geyser.getDelay.Store(int64(400 * time.Millisecond))

	// Act
	start := time.Now()
	res := f.run()
	took := time.Since(start)

	// Assert: about one read's time, not four.
	assert.Equal(t, 4, res.Protected, "%+v", res)
	assert.Less(t, took, 1200*time.Millisecond, "four protects at once (sequential would be ≥ 1.6 s)")
	for _, k := range []string{"a.bin", "b.bin", "c.bin", "d.bin"} {
		state, _, _, _ := f.row(k)
		assert.Equal(t, "complete", state, k)
	}
}

func TestVaultParity_ConcurrentProtectsKeepTheByteBudget(t *testing.T) {
	f := setupParityFixture(t)
	f.svc.ProtectConcurrency = 4
	for _, k := range []string{"a.bin", "b.bin", "c.bin", "d.bin"} {
		f.object(k, 256<<10)
	}
	f.svc.MaxBytesPerRun = 2*(256<<10) + 1

	res := f.run()

	assert.Equal(t, 2, res.Protected, "the budget admits two objects, concurrency or not: %+v", res)
}

func TestVaultParity_AFailingProtectDoesNotStopTheOthers(t *testing.T) {
	f := setupParityFixture(t)
	f.svc.ProtectConcurrency = 4
	for _, k := range []string{"a.bin", "b.bin", "c.bin"} {
		f.object(k, 256<<10)
	}
	f.leg.failSuffix.Store("/p2") // every object's shard 2 fails

	res := f.run()

	assert.Equal(t, 3, res.Partial, "%+v", res)
	assert.Len(t, res.Errors, 3)
}

func TestProtectConcurrencyFromEnv(t *testing.T) {
	for v, want := range map[string]int{"": 4, "1": 1, "16": 16, "0": 4, "17": 4, "x": 4, "8": 8} {
		core, logs := observer.New(zap.WarnLevel)
		got := protectConcurrencyFromEnv(func(k string) string {
			if k == "VAULT_PARITY_PROTECT_CONCURRENCY" {
				return v
			}
			return ""
		}, zap.New(core))
		assert.Equal(t, want, got, "%q", v)
		rejected := v == "0" || v == "17" || v == "x"
		require.Equal(t, rejected, logs.Len() == 1, "%q", v)
	}
}

// WP-FLEET-10: the protects in flight are a gauge, back at 0 after a run.
func TestVaultParity_ProtectsInFlightIsAGaugeThatReturnsToZero(t *testing.T) {
	f := setupParityFixture(t)
	f.svc.ProtectConcurrency = 4
	for _, k := range []string{"a.bin", "b.bin", "c.bin"} {
		f.object(k, 256<<10)
	}
	f.geyser.getDelay.Store(int64(300 * time.Millisecond))
	peak := make(chan float64, 1)
	go func() {
		var m float64
		for i := 0; i < 40; i++ {
			m = max(m, promtest.ToFloat64(vaultParityProtectsInFlight))
			time.Sleep(10 * time.Millisecond)
		}
		peak <- m
	}()

	res := f.run()

	assert.Equal(t, 3, res.Protected, "%+v", res)
	assert.GreaterOrEqual(t, <-peak, float64(2), "protects at once while the reads ran")
	assert.Equal(t, float64(0), promtest.ToFloat64(vaultParityProtectsInFlight))
}

// WP-FLEET-10: the gauge and the fleet's new driver series reach /metrics of
// a fresh server (declared is not registered — be9d044d). Presence only.
func TestMetrics_FleetObservabilityRegistered(t *testing.T) {
	s := &Server{startTime: time.Now(), healthChecker: NewBackendHealthChecker(), logger: zap.NewNop()}
	s.initMetrics()
	rr := httptest.NewRecorder()
	s.handleMetrics(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	for _, want := range []string{
		"# TYPE vaultaire_vault_parity_protects_in_flight gauge",
		`vaultaire_permafrost_fallback_walks_total{op="get"}`,
		`vaultaire_permafrost_range_hedges_total{outcome="won"}`,
		`vaultaire_permafrost_fragment_resumes_total{outcome="resumed"}`,
	} {
		assert.Contains(t, body, want)
	}
}
