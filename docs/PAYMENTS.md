# Payments: one entitlement record, many providers

Design of record for WP-PAY (planned 2026-10-18, Stage 1a). The owner's ask:
"make it so we can easily switch off from Stripe and add crypto payment as
well." This page is the design; the plan entries are in
`docs/IMPLEMENTATION_PLAN.md` § "WP-PAY", the owner decisions are F-22 … F-30
in the plan's decision table, and the worker prompts are drafted in
`docs/reviews/WP-PAY-PROMPTS.md`. Code: `internal/billing` (today: Stripe),
`internal/payments` (new: the neutral core), `internal/payments/btcpay` (new).

**State on prod (2026-10-18, read-only):** 3 tenants, 0 Stripe customers, 0
Stripe subscriptions, 0 floor rows, 0 `stripe_events`, 0 `subscriptions`
rows; no `STRIPE_*` in the environment. There is nothing to migrate, so the
provider-neutral refactor costs no customer anything. E-mail on prod is the
Cloudflare Email Service over SMTP (`EMAIL_PROVIDER=smtp`,
`SMTP_HOST=smtp.mx.cloudflare.net`) since 2026-10-12 — reminders can be sent.

## 0. What is true today (verified against `main` @ `c9ce46c`)

| # | Claim in the planning prompt | Verdict | Where |
|---|---|---|---|
| 1 | Entitlement lives at Stripe: the webhook sets or clears the house from the subscription status | **Confirmed.** `applyHouse`: active / trialing / past_due → `SetHouse`; canceled / unpaid / incomplete_expired / **paused** (added by #752) → `ClearHouse`; incomplete → no change | `internal/billing/webhook.go:441-490` (grant 447-458, clear 462-487) |
| 1b | No "paid through" date anywhere in our DB | **Corrected.** `subscriptions.current_period_end` exists (migration 018) but **no code writes the table** — zero `INSERT INTO subscriptions` in the tree; `billing_charges.period_end` (019) likewise has no writer. So the claim holds in effect: nothing we read says when a house was paid through. Consequence stands: with Stripe off, every paying house is stranded on the next `ClearHouse` | `internal/database/migrations/018_dashboard_foundation.sql:25`, `019_stripe_billing.sql:30`; `internal/api/account_export_sections.go:89` ("no writer (R10-14)") |
| 2 | Stripe types leak past the billing package | **Confirmed.** `HouseBilling` returns `*stripe.Subscription` (two methods); `HandleUpgrade` and `HandleManageBilling` take `*billing.StripeService`; `Deps.Stripe *billing.StripeService`. `billing_house.go` is the only dashboard file importing `stripe-go` | `internal/dashboard/handlers/billing_house.go:32-39`, `billing.go:66,120`, `internal/dashboard/router.go:48` |
| 3 | Stripe columns on `tenants` and ~21 SQL references outside `internal/billing` | **Confirmed, count corrected to 34 lines in 13 files.** Columns `stripe_customer_id` (006, 018), `stripe_subscription_id`, `subscription_status` (018), `house_period` (066), `deletion_stripe_cancelled_at` (072); tables `subscriptions` (018), `stripe_events`, `billing_charges` (019), `metered_usage_reports` (043). Readers: `dashboard/handlers/{billing,billing_house,onboarding,admin_revenue,admin_support,admin,tenants,admin_stats,admin_costs}.go`, `api/account_export_sections.go:371-378`, `api/deletion_runner.go:514,539`, `api/retention.go:79`, `account/erase.go:148,256,265`, `docs/openapi.go:1460` | `grep -rn stripe_customer_id\|stripe_subscription_id\|subscription_status\|house_period\|deletion_stripe_cancelled_at\|stripe_events\|metered_usage_reports\|subscriptions internal cmd --include='*.go'` |
| 4 | Stripe customers are created from three places | **Confirmed.** `/auth/register`, the dashboard register form, lazily at house checkout — each logs-and-continues | `internal/api/server.go:1065-1068`, `internal/dashboard/router.go:714-718`, `internal/dashboard/handlers/billing_house.go:290-296` |
| 5 | The deletion runner cancels at Stripe and defers when Stripe is not configured | **Confirmed.** `cancelSubscription`: a subscription id on the row with `r.Stripe == nil` returns an error → `defer_` (the tenant is not erased); `CancelSubscription` is immediate and idempotent | `internal/api/deletion_runner.go:509-545`; `internal/billing/stripe.go:189-227` |
| 6 | The metered reporter reports to Stripe Billing Meters; `AccruedCents` and the revenue page compute money from usage and `QuoteHouse` | **Confirmed.** Dormant (no `STRIPE_METER_*`). `AccruedCents` is called from `admin_costs.go:242`, `billing.go:246`, `admin_revenue.go:281,356,394`; `QuoteHouse` from `admin_revenue.go:228` | `internal/billing/metered.go:48,77-144` |
| 7 | Prices live only in `prices.json`; the six house prices are asserted against Stripe at boot | **Confirmed.** `QuoteHouse` and `VerifyHousePrices` read the file; `StartHouseVerification` retries every 5 min then hourly; `HouseCheckoutReady()` gates the page | `internal/api/landing/prices.json`; `internal/billing/house.go:216,307,378-400` |
| 8 | #752 shape: claim in a held transaction, fetch not trust, per-customer advisory lock | **Confirmed.** `claimEvent`: `SET LOCAL lock_timeout='5s'`, `INSERT … ON CONFLICT DO NOTHING RETURNING` held open across `handle`, `pg_advisory_xact_lock(hashtextextended('stripe-customer:'‖customer,0))`; `handleSubscriptionUpdated` and `handleCheckoutCompleted` apply the FETCHED subscription; `customer.subscription.deleted` applies its payload (terminal) | `internal/billing/webhook.go:80-203, 243-290, 463-521` |
| 9 | Prod: no `STRIPE_*`, no live subscriptions | **Confirmed, 0.** `tenants` with a subscription id = 0, customers = 0, `subscription_status <> 'none'` = 0 (of 3 tenants); `tenant_floor_quotas` 0, `stripe_events` 0, `subscriptions` 0; `grep -c '^STRIPE_' .env` = 0. Nothing to migrate | read-only, 2026-10-18 07:40 MDT, inside a 0600 script sourcing `.env`, then deleted |
| 10 | The plan already says | Phase 2 (plan ~337-395) is the Stripe build as shipped (status mirrored into `tenants`, no date). 43.5 (plan ~3038) says "prepaid credits (BTCPay or a Stripe prepaid) — tax handled as prepaid, storage is not money transmission; private buckets only"; the north star item 4 (`.private/PRIVACY_NORTH_STAR.md:98-103`) adds caveats (a) anonymous + public CDN = abuse magnet → private-only, (b) payments outside Stripe = manual tax/VAT handling → prepaid credits only, (c) storage is not money transmission. **This design fits (b) and (c) and narrows (a): crypto on an identified account is allowed everywhere; the private-only rule applies to numbered accounts (43.5) and, as a recommendation, to public surfaces on crypto-only accounts for their first 30 days (F-25). It departs from "tax handled as prepaid": prepaid is a billing shape, not a tax treatment — § 6 marks it unverified and sends it to counsel.** The blind-signed-token idea of 43.5 (pay identified, redeem unlinkably) sits on top of this ledger later: a token issuance is one more "kind" of payment | `docs/IMPLEMENTATION_PLAN.md:337-395, 3038-3041` |

Two more facts the prompt did not list: `internal/email/CLAUDE.md` still said prod
runs the `LogSender` — stale since 2026-10-12 (fixed with this PR); and the
house checkout's `UpdateHouseSubscription` keeps the **period fixed for the
life of a subscription** (switching is support) — the crypto design keeps the
same rule.

## 1. The entitlement record (Part A)

### 1.1 Principle

**What a tenant has bought lives in our database, with a date.** A payment from
any provider extends the date. A daily job clears a house whose date plus
grace has passed. No provider's status machine decides whether a customer has
storage; only the date does. Billing never deletes data: `ClearHouse` returns
the quota to the free tier and leaves every byte in place (`usage/floor.go:131`,
unchanged).

Rejected: keep Stripe's status as the truth and add a crypto "virtual
subscription" that mimics it (two state machines, Stripe off still strands
everyone); a credit balance drawn down daily (Mullvad-style — fine for one flat
plan, wrong for a house with three lines and two periods; revisit for 43.5).

### 1.2 Schema (migration 096, additive; the previous build keeps serving)

