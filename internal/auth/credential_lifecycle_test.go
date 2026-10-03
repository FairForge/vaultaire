package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R5-14: the primary key pair can be rotated and revoked, for real.
//
// Registration used to write the pair twice (tenants.access_key/secret_key
// and an api_keys row named "primary"); rotation and revocation touched the
// api_keys row only, and every credential lookup resolved tenants FIRST with
// no revocation check — a rotated primary pair kept authenticating for ever
// (live-proven in docs/reviews/WP-R10-3b.md). Every test here runs on its
// own account (an @stored.ge e-mail, which setupTestDB clears).

// lifecycleAccount is one registered account on the test database.
type lifecycleAccount struct {
	t      *testing.T
	db     *sql.DB
	svc    *AuthService
	user   *User
	tenant *Tenant
	key    *APIKey
}

func newLifecycleAccount(t *testing.T, tag string) *lifecycleAccount {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping database test")
	}
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	svc := NewAuthService(nil, db)
	email := fmt.Sprintf("lifecycle-%s-%s@stored.ge", tag, GenerateID()[:6])
	user, tenant, key, err := svc.CreateUserWithTenant(context.Background(), email, "a-long-password", "Lifecycle Co")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM sts_tokens WHERE tenant_id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM api_keys WHERE user_id::text = $1`, user.ID)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenant.ID)
		_, _ = db.Exec(`DELETE FROM users WHERE id::text = $1`, user.ID)
	})
	return &lifecycleAccount{t: t, db: db, svc: svc, user: user, tenant: tenant, key: key}
}

// authenticate signs a request with the pair and runs it through the real
// verifier (the S3 header-auth path).
func (a *lifecycleAccount) authenticate(ak, sk string) (string, *KeyScope, error) {
	a.t.Helper()
	r := httptest.NewRequest("GET", "http://stored.ge/some-bucket?list-type=2", nil)
	signV4(a.t, r, ak, sk, "us-east-1", sha256Hex(""), time.Now().UTC())
	return NewAuth(a.db, zap.NewNop()).ValidateRequest(r)
}

func (a *lifecycleAccount) tenantsPair() (string, string) {
	a.t.Helper()
	var ak, sk sql.NullString
	require.NoError(a.t, a.db.QueryRow(`SELECT access_key, secret_key FROM tenants WHERE id = $1`, a.tenant.ID).Scan(&ak, &sk))
	return ak.String, sk.String
}

func (a *lifecycleAccount) livePrimaries() int {
	a.t.Helper()
	var n int
	require.NoError(a.t, a.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE tenant_id = $1 AND is_primary AND revoked_at IS NULL`, a.tenant.ID).Scan(&n))
	return n
}

func (a *lifecycleAccount) auditMetadata(action string) map[string]any {
	a.t.Helper()
	var raw []byte
	err := a.db.QueryRow(`SELECT metadata FROM audit_logs WHERE tenant_id = $1 AND action = $2 ORDER BY timestamp DESC LIMIT 1`, a.tenant.ID, action).Scan(&raw)
	require.NoError(a.t, err, "audit row %s", action)
	var m map[string]any
	require.NoError(a.t, json.Unmarshal(raw, &m))
	return m
}

func TestPrimaryKey_RegistrationWritesOnePrimaryRow(t *testing.T) {
	a := newLifecycleAccount(t, "reg")

	assert.True(t, a.key.IsPrimary, "the key registration hands out is the primary")
	assert.Equal(t, a.tenant.ID, a.key.TenantID)
	var tenantID string
	var isPrimary bool
	require.NoError(t, a.db.QueryRow(`SELECT tenant_id, is_primary FROM api_keys WHERE key_id = $1`, a.key.Key).Scan(&tenantID, &isPrimary))
	assert.Equal(t, a.tenant.ID, tenantID, "the row carries its tenant (WP-R5-9)")
	assert.True(t, isPrimary)
	assert.Equal(t, 1, a.livePrimaries())

	tenantID, scope, err := a.authenticate(a.key.Key, a.key.Secret)
	require.NoError(t, err)
	assert.Equal(t, a.tenant.ID, tenantID)
	assert.Equal(t, []string{"*"}, scope.Permissions)
}

