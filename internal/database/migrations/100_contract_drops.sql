-- 100_contract_drops.sql: the contract of WP-PAY-7 (docs/PAYMENTS.md § 1.6
-- step 4) and decision D-12's dead billing tables.
--
-- The entitlement record (097: entitlements, payments, payment_events) is the
-- ONLY billing state read or written since WP-PAY-7 PRs 2/3 (#834, #836):
--   tenants.stripe_subscription_id   the entitlement's provider_ref
--   tenants.subscription_status      the entitlement's lapsed_at / paid_through
--   tenants.house_period             the entitlement's period
--   tenants.deletion_stripe_cancelled_at  a payment_events 'deletion_cancel' row
-- and D-12 / F-29: no writer since WP-PAY-0 (#807) —
--   subscriptions (R10-14), billing_charges, metered_usage_reports,
--   tenant_quotas.spending_cap_cents.
-- Prod at the drop (2026-10-21): 0 rows in the three tables, 0 non-zero caps,
-- 0 tenants with a subscription id, a status other than 'none', or a stamp.
--
-- THE ONE IRREVERSIBLE STEP OF THE LANE: applied only after both slots ran
-- the build that no longer touches them, and after a pg-backup.sh run.
--
-- The deploy re-applies every file: the statements that created these
-- objects (018, 019, 043, 056, 066, 072) were removed in the same change, so
-- a re-run never re-adds what this drops (a dropped column keeps its attnum
-- slot, and tenants would creep toward Postgres' 1600-column cap one deploy
-- at a time). Idempotent: every statement is IF EXISTS.

SET lock_timeout = '5s';

ALTER TABLE tenants
    DROP COLUMN IF EXISTS stripe_subscription_id,
    DROP COLUMN IF EXISTS subscription_status,
    DROP COLUMN IF EXISTS house_period,
    DROP COLUMN IF EXISTS deletion_stripe_cancelled_at;

ALTER TABLE tenant_quotas DROP COLUMN IF EXISTS spending_cap_cents;

DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS billing_charges;
DROP TABLE IF EXISTS metered_usage_reports;
