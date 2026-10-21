-- 018_dashboard_foundation.sql
-- Phase 0.1: Dashboard foundation tables and column additions
-- Idempotent — safe to re-run on every deploy

-- Add status and role columns to users
ALTER TABLE users ADD COLUMN IF NOT EXISTS status VARCHAR(20) NOT NULL DEFAULT 'active';
ALTER TABLE users ADD COLUMN IF NOT EXISTS role VARCHAR(20) NOT NULL DEFAULT 'user';

-- Add Stripe and subscription columns to tenants
-- (stripe_subscription_id and subscription_status were added here until
-- migration 100 dropped them — WP-PAY-7; the deploy re-runs every file, so
-- the ADDs went with the drop.)
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS stripe_customer_id VARCHAR(255);
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS plan VARCHAR(50) NOT NULL DEFAULT 'starter';
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS suspended_at TIMESTAMP;

-- (The subscriptions table was created here until migration 100 dropped it:
-- no writer, R10-14 / D-12.)

-- Bandwidth usage daily: time-series per-tenant for billing and dashboard charts
CREATE TABLE IF NOT EXISTS bandwidth_usage_daily (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(255) NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    date DATE NOT NULL,
    ingress_bytes BIGINT NOT NULL DEFAULT 0,
    egress_bytes BIGINT NOT NULL DEFAULT 0,
    requests_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, date)
);


-- Dashboard sessions: PostgreSQL-backed sessions replace in-memory map
CREATE TABLE IF NOT EXISTS dashboard_sessions (
    id VARCHAR(64) PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    tenant_id VARCHAR(255) NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    email VARCHAR(255) NOT NULL,
    role VARCHAR(20) NOT NULL DEFAULT 'user',
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMP NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON dashboard_sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON dashboard_sessions(expires_at);
