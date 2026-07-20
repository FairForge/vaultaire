# internal/engine

Core orchestration layer — connects the API layer to storage drivers. This is the middle of the three-layer architecture (API → Engine → Drivers).

## Key Types

- **`CoreEngine`** (`engine.go`) — the main orchestrator. Holds `map[string]Driver` (named drivers), primary/backup selection, access tracking, tiered cache, cost optimizer. Implements the `Engine` interface.
- **`Engine`** interface (`interface.go:9`) — top-level contract: `Get`, `Put`, `Delete`, `List`, `HealthCheck`, `GetMetrics`, plus future stubs (`Execute`, `Query`, `Train`, `Predict`).
- **`Driver`** interface (`interface.go:38`) — the sacred backend contract: `Name`, `Get`, `Put`, `Delete`, `List`, `Exists`, `HealthCheck`. All storage drivers implement this.

## Request Flow

- **Put**: resolve storage class → build WRITE candidate list (`buildWriteCandidateList`: target, primary, other DURABLE backends — WP-F fail-loudly excludes `local` unless it is the target or the configured primary) → failover.Execute tries in order → record mapping in `objectBackends` sync.Map → invalidate cache → optionally replicate to backup async. If every eligible backend fails with a genuine backend failure (`isBackendFailure`), Put wraps the error in `ErrAllBackendsUnavailable` (API layer → 503 + Retry-After), logs at Error level, and bumps the `write_failures` counter (exposed in GetMetrics) — customer data is never silently stranded on the hub's local disk. Client-level outcomes (quota, invalid input) keep their error identity (403/400, not 503).
- **Get**: check `objectBackends` map for backend name → check tiered cache (L1) → failover.Execute with candidate list → cache result
- **Delete**: failover.Execute against recorded backend (+ primary fallback) → remove from `objectBackends` → invalidate cache
- **List**: delegates to primary driver only

## Object Location Routing (Phase 7.1-7.2)

Two-tier backend lookup: `objectBackends sync.Map` (L1, in-memory hot cache) → `LocationStore` / `object_locations` table (L2, PostgreSQL durable). On sync.Map miss, the engine queries `object_locations` and seeds the sync.Map so subsequent GETs are fast. Put records to both layers. Delete removes from both.

`LocationStore` (`routing.go`) wraps `*sql.DB` for object location CRUD. All methods are nil-DB safe (degrade to no-op). `last_accessed` is updated on every LookupBackend call (fire-and-forget goroutine) for tiering age tracking.

Tables created in migration 048: `object_locations` (routing source of truth), `tiering_policies` (Phase 7.3), `tenant_cost_daily` (Phase 7.4). Also adds `last_accessed` column to `object_head_cache`.

## Tiering Engine (Phase 7.3)

`TieringEngine` (`tiering.go`) runs a background goroutine that periodically scans `object_locations` for objects eligible for tier migration based on `tiering_policies`. Follows the BackendMonitor Start/Stop pattern (ticker + stop chan + ctx.Done select loop).

- `Start(ctx)` — background loop, default 1-hour interval. No-op if DB is nil.
- `Stop()` — close(stop)
- `runScan(ctx)` — loads policies from `tiering_policies`, falls back to hardcoded default (90-day→geyser/GLACIER) if no policies exist. Finds candidates via `object_locations` WHERE `last_accessed < NOW() - min_age_days`. Excludes objects in buckets with a non-auto `tier_preference` (`AND NOT EXISTS (SELECT 1 FROM buckets WHERE ... tier_preference != 'auto')`) so pinned buckets are never age-migrated. Processes up to 100 per policy per tick.
- `migrateObject(...)` — crash-safe sequence: Get→Put→UpdateDB→UpdateSyncMap→Delete. Never deletes source until routing update succeeds. If put fails, source is untouched (safe).

Integrated into CoreEngine: `tiering` field, created in `NewEngine()`, started via `StartTiering(ctx)` (call after drivers are registered), stopped in `Shutdown()`.

## Supporting Files

| File | Type | Purpose |
|------|------|---------|
| `types.go` | `Container`, `Artifact` | Domain types (internal names for bucket/object) |
| `errors.go` | `NotFoundError`, `PermissionError` | Error types + sentinels (`ErrQuotaExceeded`, `ErrInvalidInput`, `ErrAllBackendsUnavailable`) |
| `context.go` | helpers | `WithTenantID`, `TenantIDFromContext`, `WithRequestID` |
| `selector.go` | `BackendSelector` | Chooses backend by health score (≥50 = healthy) |
| `cost_optimizer.go` | `CostOptimizer` | Routes by size: small→fastest, large→cheapest |
| `health.go` | `HealthScorer` | Weighted 0-100 score (latency 0.3, errors 0.3, uptime 0.2, throughput 0.2) |
| `load_balancer.go` | `LoadBalancer` | 4 strategies: RoundRobin, LeastConn, WeightedRandom, Adaptive |
| `monitor.go` | `BackendMonitor` | Periodic health probes (30s), stores to `backend_health` table |
| `replicator.go` | `Replicator` | Cross-backend replication: Sync, Async (5 workers), Quorum |
| `migrator.go` | `Migrator` | Data migration between backends with worker pools |
| `capacity.go` | `CapacityPlanner` | Linear regression to predict when backends fill |
| `disaster_recovery.go` | `DisasterRecovery` | Failover configs, recovery plans (RTO/RPO) |
| `sla.go` | `SLAMonitor` | SLA compliance tracking, violation detection |
| `failover.go` | `FailoverManager`, `BackendCircuitBreaker` | Per-backend circuit breaker (5 failures/60s → open, 30s → half-open → probe) + ordered failover execution |
| `storage_class.go` | `ResolveStorageClass`, `BackendToStorageClass` | S3 storage class ↔ backend name mapping (STANDARD→idrive, GLACIER→geyser, etc.) |
| `routing.go` | `LocationStore` | PostgreSQL-backed object location CRUD (RecordLocation, LookupBackend, RemoveLocation, CountByBackend, TouchLastAccessed) — nil-DB safe |
| `tiering.go` | `TieringEngine` | Background age-based object migration between backends (1h interval, crash-safe Get→Put→UpdateDB→Delete) |

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

## Storage Class Routing (Phase 5.12.4)

`x-amz-storage-class` header on PUT maps to a target backend:
- STANDARD → idrive, STANDARD_IA → lyve, GLACIER/DEEP_ARCHIVE → geyser
- ONEZONE_IA → onedrive, REDUCED_REDUNDANCY → local

If the target backend isn't registered, falls back to primary silently. Storage class is a hint, never an error.

`BackendToStorageClass(backendName)` provides the reverse mapping for GET/HEAD responses.

## Connection to Other Layers

- **API layer** (`internal/api/s3_engine_adapter.go`) wraps `CoreEngine` to translate S3 protocol → engine calls
- **Drivers** (`internal/drivers/`) implement the `Driver` interface; registered via `eng.AddDriver(name, driver)` in `main.go`
- **Quota** (`internal/usage/`) — quota accounting lives entirely in the API layer since WP-1 (single reservation site in `handlePutObject`, atomic displaced-size capture on head-cache upserts, releases on delete). The engine does no quota work.
