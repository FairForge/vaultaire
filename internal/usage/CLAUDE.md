# internal/usage

Quota and allowance bookkeeping in PostgreSQL. Everything here is in LOGICAL bytes.

## Files

- **quota_manager.go** — `QuotaManager`: `tenant_quotas` (the single total quota). `CheckAndReserve` / `ReleaseQuota` are the WP-1 reservation pair (`SELECT … FOR UPDATE`, one transaction); `ReconcileStorageUsage` / `ReconcileTenantStorageUsage` rewrite usage from `object_head_cache` (never call the global one from a test — the test database is shared).
- **floor.go** — the house: `tenant_floor_quotas` (066), one row per (tenant, `standard`|`vault`). `SetHouse` / `ClearHouse` are written by the Stripe webhook and are idempotent; `CheckAndReserveFloor` / `ReleaseFloor` move the total and the floor together; `FloorOf(storageClass)` decides the floor of a write. `TB` = 2^40.
- **free_tier.go** — `FreeTierLimits` (5 GiB, 1 bucket, 1 API key).
- **egress.go** — the egress allowance (WP-R10-9), **the one definition** read by the throttle and the alerter (`internal/api/egress_meter.go`, `bandwidth_alerts.go`), the dashboard overview, the admin tenant page and `/api/v1/user/usage`:
  - `QuotaManager.EgressAllowance(ctx, tenantID)` — derived at read time, one query. Precedence: admin override (`tenant_quotas.bandwidth_limit_bytes > 0`) → the house (Standard ratio × the `standard` floor limit + Vault ratio × the `vault` floor limit) → no house (Standard ratio × `storage_limit_bytes`; the free tier's 5 GiB gives 2.5 GiB). Ratios come from `prices.json` through `landing.Get()` (`EgressRatios`). Nothing is seeded: `SetHouse`/`ClearHouse` never write `bandwidth_limit_bytes`, so an override survives a webhook replay and the end of a subscription. Override 0/NULL = the plan's allowance (not "unlimited" — exempting a tenant is a per-tenant `egress_throttle` flag row).
  - `EgressMonthStart` / `EgressResetAt` — the month is the **UTC calendar month** for every reader; `MonthEgressBytes` sums `egress_bytes` only (uploads never count — Review R13-16).
  - `EgressThrottle` (`DefaultEgressThrottle`, `Rate`) — the D-25 knobs: `max(MinBytesPerSec, Factor × allowance / 2,592,000 s)`, `MaxStreams`.
  - `EgressStatus` / `EgressStatusReader` / `EgressStatusFromDB` / `EgressOver` — a tenant's position this month; the API server's live counter implements the reader, `EgressStatusFromDB` is the same answer without responses in flight.

## Tests

DB-backed against `vaultaire_test` (`make test-db`); each test creates its own tenant (`newTestTenant`) and touches only its rows. `egress_test.go` pins the ratios to `prices.json` and the override-survives-SetHouse/ClearHouse requirement.