```sql
-- One row per tenant: the house as bought, and until when.
CREATE TABLE IF NOT EXISTS entitlements (
    tenant_id      TEXT PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
    std_tb         INT  NOT NULL DEFAULT 0,
    vault_tb       INT  NOT NULL DEFAULT 0,
    pin_hot_tb     INT  NOT NULL DEFAULT 0,
    period         TEXT NOT NULL,                  -- 'monthly' | 'annual'
    paid_through   TIMESTAMPTZ NOT NULL,           -- the date; grace is added at read time
    renewal        TEXT NOT NULL,                  -- 'auto' (a provider will try to extend it) | 'prepaid' (nobody will)
    provider       TEXT NOT NULL,                  -- the provider allowed to extend it: 'stripe' | 'btcpay' | 'manual'
    provider_ref   TEXT,                           -- stripe subscription id | last settled btcpay invoice id | a ticket
    pending_provider TEXT,                         -- set during a switch (§4): the provider the next payment is expected from
    next_std_tb    INT, next_vault_tb INT, next_pin_hot_tb INT, next_period TEXT, -- a downsize or period change that takes effect at paid_through
    lapsed_at      TIMESTAMPTZ,                    -- the lapse job cleared the house (NULL = house is granted)
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (period IN ('monthly','annual')), CHECK (renewal IN ('auto','prepaid')),
    CHECK (std_tb >= 0 AND vault_tb >= 0 AND pin_hot_tb >= 0 AND pin_hot_tb <= std_tb)
);
CREATE INDEX IF NOT EXISTS idx_entitlements_due ON entitlements (paid_through) WHERE lapsed_at IS NULL;

-- The ledger: one row per money event from any provider. Created 'pending'
-- when an invoice/checkout is opened, settled by the provider's notification.
CREATE TABLE IF NOT EXISTS payments (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      TEXT REFERENCES tenants(id) ON DELETE SET NULL, -- NULL after erasure (§3.8); the row is an accounting record
    provider       TEXT NOT NULL,                  -- 'stripe' | 'btcpay' | 'manual'
    provider_id    TEXT NOT NULL,                  -- stripe invoice id | btcpay invoice id | 'manual:<uuid>'
    kind           TEXT NOT NULL,                  -- 'charge' | 'refund' | 'credit' | 'adjustment'
    purpose        TEXT NOT NULL,                  -- 'new' | 'renew' | 'resize' | 'switch' | 'manual'
    status         TEXT NOT NULL,                  -- 'pending' | 'processing' | 'settled' | 'expired' | 'expired_partial' | 'invalid' | 'refunded'
    amount_cents   BIGINT NOT NULL,                -- USD at receipt (what we priced); negative for refunds
    credit_cents   BIGINT NOT NULL DEFAULT 0,      -- over/partial payment kept as credit against the next invoice
    crypto_amount  NUMERIC(30,12), crypto_currency TEXT, rate NUMERIC(20,8), -- what was received and the USD rate BTCPay recorded
    period_start   TIMESTAMPTZ, period_end TIMESTAMPTZ,                      -- what this payment bought
    std_tb INT, vault_tb INT, pin_hot_tb INT, period TEXT,                    -- the house snapshot it bought
    invoice_number TEXT UNIQUE,                    -- ours, gapless per year: ST-2026-000001 (assigned at settle)
    refund_of      UUID REFERENCES payments(id),
    actor          TEXT,                           -- the admin for manual rows
    note           TEXT,
    metadata       JSONB,                          -- provider ids (txid, pull payment id); never an address after erasure
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    settled_at     TIMESTAMPTZ,
    UNIQUE (provider, provider_id)
);
CREATE INDEX IF NOT EXISTS idx_payments_tenant ON payments (tenant_id, created_at DESC);

-- Webhook dedup for every provider but Stripe (stripe_events keeps #752's shape).
CREATE TABLE IF NOT EXISTS payment_events (
    provider     TEXT NOT NULL,
    event_id     TEXT NOT NULL,                    -- BTCPay: COALESCE(originalDeliveryId, deliveryId)
    event_type   TEXT NOT NULL,
    tenant_id    TEXT,                             -- resolved; NULL after erasure
    received_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    data         JSONB,
    PRIMARY KEY (provider, event_id)
);
CREATE INDEX IF NOT EXISTS idx_payment_events_tenant ON payment_events (tenant_id);

-- Gapless receipt numbering, one counter row per year, taken under FOR UPDATE.
CREATE TABLE IF NOT EXISTS invoice_counters (year INT PRIMARY KEY, last INT NOT NULL DEFAULT 0);
```

**Done (WP-PAY-7, 2026-10-21, migration 100):** the four columns below and the three tables are dropped; `stripe_customer_id` stays (the Stripe customer of the tenant). As designed:
`tenants.stripe_customer_id / stripe_subscription_id / subscription_status /
house_period / deletion_stripe_cancelled_at` stay for two releases: the
previous build writes them during the two-slot switch, and the admin pages
read them until WP-PAY-7 moves those pages onto `entitlements` + `payments`
and drops the columns with D-12's `DROP TABLE` migration (`subscriptions`,
`billing_charges`, `metered_usage_reports` go too). `stripe_events` is kept as
the Stripe dedup table: it is reviewed, it works, renaming it buys nothing.

### 1.3 How it maps onto the quota tables

`tenant_floor_quotas` + `tenant_quotas` stay the enforcement truth the write
path reads (`usage/ledger_tx.go`). The entitlement row is the **billing
truth**; the two are joined by one writer:

- `payments.Grant(ctx, tx, tenant, House, period, paidThrough, provider, ref)` —
  upserts `entitlements` **and** calls a new `usage.SetHouseTx(ctx, tx, …)`
  (the body of `SetHouse`, `floor.go:92`, taking the caller's transaction) in
  the same transaction, under `pg_advisory_xact_lock(hashtextextended(
  'entitlement:'‖tenant, 0))`. Idempotent: replaying a grant with the same
  `paid_through` changes nothing.
- `payments.Extend(ctx, tx, tenant, by period)` — `paid_through = GREATEST(
  paid_through, now()) + period`: a renewal of a running house extends from its
  end; a lapsed one restarts from now. Never two overlapping periods.
- The **`entitlement_lapse` job** (daily 07:30 UTC + boot catch-up through
  `internal/api/jobs.go`, like every other job): rows with `lapsed_at IS NULL
  AND paid_through + grace < now()` → `ClearHouse` (unchanged), `lapsed_at =
  now()`, audit `billing.house_lapsed`, the "house lapsed" e-mail. A tenant
  whose `provider` was **not ready for any part of the last 24 h** (the
  readiness gauge of § 2.3) is skipped with a note (`provider_unready`), never
  cleared: a house is never lost because our processor was down. Flag
  `entitlement_lapse` (default OFF) — while OFF the job only counts and logs
  what it would clear (`would_clear` in `job_runs.result`), and the Stripe
  webhook keeps clearing as today; the flag flips after the prod proof.
- A **reconcile** inside the same job asserts floors == entitlement for every
  granted row and heals (`SetHouseTx`) with an Error line — the two tables
  cannot drift silently.

**Egress allowance (WP-R10-9)** is derived at read time from the floors
(`usage/egress.go:66`), so a lapse falls back to the free-tier allowance by
itself and an admin override survives; nothing new is seeded. The invariant
"billing never deletes data" holds because the only quota writers stay
`SetHouse`/`ClearHouse`, and neither touches `storage_used_bytes`.

### 1.4 Stripe under this model

**Stripe is a provider whose payments extend `paid_through`; its status never
shortens the date.** Walk of every event the handler sees (`webhook.go:222`),
all applied from the FETCHED subscription as #752 requires:

| Event (fetched status) | Today | Under WP-PAY |
|---|---|---|
| `checkout.session.completed` | fetch sub → `applyHouse` | fetch sub → `Grant(house = items, period, paid_through = current_period_end, provider stripe, renewal auto)`; ledger row `charge/new/settled` from the first invoice when `invoice.payment_succeeded` lands (either order is fine — both are idempotent) |
| `customer.subscription.created/updated` — `active`, `trialing` | `SetHouse` | `Grant` with the fetched items and `paid_through = current_period_end` (a resize lands here; a trial is "paid through the trial end") |
| … `past_due` | `SetHouse` (house kept) | **no change**: `paid_through` has passed, OUR grace runs; Stripe's Smart Retries run inside it (the dunning window must be ≤ grace; F-26 = 14 d, set Stripe to cancel after retries within it) |
| … `canceled` (cancel_at_period_end reached, or immediate) | `ClearHouse` now | `renewal = 'prepaid'`, `provider_ref` kept; **never cleared before `paid_through` + grace** — the customer paid for it. An operator refund (§ 7) is what shortens a date |
| … `unpaid`, `incomplete_expired`, `paused` | `ClearHouse` now | `renewal = 'prepaid'`; nothing else — nothing was paid for the new period, so `paid_through` is already in the past and the lapse job does the rest after grace |
| … `incomplete` | no change | no change |
| `customer.subscription.deleted` (payload, terminal) | `ClearHouse` | `renewal = 'prepaid'`; same as `canceled` |
| `invoice.payment_succeeded` | `subscription_status = 'active'` | fetch the invoice; ledger row `charge` (`provider_id` = invoice id, amount, period from the invoice lines) `settled`; `Extend`/`Grant` to the subscription's `current_period_end` (a renewal) — idempotent with the `updated` event |
| `invoice.payment_failed` | `subscription_status = 'past_due'` | ledger row `charge … failed` (informational); no entitlement change |

`isCurrentSubscription` (stale-subscription guard) becomes "the subscription
named in `entitlements.provider_ref`" — and nothing else since WP-PAY-7 (a
tenant with no row fails it closed). The webhook writes no tenants column
but `stripe_customer_id` (D-P23-3).

**As built (WP-PAY-7):** a terminal status on a granted row whose
`paid_through` is still ahead only stops the renewal, `entitlement_lapse` ON
or OFF (D-P23-1 — a $0 trialing Renew cancelled in the portal must not take
a manually-paid year). `charge.refunded` records each succeeded refund in the
ledger, keyed by the refund id, counted under the charge's provider, and does
not shorten `paid_through` (D-P23-4).

