package recovery

import (
	"crypto/ed25519"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"

	"filippo.io/edwards25519"
	"github.com/gtank/ristretto255"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/blake2s"
	"golang.org/x/crypto/chacha20poly1305"
)

// The Juicebox primitives, each pinned to the MIT SDK's source in
// docs/reviews/K5-PROBE.md § Primitives. Group = ristretto255.

// PINHashingMode is the Argon2id cost (rust/sdk/src/pin.rs).
type PINHashingMode uint8

const (
	// Standard2019: m = 16 MiB, t = 32, p = 1 — the SDK's production mode.
	Standard2019 PINHashingMode = iota
	// FastInsecure: m = 8 KiB, t = 1, p = 1 — tests only.
	FastInsecure
)

// MaxSecretLength is the longest secret a realm can hold
// (MAX_USER_SECRET_LENGTH); every ciphertext is EncryptedSecretSize bytes.
const (
	MaxSecretLength     = 128
	EncryptedSecretSize = MaxSecretLength + 1 + chacha20poly1305.Overhead
)

var (
	errInvalidScalar = errors.New("recovery: not a canonical scalar")
	errInvalidPoint  = errors.New("recovery: not a canonical ristretto255 point")
)

// lp is be4(len(b)) ‖ b, the SDK's length prefix for MAC inputs.
func lp(dst, b []byte) []byte {
	return append(binary.BigEndian.AppendUint32(dst, uint32(len(b))), b...)
}

// HashPIN stretches a PIN: Argon2id v0x13, 64 bytes, salt =
// be4(16) ‖ version ‖ be4(len(info)) ‖ info. Returns the access key (the
// OPRF input) and the encryption-key seed.
func HashPIN(mode PINHashingMode, pin []byte, version [16]byte, userInfo []byte) (accessKey, seed [32]byte) {
	salt := lp(lp(nil, version[:]), userInfo)
	var out []byte
	switch mode {
	case FastInsecure:
		out = argon2.IDKey(pin, salt, 1, 8, 1, 64)
	default:
		out = argon2.IDKey(pin, salt, 32, 16*1024, 1, 64)
	}
	copy(accessKey[:], out[:32])
	copy(seed[:], out[32:])
	clear(out)
	return accessKey, seed
}

// RandomScalar draws 64 bytes and reduces them mod ℓ (Scalar::random).
func RandomScalar(rand io.Reader) (*ristretto255.Scalar, error) {
	var b [64]byte
	if _, err := io.ReadFull(rand, b[:]); err != nil {
		return nil, fmt.Errorf("random scalar: %w", err)
	}
	s, err := ristretto255.NewScalar().SetUniformBytes(b[:])
	clear(b[:])
	return s, err
}

// randomElement draws 64 bytes through from_uniform_bytes (RistrettoPoint::random).
func randomElement(rand io.Reader) (*ristretto255.Element, error) {
	var b [64]byte
	if _, err := io.ReadFull(rand, b[:]); err != nil {
		return nil, fmt.Errorf("random point: %w", err)
	}
	return ristretto255.NewIdentityElement().SetUniformBytes(b[:])
}

// ScalarFromBytes parses a canonical scalar.
func ScalarFromBytes(b [32]byte) (*ristretto255.Scalar, error) {
	s, err := ristretto255.NewScalar().SetCanonicalBytes(b[:])
	if err != nil {
		return nil, errInvalidScalar
	}
	return s, nil
}

// ElementFromBytes parses a canonical compressed point.
func ElementFromBytes(b [32]byte) (*ristretto255.Element, error) {
	e, err := ristretto255.NewIdentityElement().SetCanonicalBytes(b[:])
	if err != nil {
		return nil, errInvalidPoint
	}
	return e, nil
}

func to32(b []byte) (out [32]byte) {
	copy(out[:], b)
	return out
}

func scalarFromUint32(n uint32) *ristretto255.Scalar {
	var b [32]byte
	binary.LittleEndian.PutUint32(b[:], n)
	s, _ := ristretto255.NewScalar().SetCanonicalBytes(b[:])
	return s
}

// hashToGroup is RistrettoPoint::hash_from_bytes::<Sha512>.
func hashToGroup(input []byte) *ristretto255.Element {
	d := sha512.Sum512(input)
	e, _ := ristretto255.NewIdentityElement().SetUniformBytes(d[:])
	return e
}

// oprfOutput is SHA-512("Juicebox_OPRF_2023_1;" ‖ input ‖ compress(result)).
func oprfOutput(input []byte, result *ristretto255.Element) [64]byte {
	h := sha512.New()
	h.Write([]byte("Juicebox_OPRF_2023_1;"))
	h.Write(input)
	h.Write(result.Bytes())
	var out [64]byte
	h.Sum(out[:0])
	return out
}