func TestPrimaryKey_RotationKillsTheOldPairEverywhere(t *testing.T) {
	a := newLifecycleAccount(t, "rot")
	ctx := context.Background()
	oldAK, oldSK := a.key.Key, a.key.Secret
	_, _, err := a.authenticate(oldAK, oldSK)
	require.NoError(t, err, "the pair works before the rotation")

	// Act
	newKey, err := a.svc.RotateAPIKey(ctx, a.user.ID, a.key.ID)
	require.NoError(t, err)

	// Assert: the new pair is the primary, the old one is dead on every path.
	assert.True(t, newKey.IsPrimary)
	assert.NotEqual(t, oldAK, newKey.Key)
	assert.Equal(t, 1, a.livePrimaries(), "never two live primaries")

	_, _, err = a.authenticate(oldAK, oldSK)
	require.Error(t, err, "the old pair is dead on the S3 header-auth path")
	assert.True(t, errors.Is(err, ErrAccessKeyRevoked), "typed as revoked (known-and-dead), got %v", err)
	assert.False(t, errors.Is(err, ErrUnknownAccessKey))

	tenantID, scope, err := a.authenticate(newKey.Key, newKey.Secret)
	require.NoError(t, err, "the new pair authenticates")
	assert.Equal(t, a.tenant.ID, tenantID)
	assert.Equal(t, []string{"*"}, scope.Permissions)

	ak, sk := a.tenantsPair()
	assert.Equal(t, newKey.Key, ak, "tenants.access_key mirrors the live primary")
	assert.Equal(t, newKey.Secret, sk)

	// The in-process cache in the SAME process: the old key is evicted.
	_, err = a.svc.ValidateS3Request(ctx, oldAK)
	assert.Error(t, err, "the old key is gone from the in-memory index")
	tn, err := a.svc.ValidateS3Request(ctx, newKey.Key)
	require.NoError(t, err)
	assert.Equal(t, newKey.Key, tn.AccessKey, "the in-memory tenant carries the new pair")

	// A restart (LoadFromDB) agrees.
	fresh := NewAuthService(nil, a.db)
	require.NoError(t, fresh.LoadFromDB(ctx))
	_, err = fresh.ValidateS3Request(ctx, oldAK)
	assert.Error(t, err, "after a reload the old key is not indexed")
	tn, err = fresh.ValidateS3Request(ctx, newKey.Key)
	require.NoError(t, err)
	assert.Equal(t, newKey.Secret, tn.SecretKey)
	keys, err := fresh.ListAPIKeys(ctx, a.user.ID)
	require.NoError(t, err)
	var primaries int
	for _, k := range keys {
		if k.IsPrimary && k.RevokedAt == nil {
			primaries++
			assert.Equal(t, newKey.Key, k.Key)
		}
	}
	assert.Equal(t, 1, primaries, "the listing shows one live primary")

	// The audit row.
	md := a.auditMetadata("key.rotated")
	assert.Equal(t, true, md["primary"])
	assert.Equal(t, float64(0), md["sts_tokens_invalidated"])
}

func TestPrimaryKey_RevokeIsRefused(t *testing.T) {
	a := newLifecycleAccount(t, "rev")

	err := a.svc.RevokeAPIKey(context.Background(), a.user.ID, a.key.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPrimaryKeyRevoke), "got %v", err)

	assert.Equal(t, 1, a.livePrimaries(), "the primary row is untouched")
	_, _, err = a.authenticate(a.key.Key, a.key.Secret)
	assert.NoError(t, err, "the account keeps its way in")
	var revoked sql.NullTime
	require.NoError(t, a.db.QueryRow(`SELECT revoked_at FROM api_keys WHERE id = $1`, a.key.ID).Scan(&revoked))
	assert.False(t, revoked.Valid)
}

// A scoped VLT_ key with `*` permissions is not the primary: it can be
// revoked like any other key, and revoking it never touches the primary.
func TestScopedFullAccessKey_IsNotThePrimary(t *testing.T) {
	a := newLifecycleAccount(t, "full")
	ctx := context.Background()
	full, err := a.svc.GenerateAPIKey(ctx, a.user.ID, "full", nil)
	require.NoError(t, err)
	assert.False(t, full.IsPrimary)

	require.NoError(t, a.svc.RevokeAPIKey(ctx, a.user.ID, full.ID))
	assert.Equal(t, 1, a.livePrimaries())
	_, _, err = a.authenticate(full.Key, full.Secret)
	assert.True(t, errors.Is(err, ErrAccessKeyRevoked), "got %v", err)
	_, _, err = a.authenticate(a.key.Key, a.key.Secret)
	assert.NoError(t, err)
}

