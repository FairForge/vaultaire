# internal/billing

Stripe billing integration for stored.ge subscriptions, payments, and invoices.

> **Planned (2026-10-18, WP-PAY): this package becomes the Stripe provider behind a provider-neutral entitlement record (`paid_through` in our DB, a `payments` ledger, BTCPay as a second provider). Design of record: `docs/PAYMENTS.md`; plan § WP-PAY; prompts `docs/reviews/WP-PAY-PROMPTS.md`.**

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
| `CancelSubscription(ctx, tenantID)` | Cancels **immediately** (`subscription.Cancel`). Called by the account-deletion runner (WP-R10-3, stage a); idempotent — the subscription is fetched first and an already-`canceled` one is success, so a crash between Stripe's answer and the runner's stamp cannot defer the tenant forever (`TestCancelSubscription_AlreadyCancelledIsSuccess`) |
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

- `STRIPE_SECRET_KEY` — Stripe API key
- `STRIPE_WEBHOOK_SECRET` — webhook endpoint secret. **Required for the route
  to exist**: `server.go` mounts `/webhook/stripe` only when both are set, and
  `NewWebhookHandler("")` answers 503 to everything (Review R10-01 — an empty
  secret used to mean "skip verification").
- `STRIPE_PRICE_VAULT3`, `STRIPE_PRICE_VAULT9`, `STRIPE_PRICE_VAULT18`,
  `STRIPE_PRICE_VAULT36`, `STRIPE_PRICE_STANDARD` — the five legacy pack
  plans (`registerStripePlans` → `RegisterPlan`).
- `STRIPE_PRICE_{STANDARD,VAULT,PINHOT}_{ANNUAL,MONTHLY}` — the six house
  prices, read by `HousePriceIDsFromEnv` (`house.go`); all six or the house
  checkout stays closed.
- `STRIPE_METER_STORAGE`, `STRIPE_METER_EGRESS` — Billing Meter event names;
  both or the metered reporter stays dormant.

## Webhook delivery contract (Review R10)

- **Signature**: `webhook.ConstructEvent` — scheme v1 (HMAC-SHA256 over
  `<t>.<body>`), 300 s tolerance, **and the event's `api_version` must equal
  `stripe.APIVersion` = `2023-08-16`** (stripe-go v75.11.0 refuses any other).
  The endpoint in the Stripe dashboard must therefore be created **pinned to
  2023-08-16**; left on the account default every delivery is a 400, Stripe
  retries for three days and then disables the endpoint. Upgrading the SDK
  moves the pin (WP-R10-6).
- **Body cap**: 10 MB (`http.MaxBytesReader`, 413 past it; the general
  request-limits middleware gives `/webhook/` the same 10 MB). Never a
  truncating `LimitReader`.
