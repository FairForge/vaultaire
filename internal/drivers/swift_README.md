# Blomp — the `blomp` driver (OpenStack Swift)

`swift.go` is a generic OpenStack Swift driver behind Keystone v2, written for
Blomp and registered as **`blomp`** when `BLOMP_PASSWORD` is set. Target-only
and a strict target, like `sync`: objects land there only for a bucket whose
`tier_preference = 'blomp'` (operator-set, SQL only) of a tenant with the
`blomp_backend` flag (default off). Never the primary (`STORAGE_MODE=blomp` is
a boot Fatal), never a fall-over destination.

## What Blomp is (probed live 2026-10-21, account partner@fairforge.io)

| | |
|---|---|
| Auth | Keystone **v2**, `https://authenticate.blomp.com/v2.0/tokens` (also `authenticate.ain.net`), tenant `storage`, user = the account e-mail, password. A token lives **1 hour** |
| Storage URL | from the catalog: `http://swiftproxy.acs.ai.net:8080/v1/AUTH_<id>` — **plain HTTP**, no TLS on any port |
| Containers | ONE, named after the e-mail. Account HEAD/listing 403, creating another container 403, container metadata POST 403 (so no TempURL key, no public ACL) |
| Quota | `x-container-meta-quota-bytes` on a container HEAD: 40 GiB on the free account. Enforced late: on 2026-10-21 it accepted 41.7 GiB (667 × 64 MiB at 273 MB/s) before answering `413 Upload exceeds quota.` — keep ~5 % headroom. The driver returns `engine.ErrBackendFull` for a 413/507 (`SwiftError.Unwrap`): the client gets 507 `InsufficientStorage` (never 503 "retry"), no breaker charge, alert `BackendStorageFull` |
| Cluster | Swift 2.17.1; `max_file_size` 5 GiB, `max_object_name_length` 1024, listing limit 10,000; `slo`, `bulk_delete`, `ratelimit`, `swift3`, `tempurl` enabled |
| Large objects | **SLO and DLO work with the pieces in the SAME container.** The community docs' "Permission denied" is rclone writing pieces to `<container>_segments`, which Blomp refuses |
| S3 (swift3) | unusable: SigV4 → `SignatureDoesNotMatch`, and the bucket name has an `@` |
| Writes | rate-limited per container: a 1 KiB PUT 0.3–8 s, 16 in flight = 2.75 PUT/s (p50 3.5 s, max 13.6 s). Reads on a kept-alive connection ≈ 60 ms (the RTT from SLC is 49 ms) |
| Throughput from SLC | one 64 MiB stream 16–39 MB/s up, 12–79 down; 256 MiB GET 122 MB/s single, 163 with 8 ranges; 32 × 64 MiB in parallel 150 MB/s up / 199 down with ~6 % 503; run-to-run ×4 variance |
| Listing | eventually consistent: counts and names lag writes/deletes by tens of seconds |
| Quirks | `DELETE ?multipart-manifest=delete` on a plain object answers **200** with `"Not an SLO manifest"` in the body; overwriting a large object leaves its old pieces |
| Terms | blomp.com/terms-and-conditions (2020): no explicit resale ban; a free account is closed after 1 year without access |

## Layout

`t-<tenant>/<container>/<artifact>` in the account's container, the tenant
from the call's context (`ErrNoTenant` without one) — the fixed-bucket shape.
An object larger than `BLOMP_SEGMENT_SIZE` (64 MiB), or of unknown length past
8 MiB, is a static large object: pieces at
`t-<tenant>/_segments/<sha256(name)[:16]>/<generation>/<n>` in the same
container, each MD5-checked against its ETag, then the manifest at the key.
A known size grows the piece to `ceil(size / 1000)` when that is larger
(Swift's `max_manifest_segments` is 1000 on Blomp's `/info`), so the limit
is 1000 × 5 GiB; a body of unknown length past 1000 pieces is refused
(`InvalidInput`). A failed upload stops its other pieces and, once they have
all stopped, bulk-deletes every piece it started (one write); the erasure
walk (`WalkTenant`) lists `_segments` too. Our writes never overwrite in place
(every whole-object write has a new address since WP-R2-1), so the leftover
pieces of an overwritten large object are not a product case.

## Behaviour

- **Large uploads in parallel** (2026-10-21, after the prod bench): one
  stream to Blomp carries 37–62 MB/s (a 256 MiB PUT 62, a 1 GiB multipart
  Complete 54), sixteen 64 MiB PUTs in parallel 275 MB/s. So the pieces go
  up `BLOMP_PUT_CONCURRENCY` (4) at a time. Each piece is read into memory
  first — a buffer from a pool, under a process-wide budget of
  `BLOMP_PUT_BUDGET` bytes (1 GiB = 16 pieces of 64 MiB, a semaphore taken
  before the buffer; an upload waits for it, never fails) — so a piece is
  **replayable**: sent up to 5 times, 1/2/4/8 s apart plus jitter. That is a
  correctness fix, not only speed: on prod 2026-10-21 11:05–11:06 UTC three
  6 GiB multipart uploads failed at Complete because Blomp answered `503` to
  a streamed 1 GiB piece (once after 633 MB of it) and a streamed body
  cannot be sent again. Memory: ≤ concurrency × segment per upload,
  ≤ the budget for the process.
