# internal/sitestats

Cookieless statistics for the public site. One `Collector` per process buffers
hits in memory; `Run` flushes daily aggregates every 30 s (and once more on
shutdown, after the HTTP drain) into `site_stats_daily` (migration 073).

- **What is counted:** `KindView` = a rendered public page (`PagePath` allowlist:
  `/`, `/docs*`, `/changelog`, `/status`, `/register`, `/login`, `/legal/*`,
  `/abuse`) from a non-bot browser (`IsBot`), and `KindEvent` = a name from the
  closed `allowedEvents` list posted by the landing page's beacon (`POST /api/ping`,
  `internal/api/site_stats.go`). Dimensions: referring host (no path, never our own
  hosts), `utm_source/medium/campaign` (≤100 chars), country (Cloudflare's
  `CF-IPCountry` via `clientip.Country`, trusted only behind a Cloudflare peer),
  device class. `Normalize` bounds every field; past `maxKeys` distinct keys per
  interval new keys fold onto their page without campaign dims, then drop.
- **Uniques:** `RecordVisitor` hashes (IP, user agent) under a 32-byte salt that
  rotates every UTC day and never leaves memory; the hash rows live in
  `site_visitors_daily` for two days and the count is kept as a `uniques` row.
  This is the Plausible model; the privacy policy's "Site statistics" bullet
  describes it, and the cookie policy's no-tracking section points at it.
- **Read side:** `GET /admin/stats` (dashboard `HandleAdminStats`, "Traffic" in
  the admin sidebar): 30-day table with waitlist signups and accounts per day,
  top pages/referrers/countries/devices/sources/campaigns/events, waitlist by
  country (`waitlist_signups.country`, also 073), and the all-time funnel
  waitlist → accounts → with a bucket → with an object → paying.
- **Never:** a cookie, an identifier, a third-party script, a per-visitor row.
  A failed flush merges the buffer back and retries next tick; a nil DB drops it.
