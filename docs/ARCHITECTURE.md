# Architecture

How Vaultaire is put together and how an object gets placed and found. This
describes the code on `main` as verified in the engine, driver and database
reviews (`docs/reviews/R6-engine.md`, `R7-drivers.md`, `R8-crypto-dedup.md`,
`R9-database.md`). Nothing here is aspirational; where a review named a
follow-up work package it is cited by id.

## Three layers

```
API layer      internal/api          S3 protocol translation, auth, HTTP handlers,
                                     placement decision, head cache, quota
Engine layer   internal/engine       Driver map, write/read candidate lists,
                                     circuit-breaker failover, location cache
Driver layer   internal/drivers      One implementation of engine.Driver per
                                     storage provider
```

`cmd/vaultaire/main.go` builds the engine, registers drivers from environment
variables, sets the primary backend, connects PostgreSQL (optional; the process
degrades to a no-DB fallback) and starts the HTTP server. Shutdown order is
fixed: HTTP drain and tracker flush first, then the engine (which closes the
DB).

There is one process, one PostgreSQL and one set of drivers. There is **no**
hub/spoke topology, no stateless worker fleet, no driver registry and no
machine-learning router. Earlier versions of this file described those; they
were never built.

## Dual terminology

| S3-facing | Internal | Example |
|-----------|----------|---------|
| Bucket | Container | `"<tenantID>_photos"` |
| Object | Artifact | `2026/img.jpg` |
| Key | Path | same string |

The API layer authenticates the request and fixes the tenant from the
credential alone. Every engine call receives `container = "<tenantID>_<bucket>"`
(`internal/tenant/tenant.go`), `artifact = key`, and the tenant in `ctx`
(`common.TenantIDKey`). Drivers that share one physical bucket across tenants
(iDrive, Geyser, R2, Lyve) additionally prefix keys with `t-<tenant>/`. Dedup
chunks live in the shared container `_global` under `_chunks/{hash}` (or
`_chunks/{tenant}/{hash}` when encrypted). A driver keys by the tenant in `ctx`
or by the container name, never by anything it derives; `common.GetTenantID`
falls back to `"default"` when the context carries no tenant (R6-22, a known
hazard for background callers).

## The driver contract

`engine.Driver` (`internal/engine/interface.go`) is what every backend
implements: `Name`, `Get`, `Put`, `Delete`, `List`, `Exists`, `HealthCheck`.
Optional interfaces: `RangeGetter` (byte-range reads) and `Restorer` (Geyser
tape recall). The full per-method contract and the per-driver conformance
tables are in `docs/DRIVERS.md`.

`engine.Engine` is what the API layer calls. Its `Put` returns
`(backendName, error)`: the name of the driver that stored the bytes. The
interface still declares `Execute`, `Query`, `Train`, `Predict`,
`GetContainerMetadata`, `GetArtifactMetadata` and `GetMetrics`; none of them
does anything meaningful (`GetContainerMetadata` returns a `time.Now()` stub —
R6-20) and WP-R6-4 removes them.

## Placement on PUT

`resolvePutStorageClass` (`internal/api/s3_engine_adapter.go`) is the single
placement decision for PUT and multipart complete:

1. Bucket `tier_preference` (`buckets.tier_preference`), when it is not
   `auto`/`standard`: `archive` → `GLACIER`, `resilient` → `RESILIENT`,
   `performance` → `STANDARD`. An archive bucket still honours an explicit
   `DEEP_ARCHIVE` header.
2. Otherwise, a public-read bucket with an `r2` driver registered → the
   internal class `PUBLIC` (an explicit `GLACIER`/`DEEP_ARCHIVE` header wins).
3. Otherwise the client's `x-amz-storage-class` header (`STANDARD`, `GLACIER`,
   `DEEP_ARCHIVE`; anything else counts as no preference).
4. Otherwise `""` — the primary backend.

The engine maps the class to a driver name (`internal/engine/storage_class.go`):

| Class | Backend | Note |
|-------|---------|------|
| `STANDARD` | `idrive` | |
| `GLACIER`, `DEEP_ARCHIVE` | `geyser` | tape; objects stored whole |
| `RESILIENT` | `lyve` | our tier name, not an AWS class; Lyve's own default class |
| `PUBLIC` | `r2` | internal only; public buckets / CDN origin, never a tier |
| `REDUCED_REDUNDANCY` | `local` | hub disk |
| unknown / `STANDARD_IA` | primary | class is a hint, never an error |

