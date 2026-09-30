# Product Features Roadmap

> Roadmap/feature notes — not customer-facing; prices live in `internal/api/landing/prices.json`. Written for the pack era; statuses and paths were corrected 2026-09-30 (Review R14). "Proposed" marks a file or directory that does not exist.

Comprehensive list of customer-facing features, organized by when they can be built.
Each feature references its implementation phase in the master plan.

## Phase 2: Billing Foundation (historical — the house is quota-sold, see the status notes)

### Multi-Item Subscriptions (Add-ons)
**What:** Customers can add/remove individual services (object lock, priority egress, extra storage blocks) without canceling their base plan. Uses Stripe Subscription Items — each add-on is a separate line item on one subscription.
**Where:** `internal/billing/stripe.go` — add `AddSubscriptionItem()`, `RemoveSubscriptionItem()`
**Phase:** 2.1-2.6

### Metered (Usage-Based) Billing — NOT PURSUED
**Decision (2026-09-21, R10):** stored.ge is quota-sold — whole TB, flat rate, no overage, egress never billed. `internal/billing/metered.go` and `STRIPE_METER_*` exist but stay unset; there is no usage-based line on any invoice.
**Phase:** dropped

### Pause/Resume Subscription
**What:** Customer pauses billing — data stays, no charges. Stripe supports this natively. Huge for seasonal users; nobody in cheap storage offers this.
**Where:** `internal/billing/stripe.go` — `PauseSubscription()`, `ResumeSubscription()` using Stripe's `pause_collection`
**Phase:** 2.5

### Prepaid Credits
**What:** Buy $50 credit, draw down against usage. Great for burst backups. Uses Stripe customer credit balance.
**Where:** `internal/billing/credits.go` (proposed — does not exist) — `AddCredit()`, `GetBalance()`, Stripe customer balance API
**Phase:** 2.5

### Grace Period on Failed Payments
**What:** 3/7/14 day email warnings before any access restriction. Never delete data on payment failure. Builds trust.
**Status:** dunning is delegated to Stripe; the app has no suspension-on-non-payment code (R10-35). `OverageService` is gone with the overage model.
**Where:** `internal/billing/webhook.go`, email notifications
**Phase:** 2.2 (webhook), 4.1 (enforcement)

### Instant Plan Switching
**What:** Upgrade/downgrade mid-cycle with prorated credit. No "wait until next billing cycle."
**Where:** `internal/billing/stripe.go` — use Stripe subscription update with `proration_behavior: "always_invoice"`
**Phase:** 2.5

### Volume Discounts
**What:** Automatic price break at 10TB/50TB/100TB. Uses Stripe tiered pricing on Price objects.
**Where:** Stripe Dashboard (Price configuration) + `internal/billing/stripe.go` plan registration
**Phase:** 2.1 (architecture), 2.5 (dashboard display)

### Free Tier (5GB, No Credit Card) — SHIPPED
**What:** 5 GB storage, 1 bucket, 1 scoped API key (`internal/usage/free_tier.go`), enforced on every object write and on bucket/key creation. The 80 % soft-limit prompt is dashboard copy.
**Where:** `internal/usage/quota_manager.go` — default tier = `free`, `internal/dashboard/` — upgrade CTA
**Phase:** 5.6.8

## Phase 2.5: Billing Dashboard Widgets

### Value Stack Breakdown
**What:** Show customers what they get — not what it costs you. Display durability (11 nines), encryption (AES-256 + post-quantum), redundancy (3 copies, 2 continents), erasure coding.
**Where:** `internal/dashboard/templates/customer/billing.html`
**Phase:** 2.5

### S3 Cost Comparison Widget
**What:** "This would cost $230/mo on AWS" right on their dashboard. Constant validation of their choice.
**Where:** `internal/dashboard/handlers/billing.go` — calculate equivalent AWS/Backblaze/Wasabi cost
**Phase:** 2.5

