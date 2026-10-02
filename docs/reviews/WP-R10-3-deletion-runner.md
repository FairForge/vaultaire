# WP-R10-3 — the account-deletion runner, built

**Built 2026-09-30 on `wp/R10-3-deletion-runner` (PR #529), from `main` @ #528.**
**Decisions this was built under:** D-15 (the runner ships before launch) and D-16
(`pending_deletion` blocks nothing during the grace period — cancel and export need the
session; the erasure runs on the date). **Findings closed:** R10-08 (P1), R9-23,
R11-13, R12-22 / WP-R12-7, WP-R9-10, WP-R11-9, R14 claims-table row 5, the WP-R10-3
design row in R13. **Findings left open, written as WP rows below:** R10-27 (async
export — WP-R10-3b), WP-R12-1 (streamed export).

## Before-state, reproduced on `main` (#528) against `vaultaire_test`

Local build, `STORAGE_MODE=local`, `DB_NAME=vaultaire_test`, port 8099. Registered
`before-…@test.local`, uploaded one object, scheduled deletion through
`DELETE /api/v1/manage/account`, then by SQL set `deletion_scheduled_at = NOW() - 31 days`
and `tenants.stripe_subscription_id = 'sub_before_proof'`. Seventy-five seconds later:

```
 status = pending_deletion, past_due = t
 stripe_subscription_id = sub_before_proof, subscription_status = active
 head_rows = 1;  DATA_PATH/tenant-97e2dea736d76e1a_before-bkt/obj.txt still on disk
 job_runs: retention (only)
 POST /api/v1/admin/account-deletion -> 404
 POST /auth/login -> 200
```

Nothing reads `deletion_scheduled_at` on `main`; `ExecuteDeletion` had no caller and
would not have cancelled Stripe or deleted a backend object (R10-08). The customer would
keep being billed after "deletion".

## What was built

### 1. One state machine — `internal/account` (new package)

The dashboard could not import `internal/api` (import cycle), so it ran its own
`UPDATE users` (R12-22). Both now call one service that imports nothing internal.

| Piece | Where |
|---|---|
| `Schedule` — one `UPDATE … RETURNING`; the first date wins from any entry point; `status = 'pending_deletion'`; reason `CapReason` ≤ 500 runes, default text | `internal/account/service.go:78`, `:91` |
| `Cancel` — `ErrNoPendingDeletion` when nothing is scheduled | `internal/account/service.go:129` |
| `ListDue` — `pending_deletion` users past their date, tenant joined by e-mail (the registration contract) | `internal/account/service.go:181` |
| `StillDue` / `lockedStillDue` — re-read under `FOR UPDATE` (the runner's cancel check) | `internal/account/service.go:212`, `:228` |
| `EraseRows` — one transaction: lock the user row, refuse `ErrNotPending` (cancel won), refuse `ErrObjectsRemain` while `object_head_cache` still lists objects, run `Deleted`, then the `Kept` scrubs, users and tenants last | `internal/account/erase.go:201`, guard `:212-229` |
| `Deleted` — the old 23-table list plus every survivor R9 area 5 / R13 named, ids compared as **text** (UUID-typed scaffold columns raised 22P02 and rolled the whole erase back) | `internal/account/erase.go:48-169` |
| `Kept` — `stripe_events` (tenant_id nulled), tenant-keyed `audit_logs`/`audit_logs_archive` (`ip`, `user_agent` nulled), `billing_charges`, `metered_usage_reports`, `abuse_reports` | `internal/account/erase.go:171-189` |
| Schema walk: every table with a `tenant_id` / `user_id` / `admin_user_id` / `granted_by` column must be in `Deleted` or `Kept`; every statement prepares against the migrated schema | `internal/account/schema_test.go:22` |

Entry points on the service: `DELETE /api/v1/manage/account` and `DELETE /api/v1/user`
(same handler, `internal/api/management_routes.go:751`, `internal/api/user_api.go:44`),
`POST …/cancel-deletion` (`management_routes.go:791`, 400 `no_pending_deletion`), the
dashboard (`internal/dashboard/handlers/account.go:59`, `:147`; wired through
`Deps.Account`, `internal/dashboard/router.go:49`, `:245-246`; built once in
`internal/api/server.go:388`). The audit rows R11 added (`account.deletion_scheduled`,
`account.deletion_cancelled`) are kept on every entry point.

**OAuth-only confirmation** (`internal/dashboard/handlers/account.go:110`): password
when the account has one; a TOTP code (`totp_code`, single-use through
`ConsumeTOTPCode`) when it is OAuth-only with 2FA; otherwise the account e-mail re-typed
(`confirm_email`). The settings page picks the field from `HasPassword` / `MFAEnabled`
(`settings.go:265`, `templates/customer/settings.html:154-168`; reason textarea
`maxlength="500"` at `:172`).

### 2. The runner — `internal/api/deletion_runner.go`

The R13 shape (retention.go): daily at **04:30 UTC** with a catch-up at boot through
`job_runs` (`account_deletion`; `:202-243`), ONE `pg_try_advisory_lock` per run
(`:259`, key `:66`), `POST /api/v1/admin/account-deletion` (`:645`, route
`server.go:873`, 409 `already_running` at `:654`, audit `admin.account_deletion` at
`:658`, documented in `internal/docs/openapi.go:1263`), metrics
`vaultaire_account_deletion_{tenants_total{result},objects_total{result},last_run_timestamp_seconds,runs_total{outcome}}`
registered on the server registry (`internal/api/prom_metrics.go:44`). Wired in
`Start` (`server.go:1278-1288`): engine, GCI, quota manager, the account service, Stripe
when configured, the auth service, the session store. Batches of 1000 (`:167`), 2 h per
tenant, 6 h per run (`:170-171`).

Per due tenant, `eraseTenant` (`:313`), every step idempotent and resumable:

| Stage | What | Where |
|---|---|---|
| confirm | `StillDue` under `FOR UPDATE` before anything is touched | `:334` |
| a. Stripe | `CancelSubscription` when `stripe_subscription_id` is set and `deletion_stripe_cancelled_at` (migration 072) is not; the stamp makes a crash-retry safe; a Stripe error **or a subscription id with no `STRIPE_SECRET_KEY`** defers this tenant — nothing of theirs is touched before billing is settled | `:435-466`; billing side `internal/billing/stripe.go:189` (fetches the status first: an already-`canceled` subscription is success, `:208`) |
| b. objects | keyset batches over `object_head_cache`; `StillDue` before every batch (`:493`); whole objects: unreclaimed **hot copy** of a demoted object deleted first (`smart_demotions`, `:552`), then the RECORDED backend (`HintBackend` + `engine.Delete`, `:591`), then `deleteHeadRowReleasing` (`:567`) — chunked rows release the manifest with the head row under the key lock and never touch `_global` blobs; quota released per object (`:581`); a backend failure counts and leaves the row; Object Lock does not survive the owner's erasure — counted and logged (`:471`) | `:469-604` |
| b'. multipart | active uploads aborted, every staging dir removed | `:606-641` |
| gate | any failure or any head row left → **deferred**, resumes next run | `:363-372` |
| c. rows | `account.EraseRows`; `ErrNotPending` → cancelled | `:376-384` |
| d. sessions | `Sessions.DeleteByUserID` + `auth.Evict` (the credential cache is loaded at boot; without eviction an erased user could still sign in until the next restart — S3 auth itself reads the DB) | `:389-394`; `internal/auth/auth.go:657` (`cacheMu` `:73` guards the writers and the login/API-key readers) |
| e. report | `account.erased` audit row keyed by the tenant (the user's own rows are deleted), actor `system` (or `admin_trigger` with the admin as `performed_by`), counts per stage; `account.erasure_cancelled` when a cancel won mid-walk | `:324-328`, `:410` |

### 3. Copy

Settings page banner + form text (`settings.html:144`, `:151`), the dashboard flash
(`handlers/account.go:47`), the management/user API message (`management_routes.go:692`),
the OpenAPI text (`openapi.go:1018`), and the six legal lines R14's claims-table row 5
rewrote to "our operators erase…": `baa.html:94`, `privacy.html:40`, `dpa.html:106`,
`terms.html:73`, `gdpr.html:27`, `gdpr.html:33` — all now say the deletion job cancels
the subscription and erases objects, keys and account records **on the date**, backups
age out within 7 days.

### 4. Migration `072_account_deletion_runner.sql`

`tenants.deletion_stripe_cancelled_at` (`:14`) and the partial index
`idx_users_deletion_due` on `users (deletion_scheduled_at) WHERE status = 'pending_deletion'` (`:16`).

## Tables erased, kept, untouched

**Deleted** (`erase.go:48-169`, in order): `global_content_index` refs released
set-based → `tenant_chunk_refs`, `object_metadata`, `object_head_cache`,
`object_versions`, `object_locks`, `object_locations`, `smart_demotions`,
`multipart_uploads` (+ `multipart_parts` by FK), `artifacts`, `bucket_notifications`,
`buckets`; `webhook_deliveries` → `webhook_endpoints` → `events`; `s3_access_log`,
`cdn_access_log`, `cdn_stats_daily`, `access_patterns`, `bandwidth_usage_daily`,
`bandwidth_alerts`, `bandwidth_rollover`, `tenant_cost_daily`, `dedup_statistics`,
`idempotency_cache`; `quota_usage_events`, `tenant_floor_quotas`, `grace_periods`,
`upgrade_suggestions`, `upgrade_triggers`, `usage_daily_snapshots`, `usage_reports`,
`report_schedules`, `billing_policies`, `billing_credits`, `invoices`, `tenant_quotas`,
`subscriptions`; `tiering_policies`, `retention_policies`, per-tenant `feature_flags`,
`tenant_encryption_keys`, `sts_tokens`, `admin_notifications`, `admin_notes` (about the
tenant AND authored by the user — NO ACTION FK), `change_history`, `dashboard_sessions`;
`api_keys`, `user_mfa`, `mfa_audit_log`, `oauth_accounts`, `user_activities`,
`user_consents`, `consent_audit`, `user_roles` (`granted_by` nulled first — NO ACTION FK),
`breach_affected_users`, `deletion_requests`, `gdpr_deletion_requests`,
`gdpr_subject_access_requests`, `legal_holds`, `portability_downloads`,
`portability_requests`, `account_exports`, the user's own `audit_logs` /
`audit_logs_archive` rows (`user_id` or `performed_by`), `waitlist_signups` by e-mail;
then `users`, `tenants`.

**Kept** (`erase.go:171-189`): `stripe_events` (`tenant_id` nulled), tenant-keyed
`audit_logs` / `audit_logs_archive` (`ip`, `user_agent` nulled), `billing_charges`,
`metered_usage_reports`, `abuse_reports`. **Untouched:** `global_content_index` rows
(dedup GC sweeps the zero-ref rows the release marked), `job_runs`.

**Audit policy, checked and matched:** the privacy policy R13/R14 wrote says the audit
trail is kept for the life of the service "as our security record; **your own entries
are removed with your account**" (`privacy.html:41`). The prompt's "kept + scrubbed"
was therefore applied to the tenant-keyed operator rows (suspend, quota, tier, the
erasure record itself), and the subject's own rows are removed — the policy text wins.

## Tests (all red on `main`: the package, the runner and the guard did not exist)

- `internal/account/erase_db_test.go:138` seeds a row in every table the prompt named
  and asserts every `Deleted` predicate counts zero, `stripe_events` kept with
  `tenant_id` NULL, the operator audit row kept and scrubbed, the user's row gone, the GCI
  row surviving and marked; `:189` cancel-wins guard; `:205` future date; `:239`
  `ErrObjectsRemain`; `:218` schedule round trip with the reason cap.
  `schema_test.go:22` is the contract walk. `service_test.go` (sqlmock): cap, defaults,
  first-date-wins, unknown user, no-pending cancel, nil DB.
- `internal/api/deletion_runner_test.go` — two local drivers (one fails on demand), four
  whole objects (two on the primary, one on `second`, one demoted with an unreclaimed hot
  copy), one chunked manifest, a locked object, a staged multipart part, a fake Stripe, the
  memory session store: `:202` erases everything once (Stripe called once, bytes gone
  from the recorded backend and the hot copy, manifest released and GCI marked, staging
  dir gone, every table empty, ledgers kept/scrubbed, memory session revoked, `job_runs`
  ok, second run a no-op); `:267` Stripe failure defers and touches no object, the next
  run cancels once more and erases; `:295` subscription set with no Stripe client →
  deferred; `:307` backend failure leaves the two rows on the dead backend, the Stripe
  stamp survives, the next run finishes without a second cancel; `:338` cancel during the
  walk stops it (first batch gone, the rest stay, account alive, quota honest,
  `account.erasure_cancelled`); `:366` concurrent run refused (`errJobAlreadyRunning`,
  trigger 409, then 200 with `"erased":1` and the admin audit row); `:394` not due →
  untouched; `:404` the daily schedule / catch-up.
- `internal/billing/webhook_erasure_test.go:36` — `customer.subscription.deleted`
  arriving BEFORE the erase clears the house (`ClearHouse`) and downgrades, then
  `EraseRows` succeeds without conflict; `:70` arriving AFTER the erase is 200 and
  recorded (the R10-45 not-ours path), no row re-created; `:97` a repeat
  `CancelSubscription` on an already-cancelled subscription is success, a fetch failure
  is an error.
- `internal/auth/evict_test.go:14` — login, `GetUserByEmail/ID`, the tenant's access key
  and the user's API key all stop resolving after `Evict`.
- `internal/dashboard/handlers/account_test.go:196` OAuth-only confirms with the e-mail
  (wrong e-mail refused; case-insensitive); `:240` the reason is capped; `:261` cancel
  with nothing scheduled.
- `internal/api/dedup_gc_coherence_test.go` (F5) now calls `account.EraseRows`.

`make test-db && make test-integration` (every package, `-race`, the migrated
`vaultaire_test`): **exit 0**, 33 packages ok. `make lint`: 0 issues. `make gosec`: **0
issues** (its non-zero exit on this box comes from `.private/retired-bench/*` Go files
that are gitignored and absent in CI — identical on `main`).

## Live proof (the built binary, `STORAGE_MODE=local`, `DB_NAME=vaultaire_test`)

Registered `live-…@test.local`; with aws-cli `s3api put-object` uploaded `docs/one.txt`
(10 B), `media/two.bin` (3 MB) and `media/big.bin` (70 MB → **chunked**, 38 chunk refs,
`is_chunked = t`); `create-multipart-upload` + `upload-part` left
`/tmp/vaultaire-multipart/upload-0b34…/part-00001` (3 MB) staged. Logged in to the
dashboard, POSTed `/dashboard/settings/delete-account` with the password:

```
303 -> /dashboard/settings
banner: Your account is scheduled for deletion on October 30, 2026. On that date your
        subscription is cancelled and your objects, keys and account records are erased;
        backups age out within 7 days. Until then everything keeps working, so export
        your data first if you need it.
users: status = pending_deletion, deletion_scheduled_at = 2026-10-30, reason = "live proof of WP-R10-3"
POST /auth/login -> 200   (D-16: the grace period blocks nothing)
```

Backdated by SQL (`- 31 days`), `stripe_subscription_id = 'sub_live_proof'` (this box has
no `STRIPE_SECRET_KEY`), promoted a second user to admin for the JWT.

**Per-table counts before:** users 1, tenants 1, api_keys 1, tenant_quotas 1, buckets 1,
object_head_cache 3, object_locations 40, tenant_chunk_refs 38, object_metadata 1,
multipart_uploads 1, multipart_parts 1, dashboard_sessions 1, quota_usage_events 3,
events 4, audit_logs (user rows) 3, audit_logs (tenant rows) 3, GCI rows with ref > 0
for the tenant's chunks 38; 2 files under the tenant container on disk; staging dir
present.

**Trigger 1** (`POST /api/v1/admin/account-deletion`): **500**, `{"erased":0,
"deferred":1, "errors":["tenant tenant-6968… deferred: stripe: subscription
sub_live_proof is set but STRIPE_SECRET_KEY is not configured — the account is not
erased until billing can be cancelled"]}`; head rows still 3, files still on disk. Then
`stripe_subscription_id = NULL` (what the `customer.subscription.deleted` webhook
writes), **trigger 2**: **200**,

```
outcome erased, objects_deleted 2, chunked_released 1, locked_erased 0, multipart_aborted 1,
rows: api_keys 1, audit_logs 3, bandwidth_usage_daily 1, buckets 1, dashboard_sessions 1,
      events 4, multipart_uploads 1, object_locations 38, quota_usage_events 3,
      tenant_quotas 1, tenants 1, users 1 (every other table 0), duration 150 ms
```

**After:** every count above **0**; audit_logs tenant rows **1** (the `account.erased`
row: `objs 2, chunked 1, mp 1`, `performed_by` = the admin who triggered it); GCI rows
with ref > 0 for the tenant's chunks **0** (38 rows newly `marked_for_deletion`, the
`_global/_chunks/*` blobs are dedup GC's to sweep); 0 files under the tenant container;
staging dir gone; `job_runs`: `account_deletion ok, rows_affected 1, last_success_at set`;
`POST /auth/login` → **401**; `aws s3 ls` with the erased keys → refused.

Runner log lines (zap, pruned):

```
{"level":"error","msg":"account deletion: tenant deferred to the next run","tenant_id":"tenant-6968…","stage":"stripe","error":"subscription sub_live_proof is set but STRIPE_SECRET_KEY is not configured — …","objects_deleted":0,"object_failures":0}
{"level":"info","msg":"account erased","tenant_id":"tenant-6968…","user_id":"6deb6025-…","stripe_cancelled":false,"objects_deleted":2,"chunked_released":1,"locked_erased":0,"multipart_aborted":1,"rows":56}
```

## Adversarial pass — what it found in my own code, fixed before the PR

1. **Metrics never exported.** The four counters were registered on the Prometheus
   default registry in `init()`; `/metrics` serves the server's own registry
   (`prom_metrics.go`). Moved to `prom_metrics.go:44` next to retention's.
2. **A demoted object's hot copy leaked.** The walk deleted on the recorded backend
   (the cold copy); an unreclaimed hot copy (`smart_demotions.hot_deleted_at IS NULL`)
   stayed on the hot backend forever — the WP-R6-1 second-copy class on this entry
   point. `deleteObject` now reads the ledger and deletes the hot copy first
   (`deletion_runner.go:552`); tested with a fourth object.
3. **Repeat Stripe cancel after a crash.** A crash between Stripe's answer and the
   stamp repeats the cancel, and Stripe refuses to cancel a cancelled subscription → the
   tenant would have been deferred forever. `CancelSubscription` fetches the status first
   (`stripe.go:208`); tested with the fake fetcher. The `subscription.Cancel` call itself
   is not exercised offline (no test key on this box).
4. **Claimed invariant vs the check that runs before it.** "Every object is gone" held
   only if nothing wrote between the walk's last batch and the row erase. `EraseRows` now
   counts head rows inside its transaction and refuses with `ErrObjectsRemain`
   (`erase.go:222-229`); a PUT that lands in that window defers the tenant a day instead
   of orphaning bytes. Residual: a PUT whose head-row insert commits after that count
   and before the `DELETE` in the same transaction can leave one orphan head row + blob;
   its credentials stop working the instant the transaction commits. Accepted, noted.
5. **The manual trigger's audit row said `actor: system`** while `performed_by` carried
   the admin. The metadata now says `admin_trigger` on a manual run
   (`deletion_runner.go:410`).
6. Same logic on the other entry points, grepped: no writer of `deletion_scheduled_at` /
   `status = 'pending_deletion'` remains outside `internal/account`.

## What is deliberately not done

- **Export (R10-27 / WP-R12-1) — WP-R10-3b.** `POST /api/v1/manage/account/export` is
  still synchronous and inline (`internal/api/account_export.go`), the dashboard export
  still buffers. Design for the next session: the runner shape again (`job_runs`
  `account_export`, one advisory lock, `POST /account/export` → 202 + id, a worker that
  writes the JSON through `generatedObjectWriter.write` (`background_put.go`, R13) into a
  per-tenant private system bucket `_exports` (or `<tenant>_exports` — decide with the
  bucket registry rule), sets `account_exports.expires_at = now + 7 d` and
  `file_path`, `GET /account/export/{id}` answers a presigned URL from
  `generatePresignedS3URL` (`s3_presign.go:290`) signed with the tenant's primary pair;
  the retention job purges expired export objects; add `object_versions`, floor quotas
  / house / `house_period`, `intent_*`, `oauth_accounts` (provider only), session
  metadata; never a secret column (R10 invariant 12); the dashboard export calls the
  same service. Size S–M.
- **Rows whose `backend_name` is NULL** (prod's 62 pre-#514 multipart rows) are deleted
  without a hint: `engine.Delete` resolves by `object_locations`/primary and a miss is
  taken as drift, so bytes on an unrecorded backend stay behind — the same as S3 DELETE
  today; WP-R7-5's backfill fixes the rows, not this runner.
- **A backend with no registered driver** (prod's 2,613 `onedrive` and 452 `local` bench
  rows, or a dormant Quotaless) makes every object on it a failure: the tenant is
  deferred daily with an Error log line, never claimed erased. Operator action: register
  the driver, or hand-clean the rows once the bytes are confirmed gone. No alert rule yet
  (a `vaultaire_account_deletion_tenants_total{result="deferred"}` rule belongs with the
  WP-R13-3 job rules). **Done in WP-R13-3 (#543): `AccountDeletionDeferred` in
  `deploy/monitoring/vaultaire-jobs.yml`.**
- **The auth cache's other ~25 map readers** are still unlocked (pre-existing; `cacheMu`
  covers `Evict`, the three writers and the five login/API-key readers) — WP-R5-14.
- **A JWT issued before the erasure** still passes `requireJWT` for up to 24 h; every
  handler behind it reads the database and finds nothing. Not a data path.
- **Retention did not move onto a shared scheduler** — the runner copies its shape;
  WP-R13-3 unifies both (and dedup GC, smart demotion).
- The export bucket, the admin page for deferred tenants, and a per-tenant "erased"
  e-mail were not asked for.

## [YOU]

Nothing new. The Stripe rows in SYNTHESIS's checklist (endpoint pinned to 2023-08-16,
both secrets) are what lets stage a run in prod; without `STRIPE_SECRET_KEY` any tenant
with a subscription id is deferred daily — by design.

## Post-merge review (plan driver, 2026-10-01)

Read against `main` @ #536: `internal/api/deletion_runner.go`, `internal/account/{service,erase}.go`,
`billing.CancelSubscription`, the versioned branches of `HandleDelete`, the runner's tests, and
the CI history of `main` since #529. Not re-read: the dashboard confirm form, the legal copy,
`auth.Evict`. Every row below was reproduced before it was written down.

| ID | Sev | Where | What | Status |
|----|-----|-------|------|--------|
| PM-1 | P1 (CI) | `deletion_runner_test.go` (every `RunOnce` in the fixture) | `RunOnce` walks every due account in the database, and the test database is shared by every package (`internal/billing` and `internal/account` seed their own past-due accounts to call `EraseRows` on). The fixture's runs erased them mid-test (`TestWebhook_SubscriptionDeletedBeforeTheErase…`: "account is not pending deletion") and counted them in its own result (`ErasesEverythingOnce`: second run "should be empty"). **`main` was red twice in nine runs after #529** (runs 36806636320 and 36877942809, both on docs-only merges) — the R10-11 / R15-03 class, a third time. | **fixed here**: the runner has an `onlyDue` test hook beside `beforeBatch`; the fixture scopes every run to its own user. Red-first: `TestAccountDeletionRunner_FixtureLeavesOtherPackagesDueAccountsAlone` plants another package's due account next to the fixture's (erased on `main`, kept now). Then the runner tests looped beside the billing and account erase tests on one database: green. |
| PM-2 | **P1** (money) | `deletion_runner.go` `cancelSubscription` | Nothing ever clears `tenants.deletion_stripe_cancelled_at`, and a set stamp skipped stage a whatever subscription id was on the row. A run that cancels and stamps and is then **deferred** (a backend down at 04:30) or **cancelled mid-walk** leaves the account alive — D-16 blocks nothing — with the stamp set. The customer cancels the deletion, checks out again (new subscription id), and schedules a second deletion later: that run saw the stamp, skipped Stripe and erased the account. **Stripe keeps billing a customer who no longer exists**, and the later webhooks land on the "not ours" path (200). | **fixed here**: a subscription id on the row is cancelled on every run, stamped or not (`CancelSubscription` already answers an already-cancelled subscription with success — `webhook_erasure_test.go:97`); the stamp is kept as the record that the runner cancelled and is what `stripe_cancelled` reports once the webhook has cleared the id. A stamped row whose id is still set with no `STRIPE_SECRET_KEY` is now deferred, not erased. Red-first: `TestAccountDeletionRunner_StaleStripeStampDoesNotSkipANewSubscription` (on `main`: erased with one Stripe call). `BackendFailureLeavesTheRowAndResumesNextRun` now models the `customer.subscription.deleted` delivery between its two runs. |
| PM-3 | **P1** (erasure claim) | `deletion_runner.go` `walkObjects` (walks `object_head_cache` only) vs `s3_engine_adapter.go` `HandleDelete` | "Every object is deleted on its recorded backend" holds for objects that still have a head row. On a **versioning-enabled** bucket a plain DELETE writes a delete marker, removes the head row and **leaves the blob** (it is what `GET ?versionId=<newest live>` still serves); `DELETE ?versionId` removes only the version row. Neither blob is reachable from the walk: `EraseRows` then deletes the version rows and the bytes stay on the backend with nothing pointing at them. Proven with a throwaway test in the fixture (a blob + a live version row + a marker row, no head row → outcome `erased`, the file still on disk). Same shape, not re-proven here: a cold copy leaked by an overwrite of a demoted object (R13-10 / WP-R13-2) and bytes on a backend the row does not name (the 62 NULL-backend multipart rows). | **open → WP-R10-3c** (SYNTHESIS table A): too wide for a driver fix — the right answer is placement-agnostic and destructive (list the tenant's containers on every registered backend after the walk and delete what is left), and needs each driver's `List` checked for tenant-prefix safety. Until it lands the legal copy ("your objects … are erased on the date") is not true for keys deleted on a versioned bucket. |
| PM-4 | P2 | `account/erase.go:158` (`DELETE FROM audit_logs WHERE user_id = $1 OR performed_by = $1`) | An **operator's** own erasure deletes the audit rows of what they did to *other* tenants (`performed_by` = the admin, `tenant_id` = someone else) — the same rows the policy keeps, scrubbed, when the *subject* tenant is erased. An admin account (or whoever holds it) can schedule its own deletion and the trail of its suspensions, quota edits and primary swaps goes with it 30 days later. Proven with a throwaway test (one such row → 0 after the run). | **fixed — WP-R10-3d, PR #553** (`docs/reviews/WP-R10-3d.md`): rows about other tenants, `event_type = 'admin'` rows and tenant-less rows about another subject are kept, de-identified (`user_id`, `performed_by`, `ip`, `user_agent` nulled, the id and e-mail dropped from the metadata, `actor_erased` set); the operator's own entries still go. Red-first: `TestEraseRows_OperatorErasureKeepsTheOperatorTrail`. |

Clean on this read: the `StillDue` re-checks (before the walk, before every batch, inside the
erase transaction), the keyset walk under concurrent deletes, `keyArg` skipping tenant rules
for a user with no tenant row (no statement runs with `''`), the Stripe-before-objects order,
the chunked path never touching `_global`. Noted, not a finding: `ListDue` is `LIMIT 200`
oldest-first, so 200 permanently deferred accounts would starve newer ones — the deferred
alert rule that WP-R13-3 owes is what makes that visible.

Driver housekeeping learned: the worker shares the main checkout **and** the local
`vaultaire_test` database; driver test runs use a separate database
(`make test-db TEST_DB=vaultaire_test_driver`, `DATABASE_URL=…/vaultaire_test_driver`).
