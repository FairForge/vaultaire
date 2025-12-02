package crypto

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// SimpleGCIMock is a simple GCI mock for backend integration tests
type SimpleGCIMock struct {
	chunks map[string]string // plaintextHash -> location
	refs   map[string]int    // plaintextHash -> refCount
	mu     sync.RWMutex
}

func NewSimpleGCIMock() *SimpleGCIMock {
	return &SimpleGCIMock{
		chunks: make(map[string]string),
		refs:   make(map[string]int),
	}
}

func (m *SimpleGCIMock) CheckChunk(ctx context.Context, plaintextHash []byte) (bool, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	location, exists := m.chunks[string(plaintextHash)]
	return exists, location, nil
}

func (m *SimpleGCIMock) RegisterChunk(ctx context.Context, plaintextHash []byte, location string, size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chunks[string(plaintextHash)] = location
	m.refs[string(plaintextHash)] = 1
	return nil
}

func (m *SimpleGCIMock) IncrementRef(ctx context.Context, plaintextHash []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refs[string(plaintextHash)]++
	return nil
}

// SimpleChunkFetcher implements ChunkFetcher for testing
type SimpleChunkFetcher struct {
	chunks map[string][]byte
}

func NewSimpleChunkFetcher() *SimpleChunkFetcher {
	return &SimpleChunkFetcher{
		chunks: make(map[string][]byte),
	}
}

func (m *SimpleChunkFetcher) Store(location string, data []byte) {
	m.chunks[location] = data
}

func (m *SimpleChunkFetcher) FetchChunk(ctx context.Context, location string) ([]byte, error) {
	data, ok := m.chunks[location]
	if !ok {
		return nil, fmt.Errorf("chunk not found: %s", location)
	}
	return data, nil
}

func TestNewProcessingBackend(t *testing.T) {
	masterKey, _ := GenerateMasterKey()
	keyManager, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})
	pipeline, _ := NewPipelineFromPreset("smart")

	pb, err := NewProcessingBackend(&ProcessingBackendConfig{
		Pipeline:   pipeline,
		KeyManager: keyManager,
	})
	if err != nil {
		t.Fatalf("NewProcessingBackend failed: %v", err)
	}
	if pb == nil {
		t.Fatal("ProcessingBackend is nil")
	}
}

func TestNewProcessingBackend_Validation(t *testing.T) {
	masterKey, _ := GenerateMasterKey()
	keyManager, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})
	pipeline, _ := NewPipelineFromPreset("smart")

	tests := []struct {
		name    string
		config  *ProcessingBackendConfig
		wantErr bool
	}{
		{"no pipeline", &ProcessingBackendConfig{KeyManager: keyManager}, true},
		{"no key manager", &ProcessingBackendConfig{Pipeline: pipeline}, true},
		{"valid", &ProcessingBackendConfig{Pipeline: pipeline, KeyManager: keyManager}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewProcessingBackend(tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestProcessForUpload_RoundTrip(t *testing.T) {
	// Setup
	masterKey, _ := GenerateMasterKey()
	keyManager, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})
	pipeline, _ := NewPipelineFromPreset("smart")
	chunkFetcher := NewSimpleChunkFetcher()

	pb, _ := NewProcessingBackend(&ProcessingBackendConfig{
		Pipeline:   pipeline,
		KeyManager: keyManager,
	})

	ctx := context.Background()
	tenantID := "test-tenant"
	objectKey := "test-file.txt"
	originalData := []byte("Hello, this is test data for the processing backend!")

	// Process for upload
	result, err := pb.ProcessForUpload(ctx, tenantID, objectKey, bytes.NewReader(originalData))
	if err != nil {
		t.Fatalf("ProcessForUpload failed: %v", err)
	}

	if len(result.Chunks) == 0 {
		t.Error("No chunks produced")
	}

	// Store chunks in mock fetcher
	for i, chunk := range result.Chunks {
		if chunk.IsNew && chunk.Data != nil {
			chunkFetcher.Store(result.Metadata.ChunkRefs[i].Location, chunk.Data)
		}
	}

	// Process for download
	reader, err := pb.ProcessForDownload(ctx, tenantID, &result.Metadata, chunkFetcher)
	if err != nil {
		t.Fatalf("ProcessForDownload failed: %v", err)
	}

	reconstructed, _ := io.ReadAll(reader)

	if !bytes.Equal(originalData, reconstructed) {
		t.Errorf("Data mismatch:\nOriginal: %s\nGot: %s", originalData, reconstructed)
	}
}

func TestProcessForUpload_LargeFile(t *testing.T) {
	masterKey, _ := GenerateMasterKey()
	keyManager, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})
	pipeline, _ := NewPipelineFromPreset("smart")
	chunkFetcher := NewSimpleChunkFetcher()

	pb, _ := NewProcessingBackend(&ProcessingBackendConfig{
		Pipeline:   pipeline,
		KeyManager: keyManager,
	})

	ctx := context.Background()

	// 500KB of compressible data
	originalData := bytes.Repeat([]byte("This is repeated test data. "), 20000)

	result, err := pb.ProcessForUpload(ctx, "tenant-1", "large-file.txt", bytes.NewReader(originalData))
	if err != nil {
		t.Fatalf("ProcessForUpload failed: %v", err)
	}

	t.Logf("Chunks: %d, Original: %d bytes, Stored: %d bytes",
		len(result.Chunks), result.Metadata.OriginalSize, result.Metadata.StoredSize)

	// Store and reconstruct
	for i, chunk := range result.Chunks {
		if chunk.IsNew && chunk.Data != nil {
			chunkFetcher.Store(result.Metadata.ChunkRefs[i].Location, chunk.Data)
		}
	}

	reader, _ := pb.ProcessForDownload(ctx, "tenant-1", &result.Metadata, chunkFetcher)
	reconstructed, _ := io.ReadAll(reader)

	if !bytes.Equal(originalData, reconstructed) {
		t.Errorf("Large file reconstruction failed: got %d bytes, want %d", len(reconstructed), len(originalData))
	}
}

