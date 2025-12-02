package crypto

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"sync"
)

// ProcessingBackend wraps a storage backend with crypto processing
type ProcessingBackend struct {
	// Pipeline for processing
	pipeline *Pipeline

	// Key manager for tenant keys
	keyManager *KeyManager

	// GCI for deduplication (optional, can be nil)
	gci GCIInterface

	// Metrics
	stats ProcessingStats
	mu    sync.RWMutex
}

// GCIInterface defines the interface for Global Content Index
// This allows the backend to check for existing chunks
type GCIInterface interface {
	// CheckChunk returns true if chunk already exists
	CheckChunk(ctx context.Context, plaintextHash []byte) (exists bool, location string, err error)
	// RegisterChunk registers a new chunk in the index
	RegisterChunk(ctx context.Context, plaintextHash []byte, location string, size int64) error
	// IncrementRef increments reference count for existing chunk
	IncrementRef(ctx context.Context, plaintextHash []byte) error
}

// ProcessingStats tracks processing statistics
type ProcessingStats struct {
	BytesProcessed     int64
	BytesStored        int64
	ChunksProcessed    int64
	ChunksDeduplicated int64
	CompressionRatio   float64
}

// ProcessingBackendConfig configures the processing backend
type ProcessingBackendConfig struct {
	Pipeline   *Pipeline
	KeyManager *KeyManager
	GCI        GCIInterface // Optional
}

// NewProcessingBackend creates a new processing backend wrapper
func NewProcessingBackend(config *ProcessingBackendConfig) (*ProcessingBackend, error) {
	if config.Pipeline == nil {
		return nil, fmt.Errorf("pipeline required")
	}
	if config.KeyManager == nil {
		return nil, fmt.Errorf("key manager required")
	}

	return &ProcessingBackend{
		pipeline:   config.Pipeline,
		keyManager: config.KeyManager,
		gci:        config.GCI,
	}, nil
}

// ProcessedObject represents a processed object ready for storage
type ProcessedObject struct {
	// Chunks to store (only new ones if dedup enabled)
	Chunks []ChunkToStore

	// Metadata about the object
	Metadata ObjectMetadata

	// Stats from processing
	Stats *ProcessResult
}

// ChunkToStore represents a chunk ready for backend storage
type ChunkToStore struct {
	// Unique identifier for this chunk
	ID string

	// Encrypted chunk data
	Data []byte

	// Hash of plaintext (for dedup)
	PlaintextHash string

	// Hash of ciphertext (for integrity)
	CiphertextHash string

	// Whether this is a new chunk or reference to existing
	IsNew bool

	// Location if referencing existing chunk
	ExistingLocation string
}

// ObjectMetadata contains metadata needed to reconstruct the object
type ObjectMetadata struct {
	// Original object key
	Key string

	// Tenant ID
	TenantID string

	// List of chunk references in order
	ChunkRefs []ChunkRef

	// Original size before processing
	OriginalSize int64

	// Stored size after processing
	StoredSize int64

	// Encryption key version used
	KeyVersion int
}

// ChunkRef references a chunk in storage
type ChunkRef struct {
	Sequence       int    `json:"seq"`
	PlaintextHash  string `json:"pt_hash"`
	CiphertextHash string `json:"ct_hash"`
	Location       string `json:"location"`
	Size           int64  `json:"size"`
	Nonce          string `json:"nonce,omitempty"` // Base64-encoded nonce
	Compressed     bool   `json:"compressed"`
	Encrypted      bool   `json:"encrypted"`
}

// ProcessForUpload processes data for upload to backend
func (pb *ProcessingBackend) ProcessForUpload(ctx context.Context, tenantID, objectKey string, data io.Reader) (*ProcessedObject, error) {
	// Get tenant encryption context
	tenantKey, keyVersion, err := pb.keyManager.GetTenantKey(tenantID)
	if err != nil {
		return nil, fmt.Errorf("failed to get tenant key: %w", err)
	}

	uploadCtx := &UploadContext{
		TenantKey:  tenantKey,
		KeyVersion: keyVersion,
	}

	// Process through pipeline
	processResult, err := pb.pipeline.Process(ctx, data, uploadCtx)
	if err != nil {
		return nil, fmt.Errorf("pipeline processing failed: %w", err)
	}

	// Build result
	result := &ProcessedObject{
		Chunks: make([]ChunkToStore, 0, len(processResult.Chunks)),
		Metadata: ObjectMetadata{
			Key:          objectKey,
			TenantID:     tenantID,
			ChunkRefs:    make([]ChunkRef, 0, len(processResult.Chunks)),
			OriginalSize: processResult.TotalSize,
			KeyVersion:   keyVersion,
		},
		Stats: processResult,
	}

	var storedSize int64

	for i, chunk := range processResult.Chunks {
		chunkToStore := ChunkToStore{
			ID:             fmt.Sprintf("%s/%s/chunk-%d", tenantID, objectKey, i),
			Data:           chunk.Data,
			PlaintextHash:  chunk.PlaintextHash,
			CiphertextHash: chunk.CiphertextHash,
			IsNew:          true,
		}

		// Check GCI for deduplication if available
		if pb.gci != nil {
			exists, location, err := pb.gci.CheckChunk(ctx, []byte(chunk.PlaintextHash))
			if err == nil && exists {
				// Chunk already exists, just reference it
				chunkToStore.IsNew = false
				chunkToStore.ExistingLocation = location
				chunkToStore.Data = nil // Don't need to store again

				// Increment reference count
				_ = pb.gci.IncrementRef(ctx, []byte(chunk.PlaintextHash))

				pb.mu.Lock()
				pb.stats.ChunksDeduplicated++
				pb.mu.Unlock()
			}
		}

		if chunkToStore.IsNew {
			storedSize += chunk.ProcessedSize
		}

		result.Chunks = append(result.Chunks, chunkToStore)

		// Build chunk reference for metadata (includes nonce for decryption)
		location := chunkToStore.ExistingLocation
		if chunkToStore.IsNew {
			location = chunkToStore.ID
		}

		// Encode nonce as base64 for storage
		var nonceStr string
		if len(chunk.Nonce) > 0 {
			nonceStr = base64.StdEncoding.EncodeToString(chunk.Nonce)
		}

		result.Metadata.ChunkRefs = append(result.Metadata.ChunkRefs, ChunkRef{
			Sequence:       i,
			PlaintextHash:  chunk.PlaintextHash,
			CiphertextHash: chunk.CiphertextHash,
			Location:       location,
			Size:           chunk.ProcessedSize,
			Nonce:          nonceStr,
			Compressed:     chunk.Compressed,
			Encrypted:      chunk.Encrypted,
		})
	}

	result.Metadata.StoredSize = storedSize

	// Update stats
	pb.mu.Lock()
	pb.stats.BytesProcessed += processResult.TotalSize
	pb.stats.BytesStored += storedSize
	pb.stats.ChunksProcessed += int64(len(processResult.Chunks))
	if pb.stats.BytesProcessed > 0 && pb.stats.BytesStored > 0 {
		pb.stats.CompressionRatio = float64(pb.stats.BytesProcessed) / float64(pb.stats.BytesStored)
	}
	pb.mu.Unlock()

	return result, nil
}

