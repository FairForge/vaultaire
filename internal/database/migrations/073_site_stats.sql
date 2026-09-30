-- 073_site_stats.sql: cookieless statistics for the public site
-- (internal/sitestats). Daily totals only: page views and named clicks cut
-- by page, referring host, campaign labels, country and device class. No
-- row describes a person. site_visitors_daily holds the day's visitor
-- hashes (IP + user agent under a secret that rotates every UTC day and is
-- never stored) just long enough to count them: the collector prunes rows
-- older than yesterday and keeps only the count as a 'uniques' row.
-- waitlist_signups.country is the CF-IPCountry code of the signup, so the
-- launch list can be read by region without keeping the address.
-- Idempotent — safe to re-run on every deploy.

CREATE TABLE IF NOT EXISTS site_stats_daily (
    day          DATE   NOT NULL,
    kind         TEXT   NOT NULL,              -- 'view' | 'event' | 'uniques'
    name         TEXT   NOT NULL DEFAULT '',   -- page path or event name
    referrer     TEXT   NOT NULL DEFAULT '',   -- referring host, no path
    utm_source   TEXT   NOT NULL DEFAULT '',
    utm_medium   TEXT   NOT NULL DEFAULT '',
    utm_campaign TEXT   NOT NULL DEFAULT '',
    country      TEXT   NOT NULL DEFAULT '',   -- ISO 3166-1 alpha-2 (Cloudflare)
    device       TEXT   NOT NULL DEFAULT '',   -- desktop | phone | tablet
    n            BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (day, kind, name, referrer, utm_source, utm_medium, utm_campaign, country, device)
);

CREATE INDEX IF NOT EXISTS idx_site_stats_daily_day ON site_stats_daily (day DESC);

CREATE TABLE IF NOT EXISTS site_visitors_daily (
    day     DATE NOT NULL,
    visitor TEXT NOT NULL,
    PRIMARY KEY (day, visitor)
);

ALTER TABLE waitlist_signups ADD COLUMN IF NOT EXISTS country TEXT NOT NULL DEFAULT '';
