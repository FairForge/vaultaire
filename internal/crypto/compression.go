package crypto

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Compressor provides compression and decompression
type Compressor interface {
	Compress(data []byte) ([]byte, error)
	Decompress(data []byte) ([]byte, error)
	CompressStream(dst io.Writer, src io.Reader) (int64, error)
	DecompressStream(dst io.Writer, src io.Reader) (int64, error)
	Algorithm() CompressionAlgorithm
	Level() int
}

// ZstdCompressor implements Compressor using zstd
type ZstdCompressor struct {
	level       int
	encoder     *zstd.Encoder
	decoder     *zstd.Decoder
	encoderOnce sync.Once
	decoderOnce sync.Once
	encoderErr  error
	decoderErr  error
}

// NewZstdCompressor creates a new zstd compressor
func NewZstdCompressor(level int) (*ZstdCompressor, error) {
	if level < 1 || level > 19 {
		return nil, fmt.Errorf("zstd level must be 1-19, got %d", level)
	}
	return &ZstdCompressor{level: level}, nil
}

// DefaultZstdCompressor creates a compressor with default settings (level 3)
func DefaultZstdCompressor() (*ZstdCompressor, error) {
	return NewZstdCompressor(3)
}

// FastZstdCompressor creates a fast compressor (level 1)
func FastZstdCompressor() (*ZstdCompressor, error) {
	return NewZstdCompressor(1)
}

// BestZstdCompressor creates a best compression compressor (level 19)
func BestZstdCompressor() (*ZstdCompressor, error) {
	return NewZstdCompressor(19)
}

func (c *ZstdCompressor) getEncoder() (*zstd.Encoder, error) {
	c.encoderOnce.Do(func() {
		var opts []zstd.EOption
		opts = append(opts, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(c.level)))
		opts = append(opts, zstd.WithEncoderConcurrency(1))
		c.encoder, c.encoderErr = zstd.NewWriter(nil, opts...)
	})
	return c.encoder, c.encoderErr
}

func (c *ZstdCompressor) getDecoder() (*zstd.Decoder, error) {
	c.decoderOnce.Do(func() {
		c.decoder, c.decoderErr = zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(256*1024*1024),
		)
	})
	return c.decoder, c.decoderErr
}

func (c *ZstdCompressor) Compress(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return data, nil
	}
	encoder, err := c.getEncoder()
	if err != nil {
		return nil, fmt.Errorf("failed to get encoder: %w", err)
	}
	return encoder.EncodeAll(data, make([]byte, 0, len(data))), nil
}

func (c *ZstdCompressor) Decompress(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return data, nil
	}
	decoder, err := c.getDecoder()
	if err != nil {
		return nil, fmt.Errorf("failed to get decoder: %w", err)
	}
	return decoder.DecodeAll(data, nil)
}

func (c *ZstdCompressor) CompressStream(dst io.Writer, src io.Reader) (int64, error) {
	var opts []zstd.EOption
	opts = append(opts, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(c.level)))
	encoder, err := zstd.NewWriter(dst, opts...)
	if err != nil {
		return 0, fmt.Errorf("failed to create stream encoder: %w", err)
	}
	defer func() { _ = encoder.Close() }()

	written, err := io.Copy(encoder, src)
	if err != nil {
		return written, fmt.Errorf("compression failed: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return written, fmt.Errorf("failed to close encoder: %w", err)
	}
	return written, nil
}

func (c *ZstdCompressor) DecompressStream(dst io.Writer, src io.Reader) (int64, error) {
	decoder, err := zstd.NewReader(src, zstd.WithDecoderMaxMemory(256*1024*1024))
	if err != nil {
		return 0, fmt.Errorf("failed to create stream decoder: %w", err)
	}
	defer decoder.Close()

	written, err := io.Copy(dst, decoder)
	if err != nil {
		return written, fmt.Errorf("decompression failed: %w", err)
	}
	return written, nil
}

func (c *ZstdCompressor) Algorithm() CompressionAlgorithm {
	return CompressionZstd
}

func (c *ZstdCompressor) Level() int {
	return c.level
}

// NoopCompressor is a pass-through compressor
type NoopCompressor struct{}

func NewNoopCompressor() *NoopCompressor {
	return &NoopCompressor{}
}

func (c *NoopCompressor) Compress(data []byte) ([]byte, error)   { return data, nil }
func (c *NoopCompressor) Decompress(data []byte) ([]byte, error) { return data, nil }

func (c *NoopCompressor) CompressStream(dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, src)
}

func (c *NoopCompressor) DecompressStream(dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, src)
}

func (c *NoopCompressor) Algorithm() CompressionAlgorithm { return CompressionNone }
func (c *NoopCompressor) Level() int                      { return 0 }