// OPRFEvaluate is the unoblivious evaluation the client runs at
// registration with the root key.
func OPRFEvaluate(key *ristretto255.Scalar, input []byte) [64]byte {
	return oprfOutput(input, ristretto255.NewIdentityElement().ScalarMult(key, hashToGroup(input)))
}

// OPRFBlind starts a recovery: a random blinding factor r (64 bytes drawn)
// and the blinded input H(input)·r.
func OPRFBlind(rand io.Reader, input []byte) (blind *ristretto255.Scalar, blinded [32]byte, err error) {
	r, err := RandomScalar(rand)
	if err != nil {
		return nil, blinded, err
	}
	return r, to32(ristretto255.NewIdentityElement().ScalarMult(r, hashToGroup(input)).Bytes()), nil
}

// OPRFBlindEvaluate is a realm's share of the evaluation: B·k.
func OPRFBlindEvaluate(key *ristretto255.Scalar, blinded *ristretto255.Element) *ristretto255.Element {
	return ristretto255.NewIdentityElement().ScalarMult(key, blinded)
}

// OPRFFinalize unblinds the combined result: (B·k)·r⁻¹.
func OPRFFinalize(input []byte, blind *ristretto255.Scalar, blindedOutput *ristretto255.Element) [64]byte {
	inv := ristretto255.NewScalar().Invert(blind)
	return oprfOutput(input, ristretto255.NewIdentityElement().ScalarMult(inv, blindedOutput))
}

// Proof is a Chaum–Pedersen DLEQ proof that a realm used the key behind
// its public key: {c, β_z}.
type Proof struct {
	C     [32]byte
	BetaZ [32]byte
}

func dleqChallenge(u, v, w, vt, wt []byte) *ristretto255.Scalar {
	h := sha512.New()
	h.Write([]byte("Juicebox_DLEQ_2023_1;"))
	for _, p := range [][]byte{u, v, w, vt, wt} {
		h.Write(p)
	}
	c, _ := ristretto255.NewScalar().SetUniformBytes(h.Sum(nil)) // 64 bytes: cannot fail
	return c
}

// ProveDLEQ proves log_G(V) = log_U(W) = k for U = the blinded input,
// V = k·G, W = U·k; draws one random scalar (64 bytes).
func ProveDLEQ(rand io.Reader, k *ristretto255.Scalar, u *ristretto255.Element, v [32]byte, w *ristretto255.Element) (Proof, error) {
	t, err := RandomScalar(rand)
	if err != nil {
		return Proof{}, err
	}
	vt := ristretto255.NewIdentityElement().ScalarBaseMult(t)
	wt := ristretto255.NewIdentityElement().ScalarMult(t, u)
	c := dleqChallenge(u.Bytes(), v[:], w.Bytes(), vt.Bytes(), wt.Bytes())
	bz := ristretto255.NewScalar().Add(t, ristretto255.NewScalar().Multiply(k, c))
	return Proof{C: to32(c.Bytes()), BetaZ: to32(bz.Bytes())}, nil
}

// VerifyDLEQ checks a proof over the blinded input u, the public key v and
// the blinded output w.
func VerifyDLEQ(u, v, w [32]byte, p Proof) bool {
	ue, err1 := ElementFromBytes(u)
	ve, err2 := ElementFromBytes(v)
	we, err3 := ElementFromBytes(w)
	c, err4 := ScalarFromBytes(p.C)
	bz, err5 := ScalarFromBytes(p.BetaZ)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
		return false
	}
	negC := ristretto255.NewScalar().Negate(c)
	vt := ristretto255.NewIdentityElement().Add(ristretto255.NewIdentityElement().ScalarBaseMult(bz), ristretto255.NewIdentityElement().ScalarMult(negC, ve))
	wt := ristretto255.NewIdentityElement().Add(ristretto255.NewIdentityElement().ScalarMult(bz, ue), ristretto255.NewIdentityElement().ScalarMult(negC, we))
	got := dleqChallenge(u[:], v[:], w[:], vt.Bytes(), wt.Bytes())
	return subtle.ConstantTimeCompare(got.Bytes(), p.C[:]) == 1
}

// ScalarShare is one Shamir share of a scalar; ElementShare of a point.
// Index is 1-based (the realm's position in the id-sorted configuration).
type ScalarShare struct {
	Index  uint32
	Secret *ristretto255.Scalar
}

type ElementShare struct {
	Index  uint32
	Secret *ristretto255.Element
}

