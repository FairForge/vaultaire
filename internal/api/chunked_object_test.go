package api

import (
	"context"
	"testing"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R8-4: the chunker's identity is on every chunked object's record, and
// the chunked paths share one threshold and one floor.

// The floor of a chunked object is a constant because every class that may
// chunk bills on it. The day a vault-floor class is allowed to chunk, this
// test says where the floor must be derived from the class instead.
func TestChunkedObjectFloor_IsTheFloorOfEveryClassThatChunks(t *testing.T) {
	for _, class := range []string{"", "STANDARD", "REDUCED_REDUNDANCY", "STANDARD_IA", "GLACIER", "DEEP_ARCHIVE", "RESILIENT", "PUBLIC"} {
		if storageClassDisablesChunking(class) {
			continue
		}
		assert.Equal(t, chunkedObjectFloor, usage.FloorOf(class), "class %q may chunk and must bill on the chunked floor", class)
	}
	assert.Equal(t, usage.FloorVault, usage.FloorOf("GLACIER"))
	assert.True(t, storageClassDisablesChunking("GLACIER"), "a vault-floor class never chunks")
}

func TestChunkThreshold_DefaultAndOverride(t *testing.T) {
	a := &S3ToEngine{}
	assert.Equal(t, int64(64<<20), a.chunkThreshold())
	a.chunkingThreshold = 1024
	assert.Equal(t, int64(1024), a.chunkThreshold())
}

func TestChunkedPut_RecordsTheChunkerIdentity(t *testing.T) {
	// Arrange
	f := setupChunkAddrFixture(t)
	content := generateTestData(8 * 1024)

	// Act
	f.put(f.a, "a.bin", content)

	// Assert: the record says what cut the chunks — and it is the default
	// chunker, the only one the product has ever had.
	meta, err := f.adapter.gci.GetObjectMetadata(context.Background(), f.a.ID, "test-bucket", "a.bin")
	require.NoError(t, err)
	require.NotNil(t, meta)
	require.NotNil(t, meta.PipelineConfig, "pipeline_config was NULL on every chunked object before WP-R8-4")
	require.NotNil(t, meta.PipelineConfig.Chunker)
	assert.Equal(t, crypto.DefaultChunkerIdentity(), *meta.PipelineConfig.Chunker)
	assert.Equal(t, crypto.ChunkingRabin, meta.PipelineConfig.ChunkingAlgo)
	assert.Equal(t, crypto.ChunkExpectedAverage, meta.PipelineConfig.ChunkAvgSize)
	assert.False(t, meta.PipelineConfig.EncryptionEnabled, "no master key in this fixture")
	assert.True(t, meta.PipelineConfig.DedupCrossTenant)

	// The raw column is JSON a human can read.
	var raw string
	require.NoError(t, f.db.QueryRow(`SELECT pipeline_config::text FROM object_metadata WHERE tenant_id = $1 AND bucket_name = 'test-bucket' AND object_key = 'a.bin'`, f.a.ID).Scan(&raw))
	assert.Contains(t, raw, `"library": "github.com/restic/chunker"`)
	assert.Contains(t, raw, `"version": "v0.4.0"`)
	assert.Contains(t, raw, `"polynomial": "0x2ADD89E3B790BB"`)
	assert.Contains(t, raw, `"average_bits": 20`)
}

func TestChunkedCopy_CarriesTheSourcesChunkerIdentity(t *testing.T) {
	f := setupChunkedCopyFixture(t)
	f.seedChunked(t, "src.bin", generateTestData(8*1024))

	require.Equal(t, 200, f.copyObject(t, "src.bin", "dest-bucket", "copy.bin").Code)

	meta, err := f.server.gci.GetObjectMetadata(context.Background(), f.tenantID, "dest-bucket", "copy.bin")
	require.NoError(t, err)
	require.NotNil(t, meta)
	require.NotNil(t, meta.PipelineConfig)
	require.NotNil(t, meta.PipelineConfig.Chunker)
	assert.Equal(t, crypto.DefaultChunkerIdentity(), *meta.PipelineConfig.Chunker)
}

// A source written before WP-R8-4 (no record): the copy records the default
// chunker — the only one that could have cut it.
func TestCopiedPipeline_SourceWithoutARecordGetsTheDefault(t *testing.T) {
	got := copiedPipeline(&crypto.ObjectMeta{})
	require.NotNil(t, got.Chunker)
	assert.Equal(t, crypto.DefaultChunkerIdentity(), *got.Chunker)
	enc := copiedPipeline(&crypto.ObjectMeta{PipelineConfig: &crypto.PipelineConfig{EncryptionEnabled: true}})
	assert.True(t, enc.EncryptionEnabled)
	assert.False(t, enc.DedupCrossTenant)
}
