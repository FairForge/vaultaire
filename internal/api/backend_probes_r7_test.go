package api

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Review R7-19 (R1-12 / WP-R1-6): the 12 idrive-<region> drivers and
// permafrost were never probed, and without LYVE_PROBE_* the Lyve probe
// signed the console action with the data-plane key.

func TestBuildBackendProbes_RegionDriversWithOwnKeysAreProbedStaggered(t *testing.T) {
	eng := &fakeDriverChecker{drivers: map[string]error{
		"idrive":                nil,
		"idrive-us-west-2":      nil,
		"idrive-eu-west-1":      nil,
		"idrive-ap-northeast-1": nil,
	}}
	env := map[string]string{
		"IDRIVE_ACCESS_KEY": "ak", "IDRIVE_SECRET_KEY": "sk",
		// two regions carry their own pair; the third runs on the primary pair
		// (known 403 — R7-01) and must NOT be probed into a permanent alert
		"IDRIVE_US_WEST_2_ACCESS_KEY": "la-ak", "IDRIVE_US_WEST_2_SECRET_KEY": "la-sk",
		"IDRIVE_EU_WEST_1_ACCESS_KEY": "ie-ak", "IDRIVE_EU_WEST_1_SECRET_KEY": "ie-sk",
	}

	got := buildBackendProbes(envOf(env), eng)

	var names []string
	for _, c := range got {
		names = append(names, c.name)
	}
	sort.Strings(names)
	assert.Equal(t, []string{"idrive", "idrive-eu-west-1", "idrive-us-west-2"}, names)

	ie := findCheck(t, got, "idrive-eu-west-1")
	la := findCheck(t, got, "idrive-us-west-2")
	require.NotNil(t, ie.probe)
	require.NotNil(t, la.probe)
	require.NoError(t, ie.probe(context.Background()))
	assert.Contains(t, eng.calls, "idrive-eu-west-1", "probe is the driver's signed HeadBucket")

	// Staggered: distinct start offsets, all inside one interval, none zero.
	assert.NotEqual(t, ie.initialDelay, la.initialDelay)
	for _, c := range []backendCheck{ie, la} {
		assert.Greater(t, c.initialDelay, time.Duration(0))
		assert.Less(t, c.initialDelay, defaultProbeInterval)
	}
	assert.Equal(t, time.Duration(0), findCheck(t, got, "idrive").initialDelay, "the primary keeps probing immediately")
}

func TestBuildBackendProbes_RegionDriverHalfKeyIsNotProbed(t *testing.T) {
	eng := &fakeDriverChecker{drivers: map[string]error{"idrive": nil, "idrive-us-west-2": nil}}
	env := map[string]string{
		"IDRIVE_ACCESS_KEY": "ak", "IDRIVE_SECRET_KEY": "sk",
		"IDRIVE_US_WEST_2_ACCESS_KEY": "la-ak", // secret missing → falls back to the primary pair
	}
	for _, c := range buildBackendProbes(envOf(env), eng) {
		assert.NotEqual(t, "idrive-us-west-2", c.name)
	}
}

func TestBuildBackendProbes_PermafrostUsesDriverProbe(t *testing.T) {
	eng := &fakeDriverChecker{drivers: map[string]error{"permafrost": nil}}
	got := buildBackendProbes(envOf(map[string]string{"TENANT_1_ID": "tid"}), eng)

	c := findCheck(t, got, "permafrost")
	require.NotNil(t, c.probe, "permafrost gets the authenticated Graph probe")
	require.NoError(t, c.probe(context.Background()))
	assert.Equal(t, []string{"permafrost"}, eng.calls)

	// Not registered (fleet failed to build) → no probe that can only say "not found".
	got = buildBackendProbes(envOf(map[string]string{"TENANT_1_ID": "tid"}), &fakeDriverChecker{})
	for _, c := range got {
		assert.NotEqual(t, "permafrost", c.name)
	}
}

func TestBuildBackendProbes_LocalIsProbedWheneverRegistered(t *testing.T) {
	// `local` is always registered and needs no credentials; without a probe
	// state the admin backends page showed it as "unhealthy" forever.
	eng := &fakeDriverChecker{drivers: map[string]error{"local": nil}}
	got := buildBackendProbes(envOf(nil), eng)

	c := findCheck(t, got, "local")
	require.NotNil(t, c.probe)
	require.NoError(t, c.probe(context.Background()))
	assert.Equal(t, []string{"local"}, eng.calls, "the driver's own HealthCheck (a stat of DATA_PATH)")

	for _, c := range buildBackendProbes(envOf(nil), &fakeDriverChecker{}) {
		assert.NotEqual(t, "local", c.name, "not registered → not probed")
	}
}

func TestBuildBackendProbes_LyveWithoutProbeKeyUsesSignedHeadBucket(t *testing.T) {
	// R7-02: once LYVE_* is a scoped service user, RSCustomerDetails (root
	// only) would fail — so with no dedicated probe key the probe is the
	// driver's signed HeadBucket, never the data-plane key on the console.
	eng := &fakeDriverChecker{drivers: map[string]error{"lyve": nil}}
	env := map[string]string{"LYVE_ACCESS_KEY": "STX1SERVICE", "LYVE_SECRET_KEY": "s"}

	got := buildBackendProbes(envOf(env), eng)
	c := findCheck(t, got, "lyve")

	require.NotNil(t, c.probe)
	require.NoError(t, c.probe(context.Background()))
	assert.Equal(t, []string{"lyve"}, eng.calls, "signed HeadBucket via the engine")
	assert.Equal(t, time.Duration(0), c.interval, "S3 HEAD cadence, not the console pacing")
	assert.Equal(t, "s3.us-west-1.global.lyve.seagate.com:443", c.address, "TCP address kept for diagnostics")
}

func TestRunBackendHealthLoop_HonoursInitialDelay(t *testing.T) {
	s := &Server{startTime: time.Now(), healthChecker: NewBackendHealthChecker(), logger: zap.NewNop()}
	s.healthChecker.RegisterBackend("idrive-us-west-2")
	probed := make(chan time.Time, 1)
	start := time.Now()
	b := backendCheck{
		name:         "idrive-us-west-2",
		initialDelay: 60 * time.Millisecond,
		interval:     time.Hour,
		probe: func(context.Context) error {
			select {
			case probed <- time.Now():
			default:
			}
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.runBackendHealthLoop(ctx, b)

	select {
	case at := <-probed:
		assert.GreaterOrEqual(t, at.Sub(start), 60*time.Millisecond, "first probe waits out the stagger")
	case <-time.After(2 * time.Second):
		t.Fatal("probe never ran")
	}

	// Cancellation during the delay must not leak the goroutine or probe.
	ctx2, cancel2 := context.WithCancel(context.Background())
	probed2 := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		s.runBackendHealthLoop(ctx2, backendCheck{name: "x", initialDelay: time.Hour, probe: func(context.Context) error {
			probed2 <- struct{}{}
			return nil
		}})
		close(done)
	}()
	cancel2()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not exit on cancel during the initial delay")
	}
	assert.Empty(t, probed2)
}
