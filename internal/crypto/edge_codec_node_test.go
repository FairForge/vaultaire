package crypto

// WP-EDGE-2 Part B: the Worker's 0x06 sealer (edge/s3-ingest/src/seal.js)
// is held to this package's opener. TestEdgeCodec_ExportVectorForNode
// derives a LAB key pair from a fixed seed and keeps it in
// testdata/edge_codec_node.json (with the sealed-length vectors);
// edge/s3-ingest/test/seal.test.js seals the golden 3 MiB chunk under that
// public key; TestEdgeCodec_NodeSealedChunkOpens opens the node's blob under
// the same seed and compares the plaintext. `make test-edge` runs the node
// side, then this one with EDGE_CODEC_NODE_SEALED set.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/sdk/go/e2ee"
)

const edgeCodecNodeVectorPath = "testdata/edge_codec_node.json"

// edgeCodecNodeVector is the file's shape.
type edgeCodecNodeVector struct {
	Comment       string           `json:"comment"`
	Seed          string           `json:"seed"`       // hex: the LAB X-Wing seed (never a real key)
	PublicKey     string           `json:"public_key"` // base64, 1216 bytes
	KeyVersion    int              `json:"key_version"`
	ChunkSeed     uint64           `json:"chunk_seed"` // GoldenBody(chunk_seed, chunk_size) is the chunk sealed
	ChunkSize     int              `json:"chunk_size"`
	SealedLengths map[string]int64 `json:"sealed_lengths"` // WrappedChunkSealedLength by size
}

func edgeCodecNodeSeed() []byte {
	s := sha256.Sum256([]byte("stored/edge-codec/node-vector/v1 — a lab key, never a tenant's"))
	return s[:]
}

func wantEdgeCodecNodeVector(t *testing.T) edgeCodecNodeVector {
	t.Helper()
	seed := edgeCodecNodeSeed()
	pub, err := e2ee.PublicKey(seed)
	require.NoError(t, err)
	v := edgeCodecNodeVector{
		Comment:       "WP-EDGE-2 Part B: a LAB codec key pair for the Worker's sealer test (TestEdgeCodec_ExportVectorForNode writes it; edge/s3-ingest/test/seal.test.js reads it)",
		Seed:          hex.EncodeToString(seed),
		PublicKey:     base64.StdEncoding.EncodeToString(pub),
		KeyVersion:    3,
		ChunkSeed:     20261027,
		ChunkSize:     3 << 20,
		SealedLengths: map[string]int64{},
	}
	for _, n := range []int64{1, 1 << 20, 1<<20 + 1, 3 << 20, 16 << 20} {
		v.SealedLengths[strconv.FormatInt(n, 10)] = WrappedChunkSealedLength(n)
	}
	return v
}

// TestEdgeCodec_ExportVectorForNode keeps the vector file equal to what the
// derivation gives (it is deterministic: written once, then only compared;
// a missing or different file is rewritten and the test fails so the
// change is seen).
func TestEdgeCodec_ExportVectorForNode(t *testing.T) {
	want := wantEdgeCodecNodeVector(t)
	raw, err := json.MarshalIndent(want, "", "  ")
	require.NoError(t, err)
	raw = append(raw, '\n')
	have, rerr := os.ReadFile(edgeCodecNodeVectorPath)
	if rerr == nil && bytes.Equal(have, raw) {
		return
	}
	require.NoError(t, os.WriteFile(edgeCodecNodeVectorPath, raw, 0o600))
	t.Fatalf("%s was missing or stale and has been rewritten: commit it", edgeCodecNodeVectorPath)
}

// TestEdgeCodec_NodeSealedChunkOpens opens the node sealer's blob.
func TestEdgeCodec_NodeSealedChunkOpens(t *testing.T) {
	path := os.Getenv("EDGE_CODEC_NODE_SEALED")
	if path == "" {
		t.Skip("EDGE_CODEC_NODE_SEALED unset: `make test-edge` runs the node sealer first, then this")
	}
	blob, err := os.ReadFile(filepath.Clean(path))
	require.NoError(t, err, "the node side writes it (edge/s3-ingest/test/seal.test.js)")
	v := wantEdgeCodecNodeVector(t)
	plain := GoldenBody(v.ChunkSeed, v.ChunkSize)
	require.Equal(t, WrappedChunkSealedLength(int64(len(plain))), int64(len(blob)), "the sealed length")
	o, h, err := OpenWrappedChunk(edgeCodecNodeSeed(), bytes.NewReader(blob))
	require.NoError(t, err, "the Go opener opens the Worker's blob")
	assert.Equal(t, v.KeyVersion, h.KeyVersion)
	assert.Equal(t, StreamFormatWrapped, h.Format)
	got, err := io.ReadAll(o)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(plain, got), "the plaintext, byte for byte")

	// A flipped byte in the last segment fails the open: the node's tags are real.
	bad := append([]byte(nil), blob...)
	bad[len(bad)-20] ^= 1
	o2, _, err := OpenWrappedChunk(edgeCodecNodeSeed(), bytes.NewReader(bad))
	if err == nil {
		_, err = io.ReadAll(o2)
	}
	assert.Error(t, err)
}
