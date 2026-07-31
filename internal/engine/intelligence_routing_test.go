package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func routingEngine(t *testing.T, primary string, backends ...string) *CoreEngine {
	t.Helper()
	e := NewEngine(nil, zap.NewNop(), nil)
	for _, b := range backends {
		e.AddDriver(b, &stubDriver{name: b, data: map[string][]byte{}, healthy: true})
	}
	e.SetPrimary(primary)
	return e
}

// The 2026-07-31 durability bug: AccessTracker.GetRecommendation names
// PreferredBackend "local" for essentially every artifact that has an
// access_patterns row — both the "hot data" branch and the catch-all
// default. Put applied that recommendation verbatim, so overwriting any
// previously-accessed object silently relocated customer data from a durable
// backend onto the hub's single unreplicated disk. That is precisely what
// nonDurableBackends/WP-F prevents on the failover path; the recommendation
// override was a back door around it. Found in prod with 463 objects
// (807 MB) already moved onto local.
func TestApplyBackendRecommendation_RejectsNonDurable(t *testing.T) {
	e := routingEngine(t, "idrive", "idrive", "local", "lyve")

	got := e.applyBackendRecommendation("idrive", "local")

	assert.Equal(t, "idrive", got,
		"a non-durable recommendation must not pull customer data off a durable backend")
}

// STORAGE_MODE=local is a legitimate deployment: there local IS the durability
// story, so the recommendation is a no-op rather than a demotion.
func TestApplyBackendRecommendation_AllowsLocalWhenPrimary(t *testing.T) {
	e := routingEngine(t, "local", "local", "idrive")

	got := e.applyBackendRecommendation("local", "local")

	assert.Equal(t, "local", got)
}

// Durable-to-durable recommendations are the feature working as intended.
func TestApplyBackendRecommendation_AllowsDurableBackend(t *testing.T) {
	e := routingEngine(t, "idrive", "idrive", "lyve", "local")

	got := e.applyBackendRecommendation("idrive", "lyve")

	assert.Equal(t, "lyve", got, "durable recommendations must still be honoured")
}

// The tracker hardcodes backend names ("s3", "lyve") that need not exist in a
// given deployment; an unknown name keeps the already-resolved target.
func TestApplyBackendRecommendation_IgnoresUnregisteredBackend(t *testing.T) {
	e := routingEngine(t, "idrive", "idrive", "local")

	got := e.applyBackendRecommendation("idrive", "s3")

	assert.Equal(t, "idrive", got)
}
