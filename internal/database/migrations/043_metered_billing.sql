-- 043_metered_billing.sql: Metered usage reporting to Stripe Billing Meters.
-- Idempotent — safe to re-run on every deploy.

-- Accrued-charge source + idempotency guard for daily meter reports.
-- One row per (tenant, meter, period_date). The reporter's ON CONFLICT DO NOTHING
-- means a crashed/re-run job never double-reports to Stripe. The same table also
-- guards spending-cap alerts (synthetic meters 'alert:80'/'alert:95' keyed by the
-- first-of-month date fire each threshold once per month).
CREATE TABLE IF NOT EXISTS metered_usage_reports (
    id BIGSERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    meter TEXT NOT NULL,
    period_date DATE NOT NULL,
    value BIGINT NOT NULL DEFAULT 0,
    stripe_event_id TEXT NOT NULL DEFAULT '',
    reported_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, meter, period_date)
);

CREATE INDEX IF NOT EXISTS idx_metered_usage_tenant_period
    ON metered_usage_reports (tenant_id, period_date);

-- Optional per-tenant monthly spending cap, in cents. 0 = no cap (the default).
ALTER TABLE tenant_quotas ADD COLUMN IF NOT EXISTS spending_cap_cents BIGINT NOT NULL DEFAULT 0;
