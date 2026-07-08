# Vaultaire Complete Implementation Plan

**Last updated**: 2026-06-04
**Status**: Phase 5.14 COMPLETE through 5.14.11 — all shipped & deployed to SLC prod: 5.14.1-5.14.4 (#253-256), 5.14.11 security hardening (#257), 5.14.5 HIPAA (#258), 5.14.6 GDPR/EU Data Act (#259), 5.14.7 per-bucket regions (#260), 5.14.8 SSE-C (#261), 5.14.9 access logging + inventory (#262), 5.14.10 compliance dashboard (#263). 5.11.0-5.11.12, 5.12.3-5.12.7, 5.13 complete. Gap-fill 5.10.17 object tagging shipped (#264). 5.15.1 graceful shutdown already in main.go (verify systemd TimeoutStopSec=45). **ALL gap-fills + 5.15 code COMPLETE:** 5.10.18 (#267), 2.7 (#269), 4.3 (#275), 3.6 (#276), 3.8 (#277), 3.9 (#278), landing 5.15.3 (#272), load-test gate 5.15.2 code (PR #282). **Remaining before launch = OPS only (no more code blocks Tier 1):** run the 5.15.2 load harness against live + fix what it surfaces; 5.15.4 smoke checklist; **5.15.5 prod backend/durability + Stripe meter activation + reopen signups (the true launch blocker)**; 5.14.12 status page (hosted). 5.14.12 status page = ops (hosted), not code.
**Before 5.15 (launch gate)**: gap-fills — ✅ 5.10.17 object tagging (#264); REMAINING: 5.10.18 (Content-Disposition, next), 2.7 (metered billing — revenue-critical, pay-per-TB tiers can't bill without it), 4.3 (bandwidth alerts — table exists, alert logic missing), 3.6 (audit viewer), 3.8 (revenue dashboard — only an estimate exists), 3.9 (cost dashboard). Then 5.15.3 landing page (code) + ops gates 5.15.2/5.15.4/5.14.12 + **5.15.5 (prod backend/durability + billing activation — the true launch blockers)** = launch ready. (5.15.1 done.)
**Post-launch, first 2 weeks**: 3.7 (customer support view), 3.10 (admin notifications), 3.11 (abuse queue).
**Cleanup**: delete orphan branch `phase-5.11.6-event-log-webhooks` (code is on main, merged via PRs #238/#239).
**Recent (2026-06-05)**: **Phase 8 COMPLETE.** Shipped 8.4.1 streaming PUT (#306), 8.6 GCI dedup dashboard (#307), 8.7 dedup GC with orphan reconciliation (#308), 8.8 dedup migration CLI (#309). Each reviewed by reading the actual code (the streaming/GC/migration phases carried real data-loss surfaces — all verified correct, e.g. GC's `last_accessed_at` grace guard protects in-flight PUTs, migration's verify-before-delete + flag-before-delete ordering). Also root-caused + fixed two recurring CI flakes (#310): `TestRequestQueue` (a real select race in `RequestQueue.Submit`) and `TestPipeline_Run` (racy transient-status assert). Chunking pipeline is now end-to-end + self-cleaning but **still ahead of launch** — 5.15.5 (prod durable backend + Stripe meter activation + reopen signups) remains the unstarted revenue blocker. Next in Tier-2 sequence: Phase 9 (compression).
**Recent (2026-06-04)**: Phase 8 chunking/dedup train shipped — **AHEAD of the launch sequence** (Phase 8 is post-launch on the roadmap; launch remains ops-gated on 5.15.4/5.15.5, unchanged by this work). PRs: 8.3-8.4 chunking migration (051) + dedup upload (#300), 8.5 dedup download (#301), `_global` shared chunk store fixing cross-tenant/cross-bucket dedup retrieval (#302), 8.6 bounded-streaming GET + per-chunk SHA-256 integrity (#303), SSE >256 MiB reject-guard — was silently storing plaintext (#304), 8.6.1 atomic manifest replacement on overwrite (#305). Three latent data-corruption/security bugs found & fixed by verification (cross-tenant 404, overwrite corruption, silent-plaintext). See the Phase 8 STATUS block for the sketch-vs-shipped map. Remaining chunking: **8.4.1 PUT-side streaming** (gap, now in plan as next), 8.6 GCI dashboard, 8.7 GC (delete from `_global`), Phase 10 real large-object encryption (interim: >256 MiB SSE rejected). Note: git "Phase 8.6/8.6.1" labels = 8.5 hardening, NOT plan 8.6.
**Recent (2026-05-31)**: Shipped 5.14.5→5.14.10 compliance sequence (#258-263) + 5.10.17 object tagging (#264) as a bottom-up stacked-PR train, all squash-merged and deployed to SLC prod (migrations 037-041 applied, verified `object_head_cache.tags` live). Fixed broken prod deploy — a 17-day-old orphan process held `/tmp/vaultaire-linux` busy (ETXTBSY blocked every scp); killed it and hardened deploy.yml to clear stale staging pre-scp, now self-healing (#265). Fixed flaky CI smoke test — `server.go:107` unconditionally health-checks `io.quotaless.cloud` at boot, whose DNS flakes in CI; added curl retries (#266). Quotaless removal DEFERRED to M12 exit (user not using it for a while); follow-up: make that boot health check conditional (it's the flake root cause). Orphan branch `phase-5.11.6-event-log-webhooks` still pending deletion.
**Recent (2026-05-26)**: Full plan audit. 5.14.1 GDPR export (PR #253), 5.14.2 legal docs (PR #254), 5.14.3 MFA Delete + SOC 2 (PR #255), 5.14.4 PQ SSE-S3 encryption (PR #256). 5.12.4 failover (PR #251), 5.12.5 backend health dashboard (PR #252). 5.11.12 CDN analytics shipped (PR #249), no longer stashed. 5.13 email infra shipped (PR #246). CI fixes (PRs #247, #248).
**Previous (2026-05-13)**: Phase 5.12.3 complete — iDrive + OneDrive fleet wired, end-to-end benchmarked on Mac + SLC. Pipeline components benchmarked (RS, FastCDC, zstd, AES-GCM). ContentLength passthrough added to engine PutOptions. Engine Delete fixed to target known backend only. Quotaless removed from SLC prod .env (account locked). OneDrive driver: streaming uploads (+150% concurrent), parallel range downloads (+28% single-file), DNS cache, fleet-wide TLS cache, token mutex, RateLimit tracking. iDrive driver: fixed-bucket pattern, materialize for Content-Length. Benchmark automation: `scripts/bench-vaultaire.sh`. Pipeline benchmark tool: `cmd/pipeline-bench/`.
**Previous (2026-05-12)**: Benchmark-driven optimizations — added Phase 5.12.7 (production transport tuning with adaptive MPU strategy: H1 vs H2, 4-16 parallel parts, 4MB buffers). Storage class routing added to 5.12.4. SSE-S3 managed encryption added to 5.14.4 (critical for Geyser multi-tenant). MFA Delete added to 5.14.3. Inventory reports added to 5.14.9. Cross-account access + IAM evaluator wiring added to 19.1. Lifecycle rules (12.1) expanded with S3 XML API compatibility. Quotaless endpoints updated (srv1/srv2/us DNS removed, account locked). Geyser: TLS certs now valid (InsecureTLS removable), H1 is 4.2x faster than H2 (Vail gateway preference).
**Recent (2026-06-02)**: Closed out all Tier-1 gap-fills + the 5.15 code gate. Shipped OneDrive/S3 upload perf (#279 parallel MPU for S3 backends, #280 HTTP/1.1 OneDrive upload +6-20%, #281 fleet-upload bench TEST 7). Key finding: OneDrive "12 MB/s" is per-single-file — multi-file + per-tenant concurrency (2.3x) gives 24-58 MB/s @ 3 tenants, ~100-200 fleet (NIC-bound); customers must parallelize. Landed 5.15.2 load-test gate (PR #282): SigV4 harness, 5 env-gated scenarios, pg pool 25→50. **Tier 1 is now code-complete; only ops gates (5.15.4/5.15.5) remain before launch.**
**Plan file**: `/Users/viera/.claude/plans/zazzy-growing-treasure.md`
**Master plan (historical)**: `/Users/viera/fairforge/vaultaire/.private/VAULTAIRE_MASTER_PLAN.md`

## Context

This plan consolidates and replaces the VAULTAIRE_MASTER_PLAN.md (steps 490+) with a practical, ordered roadmap. It covers everything from the dashboard/billing launch through the full storage pipeline, multi-protocol support, data intelligence, edge computing, federation, and beyond. Each phase is broken into small enough blocks that any session can pick up where the last left off.

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

Say: **"Continue from Phase X.Y — [sub-step name]. Plan: /Users/viera/.claude/plans/zazzy-growing-treasure.md"**

The new session should:
1. Read this plan file (check Status line for where we are)
2. Read `.private/CLAUDE.md` for infra/architecture context
3. Read the `CLAUDE.md` in directories it will modify (per-directory docs)
4. Check `git log --oneline -5` and `git status` for current state

### Tier 1 Handoff Points

| After Phase | What's Done | Resume With | Context for Next Session |
|-------------|-------------|-------------|--------------------------|
| **0** (done) | DB migration, LoadFromDB, templates, sessions, router | Phase 1.1 | Dashboard skeleton serves HTML. Login/register pages render but POST handlers aren't wired — Phase 1 adds those. `dashboard.Deps` struct passes auth + sessions + DB to handlers. |
| **1.3** | Login, register, dashboard overview, bucket browser | Phase 1.4 | Customer can log in and browse buckets. Handlers are in `internal/dashboard/handlers/`, templates in `templates/customer/`. Context helper (`dashauth.GetSession`) provides session data. |
| **1.7** | Full customer dashboard | Phase 2.1 | All customer pages work. API key management, usage charts, settings. The dashboard reads real data from `quotaManager` and `engine`. Next session wires Stripe. |
| **2.6** | Stripe billing wired end-to-end | Phase 3.1 | **First revenue checkpoint.** Registration creates Stripe customer. Checkout → webhook → subscription stored in DB. Billing page shows invoices. `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET` must be in env. |
| **3.5** | Admin dashboard complete | Phase 4.1 | Admin can see all tenants, suspend/enable, adjust quotas. Suspension enforced in S3 handler. Seed admin role: `UPDATE users SET role='admin' WHERE email='...'` |
| **5** | Security polish done | Phase 5.10.1 | CSRF, flash messages, rate limiting, responsive CSS, error pages all in place. Dashboard is production-ready for beta customers. |
| **5.10.9** | CDN + S3 core hardened | Phase 5.10.10 | Buckets table, public bucket toggle, CDN handler, range requests, conditional requests, CORS all working. `cdn.stored.ge/{slug}/{bucket}/{key}` serves public content with caching. Video streaming works. |
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
| **9.3** | Chunk + compress pipeline | Phase 10.1 | FastCDC chunking, global dedup via GCI, zstd compression. Upload pipeline: `chunk → compress → store`. This is the foundation encryption and erasure coding build on. |
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

## Migration Numbering

Migration numbers in this plan are **planning references**, not assignments. Existing migrations go up to `030_metadata.sql`. The next migration built gets `031`, regardless of which phase it comes from. Assign sequentially at build time — multiple phases reference the same numbers but only one migration can have each number. When starting a phase, check `ls internal/database/migrations/` for the latest number and use the next one.

## Documentation Practice

After completing each sub-step, update (or create) a `CLAUDE.md` in the directory you modified. These per-directory docs keep context local and prevent the root CLAUDE.md from bloating. Each should be <100 lines covering: what the package does, key types, how it connects to other packages, non-obvious decisions. Also document API endpoints, request/response formats, and error codes as they're built — this becomes the foundation for the developer docs in Phase 26.5.

## Critical Items That Must Start Early (regardless of phase order)

These have long lead times — start the PROCESS during Tier 1, even though full completion is later:
- **Free tier decision** (Phase 5.11.10) — decide limits at launch, announce day one
- **SOC 2 evidence collection** (Phase 28.1) — start logging/documenting during Tier 1, formal audit in Tier 2+
- **SLA definition** (Phase 28.2) — publish alongside paid plans in Phase 2
- **API versioning headers** (Phase 26.2) — add `X-Vaultaire-Version` response header in Phase 0 (costs nothing, prevents breaking changes later)
- **Multi-region planning** (Phase 28.3) — start when you get the dev server
- **`stored` CLI/TUI** (Phase 26, PULL FORWARD to ~5.7) — target market (r/datahoarder, r/cloudstorage, self-hosters) strongly prefers terminal over web UI. Ship alongside or shortly after billing. See `docs/PRODUCT_FEATURES.md` for full spec. "Just storage, no bloat" branding.
- **rclone one-liner** — `stored rclone-config` outputs pre-filled config. Ship with CLI.
- **Desktop sync client** — `stored mount` wraps rclone (Phase ~5.7). Branded Wails app post-launch if demand warrants. NOT Electron, NOT WASM.
- **Mobile app** — camera backup MVP (privacy pitch vs Google Photos). PWA first (~5.4), native iOS/Android in Tier 3-4 if demand. See `docs/PRODUCT_FEATURES.md`.
- **PWA manifest** — add during Phase 5.4 responsive CSS work. Cheap, tests mobile demand before building native.
- **JuiceFS + rclone guides** (Phase 5.10.14, PULL FORWARD to launch) — costs zero code, just docs. Each guide is a standalone LET/Reddit post. Write alongside S3 compat testing. The JuiceFS one-liner is the single best top-of-funnel for LET users.
- **stored-cache agent** (Phase 16.5, PULL FORWARD to ~post-launch month 2) — simple Go binary, high LET value. Can ship before NAS agent (Phase 16) and before federation (Phase 20). Independent of most Tier 2-3 work.
- **AI-native DX** — the foundation is stuff you're already building: S3 compat (done), complete OpenAPI (5.11.0), CLI (26.4), actionable errors (5.10.15). Publish `llms.txt` alongside OpenAPI completion — it's a plain text file, zero effort. MCP/plugin manifests are optional bonuses in Phase 26.8, not the core strategy. The CLI IS the AI integration.
- **Complete OpenAPI spec** (Phase 5.11.0, CRITICAL) — current spec covers ~60% of routes. Must reach 100% before SDK generation, AI integration, or interactive docs. Every AI agent, code generator, and developer tool reads OpenAPI first.

## Deferred Decisions (documented, not forgotten)

These were evaluated and deliberately postponed. Revisit at the noted trigger point:
- **Per-operation rate limits** — generic per-tenant rate limiting ships in 5.11.0. Per-operation limits (different rates for ListObjects vs PutObject) are overkill pre-scale. **Revisit when**: tenant count exceeds 1,000, or a single tenant's ListObjects calls cause measurable load. Until then, one limit per tenant is simpler to reason about, simpler to document, and simpler to support.
- **Full API version translation** — header-only versioning ships in 5.11.1 (`X-Vaultaire-Version` on every response). Full version translation (old API shapes → current format, like Stripe's version pinning) deferred to Phase 26.2. **Revisit when**: you need to make a breaking change to the management API. S3 wire protocol is version-less by design (AWS has never versioned S3), so this only applies to the JSON management API.
- **Custom S3 SDKs** — NOT building them. boto3, aws-cli, rclone, and every language's AWS SDK already work with stored.ge because we're S3-compatible. Only the management API needs custom SDKs (Phase 26.6). This is a strength: developers don't learn a new SDK, they use the one they already know.
- **LET community presence** — start lurking on LET during Tier 1. Comment on storage threads. Build credibility before posting offers. Don't wait for Phase 27 (GTM) to start community engagement.

## Known Gaps (implemented but incomplete — fill in when revisiting those areas)

Audit 2026-04-24 found these partial completions. Core functionality works; these are polish items to address before launch.
- **Phase 2.5 billing page gaps** — Invoice history not rendered (template doesn't call `GetInvoices()` despite it existing in `stripe.go`). No "Pause Subscription" toggle, predictive billing, add-on management, or prepaid credit display. Stripe Portal covers invoices/pause for now. **Fill in during**: 5.11.8 (Cost Comparison Widget) when the billing page is already being expanded.
- **Phase 4.3 bandwidth banking + alerts** — DB schema exists (migration 020: `bandwidth_rollover`, `bandwidth_alerts` tables) but zero application code for rollover calculation, alert threshold checking, or overage warnings. **Fill in during**: 5.11.10 (Free Tier) or 5.11.12 (Bandwidth Budgets) when quota enforcement is being refined.

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
- 5.14.8: SSE-C customer-provided encryption keys
- 5.14.9: Server access logging and inventory reports (S3-compatible audit trail)
- 5.14.10: Customer compliance dashboard + 5.14.11: Security hardening (headers, request limits, admin 2FA enforcement)

- 5.14.12: Public status page (status.stored.ge)

## Vault18 Deep Archive Sub-Phases

*Vault18 is the "Anti-Glacier" product: $1/TB launch promo, zero retrieval fees, dual-site tape (LA + London). Sits between Phase 5.14 and Phase 5.15 in the Tier 1 sequence. Full plan: `.private/VAULT18_LAUNCH.md`.*

| Phase | Description | Status |
|-------|-------------|--------|
| V18.1 | Geyser tape ingest buffer pipeline (iDrive hot → Geyser cold) | NOT STARTED |
| V18.2 | Staged retrieval with SLA guarantees (1h standard, 5min expedited) | NOT STARTED |
| V18.3 | Dual-site tape replication (LA + London Geyser endpoints) | NOT STARTED |
| V18.4 | Glacier migration tooling (`aws s3 sync` from Glacier → stored.ge) | NOT STARTED |
| V18.5 | Retention policies + immutability certificates (WORM integration) | NOT STARTED |
| V18.6 | Cost calculator (vs Glacier, Wasabi, B2 — show $0 retrieval savings) | NOT STARTED |
| V18.7 | Pack pricing integration (Vault3/9/18/36 Stripe products) | NOT STARTED |
| V18.8 | Customer-facing deep archive experience (dashboard, restore status) | NOT STARTED |

---

# TIER 1: DASHBOARD & BILLING LAUNCH (Ship before accepting paying customers)

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

> **Phase 2 COMPLETE** — PR #195 (catch-up 3.1-5.2 including billing), PR #269 (2.7 metered billing).

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
- Plan mapping: free (5GB), Vault3 (3TB), Standard (per-TB metered), Performance (per-TB metered)
- Downgrade guard: if current usage > new plan limit, warn but don't delete data

### 2.5: Billing Dashboard Page
**File**: `internal/dashboard/handlers/billing.go`
- Current plan name, status, next billing date
- Link to Stripe Portal for invoice history and payment management
- Usage vs. quota progress bars

### 2.6: Plan Upgrade/Downgrade
**File**: `internal/dashboard/handlers/billing.go`
- Three-column pricing card (Vault / Standard / Performance)
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
- Tenant slugs for CDN URLs: `cdn.stored.ge/{slug}/{bucket}/{key}`
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
- `Server: stored.ge` header (branding + diagnostics)
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
**File**: `internal/api/events.go`, `internal/api/webhooks_api.go`, new migration
**Depends on**: 5.11.0 (management API for route registration and response envelope)
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
- "Copy CDN URL" button per object (one-click clipboard copy)
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

### 5.12.1: Quotaless Production Driver Fix ⏸ BLOCKED (account locked 2026-05-12)
**Files**: `internal/drivers/quotaless.go`
- Account locked as of 2026-05-12 — contact Quotaless support to re-enable
- Quotaless removed from SLC production .env (was causing 401 retry storms on every Delete)
- Still wired in main.go code but won't activate without QUOTALESS_ACCESS_KEY in env
- When re-enabled: apply `SwapComputePayloadSHA256ForUnsignedPayloadMiddleware`, fix incompatible S3 ops
- Auto-detect priority in main.go is now: `iDrive > Quotaless > S3 > Geyser > local`
- Reference: `cmd/quotaless-bench-v2/main.go`, `internal/drivers/quotaless_README.md`

### 5.12.2: Cloudflare R2 Driver
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
- Engine currently selects primary/backup but failover is manual
- Automatic failover: if primary returns error or health check fails, retry on backup
- Backend priority list per operation type (read vs write)
- Circuit breaker pattern: after N consecutive failures, mark backend degraded for M seconds
- Prometheus metrics: `vaultaire_backend_failover_total{from, to}`, `vaultaire_backend_health{name}`
- Graceful degradation: if all backends down, return 503 with `Retry-After` header
- **Storage class mapping**: honor `x-amz-storage-class` header on PUT — `STANDARD` → iDrive, `STANDARD_IA` → Lyve (zero egress), `GLACIER` → Geyser, `DEEP_ARCHIVE` → Geyser+airgap, `ONEZONE_IA` → OneDrive (parity-only, not customer-facing). Return correct class on HEAD/GET. Default `STANDARD` if omitted. Enables S3-compatible lifecycle transitions and customer tier control via standard AWS tooling. (Quotaless mapping removed — account locked.)

### 5.12.5: Backend Health Dashboard
**Files**: `internal/dashboard/handlers/admin_backends.go` (new), `templates/admin/backends.html` (new)
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
- Track per-backend storage and egress costs from provider billing
- Input: manual entry or API polling (iDrive reseller API when available)
- Output: blended cost per TB, per-backend breakdown, margin calculation
- Dashboard: admin cost page with per-backend COGS, trend charts
- Data feeds into tiering engine (Phase 7) for cost-optimized placement

### 5.12.9: Backend Capacity Planning
**Files**: `internal/engine/capacity.go`
- Track available capacity per backend
- Alert at 80% capacity: "iDrive account approaching storage limit"
- Capacity forecast: based on current growth rate, when will each backend fill?
- Auto-provisioning hooks (future: iDrive reseller API creates new sub-accounts)

**Test**: Check admin backend dashboard shows all backends with correct health/latency → verify cost tracking aggregates correctly → capacity alerts fire at threshold.

---

## Phase V18: Vault18 Deep Archive Experience
*Depends on: Phase 5.12 (Geyser backend wired), Phase 5.14 (compliance for immutability). The "Anti-Glacier" product.*

*Reference: `.private/VAULT18_LAUNCH.md` for pricing, community posts, FAQ. `.private/VAULT_SERIES_ECONOMICS.md` for per-tier COGS.*

### V18.1: Ingest Buffer Pipeline
**Files**: `internal/engine/ingest_buffer.go` (new)
- Hot ingest to iDrive (fast, 852 MB/s) → background migration to Geyser tape (slow, 22 MB/s)
- Customer sees immediate upload completion; tape write is async
- Progress tracking: `ingest_buffer_status` with per-object state (ingested, migrating, archived)
- Retry logic for Geyser failures (tape seek timeout, connection reset)

### V18.2: Staged Retrieval with SLA
**Files**: `internal/engine/retrieval.go` (new), `internal/api/s3_handler.go`
- Standard retrieval: 1 hour (queue + Geyser fetch)
- Expedited retrieval: 5 minutes (pre-cached hot copies for frequent restores)
- `x-amz-restore` header compatibility (like Glacier RestoreObject)
- Webhook notification when retrieval completes (uses Phase 5.11.6 webhook system)

- Webhook notification when bulk retrieval completes (uses Phase 5.11.6 webhook system)

### V18.3: Dual-Site Tape Replication
**Files**: `internal/drivers/geyser.go`
- Write to both LA and London Geyser endpoints
- Geographic redundancy: survive datacenter loss
- Async replication: primary write to nearest site, background copy to remote
- London bucket re-creation needed (deleted 2026-04-20)

### V18.4: Glacier Migration Tooling
**Files**: `cmd/glacier-migrate/` (new)
- CLI tool: `glacier-migrate --source s3://bucket --dest stored.ge/bucket`
- Wraps `aws s3 sync` with Glacier restore orchestration
- Handles restore requests, waits for availability, copies to stored.ge
- Progress reporting with ETA

### V18.5: Retention Policies + Immutability
**Files**: `internal/api/s3_handler.go` (extends Object Lock from 5.10.13)
- Vault18-specific retention templates: 1yr, 3yr, 7yr, 10yr
- Immutability certificates: cryptographic proof that an object hasn't been modified
- Integration with Object Lock COMPLIANCE mode

### V18.6: Cost Calculator
**Files**: `internal/dashboard/handlers/billing.go`, template
- Interactive calculator: enter TB, access frequency → show monthly cost
- Comparison table: stored.ge Vault18 vs Glacier vs Glacier Deep Archive vs Wasabi vs B2
- Highlight: "$0 retrieval" vs Glacier's $90/TB retrieval fee

### V18.7: Pack Pricing Integration
**Files**: `internal/billing/stripe.go`
- Stripe products for Vault packs: Vault3 ($7.47/mo), Vault9 ($22.41/mo), Vault18 ($44.82/mo)
- Launch promo: $1/TB for first 5 packs (first 100TB total)
- Pack subscription management in dashboard

### V18.8: Deep Archive Dashboard
**Files**: `internal/dashboard/handlers/archive.go` (new), templates
- Archive-specific view: objects in storage, retrieval queue, retention status
- Restore request UI: select objects → choose speed (standard/expedited) → track progress
- Immutability certificate download per object
- Tape location indicator (LA / London / both)

**Test**: Upload to Vault18 tier → verify ingest buffer accepts immediately → background migration to Geyser starts → request restore → receive webhook when ready → download → verify retention policy prevents deletion → cost calculator shows savings vs Glacier.

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
- Base email template: stored.ge header, content, footer with unsubscribe
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

### 5.14.3: MFA Delete + SOC 2 Evidence
**File**: `internal/api/s3_handler.go`, `internal/compliance/soc2.go`, migration `036_mfa_delete.sql`
> **Phase 5.14.3 COMPLETE** — PR #255
- MFA Delete: `x-amz-mfa` header required to delete versioned objects when enabled
- TOTP verification inline with S3 delete request
- SOC 2 evidence tracking: `internal/compliance/soc2.go` maps Trust Services Criteria to implemented controls
- Evidence collection started (CC6.1 logical access, CC7.1 monitoring, CC8.1 change management)
- Formal audit deferred to Q4 2026 (Type I), H1 2027 (Type II observation)

### 5.14.4: Post-Quantum SSE-S3 Encryption
**File**: `internal/crypto/sse_s3.go`, `internal/crypto/postquantum.go`, migration `037_sse_s3.sql`
> **Phase 5.14.4 COMPLETE** — PR #256
- ML-KEM-768 (NIST FIPS 203) for key encapsulation — quantum-resistant
- AES-256-GCM for symmetric data encryption — hardware-accelerated
- Hybrid: ML-KEM encapsulates shared secret → HKDF → AES key → encrypt
- Per-tenant keypair stored in `sse_s3_keys` table
- Transparent: enabled via `ENCRYPTION_MASTER_KEY` env var (64 hex chars)
- Objects encrypted on PUT, decrypted on GET — caller sees plaintext
- This is server-side encryption (stored.ge holds keys). Client-side E2EE is Phase 10.0.
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
- Encrypt on PUT, decrypt on GET — transparent to S3 API
- Separate from SSE-S3 (5.14.4) — SSE-C is customer-managed, SSE-S3 is server-managed

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
- Per-bucket status: encryption (SSE-S3/SSE-C), versioning, Object Lock, region, access logging
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
- Public at `status.stored.ge`
- RSS feed for status updates
- Incident template: what happened, impact, timeline, resolution, prevention
- **Why now**: customers need to know if the service is up BEFORE contacting support. Trust signal from day one.

---

## Phase 5.15: Pre-Launch Validation
*After 5.14 compliance is done. Final gate before accepting paying customers. All items here are operational, not feature work.*

### 5.15.1: Graceful Shutdown
**Files**: `cmd/vaultaire/main.go`, `internal/api/server.go`
- `os.Signal` handler (SIGTERM, SIGINT) → stop accepting new connections → drain in-flight requests (30s timeout) → close DB pool → close Redis → exit cleanly
- Without this, every deploy drops active uploads mid-stream
- Systemd: `TimeoutStopSec=45` in vaultaire.service (gives 30s drain + 15s buffer)
- Log on shutdown: "draining N in-flight requests" + "shutdown complete"

### 5.15.2: Load Testing
**Files**: `tests/load/` (new, gitignored results)
- Use `k6` or `hey` against staging (or SLC with test tenant)
- Scenarios: 100 concurrent S3 PUT (1MB), 100 concurrent GET, 50 concurrent multipart (100MB), mixed read/write, management API burst (100 req/s per tenant)
- Validate: no 5xx under load, p99 latency < 500ms for GET, rate limiter kicks in correctly, DB connection pool doesn't exhaust, memory stays bounded
- Identify: connection pool limits, goroutine leaks, slow queries under concurrency
- Run once, fix issues, run again. Not a recurring suite — a one-time gate.

### 5.15.3: Landing Page
**Files**: `internal/dashboard/templates/public/landing.html` (new), `internal/dashboard/handlers/public.go` (new)
- `GET /` for unauthenticated users → marketing landing page (authenticated users redirect to `/dashboard`)
- Single page: hero ("$3.99/TB S3-compatible storage"), feature grid (zero egress, PQ encryption, S3 compatible), pricing table (Standard/Performance/Vault tiers), "Get Started" CTA → `/register`
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
  - [ ] `aws s3 cp` and `rclone` work against `s3.stored.ge`
  - [ ] Password reset flow works end-to-end
  - [ ] 2FA setup + login works
  - [ ] Health endpoint returns 200: `curl https://stored.ge/health`
  - [ ] SSL certificate valid, HSTS header present
  - [ ] Backup restore test: restore latest pg_dump to a test DB, verify data intact

### 5.15.5: Production Backend & Activation Gate
*Added 2026-06-02 (prod audit). The real blockers to accepting paying customers — NOT covered by 5.15.2 (load) or 5.15.4 (pg backup/SSL/smoke). Prod must be in this state before signups reopen.*
- **Prod on a real backend, NOT local disk.** Currently `/opt/vaultaire/configs/.env` has `STORAGE_MODE=quotaless` but no backend credentials, so the app falls back to the `local` filesystem (`/opt/vaultaire/data` on the single SLC box). Before launch: set real backend creds (iDrive/Geyser/whichever tier), confirm `/health` shows the active backend is not `local`, and migrate or discard the bench/test data.
- **Object-data durability.** 5.15.4's backup test covers Postgres only. Objects on local disk have NO backup/redundancy — a disk/box loss = total data loss. Confirm objects live on a durable backend (the cloud tier replicates) before taking customer data.
- **Metered billing activated.** 2.7 (#269) built the reporter but it's DORMANT. Create the two Stripe Billing Meters (storage = "last value" aggregation, egress = "sum"), attach to the Standard/Performance metered prices, set `STRIPE_METER_STORAGE` / `STRIPE_METER_EGRESS` in prod `.env`. Without this, metered tiers accrue usage but never bill.
- **Reopen signups.** Public signups are CLOSED pre-launch (`SIGNUPS_ENABLED=false`, gated at `auth.CreateUserWithTenant`; PR #274). Set `SIGNUPS_ENABLED=true` + restart — but ONLY after the backend/durability items above are done. Never take customer data onto an unbacked local disk.
- **TLS auto-renewal.** 5.15.4 checks the cert is valid; also confirm auto-renewal is configured (current cert expires 2026-07-28 — ~launch week — via Cloudflare/Google; normally auto-renews, verify).
- **Cloudflare:** cache rule for `/` (offload landing spikes) + a rate-limit/bot rule on `/api/waitlist` and `/auth/register`.
- **Why**: 5.15.2 + 5.15.4 gate *behavior*; this gates *the operational substrate*. Finishing 5.15.1-5.15.4 without this = "feature-complete" but customer data lands on a single unbacked disk. This is the true "can I charge a customer" line.

---

# TIER 2: COST OPTIMIZATION (Build after first customers)

---

## Phase 10.0: True End-to-End Encryption (Client-Side, Zero-Knowledge)
*Depends on: 5.14.4 (PQ encryption primitives). Separate from Phase 10 convergent encryption — this is client-side, that is server-side. Both coexist.*

### 10.0.1: Client-Side Encryption SDK
**Files**: `sdk/go/e2ee/` (new), `sdk/js/e2ee/` (new)
- Go + JavaScript libraries for client-side encryption
- ML-KEM-768 + AES-256-GCM (same primitives as 5.14.4, but runs on client)
- Encrypt-then-upload pattern: data never leaves client unencrypted
- Key derivation: `HKDF(user_password, salt)` → master key → per-object DEK
- Streaming encryption: `io.Reader` wrapper, AES-256-GCM with 64KB blocks
- `x-amz-meta-e2ee-version: 1` metadata tag on encrypted objects (server can't read content but can route/store)
- Key backup: encrypted key blob stored on server, recoverable with password

### 10.0.2: Browser-Side Encryption
**Files**: `sdk/js/e2ee/` (new), dashboard integration
- WebCrypto API for in-browser encryption before upload
- Pre-signed URL + client-side encrypt → browser-direct upload of encrypted data
- Dashboard toggle: "Enable client-side encryption for this bucket"
- Performance: ~200 MB/s in modern browsers (WebCrypto is hardware-accelerated)

### 10.0.3: Key Recovery + Sharing
**Files**: `sdk/go/e2ee/keyshare.go`
- Recovery phrase: BIP-39 mnemonic (24 words) shown once at setup
- Key sharing: ML-KEM encapsulate to recipient's public key
- Shared buckets: all members derive same DEK from shared secret
- Revocation: re-encrypt with new key, old members lose access

### 10.0.4: E2EE Dashboard Experience
**Files**: `internal/dashboard/handlers/encryption.go` (new)
- Encryption status indicator per bucket (server-side SSE vs client-side E2EE)
- Key management UI: generate, backup, share, rotate
- Warning: "You control the keys — if you lose your recovery phrase, data is unrecoverable"
- File preview disabled for E2EE buckets (server can't decrypt)

**Test**: Generate keys → encrypt file client-side → upload → verify server sees only ciphertext → download → decrypt client-side → compare with original → share key with another user → they can decrypt → revoke → they can't.

---


## Phase 6: Multi-Backend Wiring

> **✅ Phases 6.1-6.4 COMPLETE** — done out-of-order via Phase 5.12.3 (PR #244, 2026-05-13).
> All drivers (Lyve, iDrive, Geyser, OneDrive) implement `engine.Driver` and are wired in `main.go`.
> Auto-detect priority: `iDrive > Quotaless > S3 > Geyser > local`. Full end-to-end benchmarks on Mac + SLC.
> Skip to Phase 6.5 (Filecoin) or Phase 7 (Smart Tiering) when ready.

### 6.1: Wire Seagate Lyve Cloud ✅ (pre-existing, benchmarked in 5.12.3)

### 6.2: Wire iDrive ✅ (5.12.3, PR #244)

### 6.3: Wire Geyser Tape ✅ (pre-existing, benchmarked in 5.12.3)

### 6.4: Backend Configuration ✅ (5.12.3, PR #244)

### 6.5: Backend Research & Tier Architecture (Filecoin, Sia, Pixeldrain, Quotaless)
> **UPDATED 2026-04-18**: Comprehensive benchmarking of ALL providers completed. Tier architecture finalized:
> - **iDrive E2** = Performance tier (579 MB/s, $3.30/TB, default)
> - **Quotaless** = Bulk/Egress tier (393 MB/s, €0.60/TB at 100TB, FREE unlimited egress) — replaces Lyve gap
> - **Geyser** = Archive ($1.55/TB tape), **Vault18** = Deep Archive ($1/TB)
- **Pixeldrain** = CDN cache layer (808 MB/s download, not a storage tier)
- **Lyve Cloud** = High-throughput egress-free tier (380 MB/s sustained)
- See `.private/TIER_STRATEGY.md` for the three-tier GTM: Vault (archive), Standard (smart), Performance (B2 killer).

---

## Phase 7: Smart Tiering Engine
*Depends on: Phase 6 (multiple backends wired). Uses `object_locations` table to track where each object lives.*

> **Phase 7 COMPLETE** — PRs #297 (7.1-7.4), #298 (7.5), #299 (7.6). Shipped ahead of launch as part of Tier 2 work.

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
- Map stored.ge tenant_id → iDrive sub-account email
- Credentials stored encrypted in tenant metadata

### R3: Quota Synchronization
- Sync stored.ge plan quotas → iDrive sub-account quotas
- On plan change: update iDrive quota automatically
- Over-quota handling: iDrive rejects writes, stored.ge shows clear error

### R4: Usage Monitoring + Billing
- Poll iDrive API for per-sub-account usage
- Cross-reference with stored.ge bandwidth tracking
- Cost reconciliation: iDrive invoice vs. customer revenue

### R5: Driver Pool (Multi-Account)
**Files**: `internal/drivers/idrive_pool.go` (new)
- Pool of iDrive sub-account drivers (one per tenant with dedicated account)
- Load balancing across sub-accounts for rate limit distribution
- Health check per sub-account

### R6: White-Label Ready
- Remove iDrive branding from customer-facing surfaces
- Custom S3 endpoint per sub-account if supported
- Branded email notifications for iDrive operations

**Test**: Create tenant → provision iDrive sub-account → upload via stored.ge → verify object on iDrive sub-account → check usage sync → upgrade plan → verify quota increase on iDrive.

---


## Phase 8: FastCDC Chunking + Global Dedup
*Depends on: Phase 6 (needs backends to store chunks across). Can start without Phase 7.*

**STATUS (2026-06-04):** 8.1–8.5 shipped + hardened. Reality differs from the sketch below — actual artifacts:
- **8.1 chunker** ✅ `crypto/chunker.go` (`DefaultFastCDCChunker`, 1/4/16 MB, SHA-256). A streaming `Chunk()` exists but the PUT path does NOT use it yet — see **8.4.1**.
- **8.2 GCI** ✅ `crypto/gci.go` — Postgres-backed with a 100K-entry in-memory cache (NOT Redis/Bloom as sketched; revisit only if lookup latency bites at scale).
- **8.3 migration** ✅ shipped as `051_chunking_dedup.sql` (not `021`), tables `global_content_index` / `tenant_chunk_refs` / `object_metadata` + `object_head_cache.is_chunked` (not `chunk_manifests`/`chunk_locations`).
- **8.4 upload** ✅ `api/s3_engine_adapter.go handleChunkedPut` (PR #300). Hardened: chunks live in a shared `_global` container so cross-tenant/cross-bucket dedup is retrievable (PR #302); atomic manifest replacement on overwrite via `ReplaceObjectManifest` (PR #305). **Open gap → 8.4.1 (PUT still buffers the whole object).**
- **8.5 download** ✅ `handleChunkedGet` (PR #301), then bounded per-chunk streaming + SHA-256 integrity verification (PR #303). Fetch is **sequential** — the sketch's parallel fetch + prefetch are DEFERRED.
- **SSE interaction**: a >256 MiB object in an SSE-required context is now **rejected with 413** instead of silently stored plaintext (PR #304). The real fix (actually encrypting large objects) is **Phase 10** convergent encryption.
- **⚠ Numbering note**: git commits labeled the streaming/integrity/overwrite work "Phase 8.6 / 8.6.1" — that was **8.5 hardening**, NOT the 8.6 below. Plan **8.6 (GCI Dashboard) ✅ DONE (#307); 8.7 (GC) ✅ DONE (#308); 8.8 (migration tool) ✅ DONE (#309). Phase 8 COMPLETE. Next in Tier-2 sequence: Phase 9 (compression).**

### 8.1: FastCDC Chunker
**File**: `internal/crypto/chunker.go`
- Content-defined chunking with rolling hash (Buzhash)
- Avg 4MB chunks (min 1MB, max 16MB)
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

### 8.4.1: Streaming Chunked Upload (memory) — NOT DONE
*Gap flagged 2026-06-04. Not urgent while chunking sits behind the 64 MB threshold and out of prod traffic; do BEFORE large-object uploads go to production.*
- **Problem**: `handleChunkedPut` does `io.ReadAll` of the whole object before chunking, so peak memory scales with object size (a 2 GB upload = 2 GB RAM) — violates the "always stream, never buffer" architecture decision. (GET was fixed to bounded streaming in PR #303; PUT is the remaining half.)
- Feed the existing `hashingBody` (TeeReader→MD5) into the chunker's streaming `Chunk(ctx, r)` API; **add context cancellation to `Chunk()`** so an aborted upload doesn't leak its goroutine.
- **Commit to the chunked path** once the gate fires — do NOT fall through. (The current fallthrough is already broken: `ReadAll` drains the body, so the normal-path retry would store 0 bytes.) Return 5xx on failure; the client retries.
- Per-chunk (or windowed-batch) dedup lookup as chunks arrive; use the **measured** total size, not the declared `Content-Length`.
- Peak memory → one chunk + chunker buffer (~16–32 MB), matching the GET side.

### 8.5: Dedup-Aware Download Pipeline
- On Get: read manifest → fetch chunks (parallel) → reassemble → stream to client
- Chunk prefetching for sequential reads
- **DONE 2026-06-04** (PR #301 + #303): `handleChunkedGet` streams chunk-by-chunk into a bounded ~16 MB buffer, verifies each chunk's SHA-256 before serving, and serves ranges by selecting only overlapping chunks via `chunk_offset`. **Fetch is sequential — parallel fetch + prefetch above are DEFERRED** (revisit when chunked objects are large/hot enough to matter).

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

### 10.1: Hybrid PQ Encryption
**File**: `internal/crypto/encryptor.go`
- **ML-KEM-768** (NIST FIPS 203) for key encapsulation — quantum-resistant
- **AES-256-GCM** for symmetric encryption — fast, hardware-accelerated
- Hybrid: ML-KEM encapsulates shared secret → HKDF → AES key → encrypt

### 10.2: Key Hierarchy
**File**: `internal/crypto/keymanagement.go`
- Tenant master key (from KMS or password-derived)
- Per-object DEK: random, wrapped with tenant master key
- Convergent DEK for dedup: `HKDF(tenant_master_key, chunk_content_hash)`

### 10.3: Convergent Encryption
- Same chunk + same tenant → same ciphertext → dedup still works
- Cross-tenant dedup: opt-in shared derivation namespace (enterprise)

### 10.4: Database Migration
**File**: `internal/database/migrations/022_encryption.sql`
- `encryption_keys` table, `encrypted_chunks` metadata
- Add `encrypted` boolean to `chunk_manifests`

### 10.5: Customer Key Management
- Dashboard: enable/disable E2EE per bucket
- Key backup/recovery (show recovery phrase once)
- Key rotation (background re-encryption job)

### 10.6: Proof-of-Ownership Protocol
- Client uploads Merkle tree of chunk hashes
- Server challenges: "prove you have chunk N"
- Client responds with chunk + Merkle proof
- Prevents hash-table poisoning and confirmation attacks

### 10.7: Security Testing
- Constant-time hash comparisons (timing attack prevention)
- Rate limiting on hash lookups (1000/sec per tenant)
- Honeypot hashes to detect attackers
- Cross-tenant isolation audit

**Test**: Verify raw chunks on backend are unreadable ciphertext. Upload same file twice → verify dedup still works despite encryption.

---

## Phase 11: Reed-Solomon Erasure Coding
*Depends on: Phase 6 (needs multiple backends for shard placement). Phase 10 (encrypt before erasure coding). Full pipeline order: chunk → compress → encrypt → erasure code → store.*

### 11.1: Reed-Solomon Implementation
**File**: `internal/crypto/erasure.go`
- RS(14, 10): 10 data + 4 parity shards
- 1.4x overhead (vs 3x replication)
- Uses `klauspost/reedsolomon` (SIMD-optimized)

### 11.2: Shard Placement Strategy
- Spread 14 shards across backends: Lyve (5), iDrive (4), Geyser LA (3), Geyser London (2)
- No single backend failure loses >5 shards → data survives
- Placement considers: health score, latency, cost, region

### 11.3: Pipeline Integration
- After encryption: `chunk → compress → encrypt → erasure encode → store shards`
- On read: `fetch ≥10 shards (parallel) → decode → decrypt → decompress → reassemble`

### 11.4: RaptorQ for Large Objects
**File**: `internal/crypto/raptorq.go`
- Fountain codes for objects >1GB
- Rateless: generate unlimited repair shards on demand
- Better performance than RS for large objects

### 11.5: Locally Repairable Codes (LRC)
- Local parity groups (Azure-style)
- Faster local repair without full reconstruction
- Configurable per tier

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

# TIER 3: SCALE & FEATURES (Build once you have product-market fit)

---

## Phase 12: S3 Lifecycle Rules
*Depends on: Phase 7 (tiering engine for tier transitions)*

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

### 13.3: SFTP Server
**Files**: `internal/protocol/sftp/` (new)
- SSH key authentication → tenant lookup
- `ls`, `get`, `put`, `rm`, `mkdir`, `rmdir` map to engine operations
- Subsystem handling: `sftp-server` compatible
- Use `pkg/sftp` (Go SFTP library)
- Port: configurable, default 2222

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

**Test**: Upload mix of objects → access some frequently, leave others untouched → analyzer classifies correctly → cost advisor suggests tier changes → apply suggestion → verify cost reduction.

---

## Phase 15: Self-Hosted Distribution
*Depends on: Phase 5 (dashboard), Phase 6 (multi-backend)*

### 15.1: Docker Image
**Files**: `Dockerfile` (new), `docker-compose.yml` (new)
- Multi-stage build: Go build → Alpine runtime
- Single binary + embedded templates = small image (<50MB)
- `docker-compose up` with PostgreSQL service for quick start
- Environment variables for all configuration

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

### 15.4: Documentation Site
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
- Custom domain per reseller (CNAME → stored.ge)
- Custom branding: logo, colors, name
- Custom email templates with reseller's domain

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

# TIER 4: PLATFORM SCALE (Build when you're a real business)

---


## Phase 21: Storage Cooperative + BYOS
*Depends on: Phase 20 (federation protocol), Phase 7 (tiering engine)*

**The LET pitch**: "Bring your own storage, earn credits, join the network."


> Phase 21 turns this into a symbiotic network: users get cheap storage, stored.ge gets global edge nodes.


### 21.1: Credit System
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
- **Install**: `curl -sSL https://stored.ge/install-spoke.sh | sh` (Docker one-liner or static binary)
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
