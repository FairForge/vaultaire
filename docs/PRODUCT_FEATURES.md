# Product Features Roadmap

Comprehensive list of customer-facing features, organized by when they can be built.
Each feature references its implementation phase in the master plan.

## Phase 2: Billing Foundation (NOW)

### Multi-Item Subscriptions (Add-ons)
**What:** Customers can add/remove individual services (object lock, priority egress, extra storage blocks) without canceling their base plan. Uses Stripe Subscription Items — each add-on is a separate line item on one subscription.
**Where:** `internal/billing/stripe.go` — add `AddSubscriptionItem()`, `RemoveSubscriptionItem()`
**Phase:** 2.1-2.6

### Metered (Usage-Based) Billing
**What:** Charge per GB actually stored, reported to Stripe via usage records. Hybrid model: base plan + metered overage. The r/datahoarder crowd hates paying for unused capacity.
**Where:** `internal/billing/metered.go` — Stripe usage record reporting, background job
**Phase:** 2.2-2.6

### Pause/Resume Subscription
**What:** Customer pauses billing — data stays, no charges. Stripe supports this natively. Huge for seasonal users; nobody in cheap storage offers this.
**Where:** `internal/billing/stripe.go` — `PauseSubscription()`, `ResumeSubscription()` using Stripe's `pause_collection`
**Phase:** 2.5

### Prepaid Credits
**What:** Buy $50 credit, draw down against usage. Great for burst backups. Uses Stripe customer credit balance.
**Where:** `internal/billing/credits.go` — `AddCredit()`, `GetBalance()`, Stripe customer balance API
**Phase:** 2.5

### Grace Period on Failed Payments
**What:** 3/7/14 day email warnings before any access restriction. Never delete data on payment failure. Builds trust.
**Where:** `internal/billing/webhook.go` (already started with `OverageService`), email notifications
**Phase:** 2.2 (webhook), 4.1 (enforcement)

### Instant Plan Switching
**What:** Upgrade/downgrade mid-cycle with prorated credit. No "wait until next billing cycle."
**Where:** `internal/billing/stripe.go` — use Stripe subscription update with `proration_behavior: "always_invoice"`
**Phase:** 2.5

### Volume Discounts
**What:** Automatic price break at 10TB/50TB/100TB. Uses Stripe tiered pricing on Price objects.
**Where:** Stripe Dashboard (Price configuration) + `internal/billing/stripe.go` plan registration
**Phase:** 2.1 (architecture), 2.5 (dashboard display)

### Free Tier (5GB, No Credit Card)
**What:** Biggest signup accelerator. 5GB storage, 1GB bandwidth/month, 1 bucket, 1 API key. Soft limit at 80%, block at 100% with upgrade prompt.
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
Your 10TB on stored.ge:
  Storage:     10 TB × $3.99        $39.90
  Egress:      50 GB free included     $0
  Object Lock: enabled               $1.99
  ─────────────────────────────────────
  Total:                             $41.89
```
**Where:** `internal/dashboard/templates/customer/billing.html`
**Phase:** 2.5

## Phase 4: Bandwidth Features

### Bandwidth Banking
**What:** Unused free egress rolls over month-to-month. "You have 180GB egress banked from 3 quiet months." Nobody else offers this.
**Where:** `internal/usage/bandwidth_banking.go` — track rollover in `tenant_quotas` or new table, display in dashboard
**Phase:** 4.1-4.3

### Mid-Cycle Usage Alerts
**What:** Email at 50%/75%/90% of storage quota. 3-day warning before overage charges. Data loss anxiety is the #1 fear.
**Where:** `internal/billing/alerts.go` — background job checks thresholds, sends via email service
**Phase:** 4.2 (enforcement) + 5.6.6 (event system)

## Phase 5.5: S3 Compatibility Features

### Ransomware Recovery / Object Lock
**What:** Object lock with compliance mode + immutable snapshots. "Even if your keys are compromised, locked objects can't be deleted." Sells to IT admins and compliance teams.
**Where:** `internal/api/s3_lock.go`, migration `025_object_lock.sql`
**Phase:** 5.5.9

## Phase 5.6: Developer Experience

### Webhook Notifications to Customers
**What:** "Email me when any upload > 1GB completes" or "Alert on bulk deletion." Peace of mind. Customer-configured webhooks delivered via event system.
**Where:** `internal/api/events.go` (foundation), `internal/webhooks/delivery.go` (Phase 12.3 for full webhook delivery)
**Phase:** 5.6.6 (event log), 12.3 (webhook delivery)

### Onboarding Flow
**What:** Post-registration "Get Started" checklist with pre-filled curl examples using the user's ACTUAL API keys. Target: first API call in <5 minutes.
**Where:** `internal/dashboard/templates/customer/onboarding.html`
**Phase:** 5.6.7

### Team Billing
**What:** One payment method, multiple users/API keys under one tenant. Already have user/tenant separation — just need multi-user invite flow.
**Where:** `internal/dashboard/handlers/team.go`, `internal/auth/invites.go`
**Phase:** 5.6 (foundation), 18 (full multi-tenant)

## Phase 7: Storage Intelligence Features

### Data Residency Picker
**What:** "Keep my data in EU only" / "US only" toggle per bucket. Huge for GDPR. Geyser London enables EU. Lyve has AP regions.
**Where:** `internal/engine/routing.go` — region-aware routing, `internal/dashboard/` — region selector on bucket creation
**Phase:** 7.6

### Carbon Footprint Badge
**What:** Tape is ~90% less energy than spinning disk. "Your data produces X% less CO2 than traditional cloud." Show per-tenant stats based on which backends their data lives on.
**Where:** `internal/dashboard/handlers/overview.go` — calculate from `object_locations` backend distribution
**Phase:** 7.4 (needs cost/backend tracking)

## Pricing Architecture Notes

### Charge for Logical Bytes (Not Physical)
Every provider charges for logical bytes stored. Show the VALUE stack (durability, encryption, redundancy, erasure coding) — not the cost stack (dedup ratio, compression savings, backend cost).

### Price Tiers (from docs/BUSINESS.md)
| Product | Price | Backend |
|---------|-------|---------|
| Free | $0 (5GB) | Quotaless |
| Vault3 | $2.99/mo (3TB) | Geyser direct |
| Vault9 | $9/mo (9TB) | Geyser direct |
| Vault18 | $18/mo (18TB) | Geyser direct |
| Vault36 | $36/mo (36TB) | Geyser direct |
| Standard | $3.99/TB/mo | Lyve → Quotaless → Geyser |
| Performance | $6.99/TB/mo | Lyve direct |
| Annual | 20% off | Same backends |

### Add-on Pricing (TBD — set in Stripe Dashboard)
| Add-on | Price | Phase |
|--------|-------|-------|
| Object Lock / WORM | ~$1.99/mo | 5.5.9 |
| Priority Egress | ~$0.99/mo | 4.1 |
| Extra Storage Block (5TB) | ~$15/mo | 2.5 |
| Data Residency (EU) | ~$0.50/TB premium | 7.6 |
| Team (per additional user) | ~$2/mo | 18 |
