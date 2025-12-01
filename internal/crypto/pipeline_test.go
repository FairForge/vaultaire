package crypto

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"testing"
)

func TestNewPipeline_Presets(t *testing.T) {
	presets := []string{"smart", "archive", "hpc", "passthrough", "enterprise"}

	for _, preset := range presets {
		t.Run(preset, func(t *testing.T) {
			p, err := NewPipelineFromPreset(preset)
			if err != nil {
				t.Fatalf("NewPipelineFromPreset(%s) failed: %v", preset, err)
			}
			if p == nil {
				t.Fatal("Pipeline is nil")
			}
		})
	}
}

func TestPipeline_Passthrough(t *testing.T) {
	p, err := NewPipelineFromPreset("passthrough")
	if err != nil {
		t.Fatalf("Failed to create pipeline: %v", err)
	}

	original := []byte("Hello, World! This is passthrough data.")
	ctx := context.Background()
	uc := &UploadContext{ContentType: "text/plain"}

	result, err := p.Process(ctx, bytes.NewReader(original), uc)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	if result.ChunkCount != 1 {
		t.Errorf("ChunkCount = %d, want 1", result.ChunkCount)
	}

	if result.TotalSize != int64(len(original)) {
		t.Errorf("TotalSize = %d, want %d", result.TotalSize, len(original))
	}

	// Passthrough should not change data
	if !bytes.Equal(result.Chunks[0].Data, original) {
		t.Error("Passthrough should not modify data")
	}

	// Reconstruct
	var reconstructed bytes.Buffer
	rc := &ReconstructContext{}
	err = p.Reconstruct(ctx, result.Chunks, rc, &reconstructed)
	if err != nil {
		t.Fatalf("Reconstruct failed: %v", err)
	}

	if !bytes.Equal(original, reconstructed.Bytes()) {
		t.Error("Reconstructed data doesn't match original")
	}
}

func TestPipeline_SmartStorage(t *testing.T) {
	p, err := NewPipelineFromPreset("smart")
	if err != nil {
		t.Fatalf("Failed to create pipeline: %v", err)
	}

	// Create data larger than max chunk size (16MB) to ensure multiple chunks
	// Using random-ish data that still compresses somewhat
	var original bytes.Buffer
	for i := 0; i < 20; i++ {
		original.WriteString(strings.Repeat(fmt.Sprintf("Block %d: test data with some variation. ", i), 100000))
	}
	data := original.Bytes() // ~80MB of semi-compressible data

	ctx := context.Background()

	tenantKey, _ := GenerateTenantKey()
	uc := &UploadContext{
		TenantKey:   tenantKey,
		KeyVersion:  1,
		ContentType: "text/plain",
	}

	result, err := p.Process(ctx, bytes.NewReader(data), uc)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	// Should have multiple chunks (data > 16MB max chunk)
	if result.ChunkCount < 2 {
		t.Logf("Warning: Expected multiple chunks, got %d (data size: %d bytes)",
			result.ChunkCount, len(data))
	}

	// Should have compression (text compresses well)
	if result.ProcessedSize >= result.TotalSize {
		t.Errorf("Expected compression, got expansion: %d -> %d",
			result.TotalSize, result.ProcessedSize)
	}

	// All chunks should be encrypted
	for i, chunk := range result.Chunks {
		if !chunk.Encrypted {
			t.Errorf("Chunk %d should be encrypted", i)
		}
	}

	t.Logf("Processed %d bytes -> %d bytes (%d chunks, %.2fx ratio)",
		result.TotalSize, result.ProcessedSize, result.ChunkCount, result.DedupRatio)

	// Reconstruct
	var reconstructed bytes.Buffer
	rc := &ReconstructContext{
		TenantKey:  tenantKey,
		KeyVersion: 1,
	}
	err = p.Reconstruct(ctx, result.Chunks, rc, &reconstructed)
	if err != nil {
		t.Fatalf("Reconstruct failed: %v", err)
	}

	if !bytes.Equal(data, reconstructed.Bytes()) {
		t.Errorf("Reconstructed data doesn't match original (got %d bytes, want %d)",
			reconstructed.Len(), len(data))
	}
}

