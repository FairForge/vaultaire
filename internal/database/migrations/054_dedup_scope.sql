-- WP-7: tenant-scoped deduplication for encrypted chunks.
--
-- Convergent per-chunk encryption keys are per-tenant, so identical plaintext
-- from two tenants yields DIFFERENT ciphertext. But the Global Content Index
-- keyed chunks by plaintext_hash alone, so with deterministic chunking the two
-- tenants would collapse onto one row: tenant B's manifest would point at the
-- chunk tenant A encrypted with A's key, and B's GET would fail GCM (data loss).
--
-- Fix: partition the index by a dedup scope. Encrypted chunks live in their
-- tenant's scope (scope = tenant UUID); unencrypted chunks stay globally shared
-- (scope = '_global') so cross-tenant dedup of plaintext still works. The scope
-- is recorded on each tenant_chunk_ref so GET/DELETE/GC resolve the right row.
-- Existing rows predate the encryption feature (ENCRYPTION_MASTER_KEY unset in
-- prod), so they are all unencrypted and default to '_global'.

ALTER TABLE global_content_index
    ADD COLUMN IF NOT EXISTS dedup_scope VARCHAR(64) NOT NULL DEFAULT '_global';
ALTER TABLE tenant_chunk_refs
    ADD COLUMN IF NOT EXISTS dedup_scope VARCHAR(64) NOT NULL DEFAULT '_global';

-- Promote the GCI primary key from (plaintext_hash) to
-- (dedup_scope, plaintext_hash), and re-point the tenant_chunk_refs foreign key
-- at the composite key. Guarded on the PK still being single-column, so
-- re-applying the migration is a no-op (and never re-locks these tables).
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_constraint c
        JOIN pg_index i ON i.indexrelid = c.conindid
        WHERE c.conname = 'global_content_index_pkey'
          AND array_length(i.indkey, 1) = 1
    ) THEN
        ALTER TABLE tenant_chunk_refs
            DROP CONSTRAINT IF EXISTS tenant_chunk_refs_plaintext_hash_fkey;
        ALTER TABLE global_content_index DROP CONSTRAINT global_content_index_pkey;
        ALTER TABLE global_content_index ADD PRIMARY KEY (dedup_scope, plaintext_hash);
        ALTER TABLE tenant_chunk_refs
            ADD CONSTRAINT tenant_chunk_refs_scope_hash_fkey
            FOREIGN KEY (dedup_scope, plaintext_hash)
            REFERENCES global_content_index (dedup_scope, plaintext_hash);
    END IF;
END $$;

-- Ref-count helpers become scope-aware. The single-arg versions are dropped so
-- callers must pass a scope (a same-hash chunk can now exist in several scopes).
DROP FUNCTION IF EXISTS increment_chunk_ref(VARCHAR);
DROP FUNCTION IF EXISTS decrement_chunk_ref(VARCHAR);

CREATE OR REPLACE FUNCTION increment_chunk_ref(p_scope VARCHAR(64), p_hash VARCHAR(64))
RETURNS VOID AS $$
BEGIN
    UPDATE global_content_index
    SET ref_count = ref_count + 1,
        last_accessed_at = NOW(),
        marked_for_deletion = FALSE,
        marked_at = NULL
    WHERE dedup_scope = p_scope AND plaintext_hash = p_hash;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION decrement_chunk_ref(p_scope VARCHAR(64), p_hash VARCHAR(64))
RETURNS INTEGER AS $$
DECLARE
    new_count INTEGER;
BEGIN
    UPDATE global_content_index
    SET ref_count = ref_count - 1
    WHERE dedup_scope = p_scope AND plaintext_hash = p_hash
    RETURNING ref_count INTO new_count;

    IF new_count = 0 THEN
        UPDATE global_content_index
        SET marked_for_deletion = TRUE,
            marked_at = NOW()
        WHERE dedup_scope = p_scope AND plaintext_hash = p_hash;
    END IF;

    RETURN new_count;
END;
$$ LANGUAGE plpgsql;
