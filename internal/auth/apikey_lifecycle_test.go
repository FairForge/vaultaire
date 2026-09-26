package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// R5-01: revoke / rotate / expiry must reach the database, because the S3
// auth path (Auth.lookupCredential) reads api_keys per request and never
// looks at the in-memory maps.

func newLifecycleUser(t *testing.T, svc *AuthService, email string) *User {
	t.Helper()
	user, _, _, err := svc.CreateUserWithTenant(context.Background(), email, "Str0ngPassw0rd!", "lifecycle")
	require.NoError(t, err)
	return user
}

func TestRevokeAPIKey_PersistsAndBlocksS3Auth(t *testing.T) {
	db := setupTestDB(t)
	svc := NewAuthService(nil, db)
	user := newLifecycleUser(t, svc, "revoke@stored.ge")
	key, err := svc.GenerateAPIKey(context.Background(), user.ID, "scoped", &KeyCreateOptions{Permissions: []string{"GetObject"}})
	require.NoError(t, err)

	a := NewAuth(db, zap.NewNop())
	cred, err := a.lookupCredential(key.Key)
	require.NoError(t, err)
	require.Equal(t, user.TenantID, cred.tenantID)

	require.NoError(t, svc.RevokeAPIKey(context.Background(), user.ID, key.ID))

	_, err = a.lookupCredential(key.Key)
	assert.Error(t, err, "a revoked key must not authenticate on the S3 path")

	// Survives a restart: a fresh service loading from the DB sees the revocation.
	fresh := NewAuthService(nil, db)
	require.NoError(t, fresh.LoadFromDB(context.Background()))
	keys, err := fresh.ListAPIKeys(context.Background(), user.ID)
	require.NoError(t, err)
	var found bool
	for _, k := range keys {
		if k.ID == key.ID {
			found = true
			assert.NotNil(t, k.RevokedAt, "revoked_at must be loaded from the DB")
		}
	}
	assert.True(t, found)

	// Revoking twice is an error, and the row is untouched.
	assert.Error(t, svc.RevokeAPIKey(context.Background(), user.ID, key.ID))
}

func TestRevokeAPIKey_OtherUsersKeyIsNotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewAuthService(nil, db)
	owner := newLifecycleUser(t, svc, "owner@stored.ge")
	other := newLifecycleUser(t, svc, "other@stored.ge")
	key, err := svc.GenerateAPIKey(context.Background(), owner.ID, "k", nil)
	require.NoError(t, err)

	assert.Error(t, svc.RevokeAPIKey(context.Background(), other.ID, key.ID))

	a := NewAuth(db, zap.NewNop())
	_, err = a.lookupCredential(key.Key)
	assert.NoError(t, err, "another user's revoke attempt must not touch the key")
}

func TestRotateAPIKey_PersistsNewKeyAndRevokesOld(t *testing.T) {
	db := setupTestDB(t)
	svc := NewAuthService(nil, db)
	user := newLifecycleUser(t, svc, "rotate@stored.ge")
	old, err := svc.GenerateAPIKey(context.Background(), user.ID, "k", &KeyCreateOptions{Permissions: []string{"GetObject", "PutObject"}, BucketScope: []string{"photos"}})
	require.NoError(t, err)

	rotated, err := svc.RotateAPIKey(context.Background(), user.ID, old.ID)
	require.NoError(t, err)
	require.NotEqual(t, old.Key, rotated.Key)

	a := NewAuth(db, zap.NewNop())
	_, err = a.lookupCredential(old.Key)
	assert.Error(t, err, "the rotated-away key must stop authenticating")

	cred, err := a.lookupCredential(rotated.Key)
	require.NoError(t, err, "the new key must be persisted so the S3 path sees it")
	assert.Equal(t, user.TenantID, cred.tenantID)
	assert.Equal(t, rotated.Secret, cred.secretKey)
	assert.ElementsMatch(t, []string{"GetObject", "PutObject"}, cred.scope.Permissions)
	assert.Equal(t, []string{"photos"}, cred.scope.BucketScope, "scope carries over on rotate")
}

func TestSetAPIKeyExpiration_PersistsAndAppliesToS3Auth(t *testing.T) {
	db := setupTestDB(t)
	svc := NewAuthService(nil, db)
	user := newLifecycleUser(t, svc, "expiry@stored.ge")
	key, err := svc.GenerateAPIKey(context.Background(), user.ID, "k", nil)
	require.NoError(t, err)

	past := time.Now().Add(-time.Minute)
	require.NoError(t, svc.SetAPIKeyExpiration(context.Background(), user.ID, key.ID, past))

	a := NewAuth(db, zap.NewNop())
	cred, err := a.lookupCredential(key.Key)
	require.NoError(t, err)
	require.NotNil(t, cred.scope.ExpiresAt, "expires_at must be persisted")
	assert.True(t, IsKeyExpired(cred.scope.ExpiresAt))
}

// R5-05: a permissions-only key (the common case) used to fail the INSERT with
// a NOT NULL violation because nil slices became SQL NULL.
func TestGenerateAPIKey_PermissionsOnlyPersists(t *testing.T) {
	db := setupTestDB(t)
	svc := NewAuthService(nil, db)
	user := newLifecycleUser(t, svc, "permonly@stored.ge")

	key, err := svc.GenerateAPIKey(context.Background(), user.ID, "ro", &KeyCreateOptions{Permissions: []string{"GetObject"}})
	require.NoError(t, err)

	a := NewAuth(db, zap.NewNop())
	cred, err := a.lookupCredential(key.Key)
	require.NoError(t, err)
	assert.Equal(t, []string{"GetObject"}, cred.scope.Permissions)
	assert.Empty(t, cred.scope.BucketScope)
	assert.Empty(t, cred.scope.IPAllowlist)
}

// R5-14: a permissions column that is not a JSON array of strings must not
// degrade to ["*"].
func TestLookupCredential_CorruptPermissionsFailClosed(t *testing.T) {
	db := setupTestDB(t)
	svc := NewAuthService(nil, db)
	user := newLifecycleUser(t, svc, "corrupt@stored.ge")
	key, err := svc.GenerateAPIKey(context.Background(), user.ID, "k", &KeyCreateOptions{Permissions: []string{"GetObject"}})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE api_keys SET permissions = '"not-an-array"'::jsonb WHERE id = $1`, key.ID)
	require.NoError(t, err)

	a := NewAuth(db, zap.NewNop())
	cred, err := a.lookupCredential(key.Key)
	require.NoError(t, err)
	assert.Empty(t, cred.scope.Permissions)
	assert.False(t, CheckPermission(cred.scope.Permissions, "GetObject"))
}
