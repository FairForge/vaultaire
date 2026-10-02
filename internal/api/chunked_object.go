package api

import (
	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/usage"
)

// The knobs every chunked-object path shares (WP-R8-4; R8-18, R10-32).

// defaultChunkingThreshold is the object size from which a PUT takes the
// chunked path (when the `chunking` flag allows it).
const defaultChunkingThreshold int64 = 64 * 1024 * 1024

// chunkThreshold is the adapter's threshold: the configured one, else the
// default (tests set a small one).
func (a *S3ToEngine) chunkThreshold() int64 {
	if a.chunkingThreshold > 0 {
		return a.chunkingThreshold
	}
	return defaultChunkingThreshold
}

// chunkedObjectFloor is the floor every chunked object — a PUT's or a chunked
// copy's — is billed on. It is a constant by construction, not by accident:
// storageClassDisablesChunking keeps every class that bills on the vault
// floor (GLACIER, DEEP_ARCHIVE) whole, so a chunked object's class always
// resolves to this floor; chunk blobs live on the engine primary, the
// standard tier's backend. TestChunkedObjectFloor_IsTheFloorOfEveryClassThatChunks
// pins the coupling: the day chunking is opened to the attic, this is the
// one place to derive the floor from the class instead.
const chunkedObjectFloor = usage.FloorStandard

// chunkedPipeline is what a chunked object's pipeline_config records: the
// chunker that cut it, and whether its chunks are encrypted. Reads never
// consult it (the index rows describe each chunk); it is the record a later
// change to the chunker is told apart by.
func chunkedPipeline(chunker *crypto.RabinChunker, encrypted bool) *crypto.PipelineConfig {
	return crypto.RecordedPipeline(chunker.Identity(), encrypted)
}

// copiedPipeline is the record for a manifest copy: the source's, when it
// has one; the default chunker's otherwise.
func copiedPipeline(src *crypto.ObjectMeta) *crypto.PipelineConfig {
	if src != nil && src.PipelineConfig != nil && src.PipelineConfig.Chunker != nil {
		cfg := *src.PipelineConfig
		return &cfg
	}
	encrypted := false
	if src != nil && src.PipelineConfig != nil {
		encrypted = src.PipelineConfig.EncryptionEnabled
	}
	return crypto.RecordedPipeline(crypto.DefaultChunkerIdentity(), encrypted)
}
