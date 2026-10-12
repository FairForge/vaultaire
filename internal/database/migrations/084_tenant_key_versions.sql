-- 084: stored-random, versioned tenant keys (WP-R8-1, Stage 1a, 2026-10-12).
--
-- Before: the tenant chunk key was HKDF(master, tenant, "v1") — derived, so it
-- could never be rotated without re-encrypting everything and never shredded
-- while the master key lived; the SSE-S3 seed (037, tenant_encryption_keys) was
-- random but unversioned; every chunk ref recorded encryption_key_version = 1
-- and no read looked at it. Prod had 0 encrypted chunks and 0 SSE-S3 objects
-- (ENCRYPTION_MASTER_KEY unset), so there is no compatibility path: a dev
-- database's chunks encrypted under the HKDF key become unreadable.
--
-- tenant_key_versions: one row per (tenant, version). Both secrets are random,
-- wrapped under ENCRYPTION_MASTER_KEY with AES-256-GCM and the AAD
-- tenant || version || purpose (a wrapped blob cannot be moved to another
-- tenant, version or purpose). The current version is the highest; Rotate
-- inserts max+1 and stamps retired_at on the one before. Deleting a tenant's
-- rows (account.EraseRows) is the crypto-shred.
CREATE TABLE IF NOT EXISTS tenant_key_versions (
    tenant_id         TEXT        NOT NULL,
    key_version       INT         NOT NULL,
    chunk_key_wrapped BYTEA       NOT NULL,
    kem_seed_wrapped  BYTEA       NOT NULL,
    kem_public_key    BYTEA       NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    retired_at        TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, key_version)
);

-- The version a chunk blob was sealed under is a property of the BLOB (like
-- ciphertext_hash), so it lives on the index row; NULL = unencrypted (or sealed
-- before 084). tenant_chunk_refs.encryption_key_version (016) stays as the
-- per-ref copy, read only when the row has none.
ALTER TABLE global_content_index ADD COLUMN IF NOT EXISTS encryption_key_version INT;

-- The version a whole SSE-S3 object was sealed under (it is also in the blob's
-- 0x02 header; this copy lets the rotation backlog be counted from the table).
-- NULL = not SSE-S3, or sealed before 084 (format 0x01 = version 1).
ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS encryption_key_version INT;

-- Chunk blobs the key_reencrypt job replaced: the old address is kept for
-- retire_after (a reader that resolved the old row still finds its blob) and
-- deleted by a run after that. dedup_scope + plaintext_hash name the chunk so
-- the delete runs under the chunk's advisory lock and is skipped when an index
-- row names the address again (dedup GC swept the row and a PUT re-stored the
-- chunk at its plain key in the meantime).
CREATE TABLE IF NOT EXISTS retired_chunk_blobs (
    backend_id     TEXT        NOT NULL,
    storage_key    TEXT        NOT NULL,
    dedup_scope    TEXT        NOT NULL,
    plaintext_hash TEXT        NOT NULL,
    retire_after   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (backend_id, storage_key)
);

-- The job's candidate scan: a tenant's encrypted rows below its current
-- version, oldest first.
CREATE INDEX IF NOT EXISTS idx_gci_scope_key_version
    ON global_content_index (dedup_scope, encryption_key_version)
    WHERE encrypted;
