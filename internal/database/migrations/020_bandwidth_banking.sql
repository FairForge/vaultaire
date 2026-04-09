-- 020_bandwidth_banking.sql
-- Bandwidth banking foundation: rollover tracking and alert thresholds.
-- Idempotent — safe to re-run on every deploy.

-- Track monthly bandwidth rollover balances per tenant.
-- Unused bandwidth can carry forward to the next month.
CREATE TABLE IF NOT EXISTS bandwidth_rollover (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   VARCHAR(255) NOT NULL,
    month       DATE NOT NULL,                -- first day of the month
    base_bytes  BIGINT NOT NULL DEFAULT 0,    -- plan's base bandwidth allowance
    rollover_bytes BIGINT NOT NULL DEFAULT 0, -- carried over from previous month
    used_bytes  BIGINT NOT NULL DEFAULT 0,    -- actual usage that month
    created_at  TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, month)
);

CREATE INDEX IF NOT EXISTS idx_bandwidth_rollover_tenant
    ON bandwidth_rollover (tenant_id, month DESC);

-- Bandwidth alert configuration per tenant.
-- Admin can set thresholds; when usage crosses them, alerts fire.
CREATE TABLE IF NOT EXISTS bandwidth_alerts (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      VARCHAR(255) NOT NULL,
    threshold_pct  INTEGER NOT NULL DEFAULT 80,  -- alert at this % of limit
    alert_type     VARCHAR(50) NOT NULL DEFAULT 'email', -- "email", "dashboard", "webhook"
    enabled        BOOLEAN NOT NULL DEFAULT true,
    last_fired_at  TIMESTAMP,
    created_at     TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, threshold_pct, alert_type)
);

CREATE INDEX IF NOT EXISTS idx_bandwidth_alerts_tenant
    ON bandwidth_alerts (tenant_id);
