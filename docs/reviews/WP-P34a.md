# Stage 1a — Prompt 34a: Part 0 (the WP-P33 review's RP44) — the admin-read gate has one parser and its own switch is in the log

*2026-10-23. PRs: #922 (Part 0a, the product PR) and this docs PR (Part 0b). **No migration**; the next migration stays **105** (Prompt 34b may take it). No `sdk/`, `internal/recovery/`, `cmd/recovery-realm/` or `internal/api/recovery*` file was touched. K5 (#894–#897) and K2b's tail (#906, #907) are other sessions' stacks, still open; #897 did not merge during this prompt.*

Lane: Trust. This prompt serves `docs/ARCHITECTURE_CONTRACT.md` rule 7: "every access-widening act in the transparency log"; "customers read outcomes; auditors read the log".

Two sentences carry this prompt:
- **The tenant logged is the tenant served.** The gate reads each subject with the handler's own parser. A subject that is no tenant is a 404 before the handler runs.
- **The record's switch is itself on the record.** A tenant row can only turn `translog_admin_reads` on. Every write of a `translog_*` flag row is a `log_policy` entry, appended before the row is written, fail closed.

## Step 0

- #921 (the WP-P33 post-merge review) was first in the merge queue at the start and merged at 13:30 UTC; the branch was rebased onto it. `ls internal/database/migrations | tail -1` = `104_translog_subjects.sql`.
- Prod baseline at ~14:15 UTC, equal to the review's 13:05 figures:
  - `/version` `c699eb332a78`, active slot 8000;
  - **0** `"level":"error"` lines in 3 h;
  - `translog_entries` **24** (23 `server_build`, 1 `admin_read` at idx 23), 0 checkpoints, no high-water file (`/opt/vaultaire/data/translog/` does not exist), no signing key;
  - `translog_admin_reads` and `translog_customer_view` ON for tenant zero only (`storedge@teamviera.com`, 11:24 UTC), no global row;
  - all **16** rule files on the box matched `deploy/monitoring/` hash for hash (`vaultaire-translog.yml` `ab29f19a…`).

## Part 0a (#922) — `83dcfbd5`