If the target driver is not registered the write falls back to the primary
silently. The primary is `STORAGE_MODE`, or auto-detected
(`config.DetectStorageMode`) as iDrive > Wasabi > Quotaless > S3 > Geyser >
local (prod: `wasabi` since 2026-10-03, the interim primary while the iDrive
account is repaired; `idrive` stays registered for its rows). The dashboard
reads the engine's primary (WP-R1-4). STANDARD is the primary's class and is
not pinned to a backend. No driver sets a vendor storage class on its
upstream request.

**Region-pinned buckets bypass the engine.** A bucket whose `buckets.region` is
not the default region (`IDRIVE_REGION`, prod `us-central-1`) is written
straight to the `idrive-<region>` driver by `bucketRegionDriver`; if that
driver is not registered the PUT fails rather than landing on the primary.

`RESILIENT`, `GLACIER`, `DEEP_ARCHIVE` and `PUBLIC` objects are never chunked
(`storageClassDisablesChunking`): chunk blobs always land on the primary in
`_global`, which would break the tier's placement promise.

## Write candidates, failover and the breaker

`buildWriteCandidateList` (`internal/engine/engine.go`) produces
`[target, primary, general-purpose durable backends]`. `local`, `r2`, `geyser`,
`permafrost` and every `idrive-<region>` are **target-only**: they receive a
write only when they are the resolved target or the configured primary
(R6-04). Lyve is the only general failover leg.

`FailoverManager.Execute` (`internal/engine/failover.go`) walks the list in
order, skipping backends whose circuit breaker is open. Each backend has an
independent breaker: 5 failures within 60 s open it, it stays open 30 s, then
one half-open probe decides. A seekable body (`*bytes.Reader` from the chunk
path) is rewound before each retry; a non-seekable body that has been partially
consumed stops the walk with `ErrNoFailover` so no backend receives a truncated
object. If every eligible backend fails with a genuine backend failure, `Put`
returns `ErrAllBackendsUnavailable` (API → 503 + `Retry-After`), logs at Error
and increments `vaultaire_backend_write_failures_total`. Client-level errors
(quota, invalid input, archived) keep their identity.

## Recording the location

`Put` returns the backend name and the API layer persists it in
`object_head_cache.backend_name` in the same transaction as size, ETag and
content type. **That column is the routing truth.** The engine also caches it
in the in-memory `objectBackends` map and writes an `object_locations` row
asynchronously (migration 048); both are caches of the head-cache value and
may lag or drift — smart demotion updates the head row only (R6-14, WP-R6-1).

## Reads

- **HEAD** is answered from `object_head_cache` and never touches a backend
  (the one exception is the live `x-amz-restore` passthrough for GLACIER-class
  objects).
- **GET / range / CDN / chunk fetch**: the API layer reads the head row, calls
  `HintBackend(container, key, backend_name)`, then `engine.Get`. The engine
  resolves the preferred backend as hint → `objectBackends` →
  `object_locations` → primary, then walks `[preferred, primary, every other
  registered backend]` with the same breaker-aware `Execute`
  (`buildCandidateList`). Today a miss on the preferred backend still fans out
  to every backend; WP-R6-1 narrows this to the recorded location. Range reads
  use `RangeGetter` where the driver implements it — `idrive` (and every
  `idrive-<region>`), `lyve`, `geyser`, `r2`; `local`, `s3`, `quotaless` and
  `permafrost` get `Get` plus a discard of the leading bytes.
