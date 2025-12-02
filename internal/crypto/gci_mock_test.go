package crypto

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// MockGCI provides an in-memory implementation for testing without a database
type MockGCI struct {
	chunks     map[string]*GCIEntry
	tenantRefs map[string][]TenantChunkRef // key: "tenantID/bucket/object"
	objectMeta map[string]*ObjectMeta      // key: "tenantID/bucket/object"
	mu         sync.RWMutex
}

// NewMockGCI creates a new mock GCI for testing
func NewMockGCI() *MockGCI {
	return &MockGCI{
		chunks:     make(map[string]*GCIEntry),
		tenantRefs: make(map[string][]TenantChunkRef),
		objectMeta: make(map[string]*ObjectMeta),
	}
}

func (m *MockGCI) objectKey(tenantID uuid.UUID, bucket, key string) string {
	return fmt.Sprintf("%s/%s/%s", tenantID, bucket, key)
}

func (m *MockGCI) LookupChunk(_ context.Context, plaintextHash string) (*ChunkLookupResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, exists := m.chunks[plaintextHash]
	if !exists {
		return &ChunkLookupResult{
			Exists:     false,
			Entry:      nil,
			IsNewChunk: true,
		}, nil
	}

	return &ChunkLookupResult{
		Exists:     true,
		Entry:      entry,
		IsNewChunk: false,
	}, nil
}

func (m *MockGCI) LookupChunks(ctx context.Context, hashes []string) (map[string]*ChunkLookupResult, error) {
	results := make(map[string]*ChunkLookupResult)
	for _, hash := range hashes {
		result, _ := m.LookupChunk(ctx, hash)
		results[hash] = result
	}
	return results, nil
}

func (m *MockGCI) InsertChunk(_ context.Context, entry *GCIEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, exists := m.chunks[entry.PlaintextHash]; exists {
		existing.RefCount++
		return nil
	}

	// Copy entry
	newEntry := *entry
	m.chunks[entry.PlaintextHash] = &newEntry
	return nil
}

func (m *MockGCI) IncrementRef(_ context.Context, plaintextHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if entry, exists := m.chunks[plaintextHash]; exists {
		entry.RefCount++
	}
	return nil
}

func (m *MockGCI) DecrementRef(_ context.Context, plaintextHash string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if entry, exists := m.chunks[plaintextHash]; exists {
		entry.RefCount--
		return entry.RefCount, nil
	}
	return 0, nil
}

func (m *MockGCI) AddTenantChunkRef(_ context.Context, ref *TenantChunkRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := m.objectKey(ref.TenantID, ref.BucketName, ref.ObjectKey)
	m.tenantRefs[key] = append(m.tenantRefs[key], *ref)
	return nil
}

func (m *MockGCI) GetObjectChunks(_ context.Context, tenantID uuid.UUID, bucket, objectKey string) ([]TenantChunkRef, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	key := m.objectKey(tenantID, bucket, objectKey)
	refs := m.tenantRefs[key]

	// Return copy
	result := make([]TenantChunkRef, len(refs))
	copy(result, refs)
	return result, nil
}

func (m *MockGCI) SaveObjectMetadata(_ context.Context, meta *ObjectMeta) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := m.objectKey(meta.TenantID, meta.BucketName, meta.ObjectKey)
	m.objectMeta[key] = meta
	return nil
}

func (m *MockGCI) GetObjectMetadata(_ context.Context, tenantID uuid.UUID, bucket, objectKey string) (*ObjectMeta, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	key := m.objectKey(tenantID, bucket, objectKey)
	return m.objectMeta[key], nil
}

// Tests using MockGCI

func TestMockGCI_LookupChunk_NotFound(t *testing.T) {
	gci := NewMockGCI()
	ctx := context.Background()

	result, err := gci.LookupChunk(ctx, "nonexistent_hash")
	if err != nil {
		t.Fatalf("LookupChunk failed: %v", err)
	}

	if result.Exists {
		t.Error("Expected chunk to not exist")
	}
	if !result.IsNewChunk {
		t.Error("Expected IsNewChunk to be true")
	}
}

func TestMockGCI_InsertAndLookup(t *testing.T) {
	gci := NewMockGCI()
	ctx := context.Background()

	entry := &GCIEntry{
		PlaintextHash: "test_hash_123",
		BackendID:     "lyve-us-east",
		StorageKey:    "chunks/test/123",
		SizeBytes:     4 * 1024 * 1024,
		RefCount:      1,
	}

	// Insert
	if err := gci.InsertChunk(ctx, entry); err != nil {
		t.Fatalf("InsertChunk failed: %v", err)
	}

	// Lookup
	result, err := gci.LookupChunk(ctx, entry.PlaintextHash)
	if err != nil {
		t.Fatalf("LookupChunk failed: %v", err)
	}

	if !result.Exists {
		t.Error("Expected chunk to exist")
	}
	if result.Entry.BackendID != entry.BackendID {
		t.Errorf("BackendID = %s, want %s", result.Entry.BackendID, entry.BackendID)
	}
}

