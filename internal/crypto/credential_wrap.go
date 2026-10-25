package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// S3 secrets wrapped under the tenant's key (Stage 1a Prompt 42, plan 43.7;
// docs/surfaces/TRUST.md item 12). SigV4 needs the secret itself, so
// api_keys.secret_key and sts_tokens.secret_key were plaintext and a dump of
// one table signed as every tenant. A wrapped secret opens only with the
// tenant's key version, which lives in tenant_key_versions under the master
// key (contract rule 4: no key outside the one hierarchy — this is a
// derivation of the version's chunk key, not a new key or a new purpose
// column; rule 5: nothing here spans tenants).
//
// SealCredential / OpenCredential are the ONE seam: the key broker (43.7,
// F-20) later replaces the in-process unwrap behind it.
//
// A master re-key (ENCRYPTION_MASTER_KEY_PREVIOUS) leaves these wraps
// untouched: it re-wraps the chunk key's bytes, which do not change. A tenant
// key rotation does change them: the key_reencrypt job re-wraps credentials
// below the tenant's newest version. The erasure's crypto-shred (deleting the
// tenant's version rows) kills them with everything else.

const (
	credentialFormat   byte = 0x01
	credentialInfo          = "stored/credential/v1"
	credentialAADLabel      = "vaultaire-credential"
	credentialNonceLen      = 12
)

// ErrCredentialOpen: a wrapped credential that does not open — another row's
// or tenant's, another version's, a damaged value. The caller refuses the
// request as its own failure; it never falls back to a plaintext column.
var ErrCredentialOpen = errors.New("wrapped credential does not open")

// SealCredential wraps an access key's secret under the tenant's current key
// version (creating version 1 for a tenant that has none, as a first write
// does). It returns the wrapped bytes and the version they need.
func (k *TenantKeys) SealCredential(ctx context.Context, tenantID, keyID string, secret []byte) ([]byte, int, error) {
	kv, err := k.Current(ctx, tenantID)
	if err != nil {
		return nil, 0, fmt.Errorf("seal credential of %s: %w", tenantID, err)
	}
	wrapped, err := sealCredential(kv.ChunkKey[:], tenantID, keyID, kv.Version, nil, secret)
	if err != nil {
		return nil, 0, fmt.Errorf("seal credential of %s: %w", tenantID, err)
	}
	return wrapped, kv.Version, nil
}

// OpenCredential opens a wrapped secret with the key version its row
// records. ErrNoTenantKey: the version is gone (the account was erased);
// ErrKeyUnwrap: the master key is wrong; ErrCredentialOpen: the bytes are
// not this row's.
func (k *TenantKeys) OpenCredential(ctx context.Context, tenantID, keyID string, version int, wrapped []byte) ([]byte, error) {
	kv, err := k.Version(ctx, tenantID, version)
	if err != nil {
		return nil, fmt.Errorf("open credential of %s: %w", tenantID, err)
	}
	secret, err := openCredential(kv.ChunkKey[:], tenantID, keyID, version, wrapped)
	if err != nil {
		return nil, fmt.Errorf("open credential of %s v%d: %w", tenantID, version, err)
	}
	return secret, nil
}

// sealCredential is the format: [0x01][nonce 12][AES-256-GCM ciphertext +
// tag] under HKDF-SHA256(chunk key, no salt, "stored/credential/v1"). A nil
// nonce draws a random one (the golden-vector test passes its own).
func sealCredential(chunkKey []byte, tenantID, keyID string, version int, nonce, secret []byte) ([]byte, error) {
	aad, err := credentialAAD(tenantID, keyID, version)
	if err != nil {
		return nil, err
	}
	gcm, err := credentialGCM(chunkKey)
	if err != nil {
		return nil, err
	}
	if nonce == nil {
		nonce = make([]byte, credentialNonceLen)
		if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
			return nil, fmt.Errorf("credential nonce: %w", err)
		}
	}
	if len(nonce) != credentialNonceLen {
		return nil, fmt.Errorf("credential nonce is %d bytes", len(nonce))
	}
	out := make([]byte, 0, 1+len(nonce)+len(secret)+gcm.Overhead())
	out = append(out, credentialFormat)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, secret, aad), nil
}

func openCredential(chunkKey []byte, tenantID, keyID string, version int, wrapped []byte) ([]byte, error) {
	aad, err := credentialAAD(tenantID, keyID, version)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCredentialOpen, err)
	}
	gcm, err := credentialGCM(chunkKey)
	if err != nil {
		return nil, err
	}
	if len(wrapped) < 1+credentialNonceLen+gcm.Overhead() || wrapped[0] != credentialFormat {
		return nil, fmt.Errorf("%w: bad format", ErrCredentialOpen)
	}
	nonce := wrapped[1 : 1+credentialNonceLen]
	secret, err := gcm.Open(nil, nonce, wrapped[1+credentialNonceLen:], aad)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCredentialOpen, err)
	}
	return secret, nil
}

// credentialAAD = "vaultaire-credential" ‖ 0 ‖ tenant ‖ 0 ‖ key id ‖ 0 ‖
// version (uint32 BE). A name holding a NUL is refused, so the separators
// stay unambiguous ("a\0b"‖"c" and "a"‖"b\0c" would give one AAD).
func credentialAAD(tenantID, keyID string, version int) ([]byte, error) {
	if tenantID == "" || keyID == "" {
		return nil, errors.New("credential: tenant and key id required")
	}
	if strings.IndexByte(tenantID, 0) >= 0 || strings.IndexByte(keyID, 0) >= 0 {
		return nil, errors.New("credential: a NUL in the tenant or key id")
	}
	if version < 1 {
		return nil, fmt.Errorf("credential: key version %d", version)
	}
	aad := make([]byte, 0, len(credentialAADLabel)+len(tenantID)+len(keyID)+7)
	aad = append(aad, credentialAADLabel...)
	aad = append(aad, 0)
	aad = append(aad, tenantID...)
	aad = append(aad, 0)
	aad = append(aad, keyID...)
	aad = append(aad, 0)
	return binary.BigEndian.AppendUint32(aad, uint32(version)), nil
}

func credentialGCM(chunkKey []byte) (cipher.AEAD, error) {
	if len(chunkKey) != 32 {
		return nil, fmt.Errorf("credential: chunk key is %d bytes", len(chunkKey))
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, chunkKey, nil, []byte(credentialInfo)), key); err != nil {
		return nil, fmt.Errorf("credential key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("credential cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credential GCM: %w", err)
	}
	return gcm, nil
}
