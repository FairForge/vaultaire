package crypto

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// getTestDB connects to the test database the rest of the suite uses
// (internal/testutil: vaultaire_test by default, DATABASE_URL wins — CI sets
// it). The schema comes from the real migrations (`make test-db`), never from
// a simplified copy: this file used to carry its own pre-054 schema and a
// fallback DSN with a role that existed nowhere, so its ten tests had never
// run (R8-13 / R0-18).
func getTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx), "GCI tests need the test database (make test-db)")
	return db
}

// testScope gives each test its own dedup_scope (and tenant) so the suite can
// run in parallel with the internal/api dedup tests against the same
// database. Cleanup is scoped to exactly those keys — no table-wide DELETEs.
func testScope(t *testing.T, db *sql.DB) (scope, tenantID string) {
	t.Helper()
	scope = "gci-test-" + t.Name()
	tenantID = uuid.New().String()
	clean := func() {
		_, _ = db.Exec("DELETE FROM tenant_chunk_refs WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM object_metadata WHERE tenant_id = $1", tenantID)
		_, _ = db.Exec("DELETE FROM global_content_index WHERE dedup_scope = $1", scope)
	}
	clean()
	t.Cleanup(clean)
	return scope, tenantID
}

func TestGCI_LookupChunk_NotFound(t *testing.T) {
	db := getTestDB(t)
	scope, _ := testScope(t, db)

	gci := NewGlobalContentIndex(db)
	ctx := context.Background()

	result, err := gci.LookupChunk(ctx, scope, "nonexistent_hash_abc123")
	if err != nil {
		t.Fatalf("LookupChunk failed: %v", err)
	}

	if result.Exists {
		t.Error("Expected chunk to not exist")
	}
	if !result.IsNewChunk {
		t.Error("Expected IsNewChunk to be true")
	}
	if result.Entry != nil {
		t.Error("Expected Entry to be nil")
	}
}

func TestGCI_InsertAndLookup(t *testing.T) {
	db := getTestDB(t)
	scope, _ := testScope(t, db)

	gci := NewGlobalContentIndex(db)
	ctx := context.Background()

	// Insert a chunk
	entry := &GCIEntry{
		DedupScope:    scope,
		PlaintextHash: "abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
		BackendID:     "lyve-us-east",
		StorageKey:    "chunks/ab/cd/abc123def456",
		SizeBytes:     4 * 1024 * 1024, // 4MB
		RefCount:      1,
	}

	err := gci.InsertChunk(ctx, entry)
	if err != nil {
		t.Fatalf("InsertChunk failed: %v", err)
	}

	// Lookup the chunk
	result, err := gci.LookupChunk(ctx, scope, entry.PlaintextHash)
	if err != nil {
		t.Fatalf("LookupChunk failed: %v", err)
	}

	if !result.Exists {
		t.Error("Expected chunk to exist")
	}
	if result.IsNewChunk {
		t.Error("Expected IsNewChunk to be false")
	}
	if result.Entry == nil {
		t.Fatal("Expected Entry to not be nil")
		return
	}
	if result.Entry.BackendID != entry.BackendID {
		t.Errorf("BackendID = %s, want %s", result.Entry.BackendID, entry.BackendID)
	}
	if result.Entry.SizeBytes != entry.SizeBytes {
		t.Errorf("SizeBytes = %d, want %d", result.Entry.SizeBytes, entry.SizeBytes)
	}
}