### Predictive Billing
**What:** "At current pace, next month's bill will be ~$45." No surprises. Calculate from usage trend.
**Where:** `internal/dashboard/handlers/billing.go` — extrapolate from `bandwidth_usage_daily` + `tenant_quotas`
**Phase:** 2.5

### Transparent Invoice Breakdown
**What:** Line-item invoices showing: storage × rate, egress (free included), add-ons. Frame as value, not cost.
```
Your 10 TB downstairs (Standard) on stored.ge — prices.json, 2026-09-30:
  Storage:  10 TB × $4.49 (annual)          $44.90/mo   (× $4.99 = $49.90 if paid monthly)
  Egress:   5 TB/mo included (0.5× quota)   $0          never billed; may be throttled past it
  ─────────────────────────────────────────────────────
  Total:                                    $44.90/mo
```
(Object Lock is included; there is no add-on price for it. The pin-hot add-on is $3/TB/mo.)
**Where:** `internal/dashboard/templates/customer/billing.html`
**Phase:** 2.5

## Phase 4: Bandwidth Features

### Bandwidth Banking
**What:** Unused free egress rolls over month-to-month. "You have 180GB egress banked from 3 quiet months." Nobody else offers this.
**Where:** `internal/usage/bandwidth_banking.go` (proposed — does not exist). Egress is never billed today, so "banking" would apply to the throttle allowance only
**Phase:** 4.1-4.3

### Mid-Cycle Usage Alerts
**What:** Email at 50%/75%/90% of storage quota. (No overage charges exist; the warning is about hitting the quota.) Data loss anxiety is the #1 fear.
**Where:** `internal/billing/alerts.go` (proposed — does not exist); the shipped piece is `internal/api/bandwidth_alerts.go` (hourly bandwidth threshold check via `email.Sender`). Prod sends no mail until `EMAIL_PROVIDER` is set
**Phase:** 4.2 (enforcement) + 5.6.6 (event system)

## Phase 5.5: S3 Compatibility Features

### Ransomware Recovery / Object Lock — SHIPPED
**What:** Object Lock (GOVERNANCE + COMPLIANCE retention, legal hold, MFA Delete) enforced on every delete, overwrite, multipart complete and copy (R2/R3/R4). "Even if your keys are compromised, locked objects can't be deleted." Sells to IT admins and compliance teams.
**Where:** `internal/api/s3_lock.go`, migration `028_object_lock.sql`
**Phase:** 5.5.9

## Phase 5.6: Developer Experience

### Webhook Notifications to Customers — SHIPPED (basic)
**What:** Customer-configured webhooks (`/api/v1/webhooks`) fired from the event log (`/api/v1/events`: object.*, bucket.*, key.*, sts.token_created), HMAC-signed. One delivery attempt, no retry yet (WP-R11-3); email-on-event and size/bulk filters are not built.
**Where:** `internal/api/events.go`, `internal/api/webhooks_routes.go` (the never-linked `internal/webhooks` package was deleted in Review R15)
**Phase:** 5.11.6 shipped

### Onboarding Flow
**What:** Post-registration "Get Started" checklist with pre-filled curl examples using the user's ACTUAL API keys. Target: first API call in <5 minutes.
**Where:** shipped as the onboarding block in `internal/dashboard/templates/customer/dashboard.html` (cURL snippet with the real key, `--aws-sigv4` since R14-03); `onboarding.html` does not exist
**Phase:** 5.6.7 shipped

### Team Billing
**What:** One payment method, multiple users/API keys under one tenant. Already have user/tenant separation — just need multi-user invite flow.
**Where:** `internal/dashboard/handlers/team.go`, `internal/auth/invites.go` (both proposed — do not exist)
**Phase:** 5.6 (foundation), 18 (full multi-tenant)

## HIGH PRIORITY: CLI/TUI (Pull forward from Phase 26)

