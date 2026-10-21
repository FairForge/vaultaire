-- 043_metered_billing.sql: Metered usage reporting to Stripe Billing Meters.
--
-- Emptied by WP-PAY-7: metered_usage_reports and tenant_quotas.spending_cap_cents
-- were created here; the metered reporter was deleted by WP-PAY-0 (F-29) and
-- migration 100 drops both. The deploy re-runs every file, so the CREATE and
-- the ADD COLUMN went with the drop (a re-run must never re-add them).
SELECT 1;
