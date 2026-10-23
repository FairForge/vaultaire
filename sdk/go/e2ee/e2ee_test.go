package e2ee

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The v1 chunk format, frozen 2026-10-22. The key and the nonce (bytes 1–24
// of goldenChunk) were recomputed independently from RFC 5869 and HMAC in
// Python's standard library; the rest is XChaCha20-Poly1305 (KAT above).
const (
	goldenChunkKey = "8d98f50939b977f3be6cfe5dff981b8ae4616f69ef91dd14883018725909f0a7"
	goldenChunk    = "02e520fe89524dfaa6783f76cdf18394bc702d945f342f324a61ecd14f67dba50cdcd4a8997a16f2c083a16d632b6e0cc591ad7a181a3926a0ca43a5bfec3c"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

func key(t *testing.T, fill byte) []byte {
	t.Helper()
	return bytes.Repeat([]byte{fill}, KeySize)
}

// TestBox_XChaCha20Poly1305_KnownAnswer: draft-irtf-cfrg-xchacha
// Appendix A.3.1, as pinned by golang.org/x/crypto@v0.58.0
// chacha20poly1305/chacha20poly1305_vectors_test.go — the cipher under the
// box is exactly XChaCha20-Poly1305.
func TestBox_XChaCha20Poly1305_KnownAnswer(t *testing.T) {
	pt := unhex(t, "4c616469657320616e642047656e746c656d656e206f662074686520636c617373206f66202739393a204966204920636f756c64206f6666657220796f75206f6e6c79206f6e652074697020666f7220746865206675747572652c2073756e73637265656e20776f756c642062652069742e")
	ad := unhex(t, "50515253c0c1c2c3c4c5c6c7")
	k := unhex(t, "808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9f")
	nonce := unhex(t, "404142434445464748494a4b4c4d4e4f5051525354555657")
	want := "bd6d179d3e83d43b9576579493c0e939572a1700252bfaccbed2902c21396cbb731c7f1b0b4aa6440bf3a82f4eda7e39ae64c6708c54c216cb96b72e1213b4522f8c9ba40db5d945b11b69b982c1bb9e3f3fac2bc369488f76b2383565d3fff921f9664c97637da9768812f615c68b13b52ec0875924c1c7987947deafd8780acf49"
	got, err := xchachaSeal(k, nonce, pt, ad)
	require.NoError(t, err)
	assert.Equal(t, want, hex.EncodeToString(got))
}

func TestBox_SealOpen(t *testing.T) {
	k := key(t, 7)
	msg := []byte("space key 1 of the photos space")
	ad := []byte("stored.cloud/e2ee/v1/space-key")
	a, err := Seal(k, msg, ad)
	require.NoError(t, err)
	b, err := Seal(k, msg, ad)
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "a random 192-bit nonce per seal")
	assert.Equal(t, FormatBox, a[0])
	assert.Len(t, a, 1+24+len(msg)+16)

	got, err := Open(k, a, ad)
	require.NoError(t, err)
	assert.Equal(t, msg, got)

	_, err = Open(k, a, []byte("stored.cloud/e2ee/v1/object-key"))
	assert.ErrorIs(t, err, ErrOpen, "another purpose does not open it")
	_, err = Open(key(t, 8), a, ad)
	assert.ErrorIs(t, err, ErrOpen, "another key does not")
	tampered := append([]byte{}, a...)
	tampered[len(tampered)-1] ^= 1
	_, err = Open(k, tampered, ad)
	assert.ErrorIs(t, err, ErrOpen)
	_, err = Seal(k[:31], msg, ad)
	assert.ErrorIs(t, err, ErrKeySize)
}

// TestBox_GoldenBytes pins the 0x01 wire: 01 ‖ nonce ‖ XChaCha20-Poly1305
// under AD = 01 ‖ the caller's AD (recomputed by testdata/vectors_check.py
// with libsodium). Both directions: the fixed nonce seals to these bytes,
// and the public Open opens them.
func TestBox_GoldenBytes(t *testing.T) {
	v := loadVectors(t).Box
	require.NotNil(t, v)
	got, err := seal(FormatBox, v.Key, v.Nonce, v.Plaintext, v.AD)
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(v.Msg), hex.EncodeToString(got))
	pt, err := Open(v.Key, v.Msg, v.AD)
	require.NoError(t, err)
	assert.Equal(t, []byte(v.Plaintext), pt)
}

