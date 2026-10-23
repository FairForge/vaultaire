package e2ee

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- space keys and shares (RP37-15a) ---

func TestSpaceKey_OwnerWrapGolden(t *testing.T) {
	w := loadVectors(t).BoundWraps["space_key_owner"]
	require.NotNil(t, w)
	got, err := seal(FormatBox, w.WrappingKey, w.Nonce, w.Key, spaceKeyAD(w.SpaceID))
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(w.Wrapped), hex.EncodeToString(got))
	sk, err := OpenSpaceKey(w.WrappingKey, w.SpaceID, w.Wrapped)
	require.NoError(t, err)
	assert.Equal(t, []byte(w.Key), sk)
	_, err = OpenSpaceKey(w.WrappingKey, []byte("space-0002"), w.Wrapped)
	assert.ErrorIs(t, err, ErrOpen, "bound to its space id")
	_, err = OpenSpaceKey(w.WrappingKey, nil, w.Wrapped)
	assert.ErrorIs(t, err, ErrID)
}

func shareParties(t *testing.T) (alice, bob, mallory *Identity, v *shareVector) {
	t.Helper()
	alice, _ = goldenIdentity(t, "alice")
	bob, _ = goldenIdentity(t, "bob")
	mallory, _ = goldenIdentity(t, "mallory")
	v = loadVectors(t).Share
	require.NotNil(t, v)
	require.Equal(t, "alice", v.Sender)
	require.Equal(t, "bob", v.Recipient)
	return alice, bob, mallory, v
}

// TestShare_GoldenOpens pins the 0x05 wire from the open side (its HPKE
// layer is random): bob opens alice's share for the space.
func TestShare_GoldenOpens(t *testing.T) {
	alice, bob, _, v := shareParties(t)
	sk, err := OpenShare(bob, v.Share, alice.Public(), v.SpaceID)
	require.NoError(t, err)
	assert.Equal(t, []byte(v.SpaceKey), sk)
	assert.Equal(t, FormatShare, v.Share[0])
	assert.Len(t, v.Share, 1+32+32+4+(1+EncapsulationSize+KeySize+16)+65)
}

func TestShare_RoundTrip(t *testing.T) {
	a, err := GenerateIdentity()
	require.NoError(t, err)
	b, err := GenerateIdentity()
	require.NoError(t, err)
	sk, err := NewSpaceKey()
	require.NoError(t, err)
	share, err := NewShare(a, b.Public(), []byte("family photos"), sk)
	require.NoError(t, err)
	got, err := OpenShare(b, share, a.Public(), []byte("family photos"))
	require.NoError(t, err)
	assert.Equal(t, sk, got)
}

func TestShare_RefusesUnsigned(t *testing.T) {
	alice, bob, _, v := shareParties(t)
	unsigned := v.Share[:len(v.Share)-65]
	_, err := OpenShare(bob, unsigned, alice.Public(), v.SpaceID)
	assert.ErrorIs(t, err, ErrShareUnsigned)
}

// TestShare_RefusesAnotherSigner: the server holds every public key and so
// can seal any key to bob; what it cannot do is sign as alice.
func TestShare_RefusesAnotherSigner(t *testing.T) {
	alice, bob, mallory, v := shareParties(t)

	// Mallory's own, honest share, presented as alice's.
	theirs, err := NewShare(mallory, bob.Public(), v.SpaceID, key(t, 0x66))
	require.NoError(t, err)
	_, err = OpenShare(bob, theirs, alice.Public(), v.SpaceID)
	assert.ErrorIs(t, err, ErrShareSignature, "names another sender")

	// A key of the server's choosing, sealed to bob under alice's
	// fingerprint, signed by a key that is not alice's.
	aliceFP, err := Fingerprint(alice.Public())
	require.NoError(t, err)
	bobFP, err := Fingerprint(bob.Public())
	require.NoError(t, err)
	sealed, err := SealTo(bob.EncPublic, key(t, 0x66), append([]byte(shareInfo), lp(v.SpaceID)...), append(append([]byte{}, aliceFP...), bobFP...))
	require.NoError(t, err)
	forged := append([]byte{FormatShare}, aliceFP...)
	forged = append(forged, bobFP...)
	forged = binary.BigEndian.AppendUint32(forged, uint32(len(sealed)))
	forged = append(forged, sealed...)
	sig, err := Sign(mallory.SigPrivate, shareContext, append(lp(v.SpaceID), forged...))
	require.NoError(t, err)
	_, err = OpenShare(bob, append(forged, sig...), alice.Public(), v.SpaceID)
	assert.ErrorIs(t, err, ErrShareSignature, "signed by another key")

	// Alice's real share with mallory's signature in place of hers.
	resigned := append([]byte{}, v.Share[:len(v.Share)-65]...)
	sig, err = Sign(mallory.SigPrivate, shareContext, append(lp(v.SpaceID), resigned...))
	require.NoError(t, err)
	_, err = OpenShare(bob, append(resigned, sig...), alice.Public(), v.SpaceID)
	assert.ErrorIs(t, err, ErrShareSignature)

	// A flipped bit anywhere under the signature.
	flipped := append([]byte{}, v.Share...)
	flipped[200] ^= 1
	_, err = OpenShare(bob, flipped, alice.Public(), v.SpaceID)
	assert.ErrorIs(t, err, ErrShareSignature)
}

