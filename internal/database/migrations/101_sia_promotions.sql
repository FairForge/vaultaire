-- 101: sia_promotions — hydrate-before-serve for large Sia objects (Phase 6.5,
-- bench-results/SIA-S3D-2026-10.md SIA-BENCH-2..6). The `sia` backend has a
-- read budget: hosts refuse reads once the account's per-host balances are
-- spent (refilled every 5 min), and the patched gateway waits for the refill,
-- so a large read can sit idle for minutes and a client with a 60 s read
-- timeout gives up. A GET of a large object recorded on `sia` answers 503
-- SlowDown + Retry-After while ONE background copy streams the stored bytes
-- into the primary at a new blob address; later reads are served from that
-- copy (internal/api/sia_promotion.go).
--
-- One row per promoted copy. The head row is never re-pointed: the object
-- stays on `sia` (its home, what it is billed and routed by); the copy is a
-- cache, never billed. A row describes the object whose bytes are at
-- source_key on `sia` with this etag — a write gives the object a new
-- address, so a row of an older address is stale: never served, retired at
-- once by the write's flip transaction / the delete's aftermath, and by the
-- `sia_promotion` job (which also reclaims copies idle past SIA_PROMOTE_TTL).
-- Retiring = the row deleted and the copy queued in retired_blobs (24 h grace,
-- then deleted by the retired_blobs collector on promoted_backend).
--
-- promoted_key is chosen and recorded BEFORE the first byte is written
-- (state 'pending'), so a copy is never written that no row names.
-- Tenant data: erased with the account (account.EraseRows); the copy is under
-- the tenant's prefix on the primary, so the erasure sweep removes it too.
-- Excluded from the GDPR export (a cache of an object the export already lists).
CREATE TABLE IF NOT EXISTS sia_promotions (
    tenant_id        TEXT        NOT NULL,
    bucket           TEXT        NOT NULL,
    object_key       TEXT        NOT NULL,
    source_backend   TEXT        NOT NULL DEFAULT 'sia',
    source_key       TEXT        NOT NULL,   -- the object's address on the source: COALESCE(storage_key, object_key)
    etag             TEXT        NOT NULL,
    size_bytes       BIGINT      NOT NULL,   -- the object's size
    stored_bytes     BIGINT      NOT NULL,   -- the blob's length (a sealed blob is longer)
    promoted_backend TEXT        NOT NULL,   -- the primary when the copy was made
    promoted_key     TEXT        NOT NULL,   -- the copy's address (newBlobAddress)
    state            TEXT        NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'ready')),
    promoted_md5     TEXT        NOT NULL DEFAULT '',
    requested_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ready_at         TIMESTAMPTZ,
    last_accessed    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, bucket, object_key, source_key)
);

CREATE INDEX IF NOT EXISTS idx_sia_promotions_last_accessed ON sia_promotions (last_accessed);
CREATE INDEX IF NOT EXISTS idx_sia_promotions_promoted ON sia_promotions (tenant_id, bucket, promoted_key);
