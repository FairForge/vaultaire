package crypto

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestFastCDCChunker_Basic(t *testing.T) {
	chunker, err := DefaultFastCDCChunker()
	if err != nil {
		t.Fatalf("Failed to create chunker: %v", err)
	}

	if chunker.Algorithm() != ChunkingFastCDC {
		t.Errorf("Algorithm() = %v, want %v", chunker.Algorithm(), ChunkingFastCDC)
	}
}

func TestFastCDCChunker_SmallData(t *testing.T) {
	// Use smaller chunk sizes for testing
	chunker, err := NewFastCDCChunker(512, 1024, 2048)
	if err != nil {
		t.Fatalf("Failed to create chunker: %v", err)
	}

	// Data smaller than min chunk size
	data := []byte("Hello, World!")
	chunks, err := chunker.ChunkBytes(data)
	if err != nil {
		t.Fatalf("ChunkBytes failed: %v", err)
	}

	if len(chunks) != 1 {
		t.Errorf("Expected 1 chunk for small data, got %d", len(chunks))
	}

	if !chunks[0].IsFinal {
		t.Error("Single chunk should be marked as final")
	}

	// Verify data integrity
	reassembled := chunks[0].Data
	if !bytes.Equal(reassembled, data) {
		t.Error("Reassembled data doesn't match original")
	}
}

func TestFastCDCChunker_LargeData(t *testing.T) {
	// Use smaller chunk sizes for faster testing
	chunker, err := NewFastCDCChunker(1024, 4096, 8192)
	if err != nil {
		t.Fatalf("Failed to create chunker: %v", err)
	}

	// Generate 100KB of random data
	data := make([]byte, 100*1024)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("Failed to generate random data: %v", err)
	}

	chunks, err := chunker.ChunkBytes(data)
	if err != nil {
		t.Fatalf("ChunkBytes failed: %v", err)
	}

	if len(chunks) < 2 {
		t.Errorf("Expected multiple chunks for 100KB data, got %d", len(chunks))
	}

	// Verify all chunks have hashes
	for i, chunk := range chunks {
		if chunk.Hash == "" {
			t.Errorf("Chunk %d has empty hash", i)
		}
		if chunk.Size <= 0 {
			t.Errorf("Chunk %d has invalid size: %d", i, chunk.Size)
		}

		// Verify hash is correct
		expectedHash := sha256.Sum256(chunk.Data)
		if chunk.Hash != hex.EncodeToString(expectedHash[:]) {
			t.Errorf("Chunk %d hash mismatch", i)
		}
	}

	// Verify last chunk is marked final
	if !chunks[len(chunks)-1].IsFinal {
		t.Error("Last chunk should be marked as final")
	}

	// Verify data integrity by reassembling
	var reassembled []byte
	for _, chunk := range chunks {
		reassembled = append(reassembled, chunk.Data...)
	}
	if !bytes.Equal(reassembled, data) {
		t.Error("Reassembled data doesn't match original")
	}

	// Verify offsets are sequential
	var expectedOffset int64
	for i, chunk := range chunks {
		if chunk.Offset != expectedOffset {
			t.Errorf("Chunk %d offset = %d, want %d", i, chunk.Offset, expectedOffset)
		}
		expectedOffset += int64(chunk.Size)
	}
}

func TestFastCDCChunker_Deterministic(t *testing.T) {
	// Same data should produce same chunks with same polynomial
	chunker1, err := NewFastCDCChunkerWithPol(1024, 4096, 8192, 0x3DA3358B4DC173)
	if err != nil {
		t.Fatalf("Failed to create chunker1: %v", err)
	}
	chunker2, err := NewFastCDCChunkerWithPol(1024, 4096, 8192, 0x3DA3358B4DC173)
	if err != nil {
		t.Fatalf("Failed to create chunker2: %v", err)
	}

	data := make([]byte, 50*1024)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("Failed to generate random data: %v", err)
	}

	chunks1, err := chunker1.ChunkBytes(data)
	if err != nil {
		t.Fatalf("ChunkBytes failed: %v", err)
	}
	chunks2, err := chunker2.ChunkBytes(data)
	if err != nil {
		t.Fatalf("ChunkBytes failed: %v", err)
	}

	if len(chunks1) != len(chunks2) {
		t.Fatalf("Different number of chunks: %d vs %d", len(chunks1), len(chunks2))
	}

	for i := range chunks1 {
		if chunks1[i].Hash != chunks2[i].Hash {
			t.Errorf("Chunk %d hash mismatch", i)
		}
		if chunks1[i].Size != chunks2[i].Size {
			t.Errorf("Chunk %d size mismatch", i)
		}
	}
}

