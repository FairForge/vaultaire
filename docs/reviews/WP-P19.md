# Stage 1a — Prompt 19: Part 0 (WP-P18 review residuals), WP-PAY-0 (one product) and WP-PAY-2 (payments.Provider)

Date: 2026-10-20. Prod at the start: `d825f39` (slot 8001). Branches `stage1a-p19-part0`, `stage1a-p19-pay0`, `stage1a-p19-pay2` (stacked), worktrees `../vaultaire-p19{,a,b}`; private test DB `vaultaire_p19`.
Prompt: Part 0 = RP20-4, RP20-5, RP20-A6, RP20-3 + the prod read; Part A = WP-PAY-0 (= WP-R10-4, F-29); Part B = WP-PAY-2 as `docs/reviews/WP-PAY-PROMPTS.md` § "Prompt WP-PAY-2" with the WP-P18 facts. Design of record `docs/PAYMENTS.md`; contract `docs/ARCHITECTURE_CONTRACT.md`, lane `docs/surfaces/DEVELOPER.md`.

**PRs:** Part 0 #806 (`02d4699`), Part A #807 (`8aa2c44`), Part B #808 (`374e582`; branch commits `586befd` interface + Stripe adapter + webhook, `80a54f4` dashboard + runner + flags — the queue lands each PR as one commit); this docs PR last. **No migration. Next migration: 098.**

## Decisions

