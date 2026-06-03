-- 046_admin_notifications.sql: Admin notification system.
-- Idempotent — safe to re-run on every deploy.

CREATE TABLE IF NOT EXISTS admin_notifications (
    id         BIGSERIAL PRIMARY KEY,
    type       TEXT NOT NULL,
    message    TEXT NOT NULL,
    tenant_id  TEXT,
    read_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_admin_notif_unread
    ON admin_notifications(created_at DESC) WHERE read_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_admin_notif_created
    ON admin_notifications(created_at DESC);
