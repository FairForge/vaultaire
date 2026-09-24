package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
)

// Public-read buckets are the ONLY thing that routes to Cloudflare R2 (Smart
// tier design, revised 2026-09-19: R2 = public buckets / CDN origin, not a
// tier). Placement resolves: explicit x-amz-storage-class header → bucket
// tier_preference (non-hot tiers keep their promise) → PUBLIC when the bucket
// is public-read AND an r2 driver is registered → "" (primary).

func seedPlacementBucket(t *testing.T, tenantID, bucket, visibility, tier string) {
	t.Helper()
	db := cdnTestDB(t)
	_, err := db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (id) DO NOTHING`,
		tenantID, "placement", tenantID+"@test.local", "AK-"+tenantID, "SK-"+tenantID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO buckets (tenant_id, name, visibility, tier_preference)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, name) DO UPDATE SET visibility = $3, tier_preference = $4`,
		tenantID, bucket, visibility, tier)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM buckets WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenants WHERE id = $1", tenantID)
	})
}

func placementEngine(t *testing.T, withR2 bool) *engine.CoreEngine {
	t.Helper()
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver("local", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	eng.SetPrimary("local")
	if withR2 {
		// Any driver registered under the r2 name stands in for the real one.
		eng.AddDriver("r2", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	}
	return eng
}

func TestPublicBucketStorageClass_PublicReadWithR2(t *testing.T) {
	db := cdnTestDB(t)
	tenantID := "placement-" + t.Name()
	seedPlacementBucket(t, tenantID, "pub", "public-read", "auto")

	assert.Equal(t, "PUBLIC", publicBucketStorageClass(context.Background(), db, placementEngine(t, true), tenantID, "pub"))
}

func TestPublicBucketStorageClass_NoR2DriverStaysOnPrimary(t *testing.T) {
	db := cdnTestDB(t)
	tenantID := "placement-" + t.Name()
	seedPlacementBucket(t, tenantID, "pub", "public-read", "auto")

	assert.Equal(t, "", publicBucketStorageClass(context.Background(), db, placementEngine(t, false), tenantID, "pub"))
}

func TestPublicBucketStorageClass_PrivateBucketNeverPublic(t *testing.T) {
	db := cdnTestDB(t)
	tenantID := "placement-" + t.Name()
	seedPlacementBucket(t, tenantID, "priv", "private", "auto")

	assert.Equal(t, "", publicBucketStorageClass(context.Background(), db, placementEngine(t, true), tenantID, "priv"))
	assert.Equal(t, "", publicBucketStorageClass(context.Background(), db, placementEngine(t, true), tenantID, "missing-bucket"))
	assert.Equal(t, "", publicBucketStorageClass(context.Background(), nil, placementEngine(t, true), tenantID, "priv"), "nil db is safe")
}

func TestResolvePutStorageClass_Precedence(t *testing.T) {
	db := cdnTestDB(t)
	tenantID := "placement-" + t.Name()
	eng := placementEngine(t, true)
	ctx := context.Background()

	// Public bucket on the default tier → PUBLIC.
	seedPlacementBucket(t, tenantID, "pub-auto", "public-read", "auto")
	assert.Equal(t, "PUBLIC", resolvePutStorageClass(ctx, db, eng, tenantID, "pub-auto", ""))

	// Explicit header always wins — a client asking for GLACIER gets GLACIER.
	assert.Equal(t, "GLACIER", resolvePutStorageClass(ctx, db, eng, tenantID, "pub-auto", "GLACIER"))

	// Hot tiers (standard/performance = STANDARD) do not override the public
	// role; cold/resilient tiers keep their placement promise.
	seedPlacementBucket(t, tenantID, "pub-std", "public-read", "standard")
	assert.Equal(t, "PUBLIC", resolvePutStorageClass(ctx, db, eng, tenantID, "pub-std", ""))
	seedPlacementBucket(t, tenantID, "pub-archive", "public-read", "archive")
	assert.Equal(t, "GLACIER", resolvePutStorageClass(ctx, db, eng, tenantID, "pub-archive", ""))
	seedPlacementBucket(t, tenantID, "pub-resilient", "public-read", "resilient")
	assert.Equal(t, "RESILIENT", resolvePutStorageClass(ctx, db, eng, tenantID, "pub-resilient", ""))

	// Private bucket → tier only.
	seedPlacementBucket(t, tenantID, "priv-std", "private", "standard")
	assert.Equal(t, "STANDARD", resolvePutStorageClass(ctx, db, eng, tenantID, "priv-std", ""))
	seedPlacementBucket(t, tenantID, "priv-auto", "private", "auto")
	assert.Equal(t, "", resolvePutStorageClass(ctx, db, eng, tenantID, "priv-auto", ""))
}

// PUBLIC objects must stay whole (no chunking) — direct-from-R2 serving and
// the CDN path both need the object addressable as one R2 key.
func TestStorageClassDisablesChunking_Public(t *testing.T) {
	assert.True(t, storageClassDisablesChunking("PUBLIC"))
	assert.True(t, storageClassDisablesChunking("RESILIENT"))
	assert.True(t, storageClassDisablesChunking("GLACIER"))
	assert.True(t, storageClassDisablesChunking("DEEP_ARCHIVE"))
	assert.False(t, storageClassDisablesChunking("STANDARD"))
	assert.False(t, storageClassDisablesChunking(""))
}
