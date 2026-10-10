# internal/flags

Runtime feature flags (1.13 live-iteration kit): global kill-switches AND
per-tenant enablement, flipped via the admin API / dashboard with no deploy or
restart. Backed by the `feature_flags` table (migration 059).

## Model

- One row per (flag_key, tenant_id). `tenant_id = '*'` (`GlobalTenant`) is the
  global row — a sentinel, not NULL, because NULL can't be in a PK.
- **Resolution precedence:** tenant row → global row → registered in-code
  default. Unregistered key with no row = disabled.
- **Caching:** the whole table (tiny) is loaded into an in-memory snapshot,
  swapped under a mutex; background `Start(ctx)` loop refreshes every ~15s.
  `Set`/`Unset` write through and reload immediately, so an admin flip is
  visible on the flipping node's next request; other nodes converge within
  the refresh interval.
- **Nil-DB safe:** `New(nil, logger)` serves registered defaults only;
  `Set`/`Unset` return `ErrNoDatabase`; `Refresh`/`Start` are no-ops.

## API

- `New(db, logger)` → `*Service`; `Register(key, default)` per flag;
  `Refresh(ctx)` once at boot; `Start(ctx)` for the loop.
- `Enabled(key, tenantID) bool` — hot path, no DB access. Empty tenantID =
  global-only check (used by `signups`).
- `Set(ctx, key, tenantID, enabled, updatedBy)` / `Unset(ctx, key, tenantID)` —
  upsert/delete + immediate cache reload. `updatedBy` should be the admin's
  email (from JWT or dashboard session).
- `Registered(key)` — used by the admin API to reject typo'd keys loudly.
- Every `Set`/`Unset` writes an `audit_logs` row (`flag.set` / `flag.unset`,
  resource `flag:<key>`, metadata tenant/enabled/updated_by) via
  `internal/audit` — Review R11-09; the actor is the JWT / session user from
  the context. Nil DB = no row.
- `Resolved() []Flag` — admin view: default, global row, effective state,
  per-tenant overrides (sorted). Includes unregistered leftover DB keys.

## Registered flags

Key constants live in `internal/api/flags_wiring.go` (the two dashboard ones
are re-exported from `dashboard/handlers`); the `Register` calls are in
`api.NewServer` (`registerFlags`). Eight flags today:

| Flag | Default | Gate site |
|------|---------|-----------|
| `signups` | `SIGNUPS_ENABLED` env (unset = on) | `auth.CreateUserWithTenant` via `SetSignupsEnabledFunc` (global-only; also the landing form in `api/landing.go`) |
| `chunking` | true | the chunked-PUT entry check in `api/s3_engine_adapter.go` (`chunkingEnabled(tenantID)`) |
| `smart_demotion` | false | per tenant inside `SmartDemotionRunner.RunOnce` (`api/smart_demotion.go`) |
| `quota_checkout` | false | the dashboard billing page's house checkout (`dashboard/handlers/billing_house.go`) |
| `house_overview` | false | the house on the dashboard overview (`dashboard/handlers/overview.go`) |
| `egress_throttle` | false | the egress allowance as a rate cap (WP-R10-9; `api/egress_throttle.go`) |
| `vault_parity` | false | per tenant inside `VaultParity.RunOnce` (the job writes shards) and `VaultParity.Open` (the read fallback) — `api/vault_parity.go`, WP-VAULT-1 |
| `parallel_get` | false | per tenant in `api.S3ToEngine.openParallelGet` (`api/s3_large_get.go`): a whole object ≥ `LARGE_GET_PARALLEL_MIN_BYTES` on a fixed-bucket S3 backend is read as parallel hedged ranges behind one client stream. Canary per tenant, then global |
| `sync_backend` | false | per tenant in `api.resolvePutStorageClass` (`syncPlacementGate`): a bucket with `tier_preference = 'sync'` (operator-set) places on the Sync.com WebDAV bridge (`sync` driver) only for a tenant with the flag. **Never a global row** — Sync's terms forbid reselling the service without its written consent |

Adding a flag = key constant + `Register` call + call site (+ a `flagInfos`
entry in `dashboard/handlers/admin_flags.go`). No schema change.

## Testing

`service_test.go`: nil-DB defaults (unit), precedence/write-through/background
refresh/Resolved (integration, skip without `DATABASE_URL`). Test keys are
uniquified per run and rows cleaned up.