// ShareScalar splits secret into count shares, any threshold of which
// recover it; draws threshold−1 coefficients (64 bytes each) in order.
func ShareScalar(rand io.Reader, secret *ristretto255.Scalar, threshold, count int) ([]ScalarShare, error) {
	if threshold < 1 || count < threshold {
		return nil, fmt.Errorf("recovery: threshold %d of %d", threshold, count)
	}
	coeffs := make([]*ristretto255.Scalar, threshold-1)
	for i := range coeffs {
		c, err := RandomScalar(rand)
		if err != nil {
			return nil, err
		}
		coeffs[i] = c
	}
	shares := make([]ScalarShare, count)
	for i := range shares {
		x := scalarFromUint32(uint32(i + 1))
		acc := ristretto255.NewScalar()
		for _, c := range coeffs {
			acc.Multiply(acc.Add(acc, c), x)
		}
		shares[i] = ScalarShare{Index: uint32(i + 1), Secret: acc.Add(acc, secret)}
	}
	return shares, nil
}

// ShareElement is ShareScalar over points (coefficients are random points).
func ShareElement(rand io.Reader, secret *ristretto255.Element, threshold, count int) ([]ElementShare, error) {
	if threshold < 1 || count < threshold {
		return nil, fmt.Errorf("recovery: threshold %d of %d", threshold, count)
	}
	coeffs := make([]*ristretto255.Element, threshold-1)
	for i := range coeffs {
		c, err := randomElement(rand)
		if err != nil {
			return nil, err
		}
		coeffs[i] = c
	}
	shares := make([]ElementShare, count)
	for i := range shares {
		x := scalarFromUint32(uint32(i + 1))
		acc := ristretto255.NewIdentityElement()
		for _, c := range coeffs {
			acc.ScalarMult(x, acc.Add(acc, c))
		}
		shares[i] = ElementShare{Index: uint32(i + 1), Secret: acc.Add(acc, secret)}
	}
	return shares, nil
}

var errDuplicateShares = errors.New("recovery: duplicate share indices")

// lagrangeAtZero is λ_i = Π_{j≠i} x_j / Π_{j≠i} (x_j − x_i).
func lagrangeAtZero(indices []uint32, i int) (*ristretto255.Scalar, error) {
	num := scalarFromUint32(1)
	den := scalarFromUint32(1)
	xi := scalarFromUint32(indices[i])
	for j, idx := range indices {
		if j == i {
			continue
		}
		xj := scalarFromUint32(idx)
		num.Multiply(num, xj)
		den.Multiply(den, ristretto255.NewScalar().Subtract(xj, xi))
	}
	if den.Equal(ristretto255.NewScalar()) == 1 {
		return nil, errDuplicateShares
	}
	return num.Multiply(num, ristretto255.NewScalar().Invert(den)), nil
}

// RecoverScalar interpolates the shares at 0.
func RecoverScalar(shares []ScalarShare) (*ristretto255.Scalar, error) {
	idx := make([]uint32, len(shares))
	for i, s := range shares {
		idx[i] = s.Index
	}
	out := ristretto255.NewScalar()
	for i, s := range shares {
		l, err := lagrangeAtZero(idx, i)
		if err != nil {
			return nil, err
		}
		out.Add(out, ristretto255.NewScalar().Multiply(s.Secret, l))
	}
	return out, nil
}

// RecoverElement interpolates point shares at 0.
func RecoverElement(shares []ElementShare) (*ristretto255.Element, error) {
	idx := make([]uint32, len(shares))
	for i, s := range shares {
		idx[i] = s.Index
	}
	out := ristretto255.NewIdentityElement()
	for i, s := range shares {
		l, err := lagrangeAtZero(idx, i)
		if err != nil {
			return nil, err
		}
		out.Add(out, ristretto255.NewIdentityElement().ScalarMult(l, s.Secret))
	}
	return out, nil
}

// UnlockKey splits SHA-512(oprf output): [0:32] the commitment every realm
// stores, [32:64] the unlock key that never leaves the client.
func UnlockKey(oprfOut [64]byte) (key, commitment [32]byte) {
	d := sha512.Sum512(oprfOut[:])
	copy(commitment[:], d[:32])
	copy(key[:], d[32:])
	return key, commitment
}

func mac(key []byte, size int, parts ...[]byte) []byte {
	var h hash.Hash
	var err error
	if size == 16 {
		h, err = blake2s.New128(key)
	} else {
		h, err = blake2s.New256(key)
	}
	if err != nil {
		panic(err) // the keys are 32 bytes by construction
	}
	for _, p := range parts {
		_, _ = h.Write(lp(nil, p))
	}
	return h.Sum(nil)
}

