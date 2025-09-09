// internal/usage/grace_period_test.go
package usage

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGracePeriodManager_StartGracePeriod(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	gpm := NewGracePeriodManager(qm)
	require.NoError(t, gpm.InitializeSchema(context.Background()))
	ctx := context.Background()

	// Create tenant at 95% usage
	require.NoError(t, qm.CreateTenant(ctx, "tenant-1", "starter", 1073741824)) // 1GB
	_, err := qm.CheckAndReserve(ctx, "tenant-1", 1020054733)                   // 973MB (95%)
	require.NoError(t, err)

	// Start grace period
	grace, err := gpm.StartGracePeriod(ctx, "tenant-1", 72*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, "tenant-1", grace.TenantID)
	assert.Equal(t, GracePeriodStatusActive, grace.Status)
	assert.Equal(t, 72*time.Hour, grace.Duration)
	assert.NotNil(t, grace.ExpiresAt)
}

func TestGracePeriodManager_CheckGracePeriod(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	gpm := NewGracePeriodManager(qm)
	require.NoError(t, gpm.InitializeSchema(context.Background()))
	ctx := context.Background()

	// Create tenant
	require.NoError(t, qm.CreateTenant(ctx, "tenant-2", "starter", 1073741824))

	// No grace period initially
	grace, err := gpm.GetGracePeriod(ctx, "tenant-2")
	require.NoError(t, err)
	assert.Nil(t, grace)

	// Start grace period
	_, err = gpm.StartGracePeriod(ctx, "tenant-2", 24*time.Hour)
	require.NoError(t, err)

	// Check grace period
	grace, err = gpm.GetGracePeriod(ctx, "tenant-2")
	require.NoError(t, err)
	assert.NotNil(t, grace)
	assert.Equal(t, GracePeriodStatusActive, grace.Status)
}

func TestGracePeriodManager_ExtendGracePeriod(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	gpm := NewGracePeriodManager(qm)
	require.NoError(t, gpm.InitializeSchema(context.Background()))
	ctx := context.Background()

	// Create tenant and start grace period
	require.NoError(t, qm.CreateTenant(ctx, "tenant-3", "starter", 1073741824))
	grace, err := gpm.StartGracePeriod(ctx, "tenant-3", 24*time.Hour)
	require.NoError(t, err)

	originalExpiry := grace.ExpiresAt

	// Extend grace period
	extended, err := gpm.ExtendGracePeriod(ctx, "tenant-3", 24*time.Hour)
	require.NoError(t, err)
	assert.True(t, extended.ExpiresAt.After(*originalExpiry))
	assert.Equal(t, 1, extended.ExtensionCount)
}

func TestGracePeriodManager_ExpireGracePeriods(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	qm := NewQuotaManager(db)
	require.NoError(t, qm.InitializeSchema(context.Background()))

	gpm := NewGracePeriodManager(qm)
	require.NoError(t, gpm.InitializeSchema(context.Background()))
	ctx := context.Background()

	// Create tenant with expired grace period
	require.NoError(t, qm.CreateTenant(ctx, "tenant-4", "starter", 1073741824))

	// Start grace period with -1 hour (already expired)
	_, err := gpm.StartGracePeriod(ctx, "tenant-4", -1*time.Hour)
	require.NoError(t, err)

	// Process expired grace periods
	expired, err := gpm.ProcessExpiredGracePeriods(ctx)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	if len(expired) > 0 {
		assert.Equal(t, "tenant-4", expired[0])
	}

	// Check that grace period is now expired
	grace, err := gpm.GetGracePeriod(ctx, "tenant-4")
	require.NoError(t, err)
	assert.Equal(t, GracePeriodStatusExpired, grace.Status)
}
