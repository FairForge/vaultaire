-- 059_feature_flags.sql (item 1.13 — live-iteration kit)
-- Idempotent — safe to re-run on every deploy.
--
-- Runtime feature flags: global kill-switches AND per-tenant enablement,
-- flippable via the admin API / dashboard with no deploy or restart.
-- tenant_id = '*' is the global row — a sentinel instead of NULL because
-- NULL cannot participate in a primary key. Per-tenant rows use the real
-- tenant ID (TEXT — registration mints string IDs, see migration 058).
--
-- Resolution precedence (internal/flags): tenant row → global row →
-- registered in-code default. No row and no registration = disabled.
CREATE TABLE IF NOT EXISTS feature_flags (
    flag_key   TEXT NOT NULL,
    tenant_id  TEXT NOT NULL DEFAULT '*',
    enabled    BOOLEAN NOT NULL,
    updated_by TEXT,
    updated_at TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (flag_key, tenant_id)
);
