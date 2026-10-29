# RP78 — the post-merge review of WP-STRIPE-1 (the pricing page, the request log, RP76's fixes)

*Reviewed 2026-10-29 (session RP78): #1038 (Part 0: RP76-1…9, D-EDGE-59…62),
#1039 (PR A: WP-PRICE-1, `/pricing`), #1041 (PR B: WP-LOG-1, migration 115),
#1040 / #1042 / #1043 (the Worker 0.5.1's rows, its regression fix 0.5.2 and
0.5.2's row), #1044 (the deflake) and #1045 (the closeout
`docs/reviews/WP-STRIPE-1.md`), read adversarially from the note's
§ Decisions, § The 0.5.1 regression, § Residuals RB7-1…14 and its two open
questions, against the merged diffs, the Go and node tests, the workerd lab
and the prod proofs. Prod state read the same night (2026-10-29 ~03:20 UTC,
build `9c654d4f`):*

- *`request_log`: 371 rows from 2 tenants since 00:17 UTC (327 `edge`, 44
  `origin`), 264 kB; every row has its `key_id`; **28 of the 44 origin rows
  have no `request_id`**; 2 rows in `request_suggestions`; no
  `request_log` flag row (default ON).*
- *`edge_mirror` ON for tenant zero only; `blob_copies`: 14,251 `ready` on
  `idrive` from `lyve` (94 GB) and 6 `ready` on `lyve` from `idrive`
  (1.5 GB); 0 pending; `vaultaire_blob_copies_total{outcome="cut"} 0`.*
- *The copy pass's ledger: `egress {month 2026-10, spent {idrive: 1 GiB}}`;
  `backend_plans`: `idrive` multiple 3 (blomp, geyser NULL); the last
  `backend_capacity` run (10-28 09:18) predates the mirror copies.*
- *`chunk_orphans` 01:57 UTC walked `lyve` for the first time: 784 keys
  listed, 784 named, 0 erased (and `idrive` 761/761).*