func TestPrimaryKey_ConcurrentRotationOneWins(t *testing.T) {
	a := newLifecycleAccount(t, "race")
	ctx := context.Background()

	var wg sync.WaitGroup
	results := make([]error, 2)
	keys := make([]*APIKey, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keys[i], results[i] = a.svc.RotateAPIKey(ctx, a.user.ID, a.key.ID)
		}(i)
	}
	wg.Wait()

	var ok, refused int
	for i := range 2 {
		switch {
		case results[i] == nil:
			ok++
		case errors.Is(results[i], ErrKeyRevoked):
			refused++
		default:
			t.Fatalf("rotation %d: unexpected error %v", i, results[i])
		}
	}
	assert.Equal(t, 1, ok, "exactly one rotation wins")
	assert.Equal(t, 1, refused, "the other sees the key already revoked")
	assert.Equal(t, 1, a.livePrimaries(), "never two live primaries")
	ak, _ := a.tenantsPair()
	for i := range 2 {
		if results[i] == nil {
			assert.Equal(t, keys[i].Key, ak, "the mirror is the winner's pair")
		}
	}
}

func TestScopedKey_RevokeEvictsTheInProcessCache(t *testing.T) {
	a := newLifecycleAccount(t, "evict")
	ctx := context.Background()
	k, err := a.svc.GenerateAPIKey(ctx, a.user.ID, "ci", &KeyCreateOptions{Permissions: []string{"GetObject"}})
	require.NoError(t, err)
	_, err = a.svc.ValidateS3Request(ctx, k.Key)
	require.NoError(t, err)

	require.NoError(t, a.svc.RevokeAPIKey(ctx, a.user.ID, k.ID))

	_, err = a.svc.ValidateS3Request(ctx, k.Key)
	assert.Error(t, err, "a revoked key is not in the hot index of the same process")
	keys, _ := a.svc.ListAPIKeys(ctx, a.user.ID)
	var listed bool
	for _, l := range keys {
		if l.ID == k.ID {
			listed = true
			assert.NotNil(t, l.RevokedAt)
		}
	}
	assert.True(t, listed, "the listing still shows it as revoked")
}

// The old world, written by hand: a tenants pair whose mirror row is
// revoked. The pair must be refused — this is exactly the bug.
func TestLookupCredential_RevokedMirrorRowIsRefused(t *testing.T) {
	a := newLifecycleAccount(t, "mirror")
	_, err := a.db.Exec(`UPDATE api_keys SET revoked_at = NOW() WHERE id = $1`, a.key.ID)
	require.NoError(t, err)

	_, _, err = a.authenticate(a.key.Key, a.key.Secret)
	require.Error(t, err, "a tenants hit whose row is revoked is refused")
	assert.True(t, errors.Is(err, ErrAccessKeyRevoked), "got %v", err)
}

// A tenants pair with no api_keys row at all (nothing in prod has one — the
// read-only check in docs/reviews/WP-R5-14.md) is unknown: the tenants
// columns are a mirror, never a credential.
func TestLookupCredential_TenantsPairWithoutARowIsUnknown(t *testing.T) {
	a := newLifecycleAccount(t, "norow")
	_, err := a.db.Exec(`DELETE FROM api_keys WHERE id = $1`, a.key.ID)
	require.NoError(t, err)

	_, _, err = a.authenticate(a.key.Key, a.key.Secret)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnknownAccessKey), "got %v", err)
}

// A row with no tenant (a NULL tenant_id — nothing the product writes)
// never authenticates: fail closed, never "default".
func TestLookupCredential_RowWithoutTenantIsRefused(t *testing.T) {
	a := newLifecycleAccount(t, "notenant")
	_, err := a.db.Exec(`UPDATE api_keys SET tenant_id = NULL WHERE id = $1`, a.key.ID)
	require.NoError(t, err)

	tenantID, _, err := a.authenticate(a.key.Key, a.key.Secret)
	require.Error(t, err)
	assert.Empty(t, tenantID)
}

// WP-R5-9: the tenant comes from the row, not from users.email = tenants.email.
func TestScopedKey_SurvivesAnEmailChange(t *testing.T) {
	a := newLifecycleAccount(t, "email")
	ctx := context.Background()
	k, err := a.svc.GenerateAPIKey(ctx, a.user.ID, "ci", &KeyCreateOptions{Permissions: []string{"GetObject"}})
	require.NoError(t, err)
	_, err = a.db.Exec(`UPDATE users SET email = $1 WHERE id::text = $2`, "renamed-"+a.user.Email, a.user.ID)
	require.NoError(t, err)

	tenantID, scope, err := a.authenticate(k.Key, k.Secret)
	require.NoError(t, err)
	assert.Equal(t, a.tenant.ID, tenantID)
	assert.Equal(t, []string{"GetObject"}, scope.Permissions)
}

