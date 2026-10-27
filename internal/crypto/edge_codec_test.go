package crypto

// WP-EDGE-2 Part A: the 0x06 wrapped-key chunk blob, the keyed index hash
// and the codec key derivations. The golden-opens fixture
// (testdata/edge_codec_golden.sealed, a blob sealed once to the fixed seed
// 0x00..0x1f) pins the whole format — crypto/hpke has no seeded sender, so
// the stored Prompt 46's "fixed KEM ephemeral" is not expressible with the
// stdlib and a golden OPEN is the pin, the sdk's own pattern
// (TestSealedBox_GoldenOpens). The deterministic parts (the DEK derivation,
// the segment nonces and AAD) are pinned byte-for-byte below under a fixed
// chunk key and a hand-built header.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/sdk/go/e2ee"
)

func fixedCodecKeyPair(t *testing.T) (priv, pub []byte) {
	t.Helper()
	priv = make([]byte, 32)
	for i := range priv {
		priv[i] = byte(i)
	}
	pub, err := e2ee.PublicKey(priv)
	require.NoError(t, err)
	return priv, pub
}

func TestWrappedChunk_RoundTrip(t *testing.T) {
	priv, pub := fixedCodecKeyPair(t)
	for _, size := range []int{0, 1, 31, StreamSegmentSize, StreamSegmentSize + 1, 3*StreamSegmentSize + 12345} {
		body := GoldenBody(uint64(size)+1, size)
		r, err := SealWrappedChunk(pub, 7, bytes.NewReader(body), int64(size))
		require.NoError(t, err)
		blob, err := io.ReadAll(r)
		require.NoError(t, err)
		require.Equal(t, WrappedChunkSealedLength(int64(size)), int64(len(blob)), "size %d", size)
		require.Equal(t, StreamFormatWrapped, blob[0])

		o, h, err := OpenWrappedChunk(priv, bytes.NewReader(blob))
		require.NoError(t, err, "size %d", size)
		assert.Equal(t, 7, h.KeyVersion)
		assert.Equal(t, StreamKindWrapped, h.Kind)
		pt, err := io.ReadAll(o)
		require.NoError(t, err)
		assert.True(t, bytes.Equal(body, pt), "size %d", size)
	}
}

func TestWrappedChunk_GoldenOpens(t *testing.T) {
	priv, _ := fixedCodecKeyPair(t)
	blob, err := os.ReadFile("testdata/edge_codec_golden.sealed")
	require.NoError(t, err)
	o, h, err := OpenWrappedChunk(priv, bytes.NewReader(blob))
	require.NoError(t, err)
	assert.Equal(t, 3, h.KeyVersion)
	pt, err := io.ReadAll(o)
	require.NoError(t, err)
	want := GoldenBody(7, 65536+123)
	sum := sha256.Sum256(pt)
	assert.Equal(t, "517bf35fb54813a559f4af5597e83cfd355f8e920c16e92bda20a6a4e8f5c91c", hex.EncodeToString(sum[:]))
	assert.True(t, bytes.Equal(want, pt))
	// The header is what the layout says: version, wrap, segment size.
	assert.Equal(t, StreamHeaderWrapped, len(h.Raw))
	assert.Equal(t, StreamWrapLen, len(h.Wrap))
	assert.Equal(t, StreamSegmentSize, h.SegmentSize)
}

// The deterministic half: a fixed chunk key and a hand-built header pin the
// DEK derivation (HKDF over "stored/edge-codec/stream-v1" ‖ header), the
// segment nonces (prefix ‖ index ‖ last) and the header-only AAD, byte for
// byte. Any change to them is a new format byte, never a silent change.
func TestWrappedChunk_GoldenSegments(t *testing.T) {
	var chunkKey [32]byte
	for i := range chunkKey {
		chunkKey[i] = byte(0xA0 + i)
	}
	material := make([]byte, 0, 4+StreamWrapLen)
	material = binary.BigEndian.AppendUint32(material, 9)
	wrap := bytes.Repeat([]byte{0xC3}, StreamWrapLen)
	material = append(material, wrap...)
	header := make([]byte, 0, StreamHeaderWrapped)
	header = append(header, StreamFormatWrapped, StreamKindWrapped)
	header = append(header, material...)
	header = binary.BigEndian.AppendUint32(header, 32) // tiny segments for the vector
	header = append(header, []byte{1, 2, 3, 4, 5, 6, 7}...)
	require.Len(t, header, StreamHeaderWrapped)

	aead, err := streamAEAD(chunkKey[:], nil, edgeChunkStreamInfo, header)
	require.NoError(t, err)
	body := []byte("the wrapped chunk's golden segments, forty-one.")
	s, err := newStreamSealer(bytes.NewReader(body), aead, header, streamAAD(header, StreamIdentity{}), 32, int64(len(body)))
	require.NoError(t, err)
	blob, err := io.ReadAll(s)
	require.NoError(t, err)
	segments := blob[len(header):]
	assert.Equal(t,
		"694bc5358477d1cced52a2d228cac6bfeb551cbe5ebf64e91b68b01e123ba8d3"+
			"dd0ceed9d5f8934ae560dded2f75ece78f8aecb607cc7f73bfbd4f8f535946e9"+
			"e6065cee4419b1849aeca23bd42186",
		hex.EncodeToString(segments), "the 0x06 segment bytes moved: that is a new format, not an edit")

	// And the AAD is the header ALONE: a 0x06 blob has no object identity.
	assert.Equal(t, header, streamAAD(header, StreamIdentity{Tenant: "t", Bucket: "b", Key: "k"}))

	// The whole blob opens under the same key through the generic opener.
	o := newStreamOpener(bytes.NewReader(segments), aead, &StreamHeader{Raw: header, Format: StreamFormatWrapped,
		Kind: StreamKindWrapped, SegmentSize: 32, NoncePrefix: header[len(header)-7:]}, header)
	require.NoError(t, o.prime())
	pt, err := io.ReadAll(o)
	require.NoError(t, err)
	assert.Equal(t, body, pt)
}

