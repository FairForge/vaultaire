package crypto

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// RabinChunker is the content-defined chunker: restic's Rabin fingerprint
// chunker (github.com/restic/chunker), NOT FastCDC — the type was named for
// the algorithm that was planned, and the docs repeated it (R8-11).
//
// Its identity is ChunkerIdentity below. Every boundary a chunk has ever been
// cut at follows from it; see DefaultChunkerIdentity for what changing any
// part of it would do.
type RabinChunker struct {
	minSize     int
	maxSize     int
	averageBits int
	pol         resticchunker.Pol
}

// DefaultChunkerPolynomial is THE chunking polynomial, fixed forever.
//
// PERMANENT — NEVER CHANGE THIS VALUE. Chunk boundaries (and therefore every
// chunk hash in global_content_index and every manifest in tenant_chunk_refs)
// are defined by this polynomial. Changing it means newly uploaded content
// chunks at different boundaries: nothing would ever dedup against existing
// chunks again, and any code path that re-chunks to locate data would fail to
// find it — every stored chunk is effectively orphaned.
//
// Irreducible, degree 53 (generated once via restic/chunker.RandomPolynomial
// for WP-7; pinned by TestRabinChunker_PermanentPolynomial).
const DefaultChunkerPolynomial = 0x2ADD89E3B790BB

// The chunker's identity (WP-R8-4): one constant set, recorded with every
// chunked object (PipelineConfig.Chunker in object_metadata.pipeline_config)
// so that a future change can tell old objects from new. Each part is
// load-bearing in the same way as the polynomial — a change resets dedup for
// everything stored, because new uploads would cut at other boundaries:
//
//   - ChunkerLibrary / ChunkerLibraryVersion: the implementation and its
//     pinned module version (go.mod; TestChunkerIdentity_MatchesGoMod). The
//     library's 64-byte rolling window and its boundary rule are what the
//     version names; TestRabinChunker_GoldenBoundaries cuts a fixed input at
//     fixed offsets, so a bump that moves a boundary fails the build.
//   - ChunkerAverageBits: the split mask, 20 bits. restic's default; it was
//     never passed explicitly before, and the "4 MiB average" the old
//     constructor took was accepted and ignored. A boundary is found at the
//     first position after the minimum where the fingerprint's low 20 bits
//     are zero, so a chunk is the 1 MiB minimum plus about 1 MiB: the real
//     average is about 2 MiB (36 chunks per 70 MiB measured in R8; 33 per
//     70 MB in the WP-R8-7 live proof; 127 per 256 MB on prod). Decision
//     (status quo, to be confirmed by Isaac): keep it — SetAverageBits(22)
//     would make new objects 4 MiB chunks and stop every existing object
//     from deduplicating against new uploads.
//   - ChunkMinSize / ChunkMaxSize: 1 MiB / 16 MiB.
const (
	ChunkerLibrary        = "github.com/restic/chunker"
	ChunkerLibraryVersion = "v0.4.0"
	ChunkerAverageBits    = 20
	ChunkMinSize          = 1 * 1024 * 1024
	ChunkMaxSize          = 16 * 1024 * 1024
	// ChunkExpectedAverage is the documented expected chunk length (what
	// capacity maths and CHUNK_PUT_CONCURRENCY sizing should assume).
	ChunkExpectedAverage = ChunkMinSize + 1<<ChunkerAverageBits
)

// ChunkerIdentity is what cut a chunked object's chunks. Recorded as JSON in
// the object's pipeline_config.
type ChunkerIdentity struct {
	Library     string `json:"library"`
	Version     string `json:"version"`
	Polynomial  string `json:"polynomial"` // hex, 0x-prefixed
	MinSize     int    `json:"min_size"`
	AverageBits int    `json:"average_bits"`
	MaxSize     int    `json:"max_size"`
}

// DefaultChunkerIdentity is the identity of DefaultChunker — the only chunker
// the product has ever cut with.
func DefaultChunkerIdentity() ChunkerIdentity {
	return ChunkerIdentity{
		Library: ChunkerLibrary, Version: ChunkerLibraryVersion,
		Polynomial:  fmt.Sprintf("0x%X", uint64(DefaultChunkerPolynomial)),
		MinSize:     ChunkMinSize,
		AverageBits: ChunkerAverageBits,
		MaxSize:     ChunkMaxSize,
	}
}

// String is the identity on one line (logs, notes).
func (id ChunkerIdentity) String() string {
	return fmt.Sprintf("%s@%s pol=%s min=%d avg_bits=%d max=%d", id.Library, id.Version, id.Polynomial, id.MinSize, id.AverageBits, id.MaxSize)
}