func TestGCI_InsertDuplicate_IncrementsRefCount(t *testing.T) {
	db := getTestDB(t)
	scope, _ := testScope(t, db)

	gci := NewGlobalContentIndex(db)
	ctx := context.Background()

	hash := "duplicate_test_hash_123456789012345678901234567890123456789012"
	entry := &GCIEntry{
		DedupScope:    scope,
		PlaintextHash: hash,
		BackendID:     "lyve-us-east",
		StorageKey:    "chunks/du/pl/duplicate",
		SizeBytes:     1024,
		RefCount:      1,
	}

	// Insert first time
	err := gci.InsertChunk(ctx, entry)
	if err != nil {
		t.Fatalf("First InsertChunk failed: %v", err)
	}

	// Insert same hash again (simulating another tenant uploading same content)
	err = gci.InsertChunk(ctx, entry)
	if err != nil {
		t.Fatalf("Second InsertChunk failed: %v", err)
	}

	// Clear cache to force DB lookup
	gci.cache.clear()

	// Check ref count
	result, err := gci.LookupChunk(ctx, scope, hash)
	if err != nil {
		t.Fatalf("LookupChunk failed: %v", err)
	}

	if result.Entry.RefCount != 2 {
		t.Errorf("RefCount = %d, want 2", result.Entry.RefCount)
	}
}

func TestGCI_BatchLookup(t *testing.T) {
	db := getTestDB(t)
	scope, _ := testScope(t, db)

	gci := NewGlobalContentIndex(db)
	ctx := context.Background()

	// Insert some chunks
	hashes := []string{
		"batch_hash_1_234567890123456789012345678901234567890123456789012",
		"batch_hash_2_234567890123456789012345678901234567890123456789012",
		"batch_hash_3_234567890123456789012345678901234567890123456789012",
	}

	for i, hash := range hashes[:2] { // Only insert first 2
		entry := &GCIEntry{
			DedupScope:    scope,
			PlaintextHash: hash,
			BackendID:     "lyve-us-east",
			StorageKey:    fmt.Sprintf("chunks/batch/%d", i),
			SizeBytes:     1024,
			RefCount:      1,
		}
		if err := gci.InsertChunk(ctx, entry); err != nil {
			t.Fatalf("InsertChunk failed: %v", err)
		}
	}

	// Clear cache
	gci.cache.clear()

	// Batch lookup all 3 (2 exist, 1 doesn't)
	results, err := gci.LookupChunks(ctx, scope, hashes)
	if err != nil {
		t.Fatalf("LookupChunks failed: %v", err)
	}

	if len(results) != 3 {
		t.Errorf("Expected 3 results, got %d", len(results))
	}

	// First 2 should exist
	for _, hash := range hashes[:2] {
		if !results[hash].Exists {
			t.Errorf("Expected %s to exist", hash)
		}
	}

	// Third should not exist
	if results[hashes[2]].Exists {
		t.Errorf("Expected %s to not exist", hashes[2])
	}
	if !results[hashes[2]].IsNewChunk {
		t.Error("Expected IsNewChunk to be true for non-existent chunk")
	}
}

func TestGCI_RefCounting(t *testing.T) {
	db := getTestDB(t)
	scope, _ := testScope(t, db)

	gci := NewGlobalContentIndex(db)
	ctx := context.Background()

	hash := "refcount_test_hash_12345678901234567890123456789012345678901234"
	entry := &GCIEntry{
		DedupScope:    scope,
		PlaintextHash: hash,
		BackendID:     "lyve-us-east",
		StorageKey:    "chunks/ref/test",
		SizeBytes:     1024,
		RefCount:      1,
	}

	// Insert
	if err := gci.InsertChunk(ctx, entry); err != nil {
		t.Fatalf("InsertChunk failed: %v", err)
	}

	// Increment ref
	rows, err := gci.IncrementRef(ctx, scope, hash)
	if err != nil {
		t.Fatalf("IncrementRef failed: %v", err)
	}
	if rows != 1 {
		t.Fatalf("IncrementRef rows = %d, want 1", rows)
	}

	// Check ref count is 2
	gci.cache.clear()
	result, _ := gci.LookupChunk(ctx, scope, hash)
	if result.Entry.RefCount != 2 {
		t.Errorf("RefCount after increment = %d, want 2", result.Entry.RefCount)
	}

	// Decrement ref
	newCount, err := gci.DecrementRef(ctx, scope, hash)
	if err != nil {
		t.Fatalf("DecrementRef failed: %v", err)
	}
	if newCount != 1 {
		t.Errorf("DecrementRef returned %d, want 1", newCount)
	}

	// Decrement again - should hit 0 and mark for deletion
	newCount, err = gci.DecrementRef(ctx, scope, hash)
	if err != nil {
		t.Fatalf("Second DecrementRef failed: %v", err)
	}
	if newCount != 0 {
		t.Errorf("Second DecrementRef returned %d, want 0", newCount)
	}

	// Check marked for deletion
	var markedForDeletion bool
	err = db.QueryRow("SELECT marked_for_deletion FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2", scope, hash).Scan(&markedForDeletion)
	if err != nil {
		t.Fatalf("Failed to check marked_for_deletion: %v", err)
	}
	if !markedForDeletion {
		t.Error("Expected chunk to be marked for deletion")
	}
}

