# internal/engine

Core orchestration layer — connects the API layer to storage drivers. This is the middle of the three-layer architecture (API → Engine → Drivers).

## Key Types

- **`CoreEngine`** (`engine.go`) — the main orchestrator. Holds `map[string]Driver` (named drivers), primary/backup selection, access tracking, tiered cache, cost optimizer. Implements the `Engine` interface.
- **`Engine`** interface (`interface.go:9`) — top-level contract: `Get`, `Put`, `Delete`, `List`, `HealthCheck`, `GetMetrics`, plus future stubs (`Execute`, `Query`, `Train`, `Predict`).
- **`Driver`** interface (`interface.go:38`) — the sacred backend contract: `Name`, `Get`, `Put`, `Delete`, `List`, `Exists`, `HealthCheck`. All storage drivers implement this.
- **`QuotaManager`** interface (`engine.go:20`) — `CheckAndReserve(ctx, tenantID, bytes)` and `ReleaseQuota(ctx, tenantID, bytes)`.

## Request Flow

- **Put**: cost optimizer recommends backend → write to chosen driver → record mapping in `objectBackends` sync.Map → invalidate cache → optionally replicate to backup async
- **Get**: check `objectBackends` map for backend name → check tiered cache (L1) → fetch from driver → cache result
- **Delete**: delete from ALL backends → remove from `objectBackends` → invalidate cache
- **List**: delegates to primary driver only

## Important: objectBackends is in-memory

`objectBackends sync.Map` maps object keys to backend names. Lost on restart — falls through to primary driver for objects written before the restart. Phase 7 (Smart Tiering) replaces this with PostgreSQL-backed `object_locations`.

## Supporting Files

| File | Type | Purpose |
|------|------|---------|
| `types.go` | `Container`, `Artifact` | Domain types (internal names for bucket/object) |
| `errors.go` | `NotFoundError`, `PermissionError` | Error types + sentinels (`ErrQuotaExceeded`, `ErrInvalidInput`) |
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

## Connection to Other Layers

- **API layer** (`internal/api/s3_engine_adapter.go`) wraps `CoreEngine` to translate S3 protocol → engine calls
- **Drivers** (`internal/drivers/`) implement the `Driver` interface; registered via `eng.AddDriver(name, driver)` in `main.go`
- **Quota** (`internal/usage/`) implements `QuotaManager`; injected via `eng.SetQuotaManager()`
