# internal/payments

The provider-neutral core of billing (WP-PAY, design of record `docs/PAYMENTS.md`; WP-PAY-1 built 2026-10-20, migration 097). **What a tenant has bought lives in our database with a date**; a payment from any provider extends the date; the `entitlement_lapse` job (`internal/api/entitlement_lapse.go`) clears a house whose date plus grace has passed. Stripe (`internal/billing`) is one provider that writes here; BTCPay (`internal/payments/btcpay`, WP-PAY-4) will be the second. Imports `internal/usage` and `internal/api/landing` (the price sheet) — **never a provider SDK**: the dashboard imports this package and nothing that reaches `stripe-go` (`TestDashboardImportsNoStripe`, WP-PAY-2).

## Providers (`provider.go`, WP-PAY-2, Stage 1a Prompt 19)

`Provider` is `docs/PAYMENTS.md` § 2.2: `Name`, `Renewal`, `Ready` (configured ∧ verified), `StartCheckout(Checkout)`, `Resize` → `ResizeOutcome{Applied *Order | RedirectURL}` (the design calls the type `Resize`; that name is the record's function here — D-P19-B1), `StopRenewal`/`ResumeRenewal`, `Cancel` (stop the money now; `ErrProviderUnconfigured` when the provider has no credentials), `ManageURL`, `Webhook`. Customer creation is not on it (it is `StartCheckout`'s business — no third party knows a tenant until it buys, 43.2).

`Registry` (`NewRegistry(db, flags, grace, logger)`, built in `api/server.go`, `Deps.Payments` of the dashboard): `Register(p)` (exports `vaultaire_payment_provider_ready{provider}` at once), `Provider(name)` (whatever the flag — cancel, manage link, webhooks), `For(ctx, tenant, name)` → `ErrUnknownProvider` / **`ErrProviderDisabled`** (flag `pay_<name>` off for the tenant: 409 `provider_disabled`) / **`ErrProviderUnavailable`** (not Ready), `Enabled` (what the page offers), `Ready`, `Status` (= `StatusOf` with the grace), `StartCheckout`, `Resize` (the house's provider — the row's, else stripe; when the provider changed it in place the record + floors are written in one transaction, `payments.Resize`), `ManageURL` (never flag-gated). **The double-billing guard** is the package function `StartCheckout(ctx, registry, tenant, provider, Checkout)` (§ 2.5): under `Lock`, a granted row (`lapsed_at` NULL) renewed `auto` by ANOTHER provider → **`ErrOtherProviderLive`** (409 `other_provider_live`); then the provider's own checkout (Stripe's `HasLiveSubscription` → `ErrLiveSubscription` for a second Stripe subscription), the lock held across the provider call so a webhook and a checkout cannot pass each other.

Readiness: `StartReadiness(ctx, 60s)` records every provider into a `ReadyLog` (last unready time, **in memory — a restart forgets an outage older than the process, RP21-1**) and the gauge; `Registry.UnreadyWithin` is the lapse job's `ReadyHistory` (a provider unready NOW counts too; an unregistered provider — `manual` — never). Flags: `FlagPrefix` + name (`pay_stripe`, `pay_btcpay`), `FlagEntitlementLapse`. Tests `provider_test.go` (fake provider: flag → readiness order, gauge from Register, the history window, the guard with a Stripe `auto` row, resize writes the record and keeps the date).

## Pricing (`pricing.go`)

`QuoteHouse`, `Quote`/`QuoteLine`, `ValidateHouseOrder` (the product rules: a floor, ≤ `landing.MaxIntentTB` per floor, pin-hot ≤ downstairs, a known period), `Line*`, `Cents`, `UnitCents` — moved from `internal/billing` in WP-PAY-2 (billing keeps aliases), all from `internal/api/landing/prices.json` only.

## The one writer of a house

`tenant_floor_quotas` + `tenant_quotas` stay the enforcement truth the write path reads; `entitlements` is the billing truth. Every function that changes a house takes the **caller's transaction** and writes both in it:

