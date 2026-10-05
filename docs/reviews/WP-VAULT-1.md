# WP-VAULT-1 — the Vault layer (queue item 11), after a warm-up from the #584 review

**Worker session, 2026-10-05. Branch per part, one PR each, from `main` @ #585; each merged and green
on `main` (CI, Deploy, Security) before the next.** Plan queue item 11;
the measurements are `bench-results/VAULTAIRE-PATH-2026-10-04.md` §15–§16. Tests on the private test
database `vaultaire_test_vault`, everything under `-race`; `make lint` 0, `make gosec` 0 before each PR.

| Part | What | PR |
|---|---|---|
| 0 | warm-up: the primary cannot be expired; an allowlist of `0.0.0.0/0` / `::/0` is refused | #587 |
| 1 | Geyser `Get` as parallel ranges | #589 |
| 2 | the parity second copy for `vault`-floor objects (flag `vault_parity`, migration 077) | #590 |
| 3 | `cmd/tools/geyser-restore-probe` | #591 |
| 4 | per-PUT deadline + one retry in the fixed-bucket driver | #592, label fix #594 |

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

### Live proof (prod, 2026-10-05 17:44–17:52 UTC, from SLC with the bench key)

`vault-bench-20261004/v256.bin` (256 MiB, floor `vault`, backend `geyser`, in the landing zone), one
whole-object GET per run — `aws s3api get-object --endpoint-url https://s3.stored.ge` (a single
request: `aws s3 cp` would range it itself and hit `GetRange`):

