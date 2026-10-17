package api

import (
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
)

// The region gate (Stage 1a Prompt 10 Part A). drivers.IDriveRegionAvailable
// means "a driver is registered for it" (WP-R7-1), and main.go registers a
// region driver whenever its key pair is set. On prod every region pair has
// answered AccessDenied to every object call since 2026-10-11, so the picker
// and both CreateBucket paths offered ten regions in which a bucket can store
// nothing (every PUT 503). A bucket may now be created in a region only when
// it is:
//
//   - a region id we know (else unknown: 400 InvalidLocationConstraint /
//     invalid_region);
//   - enabled on this deployment, i.e. its driver is registered (else
//     not_enabled: 400 InvalidLocationConstraint / region_unavailable);
//   - and, unless it is the default region, healthy by the probe (the
//     /health rule: unhealthy after two failed probes in a row, or at the
//     first while it has never answered one) AND answered at least one probe
//     (the state is seeded healthy at boot: the prod proof of #753 saw two
//     dead regions offered before their staggered first probe ran), with its
//     breaker not OPEN
//     (half-open lets the next call through, as engine.CanAsk) — else down:
//     503 ServiceUnavailable + Retry-After / 503 region_unavailable_now.
//
// The default region is always offered: its buckets are served by the
// primary through the engine, and the primary's health is /health's
// business. A region driver with no probe state (tests, a dev box without
// probes) is judged on registration alone. An existing bucket pinned to a
// down region is unchanged: its writes already 503, its reads ask the
// recorded backend, and an idempotent re-create of it is not refused.

type regionCreateState int

const (
	regionOffered regionCreateState = iota
	regionUnknown
	regionNotEnabled
	regionDown
)

// regionRetryAfter is the Retry-After of a refusal for a down region: the
// probe runs every 30 s, so one minute is two probes.
const regionRetryAfter = "60"

var regionRefusalReason = map[regionCreateState]string{
	regionUnknown:    "unknown",
	regionNotEnabled: "not_enabled",
	regionDown:       "down",
}

var regionCreateRefused = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_region_create_refused_total",
	Help: "Bucket creations refused for their region (S3 CreateBucket, the management API and the dashboard), by region and reason: unknown (not a region id we know; region=\"other\"), not_enabled (no driver on this deployment), down (enabled, but its probe or breaker says it cannot take a write).",
}, []string{"region", "reason"})

func (s *Server) regionCreateState(region string) regionCreateState {
	if !drivers.IsValidRegion(region) {
		return regionUnknown
	}
	if !drivers.IDriveRegionAvailable(region) {
		return regionNotEnabled
	}
	if region == drivers.IDriveDefaultRegion(os.Getenv) {
		return regionOffered
	}
	backend := "idrive-" + region
	if s.healthChecker != nil {
		if up, known := s.healthChecker.backendProvenUp(backend); known && !up {
			return regionDown
		}
	}
	if s.regionBreakerStates()[backend] == engine.StateOpen.String() {
		return regionDown
	}
	return regionOffered
}

// regionDownMessage is the refusal's text on S3 and the management API (the
// dashboard says the same in its own words).
func regionDownMessage(region string) string {
	return "Region " + region + " is temporarily unavailable on this deployment: its storage is not answering. Retry later, or create the bucket in another region."
}

func (s *Server) regionBreakerStates() map[string]string {
	if s.regionBreakers != nil {
		return s.regionBreakers()
	}
	if s.engine != nil {
		return s.engine.GetFailoverStatus()
	}
	return nil
}

// regionAvailableNow is the gate as the dashboard's picker asks it
// (Deps.RegionAvailable): true only when a bucket may be created there now.
func (s *Server) regionAvailableNow(region string) bool {
	return s.regionCreateState(region) == regionOffered
}

// refuseRegion counts and logs one refused creation. The metric's region
// label is a region id we know, or "other": never caller input.
func (s *Server) refuseRegion(tenantID, region string, st regionCreateState) {
	label := region
	if st == regionUnknown {
		label = "other"
		if len(region) > 64 {
			region = region[:64]
		}
	}
	reason := regionRefusalReason[st]
	regionCreateRefused.WithLabelValues(label, reason).Inc()
	s.log().Info("bucket create refused: region",
		zap.String("tenant", tenantID), zap.String("region", region), zap.String("reason", reason))
}
