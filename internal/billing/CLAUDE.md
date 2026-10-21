# internal/billing

Stripe billing integration for stored.ge subscriptions, payments, and invoices.

> **WP-PAY (design of record `docs/PAYMENTS.md`): this package is becoming the Stripe provider behind a provider-neutral entitlement record. WP-PAY-1 (2026-10-20, migration 097) is in: the webhook dual-writes `internal/payments` — see "The entitlement record" below. `HouseOrder` / `Period` are aliases of `payments.Order` / `payments.Period`. **WP-PAY-2 (Stage 1a Prompt 19): `*StripeService` implements `payments.Provider`** — see "The Stripe provider" below; the house pricing (`QuoteHouse`, `Quote`, `ValidateHouseOrder`, `Line`) lives in `internal/payments/pricing.go` and is aliased here.**

## The Stripe provider (WP-PAY-2, `provider.go`)

`*StripeService` is the `payments.Provider` named `stripe` (`docs/PAYMENTS.md` § 2.2), registered in `payments.Registry` by `api/server.go` — the dashboard and the deletion runner reach Stripe only through the registry (the dashboard imports no package that reaches `stripe-go`). `Renewal` = auto. `Ready` (a `payments.Readiness`, Prompt 20 Part 0 / D-P20-1) = `Unready` without a key or price ids, after a mismatch, a missing price or a Stripe 4xx; **`Pending`** while a key is set and no verification has completed; `Ready` once the six prices matched (`HouseReadiness`; `HouseCheckoutReady` is the bool view). `VerifyHousePrices` keeps the previous verdict when Stripe could not be REACHED (`ErrPricesUnreachable`: a dial error, a timeout, a 5xx, a 429 — logged at Warn, retried in 5 min; RP22-5) — only Stripe's answer is a verdict. `StartCheckout` = `GetCustomerID`, else **`createCustomer` — the ONLY creation site** (unexported; idempotency key `customer-<tenant>-<8 hex of sha256(e-mail)>` so a changed e-mail within Stripe's 24 h is a new key, RP22-7; a failed record of the id is an error so the next checkout cannot create a second customer) — then `HasLiveSubscription` (→ `payments.ErrLiveSubscription`, R10-05) then `CreateHouseCheckout(…, purpose, …)` (the ledger's purpose in the session and subscription metadata). `Resize` = `UpdateHouseSubscription` on the current subscription (`currentSubscription`: the entitlement's `provider_ref`, nothing else — WP-PAY-7), `Applied` = the house it now bills. `StopRenewal`/`ResumeRenewal` = `subscription.Update` with `cancel_at_period_end` true/false (`renewalParams`; the `customer.subscription.updated` that follows writes the entitlement's renewal). `Cancel` = `cancelRef` on the current subscription when there is one (nil otherwise); **`CancelRef(tenant, ref)`** (`payments.RefCanceller`) cancels a named one — the deletion runner names every reference the record knows. `ManageURL` = `CreateBillingPortalSession`. `Webhook` = the `WebhookHandler` (`SetWebhook`). **`NewUnconfiguredStripe(logger)`** is the provider without a key: never Ready, every call `payments.ErrProviderUnconfigured`. No customer at registration any more (43.2). Tests `provider_test.go` (a fake Stripe REST API behind stripe-go's backend: the lazy customer created once across two checkouts and recorded, a live subscription refused; the renewal params; the unconfigured provider).

## The entitlement record (WP-PAY-1)

