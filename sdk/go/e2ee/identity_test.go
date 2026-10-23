package e2ee

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func goldenIdentity(t *testing.T, name string) (*Identity, *identityVector) {
	t.Helper()
	iv := loadVectors(t).Identities[name]
	require.NotNil(t, iv, name)
	id, err := IdentityFromKeys(iv.EncPrivate, iv.SigPrivate)
	require.NoError(t, err)
	return id, iv
}

// TestIdentity_GoldenBytes pins the identity's public keys, the fingerprint
// encoding and its text, the signed-message layout (Ed25519 is
// deterministic) and both wraps — all recomputed with libsodium and
// hashlib by testdata/vectors_check.py.
func TestIdentity_GoldenBytes(t *testing.T) {
	for _, name := range []string{"alice", "bob", "mallory"} {
		id, iv := goldenIdentity(t, name)
		assert.Equal(t, hex.EncodeToString(iv.EncPublic), hex.EncodeToString(id.EncPublic), name)
		assert.Equal(t, hex.EncodeToString(iv.SigPublic), hex.EncodeToString(id.SigPublic), name)

		fp, err := Fingerprint(id.Public())
		require.NoError(t, err)
		assert.Equal(t, hex.EncodeToString(iv.Fingerprint), hex.EncodeToString(fp), name)
		fps, err := FingerprintString(fp)
		require.NoError(t, err)
		assert.Equal(t, iv.FingerprintString, fps, name)
		assert.Len(t, fps, 8*4+7)

		sig, err := Sign(id.SigPrivate, iv.SignContext, iv.SignMessage)
		require.NoError(t, err)
		assert.Equal(t, hex.EncodeToString(iv.Signature), hex.EncodeToString(sig), name)
		require.NoError(t, Verify(id.SigPublic, iv.SignContext, iv.SignMessage, iv.Signature))

		got, err := OpenIdentity(iv.MasterKey, iv.WrappedEnc, iv.WrappedSig)
		require.NoError(t, err)
		assert.Equal(t, id, got, name)
		enc, err := seal(FormatBox, iv.MasterKey, iv.NonceEnc, id.EncPrivate, []byte(labelIdentityEnc))
		require.NoError(t, err)
		assert.Equal(t, hex.EncodeToString(iv.WrappedEnc), hex.EncodeToString(enc), name)
	}
}

func TestIdentity_GenerateWrapOpen(t *testing.T) {
	id, err := GenerateIdentity()
	require.NoError(t, err)
	assert.Len(t, id.EncPublic, PublicKeySize)
	assert.Equal(t, SigEd25519, id.SigPrivate[0])
	assert.Equal(t, SigEd25519, id.SigPublic[0])
	mk := key(t, 4)
	we, ws, err := WrapIdentity(mk, id)
	require.NoError(t, err)

	got, err := OpenIdentity(mk, we, ws)
	require.NoError(t, err)
	assert.Equal(t, id, got)
	_, err = OpenIdentity(key(t, 5), we, ws)
	assert.ErrorIs(t, err, ErrOpen, "another master key")
	_, err = OpenIdentity(mk, ws, we)
	assert.ErrorIs(t, err, ErrOpen, "each private key opens only in its own role")
}

// TestFingerprint_CoversBothKeys: the fingerprint changes with either
// public key, and two identities never share one.
func TestFingerprint_CoversBothKeys(t *testing.T) {
	alice, _ := goldenIdentity(t, "alice")
	bob, _ := goldenIdentity(t, "bob")
	fa, err := Fingerprint(alice.Public())
	require.NoError(t, err)
	fb, err := Fingerprint(bob.Public())
	require.NoError(t, err)
	assert.NotEqual(t, fa, fb)
	mixed1, err := Fingerprint(PublicIdentity{EncPublic: alice.EncPublic, SigPublic: bob.SigPublic})
	require.NoError(t, err)
	mixed2, err := Fingerprint(PublicIdentity{EncPublic: bob.EncPublic, SigPublic: alice.SigPublic})
	require.NoError(t, err)
	for _, m := range [][]byte{mixed1, mixed2} {
		assert.NotEqual(t, fa, m)
		assert.NotEqual(t, fb, m)
	}
	_, err = Fingerprint(PublicIdentity{EncPublic: alice.EncPublic[:100], SigPublic: alice.SigPublic})
	assert.Error(t, err)
	hybrid := append([]byte{SigEd25519MLDSA65}, alice.SigPublic[1:]...)
	_, err = Fingerprint(PublicIdentity{EncPublic: alice.EncPublic, SigPublic: hybrid})
	assert.ErrorIs(t, err, ErrFormat, "the reserved hybrid is not implemented: never fingerprinted as Ed25519")
}

// TestSignature_Refusals: another key, another context, another message,
// a tampered signature and the reserved hybrid alg are refused.
func TestSignature_Refusals(t *testing.T) {
	alice, iv := goldenIdentity(t, "alice")
	mallory, _ := goldenIdentity(t, "mallory")
	sig := iv.Signature
	assert.ErrorIs(t, Verify(mallory.SigPublic, iv.SignContext, iv.SignMessage, sig), ErrSignature)
	assert.ErrorIs(t, Verify(alice.SigPublic, "another purpose", iv.SignMessage, sig), ErrSignature)
	assert.ErrorIs(t, Verify(alice.SigPublic, iv.SignContext, []byte("another message"), sig), ErrSignature)
	bad := append([]byte{}, sig...)
	bad[10] ^= 1
	assert.ErrorIs(t, Verify(alice.SigPublic, iv.SignContext, iv.SignMessage, bad), ErrSignature)
	hybrid := append([]byte{SigEd25519MLDSA65}, sig[1:]...)
	assert.ErrorIs(t, Verify(alice.SigPublic, iv.SignContext, iv.SignMessage, hybrid), ErrFormat)
	assert.ErrorIs(t, Verify(alice.SigPublic, iv.SignContext, iv.SignMessage, nil), ErrFormat)
	_, err := Sign(append([]byte{SigEd25519MLDSA65}, alice.SigPrivate[1:]...), "x", nil)
	assert.ErrorIs(t, err, ErrFormat)
}
