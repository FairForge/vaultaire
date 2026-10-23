package e2ee

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// The identity: one encryption key pair (X-Wing, the sealed box's) and one
// signing key pair. The public halves are the only plaintext keys the server
// holds; the private halves are wrapped under the master key.
//
//	signing key     alg(1) ‖ key        0x01 = Ed25519 (32-byte seed / public key)
//	signature       alg(1) ‖ sig        0x01 = Ed25519 (64), over lp(context) ‖ message
//	fingerprint     SHA-256(label ‖ "fingerprint" ‖ lp(enc public) ‖ lp(sig public)), lp = be16 length ‖ bytes
//	identity wraps  0x01 boxes under the master key, AD = label ‖ "identity/enc" | "identity/sig"
//
// The alg byte is there for a post-quantum signature (F-34): 0x02 is held
// for the hybrid Ed25519 + ML-DSA-65 (both must verify), once the toolchain
// has crypto/mldsa or a vetted module is added — the owner's call. Nothing
// here signs with it yet; an envelope naming it is refused (ErrFormat).

const (
	// SigEd25519 names Ed25519 in a signing key or a signature.
	SigEd25519 = byte(0x01)
	// SigEd25519MLDSA65 is reserved for the hybrid (F-34); not implemented.
	SigEd25519MLDSA65 = byte(0x02)

	// FingerprintSize is a fingerprint's length; FingerprintString shows
	// its first 20 bytes.
	FingerprintSize = sha256.Size

	labelIdentityEnc = label + "identity/enc"
	labelIdentitySig = label + "identity/sig"
	labelFingerprint = label + "fingerprint"
	labelSignature   = label + "sig/"
)

var (
	// ErrSignature: a signature that does not verify under the key given.
	ErrSignature = errors.New("e2ee: signature does not verify")
)

// Identity is a person's (or a device's) two key pairs.
type Identity struct {
	EncPrivate []byte // X-Wing seed (32)
	EncPublic  []byte // X-Wing public key (1216)
	SigPrivate []byte // alg ‖ Ed25519 seed (33)
	SigPublic  []byte // alg ‖ Ed25519 public key (33)
}

// PublicIdentity is what others hold of an Identity — and what the
// fingerprint is over.
type PublicIdentity struct {
	EncPublic []byte
	SigPublic []byte
}

// GenerateIdentity makes both key pairs on the device.
func GenerateIdentity() (*Identity, error) {
	encPriv, _, err := GenerateKeyPair()
	if err != nil {
		return nil, err
	}
	seed, err := RandomKey()
	if err != nil {
		return nil, err
	}
	return IdentityFromKeys(encPriv, append([]byte{SigEd25519}, seed...))
}

// IdentityFromKeys rebuilds an Identity from its two private keys.
func IdentityFromKeys(encPrivate, sigPrivate []byte) (*Identity, error) {
	encPub, err := PublicKey(encPrivate)
	if err != nil {
		return nil, err
	}
	sigPub, err := signingPublic(sigPrivate)
	if err != nil {
		return nil, err
	}
	return &Identity{
		EncPrivate: append([]byte{}, encPrivate...), EncPublic: encPub,
		SigPrivate: append([]byte{}, sigPrivate...), SigPublic: sigPub,
	}, nil
}

func signingPublic(sigPrivate []byte) ([]byte, error) {
	if len(sigPrivate) != 1+ed25519.SeedSize || sigPrivate[0] != SigEd25519 {
		return nil, ErrFormat
	}
	pub := ed25519.NewKeyFromSeed(sigPrivate[1:]).Public().(ed25519.PublicKey)
	return append([]byte{SigEd25519}, pub...), nil
}

// Public is the identity's public half.
func (id *Identity) Public() PublicIdentity {
	return PublicIdentity{EncPublic: id.EncPublic, SigPublic: id.SigPublic}
}

