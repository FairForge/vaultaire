# internal/idrive

The iDrive e2 **Reseller API** client (WP-CAP-1 B; full endpoint reference
`.private/IDRIVE_RESELLER_API.md` — not in the repo). Base
`https://api.idrivee2.com/api/reseller/v1`, auth = one static header
`token: <key>` — god-mode for the whole reseller fleet, so the key is
handled like the master key and never logged.

- **`LoadResellerKey(getenv, readFile)`** — the key by the master key's
  precedence (WP-R8-1 A5): the systemd credential
  `$CREDENTIALS_DIRECTORY/idrive_reseller_api_key`, then
  `IDRIVE_RESELLER_API_KEY_FILE`, then `IDRIVE_RESELLER_API_KEY`. A source
  that is set but unreadable is `ErrResellerKeySource` — `api/server.go`
  turns it into a refused boot, never "vendor view off". `""` = not set.
- **`ResellerClient`** (`NewResellerClient(baseURL, key)`; "" = the live
  API; every request bounded 15 s): only the two READ calls the
  `backend_capacity` job needs are built — `Users(ctx, email)` (`GET
  /users`, `storage_used` in decimal GB — the caller converts and
  sanity-checks the unit) and `UsageStats(ctx, email, from, to)` (`POST
  /usage_stats` → daily `uploads`/`downloads`/`disk_used` bytes). Answers
  are decoded bare or unwrapped from a `data`/`users`/`stats` envelope (the
  console wraps some answers in `data`, the doc shows others bare); an API
  error comes back as its envelope's `type/code: message`, never the key.
- The provisioning calls (create_user, enable_user_region,
  create_access_key — the doc's `region_key` is `region` live, and
  `create_access_key` answers `data.access_key`) belong to the reseller
  integration proper (plan § 11), not this package yet.

Tests: `reseller_test.go` (httptest with the live shapes; the key sources).