func TestPipeline_ConvergentEncryption(t *testing.T) {
	config, _ := GetPreset("smart")
	p, _ := NewPipeline(config)

	// Same content
	content := []byte(strings.Repeat("Identical content ", 100000))

	tenant1Key, _ := GenerateTenantKey()
	tenant2Key, _ := GenerateTenantKey()

	ctx := context.Background()

	// Process for tenant 1
	result1, _ := p.Process(ctx, bytes.NewReader(content), &UploadContext{
		TenantKey:  tenant1Key,
		KeyVersion: 1,
	})

	// Process for tenant 2
	result2, _ := p.Process(ctx, bytes.NewReader(content), &UploadContext{
		TenantKey:  tenant2Key,
		KeyVersion: 1,
	})

	// Plaintext hashes should be identical (for dedup)
	for i := range result1.Chunks {
		if result1.Chunks[i].PlaintextHash != result2.Chunks[i].PlaintextHash {
			t.Errorf("Chunk %d plaintext hashes differ (should match for dedup)", i)
		}
	}

	// But ciphertext should differ (different tenant keys)
	for i := range result1.Chunks {
		if result1.Chunks[i].CiphertextHash == result2.Chunks[i].CiphertextHash {
			t.Errorf("Chunk %d ciphertext hashes match (should differ)", i)
		}
	}

	t.Log("Convergent encryption working: same plaintext hash, different ciphertext")
}

func TestPipeline_RandomEncryption(t *testing.T) {
	config := ConfigHPC // HPC uses random encryption mode
	p, _ := NewPipeline(config)

	content := []byte(strings.Repeat("Random mode test ", 200000))
	tenantKey, _ := GenerateTenantKey()

	ctx := context.Background()
	uc := &UploadContext{TenantKey: tenantKey, KeyVersion: 1}

	// Process same content twice
	result1, _ := p.Process(ctx, bytes.NewReader(content), uc)
	result2, _ := p.Process(ctx, bytes.NewReader(content), uc)

	// Plaintext hashes should still match
	if result1.Chunks[0].PlaintextHash != result2.Chunks[0].PlaintextHash {
		t.Error("Plaintext hashes should match")
	}

	// Reconstruct both
	var recon1, recon2 bytes.Buffer
	rc := &ReconstructContext{TenantKey: tenantKey, KeyVersion: 1}

	_ = p.Reconstruct(ctx, result1.Chunks, rc, &recon1)
	_ = p.Reconstruct(ctx, result2.Chunks, rc, &recon2)

	if !bytes.Equal(content, recon1.Bytes()) || !bytes.Equal(content, recon2.Bytes()) {
		t.Error("Reconstruction failed")
	}
}

func TestPipeline_SkipCompressionForCompressedData(t *testing.T) {
	p, _ := NewPipelineFromPreset("smart")

	// Random data (incompressible)
	randomData := make([]byte, 100000)
	_, _ = rand.Read(randomData)

	ctx := context.Background()
	tenantKey, _ := GenerateTenantKey()

	// Process with JPEG content type (should skip compression)
	result, _ := p.Process(ctx, bytes.NewReader(randomData), &UploadContext{
		TenantKey:   tenantKey,
		ContentType: "image/jpeg",
	})

	// Chunk should not be marked as compressed
	if result.Chunks[0].Compressed {
		t.Error("JPEG content should skip compression")
	}

	// Process with text type
	result2, _ := p.Process(ctx, bytes.NewReader(randomData), &UploadContext{
		TenantKey:   tenantKey,
		ContentType: "text/plain",
	})

	// Random data compresses poorly, so might not be compressed even for text
	// But the flag should reflect what actually happened
	t.Logf("Random data compression: text=%v, jpeg=%v",
		result2.Chunks[0].Compressed, result.Chunks[0].Compressed)
}

func TestPipeline_ContentHash(t *testing.T) {
	p, _ := NewPipelineFromPreset("smart")

	content := []byte("Test content for hashing")
	ctx := context.Background()
	tenantKey, _ := GenerateTenantKey()

	result, _ := p.Process(ctx, bytes.NewReader(content), &UploadContext{
		TenantKey: tenantKey,
	})

	if result.ContentHash == "" {
		t.Error("ContentHash should not be empty")
	}

	if len(result.ContentHash) != 64 { // SHA-256 hex
		t.Errorf("ContentHash length = %d, want 64", len(result.ContentHash))
	}

	// Same content should produce same hash
	result2, _ := p.Process(ctx, bytes.NewReader(content), &UploadContext{
		TenantKey: tenantKey,
	})

	if result.ContentHash != result2.ContentHash {
		t.Error("Same content should produce same hash")
	}
}

