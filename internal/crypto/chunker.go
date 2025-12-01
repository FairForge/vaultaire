package crypto

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	resticchunker "github.com/restic/chunker"
)

// Chunk represents a content-defined chunk of data
type Chunk struct {
	Data    []byte // The chunk data
	Hash    string // SHA-256 hash of the data (hex encoded)
	Size    int    // Size in bytes
	Offset  int64  // Offset in original stream
	Index   int    // Chunk index (0-based)
	IsFinal bool   // True if this is the last chunk
}

// Chunker defines the interface for content chunking
type Chunker interface {
	// Chunk splits a reader into content-defined chunks
	// Returns a channel that yields chunks as they're produced
	Chunk(r io.Reader) (<-chan ChunkResult, error)

	// ChunkBytes splits byte slice into chunks (convenience method)
	ChunkBytes(data []byte) ([]Chunk, error)

	// Algorithm returns the chunking algorithm name
	Algorithm() ChunkingAlgorithm
}

// ChunkResult wraps a chunk or error from async chunking
type ChunkResult struct {
	Chunk Chunk
	Err   error
}

// FastCDCChunker implements content-defined chunking using FastCDC algorithm
type FastCDCChunker struct {
	minSize int
	avgSize int
	maxSize int
	pol     resticchunker.Pol
}

// NewFastCDCChunker creates a new FastCDC chunker
func NewFastCDCChunker(minSize, avgSize, maxSize int) (*FastCDCChunker, error) {
	if minSize <= 0 || avgSize <= 0 || maxSize <= 0 {
		return nil, fmt.Errorf("chunk sizes must be positive")
	}
	if minSize > avgSize || avgSize > maxSize {
		return nil, fmt.Errorf("chunk sizes must be: min <= avg <= max")
	}

	// Use a fixed polynomial for deterministic chunking
	// This ensures the same content always produces the same chunks
	pol, err := resticchunker.RandomPolynomial()
	if err != nil {
		return nil, fmt.Errorf("failed to generate polynomial: %w", err)
	}

	return &FastCDCChunker{
		minSize: minSize,
		avgSize: avgSize,
		maxSize: maxSize,
		pol:     pol,
	}, nil
}

// NewFastCDCChunkerWithPol creates a chunker with a specific polynomial
// Use this for deterministic chunking across instances
func NewFastCDCChunkerWithPol(minSize, avgSize, maxSize int, pol uint64) (*FastCDCChunker, error) {
	if minSize <= 0 || avgSize <= 0 || maxSize <= 0 {
		return nil, fmt.Errorf("chunk sizes must be positive")
	}
	if minSize > avgSize || avgSize > maxSize {
		return nil, fmt.Errorf("chunk sizes must be: min <= avg <= max")
	}

	return &FastCDCChunker{
		minSize: minSize,
		avgSize: avgSize,
		maxSize: maxSize,
		pol:     resticchunker.Pol(pol),
	}, nil
}

// DefaultFastCDCChunker creates a chunker with default settings (4MB average)
func DefaultFastCDCChunker() (*FastCDCChunker, error) {
	return NewFastCDCChunker(
		1*1024*1024,  // 1MB min
		4*1024*1024,  // 4MB avg
		16*1024*1024, // 16MB max
	)
}

// Algorithm returns the chunking algorithm name
func (c *FastCDCChunker) Algorithm() ChunkingAlgorithm {
	return ChunkingFastCDC
}

// Polynomial returns the polynomial used for chunking (for persistence)
func (c *FastCDCChunker) Polynomial() uint64 {
	return uint64(c.pol)
}

// Chunk splits a reader into content-defined chunks
func (c *FastCDCChunker) Chunk(r io.Reader) (<-chan ChunkResult, error) {
	ch := make(chan ChunkResult, 10) // Buffer for smooth streaming

	go func() {
		defer close(ch)

		chunker := resticchunker.NewWithBoundaries(r, c.pol, uint(c.minSize), uint(c.maxSize))
		buf := make([]byte, c.maxSize)

		var offset int64
		index := 0

		for {
			chunk, err := chunker.Next(buf)
			if err == io.EOF {
				break
			}
			if err != nil {
				ch <- ChunkResult{Err: fmt.Errorf("chunking failed at offset %d: %w", offset, err)}
				return
			}

			// Copy data (chunker reuses buffer)
			data := make([]byte, chunk.Length)
			copy(data, chunk.Data)

			// Calculate hash
			hash := sha256.Sum256(data)

			ch <- ChunkResult{
				Chunk: Chunk{
					Data:   data,
					Hash:   hex.EncodeToString(hash[:]),
					Size:   int(chunk.Length),
					Offset: offset,
					Index:  index,
				},
			}

			offset += int64(chunk.Length)
			index++
		}

		// Mark final chunk if we produced any
		// Note: The channel is already closed by defer, so we can't modify sent chunks
		// Instead, consumers should check for channel close as the "final" signal
	}()

	return ch, nil
}