// TestShare_RefusesASenderFieldThatIsNotTheSigner: a share signed by the
// trusted key but naming another identity as its sender (a buggy or hostile
// client of alice's) is refused: the sender field, the HPKE AD and the
// signer are one identity or nothing.
func TestShare_RefusesASenderFieldThatIsNotTheSigner(t *testing.T) {
	alice, bob, mallory, v := shareParties(t)
	mfp, err := Fingerprint(mallory.Public())
	require.NoError(t, err)
	bfp, err := Fingerprint(bob.Public())
	require.NoError(t, err)
	sealed, err := SealTo(bob.EncPublic, key(t, 0x55), append([]byte(shareInfo), lp(v.SpaceID)...), append(append([]byte{}, mfp...), bfp...))
	require.NoError(t, err)
	body := append([]byte{FormatShare}, mfp...)
	body = append(body, bfp...)
	body = binary.BigEndian.AppendUint32(body, uint32(len(sealed)))
	body = append(body, sealed...)
	sig, err := Sign(alice.SigPrivate, shareContext, append(lp(v.SpaceID), body...))
	require.NoError(t, err)
	_, err = OpenShare(bob, append(body, sig...), alice.Public(), v.SpaceID)
	assert.ErrorIs(t, err, ErrShareSignature)
}

func TestShare_RefusesAnotherRecipient(t *testing.T) {
	alice, bob, mallory, v := shareParties(t)
	_, err := OpenShare(mallory, v.Share, alice.Public(), v.SpaceID)
	assert.ErrorIs(t, err, ErrShareRecipient, "addressed to bob")

	// The recipient field rewritten to mallory: hers now, but alice did not
	// sign that.
	mfp, err := Fingerprint(mallory.Public())
	require.NoError(t, err)
	readdressed := append([]byte{}, v.Share...)
	copy(readdressed[33:65], mfp)
	_, err = OpenShare(mallory, readdressed, alice.Public(), v.SpaceID)
	assert.ErrorIs(t, err, ErrShareSignature)
	_, err = OpenShare(bob, readdressed, alice.Public(), v.SpaceID)
	assert.ErrorIs(t, err, ErrShareRecipient)
}

func TestShare_RefusesAnotherSpace(t *testing.T) {
	alice, bob, _, v := shareParties(t)
	_, err := OpenShare(bob, v.Share, alice.Public(), []byte("space-0002"))
	assert.ErrorIs(t, err, ErrShareSignature, "the space id is signed")
	_, err = OpenShare(bob, v.Share[:60], alice.Public(), v.SpaceID)
	assert.ErrorIs(t, err, ErrFormat)
}

// --- chunk ids and verified chunks (RP37-15b, RP37-15c) ---

func TestChunkID_GoldenKeyedPerSpace(t *testing.T) {
	c := loadVectors(t).ChunkID
	require.NotNil(t, c)
	sum := sha256.Sum256(c.Chunk)
	require.Equal(t, []byte(c.ChunkHash), sum[:])
	id, err := ChunkID(c.SpaceKey, sum[:])
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(c.ChunkID), hex.EncodeToString(id))

	again, err := ChunkID(c.SpaceKey, sum[:])
	require.NoError(t, err)
	assert.Equal(t, id, again, "stable within a space: the server dedups by it")
	other, err := ChunkID(key(t, 0x77), sum[:])
	require.NoError(t, err)
	assert.NotEqual(t, id, other, "the same plaintext has unrelated ids in two spaces")
	assert.NotEqual(t, sum[:], id, "an id is never SHA-256(plaintext)")
	assert.NotEqual(t, sum[:], other)
	ck, err := ChunkKey(c.SpaceKey, sum[:])
	require.NoError(t, err)
	assert.NotEqual(t, ck, id, "the id is not the key")
}