func TestGCI_TenantChunkRefs(t *testing.T) {
	db := getTestDB(t)
	scope, tenantID := testScope(t, db)

	gci := NewGlobalContentIndex(db)
	ctx := context.Background()

	bucket := "test-bucket"
	objectKey := "path/to/file.txt"

	// First insert the chunk into GCI
	hash1 := "tenant_chunk_hash_1_23456789012345678901234567890123456789012"
	hash2 := "tenant_chunk_hash_2_23456789012345678901234567890123456789012"

	for _, hash := range []string{hash1, hash2} {
		entry := &GCIEntry{
			DedupScope:    scope,
			PlaintextHash: hash,
			BackendID:     "lyve-us-east",
			StorageKey:    "chunks/" + hash[:8],
			SizeBytes:     1024,
			RefCount:      1,
		}
		if err := gci.InsertChunk(ctx, entry); err != nil {
			t.Fatalf("InsertChunk failed: %v", err)
		}
	}

	// Add tenant chunk refs
	refs := []TenantChunkRef{
		{
			TenantID:             tenantID,
			BucketName:           bucket,
			ObjectKey:            objectKey,
			ChunkIndex:           0,
			ChunkOffset:          0,
			PlaintextHash:        hash1,
			DedupScope:           scope,
			EncryptionKeyVersion: 1,
		},
		{
			TenantID:             tenantID,
			BucketName:           bucket,
			ObjectKey:            objectKey,
			ChunkIndex:           1,
			ChunkOffset:          1024,
			PlaintextHash:        hash2,
			DedupScope:           scope,
			EncryptionKeyVersion: 1,
		},
	}

	for _, ref := range refs {
		if err := gci.AddTenantChunkRef(ctx, &ref); err != nil {
			t.Fatalf("AddTenantChunkRef failed: %v", err)
		}
	}

	// Retrieve chunks
	retrievedRefs, err := gci.GetObjectChunks(ctx, tenantID, bucket, objectKey)
	if err != nil {
		t.Fatalf("GetObjectChunks failed: %v", err)
	}

	if len(retrievedRefs) != 2 {
		t.Fatalf("Expected 2 chunk refs, got %d", len(retrievedRefs))
	}

	// Verify order and content
	if retrievedRefs[0].ChunkIndex != 0 {
		t.Errorf("First chunk index = %d, want 0", retrievedRefs[0].ChunkIndex)
	}
	if retrievedRefs[1].ChunkIndex != 1 {
		t.Errorf("Second chunk index = %d, want 1", retrievedRefs[1].ChunkIndex)
	}
	if retrievedRefs[0].PlaintextHash != hash1 {
		t.Errorf("First chunk hash = %s, want %s", retrievedRefs[0].PlaintextHash, hash1)
	}
}

