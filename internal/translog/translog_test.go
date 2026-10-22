package translog

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/sumdb/note"
)

func hd(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

// memLog appends n bodies "entry-0", "entry-1", … to a fresh Memory log.
func memLog(t *testing.T, n int) *Memory {
	t.Helper()
	m := &Memory{}
	for i := 0; i < n; i++ {
		_, err := m.Append([]byte(fmt.Sprintf("entry-%d", i)))
		require.NoError(t, err)
	}
	return m
}

// TestTranslog_KnownAnswer_RFC6962Vectors: the leaf inputs and root hashes of
// github.com/transparency-dev/merkle@v0.0.2 testonly/constants.go
// (LeafInputs, RootHashes — the RFC 6962 vectors of the Certificate
// Transparency code base), for every tree size 0…8.
func TestTranslog_KnownAnswer_RFC6962Vectors(t *testing.T) {
	leaves := []string{"", "00", "10", "2021", "3031", "40414243", "5051525354555657", "606162636465666768696a6b6c6d6e6f"}
	roots := []string{
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d",
		"fac54203e7cc696cf0dfcb42c92a1d9dbaf70ad9e621f4bd8d98662f00e3c125",
		"aeb6bcfe274b70a14fb067a5e5578264db0fa9b51af5e0ba159158f329e06e77",
		"d37ee418976dd95753c1c73862b9398fa2a2cf9b4ff0fdfe8b30cd95209614b7",
		"4e3bbb1f7b478dcfe71fb631631519a3bca12c9aefca1612bfce4c13a86264d4",
		"76e67dadbcdf1e10e1b74ddc608abd2f98dfb16fbce75277b5232a127f2087ef",
		"ddb89be403809e325750d3d263cd78929c2942b7942a34b77e122c9594a74c8c",
		"5dc9da79a70659a9ad559cb701ded9a2ab9d823aad2f4960cfe370eff4604328",
	}
	m := &Memory{}
	root, err := Root(0, m)
	require.NoError(t, err)
	assert.Equal(t, roots[0], hex.EncodeToString(root[:]), "the empty tree's root is SHA-256 of nothing")
	for i, l := range leaves {
		_, err := m.Append(hd(t, l))
		require.NoError(t, err)
		root, err := Root(int64(i+1), m)
		require.NoError(t, err)
		assert.Equal(t, roots[i+1], hex.EncodeToString(root[:]), "root of size %d", i+1)
	}
	// The first leaf hash is the vector's level-0 node 0.
	lh := LeafHash(nil)
	assert.Equal(t, "6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d", hex.EncodeToString(lh[:]))
}

func TestTranslog_InclusionProofVerifies(t *testing.T) {
	for _, size := range []int64{1, 2, 3, 7, 8, 9, 255, 256, 257, 1000} {
		m := memLog(t, int(size))
		root, err := Root(size, m)
		require.NoError(t, err)
		for _, idx := range []int64{0, size / 2, size - 1} {
			proof, err := InclusionProof(idx, size, m)
			require.NoError(t, err, "size %d idx %d", size, idx)
			leaf := LeafHash([]byte(fmt.Sprintf("entry-%d", idx)))
			require.NoError(t, VerifyInclusion(proof, idx, size, leaf, root), "size %d idx %d", size, idx)

			// A different body is not included.
			other := LeafHash([]byte(fmt.Sprintf("entry-%d!", idx)))
			assert.Error(t, VerifyInclusion(proof, idx, size, other, root), "size %d idx %d: another body verified", size, idx)
			// Nor under another root.
			bad := root
			bad[0] ^= 1
			assert.Error(t, VerifyInclusion(proof, idx, size, leaf, bad), "size %d idx %d: another root verified", size, idx)
		}
	}
}

func TestTranslog_ConsistencyProof_RewriteDetected(t *testing.T) {
	const oldSize, newSize = 37, 300
	m := memLog(t, newSize)
	oldRoot, err := Root(oldSize, m)
	require.NoError(t, err)
	newRoot, err := Root(newSize, m)
	require.NoError(t, err)

	proof, err := ConsistencyProof(oldSize, newSize, m)
	require.NoError(t, err)
	require.NoError(t, VerifyConsistency(proof, oldSize, newSize, oldRoot, newRoot), "the honest log is consistent")

	// The same sizes from the empty tree and with themselves.
	p0, err := ConsistencyProof(0, newSize, m)
	require.NoError(t, err)
	require.NoError(t, VerifyConsistency(p0, 0, newSize, EmptyRoot(), newRoot))
	pSame, err := ConsistencyProof(newSize, newSize, m)
	require.NoError(t, err)
	require.NoError(t, VerifyConsistency(pSame, newSize, newSize, newRoot, newRoot))

	// A log that rewrote entry 5 (inside the old tree) and re-grew to the
	// same size: its proof does not take the old checkpoint to its root.
	forged := &Memory{}
	for i := 0; i < newSize; i++ {
		body := fmt.Sprintf("entry-%d", i)
		if i == 5 {
			body = "entry-5-rewritten"
		}
		_, err := forged.Append([]byte(body))
		require.NoError(t, err)
	}
	forgedRoot, err := Root(newSize, forged)
	require.NoError(t, err)
	forgedProof, err := ConsistencyProof(oldSize, newSize, forged)
	require.NoError(t, err)
	assert.Error(t, VerifyConsistency(forgedProof, oldSize, newSize, oldRoot, forgedRoot), "a rewritten history verified")
	// Nor does the honest proof carry the old root to the forged one.
	assert.Error(t, VerifyConsistency(proof, oldSize, newSize, oldRoot, forgedRoot))
	// A shrunk log is never consistent.
	assert.Error(t, VerifyConsistency(nil, newSize, oldSize, newRoot, oldRoot))
}

func TestTranslog_CheckpointIsC2SPSignedNote(t *testing.T) {
	skey, vkey, err := GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := NewSigner(skey)
	require.NoError(t, err)
	verifier, err := NewVerifier(vkey)
	require.NoError(t, err)

	m := memLog(t, 10)
	root, err := Root(10, m)
	require.NoError(t, err)
	msg, err := SignCheckpoint(signer, 10, root)
	require.NoError(t, err)

	// Opened by golang.org/x/mod/sumdb/note itself with the verifier key.
	n, err := note.Open(msg, note.VerifierList(verifier))
	require.NoError(t, err)
	require.Len(t, n.Sigs, 1)
	assert.Equal(t, Origin, n.Sigs[0].Name, "the key is named after the origin")
	// c2sp.org/tlog-checkpoint: origin, decimal size, base64 root, each on its own line.
	assert.Equal(t, "stored.cloud/translog/v1\n10\n"+base64.StdEncoding.EncodeToString(root[:])+"\n", n.Text)
	// c2sp.org/signed-note: the signature line is "— <name> <base64(keyID ‖ sig)>",
	// keyID = SHA-256(name ‖ 0x0A ‖ 0x01 ‖ pubkey)[:4] (0x01 = Ed25519).
	parts := strings.SplitN(vkey, "+", 3)
	require.Len(t, parts, 3)
	raw, err := base64.StdEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	require.Equal(t, byte(0x01), raw[0], "Ed25519")
	sum := sha256.Sum256(append([]byte(Origin+"\n"), raw...))
	sigLine := string(msg[strings.Index(string(msg), "\n\n")+2:])
	require.True(t, strings.HasPrefix(sigLine, "— "+Origin+" "), sigLine)
	sigRaw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(sigLine, "— "+Origin+" ")))
	require.NoError(t, err)
	assert.Equal(t, sum[:4], sigRaw[:4], "key ID")
	assert.Len(t, sigRaw, 4+64, "Ed25519 signature")

	cp, err := OpenCheckpoint(msg, verifier)
	require.NoError(t, err)
	assert.Equal(t, Checkpoint{Origin: Origin, Size: 10, Root: root}, cp)

	// Another key does not open it; a changed size does not verify.
	_, otherV, err := GenerateKey(rand.Reader)
	require.NoError(t, err)
	ov, err := NewVerifier(otherV)
	require.NoError(t, err)
	_, err = OpenCheckpoint(msg, ov)
	assert.Error(t, err)
	tampered := bytes.Replace(msg, []byte("\n10\n"), []byte("\n11\n"), 1)
	_, err = OpenCheckpoint(tampered, verifier)
	assert.Error(t, err)
}