// NewCompressorFromConfig creates a compressor based on pipeline config
func NewCompressorFromConfig(config PipelineConfig) (Compressor, error) {
	if !config.CompressionEnabled {
		return NewNoopCompressor(), nil
	}
	switch config.CompressionAlgo {
	case CompressionZstd:
		return NewZstdCompressor(config.CompressionLevel)
	case CompressionNone, "":
		return NewNoopCompressor(), nil
	default:
		return nil, fmt.Errorf("unsupported compression algorithm: %s", config.CompressionAlgo)
	}
}

// CompressedChunk holds a compressed chunk with metadata
type CompressedChunk struct {
	OriginalHash     string
	CompressedData   []byte
	OriginalSize     int64
	CompressedSize   int64
	Algorithm        CompressionAlgorithm
	Level            int
	CompressionRatio float64
}

// CompressChunk compresses a chunk and returns metadata
func CompressChunk(compressor Compressor, chunk *Chunk) (*CompressedChunk, error) {
	compressed, err := compressor.Compress(chunk.Data)
	if err != nil {
		return nil, fmt.Errorf("failed to compress chunk: %w", err)
	}

	originalSize := int64(len(chunk.Data))
	compressedSize := int64(len(compressed))
	ratio := 1.0
	if compressedSize > 0 {
		ratio = float64(originalSize) / float64(compressedSize)
	}

	return &CompressedChunk{
		OriginalHash:     chunk.Hash,
		CompressedData:   compressed,
		OriginalSize:     originalSize,
		CompressedSize:   compressedSize,
		Algorithm:        compressor.Algorithm(),
		Level:            compressor.Level(),
		CompressionRatio: ratio,
	}, nil
}

// ShouldCompress determines if data should be compressed
func ShouldCompress(data []byte, contentType string) bool {
	if len(data) < 512 {
		return false
	}

	compressedTypes := map[string]bool{
		"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true,
		"video/mp4": true, "video/webm": true, "video/quicktime": true,
		"audio/mpeg": true, "audio/mp4": true, "audio/ogg": true,
		"application/zip": true, "application/gzip": true, "application/x-gzip": true,
		"application/x-bzip2": true, "application/x-xz": true,
		"application/x-7z-compressed": true, "application/x-rar-compressed": true,
		"application/pdf": true,
	}
	if compressedTypes[contentType] {
		return false
	}

	// Check magic bytes
	if len(data) >= 4 {
		if data[0] == 0x50 && data[1] == 0x4B && data[2] == 0x03 && data[3] == 0x04 {
			return false
		} // ZIP
		if data[0] == 0x1F && data[1] == 0x8B {
			return false
		} // GZIP
		if data[0] == 0x28 && data[1] == 0xB5 && data[2] == 0x2F && data[3] == 0xFD {
			return false
		} // ZSTD
		if data[0] == 0xFD && data[1] == 0x37 && data[2] == 0x7A && data[3] == 0x58 {
			return false
		} // XZ
	}
	return true
}

// CompressionStats tracks compression efficiency
type CompressionStats struct {
	TotalOriginalBytes   int64
	TotalCompressedBytes int64
	ChunksCompressed     int64
	ChunksSkipped        int64
}

func (s *CompressionStats) AddCompressed(original, compressed int64) {
	s.TotalOriginalBytes += original
	s.TotalCompressedBytes += compressed
	s.ChunksCompressed++
}

func (s *CompressionStats) AddSkipped(size int64) {
	s.TotalOriginalBytes += size
	s.TotalCompressedBytes += size
	s.ChunksSkipped++
}

func (s *CompressionStats) Ratio() float64 {
	if s.TotalCompressedBytes == 0 {
		return 1.0
	}
	return float64(s.TotalOriginalBytes) / float64(s.TotalCompressedBytes)
}

func (s *CompressionStats) BytesSaved() int64 {
	return s.TotalOriginalBytes - s.TotalCompressedBytes
}

// Pooled compressors for concurrent use
var compressorPool = sync.Pool{
	New: func() interface{} {
		c, _ := DefaultZstdCompressor()
		return c
	},
}

func GetPooledCompressor() *ZstdCompressor  { return compressorPool.Get().(*ZstdCompressor) }
func PutPooledCompressor(c *ZstdCompressor) { compressorPool.Put(c) }

func CompressBuffer(data []byte) ([]byte, error) {
	c := GetPooledCompressor()
	defer PutPooledCompressor(c)
	return c.Compress(data)
}

func DecompressBuffer(data []byte) ([]byte, error) {
	c := GetPooledCompressor()
	defer PutPooledCompressor(c)
	return c.Decompress(data)
}

func CompressReader(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	c := GetPooledCompressor()
	defer PutPooledCompressor(c)
	_, err := c.CompressStream(&buf, r)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
