package drivers

import (
	"context"
	"testing"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestWasabiConfigFromEnv_Defaults(t *testing.T) {
	endpoint, region, bucket := WasabiConfigFromEnv(func(string) string { return "" })
	assert.Equal(t, "us-west-1", region)
	assert.Equal(t, "https://s3.us-west-1.wasabisys.com", endpoint)
	assert.Equal(t, "vaultaire", bucket)
}

func TestWasabiConfigFromEnv_RegionDrivesEndpoint(t *testing.T) {
	env := map[string]string{"WASABI_REGION": "eu-central-1", "WASABI_BUCKET": "vlt-eu"}
	endpoint, region, bucket := WasabiConfigFromEnv(func(k string) string { return env[k] })
	assert.Equal(t, "eu-central-1", region)
	assert.Equal(t, "https://s3.eu-central-1.wasabisys.com", endpoint)
	assert.Equal(t, "vlt-eu", bucket)

	env["WASABI_ENDPOINT"] = "https://s3.wasabisys.com"
	endpoint, _, _ = WasabiConfigFromEnv(func(k string) string { return env[k] })
	assert.Equal(t, "https://s3.wasabisys.com", endpoint, "an explicit endpoint wins")
}

// The driver registers, errs and keys as "wasabi": a row it writes says
// backend_name = 'wasabi', and its keys follow the fixed-bucket shape, so the
// erasure sweep, chunk store and routing truth treat it exactly like iDrive.
func TestWasabiDriver_NameAndKeyShape(t *testing.T) {
	d, err := NewWasabiDriver("ak", "sk", "https://s3.us-west-1.wasabisys.com", "us-west-1", "vaultaire", zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, "wasabi", d.Name())

	ctx := common.WithTenantID(context.Background(), "tenant-a")
	assert.Equal(t, "t-tenant-a/photos/2026/a.jpg", d.ObjectKey(ctx, "photos", "2026/a.jpg"))

	// A tenant-less call is refused under the driver's own name, not iDrive's.
	_, err = d.Exists(context.Background(), "photos", "a.jpg")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoTenant)
	assert.Contains(t, err.Error(), "wasabi")
	assert.NotContains(t, err.Error(), "idrive")
}

func TestNewFixedBucketS3Driver_RejectsMissingParts(t *testing.T) {
	logger := zap.NewNop()
	_, err := NewFixedBucketS3Driver("", "ak", "sk", "https://x", "r", "b", logger)
	require.Error(t, err)
	_, err = NewFixedBucketS3Driver("wasabi", "ak", "sk", "", "r", "b", logger)
	require.ErrorContains(t, err, "wasabi: endpoint required")
	_, err = NewFixedBucketS3Driver("wasabi", "", "sk", "https://x", "r", "b", logger)
	require.ErrorContains(t, err, "wasabi: credentials required")
	_, err = NewFixedBucketS3Driver("wasabi", "ak", "sk", "https://x", "", "b", logger)
	require.ErrorContains(t, err, "wasabi: region required")
	_, err = NewFixedBucketS3Driver("wasabi", "ak", "sk", "https://x", "r", "", logger)
	require.ErrorContains(t, err, "wasabi: bucket required")
}

// NewIDriveDriver is unchanged for its callers: it still registers as
// "idrive", still falls back to the account's default region and bucket.
func TestNewIDriveDriver_StillNamedIDrive(t *testing.T) {
	d, err := NewIDriveDriver("ak", "sk", "https://s3.us-central-1.idrivee2.com", "", zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, "idrive", d.Name())
	assert.Equal(t, IDriveFallbackRegion, d.Region())
}
