# WP-VAULT-1 — the Vault layer (queue item 11), after a warm-up from the #584 review

**Worker session, 2026-10-05. Branch per part, one PR each, from `main` @ #585.** Plan queue item 11;
the measurements are `bench-results/VAULTAIRE-PATH-2026-10-04.md` §15–§16. Tests on the private test
database `vaultaire_test_vault`, everything under `-race`; `make lint` 0, `make gosec` 0 before each PR.

| Part | What | PR |
|---|---|---|
| 0 | warm-up: the primary cannot be expired; an allowlist of `0.0.0.0/0` / `::/0` is refused | _below_ |
| 1 | Geyser `Get` as parallel ranges | _below_ |
| 2 | the parity second copy for `vault`-floor objects (flag `vault_parity`, migration 077) | _pending_ |
| 3 | `cmd/tools/geyser-restore-probe` | _pending_ |
| 4 | per-PUT deadline + one retry in the fixed-bucket driver | _pending_ |

## Part 0 — warm-up (post-merge review of #584)

### The bugs, seen first

**(a) The primary pair could be expired.** `SetAPIKeyExpiration` checked `RevokedAt` and not
`IsPrimary`, so `POST /api/v1/user/apikeys/{id}/expire` on the primary answered 200 and wrote
`expires_at` on the one key the account cannot replace from the outside: the export download link,
the presigned-URL route and the credentials page all mint from it (WP-R5-14), and rotation copied
`ExpiresAt` onto the successor — so once expired, every rotation handed out another expired primary.
`TestUserKeyAPI_DaysMustBeAtLeastOne` had been *asserting* the 200 on the primary. Prod, read-only
before the fix: 0 primary rows carry an expiry, 0 are expired.

**(b) `0.0.0.0/0` / `::/0` were accepted.** `ValidateIPAllowlist` took any CIDR `net.ParseCIDR`
accepts; an allowlist of `0.0.0.0/0` is stored, shown as a restriction, and restricts nothing. The STS
route had the same hole when the parent key has no allowlist (`intersectIPRestrict` returns the
requested list as is; with a parent allowlist a `/0` already failed the overlap check). Prod: 0 live
keys with such an entry.

### What was built

- `auth.ErrPrimaryKeyExpire` (the `ErrPrimaryKeyRevoke` shape): `SetAPIKeyExpiration` refuses the
  primary; the user API answers **409 `primary_key`** ("cannot be given an expiry; rotate it instead").
  Rotating first changes nothing — the successor is the primary and refuses too.
- `RotateAPIKey` refuses an **expired** key, primary or scoped, with `auth.ErrKeyExpired`: the
  successor inherits the expiry and would be born dead. User API **409 `key_expired`**; dashboard
  flash "create a new key instead". A refused rotation revokes nothing and mints nothing.
