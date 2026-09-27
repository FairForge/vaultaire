-- 065_house_intent.sql: what a visitor built in the landing page's house
-- (whole TB downstairs = Standard, in the attic = Vault, plus the share-link
-- room), carried onto the waitlist pre-launch and onto the tenant at signup.
-- A hint for onboarding and demand reporting; nothing is billed from it.
-- Idempotent — safe to re-run on every deploy.

ALTER TABLE waitlist_signups ADD COLUMN IF NOT EXISTS plan_std_tb INTEGER NOT NULL DEFAULT 0;
ALTER TABLE waitlist_signups ADD COLUMN IF NOT EXISTS plan_vault_tb INTEGER NOT NULL DEFAULT 0;
ALTER TABLE waitlist_signups ADD COLUMN IF NOT EXISTS room TEXT NOT NULL DEFAULT '';

ALTER TABLE tenants ADD COLUMN IF NOT EXISTS intent_std_tb INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS intent_vault_tb INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS intent_room TEXT NOT NULL DEFAULT '';
