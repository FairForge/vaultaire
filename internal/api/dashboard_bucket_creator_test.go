package api

import (
	"context"
	"database/sql"
	"testing"

	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R12-10 (R15): the dashboard's create form goes through the same
// registry as S3 CreateBucket and the management API. This drives the real
// adapter against the migrated test database: the row is written with the
// stored region and residency, the caps and region rules are the registry's,
// and nothing is created under DATA_PATH any more.
func TestDashboardBucketCreator_RegistryEndToEnd(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v) — run `make test-db`", err)
	}

	tenantID := "dash-creator-" + uuid.NewString()[:8]
	_, err = db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1, 'Dash Creator', $2, $3, $4)`,
		tenantID, tenantID+"@test.local", "AK-"+tenantID, "SK-"+tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM events WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, tenantID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID)
	})

	drivers.SetAvailableIDriveRegions("us-central-1", []string{"eu-west-1"})
	t.Cleanup(drivers.ResetAvailableIDriveRegions)

	s := &Server{logger: zap.NewNop(), router: chi.NewRouter(), db: db}
	create := s.dashboardBucketCreator()
	ctx := context.Background()

	res, err := create(ctx, tenantID, "eu-dash-bucket", "eu-west-1")
	require.NoError(t, err)
	assert.Equal(t, handlers.BucketCreateOK, res.State)
	assert.Equal(t, "eu-west-1", res.Region)

	var region, residency string
	require.NoError(t, db.QueryRow(`SELECT region, COALESCE(data_residency, '') FROM buckets WHERE tenant_id = $1 AND name = 'eu-dash-bucket'`, tenantID).Scan(&region, &residency))
	assert.Equal(t, "eu-west-1", region)
	assert.Equal(t, "eu", residency, "residency is derived by the registry for every entry point")

	res, err = create(ctx, tenantID, "home-bucket", "")
	require.NoError(t, err)
	assert.Equal(t, handlers.BucketCreateOK, res.State)
	assert.Equal(t, "us-central-1", res.Region, "empty region = the deployment default")

	res, err = create(ctx, tenantID, "tokyo-bucket", "ap-northeast-1")
	require.NoError(t, err)
	assert.Equal(t, handlers.BucketCreateRegionUnavailable, res.State)

	res, err = create(ctx, tenantID, "mars-bucket", "mars-1")
	require.NoError(t, err)
	assert.Equal(t, handlers.BucketCreateInvalidRegion, res.State)

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM buckets WHERE tenant_id = $1`, tenantID).Scan(&n))
	assert.Equal(t, 2, n, "refused regions write no row")
}
