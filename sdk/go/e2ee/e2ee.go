// Package e2ee is the client-side key core's cryptography (keystone K2,
// plan Phase 10.0.1, decision F-12): the constructions every Private
// surface uses and the key hierarchy built from them (K2b), compiled
// unchanged for native Go (CLI, SDK, server tests) and for the browser
// (sdk/wasm → sdk/js/e2ee). Nothing here runs on the server
// for customer data: Private mode means these keys exist only on the device.
//
// Nothing is hand-written. The cipher is XChaCha20-Poly1305
// (golang.org/x/crypto/chacha20poly1305 NewX — the 192-bit nonce makes a
// random nonce safe for any number of messages, and it is fast without AES
// hardware: phones, WASM); key derivation is HKDF-SHA256 (crypto/hkdf); the
// sealed box is HPKE (RFC 9180, crypto/hpke) with the post-quantum hybrid
// KEM MLKEM768-X25519 (X-Wing, draft-ietf-hpke-pq, KEM 0x647a), HKDF-SHA256
// and ChaCha20-Poly1305 — the suite age ≥ 1.3 uses for its age1pq
// recipients.
//
// Every output starts with one format byte, authenticated:
//
//	0x01 box     XChaCha20-Poly1305, random nonce:  01 ‖ nonce(24) ‖ ciphertext ‖ tag(16)
//	              keys, metadata, manifests (AD = the caller's purpose)
//	0x02 chunk   XChaCha20-Poly1305, convergent:   02 ‖ nonce(24) ‖ ciphertext ‖ tag(16)
//	              key = ChunkKey(space key, SHA-256(chunk)); nonce derived
//	              from the key AND the sealed bytes (R8-01); named to the
//	              server only by ChunkID (keyed), opened only against the
//	              manifest's hash
//	0x03 sealed  HPKE base mode, X-Wing:          03 ‖ enc(1120) ‖ ciphertext ‖ tag(16)
//	              sharing a key to a member's public key (F-12 change 2);
//	              HPKE info = label ‖ 03 ‖ the caller's info
//	0x04 password wrap  header (Argon2id parameters, salt) ‖ 0x01 box under
//	              the KEK (account.go)
//	0x05 share   a 0x03 sealed space key + the sender's signature over it
//	              (share.go)
//
// Above them, the hierarchy's wraps are 0x01 boxes whose AD names the role
// (and the space, the object, the version): master key ← password KEK and
// recovery key; recovery key ← master key and vault key; identity ← master
// key; space key ← master key (owner) or a 0x05 share (member); object key ←
// space key; manifest ← object key; chunk ← its convergent chunk key, named
// in the manifest.
//
// A change to any of these bytes is a new format byte, never a silent change:
// convergent chunks are a dedup contract (TestChunk_… pins one).
package e2ee

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	// KeySize is every symmetric key's size (master, space, object, chunk).
	KeySize = chacha20poly1305.KeySize
	// MaxChunk is the frozen chunker's largest chunk (WP-R8-4: 16 MiB): a
	// chunk is sealed in one call, so it is the most one seal holds.
	MaxChunk = 16 << 20
	// PublicKeySize / EncapsulationSize: X-Wing's (draft-ietf-hpke-pq).
	PublicKeySize     = 1216
	EncapsulationSize = 1120

	FormatBox    = byte(0x01)
	FormatChunk  = byte(0x02)
	FormatSealed = byte(0x03)

	label         = "stored.cloud/e2ee/v1/"
	labelChunkKey = label + "chunk-key"
	labelNonce    = label + "chunk-nonce"
	labelChunkID  = label + "chunk-id"
	labelSealed   = label + "sealed/"
)

var (
	// ErrOpen: the bytes do not open with this key, purpose or recipient
	// (wrong key, tampered, another purpose — never said which).
	ErrOpen = errors.New("e2ee: message does not open")
	// ErrKeySize: a symmetric key that is not 32 bytes.
	ErrKeySize = errors.New("e2ee: a key is 32 bytes")
	// ErrChunkTooLarge: a chunk larger than MaxChunk.
	ErrChunkTooLarge = errors.New("e2ee: chunk larger than 16 MiB")
	// ErrFormat: not this package's envelope, or another format byte.
	ErrFormat = errors.New("e2ee: unknown or malformed envelope")
	// ErrChunkMismatch: a chunk that opens under its key but is not the
	// chunk the key and the manifest name.
	ErrChunkMismatch = errors.New("e2ee: chunk is not the one its manifest names")
)

