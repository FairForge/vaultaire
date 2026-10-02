package crypto

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R8-4 — the chunker's identity is on record and pinned.
//
// Every chunk hash in global_content_index follows from the chunker: the
// library's boundary rule, the polynomial, the split mask, the sizes. A
// change to any of them cuts new uploads at other boundaries, and nothing
// stored would ever deduplicate against them again. These tests make such a
// change a build failure instead of a silent reset.

// goldenInput is a fixed pseudo-random input (PCG is a specified generator:
// the same seeds give the same bytes on every Go version and platform).
func goldenInput() []byte {
	r := rand.New(rand.NewPCG(0x5741, 0x52383734))
	data := make([]byte, 24<<20)
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	return data
}

// goldenBoundaries were cut by DefaultChunker on 2026-10-02 with
// github.com/restic/chunker v0.4.0 (offset, size, first 16 hex of the
// SHA-256). A dependency bump, a polynomial change, a different split mask
// or minimum: at least one of these moves.
var goldenBoundaries = []struct {
	offset int64
	size   int
	hash   string
}{
	{0, 7738218, "a59702920ee8c944"},
	{7738218, 4502838, "89ce6272bd07823b"},
	{12241056, 2604825, "c3290142d50b5bc1"},
	{14845881, 1649889, "5229d3e36bede017"},
	{16495770, 1116967, "a2a32acf651a7199"},
	{17612737, 1370068, "b1095d99e94801a5"},
	{18982805, 1200409, "d62d6121a710cbf6"},
	{20183214, 1076428, "ce55265f3cf8e1c8"},
	{21259642, 1533592, "945b21de90d7d623"},
	{22793234, 1130797, "e68eb1594673df79"},
	{23924031, 1241793, "2b4fc25a5c2aa263"},
}

func TestRabinChunker_GoldenBoundaries(t *testing.T) {
	// Arrange
	c, err := DefaultChunker()
	require.NoError(t, err)

	// Act
	chunks, err := c.ChunkBytes(goldenInput())
	require.NoError(t, err)

	// Assert: exactly the recorded cuts.
	require.Len(t, chunks, len(goldenBoundaries), "a different number of chunks: the chunker changed — that resets dedup for everything stored")
	for i, want := range goldenBoundaries {
		assert.Equal(t, want.offset, chunks[i].Offset, "chunk %d offset", i)
		assert.Equal(t, want.size, chunks[i].Size, "chunk %d size", i)
		assert.Equal(t, want.hash, chunks[i].Hash[:16], "chunk %d hash", i)
	}

	// The streaming path cuts the same way.
	ch, err := c.Chunk(&byteReader{data: goldenInput()})
	require.NoError(t, err)
	i := 0
	for res := range ch {
		require.NoError(t, res.Err)
		require.Less(t, i, len(goldenBoundaries))
		assert.Equal(t, goldenBoundaries[i].offset, res.Chunk.Offset, "streamed chunk %d", i)
		assert.Equal(t, goldenBoundaries[i].size, res.Chunk.Size, "streamed chunk %d", i)
		i++
	}
	assert.Equal(t, len(goldenBoundaries), i)
}

// The recorded identity is the one DefaultChunker cuts with, and it says
// what the real average is (≈2 MiB: the 1 MiB minimum plus the expected
// ~1 MiB to a 20-bit boundary), not the 4 MiB the docs used to claim.
func TestChunkerIdentity_IsWhatTheChunkerDoes(t *testing.T) {
	c, err := DefaultChunker()
	require.NoError(t, err)
	id := DefaultChunkerIdentity()

	assert.Equal(t, id, c.Identity())
	assert.Equal(t, ChunkerIdentity{
		Library: "github.com/restic/chunker", Version: "v0.4.0",
		Polynomial: "0x2ADD89E3B790BB", MinSize: 1 << 20, AverageBits: 20, MaxSize: 16 << 20,
	}, id)
	assert.Equal(t, 2<<20, ChunkExpectedAverage)

	chunks, err := c.ChunkBytes(goldenInput())
	require.NoError(t, err)
	avg := float64(len(goldenInput())) / float64(len(chunks))
	assert.InDelta(t, float64(ChunkExpectedAverage), avg, float64(ChunkExpectedAverage)/2,
		"the real average on %d chunks is %.0f bytes", len(chunks), avg)
	assert.NotEqual(t, 4<<20, ChunkExpectedAverage, "4 MiB was the fiction")
	assert.Equal(t, ChunkExpectedAverage, ConfigSmartStorage.ChunkAvgSize)
}

// The identity's library version is the module version go.mod pins: a bump
// has to change both — and pass the golden test.
func TestChunkerIdentity_MatchesGoMod(t *testing.T) {
	gomod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	require.NoError(t, err)
	m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(ChunkerLibrary) + `\s+(v[0-9.]+)`).FindSubmatch(gomod)
	require.NotNil(t, m, "go.mod does not require %s", ChunkerLibrary)
	assert.Equal(t, ChunkerLibraryVersion, string(m[1]),
		"go.mod pins %s %s, the identity says %s: update ChunkerLibraryVersion and re-run the golden test", ChunkerLibrary, m[1], ChunkerLibraryVersion)
}

// A chunker built from a pipeline config records the same identity.
func TestChunkerIdentity_FromConfig(t *testing.T) {
	c, err := NewChunkerFromConfig(ConfigSmartStorage)
	require.NoError(t, err)
	rc, ok := c.(*RabinChunker)
	require.True(t, ok)
	assert.Equal(t, DefaultChunkerIdentity(), rc.Identity())
	assert.Equal(t, ChunkingRabin, rc.Algorithm())
}