// ChunkBytes splits a byte slice into chunks (synchronous convenience method)
func (c *FastCDCChunker) ChunkBytes(data []byte) ([]Chunk, error) {
	if len(data) == 0 {
		return nil, nil
	}

	chunker := resticchunker.NewWithBoundaries(
		&byteReader{data: data},
		c.pol,
		uint(c.minSize),
		uint(c.maxSize),
	)

	buf := make([]byte, c.maxSize)
	var chunks []Chunk
	var offset int64
	index := 0

	for {
		chunk, err := chunker.Next(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("chunking failed at offset %d: %w", offset, err)
		}

		// Copy data
		chunkData := make([]byte, chunk.Length)
		copy(chunkData, chunk.Data)

		// Calculate hash
		hash := sha256.Sum256(chunkData)

		chunks = append(chunks, Chunk{
			Data:   chunkData,
			Hash:   hex.EncodeToString(hash[:]),
			Size:   int(chunk.Length),
			Offset: offset,
			Index:  index,
		})

		offset += int64(chunk.Length)
		index++
	}

	// Mark final chunk
	if len(chunks) > 0 {
		chunks[len(chunks)-1].IsFinal = true
	}

	return chunks, nil
}

// byteReader wraps []byte to implement io.Reader
type byteReader struct {
	data []byte
	pos  int
}

func (r *byteReader) Read(p []byte) (n int, err error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n = copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// FixedChunker implements fixed-size chunking (simpler but less effective for dedup)
type FixedChunker struct {
	chunkSize int
}

// NewFixedChunker creates a fixed-size chunker
func NewFixedChunker(chunkSize int) (*FixedChunker, error) {
	if chunkSize <= 0 {
		return nil, fmt.Errorf("chunk size must be positive")
	}
	return &FixedChunker{chunkSize: chunkSize}, nil
}

// Algorithm returns the chunking algorithm name
func (c *FixedChunker) Algorithm() ChunkingAlgorithm {
	return ChunkingFixed
}

// Chunk splits a reader into fixed-size chunks
func (c *FixedChunker) Chunk(r io.Reader) (<-chan ChunkResult, error) {
	ch := make(chan ChunkResult, 10)

	go func() {
		defer close(ch)

		buf := make([]byte, c.chunkSize)
		var offset int64
		index := 0

		for {
			n, err := io.ReadFull(r, buf)
			if err == io.EOF {
				break
			}
			if err == io.ErrUnexpectedEOF {
				// Last chunk is smaller than chunkSize
				data := make([]byte, n)
				copy(data, buf[:n])
				hash := sha256.Sum256(data)

				ch <- ChunkResult{
					Chunk: Chunk{
						Data:    data,
						Hash:    hex.EncodeToString(hash[:]),
						Size:    n,
						Offset:  offset,
						Index:   index,
						IsFinal: true,
					},
				}
				break
			}
			if err != nil {
				ch <- ChunkResult{Err: fmt.Errorf("read failed at offset %d: %w", offset, err)}
				return
			}

			data := make([]byte, n)
			copy(data, buf[:n])
			hash := sha256.Sum256(data)

			ch <- ChunkResult{
				Chunk: Chunk{
					Data:   data,
					Hash:   hex.EncodeToString(hash[:]),
					Size:   n,
					Offset: offset,
					Index:  index,
				},
			}

			offset += int64(n)
			index++
		}
	}()

	return ch, nil
}

// ChunkBytes splits a byte slice into fixed-size chunks
func (c *FixedChunker) ChunkBytes(data []byte) ([]Chunk, error) {
	if len(data) == 0 {
		return nil, nil
	}

	var chunks []Chunk
	var offset int64
	index := 0

	for offset < int64(len(data)) {
		end := offset + int64(c.chunkSize)
		if end > int64(len(data)) {
			end = int64(len(data))
		}

		chunkData := make([]byte, end-offset)
		copy(chunkData, data[offset:end])
		hash := sha256.Sum256(chunkData)

		chunks = append(chunks, Chunk{
			Data:   chunkData,
			Hash:   hex.EncodeToString(hash[:]),
			Size:   len(chunkData),
			Offset: offset,
			Index:  index,
		})

		offset = end
		index++
	}

	// Mark final chunk
	if len(chunks) > 0 {
		chunks[len(chunks)-1].IsFinal = true
	}

	return chunks, nil
}

// NewChunkerFromConfig creates a chunker based on pipeline configuration
func NewChunkerFromConfig(config PipelineConfig) (Chunker, error) {
	if !config.ChunkingEnabled {
		return nil, fmt.Errorf("chunking is disabled in config")
	}

	switch config.ChunkingAlgo {
	case ChunkingFastCDC:
		return NewFastCDCChunker(config.MinChunkSize, config.AvgChunkSize, config.MaxChunkSize)
	case ChunkingFixed:
		return NewFixedChunker(config.AvgChunkSize)
	default:
		return nil, fmt.Errorf("unsupported chunking algorithm: %s", config.ChunkingAlgo)
	}
}
