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
| `customer.subscription.updated` | Sync status and plan |
| `customer.subscription.deleted` | Downgrade to starter, clear subscription |

## Environment Variables (wired in server.go)

- `STRIPE_SECRET_KEY` — Stripe API key (server.go:169)
- `STRIPE_WEBHOOK_SECRET` — webhook endpoint secret (server.go:171)

## Wiring

- Webhook route: `POST /webhook/stripe` registered in server.go:394
- Checkout + billing portal: wired in dashboard billing handler
- Registration → auto-creates Stripe customer (server.go:511-514)
- Stripe event idempotency via `stripe_events` table (migration 019)

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
