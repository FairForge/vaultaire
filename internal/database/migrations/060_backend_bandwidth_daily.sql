-- 060: per-backend daily bandwidth attribution.
--
-- bandwidth_usage_daily answers "which tenant moved bytes"; this table answers
-- "which storage backend served them", so backend egress can be costed against
-- per-backend rate cards (admin /admin/costs, modelled vs invoiced views).
-- Written by BandwidthTracker.Flush alongside the per-tenant upsert; rows only
-- exist for requests that actually touched a backend.

CREATE TABLE IF NOT EXISTS backend_bandwidth_daily (
    backend_name   TEXT   NOT NULL,
    date           DATE   NOT NULL,
    ingress_bytes  BIGINT NOT NULL DEFAULT 0,
    egress_bytes   BIGINT NOT NULL DEFAULT 0,
    requests_count BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (backend_name, date)
);

CREATE INDEX IF NOT EXISTS idx_backend_bandwidth_daily_date
    ON backend_bandwidth_daily (date);
