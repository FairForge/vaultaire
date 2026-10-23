package e2ee

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// The account root (plan 10.0, Ente's hierarchy): a random master key made
// on the device, wrapped twice — under a key-encryption key derived from the
// password with Argon2id, and under a random recovery key the person keeps
// as 24 words. The server stores both wraps and can open neither. Every key
// below the master key (identity, spaces, objects) is wrapped under it, so
// a password change or a recovery re-wraps one key and nothing else.
//
//	0x04 password wrap   04 ‖ kdf(1) ‖ memory KiB(4) ‖ iterations(4) ‖ parallelism(1) ‖ salt(16) ‖ box
//	                      the box is 0x01 under the KEK, AD = label ‖ "master-wrap/kek" ‖ the 27-byte header
//	recovery wrap        0x01 box under the recovery key, AD = label ‖ "master-wrap/recovery"
//	recovery key wrap    0x01 box under the master key, AD = label ‖ "recovery-key" (shown again when signed in)
//	vault wrap           0x01 box under the vault key, AD = label ‖ "recovery-wrap/vault" (the K5 tie, D-P31-1)

const (
	FormatPasswordWrap = byte(0x04)

	// kdfArgon2id is the header's KDF byte: Argon2id, version 0x13
	// (RFC 9106), as golang.org/x/crypto/argon2 implements it.
	kdfArgon2id = byte(0x01)
	saltSize    = 16
	headerSize  = 1 + 1 + 4 + 4 + 1 + saltSize

	// MinMemoryKiB / MinIterations are the floor below which a password
	// wrap is neither made nor opened: Interactive. A server that hands a
	// device a wrap with weaker parameters cannot make it derive a weaker
	// key (Cure53's Ente finding was weak passwords; this is the same rule
	// for weak parameters).
	MinMemoryKiB  = 64 << 10
	MinIterations = 3
	// MaxMemoryKiB / MaxIterations / MaxParallelism bound what a wrap may
	// ask of a device, so a header from the server cannot exhaust it.
	MaxMemoryKiB   = 1 << 20
	MaxIterations  = 16
	MaxParallelism = 16

	// VaultSecretSize is the size of the secret the recovery vault (K5)
	// protects: the vault key, well inside its 128-byte bound.
	VaultSecretSize = KeySize

	labelMasterWrapKEK      = label + "master-wrap/kek"
	labelMasterWrapRecovery = label + "master-wrap/recovery"
	labelRecoveryKey        = label + "recovery-key"
	labelRecoveryWrapVault  = label + "recovery-wrap/vault"
)

// KDFParams are Argon2id's costs, recorded in the wrap's header so any
// device can open it.
type KDFParams struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
}

// The two profiles, chosen by measurement (docs/reviews/WP-P31.md): an M1
// Pro derives Sensitive in ~0.6 s natively and ~1.1 s in node/WASM at a
// ~350 MB peak; Interactive in ~0.14 s / ~0.27 s at ~145 MB. Parallelism is
// 1 because WebAssembly runs one thread: more lanes would not make a browser
// faster, only a native attacker.
var (
	// Sensitive is the default where the device can spare 256 MiB.
	Sensitive = KDFParams{MemoryKiB: 256 << 10, Iterations: 3, Parallelism: 1}
	// Interactive is the floor, for a low-memory browser: never below it.
	Interactive = KDFParams{MemoryKiB: MinMemoryKiB, Iterations: MinIterations, Parallelism: 1}
)

var (
	// ErrWeakParams: Argon2id parameters under the floor (or over the bound).
	ErrWeakParams = errors.New("e2ee: password parameters outside the allowed range")
	// ErrEmptyPassword: a password wrap needs a password.
	ErrEmptyPassword = errors.New("e2ee: empty password")
)

func (p KDFParams) check() error {
	if p.MemoryKiB < MinMemoryKiB || p.MemoryKiB > MaxMemoryKiB ||
		p.Iterations < MinIterations || p.Iterations > MaxIterations ||
		p.Parallelism < 1 || p.Parallelism > MaxParallelism {
		return ErrWeakParams
	}
	return nil
}

// deriveKEK is Argon2id(password, salt) → 32 bytes. It checks nothing: the
// callers do.
func deriveKEK(password, salt []byte, p KDFParams) []byte {
	return argon2.IDKey(password, salt, p.Iterations, p.MemoryKiB, p.Parallelism, KeySize)
}

// WrapMasterKeyWithPassword wraps the master key under Argon2id(password)
// with a fresh salt; the parameters travel in the header.
func WrapMasterKeyWithPassword(masterKey, password []byte, p KDFParams) ([]byte, error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("e2ee: salt: %w", err)
	}
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("e2ee: nonce: %w", err)
	}
	return wrapMasterKeyWithPassword(masterKey, password, p, salt, nonce)
}

