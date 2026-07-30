package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// configuredBackends decides which backends get a health loop. Two properties
// matter: a backend is only probed when its credentials are actually
// configured (an unconditional probe makes the boot path depend on a third
// party, which is a CI flake source), and Lyve resolves to its real LC2
// hostname rather than the retired lyvecloud.seagate.com one.

func TestConfiguredBackends_NoneWhenUnconfigured(t *testing.T) {
	got := configuredBackends(func(string) string { return "" })
	assert.Empty(t, got, "nothing configured → nothing probed")
}

func TestConfiguredBackends_LyveUsesRegionalLC2Host(t *testing.T) {
	env := map[string]string{
		"LYVE_ACCESS_KEY": "STX1AAA",
		"LYVE_REGION":     "us-east-1",
	}
	got := configuredBackends(func(k string) string { return env[k] })

	require.Len(t, got, 1)
	assert.Equal(t, "lyve", got[0].name)
	assert.Equal(t, "s3.us-east-1.global.lyve.seagate.com:443", got[0].address)
	assert.NotContains(t, got[0].address, "lyvecloud.seagate.com",
		"the retired LC1 hostname must not come back")
}

func TestConfiguredBackends_LyveDefaultsToUSWest1(t *testing.T) {
	env := map[string]string{"LYVE_ACCESS_KEY": "STX1AAA"}
	got := configuredBackends(func(k string) string { return env[k] })

	require.Len(t, got, 1)
	assert.Equal(t, "s3.us-west-1.global.lyve.seagate.com:443", got[0].address,
		"must match the driver's us-west-1 default (closest to SLC)")
}

func TestConfiguredBackends_QuotalessOnlyWithCredentials(t *testing.T) {
	// Endpoint alone is not enough — the backend is unused without a key.
	env := map[string]string{"QUOTALESS_ENDPOINT": "https://io.quotaless.cloud:8000"}
	assert.Empty(t, configuredBackends(func(k string) string { return env[k] }))

	env["QUOTALESS_ACCESS_KEY"] = "qk"
	got := configuredBackends(func(k string) string { return env[k] })
	require.Len(t, got, 1)
	assert.Equal(t, "quotaless", got[0].name)
	assert.Equal(t, "io.quotaless.cloud:8000", got[0].address)
}

func TestConfiguredBackends_BadEndpointSkippedNotFatal(t *testing.T) {
	env := map[string]string{
		"QUOTALESS_ACCESS_KEY": "qk",
		"QUOTALESS_ENDPOINT":   "://not a url",
		"LYVE_ACCESS_KEY":      "STX1AAA",
	}
	got := configuredBackends(func(k string) string { return env[k] })

	// The bad quotaless endpoint is dropped; Lyve must still be probed.
	require.Len(t, got, 1)
	assert.Equal(t, "lyve", got[0].name)
}

func TestConfiguredBackends_MultipleBackends(t *testing.T) {
	env := map[string]string{
		"QUOTALESS_ACCESS_KEY": "qk",
		"LYVE_ACCESS_KEY":      "STX1AAA",
		"LYVE_REGION":          "eu-west-1",
	}
	got := configuredBackends(func(k string) string { return env[k] })

	names := map[string]string{}
	for _, b := range got {
		names[b.name] = b.address
	}
	assert.Equal(t, "io.quotaless.cloud:8000", names["quotaless"])
	assert.Equal(t, "s3.eu-west-1.global.lyve.seagate.com:443", names["lyve"])
}
