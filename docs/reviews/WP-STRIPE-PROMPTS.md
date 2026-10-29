# WP-STRIPE — the worker prompts of the Stripe layer

*The Stripe layer's missing primitives (plan § WP-STRIPE, added 2026-10-28):
WP-PRICE-1, WP-LOG-1, WP-DX-1, WP-PLAT-1, WP-TEAM-1, WP-KEY-1, WP-POST-1,
WP-XFER-1, WP-VAULT-2. This file carries their prompts the way
`docs/reviews/WP-EDGE-PROMPTS.md` carries the edge chain's: one prompt per
session, two editions (the chain's for Fable with `ultrathink`, the Opus
edition with every decision made and numbered steps), each prompt's Part 0 =
the previous session's post-merge review, each closeout writes the next
prompt here. The numbering continues the Stage 1a sequence: Prompt 49 follows
Prompt 48 (WP-EDGE-4); the post-merge reviews continue RP76 → RP78 → ….*

The Opus rules are `docs/reviews/WP-EDGE-PROMPTS.md` § "About the Opus
editions (read once)"; every Opus edition here assumes them.

## Stage 1a Prompt 49: WP-PRICE-1 + WP-LOG-1 — the pricing page and the developers' request log (Part 0: RP76's findings)

**From 48 (`docs/reviews/WP-EDGE-4.md`, reviewed by RP76 — `docs/reviews/WP-EDGE-4-REVIEW.md`, HOLDS at P2):**
- **The mirror is live for tenant zero** (`edge_mirror`, 14,252 ready copies on prod); the Worker 0.5.0 is deployed. Part 0 changes `putTwo` in `edge/s3-ingest/src/codec.js` and `internal/dataplane/mirror.go`: the Worker's deploy after Part 0 is 0.5.1 (manifest rows, RB-9's capture).
- **Every bucket on prod is `us-central-1`** — RP76-1 is latent, not live; its test is the lab's.
- **`standard_sealed_default`, `edge_rs` and `edge_codec` are OFF**; nothing in this prompt touches them.
- **`prices.json` is the one price source** (`internal/api/landing/prices.json`, `make landing` → `internal/api/landing.html`; `internal/api/landing/prices_test.go`, `internal/api/llms_txt_test.go` pin the pages to it); `/pricing` is a 302 to `/#pricing` (`internal/api/web_host.go:73`).
- **The access log is per bucket and opt-in** (`S3AccessLogTracker`, `s3_access_log`, 30 d, `logging_enabled`); the friendly error suggestions are `WithSuggestion` in `internal/api/s3_errors.go`; the customer dashboard's routes are `internal/dashboard/router.go` (`/apikeys`, `/usage`, `/settings`, `/billing`, `/compliance`).

