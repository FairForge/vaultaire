package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// ChunkEncryptionService provides per-chunk convergent encryption.
// Same tenant + same content always produces the same ciphertext,
// enabling deduplication of encrypted chunks within a tenant.
type ChunkEncryptionService struct {
	km *KeyManager
}

// NewChunkEncryptionService creates a new service backed by the given KeyManager.
func NewChunkEncryptionService(km *KeyManager) *ChunkEncryptionService {
	return &ChunkEncryptionService{km: km}
}

// EncryptChunkData derives a convergent key from the tenant key + plaintext hash,
// then encrypts with AES-256-GCM using a deterministic nonce (HKDF-derived).
// Returns ciphertext (nonce prepended) and the ciphertext hash (SHA-256 of the
// encrypted blob, for integrity verification on read).
//
// Ciphertext format: [nonce (12B)][GCM ciphertext + tag (len(data)+16)]
// Overhead: 28 bytes per chunk.
func (s *ChunkEncryptionService) EncryptChunkData(tenantID string, plaintextHash string, data []byte) ([]byte, string, error) {
	convergentKey, err := s.deriveConvergentKey(tenantID, plaintextHash)
	if err != nil {
		return nil, "", fmt.Errorf("derive convergent key: %w", err)
	}

	block, err := aes.NewCipher(convergentKey)
	if err != nil {
		return nil, "", fmt.Errorf("create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", fmt.Errorf("create GCM: %w", err)
	}

	nonce, err := s.deriveNonce(convergentKey, plaintextHash, gcm.NonceSize())
	if err != nil {
		return nil, "", fmt.Errorf("derive nonce: %w", err)
	}

	ciphertext := make([]byte, len(nonce), len(nonce)+len(data)+gcm.Overhead())
	copy(ciphertext, nonce)
	ciphertext = gcm.Seal(ciphertext, nonce, data, nil)

	hash := sha256.Sum256(ciphertext)
	ciphertextHash := hex.EncodeToString(hash[:])

	return ciphertext, ciphertextHash, nil
}

// DecryptChunkData derives the same convergent key and decrypts.
// Verifies ciphertextHash before decryption to detect storage corruption.
func (s *ChunkEncryptionService) DecryptChunkData(tenantID string, plaintextHash string, ciphertext []byte, expectedCiphertextHash string) ([]byte, error) {
	if expectedCiphertextHash != "" {
		hash := sha256.Sum256(ciphertext)
		if hex.EncodeToString(hash[:]) != expectedCiphertextHash {
			return nil, fmt.Errorf("ciphertext integrity check failed: stored blob is corrupt")
		}
	}

	convergentKey, err := s.deriveConvergentKey(tenantID, plaintextHash)
	if err != nil {
		return nil, fmt.Errorf("derive convergent key: %w", err)
	}

	block, err := aes.NewCipher(convergentKey)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short: %d bytes", len(ciphertext))
	}

	nonce := ciphertext[:nonceSize]
	sealed := ciphertext[nonceSize:]

	plaintext, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("GCM decryption failed: %w", err)
	}

	return plaintext, nil
}

// deriveConvergentKey produces a 32-byte AES key deterministically from
// the tenant's master key and the chunk's plaintext hash.
func (s *ChunkEncryptionService) deriveConvergentKey(tenantID string, plaintextHash string) ([]byte, error) {
	tenantKey, err := s.km.DeriveTenantKey(tenantID, 1)
	if err != nil {
		return nil, fmt.Errorf("derive tenant key: %w", err)
	}

	hashBytes, err := hex.DecodeString(plaintextHash)
	if err != nil {
		hashBytes = []byte(plaintextHash)
	}

	return DeriveConvergentKey(tenantKey, hashBytes), nil
}

// deriveNonce produces a deterministic nonce from the convergent key and
// plaintext hash using HKDF. This is safe because the (key, nonce) pair
// is unique per distinct plaintext — reusing a nonce with a DIFFERENT
// plaintext under the same key is impossible by construction (convergent
// encryption guarantees same key ↔ same plaintext).
func (s *ChunkEncryptionService) deriveNonce(convergentKey []byte, plaintextHash string, nonceSize int) ([]byte, error) {
	hkdfReader := hkdf.New(sha256.New, convergentKey, []byte(plaintextHash), []byte("vaultaire-chunk-nonce-v1"))
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(hkdfReader, nonce); err != nil {
		return nil, fmt.Errorf("HKDF nonce derivation: %w", err)
	}
	return nonce, nil
}
