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

## Stage 1a Prompt 53: WP-ACCT-1 — the account tree: `pool_root_id`, `child_kind`, caps inside the pool, residency (Part 0: RP84's findings)

**From 52 (`docs/reviews/WP-STRIPE-4.md`; the post-merge review is RP84):**
- **Members and roles are live and change nothing until an invitation is sent** (migration 118). The pieces:
  - `tenant_members` (owner / admin / developer / viewer; the backfill made every user its tenant's owner) and `tenant_invitations` (`kind member|family`, only the token's SHA-256, single use, 7 days);
  - `auth.Can(role, action)` is the one rule. `requireJWT` reads the role from the row on every request (the JWT's claim is the client's). `requireMember` + `requireRole(action)` sit on every write of `/api/v1/manage`, `/platform`, `/user`, `/webhooks`, `/sts`, and `dashboardAction` on every `/dashboard` write;
  - **the active tenant:** `POST /auth/login` answers `tenants` and takes `tenant_id`; the dashboard's switcher is on the Members page. **Keys are the tenant's** (list, revoke, rotate, expiry, the STS parent and the cap by tenant; `notChildKeySQL` is gone);
  - `requireMember` stamps the active tenant on the audit rows (`audit.WithTenant`).
- **Part 0 of 52:**
  - the pool's egress (D-STRIPE-31) goes through `usage.PoolRoot`: the root's allowance, the pool's month, the meter keyed by the root, the rows per tenant;
  - floors are pooled like the total: a child has counting floor rows, the check is the owner's floor limit against the pool's sum, and the lock order is owner total → child total → owner floor → child floor.
- **What WP-ACCT-1 inherits from 52:**
  - **ONE resolver to swap:** `usage.PoolRoot` (egress) and `lockPoolOwner` / `poolUsed` / `lockPoolFloor` / `poolFloorUsed` (quota, floors) all read `COALESCE(parent_tenant_id, id)`. `pool_root_id` replaces that expression in these functions and nowhere else.
  - **RB10-1:** a member with no tenant of its own cannot leave or erase its sign-in. Decide self-leave (`DELETE /api/v1/manage/members/{self}` for any member but the owner) and the erasure of a tenant-less sign-in (its memberships, then the user, no tenant step).
  - **The `sponsored` edge reuses invitations** (`tenant_invitations.kind = 'family'`, WP-FAMILY-1). Accepting one must ATTACH a person's own tenant as a sponsored child; it is never a member row. Leave `kind` alone here.

