package recovery

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"

	"golang.org/x/crypto/blake2s"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// Noise_NK_25519_ChaChaPoly_BLAKE2s with an empty prologue, as
// rust/noise/src/{lib,client,server}.rs implement it: the client knows the
// realm's static key; one round trip; the first payload rides under es
// (no forward secrecy), every transport message after the split under
// es+ee.

const noiseProtocolName = "Noise_NK_25519_ChaChaPoly_BLAKE2s"

// ErrNoise is any handshake or transport failure.
var ErrNoise = errors.New("recovery: noise")

type noiseCipher struct {
	aead cipher.AEAD
	n    uint64
}

func newNoiseCipher(k []byte) *noiseCipher {
	aead, err := chacha20poly1305.New(k)
	if err != nil {
		panic(err) // k is 32 bytes by construction
	}
	return &noiseCipher{aead: aead}
}

// nonce is 4 zero bytes ‖ le64(n); 2^64−1 is reserved.
func (c *noiseCipher) nonce() ([]byte, error) {
	if c.n == ^uint64(0)-1 {
		return nil, fmt.Errorf("%w: nonce exhausted", ErrNoise)
	}
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.LittleEndian.PutUint64(nonce[4:], c.n)
	c.n++
	return nonce, nil
}

func (c *noiseCipher) encrypt(ad, pt []byte) ([]byte, error) {
	n, err := c.nonce()
	if err != nil {
		return nil, err
	}
	return c.aead.Seal(nil, n, pt, ad), nil
}

func (c *noiseCipher) decrypt(ad, ct []byte) ([]byte, error) {
	n, err := c.nonce()
	if err != nil {
		return nil, err
	}
	pt, err := c.aead.Open(nil, n, ct, ad)
	if err != nil {
		return nil, fmt.Errorf("%w: decryption failed", ErrNoise)
	}
	return pt, nil
}

func newBlake2s() hash.Hash {
	h, _ := blake2s.New256(nil)
	return h
}

type noiseSymmetric struct {
	h  [32]byte
	ck [32]byte
}

func newNoiseSymmetric() noiseSymmetric {
	var s noiseSymmetric
	s.h = blake2s.Sum256([]byte(noiseProtocolName)) // the name is longer than the hash
	s.ck = s.h
	s.mixHash(nil) // the empty prologue
	return s
}

func (s *noiseSymmetric) mixHash(data []byte) {
	h := newBlake2s()
	h.Write(s.h[:])
	h.Write(data)
	h.Sum(s.h[:0])
}

// hkdfPair is HKDF with HMAC-BLAKE2s, two 32-byte outputs, empty info.
func hkdfPair(salt, ikm []byte) (a, b [32]byte) {
	r := hkdf.New(newBlake2s, ikm, salt, nil)
	var out [64]byte
	_, _ = io.ReadFull(r, out[:])
	copy(a[:], out[:32])
	copy(b[:], out[32:])
	return a, b
}

func (s *noiseSymmetric) mixKey(dh []byte) *noiseCipher {
	ck, k := hkdfPair(s.ck[:], dh)
	s.ck = ck
	return newNoiseCipher(k[:])
}

func (s *noiseSymmetric) split() (first, second *noiseCipher) {
	k1, k2 := hkdfPair(s.ck[:], nil)
	return newNoiseCipher(k1[:]), newNoiseCipher(k2[:])
}

// NoiseTransport encrypts and decrypts after the handshake.
type NoiseTransport struct {
	in, out *noiseCipher
}

func (t *NoiseTransport) Encrypt(pt []byte) ([]byte, error) { return t.out.encrypt(nil, pt) }
func (t *NoiseTransport) Decrypt(ct []byte) ([]byte, error) { return t.in.decrypt(nil, ct) }

// NoisePublicKey is the X25519 public key of a static secret.
func NoisePublicKey(secret [32]byte) ([32]byte, error) {
	var pub [32]byte
	p, err := curve25519.X25519(secret[:], curve25519.Basepoint)
	if err != nil {
		return pub, fmt.Errorf("%w: %w", ErrNoise, err)
	}
	copy(pub[:], p)
	return pub, nil
}

func dh(secret, public []byte) ([]byte, error) {
	if len(public) != 32 {
		return nil, fmt.Errorf("%w: a public key is %d bytes", ErrNoise, len(public))
	}
	out, err := curve25519.X25519(secret, public)
	if err != nil { // a low-order point: refuse rather than mix zeros
		return nil, fmt.Errorf("%w: %w", ErrNoise, err)
	}
	return out, nil
}

