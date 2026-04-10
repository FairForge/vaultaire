-- 023_session_security.sql
-- Phase 5.8: Session security hardening
-- Track per-session IP + User-Agent + last-active so customers can see
-- active devices and revoke them from the settings page.
-- Idempotent — safe to re-run on every deploy.

ALTER TABLE dashboard_sessions ADD COLUMN IF NOT EXISTS ip_address VARCHAR(64);
ALTER TABLE dashboard_sessions ADD COLUMN IF NOT EXISTS user_agent VARCHAR(512);
ALTER TABLE dashboard_sessions ADD COLUMN IF NOT EXISTS last_active_at TIMESTAMP NOT NULL DEFAULT NOW();
