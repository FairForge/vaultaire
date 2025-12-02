package crypto

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
)

// Pipeline orchestrates the chunk -> compress -> encrypt flow
type Pipeline struct {
	config     PipelineConfig
	chunker    Chunker
	compressor Compressor
	encryptor  Encryptor
}

// NewPipeline creates a new processing pipeline from config
func NewPipeline(config PipelineConfig) (*Pipeline, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid pipeline config: %w", err)
	}

	var chunker Chunker
	var err error
	if config.ChunkingEnabled {
		chunker, err = NewChunkerFromConfig(config)
		if err != nil {
			return nil, fmt.Errorf("failed to create chunker: %w", err)
		}
	}

	compressor, err := NewCompressorFromConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create compressor: %w", err)
	}

	encryptor, err := NewEncryptorFromConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create encryptor: %w", err)
	}

	return &Pipeline{
		config:     config,
		chunker:    chunker,
		compressor: compressor,
		encryptor:  encryptor,
	}, nil
}

// NewPipelineFromPreset creates a pipeline from a preset name
func NewPipelineFromPreset(presetName string) (*Pipeline, error) {
	config, err := GetPreset(presetName)
	if err != nil {
		return nil, err
	}
	return NewPipeline(config)
}

// ProcessedChunk represents a fully processed chunk ready for storage
type ProcessedChunk struct {
	// Identification
	Index         int    // Position in the original data
	PlaintextHash string // SHA-256 of original chunk data (for dedup)

	// Processed data
	Data []byte // Final data (possibly compressed + encrypted)

	// Metadata
	OriginalSize   int64  // Size before processing
	ProcessedSize  int64  // Size after processing
	Compressed     bool   // Whether compression was applied
	Encrypted      bool   // Whether encryption was applied
	Nonce          []byte // Encryption nonce (if encrypted)
	CiphertextHash string // Hash of final data (for integrity)

	// For reconstruction
	Offset int64 // Byte offset in original data
}

// ProcessResult contains the results of processing a data stream
type ProcessResult struct {
	Chunks        []*ProcessedChunk
	TotalSize     int64   // Original data size
	ProcessedSize int64   // Total size after processing
	ChunkCount    int     // Number of chunks
	DedupRatio    float64 // Compression + dedup ratio
	ContentHash   string  // Hash of entire original content
}

// UploadContext provides context for processing uploads
type UploadContext struct {
	TenantKey   []byte // Tenant's master encryption key
	KeyVersion  int    // Key version for rotation tracking
	ContentType string // MIME type (for compression decisions)
}

// Process processes data through the pipeline
func (p *Pipeline) Process(ctx context.Context, r io.Reader, uc *UploadContext) (*ProcessResult, error) {
	result := &ProcessResult{
		Chunks: make([]*ProcessedChunk, 0),
	}

	// Read all data first (we need it for hashing and chunking)
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read data: %w", err)
	}

	// Hash the entire content
	contentHash := sha256.Sum256(data)
	result.ContentHash = fmt.Sprintf("%x", contentHash)
	result.TotalSize = int64(len(data))

	// If chunking is disabled, process as single chunk
	if !p.config.ChunkingEnabled || p.chunker == nil {
		chunk := &Chunk{
			Data:   data,
			Size:   len(data),
			Offset: 0,
			Index:  0,
			Hash:   fmt.Sprintf("%x", sha256.Sum256(data)),
		}

		processed, err := p.processChunk(chunk, uc)
		if err != nil {
			return nil, fmt.Errorf("failed to process chunk: %w", err)
		}

		result.Chunks = append(result.Chunks, processed)
		result.ProcessedSize = processed.ProcessedSize
		result.ChunkCount = 1
		result.DedupRatio = float64(result.TotalSize) / float64(result.ProcessedSize)
		return result, nil
	}

	// Process with chunking using ChunkBytes
	chunks, err := p.chunker.ChunkBytes(data)
	if err != nil {
		return nil, fmt.Errorf("chunking failed: %w", err)
	}

	for i := range chunks {
		chunk := &chunks[i]

		processed, err := p.processChunk(chunk, uc)
		if err != nil {
			return nil, fmt.Errorf("failed to process chunk %d: %w", i, err)
		}

		result.Chunks = append(result.Chunks, processed)
		result.ProcessedSize += processed.ProcessedSize
	}

	result.ChunkCount = len(result.Chunks)

	if result.ProcessedSize > 0 {
		result.DedupRatio = float64(result.TotalSize) / float64(result.ProcessedSize)
	} else {
		result.DedupRatio = 1.0
	}

	return result, nil
}