| # | Decision | Why |
|---|---|---|
| D-P19-1 | An invalid house from Stripe (`payments.ErrInvalidOrder`) is **handled**: 200 on the first delivery (the claim commits; a redelivery is the duplicate), one Error line, audit `billing.house_invalid` {tenant, subscription, std, vault, pin_hot, period}, `vaultaire_stripe_house_invalid_total` (unlabelled — 0 from boot). The house and the entitlement stay as they were; a paid invoice's ledger row is still settled. | The items cannot change by retrying; a 500 was three days of retries and a disabled endpoint (the R10-45 class). `Grant` refuses before its lock and its first statement, so the settle transaction of a paid invoice is intact and commits. |
| D-P19-2 | `make lint` = CI's golangci-lint **v2.14.0 through `go run`**, the pin checked against `ci.yml` by `lint-version-check`; the pre-commit hook calls `make lint`. | Never the binary on PATH (Homebrew's 2.4.0 refused the go1.26 module; it is 2.15.0 on this Mac now). First run on a cold build cache **39 s** (module already downloaded), warm **~3 s**. |
| D-P19-3 | The tier labels (`vault3` … `vault100`, `standard`, `bench`) stay valid **quota labels** in `tenant_quotas.tier` and the admin tier picker; only the pack **checkout** goes. A tenant whose `tenants.plan` is a pack label (`legacyPlan`: anything but `''`/`starter`/`free`/`house`) sees "This account is on a legacy plan (…) — email support" and the house checkout refuses it before any Stripe call; a pack subscription Stripe still bills keeps the "would bill you twice" message. | RP20-9: Bench2 (`tenant-f4294518dc0a7a8f`, `vault100`, 1 TiB, 2 MB, no Stripe) is the one prod tenant on a pack label; its quota is unchanged. A checkout beside it would overwrite a quota support set by hand. |
| D-P19-4 | Revenue = **houses only**: `queryHouses` (live house tenants + floors + pin-hot) priced by `QuoteHouse` at `house_period`, used by `/admin/revenue` (MRR, per-floor lines, top houses), `/admin/costs` (revenue side) and the admin overview. The month trend is removed, not ported. | The trend multiplied the `subscriptions` table — which nothing writes (R10-14) — by the pack prices: it always rendered empty. The ledger feeds one in WP-PAY-3. **Departure** from WP-PAY-0's wording ("those pages show houses only") only in that the trend goes too. |
| D-P19-5 | `spending_cap_cents` and `metered_usage_reports` stay in the schema, unread, until WP-PAY-7 (D-12). | No migration in this prompt; the drop is the one irreversible step. |
| D-P19-B1 | `Provider.Resize` returns `payments.ResizeOutcome{Applied *Order; RedirectURL string}`. | The design names the type `Resize`; that is the record's own function (`payments.Resize`). **Departure** in name only. |
| D-P19-B2 | The house **pricing** (`QuoteHouse`, `Quote`, `QuoteLine`, `ValidateHouseOrder`, `Line*`, `Cents`, `UnitCents`) moved to `internal/payments/pricing.go`; `internal/billing` keeps aliases. | `TestDashboardImportsNoStripe` cannot pass while the dashboard imports `internal/billing` for a price: billing imports `stripe-go`. Not in the design's file list; required by its red test. |
| D-P19-B3 | Stripe is **always registered**: the configured service with a key, `billing.NewUnconfiguredStripe` without (never Ready, every call `ErrProviderUnconfigured`). | The prompt's prod proof wants `vaultaire_payment_provider_ready{provider="stripe"} 0`, and the deletion runner must tell "unconfigured" from "not registered" (both defer an `auto` row). |
| D-P19-B4 | `Provider.Cancel` of an unconfigured provider is `ErrProviderUnconfigured`; the runner asks a CONFIGURED provider to cancel even a `prepaid` row (it is idempotent and stops money at once), and defers only `auto` + unconfigured. | § 2.3 "a prepaid row has nothing to cancel" read as "need not", not "must not": with a key, cancelling a `cancel_at_period_end` subscription now costs nothing and closes the window. |
| D-P19-B5 | Provider refusals on the dashboard are **409 plain text** with `X-Error-Code: provider_disabled | other_provider_live`; the other checkout errors stay flashes + 303. | The prompt asks for 409; a form POST shows the body. A styled 409 page is WP-PAY-3's billing-page work. |
| D-P19-B6 | The double-billing guard holds the entitlement lock across the provider's `StartCheckout` (one Stripe round trip). | § 2.5: "the check and the write run under the tenant's advisory lock … a webhook and a checkout cannot pass each other". A webhook of the same tenant waits that long. |
| D-P19-B7 | `HandleManageBilling` takes `payments.Provider` (the router passes the Stripe provider); `Registry.ManageURL` (the row's provider) exists for WP-PAY-3. | As the prompt says; the portal is Stripe's only. |
| D-P19-B8 | The lazy customer is created with idempotency key `customer-<tenant>` and a failed record of its id is an **error** (it was logged and continued). | It is now the only creation site; a lost id would make the next checkout create a second customer. Stripe keeps keys 24 h. |
| D-P19-B9 | With `entitlement_lapse` ON, `customer.subscription.deleted` writes `subscription_status = 'canceled'`, nulls `stripe_subscription_id`, keeps `plan = 'house'` and `house_period` (the house is still in force); the lapse job's `Revoke` leaves the tenants columns alone. | The page reads the entitlement first; `plan` after the lapse is RP21-3. |

## What was built

### Part 0 (#806)

- **RP20-4 (P2)** — `payments.ErrInvalidOrder` + `Order.Validate()` (`internal/payments/entitlement.go`); `applyHouse` (`internal/billing/webhook.go`) and `recordInvoicePaid` (`webhook_ledger.go`) → `houseInvalid` (D-P19-1); `internal/billing/metrics.go` + `billing.Collectors()` registered in `internal/api/prom_metrics.go`.
- **RP20-5 (P3)** — `StripeService.SubscriptionPeriodEnd` → `(end, cancelAtPeriodEnd, err)`; `backfillOne` records `renewal = prepaid` for a stopped subscription.
- **RP20-A6** — `r9Fixture`'s cleanup opens its own handle (the callers' deferred `db.Close` ran before `t.Cleanup`).
- **RP20-3** — D-P19-2.