func TestTranslog_KeysNamedAfterTheOrigin(t *testing.T) {
	skey, _, err := note.GenerateKey(rand.Reader, "example.com/other")
	require.NoError(t, err)
	_, err = NewSigner(skey)
	assert.Error(t, err, "a key for another origin must not sign our checkpoints")
	_, err = NewSigner("not a key")
	assert.Error(t, err)
	_, err = NewVerifier("not a key")
	assert.Error(t, err)
}

// c2sp.org/tlog-tiles § "Merkle Tree" / "Log Entries": tile/<L>/<N>[.p/<W>]
// and tile/entries/<N>[.p/<W>], N in 3-digit groups, all but the last
// prefixed by x.
func TestTranslog_TilePaths_C2SP(t *testing.T) {
	cases := []struct {
		level int
		n     int64
		width int
		want  string
	}{
		{0, 0, TileWidth, "tile/0/000"},
		{0, 1234067, TileWidth, "tile/0/x001/x234/067"},
		{1, 1234067, 8, "tile/1/x001/x234/067.p/8"},
		{2, 999, TileWidth, "tile/2/999"},
		{3, 1000, 255, "tile/3/x001/000.p/255"},
		{EntriesLevel, 1234067, 129, "tile/entries/x001/x234/067.p/129"},
		{EntriesLevel, 5, TileWidth, "tile/entries/005"},
	}
	for _, c := range cases {
		got := TilePath(c.level, c.n, c.width)
		assert.Equal(t, c.want, got)
		l, n, w, err := ParseTilePath(got)
		require.NoError(t, err, got)
		assert.Equal(t, []any{c.level, c.n, c.width}, []any{l, n, w}, got)
	}
	for _, bad := range []string{
		"tile/0/1", "tile/0/x1/000", "tile/0/001/000", "tile/0/x000/000", "tile/64/000",
		"tile/0/000.p/0", "tile/0/000.p/256", "tile/0/000.p/08", "tile/a/000", "tile/0",
		"tile/0/000/", "tiles/0/000", "tile/-1/000", "tile/0/x001/x234/067.p/",
	} {
		_, _, _, err := ParseTilePath(bad)
		assert.Error(t, err, bad)
	}
}