// UnlockKeyTag is the per-realm proof of the PIN sent in Recover3.
func UnlockKeyTag(unlockKey [32]byte, realm RealmID) (tag [16]byte) {
	copy(tag[:], mac(unlockKey[:], 16, []byte("Unlock Key Tag"), realm[:]))
	return tag
}

// EncryptedSecretCommitment binds a realm's share and the ciphertext to the
// unlock key, so a client detects a realm that returns other bytes.
func EncryptedSecretCommitment(unlockKey [32]byte, realm RealmID, share [32]byte, encrypted [EncryptedSecretSize]byte) (c [16]byte) {
	copy(c[:], mac(unlockKey[:], 16, []byte("Encrypted User Secret Commitment"), realm[:], share[:], encrypted[:]))
	return c
}

// EncryptionKey derives the secret's key from the PIN-derived seed and the
// shared scalar.
func EncryptionKey(seed [32]byte, scalar [32]byte) (key [32]byte) {
	copy(key[:], mac(seed[:], 32, []byte("User Secret Encryption Key"), scalar[:]))
	return key
}

// EncryptSecret pads the secret to 129 bytes (length ‖ secret ‖ zeros) and
// seals it with ChaCha20-Poly1305 under a zero nonce (the key is fresh per
// registration).
func EncryptSecret(key [32]byte, secret []byte) ([EncryptedSecretSize]byte, error) {
	var out [EncryptedSecretSize]byte
	if len(secret) > MaxSecretLength {
		return out, fmt.Errorf("recovery: secret is %d bytes, at most %d", len(secret), MaxSecretLength)
	}
	aead, err := chacha20poly1305.New(key[:])
	if err != nil {
		return out, fmt.Errorf("secret cipher: %w", err)
	}
	var padded [MaxSecretLength + 1]byte
	padded[0] = byte(len(secret))
	copy(padded[1:], secret)
	// #nosec G407 -- the Juicebox wire format (the MIT golden vectors): the
	// key is single-use, derived per registration from a fresh OPRF scalar,
	// and seals exactly this one message.
	aead.Seal(out[:0], make([]byte, chacha20poly1305.NonceSize), padded[:], nil)
	clear(padded[:])
	return out, nil
}

// DecryptSecret opens and unpads a secret.
func DecryptSecret(key [32]byte, encrypted [EncryptedSecretSize]byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key[:])
	if err != nil {
		return nil, fmt.Errorf("secret cipher: %w", err)
	}
	padded, err := aead.Open(nil, make([]byte, chacha20poly1305.NonceSize), encrypted[:], nil)
	if err != nil {
		return nil, errors.New("recovery: the secret does not open under the recovered key")
	}
	n := int(padded[0])
	if n > MaxSecretLength {
		return nil, errors.New("recovery: bad secret padding")
	}
	secret := append([]byte(nil), padded[1:1+n]...)
	clear(padded)
	return secret, nil
}

// signatureMessage is be2(16) ‖ realm ‖ be2(32) ‖ public key.
func signatureMessage(realm RealmID, publicKey [32]byte) []byte {
	m := binary.BigEndian.AppendUint16(nil, uint16(len(realm)))
	m = append(m, realm[:]...)
	m = binary.BigEndian.AppendUint16(m, uint16(len(publicKey)))
	return append(m, publicKey[:]...)
}

// SignPublicKey signs a realm's OPRF public key with the registration's
// one-time Ed25519 key.
func SignPublicKey(signing ed25519.PrivateKey, realm RealmID, publicKey [32]byte) SignedPublicKey {
	var s SignedPublicKey
	s.PublicKey = publicKey
	copy(s.VerifyingKey[:], signing.Public().(ed25519.PublicKey))
	copy(s.Signature[:], ed25519.Sign(signing, signatureMessage(realm, publicKey)))
	return s
}

// Verify checks the signature for this realm (Ed25519; the SDK verifies
// with dalek's verify_strict, which also refuses a small-order key — so
// does this).
func (s SignedPublicKey) Verify(realm RealmID) bool {
	if isSmallOrderEd25519(s.VerifyingKey) {
		return false
	}
	return ed25519.Verify(s.VerifyingKey[:], signatureMessage(realm, s.PublicKey), s.Signature[:])
}

// isSmallOrderEd25519 reports a verifying key that is a point of small
// order (dalek's is_weak), which verify_strict refuses.
func isSmallOrderEd25519(k [32]byte) bool {
	p, err := new(edwards25519.Point).SetBytes(k[:])
	if err != nil {
		return true
	}
	return new(edwards25519.Point).MultByCofactor(p).Equal(edwards25519.NewIdentityPoint()) == 1
}
