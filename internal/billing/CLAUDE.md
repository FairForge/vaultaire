# internal/billing

Stripe billing integration for stored.ge subscriptions, payments, and invoices.

## Key Types

- **StripeService** — manages Stripe customers, checkout sessions, subscriptions, invoices, and billing portal. Holds `*sql.DB` for persisting Stripe IDs to the `tenants` table.
- **WebhookHandler** — `http.Handler` for `POST /webhook/stripe`. Verifies signatures, processes events, updates DB.
- **Plan** — maps an internal plan ID to a Stripe Price ID with display metadata.
- **InvoiceRow** — formatted invoice data for dashboard templates.
- **OverageService** — tracks grace periods for tenants exceeding quotas.

## StripeService Methods

| Method | Purpose |
|--------|---------|
| `CreateCustomer(ctx, email, tenantID)` | Create Stripe customer, persist `stripe_customer_id` |
| `GetCustomerID(ctx, tenantID)` | Look up Stripe customer ID from DB |
| `CreateCheckoutSession(customerID, planID, successURL, cancelURL)` | Create checkout for a registered plan |
| `GetSubscription(ctx, tenantID)` | Fetch subscription from Stripe |
| `CancelSubscription(ctx, tenantID)` | Cancel at period end, update DB status |
| `GetInvoices(ctx, tenantID, limit)` | List recent invoices from Stripe |
| `CreateBillingPortalSession(ctx, tenantID, returnURL)` | Self-service billing portal |
| `SaveSubscription(ctx, tenantID, subID, status, plan)` | Persist subscription state (called by webhook) |
| `LookupTenantByCustomer(ctx, customerID)` | Reverse lookup tenant from Stripe customer ID |
| `RegisterPlan(plan)` | Register a plan with Stripe Price ID at startup |

## Webhook Events Handled

