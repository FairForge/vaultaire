# internal/account

The one account-deletion state machine (WP-R10-3; decisions D-15 / D-16; findings
R10-08, R9-23, R11-13, R12-22). Imported by `internal/api` (management API, user
API, the runner) and `internal/dashboard` (settings page) — it imports nothing
internal, which is what lets both sides share it (the dashboard used to run its
own `UPDATE users` because importing `internal/api` was a cycle).

## `service.go`

- `Schedule(ctx, userID, tenantID, reason)` — one `UPDATE … RETURNING`: sets
  `deletion_scheduled_at = now + GracePeriod` (30 d), `deletion_reason`
  (`CapReason`: trimmed, ≤ 500 runes, default "User requested deletion") and
  `status = 'pending_deletion'` **only when nothing is scheduled** — a second call
  from any entry point returns the first date. Nothing else changes (D-16): login,
  S3, the APIs and export keep working through the grace period.
- `Cancel(ctx, userID)` — clears the three columns; `ErrNoPendingDeletion` when
  nothing was scheduled (the dashboard shows it as a flash, the management API as
  400 `no_pending_deletion`).
- `GetStatus`, `ListDue(ctx, now, limit)` (users `pending_deletion` with a past
  date, tenant joined by e-mail — the registration contract), `StillDue` (re-read
  under `FOR UPDATE` in a short tx: the runner's cancel check before every batch).

## `erase.go` — stage c of the runner

`EraseRows(ctx, userID, tenantID, email, now)` is one transaction: lock the users
row, refuse with `ErrNotPending` if a cancel won (or the date moved), run
`Deleted` in order, then the `Kept` scrubs, users and tenants last. Ids are
compared as **text** everywhere (`tenant_id::text = $1`): prod tenant ids are
`tenant-<hex>` while some scaffold tables typed the column UUID — a bare `= $1`
raised 22P02 and rolled the whole erase back (found by the coherence test).

`Deleted` (the exported slice) covers the old 23-table `ExecuteDeletion` list plus
every survivor R9 area 5 / R13 named: GCI refs released set-based first, then
manifests, head rows, versions, locks, locations, `smart_demotions`,
`multipart_uploads` (+parts), `artifacts`, `bucket_notifications`, `buckets`;
`webhook_deliveries` → `webhook_endpoints` → `events`; `s3_access_log`,
`cdn_access_log`, `cdn_stats_daily`, `access_patterns`, `bandwidth_*`,
`tenant_cost_daily`, `dedup_statistics`, `idempotency_cache`; the quota ledgers
and their `tenant_quotas` children; `tiering_policies`, `retention_policies`,
per-tenant `feature_flags`, `tenant_encryption_keys`, `sts_tokens`,
`admin_notifications`, `admin_notes` (about the tenant AND authored by the user —
its `admin_user_id` FK has no ON DELETE), `change_history`, `dashboard_sessions`;
`api_keys`, `user_mfa`, `mfa_audit_log`, `oauth_accounts`, `user_activities`, the
D-12 compliance tables (`user_consents`, `consent_audit`, `user_roles` — `granted_by`
nulled first, NO ACTION FK —, `breach_affected_users`, `deletion_requests`,
`gdpr_*`, `legal_holds`, `portability_*`), `account_exports`, the user's own
`audit_logs` / `audit_logs_archive` rows (`user_id` or `performed_by` = the user —
the privacy policy says "your own entries are removed with your account"),
`waitlist_signups` by e-mail, then `users`, `tenants`.

`Kept`: `stripe_events` (ledger; `tenant_id` nulled), `audit_logs` /
`audit_logs_archive` rows keyed by the tenant only (operator actions; `ip` and
`user_agent` nulled), `billing_charges`, `metered_usage_reports`,
`abuse_reports`. Never touched: `global_content_index` rows (dedup GC sweeps the
zero-ref ones the release marked), `job_runs`.

**`TestErasedTablesCoverSchema`** walks `information_schema.columns` for every
table with a `tenant_id` / `user_id` / `admin_user_id` / `granted_by` column and
fails when one is in neither list, and prepares every statement against the
migrated schema — a new tenant-scoped table cannot leak past an erasure unnoticed.
The DB-backed `erase_db_test.go` seeds a row in each table the prompt named and
asserts the deleted/kept/scrubbed split; `TestEraseRows_RefusesWhenCancelWon`
pins the guard.

## What is deliberately not here

The runner (Stripe cancel, engine walk, multipart abort, sessions, audit,
scheduling) is `internal/api/deletion_runner.go` — it needs the engine, the GCI
and the quota manager. Export (`R10-27`) is still `internal/api/account_export.go`.