func TestMockGCI_DuplicateIncrementsRefCount(t *testing.T) {
	gci := NewMockGCI()
	ctx := context.Background()

	entry := &GCIEntry{
		PlaintextHash: "duplicate_hash",
		BackendID:     "lyve-us-east",
		StorageKey:    "chunks/dup/test",
		SizeBytes:     1024,
		RefCount:      1,
	}

	// Insert twice
	_ = gci.InsertChunk(ctx, entry)
	_ = gci.InsertChunk(ctx, entry)

	// Check ref count
	result, _ := gci.LookupChunk(ctx, entry.PlaintextHash)
	if result.Entry.RefCount != 2 {
		t.Errorf("RefCount = %d, want 2", result.Entry.RefCount)
	}
}

func TestMockGCI_BatchLookup(t *testing.T) {
	gci := NewMockGCI()
	ctx := context.Background()

	// Insert 2 chunks
	hashes := []string{"hash_1", "hash_2", "hash_3"}
	for _, hash := range hashes[:2] {
		_ = gci.InsertChunk(ctx, &GCIEntry{
			PlaintextHash: hash,
			BackendID:     "test",
			StorageKey:    "test/" + hash,
			SizeBytes:     1024,
			RefCount:      1,
		})
	}

	// Batch lookup all 3
	results, err := gci.LookupChunks(ctx, hashes)
	if err != nil {
		t.Fatalf("LookupChunks failed: %v", err)
	}

	if !results["hash_1"].Exists {
		t.Error("hash_1 should exist")
	}
	if !results["hash_2"].Exists {
		t.Error("hash_2 should exist")
	}
	if results["hash_3"].Exists {
		t.Error("hash_3 should not exist")
	}
}

func TestMockGCI_RefCounting(t *testing.T) {
	gci := NewMockGCI()
	ctx := context.Background()

	hash := "refcount_hash"
	_ = gci.InsertChunk(ctx, &GCIEntry{
		PlaintextHash: hash,
		BackendID:     "test",
		StorageKey:    "test/ref",
		SizeBytes:     1024,
		RefCount:      1,
	})

	// Increment
	_ = gci.IncrementRef(ctx, hash)
	result, _ := gci.LookupChunk(ctx, hash)
	if result.Entry.RefCount != 2 {
		t.Errorf("After increment: RefCount = %d, want 2", result.Entry.RefCount)
	}

	// Decrement
	newCount, _ := gci.DecrementRef(ctx, hash)
	if newCount != 1 {
		t.Errorf("After decrement: RefCount = %d, want 1", newCount)
	}
}

func TestMockGCI_TenantChunkRefs(t *testing.T) {
	gci := NewMockGCI()
	ctx := context.Background()

	tenantID := uuid.New()
	bucket := "test-bucket"
	objectKey := "file.txt"

	// Add refs
	refs := []TenantChunkRef{
		{TenantID: tenantID, BucketName: bucket, ObjectKey: objectKey, ChunkIndex: 0, PlaintextHash: "hash_0"},
		{TenantID: tenantID, BucketName: bucket, ObjectKey: objectKey, ChunkIndex: 1, PlaintextHash: "hash_1"},
	}

	for _, ref := range refs {
		if err := gci.AddTenantChunkRef(ctx, &ref); err != nil {
			t.Fatalf("AddTenantChunkRef failed: %v", err)
		}
	}

	// Retrieve
	retrieved, err := gci.GetObjectChunks(ctx, tenantID, bucket, objectKey)
	if err != nil {
		t.Fatalf("GetObjectChunks failed: %v", err)
	}

	if len(retrieved) != 2 {
		t.Fatalf("Expected 2 refs, got %d", len(retrieved))
	}
}

func TestMockGCI_ObjectMetadata(t *testing.T) {
	gci := NewMockGCI()
	ctx := context.Background()

	tenantID := uuid.New()
	physicalSize := int64(800)
	meta := &ObjectMeta{
		TenantID:     tenantID,
		BucketName:   "bucket",
		ObjectKey:    "file.pdf",
		TotalSize:    1000,
		ChunkCount:   2,
		LogicalSize:  1000,
		PhysicalSize: &physicalSize,
	}

	// Save
	if err := gci.SaveObjectMetadata(ctx, meta); err != nil {
		t.Fatalf("SaveObjectMetadata failed: %v", err)
	}

	// Retrieve
	retrieved, err := gci.GetObjectMetadata(ctx, tenantID, "bucket", "file.pdf")
	if err != nil {
		t.Fatalf("GetObjectMetadata failed: %v", err)
	}

	if retrieved == nil {
		t.Fatal("Expected metadata to exist")
	}
	if retrieved.TotalSize != 1000 {
		t.Errorf("TotalSize = %d, want 1000", retrieved.TotalSize)
	}
	if *retrieved.PhysicalSize != 800 {
		t.Errorf("PhysicalSize = %d, want 800", *retrieved.PhysicalSize)
	}
}