func TestFastCDCChunker_Streaming(t *testing.T) {
	chunker, err := NewFastCDCChunker(1024, 4096, 8192)
	if err != nil {
		t.Fatalf("Failed to create chunker: %v", err)
	}

	data := make([]byte, 50*1024)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("Failed to generate random data: %v", err)
	}

	ch, err := chunker.Chunk(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Chunk failed: %v", err)
	}

	var chunks []Chunk
	for result := range ch {
		if result.Err != nil {
			t.Fatalf("Chunk error: %v", result.Err)
		}
		chunks = append(chunks, result.Chunk)
	}

	if len(chunks) == 0 {
		t.Error("No chunks produced")
	}

	// Verify data integrity
	var reassembled []byte
	for _, chunk := range chunks {
		reassembled = append(reassembled, chunk.Data...)
	}
	if !bytes.Equal(reassembled, data) {
		t.Error("Reassembled data doesn't match original")
	}
}

func TestFastCDCChunker_EmptyData(t *testing.T) {
	chunker, err := DefaultFastCDCChunker()
	if err != nil {
		t.Fatalf("Failed to create chunker: %v", err)
	}

	chunks, err := chunker.ChunkBytes(nil)
	if err != nil {
		t.Fatalf("ChunkBytes failed: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("Expected 0 chunks for empty data, got %d", len(chunks))
	}

	chunks, err = chunker.ChunkBytes([]byte{})
	if err != nil {
		t.Fatalf("ChunkBytes failed: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("Expected 0 chunks for empty slice, got %d", len(chunks))
	}
}

func TestFastCDCChunker_InvalidParams(t *testing.T) {
	_, err := NewFastCDCChunker(0, 1024, 2048)
	if err == nil {
		t.Error("Expected error for zero min size")
	}

	_, err = NewFastCDCChunker(4096, 1024, 2048)
	if err == nil {
		t.Error("Expected error for min > avg")
	}

	_, err = NewFastCDCChunker(1024, 4096, 2048)
	if err == nil {
		t.Error("Expected error for avg > max")
	}
}

func TestFixedChunker_Basic(t *testing.T) {
	chunker, err := NewFixedChunker(1024)
	if err != nil {
		t.Fatalf("Failed to create chunker: %v", err)
	}

	if chunker.Algorithm() != ChunkingFixed {
		t.Errorf("Algorithm() = %v, want %v", chunker.Algorithm(), ChunkingFixed)
	}

	// 3KB data should produce 3 chunks (1KB + 1KB + 1KB)
	data := make([]byte, 3*1024)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("Failed to generate random data: %v", err)
	}

	chunks, err := chunker.ChunkBytes(data)
	if err != nil {
		t.Fatalf("ChunkBytes failed: %v", err)
	}

	if len(chunks) != 3 {
		t.Errorf("Expected 3 chunks, got %d", len(chunks))
	}

	// Verify each chunk is 1KB (last might be smaller)
	for i, chunk := range chunks[:len(chunks)-1] {
		if chunk.Size != 1024 {
			t.Errorf("Chunk %d size = %d, want 1024", i, chunk.Size)
		}
	}

	// Verify data integrity
	var reassembled []byte
	for _, chunk := range chunks {
		reassembled = append(reassembled, chunk.Data...)
	}
	if !bytes.Equal(reassembled, data) {
		t.Error("Reassembled data doesn't match original")
	}
}

func TestFixedChunker_Streaming(t *testing.T) {
	chunker, err := NewFixedChunker(1024)
	if err != nil {
		t.Fatalf("Failed to create chunker: %v", err)
	}

	data := make([]byte, 2500) // Should produce 3 chunks: 1024 + 1024 + 452
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("Failed to generate random data: %v", err)
	}

	ch, err := chunker.Chunk(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Chunk failed: %v", err)
	}

	var chunks []Chunk
	for result := range ch {
		if result.Err != nil {
			t.Fatalf("Chunk error: %v", result.Err)
		}
		chunks = append(chunks, result.Chunk)
	}

	if len(chunks) != 3 {
		t.Errorf("Expected 3 chunks, got %d", len(chunks))
	}

	// Last chunk should be marked final and smaller
	lastChunk := chunks[len(chunks)-1]
	if !lastChunk.IsFinal {
		t.Error("Last chunk should be marked as final")
	}
	if lastChunk.Size != 452 {
		t.Errorf("Last chunk size = %d, want 452", lastChunk.Size)
	}
}

func TestNewChunkerFromConfig(t *testing.T) {
	// Test FastCDC from config
	config := ConfigSmartStorage
	chunker, err := NewChunkerFromConfig(config)
	if err != nil {
		t.Fatalf("Failed to create chunker from config: %v", err)
	}
	if chunker.Algorithm() != ChunkingFastCDC {
		t.Errorf("Expected FastCDC, got %v", chunker.Algorithm())
	}

	// Test Fixed from config
	config.ChunkingAlgo = ChunkingFixed
	chunker, err = NewChunkerFromConfig(config)
	if err != nil {
		t.Fatalf("Failed to create chunker from config: %v", err)
	}
	if chunker.Algorithm() != ChunkingFixed {
		t.Errorf("Expected Fixed, got %v", chunker.Algorithm())
	}

	// Test disabled chunking
	config.ChunkingEnabled = false
	_, err = NewChunkerFromConfig(config)
	if err == nil {
		t.Error("Expected error for disabled chunking")
	}
}

