# Stage 1a — Prompt 20: Part 0 (WP-P19 review residuals) and WP-PAY-3 (receipts, reminders, the billing page, admin money tooling, metrics and rules)

Date: 2026-10-20. Prod at the start: `374e5826fb30` (slot 8000), no `STRIPE_*` env, `feature_flags` empty, 0 rows in `entitlements` / `payments` / `payment_events` / `tenant_floor_quotas`, 3 tenants. Branches `stage1a-p20-part0`, `stage1a-p20-pay3` (stacked, rebased onto main after #812 merged), worktrees `../vaultaire-p20{,a}`; private test DB `vaultaire_p20`.
Prompt: Part 0 = RP22-1 (P2) + RP22-5, RP22-2, RP22-3, RP22-4 + the prod read; Part A = WP-PAY-3 as `docs/PAYMENTS.md` § 3.8 and § 7. Design of record `docs/PAYMENTS.md`; contract `docs/ARCHITECTURE_CONTRACT.md` (line 12: metadata has clocks; line 13: the tenant is the unit of disclosure), lane `docs/surfaces/DEVELOPER.md`.

**PRs:** Part 0 #812 (`ab1cba5`), Part A #813 (`5cb3a57`; branch commits `f539759` part 1 receipts + reminders + readiness + mails, `e425c12` part 2 the billing page, `08bd34e` part 3 admin + rules, `efd0c24` part 4 the policy sentences — the queue lands the PR as one commit); this docs PR last. **Migration 098. Next migration: 099.**

## Decisions

| # | Decision | Why |
|---|---|---|
| D-P20-1 | `Provider.Ready` answers a typed **`payments.Readiness`** — `Unready` (the zero value), `Ready`, `Pending` — not a third return. Stripe is `Pending` while a key is set and no verification has completed; a `VerifyHousePrices` that could not REACH Stripe (`billing.ErrPricesUnreachable`: a dial error, a timeout, a 5xx, a 429) keeps the previous verdict and logs at Warn; a mismatch, a missing price id or a Stripe 4xx (no such price, a revoked key) is `Unready`; no key = `Unready`. `ReadyLog.Record` ignores `Pending`; the gauge exports 0 for it (the Help string says so); `Registry.UnreadyWithin` treats a provider pending NOW as not unready; `Registry.For` needs `Ready`. | RP22-1: `Register` observed Stripe inside `NewServer`, before `StartHouseVerification` had run — every deploy marked Stripe unready for 24 h and the lapse job skipped every Stripe row. A typed state keeps every fake provider honest (`fakeProvider{state: Pending}`) and the zero value is the no-key case. A Stripe 4xx is Stripe's answer, so it is a verdict (RP22-5 names network errors, 5xx and timeouts only). |
| D-P20-2 | The guard's transaction in `payments.StartCheckout` sets `SET LOCAL lock_timeout = '10s'` before `Lock`; Postgres `55P03` is `ErrProviderUnavailable`. The bound is a package var (`checkoutLockTimeout`, 10 s) so the test proves the mapping at 2 s and the default separately. | RP22-4. The page already says "try again in a minute" for `ErrProviderUnavailable`. |
| D-P20-3 | `houseInvalid` records the subscription id + status on the tenant with `WHERE stripe_subscription_id IS NULL OR = '' OR = $1` — outside the rolled-back grant, never over a different live id. | RP22-3: the deletion runner reads `tenants.stripe_subscription_id` for a tenant with no entitlement row; a subscription Stripe still bills must be found there. |
| D-P20-A1 | The issuer's legal data has one source: **`hosts.Legal()`** → `LegalEntity{Name: "FairForge LLC", AddressLines: ["State of Utah, United States"], Jurisdiction, Email: support@<Domain>}`, read by the receipt page and the receipt mail. The legal templates keep their own "FairForge LLC" this prompt. **The repository holds no street address** (`terms.html` names the entity and the jurisdiction only): `AddressLines` is the jurisdiction until the owner adds the address in `hosts.go` — one line, [YOU]. | The prompt's A.0 item 4. A made-up address on a receipt would be worse than none. |
| D-P20-A2 | `PaymentWebhookFailing` reads **`vaultaire_payment_webhook_total{provider,outcome=ok\|duplicate\|rejected\|error}`**, a counter the webhook handler increments once per delivery (`billing.ServeHTTP`, `payments.CountWebhook`), every Stripe outcome pre-created at 0. | The prompt said "from the existing HTTP series — name the series you use": the only HTTP series on `/metrics` are `vaultaire_requests_total` and `vaultaire_errors_total`, both unlabelled (the labelled `RequestCounter` in `metrics.go` is not registered on the served registry). No per-route series exists, so a 5xx ratio of `/webhook/*` cannot be written; the design's § 7 names `vaultaire_payment_webhook_total{provider,event,outcome}` — built without the `event` label (one series per outcome, nothing per event type). |
| D-P20-A3 | A manual payment on a tenant with **no entitlement row must name the house** (`order {std_tb, vault_tb, pin_hot_tb}`; `payments.ErrOrderRequired`, 400); on a tenant with a row the house is kept and the date extended by one `period` (the row's house is changed with the entitlement adjustment, not with a payment). | § 7 says "a manual payment: amount, period, note → Grant/Extend" — a Grant needs a house. The local proof's "12 months" = `period: annual`. |
| D-P20-A4 | A refund shortens `paid_through` by **the refunded share of the period the charge bought** (`period_start..period_end` on the charge's row × amount/charge amount); a charge that records no period refuses the shortening (`ErrRefundNoPeriod`, 400) — the operator uses the entitlement adjustment. The charge is marked `refunded` once its refunds reach its amount; more is `ErrRefundExceeds` (409). A refund row takes a credit-note number from the same gapless counter. | A proportional rule is explainable on the receipt; inventing a period for a payment that has none is not. Every manual payment records its period (the Stripe ledger rows do since WP-PAY-1). |
| D-P20-A5 | The Renew button is **not gated by `quota_checkout`**; it shows for a prepaid or lapsed row when a provider may take the money — the row's provider when the tenant may use it (`Registry.Ready`), else any `Registry.Enabled` one — labelled "Add a year" / "Add a month" (granted, before the date) or "Renew now" (in grace or lapsed). `POST /dashboard/billing/renew` checks out the row's own house with purpose `renew`; a house a provider renews by itself is refused with a flash. | The house exists already: renewing it is not the new-business decision `quota_checkout` guards; the `pay_<provider>` flags are. A manual house is renewed by card; a lapsed Stripe house is bought again (RP21-2's way out). |
| D-P20-A6 | Audit rows name the actor in **`metadata.actor`** (an admin's user id also in `performed_by`); the webhook's actor is `stripe-webhook`. | `audit_logs.performed_by` is a UUID column (`audit.nullUUID`): a named actor cannot go there. |
| D-P20-A7 | `billing.house_granted` is written from **`payments.GrantRecorded`'s `Changed`** (the upsert's `RETURNING`: its `WHERE NOT (…)` excludes a replay, so a replay returns no row) by the three callers after their commit: the webhook (`auditGrant`), `RecordManualPayment` (and its `Extend`, always a change), nothing else grants. | The prompt's item 5. The function is not changed in signature: `Grant` wraps `GrantRecorded`. |
| D-P20-A8 | The readiness log is persisted in `payment_provider_readiness` (098): written on a state change (the first observation of a process counts as one) and once an hour; `last_reason` is the last UNREADY reason and survives a ready write; `UnreadyWithin` reads memory first, then the row. Infrastructure: no tenant column, never erased, never exported (said in `account/schema_test.go`). | RP21-1. One row per provider; writes are rare by construction. |
| D-P20-A9 | The lapse job's `skipped_unready` is a gauge **read from `job_runs.result`** on scrape (`vaultaire_entitlement_lapse_skipped_unready`, no series before the first run); the rule's "two consecutive daily runs" is `min_over_time(…[30h]) > 0`. | The R13 lesson (never an in-process value); 30 h of a value that changes once a day = the last two runs both above 0. |
| D-P20-A10 | The `vaultaire_entitlements_expiring` windows stay `7d \| 30d \| in_grace` (the code's), and the rule file documents them; no 14d/3d/grace series was added. | RP22-8 / the prompt's A.0 item 5: the rules follow the code. The reminder steps use the design's 14/3 days inside the job, not as gauges. |
| D-P20-A11 | `PaymentProviderDown` is **critical** (checkout closed, every house of the provider kept by the lapse job); the other four are warnings. | The design gives no severities; a provider down is the one customer-facing one. |
| D-P20-A12 | The four admin routes are documented in the OpenAPI document (`HouseOrder`, `Entitlement`, `LedgerRow` schemas). | `TestOpenAPIDriftGuard` refuses an undocumented route; "consciously undocumented" is for routes with no customer-facing shape. |

## What was built

### Part 0 (#812) — verified at `e462e77`, built on `c463ffa`

- **RP22-1 + RP22-5** — `payments.Readiness` (`provider.go`), `StripeService.Ready` → `HouseReadiness` (`billing/provider.go`, `house.go`: `houseState.decided`, `ErrPricesUnreachable`, `transientPriceError`, `VerifyHousePrices` keeps the verdict, `StartHouseVerification` retries an unreachable Stripe in 5 min without the "DISABLED" Error line), `ReadyLog.Record` ignores Pending, `observe` exports 0 for it, `UnreadyWithin` falls through to the log for a pending provider, the gauge Help string.
- **RP22-2** — `handleSubscriptionDeleted`: the flag-ON branch only when `HouseFromSubscription(&sub)` is ok.
- **RP22-3** — D-P20-3; `TestCancelSubscription_RecordedInvalidHouse_CancelsOnce` in the runner's tests.
- **RP22-4** — D-P20-2 (`lib/pq` error code).
- Fakes updated: `payments.fakeProvider{state}`, `dashboard/handlers.fakeHouseBilling.Ready`, `api.readyProvider`, `payments_wiring_test`.

### Part A — WP-PAY-3 (#813)

A.0, verified at `e462e77` (refs from the prompt, confirmed): `email.Sender` `internal/email/email.go:21`, `NewSender` `:28`, `LogSender.Send` `:80` (recipient + subject + sizes only); templates embedded by `templates.go` (`go:embed templates/*.html`, `password_reset.html` + `verification.html`); `audit.Entry` `internal/audit/audit.go:36`, `Record` `:98` (`performed_by` through `nullUUID` — D-P20-A6); the lapse job's `billing.house_lapsed` `entitlement_lapse.go:316`; the admin group `server.go:1036`, `handleAdminJobRun` `admin_jobs.go:122`, `adminJobTrigger` `:127`; `HandleTenantDetail` `dashboard/handlers/tenants.go:154`; `router.go:323-324`; "FairForge LLC" in the legal templates (`terms.html:101`, `aup.html:57`, `baa.html:109`) and `base.html:60` — not in `hosts` (`SupportEmail` only, `hosts.go:258`), no street address anywhere; metrics as the prompt lists them (the `expiring` windows `7d|30d|in_grace`); migration 097 has no reminders column; promtool tests in `deploy/monitoring/tests/`, no Go test ran promtool before this prompt.

1. **Migration 098** `098_payment_reminders_and_readiness.sql`: `entitlements.reminders_sent JSONB NOT NULL DEFAULT '{}'`; `payment_provider_readiness(provider PK, last_ready_at, last_unready_at, last_reason, updated_at)`. The three schema tests pass with it (`TestSQLLiteralsMatchSchema`, `TestErasedTablesCoverSchema` — the readiness table named as infrastructure in the test —, `TestExportSectionsCoverSchema`).
2. **Receipts** — `payments/receipts.go` (`Receipt`, `ReadReceipt`, `ListReceipts`, `ListLedger`); `dashboard/handlers/billing_receipts.go` (`HandleReceipt`, `populateReceipts`), `templates/customer/receipt.html` (standalone, `@media print`, `Cache-Control: no-store`, `noindex`); the receipts card on `billing.html`; `payments/mail.go` `SendReceipt` ← `billing.recordInvoicePaid` on `Settled.Newly` (`ReceiptSender`, `SetReceiptSender`), after the commit on `payments.Detached` (15 s), one Warn on failure.
3. **Reminders** — `payments/reminders.go` (D-P20 steps; `DueReminders`, `MarkReminderSent`, `ReminderKey`), `api/payment_reminders.go` (`PaymentRemindersRunner`, hourly, 3 min boot delay, 5 min ceiling; a failed mail = `failed` + a note + retried next hour; sent-but-not-marked = an Error line and one mail too many next hour; a process with no sender sends and marks nothing), `email/templates/renewal_reminder.html` (+ `RenderRenewalReminder`), `house_lapsed.html` (+ `RenderHouseLapsed`) sent by `EntitlementLapseRunner.Mailer` after `clear` (which now returns the row); `vaultaire_payment_reminders_total{step,outcome}` pre-created for all 4 × 2.
4. **Billing page** — D-P20-A5 (`renewOffer`, `HandleRenewHouse`, `HouseView.CanRenew/RenewLabel/RenewProvider`, `?renewed=1`); `refusedForProvider` renders `refusalPageHTML` (409 + `X-Error-Code` kept; `TestCheckout_ProviderFlagOff_409` etc. still pass).
5. **Admin** — `payments/admin.go` (D-P20-A3/A4; `ErrReasonRequired`, `ErrOrderRequired`, `ErrRefundExceeds`, `ErrRefundNoPeriod`, `ErrNoSuchPayment`), `api/admin_payments.go` (the four routes, `adminMoneyError` → 400/404/409; the shorten-a-date comment on `handleAdminAdjustEntitlement`), OpenAPI entries (D-P20-A12), `dashboard/handlers/admin_billing.go` (three htmx form handlers, dollars → cents, a missing reason 400 before anything is read), `populateAdminBilling` in `tenants.go`, the Billing panel in `tenant_detail.html`, routes in both routers.
6. **Audit** — D-P20-A6/A7: `payments.GrantRecorded`/`AuditGranted`, `StopRenewalChanged`/`AuditRenewal`; the webhook's `auditGrant` (renewal flips from `cancel_at_period_end`), the two flag-ON `StopRenewal` branches audit `renewal_stopped` once.
7. **Metrics + rules** — D-P20-A2/A8/A9/A11: `payments.EnabledCollector`, `api.lapseSkippedCollector`, `deploy/monitoring/vaultaire-payments.yml`, `tests/vaultaire-payments_test.yml`, `vaultaire-jobs.yml` (`PeriodicJobStale` + `payment_reminders`), README row + paragraph, `TestPaymentsRules_Parse`.
8. **RP22-7** — `customerIdempotencyKey(tenant, email)` = `customer-<tenant>-<8 hex of sha256(lower(email))>`.
9. **Policy sentences** — `docs/PAYMENTS.md` § 3.9 (part 4, not wired); **RP20-6** implemented: `account.EraseRows` `metadata = (metadata - 'addresses' - 'txids') #- '{ids,customer}'` (`TestEraseRows_StripsStripeCustomerID`).
10. **RP21-1** — D-P20-A8 (`payments/readiness_store.go`).

Docs: `internal/{payments,billing,api,dashboard,dashboard/handlers,email,account,hosts,database}/CLAUDE.md`, `deploy/monitoring/README.md`.

## Before → after

| Probe | Before | After |
|---|---|---|
| Stripe registered at boot, verified a minute later, the lapse job asks ten hours later | `UnreadyWithin(24h)` = true → every Stripe row `skipped_unready` | false (pending is not an outage); unready NOW still true |
| Hourly `VerifyHousePrices` hits a Stripe 503 after a good verdict | `verified = false` → unready 24 h | the verdict kept, one Warn, checkout open |
| `customer.subscription.deleted` of a legacy pack with `entitlement_lapse` ON | `plan` keeps the pack label | `starter`, `house_period` '' |
| Three deliveries of an invalid-house subscription | id recorded nowhere the runner reads | `tenants.stripe_subscription_id` = the subscription, status `active`; a different live id untouched |
| A checkout while another transaction holds the tenant's entitlement lock | waits for ever | `ErrProviderUnavailable` within the bound |
| A settled Stripe invoice | no mail | one receipt mail per NEWLY settled row |
| A prepaid house 13 days before its date | nothing | one `t-14d` mail, then nothing until `t-3d` |
| A lapsed house | nothing | `house_lapsed.html` after the Revoke, audit `billing.house_lapsed` |
| The billing page of a prepaid or lapsed house | the sentence only | the sentence + Renew (when a provider may take the money) + the receipts |
| 409 `provider_disabled` | plain text | a styled page, same status and header |
| An admin recording a wire | no way | a numbered `manual` row, the house granted/extended, audited |
| A restart during a Stripe outage | the outage forgotten | `payment_provider_readiness.last_unready_at` read back |

## Local live proof

The binary at `efd0c24` (Part A's branch head; `/version` `efd0c24e1616`) on port 18420 against the private DB `vaultaire_p20`, `STORAGE_MODE=local`, no Stripe key, no `EMAIL_PROVIDER` (the `LogSender`: recipient + subject only), `ENTITLEMENT_GRACE=336h`. Two scripts (`scratchpad/proof/localproof.sh`, `receiptproof.sh`); the tenants and their rows removed after.

1. Register a tenant → `tenant-aeeddb0e2dc6464f`; the user made `admin` by SQL; `/auth/login` for the JWT.
2. `POST /api/v1/admin/tenants/{id}/payments {"amount_cents":5388,"period":"annual","purpose":"manual","note":"wire ref P20","reason":"local proof","order":{"std_tb":1}}` → 201 `{"granted":true,"invoice_number":"ST-2026-000100","paid_through":"2027-10-20T22:43:50Z","payment_id":"c72cd916-…"}` (the year's counter is shared with the package tests on this DB — the number is the next gapless one, not 000001). `GET …/payments` → entitlement `manual prepaid 2027-10-20`, payments `[("ST-2026-000100","settled",5388)]`; the row records `period_start 2026-10-20`, `period_end 2027-10-20`, `std_tb 1`.
3. The entitlement sentence (the page's `houseSentence` over the row): **"Paid through 20 Oct 2027 via manual, renewal stopped."**
4. Audit rows on the tenant: `account.created`, `billing.payment_recorded_manual` (actor in the metadata + `performed_by` = the admin), `billing.house_granted` (same).
5. `POST /api/v1/admin/jobs/payment_reminders/run` → 202, run `ok {"sent":0,"failed":0,"candidates":0}` (nothing due: the date is a year away).
6. `UPDATE entitlements SET paid_through = NOW() + INTERVAL '13 days'` → run → `ok {"sent":1,"failed":0,"candidates":1}`; the LogSender line: `to=p20proof-…@test.local subject="Your Stored house is paid for 14 more days"` (13 days + the DST hour, rounded up); `reminders_sent` = `{"t-14d@2026-11-02T23:43:53Z": "2026-10-20T22:43:53Z"}` → run again → `{"sent":0,…,"candidates":0}` (once per period).
7. The `entitlement_lapse` flag set for the tenant (`PUT /api/v1/admin/flags/entitlement_lapse`), `paid_through = NOW() - 20 days` → run → `ok {"candidates":1,"cleared":1,"would_clear":0,"skipped_unready":0,"healed":0}`; after: `lapsed_at` set, 0 `tenant_floor_quotas` rows, audit `billing.house_lapsed`, the LogSender line `subject="Your Stored house has ended — nothing was deleted"`.
8. `/metrics`: `vaultaire_payment_reminders_total{outcome="sent",step="t-14d"} 1` and the other seven pairs at 0, `vaultaire_payment_provider_enabled{provider="stripe"} 0`, `vaultaire_entitlement_lapse_skipped_unready 0` (from the run's `job_runs.result`), `vaultaire_payment_webhook_total{outcome="error",provider="stripe"} 0`. **0 `"level":"error"` lines** in the server log.
9. Through a real dashboard session (login form + CSRF cookie) on a second tenant: `GET /dashboard/billing/receipts/{id}` → **200 text/html**, `<title>Receipt ST-2026-000101, Stored</title>`, "FairForge LLC", "State of Utah", "Total paid … $53.88", "recorded by Stored support"; `GET /dashboard/billing` → the sentence "Paid through 20 Oct 2027 via manual, renewal stopped.", the receipts list with the link; **no Renew button** (no provider may take money: no key, `pay_stripe` off — D-P20-A5; the button is proven with a fake provider in `TestBillingPage_ReceiptsListAndRenewButton`); another tenant's session on the same id → **404**. The admin tenant page answered 303 (admin sessions need MFA): the Billing panel's data and forms are proven by `TestAdminBillingPanel_*` with the real handlers.

The "skipped_unready = 0 ten minutes after boot with a fake Stripe REST backend" proof: `TestReadiness_PendingAtBootIsNotUnready` (a provider pending at Register, ready a minute later, `UnreadyWithin(24h)` ten hours later = false) and `TestReadiness_TransientVerifyKeepsVerdict` (the fake REST backend pattern of `billing/provider_test.go` behind `setPriceSource`: unreachable before any verdict → still pending; a 503 after a good verdict → still ready). The local run above shows the lapse job on the same binary with `skipped_unready 0` three minutes after boot.

## Prod proof

### Part 0 (after #812 deployed — `ab1cba56f8ee`, slot 8001, active since 22:20:31 UTC)

Read at 22:33 UTC (the box script, sudo + the admin JWT minted inside from `JWT_SECRET`, nothing printed): `/version` `ab1cba56f8ee` (build 22:19:05Z); **0 `"level":"error"` lines** since the switch (3 Warn); the 12 rule files on the box sha256-equal to `main` at `ab1cba5` (the worktree's `vaultaire-jobs.yml` differs by this prompt's `payment_reminders` edit only); **12 alerts** = 11 `BackendProbeFlapping{idrive, idrive-<10 regions>}` + `RoutingTruthNoVerdict{idrive}` — *flapping*, not *failing*: the iDrive drivers went back to the original reseller account the same day (#811), so the probes recover and fall; `vaultaire_payment_provider_ready{provider="stripe"} 0` with the new Help string (no key — unchanged), `vaultaire_stripe_house_invalid_total 0`; `POST /api/v1/admin/jobs/entitlement_lapse/run` → 202, the run `ok` at 22:33:29 with `{"backfilled":0,"candidates":0,"would_clear":0,"cleared":0,"skipped_unready":0,"healed":0,"grace":"336h0m0s"}`; every job's last outcome `ok` (`dedup_gc` and `routing_truth` with their iDrive notes); `entitlements` / `payments` / `payment_events` / `tenant_floor_quotas` 0 rows, 3 tenants, **`feature_flags` 3 rows** (not empty as the prompt said: the 2026-10-05/07 rows `vault_parity` per-tenant + global and `sync_backend` for the sync-lab tenant — WP-VAULT-1 / the Sync loop; not touched).

The "skipped_unready = 0 ten minutes after boot with a fake Stripe REST backend" half of the proof is the local run below (prod has no key).

### Part A (after #813 deployed — `5cb3a5770b9d`, slot 8000, active since 23:04:51 UTC)

Read at 23:05 UTC with the same box script (read-only + the one trigger): **0 `"level":"error"` lines** since the switch; **migration 098 applied** — `entitlements.reminders_sent jsonb DEFAULT '{}'::jsonb`, `payment_provider_readiness(provider, last_ready_at, last_unready_at, last_reason, updated_at)` — and already holding one row written at the boot's `Register`: `stripe | last_unready_at 2026-10-20 23:05:01 | "STRIPE_SECRET_KEY is not set"` (RP21-1 proven on prod: the outage log is in the table); the new series on `/metrics` at 0 — `vaultaire_payment_reminders_total` all eight pairs, `vaultaire_payment_webhook_total{provider="stripe"}` all four outcomes, `vaultaire_payment_provider_enabled{provider="stripe"} 0`, `vaultaire_entitlement_lapse_skipped_unready 0` (from the 22:33 run's result); **`payment_reminders` registered** (`every 1h0m0s`) and run once through `POST /api/v1/admin/jobs/payment_reminders/run` → 202, `ok` at 23:05:34 with `{"sent":0,"failed":0,"candidates":0}`; **every job's last outcome `ok`**; `GET /api/v1/admin/tenants/<tenant zero>/payments` → `{"entitlement": null, "status": {"found": false, …}, "payments": 0}` (the route answers on prod); the rule file **not** on the box (`/etc/prometheus/rules/` has no `vaultaire-payments.yml`; `vaultaire-jobs.yml` is still `b3703c6f…`, `main`'s pre-#813 hash) — the [OPS] block below; `entitlements` 0, `payments` 0, `payment_provider_readiness` 1, `tenants` 3; alerts 1 (`RoutingTruthNoVerdict{idrive}`) right after the slot switch — they climb back to the probe set within the hour, as after every switch.

**A manual payment on tenant zero was not done:** it is the owner's call (the prompt), it writes a numbered ledger row and a house on a live account, and this session had no yes. The receipt and the refund that shortens it back are proven locally (above) and by `TestAdminManualPayment_*` / `TestAdminRefund_ShortensWhenAsked` / `TestReceiptPage_RendersFromLedger`.

## Residuals (RP23-*)

| # | Sev | Item | Where it goes |
|---|---|---|---|
| RP23-1 | note | No street address in the repository: the receipt prints the entity and the jurisdiction (D-P20-A1). | [YOU] one line in `hosts.Legal()` |
| RP23-2 | note | The policy sentences (§ 3.9) are a draft; wiring them into `privacy.html` / `terms.html` is a docs PR after the owner's yes (F-28). | owner |
| RP23-3 | note | `vaultaire-payments.yml` and the re-installed `vaultaire-jobs.yml` are not on the box (the [OPS] block). | [OPS] |
| RP23-4 | note | A refund of a Stripe charge made in the Stripe dashboard is recorded by hand here (`provider_ref` = `re_…`); the `charge.refunded` webhook is not handled — WP-PAY-7 when the Stripe go-live needs it. | WP-PAY-7 |
| RP23-5 | note | The reminder steps assume `ENTITLEMENT_GRACE` ≥ 2 d (the `t+1d` and last-day windows overlap below that); prod's 14 d is fine, a lab with `1h` sends only `t-14d`/`t-3d`. | note |
| RP21-2 | closed | A house kept after a Stripe cancel can be bought again: Renew on a lapsed or prepaid row (D-P20-A5). | — |
| RP21-3 | carried | `tenants.plan` stays `house` after a lapse. | WP-PAY-7 |
| RP22-6 | carried | A downsize while the flag is off. | WP-PAY-5 |
| RP20-7 | carried | owner. | owner |

## [OPS] — install the rules (the operator, not this prompt)

```bash
scp deploy/monitoring/vaultaire-payments.yml deploy/monitoring/vaultaire-jobs.yml vaultaire-slc:/tmp/
scp deploy/monitoring/tests/vaultaire-payments_test.yml deploy/monitoring/tests/vaultaire-jobs_test.yml vaultaire-slc:/tmp/
ssh vaultaire-slc 'cd /tmp && promtool test rules vaultaire-payments_test.yml && promtool test rules vaultaire-jobs_test.yml'
ssh vaultaire-slc 'sudo install -m 0644 /tmp/vaultaire-payments.yml /tmp/vaultaire-jobs.yml /etc/prometheus/rules/ && promtool check rules /etc/prometheus/rules/vaultaire-payments.yml /etc/prometheus/rules/vaultaire-jobs.yml && sudo systemctl reload prometheus'
ssh vaultaire-slc "curl -s localhost:9090/api/v1/rules | python3 -c 'import sys,json; print([g[\"name\"] for g in json.load(sys.stdin)[\"data\"][\"groups\"]])'"
```

## Post-merge review (2026-10-21, meta)

Adversarial pass over #812 and #813 against `docs/PAYMENTS.md` § 2.5, § 3.8,
§ 4.2, `internal/payments/CLAUDE.md` and the code at `e815506`. **Every claim
above holds; no P0/P1. One P2 (RP24-1) and one P3 (RP24-2) go to Prompt 21
Part 0; RP23-3 is closed by this review.** CI, Security and Deploy green on
every merge commit; the docs PR #815 (left `CONFLICTING` by the WP-CAP-1 plan
merge #814) rebased, its STATUS line folded with #814's sentence, merged;
prod `/version` = `5cb3a5770b9d` on slot 8000.

Verified in depth (beyond the session's own gates):

- **Part 0 (#812):** `payments.Readiness` is a typed three-state value whose
  zero is `Unready`; `transientPriceError` keeps the verdict only for a
  `*stripe.Error` with status 0, 5xx or 429 and for a non-Stripe error (a
  dial, a timeout) — a Stripe 4xx decides; `StartHouseVerification` retries
  an unreachable Stripe in 5 min without the "DISABLED" Error line;
  `ReadyLog.RecordReason` ignores `Pending`; `Registry.observe` exports 0 for
  it; `houseInvalid`'s UPDATE is guarded on `stripe_subscription_id IS NULL
  OR '' OR = $1`; `handleSubscriptionDeleted` evaluates `isHouse` before the
  flag-ON branch; the guard's `SET LOCAL lock_timeout` precedes `Lock` and
  `55P03` maps to `ErrProviderUnavailable`.
- **WP-PAY-3 (#813):** `GrantRecorded`'s `RETURNING` under the upsert's
  `WHERE NOT (…)` makes a replay `sql.ErrNoRows` → `Changed=false` with the
  floors still written (idempotent, as before); `AuditGranted` writes nothing
  for it. `ReadReceipt` / `HandleReceipt` take the tenant from the session
  and answer 404 for another tenant's id (proven in the note; the route is in
  the `RequireSession` + CSRF group). The admin dashboard forms sit in the
  group with `RequireAdmin`, CSRF and `RequireAdminMFA`; the admin API routes
  are each wrapped in `requireAdmin`. `RecordRefund` reads the charge `FOR
  UPDATE` under the entitlement lock and sums settled refunds before it
  writes, so two concurrent refunds cannot exceed the charge. The lapse
  job's `clear` re-reads the row under the lock and only then revokes;
  `lapseSkippedCollector` reads `job_runs.result` (no series before the first
  run; the rule's `min_over_time(…[30h])` is two daily runs). Every new
  counter pair is pre-created at 0 (`vaultaire_payment_reminders_total` 4 × 2,
  `vaultaire_payment_webhook_total` 4 outcomes) — the RP14-9 lesson holds.
  The readiness row is written on a state change and hourly with `GREATEST`
  on both timestamps and the last reason kept across a ready write. The
  mailer is wired after both the webhook handler (`server.go:569`) and the
  lapse runner (`:409`) exist (`:621`), so neither reference is nil.
- **Rules:** `promtool test rules` passes for `vaultaire-payments_test.yml` and
  `vaultaire-jobs_test.yml` on the box's promtool 2.53 once the test files sit
  beside the rule files (the first run from a `tests/` subdirectory read the
  OLD installed `vaultaire-jobs.yml` and failed on `payment_reminders` — a
  path trap, not a rule fault). **Installed by this review (RP23-3 closed):**
  `/etc/prometheus/rules/vaultaire-payments.yml` `e1ed3ac4…` and
  `vaultaire-jobs.yml` `a73e3494…` (= `main`), the previous jobs file kept as
  `vaultaire-jobs.yml.bak-20261020c`, `promtool check rules` SUCCESS (5 + 9
  rules), `systemctl reload prometheus`, the `vaultaire-payments` group is in
  `/api/v1/rules`.
- **Prod (read-only, 00:05 UTC 2026-10-21):** 0 `"level":"error"` lines on
  slot 8000 since the #813 switch (2 Warn); `payment_provider_readiness` =
  one row `stripe | last_unready_at 23:05:01 | "STRIPE_SECRET_KEY is not set"`;
  `entitlements` 0, `payments` 0; `entitlement_lapse` last `ok` 22:33
  (`skipped_unready 0`), `payment_reminders` last `ok` 23:08 (`candidates 0`);
  every payment series at 0; `STORAGE_MODE=idrive` (#811); `/health` healthy;
  1 alert (`RoutingTruthNoVerdict{idrive}`, the 05:30 run not yet due on the
  revived account). `feature_flags` has **4** rows, not 3: `parallel_get` for
  the sync-lab tenant beside `sync_backend` and the two `vault_parity` rows.

### Findings (RP24-*)

| # | Sev | Finding | Where it goes |
|---|---|---|---|
| RP24-1 | **P2** | **Renew on a prepaid house opens a Stripe subscription that starts today.** `HandleRenewHouse` → `Registry.StartCheckout(purpose=renew)` → `StripeService.StartCheckout` → `houseCheckoutParams`: `SubscriptionData` carries metadata only — no `TrialEnd` — so Checkout bills the first period at once and the subscription's anniversary is today. `applyHouse` / `recordInvoicePaid` then `Grant` with `paid_through = GREATEST(row, current_period_end)`. D-P20-A5 shows "Add a year" on ANY granted prepaid row: a house prepaid to 2027-10-20 renewed on 2026-11-01 ends up paid through 2027-11-01 — a year's price for 12 days, the prepaid remainder forfeited, and the house auto-renews from 2026-11-01 for ever. The design routes "add a year" to a PREPAID provider's checkout (§ 3.8) and makes a card on a prepaid date a *switch* with `subscription_data.trial_end = paid_through` (§ 4.2; Stripe's ≥ 48 h minimum, verified in stripe-go v75 on 2026-10-18). Second half: `recordInvoicePaid` records `inv.AmountPaid` with no zero guard, so the $0 `subscription_create` invoice of a trialing subscription would take a gapless receipt number and mail a $0 receipt. Fix: a Stripe checkout for a GRANTED row (purpose `renew`) sets `TrialEnd = paid_through` when that is ≥ 48 h away (else starts now and the page says the remaining days are forfeited — the design's sentence); `payments.Checkout` carries the date (`StartAt`) so the interface stays provider-neutral; the button shows only inside the reminder window (≤ 14 d before the date, in grace, lapsed) — before that "Add a year" by card is WP-PAY-5's switch; a $0 invoice is recorded without a receipt number and sends no receipt. | Prompt 21 Part 0 |
| RP24-2 | P3 | `PaymentRemindersRunner` reads no flag: with `entitlement_lapse` OFF (prod's default, per tenant) the `t+1d` ("in grace, N days left") and `last-day` ("your house ends tomorrow") mails announce an ending the lapse job only counts as `would_clear`. The two before-steps are true either way. Fix: the grace steps are sent only where the lapse job would act (`flags.Enabled(entitlement_lapse, tenant)`); a skipped step is counted in the result. | Prompt 21 Part 0 |
| RP24-3 | P4 | `RecordManualPayment`'s Extend branch builds `Granted{Provider: manual, Renewal: prepaid}` for the audit while the row keeps its own provider and renewal (a wire recorded on a Stripe `auto` row audits `house_granted` as manual/prepaid); `RecordRefund` counts `CountPayment(ProviderManual, KindRefund, …)` for a refund of a Stripe charge. Report the row's values; label by the charge's provider. | Prompt 21 Part 0 |
| RP24-4 | P4 | `RecordRefund` shortens `paid_through` with its own UPDATE — `internal/payments/CLAUDE.md` says it "calls" `AdjustEntitlement`; it does not, and no `billing.paid_through_adjusted` row is written for the shortening (the `billing.refund_recorded` row carries `shortened_by`). Fix the sentence or route through `AdjustEntitlement`. | Prompt 21 Part 0 (docs) |
| RP24-5 | P4 | `HandleRenewHouse` takes any `provider` form value and hands it to `Registry.For`: an unknown name is `ErrUnknownProvider`, logged at Error with a user-chosen string. Validate against `svc.Enabled` first. | Prompt 21 Part 0 |
| RP24-6 | note | The receipt mail is sent inside the webhook's answer (`Detached`, ≤ 15 s) and the lapsed mail inside the lapse loop; with `LogSender` instant, with a slow mail provider a webhook could outlive Stripe's wait and be redelivered (the dedup makes that harmless). Fine until a real sender is wired; then consider the delivery pool. | note |
| RP24-7 | note | The note's "feature_flags 3 rows" is 4 (`parallel_get` for the sync-lab tenant, #693). | — |
| RP23-3 | closed | `vaultaire-payments.yml` + `vaultaire-jobs.yml` installed on the box by this review (hashes above). | — |
