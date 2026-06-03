-- 045_admin_notes.sql: Internal admin notes on customer accounts.
-- Idempotent — safe to re-run on every deploy.

CREATE TABLE IF NOT EXISTS admin_notes (
    id            BIGSERIAL PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    admin_user_id UUID NOT NULL REFERENCES users(id),
    note          TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_admin_notes_tenant ON admin_notes(tenant_id, created_at DESC);