| Build | `GEYSER_GET_CONCURRENCY` | Wall | Rate | sha256 |
|---|---|---|---|---|
| `ec5fe6b` (before #589) | — (one stream) | 41.8 s | 6.4 MB/s | `80800c19…a53e82dd` |
| `3bed449` (#589), run 1 | unset = 8 | 13.0 s | 20.6 MB/s | `80800c19…a53e82dd` |
| `3bed449` (#589), run 2 | unset = 8 | 11.4 s | 23.6 MB/s | `80800c19…a53e82dd` |
| the origin: Geyser's own endpoint, `aws s3 cp` with the Geyser key, key `t-<tenant>/<tenant>_vault-bench-20261004/v256.bin` | aws-cli's own ranges | 13.2 s | 20.4 MB/s | `80800c19…a53e82dd` |

The three hashes through Vaultaire equal the origin's. `/metrics` on the box, before → after the two
reads: `vaultaire_geyser_get_ranges_total` 0 → **64** (2 × 32 ranges of 8 MiB),
`vaultaire_geyser_get_bytes_per_second` count 0 → 2, sum 45.8 MB/s (mean 22.9 MB/s). 3.2–3.7× on
the default; the bench's 8-stream raw figure was 21.7 MB/s, so the driver is at the measured ceiling
for 8.

### [YOU]

- `GEYSER_GET_CONCURRENCY` for prod: 8 is the default (≈ 22 MB/s raw); 16 measured 41.6 MB/s. Both
  are within Geyser's landing-zone behaviour; the cost is 16 × 8 MiB = 128 MiB of buffer per
  concurrent archive read. Recommend `16` once the live proof below confirms the shape.

## Part 2 — the parity second copy for vault-floor objects

### The gap, seen first

The pricing page sells a second copy for the Vault tiers; the engine made none (§15: "the '+ Lyve
copy' in the cost model is not implemented — the engine treats the OneDrive fleet as a target-only
second-copy role, nothing writes a second copy automatically"). A vault object lived on Geyser alone.
The 2026-10-04 §16.3 measurement said what the copy should be: 4+4 with tape holding the data and
free legs holding the parity reads 64 MiB in 1.4–1.5 s without touching tape and survives losing
Geyser in 0.95 s — for the same byte count as a mirror.

### What the second copy IS, and IS NOT (for the pricing page's sentence — [YOU])

**IS:** Reed-Solomon 4+4 over the object, stripe by stripe (1 MiB per shard per stripe). The four
data shards are not stored: they are byte ranges of the object tape already holds. The four parity
shards — together the object's size, rounded up to whole 4 MiB stripes — are written to the free leg
(`permafrost`, the OneDrive fleet; `lyve` when permafrost is not registered) by the `vault_parity`
job, every two minutes, behind the commit. **Any four of the eight shards rebuild the object, so the
four parity shards alone are a complete copy in information terms**: with Geyser answering an error,
not found through the failover chain, or an open breaker, a GET is served by rebuilding from the
parity (the mutation and the fixture prove it — `TestHandleGet_VaultObjectIsServedFromParityWhen*`);
with a parity shard gone as well, from the data ranges Geyser can still serve plus the parity that is
left (one range per stripe per missing shard). The prompt's "parity alone cannot rebuild" is wrong
for k = m: it is exactly the measured "lose Geyser 0.95 s" case.

**IS NOT:** a byte-for-byte mirror a plain GET can read (a read is a decode — 1–3 GB/s per core, nothing);
**not synchronous** (the window between the commit and the job's pass, two minutes at most, has one
copy — an object deleted inside it never had two); **not on a second paid vendor** (the leg is a free
account; the copy's durability is the leg's — on permafrost that is the fleet's placement, which is why
the shards go through the driver's `Put` and never reorder `TENANT_N`); **not a restore** (an object
on tape, `ErrArchived`, keeps the restore semantics: the fallback never turns "restore it" into a
silent rebuild); **not for chunked objects** (a vault-floor object is never chunked —
`storageClassDisablesChunking` for every class `usage.FloorOf` maps to the attic; asserted by
`TestVaultParity_AChunkedObjectIsNeverVaultFloor`, counted and logged if a row ever breaks it, never
encoded). A pricing sentence that is true today, flag on: *"every Vault object is also written, within
minutes, as a second erasure-coded copy on an independent store, from which it can be served whole if
the tape library is unreachable."* "Second copy" alone is true in information terms; "mirror" and
"second vendor of the same grade" are not.

### What was built

- **Migration 077** `vault_parity` (one row per object; `etag`, geometry, `shard_prefix`, `legs[]`,
  `state complete|partial`, `last_error`, `attempts`, `written_at`).
- **`internal/api/vault_parity_codec.go`**: `encodeParity` streams the object through one 8 MiB stripe
  buffer and writes the four parity shards as streams; `parityRebuilder` is a pull-based reader that
  decodes stripe by stripe for a window `[off, off+n)`, reading the parity streams and asking for a data
  piece (a range of the object) only when fewer than four parity pieces are present; a stripe with
  fewer than four pieces is an error naming the shortfall, never zeros; a parity stream that ends short
  counts as absent from there on.
- **`internal/api/vault_parity.go`**: the `vault_parity` interval job (2 min, boot +45 s, 1 h ceiling,
  500 objects / 256 GiB per run; registered only when a leg is — a build with neither permafrost nor
  lyve logs why): the stale pass (head row gone or etag changed → shards deleted, row deleted), then
  the pending pass (vault-floor, non-chunked, flag on, no complete row for this etag, partial rows
  under 20 attempts). The row is written `partial` BEFORE the first byte lands (a crash leaves a row
  that says so) and `complete` only when all four shards are on the leg; one shard's `Put` failing
  does not cut the others short (`tolerantWriter`) — the row names the shards that landed and the
  error, and the next pass retries. Shards: `<tenant>__parity/<digest(bucket,key)>/<etag>/p<j>`,
  through the leg driver's `Put` with `ContentLength` (permafrost streams it; lyve single-parts it).
- **The read fallback** in `HandleGet`: for a cache-hit vault-floor row whose `engine.Get` (or ranged
  `GetRange`) failed with anything but `ErrArchived`, `VaultParity.Open` is tried (flag on for the
  tenant, row complete for the cached etag + size, leg registered and breaker not open); the 200/206
  carries the cached identity headers and `x-vaultaire-served-from: parity`; a Range request decodes
  only its stripes (the shards are read as ranges when the leg can). Counted in
  `vaultaire_vault_parity_fallback_reads_total{served|unavailable}`.
- **Erasure**: `HandleDelete` erases the shards at once (best effort; the job's stale pass finishes a
  leg that could not be reached); the account-deletion runner's new stage **b1** (after the object
  walk, before the sweep) erases every row's shards and **defers the tenant** when the leg is not
  registered or its breaker is open (`errParityLegUnavailable`, stage `parity` on the record); the
  sweep lists the `_parity` container on every pass, rows or not; `account.Deleted` erases the rows.
- Flag `vault_parity` (default OFF; per tenant first, then `*`), on `/admin/flags`; metrics
  `vaultaire_vault_parity_objects_total{complete|partial|failed|erased}`, `_bytes_total`,
  `_fallback_reads_total{served|unavailable}`; `vault_parity` in `vaultaire-jobs.yml`'s periodic list.

### Same logic on other entry points

Every read of a vault object's bytes: the S3 GET (whole and ranged) has the fallback; HEAD serves
from the cache and needs none; **`/cdn` and CopyObject source reads do not fall back** (a public
vault bucket and a copy out of the attic while Geyser is down keep failing as before — noted, small).
Every path that makes a row stale is covered by the one stale query (overwrite by PUT, multipart
complete, CopyObject destination, DeleteObject — all change or remove the head row's etag). Every
path that must remove shards: DeleteObject (sync), the job (stale), the deletion runner (stage b1),
the sweep (the `_parity` container).

### Adversarial pass (mutations seen red)

| # | mutation / case | result |
|---|---|---|
| M1 | the GET fallback removed | `TestHandleGet_VaultObjectIsServedFromParityWhenItsBackendFails` red |
| M2 | `ErrArchived` falls back too | `TestHandleGet_ArchivedVaultObjectKeepsTheRestoreSemantics` red (403 InvalidObjectState stays) |
| M3 | the delete hook removed | `TestHandleDelete_ErasesTheParityShardsWithTheObject` red |
| M4 | the deletion runner's stage removed | `TestAccountDeletion_ErasesParityShards…` red |
| M5 | the etag guard in `Open` removed | `TestVaultParity_OpenRebuildsTheObjectWhenGeyserIsGone` red |
| M6 | the breaker check in `eraseShards` removed | `TestVaultParity_EraseTenantDefersWhenTheLegCannotBeReached` red |
| M7 | `_parity` not in the sweep plan | `TestSweepPlan_AlwaysListsTheParityContainer` red |
| M8 | the rebuilder serves a stripe with < 4 pieces | both "data ranges that are readable" tests red |
| M9 | `tolerantWriter` dropped (one failed shard cuts the rest) | `TestVaultParity_PartialWriteLeavesARowThatSaysSo` red |
| M10 | chunked vault rows encoded anyway | `TestVaultParity_AChunkedObjectIsNeverVaultFloor` red |
| M11 | the flag not checked in the job | `TestVaultParity_FlagOffTenantIsSkipped` red |
| M12 | the stale pass skipped | `TestVaultParity_StaleShardsAreErased` red |
| M13 | the row inserted as `complete` before the shards land | **survived** — no test kills the process mid-write; the pre-insert is for a crash, by construction |
| M14 | an etag change not treated as stale | `TestVaultParity_StaleShardsAreErased` red |
| — | a parity write that lands on three shards and fails on one | row `partial`, `legs = [permafrost, permafrost, '', permafrost]`, `last_error` names `p2`, three files on the leg; the next pass completes it |
| — | an erasure with the leg's breaker open / the leg unregistered | deferred, nothing erased, rows kept; with the leg back: shards and rows gone, counted |
| — | Geyser's breaker open (five failures through the engine) | the GET is served from parity (the chain ended at the primary's "not found") |

### Tests

`internal/api/vault_parity_codec_test.go` (6), `vault_parity_test.go` (15: the job, the fallback
reader, the S3 GET and DELETE, the account erasure, the sweep plan), on local-driver stand-ins for
Geyser (whole GET and ranges fail on demand) and the leg (one shard's Put fails on demand).

### Decisions

- **D-VAULT-2a** One leg per deployment (`permafrost`, else `lyve`), all four shards on it. The bench's
  2+2 split across two legs survives either leg alone only with the data; one leg keeps the write
  path one driver and the erasure one breaker. `legs[]` is per shard so a split can come later
  without a migration.
- **D-VAULT-2b** The fallback is gated by the flag too: off = the copy is neither written nor read.
  A kill-switch that still reads would be a half-off.
- **D-VAULT-2c** `/cdn` and CopyObject do not fall back. Public vault buckets are not a thing yet.

### Not done

- A Prometheus rule on `vaultaire_vault_parity_fallback_reads_total{outcome="served"}` (a served
  fallback means Geyser failed a customer read) — one rule, when the metric has a day of zeros.
- The dashboard does not show a vault object's copy state; `GET /api/v1/admin/jobs` shows the job's
  last result (`protected`, `partial`, `erased`, `leg`).

### Live proof (prod `846bf76`, 2026-10-05 18:01–18:05 UTC, bench tenant only)

Prod registers both legs (`TENANT_1_ID` and `LYVE_*` are set), so the leg is **permafrost**. The job
had already run `ok` with the flag off. Script: `~/vaultaire-bench/wp-vault-1/parity-proof.sh` on SLC.

| Time | Step | Result |
|---|---|---|
| 18:01:52 | `vault_parity` ON for the bench tenant only (one `feature_flags` row, `updated_by = 'wp-vault-1 canary 2026-10-05'`) | — |
| 18:01:52 | `aws s3api put-object --storage-class GLACIER` of a fresh 24 MiB object, `vault-bench-20261004/wp-vault-1/proof-180152.bin` | head row `geyser`, floor `vault`, not chunked |
| 18:03:27–18:04:02 | the job's next pass | **4 objects protected** (the tenant's three standing vault objects and the proof object), `state = complete`, `legs = permafrost ×4`, 570,425,344 parity bytes; `job_runs.result`: `scanned 6, flag_off 2` (two other tenants' vault rows skipped), `partial 0, failed 0` |
| 18:04:04 | presigned GET through `s3.stored.ge`, tape copy present | 200, no `x-vaultaire-served-from`, sha256 equal |
| 18:04:07 | **the object's blob deleted on Geyser directly** (`aws s3 rm` with the Geyser key; HEAD there: 404) — the tape copy of this one object is lost | — |
| 18:04:08 | the same GET through `s3.stored.ge` | **200, `x-vaultaire-served-from: parity`**, same ETag, class GLACIER, 25,165,824 bytes in 8.7 s, **sha256 equal** |
| | `Range: bytes=5000000-9999999` | **206**, `content-range: bytes 5000000-9999999/25165824`, from parity, ETag and Last-Modified present, slice equal |
| | `aws s3api get-object` | rc 0, sha256 equal |
| 18:04:46 | `aws s3api delete-object` | parity row gone (it is deleted only after its four shards were), head row gone |

`/metrics` before → after: `vaultaire_vault_parity_objects_total{complete}` 0 → 4, `{erased}` 0 → 1,
`_bytes_total` 0 → 570,425,344, `_fallback_reads_total{served}` 0 → **3**, `{unavailable}` 0.

What this proves live: the write behind the commit, the row, a customer GET (whole and ranged, curl
and aws-cli) answered byte-exact from the parity when the tape copy is gone, and the erasure with the
object. What it does not: an open breaker and a Geyser 5xx (not inducible on prod without breaking
Geyser for everyone — the fixture and mutation M1 cover them), and the permafrost-shard-missing mix.

**State left on prod:** the flag is ON for the bench tenant (`tenant-14a623b16b3f7012`) and its three
standing vault objects (`obj8.bin`, `v256.bin`, `v256b.bin`) have parity on permafrost — 520 MB. That
is the canary. Off: `DELETE FROM feature_flags WHERE flag_key = 'vault_parity' AND tenant_id =
'tenant-14a623b16b3f7012'` (or `/admin/flags`). **Turning the flag off does not erase shards already
written** — they go when their object or the tenant goes; there is no "unprotect" pass ([YOU] if
one is wanted).

## Part 3 — `cmd/tools/geyser-restore-probe`

### Why

The restore path of the archive tier is unit-tested (`geyser_restore_test.go`, `s3_restore.go`) and
**untimed on prod**: Geyser moves an object from its landing zone to tape on its own schedule
(~13 days), and until then a GET simply works (§15: "the restore path stays untimed until Geyser
migrates the test objects"). Two objects are waiting: `tier-archive-20261004/obj8.bin` (8 MB) and
`vault-bench-20261004/v256.bin` (256 MiB).

### What was built

An operator tool (never linked into `cmd/vaultaire` — `TestProductBinaryDoesNotLinkTheProbe` runs
`go list -deps`), which talks to the **customer endpoint with a customer key and nothing else**:

- **Every visit** (hourly by default, `-once` for cron) is one HEAD and one 1-byte GET (`bytes=0-0`)
  per object. The 1-byte read is the signal — a HEAD cannot tell the landing zone from tape: the
  class is GLACIER either way, and `x-amz-restore` is absent for an object nobody restored (live:
  `v256.bin` has none; `obj8.bin` shows `ongoing-request="false"` with an expiry that follows the
  clock).
- **Readable:** on the first sighting the object is downloaded once and its SHA-256 kept in the
  state file (the baseline). Later visits never download it again.
- **Refused** (403 `InvalidObjectState` for an attic object; 503 + `Retry-After` for a Smart-demoted
  one — the prompt named the 503, the attic answer is the 403, the probe records whichever it gets):
  the customer path runs with timestamps — the whole GET (refused: status, code, `Retry-After`),
  `RestoreObject` (a 409 `RestoreAlreadyInProgress` is the same wait), a poll of HEAD's
  `x-amz-restore` every `-poll` until `ongoing-request="false"` or the 1-byte read works, then the
  whole GET, hashed. **Identical** = the SHA-256 baseline; with no baseline (first seen already on
  tape), the MD5 of the bytes against the ETag when the ETag is a single-part upload's MD5 (both
  waiting objects are); otherwise null — never a claimed match. One JSON line per timed restore in
  `-report`; the object is then done.
- A restore that does not complete within `-restore-timeout` is reported with the error and **resumed
  by the next visit** (the first on-tape sighting is kept). Bytes that differ fail the run (exit 1).
- The SDK's retries are off (`RetryMaxAttempts: 1`): the probe sees the first answer and its timings
  are the server's.

### Adversarial pass

| # | case | result |
|---|---|---|
| 1 | hourly visits while readable | one baseline download ever; each later visit = 1 HEAD + 1 one-byte GET, no RestoreObject |
| 2 | the refusal is a 503 + `Retry-After: 30` instead of the 403 | recorded as such (`status 503`, `code SlowDown`, `retry_after 30`), the path continues |
| 3 | different bytes after the restore | `identical: false`, both hashes in the report, the run fails |
| 4 | first seen already on tape, ETag not an MD5 | `identical: null`; with an MD5 ETag: compared (`identical_by: etag-md5`), and a mismatch is caught |
| 5 | the restore never completes | report line with `restore not ready after …`, state not done; the next visit completes it and keeps the first on-tape time |
| 6 | a bug caught by the first red run | the refusal step's pointer was overwritten by the final GET (`status 200` in `get_refused`) — separate variables now |

### Tests

`cmd/tools/geyser-restore-probe/probe_test.go`: 9 tests on `fakeArchive` (staged → tape → restoring →
restored, 403 or 503 refusals, a restore that never completes, different bytes after it).

### Live (prod, 2026-10-05 17:52 UTC, on SLC with the bench key, `-once`)

```
tier-archive-20261004/obj8.bin baseline: 8000000 bytes in 0.5 s, sha256 51503108…bab8be94
tier-archive-20261004/obj8.bin readable (landing zone or restored): class GLACIER, x-amz-restore "ongoing-request=\"false\", expiry-date=\"Mon, 05 Oct 2026 17:52:13 GMT\"", check 1
vault-bench-20261004/v256.bin baseline: 268435456 bytes in 11.5 s, sha256 80800c19…a53e82dd
vault-bench-20261004/v256.bin readable (landing zone or restored): class GLACIER, x-amz-restore "", check 1
```

Both objects are still in the landing zone; the baselines are in `~/vaultaire-bench/restore-probe/state/`
(the 256 MiB baseline took 11.5 s — Part 1's ranged read; its hash is the one Part 1 compared with
Geyser's). The timed restore itself is what the hourly visits are waiting for.

## Part 4 — per-PUT deadline in the fixed-bucket driver

### The measurement, seen first

Bench 2026-10-04 (F4, §13.1): Wasabi stalled about **4 % of PUTs for 9–123 s** in both regions — the
request is accepted and nothing comes back. A stall is not an error, so the SDK's retryer never sees
it; the tuned transport sets no response timeout for PUT bodies (`WithResponseHeaderTimeout` exists,
unused there). The load gate's ConcurrentGet p99 and every upload tail on that day were this.

### What was built (`internal/drivers/put_deadline.go`)

- `deadlinePutClient` is the uploader's view of the S3 client (`IDriveDriver.uploadClient()`; the
  fixed-bucket driver = `idrive`, `idrive-<region>`, `wasabi`). The multipart bookkeeping calls pass
  through; **the two calls that carry bytes — `PutObject` and `UploadPart` — get a deadline of
  `FIXED_BUCKET_PUT_TIMEOUT` per 64 MiB of that request's body** (default 60 s; a 16 MiB part gets
  60 s, a body of 65 MiB would get 120 s), and when it passes, **one** retry.
- **The retry is the same request on a connection opened for it**: the body is rewound and the call
  repeated with `o.HTTPClient` = a client whose transport never reuses a connection
  (`WithFreshConnections()`, keep-alives off) — a pooled connection may be the one that stalled.
- **A retry never duplicates a multipart part**: an `UploadPart` goes again with the same part number
  under the same upload id, which S3 defines as replacing that part; the retry is never around the
  streamed upload (which cannot be replayed) and never mints a part. `TestFixedBucketPut_AMultipart
  PartRetryNeverDuplicatesAPart`: part 2 of a 40 MiB upload stalls once — the fake saw part 2 twice
  under one upload id on two connections, parts 1 and 3 once, a `Complete` naming `[1, 2, 3]`, and
  the stored object hashes to the body.
- **Only for silence**: a caller whose own context is done gets nothing replayed; an error answer is
  the SDK retryer's; a body that cannot be rewound returns the deadline error as it is. A second
  deadline is the caller's error ("no answer within 60s, twice (after one retry on a fresh
  connection)") and the engine's failover takes it from there.
- `FIXED_BUCKET_PUT_TIMEOUT`: a Go duration 1s–1h, `0`/`off` disables; a rejected value is logged at
  Warn and the default kept (R13-19). `vaultaire_driver_put_retries_total{driver}` on the server
  registry, at 0 for every fixed-bucket driver from construction.

### Same logic on other entry points

`s3ParallelUpload` has four callers: the fixed-bucket driver (covered — single PUT ≤ 16 MiB, the
manager's single-part path for unknown sizes, every multipart part), and `lyve`, `r2`, `s3compat`,
which keep the bare client: the prompt scoped the deadline to the fixed-bucket driver, Lyve showed no
stalls in the bench, and R2 is the public store. Moving them is passing `deadlinePutClient` — noted.
Engine-level multipart (`UploadPart` from the S3 API) stages parts and commits through the same
driver `Put`, so it is covered there. GETs have no deadline (Part 1's ranged read is cancelled by its
reader).

### Adversarial pass (mutations seen red)

| # | mutation / case | result |
|---|---|---|
| M1 | the retry on the pooled client | red — with two idle connections warmed first, the retry landed on a pooled one |
| M2 | a caller that left is retried | `TestFixedBucketPut_AClientThatLeftIsNotRetried` red |
| M3 | no rewind before the retry | both the single-PUT and the multipart test red |
| M4 | `UploadPart` without the deadline | the multipart test red (18.9 s, counter 0) |
| M5 | a second retry | `TestFixedBucketPut_OneRetryOnly` red |
| M6 | `Put` bypasses the deadline client | red (10 s stall waited out) |
| — | both attempts stall | error is `context.DeadlineExceeded`, "one retry", exactly two attempts, nothing stored |
| — | a 500 answer | not counted as a retry (the SDK's three attempts fit the deadline) |
| — | CI's first run of this PR | red: the multipart test gave 16 MiB parts a 500 ms deadline, and on the runner under `-race` the honest retry of the part took longer ("no answer within 500ms, twice"). The test now cuts 5 MiB parts through a seeded uploader with a 4 s deadline — only the attempt the fake stalls can pass it. The driver was right; the deadline in prod is 60 s per part |
| — | the fixture bug the first run showed | a 1 s deadline was shorter than the SDK's own back-off between 500s; the deadline wraps the SDK's attempts, which the 60 s default fits |

### Tests

`internal/drivers/put_deadline_test.go` (9) on `stallS3`: stalls chosen attempts, records each
attempt's TCP connection, assembles multipart uploads.

### Live check (prod `3bf4d39`, 2026-10-05 18:48 UTC, bench tenant, primary `idrive`)

A stall cannot be induced on prod; this is the regression check of the path every PUT now takes.
`FIXED_BUCKET_PUT_TIMEOUT` unset (60 s). In a fresh bucket, through `s3.stored.ge`:

| Object | How | Wall | Bytes back |
|---|---|---|---|
| 1 MiB | `aws s3api put-object` (the single-PUT path, ≤ 16 MiB) | 0.75 s | equal |
| 40 MiB | `aws s3api put-object` (one request to Vaultaire → three `UploadPart`s to iDrive through the deadline client) | 1.06 s | equal |
| 40 MiB | `aws s3 cp` (client-side multipart, 8 MiB parts) | 1.57 s | equal |

`vaultaire_driver_put_retries_total{driver="idrive"}` 0 and `{driver="wasabi"}` 0, before and after:
no stall occurred, nothing was retried.

**What the check found (fixed in #594):** prod registers ten regional iDrive drivers, and every one
is built by `NewIDriveDriver` under the internal name `idrive` — so they had the deadline and the
retry, but a stalling region would have been counted as the primary's. `IDriveDriver.SetBackendName`
(called by `cmd/vaultaire` with `idrive-<region>`) labels the series with the engine's backend name
and the Warn line names the region; `Name()` is unchanged.
`TestFixedBucketPut_RetriesAreCountedUnderTheBackendName`. Live on `2c5dc43` (19:05 UTC): `/metrics`
carries `vaultaire_driver_put_retries_total` for `idrive`, the ten `idrive-<region>` backends and
`wasabi`, each at 0.

---

## Decisions (all parts)

| Id | Decision | Reverse by |
|---|---|---|
| D-VAULT-0a | An expired key is not rotated (refused), not rotated into a successor without the expiry | one branch in `RotateAPIKey` |
| D-VAULT-0b | `0.0.0.0/0` / `::/0` in an allowlist is a 400, not a warning | the `ones == 0` branches in `ValidateIPAllowlist` and `intersectIPRestrict` |
| D-VAULT-1a | The probe range replaces the HEAD the prompt sketched (one round trip fewer; "no size" is answered by construction) | — |
| D-VAULT-1b | Range size 8 MiB: default buffer 64 MiB per archive read | `geyserGetRangeSize` |
| D-VAULT-2a | One parity leg per deployment (`permafrost`, else `lyve`), all four shards on it; `legs[]` is per shard for a later split | `vaultParityLegs` |
| D-VAULT-2b | The flag gates the read fallback too: off = neither written nor read | one check in `Open` |
| D-VAULT-2c | `/cdn` and CopyObject-source reads do not fall back | — |
| D-VAULT-2d | The fallback never applies to `ErrArchived`: on tape is a state, the restore semantics stay | — |
| D-VAULT-3a | The probe's on-tape signal is a refused 1-byte GET, not HEAD | — |
| D-VAULT-4a | The deadline is per request (`PutObject`, `UploadPart`), never around the streamed upload; scope = the fixed-bucket driver | pass `deadlinePutClient` to lyve/r2/s3compat |

## Not done

- **The timed restore itself.** Both objects are still in Geyser's landing zone; the probe visits
  hourly and writes `~/vaultaire-bench/restore-probe/report.jsonl` when Geyser moves them.
- **A Geyser 5xx / open breaker served from parity on prod** — proven on the fixture (and by the
  deleted-tape-copy case live), not by breaking Geyser.
- `/cdn` and CopyObject-source fallback (D-VAULT-2c); an "unprotect" pass for a tenant whose flag is
  turned off; a Prometheus rule on served fallbacks; the dashboard does not show a vault object's
  copy state.
- The deadline on `lyve`, `r2`, `s3compat` uploads (D-VAULT-4a).
- `docs/API.md` line 43 (`AccessDenied` → `InvalidAccessKeyId` for a revoked key), inherited from #584.
- Mutation M13 (the row pre-inserted `partial` before the bytes land) has no killing test.

## [YOU]

1. **The pricing page's sentence about the second copy.** True today with the flag on: *"every Vault
   object is also written, within minutes, as a second erasure-coded copy on an independent store,
   from which it can be served whole if the tape library is unreachable."* Not true: "mirror",
   "synchronous", "on a second paid vendor". The copy's durability is the free leg's (the OneDrive
   fleet on prod). Decide the sentence, or decide the leg.
2. **`vault_parity` beyond the canary.** It is ON for the bench tenant only
   (`tenant-14a623b16b3f7012`, three objects, 520 MB of parity on permafrost) — the state I left on
   prod. Global: one `*` row at `/admin/flags`. Prod's whole vault floor today is 5 objects / 568 MB
   in 3 tenants. Off for the canary: delete its `feature_flags` row (shards already written stay
   until their object goes).
3. **`0.0.0.0/0` in an allowlist:** refused with a typed 400 (D-VAULT-0b). Say if it should be
   allowed with a warning instead.
4. **`GEYSER_GET_CONCURRENCY` for prod.** Unset = 8 → 11.4–13.0 s for 256 MiB (20.6–23.6 MB/s).
   The bench measured 41.6 MB/s at 16, for 128 MiB of buffer per concurrent archive read instead of
   64. Recommend `16`.
5. **The probe's cron line on SLC** (`crontab -l`, minute 17, under `flock`; the crontab before it is
   saved as `~/vaultaire-bench/restore-probe/crontab.before-wp-vault-1.*`): remove it when
   `report.jsonl` holds both objects. Log: `~/vaultaire-bench/restore-probe/probe.log`.
6. `FIXED_BUCKET_PUT_TIMEOUT` is 60 s per 64 MiB by default; nothing to set. Watch
   `vaultaire_driver_put_retries_total` — a non-zero rate on `idrive` is the Wasabi behaviour
   appearing on iDrive.
7. Install the updated `deploy/monitoring/vaultaire-jobs.yml` on SLC (`vault_parity` joined
   `PeriodicJobStale`).
