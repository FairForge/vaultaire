package crypto

// TestChunker_GoldenBoundaries (WP-EDGE-2 Part A): the frozen chunker cuts a
// 100 MiB deterministic body (GoldenBody, seed 20261026 — never a file in
// the repo) at EXACTLY the recorded offsets. The SAME vectors are asserted
// against the WASM build in edge/s3-ingest/test/chunker.test.js (node, under
// `make test-edge`): byte-identical boundaries between Go and WASM are what
// lets the edge's chunks dedup against the origin's. A dependency bump or
// any change to the chunker's identity fails both.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type chunkerGolden struct {
	Seed         uint64 `json:"seed"`
	Size         int    `json:"size"`
	BodySHA256   string `json:"body_sha256"`
	ChunkLengths []int  `json:"chunk_lengths"`
}

func loadChunkerGolden(t *testing.T) chunkerGolden {
	t.Helper()
	b, err := os.ReadFile("testdata/chunker_golden.json")
	require.NoError(t, err)
	var g chunkerGolden
	require.NoError(t, json.Unmarshal(b, &g))
	require.NotEmpty(t, g.ChunkLengths)
	return g
}

func TestChunker_GoldenBoundaries(t *testing.T) {
	if testing.Short() {
		t.Skip("100 MiB body")
	}
	g := loadChunkerGolden(t)
	body := GoldenBody(g.Seed, g.Size)
	sum := sha256.Sum256(body)
	require.Equal(t, g.BodySHA256, hex.EncodeToString(sum[:]), "GoldenBody changed: the node test feeds other bytes")

	ch, err := DefaultChunker()
	require.NoError(t, err)
	chunks, err := ch.ChunkBytes(body)
	require.NoError(t, err)
	lengths := make([]int, len(chunks))
	total := 0
	for i, c := range chunks {
		lengths[i] = c.Size
		total += c.Size
	}
	assert.Equal(t, g.ChunkLengths, lengths, "the frozen chunker's boundaries moved")
	assert.Equal(t, g.Size, total)
}
