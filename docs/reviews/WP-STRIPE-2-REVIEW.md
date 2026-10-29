# RP80 — the post-merge review of WP-STRIPE-2 (test mode, `livemode` + `stored listen`, the local image, RP78's fixes)

*Reviewed 2026-10-29 (session RP80). Read adversarially:*
- *the PRs: #1047 (Part 0: RP78-1…10), #1048 + #1052 (the Worker 0.6.0 and its rows), #1049 (PR A: the sandbox, migration 116), #1050 (PR B: `livemode`, `GET /api/v1/events?after=`, `stored listen`), #1051 (PR C: the image) and #1053 (the closeout `docs/reviews/WP-STRIPE-2.md`);*
- *against the note's decisions D-STRIPE-7…17, residuals RB8-1…7, its two open questions, the workerd lab and the prod proofs.*

*Prod state read 2026-10-29 ~13:40 UTC (build `c8c4b1f1`):*
- *one sandbox (`sandbox-691e66a0c436a457`, tenant zero's) with 0 live keys and 0 objects;*
- *`sandbox_purge` ran `ok` at 10:40;*
- *`request_log`: 79 `origin` rows, **every origin row since Part 0 carries a request id** (33 / 33; 28 of 44 were empty before), 0 `api` rows (no management call has been made since #1049: the API needs the owner's TOTP);*
- *`vaultaire_blob_copies_total`: `settled` 3, `cut` 0, `unbudgeted` 0;*
- *events of the last day: 59, all `livemode = true`;*
- *`DATA_PATH=/opt/vaultaire/data` (the NVMe, 3.4 TB free; not the `/tmp` tmpfs), so sandbox bytes do not sit in RAM;*
- *the GHCR package `fairforge/vaultaire` cannot be read without `read:packages`: it is private.*

## Verdict

**HOLDS at P2.** Part 0 did what RP78 asked, and better in two places:
- the settle's four conditions (D-STRIPE-9) are the right test for "this leg's bytes belong to a live object";
- the request id is fixed at the writer, so no handler can drift from it.

The 0.6.0 deploy followed the 0.5.1 lesson (the workerd lab with a 411-refusing sink first), and prod shows the settle working (3 settled, 0 cut).

The sandbox is the right shape: a tenant, not a mode flag, one branch at the top of placement, the edge passing it through, the jobs excluding it, erased with its parent. The image reuses the release binary and proves it by hash.

One P2 remains, and it opens with signups:
- **RP80-1:** a sandbox's downloads are never metered and never paced. A free account's test key can share presigned links to 5 GiB of `local` data with no egress limit at all, served from SLC.

Then three P3s (a suspended parent's sandbox keeps working; `stored listen` can skip an event; nothing bounds a tenant's long polls) and four P4s.

## What was checked and holds

| Area | Checked | Holds |
|---|---|---|
| RP78-1 | `Host.Budget` / `isolateBudget()` acquired for 16 + 2 MiB within 200 ms, else unmirrored and counted; released when the mirror ends | Yes. D-STRIPE-8's note is correct: the code had no "short wait" to reuse. |
| RP78-2 | `putTwo` resolves at the primary; the commit marks `mirror_settle`; `settleMirror` runs under `ctx.waitUntil` after the commit loop (a failed commit still settles, as `retired`); `POST /api/v1/edge/mirror` locks the admit row, then the key, and flips only under D-STRIPE-9's four conditions; anything else is `orphan_write` on the leg | Yes. Lock order is the commit's. A settle can never make a copy ready for a blob no row names. |
| RP78-3 | One id per request at the writer; error bodies, long ops and the request log read it | Yes. Prod: 33 / 33 origin rows have their id. |
| RP78-4, 5, 8, 10 | `RegionJurisdiction` (London = `uk`); `(multiple − 1) × stored`; `engine.StrictTargets()`; the `withdrawn` list and `predeploy` | Yes. Withdrawing only `edge-codec` 0.5.1 (D-STRIPE-12) is right: `edge-ingest`'s bytes are live. |
| The sandbox: auth | `GenerateTestKey` in one transaction (the user `FOR SHARE`, the sandbox `ON CONFLICT (sandbox_of)`, the cap of 10, the audit row); no sandbox of a sandbox; the key resolves to the sandbox and never the parent | Yes. |
| The sandbox: placement | `placeOn` and `decidePlacement` first branches (whole, unsealed, `local`); the edge prelude's `sandbox` pass-through; `notSandbox*` in the vault parity and mirror backfill queries | Yes. `TestTestKey_NeverReachesAPaidBackend` covers PUT, multipart, copy and the edge admit. |
| The sandbox: lifecycle | `sandbox_purge` (10:30, 7 days), the reset route, erasure stage b4 then `EraseRows` sandbox-first, the overview card | Yes. |
| `livemode` | A sandbox's event recorded under the parent with `livemode = false`; every delivery attempt rebuilds the body from the row | Yes. |
| The image | Distroless by digest, nonroot, the release binary checked by sha256 against the `server_build` artefact; compose binds 127.0.0.1, Postgres by digest | Yes. |

## Findings

| ID | Sev | Where | What | Fix |
|---|---|---|---|---|
| **RP80-1** | **P2** | `internal/api/s3.go` (`metered := … && !sandbox`, the bandwidth record `&& !sandbox`) | **A sandbox's downloads have no limit.** "Never metered" was read as "never counted", so a sandbox GET gets no egress span: the allowance, the throttle and the would-throttle counter never see it, and `bandwidth_usage_daily` has no row, so it is invisible to the admin too. A presigned GET resolves through the same dispatcher. Any account, the free tier included, can mint a `VLT_TEST_` key, upload its sandbox's 5 GiB to `local`, and hand out presigned links (up to 7 days) served from SLC with no egress ceiling and no pacing. That is free file hosting on the box's 660 TB a month. Latent today (signups closed; tenant zero's sandbox has no key); live the day signups open. | Test first: `TestSandbox_DownloadsArePacedNeverBilled` (a sandbox past its allowance is paced at the floor; no `payments` or parent counter changes; the bytes appear in `bandwidth_usage_daily` under the SANDBOX id). Then **meter without billing**: a sandbox gets an egress span on its own row with a fixed small allowance (e.g. 1× its 5 GiB a month, a const, not a price), paced past it like any tenant; `recordResponse` records it under the sandbox id; the parent's counters stay untouched. Also: a `VLT_TEST_` presigned URL lives at most 1 hour (`TestTestKey_PresignCappedAtOneHour`); a sandbox bucket cannot be made Public (`modes.SetBucket` refuses, 409 `sandbox_never_public`). |
| **RP80-2** | P3 | `internal/api/s3.go` `isTenantSuspended`; `auth.GenerateTestKey` | **A suspended parent's sandbox keeps working.** The suspension check reads the request's tenant, the sandbox, whose `suspended_at` is never set. An account suspended for abuse keeps its test keys and their 5 GiB, and the mint never looks at the parent's suspension. | Test first: `TestSandbox_ParentSuspendedRefusesTestKeys` (the parent suspended: a test-key PUT is 403 `AccountSuspended`, a mint is 403). Then `isTenantSuspended` reads `suspended_at` of the tenant OR its `sandbox_of` parent in one query, and the mint checks the parent. |
| **RP80-3** | P3 | `internal/api/events.go` `listEventsAfter` / `eventsAfter` | **`stored listen` can skip an event for ever.** The forward cursor is the keyset `(created_at, id)`, with `created_at = NOW()` at the INSERT and `id` a random uuid. Two concurrent `emitEvent`s can commit out of their `created_at` order. A poll that reads the later-stamped one first moves its cursor past the earlier one, which then commits behind it and is never returned. Webhook deliveries are unaffected (the outbox); `stored listen` is not. | Test first: `TestEvents_AfterNeverSkipsALateCommit` (insert B, read with `after=` A's predecessor, then commit A with an earlier `created_at`: A is still delivered). Then answer only events older than a settle horizon (`created_at <= now() - 2 s`; the long poll keeps waiting), and have `stored listen` keep the last 1,000 ids it forwarded so a re-read never double-delivers. |
| **RP80-4** | P3 | `listEventsAfter` | **Nothing bounds a tenant's long polls.** Each waiting request runs a query every 500 ms for up to 25 s. The routes' rate limit counts requests, not concurrent waits, so one JWT holding 50 waits is 100 queries a second. | Test first: `TestEvents_LongPollsBoundedPerTenant` (the 5th concurrent `wait` of one tenant is 429 `too_many_listeners` + `Retry-After`). Then a per-tenant semaphore of 4 waits. A `LISTEN/NOTIFY` wake-up instead of the 500 ms poll is a later improvement, not this fix. |
| RP80-5 | P4 | `internal/api/sandbox.go` `sandboxParent` | A lookup error answers "live tenant" and is not cached, so a sandbox write during a database fault would take the live placement, possibly a paid backend. The write needs the database anyway, so this is near-impossible. It still contradicts "never a paid backend". | Fail closed: `placeOn` answers 503 when the sandbox lookup errors. |
| RP80-6 | P4 | `edge_mirror_settle.go` | The settle accepts any edge caller for any admit id. Once nodes hold their own tokens (WP-EDGE-3), one host could settle another's admit (ids are random, so the risk is low). | Refuse when the caller's host is not `row.Host` (404, as an unknown admit). |
| RP80-7 | P4 | `internal/api/bootstrap.go` | The first-boot account's password is random and never shown, yet the message says "rotate them in the dashboard": nobody can sign in to do it. Also the bootstrap runs on any `STORAGE_MODE`; a prod `.env` that ever set the e-mail would print a secret to journald. | Print a one-time password with the keys (the local server only), and refuse the bootstrap unless `STORAGE_MODE=local`. |
| RP80-8 | P4 | `docs/reviews/WP-STRIPE-PROMPTS.md` § The chain | Prompt 51 is listed twice. | Fixed in this PR. |

## The residuals and open questions, answered

- **Open question 1 (D-STRIPE-13): keep the sandbox's own 5 GiB counter as its disk cap. Yes.** It is a quota, not a bill, and RP80-1's allowance is derived from the same row.
- **Open question 2 (RB8-2): yes, show test requests on the parent's page.** `GET /api/v1/manage/requests?mode=test` reads the sandbox's rows, and the Developers page gets a Live / Test toggle. This is small and belongs in Prompt 51's Part 0, because WP-PLAT-1 adds a third kind of row (a child's) to the same page.
- **RB8-1 (a node's shutdown drops a settle):** accepted, as the job is the net. Add `WaitMirrors()` to the node's stop when WP-EDGE-3's node next changes.
- **RB8-3 (admin views count sandboxes):** fold into PLAT-1, which must tell three kinds of tenant apart in every admin total anyway (live, sandbox, child).
- **RB8-4, 5, 7:** accepted.
- **RB8-6 (`stored listen` forwards the list item):** accepted for now. The webhook body adds only `tenant_id`, and a local handler should not depend on it.
- **The GHCR package is private.** Make it public at launch, when the guide is linked. Until then `gh auth refresh -s read:packages` pulls it. Owner's call.
- **Not proved on prod:** `stored listen` against the live log, the reset route and a management request-log row. All three need a management JWT, which needs the owner's TOTP. Their tests cover them; the next session with the owner present proves them.

## For Prompt 51, Part 0 — in this order

1. **RP80-1:** sandbox downloads metered and paced against a small own allowance, never billed; test-key presigned URLs at most 1 hour; no Public sandbox bucket.
2. **RP80-2:** the parent's suspension covers its sandbox.
3. **RP80-3 + RP80-4:** the events cursor's settle horizon, `stored listen`'s dedup, and at most 4 waits per tenant.
4. **RB8-2:** `?mode=test` on the request log and the Developers page toggle.
5. **RP80-5…7:** the P4s.
