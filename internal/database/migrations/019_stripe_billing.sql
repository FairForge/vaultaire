-- 019_stripe_billing.sql
-- Stripe webhook idempotency and billing state tracking.
-- Idempotent — safe to re-run on every deploy.

-- Prevent double-processing of Stripe webhook events.
-- Before processing any event, INSERT into this table; if it conflicts,
-- the event was already handled — skip it.
CREATE TABLE IF NOT EXISTS stripe_events (
    event_id    VARCHAR(255) PRIMARY KEY,  -- Stripe event ID (evt_xxx)
    event_type  VARCHAR(100) NOT NULL,     -- e.g. "checkout.session.completed"
    processed_at TIMESTAMP NOT NULL DEFAULT NOW(),
    tenant_id   VARCHAR(255),              -- resolved tenant, if applicable
    data        JSONB                      -- raw event data for debugging
);

CREATE INDEX IF NOT EXISTS idx_stripe_events_type
    ON stripe_events (event_type);
CREATE INDEX IF NOT EXISTS idx_stripe_events_tenant
    ON stripe_events (tenant_id);

-- Billing charges ledger (for usage-based billing records).
CREATE TABLE IF NOT EXISTS billing_charges (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   VARCHAR(255) NOT NULL,
    charge_type VARCHAR(50) NOT NULL,      -- "storage", "bandwidth", "overage", "addon"
    amount_cents INTEGER NOT NULL,
    description TEXT,
    stripe_invoice_id VARCHAR(255),
    period_start DATE,
    period_end   DATE,
    created_at  TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_billing_charges_tenant
    ON billing_charges (tenant_id, created_at);