func wrapMasterKeyWithPassword(masterKey, password []byte, p KDFParams, salt, nonce []byte) ([]byte, error) {
	if len(masterKey) != KeySize {
		return nil, ErrKeySize
	}
	if len(password) == 0 {
		return nil, ErrEmptyPassword
	}
	if err := p.check(); err != nil {
		return nil, err
	}
	hdr := make([]byte, 0, headerSize)
	hdr = append(hdr, FormatPasswordWrap, kdfArgon2id)
	hdr = binary.BigEndian.AppendUint32(hdr, p.MemoryKiB)
	hdr = binary.BigEndian.AppendUint32(hdr, p.Iterations)
	hdr = append(hdr, p.Parallelism)
	hdr = append(hdr, salt...)
	box, err := seal(FormatBox, deriveKEK(password, salt, p), nonce, masterKey, append([]byte(labelMasterWrapKEK), hdr...))
	if err != nil {
		return nil, err
	}
	return append(hdr, box...), nil
}

// PasswordWrapParams reads a wrap's parameters without deriving anything,
// so a client can say how long the next step takes, or refuse at once.
func PasswordWrapParams(wrapped []byte) (KDFParams, error) {
	if len(wrapped) < headerSize || wrapped[0] != FormatPasswordWrap || wrapped[1] != kdfArgon2id {
		return KDFParams{}, ErrFormat
	}
	return KDFParams{
		MemoryKiB:   binary.BigEndian.Uint32(wrapped[2:6]),
		Iterations:  binary.BigEndian.Uint32(wrapped[6:10]),
		Parallelism: wrapped[10],
	}, nil
}

// OpenMasterKeyWithPassword opens a password wrap. Parameters under the
// floor are refused before any derivation (ErrWeakParams).
func OpenMasterKeyWithPassword(wrapped, password []byte) ([]byte, error) {
	p, err := PasswordWrapParams(wrapped)
	if err != nil {
		return nil, err
	}
	if err := p.check(); err != nil {
		return nil, err
	}
	if len(password) == 0 {
		return nil, ErrEmptyPassword
	}
	hdr := wrapped[:headerSize]
	kek := deriveKEK(password, hdr[11:headerSize], p)
	return open(FormatBox, kek, wrapped[headerSize:], append([]byte(labelMasterWrapKEK), hdr...))
}

// NewRecoveryKey is a fresh recovery key (shown once as RecoveryMnemonic).
func NewRecoveryKey() ([]byte, error) { return RandomKey() }

// WrapMasterKeyWithRecovery wraps the master key under the recovery key.
func WrapMasterKeyWithRecovery(recoveryKey, masterKey []byte) ([]byte, error) {
	return Seal(recoveryKey, masterKey, []byte(labelMasterWrapRecovery))
}

// OpenMasterKeyWithRecovery opens it: the recovery path.
func OpenMasterKeyWithRecovery(recoveryKey, wrapped []byte) ([]byte, error) {
	return open(FormatBox, recoveryKey, wrapped, []byte(labelMasterWrapRecovery))
}

// WrapRecoveryKey wraps the recovery key under the master key (Ente stores
// both directions), so a signed-in device can show the words again.
func WrapRecoveryKey(masterKey, recoveryKey []byte) ([]byte, error) {
	return Seal(masterKey, recoveryKey, []byte(labelRecoveryKey))
}

// OpenRecoveryKey opens it.
func OpenRecoveryKey(masterKey, wrapped []byte) ([]byte, error) {
	return open(FormatBox, masterKey, wrapped, []byte(labelRecoveryKey))
}

// NewVaultKey is the secret a recovery vault registration protects (K5):
// random, VaultSecretSize bytes, handed to the K5 client as is. It opens
// one thing — the recovery key's vault wrap, which only our server stores —
// so a realm (which holds a share of it under the PIN) learns nothing it
// can open, and deleting the vault wrap ends every registration of it,
// even at a realm that failed to forget (D-P31-1).
func NewVaultKey() ([]byte, error) { return RandomKey() }

// WrapRecoveryKeyForVault wraps the recovery key under the vault key.
func WrapRecoveryKeyForVault(vaultKey, recoveryKey []byte) ([]byte, error) {
	return Seal(vaultKey, recoveryKey, []byte(labelRecoveryWrapVault))
}

// OpenRecoveryKeyFromVault opens it with the vault key the realms released.
func OpenRecoveryKeyFromVault(vaultKey, wrapped []byte) ([]byte, error) {
	return open(FormatBox, vaultKey, wrapped, []byte(labelRecoveryWrapVault))
}