- **Dedup — claim first (WP-R10-2, R10-16)**: the event id is claimed BEFORE
  its handler runs: `INSERT INTO stripe_events … ON CONFLICT (event_id) DO
  NOTHING RETURNING event_id` inside a transaction (`claimEvent`) that stays
  open while the handler runs and is **committed only after it succeeded**.
  No row back = the id is already committed → a duplicate, 200 at once. A
  concurrent delivery of the same id waits on the uncommitted row (Postgres's
  speculative insert) — then it is a duplicate (the first committed) or takes
  the event over (the first rolled back); the wait is bounded by `SET LOCAL
  lock_timeout = '5s'`, past it a 500 Stripe retries. A handler failure, a
  failed commit, a client that hung up (the request context cancels the
  transaction) or a process that died (its connection goes, Postgres rolls the
  claim back) leaves **no row**: the retry is handled — never a lost event. No
  migration: `processed_at` keeps its default and means "the claim was
  committed". Before: check-then-act (`SELECT EXISTS` … handle … `INSERT`),
  two concurrent deliveries were both handled (two Stripe fetches, two
  applications). Cost: a webhook holds two pooled connections while it runs
  (the claim + the handler's queries; the pool is 50).
- **Acknowledgement**: 200 only after the handler succeeded and the claim was
  committed. A Stripe fetch error or a DB error answers **500 and records
  nothing**, so Stripe retries (up to three days) and the retry is handled as
  a fresh event. Handlers are idempotent. A payload the SDK cannot deserialise
  is logged and dropped (200) — retrying it would only get the endpoint
  disabled. A subscription Stripe answers **404** for (`resource_missing` —
  `stripeMissing`) is nothing to apply: 200 + a Warn, never three days of 500s.
- **Tenant resolution** (`resolveTenant`, post-merge R10-45): the customer id
  first; when `tenants` does not know it, the event's own `client_reference_id`
  / `metadata.tenant_id` (stamped on every session and subscription we create;
  invoices carry it as `subscription_details.metadata`) names the tenant and
  the customer id is healed onto the row — `CreateCustomer` logs-and-continues
  when its persist fails, so a checkout can complete under a customer id the
  row never received. A customer that resolves nowhere is **not ours** (another
  product on the account, a dashboard-made customer, an erased tenant): 200 +
  recorded, never a three-day 500 retry storm.
- **Ordering (WP-R10-2, R10-15)**: Stripe does not guarantee event order, so
  no handler applies a payload's state. `checkout.session.completed` and
  `customer.subscription.{created,updated}` **fetch the subscription**
  (`fetchSubscription`) and apply the FETCHED status, items and metadata; the
  payload names only the subscription, its customer and the tenant hint
  (`resolveTenant` runs first, so a subscription that is not ours is a 200
  with no fetch). A fetch failure is a 500 (Stripe retries). And **one
  customer's events are applied one at a time** — the claim takes
  `pg_advisory_xact_lock(hashtextextended('stripe-customer:'||<customer>, 0))`
  (`eventCustomer`: `data.object.customer`) — so two events in flight cannot
  fetch, then apply, in the wrong order: the last applied fetched last.
  `customer.subscription.deleted` applies its payload: `canceled` is terminal
  at Stripe, a fetch can say nothing newer, and a stale `updated` delivered
  after it fetches `canceled` and clears again. Before: `updated(active)`
  delivered after a later `updated(canceled)` re-granted a cancelled house; a
  stale `updated(unpaid)` took a paid house away until the next event.
  **Stripe calls of a normal house purchase**: 2 subscription GETs from the
  webhook (checkout.session.completed + customer.subscription.created; was 1),
  +1 per later `customer.subscription.updated` (a resize, a dunning step);
  the dashboard's own calls (live-subscription list, checkout session) are
  unchanged.
- **The status belongs to the current subscription (RP11-1 / RP11-1b,
  Prompt 12)**: `tenants.subscription_status` (and `plan`) are written only
  for the tenant's CURRENT subscription (`stripe_subscription_id`), each in
  one guarded `UPDATE … WHERE stripe_subscription_id = <the event's>`.
  `invoice.payment_succeeded` / `payment_failed` (`writeInvoiceStatus`) take
  the id from `invoice.subscription` and never adopt one (a one-off invoice
  writes nothing); `customer.subscription.{created,updated}` write the
  fetched status, and a tenant with NONE recorded adopts the subscription
  (id set with the status — a `created` delivered before
  `checkout.session.completed` lands) unless it is `canceled` /
  `incomplete_expired`. A stale event writes neither status nor plan (an Info
  line). Before: a late `payment_succeeded` after `subscription.deleted`
  marked a starter tenant `active`; a stale `updated` of an old cancelled
  subscription wrote `canceled` while the current house stayed.
  `applyHouse` is unchanged (it grants without the guard, clears with it).
- **`stripe_events.tenant_id` (RP11-2)**: every handler returns the tenant it
  resolved and `ServeHTTP` writes it onto the claim row inside the claim
  transaction, before the commit (NULL = not ours) — the admin support page
  lists a tenant's events by it (`idx_stripe_events_tenant`, 019); it was
  never written before.
- **Dunning**: `invoice.payment_failed` → `past_due`, house **kept** (grace =
  Stripe Smart Retries); the dashboard's "after retries" setting delivers
  `canceled`/`unpaid`, which clears the house. Set it to **cancel**.
- **`paused` (WP-R10-2 decision)**: Stripe's `paused` status is a trial that
  ended without a payment method — no invoice is created while it lasts, so
  nothing is paid. It is treated exactly as `unpaid`: `applyHouse` **clears**
  the house (current-subscription guard, data untouched), and
  `HasLiveSubscription` **counts** it (resumable: a second Checkout beside it
  would be a second subscription), so the dashboard sends the customer to
  support as for `unpaid`. Our Checkout creates no trial, so `paused` arises
  only from an operator adding one by hand. Distinct from **"pause payment
  collection"** in the Stripe dashboard (`pause_collection`): the status stays
  `active` and the house is kept — an operator's deliberate choice.
- **Customer Portal** (dashboard config): keep quantity updates **off** — the
  dashboard's resize path refuses a floor below its usage, the portal would not.
- **Tests**: every webhook test signs its fixture with
  `webhook.ComputeSignature` (`webhook_signed_test.go`); unsigned, wrong-secret,
  stale, foreign-API-version, oversize and handler-failure cases are covered.

## Wiring

- Webhook route: `POST /webhook/stripe` registered in `server.go` (only when both Stripe envs are set)
- Checkout + billing portal: wired in dashboard billing handler
- Stripe customers are created from three places: `/auth/register` (`server.go`), the dashboard register form (`dashboard/router.go`), and lazily at house checkout when the tenant has none yet (`dashboard/handlers/billing_house.go`) — each logs-and-continues on failure, which is why the webhook heals the customer id onto the row
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
  `house=1`, `tenant_id`, `ClientReferenceID`, Stripe idempotency key over
  (tenant, order, 5-min window) so a double submit is one session);
  `HasLiveSubscription` lists the customer's subscriptions and the dashboard
  refuses a new checkout while any is active/trialing/past_due/incomplete/
  unpaid/paused (R10-05 — a second Checkout is a second subscription);
  `UpdateHouseSubscription` resizes
  in place with `create_prorations` (quantity changes; a line at 0 is deleted;
  the **period is fixed** for the life of a subscription — switching = support).
  `HouseFromSubscription` maps items back to an order (false for legacy packs).
- **Webhook → quotas:** `SetHouseQuotas(usage.QuotaManager)` wires `applyHouse`,
  called from `checkout.session.completed` (fetches the subscription — the
  session carries no items) and `customer.subscription.{created,updated}`:
  active/trialing/past_due → `SetHouse` (floor rows, total = sum, tier
  standard|vault, `tenants.plan='house'`, `house_period`); canceled/unpaid/
  incomplete_expired/**paused** and `customer.subscription.deleted` → `ClearHouse` (free
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
  **Stale vs prices.json (4.99/4.49, 6.99) and dormant at launch** — the
  2026-09-21 decision sells quota, not meters; no launch tenant is metered
  (house tenants have floor rows and are excluded). Deletion = WP-R10-4.

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
