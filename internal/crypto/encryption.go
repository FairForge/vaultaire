package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

// Encryptor provides encryption and decryption
type Encryptor interface {
	// Encrypt encrypts data with the given key
	Encrypt(key, plaintext []byte) (ciphertext []byte, nonce []byte, err error)

	// Decrypt decrypts data with the given key and nonce
	Decrypt(key, nonce, ciphertext []byte) (plaintext []byte, err error)

	// Algorithm returns the encryption algorithm
	Algorithm() EncryptionAlgorithm

	// KeySize returns the required key size in bytes
	KeySize() int

	// NonceSize returns the nonce size in bytes
	NonceSize() int
}

// AESGCMEncryptor implements Encryptor using AES-256-GCM
type AESGCMEncryptor struct{}

// NewAESGCMEncryptor creates a new AES-256-GCM encryptor
func NewAESGCMEncryptor() *AESGCMEncryptor {
	return &AESGCMEncryptor{}
}

func (e *AESGCMEncryptor) Algorithm() EncryptionAlgorithm { return EncryptionAESGCM }
func (e *AESGCMEncryptor) KeySize() int                   { return 32 } // 256 bits
func (e *AESGCMEncryptor) NonceSize() int                 { return 12 } // 96 bits (standard GCM)

func (e *AESGCMEncryptor) Encrypt(key, plaintext []byte) ([]byte, []byte, error) {
	if len(key) != e.KeySize() {
		return nil, nil, fmt.Errorf("invalid key size: got %d, want %d", len(key), e.KeySize())
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

func (e *AESGCMEncryptor) Decrypt(key, nonce, ciphertext []byte) ([]byte, error) {
	if len(key) != e.KeySize() {
		return nil, fmt.Errorf("invalid key size: got %d, want %d", len(key), e.KeySize())
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("invalid nonce size: got %d, want %d", len(nonce), gcm.NonceSize())
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decryption failed: %w", err)
	}

	return plaintext, nil
}

// ChaCha20Poly1305Encryptor implements Encryptor using ChaCha20-Poly1305
type ChaCha20Poly1305Encryptor struct{}

// NewChaCha20Poly1305Encryptor creates a new ChaCha20-Poly1305 encryptor
func NewChaCha20Poly1305Encryptor() *ChaCha20Poly1305Encryptor {
	return &ChaCha20Poly1305Encryptor{}
}

func (e *ChaCha20Poly1305Encryptor) Algorithm() EncryptionAlgorithm { return EncryptionChaCha }
func (e *ChaCha20Poly1305Encryptor) KeySize() int                   { return 32 } // 256 bits
func (e *ChaCha20Poly1305Encryptor) NonceSize() int                 { return 24 } // XChaCha20 uses 24-byte nonce

func (e *ChaCha20Poly1305Encryptor) Encrypt(key, plaintext []byte) ([]byte, []byte, error) {
	if len(key) != e.KeySize() {
		return nil, nil, fmt.Errorf("invalid key size: got %d, want %d", len(key), e.KeySize())
	}

	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext := aead.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

func (e *ChaCha20Poly1305Encryptor) Decrypt(key, nonce, ciphertext []byte) ([]byte, error) {
	if len(key) != e.KeySize() {
		return nil, fmt.Errorf("invalid key size: got %d, want %d", len(key), e.KeySize())
	}

	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	if len(nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("invalid nonce size: got %d, want %d", len(nonce), aead.NonceSize())
	}

	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decryption failed: %w", err)
	}

	return plaintext, nil
}

// NoopEncryptor is a pass-through encryptor (no encryption)
type NoopEncryptor struct{}

func NewNoopEncryptor() *NoopEncryptor                  { return &NoopEncryptor{} }
func (e *NoopEncryptor) Algorithm() EncryptionAlgorithm { return EncryptionNone }
func (e *NoopEncryptor) KeySize() int                   { return 0 }
func (e *NoopEncryptor) NonceSize() int                 { return 0 }
func (e *NoopEncryptor) Encrypt(_, plaintext []byte) ([]byte, []byte, error) {
	return plaintext, nil, nil
}
func (e *NoopEncryptor) Decrypt(_, _, ciphertext []byte) ([]byte, error) { return ciphertext, nil }

// NewEncryptorFromConfig creates an encryptor based on pipeline config
func NewEncryptorFromConfig(config PipelineConfig) (Encryptor, error) {
	if !config.EncryptionEnabled {
		return NewNoopEncryptor(), nil
	}

	switch config.EncryptionAlgo {
	case EncryptionAESGCM:
		return NewAESGCMEncryptor(), nil
	case EncryptionChaCha:
		return NewChaCha20Poly1305Encryptor(), nil
	case EncryptionNone, "":
		return NewNoopEncryptor(), nil
	default:
		return nil, fmt.Errorf("unsupported encryption algorithm: %s", config.EncryptionAlgo)
	}
}

// KeyDerivation handles key generation for different encryption modes

// DeriveConvergentKey derives a key from content hash (enables cross-tenant dedup)
// Uses HKDF-like construction: SHA256(tenantKey || contentHash)
func DeriveConvergentKey(tenantKey []byte, contentHash []byte) []byte {
	h := sha256.New()
	h.Write(tenantKey)
	h.Write(contentHash)
	return h.Sum(nil) // Returns 32 bytes (256 bits)
}

// GenerateRandomKey generates a random encryption key
func GenerateRandomKey(size int) ([]byte, error) {
	key := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("failed to generate random key: %w", err)
	}
	return key, nil
}

// GenerateTenantKey generates a new tenant master key
func GenerateTenantKey() ([]byte, error) {
	return GenerateRandomKey(32) // 256 bits
}

// EncryptedChunk holds an encrypted chunk with metadata
type EncryptedChunk struct {
	PlaintextHash  string              // Hash of original plaintext (for dedup lookup)
	CiphertextHash string              // Hash of encrypted data (for integrity)
	Ciphertext     []byte              // The encrypted data
	Nonce          []byte              // Nonce used for encryption
	Algorithm      EncryptionAlgorithm // Algorithm used
	KeyVersion     int                 // Version of the key used
}

// EncryptChunk encrypts a chunk and returns metadata
func EncryptChunk(encryptor Encryptor, key []byte, chunk *Chunk, keyVersion int) (*EncryptedChunk, error) {
	ciphertext, nonce, err := encryptor.Encrypt(key, chunk.Data)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt chunk: %w", err)
	}

	// Hash the ciphertext for integrity verification
	ciphertextHash := sha256.Sum256(ciphertext)

	return &EncryptedChunk{
		PlaintextHash:  chunk.Hash,
		CiphertextHash: fmt.Sprintf("%x", ciphertextHash),
		Ciphertext:     ciphertext,
		Nonce:          nonce,
		Algorithm:      encryptor.Algorithm(),
		KeyVersion:     keyVersion,
	}, nil
}

// DecryptChunk decrypts an encrypted chunk
func DecryptChunk(encryptor Encryptor, key []byte, encrypted *EncryptedChunk) ([]byte, error) {
	plaintext, err := encryptor.Decrypt(key, encrypted.Nonce, encrypted.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt chunk: %w", err)
	}
	return plaintext, nil
}

// EncryptionStats tracks encryption operations
type EncryptionStats struct {
	ChunksEncrypted int64
	BytesEncrypted  int64
	ChunksDecrypted int64
	BytesDecrypted  int64
}

func (s *EncryptionStats) AddEncrypted(bytes int64) {
	s.ChunksEncrypted++
	s.BytesEncrypted += bytes
}

func (s *EncryptionStats) AddDecrypted(bytes int64) {
	s.ChunksDecrypted++
	s.BytesDecrypted += bytes
}

// Overhead returns the encryption overhead (auth tag + nonce typically)
func EncryptionOverhead(algo EncryptionAlgorithm) int {
	switch algo {
	case EncryptionAESGCM:
		return 16 + 12 // 16-byte auth tag + 12-byte nonce
	case EncryptionChaCha:
		return 16 + 24 // 16-byte auth tag + 24-byte nonce (XChaCha20)
	default:
		return 0
	}
}