| Item | Sev | Fix | Red test on `cfeb5a75` (main) | Mutation → caught by |
|---|---|---|---|---|
| RP44-1 | P3 | **One parser** (D-P34a-1). Each resolver calls the handler's own function: `chi.URLParam`; `handlers.ParseAuditFilters` (exported from the page) and the API's new `adminAuditFilter` (shared by `handleAdminAuditList` and `apiAuditScope`; it trims, D-P34a-3); `handlers.AbuseReportID`; `handlers.AdminTenantID` | `TestAdminRead_AuditFilterWhitespace_Logged` (the review's probe on the production dashboard router, the API's real recorder behind a spy, a fresh operator per view so nothing coalesces): `/admin/audit?tenant=" <id>"` "showed the tenant's row (200) and the log has no read of it: [{scope:tenant_audit tenant: <id> err:<nil>}]". The same holds for `"<id> "` and `"\t<id>"`, on the page and on the export (6 failures) | auditScope reads `?tenant=` raw → AuditFilterWhitespace |
| RP44-1 | | the same, for every resolver of both maps | `TestAdminRead_EverySubjectResolverAgreesWithItsHandler`: 11 routes × 8–9 variants (spaces, tab, `%00`, `%0A`, upper case, trailing slash, a repeated parameter both ways, a path-escaped id); a route that shows the tenant's marked record must have logged the tenant. **22 failures** on main: the audit page and export by `tenant` and by `user` with a space, tab or newline (16), and `/admin/support?q=` (6, RP43-4 below) | support search always the list → this test + the RP43-4 test |
| RP44-1 | | **No silent nothing** (D-P34a-2; **D-P33-15 withdrawn**). `RecordAdminRead` checks existence first, whatever the flag. A subject that is no tenant, exactly as written, returns `adminread.ErrNoSuchTenant`; both gates answer 404 (`no-store` on the page) and the handler never runs. `TenantExists`: no trim, no case fold; a NUL or invalid UTF-8 is no tenant, never a DB error | `TestAdminRead_UnresolvedSubject_HandlerNeverRuns`: "the handler ran for tenant-ddb16493-fc0, which is no tenant" (and for `%20<id>` and the upper-cased id). Main answered 200 | `return nil` for a missing tenant → UnresolvedSubject; `TenantExists` with `lower(trim())` → UnresolvedSubject |
| RP44-1 | | the page side | `TestAdminRead_UnresolvedSubject_PageNeverRuns` (dashboard): does not compile on main (`adminread.ErrNoSuchTenant`) | — |
| RP44-2 | P3 | **ON wins** (D-P34a-4). `flags.OnWins` for `translog_admin_reads` and `translog_customer_view`: on when the tenant row OR the global state (the row, else the default) is on. `Set` of a tenant OFF row while the flag is on globally is `ErrNotExemptible`: API 409 `flag_not_exemptible`, dashboard 409. `translog_public` is `GlobalOnly` (400 `flag_global_only`) | `TestAdminReadFlag_PerTenantOffUnderGlobalOn_Refused`: API 200 instead of 409, "a per-tenant OFF row under a global ON was written", the dashboard 303 | the `ErrNotExemptible` case disabled → Refused |
| RP44-2 | | | `TestAdminReadFlag_PerTenantOffUnderGlobalOn_ReadStillLogged` (the OFF row written by SQL): `"[]" should have 1 item(s)` / "the per-tenant OFF row exempted the tenant's read" | `row.enabled` without `\|\| global` → ReadStillLogged |
| RP44-2 | | **The switch is a log entry** (D-P34a-5, D-P34a-6). A new kind `log_policy{flag, action, enabled, scope, nonce, commitment}`. `flags.Service.SetWriteGuard` runs before every `Set`/`Unset`; the server's guard (`internal/api/translog_flags.go`, installed by `useFlags`) appends via `adminread.Store.RecordPolicy` and returns its error. A refusal is `ErrChangeRefused`: 503 `log_frozen` / `flag_change_not_logged`, nothing written, on the API and the dashboard | `TestTranslogFlagChange_AppendsBeforeTheRow` (the test holds the advisory lock of a private log, migrations 103+104): "the row was written before its log entry" | the guard after the upsert → AppendsBeforeTheRow |
| RP44-2 | | | `TestTranslogFlagChange_LogFrozen_Refused_NothingWritten`: 200 instead of 503 for both flags, through the API and the dashboard; "a row was written although its entry was refused" | the guard skips the append → LogFrozen + TenantFindsItsOwn |
| RP44-2 | | the tenant finds it: `Find` returns tenant `log_policy` entries (`kind`, `flag`, `action`, `enabled`) | `TestTranslogFlagChange_TenantFindsItsOwn`: `"[]" should have 2 item(s)` / "A's switch changes in its own view" | `Find` skips log_policy → TenantFindsItsOwn |
| RP44-2 | | the entry | `TestLogPolicy_BodyHasNoIdentity` (golden, a tenant row and the global row), `TestLogPolicy_ForbiddenFieldsAndShapes`, `TestTranslog_EntriesNeverNameATenant` (now samples `log_policy`): do not compile on main | — |
| RP44-2 | | the guard and the rules, unit | `TestWriteGuard_RunsBeforeTheRow_RefusalWritesNothing`, `TestOnWins_TenantRowOnlyTurnsItOn` (global ON by the default, never a `'*'` row of a real flag): do not compile on main | — |
| RP44-4 | note | `tenant_audit_export`, a subject scope of its own | `TestAdminRead_AuditExportIsItsOwnScope` (a view then an export, one operator, one hour → 2 entries): does not compile on main (`ScopeTenantAuditCSV`); on main the export coalesced into the view (the review's probe) | the export logged as the view → AuditExportIsItsOwnScope |
| RP43-4 | note | `/admin/support?q=` with exactly one result, found by the page's own search (`handlers.SupportSearchSubject`), is that tenant's `support_case`; none or several = `tenant_list` | the 6 support rows of the agreement test above (`?q=<id>`, padded, upper-cased: the e-mail `ILIKE` matches), and `TestAdminRead_SupportSearchOfOneTenant_IsItsSupportCase` | support search always the list → both |
| RP43-6 | P4 | The walk test covers every admin route of every method. A route whose answer carries customer data goes through the gate, keyed `"METHOD pattern"` (D-P34a-7): resolve-null `routing_rows`, chunk-orphans and chunk-move `chunk_list`, the three money POSTs `tenant_payments`, the two bulk renewal routes `billing_list`. Every other route is `mutation: <reason>` in `apiAdminWrites` / `adminWriteRoutes`. A flag `PUT`/`DELETE` answers only the row it wrote, so it is a mutation | `TestAdminReadRoutes_AllClassified` (now ≥ 75 routes), `TestAdminWriteRoutes_AllClassified` (dashboard, engine and flags mounted), `TestFlagWrite_AnswersOnlyTheRowItWrote`: do not compile on main | a POST renamed out of `apiAdminWrites` → AllClassified; resolve-null registered without the gate → AllClassified; the write answering every row → AnswersOnlyTheRowItWrote |
| RP43-1 | test | **The cause**: a random body is content-defined, and 6 MiB holds no Rabin boundary past the 1 MiB minimum with probability e^-5 ≈ 0.67 % (2 MiB average). "Two chunk PUTs in flight" then waited 10 s for a second chunk that did not exist. Measured: 25 of 4,000 random 6 MiB bodies (**0.62 %**) and 4 of 4,000 8 MiB bodies (0.10 %; e^-7 = 0.09 %) cut into one chunk. **Fix**: `chunkyBody` draws until the server's chunker cuts at least three, a new draw each time (a run never dedups onto an earlier one). It is used by the two `cutUpload` tests and `TestChunkStore_NoConnectionHeldAcrossTheBackendPut` | `-count=200 -race`, each test alone (test binary, private DB), before → after: | — |
| | | | ARetryOfACutUpload… 0/200 (182 s) → 0/200 (205 s) | |
| | | | ADetachedPutTheBackendAnswersWithAnError… 0/200 (17 s) → 0/200 (17 s) | |
| | | | ADetachedPutWithNoAnswer… 0/200 (402 s) → 0/200 (400 s) | |
| | | | **AStoppingSlotWaitsForTheDetachedChunkPuts 1/200 (145 s), "timed out waiting for chunk PUTs in flight" → 0/200 (154 s)** | |
| RP44-3 | P4 | `docs/TRANSPARENCY.md` § After a database restore step 1 reads the job's result with `sudo -u postgres psql … job_runs`. It says that with the flag ON globally `GET /api/v1/admin/jobs` and `/admin/system` are refused while frozen, and that the trigger and the slot's `/metrics` still work. The freeze list adds that no `translog_*` flag can change | docs | — |

All 13 mutations turned their test red. M4 and M5 were first written as deletions and failed to compile (unused variables), so they were run again as compile-safe mutations: a `&& false` guard, and `_, _ = action, tenant; return nil`.

Also changed: `docs/TRANSPARENCY.md` (the `log_policy` kind row, the scope table, "One subject, one parser", § The switch is in the log, SQL flag rows in § What the log does not see), and the CLAUDE.md rows of `internal/translog`, `internal/flags` and `internal/dashboard`.

### CI on #922

The first build-and-test failed on `TestChunkPutBudget_BuffersAreReturnedOnEveryExit`: "after an abort: Should be zero, but was 1550006". It was 0/100 locally. The budget is private to the test, so nothing else held it. A sibling chunk PUT already at the backend when the first one failed runs to its own end detached (RP14-1) and gives its lease back when it returns, so it can still be returning as the handler answers. The assert now waits for the lease, bounded at 5 s (D-P34a-9). The second run went green, and the PR merged through the queue. Security: gosec 0 issues, 477 files.

### Lab

- The full suite passed: `go test -race ./...` on a private migrated DB (`vaultaire_p34`), 0 FAIL; `make lint` 0 issues.
- `TestTranslogFlagChange_AppendsBeforeTheRow` is the lab of the order. It holds the advisory lock of a private log, so the flag PUT blocks on the append. After 400 ms the row is still absent. The lock is released, the PUT answers 200, and the row and one `log_policy` entry are both there.

### Prod proof, `83dcfbd5`, slot 8001, 15:13 UTC

Deploy run 39975990767 succeeded, and the public `/version` reads `83dcfbd54e7a` (built 14:59:00Z). A 10-minute admin JWT was minted on the box from `JWT_SECRET` and never printed (D-P33-20, D-P34a-8). Only tenant zero (`tenant-4e1b43062db9d685`) and our bench tenant (`tenant-14a623b16b3f7012`) were read.

- Before: 25 entries (24 + this deploy's `server_build` at idx 24), 0 `log_policy`, no global `translog_*` row.
- `PUT /api/v1/admin/flags/translog_admin_reads {"tenant_id":"<tenant zero>","enabled":false}` gave **200** and one `log_policy` entry. There is no global row, so the OFF is not refused. Setting it back ON gave **200** and one more entry:
  ```
  25 {"kind":"log_policy","v":1,"at":"2026-10-23T15:13:51Z","entry":{"flag":"translog_admin_reads","action":"set","enabled":false,"scope":"tenant","nonce":"0114683dea8a9789a4a12015db38ee69","commitment":"0c7deff85a0d005f52473feecda0fb5e8d36a20eaa1c1e856a18431786ff117e"}}
  26 {"kind":"log_policy","v":1,"at":"2026-10-23T15:13:51Z","entry":{"flag":"translog_admin_reads","action":"set","enabled":true,"scope":"tenant","nonce":"c99262216c9c66afb6a140db253deb98","commitment":"194c3714245fe981309493c3ab49d9979986d5685da57134cdd008b93b88c372"}}
  ```
- Padded reads of tenant zero:
  - `GET /api/v1/admin/audit?tenant_id=%20<id>` and `?tenant_id=<id>%20` gave 200, 200, logged as **one** `tenant_audit` entry (the second coalesced into the first):
    `27 {"kind":"admin_read","v":1,"at":"2026-10-23T15:13:51Z","entry":{"scope":"tenant_audit","actor_role":"admin","nonce":"eef247da25ac16051895642e3c071284","commitment":"c87bedd505d5241706a304ffa91797e4b39b81b2fd1b8ac22a3645d479f345e7"}}`;
  - `GET /api/v1/admin/tenants/%20<id>/payments` and the upper-cased id: **404 `{"error":"not_found"}`**, the handler not run.
- The bench tenant's payments: 200, **0** entries appended.
- No new body names tenant zero, the bench tenant, the admin's id or `storedge`: **0**.
- Tenant zero's view (`GET /api/v1/manage/account/transparency`) lists idx 23 (`admin_read`, `tenant_payments`), 25 and 26 (`log_policy`, `translog_admin_reads`, set false / set true) and 27 (`admin_read`, `tenant_audit`). `checkpoint_size` is null everywhere: no signing key.
- `audit_logs`: `flag.set {"tenant": …, "enabled": false, "updated_by": "storedge@teamviera.com"}`, `flag.set {… "enabled": true …}`, `admin.read {"scope": "tenant_audit", "translog_index": 27}`.
- **Both flags are left ON for tenant zero only**, exactly as before (`translog_admin_reads`, `translog_customer_view`); there is still no global row.
- `vaultaire_translog_append_failures_total{kind="log_policy"} 0`: pre-created, from `Kinds()`. The slot's `/metrics` serves the two `append_refusals_total` series, the RP44-3 doc's claim.
- **0** error lines since the switch.
- Rules: no rule file and no new counter family changed, so nothing was installed. The 16 files on the box are as in Step 0.

## Departures (D-P34a-n)

- **D-P34a-1.** One parser by the second of the prompt's two forms: the classifier calls the handler's own exported function (`ParseAuditFilters`, `adminAuditFilter`, `AbuseReportID`, `AdminTenantID`, `SupportSearchSubject`), with no copy. The handlers keep parsing; they are given nothing through the context. Both call the same deterministic function of the same request, so they cannot differ, and `TestAdminRead_EverySubjectResolverAgreesWithItsHandler` is the guard against a second parse creeping into a handler. A path parameter's parser is `chi.URLParam` on both sides; it was already one, and it now has a name on the page side.
- **D-P34a-2.** A subject that is no tenant is a **404 before the handler, whatever the flag**: the existence check comes before the flag check, so it costs one query per subject read even with the flag off. Two subjects that may legitimately no longer exist fall back instead. An audit filter naming a tenant that does not exist is the list scope (an erased tenant's `account.erased` row stays readable). An abuse report naming a tenant that does not exist is `abuse_report`. A user filter whose user has no tenant stays the list (RP43-9). D-P33-15 is withdrawn.
- **D-P34a-3.** The API's audit filter now trims `tenant_id` and `user_id` as the dashboard's does, so a padded id is the tenant, and logged.
- **D-P34a-4.** ON wins for `translog_customer_view` too: no tenant can be singled out of a global view. `translog_public` is read only globally, so a tenant row of it is refused (`GlobalOnly`); clearing such a row is allowed. "On globally" means the global row, else the in-code default.
- **D-P34a-5.** The kind is **`log_policy`**. Beyond the prompt's `flag`, `enabled` and `scope` it carries `action` (`set` | `clear`), a closed list, because a clear is not a set to false: the tenant then follows the global row. A clear's `enabled` is false and validated so. The commitment label is its own, `stored.cloud/translog/log_policy`. There is no dedup key: every write is an entry. A tenant row naming no tenant is refused (404 `tenant_not_found`), because the commitment needs the tenant's salt. A stale row of an erased tenant can only be removed by the erasure (which deletes it) or by SQL.
- **D-P34a-6.** The guard lives in `flags.Service`, the one door both the API and the dashboard write through, and the API server installs it. The entry is appended before the row in a separate transaction. A row write that then fails leaves an entry for a change that did not happen; that is the fail-safe direction (RP45a-3).
- **D-P34a-7.** RP43-6, two choices beyond the two POSTs the review named.
  - The three money POSTs answer the tenant's record (the entitlement, the date, the receipt), so they are logged as `tenant_payments`. Otherwise a no-op adjustment would read past the GET's entry. The cost: while the log is frozen under a global ON, no manual payment or refund can be recorded.
  - The flag `PUT`/`DELETE` answered the flag's every override (the tenants it is set for), which is the `flag_list` read. Rather than gating them, which would make every kill-switch unflippable through the API during a freeze, the answer now names only the row written. They are mutations.
  - Two new list scopes: `routing_rows` and `chunk_list`.
- **D-P34a-8.** The prod proof used the API with a JWT minted on the box (D-P33-20). The dashboard paths are proven against the production router in tests, not on prod (they need the owner's TOTP).
- **D-P34a-9.** `TestChunkPutBudget_BuffersAreReturnedOnEveryExit` (not one of this prompt's items) waits for the abort's buffers, bounded at 5 s (§ CI on #922).

## Residuals (RP45a-n)

| # | Sev | What | Where |
|---|---|---|---|
| RP45a-1 | note | The support search runs twice, once in the gate and once in the page. A tenant created or changed between the two can make the page show one result while the read is logged as `tenant_list`, or the reverse | dashboard |
| RP45a-2 | note | `/admin/tenants?q=` with one result is still `tenant_list`. RP43-4 named the support search only; the tenant list's search shows less (no keys, no IPs) | dashboard |
| RP45a-3 | note | A `log_policy` entry whose row write then failed records a change that was not made. `audit_logs` has no `flag.set` for it, which tells the two apart | api / flags |
| RP45a-4 | note | The dashboard's `/admin/flags` page shows a per-tenant OFF row of an ON-wins flag, written by SQL, as OFF, although the flag is effectively on for that tenant | dashboard |
| RP45a-5 | note | RP43-5 after the OR rule: a list read still follows the global state only, so tenant zero's per-tenant ON logs no list page. By design: a list names many tenants. The global flip closes it | api |
| RP45a-6 | note | A global `translog_*` row deleted or set OFF by SQL turns the record off silently (`docs/TRANSPARENCY.md` § What the log does not see). Only the attested plane closes it (contract rule 10) | — |

**Carried:**
- RP43-2 (the salt in plaintext: wrap it with F-14 or the broker), RP43-3 (`Find` is linear; it now scans `log_policy` too), RP43-7, RP43-8, RP43-9, RP43-10 (Prompt 34b's view), RP43-11, RP43-12 (now in § Before the global flip).
- RP44-5 (a re-appended `admin_read` loses its coalescing key; a re-appended `log_policy` has none to lose); RP41-1; RP41-3 (Prompt 34b); RP42-5 (Prompt 34b closes it).
- RP38-2…4, RP38-7…9; RP34-2, -5, -8, -10, -12; RP35-2…5; RP37-16; RP39-* (Prompt 31's); RP36-* (Prompt 29's).

**Closed:** RP44-1, RP44-2, RP44-3, RP44-4, RP43-1, RP43-4, RP43-6.

## Part 0b — the owner's flip, written down

`docs/TRANSPARENCY.md` § Admin reads now has **"Before the global flip"**, the checklist the owner runs:
1. RP44-1 and RP44-2 closed (#922, `83dcfbd5`);
2. the signing key installed;
3. the witness cron running;
4. `trust.stored.cloud` resolving, with the SAN and the probe target;
5. what a freeze does to operators (RP43-12, RP44-3): no tenant page, audit, job, flag-list, money route, resolve-null or chunk dry run; no `translog_*` change; kill-switches still flip; `psql … job_runs` and the slot's `/metrics`.

The flip itself (`PUT /api/v1/admin/flags/translog_admin_reads {"enabled": true}`, which is itself a `log_policy` entry with `scope: global`) stays the owner's.

## Owner items (note only)

- The signing-key ceremony, and publishing the verifier key outside our infrastructure.
- `trust.stored.cloud` DNS (proxied), the origin certificate's SAN, and `TLS_CERT_PROBE_TARGETS` (RP41-2).
- The witness cron on the Mac, after the ceremony.
- F-33.
- RB-3: `BLOMP_ALLOW_UNSEALED` off before customers.
- **`translog_admin_reads` global ON**, now that Part 0a is deployed, once § Before the global flip passes.
