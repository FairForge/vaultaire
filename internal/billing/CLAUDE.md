# internal/billing

Stripe billing integration for stored.ge subscriptions, payments, and invoices.

> **WP-PAY (design of record `docs/PAYMENTS.md`): this package is becoming the Stripe provider behind a provider-neutral entitlement record. WP-PAY-1 (2026-10-20, migration 097) is in: the webhook dual-writes `internal/payments` — see "The entitlement record" below. `HouseOrder` / `Period` are aliases of `payments.Order` / `payments.Period`. **WP-PAY-2 (Stage 1a Prompt 19): `*StripeService` implements `payments.Provider`** — see "The Stripe provider" below; the house pricing (`QuoteHouse`, `Quote`, `ValidateHouseOrder`, `Line`) lives in `internal/payments/pricing.go` and is aliased here.**

## The Stripe provider (WP-PAY-2, `provider.go`)

`*StripeService` is the `payments.Provider` named `stripe` (`docs/PAYMENTS.md` § 2.2), registered in `payments.Registry` by `api/server.go` — the dashboard and the deletion runner reach Stripe only through the registry (the dashboard imports no package that reaches `stripe-go`). `Renewal` = auto. `Ready` = `STRIPE_SECRET_KEY` set ∧ `HouseCheckoutReady` (the six prices verified). `StartCheckout` = `GetCustomerID`, else **`createCustomer` — the ONLY creation site** (unexported; idempotency key `customer-<tenant>`; a failed record of the id is an error so the next checkout cannot create a second customer) — then `HasLiveSubscription` (→ `payments.ErrLiveSubscription`, R10-05) then `CreateHouseCheckout(…, purpose, …)` (the ledger's purpose in the session and subscription metadata). `Resize` = `UpdateHouseSubscription` on the current subscription (`currentSubscription`: the entitlement's `provider_ref`, else `tenants.stripe_subscription_id`), `Applied` = the house it now bills. `StopRenewal`/`ResumeRenewal` = `subscription.Update` with `cancel_at_period_end` true/false (`renewalParams`; the `customer.subscription.updated` that follows writes the entitlement's renewal). `Cancel` = `CancelSubscription` when there is a subscription (nil otherwise). `ManageURL` = `CreateBillingPortalSession`. `Webhook` = the `WebhookHandler` (`SetWebhook`). **`NewUnconfiguredStripe(logger)`** is the provider without a key: never Ready, every call `payments.ErrProviderUnconfigured`. No customer at registration any more (43.2). Tests `provider_test.go` (a fake Stripe REST API behind stripe-go's backend: the lazy customer created once across two checkouts and recorded, a live subscription refused; the renewal params; the unconfigured provider).

## The entitlement record (WP-PAY-1)

The webhook writes the house's floors AND the entitlement row in one transaction (`payments.Grant` / `payments.Revoke`, a second pooled connection beside the held claim — the claim's per-customer lock serialises the customer, `payments.Lock` the tenant), and the `tenants` columns (`stripe_subscription_id`, `subscription_status`, `plan`, `house_period`) in the same transaction, as before (readers move off them in WP-PAY-7):

