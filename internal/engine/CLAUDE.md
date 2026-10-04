# internal/engine

Core orchestration layer — connects the API layer to storage drivers. This is the middle of the three-layer architecture (API → Engine → Drivers).

## Key Types

- **`CoreEngine`** (`engine.go`) — the main orchestrator. Holds `map[string]Driver` (named drivers), the configured primary, the per-backend circuit breakers (`FailoverManager`) and the object→backend routing cache. Implements the `Engine` interface. The read cache, access tracker, cost optimizer, health-score selector, backup replication and tiering engine were inert in production and were deleted in Review R15 (WP-R6-4/5/6); `Config` is now just `DefaultBackend`.
- **`Engine`** interface (`interface.go:9`) — top-level contract: `Get`, `Put`, `Delete`, `List`, `HealthCheck`, `GetMetrics`, plus future stubs (`Execute`, `Query`, `Train`, `Predict`) and `GetContainerMetadata` / `GetArtifactMetadata` (`interface.go:30-32`), which nothing outside the package calls.
- **`Driver`** interface (`interface.go:38`) — the sacred backend contract: `Name`, `Get`, `Put`, `Delete`, `List`, `Exists`, `HealthCheck`. All storage drivers implement this.

## Request Flow

- **Put**: resolve storage class (the API layer's `resolvePutStorageClass` + `storageClassToBackend` are the ONLY placement inputs — the engine never re-derives placement; the access-tracker "recommendation" override was removed in R6, it had been relocating `auto`-bucket overwrites to Lyve, R6-03) → build WRITE candidate list (`buildWriteCandidateList`: target, primary, then the general-purpose durable backends; **target-only backends** `local`, `r2`, `geyser`, `permafrost` and every `idrive-<region>` receive a write only as the explicit target or the configured primary — WP-F + R6-04) → failover.Execute tries in order → record mapping in `objectBackends` sync.Map + async `object_locations` row. If every eligible backend fails with a genuine backend failure (`isBackendFailure`), Put returns `ErrAllBackendsUnavailable` (API layer → 503 + Retry-After), logs at Error level, and bumps the `write_failures` counter (exposed in GetMetrics) — customer data is never silently stranded on the hub's local disk. Client-level outcomes (quota, invalid input) keep their error identity (403/400, not 503).
- **Get**: check `objectBackends` map (seeded by the API layer's `HintBackend` from `object_head_cache.backend_name` — the routing truth) → `object_locations` on a miss → failover.Execute with candidate list
- **Delete**: resolve the backend like Get (hint / map → `object_locations` → primary; R6-05 — the API layer hints from the head row first) → failover.Execute against `[recorded, primary]`, stopping at the first success → remove from `objectBackends` + `object_locations`
- **List**: delegates to primary driver only (no pagination; the API uses it only as the no-DB fallback)
- **The one address of a chunk blob (WP-R8-7, `chunk_address.go`)**: `ChunkContainer` (`_global`) + `ChunkAddressTenant` (`_global`) in the driver context, built by `ChunkContext(ctx)` — THE helper every chunk Put/Get/Exists/Delete runs on (the API's `chunkStore`, dedup GC, the chunk move, `cmd/tools/dedup-migrate`). Fixed-bucket drivers key `t-<tenant in the context>/<container>/<artifact>`, so before this a chunk lived under its first uploader's prefix and a second tenant's deduplicated object was unreadable. `Put` refuses a write into the chunk container under any other tenant (`ErrChunkAddress`, wraps `ErrInvalidInput`). `LegacyChunkContext(ctx, tenant)` addresses a blob written before — for the read fallback and the chunk move only; it goes with the fallback. `IsReservedTenantID`: `""` or a leading `_` — the S3 front door refuses such a credential, the erasure sweep refuses to list under one. `GetOn` / `PutOn` / `DeleteOn` / `ExistsOn` ask ONE named backend through its breaker (a chunk is on its row's `backend_id` and nowhere else; `Get`'s candidate walk would try every other backend after a miss): a miss is the driver's miss, an unregistered name is `ErrBackendNotRegistered` (never a miss), an open breaker is `ErrAllBackendsUnavailable`. `KeyAddresser` (optional, every driver but quotaless): the key a call would address — the chunk move's proof that an old copy is a different object from the blob before it deletes it.

## Object Location Routing (Phase 7.1-7.2)

Two-tier backend lookup: `objectBackends sync.Map` (L1, in-memory hot cache) → `LocationStore` / `object_locations` table (L2, PostgreSQL durable). On sync.Map miss, the engine queries `object_locations` and seeds the sync.Map so subsequent GETs are fast. Put records to both layers. Delete removes from both.

`LocationStore` (`routing.go`) wraps `*sql.DB` for object location CRUD. All methods are nil-DB safe (degrade to no-op). `last_accessed` is updated on every LookupBackend call (fire-and-forget goroutine); nothing reads it any more (WP-R6-1 drops the touch).

Tables created in migration 048: `object_locations` (routing cache), `tiering_policies` and `tenant_cost_daily` (both orphans since the tiering engine was deleted — D-12). Also adds `last_accessed` column to `object_head_cache`.

## Tiering

There is no age-based tiering engine (deleted in Review R15, WP-R6-4: it never ran, never updated the routing truth and would have moved `_global` chunk rows to tape — R6-10). The only mover of customer data between backends is the Smart-tier demotion job (`api/smart_demotion.go`, 5.15.8).

## Supporting Files

| File | Type | Purpose |
|------|------|---------|
| `types.go` | `Container`, `Artifact` | Domain types (internal names for bucket/object) |
| `errors.go` | `NotFoundError`, `PermissionError` | Error types + sentinels (`ErrQuotaExceeded`, `ErrInvalidInput`, `ErrAllBackendsUnavailable`) |
| `health.go` | `HealthScorer` | Weighted score — scored only by `api/health_handlers.go` for the `/health` / `/status` display (R6-17); **not a routing input**. The only live routing signal is the circuit breaker |
| `load_balancer.go` | `LoadBalancer` | 4 strategies: RoundRobin, LeastConn, WeightedRandom, Adaptive — **unreachable** from the product (no constructor caller; WP-R6-4) |
| `replicator.go` | `Replicator` | Cross-backend replication: Sync, Async (5 workers), Quorum — **unreachable** (WP-R6-4) |
| `capacity.go` | `CapacityPlanner` | Linear regression to predict when backends fill — **unreachable** (WP-R6-4) |
| `disaster_recovery.go` | `DisasterRecovery` | Failover configs, recovery plans (RTO/RPO) — **unreachable** (WP-R6-4) |
| `sla.go` | `SLAMonitor` | SLA compliance tracking, violation detection — **unreachable** (WP-R6-4) |
| `analytics.go` | `Analytics`, `BackendMetrics`, `BackendStats` | Per-backend request/latency stats — **unreachable** (WP-R6-4) |
| `failover.go` | `FailoverManager`, `BackendCircuitBreaker` | Per-backend circuit breaker (5 failures/60s → open, 30s → half-open → probe) + ordered failover execution |
| `storage_class.go` | `ResolveStorageClass`, `BackendToStorageClass`, `CustomerStorageClass`, `IsArchiveClass` | S3 storage class ↔ backend name mapping (STANDARD→idrive, GLACIER→geyser, etc.). **`CustomerStorageClass(floor, backend)` is the one place the class a customer sees is derived** (WP-R13-1): a `standard`-floor object is never an archive class whatever backend holds it (a Smart-demoted object on `geyser` is STANDARD); a `vault`-floor object reports its backend's class. `BackendToStorageClass` alone is for operator views and for non-current version rows (no floor on record) |
| `interface.go` | `Restorer`, `RestoreStatus` | Optional driver interface for archive backends (V18.2): `RestoreObject(days)` + `RestoreStatus` (raw x-amz-restore passthrough). Geyser implements it. `ErrArchived`/`ErrRestoreAlreadyInProgress` sentinels in errors.go; `ErrArchived` is client-level for the breaker AND stops failover iteration (other backends would mask the archived state as 404) |
| `interface.go` | `TenantWalker`, `TenantObject` | Optional driver interface (WP-R10-3c): `WalkTenant(ctx, tenantID, fn)` hands out every object one tenant holds on the backend, in any container, each with a `Remove` bound to the key the listing returned. Contract in the comment: empty / `/`-bearing ids refused, a separator bounds the listing, every page read or an error, a key from outside the prefix aborts. The shared chunk container is not filtered — the caller (the erasure sweep, `api/erasure_sweep.go`) decides. Implemented in `drivers/tenant_walk.go` |
| `routing.go` | `LocationStore` | PostgreSQL-backed object location CRUD (RecordLocation, LookupBackend, RemoveLocation, CountByBackend, TouchLastAccessed) — nil-DB safe |

## Bucket Tier Preference (Phase 7.5)

`bucketTierStorageClass()` in `s3_engine_adapter.go` queries `tier_preference` from `buckets` and maps it to an S3 storage class: performance→STANDARD, standard→STANDARD, archive→GLACIER. Used in HandlePut when no explicit `x-amz-storage-class` header is present — does not override explicit headers. `auto` returns empty string (normal routing applies).

## Per-Bucket Region Routing (Phase 5.14.7)

`GetDriver(name string) (Driver, bool)` — returns a named driver from the registry. Used by the S3 adapter to route PUT operations directly to a region-specific iDrive driver (e.g., `idrive-eu-west-1`) when a bucket has a non-default region.

`HintBackend(container, artifact, backend string)` — seeds `objectBackends` so GET routes to the correct backend without a failed failover attempt. The S3 adapter calls this with `backend_name` from `object_head_cache` on every GET to ensure correct routing after restart.

## Circuit Breaker (Phase 5.12.4)

Each registered backend gets an independent `BackendCircuitBreaker`:
- **Closed** (healthy): all requests pass through
- **Open** (broken): after 5 consecutive failures within 60s — all requests rejected
- **Half-Open** (probing): after 30s in open state — allows one probe; success → closed, failure → open

`FailoverManager.Execute(ctx, candidates, fn)` iterates the candidate list in order, skipping backends with open breakers, recording success/failure. Returns the first successful backend name.

**Verdict when nothing succeeds (R6-02):** the FIRST candidate is the recorded/target backend. If it answered with a client-level outcome (not found, quota, archived, consumed body) that is the verdict — `all backends failed: <that error>` — and failures elsewhere do not change it. If it was unavailable (open breaker, timeout, 5xx, refused), nothing a fallback said — including "not found" — is a verdict about the object: the aggregate is `ErrAllBackendsUnavailable` (API → 503 + Retry-After, never 404). Before R6 an iDrive outage answered every GET with 404 NoSuchKey because the walk ended on `local`'s ENOENT. A cancelled request context stops the walk immediately (the client is gone; no other backend is tried or charged).

**`isBackendFailure` taxonomy** (mirrored by `api/not_found.go` `isObjectMissingErr` — keep in sync): misses are `NotFoundError` (value or pointer), `os.ErrNotExist`, and aws-sdk-go-v2 chains carrying the `NoSuchKey`/`NotFound` API codes or HTTP 404 — checked **by type** (`smithy.APIError`, `*smithyhttp.ResponseError`), because the SDK formats the status as `StatusCode: 404` and renders an empty-body 404 as `api error NotFound: Not Found`, neither of which the old string list matched (R6-01: every such miss opened breakers). `NoSuchBucket` is a backend failure (misconfigured backend). `context.Canceled` is neither — the client hung up (R6-06). `context.DeadlineExceeded`, 5xx, 403 (dead key), connection/DNS errors are failures. `failover_sdk_errors_test.go` produces the shapes from a real `s3.Client` against `httptest`.

## Storage Class Routing (Phase 5.12.4)

`x-amz-storage-class` header on PUT maps to a target backend:
- STANDARD → the primary (unmapped since 2026-10-03, when Wasabi became the interim primary: a fixed `idrive` entry sent every Standard PUT to iDrive whatever the primary was), GLACIER/DEEP_ARCHIVE → geyser, RESILIENT → lyve, PUBLIC → r2; STANDARD_IA and REDUCED_REDUNDANCY unmapped (→ primary at STANDARD)
- RESILIENT → lyve (internal class — what the `resilient` bucket `tier_preference` resolves to via `tierPreferenceToStorageClass` in `api/s3_engine_adapter.go`; NOT the removed STANDARD_IA mapping: objects land on Lyve at its default class, never Lyve's IA service tier)
- PUBLIC → r2 (internal class, never sent by clients — set by `api.resolvePutStorageClass` for public-read buckets when an `r2` driver is registered; R2 is the public-bucket / CDN origin only, not a tier)

If the target backend isn't registered, falls back to primary silently. Storage class is a hint, never an error.

`BackendToStorageClass(backendName)` is the reverse mapping; responses to customers (GET/HEAD/listings/inventory/dashboard) go through `CustomerStorageClass(floor, backendName)`, which never reports a downstairs object in an archive class.

## Review history

`docs/reviews/R6-engine.md` (2026-09-26) — findings R6-01..27, the "how an object gets placed and found" narrative, the driver contract table R7 verifies against, and WP-R6-1..9. Fixed in that PR: R6-01/06 (breaker taxonomy), R6-02 (outage ≠ 404), R6-03 (intelligence placement), R6-04 (target-only backends), R6-05 (Delete location).

## Connection to Other Layers

- **API layer** (`internal/api/s3_engine_adapter.go`) wraps `CoreEngine` to translate S3 protocol → engine calls
- **Drivers** (`internal/drivers/`) implement the `Driver` interface; registered via `eng.AddDriver(name, driver)` in `main.go`
- **Quota** (`internal/usage/`) — quota accounting lives entirely in the API layer since WP-1 (single reservation site in `handlePutObject`, atomic displaced-size capture on head-cache upserts, releases on delete). The engine does no quota work.
