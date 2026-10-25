package crypto

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// credentialGolden was computed outside Go (Python `cryptography` 45:
// HKDF-SHA256(chunk key, salt none, info "stored/credential/v1") → AES-256-GCM
// over the AAD "vaultaire-credential" ‖0‖ tenant ‖0‖ key id ‖0‖ version u32 BE),
// so the format is pinned by an independent implementation, not by our own
// output.
const credentialGolden = "016465666768696a6b6c6d6e6f6db390aacfe54075cb915f7201d2c208d6091a6016790a42ffc6290e01dcfc0ae82f029089bd1452f9f357c1ee4ed973937073fc6c47"

func goldenInputs() (chunk []byte, tenant, keyID string, version int, nonce, secret []byte) {
	chunk = make([]byte, 32)
	for i := range chunk {
		chunk[i] = byte(i)
	}
	nonce = make([]byte, 12)
	for i := range nonce {
		nonce[i] = byte(100 + i)
	}
	return chunk, "tenant-golden", "VK0123456789ABCDEF", 7, nonce, []byte("golden-secret-0123456789abcdefghijklmn")
}

func TestCredentialSeal_GoldenAndRefusals(t *testing.T) {
	chunk, tenant, keyID, version, nonce, secret := goldenInputs()

	t.Run("golden vector", func(t *testing.T) {
		got, err := sealCredential(chunk, tenant, keyID, version, nonce, secret)
		require.NoError(t, err)
		assert.Equal(t, credentialGolden, hex.EncodeToString(got))

		want, err := hex.DecodeString(credentialGolden)
		require.NoError(t, err)
		opened, err := openCredential(chunk, tenant, keyID, version, want)
		require.NoError(t, err)
		assert.Equal(t, secret, opened)
	})

	wrapped, err := hex.DecodeString(credentialGolden)
	require.NoError(t, err)
	flipped := func(i int) []byte {
		b := bytes.Clone(wrapped)
		b[i] ^= 0x01
		return b
	}
	otherChunk := bytes.Clone(chunk)
	otherChunk[0] ^= 0xff

	for name, tc := range map[string]struct {
		chunk         []byte
		tenant, keyID string
		version       int
		wrapped       []byte
	}{
		"another tenant":          {chunk, "tenant-golden2", keyID, version, wrapped},
		"a tenant prefix":         {chunk, "tenant-golde", keyID, version, wrapped},
		"another key id":          {chunk, tenant, "VK0123456789ABCDEG", version, wrapped},
		"another version":         {chunk, tenant, keyID, version + 1, wrapped},
		"another tenant's key":    {otherChunk, tenant, keyID, version, wrapped},
		"a flipped format byte":   {chunk, tenant, keyID, version, flipped(0)},
		"a flipped nonce byte":    {chunk, tenant, keyID, version, flipped(5)},
		"a flipped body byte":     {chunk, tenant, keyID, version, flipped(20)},
		"a flipped tag byte":      {chunk, tenant, keyID, version, flipped(len(wrapped) - 1)},
		"truncated":               {chunk, tenant, keyID, version, wrapped[:20]},
		"empty":                   {chunk, tenant, keyID, version, nil},
		"tenant/key id boundary":  {chunk, "tenant-golden\x00VK", "0123456789ABCDEF", version, wrapped},
		"key id/version boundary": {chunk, tenant, keyID + "\x00", version, wrapped},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			got, err := openCredential(tc.chunk, tc.tenant, tc.keyID, tc.version, tc.wrapped)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrCredentialOpen)
			assert.Nil(t, got)
		})
	}

	t.Run("a NUL in a name is refused (the separators stay unambiguous)", func(t *testing.T) {
		// "a\x00b" ‖ "c" and "a" ‖ "b\x00c" would give one AAD.
		_, err := sealCredential(chunk, "a\x00b", "c", version, nil, secret)
		assert.Error(t, err)
		_, err = sealCredential(chunk, "a", "b\x00c", version, nil, secret)
		assert.Error(t, err)
		_, err = openCredential(chunk, "a\x00b", "c", version, wrapped)
		assert.ErrorIs(t, err, ErrCredentialOpen)
		_, err = sealCredential(chunk, "", keyID, version, nil, secret)
		assert.Error(t, err, "an empty tenant")
		_, err = sealCredential(chunk, tenant, "", version, nil, secret)
		assert.Error(t, err, "an empty key id")
	})

	t.Run("seal draws a fresh nonce every time", func(t *testing.T) {
		a, err := sealCredential(chunk, tenant, keyID, version, nil, secret)
		require.NoError(t, err)
		b, err := sealCredential(chunk, tenant, keyID, version, nil, secret)
		require.NoError(t, err)
		assert.NotEqual(t, a[1:13], b[1:13])
		assert.NotContains(t, string(a), string(secret))
	})
}

