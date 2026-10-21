-- 072: Account-deletion runner (WP-R10-3, decisions D-15/D-16).
--
-- (tenants.deletion_stripe_cancelled_at was added here until migration 100
-- dropped it: the runner's stamp is a payment_events 'deletion_cancel' row,
-- keyed by the subscription it cancelled — WP-PAY-7.)
--
-- The partial index serves the runner's daily select
-- (`status = 'pending_deletion' AND deletion_scheduled_at < NOW()`).

CREATE INDEX IF NOT EXISTS idx_users_deletion_due
    ON users (deletion_scheduled_at)
    WHERE status = 'pending_deletion';