// TestBox_RelabelledFormatDoesNotOpen: the format byte is in the AEAD's
// associated data, not only checked as msg[0]. The relabelled messages carry
// the CORRECT leading byte for the function that opens them, both have the
// same (nil) caller AD, so only the AD binding can refuse them (RP37-12: the
// old relabel test passed on the msg[0] check).
func TestBox_RelabelledFormatDoesNotOpen(t *testing.T) {
	k := key(t, 7)
	pt := []byte("bytes sealed as one kind, presented as another")

	ph := sha256.Sum256(pt)
	chunk, err := SealChunk(k, pt)
	require.NoError(t, err)
	_, err = OpenChunk(k, chunk, ph[:])
	require.NoError(t, err, "the original opens")
	asBox := append([]byte{FormatBox}, chunk[1:]...)
	_, err = Open(k, asBox, nil)
	assert.ErrorIs(t, err, ErrOpen, "a chunk re-labelled 0x01 does not open as a box")

	box, err := Seal(k, pt, nil)
	require.NoError(t, err)
	_, err = Open(k, box, nil)
	require.NoError(t, err, "the original opens")
	asChunk := append([]byte{FormatChunk}, box[1:]...)
	_, err = OpenChunk(k, asChunk, ph[:])
	assert.ErrorIs(t, err, ErrOpen, "a box re-labelled 0x02 does not open as a chunk")
}

// TestChunk_ConvergentWithinASpace_BytesNeverAcross is F-12's change 1: a
// space deduplicates its own chunks; across two spaces the keys and the
// bytes never converge. Their lengths do (len + ChunkOverhead in every
// space — F-35), and this test says so.
func TestChunk_ConvergentWithinASpace_BytesNeverAcross(t *testing.T) {
	spaceA, spaceB := key(t, 1), key(t, 2)
	chunk := bytes.Repeat([]byte("vm image block "), 1000)
	h := sha256.Sum256(chunk)

	kA, err := ChunkKey(spaceA, h[:])
	require.NoError(t, err)
	kA2, err := ChunkKey(spaceA, h[:])
	require.NoError(t, err)
	kB, err := ChunkKey(spaceB, h[:])
	require.NoError(t, err)
	assert.Equal(t, kA, kA2)
	assert.NotEqual(t, kA, kB)

	c1, err := SealChunk(kA, chunk)
	require.NoError(t, err)
	c2, err := SealChunk(kA2, chunk)
	require.NoError(t, err)
	cB, err := SealChunk(kB, chunk)
	require.NoError(t, err)
	assert.Equal(t, c1, c2, "the same chunk in the same space is stored once")
	assert.NotEqual(t, c1, cB, "two spaces' bytes never converge")
	assert.Len(t, cB, len(c1), "their lengths do: len(chunk) + 41 in every space")
	assert.Equal(t, FormatChunk, c1[0])

	got, err := OpenChunk(kA, c1, h[:])
	require.NoError(t, err)
	assert.Equal(t, chunk, got)
	_, err = OpenChunk(kB, c1, h[:])
	assert.ErrorIs(t, err, ErrOpen)

	// Format pin: these bytes are the v1 wire format; a change here is a
	// knowing dedup reset for every Private space (WP-R8-4's rule).
	pinKey, err := ChunkKey(key(t, 0x11), unhex(t, "6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d"))
	require.NoError(t, err)
	pinned, err := SealChunk(pinKey, []byte("stored.cloud chunk pin"))
	require.NoError(t, err)
	assert.Equal(t, goldenChunkKey, hex.EncodeToString(pinKey))
	assert.Equal(t, goldenChunk, hex.EncodeToString(pinned))
}

// TestChunk_DeterministicNonceSeparatesMessages: the server's R8-01 lesson —
// one chunk key may meet two different messages (a chunk stored raw by one
// client version and compressed by another); a nonce derived from the key
// alone would reuse (key, nonce) for both. The nonce is derived from the
// sealed bytes.
func TestChunk_DeterministicNonceSeparatesMessages(t *testing.T) {
	k := key(t, 3)
	a, err := SealChunk(k, []byte("raw form"))
	require.NoError(t, err)
	b, err := SealChunk(k, []byte("compressed form"))
	require.NoError(t, err)
	assert.NotEqual(t, a[1:25], b[1:25], "two messages under one key never share a nonce")
	_, err = SealChunk(k, make([]byte, MaxChunk+1))
	assert.ErrorIs(t, err, ErrChunkTooLarge)
}

func TestRandomKey(t *testing.T) {
	a, err := RandomKey()
	require.NoError(t, err)
	b, err := RandomKey()
	require.NoError(t, err)
	assert.Len(t, a, KeySize)
	assert.NotEqual(t, a, b)
}

