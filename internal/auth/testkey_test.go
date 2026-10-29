package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-DX-1: a VLT_TEST_ key minted for tenant T resolves to T's sandbox —
// created on the first mint, in one transaction (the user row, then the
// tenants row with sandbox_of, the api_keys row, the tenant_quotas row) —
// and the next mint reuses it; a live key never resolves there.
func TestTestKey_ResolvesToTheSandbox(t *testing.T) {
	db := setupTestDB(t)
	svc := NewAuthService(nil, db)
	user := newLifecycleUser(t, svc, "testmode@stored.ge")
	ctx := context.Background()

	key, err := svc.GenerateTestKey(ctx, user.ID, "test key", nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(key.Key, TestKeyPrefix), key.Key)
	assert.NotEqual(t, user.TenantID, key.TenantID, "the sandbox, never the parent")
	var parent string
	require.NoError(t, db.QueryRow(`SELECT sandbox_of FROM tenants WHERE id = $1`, key.TenantID).Scan(&parent))
	assert.Equal(t, user.TenantID, parent)
	var quotas int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM tenant_quotas WHERE tenant_id = $1`, key.TenantID).Scan(&quotas))
	assert.Equal(t, 1, quotas, "the four-table rule: the sandbox has its quota row")

	a := NewAuth(db, zap.NewNop(), nil)
	cred, err := a.LookupCredential(ctx, key.Key)
	require.NoError(t, err)
	assert.Equal(t, key.TenantID, cred.TenantID)

	again, err := svc.GenerateTestKey(ctx, user.ID, "second", nil)
	require.NoError(t, err)
	assert.Equal(t, key.TenantID, again.TenantID, "one sandbox per tenant")

	live, err := svc.GenerateAPIKey(ctx, user.ID, "live", nil)
	require.NoError(t, err)
	assert.False(t, strings.HasPrefix(live.Key, TestKeyPrefix))
	cred, err = a.LookupCredential(ctx, live.Key)
	require.NoError(t, err)
	assert.Equal(t, user.TenantID, cred.TenantID, "a live key never resolves to the sandbox")

	sb, err := SandboxOf(ctx, db, user.TenantID)
	require.NoError(t, err)
	assert.Equal(t, key.TenantID, sb)
	p, err := ParentOfSandbox(ctx, db, key.TenantID)
	require.NoError(t, err)
	assert.Equal(t, user.TenantID, p)
	p, err = ParentOfSandbox(ctx, db, user.TenantID)
	require.NoError(t, err)
	assert.Empty(t, p, "a live tenant is no sandbox")
}