// processChunk applies compression and encryption to a single chunk
func (p *Pipeline) processChunk(chunk *Chunk, uc *UploadContext) (*ProcessedChunk, error) {
	processed := &ProcessedChunk{
		Index:         chunk.Index,
		PlaintextHash: chunk.Hash,
		Offset:        chunk.Offset,
		OriginalSize:  int64(chunk.Size),
	}

	data := chunk.Data

	// Step 1: Compression (if enabled and beneficial)
	if p.config.CompressionEnabled && ShouldCompress(data, uc.ContentType) {
		compressed, err := p.compressor.Compress(data)
		if err != nil {
			return nil, fmt.Errorf("compression failed: %w", err)
		}

		// Only use compressed if it's actually smaller
		if len(compressed) < chunk.Size {
			data = compressed
			processed.Compressed = true
		}
	}

	// Step 2: Encryption (if enabled)
	if p.config.EncryptionEnabled && uc.TenantKey != nil {
		var key []byte

		// Derive key based on mode
		if p.config.EncryptionMode == EncryptionModeConvergent {
			// Convergent: key derived from content (enables dedup)
			contentHash := []byte(chunk.Hash)
			key = DeriveConvergentKey(uc.TenantKey, contentHash)
		} else {
			// Random: use tenant key directly (or derive per-chunk)
			key = uc.TenantKey
		}

		ciphertext, nonce, err := p.encryptor.Encrypt(key, data)
		if err != nil {
			return nil, fmt.Errorf("encryption failed: %w", err)
		}

		data = ciphertext
		processed.Nonce = nonce
		processed.Encrypted = true
	}

	processed.Data = data
	processed.ProcessedSize = int64(len(data))
	processed.CiphertextHash = fmt.Sprintf("%x", sha256.Sum256(data))

	return processed, nil
}

// ReconstructContext provides context for reconstruction
type ReconstructContext struct {
	TenantKey  []byte
	KeyVersion int
}

// Reconstruct reconstructs original data from processed chunks
func (p *Pipeline) Reconstruct(ctx context.Context, chunks []*ProcessedChunk, rc *ReconstructContext, w io.Writer) error {
	for _, chunk := range chunks {
		data := chunk.Data

		// Step 1: Decrypt (if encrypted)
		if chunk.Encrypted && rc.TenantKey != nil {
			var key []byte

			if p.config.EncryptionMode == EncryptionModeConvergent {
				contentHash := []byte(chunk.PlaintextHash)
				key = DeriveConvergentKey(rc.TenantKey, contentHash)
			} else {
				key = rc.TenantKey
			}

			decrypted, err := p.encryptor.Decrypt(key, chunk.Nonce, data)
			if err != nil {
				return fmt.Errorf("decryption failed for chunk %d: %w", chunk.Index, err)
			}
			data = decrypted
		}

		// Step 2: Decompress (if compressed)
		if chunk.Compressed {
			decompressed, err := p.compressor.Decompress(data)
			if err != nil {
				return fmt.Errorf("decompression failed for chunk %d: %w", chunk.Index, err)
			}
			data = decompressed
		}

		// Write to output
		if _, err := w.Write(data); err != nil {
			return fmt.Errorf("write failed for chunk %d: %w", chunk.Index, err)
		}
	}

	return nil
}

// ProcessStream processes data from a reader using streaming chunking
func (p *Pipeline) ProcessStream(ctx context.Context, r io.Reader, uc *UploadContext) (*ProcessResult, error) {
	result := &ProcessResult{
		Chunks: make([]*ProcessedChunk, 0),
	}

	// If chunking is disabled, fall back to buffered processing
	if !p.config.ChunkingEnabled || p.chunker == nil {
		return p.Process(ctx, r, uc)
	}

	// Use streaming chunker
	contentHasher := sha256.New()
	teeReader := io.TeeReader(r, contentHasher)

	chunkChan, err := p.chunker.Chunk(teeReader)
	if err != nil {
		return nil, fmt.Errorf("failed to start chunking: %w", err)
	}

	for chunkResult := range chunkChan {
		if chunkResult.Err != nil {
			return nil, fmt.Errorf("chunking error: %w", chunkResult.Err)
		}

		chunk := &chunkResult.Chunk
		result.TotalSize += int64(chunk.Size)

		processed, err := p.processChunk(chunk, uc)
		if err != nil {
			return nil, fmt.Errorf("failed to process chunk %d: %w", chunk.Index, err)
		}

		result.Chunks = append(result.Chunks, processed)
		result.ProcessedSize += processed.ProcessedSize
	}

	result.ChunkCount = len(result.Chunks)
	result.ContentHash = fmt.Sprintf("%x", contentHasher.Sum(nil))

	if result.ProcessedSize > 0 {
		result.DedupRatio = float64(result.TotalSize) / float64(result.ProcessedSize)
	} else {
		result.DedupRatio = 1.0
	}

	return result, nil
}

// Config returns the pipeline configuration
func (p *Pipeline) Config() PipelineConfig {
	return p.config
}

// Stats tracks pipeline processing statistics
type PipelineStats struct {
	BytesIn          int64
	BytesOut         int64
	ChunksProcessed  int64
	ChunksCompressed int64
	ChunksEncrypted  int64
}

func (s *PipelineStats) CompressionRatio() float64 {
	if s.BytesOut == 0 {
		return 1.0
	}
	return float64(s.BytesIn) / float64(s.BytesOut)
}

func (s *PipelineStats) Add(result *ProcessResult) {
	s.BytesIn += result.TotalSize
	s.BytesOut += result.ProcessedSize
	s.ChunksProcessed += int64(result.ChunkCount)

	for _, c := range result.Chunks {
		if c.Compressed {
			s.ChunksCompressed++
		}
		if c.Encrypted {
			s.ChunksEncrypted++
		}
	}
}

// StreamingPipeline provides a streaming interface for large files
type StreamingPipeline struct {
	*Pipeline
	// reserved for future streaming optimizations
}

// NewStreamingPipeline creates a pipeline optimized for streaming
func NewStreamingPipeline(config PipelineConfig) (*StreamingPipeline, error) {
	p, err := NewPipeline(config)
	if err != nil {
		return nil, err
	}
	return &StreamingPipeline{Pipeline: p}, nil
}