func TestKeyCap_ExcludesThePrimaryByRow(t *testing.T) {
	a := newLifecycleAccount(t, "cap")
	ctx := context.Background()
	_, err := a.db.Exec(`UPDATE tenant_quotas SET tier = 'free' WHERE tenant_id = $1`, a.tenant.ID)
	require.NoError(t, err)

	// After a rotation the primary is a VLT_ row; the cap must still not count it.
	_, err = a.svc.RotateAPIKey(ctx, a.user.ID, a.key.ID)
	require.NoError(t, err)
	extra, err := a.svc.GenerateAPIKey(ctx, a.user.ID, "one extra", nil)
	require.NoError(t, err, "the free tier allows one key beyond the primary")
	_, err = a.svc.GenerateAPIKey(ctx, a.user.ID, "two extra", nil)
	assert.True(t, errors.Is(err, ErrKeyLimitReached), "got %v", err)
	require.NoError(t, a.svc.RevokeAPIKey(ctx, a.user.ID, extra.ID))
	_, err = a.svc.GenerateAPIKey(ctx, a.user.ID, "again", nil)
	assert.NoError(t, err, "revoking frees the slot")
}

func TestPrimaryAPIKey_IsTheLiveRow(t *testing.T) {
	a := newLifecycleAccount(t, "prim")
	ctx := context.Background()
	p, err := a.svc.GetPrimaryAPIKey(ctx, a.tenant.ID)
	require.NoError(t, err)
	assert.Equal(t, a.key.Key, p.Key)
	assert.Empty(t, p.Secret, "the listing copy carries no secret")

	ak, sk, err := PrimaryPair(ctx, a.db, a.tenant.ID)
	require.NoError(t, err)
	assert.Equal(t, a.key.Key, ak)
	assert.Equal(t, a.key.Secret, sk)

	nk, err := a.svc.RotateAPIKey(ctx, a.user.ID, a.key.ID)
	require.NoError(t, err)
	p, err = a.svc.GetPrimaryAPIKey(ctx, a.tenant.ID)
	require.NoError(t, err)
	assert.Equal(t, nk.Key, p.Key)
	ak, sk, err = PrimaryPair(ctx, a.db, a.tenant.ID)
	require.NoError(t, err)
	assert.Equal(t, nk.Key, ak)
	assert.Equal(t, nk.Secret, sk)
}

// --- WP-R5-5: STS tokens die with their parent; IP restrictions only narrow.

func TestSTS_ParentRevokedKillsItsTokens(t *testing.T) {
	a := newLifecycleAccount(t, "stsrev")
	ctx := context.Background()
	parent, err := a.svc.GenerateAPIKey(ctx, a.user.ID, "ci", &KeyCreateOptions{Permissions: []string{"GetObject", "ListObjects"}})
	require.NoError(t, err)
	tok, err := GenerateSTSToken(ctx, a.db, a.tenant.ID, parent.Key,
		&KeyScope{Permissions: parent.Permissions}, STSRequest{Permissions: []string{"GetObject"}, TTL: 600})
	require.NoError(t, err)
	tenantID, scope, err := a.authenticate(tok.AccessKey, tok.SecretKey)
	require.NoError(t, err)
	assert.Equal(t, a.tenant.ID, tenantID)
	assert.True(t, scope.Temporary)

	require.NoError(t, a.svc.RevokeAPIKey(ctx, a.user.ID, parent.ID))

	_, _, err = a.authenticate(tok.AccessKey, tok.SecretKey)
	require.Error(t, err, "a token of a revoked parent is dead")
	assert.True(t, errors.Is(err, ErrAccessKeyRevoked), "got %v", err)
	md := a.auditMetadata("key.revoked")
	assert.Equal(t, float64(1), md["sts_tokens_invalidated"])
}

