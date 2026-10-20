-- 097: the entitlement record (WP-PAY-1, docs/PAYMENTS.md § 1.2).
--
-- What a tenant has bought lives in our database, with a date. A payment from
-- any provider extends the date; the entitlement_lapse job clears a house
-- whose date plus grace (ENTITLEMENT_GRACE, 14 d) has passed — flag
-- entitlement_lapse, default OFF: count only. tenant_floor_quotas +
-- tenant_quotas stay the enforcement truth the write path reads; this row is
-- the billing truth, and internal/payments writes both in one transaction.
--
-- entitlements      one row per tenant: the house as bought (whole TB per
--                   line), the period, paid_through (grace is added at read
--                   time), renewal ('auto' = a provider will try to extend
--                   it, 'prepaid' = nobody will), the ONE provider allowed to
--                   extend it, lapsed_at (NULL = the house is granted). The
--                   next_* columns and pending_provider are for a downsize
--                   at period end and a provider switch (WP-PAY-2/-5); no
--                   writer yet.
-- payments          the ledger: one row per money event from any provider,
--                   keyed by (provider, provider_id) — a Stripe invoice id, a
--                   BTCPay invoice id, 'manual:<uuid>'. 'failed' is a Stripe
--                   invoice whose payment failed (informational; the same
--                   invoice settles the row if a retry succeeds).
--                   invoice_number is OURS, gapless per year
--                   (ST-2026-000001), assigned when the row settles.
--                   metadata: provider ids under "ids", crypto addresses
--                   under "addresses", transaction ids under "txids" — the
--                   erasure drops the last two and the tenant link, and keeps
--                   the row (an accounting record).
-- payment_events    webhook dedup for every provider but Stripe
--                   (stripe_events keeps #752's shape); 90-day retention.
-- invoice_counters  one row per year, taken FOR UPDATE inside the settle
--                   transaction: a rolled-back settle gives its number back.
--
-- Additive: the previous build ignores these tables during the two-slot
-- switch; nothing is dropped or renamed.

SET lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS entitlements (
    tenant_id        TEXT PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
    std_tb           INT  NOT NULL DEFAULT 0,
    vault_tb         INT  NOT NULL DEFAULT 0,
    pin_hot_tb       INT  NOT NULL DEFAULT 0,
    period           TEXT NOT NULL,
    paid_through     TIMESTAMPTZ NOT NULL,
    renewal          TEXT NOT NULL,
    provider         TEXT NOT NULL,
    provider_ref     TEXT,
    pending_provider TEXT,
    next_std_tb      INT,
    next_vault_tb    INT,
    next_pin_hot_tb  INT,
    next_period      TEXT,
    lapsed_at        TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT entitlements_period_check   CHECK (period IN ('monthly', 'annual')),
    CONSTRAINT entitlements_renewal_check  CHECK (renewal IN ('auto', 'prepaid')),
    CONSTRAINT entitlements_provider_check CHECK (provider IN ('stripe', 'btcpay', 'manual')),
    CONSTRAINT entitlements_house_check    CHECK (std_tb >= 0 AND vault_tb >= 0 AND pin_hot_tb >= 0 AND pin_hot_tb <= std_tb),
    CONSTRAINT entitlements_next_period_check CHECK (next_period IS NULL OR next_period IN ('monthly', 'annual'))
);
-- The lapse job's candidates and the expiring gauge.
CREATE INDEX IF NOT EXISTS idx_entitlements_due ON entitlements (paid_through) WHERE lapsed_at IS NULL;

CREATE TABLE IF NOT EXISTS payments (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT REFERENCES tenants(id) ON DELETE SET NULL,
    provider        TEXT NOT NULL,
    provider_id     TEXT NOT NULL,
    kind            TEXT NOT NULL,
    purpose         TEXT NOT NULL,
    status          TEXT NOT NULL,
    amount_cents    BIGINT NOT NULL,
    credit_cents    BIGINT NOT NULL DEFAULT 0,
    crypto_amount   NUMERIC(30,12),
    crypto_currency TEXT,
    rate            NUMERIC(20,8),
    period_start    TIMESTAMPTZ,
    period_end      TIMESTAMPTZ,
    std_tb          INT,
    vault_tb        INT,
    pin_hot_tb      INT,
    period          TEXT,
    invoice_number  TEXT UNIQUE,
    refund_of       UUID REFERENCES payments(id),
    actor           TEXT,
    note            TEXT,
    metadata        JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    settled_at      TIMESTAMPTZ,
    UNIQUE (provider, provider_id),
    CONSTRAINT payments_provider_check CHECK (provider IN ('stripe', 'btcpay', 'manual')),
    CONSTRAINT payments_kind_check     CHECK (kind IN ('charge', 'refund', 'credit', 'adjustment')),
    CONSTRAINT payments_purpose_check  CHECK (purpose IN ('new', 'renew', 'resize', 'switch', 'manual')),
    CONSTRAINT payments_status_check   CHECK (status IN ('pending', 'processing', 'settled', 'failed', 'expired', 'expired_partial', 'invalid', 'refunded')),
    CONSTRAINT payments_period_check   CHECK (period IS NULL OR period IN ('monthly', 'annual'))
);
CREATE INDEX IF NOT EXISTS idx_payments_tenant ON payments (tenant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS payment_events (
    provider    TEXT NOT NULL,
    event_id    TEXT NOT NULL,
    event_type  TEXT NOT NULL,
    tenant_id   TEXT,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    data        JSONB,
    PRIMARY KEY (provider, event_id)
);
CREATE INDEX IF NOT EXISTS idx_payment_events_tenant ON payment_events (tenant_id);
CREATE INDEX IF NOT EXISTS idx_payment_events_received ON payment_events (received_at);

CREATE TABLE IF NOT EXISTS invoice_counters (
    year INT PRIMARY KEY,
    last INT NOT NULL DEFAULT 0
);

RESET lock_timeout;
