package crypto

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

func TestZstdCompressor_Basic(t *testing.T) {
	c, err := DefaultZstdCompressor()
	if err != nil {
		t.Fatalf("Failed to create compressor: %v", err)
	}
	if c.Algorithm() != CompressionZstd {
		t.Errorf("Algorithm() = %v, want %v", c.Algorithm(), CompressionZstd)
	}
	if c.Level() != 3 {
		t.Errorf("Level() = %d, want 3", c.Level())
	}
}

func TestZstdCompressor_RoundTrip(t *testing.T) {
	c, err := DefaultZstdCompressor()
	if err != nil {
		t.Fatalf("Failed to create compressor: %v", err)
	}

	original := []byte("Hello, World! This is a test of zstd compression.")
	compressed, err := c.Compress(original)
	if err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	decompressed, err := c.Decompress(compressed)
	if err != nil {
		t.Fatalf("Decompress failed: %v", err)
	}

	if !bytes.Equal(original, decompressed) {
		t.Error("Decompressed data doesn't match original")
	}
}

func TestZstdCompressor_EmptyData(t *testing.T) {
	c, _ := DefaultZstdCompressor()

	compressed, err := c.Compress(nil)
	if err != nil || len(compressed) != 0 {
		t.Errorf("Expected empty result for nil input")
	}

	compressed, err = c.Compress([]byte{})
	if err != nil || len(compressed) != 0 {
		t.Errorf("Expected empty result for empty input")
	}
}

func TestZstdCompressor_LargeData(t *testing.T) {
	c, _ := DefaultZstdCompressor()
	original := bytes.Repeat([]byte("ABCDEFGHIJKLMNOP"), 64*1024)

	compressed, err := c.Compress(original)
	if err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	ratio := float64(len(original)) / float64(len(compressed))
	if ratio < 10 {
		t.Errorf("Expected >10x compression, got %.2fx", ratio)
	}

	decompressed, _ := c.Decompress(compressed)
	if !bytes.Equal(original, decompressed) {
		t.Error("Decompressed data doesn't match original")
	}
	t.Logf("Compressed 1MB: %.2fx ratio", ratio)
}

func TestZstdCompressor_RandomData(t *testing.T) {
	c, _ := DefaultZstdCompressor()
	original := make([]byte, 64*1024)
	_, _ = rand.Read(original)

	compressed, err := c.Compress(original)
	if err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	ratio := float64(len(original)) / float64(len(compressed))
	t.Logf("Random data: %.2fx ratio", ratio)

	decompressed, _ := c.Decompress(compressed)
	if !bytes.Equal(original, decompressed) {
		t.Error("Decompressed data doesn't match original")
	}
}

func TestZstdCompressor_Levels(t *testing.T) {
	data := bytes.Repeat([]byte("Test data for compression levels. "), 10000)

	for _, level := range []int{1, 3, 9, 19} {
		c, _ := NewZstdCompressor(level)
		compressed, _ := c.Compress(data)
		t.Logf("Level %d: %d -> %d bytes (%.2fx)",
			level, len(data), len(compressed),
			float64(len(data))/float64(len(compressed)))
	}
}

func TestZstdCompressor_InvalidLevel(t *testing.T) {
	if _, err := NewZstdCompressor(0); err == nil {
		t.Error("Expected error for level 0")
	}
	if _, err := NewZstdCompressor(20); err == nil {
		t.Error("Expected error for level 20")
	}
}

func TestZstdCompressor_Stream(t *testing.T) {
	c, _ := DefaultZstdCompressor()
	original := bytes.Repeat([]byte("Stream test! "), 10000)

	var compressed bytes.Buffer
	_, _ = c.CompressStream(&compressed, bytes.NewReader(original))

	var decompressed bytes.Buffer
	_, _ = c.DecompressStream(&decompressed, &compressed)

	if !bytes.Equal(original, decompressed.Bytes()) {
		t.Error("Stream round-trip failed")
	}
}