// TestChunk_OpenRefusesAPoisonedChunk: a member seals other bytes under
// ChunkKey(space, H) — the key of a chunk others will dedup against — and
// stores them under that chunk's id. Readers refuse them.
func TestChunk_OpenRefusesAPoisonedChunk(t *testing.T) {
	space := key(t, 0x21)
	real := []byte("the chunk everyone in the space has")
	h := sha256.Sum256(real)
	k, err := ChunkKey(space, h[:])
	require.NoError(t, err)
	poison, err := SealChunk(k, []byte("what the member put there instead"))
	require.NoError(t, err)
	_, err = OpenChunk(k, poison, h[:])
	assert.ErrorIs(t, err, ErrChunkMismatch)

	good, err := SealChunk(k, real)
	require.NoError(t, err)
	got, err := OpenChunk(k, good, h[:])
	require.NoError(t, err)
	assert.Equal(t, real, got)
	_, err = OpenChunk(k, good, h[:16])
	assert.Error(t, err)
}

// --- objects and manifests (RP37-15d) ---

func goldenObject(t *testing.T) *objectVector {
	t.Helper()
	o := loadVectors(t).Object
	require.NotNil(t, o)
	require.Len(t, o.Versions, 2)
	return o
}

func TestObjectKey_WrapGolden(t *testing.T) {
	w := loadVectors(t).BoundWraps["object_key"]
	require.NotNil(t, w)
	got, err := seal(FormatBox, w.WrappingKey, w.Nonce, w.Key, objectKeyAD(w.SpaceID, w.ObjectID))
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(w.Wrapped), hex.EncodeToString(got))
	ok, err := OpenObjectKey(w.WrappingKey, w.SpaceID, w.ObjectID, w.Wrapped)
	require.NoError(t, err)
	assert.Equal(t, []byte(w.Key), ok)
	_, err = OpenObjectKey(w.WrappingKey, w.SpaceID, []byte("object-0002"), w.Wrapped)
	assert.ErrorIs(t, err, ErrOpen, "bound to its object")
	_, err = OpenObjectKey(w.WrappingKey, []byte("space-0002"), w.ObjectID, w.Wrapped)
	assert.ErrorIs(t, err, ErrOpen, "and to its space")
}

// TestManifest_GoldenBytes pins the manifest v1 plaintext layout, its AD and
// the chain (recomputed by testdata/vectors_check.py), and reads every chunk
// of both versions through it.
func TestManifest_GoldenBytes(t *testing.T) {
	o := goldenObject(t)
	var seen *ManifestHead
	for _, mv := range o.Versions {
		m, err := OpenManifest(o.ObjectKey, mv.Manifest, o.SpaceID, o.ObjectID, mv.Version, seen)
		require.NoError(t, err, "v%d", mv.Version)
		resealed, err := seal(FormatBox, o.ObjectKey, mv.Nonce, m.encode(), manifestAD(o.SpaceID, o.ObjectID, mv.Version))
		require.NoError(t, err)
		assert.Equal(t, hex.EncodeToString(mv.Manifest), hex.EncodeToString(resealed))
		assert.Equal(t, []byte(mv.Previous), m.Previous)
		assert.Equal(t, hex.EncodeToString(mv.Hash), hex.EncodeToString(ManifestHash(mv.Manifest)))

		var total uint64
		for i, sealed := range mv.Sealed {
			pt, err := m.OpenChunkAt(i, sealed)
			require.NoError(t, err)
			assert.Equal(t, []byte(mv.Chunks[i]), pt)
			ref, again, err := PrepareChunk(o.SpaceKey, mv.Chunks[i])
			require.NoError(t, err)
			assert.Equal(t, []byte(sealed), again, "convergent: the writer's bytes")
			assert.Equal(t, ref, m.Chunks[i])
			total += uint64(len(mv.Chunks[i]))
		}
		assert.Equal(t, total, m.TotalSize)
		seen = &ManifestHead{Version: m.Version, Hash: ManifestHash(mv.Manifest)}
	}
	assert.Equal(t, []byte(o.Versions[0].Hash), []byte(o.Versions[1].Previous), "v2 names v1")
	assert.Equal(t, []byte(o.Versions[0].Sealed[0]), []byte(o.Versions[1].Sealed[0]), "the unchanged chunk is stored once")
}