// Identity is the identity of this chunker.
func (c *RabinChunker) Identity() ChunkerIdentity {
	return ChunkerIdentity{
		Library: ChunkerLibrary, Version: ChunkerLibraryVersion,
		Polynomial:  fmt.Sprintf("0x%X", uint64(c.pol)),
		MinSize:     c.minSize,
		AverageBits: c.averageBits,
		MaxSize:     c.maxSize,
	}
}

// NewRabinChunker creates a chunker with the permanent
// DefaultChunkerPolynomial and ChunkerAverageBits, so chunk boundaries are
// deterministic across instances, restarts and deploys — the precondition
// for dedup to ever hit.
func NewRabinChunker(minSize, maxSize int) (*RabinChunker, error) {
	return NewRabinChunkerWithPol(minSize, maxSize, DefaultChunkerPolynomial)
}

// NewRabinChunkerWithPol creates a chunker with a specific polynomial. Only
// tests use a polynomial other than the permanent one.
func NewRabinChunkerWithPol(minSize, maxSize int, pol uint64) (*RabinChunker, error) {
	if minSize <= 0 || maxSize <= 0 {
		return nil, fmt.Errorf("chunk sizes must be positive")
	}
	if minSize > maxSize {
		return nil, fmt.Errorf("chunk sizes must be: min <= max")
	}

	return &RabinChunker{
		minSize:     minSize,
		maxSize:     maxSize,
		averageBits: ChunkerAverageBits,
		pol:         resticchunker.Pol(pol),
	}, nil
}

// DefaultChunker is THE chunker of the product: DefaultChunkerIdentity.
func DefaultChunker() (*RabinChunker, error) {
	return NewRabinChunker(ChunkMinSize, ChunkMaxSize)
}

// newLibraryChunker builds the library's chunker with every part of the
// identity passed explicitly — the split mask included, which used to be the
// library's default and nothing of ours.
func (c *RabinChunker) newLibraryChunker(r io.Reader) *resticchunker.Chunker {
	ch := resticchunker.NewWithBoundaries(r, c.pol, uint(c.minSize), uint(c.maxSize))
	ch.SetAverageBits(c.averageBits)
	return ch
}

// Algorithm returns the chunking algorithm name
func (c *RabinChunker) Algorithm() ChunkingAlgorithm {
	return ChunkingRabin
}

// Polynomial returns the polynomial used for chunking (for persistence)
func (c *RabinChunker) Polynomial() uint64 {
	return uint64(c.pol)
}

// Chunk splits a reader into content-defined chunks.
// Delegates to ChunkContext with a background context.
func (c *RabinChunker) Chunk(r io.Reader) (<-chan ChunkResult, error) {
	return c.ChunkContext(context.Background(), r)
}

// ChunkContext splits a reader into content-defined chunks, respecting ctx
// cancellation. If ctx is cancelled the goroutine exits promptly instead of
// blocking on a full channel send.
func (c *RabinChunker) ChunkContext(ctx context.Context, r io.Reader) (<-chan ChunkResult, error) {
	ch := make(chan ChunkResult, 10) // Buffer for smooth streaming

	go func() {
		defer close(ch)

		chunker := c.newLibraryChunker(r)
		buf := make([]byte, c.maxSize)

		var offset int64
		index := 0

		for {
			chunk, err := chunker.Next(buf)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				select {
				case ch <- ChunkResult{Err: fmt.Errorf("chunking failed at offset %d: %w", offset, err)}:
				case <-ctx.Done():
				}
				return
			}

			// Copy data (chunker reuses buffer)
			data := make([]byte, chunk.Length)
			copy(data, chunk.Data)

			// Calculate hash
			hash := sha256.Sum256(data)

			select {
			case ch <- ChunkResult{
				Chunk: Chunk{
					Data:   data,
					Hash:   hex.EncodeToString(hash[:]),
					Size:   int(chunk.Length),
					Offset: offset,
					Index:  index,
				},
			}:
			case <-ctx.Done():
				return
			}

			offset += int64(chunk.Length)
			index++
		}
	}()

	return ch, nil
}

// ChunkBytes splits a byte slice into chunks (synchronous convenience method)
func (c *RabinChunker) ChunkBytes(data []byte) ([]Chunk, error) {
	if len(data) == 0 {
		return nil, nil
	}

	chunker := c.newLibraryChunker(&byteReader{data: data})

	buf := make([]byte, c.maxSize)
	var chunks []Chunk
	var offset int64
	index := 0

	for {
		chunk, err := chunker.Next(buf)
		if errors.Is(err, io.EOF) {
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
			if errors.Is(err, io.EOF) {
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
	case ChunkingRabin:
		return NewRabinChunker(config.ChunkMinSize, config.ChunkMaxSize)
	case ChunkingFixed:
		return NewFixedChunker(config.ChunkAvgSize)
	default:
		return nil, fmt.Errorf("unsupported chunking algorithm: %s", config.ChunkingAlgo)
	}
}