What becomes of Stripe's own machine: Smart Retries, `past_due`, `paused`,
"pause collection" are all **inside our grace** and change nothing by
themselves. Stripe is no longer able to take a house away; only the calendar
is. The only Stripe-side setting that matters is "after retries: cancel" so a
dead card ends auto-renewal (F-26 asks for a dunning window ≤ grace).

### 1.5 Recurring vs prepaid: one model

`renewal = 'auto'` means a provider will try to extend the date on its own
(Stripe); `'prepaid'` means nobody will (crypto, manual, a cancelled Stripe
subscription). Everything downstream — reminders, grace, lapse, the billing
page's sentence — reads `paid_through` and `renewal` and nothing else. A
`prepaid` row gets reminders (§ 3.7); an `auto` row gets none until a payment
fails and the date passes.

### 1.6 Upgrade path, two-slot safe

1. **Migration 096** (WP-PAY-1) creates the tables. The old build ignores
   them. Nothing is dropped or renamed.
2. **Dual write** (WP-PAY-1): the webhook writes the entitlement AND the old
   columns. Backfill at the job's first run: every tenant with floor rows and
   no entitlement row gets one (`provider stripe`, `paid_through` = the fetched
   `current_period_end` when Stripe is configured, else `now() + grace` with a
   Warn). On prod this is a no-op (0 floor rows).
3. **Flag flip** `entitlement_lapse` ON after the proof: the lapse job clears;
   the webhook stops clearing on terminal statuses (WP-PAY-2 switches that
   branch on the flag).
4. **Contract** (WP-PAY-7, two deploys later): readers move to
   `entitlements`/`payments`, columns and the three dead tables drop (D-12).
   **Done 2026-10-21** (#834, #836, #839; `docs/reviews/WP-P23.md`). Two
   lessons for the next contract: the deploy re-applies every file, so the
   statements that CREATE a dropped object are emptied in the same change
   (D-P23-5); and the build that serves during the drop is tested against
   the post-drop schema first (D-P23-6).

## 2. The provider interface (Part B)

### 2.1 Packages

- `internal/payments` — **the neutral core**: `Entitlement`, `Grant/Extend/
  Lapse`, the ledger, receipt numbering, `Provider` interface and `Registry`,
  the generic `ClaimEvent`, the reminder/lapse jobs' logic, the dashboard-
  facing read model (`Status(ctx, tenant)`: house, paid through, renewal,
  provider, pending switch, receipts). Imports `usage`, `flags`, `audit`,
  `email`. **Never imports a provider SDK.**
- `internal/billing` — becomes **the Stripe provider** (`StripeService`,
  `WebhookHandler`, house price verification; the only place `stripe-go` is
  imported besides tests). It implements `payments.Provider` and calls
  `payments.Grant/Extend`. Renaming the package to `internal/payments/stripe`
  is WP-PAY-7's cosmetic last step, not a prerequisite.
- `internal/payments/btcpay` — the BTCPay provider (§ 3).
- `internal/dashboard` imports `payments` only. The structural test
  `TestDashboardImportsNoStripe` (`go list -deps ./internal/dashboard/...`
  must not contain `github.com/stripe/stripe-go`) is the red test of WP-PAY-2.

Rejected: moving Stripe into `internal/payments/stripe` first (a 3,300-line
move with no behaviour change; the import direction is what matters);
an `interface{}`-shaped `Subscription` returned to the dashboard (the
dashboard needs a date, a house and a sentence, not a provider object).

### 2.2 The interface

```go
package payments

type Renewal string // "auto" | "prepaid"

type Order struct{ Std, Vault, PinHot int; Period Period } // = billing.HouseOrder today

type Checkout struct {
    TenantID, Email string
    Order           Order
    Purpose         string // "new" | "renew" | "resize" | "switch"
    ReturnURL, CancelURL string
    CreditCents     int64  // a credit the ledger holds for this tenant (BTCPay subtracts it)
}

type Provider interface {
    Name() string                                   // "stripe" | "btcpay"
    Renewal() Renewal                               // what this provider's payments imply
    Ready(ctx context.Context) (bool, string)       // configured AND verified (Stripe: prices match; BTCPay: store reachable, webhook registered)
    StartCheckout(ctx context.Context, c Checkout) (redirectURL string, err error)
    Resize(ctx context.Context, tenantID string, o Order) (Resize, error) // Stripe: in place with prorations; BTCPay: a top-up invoice URL
    StopRenewal(ctx context.Context, tenantID string) error  // Stripe: cancel_at_period_end = true; BTCPay: no-op
    ResumeRenewal(ctx context.Context, tenantID string) error
    Cancel(ctx context.Context, tenantID string) error       // the deletion runner: Stripe immediate cancel (idempotent); BTCPay: no-op
    ManageURL(ctx context.Context, tenantID, returnURL string) (string, error) // Stripe portal; BTCPay: "" (our page)
    Webhook() http.Handler                                   // mounted at /webhook/<Name()> when the provider's secrets are set
}
```

Not on the interface: **refunds** (manual on both providers, recorded by the
admin tool, § 7), **customer creation** (an implementation detail of
`StartCheckout` — Stripe creates its customer lazily there; the two
registration-time call sites go, which also serves 43.2: no third party
knows a tenant until it buys). `Registry.For(ctx, tenantID, name)` returns a
provider only when its flag is on for that tenant.

### 2.3 Runtime switches

Flags (`internal/api/flags_wiring.go`): `pay_stripe` (default ON when
`STRIPE_SECRET_KEY` is set, else OFF), `pay_btcpay` (default OFF),
`entitlement_lapse` (default OFF, § 1.3). Global row `'*'` plus per-tenant
overrides, as every flag — a canary tenant pays in crypto first.

"Provider off" means exactly:

| Surface | Flag off |
|---|---|
| New checkout | not offered on the billing page; `POST` → 409 `provider_disabled` |
| Renewal | **nothing happens by itself** — a Stripe subscription keeps renewing until an operator runs the bulk stop (§ 4.3). The flag is "no new business", not "cancel everyone" |
| Resize | 409 for the off provider; the page says to switch |

**Downsize while the provider is off (RP22-6 / RP31-8) — owner decision line, recommended:** a downsize is not new business: it is **allowed while the provider's flag is off, taking effect at the period end** (`paid_through`), never a refund now. Not built: it needs the scheduled change of § 1.2 (`next_*` columns) — well over the ~40 lines Prompt 26 allowed for it. Until built, the 409 stands and the page says to email support. *Owner: confirm or change this line (F-row for the owner batch).*
| Webhook route | **stays mounted** while the secrets are set: late notifications (a final invoice, a refund, a settle after the flip) must land. Only an unset secret unmounts a route (R10-01 unchanged) |
| Billing page | shows the entitlement sentence ("paid through 2027-03-01 via Stripe — renewal stopped") and the providers that ARE on |
| Deletion runner | unaffected: `Cancel` is a stop-money action and always allowed when the provider is configured; a `renewal='auto'` row whose provider is **unconfigured** still defers the tenant (today's rule); a `prepaid` row has nothing to cancel |
| Readiness gauge | `vaultaire_payment_provider_ready{provider}` = configured ∧ verified; the lapse job and the alerts read it |

### 2.4 Webhook contract for every provider (#752 kept)

`payments.ClaimEvent(ctx, db, provider, eventID, eventType, body)` is the
generic form of `claimEvent` (`webhook.go:142-203`): `BEGIN; SET LOCAL
lock_timeout='5s'; INSERT INTO payment_events … ON CONFLICT DO NOTHING
RETURNING` held open across the handler; no row = duplicate (200); then
`pg_advisory_xact_lock('entitlement:'‖tenant)` once the tenant is resolved,
so one tenant's events apply one at a time and never interleave with a
checkout start or a grant (§ 2.5). Every handler **fetches the object and
applies the fetched state**, never the payload's. A fetch error is a 5xx (the
provider retries); a fetch 404 is 200 + Warn. Stripe keeps `stripe_events`
and its own `claimEvent`; the two are the same contract with two tables, and
a `payment_events_all` view is the one place the admin page reads both.

### 2.5 The double-billing guard (R10-05 across providers)

A tenant never holds two live paid periods:

1. **One row** per tenant (`entitlements.tenant_id` PK) names the ONE provider
   allowed to extend it (`provider`) and whether anyone will (`renewal`).
2. **Extensions never overlap**: `Extend` is `GREATEST(paid_through, now()) +
   period`, so a second payment appends, never duplicates.
3. **Checkout start refuses** (409 `other_provider_live`) when the row's
   provider is another provider with `renewal='auto'` — Stripe must have been
   told `cancel_at_period_end` (§ 4) before a crypto invoice exists. The check
   and the write run under the tenant's advisory lock (the same lock the
   webhooks take), so a webhook and a checkout cannot pass each other.
4. **The Stripe path keeps `HasLiveSubscription`** (`house.go:456`) for a
   second Stripe subscription.
5. **The residual race** (a Stripe renewal lands after a crypto invoice was
   opened because the customer re-enabled renewal in the portal) is detected,
   not prevented: a settled payment on a row whose other provider also
   settled a period covering the same dates increments
   `vaultaire_payments_double_period_total`, writes `billing.double_period`
   to the audit log and e-mails the operator; the refund is by hand (§ 7).
   The portal's "reactivate subscription" is turned **off** in the Stripe
   dashboard (click-list) to make this rare.

A DB constraint cannot express "no two live periods across providers" (the
periods live in two systems); the lock plus the one-row rule is the
enforcement, the detector is the backstop.

## 3. Crypto with BTCPay Server (Part C)

