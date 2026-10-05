-- 077: the Vault parity second copy (WP-VAULT-1 part 2).
--
-- A vault-floor object's bytes live on tape (Geyser). The pricing page
-- promises a second copy and the engine made none. The second copy is
-- Reed-Solomon 4+4 over the object: the four DATA shards are not stored
-- (they are byte ranges of the object tape already holds), the four PARITY
-- shards are written to a free leg (permafrost, else lyve) by the
-- `vault_parity` job behind the commit. Any four of the eight shards rebuild
-- the object, so the four parity shards alone are a complete copy in
-- information terms — the reader fallback rebuilds from them when Geyser
-- answers an error or its breaker is open, and from the data ranges it CAN
-- read plus the parity when a parity shard is gone too.
--
-- One row per protected object (the head row's key). etag binds the shards
-- to the version they were computed from: an overwrite makes the row stale
-- and the job erases the shards and writes new ones. legs[j] is the backend
-- that holds parity shard j ('' = not written): a write that landed on some
-- shards and failed on others leaves state = 'partial' with last_error, and
-- is retried.
CREATE TABLE IF NOT EXISTS vault_parity (
    tenant_id     TEXT        NOT NULL,
    bucket        TEXT        NOT NULL,
    object_key    TEXT        NOT NULL,
    etag          TEXT        NOT NULL,
    size_bytes    BIGINT      NOT NULL,
    data_shards   SMALLINT    NOT NULL DEFAULT 4,
    parity_shards SMALLINT    NOT NULL DEFAULT 4,
    -- bytes of each shard per stripe; one stripe covers data_shards × stripe_bytes of the object
    stripe_bytes  INTEGER     NOT NULL,
    -- length of every parity shard: stripes × stripe_bytes
    shard_bytes   BIGINT      NOT NULL,
    -- artifact prefix inside the tenant's `_parity` container; shard j is <shard_prefix>/p<j>
    shard_prefix  TEXT        NOT NULL,
    legs          TEXT[]      NOT NULL,
    state         TEXT        NOT NULL CHECK (state IN ('complete', 'partial')),
    last_error    TEXT,
    attempts      INTEGER     NOT NULL DEFAULT 0,
    written_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, bucket, object_key)
);

CREATE INDEX IF NOT EXISTS idx_vault_parity_incomplete ON vault_parity (updated_at) WHERE state <> 'complete';