**Part 0 (in this order):** RP84's findings (`docs/reviews/WP-STRIPE-4-REVIEW.md`: HOLDS at P2, all latent on prod — 3 owner members, 0 invitations) and RP85's two P4s (`docs/reviews/STAGE1-CLOSEOUT-REVIEW.md`: the 52E closeout HOLDS, nothing above P4):
- **RP84-1 (P2):** `api_keys` is erased only by its MAKER (`erase.go`'s one ByUser rule + `api_keys.user_id … ON DELETE CASCADE`), but since PR C a key's maker is a member, not the tenant's owner. A team's erasure leaves the keys its members made, live and resolvable for the erased tenant. A member's own erasure takes the team's keys. Fix: a ByTenant `DELETE FROM api_keys WHERE tenant_id = $1`; before the user rows go, the keys a user made on tenants it does not own are handed to that tenant's owner member (`user_id` stays NOT NULL — the previous build scans it into a string), with an audit row `key.maker_erased` on that tenant; the schema walk checks that every table with a `tenant_id` column has a ByTenant rule. **Before RB10-1's step 7** (a tenant-less sign-in's erasure would take the team keys the person made). Tests `TestErase_TheTeamsErasureTakesEveryKeyOfTheTenant`, `TestErase_AMembersErasureKeepsTheTeamsKeys`.
- **RP84-2 (P3):** `requireJWT` makes a JWT on NO tenant an owner (the dashboard got "viewer" in PR C; the API did not). Proven: `POST /api/v1/user/apikeys` answered 201 and a secret on tenant `''` (S3 refuses the row, so no data opens). Fix: role `""` on `""` for a non-operator JWT; `memberRole()`'s default `""`; **the accept route leaves `requireMember`** (a tenant-less invitee accepts only through this default today); `GenerateAPIKeyForTenant` / `GenerateTestKeyForTenant` refuse `""`; `RegisterInvitedUser` caches no tenant and `cacheUser` takes the row's tenant as read; the login's no-membership branch mints on no tenant (RP84-6). Test `TestMembers_ATenantlessSignInIsNoOwner`.
- **RP84-3 (P3, a launch gate):** the invitation subject carries `tenants.name` raw into the SMTP DATA (`smtp.go:89`), so CR/LF in a registration's company injects headers into a DKIM-signed `stored.cloud` mail to any address; and nothing bounds invitations (100 management requests a minute) against the Cloudflare sending quota every reset and receipt shares. Fix: the sender refuses CR/LF in every header and RFC 2047-encodes the subject (overlaps WP-R14-3; do it once here); registration strips control characters from the name; ≤ 20 pending invitations per tenant and ≤ 50 invitation mails per tenant per day (429 `invitation_rate_limited`, counted from rows), on the API and the dashboard. Tests `TestEmail_HeaderValuesRefuseCRLF`, `TestRegister_TenantNameHasNoControlCharacters`, `TestMembers_InvitationsAreCapped`.
- **RP84-4 (P3):** a removed (or demoted) member keeps every S3 key it made, and nothing says so. Fix: the removal answers `keys_made` and takes `revoke_keys: true` (revoked with their STS tokens in the same transaction, one audit row); the dashboard's confirmation lists them, revoke ticked. Test `TestMembers_RemovalNamesTheKeysTheMemberMade`.
- **RP84-5 (P4):** one pending invitation per (tenant, address) — a new one replaces it (a partial unique index in migration 120); the accept answers the member's actual role; compare `lower(u.email)`.
- **RP85-1 (P4):** the group's idempotency middleware runs before each route's `requireRole`: a viewer presenting a developer's key gets its cached answer. Fix: store the key as `<acting user id>:<key>`. Test `TestIdempotency_KeyedByTheActingMember`.
- **RP85-2 (P4):** a replay sends `Idempotent-Replayed: true`, and `secret_redacted: true` where a secret was blanked.

Then RP82's three for this WP:
- **RP82-3 (P3):** a denormalised `pool_used_bytes` on the root row, moved in the member's statement. The reconcile checks it equals the sum; a mismatch is a metric (`vaultaire_pool_used_drift_bytes`), never a silent fix.
- **RP82-4 (P3):** flag inheritance, decided once in the flags service. Privacy and safety flags (`standard_sealed_default`, any future sealing default) inherit down the tree and may only be tightened. Performance flags inherit as defaults. `TestFlags_ChildInheritsSealingDefault`.
- **RP82-7 (P4):** every admin total and list gets a kind filter (live / sandbox / child).

Read first:
- plan § WP-STRIPE (the WP-ACCT-1 row: the contract) and `docs/ACCOUNTS.md` (the edges table, the rules for every edge);
- `internal/usage/pool.go` + `egress.go` (`PoolRoot`) + `floor.go` + `ledger_tx.go` (the lock order);
- `internal/auth/platform.go` (`CreateChildTenant`), `internal/api/deletion_runner.go` (stage b5, the cascade), `isTenantSuspended`;
- the region gate and bucket create (`internal/api/region_gate.go`, `bucket_create.go`), the mirror legs (`blob_copies*.go`), the parity leg (`vault_parity.go`, `VAULT_PARITY_LEGS`);
- `internal/flags` (per-tenant rows).

Lane: Engine + Developer surface. Migration 120 (119 is the engine lane's Prompt 52E, #1064): `tenants.child_kind` (`managed` / `app` / `sponsored`; today's children backfilled `managed`), `tenants.pool_root_id` (backfilled `COALESCE(parent_tenant_id, id)`, NOT NULL after the backfill), `tenants.residency` (`eu` / `uk` / `us`, NULL = any), `tenant_quotas.pool_used_bytes` (RP82-3), `tenant_quotas.egress_cap_bytes` (a child's egress cap), and RP84-5's partial unique index `tenant_invitations (tenant_id, lower(email)) WHERE accepted_at IS NULL`.

PRs:
- (0) RP84's fixes + RP82-3/4/7;
- (A) the tree: the columns, `pool_root_id` behind the one resolver, depth 2 only under an `app`, caps inside the pool (a child's `storage_limit_bytes` > 0 and `egress_cap_bytes` are min(cap, pool));
- (B) residency: set at creation, never changed, inherited and narrowable; enforced at bucket create, on the mirror leg and the parity leg (closes RP76-1); an existing tenant sets it only when every bucket complies;
- (C) the edges' rules: suspension flows down `managed`/`app` only; stage b5 cascades down `managed`/`app` and DETACHES `sponsored` (no sponsored edge exists yet; the branch and its test come first); RB10-1;
- (D) docs.

Tests (red first):
- `TestTree_PoolAtDepthTwo`;
- `TestTree_ChildCapInsideThePool`;
- `TestTree_PoolRootIsTheOneResolver` (a structure test: no `COALESCE(parent_tenant_id` outside `usage`);
- `TestResidency_BucketOutsideRefused`;
- `TestResidency_MirrorAndParityLegsStayInside`;
- `TestResidency_ChildNarrowsNeverWidens`;
- `TestTree_SponsoredNeverSuspendedOrErasedWithTheSponsor`;
- `TestMembers_ALeaveRemovesTheMembershipOnly`;
- `TestFlags_ChildInheritsSealingDefault`;
- `TestPool_UsedCounterEqualsTheSum`.

Prod proof: tenant zero's proof child re-read through `pool_root_id`; an EU child of tenant zero refuses a us-central-1 bucket.

Closeout: `docs/reviews/WP-STRIPE-5.md`, STATUS, the plan row, CLAUDE.md, `docs/API.md` § Platform (caps, residency), the ledger row; Prompt 54 (WP-KEY-1, D-STRIPE-29's management key first) written here. The post-merge review is RP86.

## Stage 1a Prompt 53 — Opus edition

**From 52:** see the chain edition's "From 52" block above.

You are building plan § WP-STRIPE's WP-ACCT-1 row in the Vaultaire repository: the account tree of `docs/ACCOUNTS.md`, built once for every account kind after it. Read the Opus rules in `docs/reviews/WP-EDGE-PROMPTS.md` § "About the Opus editions". Worktree from `origin/main`. Migration 120.

Read before editing: the chain edition's "Read first" list, `docs/reviews/WP-STRIPE-4.md` and its review `WP-STRIPE-4-REVIEW.md` (RP84).

PR 0 — Branch `stage1a-p53-part0`.
- **0a (RP84 + RP85, `docs/reviews/WP-STRIPE-4-REVIEW.md`, `STAGE1-CLOSEOUT-REVIEW.md`):** the findings in the review's order, each test first. If those files are not on main when you start, STOP and say so: never guess a review. They are latent on prod (no invitation has been sent), so ship 0a before the owner's WP-TEAM-1 prod proof.
  1. **RP84-1 (P2), the key's erasure.** Tests first:
     - `TestErase_TheTeamsErasureTakesEveryKeyOfTheTenant`: a developer's key on tenant A is gone after A's `EraseRows`, and `LookupCredential` answers `ErrUnknownAccessKey`;
     - `TestErase_AMembersErasureKeepsTheTeamsKeys`: the member's own erasure leaves the key resolving, now with A's owner as its user, plus an audit row `key.maker_erased` on A.

     Then in `internal/account/erase.go`:
     - add a ByTenant `api_keys` rule;
     - add a ByUser `UPDATE api_keys SET user_id = <owner member of the key's tenant> WHERE user_id = $1 AND tenant_id NOT IN (<tenants the user owns>)`, ordered before the `users` row (the FK cascades);
     - keep `user_id` NOT NULL: the previous build's boot load scans it into a string, and a NULL aborts it;
     - extend the schema walk: a table with a `tenant_id` column must have a ByTenant rule.

     Verify: `go test -race ./internal/account/ ./internal/api/ -run 'Erase|Members'`.
  2. **RP84-2 (P3), no role on no tenant.** Test first: `TestMembers_ATenantlessSignInIsNoOwner`. A JWT minted on `""` gets 403 `not_a_member` on `POST /api/v1/user/apikeys`, `POST /api/v1/manage/keys` and `DELETE /api/v1/user`; `POST /api/v1/manage/invitations/{token}/accept` still answers 200. Then:
     - `requireJWT` gives `""` for a non-operator JWT on `""`, and `memberRole()`'s default is `""`. Hand-mounted tests set the role explicitly;
     - the accept route is mounted under `requireJWT` only;
     - the two key generators refuse `""`;
     - `RegisterInvitedUser` caches no `TenantID`, and `cacheUser` takes the row's tenant as read;
     - `/auth/login`'s no-membership branch mints on no tenant.
  3. **RP84-3 (P3), mail.** Tests first: `TestEmail_HeaderValuesRefuseCRLF` (`buildMIMEMessage` and the Resend body), `TestRegister_TenantNameHasNoControlCharacters`, `TestMembers_InvitationsAreCapped` (20 pending per tenant, 50 mails per tenant per day, 429 `invitation_rate_limited`, counted from `tenant_invitations` rows so both slots agree). Then the guard in the sender, `mime.QEncoding` for the subject, the registration's name, and the caps on the API and the dashboard.
  4. **RP84-4 (P3), removal.** Test first: `TestMembers_RemovalNamesTheKeysTheMemberMade` (`keys_made` in the answer; `revoke_keys: true` revokes them and their STS tokens in the removal's transaction). Then the dashboard's confirmation lists them, revoke ticked by default.
  5. **RP84-5 (P4):** migration 120 gets a partial unique index on `tenant_invitations (tenant_id, lower(email)) WHERE accepted_at IS NULL`; `CreateInvitation` replaces the pending row; the accept answers the member's real role.
  6. **RP85-1 / RP85-2 (P4):** `TestIdempotency_KeyedByTheActingMember` (the stored key is `<acting user id>:<key>`); a replay sends `Idempotent-Replayed: true`, and `secret_redacted: true` where a secret was blanked.
- **0b (RP82-3).** Test first: `TestPool_UsedCounterEqualsTheSum` (two children write and delete; the root's `pool_used_bytes` equals the sum of the pool's `storage_used_bytes` after every step). Then:
  - migration 120 adds `tenant_quotas.pool_used_bytes BIGINT NOT NULL DEFAULT 0`;
  - `ReserveTx` and `ApplyLedgerTx` move it on the root's row in the statement that moves the member's (the root's row is already locked first);
  - the pool check reads it instead of `poolUsed`'s SUM;
  - the reconcile compares it with the sum and exports the difference as `vaultaire_pool_used_drift_bytes{root}`: never a silent rewrite.

  Verify: `go test -race ./internal/usage/ -run 'Pool|Ledger'`.
- **0c (RP82-4).** Test first: `TestFlags_ChildInheritsSealingDefault` (`standard_sealed_default` ON on a platform → its child reads ON; a child row OFF under a parent ON is refused 409 `flag_only_tightens`). Then `flags.Service.Enabled` walks the tree for flags registered `Inherit` (the sealing default and any flag named privacy/safety in its registration); performance flags inherit as a default the child may override. Verify: `go test -race ./internal/flags/ ./internal/api/ -run 'Flag|Inherit'`.
- **0d (RP82-7).** Admin totals and lists take `kind=live|sandbox|child` (default live). `TestAdminTenants_KindFilter`.

PR A — the tree. Branch `stage1a-p53-tree`.
1. Migration 120 (one file with 0b's column): `tenants.child_kind TEXT CHECK (child_kind IN ('managed','app','sponsored'))` (backfill `managed` where `parent_tenant_id IS NOT NULL`); `tenants.pool_root_id VARCHAR(255) REFERENCES tenants(id)` (backfill `COALESCE(parent_tenant_id, id)`; an index); `tenant_quotas.egress_cap_bytes BIGINT NOT NULL DEFAULT 0`. Then `make test-db`, the reapply test, the schema walks.
2. Test first: `TestTree_PoolRootIsTheOneResolver` (walks the repository: `COALESCE(parent_tenant_id` appears only in `usage.PoolRoot`'s migration-era comment). Then `PoolRoot`, `lockPoolOwner`, `poolUsed`, `lockPoolFloor`, `poolFloorUsed` read `pool_root_id`; `CreateChildTenant` writes it (the parent's root).
3. Test first: `TestTree_PoolAtDepthTwo` (an `app` child's `managed` child reserves against the root; a `managed` child's child is refused 400 `child_of_child`). Then `CreateChildTenant(…, kind)`: depth 2 only under an `app`.
4. Test first: `TestTree_ChildCapInsideThePool` (a child capped at 10 KiB in a 1 MiB pool is refused at 10 KiB + 1; its egress cap paces it when the pool has room). Then `ReserveTx` checks min(child cap, pool) and the egress meter's decision checks the child's cap beside the pool's position.

PR B — residency. Branch `stage1a-p53-residency`.
5. Test first: `TestResidency_BucketOutsideRefused`, `TestResidency_ChildNarrowsNeverWidens`, `TestResidency_MirrorAndParityLegsStayInside`. Then:
   - `tenants.residency` (the migration: `eu` / `uk` / `us`, NULL = any);
   - set at creation (`POST /api/v1/platform/tenants {residency}`, registration none), never changed except by an admin while every bucket complies (409 `buckets_outside_residency`);
   - a bucket create outside it is 400 `InvalidLocationConstraint` (S3) / `region_outside_residency` (management), and the default region is the first inside it;
   - the mirror leg and the parity leg choose only legs inside it (each driver names its jurisdiction; a leg without one is outside every residency).

PR C — the edges' rules. Branch `stage1a-p53-edges`.
6. Test first: `TestTree_SponsoredNeverSuspendedOrErasedWithTheSponsor` (a `sponsored` child row made by SQL: the sponsor's suspension leaves it active; the sponsor's erasure DETACHES it — `parent_tenant_id`, `pool_root_id` reset to itself, `child_kind` NULL, its data untouched). Then `isTenantSuspended` and stage b5 branch on `child_kind`.
7. Test first: `TestMembers_ALeaveRemovesTheMembershipOnly` (a viewer `DELETE /api/v1/manage/members/{self}` → 200, its sign-in and the tenant intact; the owner cannot leave, 409). Then the route allows self-removal for any member but the owner. A sign-in with no tenant and no membership left may be erased: `account.Schedule` on a user with no owned tenant erases the user rows only (`TestAccount_TenantlessSignInErasesTheUserOnly`).

PR D — docs: `docs/reviews/WP-STRIPE-5.md`, STATUS, the plan row DONE, CLAUDE.md (the tree, `pool_root_id`, residency), `docs/API.md` § Platform, the ledger row; Prompt 54 (WP-KEY-1: the management key of D-STRIPE-29 first, then the budgets of F-39) in both editions with Part 0 = RP86. The post-merge review is RP86.

Prod: migration 120 first (additive; `pool_root_id` backfilled before any reader). Then the proof: tenant zero's proof child read through `pool_root_id`, and an EU child of tenant zero refused a us-central-1 bucket.

Done means: PRs 0, A, B, C, D merged; the proof on prod; Prompt 54 written here.

## Stage 1a Prompt 54: WP-KEY-1 — management keys first, then per-key and per-bucket budgets (Part 0: RP86's findings)

**From 53 (`docs/reviews/WP-STRIPE-5.md`; the post-merge review is RP86):**
- **The tree is live and dark.** Migration 120:
  - `tenants.child_kind`, `pool_root_id` (the one resolver is `internal/usage`; `TestTree_PoolRootIsTheOneResolver`) and `residency`;
  - `tenant_quotas.pool_used_bytes` (moved by `usage.moveUsed`; drift = `vaultaire_pool_used_drift_bytes`) and `egress_cap_bytes`.
- **A platform reaches its whole tree** through `auth.DescendantT1…3`: a `managed` or `app` child, and an app's `managed` children at depth 2, never a `sponsored` child. Depth 2 is made by the platform (`parent_id` = the app) because **an app child has no credential of its own to call the API with**. That is this WP.
- **Caps inside the pool exist for a CHILD** (`storage_limit_bytes` > 0, `egress_cap_bytes`, `PATCH /api/v1/platform/tenants/{id}`). WP-KEY-1's budgets are the same leash one level finer, per key and per bucket. They reuse `poolOf.fits` and `egress_member.go`'s per-member bucket rather than inventing a third shape.
- **Residency is a tenant attribute** (eu/uk/us), copied to a child at creation.

**Part 0:** RP86's findings (`docs/reviews/WP-STRIPE-5-REVIEW.md`: HOLDS at P3, all latent on prod), in this order:
- **RP86-1 (P3):** the GDPR export ignores residency. `ensureSystemBucket` makes `_exports` in the default region (an EU tenant's export sits in `us-central-1` for 7 days), and `treeBucketRegions` counts that hidden row, so after one export an admin can never set a residency (409 "move or delete it first", for a bucket the customer cannot see). Proven by test. Fix: the system bucket is created in the first region inside the residency (none inside = the export fails with a note, never written outside); a `_`-prefixed system bucket with no live object never blocks the set and moves inside in the set's transaction. Tests `TestResidency_TheExportStaysInside`, `TestResidency_TheSystemBucketNeverBlocksTheSet`.
- **RP86-2 (P4):** `PUT /api/v1/manage/buckets/{name}/residency` writes a free `us|eu` label whatever the region (no `uk`). The label is the region's (`drivers.DataResidency`); a mismatch is 409 `residency_is_the_region`. Test `TestBucketResidencyLabelIsTheRegions`.
- **RP86-3 (P4):** the boot `BackfillBuckets` recreates a bucket row with no region. Backfill the head row's backend's region, or skip and log under a residency.
- **RP86-4 (P4):** a tenant-less sign-in that rejoins a team during its grace is still erased on the date, its membership with it. The runner re-checks under the user's lock and clears the schedule (`account.deletion_cancelled`, `became_member`). Test `TestAccount_ASignInThatRejoinedIsNotErased`.
- **RP86-5 (P4):** CI runs the pinned promtool over `deploy/monitoring/tests/*_test.yml` (the jobs test had been failing unseen since #981; fixed by the review).

**And three corrections to PR A** (RP86, read against the code):
- `audit_logs.user_id` / `performed_by` are UUID and `audit.Record` writes a non-UUID actor as NULL, so `mk:<id>` would audit as "the system did it". Migration 121 adds `audit_logs.principal TEXT` (and the archive's), and the audit page and its CSV show it.
- `management_keys.created_by` has no `ON DELETE CASCADE` from `users`. The erasure takes the table ByTenant and hands over (or nulls) `created_by` ByUser. The member removal's `keys_made` includes management keys, under the same `revoke_keys`.
- A management key never reaches the member routes (invite, role change, removal), besides the owner's acts the draft already keeps JWT-only.

WP-STRIPE-5's open question (`standard_sealed_default` per tenant) stays the owner's. The review's advice is to keep it global until F-14 (turning it on is refused without a master key); apply a change only if the owner has decided by then.

Read first:
- plan § WP-STRIPE, the WP-KEY-1 row (the contract);
- D-STRIPE-29 in `docs/reviews/WP-STRIPE-3.md` (the management key: opaque `sk_live_…` / `sk_test_…`, sha256-stored, tied to the tenant, scopes `manage:read` / `manage:write` / `platform` / `apps`, an S3 ceiling for what it mints — RP82-6: its own field, never its bucket scope);
- `docs/ACCOUNTS.md` (an app's backend and an agent grant are management keys);
- `internal/api/server.go` `requireJWT`, `member_roles.go`, `auth.CapScope`;
- `internal/auth/platform_keys.go`, `internal/usage/pool.go` (`fits`), `internal/api/egress_member.go`;
- `internal/api/edge_ingest.go` (the admit), `internal/api/s3_credential_gate` (Architecture Decision 8: a Bearer is not an AWS scheme, so no gate change).

Lane: Developer surface. Migration 121.

PRs:
- (0) RP86's fixes;
- (A) **the management key:**
  - `management_keys` (tenant, `key_hash`, prefix, scopes, `s3_ceiling` JSONB, `created_by`, `last_used_at`, `revoked_at`, livemode);
  - `requireCaller` = `requireJWT` or a management key. A key has no member and no role: its scopes are its authority, checked by the same `requireRole` through `auth.Can`'s scope column;
  - mint / list / revoke on `/api/v1/manage/management-keys` (an admin's), the secret shown once;
  - the platform routes accept it, and so does a management key **of an app child**, scoped to its subtree (the app's backend makes its users);
- (B) **budgets (F-39):**
  - `api_keys.storage_limit_bytes`, `egress_limit_bytes`, `buckets.storage_limit_bytes`, `egress_limit_bytes` (nullable = inherit);
  - the reservation checks min(key, bucket, tenant, pool), the egress meter the same, and a refusal names the cap (`x-stored-cap: key|bucket|tenant|pool`);
  - STS narrows only, and the edge admit enforces at presign;
- (C) the dashboard (the keys page sets caps, the management keys page);
- (D) docs.

Tests (red first):
- `TestManagementKey_ShownOnceStoredAsHash`;
- `TestManagementKey_ScopesAreItsAuthority` (a `manage:read` key is 403 on a write);
- `TestManagementKey_ReachesThePlatformRoutes`;
- `TestManagementKey_AnAppsKeyReachesOnlyItsSubtree`;
- `TestManagementKey_RevokedIsRefusedAtOnce`;
- `TestManagementKey_CeilingIsItsOwnField` (RP82-6);
- `TestKeyCap_PutRefusedPastTheKeysStorageCap`;
- `TestKeyCap_EgressCapPacesThenRefusesAtTheKey`;
- `TestKeyCap_STSNarrowsNeverWidens`;
- `TestKeyCap_EdgeAdmitEnforcesIt`;
- `TestKeyCap_403NamesTheCap`.

Prod proof:
- tenant zero mints a management key (the owner's TOTP once), and its server-side use makes a child and that child's key with no person in the loop;
- a scoped key capped at 100 MiB refuses the 101st MiB through stored.ge and through the Worker alike.

Closeout: `docs/reviews/WP-STRIPE-6.md`, STATUS, the plan row, CLAUDE.md, `docs/API.md` § Management keys + § Keys, the ledger row; Prompt 55 (WP-AGENT-1 + WP-APP-1) written here. The post-merge review is RP88.

## Stage 1a Prompt 54 — Opus edition

**From 53:** see the chain edition's "From 53" block above.

You are building plan § WP-STRIPE's WP-KEY-1 row in the Vaultaire repository: first the management key of D-STRIPE-29, then the per-key and per-bucket budgets of F-39. Read the Opus rules in `docs/reviews/WP-EDGE-PROMPTS.md` § "About the Opus editions". Worktree from `origin/main`, a private migrated test database, migration 121.

Read before editing: the chain edition's "Read first" list, `docs/reviews/WP-STRIPE-5.md`, and its review `docs/reviews/WP-STRIPE-5-REVIEW.md` (RP86). If that file is not on main when you start, STOP and say so: never guess a review.

PR 0 — Branch `stage1a-p54-part0`. RP86's findings in the review's order (`docs/reviews/WP-STRIPE-5-REVIEW.md`), each test first:
1. **RP86-1 (P3).** Tests first: `TestResidency_TheExportStaysInside` (an EU tenant's export lands in an EU region; with no enabled region inside, the run's note is `no_region_inside` and nothing is written), `TestResidency_TheSystemBucketNeverBlocksTheSet` (a tenant with an empty `_exports` takes a residency and the row moves inside in the same transaction; a live export object is 409 until it expires). Then:
   - `ensureSystemBucket` reads `tenants.residency` and uses `firstRegionInside`;
   - `treeBucketRegions` / `setTenantResidency` treat `_`-prefixed system buckets by those rules.

   Verify: `go test -race ./internal/api/ -run 'Residency|Export'`.
2. **RP86-2 (P4).** Test first: `TestBucketResidencyLabelIsTheRegions`. Then `handleMgmtSetBucketResidency` accepts only the region's own label (409 `residency_is_the_region` otherwise; `uk` known); `docs/API.md` marks the route deprecated.
3. **RP86-3 (P4).** `BackfillBuckets` sets the region from the head row's recorded backend (`idrive-<region>` → that region; the primary → the default), or skips a tenant with a residency with an Error line. A test on a head row with no bucket row.
4. **RP86-4 (P4).** Test first: `TestAccount_ASignInThatRejoinedIsNotErased`. The deletion runner's tenant-less branch re-checks membership under the user's lock; a member again → the schedule cleared, `account.deletion_cancelled` with `reason: became_member`.
5. **RP86-5 (P4).** A CI step (in `ci.yml`, after Lint) downloads a pinned promtool (checksum-verified) and runs `promtool test rules` over every `deploy/monitoring/tests/*_test.yml`, each beside its rule file (promtool resolves `rule_files` from the test's directory).

PR A also takes RP86's three corrections:
- **`audit_logs.principal TEXT`** (and the archive's) in migration 121, written by `audit.Record` from the context (`mk:<id>` for a key, the user id for a person). `TestManagementKey_AuditNamesThePrincipal`.
- **`created_by` without a cascade:** the erasure is ByTenant plus a ByUser handover/NULL. `TestErase_AMembersErasureKeepsThePlatformsManagementKey`. The removal's `keys_made` includes management keys.
- **No member route for a key:** the invite, role and removal routes refuse a management key (403 `person_required`). `TestManagementKey_NeverManagesMembers`.

PR A — the management key. Branch `stage1a-p54-mgmt-key`.
1. **Migration 121.** One file for the WP, landed with PR A (WP-STRIPE-5's D-STRIPE-43 rule):
   - `management_keys` (`id`, `tenant_id`, `prefix` (`sk_live_` | `sk_test_`), `key_hash` (sha256 hex, unique), `name`, `scopes text[]`, `s3_ceiling jsonb` (permissions, bucket scope, IP list, longest expiry), `created_by` uuid NULL, `created_at`, `last_used_at`, `revoked_at`);
   - the four nullable cap columns of PR B on `api_keys` and `buckets`.

   Then the erasure: a ByTenant rule, the schema walk (`TestErasedTablesCoverSchema`, the tenant-rule check) and the export section (never the hash). Verify with `make test-db` and the reapply test.
2. Test first: `TestManagementKey_ShownOnceStoredAsHash`, `TestManagementKey_RevokedIsRefusedAtOnce`. Then `internal/auth/management_keys.go`: mint (32 random bytes, base62, prefix by the tenant's livemode), lookup by sha256 under `revoked_at IS NULL` on every request (no cache, as the role), revoke, and `last_used_at` coalesced to once a minute.
3. Test first: `TestManagementKey_ScopesAreItsAuthority`, `TestManagementKey_ReachesThePlatformRoutes`. Then `requireCaller`:
   - a Bearer that starts with `sk_live_` / `sk_test_` is a management key;
   - anything else is a JWT, as today;
   - it puts the tenant, a principal id (`mk:<id>`, for the request log, the audit row's actor and the idempotency key's prefix) and a ROLE derived from its scopes on the context: `manage:write` → developer, `manage:read` → viewer, `platform` → admin on `/api/v1/platform` only, `apps` → see step 4;
   - `requireMember` passes a management key (it is the tenant's own).

   Mount it on `/api/v1/manage` and `/api/v1/platform`. Leave `/api/v1/user`, the account deletion, the export and billing JWT only: an owner's acts need a person.
4. Test first: `TestManagementKey_AnAppsKeyReachesOnlyItsSubtree`. A management key of an `app` child (minted by its platform: `POST /api/v1/platform/tenants/{app}/management-keys`) acts on `/api/v1/platform` as the app. Its tree is the app's managed children only (DescendantT with the app as the platform). Depth 2 stays the deepest.
5. Test first: `TestManagementKey_CeilingIsItsOwnField` (RP82-6). `auth.CapScope` reads the management key's `s3_ceiling`, never a bucket scope of its own.

PR B — budgets. Branch `stage1a-p54-budgets`.
6. Test first: `TestKeyCap_PutRefusedPastTheKeysStorageCap`, `TestKeyCap_403NamesTheCap`. A key's storage cap counts the bytes the key's writes hold. Use `object_head_cache.written_by_key` if it exists; otherwise this step is a STOP — write it into open questions: a per-key storage count needs a column the migration must add, and the review decides.
   - The reservation checks min(key, bucket, tenant, pool) through `poolOf.fits` extended with the two caps;
   - a refusal is 403 `QuotaExceeded` with `x-stored-cap`.
7. Test first: `TestKeyCap_EgressCapPacesThenRefusesAtTheKey`. Use the per-member bucket of `egress_member.go`, keyed by key id, for an egress cap. F-39 says "pace first", so a key past its cap is paced. Refuse only past 2 × the cap.
8. Test first: `TestKeyCap_STSNarrowsNeverWidens`, `TestKeyCap_EdgeAdmitEnforcesIt`. An STS token inherits its key's caps and may only lower them. The edge admit runs the same reservation.

PR C — the dashboard: the keys page sets a key's caps, and a management keys page (mint, show once, revoke). Both are behind `dashboardAction`; the router walks stay green.

PR D — docs: `docs/reviews/WP-STRIPE-6.md`, STATUS, the plan row DONE, CLAUDE.md, `docs/API.md` § Management keys + § Keys, the ledger row; Prompt 55 (WP-AGENT-1 + WP-APP-1) in both editions with Part 0 = RP88.

Prod:
- migration 121 first (additive);
- then the proof: tenant zero mints a management key (the owner's TOTP once) and, from a shell with no person's token, makes a child and its key; a capped scoped key refuses its 101st MiB through stored.ge and through the Worker.

Done means:
- PRs 0, A, B, C and D are merged;
- the proof has run on prod;
- Prompt 55 is written here.

## Stage 1a Prompt 54 — addendum after RP88's pre-merge pass (2026-10-31)

Read `docs/reviews/WP-STRIPE-6-REVIEW.md` § Part 1 first. PR A (#1093) holds at P4; take these before it merges, then PR B as decided there.

**Rebase on main first:** #1094 (RP88-0) fixes the `internal/payments` calendar failure that made #1093's CI red (not yours: Go's `AddDate` beside Postgres's `+ interval`, red on the 29th–31st).

PR A, before merge (tests first, as always):
1. **RP88-1:** `"test": true` on the mint body mints the key ON the tenant's sandbox (`auth.SandboxOf`, made on first use) — the one way to an `sk_test_` key; a sandbox reset or purge never deletes a key. `TestManagementKey_TestKeyActsOnTheSandbox`.
2. **RP88-2:** `expires_at TIMESTAMPTZ` in 121; the lookup refuses an expired key; the mint body takes an optional `expires_at`. `TestManagementKey_ExpiredIsRefused`.
3. **RP88-3:** `FOR UPDATE` on the tenant row around the live-key count.
4. **D-RP88-2's columns in 121, now:** `object_head_cache.written_by_key UUID`, `object_versions.written_by_key UUID`, `multipart_uploads.written_by_key UUID`, `api_keys.storage_used_bytes BIGINT NOT NULL DEFAULT 0`, `buckets.storage_used_bytes BIGINT` (NULL = not yet counted). Columns only; PR B writes them. The reapply test and `make test-db` as before.

PR B (`stage1a-p54-budgets`), in this order:
5. **D-RP88-1 first:** `KeyScope.KeyID` filled by `auth.Auth.LookupCredential` on every scope it answers (an STS token: its parent key's id); `ValidateRequest` and `ValidateDescribed` carry it unchanged; a management key's ceiling has none. `TestKeyScope_NamesItsKey` (a scoped key, the primary, an STS token, a management key's ceiling).
6. **Attribution:** every row writer sets `written_by_key` from the context's `KeyID` (`commitBlobWrite`, the version write, `CreateMultipartUpload`; the edge admit stores it in `edge_admits.attrs` and the commit writes it). `TestKeyCap_RowsNameTheirWriter`.
7. **The ledger:** `internal/usage` moves `api_keys.storage_used_bytes` and `buckets.storage_used_bytes` in the statement that moves the tenant's (+ on a landed write, − on the displaced or deleted row's key and bucket, the part reservation on the upload's key); the daily reconcile seeds a NULL bucket counter and re-asserts both, with a drift gauge each. `fits` takes min(key, bucket, tenant, pool); the refusal is 403 `QuotaExceeded` + `x-stored-cap` naming the first cap that failed. The prompt's tests `TestKeyCap_PutRefusedPastTheKeysStorageCap`, `TestKeyCap_403NamesTheCap`, plus `TestKeyCap_DeleteByAnotherKeyReleasesTheWriters` and `TestKeyCap_BucketCapAboveTheTenantsIsANote` (F-39 (b)).
8. Steps 7 and 8 of the Opus edition as written (the egress cap per key on `egress_member.go`'s bucket keyed by `KeyID`; STS narrows; the edge admit runs the same reservation). F-39 (a) stays the owner's: build pace-then-refuse-at-2× behind one constant. **Superseded 2026-10-31: the owner decided F-39** — an egress cap on a key or a bucket refuses when spent (no pacing, no 2×), the minimum always applies, the primary is never capped (`docs/reviews/WP-STRIPE-6.md` D-STRIPE-61; the test is `TestKeyCap_EgressCapRefusesAtTheKey`).

PR C and PR D as written. The post-merge review stays RP88 (Part 2 of the same file).

## Stage 1a Prompt 55: WP-AGENT-1 + WP-APP-1 — AI agents as principals, then AI-created apps (Part 0: RP88 Part 2's findings)

**From 54 (`docs/reviews/WP-STRIPE-6.md`; the post-merge review is RP88 Part 2):**
- **The management key is live and dark** (migration 121, `management_keys`): a TENANT's bearer key, `sk_live_…` or (with `"test": true`, on the sandbox) `sk_test_…`, sha256-stored, shown once, an optional `expires_at`, looked up on every request. Its scopes are its role per surface (`requireCaller`), and it never does what needs a person (`requirePerson`, 403 `person_required`). Its `s3_ceiling` bounds what it mints. An app child's key (`apps`) reaches its own subtree only.
- **An agent grant IS a management key** (`docs/ACCOUNTS.md` § Principal): this WP adds who it acts for, the consent, the approvals and the default scope. It never adds a second credential kind. The key's expiry (RP88-2) is the grant's lifetime (30 days by default).
- **Budgets are live and dark:**
  - a scoped key's and a bucket's storage cap (the bytes held, `written_by_key`) and egress cap per UTC month (refused when spent, `key.cap_warning` at 80 / 95 %);
  - min(key, bucket, tenant, pool) on every write, `x-stored-cap` on every refusal;
  - an agent grant's caps are its S3 keys' caps, and an app's cap is its tenant cap inside the pool.
- **D-STRIPE-49:** depth 2 is made by the platform (`parent_id` = the app). An app's own key (`apps`) now makes its end users itself.

**Part 0:** RP88 Part 2's findings (`docs/reviews/WP-STRIPE-6-REVIEW.md` § Part 2; the review fixed RP88-4 and RP88-5 itself), in this order, each test first:
1. **RP88-6 (P2): the egress cap refuses by the remaining bytes, not only once spent.** `egressCaps` carries each capped holder's remaining (`cap − used`); the S3 GET refuses at the response's `WriteHeader` through a wrapper outside the handler (a 200/206 whose `Content-Length` exceeds the remaining becomes the 403 `QuotaExceeded` + `x-stored-cap` + `Retry-After`, header map cleared, body dropped, nothing counted); `/cdn` the same; the edge admit `length > remaining → passThrough("capped")`. Then exactness: pre-charge the planned bytes in ONE conditional UPDATE at that moment and credit back `planned − served` after the response when they differ. `TestKeyCap_EgressCapRefusesAResponseLargerThanWhatIsLeft` (S3 whole + Range, `/cdn`, the edge admit). D-STRIPE-63 restated in WP-STRIPE-6.md and `docs/API.md` § Keys.
2. **RP88-8 (P3): a key narrows, never widens.** `PUT …/keys/{id}/caps` and `/buckets/{name}/caps` from a management key may set a cap where none is or lower one, never raise or clear it (409 `cap_widening_needs_person`); a person may do anything. `TestKeyCap_AKeyNeverWidensACap`. The agent grant of PR A then cannot lift its own budget, by construction.
3. **RP88-7 (P4):** the warning claim returns the PREVIOUS step (`UPDATE … FROM (SELECT … FOR UPDATE) o … RETURNING o.egress_warned_pct`) and the emit loop starts above it — the 80 % event fired twice when two responses claimed 80 then 95 off one read.
4. **RP88-9 (P4):** `/cdn` counts 200/206 bodies only to the bucket's cap, as `serveGetObject` does.
5. **RP88-11 (P4):** `WHERE id = $1::uuid` (not `id::text = $1`) in `holderFits`, `applyHolders`, `SetKeyCaps` and the egress count SQL — the `api_keys` primary key's index.
6. **RB12-9 → migration 122** (this prompt's one file, with PR A): partial indexes `(written_by_key) WHERE written_by_key IS NOT NULL` on `object_head_cache`, `object_versions`, `multipart_uploads`.
7. **RB12-6:** `HolderStorageDrift` (kinds `key`, `bucket`) beside `PoolUsedDrift` in `deploy/monitoring/vaultaire-quota.yml` + `tests/vaultaire-quota_test.yml` cases; install on the box at the closeout ([OPS]).
8. RB12-10 (the seed and a write in flight) stays a residual; RB12-7 (WP-KEY-1's prod proof, the owner's TOTP) runs with this prompt's.

Read first:
- plan § WP-STRIPE, the WP-AGENT-1 and WP-APP-1 rows (the contract);
- `docs/ACCOUNTS.md` (§ Principal, § Apps, § Agents);
- `docs/reviews/WP-STRIPE-6.md` and its review;
- `internal/api/management_keys.go`, `internal/auth/management_keys.go`, `internal/api/key_caps.go`, `internal/usage/holders.go`;
- the Workers MCP server's notes (40.7 / 26.8);
- `internal/api/platform_tenants.go` (`handlePlatformCreateTenant`, the app's children).

Lane: Developer surface (agents) and Consumer-adjacent (apps). Migration **122**, one file for the prompt, landed with its first PR.

PRs:
- (0) RP88 Part 2's fixes;
- (A) **agent grants** (WP-AGENT-1):
  - `agent_grants` (the management key's id, `actor = agent`, the agent's name, the granting member, `approved_scopes`);
  - the OAuth 2.1 + PKCE flow with dynamic client registration at `mcp.stored.cloud` and `stored mcp` over stdio. The consent screen shows the scope, the caps and the lifetime;
  - the result is a management key with `expires_at` = +30 days (renewable) and a test key on the sandbox to rehearse;
  - the request log and the audit name the agent and its person;
- (B) **approvals:**
  - `approvals`: a request outside the default agent scope answers 403 `approval_required` with a link (DeleteBucket, making anything Public, Object Lock bypass, minting beyond its scope, exports, the account's erasure);
  - the person grants it once in the dashboard, with TOTP for a destructive act;
  - buckets an agent creates are versioning-Enabled;
  - the optional agent workspace is a `managed` child with its own cap;
- (C) **apps** (WP-APP-1):
  - any verified, paying account makes up to 25 `app` children with no admin grant (the grant stays for third-party platforms);
  - an app's management key (`apps`) makes its end users at depth 2 and stores within the builder's pool;
  - an app is deleted as a unit (the cascade);
  - an app's content is never Public until its root is verified (line 14);
- (D) **claimable apps:**
  - a PARTNER key kind (a management key with scope `partner`, granted by an operator) creates a provisional account (1 GiB, never public, 7 days) and a claim link (`claims`);
  - claimed, it becomes an app under the claimer with its data; unclaimed, it is purged like test data. No anonymous creation;
- (E) the dashboard (Agents, Apps pages);
- (F) docs.

Tests (red first):
- `TestAgent_DefaultScopeCannotDeleteABucket`, `TestAgent_ApprovalGrantsOnce`, `TestAgent_GrantExpires`, `TestAgent_CreatedBucketsAreVersioned`, `TestAgent_RequestLogNamesTheAgent`, `TestAgent_NeverReadsPrivate`;
- `TestApps_BuilderCreatesWithoutAGrant`, `TestApps_EndUsersAtDepthTwoDrawFromTheBuildersPool`, `TestApps_DeleteCascades`, `TestApps_NeverPublicUntilVerified`;
- `TestClaimable_PartnerOnlyAndExpires`, `TestClaimable_ClaimMovesTheDataUnderTheClaimer`.

Prod proof:
- Claude connects to tenant zero over MCP, writes an object, and a DeleteBucket asks for approval, which the owner grants;
- tenant zero makes an app, whose backend key makes an end user and stores an object;
- a partner-keyed claimable is claimed by tenant zero.

Also owed: WP-KEY-1's own proof (RB12-7, the owner's TOTP).

Closeout: `docs/reviews/WP-STRIPE-7.md`, STATUS, both plan rows, CLAUDE.md, `docs/API.md` § Agents + § Apps, `llms.txt`, the ledger row; Prompt 56 (WP-FAMILY-1) written here. The post-merge review is RP90.

## Stage 1a Prompt 55 — Opus edition

**From 54:** see the chain edition's "From 54" block above.

You are building plan § WP-STRIPE's WP-AGENT-1 and WP-APP-1 rows in the Vaultaire repository: AI agents as principals (an agent grant is a management key with a person behind it, a default scope and approvals), then AI-created apps (an `app` child any paying account makes, and the claimable apps of AI app builders). Read the Opus rules in `docs/reviews/WP-EDGE-PROMPTS.md` § "About the Opus editions". Worktree from `origin/main`, a private migrated test database (`make test-db TEST_DB=vaultaire_test_p55`), migration 122.

Read before editing: the chain edition's "Read first" list, `docs/reviews/WP-STRIPE-6.md`, and its review `docs/reviews/WP-STRIPE-6-REVIEW.md` § Part 2 (RP88). If Part 2 is not on main when you start, STOP and say so: never guess a review.

PR 0 — Branch `stage1a-p55-part0`. RP88 Part 2's findings (`docs/reviews/WP-STRIPE-6-REVIEW.md` § Part 2) in the chain edition's Part 0 order, each test first — the eight items as written there: RP88-6 (the egress cap refuses by the remaining bytes at `WriteHeader`, at `/cdn` and at the edge admit's `length`, then the one-UPDATE pre-charge), RP88-8 (a management key sets or lowers a cap, never raises or clears one — 409 `cap_widening_needs_person`), RP88-7, RP88-9, RP88-11, RB12-9's three partial indexes (they go into migration 122, which PR A lands whole: write 122 in PR 0 and let PR A extend the same file before it merges), RB12-6's `HolderStorageDrift` rule with its promtool cases. Nothing else in PR 0.

PR A — agent grants. Branch `stage1a-p55-agents`.
1. **Migration 122.** One file for the prompt, landed with this PR (D-STRIPE-43's rule):
   - `agent_grants`: `id`, `tenant_id`, `management_key_id` (the grant's key), `agent_name`, `client_id` (the OAuth client), `granted_by` (uuid, no foreign key — the RP84-1 lesson), `approved_scopes text[]`, `created_at`, `renewed_at`;
   - `approvals`: `id`, `tenant_id`, `grant_id`, `action`, `resource`, `requested_at`, `decided_at`, `decided_by`, `decision`, `used_at`;
   - `claims` and the `partner` scope, for PR D;
   - the erasure rules and the export sections in the same PR.
2. Test first: `TestAgent_GrantExpires`, `TestAgent_RequestLogNamesTheAgent`. Then the grant:
   - a management key (`auth.MintManagementKey`) with `expires_at` = +30 days and a test key on the sandbox (`"test": true`, RP88-1);
   - renewal pushes `expires_at` and never widens the scope;
   - the request log's key column is `mk:<id>`, and the audit's principal is `agent:<grant>` on behalf of the granting member.
3. The connect flow: OAuth 2.1 + PKCE with dynamic client registration, the MCP authorization flow.
   - On the Worker `mcp.stored.cloud` (`edge/mcp/`, new) and in `stored mcp` over stdio.
   - The consent page is the dashboard's, a person's session with TOTP, and shows the scope, the caps and the lifetime.
   - The code is exchanged once.
   - Test `TestAgent_ConsentMintsOneGrant`.

PR B — approvals. Branch `stage1a-p55-approvals`.
4. Test first: `TestAgent_DefaultScopeCannotDeleteABucket`, `TestAgent_ApprovalGrantsOnce`. A grant's key reaching an action outside the default agent scope answers 403 `approval_required` with `approval_url`. The person approves in the dashboard (TOTP for a destructive act). The approval is single-use (`used_at`), names one action on one resource, and expires in 24 h.
5. Test first: `TestAgent_CreatedBucketsAreVersioned`, `TestAgent_NeverReadsPrivate`. A bucket an agent creates is versioning-Enabled. A grant never reaches a Private bucket (line 8): a list leaves it out and a read is 403. The optional workspace is a `managed` child of the tenant with its own cap.

PR C — apps. Branch `stage1a-p55-apps`.
6. Test first: `TestApps_BuilderCreatesWithoutAGrant`, `TestApps_EndUsersAtDepthTwoDrawFromTheBuildersPool`, `TestApps_DeleteCascades`, `TestApps_NeverPublicUntilVerified`. `POST /api/v1/manage/apps` makes an `app` child of the caller's tenant without the platform grant: a verified, paying account, up to 25 apps. Then:
   - the app's management key (`apps`) makes its end users (depth 2) and stores within the builder's pool;
   - `DELETE` cascades;
   - an app is never Public until its root is verified.

PR D — claimable apps. Branch `stage1a-p55-claimable`.
7. Test first: `TestClaimable_PartnerOnlyAndExpires`, `TestClaimable_ClaimMovesTheDataUnderTheClaimer`.
   - A `partner` management key (an operator grants the scope) makes a provisional tenant (1 GiB, never Public, 7 days) and a single-use claim link (`claims`, only its sha256 stored).
   - A claim moves the tenant under the claimer as an `app` child with its data, in one transaction: the pool counter moves (`usage.moveUsed`), as do the keys' counters (they stay the tenant's).
   - An unclaimed tenant is purged like test data.

PR E — the dashboard: an Agents page (the grants, their person, renew, revoke, the pending approvals) and an Apps page (make, list, delete), each behind `dashboardAction`.

PR F — docs: `docs/reviews/WP-STRIPE-7.md`, STATUS, both plan rows DONE, CLAUDE.md, `docs/API.md` § Agents + § Apps, `llms.txt`, the ledger row; Prompt 56 (WP-FAMILY-1) in both editions with Part 0 = RP90.

Prod (migration 122 first, additive):
- Claude connects to tenant zero over MCP, writes an object, and a DeleteBucket asks for approval, which the owner grants;
- tenant zero makes an app, and the app's backend key makes an end user that stores an object;
- a partner-keyed claimable is claimed by tenant zero;
- WP-KEY-1's owed proof runs in the same sitting (one TOTP).

Done means:
- PRs 0 and A–F are merged;
- the proofs have run on prod;
- Prompt 56 is written here.

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
- **Prompt 52 — WP-TEAM-1 — DONE 2026-10-29** (`docs/reviews/WP-STRIPE-4.md`; migration 118). PRs: #1067 Part 0 (D-STRIPE-31 + RP82-1/2/5), #1068 members and roles, #1069 invitations, #1071 several tenants (the active tenant, keys by tenant, the audit tenant). Prod proof owed (the owner's TOTP and mailbox). The post-merge review is RP84. **Reviewed by RP84 (2026-10-30, `docs/reviews/WP-STRIPE-4-REVIEW.md`): HOLDS at P2.** All findings are latent on prod (no invitation sent yet):
  - RP84-1: a key is erased by its maker. A team's erasure leaves its members' keys live; a member's own erasure takes the team's keys.
  - RP84-2: a JWT on no tenant is an owner on the API.
  - RP84-3: invitation mail header injection, and no invitation cap.
  - RP84-4: a removed member's keys are not named.
  - Two P4s.
  - The Stage 1 closeout (52E) was reviewed as **RP85** (`docs/reviews/STAGE1-CLOSEOUT-REVIEW.md`): HOLDS, two P4s.
  - Prompt 53's Part 0 is filled in both editions. Run the prod proof after it deploys.
- **Prompt 53 — WP-ACCT-1 — DONE 2026-10-30** (`docs/reviews/WP-STRIPE-5.md`; migration 120, landed whole with Part 0). PRs:
  - #1085: Part 0, RP84-1…6 + RP85-1/2 + RP82-3/4/7.
  - #1086: PR A, the tree.
  - #1087: PR B, residency.
  - #1088: PR C, the edges' rules + RB10-1.
  - PR D: this closeout.
  - Open question: `standard_sealed_default` is global only (RP82-4's example).
  - Prod proof: the read half is the operator's; the EU child needs the owner's TOTP.
  - The post-merge review is RP86. **Reviewed by RP86 (2026-10-30, `docs/reviews/WP-STRIPE-5-REVIEW.md`): HOLDS at P3** (all latent): RP86-1, the GDPR export's system bucket ignores residency and blocks the admin's set after one export; four P4s, RP86-5 fixed in the review. `vaultaire-quota.yml` and `vaultaire-jobs.yml` were installed on the box by the review.
- **Prompt 54 — WP-KEY-1 — DONE 2026-10-31** (`docs/reviews/WP-STRIPE-6.md`; migration 121, landed whole with PR A). PRs:
  - #1091: Part 0, RP86-1…5.
  - #1093: PR A, the management key. Read pre-merge by RP88 Part 1 (holds at P4); RP88-1…3 and D-RP88-2's columns were taken before it merged.
  - #1095: PR B, budgets, to F-39 as the owner decided it 2026-10-31: refused when spent, min(key, bucket, tenant, pool), the primary never capped.
  - #1096: PR C, the dashboard.
  - PR D: this closeout.
  - Prod proof owed (RB12-7: the owner's TOTP).
  - The post-merge review is **RP88 Part 2**.
- **Prompt 55 — WP-AGENT-1 + WP-APP-1** — WRITTEN above by Prompt 54's closeout, both editions. Part 0 = RP88 Part 2 (the review fills it). Migration 122. The post-merge review is RP90.
- **56 — WP-FAMILY-1**; then WP-POST-1, WP-XFER-1 (after the first node), WP-VAULT-2 — re-cut 2026-10-29 by `docs/ACCOUNTS.md`; each written by the closeout before it.