// Test deduplication scenario
func TestMockGCI_DeduplicationScenario(t *testing.T) {
	gci := NewMockGCI()
	ctx := context.Background()

	// Simulate: Two tenants upload the same file
	tenant1 := uuid.New()
	tenant2 := uuid.New()

	// Common chunk hashes (same content)
	chunks := []struct {
		hash string
		size int64
	}{
		{"shared_chunk_1", 4 * 1024 * 1024},
		{"shared_chunk_2", 4 * 1024 * 1024},
		{"shared_chunk_3", 2 * 1024 * 1024}, // Last chunk smaller
	}

	// Tenant 1 uploads first
	for i, c := range chunks {
		// Check if exists
		result, _ := gci.LookupChunk(ctx, c.hash)

		if result.IsNewChunk {
			// New chunk - store it
			_ = gci.InsertChunk(ctx, &GCIEntry{
				PlaintextHash: c.hash,
				BackendID:     "lyve-us-east",
				StorageKey:    "chunks/" + c.hash,
				SizeBytes:     c.size,
				RefCount:      1,
			})
		} else {
			// Existing chunk - just increment ref
			_ = gci.IncrementRef(ctx, c.hash)
		}

		// Add tenant ref
		_ = gci.AddTenantChunkRef(ctx, &TenantChunkRef{
			TenantID:      tenant1,
			BucketName:    "photos",
			ObjectKey:     "vacation.zip",
			ChunkIndex:    i,
			PlaintextHash: c.hash,
		})
	}

	// Tenant 2 uploads same file
	for i, c := range chunks {
		result, _ := gci.LookupChunk(ctx, c.hash)

		if result.IsNewChunk {
			_ = gci.InsertChunk(ctx, &GCIEntry{
				PlaintextHash: c.hash,
				BackendID:     "lyve-us-east",
				StorageKey:    "chunks/" + c.hash,
				SizeBytes:     c.size,
				RefCount:      1,
			})
		} else {
			// Should hit this path - chunks already exist!
			_ = gci.IncrementRef(ctx, c.hash)
		}

		_ = gci.AddTenantChunkRef(ctx, &TenantChunkRef{
			TenantID:      tenant2,
			BucketName:    "backup",
			ObjectKey:     "same-vacation.zip",
			ChunkIndex:    i,
			PlaintextHash: c.hash,
		})
	}

	// Verify ref counts are 2 for all chunks
	for _, c := range chunks {
		result, _ := gci.LookupChunk(ctx, c.hash)
		if result.Entry.RefCount != 2 {
			t.Errorf("Chunk %s RefCount = %d, want 2", c.hash, result.Entry.RefCount)
		}
	}

	// Calculate storage savings
	var logicalSize, physicalSize int64
	for _, c := range chunks {
		physicalSize += c.size    // Stored once
		logicalSize += c.size * 2 // Referenced twice
	}

	expectedSavings := logicalSize - physicalSize
	expectedRatio := float64(logicalSize) / float64(physicalSize)

	t.Logf("Dedup scenario results:")
	t.Logf("  Logical size:  %d bytes (what users see)", logicalSize)
	t.Logf("  Physical size: %d bytes (what you store)", physicalSize)
	t.Logf("  Savings:       %d bytes (%.1f%%)", expectedSavings, float64(expectedSavings)/float64(logicalSize)*100)
	t.Logf("  Dedup ratio:   %.2fx", expectedRatio)

	if expectedRatio != 2.0 {
		t.Errorf("Expected 2.0x dedup ratio, got %.2fx", expectedRatio)
	}
}

// Benchmark
func BenchmarkMockGCI_LookupChunk(b *testing.B) {
	gci := NewMockGCI()
	ctx := context.Background()

	// Pre-populate
	for i := 0; i < 10000; i++ {
		hash := fmt.Sprintf("hash_%d", i)
		_ = gci.InsertChunk(ctx, &GCIEntry{
			PlaintextHash: hash,
			BackendID:     "test",
			StorageKey:    "test/" + hash,
			SizeBytes:     1024,
			RefCount:      1,
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hash := fmt.Sprintf("hash_%d", i%10000)
		_, _ = gci.LookupChunk(ctx, hash)
	}
}