### Part A — WP-PAY-0 (#807)

Verified at `d825f39`: `registerStripePlans` `internal/api/server.go:1138`; `HandleUpgrade` `internal/dashboard/handlers/billing.go:66` → `CreateCheckoutSession` `internal/billing/stripe.go:~130`, route `internal/dashboard/router.go:324`; `MeteredReporter` `internal/billing/metered.go:77`, wired only with both `STRIPE_METER_*` (`server.go:587-593`), prod has neither; `AccruedCents` callers `billing.go:246`, `admin_revenue.go:281/356/394`, `admin_costs.go:242`; `HouseFromSubscription` `house.go:497` is false for a pack (→ the `liveOther` path, `billing_house.go:~124-160`); `CancelSubscription` `stripe.go:~189` stays. Also found: the admin overview's pack revenue estimate (`admin.go:~66`). Deleted ~1,000 lines: the pack registry and checkout, `HandleUpgrade` + route + grid, `metered.go` and its tests, the meter wiring and log lines, `SetEmailSender` on the reporter, `AccruedCents`/`MeteredRatePerTB` and every caller, `usage.QuotaManager.UpdateQuota` (RP20-8). Docs: root `CLAUDE.md` and `docs/CONFIG.md` env tables, `internal/{billing,email,dashboard,dashboard/handlers}/CLAUDE.md`. No route was in the OpenAPI document.

### Part B — WP-PAY-2 (#808)

Verified after WP-PAY-0 (`8aa2c44`): `HouseBilling` `billing_house.go:32` returned `*stripe.Subscription`; `HandleManageBilling` took `*billing.StripeService`; `Deps.Stripe` wired at `server.go:~896`, read at `router.go:~320` and `:~713`; customer creation at `server.go:~1103`, `router.go:~713` (registration) and `billing_house.go:~290` (lazy); `HasLiveSubscription` `house.go:456`; `cancelSubscription` `deletion_runner.go:~510` deferred on `r.Stripe == nil` + a subscription id; flags `flags_wiring.go:84-91`; `go list -deps ./internal/dashboard/... | grep -c stripe/stripe-go` = **9**.

