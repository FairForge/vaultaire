// internal/perf/payload.go
package perf

import (
	"bytes"
	"encoding/json"
	"sync"
)

// PayloadConfig configures payload optimization
type PayloadConfig struct {
	MaxPayloadSize    int
	EnableCompression bool
	MinCompressSize   int
	ChunkSize         int
	EnableStreaming   bool
}

// DefaultPayloadConfig returns default configuration
func DefaultPayloadConfig() *PayloadConfig {
	return &PayloadConfig{
		MaxPayloadSize:    100 * 1024 * 1024, // 100MB
		EnableCompression: true,
		MinCompressSize:   1024,      // 1KB
		ChunkSize:         64 * 1024, // 64KB
		EnableStreaming:   true,
	}
}

// PayloadOptimizer optimizes payload handling
type PayloadOptimizer struct {
	config     *PayloadConfig
	bufferPool sync.Pool
	stats      *PayloadStats
	mu         sync.Mutex
}

// PayloadStats tracks payload statistics
type PayloadStats struct {
	TotalPayloads   int64
	TotalBytes      int64
	CompressedBytes int64
	ChunkedPayloads int64
	LargePayloads   int64
}

// NewPayloadOptimizer creates a new payload optimizer
func NewPayloadOptimizer(config *PayloadConfig) *PayloadOptimizer {
	if config == nil {
		config = DefaultPayloadConfig()
	}

	return &PayloadOptimizer{
		config: config,
		bufferPool: sync.Pool{
			New: func() interface{} {
				return bytes.NewBuffer(make([]byte, 0, 4096))
			},
		},
		stats: &PayloadStats{},
	}
}

// Optimize optimizes a payload
func (o *PayloadOptimizer) Optimize(data []byte) (*OptimizedPayload, error) {
	o.mu.Lock()
	o.stats.TotalPayloads++
	o.stats.TotalBytes += int64(len(data))
	o.mu.Unlock()

	payload := &OptimizedPayload{
		Original:     data,
		OriginalSize: len(data),
	}

	// Check if chunking needed
	if len(data) > o.config.ChunkSize {
		payload.Chunks = o.chunk(data)
		payload.IsChunked = true
		o.mu.Lock()
		o.stats.ChunkedPayloads++
		o.mu.Unlock()
	}

	// Track large payloads
	if len(data) > o.config.MaxPayloadSize/10 {
		o.mu.Lock()
		o.stats.LargePayloads++
		o.mu.Unlock()
	}

	return payload, nil
}

// OptimizedPayload represents an optimized payload
type OptimizedPayload struct {
	Original       []byte
	OriginalSize   int
	CompressedSize int
	IsChunked      bool
	Chunks         [][]byte
}

func (o *PayloadOptimizer) chunk(data []byte) [][]byte {
	var chunks [][]byte
	for i := 0; i < len(data); i += o.config.ChunkSize {
		end := i + o.config.ChunkSize
		if end > len(data) {
			end = len(data)
		}
		chunk := make([]byte, end-i)
		copy(chunk, data[i:end])
		chunks = append(chunks, chunk)
	}
	return chunks
}

// Reassemble reassembles chunked payload
func (o *PayloadOptimizer) Reassemble(chunks [][]byte) []byte {
	var total int
	for _, chunk := range chunks {
		total += len(chunk)
	}

	result := make([]byte, 0, total)
	for _, chunk := range chunks {
		result = append(result, chunk...)
	}
	return result
}

// Stats returns payload statistics
func (o *PayloadOptimizer) Stats() *PayloadStats {
	o.mu.Lock()
	defer o.mu.Unlock()

	return &PayloadStats{
		TotalPayloads:   o.stats.TotalPayloads,
		TotalBytes:      o.stats.TotalBytes,
		CompressedBytes: o.stats.CompressedBytes,
		ChunkedPayloads: o.stats.ChunkedPayloads,
		LargePayloads:   o.stats.LargePayloads,
	}
}

// Config returns current configuration
func (o *PayloadOptimizer) Config() *PayloadConfig {
	return o.config
}

// JSONOptimizer optimizes JSON payloads
type JSONOptimizer struct {
	bufferPool sync.Pool
}

// NewJSONOptimizer creates a new JSON optimizer
func NewJSONOptimizer() *JSONOptimizer {
	return &JSONOptimizer{
		bufferPool: sync.Pool{
			New: func() interface{} {
				return new(bytes.Buffer)
			},
		},
	}
}

// Compact removes whitespace from JSON
func (o *JSONOptimizer) Compact(data []byte) ([]byte, error) {
	buf := o.bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer o.bufferPool.Put(buf)

	if err := json.Compact(buf, data); err != nil {
		return nil, err
	}

	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())
	return result, nil
}

// IsValid checks if JSON is valid
func (o *JSONOptimizer) IsValid(data []byte) bool {
	return json.Valid(data)
}

// StreamChunker chunks data for streaming
type StreamChunker struct {
	chunkSize int
}

// NewStreamChunker creates a new stream chunker
func NewStreamChunker(chunkSize int) *StreamChunker {
	if chunkSize <= 0 {
		chunkSize = 64 * 1024
	}
	return &StreamChunker{chunkSize: chunkSize}
}

// Chunk splits data into chunks
func (c *StreamChunker) Chunk(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}

	numChunks := (len(data) + c.chunkSize - 1) / c.chunkSize
	chunks := make([][]byte, 0, numChunks)

	for i := 0; i < len(data); i += c.chunkSize {
		end := i + c.chunkSize
		if end > len(data) {
			end = len(data)
		}
		chunk := make([]byte, end-i)
		copy(chunk, data[i:end])
		chunks = append(chunks, chunk)
	}

	return chunks
}

// ChunkSize returns the chunk size
func (c *StreamChunker) ChunkSize() int {
	return c.chunkSize
}

// PayloadValidator validates payloads
type PayloadValidator struct {
	maxSize int
}

// NewPayloadValidator creates a new validator
func NewPayloadValidator(maxSize int) *PayloadValidator {
	return &PayloadValidator{maxSize: maxSize}
}

// Validate validates a payload
func (v *PayloadValidator) Validate(data []byte) error {
	if len(data) > v.maxSize {
		return ErrPayloadTooLarge
	}
	if len(data) == 0 {
		return ErrPayloadEmpty
	}
	return nil
}

// MaxSize returns max allowed size
func (v *PayloadValidator) MaxSize() int {
	return v.maxSize
}

// ErrPayloadTooLarge indicates payload exceeds limit
var ErrPayloadTooLarge = &PayloadError{msg: "payload too large"}

// ErrPayloadEmpty indicates empty payload
var ErrPayloadEmpty = &PayloadError{msg: "payload empty"}

// PayloadError represents a payload error
type PayloadError struct {
	msg string
}

func (e *PayloadError) Error() string {
	return e.msg
}