The webhook writes the house's floors AND the entitlement row in one transaction (`payments.Grant` / `payments.Revoke`, a second pooled connection beside the held claim — the claim's per-customer lock serialises the customer, `payments.Lock` the tenant), and **nothing of `tenants` but `stripe_customer_id`** (WP-PAY-7, Stage 1a Prompt 23: the record is the only billing state read or written; `stripe_subscription_id` / `subscription_status` / `house_period` / `deletion_stripe_cancelled_at` are dropped by migration 100, `plan` is the operator's label and no longer written by the webhook — D-P23-3; source guard `TestWebhook_WritesNoTenantsBillingColumns`):

| Event (fetched status) | Entitlement |
|---|---|
| `checkout.session.completed`, `customer.subscription.{created,updated}` — `active` / `trialing` | `Grant`, `paid_through = current_period_end` (a trial is paid through its end), `renewal = auto` (`prepaid` when `cancel_at_period_end`), `provider_ref` = the subscription |
| … `past_due` | `Grant` with `paid_through = current_period_START`: Stripe moved the period on when it raised the renewal invoice and nothing was paid for it. A grant never shortens a date, so the row keeps the last paid date and the grace runs (the floors stay, as before) |
| … `canceled` / `unpaid` / `incomplete_expired` / `paused`, and `customer.subscription.deleted` | current-subscription guard (`isCurrentSubscription`: the entitlement's `provider_ref` and nothing else — a tenant with no Stripe row fails it CLOSED, the event ignored with an Info line; `TestIsCurrentSubscription_NoRow_IgnoresClearingEvent`). **`entitlement_lapse` ON for the tenant (WP-PAY-2, `SetFlags`):** `payments.StopRenewal` only — `renewal = prepaid`, the house KEPT, `lapsed_at` untouched (the date and the lapse job decide; the job is then the only writer of `lapsed_at`); **OFF (default):** `payments.EndByStatus` — a GRANTED row whose `paid_through` is after now is kept exactly as ON (renewal stopped, one Warn naming the kept date, `billing.renewal_stopped`): paid time is never taken by a status (RP26-1, D-P23-1 — a $0 trialing Renew of a manually-paid house, cancelled in the portal, reaches here); a date already passed, or no row, is cleared as before — `payments.Revoke` (floors cleared, `lapsed_at` set so the reconcile never grants it back) + `StopRenewal`. Tests `webhook_lapse_flag_test.go`, `wp_p23_part0_test.go` |
| `invoice.payment_succeeded` (`webhook_ledger.go`) | the `payments` row keyed by the invoice id is SETTLED (`AmountPaid`, the lines' period, purpose from `billing_reason`, our receipt number `ST-<year>-<n>`); the subscription is FETCHED and, for an active/trialing house, `Grant` to its `current_period_end` in the same transaction — idempotent with the `updated` event of the same renewal. A redelivery is a replay (`vaultaire_payments_total{outcome="replayed"}`, no second row). Amounts come from the signed payload: a paid invoice is final at Stripe (departure from § 1.4's "fetch the invoice") |
| `invoice.payment_failed` | ledger row `failed` (`AmountDue`); no entitlement change |
| `charge.refunded` (`webhook_refund.go`, RP23-4) | the tenant from the customer; the charge row from the charge's invoice (`payments.ChargeOf`: the ledger keys a Stripe charge by its invoice id); one `payments.RecordRefund` (`FromProvider: true`) per **succeeded** refund keyed by the refund id (`re_…`) — the payload's `refunds`, else listed from Stripe (`refundLister`; since API 2022-11-15 a charge carries none unless expanded). A redelivery / a refund already recorded (`ErrRefundRecorded`), an amount past the charge (`ErrRefundExceeds`) or a refund id the ledger holds for another charge (`ErrRefundMismatch`, both an Error line) are **200** with a log line, never a 500 storm. **A charge the ledger does not know yet** (Stripe does not order deliveries) is a 200 + Warn — its refunds are recorded when its invoice's payment lands (RP28-2, below). `paid_through` is NOT shortened here. Counted under the charge's provider (RP24-3), audited `billing.refund_recorded` actor `stripe-webhook`. Tests `TestChargeRefunded_RecordsOnce`, `_UnknownCharge200NoApply`, `_CountedByChargesProvider` |
| `charge.refund.updated` (RP28-2, Stage 1a Prompt 24) | the refund's status **succeeded** → the charge FETCHED from Stripe (`chargeFetcher`; 404 = 200 + Warn, any other error 500) and its refunds LISTED now, recorded through the same path as `charge.refunded` (idempotent on the `re_…` id); any other status (`pending`, `failed`, `canceled`, `requires_action`) = an Info line, nothing recorded. A refund pending when `charge.refunded` was delivered is recorded here when it succeeds. Test `TestChargeRefundUpdated_PendingThenSucceeded_RecordedOnce` |
| (RP28-2) a refund delivered before its charge | `recordInvoicePaid`: while the invoice's ledger row is not settled yet and the invoice names a charge, the charge's refunds are listed (one Stripe call — a listing that fails answers 500 before anything is written); when the delivery NEWLY settles the row, each succeeded refund is recorded through `recordRefunds` (a failure after the commit is an Error line naming the refunds: the redelivery is a replay, so the operator records them with the admin form under their `re_…` ids). Test `TestChargeRefunded_BeforeInvoicePaid_RecordedWhenChargeLands` |

**One refund, one row (RP28-1, Stage 1a Prompt 24):** a refund of a Stripe charge is Stripe's to record — the webhook above records it under its `re_…` id. The admin refund form and `POST /api/v1/admin/tenants/{id}/refunds` refuse to CREATE a refund of a Stripe charge unless `provider_ref` is a `re_…` id (`payments.ErrRefundProviderRef`, 400 — no row, no credit-note number spent); with a `re_…` id already in the ledger (or the refund row's own id as `payment_id`) the call is an idempotent hit (200, `existing`), whichever side came first. **To shorten `paid_through` for a Stripe refund** the operator names that refund (its `re_…` id, or its row) with *shorten* ticked and the amount blank: the date moves back by the refund's share of the charge's period **once** (the row is stamped `metadata.shortened` {seconds, at, actor}; a second shorten is `ErrRefundShortened`, 409), audited `billing.paid_through_adjusted`. If the operator records the refund (with its `re_…` id) before the delivery, the delivery is `ErrRefundRecorded` → 200. Manual and BTCPay charges keep the old form (a blank reference = a generated `refund:<uuid>`). Tests `wp_p24_part0_test.go` (`TestRefund_StripeWebhookThenAdminForm_OneRow`, `_AdminFormThenWebhook_OneRow`, `_AdminFormBlankRefOnStripeCharge_Refused`, `_ShortenAfterWebhookRefund_Once`, `_ManualChargeFormUnchanged` — they drive the dashboard's `HandleAdminRefund` beside the webhook).

**The Stripe endpoint's event list (owner click-list item 5):** `checkout.session.completed`, `invoice.payment_succeeded`, `invoice.payment_failed`, `customer.subscription.created`, `customer.subscription.updated`, `customer.subscription.deleted`, `charge.refunded`, `charge.refund.updated` — eight. An event not on the list is never delivered; one delivered and not handled is a Debug line + 200.
| a subscription that is **no house** (a legacy pack) | nothing written — not adopted, no status, no plan (WP-PAY-7; the pack checkout is gone since WP-PAY-0): a checkout of one is a Warn, an update an Info line |

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
| `CancelRef(ctx, tenantID, ref)` | Cancels subscription `ref` **immediately** (`subscription.Cancel`). Called by the account-deletion runner (WP-R10-3, stage a); idempotent — the subscription is fetched first and an already-`canceled` one is success, so a crash between Stripe's answer and the runner's stamp cannot defer the tenant forever (`TestCancelSubscription_AlreadyCancelledIsSuccess`) |
| `GetInvoices(ctx, tenantID, limit)` | List recent invoices from Stripe |
| `CreateBillingPortalSession(ctx, tenantID, returnURL)` | Self-service billing portal |
| `LookupTenantByCustomer(ctx, customerID)` | Reverse lookup tenant from Stripe customer ID |

## Webhook Events Handled

| Event | Action |
|-------|--------|
| `checkout.session.completed` | Activate subscription, save to DB |
| `invoice.payment_succeeded` | Mark subscription active; ledger row settled + house granted to the period end (WP-PAY-1) |
| `invoice.payment_failed` | Mark subscription past_due; ledger row failed (WP-PAY-1) |
| `customer.subscription.created` / `.updated` | Sync status and plan; house subscriptions → floor quotas (`applyHouse`) |
| `customer.subscription.deleted` | Downgrade to starter, clear subscription (and the house's floor quotas); ignored for a stale subscription id |
| `charge.refunded` / `charge.refund.updated` | Refund rows in the ledger, one per `re_…` id (RP23-4, RP28-1/2 — the table above) |

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
- **The record belongs to the current subscription (RP11-1 / RP11-1b,
  Prompt 12; restated by WP-PAY-7)**: the tenants status column these guards
  protected is dropped; what remains is that a clearing event touches the
  record only for the entitlement's `provider_ref`, and an invoice of a
  deleted or replaced subscription never grants (the fetched subscription
  decides). `applyHouse` grants without the guard, clears with it.
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
  (tenant, order, 5-min window) so a double submit is one session). **A renew
  of a GRANTED prepaid row defers the first charge (RP24-1, § 4.2):**
  `payments.Checkout.StartAt` = the row's `paid_through` →
  `SubscriptionData.TrialEnd` when ≥ 48 h ahead (`payments.MinDeferredStart`;
  Stripe refuses less) — the subscription is `trialing` until the paid date,
  the webhook's grant keeps the date (`current_period_end` = the trial end)
  and flips the row to `provider='stripe', renewal='auto'`; inside 48 h the
  checkout starts now and the billing page's flash says the remaining days
  are forfeited. The trial's **$0 `subscription_create` invoice** settles
  with `Payment.Unnumbered`: no gapless receipt number, no receipt mail
  (`recordInvoicePaid` gates on `settled.InvoiceNumber != ""`);
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
  active/trialing/past_due → `payments.Grant` (floor rows, total = sum, tier
  standard|vault); canceled/unpaid/incomplete_expired/**paused** and
  `customer.subscription.deleted` → the flag's branch (stop renewal, or
  `EndByStatus`: free-tier limits, **used bytes untouched** — billing never
  deletes). Clearing is honoured only for the entitlement's `provider_ref`,
  so a stale subscription's events cannot take a paid house away. Idempotent; replays
  are no-ops (`stripe_events` dedup on top).
- Tests: `house_test.go` — quote maths vs the price file, verification
  mismatches, checkout/update params, and DB-backed webhook → floor quotas.

## Testing

- Unit tests: `go test ./internal/billing/... -short` (no Stripe key needed)
- Integration tests: set `STRIPE_TEST_KEY` env var + `stripe listen --forward-to localhost:8000/webhook/stripe`


## WP-PAY-3 (Stage 1a Prompt 20) in this package

- **Receipt mail**: `recordInvoicePaid` sends the receipt of a NEWLY settled invoice (`Settled.Newly`; a replay or a duplicate delivery sends nothing) through `ReceiptSender` (`SetReceiptSender`, the server's `payments.Mailer`) after the commit, on a detached 15 s context — a mail that fails is one Warn, never a 500. Test `TestReceiptEmail_OnceOnSettle`.
- **Audit rows**: `applyHouse` and `recordInvoicePaid` grant through `payments.GrantRecorded` and write `billing.house_granted` only when the row changed (`auditGrant`; a replayed event writes none) and `billing.renewal_stopped` / `_resumed` when the grant flipped an existing row's renewal (`cancel_at_period_end`); the two flag-ON `StopRenewal` branches (a terminal status, `customer.subscription.deleted`) use `StopRenewalChanged` and audit `renewal_stopped` once. Actor `stripe-webhook` in `metadata.actor` (`performed_by` is a users.id). Test `TestWebhook_GrantAndRenewalAudits`.
- **`vaultaire_payment_webhook_total{provider="stripe",outcome}`**: every delivery counted once at the end of `ServeHTTP` — `ok`, `duplicate`, `rejected` (a bad signature, a body over the cap), `error` (500: left for Stripe to retry). The series `PaymentWebhookFailing` reads.
- **Part 0 (RP22-2/3)**, as amended by WP-PAY-7: a legacy pack's deletion has no entitlement row and is ignored (no downgrade write — the label is the operator's); `houseInvalid` records the subscription in `payment_events` (`payments.RecordInvalidHouse`, `invalid_house:<sub>`, kept by retention until the erasure anonymises it) outside the rolled-back grant, so the deletion runner cancels it (`payments.InvalidHouseRefs` → `CancelRef`).
