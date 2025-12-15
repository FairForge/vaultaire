// internal/perf/protocol.go
package perf

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"sync"
)

// CompressionType defines compression algorithms
type CompressionType string

const (
	CompressionNone   CompressionType = "none"
	CompressionGzip   CompressionType = "gzip"
	CompressionSnappy CompressionType = "snappy"
	CompressionLZ4    CompressionType = "lz4"
	CompressionZstd   CompressionType = "zstd"
)

// ErrChecksumMismatch indicates checksum verification failed
var ErrChecksumMismatch = errors.New("checksum mismatch")

// ProtocolConfig configures protocol optimizations
type ProtocolConfig struct {
	Compression      CompressionType
	CompressionLevel int
	EnableChecksum   bool
	MaxMessageSize   int
	EnablePipelining bool
	PipelineDepth    int
}

// DefaultProtocolConfig returns default configuration
func DefaultProtocolConfig() *ProtocolConfig {
	return &ProtocolConfig{
		Compression:      CompressionGzip,
		CompressionLevel: gzip.DefaultCompression,
		EnableChecksum:   true,
		MaxMessageSize:   64 * 1024 * 1024,
		EnablePipelining: true,
		PipelineDepth:    10,
	}
}

// ProtocolOptimizer optimizes protocol encoding/decoding
type ProtocolOptimizer struct {
	config     *ProtocolConfig
	gzipPool   sync.Pool
	bufferPool sync.Pool
	stats      *ProtocolStats
}

// ProtocolStats tracks protocol statistics
type ProtocolStats struct {
	mu               sync.Mutex
	MessagesEncoded  int64
	MessagesDecoded  int64
	BytesBeforeComp  int64
	BytesAfterComp   int64
	CompressionRatio float64
	ChecksumErrors   int64
}

// NewProtocolOptimizer creates a new protocol optimizer
func NewProtocolOptimizer(config *ProtocolConfig) *ProtocolOptimizer {
	if config == nil {
		config = DefaultProtocolConfig()
	}

	// 0 means NoCompression in gzip, so default to DefaultCompression
	compressionLevel := config.CompressionLevel
	if compressionLevel == 0 {
		compressionLevel = gzip.DefaultCompression
	}

	return &ProtocolOptimizer{
		config: config,
		gzipPool: sync.Pool{
			New: func() interface{} {
				w, _ := gzip.NewWriterLevel(nil, compressionLevel)
				return w
			},
		},
		bufferPool: sync.Pool{
			New: func() interface{} {
				return new(bytes.Buffer)
			},
		},
		stats: &ProtocolStats{},
	}
}

// Message represents a protocol message
type Message struct {
	Type     uint16
	Flags    uint16
	Length   uint32
	Checksum uint32
	Payload  []byte
}

// MessageFlag defines message flags
type MessageFlag uint16

const (
	FlagCompressed MessageFlag = 1 << 0
	FlagEncrypted  MessageFlag = 1 << 1
	FlagChecksum   MessageFlag = 1 << 2
	FlagPriority   MessageFlag = 1 << 3
)

// Encode encodes a message with optimizations
func (o *ProtocolOptimizer) Encode(msg *Message) ([]byte, error) {
	payload := msg.Payload
	originalSize := len(payload)

	// Compress if configured and payload exists
	if o.config.Compression != CompressionNone && len(payload) > 0 {
		compressed, err := o.compress(payload)
		if err == nil && len(compressed) < len(payload) {
			payload = compressed
			msg.Flags |= uint16(FlagCompressed)
		}
	}

	// Calculate checksum if enabled
	if o.config.EnableChecksum {
		msg.Checksum = crc32.ChecksumIEEE(payload)
		msg.Flags |= uint16(FlagChecksum)
	}

	msg.Length = uint32(len(payload))

	// Encode header + payload
	buf := o.getBuffer()
	defer o.putBuffer(buf)

	_ = binary.Write(buf, binary.BigEndian, msg.Type)
	_ = binary.Write(buf, binary.BigEndian, msg.Flags)
	_ = binary.Write(buf, binary.BigEndian, msg.Length)
	_ = binary.Write(buf, binary.BigEndian, msg.Checksum)
	_, _ = buf.Write(payload)

	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())

	// Update stats
	o.stats.mu.Lock()
	o.stats.MessagesEncoded++
	o.stats.BytesBeforeComp += int64(originalSize)
	o.stats.BytesAfterComp += int64(len(payload))
	if o.stats.BytesBeforeComp > 0 {
		o.stats.CompressionRatio = float64(o.stats.BytesAfterComp) / float64(o.stats.BytesBeforeComp) * 100
	}
	o.stats.mu.Unlock()

	return result, nil
}