- **Parallel ranged GET**: `GetRangeInfo` (`engine.VersionedRangeGetter`)
  reports the ETag (an SLO's is Swift's MD5 of its pieces' ETags, the same
  on every range of one manifest) and the total size from `Content-Range`,
  so the `parallel_get` flag reads a large Blomp object as 8 ranges at once:
  raw, 8 ranges of 256 MiB read 163 MB/s; one stream of a 1 GiB object 56.
  **Parts are the segment size** (`PreferredRangeBytes`, engine.RangePartSizer),
  not `LARGE_GET_PART_BYTES`: every range pays ~650 ms before its first byte,
  so 16 MiB parts gained little. SLC lab 2026-10-21, 1 GiB object through
  Vaultaire, sha-verified: one stream 32–51 MB/s, 8 × 16 MiB 58–157, **8 × 64
  MiB 123–369**, 16 × 64 MiB 98–358 (no better than 8), 16 × 16 MiB 47–66.
  A Blomp read holds up to 8 × 64 MiB = 512 MiB of `LARGE_GET_PARALLEL_BUDGET_BYTES`
  (2 GiB): the fifth concurrent one takes the single stream.
- **Token**: asked once and reused; asked again 5 min before it expires, and
  once on a 401 (`vaultaire_swift_auths_total{backend,reason,outcome}`). A
  changed password fails the next request after the token goes (≤ 1 h) — the
  probe's container HEAD then fails.
- **Retries**: a 5xx or a transport error is retried up to 3 attempts in all
  when the body can be sent again (no body, or a body ≤ 8 MiB held in
  memory) — 5 for a piece of a large object; a streamed body (a known size
  between 8 MiB and the segment size) is sent once
  (`vaultaire_swift_retries_total`).
- **Delete**: HEAD first (a HEAD is not rate-limited), then ONE write in the
  caller's time — a plain DELETE; for a large object its manifest is read
  (`?multipart-manifest=get`), deleted alone, and its pieces go by one bulk
  delete in the background (bounded 2 min + 0.5 s a piece, ≤ 15 min; a
  failure is a Warn and the erasure walk lists them under `_segments`).
  Never Swift's `?multipart-manifest=delete`: it removes the pieces one by
  one inside the request, and at Blomp's ~3 writes a second a 6 GiB object
  of 96 pieces outran the 15 s bound — every DELETE of it answered 503
  (prod 2026-10-21). A miss is `engine.NotFoundError`.
- **Hedge** (`swift_hedge.go`, 2026-10-21): Blomp's proxies sometimes give
  a request no first byte for ~5 s (any proxy IP, fresh or kept-alive
  connections) where the usual answer takes 60–650 ms — the 5 s tails of the
  stored.ge → Blomp path were all this. A HEAD (the probe's container HEAD,
  an object HEAD), a listing page or a **GET** with no answer after
  `DRIVER_HEDGE_AFTER` (2 s; `0`/`off` = none) is sent once more on a
  connection opened for it; the first answer of any status wins, the other
  is cancelled and a late answer's body closed unread. A GET is hedged on
  its headers, before any byte of its body is read, so the winner's body is
  the whole answer — that is why Swift hedges a GET where the S3-class
  drivers never do (theirs go through the SDK and rarely stall). **Never
  hedged:** a PUT (the bytes twice), the bulk delete, and a DELETE: its
  status is the delete's verdict, and a hedge's 404 arriving while the first
  attempt deleted would report a miss (for a large object, skip its
  pieces); the HEAD before it is hedged, and a second DELETE would spend one
  of Blomp's ~3 writes a second. `vaultaire_driver_hedges_total{driver="blomp",op}`
  / `_hedges_won_total` (op = head_bucket | head_object | list | get).
- **Range**: 206 served as is; a 200 to a Range is skipped to the offset and
  cut.
- **Bounds** (the bound invariant, `CLAUDE.md`): 25 s to the first byte after
  the body; HEAD, DELETE, a listing page and the token request 15 s whole; a
  job's walk page 60 s; the failed-upload cleanup 2 min.
- **Probe**: `HealthCheck` = a token (when none is current) + container HEAD;
  the object canary (`api/backend_canary.go`) HEADs and writes
  `t-_probe/_probe/<host>`.
- `CleartextTransport()` is true until a token names an https storage URL.

## Known limit — the listing lags

Measured 2026-10-21: a bulk delete fed from a container listing taken right
after 128 small PUTs reported every name it was given as deleted, and ~30
objects written in that minute were still there 90 minutes later — the
listing had not named them yet. `WalkTenant` (the erasure sweep) and any
cleanup that starts from a listing can therefore miss objects written in the
last minute(s); the deletion runner's second pass narrows it, it does not
close it. (Whole objects with a row are deleted by their address, never
from a listing.)

