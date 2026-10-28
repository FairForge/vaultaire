-- 113_blob_copies.sql
--
-- WP-EDGE-4 Part A (Stage 1a Prompt 48, plan 40.10 slice 3 — the second
-- copy at the edge). Additive only: the previous build reads none of it.
--
-- blob_copies: one row per second copy of a blob on another backend (a
-- "leg"), at the SAME address the primary holds it at:
--   tenant_id    the owner (a codec chunk: the tenant whose scope names it)
--   bucket       the object's bucket; '' for a codec chunk (container
--                _global, the chunk store's context)
--   object_key   the object's key; '' for a codec chunk
--   storage_key  the blob's address on the primary (a whole object's
--                <key>@<id>, a codec chunk's _chunks/<tenant>/<keyed>.k<v>)
--   source_backend  the backend the primary copy is on (the repair copy
--                reads it from there)
--   leg          the backend holding this copy (MIRROR_LEGS)
--   kind         mirror (a whole copy) | shard (an RS shard, edge_rs, dark)
--   shard_index  the shard's place (0 for a mirror)
--   state        pending (the copy is owed: the blob_copies job writes it)
--                | ready (on the leg, etag/size known)
--                | stale (the primary is gone: the job deletes the copy,
--                  then the row)
--   etag, size   the leg's answer (a mirror: the primary's MD5 too)
--   attempts, last_error   the job's repair bookkeeping
-- Tenant data: erased with the account (account.EraseRows), excluded from
-- the export (copies of the customer's own objects — the export lists the
-- objects once).
CREATE TABLE IF NOT EXISTS blob_copies (
    tenant_id      TEXT NOT NULL,
    bucket         TEXT NOT NULL DEFAULT '',
    object_key     TEXT NOT NULL DEFAULT '',
    storage_key    TEXT NOT NULL,
    source_backend TEXT NOT NULL DEFAULT '',
    leg            TEXT NOT NULL,
    kind           TEXT NOT NULL DEFAULT 'mirror' CHECK (kind IN ('mirror', 'shard')),
    shard_index    INTEGER NOT NULL DEFAULT 0,
    state          TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'ready', 'stale')),
    etag           TEXT NOT NULL DEFAULT '',
    size           BIGINT NOT NULL DEFAULT 0,
    attempts       INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, storage_key, leg, shard_index)
);
-- The job's passes walk by state, oldest first.
CREATE INDEX IF NOT EXISTS idx_blob_copies_state ON blob_copies (state, created_at);
-- A read admit and a DELETE ask by object.
CREATE INDEX IF NOT EXISTS idx_blob_copies_object ON blob_copies (tenant_id, bucket, object_key) WHERE bucket <> '';