func TestSTS_PrimaryRotatedKillsItsTokens(t *testing.T) {
	a := newLifecycleAccount(t, "stsrot")
	ctx := context.Background()
	tok, err := GenerateSTSToken(ctx, a.db, a.tenant.ID, a.key.Key,
		&KeyScope{Permissions: []string{"*"}}, STSRequest{Permissions: []string{"GetObject"}, TTL: 600})
	require.NoError(t, err)
	_, _, err = a.authenticate(tok.AccessKey, tok.SecretKey)
	require.NoError(t, err)

	_, err = a.svc.RotateAPIKey(ctx, a.user.ID, a.key.ID)
	require.NoError(t, err)

	_, _, err = a.authenticate(tok.AccessKey, tok.SecretKey)
	require.Error(t, err, "a token minted from the primary dies with the rotation")
	assert.True(t, errors.Is(err, ErrAccessKeyRevoked), "got %v", err)
	md := a.auditMetadata("key.rotated")
	assert.Equal(t, float64(1), md["sts_tokens_invalidated"])
}

// A token whose parent row does not exist (a parent named by nothing) is
// refused: there is no authority left to bound it.
func TestSTS_TokenWithoutAParentRowIsRefused(t *testing.T) {
	a := newLifecycleAccount(t, "stsorphan")
	ctx := context.Background()
	tok, err := GenerateSTSToken(ctx, a.db, a.tenant.ID, "tenant:"+a.tenant.ID,
		&KeyScope{Permissions: []string{"*"}}, STSRequest{Permissions: []string{"GetObject"}, TTL: 600})
	require.NoError(t, err)

	_, _, err = a.authenticate(tok.AccessKey, tok.SecretKey)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAccessKeyRevoked), "got %v", err)
}

