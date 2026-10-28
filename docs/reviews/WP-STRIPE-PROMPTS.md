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

## The chain

- **Prompt 49 — WP-PRICE-1 + WP-LOG-1 — ISSUED 2026-10-28** by RP76 (Part 0 = RP76-1…10). The post-merge review is RP78.
- **Prompt 50 — WP-DX-1** (test mode + the Docker image) — written by Prompt 49's closeout; Part 0 = RP78.
- **Prompt 51 — WP-PLAT-1** (API-created accounts); **52 — WP-TEAM-1**; **53 — WP-KEY-1 + WP-POST-1**; **54 — WP-XFER-1** (after the first node); **55 — WP-VAULT-2** — each written by the closeout before it.