- `auth.ErrUnrestrictedIPAllowlist`: any CIDR with prefix length 0 (`0.0.0.0/0`, `::/0`, `1.2.3.4/0`),
  alone or hidden among valid entries. Management API **400 `unrestricted_ip_allowlist`**
  (`param: ip_allowlist`, "leave ip_allowlist empty to allow every address"); dashboard form sentence
  with the entry; STS `ip_restrict` → 400 `scope_error` ("restricts nothing; omit ip_restrict to
  inherit the parent key's allowlist"). OpenAPI text updated for the four routes.

### Same logic on other entry points (audit)

Every production caller of `GenerateAPIKey` (dashboard form, user API, management API) goes through
`KeyCreateOptions.Validate` → `ValidateIPAllowlist`; `RotateAPIKey` has two callers (dashboard, user
API) and `SetAPIKeyExpiration` one (user API — neither the management API nor the dashboard has an
expiry route; a future one inherits the service guard). The only other allowlist writer is the STS
mint (`intersectIPRestrict`), covered. `0.0.0.0/1` + `128.0.0.0/1` together also admit everything and
are not refused — a deliberate two-entry construction, not a mistake (kept out, noted).

### Adversarial pass

| # | case | result |
|---|---|---|
| 1 | expire the primary, then rotate, then expire the successor | 409 `primary_key` both times (`TestUserKeyAPI_PrimaryCannotBeExpired`) |
| 2 | a primary row that already carries a past expiry (stamped straight into the cache) | rotation refused with `ErrKeyExpired`, the row stays live, no successor (`TestRotateAPIKey_ExpiredKeyIsRefused`) |
| 3 | `::/0` hidden in `["203.0.113.7","10.0.0.0/8","::/0","2001:db8::/32"]` | refused on the service, 400 on the management API, sentence on the dashboard, no key created |
| 4 | `1.2.3.4/0`, ` 0.0.0.0/0 ` (padded) | refused — the check is on the parsed prefix length |
| 5 | `0.0.0.0/1`, `128.0.0.0/1`, `::/1` | accepted (real networks) |
| 6 | STS `ip_restrict: ["203.0.113.7","0.0.0.0/0"]` from a parent with no allowlist | 400 `scope_error`, no `sts_tokens` row |

### Tests

`internal/auth/primary_key_expiry_test.go`, `key_hygiene_test.go` (+2), `internal/api/
user_api_keys_hygiene_test.go` (+3; the pre-existing days test now expires a scoped key),
`credential_lifecycle_api_test.go` (+1, DB), `internal/dashboard/handlers/apikeys_test.go` (+2).

### Decisions

- **D-VAULT-0a** An expired key is not rotated (refused), rather than rotated into a successor without
  the expiry. A scoped key that expired is replaced by a new key; the primary can no longer reach that
  state. If a primary row with a past expiry ever exists (none on prod), the operator clears
  `expires_at` on the row — recorded here so nobody adds a "rotate clears it" path by accident.
- **D-VAULT-0b** `0.0.0.0/0` / `::/0` are a 400, not a warning. [YOU] Isaac: reverse to
  "allowed with a warning" if a customer asks; the check is one `ones == 0` branch in
  `ValidateIPAllowlist` and one in `intersectIPRestrict`.

### Not done

- `docs/API.md` line 43 ("a revoked key answers `AccessDenied`" → `InvalidAccessKeyId` since
  WP-R5-14): still one word for the next docs pass, as #584 noted.

## Part 1 — Geyser `Get` as ordered parallel ranges

### The measurement, seen first

Bench 2026-10-04 §16.1: the same 256 MiB object in Geyser's landing zone reads at **5.4 MB/s as one
stream and 41.6 MB/s as 16 concurrent ranges** (raw, from the box; 10.7 → 40.8 through the origin),
every range byte-verified. The Vault read path was slow because it read one stream, not because of
tape. The driver's `Get` was one `GetObject`.

### What was built (`internal/drivers/geyser_get_ranges.go`)

- **The probe.** `Get` issues ONE request, `bytes=0-8388607`. Its `Content-Range` gives the size,
  its ETag the identity. What comes back decides the shape with no further request: an object on tape
  answers `InvalidObjectState` → one `ErrArchived` (the API's one 503 + `Retry-After`, never eight
  requests to Geyser); a 200 (a backend that ignores `Range`) is the whole object and is returned as
  is; an object that fits the probe is done; a 206 whose total is `*` is served as the probe's bytes
  followed by one open-ended `bytes=N-` stream.
- **The window.** The remaining ranges (8 MiB each) are started in order, each when one of
  `GEYSER_GET_CONCURRENCY` slots is free (the probe holds a slot while it is being read). A range is
  fetched into a channel of 256 KiB pieces sized to hold the whole range — the fetcher never waits
  for the reader — and handed over in order, piece by piece, so the first byte is the probe's first
  piece. **A slot is released when the reader has consumed the range**, so at most
  `concurrency × 8 MiB` (64 MiB by default — Put's RAM budget) is ever buffered, and a slow reader
  does not pile up completed ranges.
- **Guards.** Every range's ETag is compared with the probe's (an overwrite between ranges is "object
  changed during read", never two versions spliced); its `Content-Range` must be the one asked for; a
  body that ends short is an error with the byte count; a 200 for a LATER range is sliced to that
  range (discard the prefix, read the length) — slow, right. The first failure of any stream cancels
  the rest and is what the reader sees, not the cancellations it caused.
- **Leaving.** `Close` (and the request context) cancels every stream, starts no more, and waits for
  the fetchers, so nothing outlives the reader (`TestGeyserGet_CloseStopsTheRangesNotYetStarted`:
  a reader that left after one range of 64 had < 16 started and 0 in flight afterwards).
- `GEYSER_GET_CONCURRENCY`: 1–64, default 8 (`1` = the plain GET with no `Range`); a rejected value
  is logged at Warn and the default kept (R13-19 rule; `TestGeyserGetConcurrency_EnvRule`).
- Metrics on the server registry via `drivers.Collectors()`: `vaultaire_geyser_get_ranges_total`
  (every request the path issued) and `vaultaire_geyser_get_bytes_per_second` (histogram, 1 MB/s …
  512 MB/s, observed when a Get delivers its last byte — the single stream too).
- `GetRange` is untouched: one native request (`TestGeyserGetRange_KeepsItsNativeSingleRequest`).
  The API's ranged GETs and the clients that parallelise themselves (aws-cli, R2 Sippy > 199 MiB)
  choose their own ranges.
- **The probe replaces the HEAD the prompt sketched**: one round trip fewer on a backend with ~0.8 s
  per request, and it answers "no size" (a 200, or a `*` total) by construction. The prompt's "below
  2 × the range size → single stream" becomes "fits the probe → one request; otherwise the rest is
  windowed" — an object of 1.5 ranges costs two requests instead of one, which is the only
  difference.

### Same logic on other entry points

`engine.Get` is the one caller (the S3 GET, the CDN, the Smart promotion copy-back, the erasure
sweep's reads, the restore probe) — all get the ranged read. `GetRange` and `RestoreStatus` unchanged.
The trap in the prompt (a 206 must carry ETag + Last-Modified on every path): the CDN path sets both
on the writer before `serveRange`, so a `/cdn` 206 already carried them; it is now pinned by
`TestCDN_RangeRequest_PartialContent` (ETag, Last-Modified, Accept-Ranges) so it cannot go bare.

### Adversarial pass

| # | case | result |
|---|---|---|
| 1 | backend answers 200 + whole body to the probe | returned as the object, one request, hash equal |
| 2 | backend answers 200 to the THIRD range only | that range is sliced out of the whole body; hash equal |
| 3 | object on tape (403 `InvalidObjectState`) | `ErrArchived` after exactly one request |
| 4 | ETag flips from the third request on | reader error "object changed during read" |
| 5 | a range body 100 bytes of 1 MiB | reader error naming the shortfall; bytes delivered < object |
| 6 | reader closes after the first range of 64 (30 ms per request) | < 16 started, in-flight drains to 0, nothing starts after Close |
| 7 | `GEYSER_GET_CONCURRENCY` = `0`, `-2`, `abc`, `65`, `8.5` | Warn + default 8; `16`, `1`, `64` taken |
| 8 | three runs under `-race` | 0 races (the first-error slot is under a mutex) |

### Tests

`internal/drivers/geyser_ranges_test.go` (11 tests on the `rangeS3` fake: records every `Range`
header and the peak in flight), `internal/api/cdn_test.go` (+3 assertions).

### Live proof

_After the deploy of this part — recorded below the Part 2 section once run against `s3.stored.ge`._

### [YOU]

- `GEYSER_GET_CONCURRENCY` for prod: 8 is the default (≈ 22 MB/s raw); 16 measured 41.6 MB/s. Both
  are within Geyser's landing-zone behaviour; the cost is 16 × 8 MiB = 128 MiB of buffer per
  concurrent archive read. Recommend `16` once the live proof below confirms the shape.