**Part 0 (RP76, in this order, before PR A):** RP76-2 a mirrored write runs
at the slower leg's pace until a fixed deadline — bound the mirror's lag
(the Worker: cut the mirror 5 s after the primary finished or when it lags
by > 16 MiB; the Go host: a 16 MiB buffer on the mirror pipe, cut past it,
never block the primary's write), `vaultaire_blob_copies_total{outcome="cut"}`,
and MEASURE on prod with `edge_mirror` on and off (256 MiB and 1 GiB `aws s3
cp` into tenant zero's bucket; the numbers in the note); RP76-1 + RP76-6 the
leg must satisfy the bucket — a region-pinned bucket mirrors only to a leg
of the same jurisdiction (none for the EU today: no mirror, never a refusal,
"single copy" said on the bucket page and in `docs/API.md`), a strict-target
tier (`sia`, `blomp`, `sync`) and an `rs` row are never mirrored or
backfilled; RP76-3 the deletion runner deletes a tenant's chunk copies on
their legs through the rows BEFORE the row erase (a stage beside the sweep,
deferring as the sweep does) and `chunk_orphans` walks every mirror leg's
`_global/_chunks/<tenant>/`; RP76-4 the copy pass keeps a per-source-backend
egress budget per UTC month from `backend_plans` and loops while rows are
pending and time remains; RP76-5 (may trail PR A) each RS shard HEADed at
commit; RP76-7…10 one line each.

Continue from WP-EDGE-4 (`docs/reviews/WP-EDGE-4.md`) and its review RP76.
`git log origin/main`, `gh pr list`.

Read first: plan § WP-STRIPE (the WP-PRICE-1 and WP-LOG-1 rows — the
contract); `internal/api/landing/{build.py,landing.src.html,prices.json,
prices_test.go}`; `internal/api/web_host.go` and `web_files.go` (what a
web page is, and the s3-host rule: `/pricing` on `s3.*` stays the S3
API's); `internal/api/llms_txt.go` + its test; `internal/api/s3_errors.go`
(`WithSuggestion`); `internal/api/s3_access_log*.go` and
`internal/api/retention.go`; `internal/dashboard/router.go`, a customer
handler and template (`/usage`); `internal/api/account_export_sections.go`
and `internal/account/erase.go` (the schema-walk tests will name the new
table); `docs/API.md` § Errors.

Lane: Developer surface. Migration 115 `request_log`. PRs: (0) RP76's
fixes; (A) WP-PRICE-1; (B) WP-LOG-1; (C) docs.

What to build
1. WP-PRICE-1 — `/pricing` is a page: every tier and period from
   `prices.json` (a test pins every number on the page to the file), the
   egress rule (the three sentences of `prices.json.egress`), what is never
   charged (API calls, egress inside the allowance, Vault restores), what
   is (the throttle past the allowance today; the rate when billing
   lands), the edge's 100 MB single-PUT note, the free tier, the compare
   table against S3, B2, Wasabi, R2 with the prices dated
   (`competitors._asof`), the house builder's calculator maths on the
   public page; linked from the landing, llms.txt, the FAQ and the
   sitemap; the redirect becomes the page on web hosts, the S3 API on
   `s3.*` unchanged.
2. WP-LOG-1 — the developers' request log: migration 115 `request_log`
   (tenant, key id, ts, op, bucket, object key, status, error code,
   request id, bytes, host; 7 days; tenant data: erased with the account,
   excluded from the export with a reason — "the same requests as the S3
   access log, kept 7 days for the developer's own debugging"); written
   by the ONE access-log path for every tenant (not per bucket, not
   opt-in; no body, never an SSE-C key or any secret — the schema-walk
   pattern), the error's suggestion stored once per error code and joined
   on read; `GET /api/v1/manage/requests` (key, since, status, request id
   filters, keyset paged) and the dashboard's Developers page (last 7 days,
   per key, the error and its suggestion, searchable by request id — the
   thing a support ticket quotes); the retention job prunes at 7 d.
   The writer is behind a flag (`request_log`, default ON after one
   deploy) so a bad write path can be switched off.

Tests (red first): `TestPricingPage_EveryNumberFromPricesJSON`,
`TestPricingPage_ServedOnTheWebHostsOnly` (an `s3.` host answers XML),
`TestLlmsTxt_LinksThePricingPage`, `TestSitemap_ListsPricing`;
`TestRequestLog_EveryErrorCarriesItsSuggestion`,
`TestRequestLog_NeverHoldsAnSSECKeyOrASecret`,
`TestRequestLog_RetainedSevenDays`, `TestRequestLog_SearchByRequestID`,
`TestRequestLog_ErasedWithTheAccount`, `TestRequestLog_KeysetPaged`;
`TestErasedTablesCoverSchema` / `TestExportSectionsCoverSchema` /
`TestSQLLiteralsMatchSchema` naming `request_log`. Prod proof: `/pricing`
live and in the sitemap; a deliberate 403 by tenant zero on the Developers
page within a second with its suggestion.

Architecture notes
- The pricing page is generated by `make landing` from `prices.json` like
  the landing, never typed; the test reads the file and the rendered page.
- The request log is the access-log tracker's second sink, not a second
  tracker: one write path, one place a secret could leak, one test.
- Closeout: `docs/reviews/WP-STRIPE-1.md` (D-STRIPE-n, RB7-n), STATUS,
  plan § WP-STRIPE rows flipped, CLAUDE.md rows (table `request_log`, the
  flag, the retention line), `docs/API.md` § Request log + § Pricing, the
  ledger row; the next prompt (Prompt 50: WP-DX-1) written here. The
  post-merge review is RP78.
- Commit: `feat(api,dashboard,database): WP-STRIPE — … [WP-STRIPE]`; merge
  with `gh pr merge --auto`.

ultrathink

## Stage 1a Prompt 49 — Opus edition

**From 48 (as above).**

You are building the first two rows of plan § WP-STRIPE in the Vaultaire
repository — WP-PRICE-1 (the pricing page) and WP-LOG-1 (the developers'
request log) — after Part 0, the RP76 fixes. Read the Opus rules in
`docs/reviews/WP-EDGE-PROMPTS.md` § "About the Opus editions". Worktree
from `origin/main`. Migration 115.

Read before editing: plan § WP-STRIPE (the two rows); `docs/reviews/
WP-EDGE-4.md` + `WP-EDGE-4-REVIEW.md`; `internal/api/edge_mirror.go`,
`internal/api/blob_copies_job.go`, `internal/dataplane/mirror.go`,
`edge/s3-ingest/src/codec.js` (`putTwo`), `internal/account/erase.go`,
`internal/api/deletion_runner.go`, `internal/api/chunk_orphans.go`,
`internal/api/backend_capacity.go` (`backend_plans`); `internal/api/
landing/{build.py,landing.src.html,prices.json,prices_test.go}`,
`internal/api/web_host.go`, `web_files.go`, `llms_txt.go`;
`internal/api/s3_access_log.go` (the tracker), `s3_errors.go`,
`retention.go`, `account_export_sections.go`; `internal/dashboard/
router.go`, `handlers/usage.go` + `templates/customer/usage.html`.

PR 0 — RP76's fixes (`docs/reviews/WP-EDGE-4-REVIEW.md` § Findings).
Branch `stage1a-p49-part0`.
0a. RP76-2, test first. Node: `test/mirror.test.js` — a mirror that
    answers nothing while the primary finishes a 32 MiB body: `putTwo`
    returns within 5 s of the primary's answer with `mirrorETag ""`; a
    mirror reading at 1/10 the primary's pace on 64 MiB: the primary's
    bytes arrive at its own pace (count arrival times on the fake), the
    mirror is cut once it lags by 16 MiB. Go: `TestPutTwo_TheMirrorNever
    HoldsThePrimaryPastItsBuffer` (the primary receives every byte at its
    pace; the mirror cut at the buffer). Then `codec.js`: wrap each tee
    branch in a `TransformStream` that counts bytes; a watchdog every
    500 ms cuts the mirror (`ac.abort`) when `primaryBytes − mirrorBytes >
    16 MiB` or when the primary resolved and the mirror has not within
    5 s; keep `mirrorDeadlineMs` as the outer bound. `mirror.go`: the
    mirror pipe fed from a 16 MiB ring buffer (a goroutine copying tee →
    buffer → pipe); `mirrorTee.Write` never blocks on the mirror — it
    fails the mirror when the buffer is full. Counter `vaultaire_blob_
    copies_total{outcome="cut"}` (the commit reports `mirror_cut: true`,
    the control plane counts). Verify: `cd edge/s3-ingest && npm test`,
    `go test ./internal/dataplane -run PutTwo -race`. Then the prod
    measurement after PR 0 deploys and the Worker 0.5.1 is live: on the
    box, `aws s3 cp` 256 MiB and 1 GiB into tenant zero's `tz-proof`
    bucket through `s3.stored.cloud` with `edge_mirror` ON, then OFF (the
    flag row), MB/s each, into the closeout note.
0b. RP76-1 + RP76-6, test first: `TestMirrorLegFor_RespectsResidencyAndTier`
    — a bucket with `region = eu-west-1` (placement `idrive-eu-west-1`)
    gets no mirror link and no copy row (the lab has no EU leg); a bucket
    with `tier_preference = blomp` is skipped by the backfill; an `rs`
    head row is never enqueued; a `us-central-1` `resilient` bucket
    mirrors to `idrive`. Then `mirrorLegFor(tenantID, bucket, placement)`
    reads the bucket's region and tier once per admit (the bucket row is
    already read by the prelude: pass it); a leg qualifies only when its
    driver's region is the bucket's jurisdiction (`drivers.IDriveRegions`
    for `idrive-*`; `lyve`'s `LYVE_REGION`; else unknown = never for a
    pinned bucket); the backfill query excludes `rs`, the strict-target
    backends and buckets whose tier is strict. `docs/API.md` § Durability:
    "an EU-pinned bucket is a single copy until an EU leg exists"; the
    bucket page's line.
0c. RP76-3, test first: `TestErasure_DeletesChunkCopiesOnTheirLegsBeforeTheRows`
    (a tenant with two chunk copy rows on the fake leg → after the runner,
    the leg holds neither and the rows are gone; a leg whose breaker is
    open defers the tenant) and `TestChunkOrphans_WalksEveryMirrorLeg`.
    Then the stage in `deletion_runner.go` before `account.EraseRows`
    (walk `blob_copies` of the tenant with `bucket = ''`, delete each on
    its leg, defer on a failure) and the leg walk in `chunk_orphans.go`.
0d. RP76-4, test first: `TestBlobCopiesJob_StopsAtTheVendorsEgressBudget`
    (a `backend_plans` row for the source with `egress_free_multiple` 3
    and 1 GiB stored → the pass copies 3 GiB this month and no more,
    `skipped` with the reason) and `TestBlobCopiesJob_LoopsWhileTimeRemains`
    (4,500 pending small rows copied in one run with time). Then the
    budget (sum of `copied_bytes` per source this UTC month from
    `job_runs.result` history or a small `blob_copies_egress` row per
    month — pick the table, say why) and the loop.
0e. RP76-5 (may trail into PR C if the day is short): at `settleEdgeRS`
    HEAD each shard (`engine.ObjectHeader` on its leg), refuse a size that
    is not `ShardBytes`; a tenant-scoped host's first stripe rebuilt and
    compared. RP76-7 (recompute `Verified` after the mirror branch),
    RP76-8 (gate the read link on the flag), RP76-9 (the DEPLOY.md
    sentence), RP76-10 (noted in the note, no change).

PR A — WP-PRICE-1. Branch `stage1a-p49-pricing`.
1. Test first: `TestPricingPage_EveryNumberFromPricesJSON` (render the
   built `pricing.html`, assert each price in `prices.json` appears
   formatted as the landing formats it, and no other dollar figure does),
   `TestPricingPage_ServedOnTheWebHostsOnly` (the `wp_p39_web_host_test`
   matrix: `/pricing` on the canonical host is the page, on `s3.` the S3
   XML), `TestLlmsTxt_LinksThePricingPage`, `TestSitemap_ListsPricing`.
2. `internal/api/landing/pricing.src.html` built by `build.py` into
   `internal/api/pricing.html` (embedded like the landing), the same CSS
   and sprites, the sections of the chain prompt's item 1; the calculator
   is the builder's maths (`builder.js`) on a `<form>` with no login; the
   compare table dated from `competitors._asof`. `web_host.go`: `/pricing`
   serves the page (keep `/pricing/` → `/pricing`); `web_files.go`'s
   sitemap lists it; `llms_txt.go` links it; the landing's nav and the FAQ
   link it. `make landing` regenerates; CI's landing-drift check covers
   the new output.

PR B — WP-LOG-1. Branch `stage1a-p49-reqlog`.
3. Migration 115 `115_request_log.sql`: `request_log (id bigserial, tenant_id,
   key_id, logged_at, op, bucket, object_key, status smallint, error_code,
   request_id, bytes_in, bytes_out, host)`, indexes `(tenant_id, logged_at
   desc)` and `(tenant_id, request_id)`; `request_suggestions (error_code
   pk, suggestion)`; `make test-db`, the reapply test; `erase.go` (by
   tenant), the export's exclusion with its reason, `retention.go` 7 d.
4. Test first: `TestRequestLog_EveryErrorCarriesItsSuggestion` (a 403
   `AccessDenied` from the fixture → one row with `error_code` and the
   suggestion joined), `TestRequestLog_NeverHoldsAnSSECKeyOrASecret` (the
   schema-walk pattern: no column of the table is a key or secret; the
   writer is handed only the fields it stores), `TestRequestLog_
   RetainedSevenDays`, `TestRequestLog_SearchByRequestID`,
   `TestRequestLog_KeysetPaged`, `TestRequestLog_ErasedWithTheAccount`,
   `TestRequestLog_FlagOffWritesNothing`. Then: the access-log tracker
   gains a second sink (`request_log`, every tenant, flag `request_log`
   default ON) fed by the same record the per-bucket log gets, the
   suggestion captured from the response writer's `WithSuggestion` once
   per error code into `request_suggestions`; `GET /api/v1/manage/requests
   ?key=&since=&status=&request_id=&cursor=` (OpenAPI, `docs/API.md` §
   Request log); the dashboard `/developers` page (the last 7 days, filter
   by key and status, the suggestion inline, a request-id search box) and
   its nav entry.

PR C — docs: `docs/reviews/WP-STRIPE-1.md` (D-STRIPE-1…n, RB7-1…n, the
prod proof: `/pricing` live, the deliberate 403 on the Developers page),
STATUS, plan § WP-STRIPE's two rows flipped DONE, CLAUDE.md (table
`request_log`, flag `request_log`, retention 7 d), `docs/API.md`, the
ledger row; Prompt 50 (WP-DX-1: test mode + the Docker image) written in
this file in both editions with Part 0 = RP78's findings placeholder. The
post-merge review is RP78.

Prod: PR 0 deploys and the Worker 0.5.1 goes out before the measurement;
PR A and B deploy dark-free (a page, a log) — turn `request_log` ON after
one deploy with the writer in place.

Done means: PR 0, A, B, C merged; `/pricing` live; the Developers page
showing tenant zero's requests; the mirror measurement in the note;
Prompt 50 written here.

## Stage 1a Prompt 50: WP-DX-1 — test mode and the local server (Part 0: RP78's findings)

**From 49 (`docs/reviews/WP-STRIPE-1.md`, reviewed by RP78 — `docs/reviews/WP-STRIPE-1-REVIEW.md`):**
- **`/pricing` is a page** generated by `make landing` from `prices.json` (pinned by `TestPricingPage_EveryNumberFromPricesJSON`); the request log is live for every tenant (`request_log`, migration 115, flag `request_log` default ON, 7 days, the Developers page and `GET /api/v1/manage/requests`).
- **The mirror's lag is bounded** (RP76-2: 16 MiB / 5 s, the Worker 0.5.2 — 0.5.1 was rolled back for a lost Content-Length and must never be deployed); the leg satisfies the bucket's jurisdiction and tier (RP76-1/6); codec chunk copies are erased and walked on the legs (RP76-3); the copy pass has a per-source egress budget (RP76-4).
- Residuals open: RB7-1 (management requests not in the request log), RB7-2 (the owner's call on the privacy-policy notice), RB7-3 (RS first-stripe rebuild), RB7-4 (the `blob_copies (leg, storage_key)` index).
- The request log's writer is the one place the sandbox's requests will show up (`host` and the `x-stored-mode` header the test mode adds): a sandbox request is logged under the SANDBOX tenant, never the parent's.

**Part 0 (RP78, in its order — `docs/reviews/WP-STRIPE-1-REVIEW.md`, HOLDS at P2):**
- **RP78-1 (P2):** the Worker's 18 MiB mirror buffer and the Go host's 16 MiB mirror queue are outside the byte budgets (`ISOLATE_BYTES`, RP72-6; `Host.Budget`, RP74-6). A mirrored PUT takes its buffer from the budget with a short wait and is sent UNMIRRORED when it cannot (the copy owed, counted `unbudgeted`), never refused. Tests: concurrent mirrored PUTs under a 20 MiB isolate budget; `TestPutTwo_NoBudgetNoMirror`.
- **RP78-2 (P2), open question 1 = option (b):** the commit goes at the primary's answer, and the mirror settles in the background (Worker `ctx.waitUntil`, cut at 30 s; Go host `mirrorDeadline`), its buffer still charged. New `POST /api/v1/edge/mirror` (token gate, outside the S3 gate) flips the copy `pending → ready` only while its row and admit are this one's, else queues the leg's address in `retired_blobs` (`orphan_write`). Tests: a 2 MB/s mirror leaves the client at the primary's pace and the copy `ready` after; a DELETE before the settle leaves nothing on the leg. Worker 0.6.0, proved in the workerd lab first (a 411-refusing sink, a mirror slower than 30 s).
- **RP78-3 (P3):** 28 of 44 origin rows on prod have no request id. ONE id per S3 request, set up front as `x-amz-request-id` on every response and in every row. `TestS3_EveryResponseCarriesTheLoggedRequestID` over the dispatcher matrix.
- **RP78-4 (P3):** `eu-west-3` is London. An explicit jurisdiction per region (used by the mirror rule and `IsEURegion`; a UK label). `TestMirrorLegFor_LondonIsNotTheEU`.
- **RP78-5 (P3):** the copy pass stops at one multiple of stored left for the customers (D-EDGE-60 amended). `TestBlobCopiesJob_LeavesTheCustomersTheirEgress`.
- **RP78-6 (P3, RB7-1):** management requests go into the request log too (`op` = route pattern, `host` = `api`). This goes with PR A's new routes. `TestRequestLog_ManagementRequestsToo`.
- **RP78-7…10 (P4):** the `<-mc` that drops a late ETag (moot after RP78-2); the backfill's typed strict list → `engine.StrictTargets()`; the dead `mirrorBody.cancel`; a `withdrawn` manifest field so 0.5.1's bytes cannot be redeployed.

Continue from WP-STRIPE-1 and its review RP78. `git log origin/main`, `gh pr list`.

Read first: plan § WP-STRIPE (the WP-DX-1 row — the contract); `internal/auth` (`LookupCredential`, the key prefixes, `CreateUserWithTenant`); `internal/api/placement.go` (`decidePlacement`: the sandbox branch is one branch before every other); `internal/api/events.go` and the webhook outbox (`livemode`); `internal/usage` (what "never billed, never metered" must skip: `tenant_quotas`, `bandwidth_usage_daily`, the egress meter); `internal/api/access_log.go` + `request_log.go` (the sandbox's rows); `cmd/vaultaire/main.go` and `config.DetectStorageMode` (what `docker run` boots); `.github/workflows/deploy.yml` (where the image is built and the `server_build` entry appended); `scripts/build-release.sh` (the reproducible recipe the image must reuse).

Lane: Developer surface. Migration 116 (`tenants.sandbox_of`, the pairing). PRs: (0) RP78's fixes; (A) the sandbox tenant and `VLT_TEST_` keys; (B) `stored listen` (the CLI's first verb) and `livemode`; (C) the Docker image and `docs/guides/local.md`; (D) docs.

What to build
1. **The sandbox tenant** (migration 116): every tenant gets a paired sandbox tenant on first use (`tenants.sandbox_of = <parent>`); a key with the prefix `VLT_TEST_` resolves to the sandbox. Every write of a sandbox tenant is routed to the `local` driver under `t-<sandbox>/` — one branch at the top of `decidePlacement`, before chunking, sealing, tiers, regions, the edge — and never to a paid backend; no quota is reserved, no bandwidth recorded, no egress metered. S3 responses carry `x-stored-mode: test`. `POST /api/v1/manage/sandbox/reset` purges it; a nightly job purges sandbox objects older than 7 days. The dashboard lists it as "test data".
2. **`livemode`**: a webhook event of a sandbox tenant is delivered to the parent's endpoints with `"livemode": false` in the body (Stripe's shape); a live event carries `"livemode": true`. **`stored listen`** (`cmd/stored`, the 26.4 CLI's first verb): a long-poll on the tenant's event log replayed to `http://localhost:<port>/<path>`, signed as the webhook is.
3. **The Docker image** (15.1): `docker run stored/vaultaire` boots the binary built by `scripts/build-release.sh` with the `local` driver and one tenant pre-made (its keys printed once at first boot), Postgres from `deploy/docker/compose.yml` (no embedded database), the same OpenAPI; built and published by the Deploy workflow and listed in the transparency log as a `server_build` (the binary's sha256 is the one the log already holds).

Tests (red first): `TestTestKey_NeverReachesAPaidBackend` (every fake paid driver asserts zero calls for a sandbox write — PUT, multipart, copy, the edge admit), `TestTestKey_NeverBilled` (`tenant_quotas`, `bandwidth_usage_daily`, the egress meter unchanged), `TestSandbox_ResetPurgesEverything`, `TestSandbox_NightlyPurgeAt7Days`, `TestWebhook_LivemodeFalseFromASandboxTenant`, `TestListen_ReplaysToLocalhost`, `TestDockerImage_BootsAndAnswersTheOpenAPI` (behind a build tag), the schema walks naming the column. Prod proof: tenant zero's `VLT_TEST_` key uploads to the sandbox and the paid backends' request logs (and `edge_admits`) show nothing; the image pulled and run on the Mac in under a minute.

Architecture notes
- The sandbox is a TENANT, not a mode flag on requests: every existing guard (erasure, export, quota, the request log) already works per tenant, and the pairing is one column. A sandbox tenant is erased with its parent.
- The edge never admits a sandbox write (pass-through `sandbox`): the local driver is the origin's.
- The image reuses the release recipe; it is not a second build. Its `server_build` entry is the binary's.
- Closeout: `docs/reviews/WP-STRIPE-2.md` (D-STRIPE-n continued, RB8-n), STATUS, plan § WP-STRIPE's row, CLAUDE.md, `docs/API.md` § Test mode, the ledger row; Prompt 51 (WP-PLAT-1) written here. The post-merge review is RP80.

ultrathink

## Stage 1a Prompt 50 — Opus edition

**From 49 (as above).**

You are building plan § WP-STRIPE's WP-DX-1 row in the Vaultaire repository — test mode (a sandbox tenant per tenant, `VLT_TEST_` keys, `livemode`, `stored listen`) and the Docker image — after Part 0, RP78's fixes. Read the Opus rules in `docs/reviews/WP-EDGE-PROMPTS.md` § "About the Opus editions". Worktree from `origin/main`. Migration 116.

Read before editing: plan § WP-STRIPE (the WP-DX-1 row); `docs/reviews/WP-STRIPE-1.md` + `WP-STRIPE-1-REVIEW.md`; `internal/auth/credentials.go` (`LookupCredential`) and the key-minting code; `internal/api/placement.go`; `internal/api/edge_ingest.go` (the admit's pass-through list); `internal/api/events.go`, `webhook_outbox.go`; `internal/usage/quota*.go`, `internal/api/egress_meter.go`, `bandwidth.go`; `internal/account/erase.go`; `cmd/vaultaire/main.go`; `scripts/build-release.sh`; `.github/workflows/deploy.yml`.

PR 0 — RP78's fixes (`docs/reviews/WP-STRIPE-1-REVIEW.md`). Branch `stage1a-p50-part0`; the Worker change may be its own PR (`stage1a-p50-worker060`), deployed only after its manifest row.
- **0a (RP78-1).** Test first:
  - node: `putTwo: a mirrored PUT the isolate budget cannot cover is sent unmirrored` (`setIsolateBudget(20 << 20)`, two concurrent 64 MiB mirrored PUTs: the second has one link and `mirror_unbudgeted: true`);
  - Go: `TestPutTwo_NoBudgetNoMirror` (a `Host.Budget` with nothing left: one PUT, no mirror, no error).
  Then:
  - `putTwo` (both) acquires `MIRROR_MAX_LAG + 2 MiB` from `isolateBudget()` / `Host.Budget` with the windowed reader's short wait, before the tee, and releases it when the mirror ends (answer, cut or error);
  - no budget = the plain PUT;
  - the commit's `mirror_unbudgeted` → `vaultaire_blob_copies_total{outcome="unbudgeted"}`.
  Verify: `cd edge/s3-ingest && npm test`, `go test -race ./internal/dataplane/ -run PutTwo`.
- **0b (RP78-2).** Test first:
  - node: `putTwo: the client is answered at the primary's pace; the mirror settles after` (a 2 MB/s mirror; `putTwo` resolves at the primary's answer, the settle promise later);
  - Go: `TestEdgeMirror_SettleAfterCommit` (commit, then `POST /api/v1/edge/mirror` with the ETag → `ready`; a DELETE in between → the leg's address in `retired_blobs` on the leg, no `ready` row; a second settle is idempotent; an unknown admit is 404);
  - add `/api/v1/edge/mirror` to `TestEdgeRoutes_NeverCapturedByTheS3Gate`.
  Then:
  - the Worker returns the client's response at the primary's answer and settles the mirror under `ctx.waitUntil` (cut at 30 s after the response);
  - the Go host settles in a goroutine bounded by `mirrorDeadline`;
  - both POST `{admit_id, etag | cut}` to the new route (token gate, Cloudflare peer);
  - the control plane flips the row under the key's lock only while the row is `pending`, names the admit's `storage_key` and the admit is `committed`;
  - the budget from 0a is held until the settle ends;
  - remove the after-primary timer (RP78-7 goes with it).
  Verify:
  - `go test -race ./internal/api/ -run 'EdgeMirror|EdgeRoutes'`;
  - the workerd lab (`wrangler dev --local`, the 411-refusing sink, a 2 MB/s mirror and a 40 s mirror): both bodies keep `content-length`, the client time is the primary's, the 40 s mirror is cut and its copy is `pending`;
  - Worker 0.6.0's manifest row before its deploy.
- **0c (RP78-3).** Test first: `TestS3_EveryResponseCarriesTheLoggedRequestID` (the dispatcher matrix, every op, a success and an error: the response's `x-amz-request-id` equals the `request_log` row's `request_id`, never empty). Then:
  - the dispatcher sets the request's id (the request-id middleware's) as `x-amz-request-id` before any handler;
  - every `generateRequestID()` in the S3 handlers and error bodies uses it;
  - `recordS3Access` reads it.
  Verify: `go test -race ./internal/api/ -run 'RequestLog|EveryResponse'`.
- **0d (RP78-4).** Test first: `TestMirrorLegFor_LondonIsNotTheEU`, `TestIsEURegion_London`. Then a `Jurisdiction` per row of `IDriveRegions` (`eu-west-3` → `uk`), `legRegion` → jurisdiction through it (lyve's region `us`), `IsEURegion` from it, and the bucket page's residency label and single-copy sentence for a UK bucket. Verify: `go test ./internal/drivers/ ./internal/api/ -run 'Jurisdiction|EURegion|MirrorLegFor' ./internal/dashboard/`.
- **0e (RP78-5).** Test first: `TestBlobCopiesJob_LeavesTheCustomersTheirEgress` (multiple 3, stored 10 KiB: the pass stops at 20 KiB of copies). Then the ledger's limit becomes `max(0, (multiple − 1) × stored − used)`, and the D-EDGE-60 sentence in CLAUDE.md is amended. Verify: `go test -race ./internal/api/ -run BlobCopiesJob`.
- **0f (RP78-6).** With PR A (its routes are the reason). Test first: `TestRequestLog_ManagementRequestsToo` (a JWT `GET /api/v1/manage/usage` and a refused `POST /api/v1/manage/keys` are rows: `op` = the route pattern, `host` = `api`, `key_id` = the JWT's key id or `session`). Then a management middleware feeding the tracker's request sink. The plan row's "S3 only" note goes.
- **0g (RP78-8…10).**
  - The backfill's strict list comes from `engine.StrictTargets()` as an array parameter.
  - Drop the dead `mirrorBody.cancel` (a comment on the `ac.abort` path).
  - 0.5.1's manifest rows are marked withdrawn, and the Worker deploy and `translog-verify -client` refuse a withdrawn build. How the withdrawal is recorded (a manifest field, or a log entry of its own) is this PR's call, but it must never re-append the row or rewrite its log entry. Test `TestClientBuildManifest_WithdrawnIsRefused`.

PR A — the sandbox. Branch `stage1a-p50-sandbox`.
1. Migration 116 `116_sandbox.sql`: `ALTER TABLE tenants ADD COLUMN IF NOT EXISTS sandbox_of TEXT REFERENCES tenants(id)` + a partial unique index `(sandbox_of) WHERE sandbox_of IS NOT NULL`; `make test-db`; the reapply test; `erase.go` erases the sandbox with its parent (a sandbox row first).
2. Test first: `TestTestKey_ResolvesToTheSandbox` (a `VLT_TEST_` key minted for tenant T resolves to T's sandbox, created on first mint; a live key never does), then `auth`'s minting: `POST /api/v1/manage/keys` with `"test": true` mints `VLT_TEST_<…>` on the sandbox (created in the same transaction: users → tenants (sandbox_of) → api_keys → tenant_quotas, the four-table rule).
3. Test first: `TestTestKey_NeverReachesAPaidBackend` (register fake `idrive`, `lyve`, `geyser` drivers that fail the test on any call; a sandbox PUT, a multipart upload, a CopyObject and an edge admit all land on `local` or pass through) and `TestTestKey_NeverBilled`. Then: `decidePlacement` gets ONE first branch — `if sandboxOf(tenant) != ""` → `placeOn("local")`, no chunking, no seal, no reservation; the edge admit passes `sandbox` through; `recordResponse` and the egress meter skip a sandbox tenant; responses set `x-stored-mode: test`.
4. Test first: `TestSandbox_ResetPurgesEverything`, `TestSandbox_NightlyPurgeAt7Days`. Then `POST /api/v1/manage/sandbox/reset` (OpenAPI) and a daily job `sandbox_purge` (10:30 UTC) through the scheduler; the dashboard's "Test data" card on the overview.

PR B — `livemode` and `stored listen`. Branch `stage1a-p50-listen`.
5. Test first: `TestWebhook_LivemodeFalseFromASandboxTenant` (an event of the sandbox goes to the PARENT's endpoints with `livemode: false`; a live one `true`). Then the outbox's body.
6. Test first: `TestListen_ReplaysToLocalhost` (a fake event log, an `httptest` server as localhost: each event POSTed once, signed, in order; a restart resumes from its cursor). Then `cmd/stored` with `listen --forward-to localhost:<port>/<path>` over `GET /api/v1/manage/events?cursor=` (long-poll, 25 s).

PR C — the image. Branch `stage1a-p50-docker`.
7. `Dockerfile` (FROM a pinned distroless base, COPY the release binary from `scripts/build-release.sh`), `deploy/docker/compose.yml` (Postgres 18 + the image, migrations run by an init step), the first-boot tenant (keys printed once, idempotent). Test: `TestDockerImage_BootsAndAnswersTheOpenAPI` behind `//go:build docker` (`docker compose up`, `GET /openapi.json`, a PUT/GET with the printed keys). `deploy.yml`: build and push on `main`; the image's binary sha256 equals the `server_build` entry's.
8. `docs/guides/local.md` (the five commands), `docs/API.md` § Test mode.

PR D — docs: `docs/reviews/WP-STRIPE-2.md`, STATUS, plan row DONE, CLAUDE.md (column, flag-free, job, env), the ledger row; Prompt 51 (WP-PLAT-1) in both editions with Part 0 = RP80. The post-merge review is RP80.

Prod: PR 0 first; PR A deploys dark until a `VLT_TEST_` key exists (none exist before); the image is published by the first `main` deploy after PR C.

Done means: PR 0, A, B, C, D merged; tenant zero's test key round-trips an object with zero calls to a paid backend (its request log rows say so); the image runs on the Mac in under a minute; Prompt 51 written here.

## Stage 1a Prompt 51: WP-PLAT-1 — API-created accounts, the Connect primitive (Part 0: RP80's findings)

**From 50 (`docs/reviews/WP-STRIPE-2.md`; the post-merge review is RP80):**
- **Test mode is live and dark until a test key exists:**
  - a `VLT_TEST_` key resolves to the tenant's **sandbox** (`tenants.sandbox_of`, migration 116), a tenant of its own;
  - every sandbox write lands on `local` (`placeOn`'s first branch) and is never billed or metered;
  - `POST /api/v1/manage/sandbox/reset` and the `sandbox_purge` job (7 days) clear it; it is erased with its parent;
  - a sandbox's events go to the parent's webhooks with `livemode: false`;
  - `stored listen` replays the event log (`GET /api/v1/events?after=…&wait=25`) to localhost.
- **The local image:** `make docker`, `deploy/docker/compose.yml`, and `ghcr.io/fairforge/vaultaire:<sha12>` pushed by Deploy with its binary's sha256 checked against the release artefact.
- **The mirror:**
  - settles after the commit (`POST /api/v1/edge/mirror`, Worker 0.6.0);
  - its buffer is the host's bytes (unbudgeted = unmirrored);
  - one request id per S3 request;
  - London is the UK;
  - the copy pass leaves one multiple of free egress to the customers;
  - 0.5.1's `edge-codec` is withdrawn.
- **What PLAT-1 inherits from 50:**
  - A **users-less tenant** already exists (a sandbox has no `users` row; its keys are its parent user's). PLAT-1's child tenants are the second kind, and every schema walk must hold them too.
  - **A tenant with a parent** is a pattern now (`sandbox_of`). A platform child is NOT a sandbox: it is billed (against the parent's pool) and lives on the real backends. Never reuse `sandbox_of` for it; `parent_tenant_id` is its own column.
  - **The sandbox's own quota row is its local-disk cap** (D-STRIPE-13, open question 1 for the owner). The pool walk of PLAT-1 must not count a sandbox.
- **Open:**
  - D-STRIPE-13 (the sandbox's cap);
  - RB8-2 (test requests on the parent's Developers page);
  - RB8-1 (a node's shutdown drops a settle in flight);
  - RB8-6 (`stored listen` forwards the list item, not the webhook body).

**Part 0 (RP80, in its order — `docs/reviews/WP-STRIPE-2-REVIEW.md`, HOLDS at P2):**
- **RP80-1 (P2):** a sandbox's downloads are never metered and never paced (`metered := … && !sandbox`), so a free account's test key could serve its 5 GiB through presigned links with no egress limit. **Meter without billing:** the sandbox gets an egress span on its own row with a fixed small allowance (1× its 5 GiB a month, a const), paced past it, and recorded in `bandwidth_usage_daily` under the sandbox id; the parent's counters and payments are untouched. A `VLT_TEST_` presigned URL lives at most 1 hour; a sandbox bucket is never Public (409 `sandbox_never_public`). Tests: `TestSandbox_DownloadsArePacedNeverBilled`, `TestTestKey_PresignCappedAtOneHour`, `TestSandbox_NeverPublic`.
- **RP80-2 (P3):** a suspended parent's sandbox keeps working. `isTenantSuspended` reads the tenant OR its `sandbox_of` parent in one query, and the test-key mint checks the parent. `TestSandbox_ParentSuspendedRefusesTestKeys`. PLAT-1's children need the same rule, through `parent_tenant_id`.
- **RP80-3 (P3):** `stored listen` can skip an event (a keyset on `(created_at, random uuid)` with concurrent autocommit inserts). Answer only events older than a 2 s settle horizon; `stored listen` remembers its last 1,000 ids. `TestEvents_AfterNeverSkipsALateCommit`.
- **RP80-4 (P3):** at most 4 concurrent long polls per tenant (429 `too_many_listeners` + `Retry-After`). `TestEvents_LongPollsBoundedPerTenant`.
- **RB8-2 (open question 2, yes):** `GET /api/v1/manage/requests?mode=test` and a Live / Test toggle on the Developers page. PLAT-1's child rows land on the same page.
- **RP80-5…7 (P4):** the sandbox lookup fails closed (503, never the live placement); the mirror settle refuses a caller whose host is not the admit's; the first-boot account prints a one-time password and runs only under `STORAGE_MODE=local`.
- **Open question 1 (D-STRIPE-13):** keep the sandbox's 5 GiB counter as its disk cap (RP80-1's allowance derives from it). RB8-3 (admin totals count sandboxes) is PLAT-1's: every admin total must tell live, sandbox and child tenants apart.

Continue from WP-STRIPE-2 and its review RP80. `git log origin/main`, `gh pr list`.

Read first:
- plan § WP-STRIPE (the WP-PLAT-1 row — the contract);
- `internal/auth/testkey.go` (how a tenant that is not a user's is made in one transaction: the pattern to follow, not the column);
- `internal/auth/handlers.go` `LookupCredential` (the one lookup);
- `internal/usage/quota*.go` (the quota check a pool must sit in);
- `internal/account/erase.go` (`eraseSandboxRows`: the cascade pattern);
- `internal/api/sandbox.go` (a per-tenant fact cached for the process because it never changes — `parent_tenant_id` never changes either);
- `internal/api/management_routes.go` + `request_log.go` `logManagementRequest` (every new route is logged);
- `internal/docs/openapi.go` and the drift guard.

Lane: Developer surface (the Stripe layer). Migration 117: `tenants.parent_tenant_id`, `tenants.platform`, `tenants.external_id`, `tenants.label`; a partial unique `(parent_tenant_id, external_id)`.

PRs:
- (0) RP80's fixes;
- (A) the platform tenant and its children;
- (B) the pooled quota;
- (C) the keys and STS tokens of a child, and the cascade;
- (D) docs.

What to build
1. **A platform tenant** (`tenants.platform = true`, set only by an admin through `PUT /api/v1/admin/tenants/{id}/platform`, audited `admin.platform_granted`):
   - its JWT (or a key with the new `PlatformAdmin` permission) may call `/api/v1/platform/*`. Any other caller gets a 404, never a 403: the routes do not exist for it.
   - `POST /api/v1/platform/tenants {label, external_id?}` creates a child: no user row, no e-mail (a `platform+<id>@platform.invalid` placeholder, as the sandbox's), residency inherited from the parent and never changed, `parent_tenant_id` set.
   - `GET /api/v1/platform/tenants[/{id}]` lists and reads children (usage included).
   - `DELETE …/{id}` schedules a child's erasure (the ordinary one).
2. **Pooled quota:** a child's `tenant_quotas` row draws from the parent's pool. The quota check walks one level (`parent_tenant_id`) in ONE query. Both a child's PUT and the parent's own PUT are 403 once the pool is spent; the edge admit's reservation is the same check.
3. **Keys:**
   - `POST /api/v1/platform/tenants/{id}/keys` mints a scoped key for a child; its scope is capped by the calling key's (`TestPlatform_ChildKeysNeverExceedTheParentsScope`).
   - STS tokens likewise.
   - A child has no primary pair: every key of a child is scoped and revocable by the parent.
4. **The cascade:** the erasure of a parent erases its children first (each through the ordinary runner path, objects included), then the parent. A child's erasure is ordinary.
5. **Metering:** every request_log / bandwidth row of a child carries the child's id. The parent's dashboard sums its children (a "Children" card); the `/platform` usage endpoint answers per child.

Tests (red first):
- `TestPlatform_CreatesATenantWithoutAUser`;
- `TestPlatform_ChildQuotaDrawsFromTheParentsPool`;
- `TestPlatform_ChildKeysNeverExceedTheParentsScope`;
- `TestPlatform_ANonPlatformTenantGets404`;
- `TestPlatform_EraseParentCascades`;
- `TestPlatform_ASandboxIsNeverInThePool`;
- `TestErasedTablesCoverSchema` / `TestExportSectionsCoverSchema` naming the columns.

Prod proof: tenant zero made a platform tenant by the admin route (owner's TOTP), one child created by the API, one object through the child's scoped key, the child's usage on the parent's card.

Architecture notes
- A child is a TENANT, exactly as a sandbox is: every per-tenant guard already holds. The difference is billing: a child is billed through the parent's pool; a sandbox is never billed.
- The routes mount for every process; the 404 is decided per caller, so no platform exists on prod until the owner grants one.
- Closeout: `docs/reviews/WP-STRIPE-3.md`, STATUS, plan § WP-STRIPE's row, CLAUDE.md (the columns, the routes, the admin route), `docs/API.md` § Platform, the ledger row; Prompt 52 (WP-TEAM-1) written here. The post-merge review is RP82.

ultrathink

## Stage 1a Prompt 51 — Opus edition

**From 50 (as above).**

You are building plan § WP-STRIPE's WP-PLAT-1 row in the Vaultaire repository: platform tenants that create users-less child tenants by API, their pooled quota, their scoped keys and the erasure cascade. Part 0 (RP80's fixes) comes first. Read the Opus rules in `docs/reviews/WP-EDGE-PROMPTS.md` § "About the Opus editions". Worktree from `origin/main`. Migration 117.

Read before editing:
- plan § WP-STRIPE (the WP-PLAT-1 row);
- `docs/reviews/WP-STRIPE-2.md` + `WP-STRIPE-2-REVIEW.md`;
- `internal/auth/testkey.go`, `internal/auth/handlers.go` (`LookupCredential`), `internal/auth/apikey.go` (`GenerateAPIKey`, `checkKeyCap`, `KeyCreateOptions.Validate`);
- `internal/usage/quota*.go` (`reserveQuota` and its SQL);
- `internal/api/quota_accounting.go` (`reserveWrite`);
- `internal/api/edge_ingest.go` (the admit's reservation);
- `internal/account/erase.go` (`eraseSandboxRows`), `internal/api/deletion_runner.go` (stage b4);
- `internal/api/sandbox.go`;
- `internal/api/management_routes.go`, `request_log.go` (`logManagementRequest`);
- `internal/docs/openapi.go`.

PR 0 — RP80's fixes (`docs/reviews/WP-STRIPE-2-REVIEW.md`). Branch `stage1a-p51-part0`.
- **0a (RP80-1).** Test first:
  - `TestSandbox_DownloadsArePacedNeverBilled`: a sandbox past its allowance is paced at `EGRESS_THROTTLE_MIN_BYTES_PER_SEC` with the `egress_throttle` flag on (counted in `vaultaire_egress_would_throttle_total` with it off); its bytes are in `bandwidth_usage_daily` under the sandbox id; the parent's rows, `payments` and `entitlements` are unchanged.
  - `TestTestKey_PresignCappedAtOneHour`: a `VLT_TEST_` presigned GET with `X-Amz-Expires` over 3600 is 400 `AuthorizationQueryParametersError`.
  - `TestSandbox_NeverPublic`: `modes.SetBucket` to Public for a sandbox bucket, and a create with `x-amz-acl: public-read`, answer 409 `sandbox_never_public` / S3 400 `InvalidRequest`.

  Then:
  - in `s3.go` drop `&& !sandbox` from `metered` and from the bandwidth record;
  - `usage.QuotaManager.EgressAllowance` answers a sandbox `sandboxEgressAllowance` (5 GiB, a const beside `sandboxBackend`), never from `prices.json`;
  - the presign check and the mode door as above.

  Verify: `go test -race ./internal/api/ -run 'Sandbox|TestKey' ./internal/usage/ ./internal/modes/`.
- **0b (RP80-2).** Test first: `TestSandbox_ParentSuspendedRefusesTestKeys` (the parent suspended: a test-key PUT is 403 `AccountSuspended`; `POST /api/v1/manage/keys {test: true}` is 403). Then:
  - `isTenantSuspended` becomes `SELECT t.suspended_at IS NOT NULL OR p.suspended_at IS NOT NULL FROM tenants t LEFT JOIN tenants p ON p.id = t.sandbox_of WHERE t.id = $1`;
  - `GenerateTestKey` checks the parent.
  PR B's children reuse the same join through `parent_tenant_id`.
- **0c (RP80-3, RP80-4).** Test first:
  - `TestEvents_AfterNeverSkipsALateCommit` (an event committed after a later-stamped one is still delivered);
  - `TestEvents_LongPollsBoundedPerTenant` (the 5th concurrent wait is 429 + `Retry-After`);
  - `TestListen_NeverForwardsAnIDTwice`.

  Then:
  - `eventsAfter` adds `AND e.created_at <= now() - interval '2 seconds'`;
  - a per-tenant semaphore of 4 in `listEventsAfter`;
  - `cmd/stored listen` keeps an LRU of its last 1,000 forwarded ids.

  Verify: `go test -race ./internal/api/ -run Events ./cmd/stored/`.
- **0d (RB8-2).** Test first: `TestRequestLog_ModeTestReadsTheSandbox` (the parent's JWT with `?mode=test` reads the sandbox's rows; without it, its own). Then the query parameter (OpenAPI) and the Developers page's Live / Test toggle.
- **0e (RP80-5…7).**
  - `placeOn` answers 503 when the sandbox lookup errors (`TestSandbox_LookupErrorFailsClosed`).
  - `edgeMirrorSettle` refuses a caller whose host is not `row.Host` (404).
  - `bootstrapTenant` prints a one-time password beside the keys and refuses unless `STORAGE_MODE=local` (`TestBootstrapTenant_LocalOnly`).

PR A — the platform tenant and its children. Branch `stage1a-p51-platform`.
1. Migration 117 `117_platform.sql`:
   - `ALTER TABLE tenants ADD COLUMN IF NOT EXISTS parent_tenant_id VARCHAR(255) REFERENCES tenants(id)`, `platform BOOLEAN NOT NULL DEFAULT FALSE`, `external_id TEXT`, `label TEXT`;
   - `CREATE UNIQUE INDEX IF NOT EXISTS idx_tenants_parent_external ON tenants (parent_tenant_id, external_id) WHERE parent_tenant_id IS NOT NULL AND external_id IS NOT NULL`;
   - then `make test-db`, the reapply test, and the schema walks: `TestErasedTablesCoverSchema` needs no new table; the export section for `tenants` names the four columns.
2. Test first: `TestPlatform_ANonPlatformTenantGets404` (a JWT of a tenant with `platform = false` gets 404 on `POST /api/v1/platform/tenants`; an admin's `PUT /api/v1/admin/tenants/{id}/platform {"platform": true}` makes the next call 201, audited `admin.platform_granted`). Then:
   - `internal/api/platform_tenants.go`: routes under `/api/v1/platform` behind `requireJWT` + `logManagementRequest`, and a middleware that answers `writeManagementError(... ErrTypeNotFound ...)` unless the JWT's tenant has `platform`;
   - the admin route in `admin_routes` (classified in `apiAdminWrites`).
3. Test first: `TestPlatform_CreatesATenantWithoutAUser` (201 `{object: "tenant", id, label, external_id, residency}`; no `users` row; `tenants.parent_tenant_id` = the caller; a second create with the same `external_id` is 409 `external_id_taken`; a child of a child is 400 `child_of_child`). Then `internal/auth/platform.go` `CreateChildTenant(ctx, parentTenantID, label, externalID)`, in ONE transaction:
   - the parent row `FOR SHARE`;
   - then `tenants` (`platform+<id>@platform.invalid`, the parent's region in metadata);
   - then `tenant_quotas` (limit 0: the pool decides).
4. Test first: `TestPlatform_ListAndReadChildren` (usage per child from `tenant_quotas`). Then the GET routes; `DELETE /api/v1/platform/tenants/{id}` schedules the child's erasure through `internal/account` (a child has no user: `account.ScheduleTenant`, new, the same 30-day grace).

PR B — the pool. Branch `stage1a-p51-pool`.
5. Test first: `TestPlatform_ChildQuotaDrawsFromTheParentsPool` (parent limit 10 KiB; a 6 KiB PUT by child A, then a 6 KiB PUT by child B is 403 QuotaExceeded, and the parent's own 6 KiB PUT too), `TestPlatform_ASandboxIsNeverInThePool`. Then:
   - `reserveQuota`'s SQL resolves the pool owner `COALESCE(parent_tenant_id, id)` in the same statement and checks `SUM(used) of the pool` against the owner's limit;
   - the edge admit uses the same function (no second rule);
   - `usage.Billed*` reports a child under its own id and the pool under the owner.

PR C — keys and the cascade. Branch `stage1a-p51-keys`.
6. Test first: `TestPlatform_ChildKeysNeverExceedTheParentsScope` (the caller's key scoped to `GetObject` on bucket `a`; asking `PutObject`, or bucket `b`, for a child is 400 `scope_exceeds_parent`). Then `POST/GET/DELETE /api/v1/platform/tenants/{id}/keys[/{key}]` and `POST …/sts` (the STS scope intersection reused).
7. Test first: `TestPlatform_EraseParentCascades` (a parent with two children, each holding an object and a key: the runner erases both children — objects, rows, keys — before the parent's rows; `EraseRows` refuses the parent while a child row exists). Then the cascade in `internal/account` (children first, like `eraseSandboxRows`) and stage b5 in the deletion runner.

PR D — docs: `docs/reviews/WP-STRIPE-3.md`, STATUS, plan row DONE, CLAUDE.md (the columns, the routes, the admin route), `docs/API.md` § Platform, the ledger row; Prompt 52 (WP-TEAM-1) in both editions with Part 0 = RP82. The post-merge review is RP82.

Prod: PR 0 first. PRs A–C deploy dark: no tenant has `platform = true` until the owner grants one. The proof needs the owner's TOTP for the admin route.

Done means: PRs 0, A, B, C, D merged; tenant zero is a platform tenant, one child was made by the API, and one object went through the child's scoped key with its bytes counted in the parent's pool; Prompt 52 written here.

## Stage 1a Prompt 52: WP-TEAM-1 — teams: members, roles and invitations on a tenant (Part 0: RP82's findings)

**From 51 (`docs/reviews/WP-STRIPE-3.md`; the post-merge review is RP82):**
- **Platform tenants are live and dark until the owner grants one** (migration 117):
  - `tenants.platform` (set only by `PUT /api/v1/admin/tenants/{id}/platform`, admin + TOTP) opens `/api/v1/platform/*` to that tenant's JWT; every other tenant gets 404;
  - a **child** is a `tenants` row with `parent_tenant_id`, no `users` row, no primary pair; its keys are scoped (`auth.CapScope`: never wider than the calling credential) and their row's `user_id` is the platform user who minted them;
  - **the pool:** `usage.ReserveTx` locks the owner's quota row and sums the pool (`usage/pool.go`); the edge admit goes through it;
  - **the cascade:** the deletion runner's stage b5 erases a platform's children first; `EraseRows` refuses a platform while a child row remains; a child is scheduled on `tenants.deletion_scheduled_at`.
- **Part 0 of 51 (RP80):** a sandbox is metered (never billed) against a fixed 5 GiB allowance, its presigned links ≤ 1 h, never Public, suspended with its parent; the event cursor's 2 s horizon and ≤ 4 waits per tenant; `?mode=test` on the request log.
- **What TEAM-1 inherits from 51:**
  - **Who acts is now "a user on a tenant", not "the tenant's user".** The JWT's `tenant_id` claim is the user's one tenant today (users ↔ tenants by e-mail, the registration contract). Members make the tenant a choice: the session and the JWT must carry the ACTIVE tenant, and every route that reads `tenantIDKey` (the management API, `/api/v1/platform`, `requirePlatform`, the request log's management rows) reads the active one.
  - **Keys are listed by user today** (`AuthService.ListAPIKeys(userID)`, the free-tier cap counts `user_id`). A child's keys were kept out of that list by a clause (`notChildKeySQL`); members make "the tenant's keys" the right list. Change it to tenant-keyed listing, never a second exclusion clause.
  - **Account deletion is user-centric** (`users.deletion_scheduled_at`, `account.ListDue` joins users → tenants by e-mail). With members: deleting the ACCOUNT is the owner's, and it erases the tenant (and its sandbox, its children); a member leaving removes a `tenant_members` row, never a tenant.
  - **The audit row names the member** (`audit.Entry.Actor` = the acting user; `UserID` = the subject). `platform.*` rows already put the platform tenant in `tenant_id` and the acting user in `performed_by`.
- **Decided the same day** (`docs/reviews/WP-STRIPE-3.md` § The open questions, decided; **`docs/ACCOUNTS.md`, the account model — read it**):
  - D-STRIPE-29: a platform, an app's backend and an agent use a **management key** (WP-KEY-1), never a person's JWT;
  - D-STRIPE-30: `tenants.residency` (WP-ACCT-1);
  - D-STRIPE-31: a child's downloads count against its POOL's allowance (Part 0 below).
- **What TEAM-1 must leave room for (`docs/ACCOUNTS.md`):** a family is a pool of personal tenants that reuses THIS WP's invitations, so `tenant_invitations` gets a `kind` (`member` now, `family` in WP-FAMILY-1); and a team is one shared tenant. Never make a family "a tenant with members".

**Part 0 (in this order):**
1. **D-STRIPE-31 / RB9-2: a child's downloads count against its pool's allowance.** Test first: `TestPool_AChildsDownloadsSpendThePoolsAllowance`. Its setup: a platform with an allowance of 10 MiB; child A downloads 6 MiB and child B 6 MiB. It asserts:
   - B's second response is paced with `egress_throttle` on, and the same decision is counted with it off;
   - the rows stay under A and B;
   - the alert ladder fires for the platform.

   Then `usage.EgressAllowance` and `egressMeter` resolve the pool root through ONE function (`usage.PoolRoot(ctx, q, tenantID)`, the owner `COALESCE(parent_tenant_id, id)`; WP-ACCT-1 swaps it for `pool_root_id`):
   - the allowance is the root's;
   - the month's egress is summed over the pool (`MonthEgressBytes` of the root sums the pool's rows);
   - the live counter is the root's (`egressMeter.tenant` keyed by the root for decide and pace), while `BandwidthTracker` still records each tenant's own rows;
   - a sandbox is never in a pool (its fixed allowance, RP80-1).
2. **RP82's findings** (`docs/reviews/WP-STRIPE-3-REVIEW.md`, HOLDS at P2), in order:
   - **RP82-2 (P2)** is item 1 above. Also: a child's limit-0 row must never read as an unlimited allowance (`TestEgress_ZeroAllowanceIsNotUnlimitedForAChild`).
   - **RP82-1 (P2): floors are pooled like the total.** Today `ReserveTx` checks the pool's total, but the floor check reads the CHILD's `tenant_floor_quotas`, and a child has none. A platform with 1 TB Standard + 10 TB Vault lets its children put 11 TB on Standard at the Vault price. A child gets counting floor rows (limit 0) whenever its owner has floors (at creation, and by `SetHouse` for existing children); the floor check sums the pool's rows for that floor against the OWNER's floor limit; lock order: owner total → child total → owner floor → child floor. `TestPool_ChildFloorsDrawFromTheOwnersFloors`; the `TestLedger_LockOrder_*` structure test extended.
   - **RP82-5 (P4):** drop `GenerateChildKey`'s unused `deletion_scheduled_at` read.
   - **Not in this Part 0:** RP82-3 (a denormalised pool counter), RP82-4 (flag inheritance: `standard_sealed_default` must inherit down the tree) and RP82-7 (admin totals by tenant kind) are WP-ACCT-1's, Prompt 53. RP82-6 (a management key's mint ceiling is its own field, not its bucket scope) is WP-KEY-1's, Prompt 54.

Continue from WP-STRIPE-3 and its review RP82. `git log origin/main`, `gh pr list`.

Read first:
- plan § WP-STRIPE (the WP-TEAM-1 row — the contract);
- `internal/auth/auth.go` (`CreateUserWithTenant`, the four-table rule; `GenerateJWT` / `ValidateJWT` and the claims; `LoadFromDB`), `internal/auth/apikey.go` (`ListAPIKeys`, `checkKeyCap`, `RevokeAPIKey`'s ownership), `internal/auth/platform_keys.go` (`notChildKeySQL`, the clause members replace);
- `internal/dashboard/auth` (the session: `SessionData`) and `internal/dashboard/handlers` (every handler reads `sd.TenantID`);
- `internal/api/server.go` `requireJWT`, `internal/api/middleware.go` (`requireAdmin`), `internal/api/platform_tenants.go` `requirePlatform`;
- `internal/account/service.go` + `erase.go` (who may schedule; what a member's removal erases — nothing but the membership);
- `internal/audit` (`Entry.Actor`);
- `internal/email` (the sender; the invitation's signed link reuses `VERIFY_SECRET`'s HMAC shape).

Lane: Developer surface (the Stripe layer). Migration 118: `tenant_members` (tenant, user, role, invited_by, accepted_at; unique (tenant, user)), `tenant_invitations` (tenant, e-mail, role, token hash, expires_at, accepted_at).

PRs:
- (0) RP82's fixes;
- (A) members and roles: the tables, registration creates the owner member, the role check on every management and dashboard write, the active tenant in the session and the JWT;
- (B) invitations: e-mail, the signed link (7 days), accept, the Members page and the `members` routes;
- (C) a user in several tenants: the switcher, keys and audit by the active tenant;
- (D) docs.

What to build
1. **`tenant_members`** with roles `owner` / `admin` / `developer` / `viewer`. Registration writes the owner member in its one transaction (the four-table rule becomes five). A backfill makes every existing user the owner member of its tenant (in the migration, idempotent).
2. **Authorisation by role**, one function (`auth.Can(role, action)`) every write calls: a viewer reads; a developer makes keys and buckets; an admin invites and manages members (never the owner); the owner alone deletes the account, changes billing or grants nothing a platform admin grants. The JWT carries the active tenant and the role; `requirePlatform` reads the active tenant.
3. **Invitations:** `POST /api/v1/manage/members {email, role}` sends a signed link (7 days, single use, its hash stored); accepting creates the member (a new user registers through the link without a tenant of their own — the four-table rule's tenant step is the inviting tenant's member row).
4. **Several tenants per user:** the dashboard's switcher; `POST /auth/login` returns the user's tenants and mints a JWT for one (`tenant_id` in the body, default the owned one); every key, audit and request-log list is by the active tenant.
5. **The audit row names the member** (actor = the acting user, tenant = the active tenant).

Tests (red first):
- `TestMembers_ViewerCannotCreateAKey`;
- `TestMembers_OnlyTheOwnerDeletesTheAccount`;
- `TestMembers_InvitationExpiresIn7Days`;
- `TestMembers_AUserInTwoTenantsSeesTheRightOne`;
- `TestMembers_AuditRowNamesTheMember`;
- `TestRegister_CreatesTheOwnerMember`;
- `TestMembers_APlatformsMemberActsOnItsChildren` (a developer of a platform mints a child's key; a viewer cannot);
- the schema walks: `TestErasedTablesCoverSchema` (both tables erased with the tenant; a member's own user row is NOT erased with a tenant it does not own), `TestExportSectionsCoverSchema`.

Prod proof: tenant zero invites a second address of the owner's (the owner's mailbox), the invitee accepts and sees the buckets read-only, the audit page names both. Needs the owner (TOTP and the mailbox).

Architecture notes
- A member is a (user, tenant, role) row. Nothing about a tenant's data changes: S3 keys stay tenant-keyed; only the management surfaces learn who acts.
- The role default for an un-migrated flow is `owner` for the registering user, so every existing account keeps working unchanged.
- Closeout: `docs/reviews/WP-STRIPE-4.md`, STATUS, plan § WP-STRIPE's row, CLAUDE.md (the tables, the role rule, the active tenant), `docs/API.md` § Members, the ledger row; Prompt 53 (WP-ACCT-1) written here. The post-merge review is RP84.

ultrathink

## Stage 1a Prompt 52 — Opus edition

**From 51 (as above).**

You are building plan § WP-STRIPE's WP-TEAM-1 row in the Vaultaire repository: members, roles and invitations on a tenant, and a user who belongs to several tenants. Part 0 (RP82's fixes) comes first. Read the Opus rules in `docs/reviews/WP-EDGE-PROMPTS.md` § "About the Opus editions". Worktree from `origin/main`. Migration 118.

Read before editing:
- plan § WP-STRIPE (the WP-TEAM-1 row);
- `docs/reviews/WP-STRIPE-3.md` + `WP-STRIPE-3-REVIEW.md`;
- `internal/auth/auth.go` (`CreateUserWithTenant`, `GenerateJWT`, `ValidateJWT`, `JWTClaims`, `LoadFromDB`), `internal/auth/apikey.go` (`ListAPIKeys`, `checkKeyCap`, `RevokeAPIKey`), `internal/auth/platform_keys.go` (`notChildKeySQL`);
- `internal/dashboard/auth` (`SessionData`, the store), `internal/dashboard/handlers/keys.go`, `settings.go`;
- `internal/api/server.go` (`requireJWT`), `internal/api/management_routes.go` (every handler reading `tenantIDKey` / `userIDKey`), `internal/api/platform_tenants.go` (`requirePlatform`);
- `internal/account/service.go`, `erase.go`;
- `internal/audit/audit.go`;
- `internal/email` (the sender) and the verify-token code (`VERIFY_SECRET`).

PR 0 — Branch `stage1a-p52-part0`. Read `docs/ACCOUNTS.md` first.
- **0a (D-STRIPE-31, RB9-2).** Test first: `TestPool_AChildsDownloadsSpendThePoolsAllowance` (the platform's allowance 10 MiB; children A and B download 6 MiB each; B's second response is paced with `egress_throttle` on and counted in `vaultaire_egress_would_throttle_total` with it off; the rows stay under A and B; the platform gets the alert). Then `usage.PoolRoot(ctx, q, tenantID)` (one function; `COALESCE(parent_tenant_id, id)`), `EgressAllowance` and `MonthEgressBytes` answer the root's allowance and the pool's month, `egressMeter.tenant` keys its counter by the root while `BandwidthTracker` keeps each tenant's own rows; a sandbox keeps its fixed allowance. Verify: `go test -race ./internal/usage/ ./internal/api/ -run 'Pool|Egress|Sandbox'`.
- **0b (RP82-2's second half).** Test first: `TestEgress_ZeroAllowanceIsNotUnlimitedForAChild`: a child's own limit-0 quota row never yields an unpaced allowance. `EgressAllowance` is reached only through `PoolRoot` for a child. Verify: `go test -race ./internal/usage/ -run Egress`.
- **0c (RP82-1).** Test first:
  - `TestPool_ChildFloorsDrawFromTheOwnersFloors`: owner floors 10 KiB Standard + 100 KiB Vault; a child's 12 KiB Standard PUT is 403 QuotaExceeded while a 12 KiB Vault PUT succeeds; the owner's own Standard PUT counts against the same 10 KiB;
  - `TestPool_SetHouseGivesChildrenCountingFloors`;
  - extend `TestLedger_LockOrder_TotalBeforeFloors_Structure` to the owner-first order.

  Then:
  - `auth.CreateChildTenant` writes limit-0 `tenant_floor_quotas` rows for each floor its owner has;
  - `usage.SetHouse` adds them to existing children;
  - `ReserveTx` and `ApplyLedgerTx` lock the owner's floor row, then the child's, and check the SUM of the pool's rows for that floor against the owner's floor limit;
  - the reconcile (`usage/billed.go`) recomputes each child's floor rows from its head rows as it does the owner's.

  Verify: `go test -race ./internal/usage/ ./internal/api/ -run 'Pool|Floor|Ledger'`.
- **0d (RP82-5).** Drop the unused read in `GenerateChildKey`.

PR A — members and roles. Branch `stage1a-p52-members`.
1. Migration 118 `118_members.sql`:
   - `CREATE TABLE IF NOT EXISTS tenant_members (tenant_id VARCHAR(255) NOT NULL REFERENCES tenants(id) ON DELETE CASCADE, user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE, role TEXT NOT NULL CHECK (role IN ('owner','admin','developer','viewer')), invited_by UUID, accepted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (tenant_id, user_id))`;
   - a partial unique index: one `owner` per tenant;
   - the backfill: `INSERT … SELECT t.id, u.id, 'owner' FROM users u JOIN tenants t ON t.email = u.email ON CONFLICT DO NOTHING` (idempotent; a child and a sandbox have no user and get no row);
   - `tenant_invitations (id UUID PK, tenant_id, kind TEXT NOT NULL DEFAULT 'member' CHECK (kind IN ('member','family')), email, role, token_hash TEXT NOT NULL, invited_by UUID, expires_at, accepted_at, created_at)` (`family` is WP-FAMILY-1's; this WP accepts `member` only);
   - then `make test-db`, the reapply test, `TestErasedTablesCoverSchema` (both tables erased by tenant; `tenant_members` also by user for a user erased from a tenant it does not own) and `TestExportSectionsCoverSchema` (a `members` section).
2. Test first: `TestRegister_CreatesTheOwnerMember`. Then `CreateUserWithTenant` writes the member row in its transaction.
3. Test first: `TestMembers_ViewerCannotCreateAKey`, `TestMembers_OnlyTheOwnerDeletesTheAccount`, `TestMembers_APlatformsMemberActsOnItsChildren`. Then:
   - `internal/auth/members.go`: `Role`, `Can(role, action)` with the action table of the plan row as a Go map (the one rule), `MemberRole(ctx, db, tenantID, userID)`;
   - `JWTClaims` gains `Role`; `requireJWT` puts it on the context; a JWT minted before 118 (no role claim) is read as `owner` only when the user owns the tenant (a member row says so), else refused;
   - every management and platform write calls `requireRole(action)` (a middleware per route group, the action named per route — `TestManagementRoutes_EveryWriteNamesItsAction` walks the router);
   - the dashboard's handlers read the session's role the same way.

PR B — invitations. Branch `stage1a-p52-invitations`.
4. Test first: `TestMembers_InvitationExpiresIn7Days` (accepting at day 8 is 410 `invitation_expired`; a token is single use; its hash, never the token, is stored). Then `POST/GET/DELETE /api/v1/manage/members[/{user}]`, `POST /api/v1/manage/invitations/{token}/accept`, the e-mail through the existing sender, the dashboard's Members page (invite, change a role, remove; the owner row cannot be removed or demoted).

PR C — several tenants. Branch `stage1a-p52-switch`.
5. Test first: `TestMembers_AUserInTwoTenantsSeesTheRightOne` (a user in tenants A and B: a JWT for A lists A's buckets and keys, never B's; switching mints B's). Then `POST /auth/login` answers `tenants: [{id, role}]` and takes `tenant_id`; the dashboard's switcher sets the session's active tenant; `ListAPIKeys` becomes tenant-keyed (`notChildKeySQL` goes: a child's keys are the child's tenant's, never in any member's list).
6. Test first: `TestMembers_AuditRowNamesTheMember`. Then every `audit.Entry` from the management and dashboard paths carries the actor (the member) and the active tenant.

PR D — docs: `docs/reviews/WP-STRIPE-4.md`, STATUS, plan row DONE, CLAUDE.md (the tables, `auth.Can`, the active tenant), `docs/API.md` § Members, the ledger row; Prompt 53 (WP-ACCT-1, `docs/ACCOUNTS.md`) in both editions with Part 0 = RP84. The post-merge review is RP84.

Prod: PR 0 first. PRs A–C deploy with the backfill: every existing user is its tenant's owner, so nothing changes for anyone until an invitation is sent. The proof needs the owner (TOTP and the mailbox).

Done means: PRs 0, A, B, C, D merged; tenant zero has a second member who accepted an invitation and reads its buckets read-only, and the audit page names both; Prompt 53 (WP-ACCT-1) written here.

## The chain

- **Prompt 49 — WP-PRICE-1 + WP-LOG-1 — DONE 2026-10-28/29** (`docs/reviews/WP-STRIPE-1.md`: #1038 Part 0, #1039 pricing, #1041 request log, #1040/#1042 the Worker 0.5.1→0.5.2). The post-merge review is RP78.
- **Prompt 50 — WP-DX-1 — DONE 2026-10-29** (`docs/reviews/WP-STRIPE-2.md`). PRs:
  - #1047: Part 0, RP78-1…10.
  - #1048 / #1052: the Worker 0.6.0 and its rows.
  - #1049: the sandbox, migration 116.
  - #1050: `livemode` and `stored listen`.
  - #1051: the image.
  - The post-merge review is RP80.
  - **Reviewed by RP80 (2026-10-29, `docs/reviews/WP-STRIPE-2-REVIEW.md`): HOLDS at P2** — RP80-1 sandbox downloads unmetered and unpaced (free hosting once signups open); three P3s, four P4s.
- **Prompt 51 — WP-PLAT-1 — DONE 2026-10-29** (`docs/reviews/WP-STRIPE-3.md`; migration 117). PRs: #1055 Part 0 (RP80), #1056 the platform and its children, #1057 the pool, #1058 keys and the cascade, #1059 the Worker test flake. Prod proof: tenant zero a platform, one child, one object through its key counted in the pool. The post-merge review is RP82. **Reviewed by RP82 (2026-10-29, `docs/reviews/WP-STRIPE-3-REVIEW.md`): HOLDS at P2** — RP82-1 a child skips its platform's Standard/Vault floors, RP82-2 a child's downloads have no allowance (= D-STRIPE-31); two P3s for WP-ACCT-1, three P4s.
- **Prompt 52 — WP-TEAM-1** (members, roles, invitations, several tenants per user) — WRITTEN above by Prompt 51's closeout, both editions. Part 0 = D-STRIPE-31 + RP82-1/2/5 (filled by RP82). Migration 118. The post-merge review is RP84.
- **53 — WP-ACCT-1** (the account tree: `pool_root_id`, `child_kind`, caps inside the pool, `tenants.residency`); **54 — WP-KEY-1** (management keys first, then budgets); **55 — WP-AGENT-1 + WP-APP-1**; **56 — WP-FAMILY-1**; then WP-POST-1, WP-XFER-1 (after the first node), WP-VAULT-2 — re-cut 2026-10-29 by `docs/ACCOUNTS.md`; each written by the closeout before it.