1. `internal/payments/provider.go` — the interface (§ 2.2), `Registry` (`Register`, `Provider`, `For` → `ErrUnknownProvider` / `ErrProviderDisabled` / `ErrProviderUnavailable`, `Enabled`, `Ready`, `Status`, `StartCheckout`, `Resize`, `ManageURL`), the package `StartCheckout` guard (§ 2.5), `ReadyLog` + `Registry.UnreadyWithin` (the lapse job's `ReadyHistory`), `StartReadiness(60 s)`, `vaultaire_payment_provider_ready{provider}` from `Register`; `pricing.go` (D-P19-B2).
2. `internal/billing/provider.go` — `*StripeService` implements it (D-P19-B3, B8); `currentSubscription` (the row's `provider_ref`, else tenants) used by `Resize`, `StopRenewal`/`ResumeRenewal` (`renewalParams`), `Cancel`, `CancelSubscription` and `isCurrentSubscription` (RP20-A3 closed); the checkout's purpose in its metadata.
3. Registration creates no customer (both paths) — 43.2.
4. Dashboard: `HouseBilling` over `payments`; the page renders from `payments.Status` (the sentence, the ways to pay); `HandleHouseCheckout(svc, db, flags, baseURL, logger)` — `Registry.Resize` writes the record (the `quotas` argument and `Deps.Quotas` are gone); `Deps.Payments`.
5. Provider off: 409 (D-P19-B5); the webhook route depends on the secrets only; the runner per provider (D-P19-B4).
6. `entitlement_lapse` ON: a terminal status and `customer.subscription.deleted` → `StopRenewal`, the house kept (D-P19-B9); OFF: today's clear. With the flag on the lapse job is the only writer of `lapsed_at` (D-P18-A1 closes).
7. Flags `pay_stripe` (default = `STRIPE_SECRET_KEY` set), `pay_btcpay` (off).

## Before → after

| Probe | Before | After |
|---|---|---|
| Three deliveries of `customer.subscription.created` with `vault_m 1 + pin_m 1` | `[500 500 500]`, 0 `stripe_events` rows | `[200 200]` (the second the duplicate), 1 row, no entitlement, floors untouched, counter +1, 1 audit row |
| `invoice.payment_succeeded` of that subscription | 500, nothing settled | 200, the ledger row settled with its number, the entitlement as before |
| Backfill of a pre-097 Stripe house with `cancel_at_period_end` | `renewal = auto` | `renewal = prepaid` |
| `r9Fixture` after its test | 1 head row + tenant left | 0 |
| Pre-commit lint | the PATH binary (Homebrew 2.4.0 refused the module) | v2.14.0 via `go run`, 39 s cold / 3 s warm |
| Boot with `STRIPE_SECRET_KEY` + the old `STRIPE_METER_*` | "metered billing reporter initialized" | no metered line, no reporter |
| `POST /dashboard/billing/upgrade` | Stripe pack checkout | 404 |
| Billing page of a `standard`-tier tenant with 2 TB | "≈ $7.98 Accrued This Month" | no estimate |
| Billing page of a `vault100` tenant (Bench2's shape) | the pack grid / the house checkout | "This account is on a legacy plan (vault100) — email support"; checkout refused before Stripe |
| `/admin/revenue` MRR | packs × VAULT_SERIES prices + metered estimate + houses | houses at `QuoteHouse` only |
| `go list -deps ./internal/dashboard/... \| grep -c stripe/stripe-go` | 9 | **0** |
| Stripe customers created by registering on `/auth/register` + the form | 2 | **0** (created at the first checkout, once) |
| Checkout / resize with `pay_stripe` off | (no flag) | 409 `provider_disabled`, the provider never asked |
| Checkout while another provider renews the house (`auto`) | (no guard) | 409 `other_provider_live` |
| Terminal Stripe status, `entitlement_lapse` ON | floors cleared, `lapsed_at` set | `renewal = prepaid`, house kept, `lapsed_at` NULL |
| Deletion runner, prepaid Stripe row, no key | deferred (any subscription id + no key) | erased; an `auto` row still deferred |
| Lapse job's readiness | `AlwaysReady` | the registry: a provider unready 2 h ago → `skipped_unready` |

## Local live proof

Private DB `vaultaire_p19` (migrated to 097), the Part B binary on `:18019`, no Stripe key, the old `STRIPE_METER_*` / `STRIPE_PRICE_VAULT3` set on purpose:

```
== register (dashboard form) live-p19-…@stored.test → 200
tenant=tenant-519bb86a5bfe657f stripe_customer_id=[]
== billing page (free tier) → Build your house
== seed like Bench2: plan = tier = vault100, 1 TiB → This account is on a legacy plan (vault100)
== POST /dashboard/billing/upgrade (route gone) → 404
== metrics → vaultaire_payment_provider_ready{provider="stripe"} 0 · vaultaire_stripe_house_invalid_total 0
== log: 0 metered lines · 0 "stripe customer" lines · 0 "level":"error"
```

**Stripe test mode:** no owner test keys and no `stripe` CLI on this machine, so the Stripe path is proven by the signed-fixture suite (`internal/billing` webhook tests, signed with the v1 scheme) and a fake Stripe REST API behind stripe-go's own backend (`provider_test.go`: one `POST /v1/customers` across two checkouts, the id recorded; a live subscription refused with no session; `api/payments_wiring_test.go`: 0 customers at registration). The first live exercise is the owner's, with the test keys (click-list).

Suites (`go test -race`, private DB): billing, payments, dashboard/..., api, usage, cmd/vaultaire, database, account, flags — green at each PR's head.

## Prod proof

Before (2026-10-20 17:05 UTC, read-only): `/version` `d825f39`, 0 `"level":"error"` since 13:50, the 12 rule files sha256-equal to `main`, 12 alerts = 11 `BackendProbeFailing{idrive*}` + `RoutingTruthNoVerdict{idrive}`; `entitlements`/`payments`/`tenant_floor_quotas`/`stripe_events` 0 rows, 0 tenants with a Stripe customer; pack-label tenants: Bench2 (`vault100`, 1 TiB, 2,048,010 B) and Bench (`starter`, tier `bench`).

After #806 (`02d4699`, 17:36 UTC): `/version` `02d4699f790b`, 0 error lines since 17:33, `vaultaire_stripe_house_invalid_total 0` on `/metrics`, rules equal to `main`, 8 alerts right after the slot switch (the `for:` windows restart; the set climbs back to 11 + 1).

After #807 (`8aa2c44`, 18:02 UTC): `/version` `8aa2c4498b38`, 0 error lines since 18:00; `/dashboard/billing`, `/admin/revenue`, `/admin/flags` mounted (303 → `/login`, no 5xx — RP21-5); Bench2 `vault100` / `vault100` / 1,099,511,627,776 / 2,048,010 B — unchanged; 0 entitlements, payments, floor rows, Stripe customers. The "no metered line at boot" check is vacuous on prod (no Stripe key: the line was never logged); the local proof, with the old `STRIPE_METER_*` set, is the evidence.

After #808 (`374e582`, 18:29 UTC): `/version` `374e5826fb30`, 0 error lines since 18:25; `vaultaire_payment_provider_ready{provider="stripe"} 0` and `vaultaire_stripe_house_invalid_total 0` on `/metrics`; `GET /api/v1/admin/flags` (an admin JWT minted inside the 0600 box script from the box's `JWT_SECRET`, never printed) lists `pay_stripe` (default off: no key), `pay_btcpay` (off), `entitlement_lapse` (off); `POST /api/v1/admin/jobs/entitlement_lapse/run` → `job_runs` `ok`, `{"grace": "336h0m0s", "candidates": 0, "would_clear": 0, "skipped_unready": 0, "backfilled": 0, "cleared": 0, "healed": 0}`; Bench2 unchanged; 0 Stripe customers. **Not done on purpose:** registering a tenant on prod — with no Stripe key neither the old nor the new code can call Stripe, so it would prove nothing the local proof (and `TestRegister_CreatesNoStripeCustomer`: 2 → 0 against a fake Stripe API) does not, and it would leave an account on prod.

## Residuals

| # | Sev | Item | Where it goes |
|---|---|---|---|
| RP21-1 | P3 | The readiness history is in memory: a restart (several a day) forgets an outage older than the process, so the lapse job's 24 h window is "since boot" right after a deploy. A provider unready NOW still counts. Persist it (a `job_runs`-style row or a column) with WP-PAY-3's migration. | WP-PAY-3 |
| RP21-2 | P3 | With `entitlement_lapse` ON, a house kept after a Stripe cancel (renewal prepaid, the subscription gone) cannot be resized (Stripe has no subscription) or bought again (the page is in resize mode) until it lapses — the customer sees an error flash. The renew / resume flow is WP-PAY-3's. Prod: flag off, 0 rows. | WP-PAY-3 |
| RP21-3 | note | After the lapse job clears such a house, `tenants.plan` stays `house` (the job writes the record and the floors only). Readers of `plan` move to the record in WP-PAY-7. | WP-PAY-7 |
| RP21-4 | note | Provider refusals are a plain-text 409 (D-P19-B5); WP-PAY-3's billing page gives them the page's look. | WP-PAY-3 |
| RP21-5 | note | `/dashboard/billing` and `/admin/revenue` on prod were checked as mounted (303 to `/login`, no 5xx) — rendering them needs a dashboard session; the rendering is proven locally with the real templates. | — |

Carried forward unchanged: **RP20-6** (`payments.metadata.ids.customer` survives erasure — WP-PAY-3, the policy sentences) and **RP20-7** (dunning must fit inside the 14-day grace — the owner row, click-list item 5).

## Post-merge review (2026-10-20, meta)

Adversarial pass over #806, #807 and #808 against `docs/PAYMENTS.md` § 2,
`internal/billing/CLAUDE.md` and the code at `e462e77`. **Every claim above
holds; no P0/P1. One P2 (RP22-1, proven with a throwaway test) and two P3 go
to Prompt 20 Part 0.** CI, Security and Deploy green on every merge commit;
the docs PR #809 merged; prod `/version` = `374e5826fb30` on slot 8000.

Verified in depth (beyond the session's own gates):

- **Part 0 (#806):** `payments.Grant` validates the order before `Lock` and
  before its first statement, so the settle transaction of a paid invoice is
  intact when `recordInvoicePaid` swallows `ErrInvalidOrder`; `p.Order` is
  set in the same branch that defines `grant`, so `houseInvalid`'s `*p.Order`
  is never a nil dereference; the counter is unlabelled (0 from boot, on
  prod's `/metrics`); `isCurrentSubscription` guards both terminal branches
  before the flag-ON `StopRenewal`.
- **WP-PAY-0 (#807):** `go list -deps ./internal/dashboard/... | grep -c
  stripe-go` = 0 on `main`; no `STRIPE_METER`, `HandleUpgrade`,
  `registerStripePlans`, `AccruedCents` or `MeteredReporter` reference outside
  `one_product_boot_test.go`; the root `CLAUDE.md` and `docs/CONFIG.md` rows
  are gone; `internal/billing/CLAUDE.md` names them only in its "deleted"
  paragraph.
- **WP-PAY-2 (#808):** the double-billing guard holds the entitlement advisory
  lock (the same `pg_advisory_xact_lock` every `Grant` takes) across the
  provider call; `payments.Get` returns by value, so the deletion runner's
  `e.ProviderRef` after `ErrNoEntitlement` is `""` (no nil dereference);
  `StopRenewal` on a tenant with no row is a 0-row UPDATE, not an error — the
  flag-ON branches cannot start a retry storm; `currentSubscription` prefers
  the row's `provider_ref`, so a late terminal event of a replaced
  subscription is ignored and a new checkout after a lapse is granted under
  its own id; a lapsed row's stale `provider_ref` costs the deletion runner
  one idempotent cancel of an already-cancelled subscription, nothing else;
  `Registry.Provider` is nil-safe (`router.go:324`); `pay_stripe` defaults to
  "key set" at `registerFlags`; `TestDashboardImportsNoStripe` is the red test
  the design asked for.
- **Prod (read-only, 19:36 UTC):** 0 `"level":"error"` lines since 18:25
  (1,561 Warn = the dead-iDrive probes, RP20-10); the 12 rule files
  sha256-equal to `main`; 12 alerts = 11 `BackendProbeFailing{idrive*}` +
  `RoutingTruthNoVerdict{idrive}`; every job's last outcome `ok`
  (`entitlement_lapse` 18:29); `entitlements` / `payments` /
  `tenant_floor_quotas` / `stripe_events` 0 rows, 0 Stripe customers, 3
  tenants (`starter`, `vault100`, `starter`); `feature_flags` empty (every
  flag at its default); `vaultaire_payment_provider_ready{provider="stripe"} 0`,
  `vaultaire_stripe_house_invalid_total 0`, the three `entitlements_expiring`
  windows 0.

New residuals:

| # | Sev | Item | Where it goes |
|---|---|---|---|
| RP22-1 | **P2** | **Every deploy marks Stripe unready for 24 h, so the lapse job skips every Stripe row.** `Registry.Register` observes `Ready` inside `NewServer` (`server.go` ~418 / ~590); `StripeService.Ready` → `HouseCheckoutReady` is false until `Start` → `StartHouseVerification` has run once, so `ReadyLog.lastUnready["stripe"]` = boot on every boot. At 07:30 the job asks `UnreadyWithin(24 h)`: the provider is ready NOW (recorded), then the log says "unready at boot" → `skipped_unready`. Prod deploys two to five times a day: with the flag on and a key set, no Stripe house would ever lapse. **Proven** with a throwaway test (`fakeProvider` unready at `Register`, ready one minute later, `UnreadyWithin(24h)` ten hours later = true). RP21-1 ("a restart forgets an outage") is the same log read the other way round. Fix: readiness has three states — a provider whose verification has not completed is *pending* and never recorded; a `VerifyHousePrices` that could not reach Stripe keeps the previous verdict (only a mismatch is "unready" — RP22-5); the persistence RP21-1 asks for rides WP-PAY-3's migration. | Prompt 20 Part 0 |
| RP22-2 | P3 | With `entitlement_lapse` ON, `customer.subscription.deleted` of a **non-house** subscription (a legacy pack) returns from the flag branch (`webhook.go:806-820`) before the `plan = 'starter'` downgrade (`:826-840`): `plan` keeps the pack label, `legacyPlan` stays true, the billing page tells the customer to e-mail support and refuses a house checkout for ever. The flag branch applies to a house only (`HouseFromSubscription` ok); a pack keeps today's downgrade. Prod: flag off, no pack subscription. | Prompt 20 Part 0 |
| RP22-3 | P3 | A subscription acknowledged as an invalid house is recorded nowhere the deletion runner reads: the `tenants.stripe_subscription_id` write sits in the grant transaction that rolled back (`webhook.go:650`), and the runner never reads the ledger's `metadata.ids.subscription`. For an operator-made subscription with no Checkout session (`:576` heals the id only from a session) the runner finds "nothing to bill" and erases an account Stripe still charges. `houseInvalid` should record the subscription id and status on the tenant outside the rolled-back transaction. | Prompt 20 Part 0 |
| RP22-4 | note | The guard holds the entitlement lock across up to three Stripe round trips (customer, `HasLiveSubscription`, session) on one pool connection while the inner reads take a second; a webhook of the same tenant waits on its own two. Pool 50 (`postgres.go:49`) is fine at launch scale; a `lock_timeout` on the guard's transaction would turn exhaustion into a clean error instead of a hang. | WP-PAY-3 |
| RP22-5 | note | A transient failure of the hourly `VerifyHousePrices` (a Stripe 5xx, a timeout) sets `verified = false` → 24 h of `skipped_unready` although payments were never impeded. Same fix as RP22-1. | Prompt 20 Part 0 |
| RP22-6 | note | `Registry.Resize` refuses a **downsize** too while `pay_stripe` is off (the design's "no new checkouts/resizes"); a customer wanting to pay less is told "switched off". Decide whether a shrink is new business. | WP-PAY-5 |
| RP22-7 | note | `createCustomer`'s idempotency key `customer-<tenant>` with an e-mail changed within 24 h of a failed record = Stripe `idempotency_error` until the key expires. Include a hash of the e-mail in the key, or drop the key on that error. | WP-PAY-3 |
| RP22-8 | note | `vaultaire_stripe_house_invalid_total` has no rule; WP-PAY-3's `vaultaire-payments.yml` gets `StripeHouseInvalid` (`increase(…[1h]) > 0`; unlabelled, so born at 0). The `vaultaire_entitlements_expiring` windows are `7d | 30d | in_grace` (the design wrote 14d/3d/grace) — the rules follow the code. | WP-PAY-3 |

Re-confirmed: RP21-1..5 and RP20-6/7 as written. The next migration is
**098** (WP-PAY-3: `entitlements.reminders_sent` + the persisted readiness
log).
