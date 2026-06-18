package crypto

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
