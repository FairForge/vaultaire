-- 086: every whole object knows its own blob address (WP-R2-1, the "expand"
-- half — Stage 1a Prompt 2B, 2026-10-12).
--
-- Before: every whole-object writer put its bytes IN PLACE at <container>/<key>
-- and then ran the head-row transaction, so a key had exactly one blob. That is
-- why a non-current GET ?versionId answers 501 (R2-03: versions are
-- metadata-only), why a failure after the backend write destroys the previous
-- object (R2-05, R3-15), why two concurrent PUTs can leave the row describing
-- the other body (R2-15) and why the Smart reclaim's stale-copy delete can
-- remove a fresh PUT's bytes (R13-06). The fix is immutable blob keys: a write
-- goes to a fresh address and the head-row transaction flips a pointer.
--
-- This migration only lets every READER honour such a pointer; no writer sets
-- one yet (Prompt 2C does, after this half is deployed: the two-slot deploy
-- keeps the previous build serving the new schema for up to 15 min, and a
-- reader that does not know the column must never meet a row that needs it).
-- On prod every new column is NULL after it runs: nothing changes for any
-- customer, no backfill, no blob ever moves.
--
-- Why a per-row COLUMN and not a naming rule a reader derives (e.g. "the blob
-- is <key>@<version_id>"): (1) every row that exists today has its bytes at
-- <key>, and a rule would have to say "unless the row predates the rule" — the
-- column's NULL says exactly that, for ever, with no flag day; (2) the address
-- is a fact about the BLOB, like global_content_index.storage_key and the
-- 0x02 header's key version (084), and a reader must never re-derive a fact a
-- writer recorded (two derivations drift; R8-7 was that bug for chunks);
-- (3) the write that creates the blob picks its address — 2C may need a
-- shorter or differently separated name on a backend whose path limit a long
-- key already approaches (Sync's per-segment limit, permafrost's path limit) —
-- and a recorded address lets that choice change without touching a reader;
-- (4) a Smart demotion copies the bytes to another backend under the SAME
-- address, which only works if the address is data, not a function of the
-- backend.

-- NULL = the blob is at <key> (every row written before 2C, for ever).
ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS storage_key TEXT;

-- NULL = no bytes are retained for this version (every row today: the bytes
-- were overwritten in place) — except the newest live version behind a
-- delete marker, whose bytes are still at <key> (versionBytesIntact, kept).
ALTER TABLE object_versions ADD COLUMN IF NOT EXISTS storage_key TEXT;
-- What the retained blob of a version is sealed with: '' / NULL = plaintext,
-- the head row's values otherwise (AES256 = SSE-S3, opened under the key
-- version its 0x02 header records; SSE-C = the customer's key). A reader of a
-- non-current version must know whether to decrypt without the head row,
-- which describes the CURRENT object.
ALTER TABLE object_versions ADD COLUMN IF NOT EXISTS encryption_algorithm TEXT;

-- Where the copies a Smart demotion made live: the head row's storage_key at
-- the time of the demotion (NULL = <key>). The ledger's settle, flip-back,
-- copy-back and reclaim address the copies they know by it — never by the
-- head row of now, which may name a newer blob.
ALTER TABLE smart_demotions ADD COLUMN IF NOT EXISTS storage_key TEXT;

-- An address is never the empty string: '' would be read as "<key>" by some
-- readers (COALESCE(storage_key, '')) and as an address by none.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'object_head_cache_storage_key_nonempty') THEN
        ALTER TABLE object_head_cache ADD CONSTRAINT object_head_cache_storage_key_nonempty
            CHECK (storage_key IS NULL OR storage_key <> '') NOT VALID;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'object_versions_storage_key_nonempty') THEN
        ALTER TABLE object_versions ADD CONSTRAINT object_versions_storage_key_nonempty
            CHECK (storage_key IS NULL OR storage_key <> '') NOT VALID;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'smart_demotions_storage_key_nonempty') THEN
        ALTER TABLE smart_demotions ADD CONSTRAINT smart_demotions_storage_key_nonempty
            CHECK (storage_key IS NULL OR storage_key <> '') NOT VALID;
    END IF;
END $$;

-- Whole-object blobs a write or a version delete displaced (the
-- retired_chunk_blobs shape, for whole objects): the address is kept until
-- retire_after — a reader that resolved the old row a moment ago still finds
-- its bytes — and deleted by the collector after that (2C's job; until it
-- ships the rows wait, costing storage, never data). container is the engine
-- container (<tenant>_<bucket>); reason is 'overwritten' or 'version_deleted'.
-- Tenant data: erased with the account (account.EraseRows); the bytes it names
-- are inside the tenant's prefix, so the erasure sweep removes them too.
CREATE TABLE IF NOT EXISTS retired_blobs (
    backend_id   TEXT        NOT NULL,
    container    TEXT        NOT NULL,
    storage_key  TEXT        NOT NULL,
    tenant_id    TEXT        NOT NULL,
    reason       TEXT        NOT NULL,
    retire_after TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (backend_id, container, storage_key)
);

CREATE INDEX IF NOT EXISTS idx_retired_blobs_due ON retired_blobs (retire_after);
CREATE INDEX IF NOT EXISTS idx_retired_blobs_tenant ON retired_blobs (tenant_id);
