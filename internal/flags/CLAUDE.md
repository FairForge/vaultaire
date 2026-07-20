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
- `Resolved() []Flag` — admin view: default, global row, effective state,
  per-tenant overrides (sorted). Includes unregistered leftover DB keys.

## Registered flags

Declared in `internal/api/flags_wiring.go` (key constants + defaults +
gate sites). Day one: `signups` (default = `SIGNUPS_ENABLED` env; gated at
`auth.CreateUserWithTenant` via `SetSignupsEnabledFunc`) and `chunking`
(default true; gated at the chunked-PUT entry check in `s3_engine_adapter.go`).
Adding a flag = key constant + `Register` call + call site. No schema change.

## Testing

`service_test.go`: nil-DB defaults (unit), precedence/write-through/background
refresh/Resolved (integration, skip without `DATABASE_URL`). Test keys are
uniquified per run and rows cleaned up.
