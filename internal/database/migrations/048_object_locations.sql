CREATE TABLE IF NOT EXISTS object_locations (
    tenant_id    TEXT NOT NULL,
    bucket       TEXT NOT NULL,
    object_key   TEXT NOT NULL,
    backend_name TEXT NOT NULL,
    storage_class TEXT NOT NULL DEFAULT 'STANDARD',
    size_bytes   BIGINT NOT NULL DEFAULT 0,
    stored_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_accessed TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, bucket, object_key)
);

CREATE INDEX IF NOT EXISTS idx_object_locations_backend
    ON object_locations(backend_name);

CREATE INDEX IF NOT EXISTS idx_object_locations_age
    ON object_locations(last_accessed, backend_name);

CREATE TABLE IF NOT EXISTS tiering_policies (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   TEXT,
    bucket      TEXT,
    min_age_days INT NOT NULL DEFAULT 30,
    target_backend TEXT NOT NULL,
    target_class   TEXT NOT NULL DEFAULT 'GLACIER',
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(tenant_id, bucket, target_backend)
);

CREATE TABLE IF NOT EXISTS tenant_cost_daily (
    tenant_id   TEXT NOT NULL,
    date        DATE NOT NULL,
    backend_name TEXT NOT NULL,
    storage_bytes BIGINT NOT NULL DEFAULT 0,
    cost_microcents BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, date, backend_name)
);

ALTER TABLE object_head_cache ADD COLUMN IF NOT EXISTS last_accessed
    TIMESTAMPTZ NOT NULL DEFAULT NOW();