// ProcessForDownload reconstructs data from stored chunks
func (pb *ProcessingBackend) ProcessForDownload(ctx context.Context, tenantID string, metadata *ObjectMetadata, chunkFetcher ChunkFetcher) (io.Reader, error) {
	// Get tenant key for the version used during encryption
	tenantKey, err := pb.keyManager.DeriveTenantKey(tenantID, metadata.KeyVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to derive tenant key: %w", err)
	}

	reconstructCtx := &ReconstructContext{
		TenantKey:  tenantKey,
		KeyVersion: metadata.KeyVersion,
	}

	// Fetch all chunks
	chunks := make([]*ProcessedChunk, 0, len(metadata.ChunkRefs))
	for i, chunkRef := range metadata.ChunkRefs {
		// Fetch encrypted chunk
		encryptedData, err := chunkFetcher.FetchChunk(ctx, chunkRef.Location)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch chunk %d: %w", chunkRef.Sequence, err)
		}

		// Decode nonce from base64
		var nonce []byte
		if chunkRef.Nonce != "" {
			nonce, err = base64.StdEncoding.DecodeString(chunkRef.Nonce)
			if err != nil {
				return nil, fmt.Errorf("failed to decode nonce for chunk %d: %w", i, err)
			}
		}

		chunks = append(chunks, &ProcessedChunk{
			Index:          i,
			Data:           encryptedData,
			PlaintextHash:  chunkRef.PlaintextHash,
			CiphertextHash: chunkRef.CiphertextHash,
			Nonce:          nonce,
			Compressed:     chunkRef.Compressed,
			Encrypted:      chunkRef.Encrypted,
			ProcessedSize:  chunkRef.Size,
		})
	}

	// Reconstruct using pipeline
	var buf bytes.Buffer
	err = pb.pipeline.Reconstruct(ctx, chunks, reconstructCtx, &buf)
	if err != nil {
		return nil, fmt.Errorf("failed to reconstruct: %w", err)
	}

	return bytes.NewReader(buf.Bytes()), nil
}

// ChunkFetcher interface for fetching chunks from storage
type ChunkFetcher interface {
	FetchChunk(ctx context.Context, location string) ([]byte, error)
}

// GetStats returns current processing statistics
func (pb *ProcessingBackend) GetStats() ProcessingStats {
	pb.mu.RLock()
	defer pb.mu.RUnlock()
	return pb.stats
}

// ResetStats resets the processing statistics
func (pb *ProcessingBackend) ResetStats() {
	pb.mu.Lock()
	pb.stats = ProcessingStats{}
	pb.mu.Unlock()
}

// CalculateSavings returns storage savings information
func (pb *ProcessingBackend) CalculateSavings() SavingsInfo {
	pb.mu.RLock()
	defer pb.mu.RUnlock()

	savings := SavingsInfo{
		OriginalBytes: pb.stats.BytesProcessed,
		StoredBytes:   pb.stats.BytesStored,
		TotalChunks:   pb.stats.ChunksProcessed,
		DedupedChunks: pb.stats.ChunksDeduplicated,
	}

	if savings.OriginalBytes > 0 {
		savings.SavedBytes = savings.OriginalBytes - savings.StoredBytes
		savings.SavingsPercent = float64(savings.SavedBytes) / float64(savings.OriginalBytes) * 100
	}

	if savings.TotalChunks > 0 {
		savings.DedupPercent = float64(savings.DedupedChunks) / float64(savings.TotalChunks) * 100
	}

	return savings
}

// SavingsInfo contains storage savings information
type SavingsInfo struct {
	OriginalBytes  int64   `json:"original_bytes"`
	StoredBytes    int64   `json:"stored_bytes"`
	SavedBytes     int64   `json:"saved_bytes"`
	SavingsPercent float64 `json:"savings_percent"`
	TotalChunks    int64   `json:"total_chunks"`
	DedupedChunks  int64   `json:"deduped_chunks"`
	DedupPercent   float64 `json:"dedup_percent"`
}
