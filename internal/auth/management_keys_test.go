package auth

// WP-KEY-1 (Stage 1a Prompt 54, PR A; D-STRIPE-29): the management key — an
// opaque bearer key of a TENANT (never of a person), stored as its sha256,
// shown once, looked up on every request under revoked_at IS NULL.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedManagementTenant(t *testing.T, db *sql.DB, sandboxOf string) string {
	t.Helper()
	id := "tenant-mk-" + uuid.NewString()[:8]
	var parent any
	if sandboxOf != "" {
		parent = sandboxOf
	}
	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, sandbox_of) VALUES ($1, 'MK Co', $2, $3, $4, $5)`,
		id, id+"@test.local", "VK"+id, "SK"+id, parent)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM management_keys WHERE tenant_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, id)
	})
	return id
}

func TestManagementKey_ShownOnceStoredAsHash(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDB(t)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	tenant := seedManagementTenant(t, db, "")
	sandbox := seedManagementTenant(t, db, tenant)

	key, secret, err := MintManagementKey(ctx, db, tenant, ManagementKeyOptions{Name: "server", Scopes: []string{ScopeManageWrite}})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(secret, "sk_live_"), secret)
	assert.GreaterOrEqual(t, len(secret), len("sk_live_")+40, "32 random bytes, base62")
	sum := sha256.Sum256([]byte(secret))
	var stored, hint string
	require.NoError(t, db.QueryRow(`SELECT key_hash, hint FROM management_keys WHERE id = $1`, key.ID).Scan(&stored, &hint))
	assert.Equal(t, hex.EncodeToString(sum[:]), stored, "the row holds the sha256 of the secret")
	var leaked int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM management_keys WHERE id = $1 AND (key_hash = $2 OR hint = $2 OR name = $2 OR s3_ceiling::text LIKE '%' || $2 || '%')`,
		key.ID, secret).Scan(&leaked))
	assert.Zero(t, leaked, "the secret itself is stored nowhere")
	assert.Less(t, len(hint), 12, "the hint is never enough to use")

	// Listed without the secret, ever.
	keys, err := ListManagementKeys(ctx, db, tenant)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, key.ID, keys[0].ID)
	assert.Equal(t, []string{ScopeManageWrite}, keys[0].Scopes)

	// The lookup finds it by the secret, and only by it.
	got, err := LookupManagementKey(ctx, db, secret)
	require.NoError(t, err)
	assert.Equal(t, tenant, got.TenantID)
	_, err = LookupManagementKey(ctx, db, secret+"x")
	assert.ErrorIs(t, err, ErrManagementKeyInvalid)
	_, err = LookupManagementKey(ctx, db, "eyJhbGciOi.not.a.key")
	assert.ErrorIs(t, err, ErrManagementKeyInvalid)

	// A sandbox's key is a test key (the tenant's livemode).
	_, testSecret, err := MintManagementKey(ctx, db, sandbox, ManagementKeyOptions{Scopes: []string{ScopeManageRead}})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(testSecret, "sk_test_"), testSecret)

	// Scopes are a closed list, and at least one.
	_, _, err = MintManagementKey(ctx, db, tenant, ManagementKeyOptions{Scopes: []string{"manage:everything"}})
	assert.ErrorIs(t, err, ErrManagementKeyScope)
	_, _, err = MintManagementKey(ctx, db, tenant, ManagementKeyOptions{})
	assert.ErrorIs(t, err, ErrManagementKeyScope)
}

func TestManagementKey_RevokedIsRefusedAtOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDB(t)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	tenant := seedManagementTenant(t, db, "")
	other := seedManagementTenant(t, db, "")

	key, secret, err := MintManagementKey(ctx, db, tenant, ManagementKeyOptions{Scopes: []string{ScopeManageRead}})
	require.NoError(t, err)
	got, err := LookupManagementKey(ctx, db, secret)
	require.NoError(t, err)
	require.NotNil(t, got.LastUsedAt, "the use is recorded")
	first := *got.LastUsedAt

	// last_used_at is coalesced to once a minute: a second use within it
	// writes nothing.
	_, err = LookupManagementKey(ctx, db, secret)
	require.NoError(t, err)
	var used time.Time
	require.NoError(t, db.QueryRow(`SELECT last_used_at FROM management_keys WHERE id = $1`, key.ID).Scan(&used))
	assert.WithinDuration(t, first, used, time.Millisecond)

	assert.ErrorIs(t, RevokeManagementKey(ctx, db, other, key.ID), ErrManagementKeyNotFound, "another tenant's key is not found")
	require.NoError(t, RevokeManagementKey(ctx, db, tenant, key.ID))
	_, err = LookupManagementKey(ctx, db, secret)
	assert.ErrorIs(t, err, ErrManagementKeyInvalid, "refused on the very next request: no cache")
	assert.ErrorIs(t, RevokeManagementKey(ctx, db, tenant, key.ID), ErrManagementKeyNotFound, "revoked once")
}