func TestTranslog_TileData(t *testing.T) {
	m := memLog(t, 300)
	// Level 0, tile 0: the first 256 leaf hashes, back to back.
	full, err := TileData(0, 0, TileWidth, m)
	require.NoError(t, err)
	require.Len(t, full, 256*HashSize)
	for i := 0; i < 256; i++ {
		lh := LeafHash([]byte(fmt.Sprintf("entry-%d", i)))
		require.Equal(t, lh[:], full[i*HashSize:(i+1)*HashSize], "leaf %d", i)
	}
	// Level 1, tile 0, width 1: the root of the first 256 entries.
	top, err := TileData(1, 0, 1, m)
	require.NoError(t, err)
	r256, err := Root(256, m)
	require.NoError(t, err)
	assert.Equal(t, r256[:], top)
	// The partial level-0 tile 1 holds entries 256…299.
	part, err := TileData(0, 1, 44, m)
	require.NoError(t, err)
	assert.Len(t, part, 44*HashSize)
	lh := LeafHash([]byte("entry-299"))
	assert.Equal(t, lh[:], part[43*HashSize:])
}

// c2sp.org/tlog-tiles § "Log Entries": a bundle is each entry as a big-endian
// uint16 length followed by its bytes.
func TestTranslog_EntryBundle(t *testing.T) {
	b, err := EntryBundle([][]byte{[]byte("ab"), {}, []byte("xyz")})
	require.NoError(t, err)
	assert.Equal(t, []byte{0, 2, 'a', 'b', 0, 0, 0, 3, 'x', 'y', 'z'}, b)
	got, err := ParseEntryBundle(b)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("ab"), {}, []byte("xyz")}, got)
	_, err = EntryBundle([][]byte{make([]byte, MaxEntrySize+1)})
	assert.Error(t, err)
	_, err = ParseEntryBundle([]byte{0, 5, 'a'})
	assert.Error(t, err)
}

func TestTranslog_StoredHashesAreWhatTheReaderNeeds(t *testing.T) {
	// The store persists, per entry, StoredHashes at StoredHashIndex(0, n)…;
	// Memory is built from nothing else, so its roots prove the contract.
	m := memLog(t, 513)
	assert.Equal(t, StoredHashCount(513), int64(len(m.hashes)))
	idx, hashes, err := StoredHashes(513, []byte("entry-513"), m)
	require.NoError(t, err)
	assert.Equal(t, StoredHashCount(513), idx)
	assert.Len(t, hashes, int(StoredHashCount(514)-StoredHashCount(513)))
}

func TestTranslog_TileWidthAt(t *testing.T) {
	assert.Equal(t, 0, TileWidthAt(0, 0, 0))
	assert.Equal(t, 44, TileWidthAt(0, 1, 300))
	assert.Equal(t, TileWidth, TileWidthAt(0, 0, 300))
	assert.Equal(t, 0, TileWidthAt(0, 2, 300))
	assert.Equal(t, 1, TileWidthAt(1, 0, 300), "one complete 256-subtree")
	assert.Equal(t, 0, TileWidthAt(2, 0, 300))
	assert.Equal(t, 44, TileWidthAt(EntriesLevel, 1, 300))
	assert.Equal(t, 0, TileWidthAt(1, 0, 255))
}
