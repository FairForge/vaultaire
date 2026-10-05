# WP-VAULT-1 — the Vault layer (queue item 11), after a warm-up from the #584 review

**Worker session, 2026-10-05. Branch per part, one PR each, from `main` @ #585.** Plan queue item 11;
the measurements are `bench-results/VAULTAIRE-PATH-2026-10-04.md` §15–§16. Tests on the private test
database `vaultaire_test_vault`, everything under `-race`; `make lint` 0, `make gosec` 0 before each PR.

| Part | What | PR |
|---|---|---|
| 0 | warm-up: the primary cannot be expired; an allowlist of `0.0.0.0/0` / `::/0` is refused | #587 |
| 1 | Geyser `Get` as parallel ranges | #589 |
| 2 | the parity second copy for `vault`-floor objects (flag `vault_parity`, migration 077) | #590 |
| 3 | `cmd/tools/geyser-restore-probe` | _below_ |
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
