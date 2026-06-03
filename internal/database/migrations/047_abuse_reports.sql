CREATE TABLE IF NOT EXISTS abuse_reports (
    id            BIGSERIAL PRIMARY KEY,
    reporter_email TEXT NOT NULL,
    reporter_name  TEXT NOT NULL DEFAULT '',
    tenant_id     TEXT,
    bucket        TEXT,
    object_key    TEXT,
    report_type   TEXT NOT NULL,
    description   TEXT NOT NULL,
    url           TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'open',
    resolved_by   TEXT,
    resolved_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_abuse_reports_status
    ON abuse_reports(status, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_abuse_reports_tenant
    ON abuse_reports(tenant_id) WHERE tenant_id IS NOT NULL;