- Archived objects on Geyser return `ErrArchived`, which stops the walk (a
  fallback's 404 must not mask it) and maps to 403 `InvalidObjectState`, or to
  503 + auto-restore for smart-demoted objects.

**Not found vs. unavailable.** `isBackendFailure`
(`internal/engine/failover.go`) and `isObjectMissingErr`
(`internal/api/not_found.go`) are two halves of one taxonomy, kept in sync:

| Outcome | Examples | Breaker | S3 answer |
|---------|----------|---------|-----------|
| miss | typed `NotFoundError`, `os.ErrNotExist`, SDK HTTP 404 / `NoSuchKey` / `NotFound` | not charged | 404 `NoSuchKey` |
| backend failure | timeout, 5xx, connection/DNS error, `NoSuchBucket`, 403 (dead key) | charged | 503 (if nothing else served) |
| neither | `context.Canceled` (client gone) | not charged | walk stops |

If the recorded/preferred backend was unavailable, a fallback's not-found is
not a verdict: the read fails with `ErrAllBackendsUnavailable` → 503, never
404 (R6-02).

## Delete

The API layer hints the recorded backend; the engine tries `[recorded,
primary]` and stops at the first success (R6-24: a second copy left by a
demotion grace period is not swept by the engine — the smart-demotion ledger
covers its own case). The head row is removed with
`DELETE … RETURNING size_bytes`, which releases quota, even when the backend
already reports the object missing. Chunked objects decrement GCI refcounts
instead; the dedup GC sweep deletes blobs later.

## Health

Two models, deliberately separate:

- **Probes** (`internal/api/backend_probes.go`) feed `/health`,
  `/health/backends`, `/metrics` and alerting only. They are authenticated: a
  signed `HeadBucket` through the driver for iDrive (primary and every region
  with its own pair), Geyser and R2; the Lyve console action with the root key
  (else the Lyve driver's signed `HeadBucket`); an authenticated Graph call
  for permafrost; `local` is probed too (a stat of `DATA_PATH`). `quotaless`
  keeps a plain TCP dial and `s3` (s3compat) is not probed at all (rule 1 in
  the root `CLAUDE.md`).
- **The circuit breaker**, fed by live request outcomes, is the **only** signal
  that alters routing.

`HealthScorer` (`internal/engine/health.go`) is scored only for the `/health`
and `/status` display; it is not a routing input. The cost optimizer, the
health-score selector, the age-based tiering engine, the engine read cache and
the access-pattern tracker were inert and were deleted in Review R15 (WP-R6-4,
WP-R6-5, WP-R6-6). The only mover of customer data between backends is the
Smart-tier demotion job (`internal/api/smart_demotion.go`), gated by the
`smart_demotion` feature flag (default off). There is no 7/30/90-day
age-tiering policy in effect.

## Chunking, dedup, encryption

For Standard-floor objects the API layer splits the body with restic's
**Rabin** fingerprint chunker (`internal/crypto/chunker.go`,
`github.com/restic/chunker` v0.4.0, `RabinChunker`) — not FastCDC. Its identity
(library + version, polynomial `0x2ADD89E3B790BB`, 1 MiB min, 20 average bits,
16 MiB max) is one constant set, recorded on every chunked object's
`pipeline_config` and pinned by a golden-boundary test; the real average chunk
is about **2 MiB** (WP-R8-4 kept it — a change resets dedup for everything
stored).
Each chunk is content-hashed, optionally zstd-compressed (level 3, skipped for
incompressible content types or when it does not shrink), and stored once in
`_global`; `global_content_index` holds one row per distinct chunk with a
refcount, `tenant_chunk_refs` is the per-object manifest, and a GC sweep
deletes blobs whose refcount reached zero after a grace period.

Compress-then-encrypt applies only when SSE is on. Chunk encryption and SSE-S3
require `ENCRYPTION_MASTER_KEY` (`internal/api/server.go`); **prod does not
set it today**, so every stored chunk is plaintext (R8). When set, chunks are
sealed with a convergent key derived per tenant and a nonce bound to the
sealed bytes (R8-01). Key rotation and crypto-shredding are not implemented
(WP-R8-1).

## Public surfaces

All served by one chi router (`internal/api/server.go`):

| Surface | Path | Notes |
|---------|------|-------|
| S3 API | `/*` catch-all → `handleS3Request` | SigV4; tenant from the credential |
| Management API | `/api/v1/manage/*` (`management_routes.go`), `/api/v1/admin/*` | JSON; JWT / admin JWT; `Idempotency-Key` |
| Auth | `/auth/register`, `/auth/login`, `/auth/password-reset*` | rate-limited |
| Dashboard | `/dashboard/*` (`internal/dashboard`) | PostgreSQL-backed sessions |
| CDN | host `cdn.stored.ge` → `/{slug}/{bucket}/*`; also `/cdn/{slug}/{bucket}/*` | public buckets; 404 for a miss, 503 when backends are unavailable |
| Landing / docs | `/`, `/docs/*`, `/docs/api`, `/openapi.json`, `/changelog`, `/llms.txt` | `internal/api/landing`, `internal/docs` |
| Ops | `/health`, `/health/live`, `/health/ready`, `/health/backends`, `/metrics`, `/version`, `/status` | |
| Billing | `/webhook/stripe` | mounted only when both Stripe secrets are set |
| Misc | `/api/waitlist`, `/.well-known/security.txt`, `/og.png` | |

The schema behind all of this — by domain, with the migration runner and the
invariants — is in `docs/DATABASE.md`.