// NoiseClientHandshake is the client's state between its message and the
// realm's answer.
type NoiseClientHandshake struct {
	e   [32]byte
	sym noiseSymmetric
}

// NoiseClientStart writes "-> e, es" with payload; draws 32 bytes.
func NoiseClientStart(rand io.Reader, serverStatic [32]byte, payload []byte) (*NoiseClientHandshake, HandshakeRequest, error) {
	hs := &NoiseClientHandshake{sym: newNoiseSymmetric()}
	if _, err := io.ReadFull(rand, hs.e[:]); err != nil {
		return nil, HandshakeRequest{}, fmt.Errorf("noise ephemeral: %w", err)
	}
	epub, err := NoisePublicKey(hs.e)
	if err != nil {
		return nil, HandshakeRequest{}, err
	}
	hs.sym.mixHash(serverStatic[:])
	hs.sym.mixHash(epub[:])
	es, err := dh(hs.e[:], serverStatic[:])
	if err != nil {
		return nil, HandshakeRequest{}, err
	}
	ct, err := hs.sym.mixKey(es).encrypt(hs.sym.h[:], payload)
	if err != nil {
		return nil, HandshakeRequest{}, err
	}
	hs.sym.mixHash(ct)
	return hs, HandshakeRequest{ClientEphemeralPublic: epub[:], PayloadCiphertext: ct}, nil
}

// Finish reads "<- e, ee" and returns the transport and the realm's payload.
func (hs *NoiseClientHandshake) Finish(resp HandshakeResponse) (*NoiseTransport, []byte, error) {
	if len(resp.ServerEphemeralPublic) != 32 {
		return nil, nil, fmt.Errorf("%w: bad server ephemeral", ErrNoise)
	}
	hs.sym.mixHash(resp.ServerEphemeralPublic)
	ee, err := dh(hs.e[:], resp.ServerEphemeralPublic)
	if err != nil {
		return nil, nil, err
	}
	pt, err := hs.sym.mixKey(ee).decrypt(hs.sym.h[:], resp.PayloadCiphertext)
	if err != nil {
		return nil, nil, err
	}
	k1, k2 := hs.sym.split()
	clear(hs.e[:])
	return &NoiseTransport{in: k2, out: k1}, pt, nil
}

// NoiseServerHandshake is the realm's state between reading the client's
// message and answering it.
type NoiseServerHandshake struct {
	cs   *noiseCipher
	sym  noiseSymmetric
	epub [32]byte
}

// NoiseServerStart reads "-> e, es" and prepares "<- e, ee"; draws 32
// bytes (first, as the Rust server does).
func NoiseServerStart(rand io.Reader, staticSecret, staticPublic [32]byte, req HandshakeRequest) (*NoiseServerHandshake, []byte, error) {
	var e [32]byte
	if _, err := io.ReadFull(rand, e[:]); err != nil {
		return nil, nil, fmt.Errorf("noise ephemeral: %w", err)
	}
	defer clear(e[:])
	if len(req.ClientEphemeralPublic) != 32 {
		return nil, nil, fmt.Errorf("%w: bad client ephemeral", ErrNoise)
	}
	hs := &NoiseServerHandshake{sym: newNoiseSymmetric()}
	hs.sym.mixHash(staticPublic[:])
	hs.sym.mixHash(req.ClientEphemeralPublic)
	es, err := dh(staticSecret[:], req.ClientEphemeralPublic)
	if err != nil {
		return nil, nil, err
	}
	pt, err := hs.sym.mixKey(es).decrypt(hs.sym.h[:], req.PayloadCiphertext)
	if err != nil {
		return nil, nil, err
	}
	hs.sym.mixHash(req.PayloadCiphertext)
	if hs.epub, err = NoisePublicKey(e); err != nil {
		return nil, nil, err
	}
	hs.sym.mixHash(hs.epub[:])
	ee, err := dh(e[:], req.ClientEphemeralPublic)
	if err != nil {
		return nil, nil, err
	}
	hs.cs = hs.sym.mixKey(ee)
	return hs, pt, nil
}

// Finish encrypts the realm's payload and returns the transport.
func (hs *NoiseServerHandshake) Finish(payload []byte) (*NoiseTransport, HandshakeResponse, error) {
	ct, err := hs.cs.encrypt(hs.sym.h[:], payload)
	if err != nil {
		return nil, HandshakeResponse{}, err
	}
	k1, k2 := hs.sym.split()
	return &NoiseTransport{in: k1, out: k2}, HandshakeResponse{ServerEphemeralPublic: append([]byte(nil), hs.epub[:]...), PayloadCiphertext: ct}, nil
}