func TestProcessForUpload_MultiTenant(t *testing.T) {
	masterKey, _ := GenerateMasterKey()
	keyManager, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})
	pipeline, _ := NewPipelineFromPreset("smart")
	chunkFetcher := NewSimpleChunkFetcher()

	pb, _ := NewProcessingBackend(&ProcessingBackendConfig{
		Pipeline:   pipeline,
		KeyManager: keyManager,
	})

	ctx := context.Background()
	data := []byte("Same data, different tenants")

	// Upload for tenant 1
	result1, _ := pb.ProcessForUpload(ctx, "tenant-1", "file.txt", bytes.NewReader(data))
	for i, chunk := range result1.Chunks {
		if chunk.IsNew {
			chunkFetcher.Store(result1.Metadata.ChunkRefs[i].Location, chunk.Data)
		}
	}

	// Upload for tenant 2
	result2, _ := pb.ProcessForUpload(ctx, "tenant-2", "file.txt", bytes.NewReader(data))
	for i, chunk := range result2.Chunks {
		if chunk.IsNew {
			chunkFetcher.Store(result2.Metadata.ChunkRefs[i].Location, chunk.Data)
		}
	}

	// Both should reconstruct correctly
	reader1, _ := pb.ProcessForDownload(ctx, "tenant-1", &result1.Metadata, chunkFetcher)
	data1, _ := io.ReadAll(reader1)

	reader2, _ := pb.ProcessForDownload(ctx, "tenant-2", &result2.Metadata, chunkFetcher)
	data2, _ := io.ReadAll(reader2)

	if !bytes.Equal(data, data1) || !bytes.Equal(data, data2) {
		t.Error("Multi-tenant data reconstruction failed")
	}

	// Encrypted data should be different (different tenant keys)
	if len(result1.Chunks) > 0 && len(result2.Chunks) > 0 {
		if bytes.Equal(result1.Chunks[0].Data, result2.Chunks[0].Data) {
			t.Error("Different tenants should have different encrypted data")
		}
	}
}

func TestGetStats(t *testing.T) {
	masterKey, _ := GenerateMasterKey()
	keyManager, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})
	pipeline, _ := NewPipelineFromPreset("smart")

	pb, _ := NewProcessingBackend(&ProcessingBackendConfig{
		Pipeline:   pipeline,
		KeyManager: keyManager,
	})

	ctx := context.Background()

	// Process some data
	data := strings.Repeat("Stats test data. ", 100)
	_, _ = pb.ProcessForUpload(ctx, "tenant", "file.txt", strings.NewReader(data))

	stats := pb.GetStats()
	if stats.BytesProcessed == 0 {
		t.Error("BytesProcessed should be > 0")
	}
	if stats.ChunksProcessed == 0 {
		t.Error("ChunksProcessed should be > 0")
	}

	t.Logf("Stats: bytes=%d, chunks=%d, ratio=%.2f",
		stats.BytesProcessed, stats.ChunksProcessed, stats.CompressionRatio)
}

func TestCalculateSavings(t *testing.T) {
	masterKey, _ := GenerateMasterKey()
	keyManager, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})
	pipeline, _ := NewPipelineFromPreset("smart")

	pb, _ := NewProcessingBackend(&ProcessingBackendConfig{
		Pipeline:   pipeline,
		KeyManager: keyManager,
	})

	ctx := context.Background()

	// Process compressible data
	data := strings.Repeat("Compressible repeated content! ", 1000)
	_, _ = pb.ProcessForUpload(ctx, "tenant", "file.txt", strings.NewReader(data))

	savings := pb.CalculateSavings()

	t.Logf("Savings: original=%d, stored=%d, saved=%d (%.1f%%)",
		savings.OriginalBytes, savings.StoredBytes, savings.SavedBytes, savings.SavingsPercent)

	if savings.OriginalBytes == 0 {
		t.Error("OriginalBytes should be > 0")
	}
}

func TestResetStats(t *testing.T) {
	masterKey, _ := GenerateMasterKey()
	keyManager, _ := NewKeyManager(&KeyManagerConfig{MasterKey: masterKey})
	pipeline, _ := NewPipelineFromPreset("smart")

	pb, _ := NewProcessingBackend(&ProcessingBackendConfig{
		Pipeline:   pipeline,
		KeyManager: keyManager,
	})

	ctx := context.Background()

	_, _ = pb.ProcessForUpload(ctx, "tenant", "file.txt", strings.NewReader("test data"))

	stats := pb.GetStats()
	if stats.BytesProcessed == 0 {
		t.Error("Stats should have data before reset")
	}

	pb.ResetStats()

	stats = pb.GetStats()
	if stats.BytesProcessed != 0 {
		t.Error("Stats should be zero after reset")
	}
}