func TestWrappedChunk_TamperFails(t *testing.T) {
	priv, pub := fixedCodecKeyPair(t)
	body := GoldenBody(11, 2*StreamSegmentSize+777)
	r, err := SealWrappedChunk(pub, 2, bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err)
	blob, err := io.ReadAll(r)
	require.NoError(t, err)

	open := func(b []byte) error {
		o, _, err := OpenWrappedChunk(priv, bytes.NewReader(b))
		if err != nil {
			return err
		}
		_, err = io.Copy(io.Discard, o)
		return err
	}
	require.NoError(t, open(blob), "the untouched blob opens")

	// A flipped byte in a segment.
	bad := append([]byte(nil), blob...)
	bad[len(bad)-20] ^= 1
	assert.ErrorIs(t, open(bad), ErrStreamCorrupt)
	// Truncated at a segment boundary.
	cut := blob[:StreamHeaderWrapped+StreamSegmentSize+streamTagSize]
	assert.ErrorIs(t, open(cut), ErrStreamCorrupt)
	// Extended.
	assert.ErrorIs(t, open(append(append([]byte(nil), blob...), 0)), ErrStreamCorrupt)
	// The header's key version flipped: the DEK and the AAD cover the header.
	verFlip := append([]byte(nil), blob...)
	verFlip[5] ^= 1
	assert.Error(t, open(verFlip))
	// Another recipient's seed.
	other := bytes.Repeat([]byte{0x55}, 32)
	_, _, err = OpenWrappedChunk(other, bytes.NewReader(blob))
	assert.ErrorIs(t, err, ErrStreamCorrupt, "the chunk key does not unwrap for another key")
	// A kind that is not the format's is refused before anything is sized.
	mismatch := append([]byte(nil), blob...)
	mismatch[0] = StreamFormat // 0x04 with kind 0x03
	_, perr := ParseStreamHeader(mismatch)
	assert.ErrorIs(t, perr, ErrStreamCorrupt)
	ssesHeader := []byte{StreamFormatV3, StreamKindWrapped}
	_, perr = ParseStreamHeader(append(ssesHeader, make([]byte, StreamHeaderWrapped)...))
	assert.ErrorIs(t, perr, ErrStreamCorrupt)
	_, serr := newStreamHeaderFormat(StreamFormat, StreamKindWrapped, material999(), StreamSegmentSize)
	assert.Error(t, serr, "a writer cannot make a mismatched header either")
}

func material999() []byte {
	m := make([]byte, 4+StreamWrapLen)
	binary.BigEndian.PutUint32(m, 999)
	return m
}

// A 0x06 blob is never an SSE blob: the SSE openers refuse it cleanly, and
// the older formats still parse beside it.
func TestWrappedChunk_IsNotAnSSEBlob(t *testing.T) {
	_, pub := fixedCodecKeyPair(t)
	r, err := SealWrappedChunk(pub, 1, bytes.NewReader([]byte("x")), 1)
	require.NoError(t, err)
	blob, err := io.ReadAll(r)
	require.NoError(t, err)

	assert.True(t, IsStreamFormat(blob[0]), "IsStreamFormat grows by 0x06")
	_, err = SSEBlobKeyVersion(blob)
	assert.Error(t, err, "kind 0x03 has no SSE key version")
	svc := &SSEService{}
	_, err = svc.DecryptBytes(context.Background(), "t", blob)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "streaming")

	// An SSE-C 0x04 blob still seals and opens with MaxStreamHeader grown.
	key := bytes.Repeat([]byte{9}, 32)
	id := StreamIdentity{Tenant: "t", Bucket: "b", Key: "k"}
	sr, err := SSECSeal(key, id, strings.NewReader("older formats still open"), 24)
	require.NoError(t, err)
	sb, err := io.ReadAll(sr)
	require.NoError(t, err)
	or, err := SSECOpen(bytes.Repeat([]byte{9}, 32), id, bytes.NewReader(sb))
	require.NoError(t, err)
	pt, err := io.ReadAll(or)
	require.NoError(t, err)
	assert.Equal(t, "older formats still open", string(pt))
}

