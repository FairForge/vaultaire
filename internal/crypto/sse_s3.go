package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/hkdf"
)

const (
	SSEVersion         byte  = 0x01
	SSEOverheadBytes         = 1 + mlkem.CiphertextSize768 + 12 + 16 // 1117
	MaxEncryptableSize int64 = 256 << 20                             // 256 MiB
	SSEAlgorithm             = "ML-KEM-768+AES-256-GCM"
)

type SSEService struct {
	db        *sql.DB
	masterKey []byte
	ekCache   sync.Map
	dkCache   sync.Map
}

func NewSSEService(db *sql.DB, masterKeyHex string) (*SSEService, error) {
	key, err := hex.DecodeString(masterKeyHex)
	if err != nil {
		return nil, fmt.Errorf("decode master key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(key))
	}
	return &SSEService{db: db, masterKey: key}, nil
}

func (s *SSEService) EnsureTenantKey(ctx context.Context, tenantID string) error {
	if _, ok := s.ekCache.Load(tenantID); ok {
		return nil
	}

	var exists bool
	err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM tenant_encryption_keys WHERE tenant_id = $1)",
		tenantID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check tenant key: %w", err)
	}
	if exists {
		return nil
	}

	dk, err := mlkem.GenerateKey768()
	if err != nil {
		return fmt.Errorf("generate ML-KEM key: %w", err)
	}

	seed := dk.Bytes()
	encryptedSeed, err := encryptSeed(s.masterKey, seed)
	if err != nil {
		return fmt.Errorf("encrypt seed: %w", err)
	}

	pubKey := dk.EncapsulationKey().Bytes()

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tenant_encryption_keys (tenant_id, seed, public_key)
		VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id) DO NOTHING`,
		tenantID, encryptedSeed, pubKey)
	if err != nil {
		return fmt.Errorf("store tenant key: %w", err)
	}

	s.ekCache.Store(tenantID, dk.EncapsulationKey())
	return nil
}

func (s *SSEService) EncryptBytes(ctx context.Context, tenantID string, plaintext []byte) ([]byte, error) {
	ek, err := s.loadEncapsulationKey(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	sharedKey, kemCiphertext := ek.Encapsulate()

	aesKey, err := deriveSSEKey(sharedKey)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	aesCiphertext := gcm.Seal(nil, nonce, plaintext, nil)

	buf := make([]byte, 0, 1+len(kemCiphertext)+len(nonce)+len(aesCiphertext))
	buf = append(buf, SSEVersion)
	buf = append(buf, kemCiphertext...)
	buf = append(buf, nonce...)
	buf = append(buf, aesCiphertext...)

	return buf, nil
}

func (s *SSEService) DecryptBytes(ctx context.Context, tenantID string, data []byte) ([]byte, error) {
	minLen := 1 + mlkem.CiphertextSize768 + 12 + 16
	if len(data) < minLen {
		return nil, fmt.Errorf("encrypted data too short: %d bytes", len(data))
	}

	if data[0] != SSEVersion {
		return nil, fmt.Errorf("unsupported SSE version: 0x%02x", data[0])
	}

	off := 1
	kemCT := data[off : off+mlkem.CiphertextSize768]
	off += mlkem.CiphertextSize768
	nonce := data[off : off+12]
	off += 12
	aesCT := data[off:]

	dk, err := s.loadDecapsulationKey(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	sharedKey, err := dk.Decapsulate(kemCT)
	if err != nil {
		return nil, fmt.Errorf("ML-KEM decapsulation failed: %w", err)
	}

	aesKey, err := deriveSSEKey(sharedKey)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, aesCT, nil)
	if err != nil {
		return nil, fmt.Errorf("AES-GCM decryption failed: %w", err)
	}

	return plaintext, nil
}

func (s *SSEService) loadEncapsulationKey(ctx context.Context, tenantID string) (*mlkem.EncapsulationKey768, error) {
	if cached, ok := s.ekCache.Load(tenantID); ok {
		return cached.(*mlkem.EncapsulationKey768), nil
	}

	var pubKeyBytes []byte
	err := s.db.QueryRowContext(ctx,
		"SELECT public_key FROM tenant_encryption_keys WHERE tenant_id = $1",
		tenantID).Scan(&pubKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("load tenant public key: %w", err)
	}

	ek, err := mlkem.NewEncapsulationKey768(pubKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse encapsulation key: %w", err)
	}

	s.ekCache.Store(tenantID, ek)
	return ek, nil
}

func (s *SSEService) loadDecapsulationKey(ctx context.Context, tenantID string) (*mlkem.DecapsulationKey768, error) {
	if cached, ok := s.dkCache.Load(tenantID); ok {
		return cached.(*mlkem.DecapsulationKey768), nil
	}

	var encSeed []byte
	err := s.db.QueryRowContext(ctx,
		"SELECT seed FROM tenant_encryption_keys WHERE tenant_id = $1",
		tenantID).Scan(&encSeed)
	if err != nil {
		return nil, fmt.Errorf("load tenant seed: %w", err)
	}

	seed, err := decryptSeed(s.masterKey, encSeed)
	if err != nil {
		return nil, fmt.Errorf("decrypt tenant seed: %w", err)
	}

	dk, err := mlkem.NewDecapsulationKey768(seed)
	if err != nil {
		return nil, fmt.Errorf("reconstruct decapsulation key: %w", err)
	}

	s.dkCache.Store(tenantID, dk)
	return dk, nil
}

func deriveSSEKey(sharedKey []byte) ([]byte, error) {
	r := hkdf.New(sha256.New, sharedKey, nil, []byte("vaultaire-sse-s3"))
	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, fmt.Errorf("HKDF key derivation: %w", err)
	}
	return key, nil
}

func encryptSeed(masterKey, seed []byte) ([]byte, error) {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("create seed cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create seed GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate seed nonce: %w", err)
	}
	ct := gcm.Seal(nil, nonce, seed, nil)
	return append(nonce, ct...), nil
}

func decryptSeed(masterKey, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("create seed cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create seed GCM: %w", err)
	}
	if len(data) < gcm.NonceSize()+16 {
		return nil, fmt.Errorf("encrypted seed too short: %d bytes", len(data))
	}
	nonce := data[:gcm.NonceSize()]
	ct := data[gcm.NonceSize():]
	seed, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt seed: %w", err)
	}
	return seed, nil
}
