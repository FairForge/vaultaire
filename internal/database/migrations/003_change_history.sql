CREATE TABLE IF NOT EXISTS change_history (
    id SERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    container TEXT NOT NULL,
    artifact TEXT NOT NULL,
    operation TEXT NOT NULL,
    user_id TEXT,
    timestamp TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    metadata JSONB,
    checksum TEXT,
    size_bytes BIGINT
);

CREATE INDEX idx_change_history_tenant ON change_history(tenant_id);
CREATE INDEX idx_change_history_timestamp ON change_history(timestamp);
CREATE INDEX idx_change_history_artifact ON change_history(tenant_id, container, artifact);
