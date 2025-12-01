-- Migration: 016_global_content_index.sql
-- Purpose: Global Content Index for cross-tenant deduplication
-- Step: 352

-- Global Content Index: stores unique chunks across all tenants
CREATE TABLE IF NOT EXISTS global_content_index (
    -- Primary key is the SHA-256 hash of plaintext chunk
    plaintext_hash VARCHAR(64) PRIMARY KEY,

    -- Storage location info
    backend_id VARCHAR(255) NOT NULL,           -- Which backend stores this chunk
    storage_key VARCHAR(512) NOT NULL,          -- Key/path in the backend

    -- Chunk metadata
    size_bytes BIGINT NOT NULL,                 -- Original chunk size (before compression)
    compressed_size BIGINT,                     -- Size after compression (NULL if not compressed)
    compression_algo VARCHAR(32),               -- 'zstd', 'lz4', etc.

    -- Reference counting for garbage collection
    ref_count INTEGER NOT NULL DEFAULT 1,       -- Number of tenant references

    -- Timestamps
    first_seen_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    last_accessed_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),

    -- For garbage collection
    marked_for_deletion BOOLEAN NOT NULL DEFAULT FALSE,
    marked_at TIMESTAMP WITH TIME ZONE
);

-- Index for finding chunks by backend (useful for backend migration)
CREATE INDEX IF NOT EXISTS idx_gci_backend ON global_content_index(backend_id);

-- Index for garbage collection (find unreferenced chunks)
CREATE INDEX IF NOT EXISTS idx_gci_ref_count ON global_content_index(ref_count) WHERE ref_count = 0;

-- Index for GC cleanup (find marked chunks older than grace period)
CREATE INDEX IF NOT EXISTS idx_gci_marked ON global_content_index(marked_at) WHERE marked_for_deletion = TRUE;

-- Index for access patterns (LRU eviction, hot data identification)
CREATE INDEX IF NOT EXISTS idx_gci_last_accessed ON global_content_index(last_accessed_at);

-- Tenant chunk references: maps tenant objects to global chunks
CREATE TABLE IF NOT EXISTS tenant_chunk_refs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Tenant and object identification
    tenant_id UUID NOT NULL,
    bucket_name VARCHAR(255) NOT NULL,
    object_key VARCHAR(1024) NOT NULL,

    -- Chunk position in the object
    chunk_index INTEGER NOT NULL,               -- 0-based index
    chunk_offset BIGINT NOT NULL,               -- Byte offset in original object

    -- Reference to global chunk (by plaintext hash)
    plaintext_hash VARCHAR(64) NOT NULL REFERENCES global_content_index(plaintext_hash),

    -- Per-tenant encryption (convergent encryption key derived from tenant key + content hash)
    -- We don't store the key, just metadata about it
    encryption_key_version INTEGER NOT NULL DEFAULT 1,
    ciphertext_hash VARCHAR(64),                -- Hash of encrypted chunk (for integrity verification)

    -- Timestamps
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),

    -- Composite unique constraint: one chunk per position per object
    UNIQUE(tenant_id, bucket_name, object_key, chunk_index)
);

-- Index for reconstructing objects (get all chunks for an object)
CREATE INDEX IF NOT EXISTS idx_tcr_object ON tenant_chunk_refs(tenant_id, bucket_name, object_key, chunk_index);

-- Index for finding all references to a chunk (for ref counting)
CREATE INDEX IF NOT EXISTS idx_tcr_hash ON tenant_chunk_refs(plaintext_hash);

-- Index for tenant cleanup (delete all chunks for a tenant)
CREATE INDEX IF NOT EXISTS idx_tcr_tenant ON tenant_chunk_refs(tenant_id);

-- Object metadata: stores object-level info (separate from chunks)
CREATE TABLE IF NOT EXISTS object_metadata (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Object identification
    tenant_id UUID NOT NULL,
    bucket_name VARCHAR(255) NOT NULL,
    object_key VARCHAR(1024) NOT NULL,

    -- Object properties
    total_size BIGINT NOT NULL,                 -- Original object size
    chunk_count INTEGER NOT NULL,               -- Number of chunks
    content_hash VARCHAR(64),                   -- Hash of entire object (optional)
    content_type VARCHAR(255),                  -- MIME type

    -- Storage efficiency metrics
    logical_size BIGINT NOT NULL,               -- What user sees (= total_size)
    physical_size BIGINT,                       -- Actual storage used (after dedup)
    dedup_ratio REAL,                           -- logical/physical ratio

    -- Pipeline config used
    pipeline_config JSONB,                      -- Snapshot of config at upload time

    -- Timestamps
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),

    -- Composite unique constraint
    UNIQUE(tenant_id, bucket_name, object_key)
);