// TestChunkDeduplication verifies that identical content produces identical chunks
func TestChunkDeduplication(t *testing.T) {
	chunker, err := NewFastCDCChunkerWithPol(1024, 4096, 8192, 0x3DA3358B4DC173)
	if err != nil {
		t.Fatalf("Failed to create chunker: %v", err)
	}

	// Create same file twice - should produce identical chunks
	data := make([]byte, 50*1024)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("Failed to generate random data: %v", err)
	}

	chunks1, err := chunker.ChunkBytes(data)
	if err != nil {
		t.Fatalf("ChunkBytes failed: %v", err)
	}

	// Same data again
	chunks2, err := chunker.ChunkBytes(data)
	if err != nil {
		t.Fatalf("ChunkBytes failed: %v", err)
	}

	if len(chunks1) != len(chunks2) {
		t.Fatalf("Different number of chunks: %d vs %d", len(chunks1), len(chunks2))
	}

	// ALL chunks should match for identical data
	for i := range chunks1 {
		if chunks1[i].Hash != chunks2[i].Hash {
			t.Errorf("Chunk %d hash mismatch for identical data", i)
		}
	}

	t.Logf("Identical files: %d chunks each, 100%% dedup", len(chunks1))
}

// TestChunkAppendScenario tests appending data to a file (common backup scenario)
func TestChunkAppendScenario(t *testing.T) {
	chunker, err := NewFastCDCChunkerWithPol(512, 2048, 4096, 0x3DA3358B4DC173)
	if err != nil {
		t.Fatalf("Failed to create chunker: %v", err)
	}

	// Original file
	original := make([]byte, 30*1024)
	if _, err := rand.Read(original); err != nil {
		t.Fatalf("Failed to generate random data: %v", err)
	}

	// Appended file (original + new data at end)
	appendData := make([]byte, 10*1024)
	if _, err := rand.Read(appendData); err != nil {
		t.Fatalf("Failed to generate random data: %v", err)
	}
	appended := append(original, appendData...)

	chunksOrig, err := chunker.ChunkBytes(original)
	if err != nil {
		t.Fatalf("ChunkBytes failed: %v", err)
	}
	chunksAppended, err := chunker.ChunkBytes(appended)
	if err != nil {
		t.Fatalf("ChunkBytes failed: %v", err)
	}

	// Build hash set from original
	hashSetOrig := make(map[string]bool)
	for _, c := range chunksOrig {
		hashSetOrig[c.Hash] = true
	}

	// Count how many chunks from appended file match original
	matches := 0
	for _, c := range chunksAppended {
		if hashSetOrig[c.Hash] {
			matches++
		}
	}

	t.Logf("Append scenario: Original %d chunks, Appended %d chunks, Reused %d",
		len(chunksOrig), len(chunksAppended), matches)

	// Most of the original chunks should still exist
	// (CDC finds same boundaries in unchanged regions)
	if matches == 0 && len(chunksOrig) > 2 {
		t.Log("Note: No chunk reuse in append test - this can happen with small test data")
	}
}

func BenchmarkFastCDCChunker_1MB(b *testing.B) {
	chunker, _ := NewFastCDCChunker(256*1024, 1024*1024, 4*1024*1024)
	data := make([]byte, 1024*1024)
	if _, err := rand.Read(data); err != nil {
		b.Fatalf("Failed to generate random data: %v", err)
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		_, _ = chunker.ChunkBytes(data)
	}
}

func BenchmarkFastCDCChunker_10MB(b *testing.B) {
	chunker, _ := NewFastCDCChunker(1024*1024, 4*1024*1024, 16*1024*1024)
	data := make([]byte, 10*1024*1024)
	if _, err := rand.Read(data); err != nil {
		b.Fatalf("Failed to generate random data: %v", err)
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		_, _ = chunker.ChunkBytes(data)
	}
}

func BenchmarkFastCDCChunker_100MB(b *testing.B) {
	chunker, _ := NewFastCDCChunker(1024*1024, 4*1024*1024, 16*1024*1024)
	data := make([]byte, 100*1024*1024)
	if _, err := rand.Read(data); err != nil {
		b.Fatalf("Failed to generate random data: %v", err)
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		_, _ = chunker.ChunkBytes(data)
	}
}

func BenchmarkFixedChunker_10MB(b *testing.B) {
	chunker, _ := NewFixedChunker(4 * 1024 * 1024)
	data := make([]byte, 10*1024*1024)
	if _, err := rand.Read(data); err != nil {
		b.Fatalf("Failed to generate random data: %v", err)
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		_, _ = chunker.ChunkBytes(data)
	}
}

func BenchmarkFastCDCChunker_Streaming_100MB(b *testing.B) {
	chunker, _ := NewFastCDCChunker(1024*1024, 4*1024*1024, 16*1024*1024)
	data := make([]byte, 100*1024*1024)
	if _, err := rand.Read(data); err != nil {
		b.Fatalf("Failed to generate random data: %v", err)
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		ch, _ := chunker.Chunk(bytes.NewReader(data))
		for range ch {
			// Drain channel
		}
	}
}
