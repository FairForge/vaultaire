package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// ChunkEncryptionService provides per-chunk convergent encryption.
// Same tenant + same key version + same content always produces the same
// ciphertext, enabling deduplication of encrypted chunks within a tenant.
//
// The tenant input of the convergent key is the RANDOM chunk key of one
// version of the tenant's key (TenantKeys, WP-R8-1): a chunk is sealed under
// the tenant's current version, and the version is recorded with the blob
// (global_content_index.encryption_key_version) so it is opened under the
// same one after any number of rotations.
type ChunkEncryptionService struct {
	keys *TenantKeys
}

// NewChunkEncryptionService creates a new service backed by the tenant key store.
func NewChunkEncryptionService(keys *TenantKeys) *ChunkEncryptionService {
	return &ChunkEncryptionService{keys: keys}
}

// Keys is the store this service seals under.
func (s *ChunkEncryptionService) Keys() *TenantKeys { return s.keys }

// EncryptChunkData seals a chunk under the tenant's CURRENT key version and
// returns that version with the blob, for the caller to record. The
// convergent key is SHA-256(chunkKey_v ‖ plaintext hash); the nonce is
// HKDF-derived from the key and the bytes being sealed. Returns ciphertext
// (nonce prepended), the ciphertext hash (SHA-256 of the encrypted blob, for
// integrity verification on read) and the key version.
//
// plaintextHash is the chunk's dedup identity (SHA-256 of the chunk BEFORE
// compression); data is what is actually sealed — the raw chunk or its zstd
// output, decided per request by Content-Type. The key is fixed by the
// identity, so it is the nonce that must separate the two possible messages
// (R8-01).
//
// Ciphertext format: [nonce (12B)][GCM ciphertext + tag (len(data)+16)]
// Overhead: ChunkEncryptionOverhead (28) bytes per chunk.
func (s *ChunkEncryptionService) EncryptChunkData(ctx context.Context, tenantID string, plaintextHash string, data []byte) ([]byte, string, int, error) {
	kv, err := s.keys.Current(ctx, tenantID)
	if err != nil {
		return nil, "", 0, fmt.Errorf("tenant key: %w", err)
	}
	ct, ctHash, err := sealChunk(kv, plaintextHash, data)
	if err != nil {
		return nil, "", 0, err
	}
	return ct, ctHash, kv.Version, nil
}

// EncryptChunkDataVersion seals under one given version (the re-encrypt job:
// it seals under the version it read from the table, never a cached one).
func (s *ChunkEncryptionService) EncryptChunkDataVersion(ctx context.Context, tenantID string, keyVersion int, plaintextHash string, data []byte) ([]byte, string, error) {
	kv, err := s.keys.Version(ctx, tenantID, keyVersion)
	if err != nil {
		return nil, "", fmt.Errorf("tenant key: %w", err)
	}
	return sealChunk(kv, plaintextHash, data)
}

func sealChunk(kv *KeyVersion, plaintextHash string, data []byte) ([]byte, string, error) {
	convergentKey := chunkConvergentKey(kv, plaintextHash)
	block, err := aes.NewCipher(convergentKey)
	if err != nil {
		return nil, "", fmt.Errorf("create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", fmt.Errorf("create GCM: %w", err)
	}

	nonce, err := deriveChunkNonce(convergentKey, data, gcm.NonceSize())
	if err != nil {
		return nil, "", fmt.Errorf("derive nonce: %w", err)
	}

	ciphertext := make([]byte, len(nonce), len(nonce)+len(data)+gcm.Overhead())
	copy(ciphertext, nonce)
	ciphertext = gcm.Seal(ciphertext, nonce, data, nil)

	hash := sha256.Sum256(ciphertext)
	return ciphertext, hex.EncodeToString(hash[:]), nil
}

// DecryptChunkData opens a chunk under the key version its blob was sealed
// with (the index row's, else the ref's). Verifies ciphertextHash before
// decryption to detect storage corruption.
func (s *ChunkEncryptionService) DecryptChunkData(ctx context.Context, tenantID string, keyVersion int, plaintextHash string, ciphertext []byte, expectedCiphertextHash string) ([]byte, error) {
	if expectedCiphertextHash != "" {
		hash := sha256.Sum256(ciphertext)
		if hex.EncodeToString(hash[:]) != expectedCiphertextHash {
			return nil, fmt.Errorf("ciphertext integrity check failed: stored blob is corrupt")
		}
	}

	kv, err := s.keys.Version(ctx, tenantID, keyVersion)
	if err != nil {
		return nil, fmt.Errorf("tenant key: %w", err)
	}

	block, err := aes.NewCipher(chunkConvergentKey(kv, plaintextHash))
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

// chunkConvergentKey = SHA-256(chunkKey_v ‖ plaintext hash bytes).
func chunkConvergentKey(kv *KeyVersion, plaintextHash string) []byte {
	hashBytes, err := hex.DecodeString(plaintextHash)
	if err != nil {
		hashBytes = []byte(plaintextHash)
	}
	return DeriveConvergentKey(kv.ChunkKey[:], hashBytes)
}

// deriveChunkNonce produces a deterministic nonce from the convergent key and the
// SHA-256 of the bytes that are about to be sealed.
//
// The convergent key is a function of (tenant, plaintext hash) only, while the
// sealed message may be the raw chunk or its compressed form — the compression
// decision depends on the request's Content-Type, on whether zstd shrank the
// chunk, and on the zstd version, none of which are part of the key. Deriving
// the nonce from the plaintext hash therefore reused one (key, nonce) pair for
// two distinct messages whenever two stores of the same chunk made different
// compression decisions (concurrent first-stores with different Content-Types,
// a re-store after GC, a zstd upgrade). GCM forfeits both confidentiality and
// authenticity on nonce reuse (NIST SP 800-38D §8; cipher.AEAD.Seal requires
// the nonce to be "unique for all time, for a given key"). Salting the HKDF
// with the hash of the sealed bytes makes the nonce a function of the message:
// identical messages still converge to identical blobs (dedup unaffected),
// distinct messages under one key get distinct nonces.
//
// Decryption reads the nonce from the blob prefix, so blobs sealed under the
// previous derivation (salt = plaintext hash) keep decrypting unchanged.
func deriveChunkNonce(convergentKey []byte, sealed []byte, nonceSize int) ([]byte, error) {
	salt := sha256.Sum256(sealed)
	hkdfReader := hkdf.New(sha256.New, convergentKey, salt[:], []byte("vaultaire-chunk-nonce-v2"))
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(hkdfReader, nonce); err != nil {
		return nil, fmt.Errorf("HKDF nonce derivation: %w", err)
	}
	return nonce, nil
}

// ChunkEncryptionOverhead is what EncryptChunkData adds to a chunk: the
// 12-byte nonce and the 16-byte GCM tag. A reader sizing a buffer for a
// stored chunk from its recorded size adds it.
const ChunkEncryptionOverhead = 12 + 16
