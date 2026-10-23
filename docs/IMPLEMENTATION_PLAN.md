# Vaultaire Complete Implementation Plan

**Last updated**: 2026-10-18
**Live status moved to `docs/STATUS.md` on 2026-10-06** — prod facts, the worker queue, the lanes and the next migration number live there and are edited by every PR that changes them; every Status paragraph below is history as of that date. This file keeps the rules, the order, the decisions and the phases. Since 2026-10-12 STATUS also carries the four lanes' **convergence checklist** and the weekly owner decision batch.
**Status (2026-10-05, evening)**: **Queue item 11 DONE — the Vault layer, WP-VAULT-1 (#587, #589, #590, #591, #592, #594; `docs/reviews/WP-VAULT-1.md`): Geyser reads are parallel ranges (3.2–3.7× on prod at the default), every Vault object can have a parity second copy on the free leg that serves it when tape cannot (flag `vault_parity`, OFF; canary = the bench tenant; migration 077, next = 078), the restore probe is waiting for Geyser to move its two objects to tape, and a stalled PUT on the fixed-bucket driver is cut and retried once. Next = item 12 (edge follow-ups).** **Cloudflare session 2 (docs/CLOUDFLARE.md § Findings 2026-10-05, evening): R2 Sippy's multi-part pull signed an If-Match it never sent — every object above ~200 MiB was 403 at the origin, fixed #588 — and then R2 abandons each part after ~29 s at ~33 MB/s, so a 2 GiB object never lands in R2 (Sippy = objects up to a few hundred MiB); lifecycle expiry → re-pull proven (and it is how an overwrite reaches R2); Workers Cache gateway measured (hit TTFB 0.2 s, 56–72 MB/s, 512 MB cap = BYPASS, Range sliced at read-through cost); a pull counts once per concurrent miss; 64 MB multipart through `stored.ge` at 135 MB/s (aws-cli) / 80 MB/s (rclone), the first-HEAD-of-a-new-key 403 root-caused (edge cache turns HEAD into GET for static-looking keys) and fixed with a zone cache rule; Email Service transport proven, token + domain onboarding [YOU]; Data Access Logs toggle is dashboard-only [YOU]; plan 40.1/40.5/42.2 edited and 42.7 added; `.private/EDGE_COGS_2026-10-05.md`. Item 11 (Vault layer) is running in a worker session (#587 was its first PR).** **Queue item 10 DONE (#584). 2026-10-05 Cloudflare findings (docs/CLOUDFLARE.md § Findings 2026-10-05): a ranged read of a chunked object carried no ETag — R2 Sippy refused every chunked object; fixed #585 and re-proven with a 300 MiB object pulled into R2 byte-exact; Sippy is write-once (overwrites never seen, deletes re-pull), Workers Cache partitions per user with no storage fee, a presigned PUT with a signed Content-Length is refused at any other size, Cache Reserve is refused on the Free zone (strike 40.1(a)), Dynamic Workers is an open beta (40.5 is stale). item 11 (the Vault layer) with the #584 follow-ups came next and is done (front of this line); the stored.cloud base-layer design and its plan edits (40.1, 40.5, 42.2, a new 42.7 admission + ledger) wait on Isaac.** **Launch = October 31, 2026 (T-28). **Wasabi is the interim Standard primary (owner decision 2026-10-03, `wp/wasabi-primary`): the iDrive prod key answers 403 on every object call while the account is repaired ([YOU] 0), so the fixed-bucket driver now runs under its own name (`internal/drivers/wasabi.go`, `WASABI_*`, `STORAGE_MODE=wasabi`), STANDARD follows the primary instead of being pinned to `idrive` (`engine.ResolveStorageClass`), the Smart tier's hot backend and the dashboard's mode follow the engine's primary (WP-R1-4 done), and a `STORAGE_MODE` naming an unregistered driver fails the boot loudly. Rows already on iDrive stay addressable (keep `IDRIVE_*` set); switching back is the one `STORAGE_MODE` line. **Flipped 2026-10-03:** prod runs `STORAGE_MODE=wasabi` on a scoped IAM user (`vaultaire-prod`, bucket-only policy) against bucket `vaultaire` in us-west-1; a PUT/GET/DELETE through the public endpoint was traced to the Wasabi bucket and back. iDrive stays registered for its rows; revert is the one `STORAGE_MODE` line.** The 16-session code review (R0–R15) is COMPLETE and the build phase is running.** **WP-R7-5 routing truth shipped 2026-10-03 (`docs/reviews/WP-R7-5.md`): queue item 5 done — and its prod read found that the primary's iDrive key answers 403 to every GET/HEAD ([YOU] 0 in the note; the synthetic check is still unset). Post-merge review 2026-10-03: #562 — a chunked row's `backend_name` is a sentinel (`'chunked'` / `''`), never an unknown-backend row or read. WP-R10-3b + WP-R12-1 shipped 2026-10-03 (#563, `docs/reviews/WP-R10-3b.md`; post-merge #564: the management API's per-bucket routes refuse the system bucket) — **queue item 8 done; it found WP-R5-14 (the primary key pair cannot be rotated or revoked — SYNTHESIS table A), which became item 9 — **DONE 2026-10-03, PR #565** (`docs/reviews/WP-R5-14.md`, migration 076, with WP-R5-5 / WP-R5-9 / WP-R5-10; post-merge review clean; next migration 077). **Next = item 10, the auth leftovers** (WP-R5-4 remove `SIGV4_ENFORCE`, WP-R5-12, WP-R11-6, WP-R12-14, WP-R12-15, WP-R5-6 closure)**; items 4 (webhooks), 6 (master key) and 7 (WP-R2-1) wait on Isaac.** Before it `main` was at #555 (post-merge hardening of the erasure sweep: the fallback tenant id `default` is never swept; **progress: 3 of 8 pre-launch items + WP-R10-3c/3d of item 8 + WP-R8-7 (#556, 2026-10-02: one address for chunk blobs — `t-_global/_global/…` through `engine.ChunkContext`, read fallback + `POST /api/v1/admin/chunk-move` for blobs written before, dedup GC deletes real blobs, an erasure defers on an unmoved blob, tenant-less driver calls refused; prod's 127 blobs are in the dead iDrive account, so the move has nothing to move there — `docs/reviews/WP-R8-7.md`); WP-R8-4 shipped the same day (#557, `docs/reviews/WP-R8-4.md`: the chunker's identity — restic Rabin v0.4.0, 20 average bits ≈ 2 MiB, polynomial — is one constant set, recorded on every chunked object's `pipeline_config`, pinned by a golden-boundary test and the go.mod version; one helper for the chunked threshold and floor; status quo, Isaac confirms the average); post-merge review of both 2026-10-02: **#558** — a bucket pinned to a region is never chunked (the chunked path ignored WP-R7-1: chunks of an `eu-west-1` object sat on the primary in Dallas, and with no region driver the plain path refused the PUT while the chunked path accepted it; a chunked copy into a pinned bucket is 503/501; found by the driver, red-first); **next = item 5, WP-R7-5** (item 4 webhooks is parked until Isaac says whether they stay on the page)**) (WP-R10-3c: an account erasure reaches every byte — after the head-row walk the runner sweeps every registered backend for what the tenant still holds, defers on any failure and never touches the chunk container; `docs/reviews/WP-R10-3c.md`; **it found WP-R8-7: on a fixed-bucket backend chunk blobs are stored under the uploader's prefix — a second tenant's dedup hit is unreadable and dedup GC never deletes the blob; turn the `chunking` flag OFF until it is built**; **progress: 3 of 8 pre-launch items plus item 8's WP-R10-3c and WP-R10-3d; next = WP-R8-7, then item 4**) (#553 before it, WP-R10-3d: an operator's erasure keeps the operator audit trail — rows about other tenants and `admin` rows stay, de-identified; `docs/reviews/WP-R10-3d.md`) (#552 before it, the WP-R13-2 post-merge fix: the displaced-blob delete is skipped while the old backend's breaker is open) (WP-R12-8: 2FA enrolment keeps the pending secret on the server, shows the backup codes once and renders its own QR code — no external host is left in the CSP; before it #549, WP-R12-5: the dashboard's CSRF token is an HMAC over the session id, checked with `Sec-Fetch-Site` and `Origin`; #548, WP-R4-1: the GOVERNANCE bypass is a permission of the key; #547, WP-R13-2: no copy of an object is left where its key no longer routes, a write lost to a stale-copy delete is detected and counted; **progress: 3 of 8 pre-launch items — queue item 3 DONE (#547, #548, #549, #550), next = WP-R10-3c, then item 4**) (#546 before it: the WP-R13-3 post-merge fix, a job run cut by a deploy is `interrupted`, never the day's success) (**queue item 2 DONE: WP-R13-1 #542 — the class a customer sees comes from the floor, not the backend (a Smart-demoted Standard object is STANDARD in every listing, HEAD and GET; `aws s3 sync` downloads it); WP-R13-3 #543 — one scheduler on `job_runs` for every background job (daily jobs catch up after a restart, dedup GC / smart demotion / inventory run unattended for the first time), `GET /api/v1/admin/jobs`, async triggers, `vaultaire_job_last_success_timestamp_seconds{job_name}` read from the table, `vaultaire-jobs.yml`; #544 — the job trigger no longer echoes the requested name (CodeQL); notes `docs/reviews/WP-R13-1.md`, `WP-R13-3.md`**; before them the WP-R10-9 post-merge fixes #540 + #541; **progress: 2 of 8 pre-launch items, next = item 3**; the `smart_demotion` flag is still OFF — dry run → canary tenant → global) (after #531: #532 plan sync, #533 WP-R4-7 unrouted sub-resources, #534 site fix, #535 the full-plan track + Phases 31–39, #536 decisions F-1/F-6, #537 the WP-R10-3 post-merge fix, **#539 WP-R10-9 egress allowance enforcement — queue item 1 DONE**: one allowance in `internal/usage`, one live per-tenant month counter, a rate cap on GetObject + `/cdn` past it, the 80/95/100 % ladder, metrics + `vaultaire-egress.yml`, copy from `prices.json`; ships dark behind the flag **`egress_throttle` (OFF)**, decision **D-25** to confirm, `docs/reviews/WP-R10-9-egress-allowance.md`). What landed since the 2026-09-22 line below: (1) **landing + house** — pixel redesign and house builder (#479–#493, hero-scene physics #531; sources in `internal/api/landing/`, `make landing`), the house follows the visitor into signup/waitlist/billing (#498, migration 065), design guide `docs/DESIGN.md` (#500); (2) **dashboard plan** (`docs/DASHBOARD_PLAN.md`) Phases 0–4 all shipped — #498 handoff, #504 whole-TB house checkout + per-floor quotas (migration 066), #505 house overview, #507 the look, #508/#510 admin, #509 static versioning, #516 `make dash-shots` / `dash-lighthouse` — **flags `quota_checkout` and `house_overview` are still OFF** (flip after the six Stripe house prices exist and verify at boot); (3) **backends** — R2 public-bucket backend + `PUBLIC` placement (#466), security.txt + origin cert watchdog (#467), iDrive regions rebuilt from the account: 13 real regions, key-gated `idrive-<region>` drivers, no primary-pair fallback (#502, supersedes #460); **erasure coding was BENCHED, not built** (#469–#472, #488, #490, #495: `cmd/tools/erasure-bench`, `bench-results/ERASURE-2026-09-2*.md` — see the note under Phase 11); (4) **review R0–R15** (#473–#528; `docs/CODE_REVIEW_PLAN.md`, findings in `docs/reviews/`, **`docs/reviews/SYNTHESIS.md` = the launch document**): 8 P0 fixed and all but eight structural ones of ~70 P1 (those eight are the launch-gating WPs), each fix with a red-first test, ~93k lines of scaffolding deleted, audit trail (`internal/audit`), login events + lockout, auth-failure metrics, nightly retention job (`job_runs`, migration 071), synthetic customer check, gosec green, `.golangci.yml`; (5) **build phase** — WP-R10-3 account-deletion runner (#529, migration 072, `docs/reviews/WP-R10-3-deletion-runner.md`) and cookieless site statistics + one example house (#530, migration 073). **Phase 5.15.8 smart demotion is DONE** (#461 + #462; flag `smart_demotion` OFF — do not enable for any tenant before WP-R13-1 + WP-R13-3). Migrations: `073_site_stats.sql` is the latest, **next = 074**. Prod `.env` still has **no `STRIPE_*`, no `ENCRYPTION_MASTER_KEY`, no `EMAIL_PROVIDER`, no `IDRIVE_<REGION>_*`, no `SYNTHETIC_CHECK_*`**. Correction of record: the 09-22 line's "Lyve hygiene DONE — `vaultaire-prod` is prod's data-plane key" did not hold on the box; R7-02 (read-only check 2026-09-27) found the Lyve data plane on the account ROOT key with TFA off — SYNTHESIS checklist row 12, [YOU]. **2026-10-04 Vaultaire-path benchmark (`bench-results/VAULTAIRE-PATH-2026-10-04.md`): two launch findings — every Go-SDK client incl. rclone got 403 through Cloudflare (Accept-Encoding rewritten under the signature; fixed #571, re-proven), Cloudflare's 100 MB body limit turns a single >100 MB PUT via `stored.ge` into a 413, and uploads through Cloudflare were capped at ≈1.2 MB/s per stream — **root cause HAProxy's default 65,535-byte HTTP/2 window (Cloudflare → origin is h2), fixed the same day** (`tune.h2.initial-window-size 4194304`, docs/DEPLOY.md); the public endpoint is whole again. **Round 2 the same day: R2 Sippy proven against the prod origin; k-of-n reassembly in a Worker proven for RS and RaptorQ (64 MiB in 2 s from the edge, 0.1 s first byte on a cache hit; Phase 40.1(d)/11 need stripe-interleaved shards for objects above ~64 MiB); the new iDrive reseller account is provisioned by API with 11 regions and live on prod (17/17). **Round 3: iDrive is the primary again since 2026-10-04 15:08 UTC — the day's best backend (no tails, 494 MB/s), the load gate passes 5/5 on the origin AND through Cloudflare for the first time, conformance 22/22, 6+4 erasure over six vendors incl. Geyser and the OneDrive fleet reads 64 MiB in 0.5 s and loses any one vendor for < 0.7 s; techniques digest in report §13.1 (a per-PUT deadline in the fixed-bucket driver; stripe-interleaved shards for the edge). **Zone work done 2026-10-04 evening on the stored.ge zone (account cloudflare@fairforge.io, Free plan): Smart Tiered Cache on, minimum TLS 1.2, Early Hints on, an edge rate-limit rule on the sign-in/sign-up/waitlist paths (30 per 10 s per IP), and a zone-scoped API token for the plan driver. Free plan refuses Cache Reserve; Argo and Load Balancing are unsubscribed. Prod Workers belong in that account (zone routes), which needs Workers Paid ($5/mo) — Workers for Platforms is only for customer-run Workers (Phase 31/42) later. Workers Paid bought and R2 activated on that account the same evening; **the edge Worker is live at `edge.stored.ge`** (k-of-n reassembly 2.6–3.9 s for 64 MiB from the box, cache HIT 0.13 s, edge→R2 uploads, a Zero Trust Access gate proven on a test path). [YOU]: 2FA on both Cloudflare logins, rotate the pasted passwords, remove the stale NS1 nameserver records; decide the `/admin` Access gate.** **Round 4 (same evening): the range-GET overhead is fixed (native range first, no abandoned whole-object GET); Vaultaire-issued direct-to-R2 uploads for public buckets exist (`POST …/direct-uploads` + `/complete`); the erasure bench writes stripe-interleaved shards (`-interleave-kb`) and the edge Worker decodes them stripe by stripe at any size; `rules.stored.ge` is a programmable-bucket prototype on declarative rules that needs no Workers for Platforms (webhook, copy, tag, size and type rejections proven). **Vault + Cloudflare (night): ranged GETs now carry ETag/Last-Modified (#578 — R2 Sippy refused >199 MiB objects without it); Sippy over an archive bucket proven at 256 MiB (cold 29 s from Geyser, then 2.6 s from R2): "restore once, serve many" is real. Geyser reads are the bound at 4–7 MB/s; the Vault "+ Lyve copy" is NOT implemented in the engine — build it or drop it from the COGS table; the restore path stays untimed until Geyser migrates the test objects. **Geyser scenarios (21:00 UTC): the landing zone scales with parallel ranges (1 stream 5.4 MB/s → 16 streams 41.6 MB/s raw, 40.8 through the origin, hashes verified) → the Geyser driver should read whole objects as 8–16 ranges (day-sized WP, 8× on every restore); striped Vault 4+4 (tape data + free parity) reads 64 MiB in 1.5 s without touching tape and survives losing Geyser in 0.95 s — the second copy should be parity, not a mirror; `edge.stored.ge/pget` streams a tape object through the edge 3× faster than direct.** Conformance 22/22; load gate 4/5 (ConcurrentGet p99 1.3 s on Wasabi); Wasabi PUTs stall 9–123 s in ~4 % of calls in both regions and Lyve sustains 4× its upload rate — Lyve is the better free interim primary on the numbers (decision for Isaac); `erasure-bench -codec raptorq` exists: RS decode 0.3 s vs RaptorQ 1.0–1.3 s per 512 MiB over real legs, 11.4/11.5 stay re-scoped. Cloudflare cache: public `/cdn` HIT TTFB 3× better, private = BYPASS; CF API token rotated → Sippy/Cache Reserve need a new token [YOU].**
**Next CODE item (2026-09-30) = the pre-launch queue**, one worker session per item, issued and post-reviewed by the plan-driver session (rows, sizes and files in SYNTHESIS table A; build notes land in `docs/reviews/WP-<id>.md`):
1. ~~**WP-R10-9** egress allowance enforcement (M)~~ — **DONE 2026-10-01, PR #539** (`docs/reviews/WP-R10-9-egress-allowance.md`): the allowance derived at read time (override → house 0.5× downstairs + 1× attic → half the single quota), one live per-tenant month counter, a rate cap past the allowance (the one refusal is the stream guard), the 80/95/100 % ladder, metrics + rule file, dashboard + admin page + `/api/v1/user/usage`, copy made true from `prices.json`. Flag `egress_throttle` OFF; [YOU]: confirm D-25, exempt the demo/bench/synthetic tenants, install `vaultaire-egress.yml`, flip before signups open.
2. ~~**WP-R13-1** demoted objects must not present as GLACIER (S)~~ — **DONE 2026-10-01, PR #542** (`docs/reviews/WP-R13-1.md`): `engine.CustomerStorageClass(floor, backend)` is the one derivation (ListObjects, ListObjectVersions, HEAD, GET, inventory CSV, dashboard browser); HEAD of a downstairs object makes no backend call; RestoreObject answers 403 like any STANDARD object; a read of bytes that went to tape submits the recall and answers 503 + `Retry-After` (GET, copy source and `/cdn` — the last two were 500s), kept out of the 5xx series; the CDN promotes on read; D-23 (a) in the FAQ, to confirm. ~~+ **WP-R13-3** `job_runs` catch-up for every daily runner (S)~~ — **DONE 2026-10-01, PR #543** (`docs/reviews/WP-R13-3.md`): one scheduler (`api/jobs.go`) — daily `inventory` 00:30, `dedup_gc` 02:30, `retention` 03:30, `account_deletion` 04:30, `smart_demotion` 06:30 UTC with a boot catch-up and an hourly re-check on `job_runs`; seven interval jobs; one advisory lock per job; per-item failures are a note, not a failed run; `GET /api/v1/admin/jobs`, triggers answer 202 and run detached; metrics read from `job_runs` (label `job_name`); `vaultaire-jobs.yml`. **Item 2 is DONE — both gates of the `smart_demotion` flag are closed**; [YOU]: install `vaultaire-jobs.yml`, confirm D-23, then dry run → canary tenant → global.
3. **DONE 2026-10-02 — four PRs: #547 (WP-R13-2), #548 (WP-R4-1), #549 (WP-R12-5), #550 (WP-R12-8).** ~~**WP-R4-1** GOVERNANCE bypass as a permission (S)~~ — **DONE 2026-10-02, PR #548** (`docs/reviews/WP-R4-1.md`): the key's scope travels in the request context; `x-amz-bypass-governance-retention` is honoured only for `*` or the new `BypassGovernanceRetention` permission and only where the signature covers it (a presigned URL's holder cannot add it); STS tokens never inherit it; the three key-creation surfaces offer it; COMPLIANCE untouched. ~~**WP-R12-5** session-bound CSRF (S)~~ — **DONE 2026-10-02, PR #549** (`docs/reviews/WP-R12-5.md`): token = HMAC-SHA256 over the session id under a key derived from `JWT_SECRET` (survives restarts), no CSRF cookie, `Sec-Fetch-Site` (`same-origin`/`none` only) and `Origin` checked on every mutation of `/dashboard` and `/admin`; the 30-route matrix is in the note. ~~**WP-R12-8** server-side pending TOTP secret + vendored QR (S)~~ — **DONE 2026-10-02, PR #550** (`docs/reviews/WP-R12-8.md`): the pending secret lives in the server's memory keyed by the session (10 min, single use, bounded), the enable POST carries the 6-digit code only, backup codes are shown once after it verified, the QR code is a PNG the server renders, `cdn.jsdelivr.net` left the CSP (no host in any directive); `EnableMFA` writes the database first. ~~**WP-R13-2** demotion ledger hygiene (XS; added 2026-10-02 — it is the last item in front of the `smart_demotion` flag for a tenant with real data)~~ — **DONE 2026-10-02, PR #547** (`docs/reviews/WP-R13-2.md`): one rule — a copy is stale when the key does not route to its backend; overwrite / copy / multipart complete delete the displaced blob on another backend, the reclaim and DeleteObject settle the ledger's copies, R13-06 is detected (`vaultaire_stale_copy_lost_writes_total`, rule `StaleCopyLostWrite`), a cancelled run returns `ctx.Err()`; the adversarial pass also closed a copy-back that wrote old bytes over an overwritten object. **Prompt issued 2026-10-02.** Then, by the driver's recommendation of 2026-10-01: **WP-R10-3c** erasure reaches every byte (item 8) before item 4.
4. **WP-R11-3** webhook outbox with retries (M, after WP-R13-3) — only if webhooks stay on the page. **Parked 2026-10-02**: asked once, unanswered; the queue moves on and comes back to it when Isaac answers.
5. ~~**WP-R7-5** routing-truth reconciliation~~ — **done 2026-10-03** (`docs/reviews/WP-R7-5.md`): boot check (every `backend_name` in three tables is a registered driver; two names never share a store), the `routing_truth` job (05:30 UTC, the recorded backend only, breaker observed never charged, chunks at the one address, result in `job_runs.result` — migration 074), metrics + `vaultaire-routing.yml`, `GET /api/v1/admin/routing-truth`, `POST …/resolve-null` (dry run), `local` is no tier (REDUCED_REDUNDANCY unmapped). The prod backfill is the read-only plan in the note's [YOU] (Isaac runs it: fix the iDrive key FIRST — **prod's primary answers 403 to every GET/HEAD while HeadBucket says healthy**; then erase the three bench tenants rather than row surgery; the fleet still holds ⅔ of the `onedrive` bytes).
6. ~~Encryption track, only if the master key ships pre-launch~~ **the master key ships (F-14, decided 2026-10-12; WP-R8-1 DONE #682, migration 084)** — before the flip: the ~~`generatedObjectWriter` plaintext-into-SSE-buckets fix~~ (done #685) (XS) + **WP-R3-1** pulled ahead of WP-R6-1 + `server.go` reading `$CREDENTIALS_DIRECTORY` + the dedicated service user; then the on-box ceremony (systemd credential `host+tpm2`, `age-plugin-sss` 2-of-3 escrow, logged); the key never changes "we can read Standard-mode data". The old line, for history: **WP-R8-1** stored-random tenant keys + key versioning (M) → **WP-R3-3** SSE-S3 multipart (M, needs WP-R8-5) → **WP-R14-6** copy.
7. **WP-R2-1** write-new-key-then-swap (L) — recommended post-launch unless versioning is sold as byte retention.
8. ~~**WP-R10-3c** erasure reaches every byte (S–M; found by the 2026-10-01 post-merge review of #529 — blobs behind a delete marker survive an erasure)~~ — **DONE 2026-10-02, PR #554** (`docs/reviews/WP-R10-3c.md`: the erasure sweep; `engine.TenantWalker`; defers on any failure; `_global` never swept; found **WP-R8-7**, one address for chunk blobs — ~~S–M, gating~~ **DONE 2026-10-02, PR #556** (`docs/reviews/WP-R8-7.md`; [YOU]: run the chunk move once after the deploy, `chunking` back on, remove the fallback when the legacy counter stays at 0)) + ~~**WP-R10-3d** operator audit rows survive the operator's erasure (XS)~~ — **DONE 2026-10-02, PR #553** (`docs/reviews/WP-R10-3d.md`: kept and de-identified, not refused; the policy sentence is unchanged) + ~~**WP-R10-3b** async GDPR export (S) + WP-R12-1 streamed export~~ — **DONE 2026-10-03, PR #563** (`docs/reviews/WP-R10-3b.md`: one streamed service behind the management API and the dashboard, the `_exports` system bucket, a 1 h presigned download, 7-day expiry by the retention job, the schema-walk guard on secret columns; [YOU]: exports counted vs free, the privacy line, found: the primary key pair cannot be rotated), then SYNTHESIS table B by size.
9. **WP-R5-14** the primary key pair can be rotated and revoked, for real (S–M; found by WP-R10-3b: `lookupCredential` resolves `tenants` first with no revocation check, rotation writes `api_keys` only) + **WP-R5-5** STS IP intersection and parent-revoke cascade (S) + **WP-R5-10** JWT revocation on password change (XS) + WP-R5-9 folded in — one credential-lifecycle session; **DONE 2026-10-03, PR #565** (`docs/reviews/WP-R5-14.md`, migration 076; next migration 077).
10. **Auth leftovers** (one session, five XS/S rows of SYNTHESIS table B): **WP-R5-4** remove `SIGV4_ENFORCE` + the key-existence fallbacks, **WP-R5-12** key hygiene (`ValidPermissions` from the parser, IP allowlist validation on create, expiry in the past refused, expired-key error code), **WP-R11-6** key API hygiene (envelope, typed 404/409, `revoked_at` in listings, `days ≥ 1`), **WP-R12-14** `__Host-` session cookie, **WP-R12-15** password on MFA enable + regenerate backup codes; close **WP-R5-6** (the maps are under `cacheMu` since #565 — prove it with the race test and flip the row) — **prompt issued 2026-10-03; not run by 2026-10-04 (no auth commit on main); re-issued 2026-10-04 — **DONE 2026-10-05, PR #584** (`docs/reviews/WP-AUTH-LEFTOVERS.md`; post-merge review: complete on all six rows; two follow-ups for the next session — the primary key can be expired through `/expire` with no way back, and an allowlist of `0.0.0.0/0` / `::/0` is accepted silently, decision for Isaac).**
11. ~~**The Vault layer** (one session; from the 2026-10-04 Geyser benchmarks, report §15–§16): **(a)** Geyser driver `Get` as 8–16 parallel ranges (measured 5.4 → 41.6 MB/s on the landing zone, ranges byte-verified; env `GEYSER_GET_CONCURRENCY`, default 8, ordered streaming, one range buffered ahead, single-stream fallback); **(b)** a parity second copy for `vault`-floor objects — RS 4+4 with the data shards unstored (tape holds the data) and the four parity shards on the free legs (`permafrost`, then `lyve`) written by an async job behind the commit, a `vault_parity` table, a reader fallback on a Geyser failure, erasure on delete, flag `vault_parity` (default off) — this is what makes the pricing page's second-copy claim true (measured 4+4: 64 MiB read in 1.5 s without touching tape, Geyser lost 0.95 s); **(c)** `cmd/tools/geyser-restore-probe`: HEAD an archive object hourly, and the first time Geyser reports it on tape run the customer path (GET → 503, RestoreObject, poll, GET identical) with timestamps — `tier-archive-20261004/obj8.bin` and `vault-bench-20261004/v256.bin` are waiting; **(d)** per-PUT deadline + one retry on a fresh connection in the fixed-bucket driver (Wasabi stalls 4 % of PUTs 9–123 s), `vaultaire_driver_put_retries_total{driver}`. Report `docs/reviews/WP-VAULT-1.md`.~~ — **DONE 2026-10-05, six PRs** (`docs/reviews/WP-VAULT-1.md`): **#587** warm-up from the #584 review (the primary key is never expired — 409 `primary_key`; an expired key is not rotated; an allowlist of `0.0.0.0/0` / `::/0` is a 400, STS `ip_restrict` too); **#589 (a)** the Geyser driver reads a whole object as ordered parallel ranges — one probe range (an object on tape is ONE refusal), `GEYSER_GET_CONCURRENCY` (8) streams, ETag/length guards; live on prod: one 256 MiB GET 41.8 s → 11.4–13.0 s, hashes equal to Geyser's own copy; **#590 (b)** the parity second copy — RS 4+4, data shards unstored, four parity shards on `permafrost` (else `lyve`) by the `vault_parity` job (2 min), migration 077, a read fallback when the object's backend fails (never for `ErrArchived`), erasure with the object, the account and the sweep; flag **`vault_parity` OFF** (ON for the bench tenant as the canary); live on prod: a GET answered byte-exact from the parity after the object's tape copy was deleted; **the parity alone IS a complete copy (any 4 of 8) — the note says what the pricing sentence may claim**; **#591 (c)** `cmd/tools/geyser-restore-probe`, hourly on SLC for the two waiting objects (baselines taken; the timed restore is still to come); **#592 (d)** per-PUT deadline (`FIXED_BUCKET_PUT_TIMEOUT`, 60 s per 64 MiB) and one retry on a fresh connection in the fixed-bucket driver, never doubling a multipart part, **#594** its retries counted per regional backend. Next migration 078. [YOU]: the pricing page's second-copy sentence; `0.0.0.0/0` refused vs warned; `GEYSER_GET_CONCURRENCY` for prod (16 measured 41.6 MB/s); flip `vault_parity` beyond the canary; remove the probe's cron line when both reports are in.
12. **Edge follow-ups** (one session): R2 event notifications → Queue → `POST …/direct-uploads/complete` so direct uploads register themselves and the rules Worker sees every object; the `/admin` Access gate once decided; a Workflow around every Vault restore (submit, poll, warm R2 via Sippy, notify). `docs/CLOUDFLARE.md` is the operator page.
Code freeze = Stage 6 (week of Oct 19): after it the driver switches to owner-checklist mode. **Owner critical path ([YOU], click paths in the SYNTHESIS "Pre-launch checklist"):** Stripe (endpoint pinned to API 2023-08-16, both secrets, six house prices, then flip `quota_checkout` + `house_overview`), synthetic tenant + `SYNTHETIC_CHECK_*`, install the five `deploy/monitoring/vaultaire-{backends,tls,auth,synthetic,egress}.yml` rule files on SLC, exempt the demo/bench/synthetic tenants and flip `egress_throttle` before signups open (D-25), backups off-box (WP-R9-7), HAProxy `/metrics` deny + CF header strip (WP-R1-1), ~~iDrive regional pairs~~ (done 2026-10-04 on the new reseller account, 10 regional drivers live), Lyve scoped key + root TFA, ~~the `ENCRYPTION_MASTER_KEY` decision~~ the master-key ceremony (F-14, 2026-10-12: `libtss2-rc0` on the box, the service user, the three escrow shares — YubiKey, paper identity, trustee), CSAM/DMCA, email provider (sending domain stored.cloud), Cloudflare items **redone on the stored.cloud zone + the Porkbun DNSSEC DS (WP-DOMAIN-1, before Oct 31)**, status page (`status.stored.cloud`), ~~standby VPS~~ **the standby box bought as EPYC with SEV-SNP exposed (F-18; `sev_snp` = Y before paying)**, the two WP-DOMAIN-1 sub-decisions (F-17), and decisions D-1..D-4, D-7, D-9, D-12..D-24 (D-19 was applied and needs confirmation). The stage list under the 09-22 line below is still the owner sequence.
**Operating mode (2026-10-01, owner decision): BUILD THE FULL PLAN — Isaac is customer zero.** The whole plan is worked through with or without outside customers. After the pre-launch queue above, the order of record is the **FULL-PLAN TRACK** section (before Tier 2): ten stages in dependency order, gated by prerequisites and by use on tenant zero instead of customer counts; Phases 31–39 (smart programmable buckets and what the original master plan had that this plan did not) are at the end of the file. The public-launch gates (signups, Stripe, CSAM/DMCA, alerting) are unchanged. **(2026-10-12):** the track is worked in **four lanes** — Engine, Developer surface, Consumer surface, Trust — with charters in `docs/surfaces/*.md` and `docs/ARCHITECTURE_CONTRACT.md` cited by every worker prompt; WIP = one deep code lane + one surface lane, docs lanes in parallel; all four lanes build from 2026-10-12, dark until proven (flags OFF, new hosts, additive migrations; the launch path guarded by CI, conformance, the synthetic check and the Stage-6 freeze on launch-path changes only — owner: "I don't want to defer anything"). After launch the track forks once (F-8): 10.0 right after Stage 1a if the consumer crowd is the next year's choice, otherwise the Stripe layer first.
**Recent (2026-10-03) — Cloudflare to its maximum, and private compute.** From the owner's Cloudflare build-and-bench session (the production Cloudflare account, Workers Paid): R2 Sippy pulling from a Vaultaire whose primary was the OneDrive fleet, a Worker hydrating OneDrive → R2, WASM resize, streaming ZIP, Vaultaire itself running as a 256 MiB Container, Queues, Workflows, R2 events, Workers AI + Vectorize, an MCP server — all measured. **Three phases added at the end of the file:** **Phase 40 Cloudflare edge plane** (the principle: bytes at rest on Vaultaire, serving on Cloudflare, never R2 as the vault; pay per upload, never per view; 40.1 the 0×-SLC serving plane, 40.2 per-customer CDN hostnames, 40.3 the media pipeline rule, 40.4 restore as a workflow, 40.5 where a function runs, 40.6 Vaultaire in a Container + managed Ente, 40.7 Zero Trust and agents, 40.8 search over a bucket, 40.9 on-upload automations); **Phase 41 private compute on encrypted data** (three modes per space, queryable index, on-device ML, PIR lookups replacing 10.8's OPRF, attested enclave jobs, encrypted CRDT relay); **Phase 42 the app plane** (the plan ladder and the four-level tenancy tree that change this engine's quota model, Stored at Drime level with the customisation ladder and a Framer-model marketplace, Hearth-CF and the Forge apps, the mirrors catalogue, the LowEndTalk lineup). Cross-references were added to 5.10.6, 7.6, 10.8, 13.2, 13.3, 15.1, 16.5, 18.1, 18.4, 20, 24.2, 26.3, 26.8, 27.6, 29, 31.6, 34.3, 34.8, 35.7, 39.7; the FULL-PLAN TRACK table and decisions F-7–F-10 record where each lands. Nothing in the pre-launch queue changed.
**Recent (2026-10-11) — the Ente model and the Cape model.** Owner ask: follow Ente for end-to-end encryption and Cape for holding nothing on a customer but what the service needs, then decide the master key. Done in this docs PR: Phase 10.0 is **re-scoped to Ente's published key hierarchy** (master key, Argon2id key-encryption key, recovery key, X25519 key pair, a key per space, a key per object; SRP login; sharing by sealed box; public links with the key in the fragment; what the server sees per mode) with our two additions — convergent chunk keys under the space key so a space keeps deduplicating its own data, and a post-quantum hybrid for key sharing (decisions F-12, F-13); **Phase 43 — data minimisation, the Cape model** — is new at the end of the file: the inventory of every column we hold on a customer generated from the GDPR export's own section list, the identity ↔ data-plane disassociation (the data plane names a tenant by an opaque id, nothing else), "built to forget" retention targets per table against what the privacy page says today, the law-enforcement policy + "what we can produce" page + transparency report, and numbered accounts (demand-gated); 43.1–43.4 run as a **privacy lane** alongside Stage 1a (small, writing + one WP each), 43.5 after 10.0. **Master key verdict (recorded under F-14):** set `ENCRYPTION_MASTER_KEY` on prod **after WP-R8-1 is deployed** (the one-way door — stored-random versioned tenant keys — then real rotation and shredding exist from the first encrypted byte) and the escrow is done — **mechanics superseded 2026-10-12 (F-14 row): an on-box ceremony → systemd credential `host+tpm2`, `age-plugin-sss` 2-of-3 (YubiKey, paper identity, future trustee share), plaintext never leaves the box, every ceremony logged + in the transparency report; `.private/STAGE2_RUNBOOK.md` (which generates the key on the Mac) is rewritten to this first**; it protects **Standard**-mode data (2026-10-12: the modes are Private / Standard / Public, F-15 — "Normal mode" retired) from the storage vendors, which Ente-style E2EE never addresses for S3-API customers; the two layers stack. The copy stays honest until WP-R3-1/WP-R3-3 encrypt multipart objects: "encrypted at rest with keys we hold", never "zero-knowledge", and the privacy page's per-mode table (41.0) says what the key lets us read.
**Recent (2026-10-12) — one brand, three modes, the trust lane (owner verdicts, docs PR).** **One brand: Stored, a product of FairForge; one canonical domain: `stored.cloud`** (stored.ge and stored.pics are permanent aliases — the web 301s, the S3 API answers on both names forever with no redirect); three ambitions on one engine: least access to customer data with any access a public event; "Stripe for storage" (the developer platform); "it just works" storage for the mainstream crowd. The rules are one page, `docs/ARCHITECTURE_CONTRACT.md`; the lanes have charters in `docs/surfaces/`. **Modes are a property of the SURFACE, never a user choice — Private / Standard / Public (F-15)** ("Normal mode" and "Vault mode" retired as mode names; Vault stays the tape/attic TIER): the developer surface (S3 + management API) is Standard by default, first-class, Private via the SDK or SSE-C per bucket; the consumer surface (`stored.cloud/photos`, apps) is Private by default ONLY once a passcode recovery vault exists (F-19) — until then recoverable default + "Advanced protection" opt-in (Apple's shape); Public = plaintext. Copy: Standard = "encrypted at rest with keys we hold"; Private = outcome sentences ("Only you can see your photos"); "zero-knowledge" forbidden before an external audit; one `modes.json` feeds the privacy-page table, the OpenAPI text, the dashboard labels and a test (the `prices.json` pattern). **Master key (F-14, mechanics set):** WP-R8-1 is done (#682); the install is an on-box ceremony → systemd credential `host+tpm2` (`LoadCredentialEncrypted=`; the credential read shipped in #685: `$CREDENTIALS_DIRECTORY/encryption_master_key`, `ENCRYPTION_MASTER_KEY_FILE` for labs, the env var last), a dedicated service user (not the deploy user), mount namespacing, no PCR binding, `libtss2-rc0`; escrow = `age-plugin-sss` threshold 2-of-3 (YubiKey, paper identity, a future trustee share), plaintext never leaves the box; every ceremony logged and in the transparency report; follow-up WP: a TPM-resident KEK so no exportable master exists. Before the flip: ~~`generatedObjectWriter` writes plaintext into SSE buckets~~ (fixed #685) and WP-R3-1 is pulled ahead of WP-R6-1. **The key never changes "we can read Standard data"** — the service decrypts to serve and the S3 secret is stored for SigV4. **"Can't read" exists only client-side:** SSE-C (today single PUT/GET only — multipart 501, CopyObject unsupported, the whole object buffered → **WP-SSEC-1**: streaming, multipart, copy, an "SSE-C required" bucket flag), a client-side tools guide (restic, rclone crypt), Private mode (10.0, Ente's hierarchy, F-12). **Dedup of encrypted data is within-space convergent only** (F-12) plus the opt-in public-artefact namespace (10.3/10.6); PAKE/PIR research stays behind F-3's measurement; never a global convergent key — cross-tenant convergence and least access are opposites; homomorphic = PIR lookups only (41.3), compute on device (41.2) or attested (41.4). **The attested data plane (41.4 / F-9) is the ONE shared foundation:** self-hosted SEV-SNP EPYC (the prod box is a Ryzen 9 9950X — no SEV-SNP; EPYC Milan or later), app + Postgres + keys inside a measured confidential VM, no ssh, a Trustee KBS key broker on another box, published measurements, an attestation endpoint; it serves developer compliance processing, the consumer recovery realm + cloud AI (the Private Cloud Compute pattern) and "can't quietly"; F-9's cloud-enclave list is superseded by self-hosted first; prod does not move into a CVM before launch; **the Stage-5 standby is bought as EPYC with SEV-SNP exposed (F-18;** `/sys/module/kvm_amd/parameters/sev_snp` = Y before paying) and used now as standby + PG replica + backup target + TPM escrow share. **Consumer recovery (F-19):** 10.0.3's "later, optional" passcode recovery vault with hardware attempt limits (iCloud Keychain escrow pattern; Juicebox OSS, WhatsApp Backup Key Vault, Signal SVR3) is REQUIRED before Private is the consumer default, with trusted contacts (Ente Legacy), passkeys and recovery nudges; first realm = the attested box. **New decision — a transparency log (F-16):** append-only, signed checkpoints, at least one external witness; records client build hashes (clients refuse unlisted builds), server image measurements + key-release policy, escrow/recovery ceremonies, law-enforcement productions, admin reads of customer metadata; start = signed statements + OpenTimestamps + a Rekor mirror, later a tile log; audience press and auditors, customer copy = outcomes; lives at `trust.stored.cloud` with the 43.4 artefacts (new 43.6). **43.2 gains Ente's server pattern:** e-mail encrypted at rest under a server key + keyed BLAKE2b hash for lookup. **WP-DOMAIN-1 — domain consolidation BEFORE 2026-10-31 (F-17):** stored.cloud home (it already serves the app, 200 on `/health`); `stored.cloud/developers` + `docs.stored.cloud`; `app.stored.cloud` (dashboard); `s3.stored.cloud` (own host → own Cloudflare rules); `trust.stored.cloud`; `status.stored.cloud`; `stored.cloud/photos` later (stored.pics 301); `stored.cloud/platform` for the licensing product (Phase 30 renamed); public customer content moves to a separate non-brand content domain (the googleusercontent pattern; closes review R4's inline-HTML class). Touches `VAULTAIRE_ENDPOINT` / `VAULTAIRE_BASE_URL`, the hard-coded security.txt contact, legal pages, OpenAPI servers, landing sources, OAuth callbacks, the Stripe webhook (create it on .cloud), `SYNTHETIC_CHECK_URL`, the e-mail sending domain, the Cloudflare zone rules redone on the stored.cloud zone (sign-in rate limit, HEAD cache rule, S3 bypass), the Porkbun DNSSEC DS pending since 2026-09-20; ~92 files mention stored.ge. Two sub-decisions for the owner: `*.s3.stored.cloud` for virtual-hosted bucket names (Cloudflare ACM $10/mo) vs path-style only; the s3 host proxied (100 MB single-PUT limit) vs direct. **Operating model:** four lanes (Engine, Developer surface, Consumer surface, Trust); keystone WPs built once in the Engine lane (streaming SSE WP-R8-5, the SDK key core as a Go module + WASM, the bucket mode column, the transparency-log library, the recovery-vault interface); WIP = one deep code lane + one surface lane, docs lanes in parallel; a weekly owner decision batch; a convergence checklist in `docs/STATUS.md`. All four lanes build now, dark until proven (owner, 2026-10-12 evening — see Lanes). After launch 10.0 moves up to right after Stage 1a IF the consumer crowd is chosen for the next year; otherwise the Stripe layer (31, 42.1, 26, docs/SDKs, WP-SSEC-1) goes first — F-8 updated. Nothing in the pre-launch queue changed except item 6 (below) and the new WP-DOMAIN-1. **The same day's research** — the attested plane stack, recovery realms (Apple, Signal, WhatsApp, Juicebox), SSE-C's honest claim, the Züs review and the Broken-Ecosystem test matrix — is in `docs/research/ATTESTED_PLANE_AND_RECOVERY_2026-10-12.md` and folded into 41.4, F-17/F-18/F-19, 5.14.8a, 10.7, 43.6, 14.4, 21.1 and 6.5.
**Status (2026-09-22, superseded by the 2026-09-30 line above — kept as history)**: **Launch = October 31, 2026 (T-39). ALL launch-blocking CODE is shipped** — Stages 0, 1, 1B and 3 done; backend-outage alerting live (5.12.10, #457); quota-sold pricing copy (#458). Prod = iDrive primary (NEW reseller acct d15e6040, Dallas key swapped in 2026-09-22) + Geyser archive + Lyve `resilient`/fallback leg; **no `ENCRYPTION_MASTER_KEY` in prod yet**. Pricing is QUOTA-SOLD on every tier (section below): Vault Flex $2.55/$2.00 per TB, Standard $4.49/$4.99, Performance PARKED, no Stripe Billing Meters at launch.
**Remaining critical path = OWNER items, in stage order per `.private/LAUNCH_EXECUTION_SEQUENCE.md`:**
- **Stage 2** — confirm iDrive payment on the new acct; **(2026-10-12, F-14) the master-key ceremony on the box — systemd credential `host+tpm2`, dedicated service user, `age-plugin-sss` 2-of-3 escrow, logged; after the `generatedObjectWriter` fix + WP-R3-1; runbook rewritten first.** Was: generate + escrow `ENCRYPTION_MASTER_KEY` → prod (runbook `.private/STAGE2_RUNBOOK.md`).
- **Stage 3** — load-test rerun after the Stage-2 cutover (scripted; baseline `bench-results/LOADTEST-2026-08-03.md`).
- **Stage 4 (week of Oct 5)** — Stripe LIVE quota prices (price × TB: Standard monthly/annual, Vault monthly/annual, Founders + LET2026 coupons) + Stripe Tax + one real-card cycle. NO meters (V18.7-minimal, rescoped).
- **Stage 5 (week of Oct 12)** — Cloudflare rules incl. S3 bypass-cache **on the stored.cloud zone (WP-DOMAIN-1)**, hosted status page (5.14.12, `status.stored.cloud`), ~~cold-standby VPS~~ **the standby box = EPYC with SEV-SNP exposed (F-18, 2026-10-12), used now as standby + PG replica + backup target + TPM escrow share — the future attested data plane (41.4/F-9)** + PG replication, email provider (5.6/5.13.4), subscribe to the ntfy alert topic.
- **Stage 5.5** — DMCA agent, CSAM scanning tool (HARD blocker before signups open), single-copy disclosure, `security.txt` (contact → stored.cloud, WP-DOMAIN-1), insurance, **(2026-10-12, F-16) the first `trust.stored.cloud` page: signed statements + OpenTimestamps + a Rekor mirror.**
- **Stage 6 (week of Oct 19)** — flip the `signups` flag, canary E2E, LET post, code freeze.
**Next CODE item (2026-09-22, superseded — both shipped: 5.15.8 in #461/#462, per-region keys in #460 then rebuilt in #502; the Lyve claim is corrected in the 2026-09-30 line)**: Smart demotion job WP (Phase 5.15.8 below) → per-region iDrive keys (small, stack row 13). Lyve hygiene ✅ DONE 2026-09-23 (ops): scoped IAM service user `vaultaire-prod` (s3:* on `stored-*` only) is prod's data-plane key, root key demoted to `LYVE_PROBE_*` (console probe only), 4 policy-less `stored-*@stored.ge` users deleted, bucket-wide abort-MPU-7d lifecycle on both `stored-*` buckets + 5 stale July MPUs aborted, 99 junk/test buckets purged (keep: `stored-*`, `vaultaire-bench-2026`, `vbench-*`, the one live-COMPLIANCE-locked bucket). Migrations: `063_smart_promotions.sql` is the latest (2026-09-23).
**Status (2026-09-18, addendum — plan unchanged otherwise)**: Versity evaluation complete (versitygw @ 1246a08 studied as a parallel-implementation reference + free conformance suite; full findings in `.private/VERSITY_EVALUATION.md`). **Correction of record: Versity is NOT in our stack — Geyser's S3 layer is Spectra Logic Vail** (`internal/drivers/geyser_README.md`). Takes captured as **WP-VG1..VG4** in the new "Versity takes" section after the Post-Launch 90-Day Priority Stack: WP-VG1 (pre-launch, small) = run versitygw's 523-test BATS S3 suite against a local vaultaire → triaged gap report; WP-VG2 (post-launch) = SigV4-streaming + checksum hardening sweep (per-chunk signatures, trailer checksum validation — aws-cli v2's default path currently gets zero server-side integrity check — plus full `x-amz-checksum-*` surface); WP-VG3 (folds into full V18.2) = restore wire-fidelity checklist (we're already MORE AWS-faithful than versitygw's glacier mode); WP-VG4 (M12+) = add versitygw-posix techniques to the own-fleet eval set.
**Status (2026-07-09)**: **Launch-sequence Stage 0 (live ops triage) COMPLETE.** Root cause of three separate 2026-07-08 review findings was a single event: prod `.env` went unreadable (root:root 600) on Jun 7 — (a) daily pg_dump backups were empty 20-byte gzips while logging success (fixed: perms + hardened pg-backup.sh with fail-loud asserts; fresh 71-table dump restore-tested), (b) CI deploys were silent no-ops reporting green — psql password prompts consumed the deploy script from stdin, so prod ran a Jun 8 binary for a month and migration 052 was never applied (fixed: PR #316 `psql -w </dev/null` + pre-swap rollback binary; verified live — prod now runs main @ c41d355 with 052 applied), (c) `ENV` was absent entirely (now `production`, chaos endpoints gated). Cert-expiry blocker was stale (origin cert valid to 2041). Prod primary backend is already iDrive — Stage 2 shrinks to verify + `ENCRYPTION_MASTER_KEY`. Test baseline on main: only known `TestPostgres_TenantOperations` fails. **Next: Stage 1 code fixes (WP-5 → WP-9 → WP-2 → WP-3 → WP-1 → WP-6 → WP-7 → WP-4) per `.private/LAUNCH_EXECUTION_SEQUENCE.md`.**
<!-- reconstructed: status + audit pass 2026-07-07 — verified against git log (#173-#312), .private/ docs; changes marked with "reconstructed:" comments -->
**Status (2026-07-07, superseded)**: **Tier 1 code-complete. Launch (end of July 2026) is gated on 5.15.5 (ops) + 5.15.6 (front-door UX) + 5.15.7 (code/billing/security). ⚠️ The "ops only, no code" framing below is SUPERSEDED by two 2026-07-08 reviews that found real code blockers — see Phase 5.15.6 and Phase 5.15.7. Day-to-day driver: `.private/LAUNCH_EXECUTION_SEQUENCE.md`; ops steps: `.private/LAUNCH_RUNBOOK.md`.** Last merged work: PR #312 (2026-06-17) — range-GET passthrough (`RangeGetter` interface on iDrive/Lyve/Geyser, 5× download: 6→30 MB/s single, 106 MB/s concurrent), **convergent chunk encryption [Phase 10.1-10.3]** (HKDF deterministic nonces, dedup-safe, migration 052), OneDrive fleet renamed **"permafrost"** (admin-only, removed from user-selectable storage classes), NVMe cache enlarged (16 GB RAM / 500 GB SSD, <1ms hot reads), and `cmd/validate` 22-test E2E suite (iDrive/Lyve/Geyser all 20/20 from SLC). Before that: **Phase 9 compression COMPLETE** (#311), **Phase 8 COMPLETE** (#300-309), **Phase 7 COMPLETE** (#297-299), admin gap-fills 3.7/3.10/3.11 (#294-296). Tier 2 has shipped AHEAD of launch through Phase 10.1-10.3.
**Launch blockers (HISTORICAL — see Status 2026-09-22 for the quota-sold / Oct-31 truth; ops — SUPERSEDED 2026-07-08: now ALSO code, see Phases 5.15.6 + 5.15.7)**: **5.15.5** — prod on a durable backend (✅ iDrive since Stage 0; was local-disk fallback), ~~create + attach Stripe Billing Meters, set `STRIPE_METER_*` in prod .env~~ (NOT needed — quota-sold decision 2026-09-21), reopen signups (`signups` flag), ~~TLS auto-renewal check (cert expires 2026-07-28 — launch week)~~ (stale: origin cert valid to 2041; launch is Oct 31, 2026), Cloudflare cache/rate rules. **5.15.4** — smoke checklist run. **5.14.12** — hosted status page. **V18.7-minimal** — Stripe LIVE quota prices (price × TB for Standard/Vault monthly+annual — NO Vault packs, NO meters) + Founders + LET2026 coupons (launch posts in `.private/launch/`). **5.15.2** — ✅ run against prod 2026-08-03 (#423).
**Next code phase**: post-launch — see **"Post-Launch 90-Day Priority Stack"** (before the Tier 2 heading). In the Tier-2 pipeline sequence: Phase 10 remainder (10.4-10.8, incl. new 10.8 blind dedup/OPRF), then Phase 11 erasure coding → P2-P6 permafrost parity.
**Status (2026-06-04, superseded)**: Phase 5.14 COMPLETE through 5.14.11 — all shipped & deployed to SLC prod: 5.14.1-5.14.4 (#253-256), 5.14.11 security hardening (#257), 5.14.5 HIPAA (#258), 5.14.6 GDPR/EU Data Act (#259), 5.14.7 per-bucket regions (#260), 5.14.8 SSE-C (#261), 5.14.9 access logging + inventory (#262), 5.14.10 compliance dashboard (#263). 5.11.0-5.11.12, 5.12.3-5.12.7, 5.13 complete. Gap-fill 5.10.17 object tagging shipped (#264). 5.15.1 graceful shutdown already in main.go (verify systemd TimeoutStopSec=45). **ALL gap-fills + 5.15 code COMPLETE:** 5.10.18 (#267), 2.7 (#269), 4.3 (#275), 3.6 (#276), 3.8 (#277), 3.9 (#278), landing 5.15.3 (#272), load-test gate 5.15.2 code (PR #282). **Remaining before launch = OPS only (no more code blocks Tier 1):** run the 5.15.2 load harness against live + fix what it surfaces; 5.15.4 smoke checklist; **5.15.5 prod backend/durability + Stripe meter activation + reopen signups (the true launch blocker)**; 5.14.12 status page (hosted). 5.14.12 status page = ops (hosted), not code.
**Before 5.15 (launch gate) (HISTORICAL, 2026-06 — every item below shipped: 5.10.18 #267, 2.7 #269, 4.3 #275, 3.6 #276, 3.8 #277, 3.9 #278)**: gap-fills — ✅ 5.10.17 object tagging (#264); REMAINING: 5.10.18 (Content-Disposition, next), 2.7 (metered billing — SUPERSEDED as a launch gate by the 2026-09-21 quota-sold pricing decision; reporter stays dormant), 4.3 (bandwidth alerts — table exists, alert logic missing), 3.6 (audit viewer), 3.8 (revenue dashboard — only an estimate exists), 3.9 (cost dashboard). Then 5.15.3 landing page (code) + ops gates 5.15.2/5.15.4/5.14.12 + **5.15.5 (prod backend/durability + billing activation — the true launch blockers)** = launch ready. (5.15.1 done.)
**Post-launch, first 2 weeks**: ✅ ALREADY DONE ahead of schedule — 3.7 customer support view (#294), 3.10 admin notifications (#295), 3.11 abuse queue (#296). <!-- reconstructed: verified against git log -->
**Recent (2026-06-17)**: PR #312 merged (see Status line): range-GET passthrough, Phase 10.1-10.3 convergent encryption, permafrost rename, NVMe cache, `cmd/validate` + `scripts/validate-backends.sh`. PR #311 (2026-06-05+): Phase 9 zstd compression on chunked PUT/GET — Phase 9 COMPLETE.
**Cleanup**: delete orphan branch `phase-5.11.6-event-log-webhooks` (code is on main, merged via PRs #238/#239).
**Recent (2026-06-05)**: **Phase 8 COMPLETE.** Shipped 8.4.1 streaming PUT (#306), 8.6 GCI dedup dashboard (#307), 8.7 dedup GC with orphan reconciliation (#308), 8.8 dedup migration CLI (#309). Each reviewed by reading the actual code (the streaming/GC/migration phases carried real data-loss surfaces — all verified correct, e.g. GC's `last_accessed_at` grace guard protects in-flight PUTs, migration's verify-before-delete + flag-before-delete ordering). Also root-caused + fixed two recurring CI flakes (#310): `TestRequestQueue` (a real select race in `RequestQueue.Submit`) and `TestPipeline_Run` (racy transient-status assert). Chunking pipeline is now end-to-end + self-cleaning but **still ahead of launch** — 5.15.5 (prod durable backend + Stripe meter activation + reopen signups) remains the unstarted revenue blocker. Next in Tier-2 sequence: Phase 9 (compression).
**Recent (2026-06-04)**: Phase 8 chunking/dedup train shipped — **AHEAD of the launch sequence** (Phase 8 is post-launch on the roadmap; launch remains ops-gated on 5.15.4/5.15.5, unchanged by this work). PRs: 8.3-8.4 chunking migration (051) + dedup upload (#300), 8.5 dedup download (#301), `_global` shared chunk store fixing cross-tenant/cross-bucket dedup retrieval (#302), 8.6 bounded-streaming GET + per-chunk SHA-256 integrity (#303), SSE >256 MiB reject-guard — was silently storing plaintext (#304), 8.6.1 atomic manifest replacement on overwrite (#305). Three latent data-corruption/security bugs found & fixed by verification (cross-tenant 404, overwrite corruption, silent-plaintext). See the Phase 8 STATUS block for the sketch-vs-shipped map. Remaining chunking: **8.4.1 PUT-side streaming** (gap, now in plan as next), 8.6 GCI dashboard, 8.7 GC (delete from `_global`), Phase 10 real large-object encryption (interim: >256 MiB SSE rejected). Note: git "Phase 8.6/8.6.1" labels = 8.5 hardening, NOT plan 8.6.
**Recent (2026-05-31)**: Shipped 5.14.5→5.14.10 compliance sequence (#258-263) + 5.10.17 object tagging (#264) as a bottom-up stacked-PR train, all squash-merged and deployed to SLC prod (migrations 037-041 applied, verified `object_head_cache.tags` live). Fixed broken prod deploy — a 17-day-old orphan process held `/tmp/vaultaire-linux` busy (ETXTBSY blocked every scp); killed it and hardened deploy.yml to clear stale staging pre-scp, now self-healing (#265). Fixed flaky CI smoke test — `server.go:107` unconditionally health-checks `io.quotaless.cloud` at boot, whose DNS flakes in CI; added curl retries (#266). Quotaless removal DEFERRED to M12 exit (user not using it for a while); follow-up: make that boot health check conditional (it's the flake root cause). Orphan branch `phase-5.11.6-event-log-webhooks` still pending deletion.
**Recent (2026-05-26)**: Full plan audit. 5.14.1 GDPR export (PR #253), 5.14.2 legal docs (PR #254), 5.14.3 MFA Delete + SOC 2 (PR #255), 5.14.4 PQ SSE-S3 encryption (PR #256). 5.12.4 failover (PR #251), 5.12.5 backend health dashboard (PR #252). 5.11.12 CDN analytics shipped (PR #249), no longer stashed. 5.13 email infra shipped (PR #246). CI fixes (PRs #247, #248).
**Previous (2026-05-13)**: Phase 5.12.3 complete — iDrive + OneDrive fleet wired, end-to-end benchmarked on Mac + SLC. Pipeline components benchmarked (RS, FastCDC, zstd, AES-GCM). ContentLength passthrough added to engine PutOptions. Engine Delete fixed to target known backend only. Quotaless removed from SLC prod .env (account locked). OneDrive driver: streaming uploads (+150% concurrent), parallel range downloads (+28% single-file), DNS cache, fleet-wide TLS cache, token mutex, RateLimit tracking. iDrive driver: fixed-bucket pattern, materialize for Content-Length. Benchmark automation: `scripts/bench-vaultaire.sh`. Pipeline benchmark tool: `cmd/pipeline-bench/`.
**Previous (2026-05-12)**: Benchmark-driven optimizations — added Phase 5.12.7 (production transport tuning with adaptive MPU strategy: H1 vs H2, 4-16 parallel parts, 4MB buffers). Storage class routing added to 5.12.4. SSE-S3 managed encryption added to 5.14.4 (critical for Geyser multi-tenant). MFA Delete added to 5.14.3. Inventory reports added to 5.14.9. Cross-account access + IAM evaluator wiring added to 19.1. Lifecycle rules (12.1) expanded with S3 XML API compatibility. Quotaless endpoints updated (srv1/srv2/us DNS removed, account locked). Geyser: TLS certs now valid (InsecureTLS removable), H1 is 4.2x faster than H2 (Vail gateway preference).
**Recent (2026-06-02)**: Closed out all Tier-1 gap-fills + the 5.15 code gate. Shipped OneDrive/S3 upload perf (#279 parallel MPU for S3 backends, #280 HTTP/1.1 OneDrive upload +6-20%, #281 fleet-upload bench TEST 7). Key finding: OneDrive "12 MB/s" is per-single-file — multi-file + per-tenant concurrency (2.3x) gives 24-58 MB/s @ 3 tenants, ~100-200 fleet (NIC-bound); customers must parallelize. Landed 5.15.2 load-test gate (PR #282): SigV4 harness, 5 env-gated scenarios, pg pool 25→50. **Tier 1 is now code-complete; only ops gates (5.15.4/5.15.5) remain before launch.**
**Plan file**: THIS file (`docs/IMPLEMENTATION_PLAN.md`) is now canonical. <!-- reconstructed: the copies at /Users/viera/.claude/plans/zazzy-growing-treasure.md and .private/ZAZZY_PLAN_BACKUP.md are BOTH truncated at Phase 21.4 (1,980 lines) — Phases 21.5-30 below were rebuilt 2026-07-07 from internal cross-references, MASTER_PLAN_SUMMARY.md, and memory. Do not resume from the old copies. -->
**Master plan (historical)**: `/Users/viera/fairforge/vaultaire/.private/VAULTAIRE_MASTER_PLAN.md`

## Context

This plan consolidates and replaces the VAULTAIRE_MASTER_PLAN.md (steps 490+) with a practical, ordered roadmap. It covers everything from the dashboard/billing launch through the full storage pipeline, multi-protocol support, data intelligence, edge computing, federation, and beyond. Each phase is broken into small enough blocks that any session can pick up where the last left off.

**(2026-10-12)** The product is **Stored**, a product of FairForge, at **stored.cloud** (stored.ge and stored.pics are permanent aliases). Three ambitions on one engine: (1) least access to customer data — any access is a public event (the Trust lane: Phase 43, the transparency log F-16); (2) "Stripe for storage" — the developer surface (S3 + management API, Standard mode by default); (3) "it just works" storage for the mainstream crowd — the consumer surface (`stored.cloud/photos`, the apps; Private mode once recovery exists, F-19). Modes — **Private / Standard / Public** — are a property of the surface, never a user choice (F-15); "Vault" is a tier, not a mode. The one-page rules: `docs/ARCHITECTURE_CONTRACT.md`; the lane charters: `docs/surfaces/`.

## Architecture Decision: htmx + Go Templates

Single binary, no frontend build step, no npm, works with existing CI/CD. htmx adds SPA-like interactivity. All templates embedded via `//go:embed`.

## How to Use This Plan

- **Starting a new session?** Say: "Continue from Phase X.Y" and reference this file.
- **Each sub-step (X.Y)** is a self-contained unit of work — completable in one session.
- **"Test" sections** tell you how to verify the step works before moving on.
- **"Depends on" notes** indicate prerequisite phases.
- **Tier 1 is sequential** (each phase builds on the last). Tiers 2-4 have more flexibility.

## Session Handoff Points

Each handoff is a natural stopping point where the codebase is in a clean, committed state. When ending a session, commit/merge all work and update the **Status** line at the top of this file.

### How to resume

Say: **"Continue from Phase X.Y — [sub-step name]. Plan: docs/IMPLEMENTATION_PLAN.md"** — or use `/next` (generates the next-phase prompt from this file) or `/next-step` (implements the next phase in-session). <!-- reconstructed: updated plan path; old ~/.claude/plans copy is truncated -->

Both `/next` and `/next-step` should derive "the next phase" from the **Status** line at the top of this file: first unfinished launch-blocker if pre-launch, otherwise the top unstarted item in the Post-Launch 90-Day Priority Stack.

The new session should:
1. Read this plan file (check Status line for where we are)
2. Read `.private/CLAUDE.md` for infra/architecture context
3. (2026-10-12) Read `docs/ARCHITECTURE_CONTRACT.md` and the charter of the lane you work in (`docs/surfaces/*.md`); mode names and copy come from `modes.json`, never typed by hand
4. Read the `CLAUDE.md` in directories it will modify (per-directory docs)
5. Check `git log --oneline -5` and `git status` for current state

### Tier 1 Handoff Points

| After Phase | What's Done | Resume With | Context for Next Session |
|-------------|-------------|-------------|--------------------------|
| **0** (done) | DB migration, LoadFromDB, templates, sessions, router | Phase 1.1 | Dashboard skeleton serves HTML. Login/register pages render but POST handlers aren't wired — Phase 1 adds those. `dashboard.Deps` struct passes auth + sessions + DB to handlers. |
| **1.3** | Login, register, dashboard overview, bucket browser | Phase 1.4 | Customer can log in and browse buckets. Handlers are in `internal/dashboard/handlers/`, templates in `templates/customer/`. Context helper (`dashauth.GetSession`) provides session data. |
| **1.7** | Full customer dashboard | Phase 2.1 | All customer pages work. API key management, usage charts, settings. The dashboard reads real data from `quotaManager` and `engine`. Next session wires Stripe. |
| **2.6** | Stripe billing wired end-to-end | Phase 3.1 | **First revenue checkpoint.** Registration creates Stripe customer. Checkout → webhook → subscription stored in DB. Billing page shows invoices. `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET` must be in env. |
| **3.5** | Admin dashboard complete | Phase 4.1 | Admin can see all tenants, suspend/enable, adjust quotas. Suspension enforced in S3 handler. Seed admin role: `UPDATE users SET role='admin' WHERE email='...'` |
| **5** | Security polish done | Phase 5.10.1 | CSRF, flash messages, rate limiting, responsive CSS, error pages all in place. Dashboard is production-ready for beta customers. |
| **5.10.9** | CDN + S3 core hardened | Phase 5.10.10 | Buckets table, public bucket toggle, CDN handler, range requests, conditional requests, CORS all working. `cdn.stored.ge/{slug}/{bucket}/{key}` serves public content with caching. Video streaming works. | (history — 2026-10-12, WP-DOMAIN-1: public customer content moves to a separate non-brand content domain, the googleusercontent pattern; the brand hosts never serve customer HTML)
| **5.10.18** | S3 fully hardened | Phase 5.11.1 | +Versioning, notifications, object lock, presigned URLs, tagging, Content-Disposition, rclone/JuiceFS tested. Ready for serious S3 clients. |
| **5.11.12** | Core DX + CDN delight complete | Phase 5.12.1 | JSON management API, versioning header, idempotency, metadata, scoped keys, STS, event log, onboarding, cost comparison, data locality, free tier, CDN dashboard with previews + analytics. |
| **5.12.5** | All backends wired + failover | Phase 5.13.1 | iDrive, Lyve, Geyser, OneDrive registered in main.go (done in 5.12.3). Quotaless locked — skip until re-enabled. Multi-backend failover + circuit breaker + storage class routing. Backend health dashboard for admin. |
| **5.13.4** | Email infrastructure live | Phase 5.14.1 | Verification, password reset, and bandwidth alert emails actually send. SPF/DKIM/DMARC configured. |
| **5.14.12** | Launch compliance complete | Phase 5.15.1 | Legal docs, PQ+SSE-S3 encryption, MFA Delete, HIPAA/SOC2/GDPR ready, SSE-C, per-bucket regions, access logging, inventory reports, compliance dashboard, status page. |
| **V18.8** | Vault18 deep archive experience | Phase 5.15.1 | Ingest buffer pipeline, staged retrieval, dual-site tape, retrieval SLA, Glacier migration, retention policies, cost calculator, immutability certificates. The "Anti-Glacier" product is complete. |
| **5.15.4** | Pre-launch validation complete | Phase 6.1 | Graceful shutdown, load tested, landing page live, smoke test passed. **Tier 1 complete — product is launchable.** |

### Tier 2 Handoff Points

| After Phase | What's Done | Resume With | Context for Next Session |
|-------------|-------------|-------------|--------------------------|
| **6.4** | Traditional backends wired | Phase 6.5.1 | ✅ Already done via 5.12.3 (PR #244). Lyve, iDrive, Geyser, OneDrive all wired in `main.go`. Skip Phase 6.1-6.4 — go directly to 6.5 or Phase 7 when ready. |
| **6.5.6** | Filecoin Proven Storage complete | Phase R1 | Filecoin driver wired via Lighthouse, proof-of-storage API live, compliance certificates, deal lifecycle manager, dashboard page, Stripe tier. The differentiator. Full plan: `logical-hugging-muffin.md`. |
| **R6** | iDrive reseller integration complete | Phase 7.1 | Per-tenant iDrive sub-accounts via reseller API. Driver pool, lifecycle sync, quota sync, usage monitoring, white-label ready. |
| **7.6** | Smart tiering running | Phase 8.1 | Objects auto-migrate between tiers based on age/access. `object_locations` table tracks where each object lives. Blended cost tracked in `tenant_cost_daily`. |
| **9.3** | Chunk + compress pipeline | Phase 10.1 | Content-defined (Rabin) chunking, global dedup via GCI, zstd compression. Upload pipeline: `chunk → compress → store`. This is the foundation encryption and erasure coding build on. |
| **11.8** | Full storage pipeline | Phase P1 | **Core pipeline complete.** Full pipeline: `chunk → compress → encrypt → erasure code → store`. Self-healing repair. 11-nines durability. |
| **P6** | OneDrive fleet parity complete | Phase 12.1 | **Tier 2 complete.** OneDrive fleet stores parity shards for free durability layer. Fleet manager, health monitoring, backup verification all operational. |

### Tier 3-4 Handoff Points

| After Phase | What's Done | Resume With | Context for Next Session |
|-------------|-------------|-------------|--------------------------|
| **13.5** | Multi-protocol working | Phase 14.1 | S3 + WebDAV + SFTP all access same storage engine. Protocol abstraction layer in `internal/protocol/`. |
| **15.4** | Self-hosted distribution | Phase 16.1 | Docker image, ARM builds, Helm chart, docs. `docker-compose up` works. |
| **18.6** | Multi-tenant + reseller | Phase 19.1 | Tenant hierarchy, reseller dashboard, white-label, Stripe Connect. Platform is multi-tenant. |
| **21.6** | Cooperative + BYOS + LET Network | Phase 22.1 | Federation, BYOS, spoke nodes, credit system, LET guides + WHMCS module, peer discovery. The network effect is live, LET community engaged. |

### What to transfer between sessions

The per-directory `CLAUDE.md` files carry most of the context automatically. These are the things that WON'T be in the code:

- **Uncommitted decisions**: if you decided something mid-session but didn't code it yet, note it in this plan file before ending
- **Known issues**: if tests revealed a bug you didn't fix, note it here
- **External state changes**: if you created Stripe products, changed server config, added GitHub secrets, etc. — note it here or in `.private/CLAUDE.md`
- **Pre-existing test failures**: `internal/testing/api/TestClient_WaitForReady` has a pre-existing race condition (not from our work)

## Pricing Shape Decision (2026-09-21) — QUOTA-SOLD, ALL TIERS

**Decided by owner 2026-09-21**: all tiers sell whole-TB **quota** (Stripe plain subscription
price × TB quantity — the original Vault Flex mechanic, now universal), NOT metered stored
bytes. Rates unchanged (Vault $2.55/$2.00 · Smart/Standard $4.99/$4.49 · Performance parked
until $6.99). **Rate-discipline rule (load-bearing): quota is priced AT the linear per-TB
rates — never below** (S3/LET buyers are high-fill; discounted quota = adverse selection).
Launch mechanics: HARD quota (WP-1 enforcement + `tenant_quotas`, already live) + self-serve
prorated resize via the existing plan-change webhook; **no Stripe Billing Meters required at
launch** — 2.7's reporter stays dormant (egress = throttle-first at allowance, overage billing
= post-launch). Allowances key off QUOTA, not stored bytes: hot = 15% × quota (Smart), egress
= 0.5× quota/mo (Smart) / 1× restore allowance (Vault) / 1× (Performance). Optional add-on:
**pin-hot at $3/TB-mo × pinned quantity** (bytes exempt from demotion beyond the allowance;
crossover math intentionally pushes >⅔-pinned tenants to Performance). Full rationale +
margin-vs-fill tables: `.private/SMART_TIER_DESIGN.md` addendum 2026-09-21.
Supersedes: 2.7-as-launch-blocker (line above), 5.15.5 "create + attach Stripe Billing
Meters", and any "per-TB metered" plan mapping in 2.x — those stay accurate as HISTORY of
what shipped, but activation is no longer a launch gate.

## Migration Numbering

Migration numbers in this plan are **planning references**, not assignments. Existing migrations go up to `073_site_stats.sql` (as of 2026-09-30: 71 files numbered 003–073, 004 twice, 053 never existed; was `063_smart_promotions.sql` on 2026-09-23 and `052_chunk_encryption.sql` on 2026-07-07). <!-- verified against internal/database/migrations/ 2026-09-30 --> The next migration built gets `074`, regardless of which phase it comes from. Two PRs took the same number on 2026-09-27 (066, renumbered in #506) — re-check on a fresh `main` right before naming the file, and run `make test-db` after every pulled migration (`TestSQLLiteralsMatchSchema` prepares every SQL literal against the migrated test DB). Assign sequentially at build time — multiple phases reference the same numbers but only one migration can have each number. When starting a phase, check `ls internal/database/migrations/` for the latest number and use the next one.

## Documentation Practice

After completing each sub-step, update (or create) a `CLAUDE.md` in the directory you modified. These per-directory docs keep context local and prevent the root CLAUDE.md from bloating. Each should be <100 lines covering: what the package does, key types, how it connects to other packages, non-obvious decisions. Also document API endpoints, request/response formats, and error codes as they're built — this becomes the foundation for the developer docs in Phase 26.5.

## Critical Items That Must Start Early (regardless of phase order)

These have long lead times — start the PROCESS during Tier 1, even though full completion is later:
- **(2026-10-12) WP-DOMAIN-1 — stored.cloud canonical before Oct 31** (F-17) — host map `stored.cloud` / `app.` / `s3.` / `docs.` / `trust.` / `status.`, stored.ge + stored.pics served forever (web 301, S3 on both names, no redirect), public content on a non-brand domain; the Porkbun DNSSEC DS (pending since 2026-09-20); the Stripe webhook created on .cloud; ~92 files. Decide first: `*.s3.stored.cloud` (ACM $10/mo) vs path-style; s3 host proxied vs direct.
- **(2026-10-12) Master-key ceremony prerequisites (F-14)** — `libtss2-rc0` on the box, the dedicated service user + unit changes (`LoadCredentialEncrypted=`, mount namespacing), the three `age-plugin-sss` shares (YubiKey, paper identity, trustee), `server.go` reading `$CREDENTIALS_DIRECTORY`; the ~~`generatedObjectWriter` plaintext fix~~ (done #685) + WP-R3-1 before the flip; the runbook rewritten.
- **(2026-10-12) The EPYC SEV-SNP standby (F-18)** — buy as the Stage-5 standby, verify `sev_snp` = Y before paying; standby + PG replica + backup target + TPM escrow share now, the attested data plane (41.4/F-9, Trustee KBS on a second box) later. Prod stays outside a CVM before launch.
- **(2026-10-12) Transparency log, start now (F-16)** — signed statements + OpenTimestamps + a Rekor mirror at `trust.stored.cloud` from the first ceremony; the tile log and the external witness later; the library is an Engine-lane keystone.
- **(2026-10-12) Passcode recovery vault (F-19)** — the gate on Private-by-default for the consumer surface; interface defined in the Engine lane, first realm = the attested box; until it exists the consumer default is recoverable + "Advanced protection" opt-in.
- **Free tier decision** (Phase 5.11.10) — decide limits at launch, announce day one
- **SOC 2 evidence collection** (Phase 28.1) — start logging/documenting during Tier 1, formal audit in Tier 2+
- **SLA definition** (Phase 28.2) — publish alongside paid plans in Phase 2
- **API versioning headers** (Phase 26.2) — add `X-Vaultaire-Version` response header in Phase 0 (costs nothing, prevents breaking changes later)
- **Multi-region planning** (Phase 28.3) — start when you get the dev server
- **`stored` CLI/TUI** (Phase 26, PULL FORWARD to ~5.7) — target market (r/datahoarder, r/cloudstorage, self-hosters) strongly prefers terminal over web UI. Ship alongside or shortly after billing. See `docs/PRODUCT_FEATURES.md` for full spec. "Just storage, no bloat" branding.
- **rclone one-liner** — `stored rclone-config` outputs pre-filled config. Ship with CLI.
- **Desktop sync client** — `stored mount` wraps rclone (Phase ~5.7). Branded Wails app post-launch if demand warrants. NOT Electron, NOT WASM.
- **Mobile app** — camera backup MVP (privacy pitch vs Google Photos). PWA first (~5.4), native iOS/Android in Tier 3-4 if demand. See `docs/PRODUCT_FEATURES.md`. (2026-10-12: the consumer surface — `stored.cloud/photos` + apps, charter `docs/surfaces/CONSUMER.md`; Private by default ONLY once the passcode recovery vault exists (F-19), until then recoverable default + "Advanced protection" opt-in; copy = outcome sentences, never "zero-knowledge" before the audit)
- **PWA manifest** — add during Phase 5.4 responsive CSS work. Cheap, tests mobile demand before building native.
- **JuiceFS + rclone guides** (Phase 5.10.14, PULL FORWARD to launch) — costs zero code, just docs. Each guide is a standalone LET/Reddit post. Write alongside S3 compat testing. The JuiceFS one-liner is the single best top-of-funnel for LET users.
- **stored-cache agent** (Phase 16.5, PULL FORWARD to ~post-launch month 2) — simple Go binary, high LET value. Can ship before NAS agent (Phase 16) and before federation (Phase 20). Independent of most Tier 2-3 work.
- **AI-native DX** — the foundation is stuff you're already building: S3 compat (done), complete OpenAPI (5.11.0), CLI (26.4), actionable errors (5.10.15). Publish `llms.txt` alongside OpenAPI completion — it's a plain text file, zero effort. MCP/plugin manifests are optional bonuses in Phase 26.8, not the core strategy. The CLI IS the AI integration.
- **Complete OpenAPI spec** (Phase 5.11.0, CRITICAL) — current spec covers ~60% of routes. Must reach 100% before SDK generation, AI integration, or interactive docs. Every AI agent, code generator, and developer tool reads OpenAPI first.

## Deferred Decisions (documented, not forgotten)

These were evaluated and deliberately postponed. Revisit at the noted trigger point:
- **Per-operation rate limits** — generic per-tenant rate limiting ships in 5.11.0. Per-operation limits (different rates for ListObjects vs PutObject) are overkill pre-scale. **Revisit when**: tenant count exceeds 1,000, or a single tenant's ListObjects calls cause measurable load. Until then, one limit per tenant is simpler to reason about, simpler to document, and simpler to support.
- **Full API version translation** — header-only versioning ships in 5.11.1 (`X-Vaultaire-Version` on every response). Full version translation (old API shapes → current format, like Stripe's version pinning) deferred to Phase 26.2. **Revisit when**: you need to make a breaking change to the management API. S3 wire protocol is version-less by design (AWS has never versioned S3), so this only applies to the JSON management API.
- **Custom S3 SDKs** — NOT building them. **(2026-10-12 — narrowed, not reversed: the one SDK we DO build is the Private-mode key core, Ente's hierarchy F-12, as a Go module + WASM — an Engine-lane keystone that sits beside the AWS SDKs, or SSE-C per bucket, and never replaces them.)** boto3, aws-cli, rclone, and every language's AWS SDK already work with stored.ge because we're S3-compatible. Only the management API needs custom SDKs (Phase 26.6). This is a strength: developers don't learn a new SDK, they use the one they already know.
- **LET community presence** — start lurking on LET during Tier 1. Comment on storage threads. Build credibility before posting offers. Don't wait for Phase 27 (GTM) to start community engagement.
- **kTLS + io_uring zero-copy I/O** — kernel TLS offload for the CDN path (Netflix pattern: CPU 15%→2%) and io_uring for cache reads. Spec'd in `.private/ADVANCED_ARCHITECTURE.md` §"I/O Architecture". **Revisit when**: CDN has real sustained traffic and CPU becomes measurable; Go io_uring libraries need maturity. Not a launch or 90-day item. <!-- reconstructed: added from ADVANCED_ARCHITECTURE.md — was missing from plan -->
- **(2026-10-12) Virtual-hosted bucket names on `*.s3.stored.cloud`** — needs a Cloudflare ACM wildcard ($10/mo) vs path-style only at launch. **Decide in**: WP-DOMAIN-1 (F-17a), before the Cloudflare rules are redone.
- **(2026-10-12) `s3.stored.cloud` proxied or direct** — proxied keeps the edge rules and the 100 MB single-PUT limit (413 above it, multipart unaffected); direct exposes the origin IP. **Decide in**: WP-DOMAIN-1 (F-17b); revisit when a customer hits the limit.
- **(2026-10-12) Post-launch order fork (F-8)** — consumer crowd for the next year → 10.0 right after Stage 1a; otherwise the Stripe layer (31, 42.1, 26, docs/SDKs, WP-SSEC-1) first. **Decide in**: the first weekly owner decision batch after Oct 31.
- **(2026-10-12) TPM-resident KEK** (no exportable master) — follow-up WP after the first ceremony. **Revisit when**: the attested box exists (F-18), so the KEK can be released only to a measured image.

## Known Gaps (implemented but incomplete — fill in when revisiting those areas)

Audit 2026-04-24 found these partial completions. Core functionality works; these are polish items to address before launch.
- **Phase 2.5 billing page gaps** — Invoice history not rendered (template doesn't call `GetInvoices()` despite it existing in `stripe.go`). No "Pause Subscription" toggle, predictive billing, add-on management, or prepaid credit display. Stripe Portal covers invoices/pause for now. **Fill in during**: 5.11.8 (Cost Comparison Widget) when the billing page is already being expanded.
- **Phase 4.3 bandwidth banking + alerts** — DB schema exists (migration 020: `bandwidth_rollover`, `bandwidth_alerts` tables). ✅ Alert threshold checking + 80%/95% email/event alerts SHIPPED as 4.3 (#275). Still missing: rollover calculation + overage warnings (quota-sold launch = throttle-first at the allowance; overage billing is post-launch). **Fill in during**: stack #5 no-surprises billing / 5.11.12 (Bandwidth Budgets).
- **GLACIER storage class routes but can't restore (found 2026-07-29)** — storage-class routing (5.12.4) sends `GLACIER`/`DEEP_ARCHIVE` to Geyser, but the engine has zero `InvalidObjectState` handling: any GET on data past the ~13-day Vail staging window surfaces as a raw error (or worse, the failover path). **Fill in during**: V18.2. **2026-08-04 REPRICE DECISION (VAULT_SERIES_ECONOMICS.md, condition 4): the V18.2 MINIMUM recall slice is promoted to PRE-LAUNCH — and SHIPPED same day (#427 + #428, deployed + live-verified against the real tape library, incl. the restore-of-staged-object 403 found by the E2E). Option A holds; the ~Aug 22 Option B checkpoint is moot.** Minimum slice = typed `ErrArchived` in geyser.go, GET on archived → 403 `InvalidObjectState` XML, RestoreObject passthrough (`POST ?restore`), HEAD `x-amz-restore` status, dashboard Restore button + status — Glacier-compatible wire semantics so rclone/aws-cli restore workflows work unmodified. The full V18.2 (rehydration worker, bulk thaw, webhooks) stays first post-launch.
- **Archive-tier placement hole for >64 MB objects (confirmed 2026-07-30 E2E, flagged for plan 2026-08-04)** — archive/GLACIER-class objects **above the 64 MB chunking threshold enter the chunked path and land on the engine primary (iDrive), not Geyser** — the exact hole the `resilient` tier closed in #405 by skipping chunking (chunk blobs always live in the shared `_global` container on the primary). Consequence: the archive tier's core buyers (media archives — RAW batches, video masters, i.e. almost exclusively >64 MB files) silently get hot-tier COGS ($4.125/TB iDrive) under a $2/TB archive price, and the "it's on tape" claim is false for exactly those objects. Fix is the same gate resilient ships: archive-class skips chunking, stores whole on Geyser (deliberate trade: archive objects skip dedup — acceptable, tape economics don't need it). **This + V18.2 are the only two code gaps between the three-tier base layer and "build everything on top" (assessment 2026-08-04). CLOSED 2026-08-04 (PR #425)**: `chunkingDisabledByTier` now matches `GLACIER`/`DEEP_ARCHIVE` like `RESILIENT` — archive objects store whole on Geyser. Residual: an explicit `x-amz-storage-class` header at multipart *initiate* is not persisted (no storage_class column on `multipart_uploads`), so header-classed multiparts into an `auto` bucket still land on the primary; archive-tier *buckets* are covered (complete resolves `bucketTierStorageClass`).
- **"Smart" mid-tier hot→cold mover unverified (flagged 2026-08-04)** — the Standard-tier margin model assumes a 14-day hot window capping the iDrive share at ~15%, but no automated demotion job exists yet (7.3's hourly TieringEngine classifies generically; 7.6/8.1 are data-residency and the FastCDC chunker, NOT smart tiering — earlier mislabel). Until it runs, tier placement is static and the ~54-59% Standard margin claim is aspirational. Not launch-blocking (margin protection, not correctness), but it is the **next CODE item**: **Phase 5.15.8 Smart demotion job WP** (hot budget = 0.15 × quota, idle ≥14d or over-budget LRU, never <3d; spec `.private/SMART_TIER_DESIGN.md`). **CLOSED in code 2026-09-22/23 (#461 demotion + ledger, #462 read-time promotion)** — the job exists and ships flag-dark (`smart_demotion` OFF), so placement in prod is still static until the flag is enabled per tenant; WP-R13-1 and WP-R13-3 must land first (Review R13).
- **Lyve backend follow-ups (2026-07-29, from the readiness pass — all small)** — (1) homing guard: `HealthCheck` should compare `?rs-info` replicationPolicy against the driver region to catch misprovisioned buckets at boot; (2) wire Lyve as a replication/DR target (server-side cross-region copy measured ~47 MB/s, UploadPartCopy works for any-size objects); (3) per-region Lyve drivers mirroring `idrive_regions.go` for EU/APAC residency; (4) `RSListBillingData` → COGS reconciliation cron (pairs with meters work). Full capability matrix: `internal/drivers/lyve_README.md`. **Fill in during**: 5.12.5 (failover/routing) or a dedicated post-launch WP.

## Compliance Audit (2026-05-07) — Items Pulled Forward

These were scattered across Tiers 2-4 but are required at or near launch. Consolidated into Phase 5.14 below.

**Moved forward:**
- Phase 28.5 (data export + account deletion) → 5.14.1 (basic version at launch, GDPR requires it)
- Phase 27.1 (legal docs) → 5.14.2 (Privacy Policy + ToS + DPA required before first EU customer)
- Phase 28.1 (SOC 2 evidence) → 5.14.3 (start tracking now, not at Phase 28)
- Phase 10.1 (PQ encryption) → 5.14.4 (simplified whole-object, no chunking dependency)
- Phase 7.5 (per-bucket region) → 5.14.7 (iDrive has 14 regions, wire at launch)

**Added new (not previously in plan):**
- 5.14.5: HIPAA readiness (BAA template, emergency access, breach notification workflow)
- 5.14.6: GDPR and EU Data Act compliance (consent management, data residency documentation)
- 5.14.7: Per-bucket region selection (iDrive multi-region, data sovereignty labels)
- 5.14.8: SSE-C customer-provided encryption keys (shipped — single PUT/GET only, the whole object buffered; multipart answers 501, CopyObject unsupported → **WP-SSEC-1 (2026-10-12)**: streaming, multipart, copy, an "SSE-C required" bucket flag; the developer surface's Private-mode path beside the SDK)
- 5.14.9: Server access logging and inventory reports (S3-compatible audit trail)
- 5.14.10: Customer compliance dashboard + 5.14.11: Security hardening (headers, request limits, admin 2FA enforcement)

- 5.14.12: Public status page (`status.stored.cloud`; `status.stored.ge` an alias — WP-DOMAIN-1, 2026-10-12)

## Vault18 Deep Archive Sub-Phases

*Vault18 is the "Anti-Glacier" product — now sold as **Vault Flex**: $2.55/TB/mo monthly · $2.00/TB annual prepaid, any size, quota-sold (Founders $1/TB capped at 100 TB, 3–10 TB/acct; the Vault1/3/5/10/18/50/100 pack ladder is GONE — 2026-08-04 linear reprice + 2026-09-21 quota-sold decision), zero retrieval fees, dual-site tape (LA + London). Sits between Phase 5.14 and Phase 5.15 in the Tier 1 sequence. Full plan: `.private/VAULT18_LAUNCH.md`.*

**Measured reality (2026-07-29 Geyser probes — full data in `benchmark-results-2026-07` memory):** Geyser is TRUE Glacier — objects past the Vail disk-staging window (≤13 days) are `StorageClass: GLACIER` and refuse direct GET (`InvalidObjectState`); the engine currently has NO handling for this, so V18.2 is promoted to the first post-launch WP. RestoreObject works (bulk-friendly); **tape recall measured <3 min** (idle library) — "restores begin in minutes" is honest copy. Restored copies read ~5 MB/s/stream from staging and expire per `Days` → rehydrate to a hot tier instead of serving from staging (the Geyser×Wasabi integration pattern). Ingest re-measured 2026-07-29: **227 MB/s sustained over HTTP/1.1 multi-connection** (the earlier 27.7 MB/s "ceiling" was our forced-h2 client, fixed in `geyser.go`); reads ~2.5-3 MB/s/stream, scale with fan-out (76 MB/s @ 32 streams). **Access prereqs (owner):** fetch 2nd Geyser API key (rotation + test if ceiling is per-key), console session for `geyser_admin.go` (per-tier buckets, London recreation, billing endpoint), ask Geyser support for object-lock-enabled bucket (Vail returns proper object-lock error codes — V18.5 may be a provisioning flag), restore SLA under contention, contract restore/egress pricing. **Vail management API DISCOVERED (2026-07-29): live at `la1.geyserdata.com/sl/api/` (401 = present, needs auth) — full spec committed at `docs/references/vail-api-guide.pdf`. It exposes per-bucket `locking` (object lock), `restore` (automatic read-through recall — could delete half of V18.2's orchestration), `versioning`, lifecycles, per-object `createObjectClone` (clone to another storage "as a restore that persists N days" = rehydration as a management primitive), and clone verification (integrity attestation). Auth: `POST /sl/api/tokens` with console username/password (+MFA challenge) → JWT. [YOU]: try your Geyser console creds there; if they work we get the whole control plane, else ask Geyser for scoped API access.**

| Phase | Description | Status |
|-------|-------------|--------|
| V18.1 | Geyser tape ingest buffer pipeline (iDrive hot → Geyser cold) | NOT STARTED |
| V18.2 | Restore orchestration + hot rehydration (InvalidObjectState → RestoreObject → rehydrate to hot tier; Glacier-compatible wire semantics) | **MINIMUM RECALL SLICE SHIPPED 2026-08-04 (#427 + #428, live-verified)**: ErrArchived + 403 InvalidObjectState + RestoreObject passthrough + HEAD x-amz-restore + dashboard Restore button; real-tape E2E passed (evicted GET→403, restore→recall <4 min→readable) and #428 fixed restore-of-staged-object (Vail answers InvalidObjectState for that too — was a 500, now 403). Full orchestration (rehydration worker, bulk thaw, webhooks) stays **first post-launch WP** |
| V18.3 | Dual-site tape replication (LA + London + **São Paulo** — 3 sites confirmed via `/api/sites` 2026-07-29; our collection is currently **5 TB "Single Copy" LA**, so this is a provisioning/cost change, not code) | NOT STARTED |
| V18.4 | Glacier migration tooling (`aws s3 sync` from Glacier → stored.cloud) | NOT STARTED |
| V18.5 | Retention policies + immutability certificates (WORM integration) | NOT STARTED — ask Geyser for lock-enabled bucket first |
| V18.6 | Cost calculator (vs Glacier, Wasabi, B2 — show $0 retrieval savings) | NOT STARTED |
| V18.7 | Quota pricing integration — Stripe price × TB quantity for Standard-monthly/annual + Vault-monthly/annual, Founders + LET2026 coupons (NO packs, NO meters; rescoped 2026-09-21) | OWNER: Stage 4 (week of Oct 5) — create LIVE prices + one real-card cycle |
| V18.8 | Customer-facing deep archive experience (dashboard, restore status) | NOT STARTED |
| V18.9 | Backup-tool-aware routing (restic/borg index+snapshot keys hot, data packs to tape) | NOT STARTED |

---

# TIER 1: DASHBOARD & BILLING LAUNCH (Ship before accepting paying customers)

*(2026-10-12) In the shipped-history sections below "stored.ge" is the brand as it was written at the time; the brand is Stored and the canonical domain stored.cloud (F-17). Mode words follow F-15: Private / Standard / Public.*

---

## Phase 0: Dashboard Foundation
*No dependencies — this is the starting point.*

> **Phase 0 COMPLETE** — PR #173, merged 2026-04-05.

### 0.1: Database Migration
**File**: `internal/database/migrations/018_dashboard_foundation.sql`
- Dashboard tables: `dashboard_sessions`, user preferences, session tracking
- `AuthService.LoadFromDB()` — load users, tenants, API keys from PostgreSQL on startup

### 0.2: Template Infrastructure
**Files**: `internal/dashboard/templates/` (new), `internal/dashboard/handlers/` (new)
- Go `html/template` with `//go:embed` for single-binary deployment
- Base layout template with navigation, flash message slot, CSRF token
- `dashboard.Deps` struct passes auth + sessions + DB pool to all handlers

### 0.3: Session Management
**File**: `internal/dashboard/handlers/sessions.go`
- PostgreSQL-backed sessions (`dashboard_sessions` table) with IP/user-agent tracking
- 24-hour session lifetime, secure cookie settings (HttpOnly, SameSite=Lax)
- `dashauth.GetSession(r)` context helper for all handlers

### 0.4: Router Setup
**File**: `cmd/vaultaire/main.go`
- Mount dashboard routes under `/dashboard/`, `/login`, `/register`
- Static file serving for CSS/JS (embedded)
- Health check at `/health`

**Test**: `go test ./internal/dashboard/...` — server starts, login page renders, session roundtrip works.

---

## Phase 1: Customer Dashboard
*Depends on: Phase 0 (templates, sessions, router)*

> **Phase 1 COMPLETE** — PRs #174 (1.1), #192 (1.2-1.3), #194 (1.5).

### 1.1: Login & Registration
**File**: `internal/dashboard/handlers/account.go`
- POST `/login` — bcrypt verify, create session, redirect to `/dashboard`
- POST `/register` — create user + tenant + API key, bcrypt hash, redirect to login
- Server-side validation with flash message feedback

### 1.2: Dashboard Overview
**File**: `internal/dashboard/handlers/overview.go`
- Storage used / quota remaining progress bar
- Recent activity feed (from `object_head_cache` timestamps)
- Quick action buttons (create bucket, upload file)

### 1.3: Bucket Browser
**File**: `internal/dashboard/handlers/buckets.go`
- List buckets with object count, total size
- Drill into bucket → list objects with size, last modified, content type
- Upload/download/delete actions per object

### 1.4: API Key Management
**File**: `internal/dashboard/handlers/apikeys.go`
- List API keys (masked), create new, revoke existing
- Show access key + secret key ONCE on creation (never stored in plaintext display)

### 1.5: Usage Charts
**File**: `internal/dashboard/handlers/usage.go`
- SVG bandwidth chart (last 30 days) generated server-side
- Storage growth over time from `tenant_quotas` snapshots

### 1.6: Account Settings
**File**: `internal/dashboard/handlers/settings.go`
- Change password, update email
- Delete account (soft delete with 30-day grace)

### 1.7: Help Page
**File**: `internal/dashboard/handlers/help.go`
- S3 endpoint configuration guide
- Code examples (curl, Python boto3, AWS CLI, rclone)

**Test**: Register → login → create bucket → upload file → see it in browser → download → delete → see usage chart update.

---

## Phase 2: Stripe Billing
*Depends on: Phase 1 (dashboard for billing page)*

> **Phase 2 COMPLETE** — PR #195 (catch-up 3.1-5.2 including billing), PR #269 (2.7 metered billing). **2026-10-18: superseded as the billing model by WP-PAY (below, after 2.7): the entitlement record in our DB, Stripe as one provider, BTCPay as the second — `docs/PAYMENTS.md`.** The sub-steps stay as history of what shipped.

### 2.1: Stripe SDK Integration
**Files**: `internal/billing/stripe.go` (new), `internal/billing/webhook.go` (new), migration `019_stripe_billing.sql`
- Add `stripe-go` SDK dependency
- `BillingService` struct: create Stripe customer on registration, store `stripe_customer_id` in `tenants` table
- Migration: add `stripe_customer_id`, `stripe_subscription_id`, `plan_name`, `plan_status` columns to `tenants`
- Env vars: `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`

### 2.2: Checkout Flow
**File**: `internal/dashboard/handlers/billing.go`
- "Upgrade" button → `stripe.CheckoutSession` with price ID from env
- Success/cancel URLs back to `/dashboard/billing`
- Stripe Portal link for subscription management (invoices, payment method, cancel)

### 2.3: Webhook Handler
**File**: `internal/billing/webhook.go`
- Verify webhook signature with `STRIPE_WEBHOOK_SECRET`
- Handle events: `checkout.session.completed`, `customer.subscription.updated`, `customer.subscription.deleted`, `invoice.payment_succeeded`, `invoice.payment_failed`
- On `checkout.session.completed`: update tenant's plan_name, plan_status, adjust quotas
- Idempotency: `stripe_events` table deduplicates webhook replays

- Wire `POST /webhook/stripe` route in server.go
- Verify `Stripe-Signature` header before processing any event
- Return 200 immediately, process asynchronously (buffered channel)
- Log all events to `stripe_events` table for audit trail

### 2.4: Quota Adjustment
**File**: `internal/billing/stripe.go`, `internal/api/quota_management.go`
- On plan change webhook: look up plan limits → update `tenant_quotas` row
- Plan mapping: free (5GB), Vault Flex (quota × TB, hard cap), Standard (quota × TB, hard cap), Performance (parked — post-launch at $6.99). <!-- 2026-09-21: was "Vault3 (3TB), Standard/Performance (per-TB metered)" — quota-sold on all tiers now -->
- Downgrade guard: if current usage > new plan limit, warn but don't delete data

### 2.5: Billing Dashboard Page
**File**: `internal/dashboard/handlers/billing.go`
- Current plan name, status, next billing date
- Link to Stripe Portal for invoice history and payment management
- Usage vs. quota progress bars

### 2.6: Plan Upgrade/Downgrade
**File**: `internal/dashboard/handlers/billing.go`
- Three-column pricing card (Vault / Standard / Performance) — Performance is PARKED at launch (no Stripe product; launches post-launch at $6.99), so the launch card set is Vault Flex + Standard
- "Current plan" badge, "Upgrade" / "Downgrade" buttons
- Stripe Checkout for upgrades, Stripe Portal for downgrades

### 2.7: Metered Billing
**File**: `internal/billing/metered.go` (new), migration `043_metered_billing.sql`
> **Phase 2.7 COMPLETE** — PR #269
- Report storage and egress usage to Stripe Billing Meters hourly
- `STRIPE_METER_STORAGE` + `STRIPE_METER_EGRESS` env vars (meter event names)
- Storage: "last value" aggregation (current TB), Egress: "sum" aggregation (GB transferred)
- Metered reporter goroutine: query `tenant_quotas` + bandwidth tracking, emit `stripe.MeterEvent` per tenant

**Test**: Register → see free plan → Stripe checkout → webhook fires → plan updated → quota increased → billing page shows correct plan. Metered: upload data → wait for hourly report → verify Stripe dashboard shows meter events.

---

## WP-PAY: payments without a single provider (added 2026-10-18, Stage 1a)

*Design of record: `docs/PAYMENTS.md`. Owner decisions F-22 … F-30 (table below). Worker prompts: `docs/reviews/WP-PAY-PROMPTS.md`. The owner's ask (2026-10-18): "make it so we can easily switch off from Stripe and add crypto payment as well."*

**What it changes.** Today entitlement lives at Stripe: `applyHouse` (`internal/billing/webhook.go:441`) grants or clears the house from the subscription's status and nothing in our DB says when a house was paid through (prod 2026-10-18: 0 subscriptions, 0 floor rows — nothing to migrate). WP-PAY puts the record in our DB (`entitlements.paid_through` + grace, a `payments` ledger), makes Stripe one `payments.Provider` whose payments extend the date and whose status never shortens it, adds BTCPay Server (self-hosted, BTC on-chain first) as a second provider, and gives the owner per-provider flags (`pay_stripe`, `pay_btcpay`) plus a bulk "stop Stripe renewals" action. Billing never deletes data (`ClearHouse` is unchanged). One PR each, the provider-neutral record first with no customer-visible change, every later PR flag-gated.

**Lane and order.** Developer surface (billing API, dashboard) — the surface lane beside the Engine lane's WP-R8-2 / WP-R3-2 under the WIP limit; it displaces nothing. WP-PAY-0 and WP-PAY-1 start now; WP-PAY-4 waits for F-23 (where BTCPay runs) but its lab spike does not.

| WP | Delivers | Files | Migration | Red tests (fail on today's `main`) | Prod proof / lab | Rollback |
|---|---|---|---|---|---|---|
| **WP-PAY-0** (= WP-R10-4, F-29) ✅ **DONE 2026-10-20** (#807 `8aa2c44`, prod-proven: Bench2's `vault100` quota unchanged, the pages mounted; the tier labels stay quota labels, a pack-label tenant is told to ask support; the revenue trend removed; `docs/reviews/WP-P19.md`) | One product: delete the legacy pack path and the metered reporter — `registerStripePlans`, `HandleUpgrade`, pack `CreateCheckoutSession`, `MeteredReporter`, `STRIPE_METER_*`, `AccruedCents`, the `$3.99` ladders; `prices.json` the one price source; `HandleManageBilling` keeps working | `internal/billing/{metered.go,stripe.go}`, `internal/api/server.go:521-560,1096-1110`, `internal/dashboard/handlers/{billing,admin_revenue,admin_costs}.go`, `internal/billing/CLAUDE.md` | none (`spending_cap_cents`, `metered_usage_reports` drop with D-12 in WP-PAY-7) | `TestNoMeteredReporter` (symbol gone), `TestBillingPage_NoAccruedEstimate`, admin revenue page renders houses only | prod: deploy, `/dashboard/billing` and `/admin/revenue` render; boot log has no "metered billing dormant" line | revert the PR |
| **WP-PAY-1** ✅ **DONE 2026-10-20** (#802 `0a121b6`, migration **097**, prod-proven: 0 rows, `entitlement_lapse` ok with `would_clear=0`, the three series on `/metrics`; departures from § 1 and residuals in `docs/reviews/WP-P18.md`) | The entitlement record: migration 097 (`entitlements`, `payments`, `payment_events`, `invoice_counters`); package `internal/payments` (`Grant`/`Extend` under the tenant advisory lock, `usage.SetHouseTx`, the ledger, `Status()`); the Stripe webhook dual-writes the entitlement (`paid_through` = fetched `current_period_end`) beside the old columns; the `entitlement_lapse` job (daily 07:30 UTC, flag `entitlement_lapse` OFF = count-only; skips rows whose provider was unready in the last 24 h; reconciles floors == entitlement); backfill at first run; `EraseRows` + export sections cover the tables | `internal/database/migrations/097_payments.sql`, `internal/payments/{entitlement,ledger,lock,status}.go`, `internal/usage/floor.go` (`SetHouseTx`), `internal/billing/webhook.go`, `internal/api/{jobs_wiring.go,entitlement_lapse.go,flags_wiring.go}`, `internal/account/erase.go`, `internal/api/account_export_sections.go` | **097** | `TestGrant_ExtendsFromMaxOfPaidThroughAndNow`, `TestGrant_Idempotent`, `TestStripeWebhook_WritesEntitlement_PaidThroughIsCurrentPeriodEnd`, `TestStripeWebhook_PastDueKeepsHouse_CanceledKeepsUntilDate` (flag on), `TestLapseJob_ClearsPastGrace_UsedBytesUntouched`, `TestLapseJob_SkipsUnreadyProvider`, `TestLapseJob_FlagOff_CountsOnly`, `TestLapseJob_ReconcileHealsFloors`, `TestSQLLiteralsMatchSchema` / `TestErasedTablesCoverSchema` / `TestExportSectionsCoverSchema` naming the new tables | prod: 097 applied, 0 rows, `entitlement_lapse` runs `ok` with `would_clear=0`; lab: the signed Stripe fixtures (`webhook_signed_test.go`) through every status | flag off (default); tables additive, nothing reads them in the old build |
| **WP-PAY-2** ✅ **DONE 2026-10-20** (#808 `374e582`, no migration, ships dark — prod has no Stripe key: `pay_stripe` off, `vaultaire_payment_provider_ready{provider="stripe"} 0`; departures D-P19-B1..B9 and residuals RP21-1..5 in `docs/reviews/WP-P19.md`) | The `payments.Provider` interface + `Registry`; `billing.StripeService` implements it; flags `pay_stripe` (default = key set) / `pay_btcpay` (OFF); the dashboard imports `payments` only (`HouseBilling` returns `payments.Status`, `HandleManageBilling` via `ManageURL`); customer creation lazy in `StartCheckout` only (the two registration-time call sites go); the deletion runner cancels through `Provider.Cancel` (defers only `renewal='auto'` + unconfigured); "provider off" semantics (409 on checkout/resize, route stays mounted, page sentence); with `entitlement_lapse` ON the webhook stops clearing on terminal statuses (`renewal='prepaid'` instead) | `internal/payments/{provider.go,registry.go}`, `internal/billing/{stripe.go,house.go,webhook.go}`, `internal/dashboard/{router.go,handlers/billing.go,handlers/billing_house.go}`, `internal/api/{server.go,deletion_runner.go,flags_wiring.go}` | none | `TestDashboardImportsNoStripe` (`go list -deps` has no `stripe-go`), `TestCheckout_ProviderOff_409`, `TestWebhookRoute_MountedWhileFlagOff`, `TestRegister_CreatesNoStripeCustomer`, `TestDeletionRunner_PrepaidRowNeedsNoCancel`, `TestApplyHouse_TerminalStatusStopsRenewal_NotClear` (flag on) | prod: deploy, register a tenant → no Stripe call attempted (log), `/dashboard/billing` free-tier sentence; lab: Stripe test mode end to end | flags; the webhook branch is behind `entitlement_lapse` |
| **WP-PAY-3** ✅ **DONE 2026-10-20** (#813 `5cb3a57`, migration **098**, prod-proven: `payment_reminders` registered and run `ok` with 0 candidates, the new series at 0, rule file written but not installed — the [OPS] block; a manual payment on tenant zero not done, the owner's call; departures D-P20-A1..A12 and residuals RP23-1..5 in `docs/reviews/WP-P20.md`) | Receipts (`invoice_counters`, gapless `ST-<year>-<n>`, the receipt page), reminders (`payment_reminders` job hourly: T−14/−3/+1/+grace−1 to `prepaid` rows, templates `renewal_reminder.html`, `house_lapsed.html`, `payment_receipt.html`), the billing page on the entitlement (paid through, renew, receipts), admin tooling (`GET/POST /api/v1/admin/tenants/{id}/payments`, `POST …/entitlement`, `POST …/refunds`), every row audited (`billing.*`), metrics + `deploy/monitoring/vaultaire-payments.yml` | `internal/payments/{receipts,reminders,admin}.go`, `internal/api/{admin_payments.go,jobs_wiring.go}`, `internal/email/templates/*.html`, `internal/dashboard/templates/customer/billing.html`, `internal/dashboard/templates/admin/tenant_detail.html`, `deploy/monitoring/vaultaire-payments.yml` | none (095 holds `invoice_counters`) | `TestReceiptNumbers_GaplessUnderConcurrency`, `TestReminders_EachStepOnce_PrepaidOnly`, `TestAdminManualPayment_GrantsAndAudits`, `TestAdminPaidThroughAdjust_RequiresReason`, `TestRefundRecord_ShortensPaidThroughWhenAsked`, rule file parses (`promtool`) | prod: a manual payment on tenant zero grants a house and a receipt renders; the reminders job runs `ok`; rules installed; lab: the e-mail steps against a fake clock | flags; the admin routes are admin-only |
| **WP-PAY-4** (after F-23 for the box; the lab first) ✅ **CODE DONE 2026-10-21, dark** (#858 lab captures + client + verifier + `ClaimEvent`, #862 provider + state table + wiring; the lab ran BTCPay 2.4.5 on regtest — 84 deliveries captured, six design facts corrected in `docs/PAYMENTS.md` § 3.2/3.3; prod: no `BTCPAY_*` → no route, `pay_btcpay` OFF; the first real proof is the owner's single mainnet invoice after F-22/F-23 and click-list items 2–4; `docs/reviews/WP-P24.md`) | BTCPay provider: Greenfield client (create / fetch invoice, payment-methods, mark, refund), the webhook (`BTCPay-Sig` constant-time, 1 MiB cap, dedup on `COALESCE(originalDeliveryId, deliveryId)` through `ClaimEvent`, fetch-not-trust, tenant from our pending row ≠ metadata → refused), the state table of `docs/PAYMENTS.md` § 3.3 (grant on `Settled` only; `Expired(partiallyPaid)` → credit; paid late in full → automated mark Settled; `overPaid` → credit), per-invoice `speedPolicy` by amount, the resize top-up invoice, readiness (health + store read, 60 s), env `BTCPAY_URL`/`BTCPAY_STORE_ID`/`BTCPAY_API_KEY`/`BTCPAY_WEBHOOK_SECRET`; **the lab spike**: BTCPay regtest in Docker, every event captured into `testdata/` | `internal/payments/btcpay/{client,provider,webhook,states}.go`, `internal/api/server.go` (route `/webhook/btcpay` when secrets set), `internal/api/backend_probes.go`-style readiness, `docs/PAYMENTS.md` (the lab's corrections) | none | `TestBTCPaySig_RejectsWrongSecret_ConstantTime`, `TestBTCPayWebhook_RedeliveryIsDuplicate`, one test per row of § 3.3 (`TestBTCPay_Settled_Grants`, `_ExpiredPartial_Credits`, `_PaidLateInFull_MarksSettled`, `_Processing_NoGrant`, `_Invalid_NoGrant`, `_OverPaid_Credit`), `TestBTCPay_ForeignInvoice_200NoApply`, `TestBTCPay_Resize_TopUpAmount`, `TestBTCPay_Ready_FalseAfterTwoFailures` | lab only until the box exists: regtest end to end, a real delivery's bytes verified; prod: route not mounted (no secrets), flag OFF; then one mainnet invoice with the owner's own money | flag OFF; route absent without secrets |
| **WP-PAY-5** ✅ **CODE DONE 2026-10-22, dark** (Stage 1a Prompt 25: Part 0 #867 RP30-1/3…7 — the BTCPay webhook re-reads under the lock, releases non-applied claims, never 5xx on a permanent refusal; #869 WP-PAY-5 + RP30-2 the implicit switch; `docs/reviews/WP-P25.md`; no migration) | Switching: `StopRenewal`/`ResumeRenewal`, "Switch to crypto at renewal" (cancel_at_period_end, `pending_provider`, the first crypto invoice from T−14 d extending from `paid_through`), "Switch to card" (Stripe Checkout `subscription_data.trial_end = paid_through`, ≥ 48 h rule), the portal-uncancel handling (pending crypto invoice marked Invalid), the double-period detector (metric + audit + operator e-mail), the bulk `POST /api/v1/admin/payments/stripe/stop-renewals|resume-renewals` with `dry_run` default | `internal/payments/switch.go`, `internal/billing/{house.go,webhook.go}`, `internal/payments/btcpay/provider.go`, `internal/api/admin_payments.go`, dashboard billing page | none | `TestSwitchToCrypto_NoOverlapNoGap`, `TestSwitchToCard_TrialEndIsPaidThrough`, `TestSwitchToCard_Inside48h_StartsNow`, `TestPortalUncancel_InvalidatesPendingInvoice`, `TestDoublePeriod_DetectedAudited`, `TestBulkStop_DryRunWritesNothing`, `TestBulkStop_PerTenantAudit` | lab: Stripe test mode + regtest together; prod: the bulk dry run on 0 subscribers answers an empty list | flags; the bulk action is dry-run by default |
| **WP-PAY-6** (optional, after a month of on-chain) | Lightning (`BTCPAYGEN_LIGHTNING=lnd`, inbound channels, hot-balance cap alert + sweep runbook); Monero plugin evaluation gated on 43.5 | the BTCPay box; `docs/PAYMENTS.md` § 3.6 | none | the § 3.3 tests with `paymentMethodId = BTC-LN` fixtures | lab regtest Lightning; prod: one small mainnet LN invoice | turn the method off in the store |
| **WP-PAY-7** (two deploys after WP-PAY-2) ✅ **DONE 2026-10-21** (#834 readers, #836 writers + `charge.refunded`, #839 migration 100 `d40040f`, prod-proven: the columns and tables absent, 0 errors across the switch; the creators in 018/019/043/056/066/072 emptied so a re-run never re-adds them; `docs/reviews/WP-P23.md`. The `internal/billing` → `internal/payments/stripe` rename is NOT done — cosmetic, open) | Contract: admin revenue / costs / support / tenants pages read `entitlements` + `payments` (= WP-R10-5); the deletion runner reads the entitlement; drop `tenants.{stripe_subscription_id,subscription_status,house_period,deletion_stripe_cancelled_at}` readers then the columns; D-12's `DROP TABLE IF EXISTS subscriptions, billing_charges, metered_usage_reports`; `internal/billing` → `internal/payments/stripe` (cosmetic) | the dashboard admin handlers, `internal/api/deletion_runner.go`, migration | **next free number** at build time (additive drop after both slots run the new build) | the schema tests after the drop; `TestAdminRevenue_FromLedger` | prod: pages render from the ledger; the dropped columns absent | the drop migration is the one irreversible step — only after a backup |

**Open decisions this section needs:** F-22 (BTCPay vs hosted), F-23 (where it runs), F-24 (coins, Lightning), F-25 (which accounts), F-26 (grace), F-27 (Stripe default at launch), F-28 (refund policy), F-29 (metered tier), F-30 (tax posture after counsel). **[YOU] click-list:** `docs/reviews/WP-PAY-PROMPTS.md` § Owner click-list.

## WP-CAP: vendor capacity — what we bought, what we hold, when it renews (added 2026-10-20, Stage 1a)

*Why now.* The original iDrive reseller account died on 2026-09-07 because its free year ended on 08-27 with no card, no renewal e-mail and nothing on our side that knew a renewal date existed (`.private` memory, 12 days unnoticed). On 2026-10-20 the owner paid it again — **invoice 105170: "Yearly (1 TB)", $24.75 + $1.63 tax = $26.38, Mastercard ·5563 on file, term 2026-10-20 → 2027-10-20, over-quota $0.005/GB-month ($5.12/TB-month, billed, never blocked), egress $0.01/GB beyond 3× stored** — and prod moved back to `STORAGE_MODE=idrive` the same evening. Every Standard byte now lands in a 1 TB block that nothing measures: we export no bytes-per-backend gauge, the reseller sub-user quota is informational, and the only vendor-side numbers are in a console behind a reCAPTCHA login. Phase 5.12.9 named this in 2026-04 ("alert at 80 % capacity") and never shipped; this WP supersedes it.

**The rule of record (owner, 2026-10-20).** A yearly block costs $2.06/TB-month in year one and $4.125 at renewal; overage costs $5.12. Buy the next block when the months you expect to sit above the block exceed ~40 % of the months left in the term (year one) or ~80 % (renewal pricing); otherwise ride the overage and right-size at renewal. The trigger that pages is 80 % of the block on a rising slope; the next block on this account is 10 TB ($247.50 year one). Egress: 3× stored free, so a 1 TB block allows 3 TB out per month — one benchmark run can exceed both.

**Lane and order.** Trust lane (operator visibility), one prompt; it displaces nothing and needs no key ceremony. Migration **099** (098 is WP-PAY-3's, Prompt 20). Ships dark: without a plan row the job counts and the alerts for that backend stay silent, except `BackendPlanUnknown`, which is the point.

| WP | Delivers | Files | Migration | Red tests (fail on today's `main`) | Prod proof / lab | Rollback |
|---|---|---|---|---|---|---|
| **WP-CAP-1 A — the plan record** ✅ **DONE 2026-10-21** (#820 `e62ff68`, prod-proven: 099 applied, the owner's idrive row entered from invoice 105170 and audited, the System card renders; `docs/reviews/WP-P21.md`) | Migration 099 `backend_plans` (one row per registered backend: `vendor`, `plan_name`, `capacity_bytes`, `egress_free_multiple` (iDrive 3, NULL = unmetered), `overage_usd_per_tb_month`, `egress_usd_per_gb`, `term_start`, `term_end`, `auto_renew`, `payment_on_file`, `invoice_ref`, `note`, `updated_at`, `updated_by`); `GET/PUT /api/v1/admin/backends/{backend}/plan` (admin JWT, validated: `term_end > term_start`, `capacity_bytes > 0`, the backend registered; every PUT an `audit_logs` row `backend.plan_set`); the System page shows per backend bought / held by our rows / held by the vendor / free egress left this month / renews in N days / overage rate. Infrastructure, not tenant data: excluded from `EraseRows` and the export by name in the schema-walk tests | `internal/database/migrations/099_backend_plans.sql`, `internal/api/backend_plan.go` (+ admin routes), `internal/dashboard/handlers/admin_system.go`, `internal/account/erase.go` (exclusion list), `internal/api/account_export_sections.go` (exclusion) | **099** | `TestBackendPlan_PutValidatesTermAndCapacity`, `TestBackendPlan_UnknownBackend404`, `TestBackendPlan_AuditRow`, `TestErasedTablesCoverSchema` / `TestExportSectionsCoverSchema` naming `backend_plans` as excluded, `TestSQLLiteralsMatchSchema` | 099 applied; the owner enters the invoice's row for `idrive` (1 TB, 2026-10-20 → 2027-10-20, 3×, $5.12, $0.01, card on file, "invoice 105170") through the admin API; `geyser` (100 TB minimum, $155/mo) and `lyve` (promo, no cap) rows as F-31 decides | additive table; the old build ignores it |
| **WP-CAP-1 B — the measure** ✅ **DONE 2026-10-21** (#820, prod first run `ok`: idrive 2.3 GiB / lyve 74.5 GiB / wasabi 2.3 GiB, gauges on `/metrics`; one amendment: `retired_blobs` records no size, so it is reported as a ROW COUNT `retired_blobs_rows`, not bytes; vendor view off until the owner's key — RP25-1) | The daily **`backend_capacity`** job (08:30 UTC, the scheduler's hourly re-check, `MaxRunTime` 10 min): per backend and kind, bytes our rows say the backend holds — head rows (`object_head_cache` by `backend_name`, `size_bytes`; `chunked` rows resolved through their chunk rows), retained versions (`object_versions`, the `usage.RetainedVersionsFrom` rule so a version sharing the head's blob is not counted twice), chunk blobs (`global_content_index` by `backend_id`, `compressed_size` = what the vendor bills), multipart holdings (`multipart_uploads.reserved_bytes` of active uploads, on the upload's placement), parity shards (`vault_parity` rows × shard bytes on the recorded leg), pack files (`packs` on `sync`), retired blobs not yet collected (`retired_blobs`, by recorded backend) — summed in SQL, never walked; written to `job_runs.result` and exported from it on scrape (the R13 lesson) as `vaultaire_backend_stored_bytes{backend,kind}` and `vaultaire_backend_plan_bytes{backend}` / `vaultaire_backend_plan_term_end_timestamp_seconds{backend}` / `vaultaire_backend_egress_free_bytes{backend}` (3× the stored total, from the plan row). **The vendor view** for `idrive`: `IDRIVE_RESELLER_API_KEY` (read like the master key: credential dir, `_FILE`, env; never logged) + `IDRIVE_RESELLER_USER` → `GET /api/reseller/v1/users` (`storage_used`, GB) and `POST /usage_stats` for the UTC month to date (`uploads`, `downloads`, `disk_used` — units asserted against a known object on the first run, a note until then) → `vaultaire_backend_vendor_used_bytes{backend}` / `vaultaire_backend_vendor_egress_month_bytes{backend}`; a reseller call that fails or a key not set is a **note on an `ok` run**, never a failed run (the account-admin API has answered 5xx on dead regions). The drift `vendor_used − our stored` is the orphan signal the erasure sweep and `chunk_orphans` cannot see from inside | `internal/api/backend_capacity.go`, `internal/api/jobs_wiring.go` (the 08:30 row), `internal/api/metrics_capacity.go`, `internal/idrive/reseller.go` (the first real client: `Users`, `UsageStats`; the doc's `region_key` is `region`, `create_access_key` answers `data.access_key` — Prompt 2026-10-20), `cmd/vaultaire/main.go` (the key sources), `CLAUDE.md` env table | — | `TestBackendCapacity_SumsEveryHolderOnce` (fixture: a head row, a chunked head row with two chunk rows, a retained version sharing the head blob, a retained version with its own blob, an active upload's reservation, a parity row, a pack, a retired blob — expected per-backend/kind totals; a second tenant's rows on the same database untouched), `TestBackendCapacity_VendorFailureIsANote`, `TestBackendCapacity_GaugesReadFromJobRuns` (right after a restart), `TestResellerClient_UsersAndUsageStats` (httptest, the live shapes), `TestJobsWiring_BackendCapacity0830` | first run on prod: `idrive` ≈ 2 MB of dead May rows + the canary vs vendor `storage_used`; `lyve` ≈ the 38,658 chunk rows + objects; the gauges on `/metrics`; the System card | job dark without a plan row (sums only); no schema |
| **WP-CAP-1 C — the alerts** ✅ **DONE 2026-10-21** (#820 + #822 — the promtool test's series must sample at 1 m, D-P21-A9; installed on the box `f935ad16…` with the re-installed `vaultaire-jobs.yml` `1b2fc21c…`, 11 + 9 rules load, `BackendPlanUnknown` pending for geyser/lyve/r2/wasabi as designed) | `deploy/monitoring/vaultaire-capacity.yml`: `BackendCapacity80` (warn: `max(stored, vendor_used) ≥ 0.8 × plan_bytes`, 1 h), `BackendCapacity90` (page), `BackendCapacityForecast30d` (warn: `predict_linear(stored[14d], 30d) ≥ plan_bytes`), `BackendEgressFree80` (warn: vendor month-to-date downloads ≥ 0.8 × free multiple × stored), `BackendPlanRenewal30d` / `14d` (warn) / `7d` (page: the Aug-27 shape), `BackendPlanNoPayment` (page: `payment_on_file` false within 30 d of `term_end`), `BackendPlanUnknown` (warn: a registered paid backend — idrive, lyve, geyser, wasabi, r2 — with no plan row), `BackendVendorDrift` (warn: `vendor_used > 1.2 × stored + 1 GiB` for 3 days), `BackendCapacityStale` (no success in 26 h, from `vaultaire_job_last_success_timestamp_seconds`); a `deploy/monitoring/README.md` section and the install line | `deploy/monitoring/vaultaire-capacity.yml`, `deploy/monitoring/README.md`, `internal/api/capacity_rules_test.go` (the rule file parses, every alert names a series the collector exports — the pattern of `backend_rules_test.go`) | — | `TestCapacityRules_ParseAndNameExportedSeries` | rules installed on the box (`promtool check rules`), `BackendPlanUnknown` fires for the backends without a row until the owner enters them, `BackendCapacity80` silent at 0.0002 of 1 TB | rules are files; remove them |

**Owner click-list (this WP, besides F-31):** in the iDrive console turn on the e-mail notification preferences for disk utilization and renewal (both empty on 2026-09-20, which is why the lapse was silent), enable 2FA on the reseller account, rotate the reseller API key (it went through chat on 2026-10-20) and hand the new one to the box as the systemd credential `idrive_reseller_api_key`; keep benchmarks against iDrive under a few hundred GB or expect overage on the next bill. Montreal, Singapore and Singapore-2 are platform-inactive (`/regions` `active: false`, provisioning answers a 500 even for a new user, probed 2026-10-20) and absent from `drivers.IDriveRegions`: when iDrive opens them, the table gains three rows before their env pairs can register a driver.

---

## Phase 3: Admin Dashboard
*Depends on: Phase 2 (billing data to display)*

> **Phase 3 COMPLETE** — PRs #195 (3.1-3.5 catch-up), #276 (3.6), #277 (3.8), #278 (3.9), #294 (3.7), #295 (3.10), #296 (3.11).

### 3.1: Admin Layout + Tenant List
**File**: `internal/dashboard/handlers/admin.go`
- Role check middleware: only `role='admin'` users see `/admin/*`
- Tenant list with search, pagination, storage used, plan, status

- Register `POST /webhook/stripe` route
- Seed admin: `UPDATE users SET role='admin' WHERE email='your@email.com'`

### 3.2: Tenant Detail + Suspension
**File**: `internal/dashboard/handlers/tenants.go`
- View single tenant: usage, plan, API keys, buckets, activity
- Suspend/enable toggle — sets `tenants.suspended = true`
- Suspension enforced in S3 auth middleware: suspended tenant → 403 on all S3 ops
- Admin notes field (migration `045_admin_notes.sql`)

### 3.3: Quota Management
**File**: `internal/dashboard/handlers/admin.go`
- Adjust any tenant's storage/egress quota from admin UI
- Override plan defaults (e.g., give a beta tester extra space)

### 3.4: System Overview
**File**: `internal/dashboard/handlers/admin_system.go`
- Total storage used across all tenants, total tenants, active subscriptions
- Backend health summary (from engine health checks)
- Recent signups, recent errors from server logs

### 3.5: Activity Log (Admin)
**File**: `internal/dashboard/handlers/admin.go`
- Paginated list of admin actions (suspend, quota change, etc.)
- Filterable by admin user, action type, date range

### 3.6: Audit Log Viewer
**File**: `internal/dashboard/handlers/admin_audit.go`
> **Phase 3.6 COMPLETE** — PR #276
- Full audit log with filters (tenant, action type, date range, IP)
- CSV export for compliance evidence
- Pagination with cursor-based navigation

### 3.7: Customer Support View
**File**: `internal/dashboard/handlers/admin_support.go`
> **Phase 3.7 COMPLETE** — PR #294
- "View as customer" mode — see what the customer sees (read-only)
- Ticket-like interface for tracking customer issues
- Quick actions: reset password, extend quota, add credit

### 3.8: Revenue Dashboard
**File**: `internal/dashboard/handlers/admin_revenue.go`
> **Phase 3.8 COMPLETE** — PR #277
- Real MRR calculation from active Stripe subscriptions
- Tier breakdown (how much revenue per plan)
- Churn rate, top customers by revenue
- Month-over-month growth chart

### 3.9: Cost Dashboard
**File**: `internal/dashboard/handlers/admin_costs.go`
> **Phase 3.9 COMPLETE** — PR #278
- Per-backend COGS (iDrive, Lyve, Geyser costs from provider invoices)
- Blended cost per TB, gross margin calculation
- Cost trend over time

### 3.10: Admin Notifications
**File**: `internal/dashboard/handlers/admin_notifications.go`, migration `046_admin_notifications.sql`
> **Phase 3.10 COMPLETE** — PR #295
- In-app notification bell for admin events (new signup, failed payment, backend down)
- Notification preferences (which events to show)
- Mark as read, dismiss all

### 3.11: Abuse Queue
**File**: `internal/dashboard/handlers/admin_abuse.go`, migration `047_abuse_reports.sql`
> **Phase 3.11 COMPLETE** — PR #296
- Abuse report submission (public form + API)
- Admin queue: review, investigate, take action (warn, suspend, delete)
- Automated detection: excessive bandwidth, rapid bucket creation

**Test**: Admin login → see all tenants → suspend one → verify S3 returns 403 → enable → verify S3 works → check audit log shows actions → revenue dashboard shows MRR → cost dashboard shows margins.

---

## Phase 4: Bandwidth Tracking & Alerts
*Depends on: Phase 3 (admin views for bandwidth data)*

> **Phase 4 COMPLETE** — PR #195 (4.1-4.2 catch-up), PR #275 (4.3 alerts).

### 4.1: Bandwidth Tracking Middleware
**File**: `internal/api/bandwidth.go`
- Middleware wraps S3 responses, counts bytes written to `http.ResponseWriter`
- Accumulates per-tenant daily bandwidth in `bandwidth_daily` table
- Monthly rollup for billing comparison

### 4.2: Bandwidth Dashboard Widget
**File**: `internal/dashboard/handlers/bandwidth_chart.go`
- 30-day bandwidth chart on customer dashboard
- Show egress vs. ingress breakdown
- Quota progress bar (used / allowed)

### 4.3: Bandwidth Alerts
**File**: `internal/api/bandwidth_alerts.go`, migration `020_bandwidth_banking.sql`
> **Phase 4.3 COMPLETE** — PR #275
- Threshold alerts at 80% and 95% of bandwidth quota
- Alert delivery via email (Phase 5.13 email service) and event log (Phase 5.11.6)
- `bandwidth_alerts` table tracks which alerts have fired (don't spam)
- Admin configurable thresholds per tenant

**Test**: Upload enough to hit 80% → alert email sent + event logged → continue to 95% → second alert → hit 100% → requests throttled with 429 + Retry-After.

---

## Phase 5: Security Polish
*Depends on: Phase 1 (dashboard to secure)*

> **Phase 5 COMPLETE** — PRs #195 (5.1-5.2 catch-up), #196 (5.3 2FA), #197 (5.4 flash), #198 (5.5 rate limit), #199 (5.6 email verify), #200 (5.7 password reset), #201 (5.8 session hardening), #202 (5.9 responsive CSS).

### 5.1: CSRF Protection
**File**: `internal/dashboard/handlers/middleware.go`
- Double-submit cookie pattern: generate token, embed in forms, verify on POST
- All state-changing dashboard routes require valid CSRF token

### 5.2: OAuth Integration
**Files**: `internal/dashboard/handlers/oauth.go`, migration `021_oauth_accounts.sql`
- Google + GitHub OAuth login/register
- Link OAuth account to existing user or create new user
- `oauth_accounts` table links provider + provider_id to user_id

### 5.3: Two-Factor Authentication (TOTP)
**File**: `internal/dashboard/handlers/mfa.go`, migration `004_mfa.sql`
> **Phase 5.3 COMPLETE** — PR #196
- TOTP setup: generate secret, show QR code, verify first code
- Recovery codes (8 one-time codes, bcrypt hashed)
- Login flow: password → mfa_pending cookie → TOTP code → session
- Compatible with Ente Auth, Google Authenticator, Authy

### 5.4: Flash Messages
**File**: `internal/dashboard/handlers/context.go`
> **Phase 5.4 COMPLETE** — PR #197
- Cookie-based flash messages (success, error, warning)
- Rendered in base template layout, auto-dismiss after display

### 5.5: Login Rate Limiting
**File**: `internal/dashboard/handlers/account.go`
> **Phase 5.5 COMPLETE** — PR #198
- 5 failed login attempts per IP per minute → 429 with retry delay
- In-memory rate limiter (token bucket), no Redis dependency

### 5.6: Email Verification
**File**: `internal/dashboard/handlers/email_verify.go`, migration `022_email_verification.sql`
> **Phase 5.6 COMPLETE** — PR #199
- HMAC token in verification URL (no DB lookup needed)
- `VERIFY_SECRET` env var for HMAC signing
- Unverified users can log in but see "verify your email" banner

### 5.7: Password Reset
**File**: `internal/dashboard/handlers/account.go`
> **Phase 5.7 COMPLETE** — PR #200
- HMAC token in reset URL, 1-hour expiry
- Rate limited: 3 reset requests per email per hour

### 5.8: Session Security Hardening
**File**: `internal/dashboard/handlers/sessions.go`, migration `023_session_security.sql`
> **Phase 5.8 COMPLETE** — PR #201
- IP + user-agent tracking per session
- Concurrent session limit (5 per user)
- Session revocation from settings page

### 5.9: Responsive CSS + Error Pages
**File**: `internal/dashboard/templates/`
> **Phase 5.9 COMPLETE** — PR #202
- Responsive dashboard layout (mobile-friendly)
- Custom error pages (400, 403, 404, 500) with helpful messages
- Print-friendly billing page

**Test**: Login → enable 2FA → logout → login with 2FA → verify flash messages → test on mobile viewport → trigger 404 → see custom error page.

---

## Phase 5.10: S3 Compatibility Hardening
*Depends on: Phase 5 (security polish). This is the largest sub-phase — 18 sub-steps. Each is its own PR.*

*Reference: `.private/PHASE_5_10_PREP.md` for original 12-item prep. Expanded to 18 during implementation.*

### 5.10.1: Multipart Upload
**File**: `internal/api/s3_handler.go`, migration `024_multipart_uploads.sql`
> **Phase 5.10.1 COMPLETE** — PR #203
- InitiateMultipartUpload → returns UploadId (UUID)
- UploadPart → stores part to temp file, records in `multipart_parts` table
- CompleteMultipartUpload → concatenates parts, stores final object, cleans up
- AbortMultipartUpload → deletes temp parts + DB records
- ListParts, ListMultipartUploads for in-progress tracking
- Required for files >5GB, used by all serious S3 clients

### 5.10.2: CopyObject (Server-Side)
**File**: `internal/api/s3_handler.go`
> **Phase 5.10.2 COMPLETE** — PR #204, fixes in PR #209
- `PUT /{bucket}/{key}` with `x-amz-copy-source` header
- Get from source → Put to destination (streaming, never buffer full object)
- Cross-bucket copy within same tenant
- Required by JuiceFS for rename operations
- Follow-up fixes: atomic LocalDriver.Put, ETag propagation

### 5.10.3: Batch DeleteObjects
**File**: `internal/api/s3_handler.go`
> **Phase 5.10.3 COMPLETE** — PR #208
- `POST /{bucket}?delete` with XML body listing up to 1000 keys
- Returns XML response with per-key success/error
- Required by `aws s3 rm --recursive`, rclone sync, JuiceFS

### 5.10.3b: Driver Unit Tests
**File**: `internal/drivers/quotaless_test.go`, `internal/drivers/geyser_test.go`
> **Phase 5.10.3b COMPLETE** — PR #222
- Comprehensive unit tests for Quotaless and Geyser drivers
- Unified Driver interface conformance tests
- Mock S3 client for isolated testing

### 5.10.4: Bucket Registry + Tenant Slugs
**File**: `internal/api/s3_handler.go`, migration `025_buckets_and_slugs.sql`
> **Phase 5.10.4 COMPLETE** — PR #211
- `buckets` table: id, tenant_id, name, slug, visibility, cors_origins, created_at
- Tenant slugs for CDN URLs: `cdn.stored.ge/{slug}/{bucket}/{key}` *(2026-10-12, WP-DOMAIN-1): the public URL shape moves off the brand origin to a dedicated non-brand content domain (`<slug>.<content-domain>/<bucket>/<key>` or path-style on it — decided in WP-DOMAIN-1); `cdn.stored.ge` and `/cdn` on the brand hosts answer only with a 301 to it. Customer HTML on the brand origin (review R4) ends with this.*
- CreateBucket/DeleteBucket/ListBuckets operations now DB-backed

### 5.10.5: Bucket Settings Page
**File**: `internal/dashboard/handlers/bucket_settings.go`
> **Phase 5.10.5 COMPLETE** — PR #212
- Dashboard page for per-bucket settings
- Public/private visibility toggle
- CORS origin configuration
- Cache TTL settings

### 5.10.6: CDN Handler
**File**: `internal/api/cdn.go`
> **Phase 5.10.6 COMPLETE** — PR #214
- `GET /cdn/{slug}/{bucket}/{key}` serves public bucket objects
- Content-Type detection, Cache-Control headers
- Only serves from buckets with `visibility = 'public'`
- **Next (Phase 40.1):** every CDN byte still transits SLC (`/cdn` → engine Get); the 0×-SLC plane is Cache Reserve or R2 Sippy in front of `s3.stored.cloud`, or a 307 to a presigned backend URL, chosen per bucket class.
- **Content domain (2026-10-12, WP-DOMAIN-1, before Oct 31):** `/cdn` is served from its own non-brand hostname on its own Cloudflare zone and rules (no cookies, no dashboard origin, CSP sandbox, `Content-Disposition: attachment` for text/html stays); `buckets.cors_origins` and the "Copy CDN URL" button (5.11.11) emit that host; the brand hosts' `/cdn/*` 301 to it. The engine path is unchanged — routing + copy + one env var for the host.

### 5.10.7: Range Requests
**File**: `internal/api/cdn.go`, `internal/api/s3_handler.go`
> **Phase 5.10.7 COMPLETE** — PR #215
- HTTP Range header support for partial content (206 Partial Content)
- Required for video streaming, resume downloads
- Content-Type fix for CDN (was always `application/octet-stream`)

### 5.10.8: Conditional Requests
**File**: `internal/api/conditional.go`
> **Phase 5.10.8 COMPLETE** — PR #216
- If-None-Match (ETag comparison → 304 Not Modified)
- If-Modified-Since (timestamp comparison)
- If-Match, If-Unmodified-Since for conditional writes
- Cache-Control headers on GET/HEAD responses
- Required by JuiceFS for metadata consistency checks

### 5.10.9: CORS Configuration
**File**: `internal/api/cors.go`, CDN middleware
> **Phase 5.10.9 COMPLETE** — PR #217
- Per-bucket CORS origins (stored in `buckets.cors_origins`)
- OPTIONS preflight handling with correct Access-Control-* headers
- Shared CORS helper used by both S3 and CDN handlers
- Required for browser-based uploads and CDN content on third-party sites

### 5.10.10: ListObjectsV2 Pagination
**File**: `internal/api/s3_handler.go`
> **Phase 5.10.10 COMPLETE** — PR #218
- Full ListObjectsV2 with `continuation-token` and `start-after` parameters
- Delimiter support with `CommonPrefixes` for folder-like navigation
- V1 compatibility (ListObjects without V2 parameters)
- Tested with 10,000+ objects

### 5.10.11: Bucket Versioning
**File**: `internal/api/s3_handler.go`, migration `026_versioning.sql`
> **Phase 5.10.11 COMPLETE** — PR #219
- `PUT /{bucket}?versioning` to enable/suspend
- Version IDs (UUID) on every PUT when versioning enabled
- `GET /{bucket}/{key}?versionId=...` for specific versions
- Delete markers (soft delete, preserves all versions)
- `object_versions` table: object_key, version_id, is_latest, size, etag, is_delete_marker

### 5.10.12: Bucket Notifications
**File**: `internal/api/s3_notifications.go`, migration `027_bucket_notifications.sql`
> **Phase 5.10.12 COMPLETE** — PR #220
- `PUT /{bucket}?notification` to configure webhook destinations
- Event types: `s3:ObjectCreated:*`, `s3:ObjectRemoved:*`
- Wildcard prefix/suffix filtering
- Async webhook delivery with retry (3 attempts, exponential backoff)
- `notification_configs` and `notification_deliveries` tables

### 5.10.13: Object Lock / WORM
**File**: `internal/api/s3_handler.go`, migration `028_object_lock.sql`
> **Phase 5.10.13 COMPLETE** — PR #221
- Governance mode: requires MFA Delete to override
- Compliance mode: immutable, cannot be overridden (even by admin)
- `x-amz-object-lock-retain-until-date` and `x-amz-object-lock-mode` headers
- Legal hold: `PUT /{bucket}/{key}?legal-hold` with ON/OFF status
- `object_locks` table: object_key, version_id, mode, retain_until, legal_hold

### 5.10.14: rclone + JuiceFS Compatibility
**File**: `internal/api/compat_test_helpers.go`, user guides in `docs/`
> **Phase 5.10.14 COMPLETE** — PR #223
- 7-group compatibility test suite covering all S3 operations
- rclone configuration guide with one-liner: `rclone config create stored s3 ...`
- JuiceFS format/mount/read/write/rename/delete cycle tested
- Exported test helpers for reuse in integration tests

### 5.10.15: Friendly Error Suggestions
**File**: `internal/api/patterns.go`
> **Phase 5.10.15 COMPLETE** — PR #225
- Levenshtein distance matching on `NoSuchBucket` / `NoSuchKey` errors
- "Did you mean 'my-bucket'?" suggestion in error XML
- `AccessDenied` includes hint about which permission is missing
- Better error messages for common misconfiguration

### 5.10.16: Pre-Signed URLs
**File**: `internal/api/presigned.go`
> **Phase 5.10.16 COMPLETE** — PR #229
- SigV4 query-string authentication (AWS-compatible pre-signed URLs)
- Support both GET (download) and PUT (browser-direct upload)
- Expiration validation (reject expired signatures)
- Bucket/key scope enforcement (can't use a pre-signed URL for a different object)

### 5.10.17: Object Tagging
**File**: `internal/api/s3_handler.go`, migration `041_object_tags.sql`
> **Phase 5.10.17 COMPLETE** — PR #264
- `PUT /{bucket}/{key}?tagging` with XML tag set (up to 10 tags)
- `GET /{bucket}/{key}?tagging` returns current tags
- `DELETE /{bucket}/{key}?tagging` removes all tags
- Tags stored in `object_head_cache.tags` JSONB column
- Used for lifecycle rules, cost allocation, compliance labeling

### 5.10.18: Content-Disposition
**File**: `internal/api/s3_handler.go`, migration `042_content_disposition.sql`
> **Phase 5.10.18 COMPLETE** — PR #267
- Store `Content-Disposition` on PUT (in `object_head_cache`)
- Return on GET/HEAD, override with `response-content-disposition` query parameter
- CDN handler sets `Content-Disposition: inline` for previewable types, `attachment` for downloads
- Required for proper filename handling in browser downloads

**Test**: `rclone test backend` passes → JuiceFS mount/read/write/rename cycle works → multipart upload of 100MB file → CopyObject across buckets → batch delete 50 objects → versioned PUT/GET/DELETE with markers → pre-signed PUT from browser → CORS preflight returns correct headers → Object Lock prevents delete → tagged objects queryable.

---

## Phase 5.11: Developer Experience + CDN Delight
*Depends on: Phase 5.10 (S3 compat for the API surface these build on)*

### 5.11.0: JSON Management API
**File**: `internal/api/management.go`, `internal/api/management_routes.go`, `internal/api/management_ratelimit.go`
> **Phase 5.11.0 COMPLETE** — PR #230
- RESTful `/api/v1/manage/` endpoints for non-S3 operations
- JWT authentication (existing auth system)
- Stripe-style response envelope: `{"object": "bucket", "data": {...}, "meta": {...}}`
- 10 initial endpoints: buckets CRUD, keys CRUD, tenant info, usage
- Per-tenant rate limiting (100 req/min default, configurable)
- OpenAPI spec generation (~60% coverage, iteratively expand)

### 5.11.1: API Version Header
**File**: `internal/api/middleware.go`
> **Phase 5.11.1 COMPLETE** — PR #231
- `X-Vaultaire-Version: 2026-05-01` response header on every API response
- `X-Request-Id` unique request ID header for support/debugging
- `Server: stored.ge` header (branding + diagnostics) *(2026-10-12): becomes `Server: Stored` — a brand, not a domain; changed in WP-DOMAIN-1 (owner confirms the value).*
- Stripe-style date versioning (not semver) — pin behavior to a date

### 5.11.2: Idempotency Keys
**File**: `internal/api/idempotency.go`, migration `029_idempotency_cache.sql`
> **Phase 5.11.2 COMPLETE** — PR #233
- `Idempotency-Key` header on management API POST/PUT requests
- 24-hour cache in `idempotency_cache` table
- Replay: return cached response if same key + same request body
- Different body with same key → 409 Conflict
- Prevents double-billing, duplicate resource creation

### 5.11.3: Metadata on Resources
**File**: `internal/api/metadata.go`, migration `030_metadata.sql`
> **Phase 5.11.3 COMPLETE** — PR #233 (combined with 5.11.2)
- `metadata` JSONB column on `buckets`, `api_keys` tables
- Arbitrary key-value pairs (up to 50 keys, 500 chars per value)
- Set via management API: `PATCH /api/v1/manage/buckets/{id}` with `metadata` field
- Useful for customer tagging, integration labels, automation

### 5.11.4: Scoped API Keys
**File**: `internal/auth/auth.go`, migration `031_scoped_keys.sql`
> **Phase 5.11.4 COMPLETE** — PR #235
- `permissions` JSONB on `api_keys`: `{"buckets": ["photos-*"], "actions": ["GetObject", "PutObject"]}`
- `bucket_scope` TEXT: restrict key to specific bucket pattern
- `ip_allowlist` TEXT[]: restrict key to specific IP addresses/CIDRs
- `expires_at` TIMESTAMP: auto-expire keys
- S3 auth evaluates permissions before allowing operation
- VLT_ prefix for scoped keys (distinguishes from full tenant keys)

### 5.11.5: STS Temporary Credentials
**File**: `internal/auth/sts.go`, migration `032_sts_tokens.sql`
> **Phase 5.11.5 COMPLETE** — PR #236
- `POST /api/v1/manage/sts/token` — issue temporary credentials
- ASIA-prefixed access key (like AWS STS)
- Scope intersection: temporary token can only have FEWER permissions than the issuing key
- Duration: 15 minutes to 12 hours (default 1h)
- `sts_tokens` table with automatic hourly cleanup of expired tokens
- Use case: browser-direct uploads with time-limited, bucket-scoped credentials

**Test**: Create scoped key → verify it can only access allowed buckets → generate STS token → verify scope intersection → use STS token for browser upload → verify token expires correctly.

### 5.11.6: Event Log + Webhook Management API
**File**: `internal/api/events.go`, `internal/api/webhooks_routes.go`, new migration
**Depends on**: 5.11.0 (management API for route registration and response envelope)
> **Phase 5.11.6 COMPLETE** — merged to main alongside PRs #238/#239 (no standalone PR; orphan branch `phase-5.11.6-event-log-webhooks` is stale). Code verified present: `internal/api/events.go`, `event_type.go`, `webhooks_routes.go`, `webhook_endpoints` table. <!-- reconstructed: verified against codebase 2026-07-07 -->
- Migration: `events` table (id, type, tenant_id, data JSONB, created_at). `webhook_endpoints` table (id, tenant_id, url, event_filter TEXT[], secret, enabled, created/updated_at). `webhook_deliveries` table (id, webhook_id FK CASCADE, event_id FK, status, response_code, response_body, latency_ms, retry_count, next_retry_at, created_at).
- Event types: object.created, object.deleted, object.downloaded, bucket.created, bucket.deleted, key.created, key.revoked, sts.token_created
- `emitEvent(ctx, type, tenantID, data)` — INSERT into events, dispatch to matching webhooks asynchronously (goroutine with buffered channel, same pattern as notification dispatcher in s3_notifications.go)
- Wire emitEvent into: HandlePut (object.created), handleDeleteObject (object.deleted), handleGetObject (object.downloaded), CreateBucket/DeleteBucket (bucket.*)
- API: `GET /api/v1/events` with type filter and cursor pagination
- **Webhook CRUD API** — `internal/webhooks/webhook.go` already has the delivery engine (HMAC-SHA256 signing, retry logic). This phase exposes it:
  - `POST /api/v1/webhooks` — register. Generate HMAC secret (crypto/rand, 32 bytes hex). Secret only returned in POST response, not in GET list.
  - `GET /api/v1/webhooks` — list (hide secret)
  - `PATCH /api/v1/webhooks/{id}` — update URL, events, or enabled
  - `DELETE /api/v1/webhooks/{id}` — remove (CASCADE deletes deliveries)
  - `GET /api/v1/webhooks/{id}/deliveries` — delivery history
  - `POST /api/v1/webhooks/{id}/test` — fire synthetic "webhook.test" event through full delivery pipeline
- Event dispatch is async — don't block S3 requests on webhook delivery
- Tests: EmitEvent, ListEvents, TenantIsolation, CreateWebhook, ListWebhooks, UpdateWebhook, DeleteWebhook, TestFire, DeliveryHistory

### 5.11.7: Onboarding Flow
**File**: `internal/dashboard/handlers/onboarding.go`, modify dashboard template
**Depends on**: 5.11.0 (management API endpoints to reference in examples), 5.11.6 (webhook step)
- "Get Started" card at top of dashboard (shown until all steps complete or dismissed)
- Checklist derived from existing data (no new tables):
  - "Create your first bucket" — `SELECT COUNT(*) FROM buckets WHERE tenant_id = $1`
  - "Upload a file" — `SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`
  - "Set up a webhook" — `SELECT COUNT(*) FROM webhook_endpoints WHERE tenant_id = $1`
- Tabbed code examples pre-filled with user's ACTUAL access_key (from session/tenant lookup). NEVER expose secret_key in HTML — use "YOUR_SECRET_KEY" placeholder.
  - cURL tab: create bucket + upload via presigned URL
  - AWS CLI tab: configure + mb + cp
  - Python tab: boto3 create_bucket + put_object
  - rclone tab: `rclone config create stored s3 ...` + `rclone copy`
  - Go tab: AWS SDK v2 example
- "Dismiss" button stores cookie (not DB — zero cost, clears on browser reset which is fine)
- Progress bar: "3 of 4 steps complete"

**Test**: Register new account → see onboarding card → follow cURL example → bucket created → checklist updates → dismiss → card hidden.

> **Phase 5.11.7 COMPLETE** — PR #238

### 5.11.8: Cost Comparison Widget
**File**: `internal/dashboard/handlers/billing.go`, template partial
**Depends on**: 5.11.0 (management API for consistent response format)
> **Phase 5.11.8 COMPLETE** — PR #239
- Live widget on billing page: stored.ge vs AWS S3 vs Backblaze B2 vs Wasabi
- Uses tenant's ACTUAL usage (storage GB, egress GB) to calculate real cost difference
- Data source: `tenant_quotas` for storage, bandwidth tracking for egress
- Prices hardcoded (update quarterly): S3 $23/TB + $90/TB egress, B2 $6/TB + $10/TB egress, Wasabi $7/TB
- "You're saving $X/month vs S3" badge on dashboard overview

### 5.11.9: Data Locality Indicator
**File**: `internal/dashboard/handlers/dashboard.go`, template partial
> **Phase 5.11.9 COMPLETE** — PR #240
- SVG world map on dashboard showing where user's data is physically stored
- Data source: backend locations from engine health info
- Pulsing dot indicators for each active region
- Hover tooltip: region name, backend type, data volume

### 5.11.10: Free Tier with Quota Enforcement
**File**: `internal/billing/stripe.go`, `internal/api/quota_management.go`, migration `034_free_tier_defaults.sql`
> **Phase 5.11.10 COMPLETE** — PR #241
- Free tier limits: 5GB storage, 1GB egress/month, 1 bucket, 1 API key
- Default quota set via migration for new signups
- Quota enforcement on PUT: reject with 403 `QuotaExceeded` + upgrade CTA
- Warning at 80% usage: "You've used 4.2 of 5 GB — upgrade for unlimited storage"
- Dashboard: progress bar showing % of free tier used

### 5.11.11: CDN Dashboard Previews
**File**: `internal/dashboard/handlers/files.go`, shared `dashboard.js`
> **Phase 5.11.11 COMPLETE** — PR #242
- Image preview thumbnails in object browser (client-side rendering)
- Video/audio inline player for public bucket content
- "Copy CDN URL" button per object (one-click clipboard copy) *(2026-10-12): copies the content-domain URL — one host constant, the same source the OpenAPI servers and docs read.*
- File type icons for non-previewable content

### 5.11.12: CDN Access Analytics + Bandwidth Budgets
**File**: `internal/api/cdn_analytics.go` (new), `internal/dashboard/handlers/bucket_analytics.go`, migration `035_cdn_analytics.sql`
**Depends on**: 5.10.6 (CDN handler), 5.10.9 (CORS)
> **Phase 5.11.12 COMPLETE** — PR #249
- `cdn_access_log` table: bucket_id, object_key, ip, user_agent, referer, bytes_served, status_code, created_at
- `cdn_stats_daily` table: bucket_id, date, total_requests, total_bytes, unique_ips

- `CDNAnalyticsTracker` — buffered writer (100-event batch, 5s flush interval), inserted from CDN handler after serving. Background hourly roll-up from access_log into stats_daily.
- Dashboard: per-bucket analytics page with request count, bandwidth, top objects, top referers
- Bandwidth budget alerts: configurable per-bucket monthly bandwidth cap
- Admin: global CDN analytics with per-tenant breakdown

**Test**: Register → see onboarding checklist → create bucket with curl example from dashboard (copy-paste, it works) → verify `X-Vaultaire-Version` + `X-Request-Id` + `Server: stored.ge` headers → JSON API `GET /api/v1/buckets` returns correct envelope with `object: "list"` + cursor pagination → create restricted key → use it to upload → verify full key can't be used for wrong bucket → generate STS token → upload via browser → check event log shows all actions → billing page shows live cost comparison vs S3/B2/Wasabi → dashboard shows data locality → CDN analytics show access counts and bandwidth.

---


## Phase 5.12: Backend Readiness (Parallel Track)
*Independent of 5.10/5.11. Can run alongside. Required before multi-backend tiering in Phase 7.*

### 5.12.1: Quotaless Production Driver Fix ⏸ DEPRIORITIZED (account locked 2026-05-12; removal deferred to M12 exit)
**Files**: `internal/drivers/quotaless.go`
- **2026-07-07**: Quotaless is deprioritized, not just blocked — LAUNCH_STRATEGY plans a full exit by M9-12 (own Gorilla fleet, Phase 6.6). Don't invest here. Known follow-up: the unconditional boot health check against `io.quotaless.cloud` (`server.go`) is a CI-flake source — make it conditional on creds being present. <!-- reconstructed: verified against LAUNCH_STRATEGY.md §5 + memory -->
- Account locked as of 2026-05-12 — contact Quotaless support to re-enable
- Quotaless removed from SLC production .env (was causing 401 retry storms on every Delete)
- Still wired in main.go code but won't activate without QUOTALESS_ACCESS_KEY in env
- When re-enabled: apply `SwapComputePayloadSHA256ForUnsignedPayloadMiddleware`, fix incompatible S3 ops
- Auto-detect priority in main.go is now: `iDrive > Quotaless > S3 > Geyser > local` (describes the code; Quotaless creds have been 401-dead since 2026-08 and are unused in prod)
- Reference: `cmd/quotaless-bench-v2/main.go`, `internal/drivers/quotaless_README.md`

### 5.12.2: Cloudflare R2 Driver — ~~✖ DROPPED (2026-08-01)~~ reversed: shipped #466 (public buckets only, never a tier)
*Superseded by the tier architecture: public/CDN delivery is Cloudflare-in-front (`cdn.stored.ge`) over the existing backends, and the launch lineup (iDrive/Geyser/Lyve + OneDrive permafrost second copy; Quotaless creds dead, removal deferred to M12) has no R2 slot. Revisit only if a zero-egress replication target is needed post-launch.* *(2026-10-12): superseded again — `r2` is registered (`R2_*`, fixed bucket `vaultaire-public`) as the origin of public-read buckets; the delivery hostname is the WP-DOMAIN-1 content domain, not `cdn.stored.ge`.*
**Files**: `internal/drivers/r2.go` (new), `internal/drivers/r2_test.go` (new)
- S3-compatible driver for Cloudflare R2 ($15/TB storage, zero egress, 330 PoPs)
- Used for public CDN buckets — when tenant toggles bucket to public, replicate to R2
- Standard AWS SDK v2 S3 client (R2 is fully S3-compatible, no quirks like Quotaless)
- Env vars: `R2_ACCESS_KEY`, `R2_SECRET_KEY`, `R2_ENDPOINT`, `R2_BUCKET`
- R2 endpoint format: `https://<account-id>.r2.cloudflarestorage.com`
- Unit tests with mock S3 client, integration test tagged `//go:build integration`

### 5.12.3: iDrive + OneDrive Wiring + End-to-End Benchmarks ✅ COMPLETE (PR #244)
**Files changed**: 20 files, +1,771/-545 lines. Merged 2026-05-13.
**What was done:**
- iDrive wired in main.go with fixed-bucket + key-prefix pattern, materialize for Content-Length, ContentLength passthrough
- OneDrive fleet driver: complete rewrite from scaffold — raw HTTP, dual transport, streaming uploads, parallel range downloads, DNS cache, fleet-wide TLS cache, token mutex, RateLimit tracking
- Engine: `WithContentLength` PutOption, multipart passes size, Delete targets known backend only
- `scripts/bench-vaultaire.sh` automation, `vaultaire-local` bench-compare endpoint
- `cmd/pipeline-bench/` — RS, FastCDC, zstd, AES-GCM benchmarking tool
**Benchmark results (SLC datacenter):** iDrive 215 MB/s sustained / 563 MB/s concurrent. Lyve 380 MB/s sustained / 563 MB/s concurrent. Geyser 22 MB/s sustained. OneDrive 84.5 MB/s concurrent ingest / 259 MB/s download (3 tenants, $0). Local baseline 1,417 MB/s sustained. HEAD cache 6,016 ops/s (260x vs direct). Pipeline: 593 MB/s (RS+AES+zstd+CDC). All integrity/consistency checks pass.
**How to re-run benchmarks:** `source .env.bench && ./scripts/bench-vaultaire.sh [backends...]`. SLC: deploy `bin/vaultaire-linux` + `bin/bench-compare-linux` to `/tmp/`, source `.env.bench`, run script. See `bench-results/vaultaire-e2e/` for JSON results.
**Detailed plan file:** `/Users/viera/.claude/plans/ancient-toasting-puppy.md`

### 5.12.4: Multi-Backend Failover + Storage Class Routing
**Files**: `internal/engine/failover.go` (new), `internal/engine/engine.go`
> **Phase 5.12.4 COMPLETE** — PR #251. Hardened under load in #283. <!-- reconstructed: verified against git log -->
- Engine currently selects primary/backup but failover is manual
- Automatic failover: if primary returns error or health check fails, retry on backup
- Backend priority list per operation type (read vs write)
- Circuit breaker pattern: after N consecutive failures, mark backend degraded for M seconds
- Prometheus metrics: `vaultaire_backend_failover_total{from, to}`, `vaultaire_backend_health{name}`
- Graceful degradation: if all backends down, return 503 with `Retry-After` header
- **Storage class mapping**: honor `x-amz-storage-class` header on PUT — `STANDARD` → iDrive, `STANDARD_IA` → Lyve (zero egress), `GLACIER` → Geyser, `DEEP_ARCHIVE` → Geyser+airgap, `ONEZONE_IA` → OneDrive (parity-only, not customer-facing). Return correct class on HEAD/GET. Default `STANDARD` if omitted. Enables S3-compatible lifecycle transitions and customer tier control via standard AWS tooling. (Quotaless mapping removed — account locked.)
- ⚠ **Stale-dependency note (2026-07-07, CORRECTED 2026-09-22)**: Lyve is **IN the stack** — not a default customer tier, but the `resilient` storage class + DR/fallback hot leg + Vault second copy, closed in #405 (promo = $0 COGS through mid-2028; the 2026-07-07 "strategically DROPPED" call is superseded). Driver wired, validated 20/20 in #312, paired benches 250/206 MB/s (2026-09-19). Don't route customer `STANDARD_IA` to Lyve by default; treat it as an alias for `STANDARD` — `RESILIENT` is the Lyve-backed class. The permafrost (OneDrive) class was removed from user-selectable classes in #312. <!-- reconstructed: verified against .private/CLAUDE.md + PR #312; Lyve status corrected against #405 -->

### 5.12.5: Backend Health Dashboard
**Files**: `internal/dashboard/handlers/admin_backends.go` (new), `templates/admin/backends.html` (new)
> **Phase 5.12.5 COMPLETE** — PR #252. <!-- reconstructed: verified against git log -->
- Admin page: all registered backends with health status, latency, last-checked timestamp
- Per-backend: endpoint, region, capacity, current usage, health history (last 24h)
- Failover log: recent failover events with timestamps and reason
- Manual actions: force health check, mark degraded, set primary

### 5.12.6: Geyser Tape Registration in main.go ✅ COMPLETE (pre-existing, benchmarked in 5.12.3)
- Geyser was already wired in main.go (block 5) before 5.12.3
- Benchmarked in 5.12.3: SLC 22 MB/s sustained, 56 MB/s concurrent download
- TLS certs now valid (InsecureTLS removed from bench-compare 2026-05-12)
- LA endpoint active, London bucket deleted 2026-04-20 — re-enable when recreated

**Test**: Start with iDrive + Lyve + Geyser registered → upload to iDrive → simulate iDrive down (misconfigure endpoint) → verify failover to Lyve → verify health dashboard shows degraded iDrive → restore → verify recovery. R2 driver: PUT/GET/DELETE cycle, verify zero egress on download. Geyser: PUT/GET/DELETE against LA endpoint, verify tape retrieval works (may be slow). (Quotaless removed from test — account locked.)

### 5.12.7: Production Transport Tuning
**Files**: `internal/drivers/transport.go` (new), `internal/drivers/lyve.go`, `internal/drivers/s3compat.go`, `internal/drivers/s3.go`, `internal/drivers/geyser.go`, `internal/drivers/idrive.go`
- Production drivers use Go's default HTTP transport (MaxIdleConnsPerHost=2, 4KB buffers, compression on). Bench-compare has tuned transport that gets 28-56x better throughput on the same backends.
- Create shared `TunedHTTPClient()` helper with options (`WithInsecureTLS()`, `WithResponseHeaderTimeout()`) and apply to all S3-compatible drivers.
- **Tuned settings**: MaxIdleConns=200, MaxIdleConnsPerHost=200, 1MB read/write buffers, compression disabled, HTTP/2 enabled, DNS caching via `cachedDialContext`, TLS session cache (128 entries), TLS 1.2 minimum.
- **Geyser-specific**: preserve 5-minute ResponseHeaderTimeout for tape seek latency. Geyser TLS certs are now valid (2026-05-12) — do NOT add InsecureTLS.
- **S3Driver.Put fix**: remove `io.ReadAll(data)` memory buffer — use `io.Seeker` for size detection, stream directly. Currently violates "always stream, never buffer" architecture decision.
- **Adaptive MPU strategy**: detect backend preference (H1 vs H2), configure 4-16 parallel parts with 4MB buffers
- OneDrive-specific: HTTP/1.1 forced for CDN chunk uploads (HTTP/2 triggers 503s under load)
- DNS caching via `cachedDialContext` for backends with slow DNS resolution

**Test**: Compare throughput before/after transport tuning on SLC → iDrive should sustain 200+ MB/s, Lyve 380+ MB/s. Geyser: verify 5-minute timeout doesn't affect normal PUT/GET. OneDrive: verify H1 uploads are stable at concurrency.

> **Phase 5.12.7 COMPLETE** — PR #245

### 5.12.8: Backend Cost Tracking
**Files**: `internal/engine/cost_optimizer.go`, `internal/dashboard/handlers/admin_costs.go`
> **Largely superseded** — per-backend COGS + margin dashboard shipped as Phase 3.9 (#278) and blended-cost tracking as Phase 7.4 (#297); `admin_costs.go` further updated in #312. Remaining: automated cost input via iDrive reseller API (folds into R4). <!-- reconstructed: verified against git log -->
- Track per-backend storage and egress costs from provider billing
- Input: manual entry or API polling (iDrive reseller API when available)
- Output: blended cost per TB, per-backend breakdown, margin calculation
- Dashboard: admin cost page with per-backend COGS, trend charts
- Data feeds into tiering engine (Phase 7) for cost-optimized placement

### 5.12.9: Backend Capacity Planning
**Files**: `internal/engine/capacity.go`
> **Superseded 2026-10-20 by WP-CAP-1** (the plan record, the daily `backend_capacity` job, the capacity / renewal / egress alerts) — see § WP-CAP. The lines below are the 2026-04 sketch.
- Track available capacity per backend
- Alert at 80% capacity: "iDrive account approaching storage limit"
- Capacity forecast: based on current growth rate, when will each backend fill?
- Auto-provisioning hooks (future: iDrive reseller API creates new sub-accounts)

**Test**: Check admin backend dashboard shows all backends with correct health/latency → verify cost tracking aggregates correctly → capacity alerts fire at threshold.

### 5.12.10: Backend-Outage Alerting ✅ SHIPPED 2026-09-22 (#457)
**Files**: `internal/api/metrics.go`, `internal/engine/health*.go`, `deploy/monitoring/vaultaire-backends.yml`
- Authenticated backend probes replace the TCP-only check where a vendor can answer honestly: iDrive/Geyser = signed HeadBucket via the engine; Lyve = console `RSCustomerDetails` with the root key (one 403 retry); TCP dial stays the fallback (architecture decision #1 still holds for the unauthenticated path)
- Real Prometheus `/metrics` exposition — the legacy 2 series are kept; `vaultaire_errors_total` now counts 5xx (it was a never-incremented stub); new `vaultaire_backend_*` series (up/latency/probe result per backend)
- `deploy/monitoring/vaultaire-backends.yml` — 6 alert rules, installed on SLC 2026-09-23 → Alertmanager (#423) → ntfy bridge. [YOU]: subscribe to the ntfy topic (Stage 5)

---

## Phase V18: Vault18 Deep Archive Experience
*Depends on: Phase 5.12 (Geyser backend wired), Phase 5.14 (compliance for immutability). The "Anti-Glacier" product.*

*Reference: `.private/VAULT18_LAUNCH.md` for pricing, community posts, FAQ. `.private/VAULT_SERIES_ECONOMICS.md` for per-tier COGS.*

### V18.1: Ingest Buffer Pipeline
**Files**: `internal/engine/ingest_buffer.go` (new)
- Hot ingest to iDrive (fast, 852 MB/s) → background migration to Geyser tape (**227 MB/s sustained to staging over HTTP/1.1 with concurrent objects — the old "27.7 MB/s account ceiling" was our forced-h2 client bug, fixed 2026-07-29 in `geyser.go`**; single connection is still ~25-30 MB/s, so the migrator must drain many objects in parallel)
- Customer sees immediate upload completion; tape write is async
- Progress tracking: `ingest_buffer_status` with per-object state (ingested, migrating, archived)
- Retry logic for Geyser failures (tape seek timeout, connection reset)

### V18.2: Restore Orchestration + Hot Rehydration ⚠ MINIMUM SLICE PRE-LAUNCH, REST FIRST POST-LAUNCH
**Files**: `internal/engine/retrieval.go` (new), `internal/api/s3_handler.go`, `internal/drivers/geyser.go`

> **2026-08-04 (reprice decision, condition 4): the MINIMUM recall slice below ships PRE-LAUNCH** — GET on an archived object must never surface a raw error while Vault leads the launch (Aug 31 at decision time; launch moved to **Oct 31, 2026** on 2026-09-14). Scope: items 1 and 3 only, no restore worker — (a) `geyser.go` detects `InvalidObjectState` → typed `ErrArchived` (never a 500); (b) GET on archived → 403 `InvalidObjectState` proper S3 error XML; (c) `RestoreObject` passthrough (`POST ?restore` forwarded to Geyser); (d) HEAD returns `x-amz-restore` (`ongoing-request="true"/"false"`); (e) dashboard Restore button + status on the bucket objects page. rclone/aws-cli restore workflows work unmodified; the customer polls and re-GETs within the staging window (≤13 days) — no rehydration, no bulk thaw, no webhooks. Everything else in this section stays the first post-launch WP.

*Rewritten 2026-07-29 after live probes. Old assumption (direct Geyser reads at 4 MB/s) is wrong: objects past the Vail staging window (≤13 days) are `StorageClass: GLACIER` and GET returns 403 `InvalidObjectState`. Measured: RestoreObject accepted (incl. bulk batches), tape recall <3 min on an idle library, restored copy reads ~5 MB/s/stream from staging and expires per `Days`.*

**The rehydration design (the Geyser×Wasabi pattern — restore INTO the hot tier, never serve from staging):**
1. Driver: `geyser.go` detects `InvalidObjectState` → surfaces typed `ErrArchived` (never a 500).
2. Engine restore worker: issue `RestoreObject` (Days=2), poll HEAD `x-amz-restore` until `ongoing-request="false"` (poll interval 30s; measured completion ~3 min), then **pull once from staging and rehydrate**: ≤1 GB objects → NVMe cache; bulk → the configured rehydrate backend under a `restore/<tenant>/` prefix with TTL (7 days, extendable from dashboard). All customer reads then serve at hot line-rate — optionally via presigned direct links to skip the SLC proxy path.
   - **Geyser may be able to do the rehydration itself.** The console API
     (mapped 2026-07-29, full detail in `internal/drivers/geyser_README.md`)
     exposes `POST /api/buckets/{id}/restoreToCache {path,versionId}` *and*
     `POST /api/buckets/{id}/restore {path,integrationId,versionId}` — the
     latter pushes a recalled object straight into a configured cloud
     integration (`cloudIntegrationType` ∈ AWS|WASABI|ORACLE|GEYSER). If the
     `AWS` type accepts a custom endpoint, Geyser can rehydrate directly into
     iDrive/Lyve and the staging drain never touches our bandwidth — build
     the polling path first (S3-only, no new auth), then swap the transfer
     leg. **Console auth WIRED (2026-07-29, WP-Geyser-Admin): `geyser_admin.go`
     now does programmatic `POST /api/login` + MFA verify + cookie jar, and
     exposes `RestoreToCache`/`RestoreToCloud`/cloud-integration CRUD/
     `cloudSync`/tape/site/event endpoints.** Remaining blocker: confirming
     custom-endpoint support on the `AWS` integration type (ask Geyser).
   - **Rehydrate target is pluggable** — any registered `engine.Driver`, one config knob (`RESTORE_REHYDRATE_BACKEND`). For the current OneDrive+Geyser primary stack, **permafrost is the natural bulk target**: $0 marginal cost (Business subs already paid), fleet download 200+ MB/s for customer-facing restores, and TTL'd temporary data suits OneDrive quota churn. Write-side match is fine: staging drains at ~5 MB/s/object anyway, well under permafrost's 12 MB/s/file (fleet-parallel ~100-200 MB/s aggregate across objects). **Two gates before permafrost backs customer-visible restores**: root-cause the 10/100 GET errors from the 2026-07-15 E2E smoke (fleet propagation/throttle — flagged in benchmark memory), and accept the Graph-API rate-limit/ToS posture (restored copies are transient, which lowers but doesn't eliminate it). iDrive remains the boring fallback target; NVMe stays the first hop for small restores either way.
3. S3 wire compatibility = exactly AWS Glacier semantics, so rclone/Cyberduck/aws-cli restore workflows work unmodified: GET on archived → `InvalidObjectState`; accept `RestoreObject` passthrough; HEAD returns `x-amz-restore` status; `s3:ObjectRestore:Completed` event → webhook (Phase 5.11.6).
4. Bulk "thaw": restore-by-prefix API (one call → server-side RestoreObject fan-out, measured fine in batches) with progress in `restore_jobs` table + dashboard bar + completion webhook/email. This is the "restore a whole restic snapshot" UX.
5. SLA copy (honest, measured): "restores begin within minutes" (recall <3 min idle — re-verify under contention before printing an SLA number); drain from staging parallelized across N workers, sized against the shared gateway. **Drain re-measured (2026-07-29, HTTP/1.1): ~2.5-3 MB/s per stream, scales with fan-out — 8 streams → 25 MB/s, 32 streams → 76 MB/s aggregate ≈ 6.4 TB/day (wider fan-out untested, keep going; the earlier "~2 TB/day" figure was the h2-client bug + low concurrency).** So: progressive restore (serve each object the moment it lands hot, never wait-for-all), drain scheduler prioritizes user-requested objects first, restore workers fan out 32-64 streams, and honest copy is "restores begin in minutes; bulk restores drain multiple TB/day (shared)". Bulk recall re-verified at 24 objects (<3 min) and submission at 17.6 req/s.

### V18.3: Dual-Site Tape Replication
**Files**: `internal/drivers/geyser.go`
- Write to both LA and London Geyser endpoints
- Geographic redundancy: survive datacenter loss
- Async replication: primary write to nearest site, background copy to remote
- London bucket re-creation needed (deleted 2026-04-20)

### V18.4: Glacier Migration Tooling
*2026-07-29: Geyser already ships server-side ingest-from-cloud —
`POST /api/cloudSync {source:{type,region,bucket,accessKey,secretKey},action:"SYNC",bucketId}`
with `PENDING|QUEUED|INPROGRESS|COMPLETED` status and an SNS completion topic
(`/api/cloudSync/confirm`). This is the published "Wasabi cold archiving"
integration. For AWS/Wasabi/Oracle sources a migration may need zero
bandwidth from us — wrap cloudSync instead of proxying bytes.*

**Files**: `cmd/glacier-migrate/` (new)
- CLI tool: `glacier-migrate --source s3://bucket --dest stored.ge/bucket`
- Wraps `aws s3 sync` with Glacier restore orchestration
- Handles restore requests, waits for availability, copies to stored.ge
- Progress reporting with ETA

### V18.5: Retention Policies + Immutability
**Files**: `internal/api/s3_handler.go` (extends Object Lock from 5.10.13)
- *2026-07-29: Vail returns proper `ObjectLockConfigurationNotFoundError` — the gateway speaks WORM semantics. Ask Geyser support for an object-lock-enabled bucket before building; backend-native lock would make immutability real end-to-end, not just Vaultaire-layer.*
- Vault18-specific retention templates: 1yr, 3yr, 7yr, 10yr
- Immutability certificates: cryptographic proof that an object hasn't been modified
- Integration with Object Lock COMPLIANCE mode

### V18.6: Cost Calculator
**Files**: `internal/dashboard/handlers/billing.go`, template
- Interactive calculator: enter TB, access frequency → show monthly cost
- Comparison table: stored.ge Vault18 vs Glacier vs Glacier Deep Archive vs Wasabi vs B2
- Highlight: "$0 retrieval" vs Glacier's $90/TB retrieval fee

### V18.7: Quota Pricing Integration ⚠ LAUNCH-CRITICAL (minimal version = Stage 4, week of Oct 5)
**Files**: `internal/billing/stripe.go`
> **RESCOPED 2026-09-21 (quota-sold decision, "Pricing Shape Decision" section above).** The Vault1/3/5/10/18/50/100 pack ladder is GONE (2026-08-04 linear reprice: **Vault Flex** $2.55/TB/mo monthly · $2.00/TB annual prepaid, any size; **Founders** $1/TB capped at 100 TB, 3–10 TB/acct). **Standard** $4.49/TB/mo annual · $4.99 monthly (the old $3.99 survives only as the LET2026 coupon). **Performance PARKED** — no Stripe product at launch, launches post-launch at $6.99. No Stripe Billing Meters at launch.
- Stripe LIVE prices, one plain subscription price × TB quantity each: Standard-monthly ($4.99), Standard-annual ($4.49), Vault-monthly ($2.55), Vault-annual ($2.00) — hard quota via WP-1 + `tenant_quotas`, prorated resize via the existing plan-change webhook; allowances keyed to quota (hot 15%, egress 0.5× quota/mo Standard, 1× restore Vault)
- Founders coupon/price ($1/TB, 100 TB cap) + LET2026 coupon (10% off 3 months)
- Optional add-on: pin-hot at $3/TB-mo × pinned quantity
- **Minimal launch scope**: create those prices + coupons in Stripe LIVE, wire the price IDs into prod `.env` (`STRIPE_PRICE_*`), enable Stripe Tax, run one real-card cycle — NO Vault packs, NO meters (2.7's reporter stays dormant). The rest of V18.1-V18.8 follows post-launch on existing plumbing (tiering engine #297 + storage-class routing #251 + #425 archive-whole-on-Geyser already route archive data to tape)

### V18.8: Deep Archive Dashboard
**Files**: `internal/dashboard/handlers/archive.go` (new), templates
- Archive-specific view: objects in storage, retrieval queue, retention status
- Restore request UI: select objects → choose speed (standard/expedited) → track progress
- Immutability certificate download per object
- Tape location indicator (LA / London / both)

### V18.9: Backup-Tool-Aware Routing
**Files**: `internal/engine/routing.go` (extends storage-class routing from 5.12.4)
- restic/borg/kopia CANNOT run against Glacier-class storage directly (prune/check need reads). Route by key pattern within a Vault bucket: restic `config`, `keys/`, `index/`, `snapshots/`, `locks/` → hot backend (iDrive); `data/` packs → Geyser tape. Borg/kopia equivalents.
- Makes Vault the only ~$2/TB archive tier restic works against out of the box — flagship differentiator, pairs with V18.2 for `data/` pack restores.
- Document per-tool golden paths in the docs hub (B3).

**Test**: Upload to Vault18 tier → verify ingest buffer accepts immediately → background migration to Geyser starts → wait past staging eviction → GET returns InvalidObjectState → request restore (rclone + dashboard) → recall completes, rehydrated to hot tier → webhook fires → download at hot line-rate → verify retention policy prevents deletion → restic init/backup/restore round-trip against a Vault bucket (V18.9 routing) → cost calculator shows savings vs Glacier.

---

## Phase 5.13: Email Infrastructure
*Depends on: Phase 5.6 (email verification tokens exist but don't actually send)*

> **Phase 5.13 COMPLETE** — PR #246

### 5.13.1: Email Service Abstraction
**File**: `internal/email/email.go` (new)
- `Sender` interface: `Send(ctx, to, subject, htmlBody) error`
- Three implementations:
  - `ResendSender` — Resend API (production, $0 for first 3000/month)
  - `SMTPSender` — generic SMTP (self-hosted fallback)
  - `LogSender` — dev/test mode, logs email to stdout
- Rate limiting: 10 emails/minute per tenant, 100/day

### 5.13.2: Branded HTML Templates
**File**: `internal/email/templates/` (new), `internal/email/templates.go`
- Base email template: stored.ge header, content, footer with unsubscribe *(2026-10-12, WP-DOMAIN-1): header = "Stored"; `EMAIL_FROM` and every link in a template on stored.cloud / app.stored.cloud; stored.ge addresses stay as receiving aliases.*
- Templates: verification, password reset, bandwidth alert, welcome, plan change
- `//go:embed` for single-binary deployment
- Plain-text fallback for each template

### 5.13.3: Wire Email Sending
**Files**: `internal/dashboard/handlers/email_verify.go`, `internal/dashboard/handlers/account.go`, `internal/api/bandwidth_alerts.go`
- Verification email: actually sends now (was stub before)
- Password reset email: sends HMAC-signed reset link
- Bandwidth alerts: sends warning at 80%/95% thresholds
- Welcome email: sent after email verification confirmed

### 5.13.4: DNS Configuration (ops, not code)
- SPF record: `v=spf1 include:resend.com ~all`
- DKIM: configure via Resend dashboard
- DMARC: `v=DMARC1; p=quarantine; rua=mailto:dmarc@stored.ge`
- **(2026-10-12) WP-DOMAIN-1, mail part:** SPF/DKIM/DMARC on `stored.cloud` (the sending domain re-verified on .cloud), `rua=mailto:dmarc@stored.cloud`; `stored.ge` keeps its records so inbound `security@` / `support@` / `dmarc@stored.ge` forward forever; DNSSEC DS for both zones at Porkbun; test = send from `@stored.cloud`, all three pass, no spam folder.
- Verify: send test email → check headers → no spam folder

**Test**: Register new account → verification email arrives in inbox (not spam) → click link → verified → request password reset → reset email arrives → click link → set new password → login works.

---

## Phase 5.14: Launch Compliance
*Depends on: Phase 5.13 (email for breach notifications). Items pulled forward from Tiers 2-4 — required at or near launch.*

### 5.14.1: GDPR Data Export + Account Deletion
**File**: `internal/api/account_export.go`, `internal/api/account_deletion.go`, migration `038_account_deletion.sql`
> **Phase 5.14.1 COMPLETE** — PR #253
- GDPR Article 20: data portability — export all user data as JSON bundle
- Export includes: profile, buckets, API keys, usage history, settings (NOT object data — too large, use S3)
- GDPR Article 17: right to erasure — request account deletion
- 30-day grace period before permanent deletion (soft delete first)
- `deletion_requests` table: user_id, requested_at, scheduled_for, status
- Cancel deletion during grace period
- On permanent delete: remove user, tenant, API keys, sessions, buckets metadata (object data deleted by backend)

### 5.14.2: Legal Documents
**File**: `internal/dashboard/handlers/legal.go`, templates
> **Phase 5.14.2 COMPLETE** — PR #254
- Privacy Policy (`/legal/privacy`) — data collection, storage, sharing, rights
- Terms of Service (`/legal/terms`) — acceptable use, liability, SLA
- Data Processing Agreement (`/legal/dpa`) — GDPR Article 28, for business customers
- Cookie Policy (`/legal/cookies`) — what cookies, why, opt-out
- Acceptable Use Policy (`/legal/aup`) — prohibited content, enforcement
- Footer links on all pages, checkbox on registration form

- **(2026-10-12) Verdict alignment, before Oct 31 (WP-DOMAIN-1 + 41.0):** controller named "Stored, a product of FairForge"; service domain `stored.cloud` with stored.ge / stored.pics listed as aliases; the privacy policy carries the per-mode table generated from `modes.json` in the names **Private / Standard / Public** — Standard = "encrypted at rest with keys we hold", Private = outcome sentences, Public = plaintext — never "zero-knowledge" before an external audit; the content domain named as where public objects are served; retention periods stay in lock-step with the retention job.

### 5.14.3: MFA Delete + SOC 2 Evidence
**File**: `internal/api/s3_handler.go`, `internal/compliance/soc2.go`, migration `036_mfa_delete.sql`
> **Phase 5.14.3 COMPLETE** — PR #255
- MFA Delete: `x-amz-mfa` header required to delete versioned objects when enabled
- TOTP verification inline with S3 delete request
- SOC 2 evidence tracking: `internal/compliance/soc2.go` maps Trust Services Criteria to implemented controls
- Evidence collection started (CC6.1 logical access, CC7.1 monitoring, CC8.1 change management)
- Formal audit deferred to Q4 2026 (Type I), H1 2027 (Type II observation)

### 5.14.4: Post-Quantum SSE-S3 Encryption
**File**: `internal/crypto/sse_s3.go` (stdlib `crypto/mlkem`; the dead `postquantum.go` was deleted 2026-10-10, D-3), migration `037_sse_s3.sql`
> **Phase 5.14.4 COMPLETE** — PR #256
- ML-KEM-768 (NIST FIPS 203) for key encapsulation — quantum-resistant
- AES-256-GCM for symmetric data encryption — hardware-accelerated
- Hybrid: ML-KEM encapsulates shared secret → HKDF → AES key → encrypt
- Per-tenant keys: stored-random, versioned, wrapped by the master key — rotation, the re-encrypt job, master re-wrap and crypto-shred (WP-R8-1, #682, migration 084); the `sse_s3_keys` keypair design is history
- Transparent: enabled via `ENCRYPTION_MASTER_KEY` env var (64 hex chars) *(2026-10-12, F-14 install): the key is a systemd credential (`LoadCredentialEncrypted=`, sealed `host+tpm2`, no PCR binding, dedicated service user, mount namespacing) read from `$CREDENTIALS_DIRECTORY` with the env var as the dev fallback — a code change before the flip, since `server.go` reads only the env today. Escrow = age-plugin-sss 2-of-3; plaintext never leaves the box; every ceremony logged and in the transparency report (F-16).*
- Objects encrypted on PUT, decrypted on GET — caller sees plaintext
- This is **Standard mode** (F-15): server-side encryption, "encrypted at rest with keys we hold" — the master key protects the bytes from the storage vendors and never changes that we can read Standard-mode data; any widening of access is a public event (F-16). "Can't read" exists only client-side: SSE-C (5.14.8 / WP-SSEC-1), the client-side tools guide (rclone crypt, restic), and Private mode (Phase 10.0, Ente's hierarchy, F-12). Before the flip: ~~`generatedObjectWriter` writes plaintext into SSE buckets~~ (fixed #685) and WP-R3-1 is pulled ahead (2026-10-12).
- Critical for Geyser multi-tenant: tape stores data from all tenants, must be encrypted at rest

### 5.14.5: HIPAA Readiness
**File**: `internal/compliance/breach.go`, `internal/dashboard/handlers/legal.go`
> **Phase 5.14.5 COMPLETE** — PR #258
- BAA (Business Associate Agreement) template at `/legal/baa`
- Breach notification workflow: `breach_notifications` table, 72-hour notification SLA
- PostgreSQL-backed breach store (`internal/compliance/breach_pg.go`)
- Emergency access procedure documented
- HIPAA Security Rule mapping: access control, audit controls, integrity, transmission security all covered
- Pre-BAA signing checklist: 7/10 items complete (remaining: formal risk assessment, full request logging, pen test)

### 5.14.6: GDPR and EU Data Act
**File**: `internal/compliance/gdpr.go`, `internal/dashboard/handlers/compliance.go`
> **Phase 5.14.6 COMPLETE** — PR #259
- `/legal/gdpr` — GDPR compliance page with data subject rights
- `/legal/data-act` — EU Data Act compliance (data portability, interoperability)
- Consent management UI: view/revoke consents
- Data residency documentation: where data is stored, which jurisdictions

### 5.14.7: Per-Bucket Region Selection
**File**: `internal/api/s3_handler.go`, `internal/drivers/idrive_regions.go`, migration `039_bucket_region.sql`
> **Phase 5.14.7 COMPLETE** — PR #260
- `region` column on `buckets` table, set at bucket creation
- 8 iDrive regions available: us-east-1, us-west-1, eu-central-1, eu-west-1, ap-south-1, ap-southeast-1, ap-northeast-1, ca-central-1
- Region locked after creation (cannot move data between regions)
- Data sovereignty: EU customers can ensure data stays in EU
- Dashboard: region selector dropdown on bucket creation form
- S3 `x-amz-bucket-region` header on responses

### 5.14.8: SSE-C (Customer-Provided Keys)
**File**: `internal/crypto/ssec.go`
> **Phase 5.14.8 COMPLETE** — PR #261
- Customer provides AES-256 key via S3 headers on every request
- `x-amz-server-side-encryption-customer-algorithm: AES256`
- `x-amz-server-side-encryption-customer-key: <base64-key>`
- `x-amz-server-side-encryption-customer-key-MD5: <base64-md5>`
- Stateless: key never stored on server (customer must provide on every GET)
- Encrypt on PUT, decrypt on GET — **single-object PUT/GET only today** (multipart → 501 `NotImplemented`, CopyObject unsupported, the object buffered in memory; 2026-10-12)
- Separate from SSE-S3 (5.14.4) — SSE-C is customer-managed, SSE-S3 is server-managed

### 5.14.8a: WP-SSEC-1 — SSE-C complete (added 2026-10-12; Engine lane, before Private mode)
> **WP-SSEC-1 DONE 2026-10-20** — #796 (migration 096 + the `ssec_required` flag), #797 (format 0x05: parts sealed at UploadPart bound to their part number, Complete without a key), #798 (multipart, Range, CopyObject/UploadPartCopy with keys on either side, the rotation); (6) decided: SSE-C bodies are never chunked and never dedup. `docs/reviews/WP-P17.md`.
SSE-C is the developer surface's "can't read" path until the SDK exists, so it must cover the whole API: (1) streaming AES-256-GCM (no whole-object buffer — non-negotiable 4); (2) multipart: key headers on CreateMultipartUpload + every UploadPart + Complete, key-MD5 checked each time, the algorithm marker stored on the upload row like the other attributes (068); (3) CopyObject / UploadPartCopy with source + destination key headers (re-encrypt in stream, never plaintext at rest); (4) a per-bucket **"SSE-C required"** flag (`buckets` column, S3 400 on an unkeyed PUT, dashboard shows it, surfaced in 5.14.10 as Private-by-key); (5) HEAD/GET without the key → 400 `InvalidRequest` as AWS; (6) the chunked path (>64 MiB) refuses or encrypts per chunk — decide; it never dedups SSE-C bytes across tenants. Tests: aws-cli and rclone SSE-C round-trips incl. multipart + copy; the compat suite. Copy: "we never see the key". Shares the streaming work of WP-R8-5 / WP-R3-3. **Added 2026-10-12 — the honest claim and its guard-rails:** with an "SSE-C required" bucket we can truthfully say *"we never store your key and cannot decrypt your stored data"* — true for everything at rest; the two caveats the copy carries: the key passes through our servers on every request (a modified server could capture it from that point on — once 41.4 exists, such a change is a visible measurement change; until then the sentence stops there), and names, sizes, timestamps and access logs stay plaintext (rclone crypt / restic encrypt names too and never send us a key — they outrank SSE-C). Guard-rails in the WP: (7) **the key never appears in any log** — HAProxy, the access log, error bodies, metrics labels, the edge provider's logs — and never in a stored key-MD5 (an unkeyed hash of a key is an oracle); (8) **SSE-C is never a bucket default** — tools that send no header break, and presigned links, public buckets and the CDN cannot serve it; (9) the `s3` host terminates TLS at our origin (F-17b) so the key is never plaintext inside the edge.

### 5.14.9: Server Access Logging + Inventory Reports
**File**: `internal/api/access_log.go`, migration `040_access_logging.sql`
> **Phase 5.14.9 COMPLETE** — PR #262
- S3 server access logging: record all requests to a log bucket
- Log format: timestamp, requester, bucket, key, operation, status, bytes, latency
- Buffered writes (batch inserts every 5 seconds or 100 records)
- Inventory reports: CSV export of all objects in a bucket with metadata
- `GET /api/v1/manage/buckets/{id}/inventory` → streamed CSV download
- Required for SOC 2 audit trail and compliance evidence

### 5.14.10: Customer Compliance Dashboard
**File**: `internal/dashboard/handlers/compliance.go`
> **Phase 5.14.10 COMPLETE** — PR #263
- `/dashboard/compliance` — consolidated view of compliance posture
- Per-bucket status: encryption (SSE-S3/SSE-C), versioning, Object Lock, region, access logging *(2026-10-12): the encryption column reads the bucket's mode from `modes.json` — Public (plaintext, public-read), Standard (SSE-S3, keys we hold), Private (SSE-C-required or a 10.0 space) — the same strings as the privacy page and OpenAPI, under one test.*
- Compliance score: percentage of buckets with recommended settings enabled
- CSV export of compliance report for auditors
- Recommendations: "Enable versioning on 'backups' bucket for data protection"

### 5.14.11: Security Hardening
**File**: `internal/api/middleware.go`, `internal/dashboard/handlers/admin.go`
> **Phase 5.14.11 COMPLETE** — PR #257
- Security headers: Content-Security-Policy, X-Frame-Options, X-Content-Type-Options, Referrer-Policy
- Request body size limits: 64KB for management API, unlimited for S3 PUT
- Resource limits: 1000 buckets per tenant, 50 API keys per tenant
- Admin 2FA enforcement: admin users MUST have 2FA enabled (redirect to setup if not)
- Rate limiting hardening: stricter limits on auth endpoints (login, register, password reset)

**Test**: Compliance dashboard → all buckets show encryption status → enable SSE-S3 on new bucket → upload → verify data encrypted at rest → export compliance CSV → verify GDPR page renders → BAA page accessible → check security headers in browser dev tools → verify admin without 2FA is redirected to setup.

### 5.14.12: Public Status Page
- Use hosted service (BetterUptime, Instatus, or Atlassian Statuspage — free tier)
- Monitor: `https://stored.ge/health`, S3 PUT/GET smoke test, dashboard login
- Public at `status.stored.cloud` (2026-10-12, WP-DOMAIN-1; `status.stored.ge` 301s); it monitors `https://stored.cloud/health`, the synthetic customer-path check against `s3.stored.cloud`, `app.stored.cloud/login`, and `https://stored.ge/health` (the alias must keep answering). The in-app `/status` page stays as the origin-side page and links out — one footer link, not two unlinked pages (closes the 5.15.6 question)
- RSS feed for status updates
- Incident template: what happened, impact, timeline, resolution, prevention
- **Why now**: customers need to know if the service is up BEFORE contacting support. Trust signal from day one.

---

### 5.14.13: WP-DOMAIN-1 — one domain before Oct 31 (added 2026-10-12, F-17)
**stored.cloud canonical** (it already serves the app); stored.ge and stored.pics permanent aliases. Map: `stored.cloud` home · `stored.cloud/developers` + `docs.stored.cloud` · `app.stored.cloud` (dashboard) · `s3.stored.cloud` (own host → own Cloudflare rules) · `trust.stored.cloud` (the F-16 log + 43.4 artefacts) · `status.stored.cloud` · `stored.cloud/photos` later (stored.pics 301) · `stored.cloud/platform` (Phase 30) · public content on a separate non-brand content domain (5.10.6). **stored.ge served forever:** web 301; the S3 API answers on both names, no redirect (a signed host must not move under a client). **Touches:** `VAULTAIRE_ENDPOINT` / `VAULTAIRE_BASE_URL`, the hard-coded `security@stored.ge` in `internal/api/security_txt.go` (+ the `SECURITY_POLICY_URL` default), legal pages (5.14.2), OpenAPI `servers` / contact (`internal/docs/openapi.go`), landing sources (`make landing`), OAuth callbacks (Google / GitHub consoles), the Stripe webhook (a new endpoint on .cloud, pinned 2023-08-16), `SYNTHETIC_CHECK_URL`, the e-mail sending domain (5.13.4), the 5.15.4 smoke list, Cloudflare zone rules redone on the stored.cloud zone (`docs/CLOUDFLARE.md`), DNSSEC DS at Porkbun. **Owner sub-decisions (F-17a/b):** virtual-hosted bucket names (`*.s3.stored.cloud`, Cloudflare ACM $10/mo) vs path-style only; the `s3` host proxied (100 MB single-PUT limit) vs direct. Lane: Engine, M; the Developer-surface lane starts the docs site and status page on the new hosts in parallel.

## Phase 5.15: Pre-Launch Validation
*After 5.14 compliance is done. Final gate before accepting paying customers. All items here are operational, not feature work.*

### 5.15.1: Graceful Shutdown
**Files**: `cmd/vaultaire/main.go`, `internal/api/server.go`
> **Phase 5.15.1 COMPLETE** — shipped in main.go (systemd `TimeoutStopSec` verified); H-3 shutdown flush (#419) drains the batched hot-path trackers on the same signal path.
- `os.Signal` handler (SIGTERM, SIGINT) → stop accepting new connections → drain in-flight requests (30s timeout) → close DB pool → close Redis → exit cleanly
- Without this, every deploy drops active uploads mid-stream
- Systemd: `TimeoutStopSec=45` in vaultaire.service (gives 30s drain + 15s buffer)
- Log on shutdown: "draining N in-flight requests" + "shutdown complete"

### 5.15.2: Load Testing
**Files**: `tests/load/` (new, gitignored results)
> **Phase 5.15.2 COMPLETE** — harness #282; ✅ RUN against prod 2026-08-03 (#423): all gates pass, results in `bench-results/LOADTEST-2026-08-03.md` (RSS transient = the #400 deferred memory cap, quantified). Rerun scheduled after the Stage-2 cutover (Stage 3, scripted).
- Use `k6` or `hey` against staging (or SLC with test tenant)
- Scenarios: 100 concurrent S3 PUT (1MB), 100 concurrent GET, 50 concurrent multipart (100MB), mixed read/write, management API burst (100 req/s per tenant)
- Validate: no 5xx under load, p99 latency < 500ms for GET, rate limiter kicks in correctly, DB connection pool doesn't exhaust, memory stays bounded
- Identify: connection pool limits, goroutine leaks, slow queries under concurrency
- Run once, fix issues, run again. Not a recurring suite — a one-time gate.

### 5.15.3: Landing Page
**Files**: `internal/dashboard/templates/public/landing.html` (new), `internal/dashboard/handlers/public.go` (new)
> **Phase 5.15.3 COMPLETE** — PR #272 (waitlist page), superseded by the real homepage in 5.15.6 B1 (#354) + the #439 Vault Flex / Oct-31 copy refresh.
- `GET /` for unauthenticated users → marketing landing page (authenticated users redirect to `/dashboard`)
- Single page: hero ("$4.49/TB S3-compatible storage" — quota-sold; $3.99 is now only the LET2026 coupon), feature grid (zero egress, PQ encryption, S3 compatible), pricing table (Standard/Performance/Vault tiers), "Get Started" CTA → `/register` *(2026-10-12): the encryption tile reads "encrypted at rest with keys we hold (ML-KEM-768 + AES-256-GCM)"; Private-mode copy is outcome sentences; "zero-knowledge" is forbidden until the external audit.*
- htmx + Go templates (consistent with dashboard, no separate frontend)
- Open Graph meta tags for social sharing
- `stored.ge/pricing` route for direct link to pricing section

### 5.15.4: Production Smoke Test Checklist
- Not code — a manual run-through before announcing:
  - [ ] Register new account → verification email arrives → verify
  - [ ] Login → dashboard loads → create bucket → upload file → download file
  - [ ] Stripe checkout → webhook fires → plan upgraded → quota increased
  - [ ] Admin dashboard → see new tenant → suspend → S3 returns 403 → enable → works
  - [ ] CDN: toggle public → upload image → CDN URL serves it → toggle private → 404
  - [ ] `aws s3 cp` and `rclone` work against `s3.stored.cloud` AND `s3.stored.ge` (alias answers, no redirect — 2026-10-12)
  - [ ] Password reset flow works end-to-end
  - [ ] 2FA setup + login works
  - [ ] Health endpoint returns 200: `curl https://stored.ge/health`
  - [ ] SSL certificate valid, HSTS header present
  - [ ] Backup restore test: restore latest pg_dump to a test DB, verify data intact

### 5.15.5: Production Backend & Activation Gate
*Added 2026-06-02 (prod audit). The real blockers to accepting paying customers — NOT covered by 5.15.2 (load) or 5.15.4 (pg backup/SSL/smoke). Prod must be in this state before signups reopen.*
- **Prod on a real backend, NOT local disk.** ✅ Prod primary = iDrive (NEW reseller acct d15e6040 since 2026-09-21; Dallas key swapped into prod 2026-09-22 after the old acct lapsed and 2 weeks of silent `InvalidAccessKeyId` PUT failures) + Geyser (archive) + Lyve (`resilient`/fallback). Historical: the 2026-06-02 audit found `STORAGE_MODE=quotaless` with no creds → `local` fallback on the single SLC box. Remaining (Stage 2, owner): confirm iDrive payment on the new acct, the `ENCRYPTION_MASTER_KEY` flip = the F-14 ceremony (2026-10-12: after WP-R8-1 #682, the `generatedObjectWriter` fix and WP-R3-1; systemd credential `host+tpm2`, dedicated service user, `libtss2-rc0`; age-plugin-sss 2-of-3 escrow; logged + in the transparency report; follow-up WP: TPM-resident KEK), migrate or discard bench/test data.
- **Object-data durability.** 5.15.4's backup test covers Postgres only. Objects on local disk have NO backup/redundancy — a disk/box loss = total data loss. Confirm objects live on a durable backend (the cloud tier replicates) before taking customer data.
- **Stripe LIVE quota prices created + one real-card cycle (decision 2026-09-21 — replaces "metered billing activated").** All tiers are quota-sold: plain subscription price × TB quantity (V18.7-minimal, Stage 4 week of Oct 5) + Stripe Tax. 2.7's reporter (#269) stays DORMANT — Billing Meters are NOT needed at launch; `STRIPE_METER_*` stays unset.
- **Reopen signups.** Public signups are CLOSED pre-launch (`SIGNUPS_ENABLED=false`, gated at `auth.CreateUserWithTenant`; PR #274). Set `SIGNUPS_ENABLED=true` + restart — but ONLY after the backend/durability items above are done. Never take customer data onto an unbacked local disk. (Now a runtime `signups` feature flag via 1.13 — flip in Stage 6.)
- **5.5.1 CSAM scanning (Cloudflare CSAM Scanning Tool + NCMEC runbook) is a HARD blocker before signups open** — see `.private/LAUNCH_EXECUTION_SEQUENCE.md` Stage 5.5. **Scope (2026-10-14, F-21):** known-hash matching on the public/shared surface only; never a classifier on private content; threshold + human review; disable the link first, never auto-delete an account; appeal; counts in the transparency report.
- **TLS auto-renewal.** ✅ Resolved 2026-07-09 (Stage 0): the "cert expires 2026-07-28 — launch week" blocker was stale — the Cloudflare origin cert is valid to 2041; launch is Oct 31, 2026. 5.15.4 still checks validity + HSTS.
- **Cloudflare:** cache rule for `/` (offload landing spikes) + a rate-limit/bot rule on `/api/waitlist` and `/auth/register`.
- **Why**: 5.15.2 + 5.15.4 gate *behavior*; this gates *the operational substrate*. Finishing 5.15.1-5.15.4 without this = "feature-complete" but customer data lands on a single unbacked disk. This is the true "can I charge a customer" line.

### 5.15.6: Day-1 Customer Experience Audit Fixes
*Added 2026-07-08 (day-1 UX audit — walked the LET-reader path from landing → signup → first PUT → docs). The engine is launch-ready; the **front door is not**. Almost every item is wiring/copy on top of working code, so most are hours not weeks. The LET post promises "Signup: https://stored.ge — 5GB free, no card… keys are on the dashboard, works in under a minute" — these items make that promise true.*

**🚨 LAUNCH BLOCKERS**

- **B1 — Replace the waitlist landing page (patches 5.15.3, supersedes PR #272/#274). ✅ DONE — #354 (+#360-362), refreshed for Vault Flex / Oct 31 / 2026 competitor prices in #439.** The served `internal/api/landing.html` *was* a *pre-launch waitlist for the wrong product*: "Enterprise S3 + VPS" bundles (2GB RAM / 2 vCPU for $3.99/mo), branded on **Seagate Lyve Cloud** (not a customer tier — it is the `resilient`/DR leg), fabricated trust stats (10PB+, "decades of experience", 99.99% SLA), a countdown to July 31, and a **"Sign In Coming Soon"** modal. It links to `/login`, `/register`, `/dashboard` **nowhere** — the working product is reachable only by URL-guessing. Ship the 5.15.3 real homepage: hero (now "$4.49/TB S3-compatible storage" — quota-sold; $3.99 only via LET2026), honest pitch (reuse `.private/launch/LET_LAUNCH_POST.md` copy), pricing table (Standard/Performance/Vault), CTAs → `/register` + `/login`, footer linking `/pricing` + `/status` + `/legal/*`. Kill the fake VPS/RAM/vCPU product entirely.
- **B2 — Web signup must surface S3 credentials (patches 5.11.7). ✅ DONE.** Web form: PR #350 (2026-07-20) renders the minted access key + secret ONCE on a post-signup credentials page (`signupCredsRenderer`, `pageContent("credentials")`). OAuth signup: PR #417 (2026-07-31) closes the last gap — `CreateUserFromOAuth` now returns the minted APIKey, and a brand-new OAuth account gets the same reveal-once credentials page instead of a bare `/dashboard` redirect (existing-user OAuth logins still redirect; secret never persisted or re-shown). JSON `/auth/register` already returned creds. All three signup paths now surface the secret exactly once.
- **B3 — Actually serve customer docs. ✅ DONE — #355.** All nine `internal/docs/*` packages are **empty scaffolding** (builders, zero content, imported nowhere). The only doc route is `GET /docs` → a **mis-branded** Swagger ("Vaultaire", `support@vaultaire.io`, MIT) covering **7 of ~40** operations (`internal/docs/openapi.go:216`). A complete `docs/guides/rclone-setup.md` exists on disk but is **not served**. Serve, at minimum: getting-started, rclone, FAQ — **launch-ready copy for all three drafted 2026-07-08 (this session)**; drop into `internal/docs` + link from dashboard and landing footer. Add aws-cli + restic pages as fast-follow. *(2026-10-12): plus the **client-side encryption guide** — `rclone crypt`, restic, Kopia over Standard buckets — the documented developer-surface "can't read" path until the SDK (10.0.1); served at docs.stored.cloud.*
- **B4 — Copy-pasteable, runnable connection config in the dashboard. ✅ DONE — #351.** Onboarding tabs (`dashboard.html`) hardcode `region: "us-east-1"` and a literal `YOUR_SECRET_KEY`, have **no copy buttons**, and **no rclone tab** (5.11.7 planned one; it's absent) — so the "getting started" code can't run as-is. Fix: inject the user's real access key (never secret in HTML — reveal-once elsewhere per B2), add rclone + s3cmd blocks, add copy buttons (extend the `.btn-copy` handler already used for CDN URLs).

**📚 DOCS AUTO-SYNC ("update automatically like Stripe") — the launch tier**
*Stripe's property that matters: docs can never silently drift from the API. Reference + code samples are derived from one source; only prose is hand-written. Get the property cheaply — don't build spec-generation for launch.*

- **Tier 0 — Drift guard (do first, ~½ day). ✅ DONE — #351 (route↔OpenAPI drift guard in CI).** CI test that walks the registered chi routes and asserts every S3/management route appears in the OpenAPI spec and vice-versa. Generates nothing; just **fails the build** when spec and code disagree. Catches the exact bug already live (7-of-40, mis-branded). Fits existing "CI must pass" discipline.
- **Tier 1 — One source for volatile values (~½ day).** Centralize endpoint (`https://stored.ge`), region, and the pricing table in a single Go config, injected into the OpenAPI spec + dashboard onboarding tabs + doc pages. One edit updates every snippet/page at once. **Also permanently fixes the region inconsistency** (`us-east-1` signing/examples at `handlers.go:271` vs `us-west-1` bucket default at `buckets.go:133`) — pick `us-east-1`, document it, reconcile the bucket default.
- *(Tier 2 generated per-user code samples + Tier 3 spec-from-handlers → Phase 26.5.)*

**🔶 FAST-FOLLOW (days after launch)**

- **Billing reads as $0 on day 1.** Zero-usage zeroes every provider in the comparison (`billing.go:255-293`) → "You save **$0.00**/mo vs AWS" — the marquee value prop looks broken. Plan cards render only if Stripe is configured (`billing.html:60-76`), else "Plans are being configured." Show list pricing / a "what will N TB cost" estimate even at zero usage.
- **Tier-name inconsistency:** `starter` (dashboard/usage) vs `free` (quota logic) vs `standard`/`vault*` (billing) — user can't tell what plan they're on. Normalize.
- **Link the trust pages you already built.** `/legal/{terms,privacy,aup,dpa,gdpr,cookies,baa,data-act}` and the in-app `/status` page (`server.go:493`) **work but are orphaned** (footer links are all `#`; footer "SLA" link has no backing route). Link them from the new footer; add `/legal/sla` or drop the link. Reconcile with 5.14.12 (decide in-app `/status` vs hosted `status.stored.ge` — don't ship both unlinked). **Decided 2026-10-12 (F-17): hosted page at `status.stored.cloud`, in-app `/status` kept as the origin page that links to it.**
- **Wire a real contact/support channel.** Footer "Contact" is dead; OpenAPI support email is the wrong domain. Surface `support@stored.ge` (LET post promises founder-direct <4h). *(2026-10-12, WP-DOMAIN-1): `support@stored.cloud` / `security@stored.cloud` (stored.ge addresses forward forever); OpenAPI `servers` = `https://s3.stored.cloud` with `https://stored.ge` listed as the alias; the security.txt contact is hard-coded in `internal/api/security_txt.go` — move it to the same host/contact source.*
- **OpenAPI rebrand + expand** (`openapi.go`): stored.ge branding, `support@stored.ge`, and cover the implemented ops beyond the current 7.
- **Email verification is never sent at signup and gates nothing** (`auth.go:326`; only manual resend in settings). Either enforce it or remove the dead "unverified" banner.
- **Onboarding dead items:** "Upload a file" + "Set up a webhook" checklist entries have no backing UI (no browser upload, no webhook settings page). Wire them or cut them; add a persistent "Connect / Quickstart" page so the info doesn't vanish permanently on dismiss.

**Test**: Anonymous browser hits `/` → sees real homepage with working Sign up / Sign in → register → **sees usable credentials without hunting** → dashboard quickstart has copy-paste rclone + aws-cli that run as-is → `/docs` serves getting-started + rclone + FAQ, all correctly branded → footer links reach `/pricing`, `/status`, `/legal/terms` → CI drift-guard test fails if a route is missing from the spec.

---

## Phase 5.15.7: Pre-Launch Hardening — Code / Billing / Security Correctness
*Added 2026-07-08 (deep correctness + billing + ops review). Peer of 5.15.6 — 5.15.6 fixes the front door, 5.15.7 fixes the engine/billing/security. The happy path works; real traffic exposes blockers the phase-completion checklists missed.*

> **Security note:** the detailed findings (exploit specifics, file:line, prod state) are kept **out of this public repo** and live only in the local, gitignored `.private/` bundle — `.private/LAUNCH_READINESS_REVIEW.md`, `.private/FABLE_FIX_PLAN.md` (ordered work packages WP-1…WP-15 with tests), `.private/LAUNCH_EXECUTION_SEQUENCE.md` (**the day-by-day driver — work from this**), and `.private/LAUNCH_RUNBOOK.md` (ops + first-week monitoring). This section is the high-level launch-gate summary only.

**🚨 CRITICAL — must land before launch (details in `.private/`) — ✅ ALL SHIPPED + DEPLOYED (Stage 1, 2026-07-17 → 07-20):**
- **WP-1 — Billing/quota accounting correctness.** ✅ SHIPPED #333 (2026-07-17; deployed, Gate C reconciliation run). Single-source reservation, release-on-delete, reconciliation job. It enforces the hard quota that quota-sold billing depends on (originally framed as "feeds the metered Stripe charge").
- **WP-2 — Bound GET memory / caching.** ✅ #337. The read path can retain object bytes in RAM without eviction; cap it so the single box can't OOM under real load.
- **WP-3 — Fail loudly on metadata-write failure.** ✅ #338. A swallowed head-cache write can return success on an object that then can't be read; return 5xx instead.
- **WP-4 — Harden S3 request authentication.** ✅ #329 (header-auth path; STREAMING-* per-chunk signatures deferred to WP-VG2). Verify request signatures on the header-auth path (staged; validate against the rclone/JuiceFS/aws-cli compat suite before merge).
- **WP-5 — Remove the non-production test-credential path** ✅ #328 — from the auth code; ensure prod runs with `ENV=production`.
- **WP-6 / WP-7 — Dedup pipeline integrity.** ✅ WP-7 #330, WP-6 #344. Make GC coherent with the in-memory index; use a deterministic chunking polynomial with tenant-scoped dedup when encryption is on (both together). Fixes silent-data-loss + correctness edges on the >64 MB chunked path.
- **WP-8 / WP-9 — Migrations.** ✅ WP-8 #335 (Gate A closed, DR restore test passed), WP-9 #336. Add the runtime-only tables (schema must rebuild from migrations alone — DR), fix GDPR-deletion FK cascades, make all migrations idempotent, and make the runner fail on error instead of swallowing it.
- **Ops (Phase 0 in `.private/LAUNCH_RUNBOOK.md`):** ✅ FIXED 2026-07-09 — the nightly Postgres backups were producing empty dumps because prod `.env` had gone root:root (Stage 0, see Status 2026-07-09); pg-backup.sh hardened + restore-tested. 5.15.5 remainder = Stage 2 owner items (see Status 2026-09-22).

**🔶 HIGH (WP-10…WP-15):** ✅ WP-10-minimal abandoned-multipart reaper + per-upload byte cap (#346); ✅ WP-11/H-3 batched hot-path writes + shutdown flush (#419); remaining (fix if clean, else document): tiering-migration revalidation; gate self-service tier changes on verified subscription; collapse the PUT bucket-config query storm; retention job for per-request tables.

**MEDIUM: do NOT touch pre-launch.** Notable: ~35 of 48 `internal/` packages are unreferenced by any binary — candidate cleanup post-launch.

**Test**: WP TDD tests pass (PUT→DELETE→quota baseline, concurrent-PUT-at-limit → one 403, cross-tenant encrypted round-trip, metadata-write-failure → 5xx); then load-test (5.15.2) against fixed+backed prod → RSS flat, no 5xx; then the Phase 0 backup restore-test and a fresh-DB migration rebuild both pass.

---

## Phase 5.15.8: Smart Demotion Job WP — ✅ COMPLETE, flag OFF (added 2026-09-22, shipped 2026-09-22/23)

> **Phase 5.15.8 COMPLETE** — PR #461 (demotion job + `smart_demotions` ledger, migration 062), PR #462 (read-time promotion + auto-restore on read, migration 063). Review R13 (#523) fixed the reclaim-vs-flip-back race that could lose both copies (R13-01) and made the admin trigger single-flight. WP-R13-1 landed 2026-10-01 (PR #542: the class comes from the floor — a demoted Standard object is STANDARD everywhere and `aws s3 sync` downloads it) and WP-R13-3 the same day (PR #543: the job runs daily at 06:30 UTC through the `job_runs` scheduler, with a catch-up after every restart and a 24-hour byte budget). **Both gates are closed; the `smart_demotion` flag is still OFF for every tenant** — next: dry-run → one canary tenant → global. WP-R13-2 landed 2026-10-02 (PR #547: no stale copy is left behind by an overwrite or a delete; R13-06 detected). Open: D-23 (buckets outside the default region are outside the hot budget — documented as always hot, to confirm).

**STATUS 2026-09-23: PR A SHIPPED (demotion + ledger) — `internal/api/smart_demotion.go`, migration 062 `smart_demotions`, flag `smart_demotion` (default OFF, ships flag-dark), admin `POST /api/v1/admin/smart-demotion?dry_run=true`. Routing flips in `object_head_cache.backend_name` under an etag guard; hot copy reclaimed after a 24h grace on the next run (row-locked, etag-checked). v1 scope = whole objects, non-versioned buckets, `tier_preference!=performance`. **PR B SHIPPED 2026-09-23: read-time promotion** (`smart_promotion.go`; within grace = routing flip back, after = async copy cold→hot with daily retry) + **auto-restore on read** for evicted objects (RestoreObject 7d on the reader's behalf, GET answers 503 + Retry-After instead of 403; Vault objects keep Glacier semantics); migration 063. Flag still ships OFF: enable per tenant after a dry-run + a canary tenant on prod.**
*Spec: `.private/SMART_TIER_DESIGN.md` (+ 2026-09-21 quota-sold addendum). Makes the Standard-tier margin model real: today tier placement is static, so the ~15% iDrive-share assumption is aspirational (Known Gaps above).*
**Files**: `internal/engine/tiering.go` (extend), `internal/api/` background runner (same pattern as `dedup_gc.go`), feature flag via 1.13
- Per-tenant **hot budget = 0.15 × quota_tb** (keyed to QUOTA, not stored bytes — quota-sold decision); pin-hot add-on bytes are exempt beyond the allowance
- Demote from iDrive → Geyser when **idle ≥ 14 days OR over-budget (LRU order)**; never demote an object < 3 days old
- Daily batch (not hourly); rate-limited like 7.3's migration queue; feature-flagged via `feature_flags` (1.13) so it can be killed per-tenant or globally
- Reuses: `object_locations` + LocationStore (7.1/7.2) for placement + `last_accessed_at`; V18.2's staged-restore path for promotion back to hot; H-3 batched trackers (#419) for egress accounting
- **NO overage billing** — egress is throttle-first at the allowance; overage billing is post-launch (stack #5)
- Supersedes Phase 7.3's hot<7d / warm 7-30d / cold 30+ brackets

**Test**: tenant with 10 TB quota → hot budget 1.5 TB → upload 2 TB → daily run demotes the LRU 0.5 TB (skipping anything <3 d old) → GET on a demoted object follows the V18.2 recall path → flag off → run is a no-op; idle-14d object on an under-budget tenant is still demoted.

---

## Review 2026-09 (R0–R15) — work packages by id (added 2026-09-30)

Sixteen review sessions (2026-09-25 → 09-30, `docs/CODE_REVIEW_PLAN.md`, findings in
`docs/reviews/`, the launch document in `docs/reviews/SYNTHESIS.md`) fixed every P0 and
all but eight P1s in PRs #473–#528. What remains is listed here by work-package id with
its launch-gating flag; sizes, files and dependencies are in SYNTHESIS.md.

**LAUNCH-GATING (before 2026-10-31):** ~~WP-R10-9 egress allowance enforcement
(throttle, egress-only counter)~~ **DONE — PR #539, 2026-10-01** (flag `egress_throttle`
OFF until the [YOU] row; D-25 to confirm) · ~~WP-R8-1 stored-random tenant keys + real key versioning~~ **DONE — PR #682, 2026-10-12** (migration 084; the F-14 ceremony may now follow)
(gating for the *copy* — every rotation/shredding claim is already removed) · WP-R3-3
SSE-S3 for multipart (gating the day `ENCRYPTION_MASTER_KEY` ships; needs WP-R8-5) ·
WP-R2-1 write-new-key-then-swap (gating only if versioning is sold as byte retention) ·
~~WP-R13-1 demoted objects must not present as GLACIER + WP-R13-3 job scheduling/catch-up~~
**DONE — PRs #542 + #543, 2026-10-01** (the `smart_demotion` flag is still OFF: dry run → canary → global) · WP-R11-3 webhook
delivery with retries (gating only if webhooks are marketed) · WP-R14-6 re-open the
encryption copy when the key ships · ~~WP-R4-1 GOVERNANCE bypass as a permission~~ (done, #548) ·
~~WP-R12-5 session-bound CSRF~~ (done, #549) · ~~WP-R12-8 server-side pending TOTP secret~~ (done, #550) · ~~WP-R7-5 routing truth~~ (done 2026-10-03)
routing-truth backfill (prod's 62 NULL / 447 local / 2,613 onedrive head rows) ·
~~WP-R10-3c erasure reaches every byte~~ (done, #554) · **WP-R8-7 one address for chunk blobs**
(found by WP-R10-3c: on a fixed-bucket backend a chunk is stored under the uploader's prefix — a
second tenant's dedup hit cannot be read, dedup GC never deletes the blob; `chunking` flag off
until it lands [YOU]) ·
WP-R9-7 backups off-box [YOU] · WP-R1-1 HAProxy `/metrics` deny + CF header strip [YOU]
· checklist rows 4/10/11/12/13 + Stripe [YOU] · ~~master-key decision~~ **decided 2026-10-12 (F-14): the on-box ceremony [OPS]** (systemd `host+tpm2` credential, dedicated service user, age-plugin-sss 2-of-3 escrow — the Mac-side `.private/STAGE2_RUNBOOK.md` is rewritten first) · **WP-DOMAIN-1 stored.cloud canonical before 2026-10-31 (F-17)** [YOU: DNSSEC DS at Porkbun, the two sub-decisions].

**PRE-LAUNCH NICE:** WP-R10-3b async GDPR export (the export half of WP-R10-3; design in
`docs/reviews/WP-R10-3-deletion-runner.md`) · ~~WP-R10-3d operator audit rows survive the
operator's erasure~~ (done, #553) · WP-R2-2 CDN through the adapter · WP-R14-1/2 CDN limiter + request
cost · WP-R2-3 client-body errors ≠ backend failures · WP-R6-3 per-op driver deadlines ·
WP-R6-1 location-authoritative reads/deletes · WP-R6-2 engine concurrency · WP-R10-2
webhook ordering · WP-R10-4 one product (delete legacy packs + meters; = WP-R14-5) ·
WP-R10-5 admin pages on the house model · WP-R10-8 ledger tightening · WP-R3-5 staging
dir · WP-R3-2 UploadPartCopy · WP-R3-4 batch-delete parity · WP-R4-2/3/4/5/6 bucket
state machine, CORS preflight, notification filters, buckets-row rule, hygiene · WP-R5-9
`api_keys.tenant_id` (= WP-R9-6) · WP-R5-4/5/6/10/12 auth hygiene · WP-R11-2/4/5/6/11 ·
WP-R12-1/2/3/4/9/11/12 · ~~WP-R13-2~~ (done, #547) · WP-R14-3 · WP-R8-2/3/4/6 · **WP-R8-5 streaming SSE (Engine-lane keystone, 2026-10-12 — built once, feeds WP-R3-3 and WP-SSEC-1)** · WP-R2-4/6/7 · **WP-R3-1 (pulled ahead of WP-R6-1, 2026-10-12: before the F-14 flip)** · WP-R3-6
· WP-R1-2/4/5/8/10 · WP-R9-3/5 · WP-R7-2/3/4/6/7 · WP-R0-1 (D-1..D-9) · WP-R0-4 ·
WP-R15-1 chunk-signature verification · WP-R15-2/3/4/5.

**POST-LAUNCH:** WP-R9-8 type normalisation · D-12 orphan-table drop (WP-R11-5) ·
D-24 Go 1.26 (WP-R15-3).

**DONE in the build phase:** WP-R10-3 account-deletion runner (D-15/D-16; = WP-R9-10/WP-R11-9/
WP-R12-7) → #529, 2026-09-30 — `internal/account` state machine, daily runner, migration 072,
legal copy; note `docs/reviews/WP-R10-3-deletion-runner.md` (post-merge review #537: stale
Stripe-cancel stamp, shared-database test walk). WP-R10-9 egress allowance enforcement (D-25) →
#539, 2026-10-01 — note `docs/reviews/WP-R10-9-egress-allowance.md` (post-merge review #540: the
stream guard's 503 no longer counts as a server error; #541: the egress tests wait for the server
handler).

**DONE by the review:** WP-R0-2/3/5/6/7/8/9/10/11, WP-R1-3/6/7/9/11, WP-R2-5, WP-R5-1/2/3/
7/8(part)/11/13(=R4-5 open), WP-R6-4/5/6/7/8/9, WP-R7-1, WP-R9-1/2/9, WP-R10-1/7,
WP-R11-1/8/10, WP-R12-6/10, WP-R13 built items, WP-R14-4/7/8, WP-R15-4.

Decisions waiting on Isaac: D-1..D-4, D-7, D-9, D-12..D-24 (table in SYNTHESIS.md;
D-19 and D-25 were *applied* and need confirmation).

---

# POST-LAUNCH 90-DAY PRIORITY STACK (added 2026-07-07)
<!-- reconstructed: new section — Tier 2 shipped ahead of schedule (Phases 7-10.3 done), so the original "build after first customers" order no longer applies. Ranked by revenue impact × retention × effort, targeting what LET/Reddit users and the first 50 customers will actually hit. -->

*Assumes launch **Oct 31, 2026** (moved Jul 31 → Aug 31 via #414 → Oct 31 on 2026-09-14) with Standard + Vault Flex, all quota-sold (2026-09-21 decision above). Re-rank monthly against actual customer signal.*

| # | Item | Phase | Why this rank | Effort |
|---|------|-------|---------------|--------|
| 1 | **Launch-week ops + incident readiness** — fix what smoke/load surface, watch error rates, honest status page updates. Backend-outage alerting is already live (5.12.10, #457: authenticated probes → Prometheus `/metrics` → Alertmanager → ntfy) — subscribe to the topic | 5.15.x fallout + 5.12.10 | Every early churn story starts with a bad first week. LET users stress-test harder than enterprises and post publicly about it. | ops |
| 2 | **Vault restore experience** — staged retrieval + restore-status UI (`x-amz-restore` compat, webhook on ready) | V18.2 + V18.8 | Vault Flex is the day-one differentiator; the first thing a backup buyer does after uploading is **test a restore**. A confusing tape-restore UX = refunds + bad LET thread. | M (2-3 sessions) |
| 3 | **`stored` CLI + rclone one-liner** (`stored rclone-config`) | 26.4 (pulled forward) | #1 predicted LET/datahoarder ask — this market prefers terminal over dashboard. Each guide doubles as a marketing post. Costs little: wraps the existing management API. | M |
| 4 | **S3 lifecycle rules (XML API)** | 12.1 | Veeam/rclone/backup tooling expects `?lifecycle` for expiration; needed for the Week 4-8 B2-migration campaign. Tiering engine (#297) already does transitions — this is the S3-compatible surface. | M |
| 5 | **No-surprises billing** — real-time spend on dashboard, hard spending caps, pre-overage alerts | TIER_STRATEGY feature #9 (extends 2.7/4.3) | Trust feature that prevents the viral "surprise bill" complaint; protects revenue quality. Most pieces (WP-1 hard-quota enforcement, 4.3 alerts, the dormant 2.7 reporter) exist — this is assembly; at launch egress is throttle-first at the quota-keyed allowance, overage billing is post-launch. | S-M |
| 6 | **Backup verification reports** | 14.4 | Cheap (cron over existing checksums), unique at this price point, closes deals with the exact compliance/backup crowd the launch targets. | S |
| 6b | **Privacy quick wins** — "Built to Forget" log TTLs (purge raw s3/cdn access-log rows + IPs on a ~7d clock AFTER billing rollups + 5.14.9 customer log delivery consume them; disclose the schedule), public LE-request policy page + "what we can produce" doc (Signal-style: enumerate per tier exactly what a subpoena yields), **client-side tools guide** (2026-10-12: restic, `rclone crypt`, Kopia, SSE-C per bucket — encrypted names+content over stored.cloud today, zero code; the only "we cannot read it" offer until 10.0 and WP-SSEC-1 — doubles as LET marketing); the LE-request policy + "what we can produce" page live under 43.4 at trust.stored.cloud with the transparency log (F-16) | `.private/PRIVACY_NORTH_STAR.md` (2026-08-02) | Trust features for exactly the LET audience; each is writing or a small WP; moves protections from policy to architecture (Proton-2021 lesson: policy can be compelled, architecture can't). | S |
| 7 | **Phase 10 remainder** — key management UI, proof-of-ownership, security testing | 10.5-10.7 | Encryption is on the pricing page; 10.7's cross-tenant isolation audit + timing hardening should land before dedup+encryption carries real customer volume. | M |
| 8 | **Erasure coding + permafrost parity** | 11 + P2-P6 | The durability story ("survives 6 failures") and the margin story (free OneDrive parity) — but big engineering. Start ~day 45 once launch stabilizes; prerequisite for own-fleet migration. | L (multi-week) |
| 9 | **Own-fleet testbed (R130 + MinIO driver)** | 6.6 | LAUNCH_STRATEGY says buy the R130 early ($69/mo) and prove the ops model long before the ~M9 STOR-OGD purchase. Driver is mostly config on `s3compat`. | S code + ops |
| 10 | **stored-cache agent** | 16.5 | High LET delight ("your $3/yr VPS caches your data"), independent of everything else — good month-2/3 community moment. | M |
| 11 | **Smart egress routing** | R4 bullet | Build trigger-based: ship the *detection* (usage poller alert) now, the routing only when the first tenant actually approaches 3× egress. | S then M |
| 12 | **iDrive reseller R1-R3** | R1-R6 | Per-tenant sub-accounts matter at ~50+ customers (isolation, white-label, cost attribution). Not before. | L |
| 13 | ~~**Per-region iDrive key support**~~ ✅ DONE pre-launch — #460, then rebuilt in #502 (WP-R7-1) | 5.14.7 follow-up | Shipped differently from this row's sketch: a region driver is registered **only** with its own `IDRIVE_<REGION>_*` pair and there is **no** fallback to the primary pair (it 403s elsewhere). Remaining = [YOU] put the regional pairs into prod `.env` (SYNTHESIS checklist row 11). | done |

**Re-rank 2026-09-30 (plan driver; supersedes the row order above where they differ).** *Since 2026-10-01 the order of record is the FULL-PLAN TRACK section below: A lands in its Stage 2, B in Stage 3, C in Stage 4, D is its rule 3, E is in Stage 1; the monthly re-rank is against what tenant zero's use turns up as well as any customer signal.* Start the week after launch:
- **A — small, independent, each a LET post:** 12.1 lifecycle XML (expiration + abort-MPU first; transitions map to the smart floors), 26.4 `stored` CLI + `stored rclone-config` (spec `docs/PRODUCT_FEATURES.md`), 14.4 backup verification reports, 15.1 Docker image + compose, V18.2 full restore orchestration (rehydration worker, bulk thaw, completion webhooks; WP-VG3 folds in), WP-VG2 streaming-signature + checksum sweep.
- **B — Phase 11 erasure coding, from ~day 45**, split 11.1+11.7 → 11.2 (= P3) → 11.3 behind a flag → 11.6 → 11.8; P2/P4/P5/P6 fold in; 6.6 only when the R130 is bought. The bench findings under Phase 11 are binding.
- **C — Phase 10 remainder** (10.5 on top of WP-R8-1, 10.6, 10.7) before dedup + encryption carries real volume; 10.0 client E2EE SDK — **position decided per F-8 (2026-10-12):** right after Stage 1a if the consumer crowd is the next year's choice, else after the Stripe layer; the SDK key core is built once in the Engine lane as a Go module + WASM (keystone) whichever way F-8 falls; per `.private/PRIVACY_NORTH_STAR.md`.
- **D — re-scope before prompting:** Phases 22–30 are reconstructed text; rebuild each from `.private/VAULTAIRE_MASTER_PLAN.md` (steps 511+) and `.private/EDGE_NODE_STRATEGY.md` as a docs PR first. Edge Personal (16.5 cache agent) is the first federation step; 20/21 need 11; 19 needs a scope decision (bucket policies only vs a full evaluator); 13 needs location-authoritative reads (WP-R6-1).
- **E — hygiene any time:** D-1/D-2/D-9 class-B deletions as one PR (lets `go mod tidy` drop circl and yaml.v3; **wazero stays** since 2026-10-01 — Phase 31.6 is built on it).

**What the first 50 customers will hit that isn't feature work**: slow first-byte on tape-tier reads (mitigated by NVMe cache #312 — watch hit rates), multipart edge cases from exotic clients (keep `cmd/validate` growing), quota/suspension edge cases, and support load (3.7 support view + 3.11 abuse queue are already live — use them).

## Versity takes (evaluation 2026-09-18)

*Source: versitygw @ 1246a08 (Apache-2.0, Go) studied as a **parallel-implementation reference**, not a dependency — Geyser's S3 layer is Spectra Logic Vail, not Versity. Full evaluation (what to take/skip and why, file-level pointers, license discipline): `.private/VERSITY_EVALUATION.md`. Apache-2.0 code may be adapted with attribution; scoutfs is GPL-2 — concepts only, never code.*

### WP-VG1: versitygw BATS conformance run → triaged gap report — **✅ DONE (#442)** — was the only pre-launch item here

> **Shipped 2026-09-18 (#442)**: gap sweep RUN (841 tests), report triaged; the (A) real-bug class — A1 conformance fixes (`?acl` routing incl. the PUT-acl-overwrote-object bug, header round-trip via migration 061, `Location`, listings=HEAD class mapping, ObjectLocked→AccessDenied) — SHIPPED in the same PR. A2-A4 fold into WP-VG2 (post-launch).

Run versitygw's BATS S3 compatibility suite (523 tests, 54 suites, wire-level) against a local vaultaire via the `/verify` harness. Third-party-endpoint mode is documented: `RUN_VERSITYGW=false` + `AWS_ENDPOINT_URL` + `SKIP_USERS_TESTS/SKIP_ACL_TESTING/SKIP_POLICY/SKIP_BUCKET_OWNERSHIP_CONTROLS=true` (NOT `DIRECT=true` — that mode hits real AWS IAM). Cheapest first probe before any bats setup: `./versitygw test full-flow -a KEY -s SECRET -e http://127.0.0.1:PORT` (their Go integration suite; versitygw-specific tail failures expected). Attack order, skip list, and harness landmines (TLS needed for the openssl-driven `rest-chunked` suite, unguarded user-helper tests that fail-not-skip, ListBuckets-with-prefix teardown dependency) are in the eval doc. **Deliverable = `.private/VERSITY_GAP_REPORT.md` with every failure triaged into (A) real bug → fix WP, (B) deliberate non-feature → documented won't-fix, (C) harness artifact. Triage, don't chase a pass rate — failures may be deliberate non-features.** Expect `rest-checksum` (20 tests) to fail wholesale — that's WP-VG2's baseline measurement.

### WP-VG2: SigV4-streaming + checksum hardening sweep — POST-LAUNCH (M, 2-3 sessions; rank alongside stack #4-6 — it's the "exotic multipart clients" risk made systematic)

Today: `awsChunkedReader` discards chunk signatures AND checksum trailers, `STREAMING-*` passes auth unverified (WP-4 deferral), and there is zero `x-amz-checksum-*` support — so aws-cli v2's DEFAULT upload path (CRC32 + STREAMING-UNSIGNED-PAYLOAD-TRAILER) gets no server-side integrity verification beyond decoded-length. This corner already produced prod bugs 1.11 (multipart framing corruption) and the Lyve CopyObject-CRC32/parallel-download abort. Scope (adapt from versitygw, Apache-2.0 w/ attribution; file pointers in eval doc): (a) robust unsigned-trailer chunk reader (excess-data disambiguation, bounded header parse, min-chunk-size 8192) + trailer checksum validation CRC32/CRC32C/SHA1/SHA256/CRC64NVME; (b) per-chunk signature verification for STREAMING-AWS4-HMAC-SHA256-PAYLOAD(-TRAILER), closing the WP-4 deferral, then remove the `SIGV4_ENFORCE` kill-switch; (c) `x-amz-checksum-*` surface: validate+store on PUT/UploadPart, COMPOSITE vs FULL_OBJECT on multipart complete (lift their 181-line zlib crc-combine port), return on GET/HEAD **full-object requests only — NEVER on ranged GETs** (the Lyve CopyObject lesson: a whole-object CRC on a 206 makes SDK parallel downloads abort; also audit that range passthrough doesn't forward backend checksum headers today); (d) drain-request-body on early chunked rejection so clients see our error XML, not connection-reset; (e) adapt their chunk-reader/CRC unit tests as red-first vectors. Re-run WP-VG1's failing suites as the acceptance gate.

### WP-VG3: V18.2 restore wire-fidelity checklist — POST-LAUNCH, folds into the full V18.2 orchestration WP (S)

Cross-check found we're already MORE AWS-faithful than versitygw's scoutfs glacier mode (they ignore restore Days, never 409, emit fake `x-amz-restore` on online objects, fire restore-completed events at request time). Keep our semantics; take four small items into full-V18.2: (1) surface `StorageClass: GLACIER` in ListObjects/V2 entries for archive-tier objects (head-cache column — cheap; backup tools plan restores off it); (2) verify HEAD omits (never emits empty) `x-amz-restore` when restore state is absent/unavailable; (3) verify the ErrArchived 403 branch precedes range handling on Geyser GET/GetRange (a ranged GET of an evicted object must 403, never trigger a blocking recall); (4) rehydration-worker webhooks fire on restore COMPLETION, not request acceptance.

### WP-VG4: own-fleet S3-on-POSIX eval — M12+ (with 6.6/STOR-OGD decision)

Add to the Garage/SeaweedFS/Ceph/MinIO candidate set: (a) vaultaire-over-versitygw-posix (stateless gateway, proven O_TMPFILE+linkat atomic publish, copy_file_range multipart assembly, pluggable xattr/sidecar metadata, multi-gateway publish locks); (b) adapting those posix techniques into a native driver. Also steal-as-design (GPL, no code): scoutfs's sequence-number change indexes → make the smart-tiering mover (7.6/8.1) incremental from day one (indexed change-seq on `object_head_cache` instead of full scans), and its explicit offline/staging/stage-failed state machine with version-checked recall.

---

# FULL-PLAN TRACK (added 2026-10-01 — owner decision: build the whole plan)

**Operating mode.** Isaac is customer zero: Stored (a product of FairForge; canonical domain stored.cloud since 2026-10-12, stored.ge and stored.pics permanent aliases — F-17) is in use by its owner, so the plan is worked through to the end **with or without outside customers**. The tier headings below ("after first customers", "once you have product-market fit", "when you're a real business") and the words "demand-gated" describe the order things were once expected in; they are no longer start conditions. What gates a phase now is listed here. The **public** launch (2026-10-31: signups, Stripe, CSAM/DMCA, alerting) keeps its own gates — Stage 0 — because those protect strangers' data and money, not ours.

This section is the order of record for everything after the pre-launch queue. It supersedes the ordering (not the content) of the Post-Launch 90-Day Priority Stack and its 2026-09-30 re-rank.

## The five rules

1. **Dependency order, not customer count.** A phase starts when the phases it stands on are done. Nothing waits for "N customers" or "someone asks".
2. **Linked, flagged, used.** Every phase ships linked into the product binary (or as its own shipped binary), behind a feature flag where it changes behaviour, with tests — and is switched on for **tenant zero** (Isaac's own tenant, real data, on prod) before the next stage starts. That use is the acceptance gate that customer signal used to be. The review deleted about 93k lines that were written ahead of need and met none of the three: never linked, never flagged, never used.
3. **Re-scope before prompting.** Phases 22–39 and every "reconstructed" section are scoping text, not specifications. Each gets a docs PR that checks it against the code as it then stands before its first worker prompt.
4. **Real-world prerequisites stay gates.** Hardware (R130 / own fleet, CORVAULT, RDMA, GPU, a second site), money (SOC 2 auditor, ASN + IP space, Stripe Connect platform review), law (the review that master-plan step 1010 requires before tokens, a storage market or a DAO; the RaptorQ licence check), and third parties (Geyser object-lock buckets, a second Geyser copy). A worker session cannot close these; the stage table names them.
5. **One loop per code lane.** One worker prompt per turn in a code lane; done means merged, deployed, verified, plan line flipped, Status date bumped. The plan driver reviews every PR before the next prompt.

## Lanes (2026-10-12)

Four lanes — **Engine** (write path, keys, drivers, jobs; `docs/surfaces/ENGINE.md`), **Developer surface** (S3 + management API, SDKs, CLI, docs — Standard default, Private via SDK/SSE-C; `docs/surfaces/DEVELOPER.md`), **Consumer surface** (stored.cloud/photos, the apps — recoverable default + "Advanced protection" until F-19, then Private default; `docs/surfaces/CONSUMER.md`), **Trust** (Phase 43, the transparency log F-16, the privacy page, the LE policy, the master-key install, the attested box; `docs/surfaces/TRUST.md`). `docs/ARCHITECTURE_CONTRACT.md` is the one page every worker prompt cites; `modes.json` is the single source for the modes → privacy page, OpenAPI, dashboard and one test. **Keystones are built once, in the Engine lane:** WP-R8-5 streaming SSE, the SDK key core (Go module + WASM), the bucket mode column, the transparency-log library, the recovery-vault interface — surfaces consume, never fork. **WIP limit:** one deep code lane + one surface lane at a time; docs lanes run in parallel; one owner decision batch a week; a convergence checklist in `docs/STATUS.md`. **All four lanes build now, dark until proven (owner decision 2026-10-12, evening: "I don't want to defer anything — I am already using it"; the 2026-10-01 full-plan decision applied to the lanes).** Every lane codes from today; new behaviour ships behind a feature flag OFF, on a new host or route, or as an additive migration whose number is assigned in `docs/STATUS.md` at prompt time; the launch path (the S3 API in Standard mode, billing, the deploy) is protected by CI, the conformance suite, the synthetic check and the Stage-6 freeze on launch-path changes only — never by a lane waiting; a flag flips after a proof on tenant zero; merges go through the one queue, and review is the pacing bottleneck, so PRs stay small and the keystones go first. After launch, F-8 decides whether 10.0 moves up to right after Stage 1a (consumer crowd) or the Stripe layer goes first.

## The order

| Stage | Stands on | What (plan sections) | Outside prerequisite |
|---|---|---|---|
| **0. Public-launch safety** | — | The pre-launch queue in the Status line; the [YOU] rows of SYNTHESIS; launch stages 2–6; **WP-DOMAIN-1 stored.cloud canonical (F-17, 2026-10-12 — before 2026-10-31; built in the Engine lane; the other lanes build in parallel, dark)** | Stripe, CSAM/DMCA, e-mail provider, the standby (bought as EPYC SEV-SNP — F-18), DNSSEC DS at Porkbun + the stored.cloud zone rules [YOU] |
| **1. Foundations** — the write path, the engine and the one-way doors, before real data accumulates | 0 | **One-way doors first:** ~~WP-R8-4 chunker identity~~ (DONE #557 — 2 MiB kept and on record; changing it later is a knowing dedup reset), ~~WP-R8-1 key versioning before the master key carries data~~ (DONE #682, 2026-10-12) → **the F-14 ceremony** (2026-10-12): the ~~`generatedObjectWriter` plaintext-into-SSE-buckets fix~~ (done #685), WP-R3-1 **pulled ahead of WP-R6-1**, `server.go` reads `$CREDENTIALS_DIRECTORY` not only the env var, the dedicated service user, then the on-box key install + 2-of-3 escrow + the first logged ceremony; follow-up WP: TPM-resident KEK. **Engine-lane keystones (built once):** WP-R8-5 streaming SSE → WP-R3-3 SSE multipart → **WP-SSEC-1** (5.14.8a: streaming SSE-C, multipart, CopyObject, an "SSE-C required" bucket flag); the SDK key core (Go module + WASM); the bucket **mode column** (Private / Standard / Public, F-15) + `modes.json` + its test; the transparency-log library (F-16); the recovery-vault interface (F-19). **WP-DOMAIN-1** (5.14.13, F-17). **Write path:** WP-R2-1 write-new-key-then-swap, WP-R3-2 UploadPartCopy, WP-R3-4 batch-delete parity, WP-R2-4/6/7. **Engine:** WP-R6-1 location-authoritative reads, WP-R6-2 concurrency, WP-R6-3 per-operation deadlines, WP-R2-3 client-body errors, WP-R7-2/4/6/7 drivers. **Integrity:** WP-R8-2 orphan sweep, WP-R8-3 body integrity, WP-VG2 + WP-R15-1 streaming signatures and checksums. **The rest of SYNTHESIS table B** by size. **Hygiene:** D-1/D-2/D-9 deletions, D-12 table drop, D-24 Go 1.26 **Trust lane (was "Privacy lane" 2026-10-11; builds now, dark until proven), alongside Stage 1a, S each: Phase 43.1 inventory, 43.2 disassociation + Ente's e-mail-at-rest pattern (server-key-encrypted e-mail + keyed BLAKE2b lookup hash), 43.3 built-to-forget retention, 43.4 the LE policy + what-we-can-produce page + transparency report at trust.stored.cloud, 43.6 the transparency log start (signed statements + OpenTimestamps + a Rekor mirror, F-16), `docs/surfaces/*.md`, `docs/ARCHITECTURE_CONTRACT.md`, `modes.json` + its test** · **Payments (2026-10-18): WP-PAY-0 … WP-PAY-7 — the entitlement record in our DB, Stripe as one provider, BTCPay as the second (`docs/PAYMENTS.md`; the Developer-surface lane, beside WP-R8-2 / WP-R3-2; WP-PAY-0/1 first, no customer-visible change)** | The owner's answers to D-1..D-24; F-22 … F-30 (payments); F-14 decided — the ceremony is [OPS]; F-17's two sub-decisions (wildcard vs path-style; s3 host proxied vs direct); F-18 the standby box |
| **2. Product surface on the existing engine** | 1 | Vault: V18.2 full restore, V18.8, V18.1 ingest buffer, V18.4 Glacier migration, V18.5 retention templates + certificates, V18.6 calculator, V18.9 backup-tool-aware routing. **Phase 12 lifecycle and all of Phase 31 smart programmable buckets — profile, rules and WASM functions together (F-1), with the Phase 22.1 runtime pulled forward for it; one rule engine, see Phase 31.** 26.4 CLI, 14.4 verification reports, no-surprises billing, privacy quick wins, 15.1–15.4 Docker / ARM / Helm / docs, 26.1–26.3 and 26.5–26.8 developer experience — **with 31, 42.1, the docs site (docs.stored.cloud) and WP-SSEC-1 this is "the Stripe layer" (2026-10-12), first after Stage 1a unless F-8 picks the consumer crowd** — 5.12.9 capacity planning. **Phase 40 Cloudflare edge plane:** 40.1 the serving plane first (40.1(b) needs WP-R2-1's immutable keys or 34.3's purge-on-delete), then 40.2–40.4, 40.6–40.9; 40.5's edge runtime once 40.1 carries traffic | V18.3 second tape copy and V18.5 lock-enabled bucket are Geyser provisioning |
| **3. Durability core** | 1 | Phase 11: 11.1 + 11.7 → 11.2 (= P3) → 11.3 behind a flag → 11.6 repair → 11.8; P2, P4, P5, P6; 11.5 LRC; 11.4 RaptorQ; 6.6 own-fleet driver; Phase 38 audit jobs | 6.6 needs the box — 2026-10-12: the next box is the Stage-0 standby, bought as EPYC SEV-SNP (F-18) and used as standby + PG replica + backup target + TPM escrow share; an own-fleet leg on it is F-5; 11.4 needs the licence check |
| **4. Privacy and keys** | 1 | 10.5 key UI and rotation, 10.6 proof-of-ownership, 10.7 isolation audit, 10.0.1–10.0.4 client E2EE SDK, Phase 32.6 split keys, Phase 38 write-avoidance (needs 10.6 and the SDK). 10.8 blind dedup keeps its **measurement** trigger (F-3). **Phase 41 private compute on encrypted data** after the SDK: 41.0 the modes table **per surface, from `modes.json` (F-15, F-8 — Private / Standard / Public)** → 41.1 queryable index → 41.2 on-device ML (compute on device) → 41.3 PIR lookups (F-7: the mechanism that replaces 10.8's OPRF; **the only homomorphic use**) → **41.4 the attested data plane (F-9): the ONE shared foundation** — self-hosted SEV-SNP CVM, Trustee KBS, published measurements, attestation endpoint; serves compliance processing, the consumer recovery realm and cloud AI — built on the F-18 box, prod never moves into it before launch → 41.5 encrypted CRDT relay. **10.0 (2026-10-12):** position per F-8 — right after Stage 1a if the consumer crowd is chosen, else after the Stripe layer; **10.0.3 is no longer "later, optional": a passcode recovery vault with hardware attempt limits + trusted contacts + passkeys + recovery nudges is REQUIRED before Private becomes the consumer default (F-19); its first realm is the 41.4 box** **10.0 re-scoped 2026-10-11 to the Ente model (F-12, F-13: hierarchy, SRP, hybrid PQ sharing, public links, convergent chunks under the space key); 43.5 numbered accounts after 10.0** | An external audit before the words "zero-knowledge"; the F-18 box for 41.4 and F-19; the transparency log (F-16) carrying client build hashes before clients refuse unlisted builds |
| **5. Protocols and data intelligence** | 1 | Phase 13 WebDAV / SFTP / FUSE; Phase 14.1–14.3; Phase 23 (with 23.0 and 23.5–23.9); Phase 24 (with 24.4–24.9) | — |
| **6. Identity and the multi-tenant platform** | 1 | Phase 19 IAM and bucket policies (F-2), Phase 32 enterprise identity, Phase 18 reseller platform (with 18.7–18.9), R1–R6 iDrive reseller, Phase 28 enterprise operations, Phase 33 operations platform, (42.1 may run earlier as part of the Stripe layer, F-8 2026-10-12) **Phase 42.1–42.2** (the plan ladder with pooled organisation quotas and presign-time quota checks; the platform → reseller → organisation → space → user tree with residency as an organisation attribute) | Stripe Connect review, SOC 2 auditor, a second site |
| **7. Edge, federation, network** | 3, 6 | 16.5 cache agent → Phase 20 hub–spoke → 16.1–16.4 NAS agent → Phase 21 cooperative / BYOS / credits (with 21.7) → the rest of Phase 22: edge placement and 22.4–22.9 (the 22.1 runtime and the built-in / customer functions already exist from Stage 2) → Phase 34 networking → Phase 36 hub-to-hub federation. **F-10:** the read plane is Cloudflare (40.1); spokes keep the ingest-accelerator, storage-shard and metadata roles — 16.5 and the CDN-node role of `.private/EDGE_NODE_STRATEGY.md` are re-scoped to that | Spoke hardware; ASN + IP space for 34.8; GPU / Pi hardware |
| **8. Proofs, web3, markets** | 4, 7 | 6.5 Filecoin, Phase 25, Phase 35, Phase 17 data marketplace | **Legal review first** for 35.3–35.5 and 17.2 |
| **9. Packaging** | all | Phase 29 integrations, Phase 37 more backends + orchestration, Phase 30 platform licensing (renamed 2026-10-12 — "Platform & stored.cloud" → the platform lives at stored.cloud/platform; the domain itself is WP-DOMAIN-1 in Stage 0/1, F-17), Phase 39 business operations (owner track, runs from Stage 0), **Phase 42.3–42.5** the app plane on Cloudflare (Stored, Hearth-CF, the Forge apps, the mirrors catalogue — other repositories that stand on this engine); **42.6** the LowEndTalk lineup runs from Stage 0 with Phase 27 | CORVAULT / RDMA hardware for 37.2–37.3 |

Phase 27 (go-to-market) runs alongside from Stage 0 and is the owner's. Stages 2–5 do not depend on each other and may interleave **within the lane WIP limit (one deep code lane + one surface lane; docs lanes free — see Lanes)**; inside a stage the order is the one written.

**Size, so the track is read honestly:** roughly 250–300 worker sessions — about 8 in Stage 0, 55 in Stage 1 (table B is most of it), 30 in Stage 2, 15 in Stage 3, 12 in Stage 4, 25 in Stage 5, 35 in Stage 6, 60 in Stage 7, 25 in Stage 8, 15 in Stage 9. These are estimates from the section counts, not commitments. Phases 40–42 (added 2026-10-03) add roughly 25 sessions on the Vaultaire side — 10 for Phase 40, 12 for Phase 41, 3 for 42.1–42.2; 42.3–42.6 are other repositories and the owner track. The 2026-10-12 verdicts add roughly 20 more: WP-DOMAIN-1 ≈ 3, WP-SSEC-1 ≈ 4, the F-14 ceremony + runbook ≈ 2, the transparency log ≈ 4, the recovery vault ≈ 5, the attested box ≈ 3 (hardware first).

## Open decisions for the full track (F-1 … F-30)

| F | What | Recommendation |
|---|------|----------------|
| F-1 | Smart buckets: declarative rules only, or rules plus customer-supplied WASM functions (Phase 31.6) | **Decided 2026-10-01 (owner): both together.** Rules and functions are built as one phase; the Phase 22.1 WASM runtime moves into Stage 2 with Phase 31 |
| F-2 | Phase 19: bucket policies only, or a full IAM evaluator | Bucket policies + cross-tenant grants first; the evaluator when Phase 18 hierarchies exist |
| F-3 | 10.8 blind dedup was decided "do not build" on 2026-08-02 on **security** grounds (a server-held OPRF secret is weaker than per-tenant client keys), not for lack of customers | Keep the measurement trigger; evaluate it on tenant zero's data. **2026-10-12 (owner): cross-tenant dedup of encrypted data is the opposite of least access — only within-space convergent (F-12), the opt-in public-artefact namespace (10.3/10.6), and PAKE/PIR research behind this measurement; never a global convergent key** |
| F-4 | Phase 35.3–35.5 (token, storage market, DAO) and 17.2 (data monetisation) need a legal review before code | Book the review when Stage 7 starts; build 35.1, 35.2, 35.6–35.9 meanwhile |
| F-5 | Which hardware, and when: R130 (6.6), a second site (28.3), spoke boxes (20), Pi / GPU (22.6, 22.7), CORVAULT / RDMA (37.2, 37.3) | R130 during Stage 2 so Stage 3 has an own-fleet leg; the rest when their stage starts |
| F-6 | Tenant zero: which tenant on prod is Isaac's own, and does real data go on it before Stage 1's one-way doors (WP-R8-4, WP-R8-1) and the off-box backups (WP-R9-7) are closed | **Decided 2026-10-01 (owner): tenant zero is the owner's admin account on prod** (the account is named in the driver's private notes, not in this public file). Still open: keep irreplaceable data elsewhere too until those three are done |
| F-7 | Cross-tenant blind dedup: 10.8's server-held OPRF secret (DupLESS) or private information retrieval (Apple's `swift-homomorphic-encryption`, BFV) for the "do you already hold this chunk" lookup and shared-library lookups | **Recommend PIR (41.3)**: no server secret to steal or compel, no confirmation attack, no rate-limit boundary to sybil, one lite container; F-3's measurement trigger stays the go/no-go for building cross-tenant dedup at all. **Confirmed 2026-10-12 (owner): PIR is the only homomorphic use (41.3); compute is on device (41.2) or attested (41.4); never a global convergent key** |
| F-8 | End-to-end encryption as the default or as a mode | ~~Recommend three modes per space (41.0) — Normal, Vault, Shared vault — with Normal the default for Standard and Vault the default for the privacy tier~~ **Decided 2026-10-12 (owner): a mode is a property of the SURFACE, never a user choice (names in F-15).** Developer surface (S3 + management API): Standard default, first-class; Private via the SDK or SSE-C per bucket. Consumer surface (stored.cloud/photos, apps): Private default **only once the passcode recovery vault exists (F-19)**; until then recoverable default + "Advanced protection" opt-in (Apple's shape). Public = plaintext. E2EE as the default everywhere kills server-side search, previews and collaboration (Drime and Proton chose the same) — hence per surface. **Order:** after launch 10.0 moves from Stage 4 to right after Stage 1a if the consumer crowd is the next year's choice; otherwise the Stripe layer (31, 42.1, 26, docs/SDKs, WP-SSEC-1) first — owner picks the crowd in the first post-launch decision batch |
| F-9 | The attested data plane for 41.4: which hardware, and whether the measurement is published | ~~AWS Nitro / GCP Confidential Space / Azure Confidential Containers; decide when 41.4 starts~~ **Decided 2026-10-12 (owner): self-hosted first, and 41.4 is the ONE shared foundation.** App + Postgres + keys inside a measured confidential VM on SEV-SNP EPYC (Milan or later — the prod Ryzen 9 9950X has no SEV-SNP), no ssh, a Trustee KBS key broker on another box, published measurements, an attestation endpoint. It serves developer compliance processing, the consumer recovery realm (F-19) + cloud AI (the Apple Private Cloud Compute pattern) and "can't quietly" (F-16). First box = the standby bought as EPYC with SEV-SNP exposed (F-18). Prod does not move into a CVM before launch. The enclave code is open-sourced and its measurement is in the transparency log, or the attestation means nothing |
| F-10 | The edge CDN: community spokes (`.private/EDGE_NODE_STRATEGY.md`, Phase 20/21) or Cloudflare | **By the 2026-10-03 numbers, Cloudflare is the read plane** (an R2 working set at $15/TB with free egress and 105 ms TTFB beats volunteer NVMe at $4/TB in credits plus the trust problem); spokes keep ingest, storage shards and metadata — owner to confirm |
| F-11 | The erasure codec: Reed-Solomon GF(2^8) for every shape, RaptorQ only where ratelessness matters, Clay/MSR over LRC for repair | **Measured 2026-10-03** (`bench-results/ERASURE-2026-10-03-codecs.md`): RS is 10–20× faster to encode and 2.5–3× to decode than the best RaptorQ at every size to 1 GiB; RaptorQ's two remaining Qualcomm patents run to 2028 and 2030 under an IETF non-assert; LRC is Microsoft-patented into the 2030s, Clay is open. Recommend as stated; the interim-primary question is B2 not Wasabi (90-day minimum) — and iDrive is a key fix, not a re-platform |
| F-12 (2026-10-11) | E2EE scheme: Ente's hierarchy as published (random master key, Argon2id KEK, recovery key, X25519 pair, a key per space, a key per object), or ours | **Ente's hierarchy with two changes:** (1) the object key encrypts metadata + manifest and the chunks are convergent under the **space** key, wrapped in the manifest — a space deduplicates its own data, two spaces never converge; the `paranoid` flag per bucket = Ente's random keys, no dedup; (2) key sharing is a hybrid X25519 + ML-KEM-768 sealed box (archives sit for decades). Leakage stated on the privacy page: sizes, counts, timestamps, access, within-space chunk equality |
| F-13 (2026-10-11) | One password for login and the key-encryption key (Ente, needs SRP so the server never sees it) or a separate vault passphrase | **One password + SRP-6a, Ente's flow** (`loginKey = kdf(KEK)`, 4096-bit group, verifier never returned, session token sealed to the public key); accounts without a vault keep bcrypt until their first vault space; passkeys + TOTP as second factors |
| F-14 (2026-10-11) | Set `ENCRYPTION_MASTER_KEY` on prod now, or wait for client-side E2EE | **Set it — after WP-R8-1 (DONE #682, 2026-10-12). Install (owner, 2026-10-12, supersedes the 2026-10-11 step):** an on-box ceremony — the key is generated on the box and the plaintext never leaves it (the current `.private/STAGE2_RUNBOOK.md` generates it on the Mac and is rewritten first); sealed as a systemd credential `host+tpm2` (`systemd-creds encrypt`, `LoadCredentialEncrypted=` on both slot units; the code reads `$CREDENTIALS_DIRECTORY/encryption_master_key` since #685, migration 085), **no PCR binding**, a dedicated service user (not the deploy user), mount namespacing, `apt install libtss2-rc0` [OPS] (the SLC box has `/dev/tpmrm0`; `systemd-analyze has-tpm2` reports `partial` only for that library). **Escrow:** age-plugin-sss threshold 2-of-3 — YubiKey, paper identity, a future trustee share (the F-18 box's TPM later). Every ceremony is logged and appears in the transparency report (F-16). **Follow-up WP:** a TPM-resident KEK so no exportable master exists. **Before the flip:** the ~~`generatedObjectWriter` plaintext-into-SSE-buckets fix~~ (done #685) and WP-R3-1 pulled ahead of WP-R6-1. **The key never changes the sentence "we can read Standard-mode data"** (the service decrypts to serve; the S3 secret is stored for SigV4). It is a different layer: SSE-S3 + convergent chunks protect Standard-mode data from the storage vendors (today every byte on iDrive/Lyve/Wasabi/Sync is plaintext, tenant zero's too); Ente-style E2EE protects a vault from US and cannot be offered to a raw S3 client at all. The two stack (a Private space sets `sse_enabled = false` — its blobs are ciphertext already). WP-R8-1 first because the key is the one-way door: once it carries data, rotation and shredding need stored-random versioned tenant keys. Copy stays "encrypted at rest with keys we hold" — never "zero-knowledge" for Standard mode (nor anywhere before the audit) — ~~and names the multipart caveat until WP-R3-1 (chunk pipeline) / WP-R3-3 (streaming SSE) land~~ (both landed 2026-10-13: #703, then WP-R8-5 = WP-R3-3 #708/#709 — no upload path and no size is refused or stored plaintext for encryption, so there is no caveat to name); the privacy page's per-mode table (41.0) says what the key lets us read |
| F-15 (2026-10-12) | Mode names and who chooses them: "Normal / Vault / Shared vault" per space (41.0 as written), or per surface | **Decided (owner): Private / Standard / Public — a property of the surface, never a user choice (F-8).** "Normal mode" and "Vault mode" are retired as mode names; **Vault stays the tape/attic TIER name** (Vault Flex, V18, the `vault` floor) and is never a mode; a shared Private space is "Shared", a flag, not a fourth mode. Copy: Standard = "encrypted at rest with keys we hold"; Private = outcome sentences; Public = plaintext; "zero-knowledge" forbidden until the external audit. `modes.json` is the single source → privacy page, OpenAPI, dashboard, one test; 41.0, 43.4 and the per-mode table are renamed from it; the bucket mode column is an Engine-lane keystone |
| F-16 (2026-10-12) | A transparency log: whether to run one, what it records, who witnesses it | **Decided (owner): yes.** Append-only, signed checkpoints, at least one external witness. Records: client build hashes (clients refuse unlisted builds), server image measurements + key-release policy (41.4), escrow/recovery ceremonies (F-14, F-19), law-enforcement productions, admin reads of customer metadata. Start = signed statements + OpenTimestamps + a Rekor mirror; later a tile log. Lives at trust.stored.cloud with the 43.4 artefacts (43.6); audience = press and auditors; customer copy = outcomes. Library built once in the Engine lane; the Trust lane publishes. "Zero-knowledge" stays forbidden before the external audit whatever the log shows |
| F-17 (2026-10-12) | Domain consolidation: stored.cloud canonical, with two sub-decisions — (a) virtual-hosted wildcard (ACM ≈ $10/mo) vs path-style only; (b) the s3 host proxied by Cloudflare vs direct | **Decided (owner): one brand, Stored (a FairForge product), one domain — WP-DOMAIN-1 (5.14.13) before 2026-10-31.** Map: stored.cloud home; stored.cloud/developers + docs.stored.cloud; app.stored.cloud; s3.stored.cloud; trust.stored.cloud; status.stored.cloud; stored.cloud/photos later (stored.pics 301); stored.cloud/platform (Phase 30); public customer content on a separate non-brand content domain. stored.ge served forever: web 301, the S3 API answers on both names with no redirect. Touches env vars, the hard-coded security.txt contact, legal pages, OpenAPI, landing, OAuth callbacks, the Stripe webhook, SYNTHETIC_CHECK_URL, the e-mail domain, Cloudflare zone rules on the stored.cloud zone, DNSSEC DS at Porkbun [YOU]. **(a) and (b) still open — owner, first decision batch**; recommendation: path-style default + the wildcard when the first SDK needs virtual-hosted; s3 direct to the HAProxy origin with the web on the edge, since the 2026-10-04 bench put the edge in the upload path — **and (2026-10-12) because an SSE-C key is plaintext inside the edge provider for the length of every request when TLS terminates there: a direct `s3` host keeps customer keys inside our origin only, and inside the measured image once 41.4 exists**. **Content domain decided (2026-10-12): a purpose-made non-brand name (the githubusercontent pattern) at the most stable registrar, never stored.ge** — it must look like nobody's brand (a phishing page on the former brand domain borrows its credibility), its abuse reputation must be disposable (stored.ge serves the S3 alias and mail forwarding forever), it can be Public-Suffix-listed later for per-slug isolation, and it must not depend on the .ge registry |
| F-18 (2026-10-12) | The Stage-5 standby box: any second server, or hardware that can later carry 41.4 | **Decided (owner): buy EPYC with SEV-SNP exposed** (Milan or later; verify `/sys/module/kvm_amd/parameters/sev_snp` = Y on the box before paying — the prod Ryzen 9 9950X cannot). Used now as standby + PG replica + backup target (closes WP-R9-7's off-box gap) + a TPM escrow share of F-14; later the first attested realm (F-9, F-19). Supersedes the R130-first reading of F-5 for the next purchase; an own-fleet leg on it is F-5. **Purchase checklist (2026-10-12):** EPYC Milan / Genoa / Turin; the vendor must expose the BIOS (BMC/IPMI) and the box must allow **SMEE, SEV control, the RMP table, an SEV-ES ASID limit above 1 and SEV-SNP** to be enabled (all off by default; a provider that cannot expose those cannot sell this); on the live image `/sys/module/kvm_amd/parameters/sev_snp` = Y and `cpuid -1 -l 0x8000001f` shows SNP; stock Ubuntu ≥ 25.04 carries host support (QEMU + the AMD OVMF build), the box is on 26.04 — no custom kernel. OVH Scale 2026 advertises SEV; Hetzner EPYC lines unconfirmed — ask, then verify before paying. Plus one small rented confidential VM under another operator as the second recovery realm (F-19) |
| F-19 (2026-10-12) | When Private becomes the default on the consumer surface | **Decided (owner): only after a passcode recovery vault exists** — hardware attempt limits in an attested realm (the Apple iCloud Keychain escrow pattern; Juicebox OSS, WhatsApp Backup Key Vault, Signal SVR3 as references), plus trusted contacts (Ente "Legacy"), passkeys and recovery nudges. 10.0.3 is therefore REQUIRED, not "later, optional". First realm = the F-18 box under 41.4. Until then the consumer default is recoverable + "Advanced protection" opt-in. The developer surface is unaffected (Standard default, F-8). Recovery-vault interface = an Engine-lane keystone; every recovery ceremony is in the transparency log (F-16). **Realms (2026-10-12 research, Signal's lesson):** one realm under our control is NOT hardware-enforced — the operator can restore a VM snapshot and reset the attempt counter — so the vault needs **two realms under different control, threshold 2-of-2 (a third later)**: ours inside the measured VM of 41.4, plus a small rented confidential VM or Nitro enclave under another operator (a named monthly cost, F-18). Protocol = **Juicebox** (OPRF + secret sharing + self-destructing record; MIT SDK with Swift / Android / JS bindings that fit Ente's clients; AGPL Go software realm and Rust HSM realm — quiet since 2024, audited paper ePrint 2025/348); policy = Apple's iCloud Keychain escrow (SRP so the passcode never leaves the device, each realm counts attempts independently, release on majority, destroy after 10) |
| F-20 (2026-10-14) | The key broker: wait for the F-18 EPYC box (41.4) or start now on a rented SEV-SNP VM | **Recommend: start now, as the first step of 41.4, with the tenant as the unit of disclosure** (43.7). A small broker (Trustee KBS if it verifies a plain TPM quote, else ~500 lines of Go on `go-attestation`) in a rented SNP VM (Onidel EPYC Premium, free SNP toggle, UEFI + guest kernel ≥ 6.11, no ssh after provisioning; checked 2026-10-14) holds the master key; a prod slot sends a TPM quote from SLC's fTPM (identity + nonce; a PCR allow-list in phase 2, each change a signed ceremony) over pinned mTLS and gets ONE tenant's key unwrapped at that tenant's first use, cached for the slot's lifetime — the master key never leaves the broker, never rests on the data box, and a demand for tenant X is X's unwraps only, each a transparency-log entry with a case id. Migration from the F-14 credential = the `ENCRYPTION_MASTER_KEY_PREVIOUS` re-wrap (no data re-encrypted), then the SLC credential is deleted; recovery = the 2-of-3 escrow with one share held by a trustee in another jurisdiction. Fails safe: a deploy during a broker outage aborts, the running slot keeps its cache. **What it does NOT give, said publicly:** the VM's kernel is not in the launch measurement (no vTPM/IGVM on a rented host) so the host operator can substitute it silently; a DDR4 interposer (Battering RAM, 2025) is silent; root on SLC can still ask for keys while the box is authorised. It is "the key never rests with the data and every use is public", not "can't". The F-18 box, when bought, takes the broker over with a measured image (41.4 proper). Supersedes the "TPM-resident KEK" follow-up (the KEK leaves the box instead) |
| F-21 (2026-10-14) | Moderation scope: what is scanned, where, and what an action is | **Recommend (owner confirms with counsel): known-hash matching on public and shared surfaces only, never a classifier on private content in any mode.** 5.5.1's Cloudflare CSAM Scanning Tool (NCMEC hash lists) runs on the CDN / public-link surface; Private mode cannot be scanned by construction and Standard mode is not scanned by policy (stated in the AUP and 43.4); a match is a signal, never a verdict (perceptual hashes collide): a threshold and a human review before any action; the first action disables the share or link, never deletes the account or its data; termination is a separate human, appealable step with the data preserved while the appeal runs; reports to NCMEC when the law requires (18 U.S.C. § 2258A: on actual knowledge, no duty to search; the EU/UK positions are moving — counsel); counts (matches, reviews, links disabled, terminations, appeals, reversals) in the transparency report. Client-side scanning (Apple's 2021 NeuralHash, the EU detection-order proposals) is refused: it is the false-positive machine moved onto the customer's device and it breaks the Private promise. The reference failure is Google Photos 2022 (a classifier on a father's medical photo of his child, account and e-mail deleted, cleared by police, no appeal) |
| F-22 (2026-10-18) | Second payment provider: BTCPay Server self-hosted, or a hosted processor (NOWPayments, Coinbase Commerce), or Stripe's own stablecoin method | **Recommend: BTCPay Server, self-hosted** — 0 % fee, non-custodial (a watch-only xpub; no spending key on any server), no KYB, and the only option under which "switch off Stripe" is true: nobody can close our account. NOWPayments (1 % per its pricing page, custodial, KYB) is the hosted fallback if operations prove too heavy; Coinbase Commerce was wound down 2026-03-31 (secondary sources); Stripe stablecoins (USDC/USDP/USDG, settles to the Stripe balance in USD, no disputes, $10k/txn, US live) is a Stripe payment method, not independence — one click in the Stripe dashboard if USDC through Stripe is wanted. `docs/PAYMENTS.md` § 3.1 |
| F-23 (2026-10-18) | Where BTCPay runs: SLC beside prod, a shared cPanel box, a small VPS, or the F-18 EPYC standby | **Recommend: a dedicated small VPS (4 vCPU / 8 GB / 160 GB NVMe class, ≈ €8–15/mo), `opt-save-storage-s` pruning (≈ 50 GB), `pay.stored.cloud` DNS-only, nightly `btcpay-backup.sh` to the Sync leg.** Never SLC: no Docker there, UFW 22/80/443, and a third-party .NET app + bitcoind beside PostgreSQL and the master-key credential turns a BTCPay CVE into a foothold ("nothing non-Vaultaire goes on SLC", 2026-10-13). Hostbrr/Maxko cannot run Docker. The F-18 box may take it over later; payments do not wait for it. `docs/PAYMENTS.md` § 3.7 |
| F-24 (2026-10-18) | Launch coin set, and whether Lightning is in it | **Recommend: BTC on-chain only at launch; Lightning as WP-PAY-6 after a month of on-chain operation** (a hot wallet with keys on the server and channel liquidity to manage — kept small by a balance cap and a sweep); Monero through the community plugin only with the numbered accounts (43.5); no stablecoin plugins (Liquid needs an Elements node; Tron/ERC-20 are community plugins) — USDC is available through Stripe's method if wanted. Honest downside of on-chain only: a fee spike can exceed a $4.99 monthly payment, so the page steers crypto payers to annual. `docs/PAYMENTS.md` § 3.6 |
| F-25 (2026-10-18) | Which accounts may pay in crypto: any identified account, or only the 43.5 numbered accounts | **Recommend: every identified (e-mail-verified) account**, with one gate: a tenant whose only payments are crypto may create public buckets / CDN hostnames only after 30 days as a paying customer or any card payment; numbered accounts stay private-only always (the north star's rule). The conservative alternative — crypto only for 43.5 — keeps crypto dark until 43.5 ships. Signups stay closed by `signups`; no free crypto trial. `docs/PAYMENTS.md` § 5 |
| F-26 (2026-10-18) | Grace period after `paid_through` before the house is cleared (data never touched) | **Recommend: 14 days**, and set Stripe's dunning ("Manage failed payments") to cancel after retries within 14 days so Smart Retries always run inside our grace. Reminders at T−14, T−3, T+1, T+13 for prepaid rows. 7 days is too short for a Stripe retry ladder; 30 days gives a month of free storage to every lapsed house. Env `ENTITLEMENT_GRACE` (default `14d`). `docs/PAYMENTS.md` § 1.3–1.4 |
| F-27 (2026-10-18) | Stripe the default provider at launch, or crypto first | **Recommend: Stripe default at launch** (the R10 click-list stands — endpoint pinned 2023-08-16, six prices, portal quantity updates off, dunning cancel ≤ 14 d, **portal reactivation off**); `pay_btcpay` OFF until WP-PAY-4's lab proof and the F-23 box exist, then one canary tenant, then global. Neither provider's flag is "cancel everyone": the bulk stop-renewals action is a separate, audited, dry-run-first step (WP-PAY-5) |
| F-28 (2026-10-18) | Refund policy for prepaid (crypto and manual) payments | **Recommend, for counsel to check (§ 6 Q7):** prepaid periods are non-refundable except within 14 days of a *first* purchase; refunds are paid in the coin received through a BTCPay pull payment at the original payment's rate (`RateThen`), never in fiat; an overpayment is refunded on request at the current rate (`OverpaidAmount`) or kept as a credit; a partial payment is a credit or a refund on request; amounts under $1 are kept (network fees); every refund is a ledger row and an audit row, and may shorten `paid_through`. There are no chargebacks on this rail — say so in the terms |
| F-29 (2026-10-18) | The metered tier's future (the dormant Stripe Billing Meters reporter, the legacy pack prices) | **Recommend: delete now — WP-PAY-0 = WP-R10-4.** Quota-sold on every tier is the 2026-09-21 decision, no prod tenant is metered, and the reporter plus the pack path are the Stripe-shaped half of the surface the provider refactor would otherwise have to carry. `spending_cap_cents` and `metered_usage_reports` fall with D-12 in WP-PAY-7 |
| F-30 (2026-10-18) | Tax posture for sales outside Stripe Tax (crypto, manual): collect nothing until counsel answers, or register (US states with nexus, EU non-Union OSS) before the first crypto sale | **Not recommended either way — counsel first** (`docs/PAYMENTS.md` § 6, the eight questions). Facts to weigh: Stripe Tax covers Stripe payments only; the SLC server is physical presence in Utah; EU B2C digital services are taxed at the customer's country with no threshold for a non-EU seller (unverified); "prepaid" is a billing shape, not a tax treatment (the 43.5 line is corrected). Until answered, `pay_btcpay` stays OFF for everyone but the canary |
| F-31 (2026-10-20) | Vendor plan rows and the capacity rule: which backends get a `backend_plans` row at once (`idrive` from invoice 105170 — 1 TB yearly to 2027-10-20; `geyser` 100 TB minimum at $155/mo; `lyve` promo, no cap; `wasabi` dormant; `r2` free tier), and the buy trigger | **Recommend:** enter all five rows the day WP-CAP-1 A deploys (an unknown plan pages); buy the next iDrive block (10 TB, $247.50 year one) at 80 % of the block on a rising slope or on the first customer house above 0.5 TB, otherwise ride the $5.12/TB-month overage and right-size at the 2027-10-20 renewal; benchmarks against iDrive capped at a few hundred GB | owner |

## What the original master plan had that this plan did not — where each went

Checked 2026-10-01 against `.private/VAULTAIRE_MASTER_PLAN.md` (steps 1–1320).

| Master-plan steps | Was it carried? | Now |
|---|---|---|
| "Smart Bucket Architecture" (per-bucket pipeline config, algorithm registries); 5.10 prep "notifications = foundation for smart buckets" | No — lost in the 2026-07-07 truncation ("smart buckets" survives only in the reconstruction comment) | **Phase 31** |
| 305 SSO, 331–340 split keys (Shamir), 371–374 LDAP / AD / SAML | Scaffolding existed, unreachable, deleted in R0 (#473) | **Phase 32** |
| 361–400 HA, tracing/APM, SLO, on-call, blue-green/canary, IaC; 401–450 containers, Kubernetes, global distribution, scale testing | Partly: 15, 28; the `k8s` / `ha` / `slo` / `global` packages are unlinked scaffolding (D-1) | **Phase 33** |
| 811–910 P2P, content routing, mesh, bandwidth optimisation, BGP anycast, CDN layer, network ML, QUIC, IPv6, network security | Only 21.4 (DHT) | **Phase 34** |
| 911–1000 blockchain, storage proofs, token, market, DAO, ZK, homomorphic encryption, DID, Web3 | Only as the four-line Phase 25 | **Phase 35** |
| 1061–1100 hub-to-hub federation | No (Phase 20 is hub–spoke, a different thing) | **Phase 36** |
| 1101–1140 CORVAULT, SeaweedFS, 3FS backends; backend orchestration layer | No; the selector / cost optimiser / health scorer were inert and deleted (#524) | **Phase 37** |
| 1205 write-avoidance, 1207–1208 Redis + bloom lookups, 1214–1219 savings metrics, chunk verification, consistency audit, analytics API, attack prevention | Partly (8, 10.6, 10.7) | **Phase 38** |
| 1001–1060 legal, product management, support, billing operations, developer ecosystem | Partly (5.14.2, 27, 26.5, 26.6) | **Phase 39** |
| 741–810 function marketplace, Pi nodes, GPU, edge analytics, edge security | No | **22.5–22.9** |
| 201–230 memory / SSD cache; 611–710 ClickHouse pipeline, RL cache, predictive maintenance, intelligent routing, optimisation advisors | Cache shipped then deleted as unbounded (#524); the rest only as Phase 23's four lines | **23.0, 23.5–23.9** |
| 1222–1239 Arrow Flight, Delta-style log, statistics / zone maps, partition pruning, aggregate pushdown, natural-language queries, query builder, cross-container joins | Only as Phase 24's three lines | **24.4–24.9** |
| 1310–1315 family accounts, organisation members, department usage, chargeback, custom-domain SSL | No | **18.7–18.9** |
| 565–568 compute contribution, credit transfer, credit marketplace | No | **21.7** |
| 511–520 file-level dedup; 521–530 simple replication engine; 128 / 263–264 delta encoding for versions; 121–125 per-operation rate limits | Superseded: chunk dedup (8) replaces file dedup; 11.2 / P3 and V18.3 replace the replication engine; delta encoding is listed under Phase 38; per-operation limits stay a Deferred Decision | — |

---

# TIER 2: COST OPTIMIZATION (Build after first customers — since 2026-10-01 ordered by the FULL-PLAN TRACK, not by customer count)

> **Plan text is not code — audit of Tiers 2–4 against `main` @ #531 (2026-09-30).** Read this before treating any section below as shipped.
> - **SHIPPED:** 6.1–6.4 backends, 7.5/7.6 bucket tier + residency columns, 8 chunking + GCI dedup + GC, 9 zstd, 10.1–10.3 convergent chunk encryption (inert in prod: no master key), P1 permafrost driver, 5.15.8 smart demotion (flag OFF).
> - **SHIPPED THEN DELETED AS INERT:** 7.3/7.4 `TieringEngine` + cost optimizer, the engine read cache, access tracking (#524, WP-R6-4/5/6) — see the note under Phase 7.
> - **BENCHED, NOT BUILT:** Phase 11 erasure coding (and with it P2–P6).
> - **NOT STARTED, no package in the repo:** 10.0 client E2EE SDK (no `sdk/`), 10.4–10.8, 6.5 Filecoin, 6.6 own-fleet driver, R1–R6 reseller client, 12 lifecycle, 13 protocols, 14 (14.4 included), 15 (no `Dockerfile`, no `deploy/helm`), 16 agents (no `cmd/stored-*`), 17, 18, 19 IAM/bucket policy, 20 federation, 21 BYOS/credits, 22–30. `cmd/vaultaire` is the only product binary; everything else under `cmd/tools/` is a probe or bench.
> - **Present but unlinked (never built into the binary), pending D-1/D-2/D-9:** `internal/{k8s,container,global,ha,slo,integrations,loadtest}`, `engine/{sla,disaster_recovery,analytics,capacity,replicator,load_balancer}.go`, `drivers/wasm.go`, `crypto/postquantum.go` — their existence is not evidence that Phases 15, 22, 28 or P3 started.

---

## Phase 10.0: True End-to-End Encryption (Client-Side — Private mode)

*(2026-10-12: the title said "Zero-Knowledge" until today; the words are not used in copy or plan titles before an external audit of the SDK — F-15.)*
*Depends on: 5.14.4 (PQ encryption primitives). Separate from Phase 10 convergent encryption — this is client-side, that is server-side. Both coexist.*

**DECISION 2026-08-02 (privacy north star — full doc: `.private/PRIVACY_NORTH_STAR.md`):** the SDK's DEFAULT derivation is **per-tenant convergent chunk keys** — `K_chunk = HKDF(tenant_master_key, SHA-256(chunk))`, master key never leaves the client (10.2's sketch, made client-side) — so Private mode keeps within-space dedup (the ratios that drive the margin model are intra-tenant: own backup history, own VM images) with nothing server-side to steal or compel (the outcome sentence of Private mode; Tarsnap/restic/borg precedent). The random-per-object-DEK mode below survives as a per-bucket "paranoid" opt-in (no dedup). Binding implementation notes: **wrapped DEKs in the encrypted manifest** (GET cannot re-derive keys from plaintext it doesn't have — each chunk ref carries its DEK wrapped under the tenant master key), **pinned zstd version/params** (convergence needs byte-identical compression; divergence degrades dedup, never corrupts), **frozen FastCDC polynomial** (0x2ADD89E3B790BB, WP-7) as a published client contract. Marketing rule: the words "zero-knowledge" wait for an external audit of the SDK (10.7 extension). Interim for encrypted-names demand: document `rclone crypt` / restic over stored.cloud (2026-10-12: the client-side tools guide; stored.ge is a permanent alias). Cross-tenant blind dedup = 10.8, demand-gated.

**RE-SCOPE 2026-10-11 — the Ente model (owner ask; decisions F-12, F-13; sources at the end of this block).** Ente is the reference because it is the one consumer E2EE storage product whose scheme is published, open source (AGPL, `github.com/ente-io/ente`), audited (Cure53 with Symbolic Software, 2023-03: "architecture is sound … implementation across all clients is cryptographically accurate"; one high — weak passwords allowed — fixed) and running at scale on exactly our kind of backend (encrypted blobs on Backblaze, Wasabi, Scaleway). We adopt its hierarchy as written and change two things, both marked. **Where it applies:** the app plane (42.3 Stored, the managed photos app, the Forge apps), the SDK and CLI (10.0.1), and browser uploads from the dashboard (10.0.2) — **Private mode** (2026-10-12, F-15: the modes are Private / Standard / Public, a property of the surface, never a user choice; "Vault" is only the tape tier's name from here on; a shared Private space is "Shared", a flag). **Where it cannot apply:** a raw S3 client that sends plaintext keys and bodies — that is Standard mode ("encrypted at rest with keys we hold", F-14); a developer gets Private through the SDK key core or SSE-C per bucket (WP-SSEC-1: streaming, multipart, copy, an "SSE-C required" bucket flag), and the interim is the client-side tools guide (restic, rclone crypt) over stored.cloud.

- **The hierarchy (Ente's names, then ours).** `masterKey` — 256-bit random, generated on the device at signup, never leaves it unencrypted. `keyEncryptionKey` — derived from the password with Argon2id (libsodium `crypto_pwhash`; Ente starts at the SENSITIVE ops/mem limits and, when the device cannot afford the memory, doubles ops and halves memory down to `MEMLIMIT_MIN`, below which registration from that device is refused — we pin the same adaptive rule and record the parameters beside the encrypted key); it encrypts the master key → `encryptedMasterKey` on the server. `recoveryKey` — random, shown once as a 24-word BIP-39 mnemonic; recovery key and master key are each encrypted under the other and both ciphertexts stored, so either one recovers the account. Key pair — X25519 (`crypto_box_keypair`): the public key is the one plaintext key on the server, the private key is stored encrypted under the master key. **Space key** (Ente: `collectionKey`, one per album; ours: one per bucket or app-plane space) — random 256-bit, encrypted under the master key for the owner, sealed to a member's public key (`crypto_box_seal`) for a share. **Object key** (Ente: `fileKey`) — random per object, encrypted under the space key; it encrypts the object's metadata (name, EXIF, tags, the real path) and, in Ente, the bytes (`crypto_secretstream`, XChaCha20-Poly1305, chunked). Everything that is a key or metadata uses `crypto_secretbox` (XSalsa20-Poly1305); nonces and salts from `randombytes_buf`. **Verification ID** = BIP-39 of SHA-256(publicKey), compared out of band to verify a share. **Legacy** = the recovery key wrapped to a trusted contact's public key.
- **Our change 1 — the bytes (F-12).** Ente encrypts a file under its random file key: no deduplication anywhere, by design (duplicates are found by the client from a hash in the encrypted metadata; the server never sees a content hash). Our north star keeps intra-tenant dedup because the ratios that pay for the Vault price are a customer deduplicating against their own history. So: the object key encrypts metadata and the **manifest**; the manifest lists chunks whose keys are **convergent under the space key** — `K_chunk = HKDF(spaceKey, SHA-256(chunk))`, nonce from the sealed bytes as in R8-01 — each wrapped under the object key in the manifest (GET never re-derives from plaintext it does not have); the chunker is the frozen Rabin polynomial `0x2ADD89E3B790BB` with the WP-R8-4 identity, the compressor pinned. Two objects in one space with the same chunk store it once; two spaces never converge (different keys), so sharing a space is the only way to share a chunk. What the server learns from this and Ente's scheme does not: which chunks **within one space** are identical (ciphertext equality), and chunk boundaries (10.7's padding question). The per-bucket **paranoid flag** (north star mode c; 2026-10-12: a flag inside Private mode, not a mode of its own) switches a space to random chunk keys — Ente's exact scheme — and gives up dedup. The privacy page says both in one sentence each.
- **Our change 2 — post-quantum sharing (F-12).** Ente's sealed boxes are X25519 only. Our archives are meant to sit for decades, so key sharing (space keys to members, the auth token at login, the Legacy wrap) is a **hybrid**: X25519 sealed box + ML-KEM-768 (Go `crypto/mlkem`, already the SSE-S3 KEM; in the browser a vetted JS/WASM implementation until WebCrypto has it), both shared secrets through HKDF **together with both KEM ciphertexts** (a generic KEM combiner; X-Wing's shortcut is TLS-only — its own draft says it "should not be used outside TLS" because it leans on the transcript — survey correction 2). The data path (XChaCha20 / AES-GCM, 256-bit keys) needs no change. Hybrid, never ML-KEM alone (a defect in one scheme must not expose the key). When HPKE (RFC 9180) gains a stable hybrid KEM, the sealed box becomes HPKE. **(2026-10-22, owner: ChaCha + post-quantum on the client side — done in the K2 crypto core):** Go 1.26 ships `crypto/hpke` with MLKEM768-X25519 (X-Wing, draft-ietf-hpke-pq KEM 0x647a), so the sealed box IS HPKE: X-Wing + HKDF-SHA256 + ChaCha20-Poly1305 — the suite age ≥ 1.3 uses for its `age1pq` recipients; no hand-written combiner. X-Wing is a general-purpose KEM (draft-11 § 1.2: "usable for most applications, including specifically HPKE"); the "not outside TLS" caveat in the 2026-10-11 survey belonged to X25519Kyber768Draft00, not X-Wing (survey corrected).
- **Login — Ente's SRP flow, adopted as is (F-13).** The password never reaches the server: `loginKey = crypto_kdf_derive_from_key(keyEncryptionKey)`, SRP-6a over the 4096-bit group, the server stores the verifier and never returns it; a session token is returned sealed to the user's public key, so holding the password AND the private key is what signs you in; e-mail verification is a second factor the user may add, TOTP and passkeys stay. One password serves login and the key-encryption key — which is why SRP is required (today the dashboard sends the password over TLS and bcrypt-hashes it; that stays for accounts with no Private space (Standard mode), and the first Private space created later re-registers the account on SRP). Recovery: verify e-mail, enter the recovery key, decrypt the master key, set a new password, re-encrypt the master key under the new KEK; nothing else changes (space and object keys are under the master key).
- **Public links.** The space key travels in the URL **fragment** (never sent to the server; Ente: "the decryption key is never sent to our servers"); the server holds an opaque link id, the link's expiry and a password verifier when one is set, and serves ciphertext to anyone with the id — the viewer decrypts in the browser. A public LINK is not a public BUCKET: 40.1's CDN plane and the CSAM-scan-on-public posture apply to public buckets (plaintext by necessity), never to link-shared vault objects, which stay ciphertext end to end. (2026-10-12, F-17: the viewer page of a link ships from the app origin `app.stored.cloud` — first-party code that decrypts in the browser, never customer plaintext served — while every public-BUCKET object is served from the separate non-brand content domain, never a stored.cloud host.)
- **What the server sees, written down (the 41.0 privacy-page table starts here).** Private mode (2026-10-12 name): e-mail (login — itself encrypted at rest under a server key with a keyed BLAKE2b lookup hash, 43.2), one public key, every ciphertext (keys, metadata, manifests, chunks), object and chunk SIZES, counts, timestamps, access patterns, and — our change 1 — which chunks within a space are equal; never a name, a path, EXIF, a face or search index (41.2 builds those on the device), a content hash, or a key. Ente's own privacy policy admits the same plaintext set: e-mail, public keys, "IP address and user agent details" in server logs, crash reports, payment invoices from the processor — Phase 43 is what we do about that list.
- **The naming problem is solved by the manifest, not the S3 key.** A vault object's S3 key is an opaque random id; its real name lives in the encrypted metadata; List is a client-side operation over the encrypted index (41.1 blind indexes make it searchable). This is north star ladder item 3, delivered by the SDK — raw S3 List on a vault bucket shows ids and we say so.
- **How the advanced techniques fit (owner question 2026-10-11: convergent + homomorphic + FastCDC).** Three different tools, three different jobs — the plan keeps them apart on purpose. (1) **Content-defined chunking is a published client contract**: the SDK cuts with the same frozen identity the server uses (WP-R8-4: `restic/chunker` Rabin polynomial `0x2ADD89E3B790BB`, 1 MiB min / 20 average bits ≈ 2 MiB / 16 MiB max, golden boundaries in the test suite) — what the plan calls "FastCDC" IS this frozen set (R8-11: the library is Rabin); a true FastCDC gear hash would be a knowing dedup reset and is not on the table. The browser needs the same cutter: compile the Go chunker to WASM (or port it) and pin it to the same golden-boundary vectors, so a chunk hash computed on a phone equals the one the server would compute. (2) **Convergent encryption is the dedup tool** and nothing else: under the SPACE key it gives a vault its own-history dedup (backups, VM images, dumps — the ratios that pay for the price); across spaces it is impossible by construction, and across tenants it would need a shared derivation, which is the confirmation-attack hole the north star closed (10.8 OPRF demoted) — and, said once for the whole plan (2026-10-12, owner asked for "FastCDC across every tenant" and was told no): **cross-tenant convergence and least access are opposites** — any key that makes two tenants' ciphertexts equal is a key the server, or anyone who takes it, can use to confirm what a tenant holds; there will never be a global convergent key. The only cross-tenant convergence we will ever offer is 10.3's **opt-in shared namespace for known-public artefacts** (OS images, package mirrors — content with no secret to confirm), gated by 10.6 proof-of-ownership and by F-3's measurement (instrument the plaintext GCI for the cross-tenant fraction first). (3) **Homomorphic encryption is the private-lookup tool, not an encryption mode**: 41.3 runs Apple's `swift-homomorphic-encryption` (BFV, post-quantum) as PIR so a client can ask "is this chunk / hash / album id known?" without the server learning the question — it makes 38.1's write-avoidance ("don't upload what the server holds") and the shared-namespace check private; it does NOT make two tenants' ciphertexts equal (only a shared key does), so it never substitutes for (2). The compute side of HE stays narrow by written verdict (35.7, 41.4): FHE inference for spam/policy scoring on encrypted features; anything heavy on encrypted content runs on the device (41.2) or in an attested enclave with a published measurement (41.4); ranked search over encrypted prose has no good answer and goes enclave-only. (4) What leaks even when all three are right: sizes, counts, timestamps, access patterns, within-space chunk equality and chunk boundaries — 10.7's padding/bucketing question, revisited with the isolation audit; the per-mode table (41.0) states it.
- **Verifiability ("Don't trust us").** The SDK and the apps are open source from the first commit; this design doc is public before the tier ships; **reproducible builds** for the CLI and the web app (the hash of what runs is the hash of what is published); the words "zero-knowledge" wait for an external audit (north star rule; Ente's audit cost class). Each SDK release carries the Argon2id parameters, the chunker identity and the compressor version it writes, so a future reader can verify any object. **(2026-10-12, F-16 — the transparency log is the mechanism):** an append-only log with signed checkpoints and an external witness (keystone library in the Engine lane) records every client build hash (CLI, web app, SDK releases — clients refuse a build that is not in the log, which is how a user knows the code that holds their keys is the code that was published), the server image measurements and key-release policy of 41.4, every escrow and recovery ceremony (F-14, F-19), law-enforcement productions and admin reads of customer metadata; 10.0.2's integrity manifest is its first entry type and 10.0.3's key transparency a second log of the same library. Still no "zero-knowledge" before the audit.
- **Build order inside 10.0 (Stage 4; 2026-10-12 per F-8/F-19 — after launch it moves to right after Stage 1a if the consumer crowd is chosen):** 10.0.1 SDK key core (a Go module compiled to WASM — one implementation for CLI, SDK and browser, an Engine-lane keystone) → **41.2 on-device ML** (the consumer crowd needs search and faces on day one of Private, so it follows the key core, not the tail) → 10.0.5 SRP/OPAQUE + passkeys → 10.0.2 browser → 10.0.3 recovery + sharing (hybrid sealed box, Verification ID, Legacy) **including the passcode recovery vault** (first realm = the attested box, 41.4) → 10.0.6 public links → 10.0.4 dashboard → 41.1 blind indexes → **only then "Private by default" on the consumer surface**; until the recovery vault ships, the consumer default is recoverable + an "Advanced protection" opt-in (Apple's shape). The developer surface is Standard by default throughout; Private there is the SDK or SSE-C per bucket. **Server-side prerequisite: WP-R8-1** (stored-random, versioned tenant keys) — the Standard-mode layer must be able to rotate and shred before Private spaces land beside it — **done 2026-10-12 (#682, migration 084)**; the next server-side prerequisite is F-14 (master key as a systemd `host+tpm2` credential, on-box ceremony, age-plugin-sss 2-of-3 escrow, every ceremony in the transparency log), which never changes the Standard copy "encrypted at rest with keys we hold".

Sources (read 2026-10-11): the technique survey `docs/research/PRIVACY_TECHNIQUES_2026-10-11.md` (papers, standards, expired patents, the eight corrections above) · [Ente architecture](https://ente.com/architecture) · [Ente adopts SRP](https://ente.com/blog/ente-adopts-secure-remote-passwords/) · [Ente cryptography audit](https://ente.com/blog/cryptography-audit/) · [Ente privacy policy](https://ente.com/privacy) · [Ente data visibility FAQ](https://ente.com/faq/security-and-privacy/data-visibility).

### 10.0.1: Client-Side Encryption SDK
**Files**: `sdk/go/e2ee/` (new — the key core: hierarchy, wraps, manifests, chunker, hybrid sealed box), `sdk/wasm/` (the same module compiled with `GOOS=js GOARCH=wasm`), `sdk/js/e2ee/` (a thin binding over the WASM, no second implementation). (2026-10-12 keystone: built once in the Engine lane; the browser (10.0.2), the apps (42.3) and the CLI all load the one core, pinned to the same golden-boundary vectors.)
- Go + JavaScript libraries for client-side encryption
- ~~ML-KEM-768 + AES-256-GCM (same primitives as 5.14.4, but runs on client)~~ **(2026-10-22, owner decision: ChaCha on the client)** XChaCha20-Poly1305 for keys, metadata, manifests and chunks (192-bit random nonces; fast without AES hardware — phones, WASM) and HPKE X-Wing for sharing; the server's Standard mode keeps AES-256-GCM + ML-KEM-768 (AES hardware on the box; ChaCha buys nothing quantum-wise — both are 256-bit symmetric). Built: `sdk/go/e2ee` (formats 0x01 box, 0x02 convergent chunk, 0x03 sealed box), `sdk/wasm`, `sdk/js/e2ee`, `make sdk-wasm`; `TestE2EE_WASMMatchesNative` runs the WASM in node against native Go
- Encrypt-then-upload pattern: data never leaves client unencrypted
- Key derivation: `HKDF(user_password, salt)` → master key → **per-tenant convergent chunk DEKs by default (2026-08-02 decision above); random per-object DEK = paranoid per-bucket opt-in** **(2026-10-11: superseded by the hierarchy above — Argon2id KEK + random master key + space key + object key; the convergent chunk keys hang off the SPACE key.)**
- ~~Streaming encryption: `io.Reader` wrapper, AES-256-GCM with 64KB blocks~~ (2026-10-22: no stream construction is needed — an object is chunks of ≤ 16 MiB from the frozen chunker, each sealed in one XChaCha20-Poly1305 call; the manifest lists them)
- `x-amz-meta-e2ee-version: 1` metadata tag on encrypted objects (server can't read content but can route/store)
- Key backup: encrypted key blob stored on server, recoverable with password
- **Account key option (2026-10-11, survey correction 7):** 1Password's two-secret key derivation — a 128-bit secret held by the user is mixed into the KDF with the password, so a stolen database cannot be cracked offline; default ON for Private mode on the developer surface and for the consumer "Advanced protection" opt-in; on the recoverable consumer default the recovery vault (10.0.3) stands in for it; the cost (lose both = lose the data) is stated at setup
- **Every wrap is AEAD with AAD = owner‖version‖purpose** (MEGA 2022 lost every user key to unauthenticated wraps and one master key for every purpose — correction 5; WP-R8-1's rule, inherited)
- **Encrypted names outside the manifest** (the plain-S3 names mode of the north star's ladder item 3): deterministic AES-SIV with the parent id as associated data (Cryptomator) or HCTR2 (Android's filename mode since 2021) — never CBC-CTS (shared 16-byte prefixes leak); correction 4

### 10.0.2: Browser-Side Encryption
- **The server-serves-the-JS hole (2026-10-11):** ship an integrity manifest of every asset hash and publish it to a transparency log, in the shape of Meta + Cloudflare's Code Verify (WhatsApp Web) and Cloudflare's WAICT proposal (Oct 2025, W3C-backed draft) — a browser or extension can then refuse a page whose code is not the published code **(2026-10-12: this IS the F-16 transparency log — the same library and witness; the manifest is one entry type, the SDK/CLI build hashes another, so a native client refuses an unlisted build the way the extension refuses an unlisted page.)**
**Files**: `sdk/js/e2ee/` (new), dashboard integration
- WebCrypto API for in-browser encryption before upload
- Pre-signed URL + client-side encrypt → browser-direct upload of encrypted data
- Bucket mode is a column (`buckets.mode` = private | standard | public, an Engine-lane keystone), set at creation through the SDK/API, shown — not toggled — in the dashboard; the dashboard's upload path follows the bucket's mode (2026-10-12, F-15: no "enable encryption" switch — a mode is never a user choice on an existing surface)
- Performance: ~200 MB/s in modern browsers (WebCrypto is hardware-accelerated)

### 10.0.3: Key Recovery + Sharing
**Files**: `sdk/go/e2ee/keyshare.go`
- Recovery phrase: BIP-39 mnemonic (24 words) shown once at setup
- **Split recovery (2026-10-11):** SLIP-0039 Shamir shares of the recovery key (2-of-3 to trusted contacts) as the Legacy mechanism beside Ente's single-contact wrap; no patent (Shamir 1979)
- **Passcode recovery vault with hardware attempt limits — REQUIRED before Private is the default on the consumer surface (2026-10-12, F-19; was "later, optional", survey correction 6):** the iCloud Keychain escrow pattern — a short passcode, an attested realm that counts attempts and erases after N; first realm = the F-18 box under 41.4, a second realm under a different operator before it is called hardware-enforced (one operator can restore a snapshot and reset the counter — Signal's reason for three enclave types; ours = our measured VM + a rented confidential VM or Nitro enclave, Juicebox protocol, F-19); the realm interface is an Engine-lane keystone; beside it trusted contacts (Ente "Legacy"), passkeys (10.0.5) and recovery nudges in the apps; until it ships the consumer default is recoverable + "Advanced protection" opt-in. References: the WhatsApp Backup Key Vault (HSMs + OPAQUE, NCC-audited 2021), Signal SVR3 (OSDI '24, trust split across enclaves of different clouds) and the open-source Juicebox realms (OPRF + secret sharing, HSM realm) all give a human-memorable path; consumers lose paper. Needs hardware or an attested enclave (41.4) — a real-world prerequisite, rule 4
- **Key transparency — on the F-16 transparency log (2026-10-12; the same append-only log, signed checkpoints and external witness as the build-hash and ceremony records, so it ships as a second entry type, not a second system):** an audited Merkle log of public keys (WhatsApp 2023–24, Signal 2026 with Cloudflare and Trail of Bits as auditors; IETF KEYTRANS) replaces hand-compared Verification IDs for shared Private spaces
- Key sharing: ML-KEM encapsulate to recipient's public key
- Shared buckets: all members derive same DEK from shared secret
- Revocation: re-encrypt with new key, old members lose access

### 10.0.4: E2EE Dashboard Experience
**Files**: `internal/dashboard/handlers/encryption.go` (new)
- Encryption status indicator per bucket (server-side SSE vs client-side E2EE)
- Key management UI: generate, backup, share, rotate
- Warning: "You control the keys — if you lose your recovery phrase, data is unrecoverable"
- File preview disabled for E2EE buckets (server can't decrypt)

### 10.0.5: SRP login + passkeys (added 2026-10-11, F-13)
**Files**: `internal/auth/srp.go` (new), `internal/dashboard/handlers/login_srp.go` (new), SDK `auth/`
- **OPAQUE (RFC 9807, July 2025) or SRP-6a — decide at prompt time (F-13 amended 2026-10-11):** OPAQUE is the standard aPAKE (no salt disclosed before authentication, a proof), used by WhatsApp's backup vault; SRP-6a is Ente's and 1Password's choice with more shipped implementations. Recommend OPAQUE when a reviewed Go + JavaScript pair exists, else SRP-6a with the 4096-bit group. Either way: `loginKey = kdf(keyEncryptionKey)`, the server stores the verifier/envelope + the Argon2id parameters per user and never returns them; the session token sealed to the user's public key
- An account without a vault keeps bcrypt; creating the first vault space re-registers it on SRP in one flow (password typed once)
- WebAuthn passkeys as a second factor beside TOTP; e-mail verification optional (Ente's `Settings > Security` shape)

### 10.0.6: Public links for vault spaces (added 2026-10-11)
**Files**: `internal/api/vault_links.go` (new), the web viewer in the app plane
- Opaque link id → space, expiry, download cap, optional password (verifier only); the space key only ever in the URL fragment; viewer decrypts in the browser; revocation deletes the id (and rotates the space key when the owner asks — re-wrap for members, the chunks stay)
- Never on the CDN plane; never counted as "public" for the abuse posture (ciphertext end to end)
- (2026-10-12, F-17) The viewer renders on the app origin (`app.stored.cloud`) because it is our code decrypting in the browser, never served customer bytes; anything that IS served customer plaintext (public buckets, 40.1) lives on the separate non-brand content domain. A link URL is `https://app.stored.cloud/l/<id>#<key>`; stored.ge links keep working as a permanent alias.


**Test**: Generate keys → encrypt file client-side → upload → verify server sees only ciphertext → download → decrypt client-side → compare with original → share key with another user → they can decrypt → revoke → they can't.

---


## Phase 6: Multi-Backend Wiring

> **✅ Phases 6.1-6.4 COMPLETE** — done out-of-order via Phase 5.12.3 (PR #244, 2026-05-13).
> All drivers (Lyve, iDrive, Geyser, OneDrive) implement `engine.Driver` and are wired in `main.go`.
> Auto-detect priority: `iDrive > Quotaless > S3 > Geyser > local` (code order; Quotaless creds 401-dead since 2026-08, unused). Full end-to-end benchmarks on Mac + SLC.
> Skip to Phase 6.5 (Filecoin) or Phase 7 (Smart Tiering) when ready.

### 6.1: Wire Seagate Lyve Cloud ✅ (pre-existing, benchmarked in 5.12.3)

### 6.2: Wire iDrive ✅ (5.12.3, PR #244)

### 6.3: Wire Geyser Tape ✅ (pre-existing, benchmarked in 5.12.3)

### 6.4: Backend Configuration ✅ (5.12.3, PR #244)

### 6.5: Backend Research & Tier Architecture (Filecoin, Sia, Pixeldrain, Quotaless)
- **Züs / 0chain — reviewed 2026-10-12, not a backend and not a reference** (`docs/research/ATTESTED_PLANE_AND_RECOVERY_2026-10-12.md`): the foundation's own miners/sharders no longer resolve, the one responsive sharder's last finalized block is 2026-08-07, blobbers are paid in a token worth ≈ $0.001, no third-party audit found, and the shipped proxy-re-encryption code is a custom unaudited scheme (mnemonic → key with no KDF, an unkeyed plaintext hash stored in the clear, the proxy holding the integrity secret). Nothing to adopt; three warnings recorded (14.4, 21.1, breadth).
> **UPDATED 2026-04-18**: Comprehensive benchmarking of ALL providers completed. Tier architecture finalized:
> - **iDrive E2** = Performance tier (579 MB/s, $4.125/TB, default)
> - ~~**Quotaless** = Bulk/Egress tier (393 MB/s, €0.60/TB at 100TB, FREE unlimited egress)~~ **DEAD** — account locked 2026-05, creds 401-dead since 2026-08, unused; driver removal deferred to the M12 exit
> - **Geyser** = Archive ($1.55/TB tape COGS), **Vault Flex** = Deep Archive sold at $2.55/$2.00 per TB, quota-sold (Founders $1/TB capped; "Vault18 $1/TB" is the superseded 2026-04 framing)
- **Pixeldrain** = CDN cache layer (808 MB/s download, not a storage tier)
- **Lyve Cloud** = `resilient` storage class + DR/fallback hot leg + Vault second copy (**IN the stack** — closed in #405; promo $0 COGS through mid-2028, no egress fees). Not a default customer tier: the 2026-04-18 "DROPPED" call (Wasabi acquisition, 90-day minimum billing) still rules it out as the smart-tier warm leg. Driver wired and validated (#312, #366). <!-- reconstructed: verified against .private/CLAUDE.md + PROGRESS.md; corrected 2026-09-22 against #405 -->
- See `.private/TIER_STRATEGY.md` for the three-tier GTM: Vault (archive), Standard (smart), Performance (B2 killer).

### 6.6: Own-Fleet Backend Driver (Gorilla/Hetzner boxes) <!-- reconstructed: added from LAUNCH_STRATEGY.md §9 "Code you need to build" + §2 fleet buying sequence — had no plan phase -->
*From `.private/LAUNCH_STRATEGY.md`: buy Gorilla R130 ($69/mo, 20TB) early post-launch, STOR-LA at ~200TB customers, STOR-OGD 24x28 ($729/mo, 560TB) at ~400TB to begin the Quotaless exit (M9-12).*
**Files**: `internal/drivers/ownfleet.go` (new)
- Run MinIO or SeaweedFS on the box; driver is standard S3-compatible (reuse `s3compat` + tuned transport from 5.12.7)
- Week 1-4 plan per LAUNCH_STRATEGY §7: install MinIO → register as backend → erasure-coding tests → production canary (1% of new writes)
- This is the prerequisite for the Quotaless exit and for Phase 11 shard placement on own fleet
- **Depends on**: buying the R130 (ops decision, ~100TB paying customers or earlier as testbed)

---

## Phase 7: Smart Tiering Engine
*Depends on: Phase 6 (multiple backends wired). Uses `object_locations` table to track where each object lives.*

> **Phase 7 COMPLETE** — PRs #297 (7.1-7.4), #298 (7.5), #299 (7.6). Shipped ahead of launch as part of Tier 2 work.
>
> **2026-09-30 — what of it still exists.** Review R6 found the 7.3 `TieringEngine` and 7.4 cost optimizer constructed but never consulted (and unsafe to start: it would have moved `_global` dedup chunks to tape without updating the head cache); both were deleted in #524 (WP-R6-4), with the selector, the read cache and access tracking. What routes today: `object_head_cache.backend_name` is the routing truth, the circuit breaker is the only failover signal, `resolvePutStorageClass` places by bucket tier / header, and smart demotion (5.15.8) is the only mover. `object_locations` (7.1/7.2) is a second, drifting source that WP-R6-1 removes from the read path; `tenant_cost_daily` is an orphan table (D-12). 7.5 (`tier_preference`) and 7.6 (`data_residency`) are live columns.

**What was built:**
- 7.1-7.2: Object location tracking with PostgreSQL `object_locations` table (migration 048) + two-tier lookup (sync.Map hot cache + LocationStore)
- 7.3-7.4: TieringEngine with hourly scan, cost tracking in `tenant_cost_daily`, blended cost calculation per tenant
- 7.5: Customer tier controls — `tier_preference` column on buckets (migration 049), dashboard radio buttons, management API endpoint
- 7.6: Multi-region data residency — `data_residency` column (migration 050), auto-derived from bucket region, tiering engine respects residency constraints, carbon badge on dashboard

### 7.1: Object Location Tracking
**File**: `internal/engine/routing.go`, migration `048_object_locations.sql`
> **Phase 7.1 COMPLETE** — PR #297
- `object_locations` table: tenant_id, bucket_name, object_key, backend_name, storage_class, size_bytes, created_at, last_accessed_at
- On PUT: insert location record for the backend that stored the object
- On GET: update `last_accessed_at` timestamp
- On DELETE: remove location record
- Two-tier lookup: in-memory `sync.Map` for hot objects, PostgreSQL for cold lookups

### 7.2: Location Store
**File**: `internal/engine/routing.go`
> **Phase 7.2 COMPLETE** — PR #297
- `LocationStore` interface: `Record(ctx, loc)`, `Lookup(ctx, tenant, bucket, key)`, `Delete(ctx, tenant, bucket, key)`
- PostgreSQL implementation with batch inserts (100-record batches, 5-second flush)
- In-memory cache: 10,000-entry LRU for recent lookups

### 7.3: Tiering Engine
**File**: `internal/engine/tiering.go`
> **Phase 7.3 COMPLETE** — PR #297
- `TieringEngine` runs hourly scan of `object_locations`
- Classification rules:
  - Hot: accessed in last 7 days → keep on iDrive (fast)
  - Warm: accessed 7-30 days ago → eligible for Lyve (egress-free) or stay on iDrive
  - Cold: not accessed in 30+ days → migrate to Geyser (tape, cheapest)
- ⚠ Note (2026-07-07, updated 2026-09-22): the hot<7d / warm 7-30d / cold 30+ brackets above (and TIER_STRATEGY's 0-30/30-90/90+) are **SUPERSEDED by the Smart demotion policy** — per-tenant hot budget = 15% × quota, demote idle ≥14d or over-budget LRU (never <3d old); see Phase 5.15.8 + `.private/SMART_TIER_DESIGN.md`. The shipped engine (#297) classifies by age/access generically. Warm tier: Quotaless is dead (M12 removal), Lyve is the `resilient` class / DR leg (not the smart-tier warm leg) → warm = stay on iDrive until own-fleet (6.6) exists. <!-- reconstructed: verified against TIER_STRATEGY.md; policy updated 2026-09-22 -->
- Migration queue: objects flagged for tier change are migrated in background
- Rate limiting: max 10 GB/hour migration to avoid backend overload

### 7.4: Cost Tracking
**File**: `internal/engine/cost_optimizer.go`
> **Phase 7.4 COMPLETE** — PR #297
- `tenant_cost_daily` table: tenant_id, date, backend_name, storage_bytes, cost_usd
- Blended cost per tenant: weighted average across backends where their data lives
- Dashboard: admin cost overview, per-tenant cost breakdown
- Per-backend cost rates configurable via admin settings

### 7.5: Customer Tier Controls
**File**: `internal/engine/storage_class.go`, `internal/dashboard/handlers/bucket_settings.go`, migration `049_bucket_tier_preference.sql`
> **Phase 7.5 COMPLETE** — PR #298
- `tier_preference` column on `buckets`: 'performance', 'standard', 'archive', 'auto' (default)
- Dashboard: radio buttons on bucket settings page to select preferred tier
- Management API: `PATCH /api/v1/manage/buckets/{id}` with `tier_preference` field
- S3 `x-amz-storage-class` header respected on PUT (maps to tier preference)
- 'auto' uses the tiering engine's classification; explicit preference overrides it

### 7.6: Multi-Region Data Residency + Carbon Badge
**File**: `internal/engine/tiering.go`, migration `050_data_residency.sql`
> **Phase 7.6 COMPLETE** — PR #299
- `data_residency` column on `buckets`: 'us', 'eu', 'ap', 'any' (default)
- Auto-derived from bucket region (e.g., eu-central-1 → 'eu')
- Tiering engine respects residency: EU-resident data never migrates to US-only backends
- Carbon badge on dashboard: estimated CO2 per TB based on data center PUE ratings
- Compliance: satisfies GDPR data localization requirements when set to 'eu'
- **On the edge plane (40.1, 42.2):** the hot copy follows the residency through R2 and KV jurisdictions (`eu`); residency is an organisation attribute set at creation and never changed.

**Test**: Upload object to iDrive → verify location tracked → wait for tiering scan → object classified as hot → change tier to 'archive' → verify migration queued → check cost tracking reflects new backend → verify EU-resident data stays on EU backends → carbon badge shows on dashboard.

---

## Phases R1-R6: iDrive Reseller Integration
*Depends on: Phase 6.2 (iDrive driver wired). Uses iDrive's Reseller API for per-tenant sub-account provisioning.*

*Reference: `.private/IDRIVE_RESELLER_API.md` for full API documentation. Detailed plan: `/Users/viera/.claude/plans/ancient-brewing-nova.md`.*

### R1: Reseller API Client
**Files**: `internal/drivers/idrive_reseller.go` (new)
- Go client for iDrive Reseller API (REST + API key auth)
- Endpoints: create/list/update/delete sub-accounts, get usage, set quotas
- Error handling: rate limits, auth failures, quota exceeded

### R2: Per-Tenant Sub-Account Provisioning
**Files**: `internal/drivers/idrive_reseller.go`, `internal/billing/stripe.go`
- On plan upgrade to iDrive tier: create iDrive sub-account for tenant
- Map Stored tenant_id → iDrive sub-account email
- Credentials stored encrypted in tenant metadata

### R3: Quota Synchronization
- Sync Stored plan quotas → iDrive sub-account quotas
- On plan change: update iDrive quota automatically
- Over-quota handling: iDrive rejects writes, Stored shows clear error

### R4: Usage Monitoring + Billing
- Poll iDrive API for per-sub-account usage
- Cross-reference with Stored bandwidth tracking
- Cost reconciliation: iDrive invoice vs. customer revenue
- **Smart egress routing** (from `.private/TIER_STRATEGY.md` novel feature #1): when a tenant's egress approaches the 3× free iDrive ratio, replicate their hot objects to a free-egress backend and shift reads there transparently — avoids the $10/TB overage, invisible to the customer. Detection lives here (usage poller); routing in the engine. <!-- reconstructed: added from TIER_STRATEGY.md — doc says "Build when: Phase R4-R5" but plan had no mention -->

### R5: Driver Pool (Multi-Account)
**Files**: `internal/drivers/idrive_pool.go` (new)
- Pool of iDrive sub-account drivers (one per tenant with dedicated account)
- Load balancing across sub-accounts for rate limit distribution
- Health check per sub-account

### R6: White-Label Ready
- Remove iDrive branding from customer-facing surfaces
- Custom S3 endpoint per sub-account if supported
- Branded email notifications for iDrive operations

**Test**: Create tenant → provision iDrive sub-account → upload via s3.stored.cloud → verify object on iDrive sub-account → check usage sync → upgrade plan → verify quota increase on iDrive.

---


## Phase 8: Content-Defined (Rabin) Chunking + Global Dedup

> **(2026-10-12)** "Global" here means the one `_global` chunk address of Standard-mode PLAINTEXT objects — the server already holds the bytes, so dedup across tenants costs no access it does not have. It never extends to encrypted data: cross-tenant convergence and least access are opposites (the 10.0 block), so Private-mode chunks dedup within one space only and the only cross-tenant convergence ever offered is 10.3's opt-in public-artefact namespace behind 10.6 + F-3's measurement.
*Depends on: Phase 6 (needs backends to store chunks across). Can start without Phase 7.*

**STATUS (2026-06-04):** 8.1–8.5 shipped + hardened. Reality differs from the sketch below — actual artifacts:
- **8.1 chunker** ✅ `crypto/chunker.go` (`DefaultChunker`, restic's Rabin chunker — 1 MiB min / 20 average bits ≈ 2 MiB / 16 MiB max, SHA-256; identity on record since WP-R8-4). Candidate successor to read before any change: **Chonkers** (Berger, arXiv 2509.11121, 2025-09, CC BY-SA) — CDC with provable strict bounds on chunk size and edit locality; a chunker change is a knowing dedup reset (WP-R8-4). A streaming `Chunk()` exists but the PUT path does NOT use it yet — see **8.4.1**.
- **8.2 GCI** ✅ `crypto/gci.go` — Postgres-backed with a 100K-entry in-memory cache (NOT Redis/Bloom as sketched; revisit only if lookup latency bites at scale).
- **8.3 migration** ✅ shipped as `051_chunking_dedup.sql` (not `021`), tables `global_content_index` / `tenant_chunk_refs` / `object_metadata` + `object_head_cache.is_chunked` (not `chunk_manifests`/`chunk_locations`).
- **8.4 upload** ✅ `api/s3_engine_adapter.go handleChunkedPut` (PR #300). Hardened: chunks live in a shared `_global` container so cross-tenant/cross-bucket dedup is retrievable (PR #302); atomic manifest replacement on overwrite via `ReplaceObjectManifest` (PR #305). **Open gap → 8.4.1 (PUT still buffers the whole object).**
- **8.5 download** ✅ `handleChunkedGet` (PR #301), then bounded per-chunk streaming + SHA-256 integrity verification (PR #303); bounded prefetch shipped 2026-07-30 (PR #403, `CHUNK_GET_PREFETCH`). 8.4.1 streaming PUT shipped same sprint (PR #400).
- **SSE interaction**: a >256 MiB object in an SSE-required context is now **rejected with 413** instead of silently stored plaintext (PR #304). The real fix (actually encrypting large objects) is **Phase 10** convergent encryption.
- **⚠ Numbering note**: git commits labeled the streaming/integrity/overwrite work "Phase 8.6 / 8.6.1" — that was **8.5 hardening**, NOT the 8.6 below. Plan **8.6 (GCI Dashboard) ✅ DONE (#307); 8.7 (GC) ✅ DONE (#308); 8.8 (migration tool) ✅ DONE (#309). Phase 8 COMPLETE. Next in Tier-2 sequence: Phase 9 (compression).**
- **Divergence from `.private/ADVANCED_ARCHITECTURE.md` (deliberate)**: the spec calls for 2/8/64 KB chunks + **chunk packing** (aggregate ~8KB chunks into 100MB packs, read via Range) + Bloom-filter/Redis GCI lookup. Shipped: 1/4/16 MB chunks, no packing (unneeded at 4MB avg — per-request overhead is amortized), Postgres GCI + 100K in-memory cache. **Revisit packing + small chunks only if** backup-workload dedup ratios disappoint (small chunks dedup better) — that change would also demand the Bloom/Redis lookup path. <!-- reconstructed: verified against ADVANCED_ARCHITECTURE.md §1/§3/§4 -->

### 8.1: Content-defined chunker (sketch; shipped as restic's Rabin chunker)
**File**: `internal/crypto/chunker.go`
- Content-defined chunking with a rolling hash (Rabin fingerprint, `github.com/restic/chunker`)
- ≈2 MiB average chunks (min 1 MiB, 20 average bits, max 16 MiB — WP-R8-4; "4 MB avg" was the sketch)
- Streaming: processes `io.Reader`, emits chunks via channel

### 8.2: Global Content Index (GCI)
**File**: `internal/crypto/gci.go`
- `global_content_index` table: chunk_hash (SHA-256), size, ref_count, backend_location
- Redis cache for hot lookups (~1ms)
- Bloom filter for fast negative lookups (skip 90%+ DB queries)

### 8.3: Database Migration
**File**: `internal/database/migrations/021_chunking.sql`
- `global_content_index`, `chunk_manifests`, `chunk_locations` tables

### 8.4: Dedup-Aware Upload Pipeline
- On Put: chunk → hash each → check GCI → if exists, increment ref_count → if new, store
- On Delete: decrement ref_count → if 0, mark for garbage collection
- Chunk manifests: object_key → ordered list of chunk hashes

### 8.4.1: Streaming Chunked Upload (memory) — ✅ DONE (WP-ChunkPerf PR #400, 2026-07-30)
*Shipped: `handleChunkedPut` streams the body through `ChunkContext` with a bounded worker pool (`CHUNK_PUT_CONCURRENCY`, default 8) — peak memory is workers + chunker buffer (≤ ~19 chunks of ≤ 16 MB) regardless of object size; dedup decisions stay sequential in the dispatcher; measured size used, no fallthrough (5xx on failure). Original gap description follows.*
- **Problem**: `handleChunkedPut` does `io.ReadAll` of the whole object before chunking, so peak memory scales with object size (a 2 GB upload = 2 GB RAM) — violates the "always stream, never buffer" architecture decision. (GET was fixed to bounded streaming in PR #303; PUT is the remaining half.)
- Feed the existing `hashingBody` (TeeReader→MD5) into the chunker's streaming `Chunk(ctx, r)` API; **add context cancellation to `Chunk()`** so an aborted upload doesn't leak its goroutine.
- **Commit to the chunked path** once the gate fires — do NOT fall through. (The current fallthrough is already broken: `ReadAll` drains the body, so the normal-path retry would store 0 bytes.) Return 5xx on failure; the client retries.
- Per-chunk (or windowed-batch) dedup lookup as chunks arrive; use the **measured** total size, not the declared `Content-Length`.
- Peak memory → one chunk + chunker buffer (~16–32 MB), matching the GET side.

### 8.5: Dedup-Aware Download Pipeline
- On Get: read manifest → fetch chunks (parallel) → reassemble → stream to client
- Chunk prefetching for sequential reads
- **DONE 2026-06-04** (PR #301 + #303): `handleChunkedGet` streams chunk-by-chunk into a bounded ~16 MB buffer, verifies each chunk's SHA-256 before serving, and serves ranges by selecting only overlapping chunks via `chunk_offset`. **Prefetch shipped 2026-07-30 (WP-ChunkPerf PR #403)**: up to `CHUNK_GET_PREFETCH` chunks (default 4) fetch+verify ahead of the write cursor; writer consumes strictly in order (Lyve GET 11–15s → 5.5s on 70 MB).

### 8.6: GCI Dashboard ✅ DONE (PR #307)
- Admin: global dedup ratio, storage savings (logical vs physical) — `/admin/dedup` (`admin_dedup.go`)
- Per-tenant dedup stats — per-tenant table on the admin page
- Customer: optional visibility ("32% dedup savings") — `populateDedupSavings` on the usage page

### 8.7: Dedup Garbage Collection ✅ DONE (PR #308)
**Shipped as** `internal/api/dedup_gc.go` (`DedupGCRunner`, nightly ticker; NOT internal/jobs — background jobs live in internal/api). Reconcile (LEFT JOIN GCI↔tenant_chunk_refs, `last_accessed_at`-grace-guarded so in-flight PUTs aren't corrupted) THEN sweep (DB-row-first conditional delete, then `_global` backend delete). Manual trigger: `POST /api/v1/admin/dedup-gc`. Future: reconcile is a full-table sweep — index/incremental at scale.
- Nightly: scan for ref_count=0 (rows have `marked_for_deletion=TRUE`, set by `decrement_chunk_ref`) → 7-day grace → delete from backend → remove from GCI
  - **UPDATE 2026-06-04**: delete chunk data from the shared `_global` container using each GCI entry's `storage_key` + `backend_id` — NOT a tenant namespace (chunks are global since PR #302).
  - Also reconcile **orphans** (REQUIRED, not optional, after streaming PUT #306): a chunk whose data + GCI ref were written but whose manifest write then failed (interrupted/disconnected PUT) has `ref_count ≥ 1` but no manifest entry. Streaming PUT made this common — a client disconnect mid-upload now leaves orphans (the old buffered path failed at `io.ReadAll` before storing anything). `ref_count=0` alone will NOT catch these (a failed-then-retried chunk ends at `ref_count=2` with 1 manifest ref → leaked even after delete). Reconciliation must compare each GCI chunk's `ref_count` against its **actual `tenant_chunk_refs` reference count** and decrement the excess; chunks reaching 0 then GC normally.
- Manual trigger: `POST /v1/admin/dedup/gc`

### 8.8: Dedup Migration Tool ✅ DONE (PR #309)
**File**: `cmd/dedup-migrate/main.go`
- CLI to convert existing monolithic objects into chunked + deduped format — streams via ChunkContext, verify-before-delete (ETag check before deleting the original), flag-before-delete ordering so live GETs never 404, idempotent re-runs. Flags: --dry-run, --min-size, --tenant, --bucket, --limit, --keep-original.
- Dry-run mode: calculate savings without migrating

**Test**: Upload 1GB file → verify chunks in GCI → re-upload same file → verify no new chunks (dedup).


---

## Phase 9: Lossless Compression
*Depends on: Phase 8 (compression operates on chunks)*

> **Phase 9 COMPLETE** — PR #311 (lossless zstd compression on chunked PUT/GET). Content-type-aware skip, per-chunk ratio tracked in GCI. <!-- reconstructed: verified against git log -->

### 9.1: Compression Layer
**File**: `internal/crypto/compressor.go`
- zstd level 3 (fast, good ratio)
- Content-type aware: skip already-compressed formats (jpg, png, mp4, zip, gz)
- Streaming: wraps `io.Reader` with zstd encoder

### 9.2: Pipeline Integration
- After chunking, before encryption: `chunk → compress → encrypt → store`
- On read: `fetch → decrypt → decompress → reassemble`
- Compression ratio tracked per chunk in GCI

### 9.3: Dashboard Integration
- Admin: global compression ratio, storage saved
- Per-tenant compression stats

**Test**: Upload compressible file (text/CSV) → check `original_size` vs `compressed_size` → verify 1.3-2x ratio. Upload jpg → verify compression skipped (ratio ~1.0).

---

## Phase 10: Post-Quantum Encryption
*Depends on: Phase 8 (encryption operates on chunks). Phase 9 (compression) should come first — compress then encrypt, not the reverse.*

**STATUS (2026-07-07):** 10.1-10.3 ✅ SHIPPED in PR #312 — `ChunkEncryptionService` (`internal/crypto/chunk_encryption.go`) with HKDF-derived deterministic nonces; same tenant + same content → identical ciphertext (dedup-safe convergent encryption, matches `.private/ADVANCED_ARCHITECTURE.md` §2). Pipeline: chunk → compress → encrypt → store. Migration shipped as **052** (`encrypted` + `encryption_algo` on `global_content_index`), NOT the `022` sketched in 10.4. This also closes the ">256 MiB SSE gap" from Phase 8 (#304's 413-reject was the interim). **Remaining open: 10.5 (customer key management UI), 10.6 (proof-of-ownership), 10.7 (security testing)** — cross-tenant dedup derivation (10.3's opt-in shared namespace) also unshipped. <!-- reconstructed: verified against PR #312 + ADVANCED_ARCHITECTURE.md -->

### 10.1: Hybrid PQ Encryption
**File**: `internal/crypto/encryptor.go`
- **ML-KEM-768** (NIST FIPS 203) for key encapsulation — quantum-resistant
- **AES-256-GCM** for symmetric encryption — fast, hardware-accelerated
- Hybrid: ML-KEM encapsulates shared secret → HKDF → AES key → encrypt

### 10.2: Key Hierarchy
**File**: `internal/crypto/keymanagement.go`
- **Shipped as WP-R8-1 (#682, migration 084, 2026-10-12):** `ENCRYPTION_MASTER_KEY` → per-tenant **stored-random, versioned** keys wrapped under it (rotation, re-encrypt job, master re-wrap, crypto-shred); the master key becomes a systemd `host+tpm2` credential read from `$CREDENTIALS_DIRECTORY` by a dedicated service user, no PCR binding, age-plugin-sss 2-of-3 escrow, every ceremony logged (F-14; follow-up WP: TPM-resident KEK). None of it changes the Standard-mode sentence "encrypted at rest with keys we hold". The sketch's "password-derived" master key was 10.0's job and lives there now:
- ~~Tenant master key (from KMS or password-derived)~~
- Per-object DEK: random, wrapped with tenant master key
- Convergent DEK for dedup: `HKDF(tenant_master_key, chunk_content_hash)`

### 10.3: Convergent Encryption
- Same chunk + same tenant → same ciphertext → dedup still works
- Cross-tenant dedup: opt-in shared namespace for known-PUBLIC artefacts only (OS images, package mirrors — content with no secret to confirm; 2026-10-12: never a general cross-tenant key, never for customer data). **Candidate mechanism (2026-10-11, survey correction 8): Liu–Asokan–Pinkas (CCS 2015) — uploaders of equal content derive a shared key through a PAKE mediated by the storage server, which learns nothing; no key server (DupLESS), no server secret (10.8). Still behind F-3's measurement and 10.6 proof of ownership; the randomised dedup threshold of Harnik–Pinkas–Shulman-Peleg (2010) is the side-channel countermeasure**

### 10.4: Database Migration
**File**: `internal/database/migrations/022_encryption.sql`
- `encryption_keys` table, `encrypted_chunks` metadata
- Add `encrypted` boolean to `chunk_manifests`

### 10.5: Customer Key Management
- Dashboard shows the bucket's mode (Private / Standard / Public — a column, never a toggle; F-15, 2026-10-12) and, for Private, the key state
- Key backup/recovery (show recovery phrase once)
- Key rotation (background re-encryption job)
- **Crypto-shredding prerequisite (2026-08-02):** server-side per-tenant chunk keys currently derive from `ENCRYPTION_MASTER_KEY` + tenant ID — re-derivable, so deleting a key row shreds nothing. Switch to **stored-random per-tenant keys** (random, wrapped by master key, deleted on account deletion) so key deletion = cryptographic erasure. Small migration + derivation change; makes GDPR Art. 17 exact. E2EE-tier data shreds trivially (client holds the only key).

### 10.6: Proof-of-Ownership Protocol
- Client uploads Merkle tree of chunk hashes
- Server challenges: "prove you have chunk N"
- Client responds with chunk + Merkle proof
- Prevents hash-table poisoning and confirmation attacks

### 10.7: Security Testing
- **(2026-10-12) The Broken-Ecosystem matrix is a named test set, not an assumption.** Hofmann & Truong, "End-to-End Encrypted Cloud Storage in the Wild: A Broken Ecosystem" (ETH Zurich, CCS 2024) broke Sync, pCloud, Seafile and Icedrive under a malicious-server model; Ente was not studied. Every class is a test against our own server acting maliciously: (1) unauthenticated key material (key replacement), (2) unauthenticated public keys (certificate/key substitution — Tresorit's gap), (3) protocol downgrade (weak KDF path), (4) link-sharing leakage (secrets in URLs reaching the server), (5) unauthenticated encryption modes (CBC flipping/truncation), (6) unauthenticated chunking (block removal/reordering), (7) filename and location tampering, (8) metadata tampering (creator, sharing lists), (9) folder/file injection. F-12's AEAD-with-AAD on every wrap, signed manifests, authenticated chunk boundaries and keys in the URL fragment answer most by construction — the audit checks each one, and the privacy page may cite the paper only after they pass.
- Constant-time hash comparisons (timing attack prevention)
- Rate limiting on hash lookups (1000/sec per tenant)
- Honeypot hashes to detect attackers
- Cross-tenant isolation audit
- Chunk-size fingerprint mitigation: FastCDC boundary patterns leak content structure even under encryption — pad or bucket chunk sizes; verify a known plaintext can't be identified from its chunk-size sequence alone. **Decided 2026-10-11 (survey correction 3): the padding schedule is Padmé (PURBs, Nikitin et al., PoPETs 2019 — O(log log M) bits of length leakage at ≤ 12 % overhead, shrinking with size) for vault manifests and whole-object vault blobs; the paranoid flag pads chunks with it too and may disable compression (the CRIME/BREACH class)**

**Test**: Verify raw chunks on backend are unreadable ciphertext. Upload same file twice → verify dedup still works despite encryption.

### 10.8: Blind Dedup — Server-Aided Convergent Encryption (OPRF / DupLESS-style) — ⏸ DEMAND-GATED (decided 2026-08-02)
*Depends on: 10.3 (convergent scheme), 10.0.1 (client-side SDK), 10.6 (proof-of-ownership — mandatory at cross-tenant scope), WP-7 (tenant-scoped encrypted dedup stays the safe default this opts out of).*

**⏸ DECISION 2026-08-02 (see `.private/PRIVACY_NORTH_STAR.md` for full rationale): DO NOT BUILD unless the measurement trigger fires.** Private mode ships with **per-space (F-12; was per-tenant) convergent keys derived client-side** (10.2's `HKDF(tenant_master_key, chunk_hash)` as the 10.0 SDK default) — strictly stronger than OPRF (no server-held secret to steal/compel; OPRF's `k` theft is silent and retroactively breaks confirmation-resistance for all tenants), no new critical-path service, and the WP-7 tenant-scoped GCI machinery already shipped. The dedup OPRF would add back is only *cross-tenant* convergence on encrypted data — but the ratio drivers in the margin model (backups 10–50x, VM images 5–15x) are intra-tenant and fully preserved. OPRF's security boundary is also sybil-attackable rate limiting (N free signups = N×budget dictionary oracle), i.e. an abuse arms race, not cryptography. Precedent for the chosen scheme: Tarsnap (2008–present), restic, borg.
**2026-10-03 (decision F-7, Phase 41.3):** the OPRF design is superseded as the *mechanism* by private information retrieval — the same measurement trigger decides whether cross-tenant dedup is built at all, but the lookup then runs through a homomorphic PIR service with no server-held secret.

**Measurement trigger**: instrument the plaintext GCI for the fraction of dedup hits that are cross-tenant vs intra-tenant (that fraction is the empirical ceiling on what blind dedup could recover). Build 10.8 only if it is large AND E2EE-tier margins need it.

**Goal (if triggered)**: cross-tenant dedup where the server NEVER sees plaintext — client-side chunking + encryption, chunk keys derived via an oblivious PRF against the hub, so identical chunks converge to identical ciphertext across tenants while the hub learns nothing about content and dictionary/confirmation attacks are rate-limit-bound (DupLESS, Bellare et al. 2013).

**Files**: `internal/crypto/oprf.go` (new), `sdk/go/e2ee/blinddedup.go` (new), extension to `internal/crypto/chunk_encryption.go`

- **10.8.1: OPRF key service** — hub-side oblivious PRF endpoint (ristretto255/2HashDH; `cloudflare/circl` — already a dependency — ships OPRF). Flow per chunk: client blinds `H(chunk)`, hub evaluates under a global secret without seeing the hash, client unblinds → convergent DEK. Hub never learns chunk hash OR content.
- **10.8.2: Rate limiting as the security boundary** — per-tenant OPRF query budget (~1000/s, shared limiter with 10.7); this is what defeats the confirmation/dictionary attacks that break naive convergent encryption. Budget violations → abuse queue (3.11).
- **10.8.3: Client-side pipeline** — SDK does FastCDC → OPRF key derivation → AES-256-GCM → upload ciphertext chunks; server dedups ciphertext against the GCI exactly as in Phase 8. Opt-in per bucket.
- **10.8.4: Cross-tenant scope + PoW gate** — cross-tenant convergence requires 10.6 proof-of-ownership on first reference (prevents hash-claiming); tenant-scoped remains the default per WP-7.
- **10.8.5: Key rotation epochs** — OPRF secret rotation; old-epoch chunks stay readable, new chunks converge within their epoch (accepted tradeoff: dedup doesn't cross epochs).

**Product framing (2026-10-12, F-15 supersedes the 2026-08-02 three-mode ladder):** the modes are **Private / Standard / Public**, a property of the surface, never a customer choice between privacy and economics; "blind dedup" is not a mode but, if F-3's measurement ever fires, a PIR lookup (41.3) inside Private mode. The old (c) "no dedup" was wrong too — Private keeps within-space dedup (F-12); the paranoid flag gives it up.

**Test**: two tenants upload the same file in blind-dedup mode → GCI stores one ciphertext copy → hub logs contain only blinded OPRF inputs → a third tenant uploading a *guessed* file cannot confirm its existence faster than the rate limit permits → rotate epoch → old data still readable.

---

## Phase 11: Reed-Solomon Erasure Coding
*Depends on: Phase 6 (needs multiple backends for shard placement). Phase 10 (encrypt before erasure coding). Full pipeline order: chunk → compress → encrypt → erasure code → store.*

> **BENCHED, NOT BUILT (audit 2026-09-30).** Nothing in the product encodes, places or repairs a shard: there is no `internal/crypto/erasure.go`, no `shard_locations` / `repair_queue` table, and `klauspost/reedsolomon` in `go.mod` is imported only by `cmd/tools/erasure-bench` (an operator tool, never linked into `cmd/vaultaire`). The work so far is the rehearsal on the real drivers (#469–#472, #488, #490): `bench-results/ERASURE-2026-09-24.md`, `ERASURE-2026-09-25-all-backends.md`, `ERASURE-2026-09-27-standard-shapes.md`. **Design of record for Vault = RS(12,6) on three legs: Lyve 6 data / OneDrive 6 data / Geyser 6 parity** (RS(12,12) adds a fourth 6-parity leg; RS(10,6) and the placement in 11.1/11.2 below predate the benches). Binding findings: commit on the six Lyve shards and nothing else (578–584 MB/s at 1 GiB; gating on OneDrive too drops it to 59–72 MB/s), every other leg written asynchronously, reads take the first k with cancel, Geyser is parity-only and its repair path issues `RestoreObject`, never a GET, and a read requests all n and retries a failed shard once before calling it lost. Build order when it starts (~day 45 post-launch): 11.1 + 11.7 schema and encode/decode → 11.2 placement + async parity writer (this is P3) → 11.3 pipeline behind a flag → 11.6 repair → 11.8 dashboard; 11.4 RaptorQ and 11.5 LRC stay unscheduled.

### 11.1: Reed-Solomon Implementation
**File**: `internal/crypto/erasure.go`
- RS(10, 6): 10 data + 6 parity = 16 shards, tolerates 6 losses <!-- reconstructed: corrected against ADVANCED_ARCHITECTURE.md §5 and LAUNCH_STRATEGY.md §6 — previous reconstruction said RS(14,10)=10+4, which matches no reference doc -->
- 1.6x overhead (vs 3x replication); evolves to (10,14) then (10,13) as own fleet grows, per LAUNCH_STRATEGY §6 OneDrive-conservative plan
- Uses `klauspost/reedsolomon` GF(2^8) — **measured 2026-10-03 (`bench-results/ERASURE-2026-10-03-codecs.md`): 14–60 GB/s encode, 1.2–3 GB/s reconstruct on the SLC box; a 1 GiB object ≈ 50 ms to encode, ≈ 0.7 s to rebuild from a six-shard loss on one core. CPU is not the constraint at any shape up to 32 shards; Leopard GF(2^16) is slower here and wins only past a few hundred shards (klauspost switches automatically)**

### 11.2: Shard Placement Strategy
- **AONT-RS (2026-10-11):** Rivest's all-or-nothing transform before Reed-Solomon (Resch & Plank, FAST 2011) — no single shard, hence no single vendor, reveals anything even for an unencrypted object, with the key inside the package; a second confidentiality layer across iDrive / Lyve / Geyser at near-zero key management. The paper is open; Cleversafe's dispersed-storage patents (filings 2005–2015) are checked per claim before any construction is copied (`docs/research/PRIVACY_TECHNIQUES_2026-10-11.md` § 5)
- Per `.private/ADVANCED_ARCHITECTURE.md` §4/§6 launch placement (16 shards): own fleet 6 data (needs Phase 6.6), iDrive 4 data, Geyser LA + London 2 parity, permafrost/OneDrive fleet 4 parity across 4 different tenants <!-- reconstructed: corrected — previous placement included Lyve (5 shards); Lyve is the `resilient`/DR leg (#405), not part of the shard plan -->
- Until own fleet exists: iDrive carries the data shards; adjust scheme accordingly
- No single backend loses enough shards to break reconstruction; permafrost never holds enough to reconstruct alone (privacy by design)
- Placement considers: health score, latency, cost, region

> **Bench of record (2026-09-24, `bench-results/ERASURE-2026-09-24.md`, tool `cmd/erasure-bench`)**: RS(10,6) rehearsed on the real drivers across Lyve + Geyser + OneDrive fleet from SLC, every read shape hash-verified. Findings that bind 11.2/11.3/11.6: (1) writes are gated by OneDrive (~6 MB/s per shard → 59 MB/s client-visible at 1 GiB) → **parity shards to OneDrive/Geyser must be written asynchronously**, sync PUT commits on the 10 data shards; (2) first-k-with-cancel reads deliver 170–211 MB/s at 256 MiB–1 GiB and never wait on Geyser; (3) Geyser is parity-only: even from its disk cache it is the read bottleneck (27 s for 4 shards at 1 GiB), and tape-cold shards need RestoreObject → the 11.6 repair path issues restores, not GETs; (4) losing any one backend reconstructs (no backend holds > m shards); (5) encode 12 GB/s / decode ~1.5 GB/s — CPU is free.

### 11.3: Pipeline Integration
- After encryption: `chunk → compress → encrypt → erasure encode → store shards`
- On read: `fetch ≥10 shards (parallel) → decode → decrypt → decompress → reassemble`

### 11.4: RaptorQ — for the rateless property, not for speed (re-scoped 2026-10-03, decision F-11)
**File**: `internal/crypto/raptorq.go`
- **Measured (`bench-results/ERASURE-2026-10-03-codecs.md`):** the premise "better performance than RS for large objects" is false — Reed-Solomon GF(2^8) encodes 10–20× and decodes 2.5–3× faster than the best RaptorQ implementation at every size to 1 GiB, on the Ryzen box and on an M1. The best pure-Go RaptorQ (`mstephenholl/graptor-q`, MIT, needs Go 1.26) reaches 1.2–2.6 GB/s encode and 0.5–0.9 GB/s decode; `xssnick/raptorq` (MIT, Go 1.25) 0.6–1.4 / 0.46–0.97; both on par with the Rust reference. Still 3–5× faster than the fastest network read, so RaptorQ is *affordable*, just not *faster*.
- **What it buys:** rateless, interchangeable repair symbols — a backend holds "40 % of the symbols" with no fixed shard position, a new leg gets fresh symbols without re-layout, ≈ K+2 of any symbols rebuild (failure 10⁻⁶ at K+2; not strictly MDS). Use it where that matters: fan-out of many small repair symbols to spokes and edge nodes (Phase 20/21), or a Vault layout whose vendor mix changes. Neither RS nor RaptorQ cuts repair *traffic* below k shards — that is Clay/MSR (11.5).
- **Patents:** Qualcomm's RFC 6330 IPR (#2554) — the two core patents expired 2024-11/12; US 8,887,020 (to 2028-11) and US 9,419,749 (permanent inactivation, to 2030-12) stand; usable now under Qualcomm's non-assert for a fully RFC-compliant implementation outside wireless WAN standards, unconditionally free from 2031. LT and Tornado codes are fully expired (2017–2019). Pin the licence check here (rule 4 of the track).
- `google/gofountain` (Raptor R10) is unmaintained and fails its own round trip — never a candidate.

### 11.5: Repair-efficient codes — Clay/MSR first, LRC second (re-scoped 2026-10-03)
- The goal is repair traffic below k shards' worth when one leg dies. **Clay codes** (Vajha et al., FAST '18; the MSR construction in Ceph's erasure plugin, open) give RS's storage overhead and fault tolerance with up to 2.9× less repair traffic and 3× faster repair. **Azure-style LRC is patent-encumbered** (Microsoft's 2012 family into the early 2030s; the 2018 overlapped-LRC patent to 2038) — only with a licence check.
- Configurable per tier; the own-fleet leg (6.6) is where repair cost is ours and this pays.

### 11.6: Background Self-Healing Repair
- Goroutine checks shard availability periodically
- If backend down or shards lost → reconstruct from remaining → write new shards to healthy backends
- `repair_queue` table, priority-based

### 11.7: Database Migration
**File**: `internal/database/migrations/023_erasure_coding.sql`
- `shard_locations` table, `repair_queue` table

### 11.8: Dashboard Integration
- Admin: shard distribution, durability score, repair queue status
- Customer: "11-nines durability" indicator

**Test**: Kill a backend → verify objects still readable → verify repair job creates new shards.

---

## Phases P1-P6: OneDrive Fleet Parity Layer
*Depends on: Phase 8 (FastCDC chunking) + Phase 10 (encryption). Uses consumer OneDrive accounts (5TB each) as a free durability replication layer. NOT a primary storage tier — async replication only.*

*Reference: `internal/drivers/onedrive_README.md` (dual-transport pattern), `.private/PERMAFROST_TESTING_RESULTS.md` (v1→v3.1 benchmarks). Fleet status: 3 active tenants (viera.fun, viera.pics, fromhis.com), Strategy F = 183 MB/s fleet download.*

> **Update 2026-06-17 (#312)**: the fleet driver is now named **"permafrost"** throughout the codebase and was removed from user-selectable storage classes — it is an internal, admin-only parity tier, exactly as this phase intends. P1 remains the only completed sub-phase. <!-- reconstructed: verified against PR #312 -->

### P1: OneDrive Production Driver ✅ COMPLETE (done in Phase 5.12.3, PR #244)
**Implemented ahead of schedule in 5.12.3.** Full rewrite from scaffold:
- Raw HTTP client (no SDK — Microsoft Graph SDK adds 30+ dependencies)
- Dual-transport pattern: HTTP/2 for Graph API metadata, HTTP/1.1 for CDN chunk uploads/downloads
- Streaming uploads via upload session (createUploadSession → PUT ranges)
- Parallel range-based downloads (4 concurrent ranges for large files)
- DNS cache + fleet-wide TLS session cache (128 entries per tenant)
- Token mutex for thread-safe OAuth refresh across concurrent requests
- RateLimit tracking (429 Retry-After parsing)
- Per-tenant credentials (client_id, client_secret, tenant_id)

### P2: Fleet Manager
**Files**: `internal/drivers/onedrive_fleet.go` (new)
- Manage multiple OneDrive accounts as a storage fleet
- Account registry: tenant credentials, capacity, health, last-checked
- Round-robin or least-used allocation for new shard placement
- Auto-failover: if one account hits rate limits, route to another
- Current fleet: 3 tenants (viera.fun, viera.pics, fromhis.com), 5TB each = 15TB total

### P3: Async Parity Replication
**Files**: `internal/engine/replicator.go` (extends existing replicator)
- After primary PUT to paid backend: queue async parity shard write to OneDrive fleet
- Shard selection: erasure coding (Phase 11) determines which shards go to OneDrive
- OneDrive gets 2-4 parity shards per object (never enough to reconstruct alone)
- Replication lag target: <5 minutes for objects <100MB, <1 hour for larger
- Failure handling: if OneDrive write fails, mark shard as "pending" and retry on next cycle

### P4: Health Monitoring + Capacity Tracking
**Files**: `internal/drivers/onedrive_fleet.go`
- Per-account health: storage used/available, API rate limit headroom, token expiry
- Alert when account approaches 5TB limit (provision new account)
- Fleet-wide dashboard: total capacity, used, available, health score per account
- Rate limit budget tracking: remaining requests/second per tenant

### P5: Backup Verification
**Files**: `internal/engine/backup_verify.go` (new)
- Periodic verification: sample random shards from OneDrive, verify SHA-256 matches GCI
- Report: "99.97% of shards verified intact" on admin dashboard
- Corrupted shard detection → automatic repair from remaining shards + primary backend
- Verification frequency: 1% of shards daily (full fleet verified in ~100 days)

### P6: Fleet Scaling
- Add new OneDrive accounts: register → probation (24h health monitoring) → promote to active
- Capacity planning: when to add accounts based on growth rate
- Account rotation: retire old accounts, migrate shards to new ones
- Target: 20 accounts (100TB) at 100TB customer data, 50 accounts (250TB) at 500TB

**Test**: Upload large file → verify primary on iDrive → verify parity shards appear on OneDrive fleet within 5 minutes → kill one OneDrive account → verify object still retrievable → verify repair job creates replacement shards → fleet dashboard shows correct health.

---

# TIER 3: SCALE & FEATURES (Build once you have product-market fit — since 2026-10-01 ordered by the FULL-PLAN TRACK)

> **Brand note (2026-10-12, F-17):** where this tier says "stored.ge" as a name it means **Stored** (the product) or **stored.cloud** (the domain); stored.ge is a permanent alias. Literal URLs are fixed in place; prose is swept at each phase's re-scope PR. Public datasets and public buckets are Public mode by surface: plaintext, served from the separate non-brand content domain.

---

## Phase 12: S3 Lifecycle Rules
*Depends on: Phase 7 (tiering engine for tier transitions)*

> **NOT STARTED — no package (audit 2026-09-30).** No `?lifecycle` sub-resource is routed (`S3Parser.determineOperation` knows versioning, location, notification, object-lock, logging, inventory, uploads, versions and acl at bucket level — nothing else), there is no `internal/engine/lifecycle.go` and no lifecycle table. The only lifecycle-like behaviour in the product is the abandoned-multipart reaper (#346, server-wide, not configurable per bucket). The tiering engine this phase "depends on" was deleted (#524); Transition rules will map onto the floors (`standard` / `vault`) and smart demotion. Post-launch queue A, first item: Expiration + AbortIncompleteMultipartUpload first. **2026-10-01: build it as the first front-end of Phase 31.2's bucket-rule engine (lifecycle XML in, rules stored) — one rule table, not a separate lifecycle engine.**

### 12.1: Lifecycle Rule Engine
**Files**: `internal/engine/lifecycle.go` (new)
- S3-compatible lifecycle configuration XML: `PUT /{bucket}?lifecycle`
- Rule types: Expiration (auto-delete after N days), Transition (change storage class after N days), AbortIncompleteMultipartUpload
- Prefix/tag filtering: rules apply to objects matching prefix or tag
- Daily evaluation: background job checks all rules against all objects
- Integration with tiering engine: Transition rules map to tier changes

### 12.2: Lifecycle Dashboard
**Files**: `internal/dashboard/handlers/bucket_settings.go`
- Visual lifecycle rule builder (no XML required)
- Preview: "This rule will affect 1,234 objects (5.6 TB)"
- Rule history: when rules last ran, objects affected

**Test**: Create lifecycle rule: expire objects with prefix "tmp/" after 7 days → wait for daily evaluation → verify objects deleted → create transition rule → verify objects moved to archive tier.

---

## Phase 13: Multi-Protocol Support
*Depends on: Phase 5.10 (S3 fully working as the reference protocol)*

> **NOT STARTED — no package (audit 2026-09-30).** There is no `internal/protocol/`, no WebDAV/SFTP/FUSE listener and no `pkg/sftp` / `x/net/webdav` / FUSE dependency in `go.mod`; S3 is the only protocol, and its semantics (quota, Object Lock, versioning ledger, routing hints) live in `internal/api` handlers, not behind a protocol-neutral layer. Prerequisite before any second protocol: location-authoritative reads/deletes in the engine (WP-R6-1) and one shared object-write path (WP-R2-1 would give it). Interim for customers who ask: `rclone mount` or `rclone serve webdav|sftp` in front of the S3 endpoint, and JuiceFS (`docs/guides/juicefs-setup.md`) for a POSIX mount.

### 13.1: Protocol Abstraction Layer
**Files**: `internal/protocol/protocol.go` (new)
- `Protocol` interface: translates external protocol → engine operations
- S3 protocol adapter wraps existing S3 handlers
- Shared auth, quota, billing across all protocols
- Multi-protocol server manager: starts all protocol listeners

### 13.2: WebDAV Server (RFC 4918)
**Files**: `internal/protocol/webdav/` (new)
- `PROPFIND`, `PROPPATCH`, `MKCOL`, `GET`, `PUT`, `DELETE`, `COPY`, `MOVE`
- Maps WebDAV collections → S3 buckets, resources → S3 objects
- Auth: HTTP Basic → tenant lookup
- Locking: `LOCK`/`UNLOCK` for collaborative editing
- Port: configurable, default 8080
- **Edge variant (40.5, 42.6):** a Worker translating WebDAV to presigned S3 calls is the cheap first version for the storage-box lineup; the hub server stays for locking and FUSE consistency.

### 13.3: SFTP Server
**Files**: `internal/protocol/sftp/` (new)
- SSH key authentication → tenant lookup
- `ls`, `get`, `put`, `rm`, `mkdir`, `rmdir` map to engine operations
- Subsystem handling: `sftp-server` compatible
- Use `pkg/sftp` (Go SFTP library)
- Port: configurable, default 2222
- **Edge variant (40.6, 42.6):** SFTPGo in a Cloudflare Container with stored.ge as its S3 backend, inbound port 22 through Spectrum (needs the zone on Pro). Borg and rsync over SSH stay out of scope — no shell, no disk — and the LowEndTalk copy says so.

### 13.4: POSIX/FUSE Layer (Future)
- FUSE mount via `cgofuse` or `go-fuse`
- Local filesystem interface → engine operations
- Primarily for self-hosted deployments (Phase 15)

### 13.5: Cross-Protocol Consistency
- All protocols read/write same objects, same metadata
- Concurrent access testing: upload via S3, read via WebDAV, delete via SFTP
- Shared metadata: content-type, timestamps, permissions

**Test**: Upload via S3 → list via WebDAV → download via SFTP → verify identical content → concurrent read/write from multiple protocols.

---

## Phase 14: Data Intelligence
*Depends on: Phase 8 (chunking for content analysis)*

### 14.1: Access Pattern Analyzer
**Files**: `internal/engine/analytics.go`
- Classify objects: hot (daily access), warm (weekly), cold (monthly+), frozen (>90 days)
- Pattern detection: sequential scan, random access, time-of-day patterns
- Feed into tiering engine for smarter placement decisions

### 14.2: Cost Optimization Advisor
**Files**: `internal/engine/cost_advisor.go` (exists, expand)
- Per-tenant recommendations: "Move 200GB of cold data to archive tier to save $X/month"
- Dashboard widget with actionable suggestions
- One-click apply for recommended tier changes

### 14.3: Data Lifecycle Insights
**Files**: `internal/dashboard/handlers/analytics.go` (new)
- Object age distribution chart
- Access frequency heatmap
- Storage growth forecast (linear extrapolation)
- Cost forecast based on current growth + tier mix

### 14.4: Customer Backup Verification Reports *(2026-10-11: the honest mechanism is a proof of retrievability — Ateniese PDP 2007 / Shacham–Waters compact PoR 2008 — a sampled challenge the backend answers without the object being downloaded; on tape a scheduled job; ClearBox (CCS 2015) is the shape for attesting a customer's dedup savings. `docs/research/PRIVACY_TECHNIQUES_2026-10-11.md` § 5)* <!-- reconstructed: added from TIER_STRATEGY.md novel feature #4 — was missing from plan (P5 covers only internal shard verification, not the customer-facing feature) --> **(2026-10-12 — what NOT to accept, from the Züs review):** a challenge that proves a provider can *produce* a block within a window proves nothing about where it is held — a provider can fetch it from a peer at challenge time. Retrievability proofs here and for Phase 21 spokes are shard-specific, time-bound and tied to the erasure layout, as written above.
**Files**: `internal/engine/backup_verify.go` (shares machinery with P5), dashboard + email template
- Weekly/monthly per-bucket integrity check: sample N random objects, GET, verify checksum vs `object_head_cache`
- Report on dashboard: "All 15,247 objects in 'db-backups' verified intact. Last check: 2h ago."
- Email/webhook alert on ANY verification failure (uses 5.11.6 events + 5.13 email)
- This is a $10K+/yr enterprise feature (Veeam SureBackup) offered built-in — the deciding feature for compliance-sensitive backup buyers
- **Can be pulled forward** — simple cron over existing primitives; see Post-Launch 90-Day Priority Stack

**Test**: Upload mix of objects → access some frequently, leave others untouched → analyzer classifies correctly → cost advisor suggests tier changes → apply suggestion → verify cost reduction. Corrupt one object on the backend → verification run flags it → alert fires.

---

## Phase 15: Self-Hosted Distribution

> **(2026-10-12, F-9/F-18)** Two meanings of "self-hosted" meet here: customers running Vaultaire (this phase), and Stored running its own attested data plane on SEV-SNP EPYC (41.4, the F-18 box). Add a **15.5: Attested image** — a reproducible build whose measurement is published to trust.stored.cloud (F-16); the same image is what the licensing product (Phase 30, stored.cloud/platform) ships and what the SEV-SNP box boots.
*Depends on: Phase 5 (dashboard), Phase 6 (multi-backend)*

### 15.1: Docker Image
**Files**: `Dockerfile` (new), `docker-compose.yml` (new)
- Multi-stage build: Go build → Alpine runtime
- Single binary + embedded templates = small image (<50MB)
- `docker-compose up` with PostgreSQL service for quick start
- Environment variables for all configuration
- **Proven 2026-10-03:** a 30 MB static binary in a 54 MB alpine image boots as a Cloudflare `lite` Container (256 MiB; cold start 5.4 s, warm 150 ms) — the Phase 40.6 artefact; it needs Hyperdrive to a Postgres for keys and head rows.

### 15.2: ARM Builds
**Files**: `.github/workflows/release.yml` (new)
- Cross-compile for linux/amd64, linux/arm64, darwin/amd64, darwin/arm64
- GitHub Releases with pre-built binaries
- Raspberry Pi 4/5 compatible (arm64)

### 15.3: Helm Chart
**Files**: `deploy/helm/` (new)
- Kubernetes deployment with configurable replicas, resources, secrets
- PostgreSQL subchart or external database URL
- Ingress configuration for S3 + dashboard endpoints

### 15.4: Documentation Site → **merged into 26.5 at docs.stored.cloud** (2026-10-12, F-17): a "Self-hosting" section of the one portal, not a separate site; the `docs/` tree stays the source.
**Files**: `docs/` (expand)
- Installation guide, configuration reference, API documentation
- Architecture overview for self-hosters
- Migration guide from other S3-compatible services

**Test**: `docker-compose up` → dashboard accessible → create bucket → upload file → download file → verify single-binary simplicity.

---

## Phase 16: NAS Agent + Cache Agent
*Depends on: Phase 6 (backend drivers), Phase 11 (erasure coding for shard distribution)*

### 16.1: NAS Agent
**Files**: `cmd/stored-nas/` (new)
- Lightweight Go agent for home NAS (Synology, QNAP, TrueNAS, Unraid)
- Auto-discover local storage, register with stored.ge hub
- Selective sync: choose which buckets to cache locally
- WireGuard tunnel for secure communication with hub

### 16.2: NAS Dashboard
- Web UI on local network: sync status, bandwidth used, storage used
- Conflict resolution for offline edits
- Auto-update mechanism

### 16.3: Synology/QNAP Packages
- `.spk` package for Synology Package Center
- `.qpkg` package for QNAP App Center
- One-click install, guided setup

### 16.4: NAS Backup Integration
- Local NAS as backup destination for stored.ge data
- Reverse: NAS data backed up to stored.ge cloud
- 3-2-1 backup strategy made easy

### 16.5: stored-cache Agent
**Files**: `cmd/stored-cache/` (new)
- Lightweight Go binary for VPS cache nodes
- Caches hot objects locally, serves nearby users
- Reduces egress from primary backends
- Simple install: single binary + config file
- **LET value**: turn a $3/year VPS into a local cache for stored.ge data
- Independent of NAS agent — can ship separately, sooner
- **F-10 (2026-10-03):** the *read* role moves to Cloudflare (Phase 40.1); the cache agent keeps the write-local ingest and the LAN/NAS roles.

**Test**: Install NAS agent → register with hub → sync a bucket → modify file on NAS → see sync to cloud → modify in cloud → see sync to NAS. Cache agent: install on VPS → configure → access object → verify local cache hit on second access.

---

## Phase 17: Data Marketplace (Future Vision)
*Depends on: Phase 14 (data intelligence), Phase 20 (federation)*

### 17.1: Dataset Catalog
- Public datasets hosted on stored.ge (open data, ML training sets)
- Searchable catalog with metadata, preview, license
- Zero-egress access for stored.ge customers

### 17.2: Data Monetization
- Content creators publish datasets, set pricing
- Subscribers pay per-access or monthly
- Revenue share: 70% creator, 30% platform

**Test**: Publish dataset → catalog listing appears → subscriber pays → access granted → creator dashboard shows earnings.

---

## Phase 18: Multi-Tenant Platform + Reseller
*Depends on: Phase 7 (tiering), Phase 5.14 (compliance)*

### 18.1: Tenant Hierarchy
**Files**: `internal/auth/tenant_hierarchy.go` (new)
- Parent-child tenant relationships
- Reseller creates sub-tenants, manages their quotas
- Billing rolls up: sub-tenant usage → parent invoice
- **The tree of record (42.2):** platform → reseller → organisation (the paying and isolation unit: a family, a team, a business) → space → user; a user may belong to several organisations; metering tagged with organisation and reseller ids at write time.

### 18.2: Reseller Dashboard
**Files**: `internal/dashboard/handlers/reseller.go` (new)
- Reseller-specific admin view: manage sub-tenants
- Aggregate usage, billing, support for all sub-tenants
- White-label: reseller's branding on sub-tenant dashboards

### 18.3: Stripe Connect Integration
**Files**: `internal/billing/stripe_connect.go` (new)
- Resellers as Stripe Connected accounts
- Platform fee: stored.ge takes 10-20%, reseller keeps rest
- Sub-tenant billing flows through reseller's Stripe account

### 18.4: White-Label Configuration
- Custom domain per reseller (CNAME → stored.cloud)
- Custom branding: logo, colors, name
- Custom email templates with reseller's domain
- **On the edge (40.2):** Cloudflare for SaaS custom hostnames (100 free, $0.10 each after; apex and wildcard are Enterprise-only — document it), the reseller's sending domain through Email Service, theme tokens from the 42.3 ladder's layer 1.

### 18.5: Reseller API
- API for reseller operations: create/manage sub-tenants, check usage, adjust quotas
- API key scoping: reseller keys only see their sub-tenants

### 18.6: Affiliate Tracking
- Referral links with attribution
- Commission tracking and payout
- Integration with Stripe for automatic payouts

**Test**: Reseller creates sub-tenant → sub-tenant signs up via white-label → uses storage → usage appears on reseller dashboard → reseller invoice includes sub-tenant usage → Stripe Connect payout works.

---

## Phase 19: Cross-Account Access + IAM
*Depends on: Phase 18 (tenant hierarchy for cross-account context)*

> **NOT STARTED — no package (audit 2026-09-30).** No `?policy` sub-resource is routed, there is no `internal/auth/iam.go`, no policy table, and `internal/rbac` was deleted in #518 (D-11: it took authority from an `Authorization` substring). The whole authorization model today is: the tenant's primary key (full access), scoped `VLT_` keys (permission list, bucket scope, IP allowlist, expiry) and STS tokens that only narrow them. Cross-tenant access is impossible by construction (a foreign bucket is a 404). Needs a scope decision before any prompt: bucket policies only (public-read prefixes, cross-tenant grants) vs a full IAM evaluator. WP-R4-1 (GOVERNANCE bypass as a key permission) is the only pre-launch item in this area.

### 19.1: IAM Policy Evaluator
**Files**: `internal/auth/iam.go` (new)
- AWS IAM-style policy language (JSON): Effect, Action, Resource, Condition
- Policy evaluation: explicit deny wins, then explicit allow, then implicit deny
- Cross-account bucket policies: grant access to specific tenant IDs
- Integration with S3 auth: evaluate IAM policy before allowing operation

### 19.2: Bucket Policies
- `PUT /{bucket}?policy` — set bucket policy (JSON)
- Policy principals: tenant IDs, specific API keys, wildcards
- Actions: s3:GetObject, s3:PutObject, s3:DeleteObject, s3:ListBucket, etc.
- Cross-account: tenant A grants tenant B read access to specific bucket

### 19.3: Access Grants
- Simplified cross-account sharing: "Share this bucket with tenant X"
- Dashboard UI for managing grants
- Time-limited grants with automatic expiry

**Test**: Tenant A creates bucket → sets bucket policy granting Tenant B read access → Tenant B can GET objects → Tenant B cannot PUT → remove policy → Tenant B gets 403.

---

## Phase 20: Federation Protocol
*Depends on: Phase 11 (erasure coding for shard distribution), Phase 18 (multi-tenant for hub identity)*

*Reference: `.private/ADVANCED_ARCHITECTURE.md` for protocol spec, `.private/EDGE_NODE_STRATEGY.md` for edge node architecture.*

> **NOT STARTED — no package (audit 2026-09-30).** There is no `internal/federation/`, no `cmd/stored-spoke`, no gRPC or libp2p dependency in `go.mod`, no `nodes` table and no `/v1/federation/*` route. The hub is one process on one box; "spoke" exists only in these documents. Shard placement on spokes needs Phase 11 first, and the first federation step is not this phase but Edge Personal (16.5 cache agent: a single binary with a local S3 endpoint, write-through cache and a heartbeat — no sharing, no credits). Re-scope 20/21 from `.private/EDGE_NODE_STRATEGY.md` "Implementation Phases" before prompting — and under **F-10 (2026-10-03)**: the CDN-node role goes to Cloudflare (Phase 40.1); spokes keep ingest acceleration, storage shards and metadata.

### 20.1: Hub-Spoke Protocol
**Files**: `internal/federation/protocol.go` (new), `internal/federation/hub.go` (new)
- gRPC protocol for hub-spoke communication
- Protobuf service definitions: NodeAgent (register, heartbeat, task, report)
- Hub manages spoke node lifecycle: register → probation (24h) → promote → active
- Spoke types: cache, shard, egress, compute

### 20.2: Spoke Node Agent
**Files**: `cmd/stored-spoke/` (new)
- Lightweight Go binary for spoke nodes
- Auto-register with hub on first start
- Health beacon every 60 seconds: disk free, CPU, bandwidth, latency
- Task executor: store shard, serve cached object, relay ingress

### 20.3: Trust + Reputation System
**Files**: `internal/federation/trust.go` (new)
- Trust score 0-100 per spoke node
- Trust increases with uptime: 0-30 days = low (cache only), 30-90 = medium (shard), 90+ = high (egress)
- Trust decreases with: missed health checks, failed shard verifications, downtime
- Automatic drain at trust < 20: migrate all shards to other nodes within 1 hour

### 20.4: Shard Distribution
**Files**: `internal/federation/placement.go` (new)
- Placement engine: decide which shards go to which spoke nodes
- Constraints: geographic spread, backend diversity, trust level
- Spoke never has enough shards to reconstruct data (privacy by design)
- Rebalancing: as nodes join/leave, redistribute shards to maintain durability targets

### 20.5: Federation API
**Files**: `internal/api/federation.go` (new)
- `POST /v1/federation/register` — spoke registration
- `POST /v1/federation/heartbeat` — health beacon
- `GET /v1/federation/tasks` — pending tasks for spoke
- `POST /v1/federation/report` — task completion report
- Admin: `GET /v1/admin/federation/nodes` — all registered nodes with health/trust

### 20.6: Federation Dashboard
**Files**: `internal/dashboard/handlers/admin_federation.go` (new)
- World map with spoke node locations
- Per-node: health, trust score, storage contributed, bandwidth served, uptime
- Admin actions: approve/reject pending nodes, drain node, ban node
- Network stats: total spoke storage, total bandwidth served, geographic coverage

**Test**: Start spoke agent → registers with hub → passes probation → receives shard storage task → stores shard → heartbeat shows healthy → trust increases → second spoke joins → shards redistributed → kill first spoke → shards rebuilt on other nodes within 1 hour.

---

# TIER 4: PLATFORM SCALE (Build when you're a real business — since 2026-10-01 ordered by the FULL-PLAN TRACK)

---


## Phase 21: Storage Cooperative + BYOS
*Depends on: Phase 20 (federation protocol), Phase 7 (tiering engine)*

**The LET pitch**: "Bring your own storage, earn credits, join the network."


> Phase 21 turns this into a symbiotic network: users get cheap storage, stored.ge gets global edge nodes.


### 21.1: Credit System
- **(2026-10-12) Credits are fiat-denominated, never a floating token** — the Züs review: storage paid in a token loses its providers when the token does (≈ $200k market cap, ≈ $5/day volume and a chain stalled since 2026-08-07 as of 2026-10-12).
**Files**: `internal/billing/credits.go` (new), migration
- Credit ledger: earn credits by contributing storage/bandwidth, spend credits on stored.ge services
- Credit types: storage contribution (1 credit/GB-month), bandwidth served (0.5 credits/GB), uptime bonus (1.2x at 99.9%+)
- Credits deducted from monthly invoice before Stripe charges remainder
- Dashboard: credit balance, earning history, spending history, forecast
- Anti-abuse: minimum 72-hour uptime before earning, rate limit on credit claims
- Conversion: 1 credit = $0.01 (100 credits = $1 off invoice)
- Credit expiry: unused credits expire after 12 months

**Test**: Contribute 50GB storage → earn 50 credits/month → invoice shows $0.50 credit → bandwidth served → additional credits → verify anti-abuse limits.


### 21.2: Bring Your Own Storage (BYOS)
**Files**: `internal/drivers/byos.go` (new), dashboard integration
**Depends on**: Phase 6 (driver interface), Phase 7 (tiering engine)

**The LET pitch**: "Your $15/year RackNerd VPS becomes your personal storage tier."

Customer connects their own storage (S3-compatible endpoint, local disk on a VPS, NAS) to stored.ge. Three BYOS modes:


1. **BYOS-only**: all data on customer's storage. Vaultaire provides: S3 API translation, dashboard, metadata management, access control. Pricing: $0.99/TB/mo (orchestration fee only).
2. **Hybrid**: hot data on stored.ge, cold data on customer's BYOS backend. Vaultaire handles tiering automatically. Pricing: stored.ge rate for hot tier + $0.49/TB/mo orchestration for BYOS tier.
3. **BYOS as cache**: customer's VPS storage is the cache layer, stored.ge is durable backend. Like Phase 16.5 but managed through the dashboard rather than CLI.

**BYOS backend registration**:
- Dashboard: "Add Storage Backend" → enter S3 endpoint, access key, secret key → stored.ge runs health check + speed test → register
- Validation: stored.ge writes/reads a test object, measures latency, verifies encryption support

- Storage from BYOS backends counts toward the user's quota (they're providing the storage, so it doesn't cost stored.ge anything — just orchestration margin).
- Tiering: objects accessed frequently stay on stored.ge (fast). Objects accessed rarely migrate to user's BYOS backend (cheap). User's $15/year VPS becomes their personal cold tier.


### 21.3: Federated Spoke Nodes (Bring Your Own Node)
*Uses Phase 20 federation protocol*

**The LET pitch**: "Your $3/year VPS earns you free stored.ge credits just by running our agent."

Anyone runs a Vaultaire spoke node on their VPS → joins the stored.ge network:
- **Install**: `curl -sSL https://stored.cloud/install-spoke.sh | sh` (Docker one-liner or static binary)
- **Registration**: spoke calls `POST /v1/federation/register` with: location, available disk, bandwidth cap, uptime SLA commitment
- **Admin approval**: stored.ge admin approves/rejects nodes, sets trust level (0-100)
- **What the spoke does**:
  1. **Cache shard**: stores erasure coding shards (Phase 11) for redundancy. stored.ge places 1-2 shards on spoke nodes as extra redundancy layer. Spoke never has enough shards to reconstruct data (privacy by design).
  2. **Egress offload**: when a stored.ge user requests an object and the spoke has a cached copy, stored.ge redirects the download to the spoke (saves stored.ge egress, spoke earns bandwidth credits).
  3. **Ingress relay**: users near the spoke can upload through it (lower latency). Spoke queues and forwards to stored.ge hub.
  4. **Health beacon**: spoke reports health every 60s (disk free, CPU, bandwidth used, latency to hub). Unhealthy spokes get drained automatically.

**Trust model** (LET servers are unreliable — design for it):
- Spoke storage is NEVER primary. It's always redundancy or cache.
- Spoke going offline = one fewer cache/shard node. Zero data loss.
- Minimum 72-hour uptime before spoke gets any shard placements
- Trust score increases with uptime: 0-30 days = low trust (cache only), 30-90 = medium (shard placement), 90+ = high (egress offload)
- Spoke data is encrypted at rest (spoke operator cannot read stored data)
- Automatic drain: if spoke misses 3 consecutive health checks, all shards are rebuilt elsewhere within 1 hour

**Credit earning for spoke operators**:
- Storage contribution: 1 credit/GB-month at trust level ≥ medium
- Bandwidth served: 0.5 credits/GB for cache hits served to other users
- Uptime bonus: 1.2x at 99.9%+, 1.5x at 99.99%+
- Example: 50GB contributed + 200GB/month served + 99.9% uptime = ~110 credits/month = $1.10/month off your bill
- A $3/year RackNerd box with 50GB free disk pays for itself in ~3 months of spoke credits

**Spoke tiers** (maps to LET server classes):
| Tier | Requirements | Trust | Earns | Typical LET box |
|---|---|---|---|---|
| **Micro** | 10GB disk, 256MB RAM | Cache only | ~20 credits/mo | $3/year NAT VPS |
| **Standard** | 50GB disk, 512MB RAM, 1Gbps | Cache + shards | ~80 credits/mo | $15/year RackNerd KVM |
| **Contributor** | 200GB+ disk, 1GB RAM, 1Gbps | Full (cache + shards + egress) | ~200 credits/mo | $30/year HostHatch or BuyVM slab |
| **Partner** | 1TB+ disk, dedicated, 99.9% SLA | Priority placement | ~500+ credits/mo | $5-10/mo dedicated |

### 21.4: Peer Discovery + Trust
- DHT-based peer discovery (libp2p)
- Trust scores extend the Phase 20.3 reputation system to community-run nodes
- Sybil resistance: new nodes earn placement slowly (72h minimum uptime, probation)
- NAT traversal for home-network spokes (most LET NAT VPS boxes)

<!-- ============================================================
     reconstructed 2026-07-07: EVERYTHING BELOW THIS LINE was lost in the
     truncation (all three copies of the plan end mid-21.4 at 1,980 lines).
     Rebuilt from: this file's own internal cross-references (26.2/26.4/26.5/
     26.6/26.8, 27.1, 28.1/28.2/28.3/28.5), the Tier 3-4 handoff table
     ("21.6 Cooperative + BYOS + LET Network → 22.1"), MASTER_PLAN_SUMMARY.md
     step-phases 8/9/11/12/15, and memory (Tier 4 = "multi-tenant/reseller,
     smart buckets, federation, cooperative, BYOS, edge, ML, blockchain,
     advanced DX, integrations"). Sub-phase breakdowns are best-effort
     approximations, NOT the original text — re-scope each before building.
     ============================================================ -->

### 21.5: LET Guides + WHMCS Module
- Standalone guides, each doubling as a community post: "Turn your $15/yr VPS into a storage tier", "Earn credits with a spoke node", JuiceFS/rclone + BYOS recipes
- WHMCS provisioning module so LET hosts can resell stored.ge (pairs with Phase 18 reseller platform)
- `curl | sh` spoke installer polished + documented

### 21.6: Cooperative Network Launch
- Public network stats page (nodes, regions, storage contributed, bandwidth served)
- Credit dashboard GA; payout/credit redemption flow hardened
- LET launch thread for the spoke program; admission throttled via admin approval (20.6)

**Test**: End-to-end: stranger installs spoke on a VPS → probation → earns credits → credits reduce invoice → BYOS backend registered → hybrid tiering works → WHMCS reseller provisions a sub-tenant.

---

## Phase 22: Edge Compute (WASM Functions)
*Depends on: Phase 20 (spoke nodes as execution locations), Phase 16.5 (cache agent). From MASTER_PLAN steps 711-810.*

### 22.1: WASM Runtime
- Wasmtime embedded in hub + spoke agents; functions triggered on object events (uses 5.10.12/5.11.6 event plumbing)
### 22.2: Built-in Functions
- Image resize/thumbnail, video transcode probe, virus scan, metadata extraction — run on upload, results to metadata/webhooks
### 22.3: Customer Functions
- Upload WASM module, bind to bucket events, resource limits + sandboxing, per-invocation metering
### 22.4: Edge Placement
- Run functions on the spoke nearest the data (federation-aware scheduling)

---

## Phase 23: ML Intelligence
*Depends on: Phase 14 (access analytics as training data). From MASTER_PLAN steps 611-710.*

### 23.1: Access Prediction
- Predict object heat (next-7-day access probability) → feed tiering engine; start with simple heuristics, LSTM only if it earns its keep
### 23.2: Smart Cache Admission
- Cache admission/eviction informed by prediction (extends the NVMe cache shipped in #312)
### 23.3: Anomaly Detection
- Per-tenant behavioral baselines: exfiltration spikes, ransomware-pattern writes (mass overwrite), credential abuse → abuse queue (3.11)
### 23.4: Capacity Forecasting
- Fleet-wide growth forecasting driving the LAUNCH_STRATEGY buy-sequence triggers

---

## Phase 24: Data Query Engine
*Depends on: Phase 8 (chunk layout), Phase 13 (protocol layer). From MASTER_PLAN steps 1201-1240 "data-aware orchestration".*

### 24.1: S3 Select
- SQL over CSV/JSON/Parquet objects in place (DuckDB embedded), S3 Select API compatibility
### 24.2: Bucket Catalog + SQL
- Schema inference, zone maps/data-skipping indexes, cross-bucket queries
- **Edge variants (40.8):** DuckDB-WASM in the browser over range reads against presigned URLs (no server compute); Pipelines → Basin (Iceberg on R2) + R2 SQL for logs, inventories and exports as a lake at R2 prices.
### 24.3: Query API
- GraphQL endpoint + management API surface; streamed results; per-tenant query metering (billable feature)

---

## Phase 25: Web3 / Proof-of-Storage (OPTIONAL — demand-gated)
*Depends on: Phase 6.5 verdict — "premium proof-of-storage feature, not a cost play. Build only if customers ask." From MASTER_PLAN steps 911-1000.*

- Cryptographic storage proofs (Merkle challenges — shares machinery with 10.6)
- Filecoin anchoring via Lighthouse/Filebase for compliance certificates (Phase 6.5.x plan: `logical-hugging-muffin.md`)
- IPFS gateway for public buckets
- **Do not build speculatively** — every sub-item here needs a paying customer asking first. *(2026-10-01, owner decision: scheduled in Stage 8 of the FULL-PLAN TRACK and expanded as Phase 35; the gate is now the legal review for the token / market / governance parts, not customer demand.)*

---

## Phase 26: Advanced Developer Experience

> **The Developer-surface lane (2026-10-12; charter `docs/surfaces/DEVELOPER.md`):** Phase 26 is the home of ambition 2, "Stripe for storage" — an API that never surprises, docs, SDKs, idempotency, webhooks, sandbox tenants, transparent pricing, resellers on top. The lane = Phases 26, 31, 42.1, docs/SDKs and WP-SSEC-1. Mode on this surface is a property of the surface, never a user choice: Standard by default and first-class; Private via the SDK or SSE-C per bucket (WP-SSEC-1); Public = plaintext. After launch this lane goes first unless F-8 picks the consumer crowd. **Stripe-standard gap list (what 26 does not yet carry):** (1) test-mode tenants with distinguishable test keys and a sandbox S3 endpoint (26.1 names it, nothing scopes it); (2) a date-pinned API version from day one, not "at first breaking change" (26.2); (3) SDKs: Go/TS/Python listed, no decision on Rust/Java/.NET/PHP and no thin Private-mode client per language (26.6); (4) **a public status page — status.stored.cloud** (missing from 26 and 28.2); (5) **a changelog / deprecation feed** (missing); (6) a published error catalogue + rate-limit headers; (7) docs host = docs.stored.cloud, landing = stored.cloud/developers (26.5 named neither).
*Depends on: 5.11 (management API). The plan's internal references pin: 26.2 version translation, 26.4 CLI, 26.5 docs, 26.6 SDKs, 26.8 AI-native DX.*

### 26.1: API Sandbox
- Test-mode tenants (Stripe-style): fake backend, real API shapes, instant reset ; (2026-10-12) test keys carry their own prefix, requests to a test tenant are never billed and never leave the sandbox backend, a sandbox endpoint under s3.stored.cloud, data purged on reset — the gate before "resellers on top" (18.5) can be developed against
### 26.2: Full API Version Translation
*(2026-10-12)* pull the *pinning* forward: a dated version header accepted and echoed from launch, translation layers only when a second version exists; the version appears in the changelog (gap 5 above).
- Stripe-style version pinning for the management API (deferred from 5.11.1 — build at first breaking change)
### 26.3: Embeddable Elements
- Drop-in JS components: upload widget, file browser, usage meter (white-label ready for Phase 18); the same components are layer-4 widgets of the 42.3 customisation ladder (a sandboxed iframe on a second origin, postMessage SDK)
### 26.4: `stored` CLI/TUI ⚠ PULLED FORWARD to post-launch month 1 (see 90-Day Stack #3)
- (2026-10-12) default endpoint `s3.stored.cloud` (stored.ge accepted as alias); the CLI is the first client of the Private-mode SDK path — a per-bucket Private flag that does client-side encryption or SSE-C (WP-SSEC-1), never a "mode" the user picks per object
- `stored login/buckets/cp/usage/rclone-config/mount`; "just storage, no bloat" branding; spec in `docs/PRODUCT_FEATURES.md`
### 26.5: Developer Docs Portal — **docs.stored.cloud** (landing stored.cloud/developers; 2026-10-12, F-17). One portal for developers AND self-hosters (absorbs 15.4). Copy rules apply to every page: Private / Standard / Public; Standard = "encrypted at rest with keys we hold"; Private in outcome sentences; "zero-knowledge" forbidden until the external audit. The client-side tools guide (restic, rclone crypt, Kopia) is a page of this portal, not a marketing post.
- Built from the per-directory CLAUDE.md discipline + tested code examples (TIER_STRATEGY feature #8: every example pulled from passing tests)
- **Stripe-style docs auto-sync (Tiers 2-3; Tiers 0-1 land pre-launch in 5.15.6).** The property to reach: reference + code samples are *derived*, never hand-typed, so they can't drift. Prose (getting-started, FAQ) stays hand-written — Stripe hand-writes theirs too.
  - **Tier 2 — Generated per-user code samples.** Generate the aws-cli/boto3/rclone/curl/Go blocks from the OpenAPI spec + the logged-in user's *actual* key (reveal-once, copy buttons), so dashboard/docs snippets are genuinely runnable — not `YOUR_SECRET_KEY`. This is the "copy, paste, it works" moment customers feel. (~1-2 days.)
  - **Tier 3 — Spec generated from handlers.** True auto-derivation (swaggo/swag annotations, or generate the spec from handler tests) so the OpenAPI spec is never hand-written. Highest effort, awkward in Go, and the 5.15.6 Tier-0 drift guard already prevents the stale-reference failure mode — so **defer until docs volume justifies it**.
  - Pairs with 26.6 (SDKs generated from the same spec) and 26.7 (Scalar/Swagger explorer over the complete spec).
### 26.6: Management API SDKs — **(2026-10-12)** the "no S3 SDKs" deferred decision stands for plain S3 (AWS SDKs work), but Private mode needs a thin client library per language that wraps the AWS SDK with per-bucket SSE-C / the key core (WP-SSEC-1, 10.0.1) — scope it here; language list = the ones that matter to the developer crowd (Go, TS, Python first; Rust/Java/.NET on demand), generated from the same spec; every SDK example drawn from passing tests (26.5).
- Go + TypeScript + Python SDKs generated from the OpenAPI spec (S3 SDKs explicitly NOT built — see Deferred Decisions)
### 26.7: Interactive API Explorer
- Swagger/Scalar UI over the complete OpenAPI spec
### 26.8: AI-Native DX
- llms.txt (shipped), MCP server + plugin manifests as optional bonuses — "the CLI IS the AI integration"; an MCP server on Workers was proven 2026-10-03 (six tools; an agent provisioned a tenant and ran a probe) — Phase 40.7

---

## Phase 27: Go-to-Market Execution

> **Copy rules for every campaign (2026-10-12, F-15/F-16/F-17):** the brand is Stored, a product of FairForge; every post names **stored.cloud** and the endpoint **s3.stored.cloud** (stored.ge is an alias served forever — never the headline). Modes are Private / Standard / Public; Standard = "encrypted at rest with keys we hold"; Private is described in outcome sentences; **"zero-knowledge" is forbidden in any copy before an external audit**; "we cannot read it" is said only of client-side encryption (SSE-C, restic/rclone crypt, Private mode) and links to the guide at docs.stored.cloud. Trust claims point to trust.stored.cloud once the transparency log has its first signed statement.
*Runs continuously post-launch — sequence from `.private/TIER_STRATEGY.md` GTM section. 27.1 (legal docs) already shipped early as 5.14.2 (#254).*

### 27.2: LET/Reddit Launch (Week 1-4) — posts ready in `.private/launch/` — **(2026-10-12) re-sweep before posting:** stored.ge → stored.cloud, endpoint s3.stored.cloud, mode names per F-15, no "zero-knowledge"; blocked on WP-DOMAIN-1 (the 301 and the S3 alias must be live first).
### 27.3: B2 Migration Campaign (Week 4-8) — "B2 raised prices to $6.95. Here's faster for less." + rclone migration guide + calculator
### 27.4: Backup & Compliance Campaign (Week 8-16) — Veeam/MSP360/Duplicati guides, immutability/ransomware angle (needs 5.10.13 ✅ + 14.4)
### 27.5: Developer/SaaS Campaign (Week 16+) — OpenAPI/llms.txt, "file upload service in 10 minutes", honest comparison posts ; (2026-10-12) the "Stripe for storage" campaign: sandbox tenants, pinned API version, SDKs, status + changelog, idempotency and webhooks (the Phase 26 gap list) — links to stored.cloud/developers; "honest comparison" obeys the copy rules above
### 27.6: The LowEndTalk lineup (2026-10-03) — storage box, Vault, Edge Box, free Transfer/Paste/Links/Status, Edge Site, immutable backup target; the thread's checklist, communities and protocol truth table are in Phase 42.6; the VPS on-ramp is 39.7

---

## Phase 28: Enterprise Operations
*28.5 (data export) shipped early as 5.14.1 (#253); 28.1 evidence collection started in 5.14.3 (#255).*

### 28.1: SOC 2 Formal Audit ; (2026-10-12, F-16) the transparency log at trust.stored.cloud is a standing evidence source — admin metadata reads, key-release policy, escrow ceremonies, law-enforcement productions; the external audit here is also the gate that lifts the "zero-knowledge" copy ban
- Type I targeted Q4 2026, Type II observation H1 2027 (per 5.14.3); evidence tracker: `internal/compliance/soc2.go` + `.private/SOC2_EVIDENCE.md`
### 28.2: SLA Definition + Enforcement
- Published SLA (99.9%), credit policy, automated uptime accounting from health checks ; published at **status.stored.cloud** (2026-10-12, F-17 — the Stripe-standard status page, gap 4 of Phase 26), fed by the same uptime accounting and the synthetic check
### 28.3: Multi-Region Hub — **(2026-10-12, F-18)** the second-site candidate is the EPYC SEV-SNP box — the one attested data plane (41.4 / F-9): hub standby + PG replica AND the attested runner for compliance processing and the consumer recovery realm, so the DR site and the trust foundation are bought once.
- Hub replica (PG streaming replication) in second region; Cloudflare DNS failover; kills the single-server SPOF (RISK_REGISTER top item; see DISASTER_RECOVERY.md §1 hub-standby as the interim step)
### 28.4: DR Drills + Runbooks
- Quarterly restore + failover drills per `DISASTER_RECOVERY.md`; PG restore test is also a 5.15.4 checklist item

---

## Phase 29: Ecosystem Integrations
*Demand-ordered — each integration is also a marketing channel.*

- Veeam Ready certification; MSP360/Duplicati/Arq/Kopia/restic guides (backup vertical from 27.4) ; (2026-10-12) restic, Kopia, Duplicati and rclone crypt encrypt client-side — their guides double as **the client-side tools guide** at docs.stored.cloud, the one place "we cannot read it" is said before the audit; every guide names s3.stored.cloud
- Nextcloud/ownCloud external-storage integration
- Managed Ente: Ente's 60 MB server in a Cloudflare lite Container per tenant with stored.ge as its S3 (40.6, ≈ $2/mo of compute + storage); Immich stays a VPS workload (8 GB RAM) with stored.ge as its storage *(2026-10-12: stored.pics → stored.cloud/photos (F-17); the Cloudflare-Container run was stopped 2026-10-11, data kept in R2; if revived, Ente's server per tenant with s3.stored.cloud as its S3 — the consumer-surface lane, Private default only after the recovery vault, F-19.)*
- WHMCS module (with 21.5) for the reseller channel
- Terraform provider + Pulumi for bucket/key management
- Zapier/n8n triggers on the 5.11.6 event log

---

## Phase 30: Platform licensing & enterprise tier (stored.cloud/platform)

> **Re-scoped 2026-10-12 (F-17; overrides the MASTER_PLAN Phase 2 "stored.cloud enterprise brand"):** there is one brand, Stored, at one domain, stored.cloud — it is NOT a second brand or an enterprise tier. The enterprise SLA tier is a tier of Stored (sold from stored.cloud, served on s3.stored.cloud); the platform/licensing product lives at stored.cloud/platform. Domain consolidation itself is WP-DOMAIN-1 (5.14.13, before Oct 31), not this phase.
*The MASTER_PLAN endgame: Phase 2 (stored.cloud enterprise, $15-50/TB, 99.99% SLA) and Phase 3 ("Stripe for Storage" platform licensing: Starter $500/mo → Enterprise $5,000/mo). Everything in Tiers 1-4 is a prerequisite; this phase is packaging, not new engine work.*

- Enterprise SLA tier of Stored on the same engine (multi-region from 28.3 — the second site is the EPYC SEV-SNP box, F-18; IAM from 19; compliance from 5.14); sold on stored.cloud, no second brand (2026-10-12) (multi-region from 28.3, IAM from 19, compliance from 5.14)
- Self-hosted/white-label licensing of the Vaultaire engine at **stored.cloud/platform** (builds on Phase 15 distribution + 18 multi-tenancy); the licensed image includes the attested data-plane build of 15.5 / 41.4 so a licensee can publish the same measurements to its own transparency log (2026-10-12) (builds on Phase 15 distribution + 18 multi-tenancy)
- Enterprise sales motion: RFP templates, security questionnaire pack (SOC 2 from 28.1), custom contracts

---

# ADDITIONS 2026-10-01 — Phases 31–39 and the sub-steps the consolidated plan never carried

*Added by owner decision (see FULL-PLAN TRACK). Sources: `.private/VAULTAIRE_MASTER_PLAN.md` step numbers given per phase, and for Phase 31 the owner's request of 2026-10-01. Everything here is **scoping text**: each phase gets a re-scope docs PR against the code before its first worker prompt (rule 3 of the track). "Today" lines say what exists in the code on 2026-10-01.*

---

## Phase 31: Smart Programmable Buckets
*Requested by the owner 2026-10-01. Sources: master plan "Smart Bucket Architecture" (per-bucket pipeline config, passthrough mode, algorithm registries); `.private/PHASE_5_10_PREP.md` ("bucket notifications — foundation for smart buckets"); the "Stripe for Storage, programmable buckets" goal. Stands on: Stage 1 (WP-R8-4, WP-R3-1), WP-R13-3 job scheduling, WP-R11-3 webhook outbox. **Decision F-1 (2026-10-01): rules and WASM functions are built together, so the Phase 22.1 runtime is built inside this phase.** Build order: 31.1 profile → 31.2 + 31.3 rules and runtime (lifecycle XML as the first front-end) → 22.1 WASM runtime → 31.6 functions → 31.4 dashboard → 31.5 recipes → 31.7 bucket as code.*

**The idea in one line:** a bucket carries a *program* — a **profile** that says how its bytes are processed and placed, and **rules** that say what happens to its objects and when — set from the dashboard, the management API, the CLI, or plain S3 calls.

**Today:** the profile exists as scattered switches — `buckets.tier_preference`, `sse_enabled`, `region`, `visibility` → R2, `storageClassDisablesChunking`, the `chunking` flag, the 64 MiB chunking threshold — resolved in several places (`resolvePutStorageClass`, `willChunkEncrypt`, `bucketRegionDriver`). The rules exist as three unrelated features: bucket notifications (5.10.12; prefix/suffix filters are not persisted — WP-R4-4), per-bucket inventory and access logging (5.14.9), and the server-wide multipart reaper. There is no lifecycle (Phase 12) and no way to say "do X to objects matching Y".

### 31.1: Bucket pipeline profile ("smart")
- One versioned JSON document per bucket (`buckets.pipeline_profile`): chunking (on/off, minimum object size), dedup scope (off / tenant / global), compression (off / zstd level), encryption (none / SSE-S3 / convergent / client E2EE marker), placement (floor, backend, region, and the erasure scheme once Phase 11 exists), passthrough (no processing at all).
- Presets a customer picks instead of writing JSON: `standard` (today's behaviour), `backup` (dedup + zstd, restic/borg-aware routing from V18.9), `archive` (whole objects to tape), `media` (no compression, large chunks), `passthrough` (the master plan's "HPC" bucket), `public` (R2).
- One resolver, `resolveBucketPipeline`, replaces the scattered checks; the existing columns become views of the profile. A profile change affects **new writes only** — manifests are self-describing, so old objects keep reading.
- Algorithm registries (chunker / compressor / encryptor interfaces) are built only when a second implementation exists; one implementation behind a registry is the scaffolding the review deleted.

### 31.2: Bucket rules ("programmable", declarative)
- A rule = **trigger + filter + action**, stored per bucket, tenant-scoped.
- Triggers: object created / deleted / restored / read (the 5.11.6 event types), a schedule (daily), a threshold (bucket bytes, object age, egress).
- Filters: prefix, suffix, tags, size range, content type, age, storage floor.
- Actions: expire, abort incomplete uploads, move to a floor (downstairs ↔ attic), tag, copy to another bucket of the same tenant, apply retention / legal hold, request a restore, send a webhook (through the WP-R11-3 outbox), refuse the write (per-prefix size or type limits).
- **Phase 12 is this engine's first front-end:** S3 lifecycle XML (`PUT ?lifecycle`) is parsed into rules (Expiration, AbortIncompleteMultipartUpload, Transition → floor moves) and read back from them. `PUT ?notification` becomes the second front-end. One rule table, no parallel lifecycle engine.

### 31.3: Rule runtime
- Event-triggered rules run from the event stream; scheduled ones from a daily runner in the `job_runs` shape (catch-up at boot, one advisory lock, admin trigger, metrics).
- Every rule has a **dry run**: "would affect N objects, X TB" (Phase 12.2's preview) before it is enabled, per-run caps, idempotent actions, an audit row per action batch.
- Object Lock always wins; a rule can never touch another tenant's objects; deletes go through the same path as S3 DELETE (quota release, version ledger, chunk refs).

### 31.4: Dashboard — the bucket's program page
- Profile picker (presets with what each costs and does), rule builder without XML, dry-run result, run history, in the house language (downstairs / attic).

### 31.5: Recipes
- One-click bundles of profile + rules: "backup bucket" (backup profile, weekly verification report 14.4, keep 30 daily / 12 monthly), "media library", "log bucket" (expire after 30 days), "camera roll", "static site". Each recipe is a document a customer can read, export and edit.

### 31.6: Functions (built with the Phase 22.1 runtime — decision F-1)
- A rule action may be a WASM function: transform on upload (thumbnail, transcode probe, metadata extraction, virus scan), validate (reject on schema), or a customer-supplied module. Sandboxed, CPU/memory/time limits, per-invocation metering, results to object metadata or a sibling key. This is where Phases 22.1, 22.2 and 22.3 land. **Runtime: wazero** — pure Go, no cgo, already in `go.mod` — so the product stays one static binary; Phase 22.1 and the master plan say Wasmtime, which needs cgo. (`internal/drivers/wasm.go` is an unlinked driver stub, not this runtime; D-2 still deletes it, but `wazero` now stays in `go.mod`.) Customer-supplied modules are the dangerous half: no host filesystem or network unless a rule grants one named bucket, hard CPU / memory / wall-clock limits per invocation, a per-tenant invocation budget, and the module's bytes stored as an object the tenant owns. **Where a function runs (40.5):** on the hub (wazero) for objects on Vaultaire backends; at the edge (a Worker) for R2-held objects and per-request transforms — one rule format, two runtimes.

### 31.7: Bucket as code
- `GET|PUT /api/v1/manage/buckets/{name}/program` (profile + rules as one JSON document, in the OpenAPI spec), `stored bucket program get|apply` in the CLI (26.4), YAML export/import so a bucket's behaviour can live in a repository.

**Test**: create a bucket from the `backup` recipe → upload → the profile's dedup and compression show in the bucket's stats → add a rule "expire `tmp/` after 7 days", dry-run names the right objects, the daily run deletes them and releases quota → the same rule set via `aws s3api put-bucket-lifecycle-configuration` reads back identically in the dashboard → a locked object is skipped and reported → change the profile and confirm old objects still read.

---

## Phase 32: Enterprise Identity and Split Keys
*Master plan steps 305, 331–340 (split key management), 371–374. Stands on: Phase 18.1 (organisations), Phase 19.*

**Today:** dashboard login is password + TOTP, or Google / GitHub OAuth. The SAML, LDAP, Active Directory and service-account files were unreachable scaffolding and were deleted in R0 (#473).

- **32.1** Generic OIDC single sign-on for an organisation (issuer, client, domain claim), with the R12 rules kept: MFA honoured, sessions revoked on reset.
- **32.2** SAML 2.0 service provider.
- **32.3** LDAP / Active Directory bind and group sync.
- **32.4** SCIM provisioning and organisation member management.
- **32.5** Service accounts (non-human principals with scoped keys and their own audit actor).
- **32.6** Split keys — **re-read 2026-10-12:** server-side quorum keys (split API keys two holders must combine; Shamir-shared tenant keys reconstructed on read) are a **Standard-mode** control — the server reconstructs, so the copy stays "encrypted at rest with keys we hold"; the same sss machinery (age-plugin-sss) is what F-14's ceremony escrows, and every reconstruction is a 43.6 entry; "can't read" split keys exist only client-side — shares held by the customer's own holders, combined in the SDK key core (K2); SSE-C per bucket (WP-SSEC-1) is the no-SDK path. As written 2026-10-01: split API keys (two holders must combine), and Shamir-shared encryption keys with a per-bucket policy, reconstructed on read. Stands on WP-R8-1.

**Test**: an organisation signs in through OIDC and SAML test providers → a deprovisioned SCIM user loses sessions and keys → a split-key bucket cannot be read with one share.

---

## Phase 33: Operations Platform
*Master plan steps 361–400 and 401–450. Stands on: Phase 15, Phase 28.3.*

**Today:** one box, systemd, HAProxy, Prometheus + Alertmanager + ntfy, auto-rollback deploys. `internal/{k8s,ha,slo,container,global}` are unlinked scaffolding (D-1: delete, then build what is needed linked).

- **33.1** Distributed tracing (OpenTelemetry) through API → engine → driver; one request id end to end (WP-R1-8).
- **33.2** SLO / SLI tracking with burn-rate alerts, feeding the published SLA (28.2).
- **33.3** Blue-green and canary deploys across the hub and its standby; a deploy never drops an in-flight upload. — the standby is the **Stage-5 EPYC with SEV-SNP exposed** (F-18: standby + PG streaming replica + backup target + one TPM escrow share of F-14; the attested plane of 41.4 later), never a second 9950X; DNS = the WP-DOMAIN-1 map (stored.cloud canonical; stored.ge, stored.pics permanent aliases).
- **33.4** Infrastructure as code for the box, HAProxy, Prometheus rules and DNS.
- **33.5** A nightly suite that really runs: chaos (backend down, DB down, disk full), soak, spike — the one R15 deleted reported green while every step failed.
- **33.6** Kubernetes operator and autoscaling beyond the 15.3 Helm chart — only once there is more than one hub node.
- **33.7** Escalation policy and on-call runbooks.

---

## Phase 34: Advanced Networking
*Master plan steps 811–910. Stands on: Phase 20 for the peer-to-peer items.*

**Today:** HTTP/1.1 and HTTP/2 through Cloudflare and HAProxy; IPv4; no peer-to-peer code or dependency.

- **34.1** HTTP/3 (QUIC) at the edge first, in the hub and spoke agents later.
- **34.2** IPv6 dual stack (listener, allowlists, `clientip`, rate limiters).
- **34.3** CDN integration layer: cache purge on overwrite/delete, origin configuration, adapters beyond Cloudflare, per-CDN cost and analytics. Stands on 40.1; purge-on-delete is what R2 Sippy needs until WP-R2-1 makes keys immutable.
- **34.4** Peer-to-peer foundation: libp2p, Kademlia DHT, NAT traversal, gossip (shared with 21.4).
- **34.5** Content routing: content addressing, provider announcements, multi-path.
- **34.6** Mesh topology management and healing.
- **34.7** Bandwidth optimisation: traffic shaping, multi-path aggregation, adaptive streaming.
- **34.8** BGP anycast — needs an ASN, IP space and at least two points of presence. The owner holds an ARIN registration (2026-10-03): the path is an ASN (issuance fee waived since 2024; needs a multihoming or unique-routing justification), a /24 leased (≈ $130–250/mo) or bought (≈ $8–9k + ARIN's recipient fee; the waitlist runs years), a /36–/40 of IPv6, the 3X-Small plan fee (≈ $275/yr), RPKI ROAs and IRR route objects (transit rejects announcements without them), reverse-DNS delegation so customers set PTRs from the panel, SWIP reassignments automated through Reg-RWS, a monitored abuse mailbox; announced BYOIP from the Salt Lake box's provider or a BYOIP-friendly host. The VPS on-ramp decision is 39.7.
- **34.9** Network telemetry and learned path selection.
- **34.10** Network security: DDoS posture, geo-blocking, intrusion detection, automated blocking.

---

## Phase 35: Blockchain, Proofs and Advanced Cryptography
*Master plan steps 911–1000; extends Phase 25. Stands on: 10.6 (Merkle challenge machinery), Phase 21 (credits) for the economic parts. **35.3–35.5 need the legal review of master-plan step 1010 before any code** (decision F-4).*

**Today:** nothing; Phase 25 is four lines.

- **35.1** Publicly verifiable storage proofs (challenge generation, proof verification, monitoring).
- **35.2** On-chain anchoring of proofs and immutability certificates (pairs with V18.5).
- **35.3** Token model, staking, reward distribution.
- **35.4** Storage market: bid/ask, price discovery, escrow, dispute resolution, reputation.
- **35.5** Decentralised governance: proposals, voting, timelock, multi-sig treasury.
- **35.6** Zero-knowledge proofs of possession.
- **35.7** Homomorphic encryption — **verdict written 2026-10-03 (Phase 41):** FHE inference is narrow (small models, seconds per inference, a rented GPU box); the practical paths are queryable encryption (41.1), PIR lookups (41.3) and the attested data plane (41.4, self-hosted SEV-SNP). Ranked full-text relevance over encrypted prose and an LLM over encrypted documents remain enclave-only.
- **35.8** Decentralised identity: DIDs, verifiable credentials, wallet login.
- **35.9** Web3 gateway: IPFS pinning and gateway for public buckets, ENS, cross-chain storage API.

---

## Phase 36: Hub-to-Hub Federation
*Master plan steps 1061–1100. Not Phase 20: that is one hub with spokes; this is independent Vaultaire hubs sharing data. Stands on: Phase 15 (a second hub must be installable), Phase 19 (permissions for foreign users), Phase 20.*

- **36.1** Server-to-server protocol: versioned API, `/version`, OpenAPI specification, a multi-hub test environment.
- **36.2** Hub identity and trust: key exchange, DNS TXT verification, signed requests, key rotation, trust and block lists, a shared blocklist protocol, S2S rate limits.
- **36.3** Federated user identity (`@user:hub-domain`).
- **36.4** Federated sharing: share request, accept / reject, permissions on local resources, updates, revocation, remote shares in the dashboard.
- **36.5** Inter-hub data transfer (direct, or brokered peer-to-peer through 34.4).
- **36.6** Discovery: a central directory first, peer-to-peer later; caching of remote metadata; eventual consistency of shared state.
- **36.7** Abuse and spam handling across the federation; diagnostics for S2S traffic.

---

## Phase 37: More Backends and Backend Orchestration
*Master plan steps 1101–1140, 181–190. Stands on: WP-R6-1, WP-R6-3, Phase 6.6.*

**Today:** eight drivers behind `engine.Driver`; placement is by bucket tier and header; the circuit breaker is the only routing signal. The selector, cost optimiser and health scorer were constructed and never consulted, and were deleted in #524.

- **37.1** SeaweedFS backend (the own-fleet candidate beside MinIO; decides 6.6's software).
- **37.2** CORVAULT backend (Redfish API) — needs the hardware.
- **37.3** DeepSeek 3FS backend (FUSE, RDMA-aware) — needs the hardware.
- **37.4** Orchestration, rebuilt wired: workload detection (small files, large files, streaming), a per-backend cost engine, policy routing rules per tenant and per bucket (the profile of 31.1 is where a customer states them), health score as a routing input. Each rule lands with a test proving it changes a routing decision.
- **37.5** Backend migration tooling: move a tenant or bucket between backends under the etag guard smart demotion uses.

---

## Phase 38: Dedup Write-Avoidance and Audit
*Master plan steps 1205, 1207–1208, 1214–1219, 128. Stands on: 10.6 (mandatory for 38.1), the client SDK (10.0) or CLI (26.4), WP-R8-2, WP-R8-3.*

**Today:** server-side chunking, GCI, GC with reconcile, the admin dedup page, per-chunk SHA-256 on read. A client always uploads every byte.

- **38.1** Client-side dedup check (`POST /v1/dedup/check`: the client chunks, sends hashes, uploads only new chunks) — only with proof-of-ownership, never across tenants for encrypted data. **(2026-10-12)** Dedup is within-space convergent only (F-12); the opt-in public-artefact namespace is the one cross-space case; the 41.3 PIR check is research behind F-3's measurement; never a global convergent key — and 38.4's audit never compares `plaintext_hash` across spaces (43.1 names it a cross-tenant equality oracle).
- **38.2** Bandwidth-savings accounting and a per-tenant dedup analytics API.
- **38.3** Scheduled chunk verification: sample, re-hash, repair from another copy (shares machinery with P5 and 14.4).
- **38.4** Monthly consistency audit: every GCI row has a blob, every manifest ref has a row, ref counts match, no orphan blobs (extends WP-R8-2).
- **38.5** Bloom filter / external cache for negative lookups — only if measured lookup latency asks for it (the Phase 8 note).
- **38.6** Delta encoding between versions of an object — after WP-R2-1 makes versions real bytes.

---

## Phase 39: Business Operations (owner track)
*Master plan steps 1001–1060. Runs alongside from Stage 0; mostly not code.*

**Done:** terms, privacy, AUP, DPA, BAA, GDPR pages (5.14.2); FAQ; launch posts; the in-app support view and abuse queue.

- **39.1** Legal: counsel review of the published documents; NDA, contractor and enterprise agreement templates; the F-4 review.
- **39.2** Product: customer interviews, personas, public feature voting, a public roadmap.
- **39.3** Support: helpdesk, canned responses, runbooks, hosted status page (5.14.12), community forum or Discord with moderation rules.
- **39.4** Billing operations: refunds and disputes in the admin dashboard, dunning, revenue analytics, Stripe Tax, bookkeeping.
- **39.5** Developer ecosystem: docs portal (26.5), SDKs (26.6), tutorials, a showcase page.
- **39.6** Platform sales motion (stored.cloud/platform; one brand, Stored): CRM, deck, security questionnaire pack (with Phase 30).
- **39.7** The VPS on-ramp decision (2026-10-03): whether to sell KVM at all (≈ 20% margin and the most support load — most LowEndTalk operators run it to acquire customers who then buy storage) or route VPS-shaped demand to the WHM reseller account and the Salt Lake box with the edge panel, status page and looking glass in front; on the box Proxmox VE, a panel over its API (Forge), noVNC, templates and rescue mode, Proxmox Backup Server with Vaultaire as the datastore, per-VM bandwidth into Analytics Engine, DDoS filtering from the upstream (Magic Transit is Enterprise-only), NetBox for IPAM with SWIP automated. Network prerequisites in 34.8. Economics on one 9950X box: $60–110 for the box, ≈ $200 for a leased /24, $275/yr ARIN; forty VPS at $4 is $160 — yearly prepay and dense packing or nothing.

---

## Phase 40: Cloudflare Edge Plane
*Added 2026-10-03 from the owner's Cloudflare build-and-bench session (write-ups `~/fairforge/CLOUDFLARE-BENCH-2026-10-03.md` and `~/fairforge/CLOUDFLARE-SHOWCASE-2026-10-03.md`; code in `~/fairforge/edge-showcase/` and `~/fairforge/hearth/cloudflare/`, outside this repository) and the 2026-09-26/27 B2 + Workers evaluation (`.private/TIER_COMBO_MATRIX_2026-09-27.md`). Stands on: 5.10.6, 5.11.12, scoped keys (5.11.4), WP-R2-1 or purge-on-delete (34.3) for 40.1(b), 15.1 for 40.6. Stage 2 for 40.1–40.4 and 40.6–40.9; 40.5's edge runtime waits for real edge traffic.*

**The principle — use Cloudflare to its maximum, never as the vault.** Cloudflare and the Vaultaire plane have opposite cost shapes: R2 is $15 per TB at rest with free egress; our backends are $2–4 per TB at rest and every byte that transits the Salt Lake box costs bandwidth (660 TB/mo on one box). So: **bytes at rest on Vaultaire, serving on Cloudflare.** Write once, promote on read, expire; **pay per upload, never per view.** Everything below ran on the production Cloudflare account on the $5 Workers Paid plan on 2026-10-03, measured from El Paso.

**Proven:** R2 presigned PUT/GET, a 4-part presigned multipart (50 MB in 1.8 s), bucket CORS (none of which the dead Quotaless backend could do); a Worker hydrating OneDrive → R2 on a miss (cold 1.7 s, then 150 ms, 3 ms CPU per cold pull); **R2 Sippy pulling from a local Vaultaire over a Quick Tunnel with the OneDrive fleet as the primary** — Vaultaire's SigV4, HEAD and GET accepted as-is, cold 852 ms, warm 240–400 ms; Sippy from B2 (cold 0.8 s); WASM image resize in a Worker (67 ms, 15 ms on a cache hit — Paid only: the Free plan's rolling per-isolate CPU budget kills it); a streaming ZIP from R2 (61 MB in 2 s); D1 (1,000-row batch in 166 ms, point reads 12 ms), KV, Durable Objects (44 ms warm), Queues (500 messages in 380 ms), Workflows, Cron Triggers, R2 event notifications reaching a queue consumer 4 s after the PUT, D1 read replication; **Vaultaire itself as a 256 MiB `lite` Container** (54 MB alpine image, cold 5.4 s, warm 150 ms); Browser Rendering; Workers AI (Flux, LLaVA, bge, Llama, Whisper, Aura) with Vectorize search in 41 ms; an MCP server; Turnstile; the rate-limiting binding. Worker TTFB 105 ms against 670 ms for the Croatia cPanel box. **Still gated:** zone-level features on the bench token (Email Sending, Cache Reserve, custom hostnames, Access) — the stored.ge zone has them; Dynamic Workers (closed beta); Workers for Platforms ($25/mo).

### 40.1: The 0×-SLC serving plane (cdn v2)
- **Today:** `/cdn/{slug}/{bucket}/{key}` (5.10.6) and every S3 GET transit SLC; `cdn.stored.ge` has no proxied DNS yet (Cloudflare readiness [YOU] item 4). Three builds, cheapest first, chosen per bucket class:
- **(2026-10-12, WP-DOMAIN-1) Public customer content never sits on the brand origin:** public buckets, `/cdn` and share links are served from a separate **non-brand content domain** (a second zone, never a subdomain of stored.cloud), so uploaded HTML/SVG/JS can never run with a stored.cloud origin or cookie — the R4 inline-HTML class ends by construction, not by `Content-Disposition`; `cdn.stored.ge` 301s to it. The API endpoint is **`s3.stored.cloud`**; `s3.stored.ge` is served forever as an alias (both stay valid SigV4 hosts; (a)/(b) presign on `s3.stored.cloud`). 40.2's per-customer hostnames are the customer's own names, unchanged, and CNAME the content domain.
  - (a) ~~**Cache Reserve** in front of `cdn.stored.ge` — one Cache Rule, keeps objects by access not age, works against any origin, no code; objects over 512 MB are not cached on self-serve plans.~~ **Struck 2026-10-05:** Cache Reserve is refused on the Free zone (API 1135, "not available for your plan type"; the dashboard's button is the Smart Shield upsell, Enterprise). **Private reads go through a Worker + Workers Cache instead** (launched 2026-07-06, any Workers plan, no storage fee, tiered, request-collapsing, 512 MB per object): a gateway Worker verifies the caller's scoped key or STS token against the origin, calls a cached entrypoint whose cache key carries the key id in `ctx.props` (one partition per key — per tenant once an introspection call tells the gateway the tenant), the entrypoint presigns the object on `s3.stored.ge` and fetches it whole; purge by tag from the cached entrypoint only. Measured 2026-10-05 against the prod origin (`docs/CLOUDFLARE.md` § Findings, evening): hit TTFB 0.16–0.20 s from the box, 0.4 s from a home link; 20 MiB hit 29 MB/s, 300 MiB hit 56–72 MB/s (miss 20 MB/s); an object above 512 MB is BYPASS and streams through the Worker at 50 MB/s every time; a `Range` is sliced in the gateway, which reads through the cached body to the offset (1 KiB at 150 MiB = 2.2 s, at the tail of 300 MiB = 7 s) — cache in fixed blocks (`?block=N`, one origin range per block) before this serves video. The 100 MB request-body limit is the zone plan; the first HEAD of a new key with a static-looking extension was turned into a GET by the edge cache (SigV4 mismatch, 403) until the 2026-10-05 cache rule bypassed the cache for everything but `/cdn/` and the static paths.
  - (b) **R2 + Sippy**: an R2 bucket whose source is `https://s3.stored.ge/<bucket>` with a scoped read-only key per bucket; R2 serves on a hit and streams-and-stores on a miss; a 7–30-day lifecycle rule keeps only the working set. Expiry is by age since upload, not last access, and **Sippy never propagates deletes or overwrites** — so either immutable content-addressed keys (WP-R2-1) or purge-on-delete (34.3) comes first. **Proven 2026-10-05:** after the lifecycle rule expires a key the listing drops it and the next GET re-pulls it from the origin (20 MiB in 1.2 s, byte-exact) — and that re-pull is how an overwrite finally reaches R2, so the lifecycle window bounds staleness; a key whose first pull failed answered `InternalError: Sippy upstream ETag changed during read` for hours in that test (2026-10-09 re-test: failed pulls of unchanged objects resume on the next view — see (e)); a pull counts once per *concurrent* miss on the origin's egress meter (two viewers of the same uncached 50 MiB key 4 s apart = two origin reads), once the copy exists it is free; objects above ~200 MiB are pulled as parallel 200 MiB parts whose GETs sign an If-Match the client never sends — Vaultaire now verifies that shape (#588); the copy of a 300 MiB object lands within the first viewer's read (37 MB/s from the box), a 2 GiB object's **never** — R2 abandons each 200 MiB part after ~29 s at the ~33 MB/s its puller takes from any origin, retries nothing and leaves an in-progress multipart upload behind (abort them by lifecycle): Sippy is for objects up to a few hundred MiB, larger media reaches R2 by direct upload or a Worker copy.
  - (c) **307 to a presigned backend URL** for Performance-tier reads (the presigned-redirect lever of the tier matrix: 0× SLC) with the SDK 307-compatibility matrix.
  - (d) **DR read plane** (after Phase 11): a Worker (objects under ≈ 40 MiB, the 128 MB isolate) or a container holding the shard map and read-only vendor keys reconstructs an erasure-coded object from Lyve / B2 / OneDrive / Geyser directly when the SLC box is down — RS decode runs at 1.2–3 GB/s per core (`bench-results/ERASURE-2026-10-03-codecs.md`), so a 64 MiB object is ≈ 0.1 s of CPU. Hot objects already in R2 keep serving; this closes the cold-miss gap.
  - (e) **The DO copier — large objects into R2 without Sippy (proven 2026-10-09, `docs/CLOUDFLARE.md` § Findings 2026-10-09).** Sippy gives up above a few hundred MiB (a 2 GiB object never landed, 2026-10-05). A coordinator Durable Object creates the R2 multipart upload, hands each child DO its own parts (ranged presigned GETs on the origin → `uploadPart`), completes when all report, and runs from DO alarms so a client disconnect or a failed part is retried. Measured against the prod origin: one DO streaming one GET into `R2.put()` landed 2 GiB in 57 s (~38 MB/s; one outbound fetch lived 13.8 min); 8 child DOs landed **5 GiB in 39 s (138 MB/s)** and 2 GiB in 17–29 s; every copy SHA-256 byte-exact, zero retries; ≈ $0.20 of Cloudflare usage per TiB copied. Rules: ≤ 3 parts in flight per DO (the six-connection limit: each part is a GET and an upload), multipart only at ≥ 5 GiB (a single `put()` is refused *after* the whole body streamed), a watchdog alarm (a DO stalled once after a connection-limit failure), set `limits.subrequests` explicitly (default 10,000). Keyed by bucket + key + ETag, so one copy runs per object however many viewers miss at once (Sippy pulls once per concurrent miss) and an overwrite is a new copy, never a stale one. Stored's own per-stream rate (24–48 MB/s) is the ceiling of one stream, so the fan-out is what makes it fast.
  - **Sippy stays, as the small-object path and the fallback** (owner decision 2026-10-09). **Measured the same evening on the same objects** (origin bytes from Stored's own access log): Sippy lands on the first view up to 500 MiB (7/7), 600–650 MiB 2/3, 750 MiB–1.5 GiB 0/6 — but a failed key is NOT poisoned (that 2026-10-05 reading was wrong): the next view resumes the in-progress multipart and every failed key landed by view 2–3. Sippy reads the origin once below 200 MiB, **twice for 200–650 MiB** (the viewer's stream plus the 200 MiB parts) and 3–5× above, and once more per concurrent cold viewer (1/2/5 viewers = 2/3/6×); the copier reads it once whatever the viewers (5 triggers → 1 job). Every origin read counts on the tenant's egress meter (WP-R10-9), so above 200 MiB Sippy charges the allowance at least twice per promotion. First viewer: Sippy TTFB 0.8–2.5 s while it copies; the copier serves nobody but lists 300 MiB in 6.6 s, 1 GiB in 10 s, 5 GiB in 40 s. Cloudflare usage per 1,000 objects of 300 MiB: Sippy ≈ $0.02, copier ≈ $0.07 — both ≈ 1 % of the same objects' first month of R2 storage, so cost does not decide it. **Split of record: Sippy below 200 MiB; 200–512 MiB Sippy, with a copier push at upload for hot objects or tenants near their egress allowance; the copier above 512 MiB; Sippy configured on every plane bucket as the fallback; a 1-day abort-incomplete-multipart lifecycle rule on Sippy buckets** (a failed pull leaves one behind).
  - Per class: PUBLIC → (b), objects above Sippy's ceiling via (e); Standard → (a); Performance → (c); Vault never — tape stays out of the read path (40.4). B2 as a direct origin (free egress to Cloudflare, no 3× pool) stays the "Performance Unlimited" SKU of the 2026-09-26 verdict.
- **Verify on the Vaultaire side with a real R2 bucket before relying on it:** SigV4 from Cloudflare's IPs against `s3.stored.ge`, Range GETs for objects over 199 MiB, ETags from the HEAD cache accepted by Sippy, and the egress accounting of a promotion (WP-R10-9 counts it once per expiry window, not once per viewer — the FAQ says so).
- **The hot-egress trap:** a public bucket serving 50 TB a month from R2 costs the working set's storage, not bandwidth — cap "public CDN bandwidth" per plan (5.11.12 budgets) and sell overage at $0.01 per GB so nobody can hurt the margin.
- Metrics: promotions per bucket class, bytes served per plane, SLC transit avoided; an alert when the plane's miss rate says the lifecycle window is wrong.

### 40.2: Per-customer CDN hostnames
- Cloudflare for SaaS: `cdn.theirdomain.com` → the customer's public bucket with TLS issued automatically; 100 hostnames free, $0.10 each after; apex and wildcard hostnames are Enterprise-only (document it). Fallback origin = the 40.1 plane on the content domain. The same mechanism is 18.4's white-label domain and 18.9's custom-domain SSL.

### 40.3: The media pipeline rule
- **Originals never touch a Worker** (128 MB per isolate; cameras shoot 24–45 MP). One 2,048 px mezzanine per image, made once at upload — in the browser with wasm-vips (WordPress 7.1's path; libheif in WASM also ends the HEIC black-pixel class) or in a Container at active-CPU rates — then every derivative from the mezzanine in a Worker with Photon or jSquash behind the Cache API (≈ $4 per million resizes in CPU; Cloudflare Images would be $500 for the same million). Video: Container ffmpeg at upload → HLS segments in R2 (cents per hour of footage, free delivery); Stream ($5 per 1,000 minutes stored) only for adaptive-bitrate ladders at wedding-film volume. ZIP: assembled in the browser (client-zip) streaming each file from a presigned URL, or in a Worker with a WASM CRC above 1 GB. A Private space's thumbnails are device-only (41.2); a Vault-tier (tape) bucket in Standard mode gets them at upload like any other. These are Phase 31.6's built-in functions, placed at the edge by 40.5.

### 40.4: Restore as a workflow
- Today a read of an attic object answers 503 + `Retry-After` while the recall runs (WP-R13-1). On the edge plane a Workflow starts the Geyser restore, polls, copies the object into R2 or Cache Reserve, and Email Service sends the link; the dashboard shows the queue. The 200-second recall becomes a feature, and tape is never in a synchronous read path.

### 40.5: Where a function runs
- One rule format (31.2), two runtimes: **wazero on the hub** for objects on Vaultaire backends (31.6); **a Worker at the edge** for R2-held objects and anything that must answer per request. Customer-supplied code at the edge needs the isolation Cloudflare sells as **Dynamic Workers — an OPEN beta on Workers Paid since 2026 (1,000 unique Workers a month included, then $0.002 per Worker-day; a Worker loaded from a code string with a CPU limit and only the bindings handed in; corrected 2026-10-05, the plan said closed)** or Workers for Platforms ($25/mo, 1,000 scripts, an Outbound Worker in front of every fetch — which does not see fetches made from a Durable Object: never hand customer code a DO binding). Two interim mechanisms were proven: QuickJS compiled to WASM inside our Worker (separate heap, instruction-count CPU cap, no fetch, no env — an infinite loop stopped in 0.47 s; not a V8 isolate boundary) and per-tenant scripts deployed as ordinary Workers through the API (647 ms, a `cpu_ms` limit, own hostname, zero bindings; 500 scripts per account). **2026-10-09:** a request into a Durable Object may now run 10 dynamic workers at once (a plain Worker request still 4; measured) — a tenant's functions live in the tenant's DO. Build 31.6 on the hub first; the edge runtime when 40.1 carries real traffic.

### 40.6: Vaultaire in a Container, and managed self-hosted apps
- The 15.1 image runs as a Cloudflare `lite` Container (proven; it needs Hyperdrive to a Postgres for keys and head rows). Uses: a second serving hub for EU residency without a second box (after WP-R6-1, because reads must be location-authoritative); the platform's "the engine next to your data" SKU (Phase 30, stored.cloud/platform); **managed Ente** — Ente's 60 MB server in a lite container per tenant at ≈ $2/mo of compute plus Stored storage, a product nobody else can price; Immich stays a VPS workload (8 GB RAM, four cores) with stored.ge as its S3. **Re-measured 2026-10-09 on the native `ctx.container` API (`scheduling_policy: "durable_object"`): cold start ≈ 0.26 s inside the DO (≈ 0.55 s end to end), wake ≈ 0.3–0.5 s; an idle container stops 20–25 s after its last request — SIGTERM, then SIGKILL ≈ 5 s later even when PID 1 ignores SIGTERM (the 2026-10-06 "never sleeps, bills forever" finding no longer holds on this API), so a graceful shutdown must finish in < 5 s; no `max_instances` under this policy (cap instances in code); 20 GB disk needs ≥ 3 GiB memory; snapshots restore a small delta in ≈ 0.3 s, 3 GiB in ≈ 29 s. The `Container` class is maintained only to 2026-12-31 — migrate the photos surface's `ente-api` (stored.pics → stored.cloud/photos, F-17) (shorten its Postgres shutdown first).** Honest limits of a container: no public IP, no persistent disk (snapshots in beta), no UDP, outbound port 25 blocked, sleeps after idle (a one-minute cron keeps one warm at ≈ $2/mo), egress past 1 TB at $0.025 per GB — serve files from R2, never from a container.

### 40.7: Zero Trust, operations, agents
- Tunnel + Access in front of SSH, HAProxy stats and `/metrics` on SLC (identity instead of the WP-R1-1 ACL, and no inbound port for SSH); Gateway and WARP resold to MSP clients (free to 50 users); Logpush for an off-box audit trail (pairs with WP-R9-7); the 5.14.12 uptime check as a Cron Trigger writing D1 with a public history; **the MCP server of 26.8 on Workers** in front of the management API so an agent can provision a tenant, mint a key and run a probe (proven with six tools).

### 40.8: Search and intelligence over a bucket
- At upload (a 31.6 function, or the client): CLIP and face embeddings and captions → Vectorize, one index per bucket (a million photos ≈ $0.26/mo of index); OCR and document Q&A via AI Search; names and tags via D1 FTS5 or Postgres — "search your backup" at $4 per TB. For Private spaces the index is built on the device and stored encrypted (41.2). Feeds 14.1, 23.5 and the edge variants of Phase 24.

### 40.9: On-upload automations
- R2 event notifications → Queues → Workflows is the Cloudflare front-end of the 31.2 rule engine for R2-held objects (scan, thumbnail, receipt, copy to Vault, webhook through WP-R11-3's outbox); on the hub the same rules run from the 5.11.6 event stream. The Zapier/n8n-shaped recipes of 31.5 grow from here.

**Costs and traps (for pricing docs and the Status line):** everything above shares one $5 plan (500 Workers per account); the meters that move are R2 storage of the working set, Email past 3,000 a month ($0.35 per 1,000), container minutes, and Images only if used. The 2026 bill-spike classes: unindexed D1 reads, a Durable Object alarm loop (one ran to $35,000 with zero users), container egress — index everything, set spend alerts on day one, serve bytes from R2 only. Cloudflare's terms require non-Enterprise plans to serve large files through the Developer Platform, Images or Stream — Workers Paid is compliant by the letter.

**[YOU] (2026-10-03):** rotate the Cloudflare API token and the B2 master key that were pasted into the chat (they sit in the owner's local credentials file, outside every repository); the bench resources created that day (the bench Worker, the showcase Worker and container, three Forge example Workers, the bench buckets and the Vectorize index) live on the production Cloudflare account — name or tear them down before launch; the zone-scoped token is readiness [YOU] item 8; confirm F-10.

**Test**: a public bucket's object is served twice — the first time through the hub (one promotion counted), the second from R2 with no request in the SLC access log → the key is deleted on Vaultaire and is gone from the edge within the purge window → `cdn.example.com` serves it with a valid certificate → an attic object requested through the edge produces a workflow run, an e-mail and a hot copy → a 36 MP upload yields one mezzanine and three derivatives with no Worker exceeding 128 MB.

---

## Phase 41: Private Compute on Encrypted Data
*Added 2026-10-03 from the owner's research on what ships in 2026 (FHE, queryable encryption, PIR, on-device ML, enclaves; sources at the end). Stands on: 10.0.1–10.0.4 (client SDK), 10.2 key hierarchy, 10.5 key UI, `.private/PRIVACY_NORTH_STAR.md` (client-side per-tenant convergent keys — unchanged), WP-R8-1. Stage 4 for 41.1–41.3 and 41.5; 41.4's hardware arrives with the Stage-5 standby box (F-18). Decisions F-7, F-8 (re-decided 2026-10-12 → per surface, 41.0), F-9 (re-decided 2026-10-12 → self-hosted SEV-SNP, 41.4), F-12 (within-space convergent keys — "per-tenant" in the north star reads as "per space" since F-12).*

**Why:** convergent encryption buys deduplication, not computation — the trick that makes dedup work (identical content → identical ciphertext) is what stops the server doing anything else, and it carries the confirmation and learn-the-remaining-information attacks (fixed by per-space keys (F-12), which is the north star). "Compute on E2EE data" is five techniques that ship today, each covering one slice. What still has no good answer: ranked full-text relevance over encrypted prose without leaking access patterns, and an LLM chatting with encrypted documents on a server the user does not trust — both are enclave-only (41.4).

### 41.0: Modes are a property of the surface — Private / Standard / Public (decision F-8, re-decided 2026-10-12; F-15)
- **A mode belongs to the SURFACE a space is reached through, never to a user's choice.** Names: **Private / Standard / Public**; "Normal", "Vault mode" and "Shared vault" are retired as mode names (**Vault stays the tape/attic TIER name**); sharing is a flag on a Private space ("Shared"), not a fourth mode.

| Surface | Default | Also available | What the server holds |
|---|---|---|---|
| **Developer** (S3 API, management API, SDK) | **Standard**, first-class — "encrypted at rest with keys we hold" (SSE-S3, within-space convergent chunks); server-side search, previews, lifecycle, rules, AI | **Private** via the SDK key core (K2) or **SSE-C per bucket (WP-SSEC-1)** | Standard: everything, readable by us (F-14). Private: ciphertext, sizes, counts, timestamps, access patterns, blind-index matches |
| **Consumer** (stored.cloud/photos, app.stored.cloud, the apps) | **Recoverable (Standard) + "Advanced protection" opt-in (= Private) until the passcode recovery vault of F-19 exists; Private becomes the default the day it does** | public links per item (10.0.6) | as above |
| **Public** (public buckets, share links, the content domain of 40.1) | **Public** — plaintext | — | everything, by design |

- **One source — `modes.json`** (beside `prices.json` in `internal/api/landing/`): mode names, per-surface defaults, the "what the server holds" sentence and the customer sentence of each mode. The privacy page, the law-enforcement page (43.4), the OpenAPI text of the bucket mode and `x-amz-server-side-encryption*`, the dashboard labels and the SDK enum are **generated** from it; a test fails on a mode name typed anywhere else (the `prices.json` pattern). Copy: Standard = "encrypted at rest with keys we hold"; Private = outcome sentences; "zero-knowledge" nowhere before an external audit. Keystone K3: the `buckets.mode` column (`standard` | `private` | `public`) is the one place a surface writes its mode.
- *(The 2026-10-03 per-space text below is history.)*
- **Normal:** server-side everything — search, previews, collaboration, AI — encrypted at rest with our keys (SSE-S3, convergent chunks). **Vault:** true E2EE — libsodium / WebCrypto on the device, Argon2id key derivation, the server stores and relays ciphertext; compute on the device (41.2); search by blind index (41.1) and client-side over an encrypted embedding index; dedup per-tenant convergent, cross-user only through 41.3. **Shared vault:** Vault plus key sharing between members and encrypted CRDT updates relayed by a server that cannot read them (41.5). The privacy page names what the server learns per mode: sizes, counts, timestamps, access patterns, which encrypted rows matched which opaque token; in 41.4 only that a job ran. (Drime and Proton made the same mode-not-default choice; as the default, E2EE kills server-side search, previews and collaboration for the families and teams who want them.)

### 41.1: Queryable index — always on in Private
- The MongoDB Queryable Encryption / CipherStash EQL pattern on our metadata store: each file's metadata written as blind-index tokens (HMAC under a client key) for equality, **bucketed blind-index tokens for dates and sizes (day / month / year, size buckets; a range = the union of buckets) — never order-revealing or order-preserving encodings: Naveed–Kamara–Wright (CCS 2015) recovered OPE columns for > 80 % of records from public statistics and Grubbs et al. (2016) did the same to ORE (survey correction 1, `docs/research/PRIVACY_TECHNIQUES_2026-10-11.md`)**, encrypted tokens for free text; the server runs the query and returns ciphertext ids, the client decrypts. Filenames, tags, dates, sizes and EXIF become searchable with client-held keys (prefix and substring are still preview-grade upstream). A sidecar table beside `object_head_cache`, or a per-space table in the app plane; the leakage sentence goes on the privacy page. Identity-bound split keys (the ZeroKMS shape: per-value data keys derived from a client secret plus a server seed, nothing stored whole — a breach yields nothing) belong with 32.6.

### 41.2: On-device ML with encrypted indexes — *(2026-10-12)* Ente's clients already ship on-device CLIP search and face grouping; on the consumer surface this arrives with the clients (42.3), and 41.2's work is the index format and the dashboard's use of it.
- Thumbnails, EXIF, CLIP embeddings, faces, OCR and the search index are produced on the device (transformers.js v4 with WebGPU, WASM fallbacks; Chrome and Edge run the whole pipeline, Safari and Firefox fall back to the 40.3 container only on the Standard surface), encrypted with the space key, then synced; search by meaning runs locally over the downloaded index (10,000 photos ≈ 5 MB). This is Ente's Magic Search and Proton Drive's model, and the only option where the server may not see pixels. Costs nothing beyond the storage.

### 41.3: Private information retrieval for lookups (decision F-7)
- A homomorphic lookup service — Apple's open-source `swift-homomorphic-encryption` (BFV, quantum-resistant), the library behind Live Caller ID on every iPhone; it compiles to a Linux binary that runs in one lite Container (≈ $2/mo) with batches scheduled by Workflows or the hub's scheduler — answers "do you already hold this chunk" (the 38.1 dedup check, the cross-tenant case 10.8 was for), "is this hash in the breach list" and "which shared album contains this" **without learning the question**. **This replaces 10.8's server-held OPRF secret as the mechanism:** no key to steal or compel, no confirmation attack, no rate-limit boundary to sybil. F-3's measurement trigger stays the go/no-go for building any cross-tenant dedup at all. **(2026-10-12)** Cross-tenant dedup of encrypted data is the opposite of least access: dedup is **within-space convergent only (F-12)**; the one cross-space case is the **opt-in public-artefact namespace**; PAKE/PIR dedup stays **research behind F-3's measurement**, never a build item; **never a global convergent key**. Homomorphic machinery is PIR lookups only — compute is on the device (41.2) or attested (41.4). For a large server-side semantic index, Apple's Wally (batched queries, fake queries, anonymised routing, somewhat-homomorphic encryption, differential privacy) is the roadmap answer; personal libraries use 41.2 instead.

### 41.4: The attested data plane — self-hosted SEV-SNP (decision F-9, re-decided 2026-10-12)
- **One shared foundation, three tenants of it:** (1) developer compliance processing (the job runner below); (2) the consumer **recovery realm** (the passcode recovery vault of F-19 — the first realm IS this box) and cloud AI over Private spaces (the Apple Private Cloud Compute pattern); (3) **"can't quietly"**: an operator read of customer data or metadata exists only through the measured image, whose key-release policy logs it (43.6). No longer "opt-in per job".
- **First step now (2026-10-14, F-20):** the key broker starts before the EPYC box exists, on a rented SEV-SNP VM, unwrapping per tenant (43.7); the box takes it over with a measured image when bought. Prod's own boot state is published from day one (43.8: a daily fTPM quote of SLC and the binary hash of every deploy), so "the measurement changed" is a signal before any CVM exists.
- **Hardware line (2026-10-12):** the prod box is a **Ryzen 9 9950X — no SEV-SNP**; the plane needs **EPYC Milan or newer** with SEV-SNP exposed. The Stage-5 standby box is bought as that EPYC (F-18; verify `/sys/module/kvm_amd/parameters/sev_snp` = `Y` on the vendor's live image **before paying**) and serves at once as standby + PG replica + backup target + one TPM escrow share of F-14; its CVM role starts when 41.4 starts. **Prod does not move into a CVM before launch.**
- **Shape:** app + Postgres + keys inside one measured confidential VM (SEV-SNP guest, measured boot, dm-verity root, **no ssh**); a **Trustee KBS** (CoCo key broker: attestation service + reference values + release policy) **on another box** releases data keys only to a guest whose report matches the published measurement; measurements + policy are published in the transparency log (43.6) and served live at an **attestation endpoint** (`/.well-known/attestation`: report, measurement, policy hash, log inclusion proof) that clients and auditors verify. **Stack named (2026-10-12 research):** stock Ubuntu host (host-side SNP in 25.04+); QEMU `sev-snp-guest` with `kernel-hashes=on` so kernel + initramfs + command line are in the launch measurement, `-cpu EPYC-v4`, vCPU count pinned (any change moves the measurement); the guest is a reproducible image (Phase 15.5) with a dm-verity root, **no ssh and no shell in the initramfs** (the hypervisor can inject terminal input); the data disk's LUKS key is released only against an attestation report that carries a fresh guest-generated public key in its report data (the report-bound-key pattern), so a copied disk never decrypts outside the measured image — Apple's boot-fresh data key, at one-box scale; verification with `snpguest` / go-sev-guest against AMD's ARK → ASK → VCEK chain; **Trustee** (CoCo KBS + attestation service + reference values, Apache-2, docker-compose, no Kubernetes needed) on the OTHER box, release policy = the published measurement; TLS for `s3.stored.cloud` terminated inside the guest. Not claimed: Apple's OHTTP relay / non-targetability — a fleet property.
- For heavy work (video transcode, OCR of a scanned archive, a large model over a document set): the client wraps a one-time job key so that only an enclave with a **published code measurement** can unwrap it — ~~AWS Nitro Enclaves with a KMS attestation policy, GCP Confidential Space or Azure Confidential Containers; Cloudflare has no enclave~~ the self-hosted SEV-SNP plane above (F-9 re-decided 2026-10-12: self-hosted first; a cloud enclave is at most a later second realm under another operator, the Juicebox / SVR3 lesson), so this is a sidecar in a confidential cloud. The enclave pulls ciphertext from R2 at zero egress, processes at native speed, re-encrypts the output, discards the key; the client verifies the attestation document before releasing anything. Trust moves from "FairForge" to "this exact open-source build on this hardware" — the Signal and Apple Private Cloud Compute posture; the enclave code is open-sourced and its hash published, or the attestation means nothing. Cost: a four-vCPU confidential VM processing a thousand photos runs about ten minutes, a few cents, only when asked. **FHE inference stays narrow** (Zama Concrete ML; TFHE bootstrapping now sub-millisecond on an H100; MNIST in 1 s, logistic regression on 10,000 rows in seconds, CIFAR-10 in 47 s): spam and policy scoring on encrypted features, not image pipelines, and it needs a rented GPU box — that is 35.7's written verdict.

### 41.5: Encrypted CRDT relay for shared Private spaces
- Yjs updates encrypted on the clients; a Durable Object (or the hub's WebSocket relay) relays and stores ciphertext it cannot read; the server sees timing and sizes only. Also the collaboration layer of 42.3's docs, sheets and whiteboard in Private mode.

**Build order (2026-10-12):** 41.1 → 41.2 in the Consumer lane; **41.4's box and attestation endpoint in the Trust lane as soon as the standby EPYC lands** (the recovery realm of F-19 and keystone K5 wait on it) → 41.5 → 41.3 research only. The 2026-10-03 order, for history: 41.1 (filename, tag and date search is what people reach for first) → 41.2 → 41.3 → 41.4 (OCR and video first) → 41.5. 41.1 and 41.2 cost nothing beyond storage; 41.3 one lite container; 41.4 metered per job.

**Test**: a Private space finds a photo by filename, date range and tag with no plaintext in any server log → semantic search over 10,000 encrypted photos completes on the device → two tenants' dedup check through PIR returns the right answer while the service's log holds no hash → an attested OCR job's output decrypts only on the client and the job's logs name no content → two members of a shared Private space see each other's edits while the relay stores ciphertext only.

Sources: [FHE in 2026](https://wavect.io/blog/fully-homomorphic-encryption-practical-2026/) · [Zama TFHE benchmarks](https://docs.zama.org/tfhe-rs/get-started/benchmarks) and [Concrete ML](https://github.com/zama-ai/concrete-ml) · [FHE inference benchmarks 2026](https://dualitytech.com/blog/homomorphic-encryption-performance-2026/) · [MongoDB Queryable Encryption](https://www.mongodb.com/docs/manual/core/queryable-encryption/reference/supported-operations/) · [CipherStash searchable encryption](https://cipherstash.com/docs/concepts/searchable-encryption) and [ZeroKMS](https://cipherstash.com/docs/concepts/key-management) · [Apple swift-homomorphic-encryption](https://github.com/apple/swift-homomorphic-encryption) and [Live Caller ID via PIR](https://www.swift.org/blog/announcing-swift-homomorphic-encryption/) · [Apple Wally](https://machinelearning.apple.com/research/wally-search) · [Ente on-device ML](https://ente.com/help/photos/features/search-and-discovery/machine-learning) · [Confidential computing across the clouds, 2026](https://cloudandclear.uk/confidential-computing-aws-azure-gcp/) and [Nitro attestation with KMS](https://thebackenddevelopers.substack.com/p/confidential-computing-for-backends) · [Privacy-preserving vector search benchmark](https://arxiv.org/pdf/2608.01192) and [CoSecRAG](https://eprint.iacr.org/2026/1442.pdf)

---

## Phase 42: The App Plane on Cloudflare — plans, tenancy, Stored, Hearth-CF, the Forge apps, the LET lineup
*Added 2026-10-03 from the same session. Most of this lives in other repositories (`~/fairforge/storedpics`, `~/fairforge/hearth`, `~/fairforge/snapshelter`, `~/fairforge/edge-showcase`); it is in this plan because every product below stands on this engine, and 42.1–42.2 change this engine's quota, tenancy and residency model. Stands on Phases 18, 40, 41. 42.1–42.2 are Stage 6; 42.3–42.5 Stage 9; 42.6 runs with Phase 27 from Stage 0.*

### 42.1: The plan ladder — price the pool, meter the bytes, give seats away
- The cost shape is storage at $2–4 per TB plus near-zero compute, so seats are nearly free and **the pool is the price**. Draft ladder (list price → pool → seats; COGS at full fill): Free $0 → 20 GB → 1 ($0.08); Personal $3 or $30/yr → 1 TB → 1 ($2.06); Family $6 or $60/yr → 3 TB shared → up to 6 ($6.18); Team $4/seat, 5-seat minimum → 1 TB per seat pooled → 5–50 ($2.06/seat); Business $8/seat → 2 TB per seat → unlimited ($4.12/seat, adds retention, legal hold, EU residency, immutable backups); Storage only $4.49/TB; Vault $2/TB yearly; Reseller $29/mo + 70% of list.
- Rules: pooled quotas, not per-seat (a family of six with one photographer should not buy six slots; members draw from the pool with optional caps); seats are free until they cost something (mail, meetings, SSO); fill-rate pricing is the margin (at a real 25% fill Family costs $1.50 against $6 — price for fill, meter for the outlier); hot egress is the one trap (40.1); yearly shown first. On this engine: `tenant_floor_quotas` becomes per-organisation pools with optional member caps; the quota check happens when the presigned URL is minted, so an over-quota upload never consumes a byte. Ties to V18.7 and `prices.json`.

### 42.2: Tenancy — one tree, four levels, isolation by construction
- `platform → reseller → organisation → space → user`. The **organisation** is the paying unit and the isolation unit (in the app plane one SQLite database per organisation — a Durable Object; on this engine the tenant): a bug cannot read another organisation's rows because there is no shared table to read. **Users are identities, not storage**, and may belong to several organisations (home and work — the thing every family-plus-team product gets wrong); the OAuth provider issues the session, the organisation decides what they may touch. **Resellers** are a parent pointer plus a brand: theme tokens, a custom hostname (40.2), a sending domain, a support address, a billing mode (we bill their customers at 70/30, or they bill on their own Stripe and we invoice wholesale), API keys scoped to their subtree, bulk provisioning, an exportable revenue-share statement — Cloudflare's own Tenant platform shape. Metering is tagged with organisation and reseller ids at write time, so an invoice at any level is one query. **Residency is an organisation attribute set at creation and never changed** (7.6; EU → EU jurisdictions on R2/KV and EU backends here). Extends 18.1, 18.5 and 18.7–18.9. **On this engine (2026-10-05): the end user is the tenant; an app is a scoped key or an STS token of that tenant** — an app never owns the bytes, so a compromised app key is one key to rotate and an app that goes away leaves the user's pool intact; the platform base layer (42.7) mints those keys and tokens and never holds a tenant's primary pair.

### 42.3: The consumer surface — stored.cloud/photos and app.stored.cloud (Stored is the brand; Drime-class features, a customisation ladder and a marketplace)
- **(2026-10-12) The consumer app and its modes (F-15, F-19; charter `docs/surfaces/CONSUMER.md`).** The consumer surface is **`stored.cloud/photos`** (stored.pics 301 → it; the `stored.pictures` app-tenant becomes `photos`) and **`app.stored.cloud`**, built on **Ente's audited clients** (web, desktop, iOS, Android — AGPL; Cure53 / Symbolic Software audit 2023) talking to this engine through the 42.7 adapter, so the device-side crypto is audited code rather than ours. **What Ente solves and what stays ours (owner question 2026-10-12):** solved by the clients — every photo, video, thumbnail and metadata field encrypted on the device, faces and search on the device, albums, shared albums, fragment-key public links, casting, playback; museum stores ciphertext in our S3 through presigned multipart (a real ciphertext workload on our own paths). Still ours: (1) mainstream recovery — Ente recovers with 24 words + trusted contacts, hence F-19; (2) the web client is served by us — reproducible builds + build hashes in the log (F-16); (3) brand and licence — server and clients are AGPL (run as a service = publish our changes), selling it as Stored means forking and rebranding the apps under our own store accounts and respecting Ente's trademark policy (check before deciding; "powered by Ente" is the no-fork path); (4) identity — museum has its own accounts and Stripe plans until the 42.7 adapter. **Private is the default here ONLY once the passcode recovery vault exists:** a passcode protects the recovery key inside the recovery realm — the attested box of 41.4 — under **hardware-enforced attempt limits** (the Apple iCloud Keychain escrow pattern; Juicebox OSS, WhatsApp Backup Key Vault, Signal SVR3 are the references; one realm first, more under other operators later), plus trusted contacts (10.0.3 Legacy), passkeys (10.0.5) and recovery nudges. **Until then:** the recoverable default (Standard — e-mail + password reset works) and **"Advanced protection"** opt-in = Private, with Apple's warning copy. The flip from opt-in to default is one `modes.json` edit and a dated transparency-log entry (43.6). Keystone K5 = the recovery-vault interface.
- **Feature map:** files, folders, sync and share links with expiry and passwords → a Durable Object per user for the tree, Vaultaire for bytes, presigned uploads, KV links (the desktop sync client is the open engineering); collaborative docs → Yjs with Tiptap or Lexical and a DO relay; spreadsheets → Univer client-side (imports xlsx); PDF markup, redaction, fillable forms → pdf-lib and pdf.js; e-signatures → pdf-lib + WebCrypto in a Worker with a D1 audit trail (eIDAS-qualified needs a certificate provider); Private mode → 41.0 (Advanced protection until the recovery vault above exists); AI assistant → Workers AI through AI Gateway and AI Search; whiteboard → tldraw (its sync backend is built on Durable Objects and R2); transfers → live; audit and compliance → Analytics Engine + D1; EU residency → jurisdictions; teams → 42.2. Months of work for one person on Hearth-CF, not years; the sync client and editor polish are the real engineering.
- **The customisation ladder** (users climb as far as they want; a template is a saved position on it): 1 theme tokens → 2 layout, a JSON block tree of first-party blocks → 3 custom CSS, parsed, scoped (`@scope` or a shadow root per block), imports and non-allowlisted `url()` dropped — the MySpace layer → 4 widgets, user HTML/JS in a sandboxed iframe on a second cookie-less origin with a postMessage SDK and a short-lived token scoped to the approved data → 5 logic, a code string run as a Dynamic Worker (or the 40.5 QuickJS interim) with no network, a CPU cap and only the bindings handed in. **Templates never fetch data:** a block declares a binding ("media, by date, limit 24") that the host resolves — Stored against the library, SnapShelter against a gallery, TShop against a catalogue — so one template runs in every app; a template is one JSON manifest (id, semver, target apps, required scopes approved on install, tokens, block tree, CSS, widget refs, optional code); installs copy it, edits fork it, creator updates are offered as diffs. **Marketplace:** catalogue in D1, bundles in R2, screenshots via Browser Rendering at publish, a moderation queue, install counts in Analytics Engine, Stripe Connect payouts; **Framer's creator model** (take under 15% on paid templates; pay an affiliate share on free templates that convert — the mechanic that fills a marketplace before anyone buys). The growth loop is the "use this look" fork button on every public page (Linktree's and Bento's whole engine). Day-one guardrails: a safe-mode render with layers 3–5 off so a broken template never locks anyone out; Turnstile and moderation on public pages (a free page builder on a second origin is a phishing kit otherwise). AI at layers 1–3: "make it look like a 2006 band profile" → tokens, block tree and CSS.
- **The Microsoft 365-shaped slice for small teams:** files (Drive), docs/sheets/slides (Tiptap, Univer; OnlyOffice or Collabora in a container at ≈ $42/mo per editing server where .docx/.xlsx fidelity matters), mail in via Email Routing to a Worker and out via Email Service (IMAP only through Spectrum, which is Enterprise-only, or JMAP over HTTPS, or Stalwart in a container; **own MX/SMTP is impossible — port 25 is blocked**), CalDAV and CardDAV on Workers, chat on Durable Object WebSockets, meetings on the Realtime SFU (1 TB free egress) or RealtimeKit ($0.002 per participant-minute), identity via the Workers OAuth Provider (released 2026-10-01) plus passkeys and Access for SSO into third-party apps, Copilot via AI Gateway + AI Search over the tenant's documents. Not replaceable: telephony, device management, Windows licences. A $4 seat with 50 GB and mail runs at better than 90% margin against Drime's €2.99–19.99 and Microsoft's $6–22. **Build order:** Drive + Transfer + Private spaces (recovery vault before Private is default) → identity → docs/sheets/whiteboard → mail/calendar/contacts (needs the zone) → chat and meetings → Copilot and search → admin, billing, compliance.

### 42.4: Hearth-CF and the Forge apps
- `@fairforge/hearth-cf` exists (`~/fairforge/hearth/cloudflare/`): router with URLPattern params, D1 with migrations, HMAC session cookies and a rate limiter, storage with presigned URLs for R2 **or Vaultaire** plus the hydrate-on-miss pattern and WASM resize, email through Email Service with a KV outbox until the zone exists, queues and workflows with idempotency, metering to Analytics Engine, themed pages with a CSS sanitiser, AI helpers, multi-tenant provisioning by API key, handle or custom hostname; a scaffolder stamps out an app. LinkForge, FormForge and PingForge were scaffolded and deployed in 15–18 lines each; StatForge, PinForge, SendForge, BookForge, DocsForge, DeskForge, MailForge, LeadForge and LedgerForge are the same shape. The cPanel Hearth plan's modules (db, auth, email, storage, cdn, cache, stats, tiering, monitor, cron, queue, rate limit, deploy, probe, theme) each map to a Cloudflare primitive or disappear; erasure stays in Vaultaire. **SnapShelter:** the fast path is the Express app in a Container with PlanetScale Postgres through Hyperdrive ($11–17/mo; the app's queries are already Postgres-style behind a MySQL shim), the native path Workers + D1 ($5); the contained first steps — the Free zone in front of the cPanel box, photos on R2 or Vaultaire by presigned upload, Email Service, Cron Triggers for the double-firing jobs — each remove a live problem and are prerequisites for either path. Workers Builds and per-branch previews replace the GitHub Actions + PHP deploy.

### 42.5: Mirrors of apps people already use (the catalogue)
- "Self-hosted apps, hosted, with your data in your own bucket" is a category nobody owns; the pain they sell against is operations (updates, backups, exposure). Live or easy: Transfer (WeTransfer), Paste (PrivateBin), LinkForge (Bitly), PingForge + cron monitors (Uptime Kuma, Healthchecks), StatForge (Plausible, Umami), FormForge (Typeform), BookForge (Cal.com), DocsForge (Ghost), Profiles (Linktree, Bento, MySpace), changedetection (Browser Rendering + cron + diff), ArchiveBox/Wallabag, Stirling-PDF, Open WebUI (Workers AI + AI Gateway), Audiobookshelf (R2 + RSS), Trello/Vikunja, Invoice Ninja/Freshdesk (LedgerForge, DeskForge). Medium: Stored Drive and Photos (42.3), Paperless-ngx (OCR via AI Search, tags, FTS, retention — MSPs pay for this), Calibre/Navidrome/Jellyfin libraries over R2 with HLS via a container, Vaultwarden (client-side-encrypted vault; audit it), Notion/Docs (Yjs + DO), Discourse/Slack (D1 + DO WebSockets), n8n/Zapier (Workflows + Queues + MCP tools), Auth0/Keycloak (Workers OAuth Provider + passkeys — the identity under every app above), Jupyter (Sandbox SDK). Large: Nextcloud (Drive + Docs + CalDAV/CardDAV), Gitea (Cloudflare's Artifacts, open beta), Mastodon/Bluesky PDS. Resell rather than clone: Tailscale and Pi-hole (Zero Trust WARP and Gateway). Not possible on this stack: mail servers (port 25), Home Assistant and Minecraft (local hardware, UDP), Syncthing (no P2P, no UDP). Sold as a platform the same primitives are Vercel/Supabase-shaped: Edge Site and customer edge apps, a D1 or DO SQLite per tenant, jobs as a service, sandboxes, AI endpoints, media APIs, auth, the marketplace. **Order of construction:** zero-egress video hosting (40.3; bandwidth is what kills every small video host) → on-upload automations (40.9) → search inside the bucket (40.8) → the three weekend clones changedetection, Healthchecks, Open WebUI → the Paperless clone → auth as a service → Stored Drive with a sync client.

### 42.6: The LowEndTalk offer lineup and the hosting-business surface (owner track, with Phase 27)
- **Lead with storage, back it with free edge extras, keep VPS an on-ramp.** Lineup: Storage box $4.49/TB/mo, $3.99 yearly (S3, zero-egress downloads, erasure across three vendors, immutable buckets — under B2's $6.95, level with storage-VPS prices, durability they cannot get); Vault $2/TB, 1 TB minimum, yearly; Edge Box $25/yr (a lite container: outbound-only Linux for bots, cron, monitors, scrapers — say plainly: no inbound ports, no VPN); Transfer, Paste, Links and status monitors free with any paid plan; Edge Site free with a custom domain at $1/mo (static + forms + a profile page, 100 custom hostnames); Immutable backup target $4.99/TB + $20 setup for the MSP and r/sysadmin crowd. Skip in the first post and say so in one line: VPS, game servers, mail, VPN, GPU.
- **What the thread needs** (all built in the showcase): a looking glass with 100 MB and 1 GB test files, a speed test, a status page with history, an order form behind Turnstile, a panel with API keys and per-GB metering, yearly prepay up front, PayPal and crypto (TShop) beside Stripe, a hard AUP (no Borg or rsync over SSH, no inbound ports on Edge Box, no VPN resale, abuse answered within 24 hours), the founder in the thread for 48 hours, provider registration with company name, terms and privacy policy or the thread is removed; "say where the bytes live" (iDrive, Lyve, OneDrive with parity; hot copies at the edge; Salt Lake and Frankfurt). Title: "[Offer] Stored (stored.cloud): S3 storage from $2 per TB, zero egress, immutable, plus a free status page, transfer, and a $25-a-year edge box."
- **Communities:** r/datahoarder (box + Vault: "3-2-1 offsite target, immutable, egress free"); r/selfhosted (box + Edge Box: "S3 for Immich, Ente, Nextcloud plus a $25-a-year box for your bots"); r/msp (immutable target: "Veeam and Kopia target with object lock, per-client buckets, metering"); indie hackers and r/webdev (Edge Site, customer-deployed edge apps); photography forums (SnapShelter: "Pic-Time plus HoneyBook for $25"); privacy forums (stored.cloud/photos — Ente's audited clients on our engine; client-side-encrypted paste).
- **Protocol truth table for a storage box:** S3 live (rclone, restic, Kopia, Duplicati, Veeam, Nextcloud, Immich, Ente); WebDAV via a Worker translating to S3 (13.2); SFTP/FTPS via SFTPGo in a container with inbound port 22 through Spectrum (13.3; needs the zone on Pro); Borg and rsync over SSH **no** (a shell and a disk — Hetzner keeps that edge); immutable buckets live; zero-egress downloads and a CDN hostname per bucket via 40.1/40.2.
- **The VPS question:** a container is not a VPS and must not be called one (no public IP, no persistent disk, no UDP, port 25 blocked, sleeps). The dedicated-style play is hybrid: the WHM reseller account and the Salt Lake box for workloads that need a real machine, with the edge status page, looking glass and panel in front; the ARIN/ASN/IPv4 path and Proxmox are in 34.8 and 39.7. VPS has 20% margin and the most support load; it is an acquisition channel for storage and add-ons, not the product.


### 42.7: Admission and the ledger — the platform base layer (stored.cloud/platform) on this engine (2026-10-05; renamed 2026-10-12 — stored.cloud is the brand's domain, F-17; Hearth-CF keeps its name)
*From the snapshelter session's "stored.cloud base layer" proposal (2026-10-05) and the plan-driver session's live tests on the prod origin (`docs/CLOUDFLARE.md` § Findings 2026-10-05). Stands on 42.1, 42.2, 40.1, WP-R10-9, queue item 4 (the webhook outbox). Stage 6 with 42.1–42.2.*
- **Four primitives, one authority.** The platform (Hearth-CF on Workers: identity, ledger, billing, dashboard, SDK) provides identity, spaces, admission and events; **Vaultaire is the quota and ledger authority for bytes.** The platform *mints* — a space is a bucket on Vaultaire per user per app with a scoped key, an upload is a presigned PUT minted by the platform on that key with a **signed `Content-Length`** — and Vaultaire *admits*: a presigned PUT whose signature covers `content-length` is refused (403) at any other body size and the quota is checked at PUT (proven 2026-10-05), so an app cannot overrun a pool, no app carries quota code, and nothing is reserved that the engine did not accept. R2 documents no equivalent, which is why R2-direct uploads stay public-bucket only with the `direct-uploads/complete` drain (40.1).
- **Meter at mint, reconcile against the engine.** The platform records every mint (size, space, app, user) in a Durable Object ledger per organisation (42.2) and settles it on the engine's event (object created / deleted through WP-R11-3's outbox, queue item 4 — the outbox is on this plane's critical path), releasing on expiry; the ledger's per-space totals are reconciled daily against `tenant_quotas` / `tenant_floor_quotas` and `bandwidth_usage_daily`, and the engine's number wins. R2 Data Access Logs (GA 2026-09-04, "asynchronous and best effort, events may be delayed or omitted", seven days in Workers Observability, dashboard-only toggle, no API) feed dashboards and anomaly views, never an invoice.
- **Reads.** Private reads go through the gateway Worker + Workers Cache (40.1(a)); Sippy stays for the archive (restore once, serve many) and for app-owned buckets with immutable keys (Ente's museum bucket: random keys, never overwritten, deletes reach the R2 copy through the outbox → purge). An overwrite never reaches a Sippy copy except through lifecycle expiry; a key whose pull once failed must be re-keyed.
- **The Ente adapter is one app-tenant:** museum's secondary bucket is a Vaultaire bucket on a scoped key of the `photos` app-tenant (the consumer surface of 42.3; `stored.pictures` was its name until 2026-10-12), its per-user quota is pushed from the ledger through museum's admin API, usage flows back from the engine's events; Ente stays unmodified. Any third-party app hosted later gets the same shape.
- **Key verification at the edge** costs one signed `bytes=0-0` GET against the origin (170–375 ms per isolate, memoised 60 s; never a HEAD — the edge turns a HEAD on a presigned URL into a GET). Workers VPC + Hyperdrive to the Salt Lake Postgres was weighed and refused: it would put a credential that can read `api_keys` on the edge for a lookup the origin already answers, and the cheap version is a cached verdict keyed on the key id + a hash of the secret with a 60 s TTL. The missing piece is an **introspection call on Vaultaire** (`GET /api/v1/manage/whoami` signed by the key: tenant id, scope, expiry) so the cache can partition per tenant instead of per key — XS, with 42.2.
- **Costs with the edge** (the one-page table is `.private/EDGE_COGS_2026-10-05.md`): Vault $1.55 + the R2 working set at $0.50/TB-day of the lifecycle window; Standard $1.94–2.07 + $0.30 per million gateway requests (no cache storage fee); Performance = the presigned 307 at $0; public buckets = R2 at $15.36/TB of working set with free egress and the 40.1 bandwidth cap.
- **Test:** an app mints a 1 GiB upload for a user with 512 MiB left → the engine refuses the PUT at the signed size and the ledger releases the reservation on expiry → the same app uploads 100 MiB and the organisation's ledger, `tenant_quotas` and the dashboard agree within a day → the user's key for app A cannot read app B's space → an Ente delete removes the R2 copy within the purge window.

---

## Phase 43: Data Minimisation — the Cape model ("hold nothing on a customer but what the service needs")
*Added 2026-10-11 (owner ask). Stands on: `.private/PRIVACY_NORTH_STAR.md` (the metadata ladder and "Built to Forget"), the Post-Launch stack row 6b (absorbed here), WP-R10-3 (erasure), WP-R10-3b (the export — its section list IS the inventory), WP-R13-3 (the retention job), 10.0 (the Ente model) and 41.0 (the per-surface mode table, `modes.json`). 43.1–43.4 and 43.6 are the **Trust lane** (2026-10-12: Phase 43 + the transparency log + the F-14 install + the attested box; charter `docs/surfaces/TRUST.md`) of Stage 1 (S each, alongside Stage 1a); 43.5 is demand-gated after 10.0. Decisions F-14 (the master key) and the retention targets below (owner confirms the table before the WP).*

**Why Cape.** Cape is a US mobile carrier that sells privacy as the product and publishes what it holds: at signup "we do not collect your name, only your phone number and zip code" (the zip for tax, three years); "we intentionally segregate customer data between our payment gateway, Stripe, and Cape's internal business systems" and store a Stripe token, never the card; a website e-mail "cannot [be linked] to your phone number or other personal details"; the app's key pair has its private key "stored locally on your device and … never known by Cape"; usage records live on a short clock — call detail records and their IP addresses one day, attach data one month, cell id up to thirty days, undelivered messages seven days, customer-service threads 120 days; website and survey data are never associated with an account, spam metadata is "hashed and encrypted", diagnostics de-identified; identifiers are rotated daily so the underlying carrier's data is "messy, fragmented, and disconnected from your personal information"; and under legal process they fight overbroad requests, notify the subscriber, and can only produce what the clock has not yet erased ("systems designed to forget rather than remember"). The honest reading, which we copy too: Cape's usage records ARE tied to the account while they exist — the protection is the clock and the short identity record, not anonymity. Sources: [Cape FAQ](https://www.cape.co/faqs) · [Built to Forget](https://www.cape.co/blog/built-to-forget-cape-is-a-new-kind-of-mobile-carrier) · [Cape privacy policy](https://www.cape.co/privacy-policy) (the retention table).

**The principle for Stored:** three records, three systems, three clocks. **Identity** (who pays and signs in) = `users`: an e-mail and a password verifier, nothing more — no name, no address, no company, no attribution; billing identity lives at Stripe behind a customer token. **The data plane** (what is stored) names its owner by an opaque tenant id and nothing else — no e-mail on any data-plane row. **Usage** (who did what, from where) is written for billing, security and the customer's own logs, then **forgotten on a clock we publish**, aggregates kept. Policy can be compelled; a column that does not exist cannot (the Proton 2021 lesson in the north star).

### 43.1: The inventory, generated — "what we hold on you"
**Files**: `internal/api/account_export_sections.go` (`exportSections`, `exportExcludedTables`, `exportNeverColumns` — the three maps `TestExportSectionsCoverSchema` already walks), a new `internal/privacy/inventory.go`, `/legal/data-inventory` page, `docs/PRIVACY_INVENTORY.md`
- One generated table, from the schema walk: every tenant- or user-keyed table and column → purpose (`identity` / `data-plane` / `usage` / `billing` / `security`), personal or not, retention clock, the job that enforces it, the mode in which it exists. The export's maps already classify every table; add the two missing fields (clock, enforcer) and render the table as a legal page and a doc. **Build rule:** a new PII column without a row in this table fails the build (the WP-R10-3b pattern), so the inventory cannot drift.
- Today's answer, from the schema (2026-10-11): identity = `users.email`, `users.company` (asked at signup, read by nothing that matters), `users.signup_referrer / signup_utm_*` (070: per-user marketing attribution), `oauth_accounts.email`, `tenants.email` (a copy of the user's e-mail on the data-plane row), `tenants.intent_*` (the house a visitor built); usage with personal data = `audit_logs.ip / user_agent` (kept for ever), `dashboard_sessions.ip_address / user_agent` (24 h), `s3_access_log.source_ip / user_agent` (30 d), `cdn_access_log` (visitor, 2 d), `waitlist_signups.ip_address / user_agent / country` (90 d), `stripe_events` bodies (90 d — carry the customer's billing address and e-mail), `events` (90 d), the box's journald request lines (no published clock), the nightly DB dump off-box (30 d — deleted rows survive in backups); data plane = object keys and ETags in plaintext (the S3 contract in Standard mode; MD5 is an oracle for guessable content — 10.0's manifest is the answer), `object_metadata.content_hash`, chunk `plaintext_hash` (cross-tenant equality of unencrypted chunks).

### 43.2: Disassociation — the data plane knows an id, the identity record knows an e-mail
**Files**: migration (drop `tenants.email` after the two readers move to `users`; drop `users.company`, `users.signup_*`, `tenants.intent_*` once 43.1's table says they have no purpose — owner confirms), `internal/auth`, `internal/dashboard`, `internal/api/landing/attribution.go`
- `tenants.email` is a join key by e-mail between identity and data plane (R10: registration writes four tables; `account.ListDue` joins tenant to user by e-mail). Replace with the `users.tenant_id` / `tenants.id` relation only; the data plane never carries an e-mail again. Every notice (alerts, deletion, export) resolves the e-mail from `users` at send time.
- **(2026-10-12, Ente's server pattern)** The e-mail itself is stored **encrypted at rest under a server-held key** (`users.email_encrypted`, AEAD under the WP-R8-1 master key) and **looked up by a keyed BLAKE2b hash** (`users.email_hash = BLAKE2b-256(key = server hash key, lower(email))`, unique index) at registration, login and notice time — Ente's `encrypted_email` + `email_hash` columns exactly — so a dump of `users` yields neither the address nor an unkeyed rainbow table; `users.email` plaintext drops once the readers move. Standard copy applies: this is "keys we hold", stated as such.
- Signup asks for e-mail and password. `company` goes; attribution (070) stays **aggregate** in `site_stats_daily` (the cookieless counts already there) and leaves the user row — a per-user "came from X" column is the Cape "never associated with an account" line.
- Billing: Stripe customer token only (already); `stripe_events` bodies pruned 7 days after they are applied (they are the processor's records, not ours); the invoice PDF link is fetched from Stripe when the customer asks, never mirrored.
- Operator records about a customer (`admin_notes`) are declared in 43.1 with a purpose and erased with the account (already); support e-mail threads live in the mailbox with its own clock (declare it).
- Not changed by this phase: e-mail as the login identity (Ente's choice too), the audit trail of operator actions (`audit_logs` events stay for ever — 43.3 coarsens their IP/UA).

### 43.3: Built to forget — the clocks (owner confirms the targets before the WP)
**Files**: `internal/api/retention.go` (`RetentionJob`, `TestRetention_PoliciesMatchThePolicyPage`), `deploy/` (journald + log config on the box), the privacy policy + DPA (the periods are stated there — change both together, root CLAUDE.md rule)
| Record | Today | Target | Why / constraint |
|---|---|---|---|
| `s3_access_log` raw rows (IP, UA) | 30 d | **7 d**, IP and UA **blanked at 24 h** | after the customer's own log delivery (5.14.9, every 5 min) and the billing rollups; error rows for support keep the key/status, not the IP |
| `audit_logs.ip / user_agent` | for ever | **nulled after 90 d**, the event row kept for ever | the security window; an erasure already nulls them (WP-R10-3) |
| `dashboard_sessions` | 24 h | unchanged | already a clock |
| `cdn_access_log` | 2 d | unchanged (visitors' data) | rollup needs yesterday |
| `events` | 90 d | **30 d** | the customer's own event log; webhooks deliver within minutes |
| `stripe_events` bodies | 90 d | **7 d after applied** | contain billing address + e-mail |
| `waitlist_signups.ip / ua` | 90 d | **7 d** (country stays) | abuse window only |
| `users.signup_*`, `tenants.intent_*` | for ever | **gone** (43.2) | aggregate exists |
| journald on the box (request lines) | box default | **7 d**, request lines carry **no client IP** (`loggingMiddleware`: tenant, op, status, latency) | the audit path is the one IP writer |
| nightly DB dump, off-box | 30 d | unchanged, **disclosed**: erased rows live ≤ 30 d in backups | restore drill passed on PG 18 |
| Prometheus / Alertmanager | no tenant labels | unchanged (keep it so) | a tenant label is a log |
- Order of operations is the north star's: deliver (customer logs) → roll up (billing) → forget. `TestRetention_PoliciesMatchThePolicyPage` pins the new numbers; the privacy page, the DPA and 43.1's table change in the same PR.

### 43.4: The legal artefacts — say exactly what a request yields
- **Evidence, not promises (2026-10-11):** publish Merkle roots of `audit_logs` to **the transparency log (43.6, trust.stored.cloud)** so history cannot be rewritten silently; small counts in the transparency report and in `site_stats_daily` carry differential-privacy noise so no table singles anyone out; a Privacy Pass token (RFC 9576–9578) or OHTTP relay (RFC 9458) is how rate limiting and abuse control work once request lines carry no client IP (43.3)
**Files**: `/legal/law-enforcement` page, `/legal/data-inventory` (43.1), `trust.stored.cloud` (the 43.4 artefacts and the 43.6 log live there; `docs/TRANSPARENCY.md` is its source) + a yearly page, `security.txt` already in place
- **"What we can produce"**, per SURFACE and mode, generated from 43.1 and `modes.json` (41.0): **Standard** — the identity record, the data-plane rows (bucket and object names, sizes, ETags, timestamps), the usage rows still inside their clocks, and the bytes: Standard is "encrypted at rest with keys we hold", and that key never makes data unreadable to us (F-14); **Private** — identity, ciphertext, sizes, counts, timestamps, access patterns; nothing else exists to give; **Public** — plaintext, by design. The page is honest about the master key (F-14: an on-box TPM-sealed install ceremony with age-plugin-sss escrow, every ceremony logged in 43.6), about backups (≤ 30 d) and about every production (an entry in 43.6). **(2026-10-12)** It also says the thing no provider can promise: "can't read, prospectively" does not exist anywhere — a server can be modified to capture SSE-C keys and a client can be modified to leak Private keys, from the day of the change; our defence is that every server image and client build is published to the log, so the change is visible, and client-side tools we never ship (restic, rclone crypt) have nothing of ours to modify.
- **LE request policy** (Cape's and Signal's shape): legal process required, overbroad requests contested, the subscriber notified before disclosure unless a court forbids it, US jurisdiction stated plainly (no Swiss cosplay), EU residency = Geyser London region.
- **Transparency report**, yearly: counts of requests by type and what was produced; no warrant canary (contested doctrine — north star).

### 43.5: Numbered accounts (demand-gated; after 10.0)
- **The cryptographic form of Cape's disassociation (2026-10-11):** pay identified (Stripe) and receive blind-signed tokens (RSA blind signatures RFC 9474 / Privacy Pass issuance RFC 9578; Chaum's patent long expired) for quota and time; redemption is unlinkable to the payment, so the data plane can never be joined to the billing identity even by us
- The Mullvad shape: an account is a random number, no e-mail; recovery only by the recovery key (10.0.3); prepaid credits (BTCPay or a Stripe prepaid) — ~~tax handled as prepaid~~ (**2026-10-18: unverified and probably wrong — prepaid is a billing shape, not a tax treatment; counsel question 3 in `docs/PAYMENTS.md` § 6**), storage is not money transmission (counsel question 4); the ledger, the provider interface and BTCPay are built by WP-PAY, so 43.5 inherits them and adds only the blind-signed token kind; **private buckets only** (a public bucket or CDN hostname requires an identified account — the abuse posture of the north star and 40.1 stays coherent). Trigger: a paying customer asks, or the LET thread does.

### 43.6: The transparency log — trust.stored.cloud (added 2026-10-12, decision F-16)
**Files**: `internal/translog/` (keystone K4: append-only Merkle log, signed checkpoints, inclusion/consistency proofs — the tlog-tiles / Trillian shape, c2sp.org/tlog-checkpoint), `cmd/tools/translog-witness`, `trust.stored.cloud`, `docs/TRANSPARENCY.md`
- **Recorded, one signed entry each:** client build hashes (web, desktop, mobile, SDK — **clients refuse an unlisted build**); server image measurements and the key-release policy of the attested plane (41.4); every master-key ceremony (F-14 install, re-wrap, share rotation) and every escrow / recovery-realm ceremony (F-19); every law-enforcement production (type + count, at 43.4's notification time); **every admin read of customer metadata** (`/admin/*` pages and `GET /api/v1/admin/*` reads of tenant rows — "any access a public event"); the `audit_logs` Merkle roots of 43.4.
- **Shape:** append-only, signed checkpoints, **at least one external witness** co-signing every checkpoint, consistency proofs so history cannot be rewritten silently. **Start:** signed statements published at trust.stored.cloud, each stamped with **OpenTimestamps** (= 35.2's anchoring) and mirrored into **Rekor** (best-effort public instance, a second witness, never the only one); **later** the tile log with its own checkpoint and witness. **Witness pattern to copy (2026-10-12):** Meta's 2026 Backup Key Vault change — fleet public keys ship in a bundle signed by Cloudflare and counter-signed by Meta, and Cloudflare keeps its own audit log of every bundle; our equivalent is an independent party co-signing each checkpoint and keeping its own record, with OpenTimestamps + the Rekor mirror as the stand-ins until one exists.
- **Audience and copy:** the log is for press and auditors; customer copy states **outcomes** ("every build you run is on a public list; every time we open a key it is on the same list"); **"zero-knowledge" is forbidden before an external audit**. Small counts carry 43.4's DP noise. A gag order can stop us talking; it cannot stop an entry the mechanism itself requires — which is why the architecture signal (a changed measurement) outranks a warrant canary.
- **Built (K4, 2026-10-22, `docs/reviews/WP-P28.md`):** the library, the Postgres store (migration 103, append-only by trigger), the checkpoint job and its signing key (credential `translog_signing_key`; not on prod until the owner's ceremony), the `server_build` entry per distinct binary at boot, the `key_ceremony` admin route, the c2sp tlog-tiles read side behind `translog_public` (OFF), `cmd/tools/translog-verify` (signature, inclusion, consistency with a saved checkpoint — the witness role until K4b). **K4b built (2026-10-23, `docs/reviews/WP-P32.md`):** `cmd/tools/translog-witness` (our own off-box witness: consistency with its last checkpoint, a byte-exact mirror of the log proven entry by entry, each checkpoint co-signed under the witness's key), `cmd/tools/translog-restore` (`logstore.ReAppend`: entries a restore lost put back byte for byte from the witness cache, the origin kept), the read side on trust.stored.cloud only (`TRUST_PUBLIC_HOST`; DNS + certificate SAN are the owner's). **K4c (not built):** a co-signature by an independent third party (c2sp tlog-cosignature), OpenTimestamps, the Rekor mirror, admin-read entries, client build hashes, the fTPM quote in `server_build`.
- **Test:** a client with a build hash not in the log refuses to start → a ceremony's entry is co-signed by the external witness within the hour → an admin read of a tenant's rows appears on trust.stored.cloud with actor, opaque subject id and reason → a rewritten history fails the consistency proof against the witness's last checkpoint.


### 43.7: Blast radius — the tenant is the unit of disclosure (added 2026-10-14, decision F-20)
**Files**: `internal/crypto/master_key_source*.go` (a fourth source, `broker`), `internal/crypto/tenant_keys.go` (the unwrap moves out of the process), `cmd/tools/keybroker` or Trustee, `internal/auth/auth.go` (the secret read), `deploy/scripts/pg-backup.sh`
- **The rule:** answering a demand for one account never touches another account's keys, rows or logs, in any mode. Private: nothing to produce. Standard: one tenant's unwraps, each logged. Public: already public.
- **The key broker (F-20):** the master key lives only in the broker; a slot proves its identity with a TPM quote and receives one tenant's key at that tenant's first use. Every unwrap is a 43.6 entry (actor = the slot's build, opaque tenant id, reason = `serve` | `legal:<case>` | `ceremony`); an unwrap outside a deploy/restart window or with no matching request pattern fires an alert. The broker's own SNP report (TCB version, measurement) and a signed heartbeat are published daily.
- **`api_keys.secret_key` wrapped under the tenant's key** (today plaintext, `internal/auth/auth.go` — SigV4 needs the secret; a dump of the table signs as every tenant through the front door). Wrapped, the dump is useless without the broker and one tenant's secret can be produced without the others. Additive migration, lazy backfill at lookup, the mirror columns `tenants.access_key/secret_key` dropped with D-12.
- **The dumps encrypted before they exist on disk**: `pg_dump | gzip | age -r <offline recipient>` in `pg-backup.sh` (today gzip only; the 7-day on-box copy is plaintext, the off-box copy relies on Sync.com's own encryption); the restore drill in `docs/DEPLOY.md` decrypts with the offline identity; 43.3's "erased rows live ≤ 30 d in backups" stays disclosed.
- **No cross-tenant material anywhere:** chunk keys per tenant (WP-R8-1), no global convergent key (contract line 5), no tenant label in Prometheus (43.3), access logs scoped by tenant and on 43.3's clocks. A master-key recovery needs the escrow quorum with a trustee share outside the owner's jurisdiction, so no single person can be compelled into the whole.
- **Legal shape (43.4 gains):** every order is answered for the named tenant only; a request for "all data" or for the master key is contested as overbroad and, if it survives, is itself a logged event; the transparency report counts tenants affected per request.
- **Test:** a dump restored on a clean box yields no usable S3 secret and no tenant key → a broker log shows exactly the tenants served since the last deploy → a production for tenant X leaves every other tenant's unwrap count unchanged → a slot that cannot reach the broker serves the tenants it has cached and refuses new ones with 503 + an alert, never a 200 in plaintext.

### 43.8: Defensibility — what is a public event, and what we never hold (added 2026-10-14)
*Extends 43.6's entry list and 43.3's clocks. Each line is S; the Trust lane picks them up in this order.*
- **Boot-state and build transparency:** every deploy writes a 43.6 entry (git sha, the reproducible binary's hash — Phase 15.5's build made reproducible for `cmd/vaultaire` first — and a fresh fTPM PCR quote of the slot's host); a daily quote while nothing deploys. Anyone can see that SLC's boot chain changed outside a deploy. `/version` carries the binary hash so the entry can be checked against the running server.
- **Canary credentials:** one tenant and one dashboard account that exist only in the database and the backups, never used; any SigV4 request or sign-in with them is a page (`vaultaire_canary_hits_total`) and a 43.6 entry. A leaked dump announces itself.
- **The customer's own audit view:** every operator action on an account (suspend, quota, tier, bandwidth, key reads, the 43.6 "admin read of metadata" entries) is listed on the customer's dashboard with actor role, time and reason — the private counterpart of 43.6's public entry. 3.6 is the operator's viewer; this is the customer's.
- **Sub-processors and data locations, published:** `/legal/sub-processors` lists every party that can hold customer bytes or identity (Lyve, iDrive, Geyser, Sync.com, Cloudflare, Stripe, the e-mail provider, GitHub for the deploy identity) with location and what it holds per mode; a change is a 43.6 entry and a 30-day notice; the data-location map (SLC; the backends' regions; the backups' location) is on the same page.
- **Edge termination is a disclosure (F-17b gains a privacy dimension):** a proxied `s3` host means Cloudflare terminates TLS and sees Standard-mode bytes and SSE-C keys in transit; Private-mode traffic is ciphertext either way. The decision between proxied and direct is recorded with that sentence, and the sub-processor page says it.
- **Incidents:** a public incident entry within 72 h (the GDPR clock) on status.stored.cloud and in 43.6, with the tenants-affected count; the runbook's legal-hold step preserves the named tenant's rows only.
- **Two more clocks for 43.3's table:** `s3_access_log.object_key` replaced by a keyed hash once the customer's log delivery and the billing rollup have run (support keeps status and op); `user_agent` reduced to its family at write time. **Private mode pads sizes** (Padmé, from the 2026-10-11 research) so the one thing a ciphertext row still says — its length — says less.
- **Operator access policy, published:** dedicated service user (F-14), hardware-backed SSH identities, no shared accounts, no route that serves object bytes to an operator (`TestNoAdminRouteServesObjectBodies`, a structure test), Cloudflare Access in front of SSH (40.7).
- **Not added, on purpose:** a warrant canary as the signal (43.4's stance: a changed measurement outranks a canary that stops); client-side scanning (F-21); numbered accounts before demand (43.5).

**Test** (43.1–43.4; 43.6 has its own): the schema walk fails on a new PII column without an inventory row → a tenant row carries no e-mail anywhere outside `users` and `oauth_accounts` → a request-line log on the box carries no client IP → after the retention run, an `s3_access_log` row older than 24 h has an empty IP, one older than 7 d is gone, an `audit_logs` row older than 90 d has a null IP and its event intact → the privacy page, the DPA and `/legal/data-inventory` state the same periods (`TestRetention_PoliciesMatchThePolicyPage`) → `/legal/law-enforcement` renders the per-mode production list from the same table.

---


## Sub-steps added to existing phases (2026-10-01)

- **Keystones (2026-10-12) — built first, each small, each a contract the four lanes share:** **K1** streaming SSE (WP-R8-5: the encrypt/decrypt path never buffers an object — Private at any size) — **DONE 2026-10-13** (#708 SSE-S3 blob `0x03`, #709 SSE-C; `docs/reviews/WP-R8-5.md`); **K2** the SDK key core as one Go module compiled to WASM (one implementation of F-12's hierarchy for server tests, browser, CLI and the Ente-client bridge) — **crypto core DONE 2026-10-22** (`sdk/go/e2ee`: XChaCha20-Poly1305 box, convergent chunks under the space key, HPKE X-Wing post-quantum sealed box; `sdk/wasm` + `sdk/js/e2ee`, proven byte-identical in node; still to build: Argon2id KEK + recovery key, the hierarchy's wraps, manifests, the chunker in WASM, SRP); **K3** the `buckets.mode` column + `modes.json` + its test (41.0) — **DONE 2026-10-22** (#872 probe, #874 migration 102 `buckets.mode` GENERATED + `internal/modes` + the one write door `modes.SetBucket`, #875 the readers: management API, OpenAPI, dashboard labels and the privacy table behind `modes_copy` OFF; `docs/reviews/WP-P26.md`); **K4** the transparency-log library (`internal/translog`, 43.6) — **DONE 2026-10-22** (#879 the library: RFC 6962 log on `x/mod/sumdb/tlog`, C2SP signed-note checkpoints, proofs verified by `transparency-dev/merkle`, tlog-tiles paths; #880 **migration 103** store + signing key + `translog_checkpoint` job; #884 producers `server_build` / `key_ceremony`, the read side dark behind `translog_public`, `translog-verify`; `docs/reviews/WP-P28.md`, `docs/TRANSPARENCY.md`; K4b DONE 2026-10-23 = the witness, the byte-exact restore, trust.stored.cloud (`docs/reviews/WP-P32.md`); K4c = a third-party co-signature, OpenTimestamps, Rekor, admin-read entries, client builds); **K5** the recovery-vault interface (the Juicebox protocol; two realms under different control before Private is a consumer default, F-19) (`internal/recovery`: enrol / recover / attempt-count; an in-process realm for tests, the attested realm of 41.4 behind it). Built in the Engine lane; consumed by Developer (K2, K3), Trust (K4), Consumer (K2, K5). Also new: **WP-SSEC-1** (5.14.8a, SSE-C per bucket, the no-SDK Private path) and **WP-DOMAIN-1** (5.14.13, stored.cloud canonical before Oct 31).

- **18.7** Family accounts and organisation member management. **18.8** Department-level usage and a chargeback reporting API. **18.9** Custom-domain SSL provisioning for white-label domains. *(master 1310–1315)*
- **21.7** Compute contribution (GPU hours), credit transfer between users, a credit marketplace. *(master 565–568)*
- **22.5** Function marketplace: packaging, discovery, ratings, billing. **22.6** Raspberry Pi edge nodes (Pi-optimised agent, thermal and SD-card care). **22.7** GPU acceleration for transcode and inference — needs the hardware. **22.8** Edge analytics. **22.9** Edge security: node certificates, edge-to-edge encryption, audit. *(master 741–810)*
- **23.0** A bounded read cache, wired and measured — prerequisite for 23.2; the earlier cache was unbounded and was deleted in #524. **23.5** Access-log analytics pipeline. **23.6** Predictive maintenance from SMART data — needs own fleet (6.6). **23.7** Latency-aware routing. **23.8** Learned cache policy, A/B against LRU. **23.9** Performance and cost optimisation advisors. *(master 201–230, 611–710)*
- **24.4** Arrow Flight streaming results. **24.5** Delta-style transaction log with time travel. **24.6** File statistics, zone maps, partition pruning, aggregate pushdown. **24.7** Natural-language queries, with the generated SQL shown. **24.8** Query builder UI and cross-bucket joins. **24.9** Per-GB-scanned metering and query quotas. *(master 1222–1239)*


---

# SESSION HANDOFF (end of plan)
<!-- reconstructed 2026-07-07: the original closing handoff section was lost in the truncation; rewritten to match current state and the /next + /next-step skills -->

## How to end a session
1. Commit/merge all work (CI must pass — never `--admin`).
2. Update the **Status** line at the top of THIS file (`docs/IMPLEMENTATION_PLAN.md`) — it is the single source of truth `/next` and `/next-step` read.
3. Mark the finished phase with a `> **Phase X.Y COMPLETE** — PR #NNN` line in its section.
4. Add a dated `**Recent (YYYY-MM-DD)**` entry at the top if the session shipped something notable.
5. Update `.private/PROGRESS.md` (one line per PR) and the per-directory `CLAUDE.md` files you touched.
6. Note uncommitted decisions, known issues, and external state changes (Stripe products, server config, GitHub secrets) in the Status block or `.private/CLAUDE.md`.

## How to resume (for /next and /next-step)

0. (2026-10-12) Read `docs/ARCHITECTURE_CONTRACT.md` — the one page the four lanes (Engine, Developer surface, Consumer surface, Trust) share: modes per surface from `modes.json`, "any access a public event", F-12's key hierarchy, the attested plane, the copy rules — and the lane charter in `docs/surfaces/<LANE>.md` for the work at hand; every prompt names its lane and the keystone it stands on; owner decisions go to the weekly batch in `docs/STATUS.md`, never mid-prompt.
1. Read `docs/STATUS.md` first (since 2026-10-06: prod facts, the worker queue, the lanes, the next migration number), then the **Status** line at the top of this file for the history behind it.
2. Pre-launch (until 2026-10-31): two tracks. **Code** — take the first unfinished item of the pre-launch queue in the Status line (2026-09-30: WP-R10-9 → WP-R13-1 + WP-R13-3 → WP-R4-1 / WP-R12-5 / WP-R12-8 → WP-R11-3 → WP-R7-5 → encryption track → WP-R2-1 → WP-R10-3b); each row's files, size and dependencies are in `docs/reviews/SYNTHESIS.md` table A, the shape of a finished one is `docs/reviews/WP-R10-3-deletion-runner.md`, and the "Review 2026-09" section above lists every WP id. A WP ends with its note in `docs/reviews/WP-<id>.md`, its plan line moved to DONE, the Status line date bumped and the SYNTHESIS status cell flipped. **Owner** — the [YOU] rows of the SYNTHESIS pre-launch checklist and the launch stages (Stage 2 → 3 → 4 Stripe house prices → 5 status page, email → 5.5 CSAM/DMCA → 6 smoke + signups) per `.private/LAUNCH_EXECUTION_SEQUENCE.md`. After the Stage 6 code freeze (week of Oct 19) only owner items remain. After the queue: the **FULL-PLAN TRACK** section — take the first unfinished item of the lowest unfinished stage; a stage is finished when its items are merged, deployed and in use on tenant zero. Phases 22–39 are scoping text and are re-scoped in a docs PR before any prompt.
3. Read `.private/CLAUDE.md` (infra/creds), the per-directory `CLAUDE.md` files for packages you'll touch, and check `git log --oneline -5` + `git status`.
4. Migration numbers: `ls internal/database/migrations/ | tail -1` on a fresh `main` and take the next sequential (`073_site_stats.sql` is the latest as of 2026-09-30, so the next is `074`); then `make test-db` so the local `vaultaire_test` database carries it (never point tests at the dev DB `vaultaire`).
5. Branch per phase: `phase-X.Y-feature-name`, commit format `type(scope): description [Phase X.Y]`, squash-merge after CI.
