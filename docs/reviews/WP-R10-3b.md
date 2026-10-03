# WP-R10-3b — the asynchronous GDPR export (with WP-R12-1)

**Worker session, 2026-10-03. Branch `wp/R10-3b-async-export`, from `main` @ #562.** Closes
SYNTHESIS table A row WP-R10-3b (= R10-27, the export half of WP-R10-3) and table B row
WP-R12-1 (+ R12-17); the compliance route's export half of R11-23 is deleted under D-12.
**Migration 075** (`075_account_exports_async.sql`; next is 076).

## The bug, seen first

Three export entry points, none right:

- `POST /api/v1/manage/account/export` built the whole JSON in memory and returned it **inline**:
  no artifact, nothing re-downloadable, `account_exports.file_path` / `expires_at` never set since
  migration 038, `GET /export/{id}` answered status and size only. Prod read-only, 2026-10-03:
  **0 rows in `account_exports`, 0 `account.export*` audit rows** — nobody has ever used it.
- The dashboard's `POST /settings/export` had a second copy of the collector:
  `SELECT … FROM object_head_cache WHERE tenant_id = $1` with no limit into a `[]map`, then one
  `json.MarshalIndent` — one click by a large tenant was a memory spike on the hub (R12-17), against
  the stream-never-buffer rule.
- `/api/compliance/export` (server.go) read a context key `requireJWT` never sets: **401 for
  everyone**, behind an admin-only group (R11-23). Dead by auth.

Art. 20 was met (the data came out, once, if you were quick); re-downloadability and completeness
were not. Red first: `account_export_test.go` (10 tests) and `account_export_schema_test.go`,
written before the service existed (`go vet`: `undefined: AccountExportService`).

## What was built

### One service — `internal/api/account_export.go` (+ `account_export_sections.go`)

Kept in `internal/api`, not `internal/account`: the render needs the customer write path
(`generatedObjectWriter`, placement, head row, version ledger, quota), the presign helper and the
engine — all of them live in `api`, and `account` is a leaf package the dashboard imports. The
dashboard sees the service through `handlers.ExportService` (`dashboard_exports.go` adapts it).

