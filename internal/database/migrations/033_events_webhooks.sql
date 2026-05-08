-- Phase 5.11.6: Event Log + Webhook Management API
-- Persistent event log, webhook endpoint registry, and delivery tracking.

CREATE TABLE IF NOT EXISTS events (
    id          TEXT PRIMARY KEY,
    type        TEXT NOT NULL,
    tenant_id   TEXT NOT NULL,
    data        JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_events_tenant ON events(tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_type   ON events(type);

CREATE TABLE IF NOT EXISTS webhook_endpoints (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    url           TEXT NOT NULL,
    event_filter  TEXT[] NOT NULL DEFAULT '{}',
    secret        TEXT NOT NULL,
    enabled       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_webhook_endpoints_tenant ON webhook_endpoints(tenant_id);

CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id             TEXT PRIMARY KEY,
    webhook_id     TEXT NOT NULL REFERENCES webhook_endpoints(id) ON DELETE CASCADE,
    event_id       TEXT NOT NULL REFERENCES events(id),
    status         TEXT NOT NULL DEFAULT 'pending',
    response_code  INT,
    response_body  TEXT,
    latency_ms     INT,
    retry_count    INT NOT NULL DEFAULT 0,
    next_retry_at  TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_webhook ON webhook_deliveries(webhook_id, created_at DESC);