// RandomKey is a fresh 256-bit key (a master, space, object or paranoid
// chunk key).
func RandomKey() ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("e2ee: random key: %w", err)
	}
	return k, nil
}

func xchachaSeal(key, nonce, plaintext, ad []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, ErrKeySize
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("e2ee: %w", err)
	}
	return aead.Seal(nil, nonce, plaintext, ad), nil
}

// seal writes format ‖ nonce ‖ XChaCha20-Poly1305(plaintext), the format
// byte prepended to the associated data.
func seal(format byte, key, nonce, plaintext, ad []byte) ([]byte, error) {
	ct, err := xchachaSeal(key, nonce, plaintext, append([]byte{format}, ad...))
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(nonce)+len(ct))
	out = append(out, format)
	out = append(out, nonce...)
	return append(out, ct...), nil
}

func open(format byte, key, msg, ad []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, ErrKeySize
	}
	ns := chacha20poly1305.NonceSizeX
	if len(msg) < 1+ns+chacha20poly1305.Overhead || msg[0] != format {
		return nil, ErrFormat
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("e2ee: %w", err)
	}
	pt, err := aead.Open(nil, msg[1:1+ns], msg[1+ns:], append([]byte{format}, ad...))
	if err != nil {
		return nil, ErrOpen
	}
	return pt, nil
}

// Seal is the box: XChaCha20-Poly1305 under key with a random 192-bit nonce.
// ad names the purpose (and the parent) so a sealed key cannot be moved to
// another role.
func Seal(key, plaintext, ad []byte) ([]byte, error) {
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("e2ee: nonce: %w", err)
	}
	return seal(FormatBox, key, nonce, plaintext, ad)
}

// Open opens a box.
func Open(key, msg, ad []byte) ([]byte, error) { return open(FormatBox, key, msg, ad) }

// ChunkKey is a chunk's convergent key in a space (F-12 change 1):
// HKDF-SHA256(space key, info = label ‖ SHA-256 of the chunk). The same
// chunk in the same space has the same key; two spaces never share one —
// so their bytes and ids never converge. Their LENGTHS do (len + 41, and the
// chunker is public): F-35.
func ChunkKey(spaceKey, chunkHash []byte) ([]byte, error) {
	if len(spaceKey) != KeySize {
		return nil, ErrKeySize
	}
	if len(chunkHash) != sha256.Size {
		return nil, errors.New("e2ee: a chunk hash is SHA-256 (32 bytes)")
	}
	k, err := hkdf.Key(sha256.New, spaceKey, nil, labelChunkKey+string(chunkHash), KeySize)
	if err != nil {
		return nil, fmt.Errorf("e2ee: chunk key: %w", err)
	}
	return k, nil
}

// SealChunk seals a chunk deterministically: the nonce is
// HMAC-SHA256(chunk key, label ‖ plaintext) cut to 192 bits, so the same
// bytes under the same key are the same ciphertext (dedup) and two
// different messages under one key never share a nonce. With a RandomKey
// it is the paranoid flag's sealing (no dedup).
func SealChunk(chunkKey, plaintext []byte) ([]byte, error) {
	if len(plaintext) > MaxChunk {
		return nil, ErrChunkTooLarge
	}
	if len(chunkKey) != KeySize {
		return nil, ErrKeySize
	}
	m := hmac.New(sha256.New, chunkKey)
	m.Write([]byte(labelNonce))
	m.Write(plaintext)
	nonce := m.Sum(nil)[:chacha20poly1305.NonceSizeX]
	return seal(FormatChunk, chunkKey, nonce, plaintext, nil)
}

// OpenChunk opens a chunk and checks that its plaintext is the chunk the
// key was derived for: SHA-256(plaintext) must equal expectedHash (the
// manifest's entry). A space member who sealed other bytes under another
// chunk's key — to poison the dedup of everyone in the space who later
// writes that chunk — is refused with ErrChunkMismatch, never served.
func OpenChunk(chunkKey, msg, expectedHash []byte) ([]byte, error) {
	if len(expectedHash) != sha256.Size {
		return nil, errors.New("e2ee: a chunk hash is SHA-256 (32 bytes)")
	}
	pt, err := open(FormatChunk, chunkKey, msg, nil)
	if err != nil {
		return nil, err
	}
	if h := sha256.Sum256(pt); subtle.ConstantTimeCompare(h[:], expectedHash) != 1 {
		return nil, ErrChunkMismatch
	}
	return pt, nil
}

