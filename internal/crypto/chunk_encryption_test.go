package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/hkdf"
)

func newTestChunkEncryptionService(t *testing.T) *ChunkEncryptionService {
	t.Helper()
	masterHex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	km, err := NewKeyManager(&KeyManagerConfig{MasterKeyHex: masterHex, EnableCaching: true})
	require.NoError(t, err)
	return NewChunkEncryptionService(km)
}

func TestChunkEncryption_RoundTrip(t *testing.T) {
	svc := newTestChunkEncryptionService(t)
	tenantID := "tenant-aaa-111"
	plaintext := []byte("hello world this is some chunk data for testing")
	hash := sha256.Sum256(plaintext)
	plaintextHash := hex.EncodeToString(hash[:])

	ciphertext, ctHash, err := svc.EncryptChunkData(tenantID, plaintextHash, plaintext)
	require.NoError(t, err)
	assert.NotEmpty(t, ctHash)
	assert.NotEqual(t, plaintext, ciphertext)
	assert.Greater(t, len(ciphertext), len(plaintext))

	decrypted, err := svc.DecryptChunkData(tenantID, plaintextHash, ciphertext, ctHash)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

func TestChunkEncryption_Convergent(t *testing.T) {
	svc := newTestChunkEncryptionService(t)
	tenantID := "tenant-bbb-222"
	plaintext := []byte("same content produces same ciphertext")
	hash := sha256.Sum256(plaintext)
	plaintextHash := hex.EncodeToString(hash[:])

	ct1, hash1, err := svc.EncryptChunkData(tenantID, plaintextHash, plaintext)
	require.NoError(t, err)

	ct2, hash2, err := svc.EncryptChunkData(tenantID, plaintextHash, plaintext)
	require.NoError(t, err)

	assert.Equal(t, ct1, ct2, "same tenant + same content must produce identical ciphertext")
	assert.Equal(t, hash1, hash2)
}

func TestChunkEncryption_DifferentTenants(t *testing.T) {
	svc := newTestChunkEncryptionService(t)
	plaintext := []byte("shared content across tenants")
	hash := sha256.Sum256(plaintext)
	plaintextHash := hex.EncodeToString(hash[:])

	ct1, _, err := svc.EncryptChunkData("tenant-aaa", plaintextHash, plaintext)
	require.NoError(t, err)

	ct2, _, err := svc.EncryptChunkData("tenant-bbb", plaintextHash, plaintext)
	require.NoError(t, err)

	assert.NotEqual(t, ct1, ct2, "different tenants must produce different ciphertext")
}

func TestChunkEncryption_IntegrityCheck(t *testing.T) {
	svc := newTestChunkEncryptionService(t)
	tenantID := "tenant-ccc-333"
	plaintext := []byte("integrity test data")
	hash := sha256.Sum256(plaintext)
	plaintextHash := hex.EncodeToString(hash[:])

	ciphertext, _, err := svc.EncryptChunkData(tenantID, plaintextHash, plaintext)
	require.NoError(t, err)

	// Tamper with ciphertext
	tampered := make([]byte, len(ciphertext))
	copy(tampered, ciphertext)
	tampered[len(tampered)-1] ^= 0xFF

	_, err = svc.DecryptChunkData(tenantID, plaintextHash, tampered, "deadbeef")
	assert.Error(t, err, "tampered ciphertext with wrong hash must fail integrity check")

	// Correct hash but tampered data should fail GCM auth
	tamperedHash := sha256.Sum256(tampered)
	_, err = svc.DecryptChunkData(tenantID, plaintextHash, tampered, hex.EncodeToString(tamperedHash[:]))
	assert.Error(t, err, "tampered ciphertext must fail GCM decryption")
}

func TestChunkEncryption_DeterministicNonce(t *testing.T) {
	svc := newTestChunkEncryptionService(t)
	tenantID := "tenant-ddd-444"
	plaintext := []byte("nonce determinism check")
	hash := sha256.Sum256(plaintext)
	plaintextHash := hex.EncodeToString(hash[:])

	ct1, _, err := svc.EncryptChunkData(tenantID, plaintextHash, plaintext)
	require.NoError(t, err)

	ct2, _, err := svc.EncryptChunkData(tenantID, plaintextHash, plaintext)
	require.NoError(t, err)

	// Nonce is the first 12 bytes — must be identical for convergent encryption
	assert.Equal(t, ct1[:12], ct2[:12], "deterministic nonce must produce same prefix")
}

func TestChunkEncryption_EmptyData(t *testing.T) {
	svc := newTestChunkEncryptionService(t)
	tenantID := "tenant-eee-555"
	plaintext := []byte{}
	hash := sha256.Sum256(plaintext)
	plaintextHash := hex.EncodeToString(hash[:])

	ciphertext, ctHash, err := svc.EncryptChunkData(tenantID, plaintextHash, plaintext)
	require.NoError(t, err)
	assert.NotEmpty(t, ctHash)
	// 12 bytes nonce + 16 bytes GCM tag = 28 bytes overhead minimum
	assert.Equal(t, 28, len(ciphertext))

	decrypted, err := svc.DecryptChunkData(tenantID, plaintextHash, ciphertext, ctHash)
	require.NoError(t, err)
	assert.Empty(t, decrypted)
}

func TestChunkEncryption_DecryptWithoutHash(t *testing.T) {
	svc := newTestChunkEncryptionService(t)
	tenantID := "tenant-fff-666"
	plaintext := []byte("no hash verification")
	hash := sha256.Sum256(plaintext)
	plaintextHash := hex.EncodeToString(hash[:])

	ciphertext, _, err := svc.EncryptChunkData(tenantID, plaintextHash, plaintext)
	require.NoError(t, err)

	// Empty expectedCiphertextHash skips integrity check
	decrypted, err := svc.DecryptChunkData(tenantID, plaintextHash, ciphertext, "")
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

// TestChunkEncryption_NonceBoundToSealedBytes — R8-01. The convergent key is
// fixed by (tenant, plaintextHash), but the bytes actually sealed are the
// chunk AFTER the compression decision (raw or zstd, by the request's
// Content-Type and the zstd version). Two different messages under one key
// must never share a nonce (cipher.AEAD.Seal: "unique for all time, for a
// given key"; NIST SP 800-38D §8), so the nonce has to be derived from what
// is sealed, not from the plaintext identity.
func TestChunkEncryption_NonceBoundToSealedBytes(t *testing.T) {
	svc := newTestChunkEncryptionService(t)
	tenantID := "tenant-r8-nonce"
	chunk := []byte("the chunk, whose hash is the dedup identity")
	sum := sha256.Sum256(chunk)
	plaintextHash := hex.EncodeToString(sum[:])

	raw := chunk
	compressed := []byte("zstd(chunk) — a different message under the same convergent key")

	ctRaw, hRaw, err := svc.EncryptChunkData(tenantID, plaintextHash, raw)
	require.NoError(t, err)
	ctZ, hZ, err := svc.EncryptChunkData(tenantID, plaintextHash, compressed)
	require.NoError(t, err)

	assert.NotEqual(t, ctRaw[:12], ctZ[:12],
		"different sealed bytes under one convergent key must get different nonces")

	// Both still decrypt (the nonce travels with the blob).
	gotRaw, err := svc.DecryptChunkData(tenantID, plaintextHash, ctRaw, hRaw)
	require.NoError(t, err)
	assert.Equal(t, raw, gotRaw)
	gotZ, err := svc.DecryptChunkData(tenantID, plaintextHash, ctZ, hZ)
	require.NoError(t, err)
	assert.Equal(t, compressed, gotZ)

	// Convergence is preserved: the same sealed bytes give the same blob.
	ctRaw2, hRaw2, err := svc.EncryptChunkData(tenantID, plaintextHash, raw)
	require.NoError(t, err)
	assert.Equal(t, ctRaw, ctRaw2)
	assert.Equal(t, hRaw, hRaw2)
}

// TestChunkEncryption_LegacyV1BlobDecrypts — blobs sealed before R8-01 carry
// their (plaintext-hash-derived) nonce as the 12-byte prefix; decryption
// reads the nonce from the blob, so no stored chunk needs re-encrypting.
func TestChunkEncryption_LegacyV1BlobDecrypts(t *testing.T) {
	svc := newTestChunkEncryptionService(t)
	tenantID := "tenant-r8-legacy"
	plaintext := []byte("a chunk stored under the v1 nonce derivation")
	sum := sha256.Sum256(plaintext)
	plaintextHash := hex.EncodeToString(sum[:])

	key, err := svc.deriveConvergentKey(tenantID, plaintextHash)
	require.NoError(t, err)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)

	// The pre-R8 derivation: HKDF(convergentKey, salt = plaintextHash, info v1).
	nonce := make([]byte, gcm.NonceSize())
	_, err = io.ReadFull(hkdf.New(sha256.New, key, []byte(plaintextHash), []byte("vaultaire-chunk-nonce-v1")), nonce)
	require.NoError(t, err)
	legacy := append(append([]byte{}, nonce...), gcm.Seal(nil, nonce, plaintext, nil)...)
	legacySum := sha256.Sum256(legacy)

	got, err := svc.DecryptChunkData(tenantID, plaintextHash, legacy, hex.EncodeToString(legacySum[:]))
	require.NoError(t, err)
	assert.Equal(t, plaintext, got)
}

func TestChunkEncryption_OverheadIsTheConstant(t *testing.T) {
	// The chunked GET sizes its read buffer with ChunkEncryptionOverhead.
	svc := newTestChunkEncryptionService(t)
	for _, n := range []int{0, 1, 2 << 20} {
		data := make([]byte, n)
		sum := sha256.Sum256(data)
		ct, _, err := svc.EncryptChunkData("t", hex.EncodeToString(sum[:]), data)
		require.NoError(t, err)
		assert.Equal(t, n+ChunkEncryptionOverhead, len(ct))
	}
}