| Step | What |
|---|---|
| `Request(ctx, user, tenant)` | one `INSERT … status = 'pending'`; **one in flight per user** is a partial unique index (`idx_account_exports_one_pending`, 075) — a second request fails on insert (23505 → `ErrExportInFlight`), never read-then-insert |
| the `account_export` job | an interval job of the one scheduler (WP-R13-3): every minute, 15 s after boot, 10 min ceiling, `job_runs`, advisory lock. A run claims the oldest pending row with `UPDATE … WHERE id = (SELECT … FOR UPDATE SKIP LOCKED) RETURNING` (bumps `attempts`, stamps `claimed_at`), renders it, and goes on to the next — a row is attempted **once per run** (`NOT (id = ANY($tried))`); up to 50 per run |
| the render | `renderExport` streams **one JSON document** through a `json.Encoder`: fields and sections written as they are produced, arrays element by element; `object_head_cache`, `object_versions`, `object_locks`, `events` and the audit trail are **keyset-paged** in `COLLATE "C"` order (R4's listing), 2,000 rows a page (`PageSize`; the fixture uses 2 so the pages cross a bucket boundary). Nothing holds more than one page |
| the object | `generatedObjectWriter.writeOpts` — the customer write path (R13-02): spool, exact size + MD5, placement, head row, version ledger, quota — into **`_exports/account-export-<id>.json`** in the tenant's namespace. `generatedWriteOptions{AllowOverQuota: true}`: a GDPR right is never quota-gated — when the reservation is refused the bytes are **force-accounted** (a negative release adds; the usage the customer sees stays true), the result says `OverQuota`, the `account.export_completed` audit row carries `over_quota: true` and the server logs a Warn. Reports and access logs never set it |
| the row | `completed` (file_path, size, **etag**, `completed_at`, `expires_at = +7 d`) is written **only after the writer returned**. A deploy mid-render leaves `pending` with a fresh `claimed_at`; a claim older than 15 min is a dead process's and is picked up again. A failed attempt releases the claim at once (`claimed_at = NULL`, `error_message`) for the next run; after **3** attempts the row is `failed`. If the row vanished under the render (an erasure between claim and write) the object is removed again |
| the download | `DownloadURL`: `generatePresignedS3URL(VAULTAIRE_ENDPOINT, tenants.access_key, tenants.secret_key, "_exports", key, GET, 3600)` — minted per call, for the signed-in owner, never logged (the dashboard sends it in a `Location` header only, `Cache-Control: no-store`; `http.Redirect` would have printed it in an HTML body) |
| expiry | `PurgeExpired` is a step of the **retention job** (`RetentionJob.Exports`, counted under table `account_exports`): the object goes through the customer delete path (hint the recorded backend, `engine.Delete`, `deleteHeadRowReleasing`, quota released), the row becomes `expired` and stays — the audit trail of the request. A row whose head row is already gone is still purged (the blob is removed by key). `GET` answers **410** `export_expired` from `expires_at` on, before the job runs |
| metrics | `vaultaire_account_exports_total{result=completed|retried|failed|expired}`; the job is in `PeriodicJobStale` (`vaultaire-jobs.yml`; the rule test enforces it) |

### The sections (`account_export_sections.go`)

`format` · `export_id` · `exported_at` · `user` (id, e-mail, company, role, status, verified,
dates, deletion schedule, sign-up attribution) · `security` (MFA on/off, MFA events) · `tenant`
(name, e-mail, **primary access key id**, plan, subscription status, slug, house period, dates) ·
`quota` (totals, floors, house = `intent_*` + `house_period`) · `buckets` (every setting: region,
residency, versioning, lock + default retention, MFA delete, SSE, CORS, cache, budget, CDN, tier
preference, logging, inventory, metadata, dates) · `bucket_notifications` · `objects` (bucket, key,
size, etag, content type, **`storage_class` = `engine.CustomerStorageClass(floor, backend)`**,
floor, last modified, created, last accessed, user metadata, tags, the content-* headers, SSE
algorithm, deduplicated — never the backend) · `object_versions` · `object_locks` ·
`multipart_uploads` · `api_keys` (id, key id, name, permissions, bucket scope, ip allowlist,
expiry, revocation, last used, created — never `secret_hash`/`secret_key`) · `bandwidth_usage`
(90 d) · `events` · `webhooks` (url, events, enabled — never `secret`) · `oauth_accounts`
(**provider and linked_at only**) · `sessions` (created, last seen, expiry, user agent, ip) ·
`signup` (the waitlist row by e-mail) · `exports` (the export records themselves) · `audit_trail`
(the user's own `audit_logs` + archive rows, and `user_activities`).

**The contract is a schema walk** (`TestExportSectionsCoverSchema`, the
`TestErasedTablesCoverSchema` / `TestSweepBucketSourcesCoverSchema` idea): every table with a
`tenant_id` / `user_id` / `admin_user_id` / `granted_by` column is in `exportSections` or in
`exportExcludedTables` **with the reason** (49 excluded: internal placement/dedup ledgers, operator-side
records, logs with their own retention, credentials, D-12 scaffolding with no writer); every
column whose name says `secret` / `hash` / `token` / `password` / `code` is in `exportNeverColumns`
(19, three of them false positives with the note) **and absent from its table's section SQL**
(a word-boundary match on the statement). Every section statement is prepared against the schema.

### The system bucket — `tenant.ExportsBucket` = `_exports` (`internal/tenant/system_bucket.go`)

The bucket-registry rule (WP-R12-10): the writer refuses a bucket with no `buckets` row, so the
service creates it on first use — `private`, `versioning_status = 'disabled'`, `object_lock_enabled
= FALSE`, default region/residency, `sse_enabled` when the server has SSE, `metadata.system = true`.
**A leading `_` is not an S3 bucket name** (`s3BucketNameRe`), so no customer can create or collide
with it (`TestSystemBucket_NameIsNotAnS3BucketName`). The hidden flag is not theatre:

| Surface | A system bucket is |
|---|---|
| S3 `ListBuckets`, management `GET /buckets`, the bucket caps (free tier, max), the NoSuchBucket suggestion list, the dashboard bucket list, onboarding count, compliance report | **absent** — every query carries `name NOT LIKE '\_%'` (the literal, in each statement: gosec G202 refuses a concatenated fragment) |
| S3 `ListObjects`, `ListObjectVersions`, `HeadBucket`, `DeleteBucket`, every bucket sub-resource, `PutObject`, `DeleteObject`, `PostObject` … | **NoSuchBucket** (`systemBucketRefused`, in the dispatcher after the scope checks) |
| S3 `GetObject` / `HeadObject` of its keys | served — that is the presigned download |
| the dashboard's `/buckets/{name}…` pages (6 routes) | **404** (`noSystemBucket` middleware) |

Never versioned, never lock-enabled: the retention delete is never refused.

### The three entry points

- **Management API**: `POST /account/export` → **202** `{id, status: pending}` (409
  `export_in_progress`); `GET /account/export/{id}` → status, size, dates, and when `completed`:
  `etag`, `download_url`, `download_url_expires_at`; **404** for an id that is not the caller's
  (never 403 — existence is not confirmed), **410** `export_expired`. OpenAPI rewritten
  (`DataExportRequested`, `DataExportStatus`); `TestOpenAPIDriftGuard` green.
- **Dashboard**: `POST /settings/export` records through the service, flash, redirect; the settings
  card shows *being prepared* → *ready until \<date\>, Download (N MB)* → *expired* / *failed*;
  `GET /settings/export/download` is a 302 to a fresh presigned URL. The private collector
  (`collectExportData`, 176 lines) is deleted.
- **Compliance route**: `POST|GET /api/compliance/export…` and the handlers, the
  `portabilityService` of `NewAPIHandler`, `portability.go` (+ test, 609 lines) deleted; the drift
  allowlist shrinks by two.

### Erasure

`account.EraseRows` already covered `account_exports` (ByUser, since #529). The export object is a
head row in the tenant's namespace, so the runner's object walk deletes it like any object and
`EraseRows` drops the `_exports` registry row with the other buckets.
`TestAccountExport_ErasureRemovesTheExportObjectAndRow` proves it on the deletion fixture — and,
with it, D-16: a user past their deletion date requests an export and gets it.

### Legal copy (the periods are stated together with `retention.go`)

Privacy policy: retention line *"Data exports — the file … is kept for 7 days, then automatically
deleted; the record … stays with the account"*, portability right reworded; DPA monitoring line
*"data-export files 7 days"*; GDPR page's export paragraph rewritten.

## Same logic on the other entry points (the audit)

- **Other in-memory per-tenant JSON builders**, listed, not fixed here: the dashboard
  `HandleComplianceExport` (`compliance.go`: a `MarshalIndent` of the tenant's bucket settings —
  bounded by `maxBucketsPerTenant`, fine); `HandleAdminAuditExport` (≤ 20k rows CSV, admin-only,
  R12 bounded); `HandleAdminWaitlistExport` (streams). No other GDPR builder exists.
- **Bucket listings**: ten sites read `FROM buckets WHERE tenant_id = $1`; the six a customer sees
  (above) now filter; `erasure_sweep.go`, `erase.go`, `access_log.go`, `s3_inventory.go`,
  `smart_demotion.go`, `s3_lock.go` are correct unfiltered (an erasure must sweep the system bucket;
  configuration lookups are by name).
- **The writer's allowance** is a per-call option; `access_log.go` and `s3_inventory.go` call
  `write` unchanged and still get `errTargetQuotaExceeded`.

## Adversarial pass (every mutation seen red, then restored)

| # | Mutation / probe | Result |
|---|---|---|
| 1 | A second tenant's user id on the export id (`Get`, `DownloadURL`); a non-UUID id | `ErrExportNotFound` → 404 — live: `HTTP 404` with the other user's JWT |
| 2 | A presigned URL after the primary pair changed (test: both columns of `tenants` rewritten) | `403 AccessDenied` — the verifier looks the key id up in `tenants` first. **Live finding below: no product path changes that pair** |
| 3 | A write that fails half-way (`beforeWrite` returns an error on the first attempt) | row `pending`, `attempts = 1`, no blob, no head row; the next run completes it, `attempts = 2`. Three failures → `failed` with the message; the row is not claimed again; a new request is allowed |
| 4 | Throwaway migration `ALTER TABLE users ADD COLUMN recovery_secret TEXT`, run on the test DB | `TestExportSectionsCoverSchema` **red**: "column users.recovery_secret looks like a secret: add it to exportNeverColumns"; green again after `DROP COLUMN` |
| 5 | The `user` section's SQL made to select `password_hash` | red: "section user selects users.password_hash" |
| 6 | The retention step on an export whose head row is already gone | the blob is removed by key, the row is `expired` (test + live) |
| 7 | `systemBucketRefused` disabled in the dispatcher | `TestAccountExport_SystemBucketDoesNotExistToS3ExceptForItsObjects` red (ListObjects 200) |
| 8 | `AllowOverQuota: false` | `TestAccountExport_OutOfQuotaStillSucceedsAndIsAudited` red (`failed`: over quota) |
| 9 | ListBuckets without the `NOT LIKE '\_%'` filter | the system-bucket test red (the name appears) |
| 10 | `completed` stamped before the write | `TestAccountExport_AFailedRenderStaysPendingAndIsRetried` red ("never 'completed' without a whole object") |
| 11 | The same row re-claimed inside one run (the first implementation did: a failed attempt was retried at once and succeeded in the same run — the retry test caught it) | fixed: `NOT (id = ANY($tried))` |
| 12 | Keyset paging across a bucket boundary with `PageSize = 2` | five objects of two buckets come out in `COLLATE "C"` order, once each |
| 13 | A tenant with no objects, no buckets, no floor rows | completes; `objects: []`, `buckets: []`, `floors: []` (arrays, never `null`) |

**Found, not built here (pre-existing, outside the WP):** **the primary key pair cannot be
rotated or revoked.** Registration mirrors `tenants.access_key/secret_key` into an `api_keys` row
named `primary` (R12-02). `RotateAPIKey` / `RevokeAPIKey` act on `api_keys` only; nothing in the
product writes `tenants.access_key` (`grep "UPDATE tenants SET access_key"` = 0). Live: rotating
the `primary` row through `POST /api/v1/user/apikeys/{id}/rotate` revoked it and minted
`VLT_…`, `tenants.access_key` stayed `VK7442…`, and the presigned export URL minted before the
rotation — and any SigV4 request signed with the "rotated" pair — still answers **200**, because
the S3 verifier (`verifyPresignedURL`, `lookupCredential`) looks `tenants` up first. The export's
exposure is bounded by the 1 h link TTL; the underlying gap is a WP for the auth area (R5/R12):
a rotation of the primary row must rewrite the `tenants` pair, or the `tenants` lookup must honour
the mirror row's `revoked_at`.

## Tests

- `internal/api/account_export_test.go` — 11 DB-backed tests on their own tenant (fixture seeds a
  user with password + MFA + backup codes, a house tenant, two buckets, five objects incl. an attic
  one on `geyser`, a version, a lock, a scoped key with secrets, OAuth, a session, a webhook with
  a signing secret, an STS token, events, bandwidth, a waitlist row, an audit row; every secret
  value is asserted absent from the export body): request → run → one streamed object (sections,
  class, metadata, tags, paging, no backend, provider-only OAuth, no secrets); empty tenant; one
  in flight; the presigned download through the real verifier (not test mode) + rotation; another
  user's id; expiry → 410 + retention purge + quota release; purge without a head row; a failed
  render retried; three failures; out of quota; the system bucket on S3 (ListBuckets, 8 refused
  operations, GetObject/HeadObject allowed, nothing written); the bucket cap; erasure on the
  deletion fixture (D-16).
- `internal/api/account_export_schema_test.go` — the schema walk (both directions).
- `internal/api/r9_schema_test.go` — R9-02 re-targeted at `renderExport`.
- `internal/dashboard/handlers/export_test.go` — 7 handler tests on a fake service (flashes, the
  one-Location-header redirect, the three states of the card).
- `internal/tenant/system_bucket_test.go`; `jobs_runners_test.go` / `jobs_test.go` /
  `job_rules_test.go` (the new job, its lock key, the staleness rule); `TestOpenAPIDriftGuard`.
- `go test -race ./...` on `vaultaire_test_r103b`: 34 packages ok (twice). `make lint` 0,
  `make gosec` 0, `make deadcode` adds nothing.

## Live proof (the built binary on `vaultaire_test_r103b`, local backend, `VAULTAIRE_ENDPOINT=http://localhost:8099`)

```
register exp-live-…  → VK7442c1456b992dc8 / SK…          aws s3 mb live-bkt-2dc8; cp docs/hello.txt (13 B)
POST /api/v1/manage/account/export              HTTP/1.1 202 {"id":"e32a2f33-…","status":"pending"}
POST again                                      HTTP/1.1 409 export_in_progress
GET  /account/export/e32a2f33-… (35 s later)    completed, 4921 bytes, etag 5550fb6f…, expires_at = completed_at + 7 d,
                                                download_url http://localhost:8099/_exports/account-export-e32a2f33-….json?X-Amz-…
curl <download_url>  (no Authorization)         HTTP 200, 4921 bytes — one JSON document, 24 keys;
                                                objects: [(live-bkt-2dc8, docs/hello.txt, 13, STANDARD)]; buckets: [live-bkt-2dc8];
                                                api_keys: primary → id, key_id, name, permissions, scope … (no secret)
GET  same id, the other user's JWT              HTTP 404
aws s3 ls                                       live-bkt-2dc8                      (no _exports)
aws s3 ls s3://_exports                         NoSuchBucket
aws s3api head-bucket --bucket _exports         404
aws s3api head-object _exports/<key>            ContentLength 4921, ETag "5550fb6f…", application/json   (allowed)
aws s3api delete-object / put-object / delete-bucket on _exports     NoSuchBucket, NoSuchBucket, NoSuchBucket
account_exports row                             completed | attempts 1 | _exports/account-export-….json | 4921 | 5550fb6f… | 7 days
audit_logs                                      account.export_requested {via: management_api}
                                                account.export_completed {bytes 4921, etag, attempts 1, over_quota false, …}
job_runs                                        account_export | ok | 1
UPDATE expires_at = now - 1 min; GET            HTTP/1.1 410 export_expired
POST /api/v1/admin/jobs/retention/run           202; row → expired, head row 0, blob 0, job_runs retention ok rows 1
second export; rotate the primary api_keys row  rotate 200 → api_keys: primary (revoked), primary (rotated) VLT_…; tenants.access_key unchanged;
                                                the URL minted before the rotation: HTTP 200   (the finding above)
```

The dashboard flow is covered by handler tests (a fake service); the settings card was not
driven in a browser this session.

## Prod, read-only (2026-10-03)

`account_exports`: 0 rows (status, file_path, expires_at all never set); `audit_logs`
`account.export*`: 0; buckets named `_%`: 0; migration 075 not applied (expected — the deploy
applies it); `job_runs` retention: ok, 03:56 UTC today.

## Decisions taken

- Service in `internal/api` (write path, presign, engine live there); the dashboard gets an
  interface. Bucket name `_exports` (one per tenant, in the tenant's namespace, impossible as an S3
  name); registry row created lazily by the service, not by registration.
- Worker = the scheduler job (1 min), not a trigger after `Request`: a request needs no goroutine,
  a deploy mid-render is the job's catch-up, a trigger would still need the job. A click waits ≤ 60 s.
- Exports count against the standard floor and are written over quota, force-accounted, audited
  (the brief's default; [YOU] below).
- The compliance export: deleted (dead by auth), not delegated — D-12.
- The `_id` tiebreak column of the audit cursor is stripped from the output; event rows keep their id.

## Not done

- The dashboard card was not screenshotted (`make dash-shots` is the routine for it).
- No e-mail when the export is ready (the e-mail provider is unset in prod; the page says "reload in
  a minute").
- `multipart_parts` has no tenant column and is not in the walk; the in-flight uploads section lists
  the uploads, not their parts.
- `s3_access_log` is excluded from the export with a reason (operational, 30 d, deliverable to the
  customer's own bucket); if Isaac wants it in, it is one more paged section.
- The primary-pair rotation gap (above) — a separate WP.

## [YOU]

1. **Privacy page line to confirm** (Retention Periods): *"Data exports — the file produced by a
   data-export request is kept for 7 days, then automatically deleted; the record that an export was
   requested stays with the account."* (+ the DPA's "data-export files 7 days").
2. **Quota: counted or free?** Built as the brief said: the export is billed on the standard floor
   like any object, and written anyway when the account is over quota (force-accounted, audited,
   the UI line says so). **Recommendation: make it free** — exempt the `_exports` bucket from the
   reservation and from the usage totals (a GDPR copy of the customer's metadata is the service's
   obligation, typically kilobytes to a few MB, and "your export pushed you over your quota" is a
   support ticket waiting to happen). One flag flip in `renderOne` + a skip in `reserveQuota` for
   the system bucket, if you say so.
3. The primary-pair rotation finding: decide whether it is a launch item (it is the one credential
   every account has, shown once at registration, and it never dies).
4. Nothing to install: the job rides the existing scheduler and `vaultaire-jobs.yml` (already on
   the install list) gains the job name in `PeriodicJobStale`.

## Post-merge review (plan driver, 2026-10-03)

Read against the note: `account_export.go` (request, claim, render, complete, purge), the sections
file's statement list and exclusions, `background_put.go` (the over-quota allowance: a negative
release, accounted), `system_bucket.go`, the S3 dispatcher's refusal, the dashboard's
`noSystemBucket`, the retention step, migration 075. Checked and held: both the write and the
purge carry the tenant in the context (the fixed-bucket drivers refuse a call without one since
WP-R8-7); `completed` is stamped only after the writer returned; the one-in-flight rule is the
partial unique index; an erasure between claim and write removes the object again; the presigned
GET is served because `systemBucketRefused` lets `GetObject`/`HeadObject` through.

**PM-1 (P2, fixed #564): the management API did not know the system bucket.** S3 answers
NoSuchBucket and the dashboard 404s, but `GET/PATCH/DELETE /api/v1/manage/buckets/{name}`,
`…/objects`, `…/tier` and `…/residency` treated `_exports` as a customer bucket — the red run
re-tiered the export bucket to `archive` through `PUT …/tier` (the next export would have landed
on tape behind a presigned GET that answers 503), `PATCH` wrote metadata, `PUT …/residency` pinned
it. Fix: `mgmtSystemBucketNotFound` first thing in the six handlers (the envelope's
`bucket_not_found`). `TestMgmt_SystemBucketDoesNotExistOnThePerBucketRoutes`. The same rule on
the third entry point.

**The primary-pair rotation finding is confirmed in the auth code and is now a SYNTHESIS table A
row (WP-R5-14):** `lookupCredential` resolves `tenants.access_key` first with no revocation check;
`RotateAPIKey` / `RevokeAPIKey` write `api_keys` only; nothing writes `tenants.access_key`. A
customer who rotates a leaked primary key has not revoked anything. It is the next worker prompt.

Noted, not changed: the export bucket is created in the default region, so a tenant whose buckets
are all pinned to another region has its export (metadata) in Dallas — acceptable for a 7-day
metadata file, but say so in the privacy line if regions are sold as residency; an operator's own
export carries their admin audit rows about other tenants (their actions, de-identified per
WP-R10-3d only at erasure).