| Event | Action |
|-------|--------|
| `checkout.session.completed` | Activate subscription, save to DB |
| `invoice.payment_succeeded` | Mark subscription active |
| `invoice.payment_failed` | Mark subscription past_due |
| `customer.subscription.created` / `.updated` | Sync status and plan; house subscriptions → floor quotas (`applyHouse`) |
| `customer.subscription.deleted` | Downgrade to starter, clear subscription (and the house's floor quotas); ignored for a stale subscription id |

## Environment Variables (wired in server.go)

- `STRIPE_SECRET_KEY` — Stripe API key (server.go:169)
- `STRIPE_WEBHOOK_SECRET` — webhook endpoint secret (server.go:171)

## Wiring

- Webhook route: `POST /webhook/stripe` registered in server.go:394
- Checkout + billing portal: wired in dashboard billing handler
- Registration → auto-creates Stripe customer (server.go:511-514)
- Stripe event idempotency via `stripe_events` table (migration 019)

## Whole-TB House Checkout (dashboard plan Phase 1, `house.go`)

The site sells storage as a house (downstairs = Standard, attic = Vault, whole
TB per floor, pin-hot add-on on Standard). Billing sells exactly that: **one
Stripe subscription per tenant, one item per line, `quantity` = TB.**

- **Prices** live only in `internal/api/landing/prices.json`. Six Stripe price
  ids come from env (`STRIPE_PRICE_{STANDARD,VAULT,PINHOT}_{ANNUAL,MONTHLY}`);
  `ConfigureHouse` + `StartHouseVerification` fetch each one and assert amount /
  USD / interval against the file (retry every 5 min until it passes, then
  hourly). `HouseCheckoutReady()` is false until then and the page says so.
- **Stripe setup (test and live, by hand in the dashboard):**
  Standard annual = yearly, per unit, **$53.88** (4.49 × 12); Standard monthly =
  monthly, per unit, **$4.99**; Vault annual = yearly, per unit, **$24.00**;
  Vault monthly = monthly, **volume tiers**: 1 unit → flat **$4.99** (unit $0),
  2+ units → **$2.55**/unit (this is the "$4.99 monthly minimum" the site
  prints); Pin-hot annual = yearly, per unit, **$36.00**; Pin-hot monthly =
  monthly, per unit, **$3.00**. Coupons: `AllowPromotionCodes` is on (LET code).
- `QuoteHouse(std, vault, pin, period)` is the receipt (per-line cents, the
  Vault monthly minimum for 1 TB, ×12 when annual); `ValidateHouseOrder` = the
  product rules (≥1 floor, ≤300 TB/floor, pin-hot ≤ downstairs, known period).
  **Attic-only houses are sold** and **both periods ship** (decisions 2026-09-27).
- `CreateHouseCheckout` → hosted Checkout (subscription mode, metadata
  `house=1`, `tenant_id`, `ClientReferenceID`); `UpdateHouseSubscription` resizes
  in place with `create_prorations` (quantity changes; a line at 0 is deleted;
  the **period is fixed** for the life of a subscription — switching = support).
  `HouseFromSubscription` maps items back to an order (false for legacy packs).
- **Webhook → quotas:** `SetHouseQuotas(usage.QuotaManager)` wires `applyHouse`,
  called from `checkout.session.completed` (fetches the subscription — the
  session carries no items) and `customer.subscription.{created,updated}`:
  active/trialing/past_due → `SetHouse` (floor rows, total = sum, tier
  standard|vault, `tenants.plan='house'`, `house_period`); canceled/unpaid/
  incomplete_expired and `customer.subscription.deleted` → `ClearHouse` (free
  tier limits, **used bytes untouched** — billing never deletes). Clearing is
  honoured only for the tenant's current `stripe_subscription_id`, so a stale
  subscription's events cannot take a paid house away. Idempotent; replays
  are no-ops (`stripe_events` dedup on top).
- **Never metered:** `ReportDaily` and the dashboard's accrued estimate skip any
  tenant with floor rows (quota-sold), even when `tier='standard'`.
- Tests: `house_test.go` — quote maths vs the price file, verification
  mismatches, checkout/update params, and DB-backed webhook → floor quotas.

## Metered Usage Reporting (Phase 2.7)

`metered.go` — **MeteredReporter** reports daily usage for metered tiers
(`standard`, `performance`) to Stripe Billing Meters. Fixed-price Vault packs and
the free tier are never metered.

- `NewMeteredReporter(stripe, db, logger, storageMeter, egressMeter)` — meter args
  are the Stripe Billing Meter **event-name** strings.
- `ReportDaily(ctx, date)` — for each metered tenant with a `stripe_customer_id`:
  sends a storage event (gauge = `tenant_quotas.storage_used_bytes`) and an egress
  event (sum of `bandwidth_usage_daily.egress_bytes` for `date`). Records each in
  `metered_usage_reports`.
- `StartMeteredReporting(ctx)` — hourly goroutine; runs `ReportDaily` for the
  **previous** UTC day at hour 0 (and once on startup as catch-up), checks spending
  caps hourly. Nil-safe on stripe/db.
- `SetEmailSender(email.Sender)` — optional; wires the spending-cap alert email.
- `AccruedCents(tier, storageBytes, egressBytes)` / `MeteredRatePerTB(tier)` —
  exported pricing helpers (Standard $3.99/TB, Performance $6.00/TB; egress $0).
  Used by the dashboard billing handler for the "≈ $X.XX this month" estimate.

**Idempotency / no double-billing**: `metered_usage_reports` has
`UNIQUE(tenant_id, meter, period_date)`. `reportMeter` skips the Stripe call if a
row already exists, and the Stripe meter event `identifier`
(`{tenant}-{meter}-{date}`) is a second dedup layer, so a crash between send and
record cannot double-count. A failed send writes **no** row → the next run retries.

**Stripe SDK note**: stripe-go **v75 has no Billing Meter Events API**, so events are
POSTed raw to the stable `/v1/billing/meter_events` REST endpoint
(`httpMeterSender`, auth via the SDK-global API key). The `meterSender` interface
lets tests substitute a fake.

**Spending caps**: `tenant_quotas.spending_cap_cents` (0 = none). `checkSpendingCaps`
alerts at 80%/95% of cap, each threshold once per month (guarded by synthetic
`alert:80`/`alert:95` rows in `metered_usage_reports` keyed by first-of-month).
Each alert records a `billing.spending_cap_alert` event (inserted directly — billing
cannot import `api.emitEvent` without a cycle) and emails the tenant.

**Operator setup (config, not code)**: in the Stripe dashboard create two Billing
Meters — storage with **"last value"** aggregation (it's a gauge), egress with
**"sum"** (daily counter) — attach them to the Standard/Performance metered prices,
and set `STRIPE_METER_STORAGE` / `STRIPE_METER_EGRESS` to their event-name strings.
Without both envs the reporter stays dormant (no-op).

## Testing

- Unit tests: `go test ./internal/billing/... -short` (no Stripe key needed)
- `metered_test.go` uses a `fakeMeterSender` + sqlmock (no real Stripe/DB).
- Integration tests: set `STRIPE_TEST_KEY` env var + `stripe listen --forward-to localhost:8000/webhook/stripe`