type xwingVector struct {
	Info        string `json:"info"`
	SkRm        string `json:"skRm"`
	PkRm        string `json:"pkRm"`
	Enc         string `json:"enc"`
	Encryptions []struct {
		AAD string `json:"aad"`
		CT  string `json:"ct"`
		PT  string `json:"pt"`
	} `json:"encryptions"`
}

// TestSealedBox_HPKE_XWing_KnownAnswer: Go 1.26's own draft-ietf-hpke-pq
// vector for our suite (X-Wing 0x647a, HKDF-SHA256, ChaCha20Poly1305),
// opened through this package's envelope.
func TestSealedBox_HPKE_XWing_KnownAnswer(t *testing.T) {
	raw, err := os.ReadFile("testdata/hpke_xwing_vector.json")
	require.NoError(t, err)
	var v xwingVector
	require.NoError(t, json.Unmarshal(raw, &v))
	e := v.Encryptions[0]
	msg := append(append([]byte{FormatSealed}, unhex(t, v.Enc)...), unhex(t, e.CT)...)

	// The vector's info is raw: open without the package label.
	pt, err := openSealed(unhex(t, v.SkRm), msg, unhex(t, v.Info), unhex(t, e.AAD))
	require.NoError(t, err)
	assert.Equal(t, e.PT, hex.EncodeToString(pt))

	pub, err := PublicKey(unhex(t, v.SkRm))
	require.NoError(t, err)
	assert.Equal(t, v.PkRm, hex.EncodeToString(pub))
}

// TestSealedBox_RoundTripAndBinding: F-12's change 2 — key sharing is a
// hybrid post-quantum sealed box; the info and the associated data bind it.
func TestSealedBox_RoundTripAndBinding(t *testing.T) {
	priv, pub, err := GenerateKeyPair()
	require.NoError(t, err)
	assert.Len(t, pub, PublicKeySize)
	info := []byte("stored.cloud/e2ee/v1/share space-key")
	ad := []byte("space 0f3c")
	msg, err := SealTo(pub, []byte("the space key"), info, ad)
	require.NoError(t, err)
	assert.Equal(t, FormatSealed, msg[0])
	assert.Len(t, msg, 1+EncapsulationSize+len("the space key")+16)

	got, err := OpenSealed(priv, msg, info, ad)
	require.NoError(t, err)
	assert.Equal(t, []byte("the space key"), got)

	_, err = OpenSealed(priv, msg, []byte("stored.cloud/e2ee/v1/share legacy"), ad)
	assert.ErrorIs(t, err, ErrOpen)
	_, err = OpenSealed(priv, msg, info, []byte("space 0f3d"))
	assert.ErrorIs(t, err, ErrOpen)
	other, _, err := GenerateKeyPair()
	require.NoError(t, err)
	_, err = OpenSealed(other, msg, info, ad)
	assert.ErrorIs(t, err, ErrOpen)
	_, err = SealTo(pub[:100], []byte("x"), info, ad)
	assert.Error(t, err)
}

// TestSealedBox_GoldenOpens pins the 0x03 wire from the OPEN side (HPKE's
// encapsulation is random, so a sealed box cannot be reproduced): a fixed
// X-Wing seed and a message sealed once (vectors.json "sealed") open through
// the public OpenSealed with the fixed info and ad. It fails when the label
// changes (M2), when the label is dropped (M3) and when the format byte
// leaves HPKE's info — the KAT above opens below the label and pins none of
// them (RP37-12).
func TestSealedBox_GoldenOpens(t *testing.T) {
	v := loadVectors(t).Sealed
	require.NotNil(t, v)
	pub, err := PublicKey(v.Seed)
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(v.Pub), hex.EncodeToString(pub))

	pt, err := OpenSealed(v.Seed, v.Msg, v.Info, v.AD)
	require.NoError(t, err)
	assert.Equal(t, []byte(v.Plaintext), pt)

	// The layout, said explicitly: HPKE info = label ‖ 0x03 ‖ info.
	info := append(append([]byte("stored.cloud/e2ee/v1/sealed/"), FormatSealed), v.Info...)
	_, err = openSealed(v.Seed, v.Msg, info, v.AD)
	require.NoError(t, err)
	info[len("stored.cloud/e2ee/v1/sealed/")] = 0x04
	_, err = openSealed(v.Seed, v.Msg, info, v.AD)
	assert.ErrorIs(t, err, ErrOpen, "the same HPKE context under another format byte does not open")
}