// TestTenantKeys_CredentialRoundTripAndShred: the store's seam seals under
// the tenant's CURRENT version, opens under the version the row records
// (after a rotation too), refuses another tenant's row, and once the
// tenant's key rows are gone (the erasure's crypto-shred) nothing opens.
func TestTenantKeys_CredentialRoundTripAndShred(t *testing.T) {
	db := getTestDB(t)
	ctx := context.Background()
	keys := newTestKeys(t, db, testMasterA, "")
	tenant, other := keyTenant(t, db), keyTenant(t, db)
	secret := []byte("s3-secret-of-the-tenant-0123456789abcdef")

	wrapped, v, err := keys.SealCredential(ctx, tenant, "VKROUND", secret)
	require.NoError(t, err)
	assert.Equal(t, 1, v)
	opened, err := keys.OpenCredential(ctx, tenant, "VKROUND", v, wrapped)
	require.NoError(t, err)
	assert.Equal(t, secret, opened)

	// Rotation: new seals use v2, the v1 row still opens under v1.
	_, err = keys.Rotate(ctx, tenant)
	require.NoError(t, err)
	w2, v2, err := keys.SealCredential(ctx, tenant, "VKROUND", secret)
	require.NoError(t, err)
	assert.Equal(t, 2, v2)
	_, err = keys.OpenCredential(ctx, tenant, "VKROUND", v2, w2)
	require.NoError(t, err)
	_, err = keys.OpenCredential(ctx, tenant, "VKROUND", 1, wrapped)
	require.NoError(t, err)
	_, err = keys.OpenCredential(ctx, tenant, "VKROUND", 2, wrapped)
	assert.ErrorIs(t, err, ErrCredentialOpen, "a v1 wrap never opens as v2")

	// Another tenant (with a key of its own) cannot open it.
	_, _, err = keys.SealCredential(ctx, other, "VKOTHER", secret)
	require.NoError(t, err)
	_, err = keys.OpenCredential(ctx, other, "VKROUND", 1, wrapped)
	assert.ErrorIs(t, err, ErrCredentialOpen)

	// A master re-key leaves credential wraps untouched: they hang off the
	// chunk key, whose bytes the re-wrap keeps.
	rekeyed := newTestKeys(t, db, testMasterB, testMasterA)
	_, err = rekeyed.RewrapTenant(ctx, tenant)
	require.NoError(t, err)
	opened, err = rekeyed.OpenCredential(ctx, tenant, "VKROUND", 1, wrapped)
	require.NoError(t, err)
	assert.Equal(t, secret, opened)

	// The crypto-shred: the version rows go, the wrap is dead.
	_, err = db.Exec(`DELETE FROM tenant_key_versions WHERE tenant_id = $1`, tenant)
	require.NoError(t, err)
	fresh := newTestKeys(t, db, testMasterB, "")
	_, err = fresh.OpenCredential(ctx, tenant, "VKROUND", 1, wrapped)
	assert.ErrorIs(t, err, ErrNoTenantKey)
}
