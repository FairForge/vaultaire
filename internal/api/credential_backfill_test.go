package api

import (
	"context"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// The key_reencrypt job's credential pass (Stage 1a Prompt 42 PR B, plan
// 43.7): rows written before 108, or by a server without a master key, are
// wrapped; rows below their tenant's newest key version are re-wrapped.

// credentialRunner is the re-encrypt job scoped to the fixture's tenant, on
// a key store under keyTestMasterA.
func (f *lifecycleFixture) credentialRunner(keys *crypto.TenantKeys) *KeyReencryptRunner {
	f.t.Helper()
	k := NewKeyReencryptRunner(f.db, f.srv.engine, crypto.NewGlobalContentIndex(f.db), crypto.NewChunkEncryptionService(keys), zap.NewNop())
	require.NotNil(f.t, k)
	k.onlyScope = f.tenant.ID
	k.JobName = "key_reencrypt_test_" + f.tenant.ID
	return k
}

// sabotagedSeal corrupts every seal: the job's re-open check must catch it.
type sabotagedSeal struct{ *crypto.TenantKeys }

func (s sabotagedSeal) SealCredential(ctx context.Context, tenantID, keyID string, secret []byte) ([]byte, int, error) {
	w, v, err := s.TenantKeys.SealCredential(ctx, tenantID, keyID, secret)
	if err == nil {
		w[len(w)-1] ^= 0x01
	}
	return w, v, err
}

func TestKeyReencrypt_BackfillWrapsVerifiesAndRaces(t *testing.T) {
	// The account and its keys were made by a server WITHOUT a master key:
	// plaintext only.
	f := setupLifecycleFixture(t)
	ctx := context.Background()
	extra, err := f.svc.GenerateAPIKey(ctx, f.user.ID, "extra", nil)
	require.NoError(t, err)
	// A revoked key (inserted: the plan's key cap allows one extra).
	revoked := struct{ Key string }{"VKREVOKED" + f.tenant.ID[7:15]}
	f.exec(`INSERT INTO api_keys (user_id, tenant_id, name, key_id, secret_hash, secret_key, revoked_at)
	        VALUES ($1, $2, 'revoked', $3, 'x', 'revoked-secret', NOW())`, f.user.ID, f.tenant.ID, revoked.Key)
	sts, err := auth.GenerateSTSToken(ctx, f.db, nil, f.tenant.ID, f.key.Key, &auth.KeyScope{Permissions: []string{"*"}}, auth.STSRequest{TTL: 600})
	require.NoError(t, err)
	f.exec(`INSERT INTO sts_tokens (access_key, secret_key, tenant_id, parent_key_id, expires_at)
	        VALUES ($1, 'expired-secret', $2, $3, NOW() - INTERVAL '1 minute')`, "ASIAEXPIRED"+f.tenant.ID[7:15], f.tenant.ID, f.key.Key)
	for _, k := range []string{f.key.Key, extra.Key, revoked.Key} {
		w, _, _ := f.wrappedRow("api_keys", "key_id", k)
		require.Empty(t, w)
	}

	keys := testTenantKeys(t, f.db, keyTestMasterA)
	k := f.credentialRunner(keys)

	// A seal that does not open back is caught before any write.
	k.credKeys = sabotagedSeal{keys}
	res, err := k.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, res.CredentialsWrapped)
	assert.Equal(t, 3, res.CredentialFailures, "the primary, extra and the live STS token: each refused by the re-open check")
	w, _, _ := f.wrappedRow("api_keys", "key_id", f.key.Key)
	assert.Empty(t, w, "nothing written on a failed check")

	// The budget: one row per run.
	k.credKeys = keys
	k.credentialBatch = 1
	res, err = k.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.CredentialsWrapped)

	// A lost race: another process wraps the next row between the scan and
	// the write. The job's guarded UPDATE matches nothing; the other wrap
	// stays and is not counted.
	k.credentialBatch = 1000
	var raced string
	k.beforeCredentialWrite = func(table, keyID string) {
		if raced != "" {
			return
		}
		raced = keyID
		w, v, err := keys.SealCredential(ctx, f.tenant.ID, keyID, []byte("someone-else"))
		require.NoError(t, err)
		f.exec(`UPDATE `+table+` SET secret_wrapped = $1, secret_key_version = $2 WHERE `+map[string]string{"api_keys": "key_id", "sts_tokens": "access_key"}[table]+` = $3`, w, v, keyID)
	}
	res, err = k.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.CredentialsWrapped, "two rows left, one lost to the race")
	assert.Equal(t, 1, res.CredentialsRaced)
	require.NotEmpty(t, raced)

	// What is left: the revoked key and the expired token are never wrapped
	// (they never authenticate); a third run finds nothing.
	k.beforeCredentialWrite = nil
	res, err = k.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, res.CredentialsWrapped)
	w, _, _ = f.wrappedRow("api_keys", "key_id", revoked.Key)
	assert.Empty(t, w)
	w, _, _ = f.wrappedRow("sts_tokens", "access_key", "ASIAEXPIRED"+f.tenant.ID[7:15])
	assert.Empty(t, w)

	// The job's wraps sign alone (the race's row aside — it holds another
	// secret on purpose).
	f.srv.tenantKeys = keys
	f.nullPlaintext()
	for _, pair := range [][2]string{{f.key.Key, f.key.Secret}, {extra.Key, extra.Secret}, {sts.AccessKey, sts.SecretKey}} {
		if pair[0] == raced {
			assert.Equal(t, ErrSignatureDoesNotMatch, f.put(pair[0], pair[1], "raced.txt"))
			continue
		}
		assert.Equal(t, "", f.put(pair[0], pair[1], "backfilled.txt"), pair[0])
	}
}

