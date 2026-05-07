CREATE TABLE IF NOT EXISTS idempotency_cache (
    tenant_id        TEXT        NOT NULL,
    idempotency_key  TEXT        NOT NULL,
    method           TEXT        NOT NULL,
    path             TEXT        NOT NULL,
    response_status  INTEGER     NOT NULL,
    response_headers JSONB       NOT NULL DEFAULT '{}',
    response_body    BYTEA       NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_idempotency_cache_created_at ON idempotency_cache (created_at);