func TestKeyedChunkHash_Golden(t *testing.T) {
	indexKey := bytes.Repeat([]byte{0x11}, 32)
	plain := sha256.Sum256([]byte("a chunk"))
	got, err := KeyedChunkHash(indexKey, plain[:])
	require.NoError(t, err)
	// HMAC-SHA256(0x11…, sha256("a chunk")) — recomputable anywhere.
	assert.Equal(t, "803d35467289798bfa30b35751e85ab9aa676b245faf121a7db81b5f1d502918", got)
	_, err = KeyedChunkHash(indexKey[:16], plain[:])
	assert.Error(t, err)
	_, err = KeyedChunkHash(indexKey, plain[:16])
	assert.Error(t, err)
}

// The derivations: stable, distinct, and through TenantKeys bound to the
// version — a rotation changes the index key AND the codec key pair, and a
// wrap sealed to one version's public key opens only under that version's
// seed. Deleting the version's row (the crypto-shred) takes both with it.
func TestEdgeCodecKeys_PerVersionThroughTheHierarchy(t *testing.T) {
	db := getTestDB(t)
	ctx := context.Background()
	tenant := keyTenant(t, db)
	k := newTestKeys(t, db, testMasterA, "")

	pub1, v1, err := k.EdgeCodecPublicKey(ctx, tenant)
	require.NoError(t, err)
	require.Equal(t, 1, v1)
	idx1, iv1, err := k.IndexKey(ctx, tenant, 0)
	require.NoError(t, err)
	require.Equal(t, 1, iv1)
	idx1Again, _, err := k.IndexKey(ctx, tenant, 1)
	require.NoError(t, err)
	assert.Equal(t, idx1, idx1Again, "the index key is a pure function of the version")
	kv, err := k.Version(ctx, tenant, 1)
	require.NoError(t, err)
	seed, err := edgeCodecSeed(kv)
	require.NoError(t, err)
	ik, err := IndexKeyFrom(kv)
	require.NoError(t, err)
	assert.Equal(t, idx1, ik)
	assert.NotEqual(t, seed, ik, "two labels, two keys")
	assert.NotEqual(t, kv.ChunkKey[:], ik, "never the chunk key itself")

	// A chunk sealed to v1 opens under v1's seed and no other.
	body := []byte("rotation leaves old blobs readable under their version")
	r, err := SealWrappedChunk(pub1, v1, bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err)
	blob, err := io.ReadAll(r)
	require.NoError(t, err)
	priv1, err := k.EdgeCodecPrivateKey(ctx, tenant, 1)
	require.NoError(t, err)
	o, h, err := OpenWrappedChunk(priv1, bytes.NewReader(blob))
	require.NoError(t, err)
	assert.Equal(t, 1, h.KeyVersion)
	pt, err := io.ReadAll(o)
	require.NoError(t, err)
	assert.Equal(t, body, pt)

	v2, err := k.Rotate(ctx, tenant)
	require.NoError(t, err)
	pub2, cv, err := k.EdgeCodecPublicKey(ctx, tenant)
	require.NoError(t, err)
	assert.Equal(t, v2, cv)
	assert.NotEqual(t, pub1, pub2, "a rotation is a new codec key pair")
	idx2, _, err := k.IndexKey(ctx, tenant, v2)
	require.NoError(t, err)
	assert.NotEqual(t, idx1, idx2, "and a new index key: old keyed hashes stop matching new uploads")
	priv2, err := k.EdgeCodecPrivateKey(ctx, tenant, v2)
	require.NoError(t, err)
	_, _, err = OpenWrappedChunk(priv2, bytes.NewReader(blob))
	assert.ErrorIs(t, err, ErrStreamCorrupt, "v2's seed does not unwrap a v1 blob")
}

// StreamWrapLen is e2ee's sealed box of a 32-byte key — pinned against the
// sdk's constants so neither drifts alone.
func TestStreamWrapLen_IsTheE2EESealedBox(t *testing.T) {
	assert.Equal(t, 1+e2ee.EncapsulationSize+32+16, StreamWrapLen)
	assert.Equal(t, 2+4+StreamWrapLen+4+7, StreamHeaderWrapped)
	assert.Equal(t, StreamHeaderWrapped, MaxStreamHeader, "the ranged readers fetch the longest header")
}