- *`edge_admits` since Part 0: 448 parts and 7 PUTs committed, 3 PUTs failed
  (0.5.1's 411s). `LYVE_REGION=us-east-1`, `IDRIVE_REGION=us-central-1`,
  `MIRROR_LEGS` unset.*

## Verdict

**HOLDS at P2.** Every RP76 finding landed with a red test, and the two P2s
are fixed in substance: a stalled mirror can no longer hold a write (the
workerd lab shows 0.74 s against 0.5.0's 34.75 s), and the leg now has to
satisfy the bucket. The request log is the right shape: one writer (the
tracker's second sink, one `unnest` INSERT a flush), one reader
(`internal/requestlog`), recorded once from the moment a tenant is known so
a refusal after authentication is in it, no IP, user agent, header, body or
secret, and a suggestion table that cannot leak one tenant's names to
another (D-STRIPE-1 is a better call than the prompt's). `/pricing` is
generated from `prices.json` and pinned to it. The 0.5.1 regression was
caught by the first prod PUT, rolled back in two minutes, reproduced in
workerd and fixed with the right primitive.

Two P2s remain, both in the mirror, both live for tenant zero:

- **RP78-1:** the mirror's new 18 MiB buffer lives outside the isolate's one
  byte budget (RP72-6), and the Go host's 16 MiB queue outside the host's.
- **RP78-2:** the note's open question 1 is decided as option (b). A healthy
  slower leg costs the only write the mirror touches 2.3×, and the client
  should not wait for a copy the system already knows how to owe.

Four P3s follow: success rows with no request id, London counted as the EU,
a copy pass allowed to spend the customers' free egress, and management
requests missing from the log. Four P4s close the list.

## What was checked and holds

| Area | Checked | Holds |
|---|---|---|
| RP76-2, the Worker | `putTwo`: the tee branch buffered 16 + 2 MiB, the lag checked on every counted read of the primary, the 5 s after-primary timer, the outer deadline kept; node tests with Node's tee and a workerd-paced tee; 0.5.2's `FixedLengthStream` on both bodies | Yes. The cut works through `ac.abort` (the fetch's body is cancelled, and that propagates through `pipeThrough` to the tee branch). The lab measured it in workerd itself. |
| RP76-2, the Go host | `mirrorTee`: a queue of copied 1 MiB buffers, ≤ 16 MiB, fed by its own goroutine, so `Write` never waits for the mirror; cut = cancel + pipe closed; `fail()` is not a cut | Yes (RP78-7 is a one-line race). |
| `mirror_cut` | The commit field for PUTs and codec chunks, counted once at the control plane; the copy row stays `pending` | Yes. Pinned by `TestEdgeMirror_ACutMirrorIsCountedAndTheCopyOwed`. |
| RP76-1 / 6 | `bucketScope`, `legRegion`, `allows`; strict tier, strict placement and `rs` never mirrored; the backfill joins `buckets` and excludes strict tiers; `unmirrorable` makes a pending copy of an unreadable source stale; the stale pass never revives it | Yes, under D-EDGE-59's rule. The rule itself is RP78-4. |
| RP76-3 | Stage b1b deletes each codec chunk copy through its row, inside the tenant branch before the sweep; a leg that is down defers; `chunk_orphans` walks every registered leg, and a `blob_copies` row names a key | Yes. Prod's first walk of `lyve` named all 784 keys and erased nothing. |
| RP76-4 | The copy pass loops on a keyset cursor while rows are pending and time remains; the per-source month ledger lives in `job_runs.result` (D-EDGE-60); a failed copy gives its reservation back | Yes. The budget's size is RP78-5. |
| RP76-5 / 7 / 8 | RS shards HEADed (size and ETag) at commit; `Verified` set after the mirror branch; the mirror read link asked only for a mirrored tenant | Yes. |
| WP-PRICE-1 | `pricing.src.html` + `pricing.js` → `pricing.html` under the shared sources stamp; every number from `prices.json` (the competitors' egress moved in); `/pricing/` → 301; S3 hosts keep the S3 API | Yes. |
| WP-LOG-1, write | The deferred `recordS3Access` from the moment `tenantID` is known; `noteS3Error` walks the `Unwrap` chain; the edge rows carry `key_id` from the admit's attrs and the data-plane host's name; flag default ON, OFF writes nothing | Yes, except the success rows' request id (RP78-3). |
| WP-LOG-1, read | Tenant-scoped in every query; keyset `(logged_at, id) <` with a microsecond-exact cursor; status filter; `request_id` search on its own index | Yes. |
| WP-LOG-1, privacy | Schema pinned to 13 columns; erased with the account; excluded from the export and the sweep sources with reasons; 7-day retention; privacy policy, DPA and produce page rows | Yes. RB7-2's notice is answered below. |
| The 0.5.1 regression | Cause, rollback, the workerd lab with a 411 sink, the 0.5.2 manifest row at `f134825a` | Yes. The lesson (a Worker body keeps its length in workerd, tested against a refusing sink) is the right rule. RP78-10 makes it impossible to redeploy 0.5.1's bytes. |

## Findings

| ID | Sev | Where | What | Fix |
|---|---|---|---|---|
| **RP78-1** | **P2** | `edge/s3-ingest/src/codec.js` `putTwo`; `internal/dataplane/mirror.go` `mirrorTee` | **The mirror's buffer is outside the byte budgets.** RP72-6 made `ISOLATE_BYTES` (96 MiB) "the ONE byte budget of an isolate", because an isolate is 128 MB and Workers run many requests in one isolate. 0.5.1 added an 18 MiB `ByteLengthQueuingStrategy` buffer per mirrored whole PUT and took none of it from that budget. Five concurrent mirrored PUTs against a slow leg hold 90 MiB. Beside one codec write's 48 MiB, the isolate is past its limit and is killed, failing every request in it, mirrored or not. The Go host is the same: up to 16 MiB of copies per mirrored write, outside `Host.Budget` (RP74-6). Live for tenant zero (the only mirrored tenant, few concurrent whole PUTs). It is a default cost for any tenant once `edge_mirror` widens, and RP78-2 lengthens how long the buffer lives. | Test first: with `setIsolateBudget(20 MiB)`, two concurrent mirrored 64 MiB PUTs; the second is sent UNMIRRORED (one link, the copy owed, `vaultaire_blob_copies_total{outcome="unbudgeted"}` after the commit says so). Then `putTwo` takes `MIRROR_MAX_LAG + 2 MiB` from `isolateBudget()` with a short wait (the windowed reader's rule), releases it when the mirror ends, and sends the PUT unmirrored when it cannot get it. Never a refusal: the mirror is an extra, so no 503. Go: the same against `Host.Budget` (a try-acquire), `TestPutTwo_NoBudgetNoMirror`. |
| **RP78-2** | **P2** | `putTwo` (both); the commit | **Open question 1 decided: option (b).** The edge mirror applies only to a whole PutObject of 4–64 MiB. On that path, prod measured 2.5 s → 5.8 s with a healthy Lyve leg, because the client waits for the slower leg's last 48 MiB and its answer. Option (a), a 4 MiB lag bound, still pays most of that and writes more copies through the 2× lane. Option (b) makes the client's time the primary's. The copy row is already `pending` until a mirror ETag arrives, and the job already owes it. **The write's durability is unchanged: the primary held it either way.** | Test first: a mirror at 2 MB/s; the client's answer arrives at the primary's pace, and the copy becomes `ready` afterwards; a DELETE between the commit and the settle leaves no copy on the leg. Then: (1) The commit is sent when the primary answers. Its `blob_copies` row is `pending`, and its result carries `mirror_settle: true`. (2) The mirror finishes in the background: the Worker under `ctx.waitUntil`, cut at the Workers limit (30 s after the response); the Go host bounded by `mirrorDeadline`. Its buffer stays charged to RP78-1's budget until it ends. (3) New `POST /api/v1/edge/mirror` (token gate, Cloudflare peer, never captured by the S3 gate: add it to `TestEdgeRoutes_NeverCapturedByTheS3Gate`) takes `{admit_id, etag \| cut}`. It flips the row `pending → ready` only while the row exists, is `pending` and its admit is this one's. Otherwise the leg's address is queued in `retired_blobs` on the leg (`orphan_write`), because nothing else would collect a whole object on a leg for a living tenant. Idempotent like the commit. (4) A settle that never arrives is the job's copy (as today). Worker 0.6.0, proved in the workerd lab first (the RB-9 / 0.5.1 lesson: a 411-refusing sink, and a mirror slower than 30 s). |
| **RP78-3** | P3 | `internal/api/s3.go` `recordS3Access`; the S3 handlers | **Success rows have no request id.** On prod, 28 of 44 origin rows have `request_id = ''`: DeleteObject 204, Create/CompleteMultipartUpload 200 and others. Many handlers never set `x-amz-request-id`, and the third fallback reads the REQUEST's `X-Request-Id`, which only a client or proxy would send. The response's own `X-Request-Id`, set by the request-id middleware, is never read. AWS answers `x-amz-request-id` on every response, and that id is what a support ticket quotes. D-STRIPE-3 holds for errors only. | Test first: `TestS3_EveryResponseCarriesTheLoggedRequestID`, the dispatcher matrix (every op, success and error): the response's `x-amz-request-id` equals its `request_log` row's. Then the dispatcher sets ONE id for the request up front (the middleware's) as `x-amz-request-id`. The handlers' own `generateRequestID()` calls and the error bodies' `RequestId` use it, and `recordS3Access` reads that. |
| **RP78-4** | P3 | `internal/api/edge_mirror.go` `jurisdiction`; `internal/drivers/idrive_regions.go`; `bucket_settings.html` | **London is counted as the EU.** D-EDGE-59 takes a region's jurisdiction from its first segment. iDrive's `eu-west-3` is London (UK, `IDriveRegions`' own label), so a Paris bucket's copy could go to a London leg. The new dashboard sentence says "never copied outside the EU". The pre-existing `IsEURegion` labels London EU too, which is also the bucket page's residency label. Latent: no `eu-*` leg is in `MIRROR_LEGS` and no prod bucket is pinned. | Test first: `TestMirrorLegFor_LondonIsNotTheEU`. Then give the region table an explicit jurisdiction per region (`eu-west-3` → `uk`, the rest by their country's), and use it in both `jurisdiction` and `IsEURegion`. A UK bucket gets a "UK" residency label, and the sentence names the jurisdiction it means. |
| **RP78-5** | P3 | `internal/api/blob_copies_job.go` `egressLedger` | **The copy pass may spend all of a source's free egress.** The budget is `multiple × stored − max(vendor MTD, own spend)`, so the backfill can use the whole allowance. Customers' GETs on that backend in the same month then go to the vendor's overage rate, and the 2× lane is never worth that. Live but small on prod (`idrive` is a source for 6 rows; the 94 GB went the other way, from `lyve`, which has no plan row). | Test first: `TestBlobCopiesJob_LeavesTheCustomersTheirEgress`: the pass stops when the source's remaining free egress reaches 1× stored. Then amend D-EDGE-60: the pass may spend only the remaining allowance above one multiple of stored. |
| **RP78-6** | P3 | `internal/api/management_*.go` | **RB7-1, re-graded.** The plan row the closeout marks DONE says "every S3 **and management** request". Prompt 50 adds management routes (sandbox reset, test keys), and a developer debugging test mode needs to see them. | Fold into Prompt 50: a management middleware writes the same row (`op` = the route pattern, `host` = `api`, `key_id` = the JWT's key or `session`). `TestRequestLog_ManagementRequestsToo`. The plan row stays "S3 only" until then. |
| RP78-7 | P4 | `internal/dataplane/mirror.go` `putTwo` | A mirror that answers at the same instant `mirrorAfter` fires is not cut (`fail()` already set `dead`), but `<-mc` discards its ETag, so the copy is redone by the job. | Moot once RP78-2 lands (the after-primary timer goes). Otherwise `mirrorETag = <-mc`. |
| RP78-8 | P4 | `blob_copies_job.go` backfill SQL | The strict list `'sync','blomp','sia'` is typed in SQL beside `engine.IsStrictTarget`. A new strict target is enqueued, then made stale: self-correcting, at the cost of rows. | Pass `engine.StrictTargets()` as an array parameter. |
| RP78-9 | P4 | `codec.js` `putTwo` | `mirrorBody.cancel(why)` is called on a stream `FixedLengthStream`'s pipe has locked: it rejects (caught) and does nothing. The cut works through `ac.abort`. | Drop the call or cancel the piped readable, with a comment saying which path cuts. |
| RP78-10 | P4 | `internal/translog/clientbuilds/manifest.json`; the deploy check | RB7-12 "never deploy 0.5.1" is a sentence. 0.5.1's bytes have manifest rows, so `repro-check-clients` and `translog-verify -client` would accept them. | A `withdrawn` field on a manifest row (an appended `client_build` entry cannot be removed). The Worker deploy refuses a withdrawn row, and `translog-verify -client` reports it. |

## The residuals and open questions, re-checked

- **RB7-1:** is RP78-6.
- **RB7-2 (the privacy-policy notice):** prod has three accounts, the owner's and two internal `vaultaire.local` ones (read 2026-10-29), signups are closed, and no customer has accepted the earlier text. The 30-day rule protects existing customers, and there are none. Record it as shipped before the first customer; the rule applies from the first customer on. The owner confirms in one line or turns the flag off globally.
- **RB7-3 (RS first stripe):** stays with `edge_rs` dark, on the pre-flip list.
- **RB7-4 (`blob_copies (leg, storage_key)` index):** fine at 14k rows; it goes with WP-VAULT-2's `leg_etag` migration as planned.
- **RB7-5, 6, 7, 9, 11, 14:** accepted as written.
- **RB7-8:** agreed.
- **RB7-10:** the `AccessDenied` suggestion gains the SSE-C clause when RP78-3 touches the catalogue.
- **RB7-12:** is RP78-10.
- **RB7-13 / open question 1:** is RP78-2, option (b).
- **Open question 2:** answered under RB7-2.

## For Prompt 50, Part 0 — in this order

1. **RP78-1 + RP78-2 together**: the budget first, then the background
   settle and `POST /api/v1/edge/mirror`. One Worker version (0.6.0), proved
   in the workerd lab before deploy. The Go host goes in the same PR.
2. **RP78-3**: one request id per S3 request, on every response and in
   every row.
3. **RP78-4**: a jurisdiction per region; London is the UK.
4. **RP78-5**: the copy pass leaves one multiple for the customers.
5. **RP78-6**: management requests in the request log (with PR A's new
   routes, or in Part 0 if PR A is not reached).
6. **RP78-7…10**: the P4s.
