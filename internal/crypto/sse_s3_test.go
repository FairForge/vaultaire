package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/mlkem"
	"crypto/rand"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/lib/pq"
)

func newTestSSE(t *testing.T) (*SSEService, *TenantKeys, string) {
	t.Helper()
	db := getTestDB(t)
	keys := newTestKeys(t, db, testMasterA, "")
	return NewSSEService(keys), keys, keyTenant(t, db)
}

// legacyEncryptV2 is the 0x02 writer as it was before WP-R8-5 (EncryptBytes):
// kept as a fixture so the compatibility opener is tested against the bytes
// the old code wrote. No production code writes 0x02.
func legacyEncryptV2(ctx context.Context, s *SSEService, tenantID string, plaintext []byte) ([]byte, int, error) {
	kv, err := s.keys.Current(ctx, tenantID)
	if err != nil {
		return nil, 0, err
	}
	shared, kemCT := kv.Encap.Encapsulate()
	aesKey, err := deriveSSEKey(shared)
	if err != nil {
		return nil, 0, err
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, 0, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, 0, err
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, 0, err
	}
	buf := append([]byte{SSEVersion}, binary.BigEndian.AppendUint32(nil, uint32(kv.Version))...)
	header := append([]byte(nil), buf...)
	buf = append(buf, kemCT...)
	buf = append(buf, nonce...)
	return gcm.Seal(buf, nonce, plaintext, header), kv.Version, nil
}

// legacyEncryptV1 is a 0x01 blob as the code before WP-R8-1 wrote it (no
// version, no AAD), sealed against kv.
func legacyEncryptV1(t *testing.T, kv *KeyVersion, plaintext []byte) []byte {
	t.Helper()
	shared, kemCT := kv.Encap.Encapsulate()
	aesKey, err := deriveSSEKey(shared)
	require.NoError(t, err)
	block, err := aes.NewCipher(aesKey)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	legacy := append([]byte{SSEVersionV1}, kemCT...)
	legacy = append(legacy, nonce...)
	return gcm.Seal(legacy, nonce, plaintext, nil)
}

func TestSSEService_EncryptDecrypt_RoundTrip(t *testing.T) {
	svc, _, tenantID := newTestSSE(t)
	ctx := context.Background()

	v, err := svc.Current(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, 1, v)

	plaintext := []byte("hello, post-quantum world!")
	ciphertext, kv, err := legacyEncryptV2(ctx, svc, tenantID, plaintext)
	require.NoError(t, err)
	assert.Equal(t, 1, kv)

	decrypted, err := svc.DecryptBytes(ctx, tenantID, ciphertext)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

// WP-R8-1: a 0x02 blob names its key version (bound as AAD); a 0x01 blob —
// the format before — opens as version 1.
func TestSSES3_V2BlobCarriesVersion_V1BlobStillDecrypts(t *testing.T) {
	svc, keys, tenantID := newTestSSE(t)
	ctx := context.Background()

	v1blob, v, err := legacyEncryptV2(ctx, svc, tenantID, []byte("sealed under v1"))
	require.NoError(t, err)
	require.Equal(t, 1, v)
	_, err = keys.Rotate(ctx, tenantID)
	require.NoError(t, err)
	v2blob, v, err := legacyEncryptV2(ctx, svc, tenantID, []byte("sealed under v2"))
	require.NoError(t, err)
	require.Equal(t, 2, v)

	assert.Equal(t, SSEVersion, v2blob[0])
	assert.Equal(t, uint32(2), binary.BigEndian.Uint32(v2blob[1:5]), "the version is in the blob")
	got, err := SSEBlobKeyVersion(v2blob)
	require.NoError(t, err)
	assert.Equal(t, 2, got)
	got, err = SSEBlobKeyVersion(v1blob)
	require.NoError(t, err)
	assert.Equal(t, 1, got)

	for blob, want := range map[*[]byte]string{&v1blob: "sealed under v1", &v2blob: "sealed under v2"} {
		pt, err := svc.DecryptBytes(ctx, tenantID, *blob)
		require.NoError(t, err)
		assert.Equal(t, want, string(pt), "each blob opens under the version it records")
	}

	// The version field is authenticated: pointing a v2 blob at v1 fails.
	moved := append([]byte(nil), v2blob...)
	binary.BigEndian.PutUint32(moved[1:5], 1)
	_, err = svc.DecryptBytes(ctx, tenantID, moved)
	assert.Error(t, err)

	// A 0x01 blob as the old code wrote it (no version, no AAD), sealed
	// against the tenant's version 1 key.
	kv1, err := keys.Version(ctx, tenantID, 1)
	require.NoError(t, err)
	shared, kemCT := kv1.Encap.Encapsulate()
	aesKey, err := deriveSSEKey(shared)
	require.NoError(t, err)
	block, err := aes.NewCipher(aesKey)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	legacy := append([]byte{SSEVersionV1}, kemCT...)
	legacy = append(legacy, nonce...)
	legacy = gcm.Seal(legacy, nonce, []byte("written before 084"), nil)
	pt, err := svc.DecryptBytes(ctx, tenantID, legacy)
	require.NoError(t, err)
	assert.Equal(t, "written before 084", string(pt))
}

func TestSSEService_EncryptedFormat(t *testing.T) {
	svc, _, tenantID := newTestSSE(t)
	ct, _, err := legacyEncryptV2(context.Background(), svc, tenantID, []byte("format check"))
	require.NoError(t, err)

	assert.Equal(t, SSEVersion, ct[0])
	nonceStart := sseHeaderBytes + mlkem.CiphertextSize768
	assert.Len(t, ct[nonceStart:nonceStart+12], 12)
}

func TestSSEService_EncryptedSizeOverhead(t *testing.T) {
	svc, _, tenantID := newTestSSE(t)
	for _, size := range []int{0, 1, 100, 4096, 65536} {
		plaintext := make([]byte, size)
		_, _ = rand.Read(plaintext)

		ct, _, err := legacyEncryptV2(context.Background(), svc, tenantID, plaintext)
		require.NoError(t, err)
		assert.Equal(t, size+SSEOverheadBytes, len(ct), "overhead mismatch for plaintext size %d", size)
	}
}

func TestSSEService_DecryptWrongTenant(t *testing.T) {
	svc, _, tenant1 := newTestSSE(t)
	tenant2 := keyTenant(t, getTestDB(t))
	ctx := context.Background()
	_, err := svc.Current(ctx, tenant2)
	require.NoError(t, err)

	ct, _, err := legacyEncryptV2(ctx, svc, tenant1, []byte("secret data"))
	require.NoError(t, err)

	_, err = svc.DecryptBytes(ctx, tenant2, ct)
	require.Error(t, err)
}

func TestSSEService_DecryptCorruptedData(t *testing.T) {
	svc, _, tenantID := newTestSSE(t)
	ctx := context.Background()
	ct, _, err := legacyEncryptV2(ctx, svc, tenantID, []byte("original"))
	require.NoError(t, err)

	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-1] ^= 0xff
	_, err = svc.DecryptBytes(ctx, tenantID, tampered)
	require.Error(t, err)
}

