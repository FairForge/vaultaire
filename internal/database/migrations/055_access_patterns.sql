-- access_patterns: ML access-tracking table (internal/intelligence AccessTracker).
-- The tracker has been wired into the request path for several phases but its
-- table was never migrated — every batch flush errors with "relation
-- access_patterns does not exist" (prod logs this on each flush and the
-- access data is silently dropped). Columns cover every query in
-- internal/intelligence/access_tracker.go, anomaly.go and
-- internal/api/patterns.go; the primary key matches the tracker's
-- ON CONFLICT (tenant_id, container, artifact_key) upsert.

CREATE TABLE IF NOT EXISTS access_patterns (
    tenant_id        VARCHAR(255) NOT NULL,
    container        VARCHAR(255) NOT NULL,
    artifact_key     VARCHAR(1024) NOT NULL,
    operation        VARCHAR(32) NOT NULL,
    size_bytes       BIGINT NOT NULL DEFAULT 0,
    latency_ms       BIGINT NOT NULL DEFAULT 0,
    backend_used     VARCHAR(255),
    cache_hit        BOOLEAN NOT NULL DEFAULT FALSE,
    access_time      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    success          BOOLEAN NOT NULL DEFAULT TRUE,
    access_count     BIGINT NOT NULL DEFAULT 1,
    total_bytes      BIGINT NOT NULL DEFAULT 0,
    access_frequency DOUBLE PRECISION NOT NULL DEFAULT 0,
    temperature      VARCHAR(16) NOT NULL DEFAULT 'cold',
    first_seen       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, container, artifact_key)
);

-- GetHotData: WHERE tenant_id AND temperature ORDER BY access_frequency DESC
CREATE INDEX IF NOT EXISTS idx_access_patterns_tenant_temp
    ON access_patterns (tenant_id, temperature, access_frequency DESC);

-- patterns.go CSV export: WHERE tenant_id ORDER BY access_time DESC
CREATE INDEX IF NOT EXISTS idx_access_patterns_tenant_time
    ON access_patterns (tenant_id, access_time DESC);