func TestGCI_ObjectMetadata(t *testing.T) {
	db := getTestDB(t)
	_, tenantID := testScope(t, db)

	gci := NewGlobalContentIndex(db)
	ctx := context.Background()

	bucket := "test-bucket"
	objectKey := "path/to/document.pdf"

	contentType := "application/pdf"
	physicalSize := int64(800 * 1024) // 800KB after dedup
	dedupRatio := float32(1.25)       // 25% space savings

	meta := &ObjectMeta{
		TenantID:     tenantID,
		BucketName:   bucket,
		ObjectKey:    objectKey,
		TotalSize:    1024 * 1024, // 1MB
		ChunkCount:   2,
		ContentType:  &contentType,
		LogicalSize:  1024 * 1024,
		PhysicalSize: &physicalSize,
		DedupRatio:   &dedupRatio,
	}

	// Save metadata
	if err := gci.SaveObjectMetadata(ctx, meta); err != nil {
		t.Fatalf("SaveObjectMetadata failed: %v", err)
	}

	// Retrieve metadata
	retrieved, err := gci.GetObjectMetadata(ctx, tenantID, bucket, objectKey)
	if err != nil {
		t.Fatalf("GetObjectMetadata failed: %v", err)
	}

	if retrieved == nil {
		t.Fatal("Expected metadata to be found")
		return
	}

	if retrieved.TotalSize != meta.TotalSize {
		t.Errorf("TotalSize = %d, want %d", retrieved.TotalSize, meta.TotalSize)
	}
	if retrieved.ChunkCount != meta.ChunkCount {
		t.Errorf("ChunkCount = %d, want %d", retrieved.ChunkCount, meta.ChunkCount)
	}
	if retrieved.ContentType == nil || *retrieved.ContentType != contentType {
		t.Errorf("ContentType mismatch")
	}
	if retrieved.PhysicalSize == nil || *retrieved.PhysicalSize != physicalSize {
		t.Errorf("PhysicalSize mismatch")
	}
}

