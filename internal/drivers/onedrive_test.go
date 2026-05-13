package drivers

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testCtxWithTenant(tid string) context.Context {
	return context.WithValue(context.Background(), common.TenantIDKey, tid)
}

func testCtxNoTenant() context.Context {
	return context.Background()
}

func TestNewOneDriveFleetDriver_NoTenants(t *testing.T) {
	for _, k := range []string{"TENANT_1_ID", "TENANT_1_CLIENT_ID", "TENANT_1_SECRET", "TENANT_1_USER"} {
		t.Setenv(k, "")
	}

	logger, _ := zap.NewDevelopment()
	_, err := NewOneDriveFleetDriver(logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no valid tenant configurations")
}

func TestNewOneDriveFleetDriver_IncompleteTenant(t *testing.T) {
	t.Setenv("TENANT_1_ID", "some-id")
	t.Setenv("TENANT_1_CLIENT_ID", "")
	t.Setenv("TENANT_1_SECRET", "")
	t.Setenv("TENANT_1_USER", "")

	logger, _ := zap.NewDevelopment()
	_, err := NewOneDriveFleetDriver(logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no valid tenant configurations")
}

func TestOneDriveDriver_Name(t *testing.T) {
	d := &OneDriveDriver{}
	assert.Equal(t, "onedrive", d.Name())
}

func TestOneDriveDriver_TenantCount(t *testing.T) {
	d := &OneDriveDriver{tenants: make([]*odTenant, 3)}
	assert.Equal(t, 3, d.TenantCount())
}

func TestOneDriveDriver_PickTenant_SkipsThrottled(t *testing.T) {
	t1 := &odTenant{name: "t1"}
	t2 := &odTenant{name: "t2"}
	t3 := &odTenant{name: "t3"}

	t1.throttledUntil.Store(9999999999)

	d := &OneDriveDriver{tenants: []*odTenant{t1, t2, t3}}

	picked := d.pickTenant()
	assert.NotEqual(t, "t1", picked.name)
}

func TestOneDriveDriver_BuildPath(t *testing.T) {
	d := &OneDriveDriver{}
	path := d.buildPath(testCtxWithTenant("tenant-abc"), "my-bucket", "path/to/obj.bin")
	assert.Equal(t, "t-tenant-abc/my-bucket/path/to/obj.bin", path)
}

func TestOneDriveDriver_BuildPath_DefaultTenant(t *testing.T) {
	d := &OneDriveDriver{}
	path := d.buildPath(testCtxNoTenant(), "bucket", "key")
	assert.Equal(t, "t-default/bucket/key", path)
}

func TestOdDecorrelatedJitter(t *testing.T) {
	base := 500 * time.Millisecond
	prev := base
	cap := 30 * time.Second

	for i := 0; i < 100; i++ {
		j := odDecorrelatedJitter(base, prev, cap)
		assert.GreaterOrEqual(t, j, base)
		assert.LessOrEqual(t, j, cap)
		prev = j
	}
}

func TestOneDriveFleetDriver_Integration(t *testing.T) {
	if os.Getenv("TENANT_1_ID") == "" {
		t.Skip("skipping integration test — no TENANT_1_ID")
	}

	logger, _ := zap.NewDevelopment()
	d, err := NewOneDriveFleetDriver(logger)
	require.NoError(t, err)
	assert.Greater(t, d.TenantCount(), 0)

	err = d.HealthCheck(testCtxNoTenant())
	require.NoError(t, err)
}