| Event (fetched status) | Entitlement |
|---|---|
| `checkout.session.completed`, `customer.subscription.{created,updated}` — `active` / `trialing` | `Grant`, `paid_through = current_period_end` (a trial is paid through its end), `renewal = auto` (`prepaid` when `cancel_at_period_end`), `provider_ref` = the subscription |
| … `past_due` | `Grant` with `paid_through = current_period_START`: Stripe moved the period on when it raised the renewal invoice and nothing was paid for it. A grant never shortens a date, so the row keeps the last paid date and the grace runs (the floors stay, as before) |
| … `canceled` / `unpaid` / `incomplete_expired` / `paused`, and `customer.subscription.deleted` | current-subscription guard (`isCurrentSubscription`: the entitlement's `provider_ref` first, `tenants.stripe_subscription_id` for a tenant with no row — RP20-A3). **`entitlement_lapse` ON for the tenant (WP-PAY-2, `SetFlags`):** `payments.StopRenewal` only — `renewal = prepaid`, the house KEPT, `lapsed_at` untouched (the date and the lapse job decide; the job is then the only writer of `lapsed_at`); `tenants.subscription_status` written (a delete also nulls `stripe_subscription_id`; `plan`/`house_period` kept). **OFF (default):** cleared as before — `payments.Revoke` (floors cleared, `lapsed_at` set so the reconcile never grants it back) + `StopRenewal`. Tests `webhook_lapse_flag_test.go` |
| `invoice.payment_succeeded` (`webhook_ledger.go`) | the `payments` row keyed by the invoice id is SETTLED (`AmountPaid`, the lines' period, purpose from `billing_reason`, our receipt number `ST-<year>-<n>`); the subscription is FETCHED and, for an active/trialing house, `Grant` to its `current_period_end` in the same transaction — idempotent with the `updated` event of the same renewal. A redelivery is a replay (`vaultaire_payments_total{outcome="replayed"}`, no second row). Amounts come from the signed payload: a paid invoice is final at Stripe (departure from § 1.4's "fetch the invoice") |
| `invoice.payment_failed` | ledger row `failed` (`AmountDue`); no entitlement change |

**An invalid house is handled, never retried (RP20-4, Prompt 19 Part 0):** a subscription whose items are no valid house (`payments.ErrInvalidOrder` — pin-hot over Standard after an operator deleted the Standard item at Stripe, a negative line, an unknown period; migration 097's CHECK is the same rule) is answered **200** on the first delivery (the claim commits, a redelivery is the duplicate): `houseInvalid` writes one Error line naming tenant, subscription and the order, an audit row `billing.house_invalid` {tenant, subscription, std, vault, pin_hot, period}, and `vaultaire_stripe_house_invalid_total` (unlabelled, 0 from boot; `metrics.go`, registered by `internal/api/prom_metrics.go`). The house and the entitlement are left exactly as they were; a paid invoice of such a subscription still SETTLES its ledger row (the grant is refused before any statement, so the settle transaction stays intact). Any other grant error stays a 500. Before: three deliveries answered `[500 500 500]` with no claim row — a three-day retry storm until Stripe disabled the endpoint (`webhook_invalid_house_test.go`).

`SubscriptionPeriodEnd(ctx, id)` → `(end, cancelAtPeriodEnd, err)` is the read the lapse job's backfill uses: a subscription whose renewal was stopped is backfilled `renewal = prepaid` (RP20-5). Tests: `webhook_entitlement_test.go` (signed fixtures + the fetched subscription with its period).

## Key Types

- **StripeService** — manages Stripe customers, checkout sessions, subscriptions, invoices, and billing portal. Holds `*sql.DB` for persisting Stripe IDs to the `tenants` table.
- **WebhookHandler** — `http.Handler` for `POST /webhook/stripe`. Verifies signatures, processes events, updates DB.
- **InvoiceRow** — formatted invoice data for dashboard templates.
- **OverageService** — tracks grace periods for tenants exceeding quotas.

## StripeService Methods

| Method | Purpose |
|--------|---------|
| `GetCustomerID(ctx, tenantID)` | Look up Stripe customer ID from DB |
| `GetSubscription(ctx, tenantID)` | Fetch subscription from Stripe |
| `CancelSubscription(ctx, tenantID)` | Cancels **immediately** (`subscription.Cancel`). Called by the account-deletion runner (WP-R10-3, stage a); idempotent — the subscription is fetched first and an already-`canceled` one is success, so a crash between Stripe's answer and the runner's stamp cannot defer the tenant forever (`TestCancelSubscription_AlreadyCancelledIsSuccess`) |
| `GetInvoices(ctx, tenantID, limit)` | List recent invoices from Stripe |
| `CreateBillingPortalSession(ctx, tenantID, returnURL)` | Self-service billing portal |
| `SaveSubscription(ctx, tenantID, subID, status, plan)` | Persist subscription state (called by webhook) |
| `LookupTenantByCustomer(ctx, customerID)` | Reverse lookup tenant from Stripe customer ID |

## Webhook Events Handled

| Event | Action |
|-------|--------|
| `checkout.session.completed` | Activate subscription, save to DB |
| `invoice.payment_succeeded` | Mark subscription active; ledger row settled + house granted to the period end (WP-PAY-1) |
| `invoice.payment_failed` | Mark subscription past_due; ledger row failed (WP-PAY-1) |
| `customer.subscription.created` / `.updated` | Sync status and plan; house subscriptions → floor quotas (`applyHouse`) |
| `customer.subscription.deleted` | Downgrade to starter, clear subscription (and the house's floor quotas); ignored for a stale subscription id |

## Environment Variables (wired in server.go)

- `STRIPE_SECRET_KEY` — Stripe API key
- `STRIPE_WEBHOOK_SECRET` — webhook endpoint secret. **Required for the route
  to exist**: `server.go` mounts `/webhook/stripe` only when both are set, and
  `NewWebhookHandler("")` answers 503 to everything (Review R10-01 — an empty
  secret used to mean "skip verification").
- `STRIPE_PRICE_{STANDARD,VAULT,PINHOT}_{ANNUAL,MONTHLY}` — the six house
  prices, read by `HousePriceIDsFromEnv` (`house.go`); all six or the house
  checkout stays closed.

**One product (WP-PAY-0 = WP-R10-4, decision F-29, Stage 1a Prompt 19):** the
house is the only thing sold. Deleted: the legacy pack plans
(`STRIPE_PRICE_VAULT3/9/18/36`, `STRIPE_PRICE_STANDARD`, `registerStripePlans`,
`Plan`, `RegisterPlan`/`GetPlan`/`Plans`, the pack `CreateCheckoutSession`, the
dashboard's `HandleUpgrade` + `/dashboard/billing/upgrade`) and the metered
reporter (`metered.go`: `MeteredReporter`, `STRIPE_METER_*`, `AccruedCents`,
`MeteredRatePerTB`, the spending-cap alert). Old variables left in an `.env`
are ignored. `tenants.plan` / `tenant_quotas.tier` values such as `vault100`
stay valid **quota labels** (the admin tier picker); a tenant carrying one is
told on the billing page to ask support (`legacyPlan`, dashboard handlers).
`tenant_quotas.spending_cap_cents` and `metered_usage_reports` stay in the
schema, unread, until WP-PAY-7 (D-12).

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
  the customer id is healed onto the row — the pre-WP-PAY-2 `CreateCustomer` logged-and-continued
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
- Tests: `house_test.go` — quote maths vs the price file, verification
  mismatches, checkout/update params, and DB-backed webhook → floor quotas.

## Testing

- Unit tests: `go test ./internal/billing/... -short` (no Stripe key needed)
- Integration tests: set `STRIPE_TEST_KEY` env var + `stripe listen --forward-to localhost:8000/webhook/stripe`