func TestNoopCompressor(t *testing.T) {
	c := NewNoopCompressor()
	if c.Algorithm() != CompressionNone {
		t.Errorf("Algorithm() = %v, want %v", c.Algorithm(), CompressionNone)
	}

	original := []byte("Test data")
	compressed, _ := c.Compress(original)
	if !bytes.Equal(original, compressed) {
		t.Error("NoopCompressor should return unchanged data")
	}
}

func TestNewCompressorFromConfig(t *testing.T) {
	tests := []struct {
		name     string
		config   PipelineConfig
		wantAlgo CompressionAlgorithm
	}{
		{"disabled", PipelineConfig{CompressionEnabled: false}, CompressionNone},
		{"zstd", PipelineConfig{CompressionEnabled: true, CompressionAlgo: CompressionZstd, CompressionLevel: 3}, CompressionZstd},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewCompressorFromConfig(tt.config)
			if err != nil {
				t.Fatalf("NewCompressorFromConfig() error = %v", err)
			}
			if c.Algorithm() != tt.wantAlgo {
				t.Errorf("Algorithm() = %v, want %v", c.Algorithm(), tt.wantAlgo)
			}
		})
	}
}

func TestShouldCompress(t *testing.T) {
	tests := []struct {
		name        string
		data        []byte
		contentType string
		want        bool
	}{
		{"small", []byte("tiny"), "text/plain", false},
		{"text", bytes.Repeat([]byte("t"), 1000), "text/plain", true},
		{"jpeg", bytes.Repeat([]byte("x"), 1000), "image/jpeg", false},
		{"zip magic", append([]byte{0x50, 0x4B, 0x03, 0x04}, bytes.Repeat([]byte("x"), 1000)...), "", false},
		{"gzip magic", append([]byte{0x1F, 0x8B}, bytes.Repeat([]byte("x"), 1000)...), "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldCompress(tt.data, tt.contentType); got != tt.want {
				t.Errorf("ShouldCompress() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompressChunk(t *testing.T) {
	c, _ := DefaultZstdCompressor()
	chunkData := bytes.Repeat([]byte("chunk data "), 1000)
	chunk := &Chunk{Hash: "abc123", Data: chunkData, Size: len(chunkData)}

	compressed, err := CompressChunk(c, chunk)
	if err != nil {
		t.Fatalf("CompressChunk failed: %v", err)
	}
	if compressed.OriginalHash != chunk.Hash {
		t.Errorf("OriginalHash mismatch")
	}
	if compressed.CompressionRatio <= 1.0 {
		t.Errorf("Expected ratio > 1.0, got %.2f", compressed.CompressionRatio)
	}
	t.Logf("Chunk: %d -> %d bytes (%.2fx)", compressed.OriginalSize, compressed.CompressedSize, compressed.CompressionRatio)
}

func TestCompressionStats(t *testing.T) {
	stats := &CompressionStats{}
	stats.AddCompressed(1000, 500)
	stats.AddCompressed(2000, 800)
	stats.AddSkipped(500)

	if stats.ChunksCompressed != 2 || stats.ChunksSkipped != 1 {
		t.Error("Stats counts wrong")
	}
	if stats.BytesSaved() != 1700 {
		t.Errorf("BytesSaved() = %d, want 1700", stats.BytesSaved())
	}
}

func TestPooledCompressor(t *testing.T) {
	original := bytes.Repeat([]byte("pooled "), 1000)
	compressed, _ := CompressBuffer(original)
	decompressed, _ := DecompressBuffer(compressed)
	if !bytes.Equal(original, decompressed) {
		t.Error("Pooled round-trip failed")
	}
}

func TestCompression_RealisticText(t *testing.T) {
	c, _ := DefaultZstdCompressor()
	jsonData := []byte(strings.Repeat(`{"id":123,"name":"User"},`, 1000))
	compressed, _ := c.Compress(jsonData)
	ratio := float64(len(jsonData)) / float64(len(compressed))
	t.Logf("JSON: %.2fx compression", ratio)
	if ratio < 5 {
		t.Errorf("Expected >5x for JSON, got %.2fx", ratio)
	}
}