func TestManifest_RefusesReorderedChunks(t *testing.T) {
	o := goldenObject(t)
	mv := o.Versions[0]
	m, err := OpenManifest(o.ObjectKey, mv.Manifest, o.SpaceID, o.ObjectID, 1, nil)
	require.NoError(t, err)
	_, err = m.OpenChunkAt(0, mv.Sealed[1])
	assert.Error(t, err, "chunk 1 served as chunk 0")
	_, err = m.OpenChunkAt(1, mv.Sealed[0])
	assert.Error(t, err)
	_, err = m.OpenChunkAt(3, mv.Sealed[0])
	assert.ErrorIs(t, err, ErrManifest, "past the end")
	_, err = m.OpenChunkAt(0, append(append([]byte{}, mv.Sealed[0]...), 0))
	assert.ErrorIs(t, err, ErrChunkMismatch, "not the stored length: refused before opening")
}

// TestManifest_RefusesTruncation: a cut sealed manifest does not open; a
// manifest whose list was cut (by a writer bug or a key holder) does not
// pass its own rules; a list shorter than its count does not parse.
func TestManifest_RefusesTruncation(t *testing.T) {
	o := goldenObject(t)
	mv := o.Versions[0]
	_, err := OpenManifest(o.ObjectKey, mv.Manifest[:len(mv.Manifest)-20], o.SpaceID, o.ObjectID, 1, nil)
	assert.ErrorIs(t, err, ErrOpen)

	m, err := OpenManifest(o.ObjectKey, mv.Manifest, o.SpaceID, o.ObjectID, 1, nil)
	require.NoError(t, err)
	cut := *m
	cut.Chunks = cut.Chunks[:2]
	_, err = SealManifest(o.ObjectKey, &cut)
	assert.ErrorIs(t, err, ErrManifest, "the total no longer adds up")
	sealed, err := Seal(o.ObjectKey, cut.encode(), manifestAD(o.SpaceID, o.ObjectID, 1))
	require.NoError(t, err)
	_, err = OpenManifest(o.ObjectKey, sealed, o.SpaceID, o.ObjectID, 1, nil)
	assert.ErrorIs(t, err, ErrManifest)

	enc := m.encode()
	short, err := Seal(o.ObjectKey, enc[:len(enc)-chunkRefSize], manifestAD(o.SpaceID, o.ObjectID, 1))
	require.NoError(t, err)
	_, err = OpenManifest(o.ObjectKey, short, o.SpaceID, o.ObjectID, 1, nil)
	assert.ErrorIs(t, err, ErrManifest, "the count says three, the bytes hold two")
}

// TestManifest_RefusesASpliceFromAnotherObject: another object of the same
// space, under the same space key, cannot stand in for this one — not its
// manifest, not its chunks.
func TestManifest_RefusesASpliceFromAnotherObject(t *testing.T) {
	o := goldenObject(t)
	otherID := []byte("object-0002")
	otherKey := key(t, 0x31)
	ref, otherChunk, err := PrepareChunk(o.SpaceKey, []byte("a chunk of the other object"))
	require.NoError(t, err)
	other := &Manifest{SpaceID: o.SpaceID, ObjectID: otherID, Version: 1, Previous: make([]byte, 32), TotalSize: uint64(ref.Size), Chunks: []ChunkRef{ref}}
	sealedOther, err := SealManifest(otherKey, other)
	require.NoError(t, err)

	_, err = OpenManifest(otherKey, sealedOther, o.SpaceID, o.ObjectID, 1, nil)
	assert.ErrorIs(t, err, ErrOpen, "its manifest names its own object")
	m, err := OpenManifest(o.ObjectKey, o.Versions[0].Manifest, o.SpaceID, o.ObjectID, 1, nil)
	require.NoError(t, err)
	_, err = m.OpenChunkAt(2, otherChunk)
	assert.Error(t, err, "its chunk is not this object's third")

	// The same manifest bytes, sealed by a key holder under this object's
	// AD but naming the other object inside, are refused too.
	inner, err := Seal(o.ObjectKey, other.encode(), manifestAD(o.SpaceID, o.ObjectID, 1))
	require.NoError(t, err)
	_, err = OpenManifest(o.ObjectKey, inner, o.SpaceID, o.ObjectID, 1, nil)
	assert.ErrorIs(t, err, ErrManifest)
}