func TestSSEService_DecryptBadVersion(t *testing.T) {
	svc, _, tenantID := newTestSSE(t)
	data := make([]byte, SSEOverheadBytes+32)
	data[0] = 0xFF
	_, err := svc.DecryptBytes(context.Background(), tenantID, data)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported SSE version")
}

func TestSSEService_DecryptTooShort(t *testing.T) {
	svc, _, tenantID := newTestSSE(t)
	_, err := svc.DecryptBytes(context.Background(), tenantID, []byte{SSEVersion, 0, 0})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too short")
}

func TestSSEService_DecryptShreddedKey(t *testing.T) {
	svc, keys, tenantID := newTestSSE(t)
	ctx := context.Background()
	ct, _, err := legacyEncryptV2(ctx, svc, tenantID, []byte("erased with the account"))
	require.NoError(t, err)

	_, err = getTestDB(t).Exec(`DELETE FROM tenant_key_versions WHERE tenant_id = $1`, tenantID)
	require.NoError(t, err)
	keys.Forget(tenantID)

	_, err = svc.DecryptBytes(ctx, tenantID, ct)
	assert.ErrorIs(t, err, ErrNoTenantKey, "the key rows are gone: the blob is unreadable for good")
}

func TestSSEService_EmptyPlaintext(t *testing.T) {
	svc, _, tenantID := newTestSSE(t)
	ctx := context.Background()
	ct, _, err := legacyEncryptV2(ctx, svc, tenantID, []byte{})
	require.NoError(t, err)
	assert.Equal(t, SSEOverheadBytes, len(ct))

	pt, err := svc.DecryptBytes(ctx, tenantID, ct)
	require.NoError(t, err)
	assert.Empty(t, pt)
}

func TestSSEOverheadConstant(t *testing.T) {
	assert.Equal(t, 1121, SSEOverheadBytes, "0x02: one version byte, four key-version bytes, the KEM ciphertext, nonce, tag")
	assert.Equal(t, 1117, sseV1OverheadBytes)
}

func TestDeriveSSEKey(t *testing.T) {
	key1, err := deriveSSEKey(make([]byte, 32))
	require.NoError(t, err)
	assert.Len(t, key1, 32)

	key2, err := deriveSSEKey(make([]byte, 32))
	require.NoError(t, err)
	assert.Equal(t, key1, key2, "same input should produce same output")

	differentInput := make([]byte, 32)
	differentInput[0] = 1
	key3, err := deriveSSEKey(differentInput)
	require.NoError(t, err)
	assert.NotEqual(t, key1, key3)
}