-- Index for listing objects
CREATE INDEX IF NOT EXISTS idx_om_listing ON object_metadata(tenant_id, bucket_name, object_key);

-- Index for finding large objects (for tiering decisions)
CREATE INDEX IF NOT EXISTS idx_om_size ON object_metadata(total_size);

-- Dedup statistics: track dedup efficiency over time
CREATE TABLE IF NOT EXISTS dedup_statistics (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Scope
    tenant_id UUID,                             -- NULL for global stats

    -- Time bucket
    stat_date DATE NOT NULL,
    stat_hour INTEGER,                          -- 0-23, NULL for daily aggregates

    -- Metrics
    chunks_processed BIGINT NOT NULL DEFAULT 0,
    chunks_deduplicated BIGINT NOT NULL DEFAULT 0,  -- Chunks that already existed
    bytes_logical BIGINT NOT NULL DEFAULT 0,        -- Total bytes received
    bytes_physical BIGINT NOT NULL DEFAULT 0,       -- Bytes actually stored
    bytes_saved BIGINT NOT NULL DEFAULT 0,          -- bytes_logical - bytes_physical

    -- Compression stats
    bytes_before_compression BIGINT NOT NULL DEFAULT 0,
    bytes_after_compression BIGINT NOT NULL DEFAULT 0,

    -- Timestamps
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),

    -- One record per scope + time bucket
    UNIQUE(tenant_id, stat_date, stat_hour)
);

-- Index for querying stats by date
CREATE INDEX IF NOT EXISTS idx_ds_date ON dedup_statistics(stat_date);

-- Index for tenant-specific stats
CREATE INDEX IF NOT EXISTS idx_ds_tenant ON dedup_statistics(tenant_id) WHERE tenant_id IS NOT NULL;

-- Function to increment chunk reference count
CREATE OR REPLACE FUNCTION increment_chunk_ref(p_hash VARCHAR(64))
RETURNS VOID AS $$
BEGIN
    UPDATE global_content_index
    SET ref_count = ref_count + 1,
        last_accessed_at = NOW(),
        marked_for_deletion = FALSE,
        marked_at = NULL
    WHERE plaintext_hash = p_hash;
END;
$$ LANGUAGE plpgsql;

-- Function to decrement chunk reference count
CREATE OR REPLACE FUNCTION decrement_chunk_ref(p_hash VARCHAR(64))
RETURNS INTEGER AS $$
DECLARE
    new_count INTEGER;
BEGIN
    UPDATE global_content_index
    SET ref_count = ref_count - 1
    WHERE plaintext_hash = p_hash
    RETURNING ref_count INTO new_count;

    -- Mark for deletion if ref_count hits 0
    IF new_count = 0 THEN
        UPDATE global_content_index
        SET marked_for_deletion = TRUE,
            marked_at = NOW()
        WHERE plaintext_hash = p_hash;
    END IF;

    RETURN new_count;
END;
$$ LANGUAGE plpgsql;

-- Function to get dedup ratio for a tenant
CREATE OR REPLACE FUNCTION get_tenant_dedup_ratio(p_tenant_id UUID)
RETURNS TABLE(logical_bytes BIGINT, physical_bytes BIGINT, ratio REAL) AS $$
BEGIN
    RETURN QUERY
    SELECT
        COALESCE(SUM(om.logical_size), 0)::BIGINT as logical_bytes,
        COALESCE(SUM(om.physical_size), 0)::BIGINT as physical_bytes,
        CASE
            WHEN COALESCE(SUM(om.physical_size), 0) > 0
            THEN (SUM(om.logical_size)::REAL / SUM(om.physical_size)::REAL)
            ELSE 1.0
        END as ratio
    FROM object_metadata om
    WHERE om.tenant_id = p_tenant_id;
END;
$$ LANGUAGE plpgsql;