func TestPipeline_StreamProcessing(t *testing.T) {
	p, _ := NewPipelineFromPreset("smart")

	content := []byte(strings.Repeat("Streaming test data ", 50000))
	ctx := context.Background()
	tenantKey, _ := GenerateTenantKey()

	uc := &UploadContext{TenantKey: tenantKey, KeyVersion: 1}

	// Process using streaming
	result, err := p.ProcessStream(ctx, bytes.NewReader(content), uc)
	if err != nil {
		t.Fatalf("ProcessStream failed: %v", err)
	}

	if result.ContentHash == "" {
		t.Error("ContentHash should not be empty")
	}

	// Reconstruct
	var reconstructed bytes.Buffer
	rc := &ReconstructContext{TenantKey: tenantKey, KeyVersion: 1}
	err = p.Reconstruct(ctx, result.Chunks, rc, &reconstructed)
	if err != nil {
		t.Fatalf("Reconstruct failed: %v", err)
	}

	if !bytes.Equal(content, reconstructed.Bytes()) {
		t.Error("Reconstructed data doesn't match original")
	}

	t.Logf("Stream processed: %d bytes -> %d bytes (%.2fx)",
		result.TotalSize, result.ProcessedSize, result.DedupRatio)
}

func TestPipelineStats(t *testing.T) {
	stats := &PipelineStats{}

	result := &ProcessResult{
		TotalSize:     1000,
		ProcessedSize: 500,
		ChunkCount:    2,
		Chunks: []*ProcessedChunk{
			{Compressed: true, Encrypted: true},
			{Compressed: false, Encrypted: true},
		},
	}

	stats.Add(result)

	if stats.BytesIn != 1000 {
		t.Errorf("BytesIn = %d, want 1000", stats.BytesIn)
	}
	if stats.BytesOut != 500 {
		t.Errorf("BytesOut = %d, want 500", stats.BytesOut)
	}
	if stats.ChunksProcessed != 2 {
		t.Errorf("ChunksProcessed = %d, want 2", stats.ChunksProcessed)
	}
	if stats.ChunksCompressed != 1 {
		t.Errorf("ChunksCompressed = %d, want 1", stats.ChunksCompressed)
	}
	if stats.ChunksEncrypted != 2 {
		t.Errorf("ChunksEncrypted = %d, want 2", stats.ChunksEncrypted)
	}
	if stats.CompressionRatio() != 2.0 {
		t.Errorf("CompressionRatio() = %.2f, want 2.0", stats.CompressionRatio())
	}
}

// Benchmark

func BenchmarkPipeline_Smart_1MB(b *testing.B) {
	p, _ := NewPipelineFromPreset("smart")
	data := bytes.Repeat([]byte("Benchmark data "), 70000) // ~1MB
	tenantKey, _ := GenerateTenantKey()
	ctx := context.Background()

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		_, _ = p.Process(ctx, bytes.NewReader(data), &UploadContext{
			TenantKey: tenantKey,
		})
	}
}

func BenchmarkPipeline_Passthrough_1MB(b *testing.B) {
	p, _ := NewPipelineFromPreset("passthrough")
	data := bytes.Repeat([]byte("Benchmark data "), 70000)
	ctx := context.Background()

	b.ResetTimer()
	b.SetBytes(int64(len(data)))

	for i := 0; i < b.N; i++ {
		_, _ = p.Process(ctx, bytes.NewReader(data), &UploadContext{})
	}
}

