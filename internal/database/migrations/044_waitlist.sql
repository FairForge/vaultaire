-- 044_waitlist.sql: Pre-launch waitlist email capture for the stored.ge landing page.
-- Idempotent — safe to re-run on every deploy.

CREATE TABLE IF NOT EXISTS waitlist_signups (
    id BIGSERIAL PRIMARY KEY,
    email TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT 'landing',
    ip_address TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (email)
);

CREATE INDEX IF NOT EXISTS idx_waitlist_created ON waitlist_signups (created_at DESC);