// ChunkID is the only identifier of a chunk that leaves the device:
// HMAC-SHA256(HKDF-SHA256(space key, info = label ‖ "chunk-id"), SHA-256 of
// the chunk). It is stable within a space (the server dedups by it) and
// keyed, so the server cannot test it against the hash of a file it knows,
// and two spaces holding one file show it under two unrelated ids (RP37-15b).
// SHA-256 of a chunk itself never leaves the device.
func ChunkID(spaceKey, chunkHash []byte) ([]byte, error) {
	if len(spaceKey) != KeySize {
		return nil, ErrKeySize
	}
	if len(chunkHash) != sha256.Size {
		return nil, errors.New("e2ee: a chunk hash is SHA-256 (32 bytes)")
	}
	idKey, err := hkdf.Key(sha256.New, spaceKey, nil, labelChunkID, KeySize)
	if err != nil {
		return nil, fmt.Errorf("e2ee: chunk id: %w", err)
	}
	m := hmac.New(sha256.New, idKey)
	m.Write(chunkHash)
	return m.Sum(nil), nil
}

// The sealed box's suite.
var (
	kem  = hpke.MLKEM768X25519()
	kdf  = hpke.HKDFSHA256()
	aead = hpke.ChaCha20Poly1305()
)

// GenerateKeyPair makes a sealed-box key pair: priv is X-Wing's 32-byte
// seed (store it under the master key, in a box), pub the 1216-byte public
// key (the one plaintext key the server holds).
func GenerateKeyPair() (priv, pub []byte, err error) {
	k, err := kem.GenerateKey()
	if err != nil {
		return nil, nil, fmt.Errorf("e2ee: generate key pair: %w", err)
	}
	priv, err = k.Bytes()
	if err != nil {
		return nil, nil, fmt.Errorf("e2ee: generate key pair: %w", err)
	}
	return priv, k.PublicKey().Bytes(), nil
}

// PublicKey is priv's public key.
func PublicKey(priv []byte) ([]byte, error) {
	k, err := kem.NewPrivateKey(priv)
	if err != nil {
		return nil, errors.New("e2ee: not an X-Wing private key")
	}
	return k.PublicKey().Bytes(), nil
}

// SealTo seals plaintext to a public key (HPKE base mode, one message):
// info names the purpose (HPKE's info is this package's label ‖ 0x03 ‖
// info), ad is bound as associated data.
func SealTo(pub, plaintext, info, ad []byte) ([]byte, error) {
	pk, err := kem.NewPublicKey(pub)
	if err != nil {
		return nil, errors.New("e2ee: not an X-Wing public key")
	}
	enc, s, err := hpke.NewSender(pk, kdf, aead, sealedInfo(info))
	if err != nil {
		return nil, fmt.Errorf("e2ee: seal to: %w", err)
	}
	ct, err := s.Seal(ad, plaintext)
	if err != nil {
		return nil, fmt.Errorf("e2ee: seal to: %w", err)
	}
	out := make([]byte, 0, 1+len(enc)+len(ct))
	out = append(out, FormatSealed)
	out = append(out, enc...)
	return append(out, ct...), nil
}

// OpenSealed opens a sealed box with the recipient's private key.
func OpenSealed(priv, msg, info, ad []byte) ([]byte, error) {
	return openSealed(priv, msg, sealedInfo(info), ad)
}

func openSealed(priv, msg, info, ad []byte) ([]byte, error) {
	if len(msg) < 1+EncapsulationSize+16 || msg[0] != FormatSealed {
		return nil, ErrFormat
	}
	k, err := kem.NewPrivateKey(priv)
	if err != nil {
		return nil, errors.New("e2ee: not an X-Wing private key")
	}
	r, err := hpke.NewRecipient(msg[1:1+EncapsulationSize], k, kdf, aead, info)
	if err != nil {
		return nil, ErrOpen
	}
	pt, err := r.Open(ad, msg[1+EncapsulationSize:])
	if err != nil {
		return nil, ErrOpen
	}
	return pt, nil
}

// sealedInfo is HPKE's info: the package label ‖ the format byte ‖ the
// caller's info. The label keeps a box sealed here from opening in another
// protocol using the same keys; the format byte keeps a later envelope that
// reuses HPKE from ever opening as 0x03 (RP37-12). Pinned by
// TestSealedBox_GoldenOpens.
func sealedInfo(info []byte) []byte {
	out := make([]byte, 0, len(labelSealed)+1+len(info))
	out = append(out, labelSealed...)
	out = append(out, FormatSealed)
	return append(out, info...)
}
