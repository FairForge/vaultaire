package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
)

// WP-R7-1 (Review R7-01): a bucket pinned to a region routes to that region's
// driver, the default region routes through the engine (primary), and a
// region with no registered driver is REFUSED — never silently written to the
// primary under a residency label.

func seedRegionBucket(t *testing.T, tenantID, bucket, region string) {
	t.Helper()
	db := cdnTestDB(t)
	_, err := db.Exec(`
		INSERT INTO tenants (id, name, email, access_key, secret_key)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (id) DO NOTHING`,
		tenantID, "region", tenantID+"@test.local", "AK-"+tenantID, "SK-"+tenantID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO buckets (tenant_id, name, visibility, region)
		VALUES ($1, $2, 'private', $3)
		ON CONFLICT (tenant_id, name) DO UPDATE SET region = $3`,
		tenantID, bucket, region)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM buckets WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM tenants WHERE id = $1", tenantID)
	})
}

func regionEngine(t *testing.T, regionDrivers ...string) *engine.CoreEngine {
	t.Helper()
	eng := engine.NewEngine(nil, zap.NewNop(), nil)
	eng.AddDriver("local", drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	eng.SetPrimary("local")
	for _, r := range regionDrivers {
		eng.AddDriver("idrive-"+r, drivers.NewLocalDriver(t.TempDir(), zap.NewNop()))
	}
	return eng
}

func TestBucketRegionDriver_DefaultRegionUsesEngine(t *testing.T) {
	db := cdnTestDB(t)
	tenantID := "region-" + t.Name()
	seedRegionBucket(t, tenantID, "b", drivers.IDriveDefaultRegion(os.Getenv))

	name, err := bucketRegionDriver(context.Background(), db, regionEngine(t, "eu-west-1"), tenantID, "b")
	require.NoError(t, err)
	assert.Equal(t, "", name, "the primary serves the default region")

	name, err = bucketRegionDriver(context.Background(), db, regionEngine(t), tenantID, "missing-bucket")
	require.NoError(t, err)
	assert.Equal(t, "", name, "unknown bucket → engine decides (404 later)")

	name, err = bucketRegionDriver(context.Background(), nil, regionEngine(t), tenantID, "b")
	require.NoError(t, err)
	assert.Equal(t, "", name, "nil db is safe")
}

func TestBucketRegionDriver_PinnedRegionWithDriver(t *testing.T) {
	db := cdnTestDB(t)
	tenantID := "region-" + t.Name()
	seedRegionBucket(t, tenantID, "eu", "eu-west-1")

	name, err := bucketRegionDriver(context.Background(), db, regionEngine(t, "eu-west-1"), tenantID, "eu")
	require.NoError(t, err)
	assert.Equal(t, "idrive-eu-west-1", name)
}

func TestBucketRegionDriver_PinnedRegionWithoutDriverIsRefused(t *testing.T) {
	db := cdnTestDB(t)
	tenantID := "region-" + t.Name()
	seedRegionBucket(t, tenantID, "tokyo", "ap-northeast-1")

	name, err := bucketRegionDriver(context.Background(), db, regionEngine(t, "eu-west-1"), tenantID, "tokyo")
	assert.Equal(t, "", name)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errRegionDriverUnavailable), "%v", err)
}

func TestCreateBucket_RegionNotEnabledOnDeploymentIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testS3DB(t)
	defer func() { _ = db.Close() }()
	cleanupS3BucketData(t, db)
	defer cleanupS3BucketData(t, db)
	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ('test-s3-r9', 'Region Co', 'r9@test.com', 'VK-r9', 'SK-r9') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	// This deployment serves the default region and eu-west-1 only.
	drivers.SetAvailableIDriveRegions(drivers.IDriveDefaultRegion(os.Getenv), []string{"eu-west-1"})
	t.Cleanup(drivers.ResetAvailableIDriveRegions)

	s := s3ServerWithDB(t, db)
	defer func() { _ = os.RemoveAll("/tmp/vaultaire/test-s3-r9") }()

	req := httptest.NewRequest("PUT", "/tokyo-bucket", nil)
	req.Header.Set("X-Stored-Region", "ap-northeast-1")
	req = withTenantCtx(req, "test-s3-r9")
	w := httptest.NewRecorder()
	s.CreateBucket(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "InvalidLocationConstraint")
	assert.Contains(t, w.Body.String(), "not enabled")
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM buckets WHERE tenant_id = 'test-s3-r9'`).Scan(&n))
	assert.Equal(t, 0, n, "no bucket row for a region nothing can serve")

	// The enabled region still works.
	req = httptest.NewRequest("PUT", "/dublin-bucket", nil)
	req.Header.Set("X-Stored-Region", "eu-west-1")
	req = withTenantCtx(req, "test-s3-r9")
	w = httptest.NewRecorder()
	s.CreateBucket(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "eu-west-1", w.Header().Get("x-amz-bucket-region"))
}