The target market (r/datahoarder, r/cloudstorage, self-hosters, devs) strongly prefers terminal over web UI. A post on r/cloudstorage asking for "just storage, nothing more" validates that our customers want minimal, scriptable, no-bloat tools. CLI should ship alongside or shortly after the billing dashboard.

### `stored` CLI
**What:** Single binary CLI for all stored.ge operations. Pipe-friendly, scriptable, no browser needed.
**Where:** `cmd/stored/` (proposed — does not exist) — Go binary, uses stored.ge API
**Stack:** cobra (CLI framework) + lipgloss (styled output)
```
stored signup                            # register from terminal
stored login                             # authenticate (stores token in ~/.stored.toml)
stored keys list|create|revoke           # API key management
stored buckets list|create|delete        # bucket operations
stored put <bucket/key> [< stdin]        # upload (pipe-friendly)
stored get <bucket/key> [> stdout]       # download (pipe-friendly)
stored ls <bucket> [prefix]              # list objects
stored rm <bucket/key>                   # delete
stored usage                             # storage + bandwidth summary
stored billing                           # plan, invoices, upgrade link
stored rclone-config                     # output rclone config block
stored mount <bucket> <mountpoint>       # wrapper for s3fs/JuiceFS
```
**Phase:** Originally 26. Recommend pulling to Phase 5.7 or right after security polish.

### `stored` TUI (Interactive Mode)
**What:** Full-screen terminal UI built with Bubble Tea. Browse buckets/objects with arrow keys, live usage bars, key management. `stored tui` or just `stored` with no args.
**Where:** `cmd/stored/tui/` (proposed — does not exist) — uses bubbletea + bubbles + lipgloss
**Phase:** Same as CLI, or as a follow-up.

### rclone One-Liner Setup
**What:** `stored rclone-config >> ~/.config/rclone/rclone.conf` outputs a pre-filled rclone remote config with the user's actual credentials. Zero manual config.
**Where:** CLI `rclone-config` subcommand
**Phase:** Ships with CLI

### Shell Completions
**What:** `stored completion bash|zsh|fish` — tab-complete bucket names, key names, subcommands.
**Where:** cobra built-in completion support
**Phase:** Ships with CLI

### "Just Storage" Branding
**What:** Landing page and docs prominently state: "We don't do collaboration, editing, or social features. Just storage. S3-compatible. Pipe-friendly. That's it." This is the anti-bloat positioning that resonates with the r/cloudstorage, r/datahoarder, and LowEndTalk audience.
**Where:** Landing page, README, docs
**Phase:** Pre-launch marketing

## Desktop Sync Client (OneDrive/Google Drive/Dropbox Replacement)

### Phase 1: rclone-Powered Mount (Ship with CLI, ~Phase 5.7)
**What:** `stored mount ~/StoredDrive` wraps rclone under the hood. One command → mounted drive. Also `stored sync ~/folder` for two-way Dropbox-style sync via `rclone bisync`. Branded one-click installer that configures rclone automatically.
**Where:** `cmd/stored/mount.go` (proposed — does not exist) — wraps rclone, auto-downloads if not present
**Why rclone:** Already supports S3, handles caching, retries, bandwidth limits, and has 50+ backend support. Free, battle-tested, open source. Covers 90% of desktop sync use cases with zero custom sync engine development.
**Phase:** ~5.7 (ships with CLI)