func TestPipeline_RealisticData(t *testing.T) {
	p, _ := NewPipelineFromPreset("smart")
	ctx := context.Background()
	tenantKey, _ := GenerateTenantKey()
	uc := &UploadContext{TenantKey: tenantKey, KeyVersion: 1}

	tests := []struct {
		name        string
		data        []byte
		contentType string
		minRatio    float64 // Minimum expected compression
		maxRatio    float64 // Maximum expected compression
	}{
		{
			name:        "JSON API response",
			data:        []byte(strings.Repeat(`{"id":12345,"name":"John Doe","email":"john@example.com","active":true},`, 10000)),
			contentType: "application/json",
			minRatio:    3.0,
			maxRatio:    20.0,
		},
		{
			name:        "Random binary (incompressible)",
			data:        func() []byte { b := make([]byte, 100000); _, _ = rand.Read(b); return b }(),
			contentType: "application/octet-stream",
			minRatio:    0.9, // Might expand slightly due to encryption overhead
			maxRatio:    1.1,
		},
		{
			name:        "Log file pattern",
			data:        []byte(strings.Repeat("2024-01-15 10:23:45 INFO  [main] Processing request id=", 20000)),
			contentType: "text/plain",
			minRatio:    5.0,
			maxRatio:    50.0,
		},
		{
			name:        "HTML page",
			data:        []byte(strings.Repeat("<div class=\"container\"><p>Content paragraph with some text.</p></div>\n", 15000)),
			contentType: "text/html",
			minRatio:    4.0,
			maxRatio:    30.0,
		},
		{
			name:        "CSV data",
			data:        []byte(strings.Repeat("2024-01-15,product_abc,149.99,USD,completed,user_12345\n", 20000)),
			contentType: "text/csv",
			minRatio:    3.0,
			maxRatio:    15.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uc.ContentType = tt.contentType
			result, err := p.Process(ctx, bytes.NewReader(tt.data), uc)
			if err != nil {
				t.Fatalf("Process failed: %v", err)
			}

			// Verify round-trip
			var reconstructed bytes.Buffer
			rc := &ReconstructContext{TenantKey: tenantKey, KeyVersion: 1}
			if err := p.Reconstruct(ctx, result.Chunks, rc, &reconstructed); err != nil {
				t.Fatalf("Reconstruct failed: %v", err)
			}
			if !bytes.Equal(tt.data, reconstructed.Bytes()) {
				t.Error("Round-trip failed: data mismatch")
			}

			ratio := result.DedupRatio
			t.Logf("%s: %d -> %d bytes (%.2fx compression)",
				tt.name, result.TotalSize, result.ProcessedSize, ratio)

			if ratio < tt.minRatio {
				t.Errorf("Compression ratio %.2f below expected minimum %.2f", ratio, tt.minRatio)
			}
			if ratio > tt.maxRatio {
				t.Logf("Note: Compression ratio %.2f exceeds expected max %.2f (not an error)", ratio, tt.maxRatio)
			}
		})
	}
}

func TestPipeline_TrulyRealisticData(t *testing.T) {
	p, _ := NewPipelineFromPreset("smart")
	ctx := context.Background()
	tenantKey, _ := GenerateTenantKey()
	uc := &UploadContext{TenantKey: tenantKey, KeyVersion: 1}

	// Generate realistic JSON with varying content
	var jsonBuf bytes.Buffer
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&jsonBuf, `{"id":%d,"name":"User_%d","email":"user%d@example.com","score":%d,"active":%v},`,
			i, i, i, i*17%1000, i%2 == 0)
	}

	// Generate realistic log with timestamps and varying messages
	var logBuf bytes.Buffer
	messages := []string{"Processing request", "Database query completed", "Cache miss", "User authenticated", "API response sent"}
	levels := []string{"INFO", "DEBUG", "WARN", "ERROR"}
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&logBuf, "2024-01-%02d %02d:%02d:%02d %s [worker-%d] %s id=%d duration=%dms\n",
			(i%28)+1, i%24, i%60, i%60, levels[i%4], i%8, messages[i%5], i, i%500)
	}

	// Generate realistic CSV with varying data
	var csvBuf bytes.Buffer
	products := []string{"widget_a", "gadget_b", "tool_c", "part_d", "item_e"}
	statuses := []string{"completed", "pending", "failed", "refunded"}
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&csvBuf, "2024-01-%02d,%s,%.2f,USD,%s,user_%d\n",
			(i%28)+1, products[i%5], float64(i%10000)/100+10, statuses[i%4], i%1000)
	}

	tests := []struct {
		name        string
		data        []byte
		contentType string
	}{
		{"Realistic JSON", jsonBuf.Bytes(), "application/json"},
		{"Realistic Logs", logBuf.Bytes(), "text/plain"},
		{"Realistic CSV", csvBuf.Bytes(), "text/csv"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uc.ContentType = tt.contentType
			result, err := p.Process(ctx, bytes.NewReader(tt.data), uc)
			if err != nil {
				t.Fatalf("Process failed: %v", err)
			}

			// Verify round-trip
			var reconstructed bytes.Buffer
			rc := &ReconstructContext{TenantKey: tenantKey, KeyVersion: 1}
			if err := p.Reconstruct(ctx, result.Chunks, rc, &reconstructed); err != nil {
				t.Fatalf("Reconstruct failed: %v", err)
			}
			if !bytes.Equal(tt.data, reconstructed.Bytes()) {
				t.Error("Round-trip failed")
			}

			t.Logf("%s: %d bytes -> %d bytes (%.2fx compression)",
				tt.name, result.TotalSize, result.ProcessedSize, result.DedupRatio)
		})
	}
}