// Fingerprint is SHA-256 over a fixed encoding of both public keys: what a
// person compares out of band, and what a key-transparency entry (K4b) will
// carry. Nothing publishes it yet.
func Fingerprint(pub PublicIdentity) ([]byte, error) {
	if len(pub.EncPublic) != PublicKeySize {
		return nil, errors.New("e2ee: not an X-Wing public key")
	}
	if len(pub.SigPublic) != 1+ed25519.PublicKeySize || pub.SigPublic[0] != SigEd25519 {
		return nil, ErrFormat
	}
	h := sha256.New()
	h.Write([]byte(labelFingerprint))
	h.Write(lp(pub.EncPublic))
	h.Write(lp(pub.SigPublic))
	return h.Sum(nil), nil
}

// fingerprintText is base32 without padding, lower case.
var fingerprintText = base32.StdEncoding.WithPadding(base32.NoPadding)

// FingerprintString is a fingerprint as people compare it: its first 160
// bits in base32, eight groups of four ("mfrg gzdf …").
func FingerprintString(fp []byte) (string, error) {
	if len(fp) != FingerprintSize {
		return "", fmt.Errorf("e2ee: a fingerprint is %d bytes", FingerprintSize)
	}
	s := strings.ToLower(fingerprintText.EncodeToString(fp[:20]))
	groups := make([]string, 0, 8)
	for i := 0; i < len(s); i += 4 {
		groups = append(groups, s[i:i+4])
	}
	return strings.Join(groups, " "), nil
}

// Sign signs message under context (a purpose label: the same bytes signed
// for two purposes are two different signatures). The result is the
// envelope alg ‖ sig.
func Sign(sigPrivate []byte, context string, message []byte) ([]byte, error) {
	if len(sigPrivate) != 1+ed25519.SeedSize || sigPrivate[0] != SigEd25519 {
		return nil, ErrFormat
	}
	sig := ed25519.Sign(ed25519.NewKeyFromSeed(sigPrivate[1:]), signed(context, message))
	return append([]byte{SigEd25519}, sig...), nil
}

// Verify checks a signature envelope under a signing public key. An
// envelope or key naming another algorithm (the reserved hybrid among them)
// is ErrFormat; a signature that does not verify is ErrSignature.
func Verify(sigPublic []byte, context string, message, signature []byte) error {
	if len(sigPublic) != 1+ed25519.PublicKeySize || sigPublic[0] != SigEd25519 {
		return ErrFormat
	}
	if len(signature) != 1+ed25519.SignatureSize || signature[0] != SigEd25519 {
		return ErrFormat
	}
	if !ed25519.Verify(ed25519.PublicKey(sigPublic[1:]), signed(context, message), signature[1:]) {
		return ErrSignature
	}
	return nil
}

func signed(context string, message []byte) []byte {
	out := lp([]byte(labelSignature + context))
	return append(out, message...)
}

// WrapIdentity wraps both private keys under the master key, one box per
// role.
func WrapIdentity(masterKey []byte, id *Identity) (wrappedEnc, wrappedSig []byte, err error) {
	if wrappedEnc, err = Seal(masterKey, id.EncPrivate, []byte(labelIdentityEnc)); err != nil {
		return nil, nil, err
	}
	if wrappedSig, err = Seal(masterKey, id.SigPrivate, []byte(labelIdentitySig)); err != nil {
		return nil, nil, err
	}
	return wrappedEnc, wrappedSig, nil
}

// OpenIdentity opens both wraps and rebuilds the public keys.
func OpenIdentity(masterKey, wrappedEnc, wrappedSig []byte) (*Identity, error) {
	enc, err := open(FormatBox, masterKey, wrappedEnc, []byte(labelIdentityEnc))
	if err != nil {
		return nil, err
	}
	sig, err := open(FormatBox, masterKey, wrappedSig, []byte(labelIdentitySig))
	if err != nil {
		return nil, err
	}
	return IdentityFromKeys(enc, sig)
}

// lp is a length-prefixed field: be16(len) ‖ b. Every variable-length field
// of a fixed encoding (fingerprints, ADs, signed messages, manifests) is one,
// so two different field lists never encode to the same bytes.
func lp(b []byte) []byte {
	if len(b) > 0xffff {
		panic("e2ee: a length-prefixed field is at most 65535 bytes")
	}
	out := make([]byte, 0, 2+len(b))
	out = binary.BigEndian.AppendUint16(out, uint16(len(b)))
	return append(out, b...)
}