### Phase 2: Branded Desktop App (Post-launch, if demand)
**What:** System tray icon + settings panel + selective sync + bandwidth controls. Native feel on Mac/Windows/Linux.
**Stack:** **Wails** (Go backend + web frontend = single native binary). Natural fit — reuses existing Go codebase. The sync daemon shares S3 client code with the server.
**Features:**
- System tray icon with sync status
- Selective sync (choose which buckets/folders)
- Bandwidth throttling (don't saturate connection)
- Conflict resolution UI
- File versioning browser (when Phase 5.5.6 versioning ships)
- Pause/resume sync
- Multi-account support
**NOT Electron** (too heavy), **NOT WASM** (wrong tool), **NOT Flutter** (different language).
**Where:** `cmd/stored-desktop/` (proposed — does not exist) — Wails app
**Phase:** Post-launch (Tier 3-4), only if customer demand warrants it

## Mobile App

### stored Mobile (iOS + Android)
**What:** Mobile file browser and camera backup for stored.ge. Not a full sync client — focused on browse, upload, and photo/video backup.
**Stack:** **Go Mobile** backend (shared S3/auth code) + **Swift UI** (iOS) / **Jetpack Compose** (Android). OR cross-platform with **Flutter** or **React Native** if one codebase preferred.
**Core features:**
- Browse buckets and objects
- Photo/video auto-backup (like Google Photos but to your own storage)
- Share files via presigned URLs (tap → copy link)
- Upload from camera roll or files app
- Download/offline access for pinned files
- Push notifications for upload completion, storage alerts
- Biometric auth (Face ID / fingerprint)
**Differentiator features:**
- "Camera backup to YOUR storage" — privacy pitch against Google Photos
- QR code setup — scan from dashboard, auto-configures the app
- Widget showing storage usage on home screen
- Shortcut/Siri integration: "Hey Siri, back up my photos to stored"
**Where:** `mobile/ios/`, `mobile/android/` (or `mobile/` for cross-platform) — proposed, nothing exists
**Phase:** Tier 3-4. Consider after desktop client proves demand. Camera backup alone could be an MVP — it's the #1 reason normal people use cloud storage.

### Mobile-First Alternative: Progressive Web App (PWA)
**What:** The dashboard (`stored.ge/dashboard`) as a PWA — installable on phones, works offline for cached data, push notifications. Much cheaper than native apps. Test demand before building native.
**Where:** `internal/dashboard/` — add PWA manifest, service worker, responsive CSS (Phase 5.4 already has responsive)
**Phase:** ~5.4 (responsive CSS phase — add PWA manifest at same time)

## What People Expect (Table Stakes)

These are features users take for granted. Missing any one creates friction:

| Feature | Status | Phase |
|---------|--------|-------|
| Web dashboard | Shipped (house checkout + overview behind flags) | 1.x, dashboard plan Phases 0-4 |
| S3 API compatibility | Shipped; conformance fixes A1, reviews R2-R4 | 5.5 |
| File sharing (presigned URLs) | Shipped: presigned GET/PUT (SigV4 query auth, `GET /api/v1/user/presigned`) + STS temporary credentials (`POST /api/v1/sts/token`) | 5.10.16, 5.11.5 |
| Search / find files | Not started (prefix listing only) | 5.6.3 (metadata) |
| Trash / undelete | Partial: versioning is metadata-only, a previous version's bytes cannot be retrieved (WP-R2-1) | 5.5.6 |
| File preview (images, PDF) | Not started | Tier 3 |
| Activity log | Shipped: `GET /api/v1/events` (customer events) + `audit_logs` via `internal/audit` (operator trail, dashboard audit page) | 5.11.6, R11/R12 |
| 2FA / MFA | Shipped: TOTP (#196), honoured by OAuth and MFA Delete | 5.1 |
| Email notifications | Code shipped (bandwidth alerts, password reset, verification); prod sends nothing until `EMAIL_PROVIDER` is set | 4.3 |
| API documentation | Shipped: `/docs`, `/docs/api` (Swagger UI), `/openapi.json` (drift-guarded), `/llms.txt`, `docs/API.md` | 5.6.7, R14 |
| Status page | Shipped: `/status` (HTML), `/health*` | launch |
| CLI tool | Not started | ~5.7 |
| Desktop sync | Not started (rclone guide is the answer today) | ~5.7 |
| Mobile app | Not started | Tier 3-4 |

## Wow Factor Summary

Things that make people switch or tell their friends:

1. **"Just storage" positioning** — anti-bloat, resonates with technical crowd
2. **Whole-TB flat pricing** (see `prices.json`: Standard $4.49/TB annual) — ~5× cheaper than AWS S3
3. **Egress never billed** — 0.5× quota/month free on Standard, then throttled at most; no per-request fees
4. **Pipe-friendly CLI** — `cat file | stored put bucket/key`
5. **rclone one-liner** — `stored rclone-config >> ~/.config/rclone/rclone.conf`
6. **Pause subscription** — data stays, billing stops
7. **Bandwidth banking** — unused egress rolls over
8. **Camera backup to YOUR storage** — privacy pitch vs Google Photos
9. **Object lock** — ransomware-proof backups
10. **Transparent value display** — show durability/encryption/redundancy, not costs
11. **S3 cost comparison** — "This costs $230/mo on AWS" on dashboard
12. **EU data residency** — per-bucket region at creation (shipped; five EU regions)
13. **Terminal-first** — everything works without a browser
14. **Open source core** — trust through transparency

## Phase 7: Storage Intelligence Features

### Data Residency Picker — SHIPPED (per-bucket region)
**What:** A bucket's region is chosen at creation (`LocationConstraint` / dashboard selector) from the 13 iDrive regions the deployment has key pairs for (`us-*`, `eu-west-1/3/4`, `eu-central-1`, `eu-south-1`, `ap-northeast-1`) and is immutable. Exceptions stated in the GDPR page: Vault objects go to the tape backend (Los Angeles) and public-read objects to the CDN origin.
**Where:** `internal/api/s3_buckets.go`, `bucketRegionDriver` in `s3_engine_adapter.go`, `internal/drivers/idrive_regions.go` (`internal/engine/routing.go` is not the region path)
**Phase:** 5.14.7 + WP-R7-1 shipped

### Carbon Footprint Badge
**What:** Tape is ~90% less energy than spinning disk. "Your data produces X% less CO2 than traditional cloud." Show per-tenant stats based on which backends their data lives on.
**Where:** `internal/dashboard/handlers/overview.go` — calculate from `object_locations` backend distribution
**Phase:** 7.4 (needs cost/backend tracking)

## Pricing Architecture Notes

### Charge for Logical Bytes (Not Physical)
Every provider charges for logical bytes stored. Show the VALUE stack (durability, encryption, redundancy, erasure coding) — not the cost stack (dedup ratio, compression savings, backend cost).

### Price Tiers
See `internal/api/landing/prices.json` — the one price file (landing page, dashboard, legal pages and `llms.txt` render from it). As of 2026-09-30: Standard $4.49/TB/mo annual, $4.99 monthly; Vault $2.00 annual, $2.55 monthly with a $4.99 monthly minimum; pin-hot $3/TB/mo; Performance $6.99 parked until after launch; 5 GB free. Quota-sold, no overage, egress never billed. Backends per tier: Standard → iDrive (hot), Vault → Geyser tape, public buckets → R2 (`docs/ARCHITECTURE.md`).

The pack-era table (`Vault3/9/18/36`, `$3.99/TB`) is gone from every product surface; the only leftover is `registerStripePlans` in `internal/api/server.go`, which still registers `STRIPE_PRICE_VAULT3…` plans with the retired labels when those env vars are set (they are not). Removing it is a work package.

### Add-on Pricing (not on the price list — none of these is sold today)
| Add-on | Price | Phase |
|--------|-------|-------|
| Object Lock / WORM | ~$1.99/mo | 5.5.9 |
| Priority Egress | ~$0.99/mo | 4.1 |
| Extra Storage Block (5TB) | ~$15/mo | 2.5 |
| Data Residency (EU) | ~$0.50/TB premium | 7.6 |
| Team (per additional user) | ~$2/mo | 18 |