### 3.1 Why BTCPay, honestly weighed (F-22)

| Option | Fee | Custody / KYB | Dependency | Verdict |
|---|---|---|---|---|
| **BTCPay Server, self-hosted** | 0 % (network fees only) | non-custodial: funds go to OUR watch-only xpub; no KYB | our own box; .NET app + bitcoind to run and back up | **Recommended** — it is the only option that makes "switch off Stripe" true: no processor can close our account |
| Stripe stablecoin payments (USDC on Ethereum/Solana/Polygon/Base/Tempo, USDP, USDG) | 1.5 % (third-party figure; Stripe's page does not print it) | Stripe KYB; settles to the Stripe balance in USD; refunds in stablecoin; **no disputes**; $10,000 per transaction; US businesses live, EU private preview (Stripe docs, fetched 2026-10-18) | Stripe | **Not a second provider**: it is a Stripe payment method. Worth one click in the Stripe dashboard if the owner wants USDC *through Stripe*; it does not satisfy the ask |
| NOWPayments | 0.5–1 % (its pricing page says 1 %; comparisons say 0.5 % without conversion) | custodial by default, KYB for payouts | a processor that can freeze or close the account | a hosted fallback if BTCPay operations prove too heavy; weaker on the "switch off" promise |
| Coinbase Commerce | 1 % | custodial, KYB | **wound down 2026-03-31** per secondary sources (migration to Coinbase Business, US/SG only); unverified against Coinbase | not available |

What BTCPay costs to run: § 3.7. What it does not give: fiat settlement (we
hold BTC; converting is the owner's act), tax computation (§ 6), dispute
handling (none exists — § 3.4).

### 3.2 Greenfield API, as used

- **Create invoice**: `POST /api/v1/stores/{storeId}/invoices` with
  `amount` (USD, decimal string from `QuoteHouse` cents), `currency: "USD"`,
  `metadata: {tenant_id, payment_id (our ledger uuid), purpose, order,
  period}`, `checkout: {expirationMinutes: 30, monitoringMinutes: 1440,
  speedPolicy, redirectURL: <dashboard billing page>, redirectAutomatically:
  true}`, `receipt: {enabled: true, showPayments: true}`. The customer is sent
  to BTCPay's hosted checkout (`checkoutLink`). Our `payments` row is
  inserted `pending` **before** the invoice is created, with the invoice id
  written back; a crash between the two leaves a pending row that expires.
- **Fetch invoice**: `GET /api/v1/stores/{storeId}/invoices/{id}` → `status`
  ∈ {`New`, `Processing`, `Expired`, `Invalid`, `Settled`}, `additionalStatus`
  ∈ {`None`, `PaidLate`, `PaidPartial`, `Marked`, `Invalid`, `PaidOver`}
  (the official OpenAPI, `swagger.template.invoices.json`, fetched
  2026-10-18); `GET …/payment-methods` → `rate`, `totalPaid`, `due`,
  `payments[] {id, receivedDate, value, fee, status ∈ Invalid|Processing|
  Settled, destination}` — the txid-level record for the ledger.
- **Mark**: `POST /api/v1/invoices/{id}/status` with `status: Settled |
  Invalid` (used by the paid-late automation, audited).
- **Refund**: `POST /api/v1/invoices/{id}/refund` with `refundVariant ∈
  RateThen | CurrentRate | OverpaidAmount | Fiat | Custom`, `payoutMethods`,
  `subtractPercentage`, `customAmount`, `customCurrency` — creates a **pull
  payment** the customer claims with an address; the owner approves and signs
  the payout (BTCPay refund docs). Manual by design here.
- **Webhook**: registered once in the store (click-list) for the invoice
  events; secret stored as `BTCPAY_WEBHOOK_SECRET` (systemd credential or
  `.env` through the 0600 path, like every secret). **Signature header**
  `BTCPay-Sig: sha256=HMAC256(UTF8(secret), body)` — verbatim from the
  official OpenAPI (`swagger.template.webhooks.json`: "The HMAC of the body's
  byte with the secret's of the webhook"); compare in constant time, reject
  400 on mismatch, cap the body at 1 MiB (BTCPay payloads are small),
  **no timestamp in the scheme** → replay protection is the dedup table.
  Common fields `deliveryId, webhookId, originalDeliveryId, isRedelivery,
  type, timestamp`; invoice events add `storeId, invoiceId, metadata`.
  Dedup key = `COALESCE(originalDeliveryId, deliveryId)` (a redelivery has a
  new `deliveryId`). Retry policy is BTCPay's `automaticRedelivery` ("after 10
  seconds, 1 minute and up to 6 times after 10 minutes" — the OpenAPI field
  description); our handler answers 5xx to be retried, exactly as with Stripe.
  **The queue (BTCPay 2.4.5 `WebhookSender.cs`, read 2026-10-21 — RP30-4):**
  deliveries of one webhook go through ONE queue (`MultiProcessingQueue`
  keyed by the webhook id), one at a time, on an `HttpClient` with no
  timeout set (.NET's 100 s); the automatic redelivery's waits run INSIDE
  the queued task — so one 5xx holds **every other invoice's events of the
  store for up to ~71 min**. Our handler therefore answers 5xx only for what
  a retry can fix (a database error, a Greenfield 5xx or no answer, the
  entitlement lock not free within 15 s); a permanent Greenfield refusal (a
  4xx: `MarkStatus` 422 outside `availableStatusesForManualMarking`, a 403
  without `canmodifyinvoices`, a 401 wrong key) is **200 + an Error line +
  `vaultaire_payment_webhook_total{provider="btcpay",outcome=mark_refused|
  fetch_refused}`** (rule `BTCPayPaymentNotApplied`, pages), its claim
  rolled back so the redelivery after the fix applies. A second webhook on
  the store has a queue of its own and delivery ids of its own — its
  deliveries of the same invoice can overlap ours (RP30-1, handled under
  the lock), so the store keeps **ONE webhook, ever**: delete the old URL
  before adding a new one (WP-DOMAIN-1).
- **API key**: a Greenfield key scoped to one store: `btcpay.store.
  cancreateinvoice`, `btcpay.store.canviewinvoices`, `btcpay.store.
  canmodifyinvoices` (mark + refund). Never the server admin key.


**Verified on a real BTCPay (the WP-PAY-4 lab, 2026-10-21 — BTCPay 2.4.5,
NBXplorer 2.6.19, bitcoind 31.1 on regtest; 84 deliveries captured into
`internal/payments/btcpay/testdata/`, every one's signature verified).**
Where the lab and the text above differ, the lab wins:

- **Signature**: exactly `BTCPay-Sig: sha256=<64 lowercase hex>` of
  HMAC-SHA256(secret, the raw body); no timestamp header; bodies are
  pretty-printed JSON — verify over the raw bytes.
- **Dedup**: `originalDeliveryId` is **always** set — equal to `deliveryId`
  on a first delivery, the original's id on a redelivery (`isRedelivery:
  true`, same `timestamp`). The dedup key is `originalDeliveryId`
  (`COALESCE` kept, harmless).
- **Payment method id** is `BTC-CHAIN` (2.x), not `BTC`; a payment's `id`
  is `<txid>-<vout>` (the ledger keeps the txid).
- **Mark**: `POST /api/v1/stores/{storeId}/invoices/{id}/status` (the store
  path — the key is store-scoped; `/api/v1/invoices/{id}/status` also
  works); a transition not in `availableStatusesForManualMarking` is 422.
- **No 404 for an unknown invoice**: `GET …/invoices/<unknown>` answers
  **403 `missing-permission` (canviewinvoices)**, even to an unrestricted
  key; a wrong key is 401. The handler reads that 403 as "no such invoice"
  (200 + Warn, counted `foreign`); a key that really lost the permission is
  caught by the readiness check below. **Corrected (RP30-3, Prompt 25):**
  RP29-4's remedy ("re-deliver from the BTCPay UI") did not work while the
  claim of a non-applied delivery was committed — a redelivery carries the
  original's `originalDeliveryId` and was a duplicate. Since Part 0 of
  Prompt 25 a verdict that applied nothing **rolls its claim back** and
  answers 200, so a redelivery (automatic or from the UI after the fix) is
  handled fresh.
- **The scoped key cannot read the store** (`GET /api/v1/stores/{s}` → 403,
  `canviewstoresettings`): readiness reads `GET …/invoices?take=1`
  (canviewinvoices) instead. `GET /api/v1/health` answers **200
  `{"synchronized":false}`** while the node is down — the body decides.
- **Refund** needs a **fourth** permission,
  `btcpay.store.cancreatenonapprovedpullpayments` (the three above → 403);
  it creates a pull payment and sends `InvoiceRefund {pullPaymentId}`. The
  provider never calls it today (refunds are recorded by hand, § 7); the
  owner adds the permission only to open refunds through the API.
- **Event types not in the design**: `InvoiceExpiredPaidPartial` (right
  after `InvoiceExpired{partiallyPaid}`), `InvoicePaidAfterExpiration` (a
  late payment that covers the invoice), `InvoiceRefund`.
- **`paidAmount`** (invoice, USD at the invoice rate) is the money input:
  15.00 on a 1.5× payment, 5.00 on a half. The crypto `due` grows by a
  network fee once a first partial payment arrived (`networkFeeMode
  MultiplePaymentsOnly`), so "paid in full" and credits are computed from
  `paidAmount`, never from `totalPaid ≥ due`.
- **SSRF**: BTCPay 2.4 refuses to deliver to a private address
  (`BTCPAY_SSRFEXCEPTIONS` lifts it) — prod's public URL is unaffected.

### 3.3 Invoice states → `paid_through`

| BTCPay event (fetched status / flags) | Ledger row | Entitlement |
|---|---|---|
| `InvoiceCreated` | `pending` (already) | — |
| `InvoiceReceivedPayment` (`afterExpiration=false`) | `processing` once the fetched invoice is `Processing` (BTCPay's own "paid in full, unconfirmed"), else stays `pending` | — (no grant on an unconfirmed payment) |
| `InvoiceProcessing` (`overPaid` recorded) | `processing` | — |
| **`InvoiceSettled`** (`manuallyMarked`, `overPaid` recorded) | `settled`, `settled_at`, `rate`, `crypto_amount`, `invoice_number` assigned | **Grant/Extend**: `paid_through = GREATEST(paid_through, now()) + period`; `provider = 'btcpay'`, `renewal = 'prepaid'`, `pending_provider` cleared; a resize applies `Order`; `overPaid` → `credit_cents` = the excess at the invoice rate (§ 3.4) |
| `InvoiceExpired` (`partiallyPaid=false`) | `expired` | — (nothing was paid; the customer may start again — a new invoice at a new rate) |
| `InvoiceExpired` (`partiallyPaid=true`) | `expired_partial`, `credit_cents` = what arrived **and confirmed** (§ 3.4 "Only confirmed bitcoin is value"; 0 while the payment is unconfirmed — the `InvoicePaymentSettled` of its confirmation sets it) | — ; e-mail: "we received $X of $Y; it is a credit on your next invoice, or ask for a refund" (§ 3.4) |
| `InvoiceReceivedPayment` with `afterExpiration=true` ("paid late") | the payment recorded on the expired row | **automation**: when the CONFIRMED amount ≥ `amount` (RP33-2, Stage 1a Prompt 27: the late payment's own status Settled — the lab: `Expired/PaidLate` with the payment `Processing` until it is mined, then `InvoicePaymentSettled` with the invoice still `Expired/PaidLate` and the payment `Settled`; departure kept: the crypto amount at the invoice's rate, not a then-rate — BTCPay records none), we `POST …/status Settled` (audit `billing.invoice_marked`, reason `paid_late_in_full`) → BTCPay sends `InvoiceSettled (manuallyMarked)` → the grant above. Seen but unconfirmed → **nothing yet** (the `InvoicePaymentSettled` of its confirmation fetches the invoice again and marks it). Less than due → credit of the confirmed part, as a partial |
| `InvoiceInvalid` (`manuallyMarked`, or the monitoring window of 24 h passed without confirmation: a double-spend, an RBF replacement, a dropped transaction) | `invalid`; `credit_cents` = what arrived **and confirmed** (RP31-1 b, RP33-2 — an unconfirmed payment credits nothing: `paidAmount` still counts it after the window) | — ; if a grant had been made from this invoice (impossible in the table above: we grant on `Settled` only) nothing to undo |
| **Lab (Prompt 27):** paid in full in time, never mined, the monitoring window passes | BTCPay makes the invoice `Invalid/None` (`paidAmount` 10.00, the payment `Processing`) — and sent **no `InvoiceInvalid` delivery** (2.4.5; the change was seen only after a BTCPay restart, 6 min past the window — open question RP34) | our row stays `processing` until another delivery fetches it; whatever fetches it credits 0 |
| **Lab (Prompt 27, RP33-11):** that payment is mined AFTER the Invalid | BTCPay still delivers **`InvoicePaymentSettled`** (the invoice stays `Invalid/None`, the payment `Settled`) | the fetched state closes the row `invalid` and credits the confirmed amount (1000 cents) — never a grant |
| **Lab:** `InvoiceSettled` with `manuallyMarked` and `paidAmount` < `amount` (an operator marked an UNPAID invoice Settled — BTCPay sends it with `paidAmount` 0) | unchanged | **no grant**, an Error line (an operator who means it records a manual payment) |
| **Lab:** a double spend / RBF replacement **before** confirmation | the invoice silently returns `Processing → New` (no event), then `InvoiceExpired{partiallyPaid:false}` → `expired` | — (it never becomes `Invalid`: Processing is not monotonic, nothing acts on it) |
| **Lab:** `InvoiceInvalid` after a hand-marked `Settled` (an operator's act) | stays `settled` | the grant stands, a Warn; the operator adjusts by hand |
| `InvoicePaymentSettled` | the `payments[]` entry marked confirmed; the fetched invoice is applied again (an Expired invoice paid late is marked Settled once confirmed; an Expired or Invalid invoice's credit grows to the confirmed amount) | — |
| Any event for an invoice our `payments` table does not know, or whose `metadata.tenant_id` ≠ the row's tenant | 200, Warn, counted `foreign` — the claim **released** (RP30-3: a redelivery is handled fresh) | — |
| **Prompt 25 Part 0:** two deliveries of one invoice that overlap (InvoiceSettled + InvoicePaymentSettled; two webhooks on the store; one outliving 100 s) | the second takes the tenant's lock after the first committed and re-reads the row (`LockedPending`, FOR UPDATE) — a replay | granted ONCE (RP30-1); an `Expired` fetch on a row no longer open (settled, refunded) writes neither a status nor a `partial:` credit |
| `Settled` with `paidAmount` < `amount` | unchanged, the claim released | no grant, an Error, counted `settled_unpaid` (pages — RP30-5) |
| Paid late in full, `MarkStatus` refused with a 4xx | unchanged, the claim released | no grant, an Error, counted `mark_refused` (pages); an operator marks it by hand |

**Confirmation policy** (`checkout.speedPolicy` per invoice): `MediumSpeed`
(1 confirmation) for invoices under $200, `LowMediumSpeed` (2) at or above.
**Verified in the lab:** the enum is `HighSpeed` / `MediumSpeed` /
`LowMediumSpeed` / `LowSpeed` (case-insensitive; an unknown name → 422;
omitted = the store default, `MediumSpeed`), settling at **0 / 1 / 2 / 6**
confirmations; `HighSpeed` waited for 1 confirmation on an RBF-signalling
payment (bitcoind 31's default). Lightning (later, § 3.6) is final at once.

**Reorg after `Settled`**: BTCPay does not revert a `Settled` invoice; a
reorg deeper than the chosen confirmation count after settle is our loss.
**Verified in the lab:** `invalidateblock` on the settling block, the
payment double-spent and the new chain mined 3 blocks (the payment at −2
confirmations, conflicted) — the invoice and the payment stay `Settled`,
`paidAmount` 10.00, and **no webhook is sent**: the loss is invisible to
the handler.
At 1–2 confirmations this is a documented, accepted risk for payments of
tens of dollars; the two-conf tier above $200 is the mitigation. The ledger
keeps the txid, so an operator can audit a suspected case by hand.

**Invoice expiry and the rate lock** are the same thing: BTCPay fixes the
crypto amount at creation and holds it for `expirationMinutes` (store
default 15 min, we pass 30). After it, a payment is "paid late" at whatever
the then-rate makes of it — handled above; a customer who wants certainty
pays inside the window.

### 3.4 Money edge cases

- **Pricing**: USD from `prices.json` through `QuoteHouse` — the same cents as
  Stripe's receipt (`house.go:216`). The crypto amount is BTCPay's rate
  source's business; we record the `rate` BTCPay used.
- **Underpayment**: never a partial grant (no fractional months). The shortfall
  is a `credit_cents` on the next invoice (we create it for `amount − credit`),
  or a refund on request. **Payment tolerance** stays 0 %: we do not accept
  "almost".
- **Overpayment**: `credit_cents`; refunded on request through
  `refundVariant: OverpaidAmount` (BTCPay computes it), else applied to the
  next invoice. Under $1 is kept (network fees make a refund absurd) — stated
  in the terms (F-28).
- **Late payment**: § 3.3 — settled in full = granted; short = credit.
- **Only confirmed bitcoin is value (RP33-2, Stage 1a Prompt 27).**
  `paidAmount` counts a payment the moment BTCPay sees it (`Processing` =
  "paid in full, unconfirmed"); an unconfirmed payment can still be
  replaced (RBF) or dropped. Everything that turns money into value
  WITHOUT BTCPay's own `Settled` — the `partial:` credit of an Expired or
  Invalid invoice, the paid-late mark — uses the CONFIRMED amount, from the
  invoice's `GET …/payment-methods` (`payments[].status` ∈ `Invalid` |
  `Processing` | `Settled`), each payment at its method's `rate` (USD/BTC),
  in cents, rounded half up once per sum (`btcpay.confirmedCents`):

  `confirmed = max(0, min(paidAmount − Σ value × rate over the payments not
  Settled and not Invalid, Σ value × rate over the Settled payments))`

  The first term keeps BTCPay's own net figure (a second payment's network
  fee — `MultiplePaymentsOnly` — counts toward nothing); the second never
  exceeds what the Settled payments are worth. Lab: 0.0001181 BTC × 84675 =
  10.0001 → 1000 cents once `Settled`, 0 while `Processing`; Prompt 27's
  regtest 0.00012249 × 81642 = 10.0003 → 1000. A grant still waits for
  BTCPay's `InvoiceSettled` with `paidAmount ≥ amount` (unchanged).
- **Refunds**: manual, in the coin paid (BTCPay pull payment), at the rate of
  the original payment (`RateThen`) for a refund of a purchase, at the
  current rate (`CurrentRate`) for returning an overpayment's excess —
  recorded as a `payments` row `kind='refund'`, `refund_of` the original,
  negative `amount_cents`, by the admin tool (§ 7) which also shortens
  `paid_through` when the refund is for unused time. Never in fiat: we hold
  no fiat from a crypto sale.
- **No chargebacks**: a bitcoin payment cannot be reversed by the payer's
  bank — there is no dispute process, no reserve, no 90-day exposure. This
  is an advantage worth stating in the terms, and its mirror: every refund is
  our policy, so the policy must be written down before the first sale (F-28).
- **Network fee**: BTCPay's `networkFeeMode` stays the store default (added
  only when the customer pays an invoice in several transactions).

### 3.5 Plan changes

- **Resize up** mid-period: `Provider.Resize` → a `purpose='resize'` top-up
  invoice for `(QuoteHouse(new) − QuoteHouse(old)).MonthlyCents ×
  remaining_days / period_days` (annual: the yearly difference pro-rated by
  day); on `Settled` the house becomes `new`, `paid_through` unchanged.
  Prorations are therefore ours, not Stripe's, for crypto — the same formula
  is printed on the invoice.
- **Resize down**: no refund; the dashboard refuses a floor below its usage
  (the Stripe rule, kept); takes effect immediately with no money movement
  (the customer gave up quota they paid for — stated on the page), the next
  renewal is priced at the new size. Rejected: effective at renewal
  (`next_*` columns) — supported by the schema for the period switch, but a
  downsize that waits invites "I clicked down and you still bill me".
- **Period switch** (monthly ↔ annual): at renewal only — `next_period` set,
  the next invoice priced on it. Same rule as Stripe ("period fixed for the
  life of a subscription", `house.go`), now self-serve instead of support.

### 3.6 Coins (F-24)

**Launch set: BTC on-chain only.** Reasons: it is BTCPay's core (every other
coin is a plugin the project explicitly does not maintain — "Bitcoin is the
only focus", BTCPay Altcoins FAQ); a watch-only xpub keeps every spending key
off every server; the operational surface is one pruned node.

- **Lightning**: the right rail for $4.99–$50 payments (on-chain fees can
  exceed a monthly Vault payment in a fee spike — honest downside of on-chain
  only; the billing page will steer crypto payers to annual). But LND/CLN is a
  **hot wallet with keys on the server** and channel liquidity to manage.
  Recommendation: WP-PAY-6, after a month of on-chain operation, inbound-
  only channels, a hot balance cap with an automatic sweep to the cold xpub
  (BTCPay's "Lightning → on-chain" is manual; the cap is our alert + the
  owner's weekly sweep until automated).
- **Monero**: the plugin is community-maintained since BTCPay 2.1 (the
  project moved XMR and ZEC out of core); it is the coin of the Mullvad
  posture and belongs with the numbered accounts (43.5), not before them.
- **Stablecoins**: USDt on Liquid needs an Elements node; the Tron and ERC-20
  plugins are community plugins. Not at launch. A customer who wants to pay
  in USDC can do so **through Stripe's stablecoin method** (F-22 table) —
  which is the honest answer: that path keeps the Stripe dependency.

### 3.7 Wallets and hosting (F-23)

**Wallet**: a hardware wallet the owner holds (Coldcard / Trezor / Ledger);
BTCPay imports its **account xpub** (BIP84, `m/84'/0'/0'`) as a watch-only
hot wallet — BTCPay generates addresses, never signs. Spending = the owner
signs on the device (BTCPay's PSBT flow or the vendor's app). **Recovery** =
the BIP39 seed (steel, two copies) + the derivation path + the xpub written
into the F-14 escrow envelope; BTCPay's own database adds nothing the seed
cannot recreate except invoice history, which `payments` mirrors. Lightning
(later) adds a hot seed and SCB backup — a different risk class, kept small
by the cap.

**Where it runs — recommendation: a dedicated small VPS, never SLC.**

| Option | For | Against |
|---|---|---|
| SLC beside prod (Docker; 32 cores, 121 GB RAM, 3.4 TB NVMe free, measured 2026-10-18) | free; fast | **no Docker installed**; UFW 22/80/443 and HAProxy would front a third-party .NET app + a bitcoind on the money/data box — a BTCPay CVE becomes a foothold next to PostgreSQL and the master key credential; the owner's standing rule "nothing non-Vaultaire goes on SLC" (2026-10-13) |
| Hostbrr / Maxko | exist | shared cPanel, no Docker, no daemons (Hostbrr AUP); Maxko LVE 1 core / 5 GB — a pruned bitcoind's IBD would run for days |
| **A small VPS** (4 vCPU, 8 GB, 160 GB NVMe class — Hetzner CPX31/CX32 ≈ €8–15/mo; Maxko 4c/4GB/50GB €14 is too small on disk) | isolates the processor; BTCPay's docker stack is built for exactly this; `btcpay-backup.sh` | one more box to patch (Docker images update themselves via `btcpay-update.sh`; the OS is ours) |
| The F-18 EPYC standby | would host it well | not bought yet; do not block payments on it. Move there later if wanted |

Sizing (BTCPay deployment FAQ, fetched 2026-10-18): minimum 2 GB RAM and
80 GB with pruning; fragments `opt-save-storage-s` ≈ 50 GB (6 months of
blocks), `-xs` ≈ 25 GB (3 months), `-xxs` ≈ 5 GB (2 weeks, **no Lightning**).
Pick `opt-save-storage-s` on 160 GB; initial block download ≈ 1–2 days on a
VPS. Stack: `BTCPAYGEN_CRYPTO1=btc`, `NBITCOIN_NETWORK=mainnet`,
`BTCPAY_HOST=pay.stored.cloud` (Let's Encrypt by the stack's nginx),
`BTCPAYGEN_LIGHTNING=none` at launch (`lnd` in WP-PAY-6). Cloudflare: DNS-only
for `pay.` (the checkout is BTCPay's own TLS; proxying hides nothing useful).

**Backup**: nightly `btcpay-backup.sh` (its Postgres + config) shipped to the
same off-box Sync leg as `pg-backup.sh`, 30 days; the store settings and the
webhook secret documented in the runbook; the wallet needs no backup (watch-
only). Restore drill once before the first real invoice.

**When BTCPay is down**: `Ready()` = false after two failed health checks
(GET `/api/v1/health` + a store read, every 60 s) → the crypto button is
hidden, `POST` answers 503 `provider_unavailable` + `Retry-After`, renewals
cannot be paid — and the **lapse job skips every `btcpay` row** while the
provider was unready at any point in the last 24 h (§ 1.3), extending grace
by the outage. Alerts: `PaymentProviderDown` (§ 7). Never a house cleared
because our processor was down.

### 3.8 Renewal UX, receipts, retention

- **Reminders** (`prepaid` rows only; via the existing `email.Sender` — prod
  = Cloudflare Email Service since 2026-10-12): T−14 d, T−3 d, T+1 d ("in
  grace, N days left"), T+(grace−1) d ("last day"). Templates
  `renewal_reminder.html`, `house_lapsed.html`, `payment_receipt.html` beside
  `password_reset.html`. Sent by the `payment_reminders` job (hourly; one row
  per (tenant, step) in `payments_reminders_sent` or a JSONB on the
  entitlement — the WP decides; never twice).
- **Dashboard**: the billing page shows the entitlement sentence, "Renew now
  / add a year" (`StartCheckout(purpose=renew)` → BTCPay's checkout page),
  "Switch to crypto at renewal" / "Switch to card" (§ 4), the receipts list.
- **Receipts we issue** (crypto and manual; Stripe keeps Stripe's invoices,
  mirrored into the ledger): gapless numbering `ST-<year>-<000001>` from
  `invoice_counters` under `FOR UPDATE`, assigned at settle; the receipt
  page `/dashboard/billing/receipts/{id}` (print-ready HTML) names the
  issuer (FairForge LLC, address from `internal/hosts` legal data), the
  tenant's e-mail, line items from the house snapshot, the USD amount, the
  crypto amount + rate + BTCPay invoice id, "paid" and the date. No PDF
  generator; the browser prints.
- **Retention**: `payments` rows are **accounting records** — kept 7 years
  (US) — never pruned by the retention job; `payment_events` 90 d (like
  `stripe_events`); the privacy policy and the DPA state both (change them
  together, as for every period in `retention.go`).
- **Erasure** (`account.EraseRows`): `entitlements` DELETE; `payments` **UPDATE**
  — `tenant_id = NULL`, `metadata` stripped to provider ids (invoice id, pull
  payment id; **addresses and txids removed** — they are pseudonymous
  personal data once linked to a person), `note` cleared; `payment_events`
  `tenant_id = NULL` (as `stripe_events`); the schema-walk tests
  (`TestErasedTablesCoverSchema`, `TestExportSectionsCoverSchema`) must name
  every new table. **Export**: `entitlements` and `payments` are the
  customer's data (included); `payment_events` are the processor's records
  (listed with a reason, as `stripe_events`). The BTCPay store keeps its
  invoice (address, txid, our opaque `tenant_id`) — it is our accounting
  record too; BTCPay can archive but not delete invoices. The privacy policy
  says so.

### 3.9 Policy sentences (WP-PAY-3 item 8 — drafted for the owner's approval, NOT wired into the pages)

The privacy policy and the terms state the retention periods
(`internal/api/retention.go` and the policy change together); these are the
sentences the money adds. They are a draft in this document until the owner
approves them (F-28 refund policy) and a docs PR puts them into
`internal/dashboard/templates/legal/{privacy,terms}.html`.

**Terms — Payments and refunds (F-28):**

> A house is prepaid for its period. A payment is refundable in full within
> 14 days of the day it was made if nothing was added to the house in that
> time; after 14 days a prepaid period is not refundable, but you may stop
> the renewal at any time and the house runs to the end of what you paid for,
> then a 14-day grace period. Refunds are made in the coin you paid in: a card
> payment to the card, a bitcoin payment to an address you give us, at the
> rate recorded on the receipt. A refund of less than one US dollar is kept.
> Bitcoin payments are final once confirmed: there is no chargeback on a
> crypto payment, and a double-spent or unconfirmed payment buys nothing.

**Privacy policy — Payment records:**

> We keep a record of every payment — its amount, what it bought, the date,
> our receipt number and the payment provider's own reference — for seven
> years, as accounting law requires. When your account is erased these
> records stay but are de-linked from you: the link to your account, any
> note, any cryptocurrency address or transaction id, and the payment
> provider's customer id for you are removed from them. Event bodies our
> payment providers send us (webhooks) are kept 90 days. Stripe, Inc. keeps
> its own records of your card and invoices under its own policy; a bitcoin
> payment's invoice (address, transaction id and our opaque account id)
> stays in our BTCPay Server as an accounting record, which cannot be deleted
> but is never linked back to you after erasure.

The customer-id stripping the second paragraph promises is implemented in
`account.EraseRows` since WP-PAY-3 (`metadata #- '{ids,customer}'`; RP20-6).

## 4. Switching a customer between providers (Part D)

### 4.1 Stripe → crypto, per tenant

1. Customer clicks "Switch to crypto at renewal". Under the tenant lock:
   `renewal = 'prepaid'`, `pending_provider = 'btcpay'` and a pending
   `renewal_stop` row for the subscription, in one transaction; after the
   commit Stripe is told to stop by the subscription's id
   (`StopRenewalRef`: `cancel_at_period_end = true`, or cancelled now when
   the period is in dunning — Stage 1a Prompt 27, RP33-3/RP33-7; the
   build's departures in § 4.4); audit `billing.renewal_stopped`.
   `paid_through` is untouched — they keep what they paid for.
2. From T−14 d the reminders and the page offer the crypto invoice
   (`purpose='switch'`, priced at the current `prices.json`); paying early
   **extends from `paid_through`**, so there is no overlap and no gap.
3. On `InvoiceSettled`: `provider = 'btcpay'`, `provider_ref` = invoice id,
   `pending_provider` cleared. Stripe's subscription ends at its period end
   on its own (`customer.subscription.deleted` → `renewal` already prepaid,
   nothing to do).
4. Not paying → grace → lapse (no gap unless the customer never pays).
5. The portal race (customer un-cancels at Stripe meanwhile): the `updated`
   event shows `cancel_at_period_end=false` → `renewal='auto'` again,
   `pending_provider` cleared, any `pending` crypto invoice is marked
   `Invalid` (audited); if the crypto invoice **already settled** both have
   paid → § 2.5's detector, refund by hand. Portal reactivation is turned off
   in the click-list to keep this to the one case the API cannot prevent.

### 4.2 Crypto → Stripe

`StartCheckout(purpose='switch')` on Stripe with
`subscription_data.trial_end = paid_through` (stripe-go v75 has
`CheckoutSessionSubscriptionDataParams.TrialEnd` and `BillingCycleAnchor`,
verified in the module cache 2026-10-18): the subscription is `trialing` until
the date the crypto period ends, then bills — our model treats `trialing` as
"paid through the trial end", so the row flips to `provider='stripe',
renewal='auto'` on `checkout.session.completed` with `paid_through`
unchanged. Stripe requires `trial_end` ≥ 48 h ahead; inside 48 h the checkout
starts now and the remaining crypto days are forfeited (stated on the page).
Rejected: `billing_cycle_anchor` with `proration_behavior=none` — it charges
the first period immediately in some configurations; `trial_end` is the
unambiguous "first charge on this date".

### 4.3 Bulk: the owner flips Stripe off with N live subscribers

The flag alone stops new business (§ 2.3). To end Stripe billing for everyone:
`POST /api/v1/admin/payments/stripe/stop-renewals` (`dry_run=true` by
default; the response lists every tenant with its `paid_through`), then with
`dry_run=false`: for each live subscription `StopRenewal`, `renewal =
'prepaid'`, one audit row per tenant (`billing.renewal_stopped`, actor the
admin, reason `bulk`), one e-mail each ("paid through <date>; to continue,
pay with crypto from your billing page, or write to support"). The lapse job
then clears each at `paid_through + grace` unless they pay. **Back on**:
flag ON + `resume-renewals` (`cancel_at_period_end=false`) for every row
whose period still runs and whose `pending_provider` is unset. "Without any
paying customer losing storage" = every date is honoured; a customer who
does not pay anyone loses the **house** (quota) after grace, never a byte.

### 4.4 As built (WP-PAY-5, Stage 1a Prompt 25) — where the build departs

- **Implicit switch (RP30-2, item 0).** A BTCPay invoice settles up to 24 h
  after it opened, after a Stripe checkout that passed the guard: a prepaid
  settle over a house another provider renews `auto` is an **implicit
  switch** — the BTC period appended from `paid_through`, the house named
  btcpay/prepaid, and the old subscription written into a `renewal_stop`
  row (`payment_events`) **in the same transaction**. The Stripe stop
  (`cancel_at_period_end`) is called **after the commit** (and every 10 min
  by the `renewal_stops` job until confirmed), never before the grant: a
  Stripe outage must not turn a BTCPay delivery into a 5xx that stalls the
  store's queue (RP30-4). Until confirmed the row keeps the subscription
  findable — the deletion runner cancels it; the Stripe webhook never lets
  it take the house back (a granting event of it is not applied; an
  un-cancel re-asserts the stop). `isCurrentSubscription` deliberately does
  NOT name it: its clearing events must not clear the house that replaced
  it. Rule `RenewalStopsPending` (> 0 for 1 h).
- **Checkout side (item 0 c).** A provider that renews by itself is refused
  409 `crypto_invoice_open` while a prepaid invoice of the tenant is
  pending/processing or expired inside the 24 h monitoring window; the page
  names the date it stops being payable.
- **§ 4.1 step 5, a switch that already settled:** besides the detector,
  the stop is **re-asserted** (prevention first; the detector stays the
  backstop if Stripe bills anyway).
- **§ 2.5 point 5, the detector:** an overlap on **both** bounds
  (`other.end > this.start AND other.start < this.end`; "end > start" alone
  would flag a later, non-overlapping prepaid period) and never a $0 row (a
  trialing subscription's first invoice, a credit-covered order paid
  nothing twice). Counted `vaultaire_payments_double_period_total{provider}`,
  audited `billing.double_period`, mailed to the support address; rule
  `PaymentDoublePeriod`.
- **§ 4.3 resume:** only rows whose **last** renewal change was the bulk
  stop (audit `reason = bulk`), period still running, no pending switch —
  a customer who stopped their own renewal is never resumed by the owner.
  Bulk = a manual job (`stripe_bulk_renewals`): dry run by default (200 +
  the list), `dry_run:false` = 202, 409 while one runs; one audit row and
  one mail (`renewal_stopped.html`) per tenant; never cancels a period.
- **Prompt 26 Part 0 (#873, the WP-P25 review's RP31-1…7):**
  - **RP31-1:** `InvalidateOpenInvoices` (the portal race, the bulk resume) FETCHES each open invoice and marks it Invalid only when **provably unpaid** — New or Expired with `paidAmount` 0. Processing (= paid) and partly-paid invoices are left to settle (their settle is an implicit switch, which re-asserts the Stripe stop), counted `vaultaire_payment_webhook_total{btcpay,invalidate_skipped_paid}`. **§ 3.3's Invalid row now credits:** money on an Invalid invoice that never settled is the tenant's credit (`partial:<invoice>`), never lost.
  - **RP31-2, the detector:** a `resize` row on either side never matches (a top-up prices the rest of a period already bought); a period that starts inside `MinDeferredStart` of another provider's end is the designed forfeit (the switch to card, or a renew, inside 48 h) — `billing.prepaid_forfeited` {days}, no counter, no mail.
  - **RP31-3:** the "switched away" check is read inside the grant transaction after the entitlement lock.
  - **RP31-4:** an implicit switch from a `past_due` / `unpaid` / `incomplete` subscription **cancels it now** (no proration, no final invoice): its period was never paid, and `cancel_at_period_end` left Stripe's dunning retrying. A paid period still runs out.
  - **RP31-5:** the `crypto_invoice_open` 409 offers **"Cancel the bitcoin invoice and continue"** — only for an invoice nobody paid (RP31-1's rule); it is marked Invalid, our row closed at once, and the refused checkout continues. One holding a payment keeps the 409 with its date.
  - **RP31-6:** `SwitchToPrepaid` commits the row, then stops the renewal; a refused stop puts the row back (only if unchanged).
  - **RP31-7:** with `entitlement_lapse` OFF a Stripe house whose renewal is off ends at Stripe's deletion on the date, no grace; the billing page and the reminders say "Your house ends on <date> unless the bitcoin payment arrives" (or "unless you renew") instead of promising grace.
- **§ 4.2** is RP24-1's path with purpose `switch`: `trial_end =
  paid_through` (≥ 48 h, else starts now and the page says the prepaid
  days are forfeited); the completed trialing subscription flips the row to
  stripe/auto, the date kept.

## 5. Accounts, privacy, abuse (Part E)

- **Who may pay in crypto (F-25, recommendation)**: **every identified
  account** (an e-mail-verified tenant). The rail does not change the
  account's identity; it removes the card. The abuse lever the north star
  names is *anonymity + public surfaces*, so: a tenant whose only payments
  are crypto may create **public buckets / CDN hostnames only after 30 days
  as a paying customer** (or after any card payment); numbered accounts
  (43.5) are **private-only, always** (the north star's rule, unchanged).
  The owner may instead choose "crypto only for 43.5" (the conservative
  reading) — that keeps crypto dark until 43.5 ships.
- **Signups** stay closed by the `signups` flag; crypto changes nothing
  there. **Free tier** unchanged; there is no free crypto trial. Fraud
  shape: stolen cards do not exist on this rail; the risk is content abuse,
  handled by the gate above and the existing abuse tooling.
- **What we learn about a payer**: BTCPay holds the invoice (addresses,
  txids, amounts, the checkout page's user agent in its logs) keyed by our
  opaque `tenant_id`; our `payments` row holds the USD amount, the rate,
  the provider ids and — until erasure — the txids in `metadata`. An on-
  chain address is pseudonymous personal data once we can link it to an
  e-mail: retained as part of the accounting record (7 years), stripped from
  our row at erasure (§ 3.8), stated in the privacy policy. No payer IP is
  recorded by us (BTCPay's nginx logs are rotated on its box).

## 6. Tax, legal, accounting — for counsel, not decided here (Part F)

- **Sales tax / VAT.** Stripe Tax computes and collects for Stripe payments
  only. A crypto sale is a sale we must tax ourselves: US state taxability of
  cloud storage varies (SaaS is taxed in roughly 20 states and DC as of
  2026; Texas taxes it as a data processing service; California and
  Colorado start taxing SaaS on 2027-01-01 — third-party guides, unverified
  per state), and **the SLC server is physical presence in Utah**; economic
  nexus elsewhere is far above our volume. EU B2C digital services are
  taxed where the customer is, with the non-Union OSS scheme for a non-EU
  seller from the first sale (no threshold) — **unverified**. The ledger
  records the customer's country when known (Stripe's billing address; for
  crypto we have an e-mail and nothing else — a collection question).
- **"Prepaid" is a billing shape, not a tax treatment.** The north star's
  "tax handled as prepaid" is **unverified and probably wrong**: tax is due
  on the supply, and when it is invoiced; paying a year ahead does not change
  what is taxable. The 43.5 line is corrected to point here.
- **Money transmission / sanctions.** Accepting bitcoin for our own service
  is not money transmission (FinCEN's 2013/2019 guidance treats a merchant
  as a "user", not an MSB) — counsel confirms. OFAC sanctions bind a US
  company on every rail; Stripe screens its stablecoin payers, BTCPay
  screens nobody. Question: do we screen payer addresses (Chainalysis /
  Elliptic-class APIs, a monthly cost) or accept the small-merchant posture
  with a documented policy to refuse and refund a flagged payment.
- **Bookkeeping.** Revenue is the USD invoice amount at receipt (the rate
  recorded on the row); the bitcoin received is property on the books, with
  gain or loss on disposal (IRS Notice 2014-21 shape). The admin revenue page
  counts `payments.amount_cents` by month regardless of provider; a "crypto
  held" line is a separate, owner-side number.

**Questions for an accountant / counsel, in plain words:**

1. Which US states tax what we sell (object storage sold as monthly quota), and does the Utah server create a Utah collection duty today?
2. For sales to EU/UK consumers paid in bitcoin, must we register for VAT (non-Union OSS) from the first sale, and what evidence of the customer's country is enough when all we hold is an e-mail?
3. Does being paid a year in advance change when tax is due or how revenue is recognised?
4. Is accepting bitcoin for our own service money transmission anywhere we sell (US federal, Utah, EU)?
5. Must we screen payer addresses against sanctions lists, and what do we do with a payment from a listed address?
6. How do we book bitcoin received (basis, disposal, the rate source we must use), and is a gapless receipt number per year required anywhere we sell?
7. Does our refund policy (prepaid, non-refundable after 14 days, overpayments under $1 kept) comply with US state and EU consumer law for a B2B-and-B2C service?
8. How long must payment records be kept, and may we keep them after a customer's erasure request (our answer: 7 years, anonymised)?

## 7. Everything else that touches money (Part G)

- **Metered reporter and legacy packs: delete** (F-29) — WP-PAY-0 is
  WP-R10-4 executed first: `registerStripePlans`, `HandleUpgrade`,
  `MeteredReporter`, `STRIPE_METER_*`, `AccruedCents`, the pack
  `CreateCheckoutSession`, `spending_cap_cents` writers; `prices.json` is the
  one price source. Quota-sold on all tiers is the owner's 2026-09-21
  decision; nothing on prod is metered. Doing it first halves the surface
  the provider refactor must carry.
- **Metrics** (`internal/payments/metrics.go`): `vaultaire_payments_total
  {provider,kind,outcome}`, `vaultaire_payment_webhook_total{provider,event,
  outcome}`, `vaultaire_payment_provider_ready{provider}` (gauge),
  `vaultaire_entitlements_expiring{window="14d"|"3d"|"grace"}` (gauges read
  from the table on scrape, so right after a restart — the R13 lesson),
  `vaultaire_entitlements_lapsed_total`, `vaultaire_payments_double_period_
  total`, `vaultaire_payment_reminders_total{step,outcome}`. **Rules**
  `deploy/monitoring/vaultaire-payments.yml`: `PaymentProviderDown` (ready=0
  for 15 m while its flag is on), `PaymentWebhookFailing` (5xx ratio),
  `DoublePeriodDetected` (any), `EntitlementLapseSkippedUnready` (the job
  skipped rows for 2 days running); the generic `JobStale` covers the jobs.
- **Audit rows** (`internal/audit`, `event_type='billing'`):
  `billing.house_granted` (every grant: provider, dates, house),
  `billing.house_lapsed`, `billing.renewal_stopped`/`_resumed` (actor:
  customer or admin, reason), `billing.provider_switch_requested`,
  `billing.payment_recorded_manual`, `billing.paid_through_adjusted`,
  `billing.refund_recorded`, `billing.invoice_marked`, `billing.double_
  period`; a provider flag flip is the existing `flag.set`.
- **Admin tooling** (admin JWT, every call audited; the dashboard's tenant
  page gets a "Billing" panel calling them): `GET /api/v1/admin/tenants/{id}/
  payments` (ledger + entitlement); `POST …/payments` (a manual payment:
  provider `manual`, amount, period, note → Grant/Extend — wire transfer,
  support credit); `POST …/entitlement` (set `paid_through` / house, reason
  required); `POST …/refunds` (record a refund made outside: amount,
  `refund_of`, provider ref, whether to shorten `paid_through`); `POST
  /api/v1/admin/payments/stripe/stop-renewals|resume-renewals` (§ 4.3).

## 8. What can be proven where

| | On prod now (no Stripe or BTCPay keys) | Only in a lab |
|---|---|---|
| Migration 096 applied, 0 rows; the lapse job runs `ok` with 0 candidates; the reminder job sends nothing; metrics present | yes | |
| The dashboard binary has no `stripe-go` import; the billing page renders the free-tier sentence | yes | |
| Stripe webhook → entitlement; `past_due` keeps the house; `canceled` keeps it until the date; the lapse clears after grace | | Stripe test mode + `stripe listen`, or the signed fixtures of `webhook_signed_test.go` |
| BTCPay: signature, redelivery dedup, each state transition, paid-late automation, the resize invoice | | BTCPay regtest in Docker (the WP-PAY-4 spike records real deliveries as fixtures) |
| The switch both ways, the bulk stop, the double-period detector | | Stripe test mode + BTCPay regtest together |
| A real bitcoin payment landing in the owner's wallet | | mainnet, one invoice, the owner's own money, before the flag flips for anyone else |

## 9. Lab spike, honestly

**Done 2026-10-21 (Stage 1a Prompt 24):** BTCPay 2.4.5 / NBXplorer 2.6.19 /
bitcoind 31.1 on regtest in colima, the compose assembled by hand from
btcpayserver-docker's fragments (`a561533`); every § 3.3 row driven with
`bitcoin-cli`; the facts are in § 3.2's "Verified" block and the § 3.3
rows marked **Lab**; the containers, volumes, images and the clone were
deleted after (`docs/reviews/WP-P24.md`). The paragraph below is the
planning session's, kept for the record.

No BTCPay was run during this planning session: the Mac's Docker daemon
(colima) was not running and the deliverable is the design. Every protocol
fact above comes from BTCPay's **official OpenAPI templates** (webhooks,
invoices, stores — fetched from the `btcpayserver` repository 2026-10-18)
and its deployment FAQ, and is marked where a secondary source was used. The
spike is **mandatory in WP-PAY-4**: BTCPay on regtest, every invoice event
captured as a test fixture, the `SpeedPolicy` names and the `BTCPay-Sig`
bytes verified against a real delivery before the handler is written.