func TestSTS_IPRestrictOnlyNarrows(t *testing.T) {
	cases := []struct {
		name      string
		parent    []string
		requested []string
		want      []string
		wantErr   bool
	}{
		{"no restriction anywhere", nil, nil, nil, false},
		{"parent only", []string{"10.0.0.0/8"}, nil, []string{"10.0.0.0/8"}, false},
		{"request only", nil, []string{"1.2.3.4"}, []string{"1.2.3.4"}, false},
		{"request inside the parent", []string{"10.0.0.0/8"}, []string{"10.1.0.0/16", "10.2.3.4"}, []string{"10.1.0.0/16", "10.2.3.4"}, false},
		{"request wider than the parent is dropped", []string{"10.1.0.0/16"}, []string{"10.0.0.0/8", "10.1.5.0/24"}, []string{"10.1.5.0/24"}, false},
		{"request disjoint from the parent is refused", []string{"10.0.0.0/8"}, []string{"192.168.1.1"}, nil, true},
		{"the finding: a token cannot drop the parent's restriction", []string{"203.0.113.7"}, []string{"0.0.0.0/0"}, nil, true},
		{"single parent ip, same ip", []string{"203.0.113.7"}, []string{"203.0.113.7"}, []string{"203.0.113.7"}, false},
		{"single parent ip, a range around it is wider", []string{"203.0.113.7"}, []string{"203.0.113.0/24"}, nil, true},
		{"garbage in the request is refused", []string{"10.0.0.0/8"}, []string{"not-an-ip"}, nil, true},
		{"garbage in the request with no parent is refused", nil, []string{"10.0.0.0/33"}, nil, true},
		{"ipv6 inside", []string{"2001:db8::/32"}, []string{"2001:db8:1::/48"}, []string{"2001:db8:1::/48"}, false},
		{"ipv6 outside", []string{"2001:db8::/32"}, []string{"2001:db9::1"}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := intersectIPRestrict(tc.parent, tc.requested)
			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, errors.Is(err, ErrSTSScope), "got %v", err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSTS_TTLClampedToTheParentExpiry(t *testing.T) {
	soon := time.Now().Add(90 * time.Second)
	tok, err := GenerateSTSToken(context.Background(), nil, "tenant-x", "VLT_P",
		&KeyScope{Permissions: []string{"*"}, ExpiresAt: &soon}, STSRequest{Permissions: []string{"GetObject"}, TTL: 3600})
	require.NoError(t, err)
	assert.WithinDuration(t, soon, tok.ExpiresAt, time.Second, "a token never outlives its parent")

	_, err = GenerateSTSToken(context.Background(), nil, "tenant-x", "VLT_P",
		&KeyScope{Permissions: []string{"*"}, ExpiresAt: ptrTime(time.Now().Add(-time.Minute))}, STSRequest{Permissions: []string{"GetObject"}})
	require.Error(t, err, "an expired parent mints nothing")
	assert.True(t, errors.Is(err, ErrSTSScope), "got %v", err)
}

func ptrTime(t time.Time) *time.Time { return &t }

// --- WP-R5-10: JWTs die on a password change or reset; the issuer is checked.

func TestJWT_DiesOnPasswordChange(t *testing.T) {
	svc := NewAuthService(nil, nil)
	ctx := context.Background()
	user, _, _, err := svc.CreateUserWithTenant(ctx, "jwt-change@stored.ge", "first-password", "")
	require.NoError(t, err)
	old, err := svc.generateJWTAt(user, time.Now().Add(-2*time.Second))
	require.NoError(t, err)
	_, err = svc.ValidateJWT(old)
	require.NoError(t, err, "valid before the change")

	require.NoError(t, svc.ChangePassword(ctx, user.ID, "first-password", "second-password"))

	_, err = svc.ValidateJWT(old)
	require.Error(t, err, "a JWT issued before the password change is dead")
	assert.True(t, errors.Is(err, ErrJWTRevoked), "got %v", err)

	fresh, err := svc.GenerateJWT(user)
	require.NoError(t, err)
	claims, err := svc.ValidateJWT(fresh)
	require.NoError(t, err, "a JWT issued after the change is valid")
	assert.Equal(t, user.ID, claims.UserID)
}

func TestJWT_DiesOnPasswordReset(t *testing.T) {
	svc := NewAuthService(nil, nil)
	svc.SetVerifySecret("reset-secret-for-the-test")
	ctx := context.Background()
	user, _, _, err := svc.CreateUserWithTenant(ctx, "jwt-reset@stored.ge", "first-password", "")
	require.NoError(t, err)
	old, err := svc.generateJWTAt(user, time.Now().Add(-2*time.Second))
	require.NoError(t, err)

	token, err := svc.RequestPasswordReset(ctx, user.Email)
	require.NoError(t, err)
	_, err = svc.CompletePasswordReset(ctx, token, "second-password")
	require.NoError(t, err)

	_, err = svc.ValidateJWT(old)
	require.Error(t, err, "a reset by e-mail kills every JWT issued before it")
	assert.True(t, errors.Is(err, ErrJWTRevoked), "got %v", err)
}

func TestJWT_PasswordChangeIsPersistedAndSurvivesAReload(t *testing.T) {
	a := newLifecycleAccount(t, "jwtdb")
	ctx := context.Background()
	old, err := a.svc.generateJWTAt(a.user, time.Now().Add(-2*time.Second))
	require.NoError(t, err)
	require.NoError(t, a.svc.ChangePassword(ctx, a.user.ID, "a-long-password", "another-long-password"))
	var changed sql.NullTime
	require.NoError(t, a.db.QueryRow(`SELECT password_changed_at FROM users WHERE id::text = $1`, a.user.ID).Scan(&changed))
	assert.True(t, changed.Valid, "users.password_changed_at is stamped")

	fresh := NewAuthService(nil, a.db)
	fresh.SetJWTSecret(string(a.svc.jwtSecret))
	require.NoError(t, fresh.LoadFromDB(ctx))
	_, err = fresh.ValidateJWT(old)
	require.Error(t, err, "after a restart the old JWT is still dead")
	assert.True(t, errors.Is(err, ErrJWTRevoked), "got %v", err)
	md := a.auditMetadata("auth.password_changed")
	assert.NotEmpty(t, md["jwts_invalidated_before"])
}

func TestJWT_IssuerIsChecked(t *testing.T) {
	svc := NewAuthService(nil, nil)
	user, _, _, err := svc.CreateUserWithTenant(context.Background(), "jwt-iss@stored.ge", "first-password", "")
	require.NoError(t, err)

	claims := JWTClaims{UserID: user.ID, Email: user.Email, TenantID: user.TenantID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "someone-else",
		}}
	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(svc.jwtSecret)
	require.NoError(t, err)
	_, err = svc.ValidateJWT(forged)
	require.Error(t, err, "a token with another issuer is refused")
	assert.Contains(t, strings.ToLower(err.Error()), "issuer")

	claims.Issuer = "vaultaire"
	claims.IssuedAt = nil
	noIat, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(svc.jwtSecret)
	require.NoError(t, err)
	_, err = svc.ValidateJWT(noIat)
	require.Error(t, err, "a token without iat cannot be checked against a password change")
}

func TestJWT_UnknownUserIsRefused(t *testing.T) {
	svc := NewAuthService(nil, nil)
	ghost := &User{ID: "no-such-user", Email: "ghost@stored.ge", TenantID: "tenant-ghost"}
	tok, err := svc.GenerateJWT(ghost)
	require.NoError(t, err)
	_, err = svc.ValidateJWT(tok)
	require.Error(t, err, "a JWT for a user this process does not know (erased, evicted) is refused")
}