| Function | What it does |
|---|---|
| `Lock(ctx, tx, tenant)` | `pg_advisory_xact_lock(hashtextextended('entitlement:'‖tenant, 0))` — one tenant's grants, extensions, clears (and WP-PAY-2's checkout start) one at a time. Taken first by every function below; re-entrant within a transaction |
| `Grant(ctx, tx, tenant, Order, paidThrough, provider, ref, Renewal)` | upsert the row (`lapsed_at` cleared) + `usage.SetHouseTx`. **Never shortens a running date** (`GREATEST(paid_through, given)` — a stale or replayed event cannot take paid time away; only an operator refund shortens one, WP-PAY-7); a lapsed row takes the given date. Idempotent: a replay with the same values updates no row (`updated_at` unchanged) |
| `Extend(ctx, tx, tenant, Period)` | `paid_through = GREATEST(paid_through, now()) + 1 month | 1 year`, in UTC; grants a lapsed row again (floors restored). **Month-end rule (accepted):** `2026-01-31 + 1 month = 2026-02-28` (Postgres), and the date stays on the 28th after |
| `StopRenewal` | `renewal = 'prepaid'`: nobody will extend it; the house runs to `paid_through` + grace |
| `Revoke` | `usage.ClearHouseTx` (free-tier limits, **used bytes untouched** — billing never deletes data) + `lapsed_at = now()`. Writers of `lapsed_at`: the lapse job, and — while the `entitlement_lapse` flag is OFF — the Stripe webhook's clearing branch **while `entitlement_lapse` is OFF for the tenant** — ON, a terminal Stripe status only stops the renewal (WP-PAY-2) and the job is the only writer. Marking the row is what keeps the job's reconcile from granting back a house the webhook took away |
| `Resize` | the dashboard's in-place resize: the lines of a granted row change, the date stays; floors written |
| `HouseWriter` | `billing.HouseQuotas` over `Resize` / `Revoke` in one transaction each — what `server.go` wires into the webhook and the dashboard (`houseQuotas`), so no path changes a house's floors without its entitlement |
| `Get`, `StatusOf(ctx, db, tenant, grace, now)` | the read model (`Found`, `Granted`, `InGrace`, `GraceEnds`) the billing page renders from (`Registry.Status`, WP-PAY-2) |
| `InTx` | begin → fn → commit |

Lock order: entitlement lock → `tenant_quotas` → `tenant_floor_quotas` (`usage/ledger_tx.go`). Nothing that holds a key, head-row or upload lock takes the entitlement lock.

`Order` = `{Std, Vault, PinHot int; Period}` — `billing.HouseOrder` and `billing.Period` are aliases of `payments.Order` / `payments.Period`. `Order.Validate()` returns the exported, `errors.Is`-able **`ErrInvalidOrder`** (unknown period, a negative line, `PinHot > Std` — migration 097's CHECK `pin_hot_tb <= std_tb` is the same rule); `Grant` checks it before it takes the lock or runs any statement, so a caller's transaction is intact after the refusal and a webhook can tell the order's fault from a transient one (RP20-4).

## The ledger (`ledger.go`)

`payments` rows keyed by `(provider, provider_id)` (a Stripe invoice id, a BTCPay invoice id, `manual:<uuid>`). `RecordPending`, `Settle` (creates or moves the row to `settled` and assigns **our invoice number** `ST-<year>-<000001>` from `invoice_counters` — the year's row is incremented with `INSERT … ON CONFLICT DO UPDATE … RETURNING` inside the settle transaction, so concurrent settles are numbered one after another and a rolled-back settle gives its number back: gapless per UTC year; a replay of a settled row returns it, `Newly` false), `Fail` (a declined charge; never moves a settled row back), `Expire` (partial or not) / `Invalidate` (open rows only), `Credit` (a settled `credit` row, no receipt number). Statuses: `pending processing settled failed expired expired_partial invalid refunded` ('failed' is WP-PAY-1's addition to the design). **Metadata keys:** `ids` (provider ids, kept), `addresses` and `txids` (dropped at erasure). The ledger is an accounting record: never pruned by the retention job; `account.EraseRows` nulls `tenant_id` and `note` and drops `addresses`/`txids`.

## Knobs, readiness, metrics

- `ENTITLEMENT_GRACE` (`GraceFromEnv`): a Go duration, 1h–60d, default 14 d (336h, F-26); a rejected value is a Warn and the default.
- `ReadyHistory` — `UnreadyWithin(provider, 24h)`: the lapse job skips a tenant whose provider was unready in the window (a house is never lost because our processor was down). Production wiring = the `Registry` (WP-PAY-2); `AlwaysReady` is the runner's default without one (tests).
- `vaultaire_payment_provider_ready{provider}` — 1 while the provider can take a payment; every registered provider's series from `Register` (prod without a Stripe key: `stripe` 0).
- `vaultaire_payments_total{provider,kind,outcome=settled|failed|replayed}` (`CountPayment`, called AFTER the caller's commit; Stripe charge outcomes at 0 from boot), registered through `Collectors()`. The lapse job adds `vaultaire_entitlements_lapsed_total` and `vaultaire_entitlements_expiring{window=7d|30d|in_grace}` (read from the table) in `internal/api`.

## Tests

DB-backed (`testutil.DSN()`), each test on a tenant of its own (`newFixture`); invoice-number tests use a year of their own (the counter is global). `entitlement_test.go`: grant idempotent / never shortens / re-grants a lapsed row, Extend from the later of the date and now, the month-end rule (31 Jan, leap year, 31 Mar), StopRenewal + Revoke keep used bytes, the HouseWriter resize keeps the date, settle idempotent and failed→settled, **ten concurrent settles numbered 1..10 after a rolled-back one**, expire/invalidate/credit.