func TestKeyReencrypt_RotationRewrapsCredentials(t *testing.T) {
	f := setupLifecycleFixtureWith(t, keyTestMasterA)
	ctx := context.Background()
	sts, err := auth.GenerateSTSToken(ctx, f.db, f.srv.credentialKeys(), f.tenant.ID, f.key.Key, &auth.KeyScope{Permissions: []string{"*"}}, auth.STSRequest{TTL: 600})
	require.NoError(t, err)
	pending := func() int64 {
		var chunk, whole, cred int64
		require.NoError(t, f.db.QueryRow(keyRotationPendingSQL, crypto.SSEAlgorithm, f.tenant.ID).Scan(&chunk, &whole, &cred))
		return cred
	}
	assert.Equal(t, int64(0), pending())

	_, err = f.srv.tenantKeys.Rotate(ctx, f.tenant.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), pending(), "the primary and the token are below the newest version")

	// One damaged wrap is a failure, left as it is (it already answers 500).
	extra, err := f.svc.GenerateAPIKey(ctx, f.user.ID, "extra", nil) // wrapped under v2 already
	require.NoError(t, err)
	_, v, _ := f.wrappedRow("api_keys", "key_id", extra.Key)
	require.Equal(t, 2, *v)

	k := f.credentialRunner(f.srv.tenantKeys)
	res, err := k.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, res.CredentialsRewrapped)
	assert.Equal(t, 0, res.CredentialsWrapped)
	assert.Equal(t, int64(0), pending())
	for _, id := range []string{f.key.Key} {
		_, v, _ := f.wrappedRow("api_keys", "key_id", id)
		assert.Equal(t, 2, *v)
	}
	_, v, _ = f.wrappedRow("sts_tokens", "access_key", sts.AccessKey)
	assert.Equal(t, 2, *v)

	f.nullPlaintext()
	assert.Equal(t, "", f.put(f.key.Key, f.key.Secret, "rewrapped.txt"))
	assert.Equal(t, "", f.put(sts.AccessKey, sts.SecretKey, "rewrapped-sts.txt"))

	// A second rotation with a damaged v2 wrap: counted, untouched.
	_, err = f.srv.tenantKeys.Rotate(ctx, f.tenant.ID)
	require.NoError(t, err)
	f.exec(`UPDATE api_keys SET secret_wrapped = set_byte(secret_wrapped, 20, get_byte(secret_wrapped, 20) # 1) WHERE key_id = $1`, extra.Key)
	res, err = k.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.CredentialFailures)
	assert.Equal(t, 2, res.CredentialsRewrapped)
	_, v, _ = f.wrappedRow("api_keys", "key_id", extra.Key)
	assert.Equal(t, 2, *v, "a wrap that does not open is never rewritten")
}

// The erasure's crypto-shred kills a wrapped secret: a dump taken before the
// erasure holds bytes no key opens once the tenant's versions are gone.
func TestCredentialWrap_ErasureShredsTheWrappedSecret(t *testing.T) {
	f := setupLifecycleFixtureWith(t, keyTestMasterA)
	ctx := context.Background()
	dumped, version, _ := f.wrappedRow("api_keys", "key_id", f.key.Key)
	require.NotEmpty(t, dumped)
	opened, err := f.srv.tenantKeys.OpenCredential(ctx, f.tenant.ID, f.key.Key, *version, dumped)
	require.NoError(t, err)
	require.Equal(t, f.key.Secret, string(opened))

	acct := account.NewService(f.db, zap.NewNop())
	_, err = acct.Schedule(ctx, f.user.ID, f.tenant.ID, "test")
	require.NoError(t, err)
	_, err = acct.EraseRows(ctx, f.user.ID, f.tenant.ID, f.user.Email, time.Now().Add(31*24*time.Hour))
	require.NoError(t, err)

	fresh := testTenantKeys(t, f.db, keyTestMasterA) // no cache of the erased versions
	_, err = fresh.OpenCredential(ctx, f.tenant.ID, f.key.Key, *version, dumped)
	assert.ErrorIs(t, err, crypto.ErrNoTenantKey)
}
