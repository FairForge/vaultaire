# WP-P38 post-merge review (Prompt 38, Sessions A and B) — RP54

*2026-10-24, the session after both halves merged. Reviewed what MERGED on `main` (#937 `8d17d96f`, #938 `3d16ae43`, #940 `2e772d2e`, #939 `a2dfce6b`, #942 `390a1236`, #943 `2546cbee`), never the notes. Own worktree off `origin/main`, own database `vaultaire_test_rev38`. Fix PRs, all merged and deployed: #944 `cf41f162` (Step 0 docs), #946 `07d29124` (RP54-1), #945 `70ad5946` (RP53-7); this note's PR last. **No migration** (the next is still **108**).*

## Step 0 — the docs drift (#944)

#942 (14:11 UTC) merged before #939 (14:14) and #943 (14:51), so `WP-P38.md`, STATUS, the plan and CLAUDE.md described Part 0 as in flight. Each claim below was checked against the merged diff, not the notes:

| Claim | Where | Was | Now (#944) |
|---|---|---|---|
| `RECOVERY_*` all three or none; none = no realm asked | CLAUDE.md env rows | the rows said only "503 while the flag is on" | the subject-key row: an erasure asks no realm, `realms_not_configured: true` + a note; the realms-file row: stage b3, what defers, a removed realm is never asked again |
| the account-deletion paragraph names stage b3 and the three audit fields | CLAUDE.md Key Database Tables | sweep → `EraseRows`, no realms | "then the recovery realms (stage b3 …)" + `realms_deleted` / `realms_deferred` / `realms_not_configured` with their meanings |
| `make lint-wasm` in Build/Lint | CLAUDE.md | absent | present, with what it runs and why |
| the client repro paragraph names the checkout warning | CLAUDE.md translog bullet, `docs/TRANSPARENCY.md` § Client builds | "fails on a different hash" only | + the checkout's own builds and the `::warning::` |
| STATUS and the plan say Part 0 merged, with SHAs | `docs/STATUS.md`, plan K5 row | IN FLIGHT | the three SHAs; the K5 row says the erasure reaches the realms |
| `WP-P38.md`'s Part 0 links the merged note, no "in flight" | `docs/reviews/WP-P38.md` | in flight, "owed when #939 merges" | merged, per PR, the owed rows done by this review |
| `sdk/CLAUDE.md` lint recipe | | the manual recipe | `make lint-wasm` |

Rule 8 held: every sentence was written from `a2dfce6b`, `2e772d2e`, `2546cbee`.

## Part 1 — Session A's code (RP52-3, RP51-3, RP51-2)

| # | Question | Evidence | Verdict |
|---|---|---|---|
| 1 | **Placement and order.** Can `account.erased` / `account.erasure_settling` be written while a realm may hold the record? | `eraseTenant` (`deletion_runner.go`): b3 runs after the sweep, before `recordSettles` and `EraseRows`; every error path before stage c returns `defer_` (outcome deferred, no erased row). `TenantID == ""`: b3 still runs on `d.UserID`. Cancel paths: `StillDue` false at the top → cancelled before b3; `ErrNotPending` at `EraseRows` → cancelled AFTER b3 — the account stays, the realm record is gone (like the objects: "the grace period is the time to change one's mind"). A run cut by `TenantDeadline` / `MaxRunTime` / a shutdown mid-realm: the ctx dies, `client.Do` fails, `ErrTransient` retries fail at once, `Delete` errors → deferred. The settle path (`erasure_settle.go`) writes `account.erased` only for a tenant whose rows went — after b3 — EXCEPT a settle recorded by a build before #939, which completes under this one with no realm Delete (RP54-5: nothing owed on prod, `RECOVERY_*` has never been set) | holds; the late cancel is now said in the log (#946, RP54-8) |
| 2 | **The token window.** The SQL; the grace; RP53-3/4 | `recentRealmToken`: `audit_logs.timestamp` (TIMESTAMPTZ), `user_id` (UUID = a uuid string), `make_interval(secs => $2)` with `(10 min + 1 min).Seconds()` — right, and the test proves it both ways (a fresh row defers, an 11-minute-old one does not). The grace: the realm accepts `exp` + 5 s leeway; the row's `NOW()` is the DB clock, the token's `iat` the app clock, both on one box; 1 min covers it. RP53-3 and RP53-4 were under-rated as a pair: on main a user whose erasure is owed could still mint after the check, and the row is written best effort AFTER the tokens are served — `TestRecoveryTokens_NoTrailNoTokens` on main: 200 with tokens and 0 rows. **Closed by #946 (RP54-1):** one transaction — the user row under `FOR SHARE` (`account.DueShared`; due or gone = 409 `account_erasure_due`; the grace period blocks nothing, D-16), the mint, the trail through `audit.RecordTx`, committed before a token leaves (503 `trail_not_written`). The erase transaction's `FOR UPDATE` waits for it, so a token's row is always visible to the check or the user row is gone before the mint reads it | RP54-1 fixed |
| 3 | **Transport.** A slow realm; a 401 realm | `HTTPTransport.Send`: 5 attempts, no backoff, on `ErrTransient` / missing session; the 15 s HTTP timeout × the 30 s per-realm ctx = at most 2 attempts against a hanging realm, 5 instant ones against a refusing one. Cost ≤ 30 s × realms per tenant: 200 due tenants × 2 dead realms = 3.3 h of the 6 h run, never the 2 h tenant deadline (RP54-4). A 401 (`ClientInvalidAuth`, a rotated key, OR `nbf` refused by a realm clock > 5 s behind — the real realm's `WithLeeway(5s)`) is `ErrInvalidAuth`, not retried → deferred every run; visible in `job_runs.last_error` ("invalid auth: realm <id>"), the `tenant deferred` Error line and `AccountDeletionDeferred`; no per-realm metric (RP54-3) | holds; notes |
| 4 | **Subject and secrets.** | Grepped every error string and log on the path: `client` wraps realm IDs and the address URL (config), never `sub` or the token; `post`'s token error carries `token()`'s "sign the token for realm %s"; `defer_` logs `zap.Error(err)` = those strings; the realm logs refused tokens as the jwt error (never the token). `TestAccountDeletionRunner_ErasesTheUsersRecordAtEveryRealm` asserts the audit row; #946's trail row carries the user id (the operator's trail), never the subject | holds |
| 5 | **The test realm vs `cmd/recovery-realm`.** | Diffed `httpTestRealm.serve` against `server.go`'s `handleReq`/`handshake`: same path for a Delete riding the handshake (`NoiseServerStart` → `NeedsForwardSecrecy` refused → `Handle` → `Finish` → `Handshake` reply + `SessionLifetime`); the real one also requires `nbf`/`exp`, one `aud`, `iss` = the kid's tenant, 5 s leeway, and `putSession` after the Delete (RP54-2). **Lab against the real binary** (below): `eraseUser` → `delete ok`, a Recover answers `ErrNotRegistered`, the Bolt file loads `NotRegistered`, no secret material | holds |
| 6 | **RP51-3.** False warnings; silence; "never fails" | `build-client.sh` pins `GOTOOLCHAIN` from go.mod, `CGO_ENABLED=0 -trimpath -buildvcs=false -ldflags=-s`: the checkout build with the runner's caches equals the row's fresh-cache build (main's tip Deploy run 40104392996: both checkout builds `listed`, sdk-wasm 6 s, sdk-js 0 s). A dirty tree or another toolchain gives another hash → a TRUE warning (that checkout does build unlisted bytes). "Never fails": a build failure is caught (`if ! bash …`); `jq -e` false is a warning; the one hole is `got=$(shasum …)` under `set -e` when `build-client.sh` exits 0 without writing its output — impossible with the two recipes (RP54-7) | holds |
| 7 | **RP51-2.** Full config or typecheck only? | Probe `os.Open("probe")` unchecked in `sdk/wasm/zz_probe.go`: `make lint-wasm` → `2 issues: errcheck: 1, gofmt: 1`, exit 2 — errcheck is `.golangci.yml`'s, not the compiler's | holds |

### Lab — the merged `eraseUser` against the REAL `cmd/recovery-realm` (BoltStore)

One realm `dcb0866d…` on `:18880` (`RECOVERY_REALM_DB` a Bolt file, tenant key `stored:1`), `loadRecoveryVault` with that realm, a temporary env-gated test in `internal/api` (never committed) driving the production code:

```
LAB user 6fe88b83-… sub yNo7_aOiZCEvNDqwxoOcRWR0fkNs0xPG6toOpX-N_p0
LAB registered + recovered at the real realm            # client.Register/Recover through HTTPTransport on recoveryRealm.token
LAB eraseUser #1: deleted=1 deferred=[]
LAB recover after erase: recovery: no secret registered  # client.ErrNotRegistered
LAB eraseUser #2 (NotRegistered = OK): deleted=1 deferred=[]
realm metrics: requests_total{op="delete",outcome="ok"} 2, register1/2 1, recover1/2/3 1, records_destroyed_total 0
realm log: 1 line, 0 "level":"error"
# realm stopped, the Bolt file opened with realm.OpenBoltStore:
LAB bolt load: state=NotRegistered revision=0 holds_secret_material=false
```

## Part 2 — Session B's code (43.4)

| # | Question | Evidence | Verdict |
|---|---|---|---|
| 1 | **Rule 8**, row by row | `produce.json` rows are held by `produce_test.go` (sections = `exportSections`, 9 retention rows = `defaultRetentionPolicies()` incl. `payment_events`' `Extra` and `webhook_outbox`'s `state <> 'pending'`, `exports_days` = `exportTTL`, backups = `pg-backup.sh`'s `-mtime +7` / `OFFBOX_KEEP_DAYS=30` — the Blomp leg prunes by the SAME `CUTOFF`, every table/column exists, the SSE-C row by a real PUT). The typed prose of the two templates: "the key itself is in none of our records" (true: `ssec_key_md5` only), "sections of the data export" (true), "`cdn_access_log` … no IP address" (035's schema has none), "`quota_usage_events` no longer written" (no `INSERT INTO quota_usage_events` outside tests). **Erasure at the realms:** the pages say nothing about the recovery vault (dark, by design in WP-P38 § Still open) and nothing about what an erasure does at a realm — right while `RECOVERY_*` is unset; when the vault is configured, the produce page owes a sentence (owed with the `recovery_vault` flip, not before). One soft spot: "up to 30 days" in a backup rests on the Blomp leg's retention, whose failed delete is logged, never alerted (RP54-6) | holds |
| 2 | **Mode names** only from `modes.json` | the templates render `{{.Name}}` from `modes.Get`; no typed Private/Standard/Public anywhere in them; `TestModeNamesOnlyFromModesJSON` green | holds |
| 3 | **Dark** | `TestLegalLEPages_FlagOff404OnEveryHost`; prod now: `stored.ge` and `stored.cloud` `/legal/law-enforcement` and `/legal/what-we-can-produce` → 404 `text/plain` `cc=no-store`. No link in the landing (its hrefs: `/register`, `/status`, `/login`, `/docs…`, the legal pages, `/abuse`), in `base.html`'s default footer, in `llms_txt.go` or `docs_pages.go` (grep of `internal/` for the two paths: only `trust_host.go`, `flags_wiring.go`, `produce.go` comments and the two templates, which link each other) | holds |

## Part 3 — prod (read-only; nothing restarted)

- `/version` `2546cbee42ed` (build 2026-10-24T14:36:06Z) = main's tip. Active slot 8000 since 14:55:28 UTC: **2,859** journal lines, **0** `"level":"error"`.
- `feature_flags`: no row for `recovery_vault`, `translog_public` or `legal_le_pages` (8 rows in the table, none of them). `RECOVERY_*`: 0 lines in `.env`.
- `account_deletion`: `ok`, last success 04:34:30 UTC today (before #939), `result {"settling": null}`; **0** users `pending_deletion`, so the next run (04:30 UTC 2026-10-25) erases nobody; no run was triggered by this review. The first prod erasure under this build will carry `realms_not_configured: true`.
- Deploy of main's tip (run 40104392996), step "Reproducible client builds": the four rows OK, then `this checkout's sdk-wasm bc06f7a6… is listed (6 s)`, `sdk-js beea90d7… is listed (0 s)`; no `::warning::`.

## Fixes (this review)

| PR | What | RED on main | Mutation → caught by |
|---|---|---|---|
| #944 `cf41f162` | Step 0 docs | — | — |
| #945 `70ad5946` | RP53-7: `TestStream_BudgetIsReturnedAfterClose` waits for part 1's fetch (admit charges the budget before `runPart` calls fetch; the room is held until the next `Next`/`Close`) | the old test under a 50 ms delay before `admit(1)`: `"0" is not positive`; the new one passes 5/5 under it; 300/300 under `-race` without | — (a test) |
| #946 `07d29124` | RP54-1: the mint and the erasure agree | `TestRecoveryTokens_RefusedOnceTheErasureIsDue`: `expected: 409 actual: 200`; `TestRecoveryTokens_NoTrailNoTokens`: `expected: 503 actual: 200` (and 0 rows) | the due check dropped → the first red; the trail error alone ignored → still 503 (the aborted tx fails its commit); trail AND commit ignored → the second red. `make lint` 0, gosec@latest 0/507 files |

## RP54 — findings and residuals

| # | Sev | Finding | State |
|---|---|---|---|
| RP54-1 | P3 (when `recovery_vault` is on; dark today) | The mint ignored the erasure: a due user could mint after b3's recent-token check (RP53-3) and the trail row was best effort after the tokens left (RP53-4) | **fixed #946** |
| RP54-2 | P4 | `cmd/recovery-realm`: a Delete riding the handshake is APPLIED before `putSession`; a full session table (`maxSessions`) then answers `Unavailable` for a record already gone → the runner retries and defers although the realm holds nothing (benign: idempotent, next run) | residual |
| RP54-3 | P4 | A realm that refuses the token (rotated key; or `nbf` refused because its clock is > 5 s behind Vaultaire's) defers every erasure, every run; seen only in `job_runs.last_error`, the Error line and `AccountDeletionDeferred` — no per-realm metric or rule | residual |
| RP54-4 | P4 | A hanging realm costs ≤ 30 s per realm per tenant per run (2 attempts); 200 tenants × 2 realms = 3.3 h of the 6 h run; the stage could stop asking a realm after its first no-answer in a run | residual |
| RP54-5 | note | A settle recorded by a build before #939 completes under this one without a realm Delete; nothing owed on prod | note |
| RP54-6 | P4 | `pg-backup.sh`'s Blomp leg: an old backup it cannot delete is logged, "never fatal" — the produce page's "up to 30 days in a backup" rests on it; the Sync leg fails the run (alerted). Count undeleted files in the textfile metric + a rule | residual |
| RP54-7 | note | `repro-check-clients.sh` "never fails" except `got=$(shasum …)` under `set -e` when a recipe exits 0 with no output file (impossible with the two recipes) | note |
| RP54-8 | note | A cancel landing after b3 keeps the account but the realm record is gone (the client registers again) — stated in the runner's header and the cancel log (#946); behaviour kept, like the objects | done (wording) |

### Verdicts on the sessions' residuals

| # | Was | Verdict |
|---|---|---|
| RP53-1 | P4 | keep — a replay tool for records minted while the vault was set and since unset |
| RP53-2 | P3 | keep — a realm removed from the file while it holds records |
| RP53-3 | P4 | **closed** (#946): no token once the erasure is due |
| RP53-4 | P4 | **closed** (#946): no token without its row |
| RP53-5 | P4 | keep — the unlisted-client warning has no metric |
| RP53-6 | note | keep |
| RP53-7 | P4 (CI) | **closed** (#945) |
| RP53-10 | — | fixed in #938 (verified: Private's `server_holds` names content type, metadata, the key's MD5, the plaintext's MD5) |
| RP53-11 | P3 | keep — no preservation mechanism (F-36) |
| RP53-12 | P3 owner | keep — the SSE-C ETag is the plaintext's MD5 |
| RP53-13 | P4 | keep |
| RP53-14 | P4 | keep |

## Prompt 39 (written by this review: `PROMPT-39.md`)

Part 0 = RP54-2…RP54-7 that are worth a PR (RP54-6 metric + rule, RP54-4 short-circuit, RP54-2 realm order, RP54-7 guard); Part A/B = the launch-facing web host (at T-7 `stored.cloud/pricing`, `/signup`, `/legal/refunds`, `/legal/dmca` answer the S3 API's 403 XML to a browser) and the tenant-zero proof of every dark flag — see the prompt for the pick and its alternatives.
