package crypto

import (
	mathrand "math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deterministicData produces the same pseudo-random buffer on every call —
// chunk boundaries on it must therefore be stable across chunker instances,
// process restarts, and deploys.
func deterministicData(size int) []byte {
	rng := mathrand.New(mathrand.NewSource(0x5EED))
	data := make([]byte, size)
	_, _ = rng.Read(data)
	return data
}

// TestFastCDCChunker_DeterministicAcrossInstances is the WP-7 step-1 guard:
// dedup only works if the same content always chunks the same way. Two
// independently constructed chunkers must produce identical chunk sequences.
// (Before WP-7 every chunker instance drew a fresh RandomPolynomial, so no
// two PUTs of the same file ever produced matching chunk hashes.)
func TestFastCDCChunker_DeterministicAcrossInstances(t *testing.T) {
	data := deterministicData(8 * 1024 * 1024)

	c1, err := DefaultFastCDCChunker()
	require.NoError(t, err)
	c2, err := DefaultFastCDCChunker()
	require.NoError(t, err)

	chunks1, err := c1.ChunkBytes(data)
	require.NoError(t, err)
	chunks2, err := c2.ChunkBytes(data)
	require.NoError(t, err)

	require.Equal(t, len(chunks1), len(chunks2),
		"two chunker instances must find the same boundaries")
	for i := range chunks1 {
		assert.Equal(t, chunks1[i].Hash, chunks2[i].Hash, "chunk %d hash", i)
		assert.Equal(t, chunks1[i].Offset, chunks2[i].Offset, "chunk %d offset", i)
		assert.Equal(t, chunks1[i].Size, chunks2[i].Size, "chunk %d size", i)
	}
}

// TestFastCDCChunker_PermanentPolynomial pins the polynomial constant.
// CHANGING THIS VALUE ORPHANS EVERY EXISTING CHUNK: stored objects were
// chunked at boundaries this polynomial defines; a different polynomial
// produces different boundaries and different chunk hashes, so no existing
// manifest could ever dedup against or be reassembled from new chunks.
// This test failing means someone changed it — that must never ship.
func TestFastCDCChunker_PermanentPolynomial(t *testing.T) {
	c, err := DefaultFastCDCChunker()
	require.NoError(t, err)
	assert.Equal(t, uint64(0x2ADD89E3B790BB), c.Polynomial(),
		"chunker polynomial is PERMANENT — changing it orphans every stored chunk")

	cfg, err := NewChunkerFromConfig(PipelineConfig{
		ChunkingEnabled: true,
		ChunkingAlgo:    ChunkingFastCDC,
		ChunkMinSize:    1 * 1024 * 1024,
		ChunkAvgSize:    4 * 1024 * 1024,
		ChunkMaxSize:    16 * 1024 * 1024,
	})
	require.NoError(t, err)
	fc, ok := cfg.(*FastCDCChunker)
	require.True(t, ok)
	assert.Equal(t, uint64(0x2ADD89E3B790BB), fc.Polynomial(),
		"config-constructed chunkers must use the same permanent polynomial")
}