// Decode decodes a message
func (o *ProtocolOptimizer) Decode(data []byte) (*Message, error) {
	if len(data) < 12 {
		return nil, io.ErrShortBuffer
	}

	msg := &Message{}
	reader := bytes.NewReader(data)

	_ = binary.Read(reader, binary.BigEndian, &msg.Type)
	_ = binary.Read(reader, binary.BigEndian, &msg.Flags)
	_ = binary.Read(reader, binary.BigEndian, &msg.Length)
	_ = binary.Read(reader, binary.BigEndian, &msg.Checksum)

	if int(msg.Length) > len(data)-12 {
		return nil, io.ErrShortBuffer
	}

	payload := make([]byte, msg.Length)
	_, _ = reader.Read(payload)

	// Verify checksum if present
	if msg.Flags&uint16(FlagChecksum) != 0 {
		expected := crc32.ChecksumIEEE(payload)
		if expected != msg.Checksum {
			o.stats.mu.Lock()
			o.stats.ChecksumErrors++
			o.stats.mu.Unlock()
			return nil, ErrChecksumMismatch
		}
	}

	// Decompress if compressed
	if msg.Flags&uint16(FlagCompressed) != 0 {
		decompressed, err := o.decompress(payload)
		if err != nil {
			return nil, err
		}
		payload = decompressed
	}

	msg.Payload = payload

	o.stats.mu.Lock()
	o.stats.MessagesDecoded++
	o.stats.mu.Unlock()

	return msg, nil
}

func (o *ProtocolOptimizer) compress(data []byte) ([]byte, error) {
	buf := o.getBuffer()
	defer o.putBuffer(buf)

	w := o.gzipPool.Get().(*gzip.Writer)
	w.Reset(buf)

	_, err := w.Write(data)
	if err != nil {
		o.gzipPool.Put(w)
		return nil, err
	}

	err = w.Close()
	o.gzipPool.Put(w)
	if err != nil {
		return nil, err
	}

	result := make([]byte, buf.Len())
	copy(result, buf.Bytes())
	return result, nil
}

func (o *ProtocolOptimizer) decompress(data []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()

	return io.ReadAll(reader)
}

func (o *ProtocolOptimizer) getBuffer() *bytes.Buffer {
	buf := o.bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

func (o *ProtocolOptimizer) putBuffer(buf *bytes.Buffer) {
	o.bufferPool.Put(buf)
}

// Stats returns protocol statistics
func (o *ProtocolOptimizer) Stats() *ProtocolStats {
	o.stats.mu.Lock()
	defer o.stats.mu.Unlock()

	return &ProtocolStats{
		MessagesEncoded:  o.stats.MessagesEncoded,
		MessagesDecoded:  o.stats.MessagesDecoded,
		BytesBeforeComp:  o.stats.BytesBeforeComp,
		BytesAfterComp:   o.stats.BytesAfterComp,
		CompressionRatio: o.stats.CompressionRatio,
		ChecksumErrors:   o.stats.ChecksumErrors,
	}
}

// Config returns current configuration
func (o *ProtocolOptimizer) Config() *ProtocolConfig {
	return o.config
}

// Pipeline manages request pipelining
type Pipeline struct {
	mu       sync.Mutex
	pending  chan *PipelineRequest
	maxDepth int
	inflight int
}

// PipelineRequest represents a pipelined request
type PipelineRequest struct {
	ID       uint64
	Data     []byte
	Response chan *PipelineResponse
}

// PipelineResponse represents a pipeline response
type PipelineResponse struct {
	ID    uint64
	Data  []byte
	Error error
}

// NewPipeline creates a new pipeline
func NewPipeline(depth int) *Pipeline {
	return &Pipeline{
		pending:  make(chan *PipelineRequest, depth),
		maxDepth: depth,
	}
}

// Submit submits a request to the pipeline
func (p *Pipeline) Submit(req *PipelineRequest) bool {
	p.mu.Lock()
	if p.inflight >= p.maxDepth {
		p.mu.Unlock()
		return false
	}
	p.inflight++
	p.mu.Unlock()

	select {
	case p.pending <- req:
		return true
	default:
		p.mu.Lock()
		p.inflight--
		p.mu.Unlock()
		return false
	}
}

// Complete marks a request as complete
func (p *Pipeline) Complete() {
	p.mu.Lock()
	if p.inflight > 0 {
		p.inflight--
	}
	p.mu.Unlock()
}

// Pending returns the pending channel
func (p *Pipeline) Pending() <-chan *PipelineRequest {
	return p.pending
}

// Depth returns current pipeline depth
func (p *Pipeline) Depth() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight
}

// MaxDepth returns maximum depth
func (p *Pipeline) MaxDepth() int {
	return p.maxDepth
}

// Close closes the pipeline
func (p *Pipeline) Close() {
	close(p.pending)
}
