package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Backend health used to be a TCP dial of whichever endpoints had env vars —
// which meant iDrive (the PRIMARY) and Geyser were never probed at all, and a
// dead access key looked exactly like a healthy backend for two weeks. Probes
// are now authenticated: iDrive/Geyser = the driver's signed HeadBucket via
// the engine; Lyve = console RSCustomerDetails with the root key. TCP dial
// remains the fallback for backends without an authed probe.

type fakeDriverChecker struct {
	drivers map[string]error // name → HealthCheck result
	calls   []string
}

func (f *fakeDriverChecker) GetDriverNames() []string {
	names := make([]string, 0, len(f.drivers))
	for n := range f.drivers {
		names = append(names, n)
	}
	return names
}

func (f *fakeDriverChecker) CheckDriver(_ context.Context, name string) error {
	f.calls = append(f.calls, name)
	err, ok := f.drivers[name]
	if !ok {
		return errors.New("driver not found")
	}
	return err
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func findCheck(t *testing.T, checks []backendCheck, name string) backendCheck {
	t.Helper()
	for _, c := range checks {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %+v", name, checks)
	return backendCheck{}
}

func TestBuildBackendProbes_NothingConfigured(t *testing.T) {
	got := buildBackendProbes(envOf(nil), &fakeDriverChecker{})
	assert.Empty(t, got)
}

func TestBuildBackendProbes_IDriveUsesSignedDriverCheck(t *testing.T) {
	// Arrange
	eng := &fakeDriverChecker{drivers: map[string]error{"idrive": nil}}
	env := map[string]string{"IDRIVE_ACCESS_KEY": "ak", "IDRIVE_SECRET_KEY": "sk"}

	// Act
	got := buildBackendProbes(envOf(env), eng)
	c := findCheck(t, got, "idrive")
	require.NotNil(t, c.probe, "idrive must get an authenticated probe, not a TCP dial")
	err := c.probe(context.Background())

	// Assert
	require.NoError(t, err)
	assert.Equal(t, []string{"idrive"}, eng.calls, "probe delegates to the driver's HealthCheck")
}

func TestBuildBackendProbes_GeyserUsesSignedDriverCheck(t *testing.T) {
	eng := &fakeDriverChecker{drivers: map[string]error{"geyser": errors.New("403 Forbidden")}}
	env := map[string]string{"GEYSER_ACCESS_KEY": "ak", "GEYSER_SECRET_KEY": "sk"}

	got := buildBackendProbes(envOf(env), eng)
	c := findCheck(t, got, "geyser")
	require.NotNil(t, c.probe)
	assert.EqualError(t, c.probe(context.Background()), "403 Forbidden")
}

func TestBuildBackendProbes_SkipsDriverThatFailedToRegister(t *testing.T) {
	// Credentials present but the engine never got the driver (bad config at
	// boot is already logged loudly there) — do not add a probe that can only
	// ever say "driver not found".
	eng := &fakeDriverChecker{drivers: map[string]error{}}
	env := map[string]string{"IDRIVE_ACCESS_KEY": "ak", "IDRIVE_SECRET_KEY": "sk"}

	got := buildBackendProbes(envOf(env), eng)
	for _, c := range got {
		assert.NotEqual(t, "idrive", c.name)
	}
}

func TestBuildBackendProbes_LyveConsoleProbeWhenSecretPresent(t *testing.T) {
	env := map[string]string{"LYVE_ACCESS_KEY": "STX1ROOT", "LYVE_SECRET_KEY": "s", "LYVE_REGION": "us-east-1"}

	got := buildBackendProbes(envOf(env), &fakeDriverChecker{})
	c := findCheck(t, got, "lyve")

	require.NotNil(t, c.probe, "with a secret the Lyve probe is the console action")
	assert.Equal(t, "s3.us-east-1.global.lyve.seagate.com:443", c.address, "TCP address kept for diagnostics")
	assert.Equal(t, lyveConsoleProbeInterval, c.interval, "console probe is paced slower than the S3 HEADs")
}

func TestBuildBackendProbes_LyveFallsBackToTCPWithoutSecret(t *testing.T) {
	env := map[string]string{"LYVE_ACCESS_KEY": "STX1ROOT"}

	got := buildBackendProbes(envOf(env), &fakeDriverChecker{})
	c := findCheck(t, got, "lyve")

	assert.Nil(t, c.probe, "no secret → cannot sign → TCP dial fallback")
	assert.NotEmpty(t, c.address)
}

func TestLyveProbeCredentials_DedicatedProbeKeyWins(t *testing.T) {
	// After the Lyve hygiene pass prod's LYVE_* becomes a scoped service user,
	// but RSCustomerDetails is root-only — so the probe key is separate.
	env := map[string]string{
		"LYVE_ACCESS_KEY":       "STX1SERVICE",
		"LYVE_SECRET_KEY":       "service-secret",
		"LYVE_PROBE_ACCESS_KEY": "STX1ROOT",
		"LYVE_PROBE_SECRET_KEY": "root-secret",
		"LYVE_PROBE_CUSTOMER":   "v01",
	}
	ak, sk, customer := lyveProbeCredentials(envOf(env))
	assert.Equal(t, "STX1ROOT", ak)
	assert.Equal(t, "root-secret", sk)
	assert.Equal(t, "v01", customer)

	delete(env, "LYVE_PROBE_ACCESS_KEY")
	delete(env, "LYVE_PROBE_SECRET_KEY")
	delete(env, "LYVE_PROBE_CUSTOMER")
	ak, sk, customer = lyveProbeCredentials(envOf(env))
	assert.Equal(t, "STX1SERVICE", ak, "falls back to the data-plane key (today that IS root)")
	assert.Equal(t, "service-secret", sk)
	assert.Equal(t, "v01", customer, "customer id defaults to ours")
}

func TestBuildBackendProbes_QuotalessStaysTCP(t *testing.T) {
	env := map[string]string{"QUOTALESS_ACCESS_KEY": "qk"}
	got := buildBackendProbes(envOf(env), &fakeDriverChecker{})
	c := findCheck(t, got, "quotaless")
	assert.Nil(t, c.probe)
	assert.Equal(t, "io.quotaless.cloud:8000", c.address)
}

func TestProbeBackendOnce_UpdatesHealthFromProbeResult(t *testing.T) {
	// Arrange
	s := &Server{startTime: time.Now(), healthChecker: NewBackendHealthChecker(), logger: zap.NewNop()}
	s.healthChecker.RegisterBackend("idrive")
	boom := errors.New("InvalidAccessKeyId: The Access Key Id you provided does not exist")
	failing := backendCheck{name: "idrive", probe: func(context.Context) error { return boom }}
	passing := backendCheck{name: "idrive", probe: func(context.Context) error { return nil }}

	// Act + Assert: failure is recorded with the auth error text
	s.probeBackendOnce(context.Background(), failing)
	st := s.healthChecker.GetBackendStates()["idrive"]
	require.NotNil(t, st)
	assert.False(t, st.Healthy)
	assert.Contains(t, st.LastError, "InvalidAccessKeyId")
	assert.Equal(t, int64(1), st.Failures)

	// recovery clears it
	s.probeBackendOnce(context.Background(), passing)
	st = s.healthChecker.GetBackendStates()["idrive"]
	assert.True(t, st.Healthy)
	assert.Empty(t, st.LastError)
	assert.Equal(t, int64(1), st.Failures, "failure counter is cumulative, not reset")
}

func TestProbeBackendOnce_ProbeIsBoundedByTimeout(t *testing.T) {
	// A hung backend (Geyser has a 5-minute response-header timeout) must not
	// wedge the health loop.
	s := &Server{startTime: time.Now(), healthChecker: NewBackendHealthChecker(), logger: zap.NewNop()}
	s.healthChecker.RegisterBackend("geyser")
	hung := backendCheck{name: "geyser", timeout: 20 * time.Millisecond, probe: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}

	start := time.Now()
	s.probeBackendOnce(context.Background(), hung)

	assert.Less(t, time.Since(start), 2*time.Second)
	st := s.healthChecker.GetBackendStates()["geyser"]
	assert.False(t, st.Healthy)
	assert.Contains(t, st.LastError, "deadline")
}

func TestBuildBackendProbes_R2UsesSignedDriverCheck(t *testing.T) {
	// R2 (public-bucket/CDN backend) is probed with the driver's signed
	// HeadBucket like idrive/geyser — a revoked R2 token must trip the alert.
	eng := &fakeDriverChecker{drivers: map[string]error{"r2": nil}}
	env := map[string]string{"R2_ACCOUNT_ID": "acct", "R2_ACCESS_KEY": "ak", "R2_SECRET_KEY": "sk"}

	got := buildBackendProbes(envOf(env), eng)
	c := findCheck(t, got, "r2")
	require.NotNil(t, c.probe, "r2 must get an authenticated probe")
	require.NoError(t, c.probe(context.Background()))
	assert.Equal(t, []string{"r2"}, eng.calls)

	// Not registered (boot failure) → no probe.
	got = buildBackendProbes(envOf(env), &fakeDriverChecker{drivers: map[string]error{}})
	for _, c := range got {
		assert.NotEqual(t, "r2", c.name)
	}
}