**Closed for the erasure (RB-1, 2026-10-21):** the driver declares the lag
(`engine.ListingLagger`, `ListingLag()` = `BLOMP_LISTING_SETTLE`, 3 h).
On such a backend the account-deletion runner erases the rows as before but
records `account.erasure_settling`, not `account.erased`: the tenant is
declared erased only after a walk that STARTED at least the lag after the
end of the row erase (keys gone, sessions revoked, auth cache evicted — the
moment no write for the tenant can land) and found nothing. Every walk
deletes what it finds; one that finds something is walked again an hour
later; one that cannot run defers the tenant (the run fails, retried
hourly). The owed walks live in the `account_deletion` job's
`job_runs.result` (`settling`). See `internal/api/erasure_settle.go`.

## Capacity — the vendor view

`VendorUsage` (`engine.VendorUsageReporter`) is one container HEAD:
`X-Container-Bytes-Used` and, when a quota is set, `X-Container-Meta-Quota-Bytes`
(Blomp's free account: 42,949,672,960 = 40 GiB). The account HEAD is 403 on
Blomp. The daily `backend_capacity` job (08:30 UTC) records it as
`vendor.blomp` in `job_runs.result`; `/metrics` exports
`vaultaire_backend_vendor_used_bytes{backend="blomp"}` and
`vaultaire_backend_vendor_limit_bytes{backend="blomp"}`, and
`BackendVendorQuota80` / `BackendVendorQuota90` warn and page at 80 / 90 % of
the quota (`deploy/monitoring/vaultaire-capacity.yml`) — before the 413 that
becomes a 507 and `BackendStorageFull`.

Mind what the counter is:

- **It lags.** Measured 2026-10-21: the HEAD said 1,746,932,237 bytes in 31
  objects while the listing named 34 objects of 3,021,996,109 bytes — both
  trail writes and deletes by minutes. A daily reading is fine for growth; a
  fill at 273 MB/s (40 GiB in ~2.5 min) outruns it, and only the 507 alert
  catches that.
- **It overshoots.** Swift admits a write in flight past the quota: 41.7 GiB
  went into the 40 GiB account before the first 413.
- **It counts everything in the container**, not only what our rows name:
  the lab instance's objects, pieces an SLO upload left when it crashed
  (RB-2), the canary. `BackendVendorDrift` (vendor > 1.2 × our stored + 1 GiB
  for 3 days) is the signal that those have grown.

## Encryption — plain HTTP

Rule 4 of `docs/ARCHITECTURE_CONTRACT.md`: no surface invents a key, so the
driver does not encrypt. `api.decidePlacement` seals every write of the
`BLOMP` class with SSE-S3 (the tenant key hierarchy) whatever the bucket says,
the customer's SSE-C key aside, and refuses it (503) on a process with no
master key; CopyObject into a `blomp` bucket seals the same way. The Vault
parity job never makes a cleartext-transport driver its leg — the four parity
shards of an unsealed object rebuild it.

**`BLOMP_ALLOW_UNSEALED=true`** (the owner's testing decision, 2026-10-21)
lifts both: blomp-tier writes follow the bucket's own encryption setting and
Blomp may be the parity leg. One Warn line at boot says it is set. Remove it
before any customer data goes there.

## Configuration

| Variable | Default | |
|---|---|---|
| `BLOMP_PASSWORD` | — | set = the driver is registered (never logged) |
| `BLOMP_USER` | — (required with the password) | the account e-mail |
| `BLOMP_AUTH_URL` | `https://authenticate.blomp.com/v2.0` | Keystone v2 base |
| `BLOMP_TENANT` | `storage` | |
| `BLOMP_CONTAINER` | the user | |
| `BLOMP_SEGMENT_SIZE` | `64MiB` | 1 MiB (Swift's min_segment_size) – 5 GiB; above it an object is pieces |
| `BLOMP_PUT_CONCURRENCY` | `4` | 1–16 pieces of one object in flight |
| `BLOMP_PUT_BUDGET` | `1GiB` | bytes of pieces held by every upload of the process; at least one segment |
| `BLOMP_ALLOW_UNSEALED` | off | see above |
| `BLOMP_LISTING_SETTLE` | `3h` | how long the listing may lag a write (1m–48h, or `off` — a lab only): the account erasure's wait before an empty walk proves the tenant gone |

## Live tests

`go test ./internal/drivers/ -run 'TestSwift|TestBlomp'` runs every behaviour
test on an in-process fake (`swift_fake_test.go`) and, with `BLOMP_USER` +
`BLOMP_PASSWORD` set, again against the real account (segments of 1 MiB; a
random `_test-<hex>` tenant per test, removed at the end; listing assertions
skip on the live run because the listing lags):

```
BLOMP_USER=partner@fairforge.io BLOMP_PASSWORD="$(security find-generic-password -s blomp -w)" \
  go test ./internal/drivers/ -run 'TestSwift|TestBlomp' -count=1 -v
```

Pairings: `cmd/tools/erasure-bench` registers `blomp` from the same variables
(`-layout local:4,blomp:4`, `geyser:4,blomp:1,lyve:1,sync:1,permafrost:1`, …).
