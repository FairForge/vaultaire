-- 072: Account-deletion runner (WP-R10-3, decisions D-15/D-16).
--
-- tenants.deletion_stripe_cancelled_at records that the runner cancelled the
-- tenant's Stripe subscription (stage a of the erasure). The cancel is the one
-- step that talks to a third party, so it must be idempotent across crashes
-- and retries: a run that finds the stamp skips Stripe and goes on to the
-- object walk; a run that fails after the API call but before the stamp
-- repeats the cancel, which Stripe answers with the already-cancelled
-- subscription (no second charge, no error the runner treats as fatal).
--
-- The partial index serves the runner's daily select
-- (`status = 'pending_deletion' AND deletion_scheduled_at < NOW()`).

ALTER TABLE tenants ADD COLUMN IF NOT EXISTS deletion_stripe_cancelled_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_users_deletion_due
    ON users (deletion_scheduled_at)
    WHERE status = 'pending_deletion';