func TestManifest_RefusesAnotherVersion(t *testing.T) {
	o := goldenObject(t)
	_, err := OpenManifest(o.ObjectKey, o.Versions[0].Manifest, o.SpaceID, o.ObjectID, 2, nil)
	assert.ErrorIs(t, err, ErrOpen, "version 1 presented as version 2")
	_, err = OpenManifest(o.ObjectKey, o.Versions[1].Manifest, o.SpaceID, o.ObjectID, 1, nil)
	assert.ErrorIs(t, err, ErrOpen, "and the other way")
}

// TestManifest_DetectsRollback: a client that has seen version 2 refuses
// version 1 as the latest, another version 2, and a version 3 that does not
// name its version 2.
func TestManifest_DetectsRollback(t *testing.T) {
	o := goldenObject(t)
	v1, v2 := o.Versions[0], o.Versions[1]
	seen := &ManifestHead{Version: 2, Hash: v2.Hash}

	_, err := OpenManifest(o.ObjectKey, v1.Manifest, o.SpaceID, o.ObjectID, 1, seen)
	assert.ErrorIs(t, err, ErrRollback, "an older version")

	m2, err := OpenManifest(o.ObjectKey, v2.Manifest, o.SpaceID, o.ObjectID, 2, seen)
	require.NoError(t, err)
	fork, err := SealManifest(o.ObjectKey, m2) // same content, another nonce: other bytes
	require.NoError(t, err)
	_, err = OpenManifest(o.ObjectKey, fork, o.SpaceID, o.ObjectID, 2, seen)
	assert.ErrorIs(t, err, ErrRollback, "another version 2")

	v3 := *m2
	v3.Version, v3.Previous = 3, v1.Hash
	sealed, err := SealManifest(o.ObjectKey, &v3)
	require.NoError(t, err)
	_, err = OpenManifest(o.ObjectKey, sealed, o.SpaceID, o.ObjectID, 3, seen)
	assert.ErrorIs(t, err, ErrRollback, "a version 3 that skips the version 2 we saw")

	v3.Previous = v2.Hash
	sealed, err = SealManifest(o.ObjectKey, &v3)
	require.NoError(t, err)
	_, err = OpenManifest(o.ObjectKey, sealed, o.SpaceID, o.ObjectID, 3, seen)
	require.NoError(t, err)
}

func TestManifest_Rules(t *testing.T) {
	ref, _, err := PrepareChunk(key(t, 1), []byte("x"))
	require.NoError(t, err)
	base := func() *Manifest {
		return &Manifest{SpaceID: []byte("s"), ObjectID: []byte("o"), Version: 1, Previous: make([]byte, 32), TotalSize: 1, Chunks: []ChunkRef{ref}}
	}
	_, err = SealManifest(key(t, 2), base())
	require.NoError(t, err)
	empty := base()
	empty.Chunks, empty.TotalSize = nil, 0
	_, err = SealManifest(key(t, 2), empty)
	require.NoError(t, err, "an empty object")

	for name, mutate := range map[string]func(m *Manifest){
		"version 0":                   func(m *Manifest) { m.Version = 0 },
		"version 1 names a previous":  func(m *Manifest) { m.Previous = bytes.Repeat([]byte{1}, 32) },
		"version 2 names none":        func(m *Manifest) { m.Version = 2 },
		"a compressor v1 has not got": func(m *Manifest) { m.Compressor = 1 },
		"sealed size not size + 41":   func(m *Manifest) { m.Chunks[0].SealedSize++ },
		"total size":                  func(m *Manifest) { m.TotalSize = 2 },
		"empty space id":              func(m *Manifest) { m.SpaceID = nil },
		"a short chunk id":            func(m *Manifest) { m.Chunks[0].ID = m.Chunks[0].ID[:31] },
	} {
		m := base()
		m.Chunks = append([]ChunkRef{}, m.Chunks...)
		mutate(m)
		_, err := SealManifest(key(t, 2), m)
		assert.Error(t, err, name)
	}
}