func TestGCI_DeleteObjectChunks(t *testing.T) {
	db := getTestDB(t)
	scope, tenantID := testScope(t, db)

	gci := NewGlobalContentIndex(db)
	ctx := context.Background()

	bucket := "delete-test-bucket"
	objectKey := "to-be-deleted.txt"

	// Insert chunk
	hash := "delete_test_hash_123456789012345678901234567890123456789012345"
	entry := &GCIEntry{
		DedupScope:    scope,
		PlaintextHash: hash,
		BackendID:     "lyve-us-east",
		StorageKey:    "chunks/delete/test",
		SizeBytes:     1024,
		RefCount:      1,
	}
	if err := gci.InsertChunk(ctx, entry); err != nil {
		t.Fatalf("InsertChunk failed: %v", err)
	}

	// Add tenant ref
	ref := &TenantChunkRef{
		TenantID:             tenantID,
		BucketName:           bucket,
		ObjectKey:            objectKey,
		ChunkIndex:           0,
		ChunkOffset:          0,
		PlaintextHash:        hash,
		DedupScope:           scope,
		EncryptionKeyVersion: 1,
	}
	if err := gci.AddTenantChunkRef(ctx, ref); err != nil {
		t.Fatalf("AddTenantChunkRef failed: %v", err)
	}

	// Save object metadata
	meta := &ObjectMeta{
		TenantID:    tenantID,
		BucketName:  bucket,
		ObjectKey:   objectKey,
		TotalSize:   1024,
		ChunkCount:  1,
		LogicalSize: 1024,
	}
	if err := gci.SaveObjectMetadata(ctx, meta); err != nil {
		t.Fatalf("SaveObjectMetadata failed: %v", err)
	}

	// Delete object chunks
	if err := gci.DeleteObjectChunks(ctx, tenantID, bucket, objectKey); err != nil {
		t.Fatalf("DeleteObjectChunks failed: %v", err)
	}

	// Verify tenant refs deleted
	refs, err := gci.GetObjectChunks(ctx, tenantID, bucket, objectKey)
	if err != nil {
		t.Fatalf("GetObjectChunks failed: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("Expected 0 refs after delete, got %d", len(refs))
	}

	// Verify object metadata deleted
	retrieved, err := gci.GetObjectMetadata(ctx, tenantID, bucket, objectKey)
	if err != nil {
		t.Fatalf("GetObjectMetadata failed: %v", err)
	}
	if retrieved != nil {
		t.Error("Expected metadata to be deleted")
	}

	// Verify chunk ref count decremented and marked for deletion
	gci.cache.clear()
	result, _ := gci.LookupChunk(ctx, scope, hash)
	if result.Entry.RefCount != 0 {
		t.Errorf("RefCount = %d, want 0", result.Entry.RefCount)
	}
}

func TestGCI_Cache(t *testing.T) {
	db := getTestDB(t)
	scope, _ := testScope(t, db)

	gci := NewGlobalContentIndex(db)
	ctx := context.Background()

	hash := "cache_test_hash_1234567890123456789012345678901234567890123456"
	entry := &GCIEntry{
		DedupScope:    scope,
		PlaintextHash: hash,
		BackendID:     "lyve-us-east",
		StorageKey:    "chunks/cache/test",
		SizeBytes:     2048,
		RefCount:      1,
	}

	// Insert (should cache)
	if err := gci.InsertChunk(ctx, entry); err != nil {
		t.Fatalf("InsertChunk failed: %v", err)
	}

	// Verify in cache
	cached := gci.cache.get(cacheKey(scope, hash))
	if cached == nil {
		t.Fatal("Expected chunk to be cached")
		return
	}
	if cached.SizeBytes != 2048 {
		t.Errorf("Cached SizeBytes = %d, want 2048", cached.SizeBytes)
	}

	// Lookup should use cache (won't hit DB)
	result, err := gci.LookupChunk(ctx, scope, hash)
	if err != nil {
		t.Fatalf("LookupChunk failed: %v", err)
	}
	if !result.Exists {
		t.Error("Expected chunk to exist from cache")
	}

	// Clear cache and lookup again (will hit DB)
	gci.cache.clear()
	result, err = gci.LookupChunk(ctx, scope, hash)
	if err != nil {
		t.Fatalf("LookupChunk after cache clear failed: %v", err)
	}
	if !result.Exists {
		t.Error("Expected chunk to exist from DB")
	}

	// Should be cached again
	cached = gci.cache.get(cacheKey(scope, hash))
	if cached == nil {
		t.Error("Expected chunk to be re-cached after DB lookup")
	}
}

// TestGCI_CacheEviction tests that cache evicts old entries when full
func TestGCI_CacheEviction(t *testing.T) {
	cache := &gciCache{
		entries: make(map[string]*GCIEntry),
		maxSize: 10, // Small size for testing
	}

	// Fill cache beyond max
	for i := 0; i < 15; i++ {
		hash := fmt.Sprintf("hash_%02d_567890123456789012345678901234567890123456789012", i)
		cache.set(hash, &GCIEntry{
			PlaintextHash: hash,
			SizeBytes:     1024,
		})
	}

	// Cache should have evicted some entries
	if len(cache.entries) > 10 {
		t.Errorf("Cache size = %d, expected <= 10", len(cache.entries))
	}
}

// Benchmark tests

func BenchmarkGCI_LookupChunk_Cached(b *testing.B) {
	cache := &gciCache{
		entries: make(map[string]*GCIEntry),
		maxSize: 100000,
	}

	hash := "benchmark_hash_12345678901234567890123456789012345678901234567"
	cache.set(hash, &GCIEntry{
		PlaintextHash: hash,
		BackendID:     "lyve-us-east",
		StorageKey:    "chunks/bench/test",
		SizeBytes:     4 * 1024 * 1024,
		RefCount:      1,
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cache.get(hash)
	}
}

func BenchmarkGCI_CacheSet(b *testing.B) {
	cache := &gciCache{
		entries: make(map[string]*GCIEntry),
		maxSize: 100000,
	}

	entry := &GCIEntry{
		PlaintextHash: "benchmark_hash",
		BackendID:     "lyve-us-east",
		StorageKey:    "chunks/bench/test",
		SizeBytes:     4 * 1024 * 1024,
		RefCount:      1,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hash := fmt.Sprintf("hash_%d", i)
		entry.PlaintextHash = hash
		cache.set(hash, entry)
	}
}
