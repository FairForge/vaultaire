package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

const (
	// SSEVersion is the blob format written since WP-R8-1:
	// [0x02][key_version uint32 BE][ML-KEM ct 1088][nonce 12][AES-GCM ct+tag],
	// the first five bytes bound to the ciphertext as AAD.
	SSEVersion byte = 0x02
	// SSEVersionV1 is the format before it ([0x01][KEM ct][nonce][ct], no
	// version, no AAD); such a blob was sealed under key version 1.
	SSEVersionV1       byte  = 0x01
	sseHeaderBytes           = 1 + 4
	SSEOverheadBytes         = sseHeaderBytes + mlkem.CiphertextSize768 + 12 + 16 // 1121
	sseV1OverheadBytes       = 1 + mlkem.CiphertextSize768 + 12 + 16              // 1117
	MaxEncryptableSize int64 = 256 << 20                                          // 256 MiB
	SSEAlgorithm             = "ML-KEM-768+AES-256-GCM"
)

// SSEService is whole-object SSE-S3: a fresh ML-KEM-768 encapsulation per
// object against the tenant's current key version, AES-256-GCM under the
// derived key. The decapsulation key is the version's random seed, wrapped
// under the master key in tenant_key_versions (TenantKeys, WP-R8-1).
type SSEService struct {
	keys *TenantKeys
}

// NewSSEService builds the service on the tenant key store.
func NewSSEService(keys *TenantKeys) *SSEService {
	return &SSEService{keys: keys}
}

// Current makes sure the tenant has a key and returns its current version
// (it was EnsureTenantKey; a PUT calls it before reading the body).
func (s *SSEService) Current(ctx context.Context, tenantID string) (int, error) {
	kv, err := s.keys.Current(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	return kv.Version, nil
}

// EncryptBytes seals plaintext under the tenant's current key version and
// returns the 0x02 blob and that version (the caller records it on the head
// row; it is also in the blob).
func (s *SSEService) EncryptBytes(ctx context.Context, tenantID string, plaintext []byte) ([]byte, int, error) {
	kv, err := s.keys.Current(ctx, tenantID)
	if err != nil {
		return nil, 0, fmt.Errorf("tenant key: %w", err)
	}

	sharedKey, kemCiphertext := kv.Encap.Encapsulate()

	aesKey, err := deriveSSEKey(sharedKey)
	if err != nil {
		return nil, 0, err
	}

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, 0, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, 0, fmt.Errorf("create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, 0, fmt.Errorf("generate nonce: %w", err)
	}

	buf := make([]byte, 0, SSEOverheadBytes+len(plaintext))
	buf = append(buf, SSEVersion)
	buf = binary.BigEndian.AppendUint32(buf, uint32(kv.Version))
	header := buf[:sseHeaderBytes]
	buf = append(buf, kemCiphertext...)
	buf = append(buf, nonce...)
	buf = gcm.Seal(buf, nonce, plaintext, header)

	return buf, kv.Version, nil
}

// SSEBlobKeyVersion reads the key version a blob was sealed under from its
// header: the 0x02 field, 1 for a 0x01 blob.
func SSEBlobKeyVersion(data []byte) (int, error) {
	switch {
	case len(data) >= SSEOverheadBytes && data[0] == SSEVersion:
		v := int(binary.BigEndian.Uint32(data[1:sseHeaderBytes]))
		if v < 1 {
			return 0, fmt.Errorf("SSE blob names key version %d", v)
		}
		return v, nil
	case len(data) >= sseV1OverheadBytes && data[0] == SSEVersionV1:
		return 1, nil
	case len(data) > 0 && data[0] != SSEVersion && data[0] != SSEVersionV1:
		return 0, fmt.Errorf("unsupported SSE version: 0x%02x", data[0])
	default:
		return 0, fmt.Errorf("encrypted data too short: %d bytes", len(data))
	}
}

// DecryptBytes opens a blob under the key version it records.
func (s *SSEService) DecryptBytes(ctx context.Context, tenantID string, data []byte) ([]byte, error) {
	version, err := SSEBlobKeyVersion(data)
	if err != nil {
		return nil, err
	}
	off, aad := 1, []byte(nil)
	if data[0] == SSEVersion {
		off, aad = sseHeaderBytes, data[:sseHeaderBytes]
	}
	kemCT := data[off : off+mlkem.CiphertextSize768]
	off += mlkem.CiphertextSize768
	nonce := data[off : off+12]
	off += 12
	aesCT := data[off:]

	kv, err := s.keys.Version(ctx, tenantID, version)
	if err != nil {
		return nil, fmt.Errorf("tenant key: %w", err)
	}

	sharedKey, err := kv.KEM.Decapsulate(kemCT)
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

	plaintext, err := gcm.Open(nil, nonce, aesCT, aad)
	if err != nil {
		return nil, fmt.Errorf("AES-GCM decryption failed: %w", err)
	}

	return plaintext, nil
}

func deriveSSEKey(sharedKey []byte) ([]byte, error) {
	r := hkdf.New(sha256.New, sharedKey, nil, []byte("vaultaire-sse-s3"))
	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, fmt.Errorf("HKDF key derivation: %w", err)
	}
	return key, nil
}
