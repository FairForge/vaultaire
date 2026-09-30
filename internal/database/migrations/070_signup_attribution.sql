-- 070_signup_attribution.sql: where a sign-up came from (Review R12,
-- pre-launch checklist item 7). Every waitlist_signups.source was 'landing'
-- and nothing recorded the referrer or campaign, so the LET/Reddit posts that
-- produce sign-ups could not be told apart. The referrer is stored as its
-- host only (no path, no query — nothing personal about the visitor); the
-- utm_* values are the campaign's own labels, capped at 100 characters.
-- Idempotent — safe to re-run on every deploy.

ALTER TABLE waitlist_signups ADD COLUMN IF NOT EXISTS referrer     TEXT NOT NULL DEFAULT '';
ALTER TABLE waitlist_signups ADD COLUMN IF NOT EXISTS utm_source   TEXT NOT NULL DEFAULT '';
ALTER TABLE waitlist_signups ADD COLUMN IF NOT EXISTS utm_medium   TEXT NOT NULL DEFAULT '';
ALTER TABLE waitlist_signups ADD COLUMN IF NOT EXISTS utm_campaign TEXT NOT NULL DEFAULT '';

ALTER TABLE users ADD COLUMN IF NOT EXISTS signup_referrer     TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS signup_utm_source   TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS signup_utm_medium   TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS signup_utm_campaign TEXT NOT NULL DEFAULT '';
