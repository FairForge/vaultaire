package drivers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The new iDrive reseller account mints one key pair PER REGION, while the
// engine registers an idrive-<region> driver for every region using the single
// primary pair — so bucket-level region routing to any non-primary region
// 403s. Per-region env overrides fix that; the primary pair stays the fallback.

func TestIDriveRegionCredentials_OverrideWins(t *testing.T) {
	env := map[string]string{
		"IDRIVE_ACCESS_KEY":              "primary-ak",
		"IDRIVE_SECRET_KEY":              "primary-sk",
		"IDRIVE_EU_WEST_2_ACCESS_KEY":    "ldn-ak",
		"IDRIVE_EU_WEST_2_SECRET_KEY":    "ldn-sk",
		"IDRIVE_US_CENTRAL_1_ACCESS_KEY": "dal-ak",
		"IDRIVE_US_CENTRAL_1_SECRET_KEY": "dal-sk",
	}
	getenv := func(k string) string { return env[k] }

	ak, sk := IDriveRegionCredentials(getenv, "eu-west-2")
	assert.Equal(t, "ldn-ak", ak)
	assert.Equal(t, "ldn-sk", sk)

	ak, sk = IDriveRegionCredentials(getenv, "us-central-1")
	assert.Equal(t, "dal-ak", ak)
	assert.Equal(t, "dal-sk", sk)
}

func TestIDriveRegionCredentials_FallsBackToPrimary(t *testing.T) {
	env := map[string]string{
		"IDRIVE_ACCESS_KEY": "primary-ak",
		"IDRIVE_SECRET_KEY": "primary-sk",
		// half an override must not be honoured
		"IDRIVE_EU_WEST_1_ACCESS_KEY": "ie-ak",
	}
	getenv := func(k string) string { return env[k] }

	ak, sk := IDriveRegionCredentials(getenv, "us-west-2")
	assert.Equal(t, "primary-ak", ak)
	assert.Equal(t, "primary-sk", sk)

	ak, sk = IDriveRegionCredentials(getenv, "eu-west-1")
	assert.Equal(t, "primary-ak", ak, "access key without its secret is ignored")
	assert.Equal(t, "primary-sk", sk)
}

func TestIDriveRegionEnvKey(t *testing.T) {
	assert.Equal(t, "IDRIVE_US_CENTRAL_1_ACCESS_KEY", IDriveRegionEnvKey("us-central-1", "ACCESS_KEY"))
	assert.Equal(t, "IDRIVE_EU_SOUTH_1_SECRET_KEY", IDriveRegionEnvKey("eu-south-1", "SECRET_KEY"))
}

func TestIDriveRegionEndpoint_OverrideElseDefault(t *testing.T) {
	env := map[string]string{"IDRIVE_US_CENTRAL_1_ENDPOINT": "https://s3.us-central-1.idrivee2.com"}
	getenv := func(k string) string { return env[k] }
	assert.Equal(t, "https://s3.us-central-1.idrivee2.com", IDriveRegionEndpoint(getenv, "us-central-1"))
	assert.Equal(t, IDriveRegions["us-west-2"], IDriveRegionEndpoint(getenv, "us-west-2"))
}
