-- Phase 8.3: Global Content Index tables for content-defined chunking + dedup

-- 1. Global Content Index — the dedup index
CREATE TABLE IF NOT EXISTS global_content_index (
    plaintext_hash VARCHAR(64) PRIMARY KEY,
    backend_id VARCHAR(255) NOT NULL,
    storage_key VARCHAR(512) NOT NULL,
    size_bytes BIGINT NOT NULL,
    compressed_size BIGINT,
    compression_algo VARCHAR(32),
    ref_count INTEGER NOT NULL DEFAULT 1,
    first_seen_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    last_accessed_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    marked_for_deletion BOOLEAN NOT NULL DEFAULT FALSE,
    marked_at TIMESTAMP WITH TIME ZONE
);

-- 2. Tenant chunk references — per-tenant chunk manifest
CREATE TABLE IF NOT EXISTS tenant_chunk_refs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    bucket_name VARCHAR(255) NOT NULL,
    object_key VARCHAR(1024) NOT NULL,
    chunk_index INTEGER NOT NULL,
    chunk_offset BIGINT NOT NULL,
    plaintext_hash VARCHAR(64) NOT NULL,
    encryption_key_version INTEGER NOT NULL DEFAULT 1,
    ciphertext_hash VARCHAR(64),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE(tenant_id, bucket_name, object_key, chunk_index)
);

-- 3. Object metadata — object-level pipeline metadata
CREATE TABLE IF NOT EXISTS object_metadata (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    bucket_name VARCHAR(255) NOT NULL,
    object_key VARCHAR(1024) NOT NULL,
    total_size BIGINT NOT NULL,
    chunk_count INTEGER NOT NULL,
    content_hash VARCHAR(64),
    content_type VARCHAR(255),
    logical_size BIGINT NOT NULL,
    physical_size BIGINT,
    dedup_ratio REAL,
    pipeline_config JSONB,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE(tenant_id, bucket_name, object_key)
);

-- 4. Helper functions

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

CREATE OR REPLACE FUNCTION decrement_chunk_ref(p_hash VARCHAR(64))
RETURNS INTEGER AS $$
DECLARE
    new_count INTEGER;
BEGIN
    UPDATE global_content_index
    SET ref_count = ref_count - 1
    WHERE plaintext_hash = p_hash
    RETURNING ref_count INTO new_count;

    IF new_count = 0 THEN
        UPDATE global_content_index
        SET marked_for_deletion = TRUE,
            marked_at = NOW()
        WHERE plaintext_hash = p_hash;
    END IF;

    RETURN new_count;
END;
$$ LANGUAGE plpgsql;

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

-- 5. Indexes
CREATE INDEX IF NOT EXISTS idx_tenant_chunk_refs_object
    ON tenant_chunk_refs (tenant_id, bucket_name, object_key);
CREATE INDEX IF NOT EXISTS idx_tenant_chunk_refs_hash
    ON tenant_chunk_refs (plaintext_hash);
CREATE INDEX IF NOT EXISTS idx_gci_marked_for_deletion
    ON global_content_index (marked_for_deletion) WHERE marked_for_deletion = TRUE;
CREATE INDEX IF NOT EXISTS idx_object_metadata_tenant_bucket
    ON object_metadata (tenant_id, bucket_name);

-- 6. Add is_chunked flag to object_head_cache
ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS is_chunked BOOLEAN NOT NULL DEFAULT FALSE;
