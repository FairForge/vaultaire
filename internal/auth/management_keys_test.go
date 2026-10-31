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

// RP88-2: every principal has an expiry (docs/ACCOUNTS.md § Principal); a
// management key's is optional (NULL = none, a server key's default), and
// the lookup refuses a key past it on the very next request.
func TestManagementKey_ExpiredIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDB(t)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	tenant := seedManagementTenant(t, db, "")

	past := time.Now().Add(-time.Minute)
	_, _, err := MintManagementKey(ctx, db, tenant, ManagementKeyOptions{Scopes: []string{ScopeManageRead}, ExpiresAt: &past})
	assert.ErrorIs(t, err, ErrManagementKeyExpiry, "a key is never minted already expired")

	soon := time.Now().Add(time.Hour)
	key, secret, err := MintManagementKey(ctx, db, tenant, ManagementKeyOptions{Scopes: []string{ScopeManageRead}, ExpiresAt: &soon})
	require.NoError(t, err)
	require.NotNil(t, key.ExpiresAt)
	assert.WithinDuration(t, soon, *key.ExpiresAt, time.Second)
	_, err = LookupManagementKey(ctx, db, secret)
	require.NoError(t, err, "live until its expiry")
	keys, err := ListManagementKeys(ctx, db, tenant)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	require.NotNil(t, keys[0].ExpiresAt, "the list shows it")

	_, err = db.Exec(`UPDATE management_keys SET expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, key.ID)
	require.NoError(t, err)
	_, err = LookupManagementKey(ctx, db, secret)
	assert.ErrorIs(t, err, ErrManagementKeyInvalid, "refused past its expiry")

	_, never, err := MintManagementKey(ctx, db, tenant, ManagementKeyOptions{Scopes: []string{ScopeManageRead}})
	require.NoError(t, err)
	got, err := LookupManagementKey(ctx, db, never)
	require.NoError(t, err)
	assert.Nil(t, got.ExpiresAt, "no expiry is the default")
}

// RP88-3: the live-key count runs under the tenant row's FOR UPDATE, so two
// concurrent mints at 24 never both insert.
func TestManagementKey_LiveCountHoldsUnderConcurrentMints(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDB(t)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	tenant := seedManagementTenant(t, db, "")
	for i := 0; i < MaxLiveManagementKeys-1; i++ {
		_, _, err := MintManagementKey(ctx, db, tenant, ManagementKeyOptions{Scopes: []string{ScopeManageRead}})
		require.NoError(t, err)
	}
	// Every mint lingers between its count and its insert: without the
	// lock, all of them count 24.
	mintAfterCount = func() { time.Sleep(50 * time.Millisecond) }
	t.Cleanup(func() { mintAfterCount = func() {} })
	const racers = 6
	errs := make(chan error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		go func() {
			<-start
			_, _, err := MintManagementKey(ctx, db, tenant, ManagementKeyOptions{Scopes: []string{ScopeManageRead}})
			errs <- err
		}()
	}
	close(start)
	ok := 0
	for i := 0; i < racers; i++ {
		if err := <-errs; err == nil {
			ok++
		} else {
			assert.ErrorIs(t, err, ErrKeyLimitReached)
		}
	}
	assert.Equal(t, 1, ok, "exactly one mint takes the last place")
	var live int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM management_keys WHERE tenant_id = $1 AND revoked_at IS NULL`, tenant).Scan(&live))
	assert.Equal(t, MaxLiveManagementKeys, live)
}

// RP88-1: Test on the mint makes the key ON the tenant's sandbox (made on
// first use, as a test S3 key's is) — the one way to an sk_test_ key.
func TestManagementKey_TestMintIsTheSandboxs(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDB(t)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	tenant := seedManagementTenant(t, db, "")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM management_keys WHERE tenant_id IN (SELECT id FROM tenants WHERE sandbox_of = $1)`, tenant)
		_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id IN (SELECT id FROM tenants WHERE sandbox_of = $1)`, tenant)
		_, _ = db.Exec(`DELETE FROM tenants WHERE sandbox_of = $1`, tenant)
	})

	k, secret, err := MintManagementKey(ctx, db, tenant, ManagementKeyOptions{Scopes: []string{ScopeManageWrite}, Test: true})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(secret, ManagementKeyTestPrefix), secret)
	sandbox, err := SandboxOf(ctx, db, tenant)
	require.NoError(t, err)
	require.NotEmpty(t, sandbox, "the sandbox is made on first use")
	assert.Equal(t, sandbox, k.TenantID, "the key is the sandbox's")
	var quota int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM tenant_quotas WHERE tenant_id = $1`, sandbox).Scan(&quota))
	assert.Equal(t, 1, quota, "the sandbox has its quota row")

	k2, _, err := MintManagementKey(ctx, db, tenant, ManagementKeyOptions{Scopes: []string{ScopeManageRead}, Test: true})
	require.NoError(t, err)
	assert.Equal(t, sandbox, k2.TenantID, "one sandbox per tenant")

	_, _, err = MintManagementKey(ctx, db, sandbox, ManagementKeyOptions{Scopes: []string{ScopeManageRead}, Test: true})
	assert.ErrorIs(t, err, ErrSandboxOfSandbox)
	_, live, err := MintManagementKey(ctx, db, tenant, ManagementKeyOptions{Scopes: []string{ScopeManageRead}})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(live, ManagementKeyLivePrefix), "without Test, the live tenant's")

	keys, err := ListManagementKeys(ctx, db, tenant)
	require.NoError(t, err)
	assert.Len(t, keys, 3, "the tenant's list holds its sandbox's keys")
	require.NoError(t, RevokeManagementKey(ctx, db, tenant, k.ID), "and the tenant revokes them")
}
