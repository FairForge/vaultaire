# Stage 1a — Prompt 33: Part 0 (the WP-P32 review's RP42) + K4c-1, admin reads in the transparency log

*2026-10-23. PRs: #916 (Part 0a, the freeze), #917 (Part 0b, the trust host), #918 (Part 0c), #919 (K4c-1, migration **104**), and this docs PR. **Migration 104** (`104_translog_subjects.sql`, K4c-1); **next migration: 105.** No `sdk/`, `internal/recovery/`, `cmd/recovery-realm/` or `internal/api/recovery*` file was touched: K5 (#894–#897) and K2b (#906, #907) are other sessions' stacks.*

Lane: Trust. It serves `docs/ARCHITECTURE_CONTRACT.md` rule 7 ("any act that could widen our access writes to the transparency log … an admin read of customer metadata"; "customers read outcomes; auditors read the log") and rule 11 (`trust.` on stored.cloud), and `docs/surfaces/TRUST.md` items 3 and 13.

Four sentences carry this prompt:
- **A restored database freezes the log, not the service** (#916). Nothing can take index M before the re-append, so the restore always stays possible. The slots may serve customers meanwhile.
- **The trust host is the log and nothing else** (#917).
- **A portal un-cancel needs evidence** (#918). A stop that never landed no longer looks like the customer's hand.
- **An operator reading a tenant's records is a public event** (#919). The entry names no one, and the tenant can find its own.

## Step 0

- #915 and #909 were merged before the start (03:43 and 03:28 UTC). Main was `2c0fc9c2`, and the latest migration was `103_translog.sql`.
- Prod baseline at ~09:50 UTC:
  - `/version` `c6b7faf0d606`, active slot 8000;
  - **0** `"level":"error"` lines in 2 h;
  - `translog_entries` **19** (all `server_build`), 0 checkpoints;
  - no `translog_*` flag row, so `translog_public` is OFF;
  - no high-water file: `/opt/vaultaire/data/translog/` does not exist;
  - all **16** rule files on the box matched `deploy/monitoring/` hash for hash.

## Part 0a (#916) — `f3f3906a`, the log freezes while the database is behind

| Item | Sev | Fix | Red test on `2c0fc9c2` | Mutation → caught by |
|---|---|---|---|---|
| RP42-1 | P3 | `Append` reads the high-water file inside its transaction, under `lockLog`, on every append. `n < hw.Size` gives `behind_highwater`, and a file that exists but does not parse gives `highwater_unreadable`. Either one is `ErrLogFrozen`, writes nothing, and counts in `vaultaire_translog_append_refusals_total{reason}` (pre-created, excluded from `append_failures_total`). An absent file appends. `ReAppend` is not gated | `TestAppend_BehindHighwater_Refused`: `An error is expected but got nil … appended=true` | gate off → BehindHighwater + HighwaterUnreadable; `<` → `<=` → BehindHighwater + all four `TestReAppend_*` |
| RP42-1 | | `TestAppend_HighwaterUnreadable_Refused` (garbage, empty, another origin, a directory) | `ErrLogFrozen` undefined; the append went through | unreadable allowed → HighwaterUnreadable; refusals counted as failures → BehindHighwater |
| RP42-1 boot entry | | `bootBuild`: hashed once at boot and kept while it waits; every `translog_checkpoint` run retries it before checkpointing; an invalid entry is never kept (`TestBootBuild_InvalidEntry_NotRetried`) | `TestRestore_SlotStartedFirst_StillRestores`: on main the slot's boot appended at 6, then `ReAppend` → `ErrReAppendSize` (7 + 4 ≠ 10) | the job's retry removed → SlotStartedFirst; a retried entry kept pending → SlotStartedFirst |
| RP42-1 ceremony | | `Frozen(ctx)` before MFA: 503 `{"error":"log_frozen"}`, the code not consumed, nothing stores the 503 | `TestCeremonyRoute_LogFrozen_503_CodeNotBurned`: `expected: 503 actual: 201`, 1 entry | the pre-check off → the same test (the code was burned) |
| RP42-6 | note | A file of another origin is refused as `highwater_unreadable`, and the error names `mv … highwater.v1`. The step is in the v2 runbook | `TestReadHighwater_AnotherOrigin_NamesTheArchiveStep` | — |

Also:
- Rule **TranslogFrozen** (page), with its promtool test (SUCCESS on the box).
- `TestCheckpoint_DatabaseRestoredBehindHighwater_RefusesToSign` now appends through an ungated store, a build before the gate (the defence-in-depth case).
- The runbook: `docs/TRANSPARENCY.md` § After a database restore and `docs/DEPLOY.md` § Backups.

**Lab** (`vaultaire_p33lab_main` / `_branch`, a lab key, a lab appender never committed, `translog-witness`, signed at 6 and 10, `pg_dump` at 6 → `pg_restore`, then a slot of ANOTHER binary, BuildSHA `abcdef1234567`, started BEFORE `translog-restore`):

| | main `2c0fc9c2` | branch |
|---|---|---|
| the slot's boot | `server build appended`, entries 7 | `server build waits — the log is frozen`, entries 6, `append_refusals{behind_highwater} 1`, `append_failures{server_build} 0` |
| `translog-restore` | `FAIL: … the recomputed root is not the checkpoint's: 2a3db01b… at 10, the checkpoint signed e4cfa404…` | `re-appended entries 6…9: the root at 10 is 09e470ef… = the high-water's` |
| the job's first run | `nothing signed … holds 7 entries but this box signed a tree of 10`, `checkpoint_refusals{behind_highwater} 1` | `server build appended","index":10,"retried":true` then `checkpoint signed","size":11`, refusals 0 |
| the witness | `FAIL: … the log shrank: 6 entries, a checkpoint of 10 was seen before` | `size 11 … consistent with 10` |

**Lab slips, recorded:**
- The first attempt called `vaultaire --version`, which started a server against the dev DB `vaultaire`. It appended nothing, because the dev DB has no translog tables. It was killed.
- A server left over from that attempt held the port, so the second run measured the wrong process. It was rerun with a check that the port's listener is the started PID.
- In the third run, the second binary's BuildSHA `labslot2` is not hex, so both builds refused the entry as invalid. That exposed the "retried for ever" case, which is now fixed (`TestBootBuild_InvalidEntry_NotRetried`).

**Prod**, `f3f3906a`, slot 8001:
- `translog_entries` went from 19 to **20**, the one new entry being idx 19, `server_build` `f3f3906a8b83`, binary `b9ba3258…`;
- every append-refusal and checkpoint-refusal counter is at **0**, with 0 error lines;
- `vaultaire-translog.yml` passed promtool **SUCCESS**, was installed and Prometheus reloaded. The box and the repository agree: `ab29f19adeb5f9c14505c44ab088811f9ee552fad323d13403d8660131f6f4a8`.

## Part 0b (#917) — `e2b04254`, the trust host is the log only

| Item | Sev | Fix | Red on main | Mutations |
|---|---|---|---|---|
| RP42-2 | P4 | `TrustHostRouter` (beside `CDNHostRouter`) applies on a public canonical origin. It serves GET/HEAD of the two log paths behind `translog_public` (a signed or presigned request 404) and `/.well-known/security.txt`; everything else is 404 `text/plain` `no-store`. A dev origin is unchanged | `TestTrustHost_ServesOnlyTheLog` on main: `/` 200 html, `/dashboard/login` 303, `/api/v1/manage/buckets` 401, `POST /auth/register` 400, `/metrics` 200, SigV4 PUT/GET `/bucket/key` 503 (the S3 API) | router never matches → ServesOnlyTheLog + HeadCheckpoint; applied on dev → CanonicalHost_Unchanged; signed served → HeadCheckpoint; no `no-store` → both |
| RP41-6 | note | HEAD on `/translog/…` on the trust router AND the main router (same headers, no body; on other hosts the S3 API's, like GET) | `TestTrustHost_HeadCheckpoint` (`newTrustRouter` undefined on main) | no HEAD route → HeadCheckpoint; no HEAD on main router → CanonicalHost_Unchanged |
| Cache-Control | | The read side's own 404s are now `no-store` (`internal/translog/serve.go`) | `TestTranslogHandler_*`: no header on main | — |

**Cache-Control as served:**

| Answer | Cache-Control |
|---|---|
| checkpoint | `no-store` |
| full tile (hash and entries) | `public, max-age=31536000, immutable` |
| partial tile | `no-store` |
| 503 | `no-store` |
| 404 | was unset → now `no-store` (a tile beyond the checkpoint, or the checkpoint before the first signature, exists later; a 404 served while the flag is OFF must not outlive the flip at an edge) |
| trust-host 404 | `no-store` |

**Prod**, `curl --resolve <host>:443:127.0.0.1`:

| Host / path | Before (`c6b7faf0`) | After (`e2b04254`) |
|---|---|---|
| `trust.stored.cloud` `/` | 200 text/html | **404 text/plain** |
| `trust.stored.cloud` `/dashboard/login` | 303 | **404 text/plain** |
| `trust.stored.cloud` `/translog/checkpoint` | 404 text/plain | 404 text/plain; HEAD 404 `cache-control: no-store` |
| `trust.stored.ge` (the same three) | 200 / 303 / 404 | **404 / 404 / 404** |
| `stored.cloud` `/`, `/dashboard/login`, `/translog/checkpoint` | 200 / 303 / 403 S3 XML | 200 / 303 / 403 S3 XML |
| `stored.ge` (the same) | 200 / 303 / 403 S3 XML | 200 / 303 / 403 S3 XML |

## Part 0c (#918) — `2951afce`

| Item | Sev | Fix | Red test on `2c0fc9c2` | Mutation → caught by |
|---|---|---|---|---|
| RP42-3 | P4 (**now proven**) | `grantStripe` reads an un-cancel only from **evidence**: `subscriptionSignal(event)` (`previous_attributes.cancel_at_period_end == true`), or the owed stop row `stopped`. An `invoice.*` grant never counts. Without evidence, while a switch is in progress (stop pending or `pending_provider`), the grant keeps `prepaid`, the switch and the row (`stripeGrant.kept`, a Warn line). With no switch in progress the live state is written with no withdrawals | `TestUncancel_StopNeverLanded_SwitchKept` (the Stripe fake 500s the stop): `expected "prepaid" actual "auto"`, `expected "btcpay" actual ""`, the stop row gone | evidence always true → StopNeverLanded + InvoiceEvent; `previous_attributes` ignored → PreviousAttributesTrue + both PortalUncancel tests; stopped-row evidence off → StopConfirmed; invoice flag ignored → InvoiceEvent/stop confirmed; kept writes auto → StopNeverLanded + InvoiceEvent; withdrawals without evidence → NoSwitchNoEvidence |
| RP42-4 | test | `cutUpload` samples `inFlight` after the handler answered, once the count has held still for 200 ms | `-count=200 -race` of `TestChunkClaim_AStoppingSlotWaitsForTheDetachedChunkPuts` alone: main **1/200** `expected: int32(2) actual: int(3)`; branch **0/200** | — |
| RP41-4 | note | `Checkpoint` records the DB checkpoint size before any refusal | `TestCheckpoint_BehindHighwater_ReportsTheDatabaseCheckpoint`: `expected: 5 actual: 0` | the move undone → the same test |

Stripe on `data.previous_attributes` (docs.stripe.com/api/events/object): *"Object containing the names of the updated attributes and their values prior to the event (only included in events of type `*.updated`)."* The cancel page says reactivation is "updating `cancel_at_period_end` to `false`", and that `customer.subscription.updated` is "sent for any subscription update". Stripe does not state in so many words that an un-cancel carries `cancel_at_period_end` in `previous_attributes`. That follows from the field's definition; see RP43-7.

Two tests that modelled a real un-cancel without the field now send it: `TestPortalUncancel_AfterDurableSwitch_WithdrawsTheStop` and `TestPortalUncancel_RestoresAuto_InvalidatesPendingInvoice`.

**RP42-4, the measurement.** The prompt's `-count=200` over the four `TestChunkClaim_*` tests in one process does not measure the flake. Some of the tests count every pending fixed claim in the global scope, and `…WithNoAnswerStillKeepsItsClaimForTheWindow` keeps its claim for 60 s on purpose. So the iterations fail each other: 489 and 543 failures, before and after alike, all "pending" or claims left behind.

My first attempt also ran before and after concurrently on one database, which made it worse. The flaky test was therefore run alone, one run after the other.

A different wait, "timed out waiting for chunk PUTs in flight" (before the sample point, untouched), failed 1/200 before and 4/200 after in that mode. That is RP43-1.

## K4c-1 (#919) — `c699eb33`, admin reads in the transparency log (migration 104)

Contract rule 7: *"an admin read of customer metadata"* writes to the log. Before #919, an operator reading one tenant's records left a row in `audit_logs` (which we can rewrite) and nothing in the log.

### The inventory (every admin GET route, read in its handler, template and SQL)

| Route | Reads | Verdict | Scope |
|---|---|---|---|
| `GET /api/v1/admin/tenants/{tenant_id}/payments` | `entitlements` + `payments` ledger (provider refs, invoice numbers) of one tenant | **log** | `tenant_payments` |
| `GET /api/v1/admin/audit` | `audit_logs` (tenant, user, IP, user agent, metadata); filters `tenant_id`, `user_id` | **log**: one tenant when filtered (a user's → the user's tenant), else no subject | `tenant_audit` / `audit_list` |
| `GET /api/v1/admin/jobs` | `job_runs` notes/results — name tenant ids, user ids, `tenant bucket/key` (smart demotion), settling tenants | **log, no subject** | `job_list` |
| `GET /api/v1/admin/flags` | `feature_flags` overrides with their tenant ids | **log, no subject** | `flag_list` |
| `GET /api/v1/admin/routing-truth` | counts per (table, backend); the job's counts | exempt: no tenant, bucket or key | — |
| `GET /api/v1/admin/backends/{backend}/plan` | `backend_plans` | exempt: what we bought from a vendor | — |
| `GET /api/v1/admin/breaches`, `/api/compliance/breach*` | `breach_records` (no tenant/user column) | exempt: the operator's breach register | — |
| `/api/compliance/activities`, `/ropa/*`, `/consent/purposes` | the processing activities | exempt: no customer | — |
| `/api/compliance/sar/{id}`, `/inventory`, `/consent*`, `/privacy/purpose/…` | scaffolding: a fixed answer, or a user context `requireJWT` never sets (R11-01) | exempt: reads nothing of anyone | — |
| dashboard `/admin/` | counts + the 10 newest users' e-mail and company | **log, no subject** | `tenant_list` |
| `/admin/tenants` | id, name, e-mail, plan, quota of up to 100 tenants | **log, no subject** | `tenant_list` |
| `/admin/tenants/{id}` | one tenant's account, quota, bandwidth, egress, entitlement, ledger | **log** | `tenant_detail` |
| `/admin/system` | runtime stats + `job_runs` notes (tenant ids, bucket/key) | **log, no subject** | `job_list` |
| `/admin/audit`, `/admin/audit/export` | `audit_logs`; filters tenant, user, actor, IP | **log**: tenant/user filter → one tenant, else no subject | `tenant_audit` / `audit_list`, `audit_export` |
| `/admin/waitlist`, `/export` | waitlist e-mails | **log, no subject** | `waitlist_list`, `waitlist_export` |
| `/admin/stats` | site statistics, daily counts | exempt: no tenant, no e-mail | — |
| `/admin/revenue`, `/admin/costs` | top customers' e-mail; margin per tenant by e-mail | **log, no subject** | `billing_list` |
| `/admin/dedup` | dedup per tenant by e-mail | **log, no subject** | `usage_list` |
| `/admin/notifications` | notifications with tenant ids | **log, no subject** | `notice_list` |
| `/admin/notifications/count` | one count | exempt | — |
| `/admin/abuse` | reporters' e-mails, reported URLs | **log, no subject** | `abuse_list` |
| `/admin/abuse/{id}` | one report: reporter, URL, the accused tenant | **log**: the report's `tenant_id` when set, else no subject | `abuse_case` / `abuse_report` |
| `/admin/support` | a search over tenants (e-mail, id, key id, Stripe id) | **log, no subject** | `tenant_list` |
| `/admin/support/{id}` | one tenant: detail + Stripe customer id, events, S3 errors (bucket, key, source IP), notes | **log** | `support_case` |
| `/admin/backends` | drivers and their health | exempt: no customer | — |
| `/admin/flags` | flag overrides with tenant ids | **log, no subject** | `flag_list` |

The classification lives beside the routes:
- the API side in `internal/api/admin_reads.go` (`apiAdminReads`, wrapped through `s.adminRead(pattern, h)`);
- the dashboard side in `internal/dashboard/admin_reads.go` (`adminReadRoutes`, registered through `adminGet`).

An unclassified route panics at registration. `TestAdminReadRoutes_AllClassified` walks the whole `NewServer` router and checks every GET under `/api/v1/admin/`, `/api/compliance/` and `/admin`:
- it is classified;
- it is registered through the gate (`adminReadWrapped`);
- every classification names a live route.

`TestAdminReadRoutes_ExemptAreTheAggregates` and `_APIExemptAreTheAggregates` pin the exempt sets, so moving a page to exempt is a visible change of a list.

### The entry, the commitment, the coalescing

- **`admin_read {scope, actor_role, nonce, commitment}`**, decoded strictly. The field is `commitment`, not `subject` (D-P33-11). Golden body:
  `{"kind":"admin_read","v":1,"at":"2026-10-23T12:00:00Z","entry":{"scope":"tenant_detail","actor_role":"admin","nonce":"0a…0a","commitment":"c3…c3"}}`.
  A body carrying `tenant_id`, `email`, `object_key`, `ip`, `actor`, `subject` or `user_id` does not decode. A person as `actor_role`, an off-list scope, a subject scope without its commitment, or a list scope with one: refused (`TestAdminRead_BodyHasNoIdentity`).
- **The commitment.** `commitment = HMAC-SHA256(salt_t, "stored.cloud/translog/admin_read" ‖ nonce)`, with a fresh 16-byte nonce per entry. `salt_t` is 32 random bytes in `translog_subjects` (migration **104**):
  - created at the tenant's first admin read;
  - deleted by `account.EraseRows` (`TestEraseRows_RemovesTranslogSubjects`, and the schema walk);
  - excluded from the export, as key material.
  - The public cannot link two reads of one tenant, and after the erasure no one can (`TestAdminRead_SubjectUnlinkableWithoutSalt`).
- **Coalescing.** One entry per (operator, scope, tenant) per UTC hour (`TestAdminRead_RefreshCoalesced`). The key lives in `translog_entries.dedup_key`, which the tiles never serve:
  - `admin_read:HMAC(salt_t, operator ‖ scope ‖ hour)` for a tenant read;
  - `SHA-256(operator ‖ scope ‖ hour)` for a list.

### Fail closed, append first

- `TestAdminRead_TenantPayments_AppendsBeforeServing` (API) and the dashboard's `TestAdminRead_TenantDetail_AppendsBeforeServing`: the entry exists when the handler runs, the body names no one, the audit row `admin.read` carries `translog_index`, and a refresh adds neither an entry nor an audit row.
- `TestAdminRead_LogFrozen_ReadRefused`: a frozen log gives 503 `log_frozen` and nothing is read; any other recorder failure gives 503 `admin_read_not_logged`; with the flag OFF, today's behaviour.
- `TestAdminRead_EveryLoggedPageFailsClosed`: the production dashboard router with a recorder that always fails. Every logged page answers 503; every exempt page is served without asking.
- `_LogDown_PageHandlerNeverRuns`.
- A subject that is not a tenant records nothing.

### Customers read outcomes

`GET /api/v1/manage/account/transparency` (JWT, behind `translog_customer_view`) returns `{index, scope, at, checkpoint_size}` for each of the tenant's entries: the first signed checkpoint whose tree includes it, or null. It has an OpenAPI entry.

`TestCustomerTransparency_FindsOwnEntriesOnly`: A sees its two entries and B its one; a tenant without the flag gets 404; a tenant never read gets an empty list. `TestFind_NamesTheCheckpointThatIncludesEach`.

### Mutations (each caught)

| Mutation | Caught by |
|---|---|
| serve, then append | AppendsBeforeServing + LogFrozen |
| a failed append ignored | LogFrozen |
| commitment without the nonce | SubjectUnlinkable |
| coalescing key without the hour | RefreshCoalesced + NamesTheCheckpoint |
| `Find` skips the HMAC check | FindsOwnEntriesOnly |
| the erase rule removed | RemovesTranslogSubjects + TestErasedTablesCoverSchema |
| the dashboard's 503 path without `return` | **survived** `EveryLoggedPageFailsClosed` (the status was already 503) → `_LogDown_PageHandlerNeverRuns` added, now caught |
| `/api/v1/admin/flags` registered without the wrapper | AllClassified |
| the flag ignored | LogFrozen |
| `/admin/revenue` downgraded to exempt | **survived** the sweep → `_ExemptAreTheAggregates` added, now caught |
| `/api/v1/admin/jobs` downgraded to exempt | `_APIExemptAreTheAggregates` |

Tests changed:
- `TestReAppend_RefusesUndecodableBody` now uses an unknown kind, because `admin_read` with `{}` decodes now.
- `TestTranslog_OnlyTheStoreWritesTheLogTables` now matches the log's three tables only (`translog_subjects` is tenant data, written by `adminread` and `account`).

### Lab (a private DB, a lab key, `translog_public` ON, both flags ON for tenant "zero")

```
1. GET payments of tenant zero ×2: 200, 200 → admin_read entries: 1
2. GET payments of another tenant (flag off): 200 → admin_read entries: 1
3. entry 1: {"kind":"admin_read","v":1,"at":"2026-10-23T10:35:16Z","entry":{"scope":"tenant_payments","actor_role":"admin","nonce":"b504d7b5…","commitment":"af60e063…"}}
   names tenant zero? f   dedup_key admin_read:af571b67…   audit admin.read {"scope": "tenant_payments", "translog_index": 1}
4. tenant zero's view (unsigned): {"data":[{"index":1,"scope":"tenant_payments",…,"checkpoint_size":null}],…}   another tenant's view: 404
5. checkpoint signed at 2
6. tenant zero's view (signed): … "checkpoint_size":2
7. translog-verify -entry 1: signature OK — entry 1: included OK
```

### Prod proof on tenant zero (`tenant-4e1b43062db9d685`), `c699eb33`, slot 8000, 11:25 UTC

- Migration 104 is applied.
- A 10-minute admin JWT was minted on the box from `JWT_SECRET`, never printed (D-P33-20).
- `PUT /api/v1/admin/flags/translog_admin_reads` and `…/translog_customer_view` with `tenant_id` tenant zero: **200, 200**, audited by the flags API.
- Before: 0 `admin_read` entries, 23 entries in all (19 + the `server_build` of each of the four deploys).
- `GET /api/v1/admin/tenants/tenant-4e1b43062db9d685/payments` twice gave 200, 200, and **exactly one** entry, idx 23:
  `{"kind":"admin_read","v":1,"at":"2026-10-23T11:25:11Z","entry":{"scope":"tenant_payments","actor_role":"admin","nonce":"827208d0846a9210292ea544b1b8550c","commitment":"1de60d047bc8f21dc1476f431aac7ea0c2e5268d2ac643d5c9ff5df7807ce85e"}}`.
  The body matches neither the tenant id, the admin's id nor `storedge`: **f**.
- Another tenant's payments, the bench tenant `tenant-14a623b16b3f7012` (ours, flag off), gave 200 and **no** new entry.
- Tenant zero's view: `{"data":[{"index":23,"scope":"tenant_payments","at":"2026-10-23T11:25:11Z","checkpoint_size":null}],"object":"list","origin":"stored.cloud/translog/v1"}`.
  `checkpoint_size` is null because there is no signing key on prod, so inclusion is proven in the lab (`translog-verify` above).
- `audit_logs`: `admin.read {"scope": "tenant_payments", "translog_index": 23}`.
- 1 salt row; 0 error lines.
- **Both flags are left ON for tenant zero only.**

**What ON globally means** (the owner's flip): every admin read of customer metadata is in the public log before it is served. While the log is frozen, or the database refuses the append, an operator cannot read a tenant's records through the application. There is no break-glass (RP43-12).

## Departures (D-P33-n)

- **D-P33-1.** `TranslogFrozen` is its own page rule, not folded into `TranslogCheckpointRefused`. A restore fires both, and they say different things: "nothing is signed" and "producers are turned away (a ceremony 503, an admin read refused)".
- **D-P33-2.** The refused boot entry is retried from the `translog_checkpoint` job's run, before it checkpoints, not by a goroutine with backoff. That makes "lands at N, then the job signs" one run.
  - The binary is hashed once at boot, so the entry is always the running binary's.
  - An invalid entry is never kept.
  - A failure to hash is lost as before (an Error line and the counter).
- **D-P33-3.** RP42-6: a file of another origin is refused (`highwater_unreadable`), never read as "archived". Archiving is the operator's `mv … highwater.v1`, which the error names and the v2 runbook writes down.
- **D-P33-4.** The gate reads the file on every append, with no cache.
- **D-P33-5.** In the case-3 runbook the file is created as `user1`, the slots' user on the box.
- **D-P33-6.** On the trust host, `GET /` gets a 404, not a 301 to the canonical origin. No trust page exists yet, and a 301 would put the app's login one click from the origin readers are taught to trust.
- **D-P33-7.** The log handler's own 404s are now `no-store` (`serve.go`, with a test).
- **D-P33-8.** HEAD on `/translog/…` is routed on the main router too. RP41-6 is closed on every host; on the S3 hosts it is the S3 API's, like GET.
- **D-P33-9.** RP42-3: with no evidence and no switch in progress, the live state is written with none of the un-cancel's withdrawals. That changes things for a customer who never switched: an ambiguous signal no longer invalidates open prepaid invoices.
  - An invoice event never counts, even after a confirmed stop.
  - RP40-1's `renewsAgain` branch in the stop job stays as the defensive path.
- **D-P33-10.** RP42-4 was measured on the flaky test alone (§ Part 0c).
- **D-P33-11.** The field is `commitment`, not `subject`: `TestTranslog_EntriesNeverNameATenant` forbids `subject` as a field token.
- **D-P33-12.** The coalescing key of a tenant read is `HMAC(salt_t, operator ‖ scope ‖ hour)`, not the prompt's `sha256(actor ‖ scope ‖ tenant ‖ hour)`. A plain hash of the tenant id can be recomputed by anyone holding the id, and the `account.erased` audit row keeps it. That would break "after an erasure, no one can link".
- **D-P33-13.** The subject scopes are an explicit set of five. Scopes added beyond the prompt's examples, from the inventory: `abuse_report`, `billing_list`, `usage_list`, `flag_list`, `job_list`, `notice_list`. A filtered audit export is `tenant_audit`.
- **D-P33-14.** The admin-only `/api/compliance` group is classified too (all exempt), because it is in the walk.
- **D-P33-15.** A subject read of a tenant that does not exist records nothing.
- **D-P33-16.** An audit view filtered to a user is logged against the user's tenant (`tenants.email = users.email`). An actor or IP filter is a list.
- **D-P33-17.** The `admin.read` audit row is written only when an entry is appended, so a coalesced refresh writes none.
- **D-P33-18.** The customer view lists subject entries only. Reads across tenants carry no commitment.
- **D-P33-19.** `TestReAppend_RefusesUndecodableBody` and `TestTranslog_OnlyTheStoreWritesTheLogTables` were changed (§ K4c-1, tests changed).
- **D-P33-20.** The prod proof read tenant zero through the API payments route with a JWT minted on the box, not through the dashboard tenant page. The page needs the owner's TOTP, and minting a session would bypass MFA. The page's gate is proven by the dashboard tests against the production router. The "other tenant" read used our own bench tenant, so no customer's record was read.
- **D-P33-21.** The salt is stored as plain bytes (RP43-2).

## Residuals (RP43-n)

| # | Sev | What | Where |
|---|---|---|---|
| RP43-1 | test | Under `-count` in one process, `TestChunkClaim_AStoppingSlotWaitsForTheDetachedChunkPuts` sometimes times out before its first sample point: "timed out waiting for chunk PUTs in flight", 1/200 on main, 4/200 after. The four `TestChunkClaim_*` tests are not repeat-safe together. Not seen at `-count=1` in CI | api tests |
| RP43-2 | P4 | `translog_subjects.salt` is plaintext. A leaked database dump links the public admin_read entries to tenants. Wrap it under the master key once F-14 lands, or under the key broker (43.7) | translog/adminread |
| RP43-3 | note | `Find` scans every admin_read entry. Fine at today's counts; an index will be needed later | translog/adminread |
| RP43-4 | note | `/admin/support?q=<tenant id>` pins one tenant but is logged as `tenant_list` | dashboard |
| RP43-5 | note | List reads follow the GLOBAL flag row, so tenant zero's per-tenant ON logs no list page | api |
| RP43-6 | P4 | The gate covers GET routes only. Admin POSTs that answer customer data are not classified: `POST /api/v1/admin/routing-truth/resolve-null` returns tenant/bucket/key, and `POST /chunk-orphans` is a dry-run listing | api |
| RP43-7 | note | If Stripe ever sent a portal un-cancel without `previous_attributes.cancel_at_period_end`, the switch would be kept and the stop job would re-assert the stop, undoing the customer's re-enable. This is the fail-safe direction | billing |
| RP43-8 | note | The `admin.read` audit row is best effort (`audit.Record` after the append) | api |
| RP43-9 | note | `TenantOfUser` joins on e-mail. A user whose e-mail no longer matches a tenant is logged as a list read | translog/adminread |
| RP43-10 | note | The customer view is API only. The dashboard's "access to your account" view is K4c-2 | dashboard |
| RP43-11 | note | An admin read through a JWT minted on the box (the prod proof's) is in the log, but the minting is not ("what the log does not see") | — |
| RP43-12 | note | With the flag ON globally, an incident that freezes the log also stops operators reading tenants' records through the app until the re-append (by design, no break-glass). `docs/TRANSPARENCY.md` says so | docs |

**Carried:**
- RP41-1 (a re-appended ceremony has no dedup key) and RP41-3 (a third-party co-signature, K4c-2): still open.
- **RP41-5 closed** (Part 0a).
- **RP41-6 closed** on every host (D-P33-8).
- RP42-5 (no lock on the witness directory): open.
- **RP42-1, RP42-2, RP42-3, RP42-4, RP42-6 and RP41-4 closed.**
- RP38-2…4, RP38-7…9; RP34-2, -5, -8, -10, -12; RP35-2…5; RP37-16; RP39-* (Prompt 31's); RP36-* (Prompt 29's).

## Owner items (note only)

- The signing-key ceremony, and publishing the verifier key outside our infrastructure.
- `trust.stored.cloud` DNS (proxied), plus the origin certificate's SAN and `TLS_CERT_PROBE_TARGETS` (RP41-2). The trust host now serves the log only.
- The witness cron on the Mac, after the ceremony.
- **F-33**, now narrower: a new origin is needed only when no witness cache reaches N. The freeze keeps the re-append possible otherwise.
- RB-3: `BLOMP_ALLOW_UNSEALED` off before customers.
- **`translog_admin_reads` global ON**, after reading `docs/TRANSPARENCY.md` § Admin reads (no break-glass, RP43-12); `translog_customer_view` when the customer copy is wanted.

## Post-merge review (2026-10-23)

Reviewed on `e3b48288` (main; prod `/version` `c699eb332a78`, built 11:06:55Z, slot 8000). The four product diffs (#916–#919) were read against the code paths they touch, and two throwaway tests were run on a private database (`vaultaire_test_rp43`, migrated through 104; neither test is committed). CI, Security, CodeQL and Deploy are green on main. **Verdict: the freeze, the trust host and the un-cancel evidence hold. K4c-1's entry, commitment and fail-closed order hold, but the gate has two holes. No P0, P1 or P2. Two P3s, both proven. Each lets an operator read one tenant's records with no entry, even once `translog_admin_reads` is ON globally.** The flag is ON for tenant zero only, so no customer's read has gone unlogged. Both must be closed before the owner's global flip.

### Findings (RP44-n)

| # | Sev | What | Proof | Where |
|---|---|---|---|---|
| RP44-1 | **P3** proven | **The classifier and the handler parse the subject differently, and a subject that does not resolve records nothing.** `auditScope` (dashboard) reads `q.Get("tenant")` raw. `HandleAdminAudit` and its export read `strings.TrimSpace(q.Get("tenant"))`. So `?tenant=%20<id>` reaches `RecordAdminRead` as `" <id>"`. `TenantExists` is false, and the recorder returns nil with no entry: "a subject scope naming no tenant that exists records nothing" is a fail-open default. The handler then trims, matches exactly and serves the tenant's audit trail. The same happens for the CSV export. `?user=%20<uuid>` falls to the list scope in the same way: under a per-tenant ON that is unlogged too, and under a global ON it is logged as a list, not against the tenant. The global flag does not help, because the recorder's early return comes after the flag check | A probe on the production dashboard router (`RegisterRoutes` with a DB), using a recorder that is `RecordAdminRead`'s logic. `/admin/audit?tenant=<id>` → 200, the tenant's row shown, **1** entry. `?tenant=%20<id>` → 200, the row shown, **0** entries (the recorder saw `"tenant_audit\| <id>"`). `/admin/audit/export?tenant=%20<id>` → 200 CSV with the row, **0** entries | `internal/dashboard/admin_reads.go` `auditScope`; `internal/api/admin_reads.go` `RecordAdminRead` (`!exists → return nil`) |
| RP44-2 | **P3** proven | **The gate's own switch is not in the log.** `flags.Enabled` reads the per-tenant row first. Under a global ON, a per-tenant OFF row for `translog_admin_reads` is therefore a silent exemption for that one tenant: `PUT /api/v1/admin/flags/translog_admin_reads {tenant_id, enabled:false}`, read, then `DELETE`. Only `audit_logs` sees the two flag writes, and that is the table the log exists not to depend on. A global OFF is just as silent. Contract rule 7 says every access-widening act goes in the log, and switching off the record of reads is one | A probe on the API fixture (`setupReadFixture`, per-tenant ON): set the tenant's row OFF, `Refresh`, GET `/api/v1/admin/tenants/{id}/payments` → **200** with the ledger, then set it ON. `translog_entries` before 0, after **0**; the tenant's own view is empty | `internal/flags/service.go` `Enabled`; `internal/api/flags*.go`; `internal/dashboard/handlers` flag set/clear |
| RP44-3 | P4 | With `translog_admin_reads` ON globally and the log frozen, `GET /api/v1/admin/jobs` and `/admin/system` are refused (`job_list`). The restore runbook checks the `translog_checkpoint` run's result after the re-append, and the operator's way to see it is gone exactly then. `POST …/jobs/translog_checkpoint/run` still works. The runbook should name `psql … job_runs` for that step, or `job_list` could be classified so it reads only rows that name no tenant | reading `docs/TRANSPARENCY.md` § After a database restore step 1 | docs |
| RP44-4 | note | The audit CSV export of one tenant is the same scope as the view (`tenant_audit`, D-P33-13), so an export within the hour of a view appends nothing. The probe's first row shows it: view 1 entry, export 0. The public cannot tell a bulk export from a page view. A subject scope of its own (`tenant_audit_export`) would make the export visible | the RP44-1 probe, first two lines | `internal/dashboard/admin_reads.go` |
| RP44-5 | note | A re-appended `admin_read` loses its coalescing key, because `ReAppend` takes the dedup key from the body and the HMAC'd key is not in it. A re-read in the same hour after a restore appends one more entry. This is harmless and is the same class as RP41-1 | reading `logstore.ReAppend` | translog |

### What holds (checked)

- **The freeze (#916).** The gate sits inside `Append`'s transaction, after `lockLog` and the size read. The dedup lookup comes first, so a read already logged in that hour is served while the log is frozen (it is in the log). `ReAppend` is ungated. `Frozen()` is advisory only and is re-checked under the lock. The boot entry is hashed once, and an invalid entry is never kept. The refusal counters are pre-created. On prod: `vaultaire_translog_append_refusals_total{behind_highwater,highwater_unreadable}` 0, `append_failures_total{admin_read,key_ceremony,server_build}` 0, and still no high-water file (no key).
- **The trust host (#917).** `TrustHostRouter` wraps the CDN router, so it is reached first. `IsTrustHost` normalizes the host. The 404s are `no-store`. A signed request on the trust host is 404, never the S3 API. A dev origin is unchanged.
- **The un-cancel evidence (#918).** The evidence is read from the event (`previous_attributes`) while the state is read from the fetched subscription. Out-of-order delivery cannot forge it: the switch's own stop sends `previous_attributes.cancel_at_period_end = false`, never true. An `invoice.*` grant never counts. The live-state fallback writes no withdrawals.
- **K4c-1 (#919).** `requireAdmin` wraps `adminRead`, so a non-admin cannot append. Every admin GET on both routers goes through the gate, and the walk test enforces it. `{id}` on `/admin/tenants/{id}` and `/admin/support/{id}` is the tenant id the handlers read (`loadTenantDetail`). The commitment is unlinkable without the salt. The salt is erased and excluded from the export. The body allow-list is strict.

### Prod (13:05 UTC)

- `/version` `c699eb332a78`, active slot 8000;
- **0** error lines in 3 h. The 10 warn lines are the expected `BLOMP_ALLOW_UNSEALED` (RB-3) and HEAD cache misses;
- `translog_entries` 24: 23 `server_build` + 1 `admin_read` (idx 23);
- 0 checkpoints, 1 `translog_subjects` row;
- the `translog_checkpoint` job ok: `no signing key`, `first run: no high-water file yet`;
- both `translog_*` flags ON for tenant zero only, set by `storedge@teamviera.com`;
- one `admin.read` audit row (`translog_index` 23);
- all 16 rule files on the box match `deploy/monitoring/` hash for hash (`vaultaire-translog.yml` `ab29f19a…`).

### Process

- #920 (this note's docs PR) merged at 11:52 UTC.
- Still open and unreviewed, not touched: K5 #894–#897 (#895 UNSTABLE) and K2b #906/#907.
- RP44-1 and RP44-2 → **Prompt 34 Part 0** (before the global flip). The owner's item "`translog_admin_reads` global ON" now waits for them.
