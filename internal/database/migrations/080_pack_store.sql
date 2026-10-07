-- 080: the pack store (internal/packstore, Phase 37).
--
-- Small immutable objects are batched into large, content-addressed pack
-- files on a slow per-file backend (first user: `sync`, Sync.com's WebDAV
-- bridges — ~0.5–1 s and a rate limit per file operation, 156/450 MB/s on
-- large objects). A pack is written ONCE under `<aa>/<sha256>.pack` and never
-- overwritten; this index says which member lives where.
--
-- packs: one row per pack file. Infrastructure, not tenant data — no
-- tenant_id: a pack mixes members of many tenants. The row is inserted
-- BEFORE the upload (sealed_at NULL = an upload in flight, the orphan marker
-- GC uses), sealed in the transaction that records the members, and retired
-- (retired_at) before GC deletes the file, so a writer never re-uploads a
-- name that is being deleted. Every age GC reasons about is one of these
-- Postgres timestamps — never a backend modtime (Sync's bridge reports 1970
-- for every file).
CREATE TABLE IF NOT EXISTS packs (
    id           BIGSERIAL   PRIMARY KEY,
    backend      TEXT        NOT NULL,
    -- the artifact inside the pack container: <aa>/<sha256>.pack
    name         TEXT        NOT NULL,
    -- the first-level folder (<aa>): the 50,000-entries-per-folder guard counts by it
    folder       TEXT        NOT NULL,
    size         BIGINT      NOT NULL,
    sha256       TEXT        NOT NULL,
    member_count INTEGER     NOT NULL,
    -- sum of byte_length of the live member rows (a counter; GC recomputes from the rows)
    live_bytes   BIGINT      NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sealed_at    TIMESTAMPTZ,
    retired_at   TIMESTAMPTZ,
    UNIQUE (backend, name)
);

CREATE INDEX IF NOT EXISTS idx_packs_folder ON packs (backend, folder) WHERE retired_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_packs_unsealed ON packs (backend, created_at) WHERE sealed_at IS NULL;

-- pack_members: one row per member. Tenant data (erased with the account by
-- account.EraseRows; a pack whose rows no longer cover its member_count is
-- rewritten by pack_gc, which removes the erased bytes from the backend).
-- deleted_at is the tombstone: the row stays until its pack is deleted
-- (ON DELETE CASCADE).
CREATE TABLE IF NOT EXISTS pack_members (
    id          BIGSERIAL   PRIMARY KEY,
    pack_id     BIGINT      NOT NULL REFERENCES packs(id) ON DELETE CASCADE,
    tenant_id   TEXT        NOT NULL,
    member_key  TEXT        NOT NULL,
    byte_offset BIGINT      NOT NULL,
    byte_length BIGINT      NOT NULL,
    sha256      TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at  TIMESTAMPTZ
);

-- one live member per (tenant, key); the read path's lookup
CREATE UNIQUE INDEX IF NOT EXISTS uq_pack_members_live ON pack_members (tenant_id, member_key) WHERE deleted_at IS NULL;
-- GC's per-pack aggregates and the cascade
CREATE INDEX IF NOT EXISTS idx_pack_members_pack ON pack_members (pack_id);
-- the account erasure (every row of the tenant, tombstones included)
CREATE INDEX IF NOT EXISTS idx_pack_members_tenant ON pack_members (tenant_id);
