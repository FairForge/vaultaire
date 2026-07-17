package crypto

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// GlobalDedupScope is the dedup scope for unencrypted chunks: they are shared
// across all tenants (cross-tenant dedup). Encrypted chunks instead use their
// tenant's ID as the scope, so a chunk encrypted with one tenant's convergent
// key is never handed to another tenant. See migration 054.
const GlobalDedupScope = "_global"

// GCIEntry represents an entry in the Global Content Index
type GCIEntry struct {
	DedupScope      string    `json:"dedup_scope"`
	PlaintextHash   string    `json:"plaintext_hash"`
	BackendID       string    `json:"backend_id"`
	StorageKey      string    `json:"storage_key"`
	SizeBytes       int64     `json:"size_bytes"`
	CompressedSize  *int64    `json:"compressed_size,omitempty"`
	CompressionAlgo *string   `json:"compression_algo,omitempty"`
	Encrypted       bool      `json:"encrypted"`
	EncryptionAlgo  *string   `json:"encryption_algo,omitempty"`
	RefCount        int       `json:"ref_count"`
	FirstSeenAt     time.Time `json:"first_seen_at"`
	LastAccessedAt  time.Time `json:"last_accessed_at"`
}

// TenantChunkRef represents a tenant's reference to a global chunk
type TenantChunkRef struct {
	ID                   uuid.UUID `json:"id"`
	TenantID             uuid.UUID `json:"tenant_id"`
	BucketName           string    `json:"bucket_name"`
	ObjectKey            string    `json:"object_key"`
	ChunkIndex           int       `json:"chunk_index"`
	ChunkOffset          int64     `json:"chunk_offset"`
	PlaintextHash        string    `json:"plaintext_hash"`
	DedupScope           string    `json:"dedup_scope"`
	EncryptionKeyVersion int       `json:"encryption_key_version"`
	CiphertextHash       *string   `json:"ciphertext_hash,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
}

// ObjectMeta represents object-level metadata
type ObjectMeta struct {
	ID             uuid.UUID       `json:"id"`
	TenantID       uuid.UUID       `json:"tenant_id"`
	BucketName     string          `json:"bucket_name"`
	ObjectKey      string          `json:"object_key"`
	TotalSize      int64           `json:"total_size"`
	ChunkCount     int             `json:"chunk_count"`
	ContentHash    *string         `json:"content_hash,omitempty"`
	ContentType    *string         `json:"content_type,omitempty"`
	LogicalSize    int64           `json:"logical_size"`
	PhysicalSize   *int64          `json:"physical_size,omitempty"`
	DedupRatio     *float32        `json:"dedup_ratio,omitempty"`
	PipelineConfig *PipelineConfig `json:"pipeline_config,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// DedupStats holds deduplication statistics
type DedupStats struct {
	TenantID           *uuid.UUID `json:"tenant_id,omitempty"`
	ChunksProcessed    int64      `json:"chunks_processed"`
	ChunksDeduplicated int64      `json:"chunks_deduplicated"`
	BytesLogical       int64      `json:"bytes_logical"`
	BytesPhysical      int64      `json:"bytes_physical"`
	BytesSaved         int64      `json:"bytes_saved"`
	DedupRatio         float64    `json:"dedup_ratio"`
}

// ChunkLookupResult represents the result of looking up a chunk
type ChunkLookupResult struct {
	Exists     bool      // Whether the chunk exists in GCI
	Entry      *GCIEntry // The GCI entry if exists
	IsNewChunk bool      // True if this is a new chunk (needs to be stored)
}

// GlobalContentIndex manages the deduplication index
type GlobalContentIndex struct {
	db    *sql.DB
	cache *gciCache // In-memory cache for hot lookups
}

// gciCache provides fast in-memory lookups for hot chunks
type gciCache struct {
	entries map[string]*GCIEntry
	maxSize int
	mu      sync.RWMutex
}

// NewGlobalContentIndex creates a new GCI instance
func NewGlobalContentIndex(db *sql.DB) *GlobalContentIndex {
	return &GlobalContentIndex{
		db: db,
		cache: &gciCache{
			entries: make(map[string]*GCIEntry),
			maxSize: 100000, // Cache up to 100K hot chunks
		},
	}
}

// cacheKey namespaces a hash by its dedup scope so an unencrypted global chunk
// and a tenant's encrypted chunk with the same plaintext hash never alias.
func cacheKey(scope, plaintextHash string) string {
	return scope + "\x00" + plaintextHash
}

// LookupChunk checks if a chunk exists in the index within the given dedup
// scope (GlobalDedupScope for unencrypted chunks, the tenant ID for encrypted
// chunks).
func (g *GlobalContentIndex) LookupChunk(ctx context.Context, scope, plaintextHash string) (*ChunkLookupResult, error) {
	// Check cache first
	if entry := g.cache.get(cacheKey(scope, plaintextHash)); entry != nil {
		return &ChunkLookupResult{
			Exists:     true,
			Entry:      entry,
			IsNewChunk: false,
		}, nil
	}

	// Query database
	var entry GCIEntry
	var compressedSize sql.NullInt64
	var compressionAlgo sql.NullString
	var encryptionAlgo sql.NullString

	err := g.db.QueryRowContext(ctx, `
		SELECT dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes,
		       compressed_size, compression_algo, encrypted, encryption_algo,
		       ref_count, first_seen_at, last_accessed_at
		FROM global_content_index
		WHERE dedup_scope = $1 AND plaintext_hash = $2
	`, scope, plaintextHash).Scan(
		&entry.DedupScope,
		&entry.PlaintextHash,
		&entry.BackendID,
		&entry.StorageKey,
		&entry.SizeBytes,
		&compressedSize,
		&compressionAlgo,
		&entry.Encrypted,
		&encryptionAlgo,
		&entry.RefCount,
		&entry.FirstSeenAt,
		&entry.LastAccessedAt,
	)

	if err == sql.ErrNoRows {
		return &ChunkLookupResult{
			Exists:     false,
			Entry:      nil,
			IsNewChunk: true,
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to lookup chunk: %w", err)
	}

	if compressedSize.Valid {
		entry.CompressedSize = &compressedSize.Int64
	}
	if compressionAlgo.Valid {
		entry.CompressionAlgo = &compressionAlgo.String
	}
	if encryptionAlgo.Valid {
		entry.EncryptionAlgo = &encryptionAlgo.String
	}

	// Add to cache
	g.cache.set(cacheKey(scope, plaintextHash), &entry)

	return &ChunkLookupResult{
		Exists:     true,
		Entry:      &entry,
		IsNewChunk: false,
	}, nil
}

// LookupChunks performs a batch lookup for multiple chunks within one dedup
// scope (more efficient than per-chunk LookupChunk).
func (g *GlobalContentIndex) LookupChunks(ctx context.Context, scope string, hashes []string) (map[string]*ChunkLookupResult, error) {
	results := make(map[string]*ChunkLookupResult)
	var uncachedHashes []string

	// Check cache first
	for _, hash := range hashes {
		if entry := g.cache.get(cacheKey(scope, hash)); entry != nil {
			results[hash] = &ChunkLookupResult{
				Exists:     true,
				Entry:      entry,
				IsNewChunk: false,
			}
		} else {
			uncachedHashes = append(uncachedHashes, hash)
			// Default to not found (will be updated if found in DB)
			results[hash] = &ChunkLookupResult{
				Exists:     false,
				Entry:      nil,
				IsNewChunk: true,
			}
		}
	}

	if len(uncachedHashes) == 0 {
		return results, nil
	}

	// Query database for uncached hashes
	rows, err := g.db.QueryContext(ctx, `
		SELECT dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes,
		       compressed_size, compression_algo, encrypted, encryption_algo,
		       ref_count, first_seen_at, last_accessed_at
		FROM global_content_index
		WHERE dedup_scope = $1 AND plaintext_hash = ANY($2)
	`, scope, uncachedHashes)
	if err != nil {
		return nil, fmt.Errorf("failed to batch lookup chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var entry GCIEntry
		var compressedSize sql.NullInt64
		var compressionAlgo sql.NullString
		var encryptionAlgo sql.NullString

		err := rows.Scan(
			&entry.DedupScope,
			&entry.PlaintextHash,
			&entry.BackendID,
			&entry.StorageKey,
			&entry.SizeBytes,
			&compressedSize,
			&compressionAlgo,
			&entry.Encrypted,
			&encryptionAlgo,
			&entry.RefCount,
			&entry.FirstSeenAt,
			&entry.LastAccessedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan chunk row: %w", err)
		}

		if compressedSize.Valid {
			entry.CompressedSize = &compressedSize.Int64
		}
		if compressionAlgo.Valid {
			entry.CompressionAlgo = &compressionAlgo.String
		}
		if encryptionAlgo.Valid {
			entry.EncryptionAlgo = &encryptionAlgo.String
		}

		// Update result
		results[entry.PlaintextHash] = &ChunkLookupResult{
			Exists:     true,
			Entry:      &entry,
			IsNewChunk: false,
		}

		// Add to cache
		g.cache.set(cacheKey(entry.DedupScope, entry.PlaintextHash), &entry)
	}

	return results, rows.Err()
}

// InsertChunk adds a new chunk to the index. entry.DedupScope defaults to
// GlobalDedupScope when unset so callers storing unencrypted chunks need not
// set it explicitly.
func (g *GlobalContentIndex) InsertChunk(ctx context.Context, entry *GCIEntry) error {
	if entry.DedupScope == "" {
		entry.DedupScope = GlobalDedupScope
	}
	_, err := g.db.ExecContext(ctx, `
		INSERT INTO global_content_index
		(dedup_scope, plaintext_hash, backend_id, storage_key, size_bytes, compressed_size, compression_algo, encrypted, encryption_algo, ref_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (dedup_scope, plaintext_hash) DO UPDATE SET
			ref_count = global_content_index.ref_count + 1,
			last_accessed_at = NOW()
	`, entry.DedupScope, entry.PlaintextHash, entry.BackendID, entry.StorageKey, entry.SizeBytes,
		entry.CompressedSize, entry.CompressionAlgo, entry.Encrypted, entry.EncryptionAlgo, entry.RefCount)

	if err != nil {
		return fmt.Errorf("failed to insert chunk: %w", err)
	}

	// Update cache
	g.cache.set(cacheKey(entry.DedupScope, entry.PlaintextHash), entry)

	return nil
}

// IncrementRef increments the reference count for a chunk in the given scope.
func (g *GlobalContentIndex) IncrementRef(ctx context.Context, scope, plaintextHash string) error {
	_, err := g.db.ExecContext(ctx, `SELECT increment_chunk_ref($1, $2)`, scope, plaintextHash)
	if err != nil {
		return fmt.Errorf("failed to increment ref: %w", err)
	}

	// Invalidate cache entry (will be refreshed on next lookup)
	g.cache.delete(cacheKey(scope, plaintextHash))

	return nil
}

// DecrementRef decrements the reference count for a chunk in the given scope.
func (g *GlobalContentIndex) DecrementRef(ctx context.Context, scope, plaintextHash string) (int, error) {
	var newCount int
	err := g.db.QueryRowContext(ctx, `SELECT decrement_chunk_ref($1, $2)`, scope, plaintextHash).Scan(&newCount)
	if err != nil {
		return 0, fmt.Errorf("failed to decrement ref: %w", err)
	}

	// Invalidate cache entry
	g.cache.delete(cacheKey(scope, plaintextHash))

	return newCount, nil
}

// AddTenantChunkRef adds a tenant's reference to a chunk. ref.DedupScope
// defaults to GlobalDedupScope when unset.
func (g *GlobalContentIndex) AddTenantChunkRef(ctx context.Context, ref *TenantChunkRef) error {
	scope := ref.DedupScope
	if scope == "" {
		scope = GlobalDedupScope
	}
	_, err := g.db.ExecContext(ctx, `
		INSERT INTO tenant_chunk_refs
		(tenant_id, bucket_name, object_key, chunk_index, chunk_offset,
		 plaintext_hash, dedup_scope, encryption_key_version, ciphertext_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (tenant_id, bucket_name, object_key, chunk_index) DO UPDATE SET
			plaintext_hash = EXCLUDED.plaintext_hash,
			dedup_scope = EXCLUDED.dedup_scope,
			encryption_key_version = EXCLUDED.encryption_key_version,
			ciphertext_hash = EXCLUDED.ciphertext_hash
	`, ref.TenantID, ref.BucketName, ref.ObjectKey, ref.ChunkIndex,
		ref.ChunkOffset, ref.PlaintextHash, scope, ref.EncryptionKeyVersion, ref.CiphertextHash)

	if err != nil {
		return fmt.Errorf("failed to add tenant chunk ref: %w", err)
	}

	return nil
}

// GetObjectChunks retrieves all chunk references for an object
func (g *GlobalContentIndex) GetObjectChunks(ctx context.Context, tenantID uuid.UUID, bucket, key string) ([]TenantChunkRef, error) {
	rows, err := g.db.QueryContext(ctx, `
		SELECT id, tenant_id, bucket_name, object_key, chunk_index, chunk_offset,
		       plaintext_hash, dedup_scope, encryption_key_version, ciphertext_hash, created_at
		FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3
		ORDER BY chunk_index ASC
	`, tenantID, bucket, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get object chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var refs []TenantChunkRef
	for rows.Next() {
		var ref TenantChunkRef
		var ciphertextHash sql.NullString

		err := rows.Scan(
			&ref.ID, &ref.TenantID, &ref.BucketName, &ref.ObjectKey,
			&ref.ChunkIndex, &ref.ChunkOffset, &ref.PlaintextHash,
			&ref.DedupScope, &ref.EncryptionKeyVersion, &ciphertextHash, &ref.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan chunk ref: %w", err)
		}

		if ciphertextHash.Valid {
			ref.CiphertextHash = &ciphertextHash.String
		}

		refs = append(refs, ref)
	}

	return refs, rows.Err()
}

// DeleteObjectChunks removes all chunk references for an object and decrements ref counts
func (g *GlobalContentIndex) DeleteObjectChunks(ctx context.Context, tenantID uuid.UUID, bucket, key string) error {
	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Get all chunks (scope + hash) for this object.
	rows, err := tx.QueryContext(ctx, `
		SELECT dedup_scope, plaintext_hash FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3
	`, tenantID, bucket, key)
	if err != nil {
		return fmt.Errorf("failed to get chunk hashes: %w", err)
	}

	type scopedHash struct{ scope, hash string }
	var chunks []scopedHash
	for rows.Next() {
		var c scopedHash
		if err := rows.Scan(&c.scope, &c.hash); err != nil {
			_ = rows.Close()
			return fmt.Errorf("failed to scan hash: %w", err)
		}
		chunks = append(chunks, c)
	}
	_ = rows.Close()

	// Delete chunk references
	_, err = tx.ExecContext(ctx, `
		DELETE FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3
	`, tenantID, bucket, key)
	if err != nil {
		return fmt.Errorf("failed to delete chunk refs: %w", err)
	}

	// Decrement ref counts for each chunk in its own dedup scope.
	for _, c := range chunks {
		_, err = tx.ExecContext(ctx, `SELECT decrement_chunk_ref($1, $2)`, c.scope, c.hash)
		if err != nil {
			return fmt.Errorf("failed to decrement ref for %s: %w", c.hash, err)
		}
		g.cache.delete(cacheKey(c.scope, c.hash))
	}

	// Delete object metadata
	_, err = tx.ExecContext(ctx, `
		DELETE FROM object_metadata
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3
	`, tenantID, bucket, key)
	if err != nil {
		return fmt.Errorf("failed to delete object metadata: %w", err)
	}

	return tx.Commit()
}

// ReplaceObjectManifest atomically swaps an object's chunk manifest. In a single
// transaction it releases the previous version's chunk references (decrementing
// their global ref counts), installs the new manifest, and upserts the object
// metadata — so a reader never observes a mixed or partial manifest and an
// overwritten object's old chunks do not leak their references.
//
// The NEW chunks' data and global_content_index entries (InsertChunk/IncrementRef)
// must already be persisted before calling this. Callers should IncrementRef new
// chunks BEFORE this call so that a chunk shared between the old and new versions
// never transiently drops to ref_count 0 (which would mark it for deletion).
func (g *GlobalContentIndex) ReplaceObjectManifest(ctx context.Context, tenantID uuid.UUID, bucket, key string, newRefs []TenantChunkRef, meta *ObjectMeta) error {
	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin manifest swap: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Capture the previous version's chunks (scope + hash) so we can release them.
	rows, err := tx.QueryContext(ctx, `
		SELECT dedup_scope, plaintext_hash FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3
	`, tenantID, bucket, key)
	if err != nil {
		return fmt.Errorf("read previous manifest: %w", err)
	}
	type scopedHash struct{ scope, hash string }
	var oldChunks []scopedHash
	for rows.Next() {
		var c scopedHash
		if scanErr := rows.Scan(&c.scope, &c.hash); scanErr != nil {
			_ = rows.Close()
			return fmt.Errorf("scan previous hash: %w", scanErr)
		}
		oldChunks = append(oldChunks, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate previous manifest: %w", err)
	}

	// Remove the old manifest and release its chunk references.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM tenant_chunk_refs
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3
	`, tenantID, bucket, key); err != nil {
		return fmt.Errorf("delete previous manifest: %w", err)
	}
	for _, c := range oldChunks {
		if _, err := tx.ExecContext(ctx, `SELECT decrement_chunk_ref($1, $2)`, c.scope, c.hash); err != nil {
			return fmt.Errorf("release chunk %s: %w", c.hash, err)
		}
	}

	// Install the new manifest.
	for i := range newRefs {
		ref := &newRefs[i]
		scope := ref.DedupScope
		if scope == "" {
			scope = GlobalDedupScope
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tenant_chunk_refs
			(tenant_id, bucket_name, object_key, chunk_index, chunk_offset,
			 plaintext_hash, dedup_scope, encryption_key_version, ciphertext_hash)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (tenant_id, bucket_name, object_key, chunk_index) DO UPDATE SET
				chunk_offset = EXCLUDED.chunk_offset,
				plaintext_hash = EXCLUDED.plaintext_hash,
				dedup_scope = EXCLUDED.dedup_scope,
				encryption_key_version = EXCLUDED.encryption_key_version,
				ciphertext_hash = EXCLUDED.ciphertext_hash
		`, ref.TenantID, ref.BucketName, ref.ObjectKey, ref.ChunkIndex,
			ref.ChunkOffset, ref.PlaintextHash, scope, ref.EncryptionKeyVersion, ref.CiphertextHash); err != nil {
			return fmt.Errorf("insert chunk ref %d: %w", ref.ChunkIndex, err)
		}
	}

	// Upsert object metadata.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO object_metadata
		(tenant_id, bucket_name, object_key, total_size, chunk_count, content_hash,
		 content_type, logical_size, physical_size, dedup_ratio, pipeline_config)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (tenant_id, bucket_name, object_key) DO UPDATE SET
			total_size = EXCLUDED.total_size,
			chunk_count = EXCLUDED.chunk_count,
			content_hash = EXCLUDED.content_hash,
			content_type = EXCLUDED.content_type,
			logical_size = EXCLUDED.logical_size,
			physical_size = EXCLUDED.physical_size,
			dedup_ratio = EXCLUDED.dedup_ratio,
			pipeline_config = EXCLUDED.pipeline_config,
			updated_at = NOW()
	`, meta.TenantID, meta.BucketName, meta.ObjectKey, meta.TotalSize,
		meta.ChunkCount, meta.ContentHash, meta.ContentType, meta.LogicalSize,
		meta.PhysicalSize, meta.DedupRatio, meta.PipelineConfig); err != nil {
		return fmt.Errorf("save object metadata: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit manifest swap: %w", err)
	}

	// Released chunks' ref counts changed — drop their cache entries.
	for _, c := range oldChunks {
		g.cache.delete(cacheKey(c.scope, c.hash))
	}
	return nil
}

// SaveObjectMetadata saves or updates object metadata
func (g *GlobalContentIndex) SaveObjectMetadata(ctx context.Context, meta *ObjectMeta) error {
	_, err := g.db.ExecContext(ctx, `
		INSERT INTO object_metadata
		(tenant_id, bucket_name, object_key, total_size, chunk_count, content_hash,
		 content_type, logical_size, physical_size, dedup_ratio, pipeline_config)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (tenant_id, bucket_name, object_key) DO UPDATE SET
			total_size = EXCLUDED.total_size,
			chunk_count = EXCLUDED.chunk_count,
			content_hash = EXCLUDED.content_hash,
			content_type = EXCLUDED.content_type,
			logical_size = EXCLUDED.logical_size,
			physical_size = EXCLUDED.physical_size,
			dedup_ratio = EXCLUDED.dedup_ratio,
			pipeline_config = EXCLUDED.pipeline_config,
			updated_at = NOW()
	`, meta.TenantID, meta.BucketName, meta.ObjectKey, meta.TotalSize,
		meta.ChunkCount, meta.ContentHash, meta.ContentType, meta.LogicalSize,
		meta.PhysicalSize, meta.DedupRatio, meta.PipelineConfig)

	if err != nil {
		return fmt.Errorf("failed to save object metadata: %w", err)
	}

	return nil
}

// GetObjectMetadata retrieves object metadata
func (g *GlobalContentIndex) GetObjectMetadata(ctx context.Context, tenantID uuid.UUID, bucket, key string) (*ObjectMeta, error) {
	var meta ObjectMeta
	var contentHash, contentType sql.NullString
	var physicalSize sql.NullInt64
	var dedupRatio sql.NullFloat64

	err := g.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, bucket_name, object_key, total_size, chunk_count,
		       content_hash, content_type, logical_size, physical_size, dedup_ratio,
		       created_at, updated_at
		FROM object_metadata
		WHERE tenant_id = $1 AND bucket_name = $2 AND object_key = $3
	`, tenantID, bucket, key).Scan(
		&meta.ID, &meta.TenantID, &meta.BucketName, &meta.ObjectKey,
		&meta.TotalSize, &meta.ChunkCount, &contentHash, &contentType,
		&meta.LogicalSize, &physicalSize, &dedupRatio,
		&meta.CreatedAt, &meta.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get object metadata: %w", err)
	}

	if contentHash.Valid {
		meta.ContentHash = &contentHash.String
	}
	if contentType.Valid {
		meta.ContentType = &contentType.String
	}
	if physicalSize.Valid {
		meta.PhysicalSize = &physicalSize.Int64
	}
	if dedupRatio.Valid {
		ratio := float32(dedupRatio.Float64)
		meta.DedupRatio = &ratio
	}

	return &meta, nil
}

// GetTenantDedupStats retrieves deduplication statistics for a tenant
func (g *GlobalContentIndex) GetTenantDedupStats(ctx context.Context, tenantID uuid.UUID) (*DedupStats, error) {
	var stats DedupStats
	var logical, physical int64

	err := g.db.QueryRowContext(ctx, `SELECT * FROM get_tenant_dedup_ratio($1)`, tenantID).Scan(
		&logical, &physical, &stats.DedupRatio,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get tenant dedup stats: %w", err)
	}

	stats.TenantID = &tenantID
	stats.BytesLogical = logical
	stats.BytesPhysical = physical
	stats.BytesSaved = logical - physical

	return &stats, nil
}

// GetGlobalDedupStats retrieves global deduplication statistics
func (g *GlobalContentIndex) GetGlobalDedupStats(ctx context.Context) (*DedupStats, error) {
	var stats DedupStats

	err := g.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) as total_chunks,
			COALESCE(SUM(size_bytes), 0) as total_bytes,
			COALESCE(SUM(CASE WHEN ref_count > 1 THEN size_bytes * (ref_count - 1) ELSE 0 END), 0) as bytes_saved
		FROM global_content_index
	`).Scan(&stats.ChunksProcessed, &stats.BytesPhysical, &stats.BytesSaved)

	if err != nil {
		return nil, fmt.Errorf("failed to get global dedup stats: %w", err)
	}

	stats.BytesLogical = stats.BytesPhysical + stats.BytesSaved
	if stats.BytesPhysical > 0 {
		stats.DedupRatio = float64(stats.BytesLogical) / float64(stats.BytesPhysical)
	} else {
		stats.DedupRatio = 1.0
	}

	return &stats, nil
}

// CompressionStatsResult holds global compression statistics from the GCI.
type CompressionStatsResult struct {
	ChunksCompressed   int64   `json:"chunks_compressed"`
	ChunksUncompressed int64   `json:"chunks_uncompressed"`
	TotalPlaintext     int64   `json:"total_plaintext"`
	TotalStored        int64   `json:"total_stored"`
	BytesSaved         int64   `json:"bytes_saved"`
	CompressionRatio   float64 `json:"compression_ratio"`
}

// GetGlobalCompressionStats retrieves compression statistics across all chunks.
func (g *GlobalContentIndex) GetGlobalCompressionStats(ctx context.Context) (*CompressionStatsResult, error) {
	var stats CompressionStatsResult

	err := g.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE compression_algo IS NOT NULL) as chunks_compressed,
			COUNT(*) FILTER (WHERE compression_algo IS NULL) as chunks_uncompressed,
			COALESCE(SUM(size_bytes), 0) as total_plaintext,
			COALESCE(SUM(CASE WHEN compressed_size IS NOT NULL
				THEN compressed_size ELSE size_bytes END), 0) as total_stored,
			COALESCE(SUM(size_bytes) - SUM(CASE WHEN compressed_size IS NOT NULL
				THEN compressed_size ELSE size_bytes END), 0) as bytes_saved
		FROM global_content_index
	`).Scan(
		&stats.ChunksCompressed,
		&stats.ChunksUncompressed,
		&stats.TotalPlaintext,
		&stats.TotalStored,
		&stats.BytesSaved,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get compression stats: %w", err)
	}

	if stats.TotalStored > 0 {
		stats.CompressionRatio = float64(stats.TotalPlaintext) / float64(stats.TotalStored)
	} else {
		stats.CompressionRatio = 1.0
	}

	return &stats, nil
}

// Cache methods

func (c *gciCache) get(hash string) *GCIEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.entries[hash]
}

func (c *gciCache) set(hash string, entry *GCIEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Simple eviction: if at max, clear half
	if len(c.entries) >= c.maxSize {
		count := 0
		for k := range c.entries {
			delete(c.entries, k)
			count++
			if count >= c.maxSize/2 {
				break
			}
		}
	}

	c.entries[hash] = entry
}

func (c *gciCache) delete(hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, hash)
}

func (c *gciCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*GCIEntry)
}
