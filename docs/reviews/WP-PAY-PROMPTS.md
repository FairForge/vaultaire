# WP-PAY — worker prompt drafts and the owner click-list

Drafted 2026-10-18 with the design (`docs/PAYMENTS.md`) and the plan section
(`docs/IMPLEMENTATION_PLAN.md` § WP-PAY). Each prompt is issued by the plan
driver when its turn comes, after re-reading the files it cites (line numbers
below are from `main` @ `c9ce46c` and will have moved). Written so that each
can run even if the one before it has not (the Part 0 of each re-verifies).

## Owner click-list ([YOU])

Nothing below is code. Items 1–4 are needed before any crypto flag flips;
item 5 is the standing Stripe list with two additions; 6–7 are counsel.

1. **Decide F-22 … F-30** (plan decision table): BTCPay yes/no, where it runs,
   coins, which accounts, grace, Stripe default, refund policy, delete the
   metered tier, tax posture after counsel. Recommendations are in each row.
2. **The BTCPay box (F-23):** rent a 4 vCPU / 8 GB / 160 GB NVMe VPS (Hetzner
   CPX31/CX32 class or equal); Ubuntu LTS; `pay.stored.cloud` A/AAAA records
   **DNS-only** in the stored.cloud zone; install per
   `https://docs.btcpayserver.org/Docker/` with `BTCPAYGEN_CRYPTO1=btc`,
   `NBITCOIN_NETWORK=mainnet`, `BTCPAYGEN_ADDITIONAL_FRAGMENTS=opt-save-storage-s`,
   `BTCPAYGEN_LIGHTNING=none`, `BTCPAY_HOST=pay.stored.cloud`; wait for the
   initial block download (a day or two); register the server, create the
   store "Stored", **disable registration** afterwards; turn on 2FA on the
   BTCPay admin. Schedule `btcpay-backup.sh` nightly and ship the archive to
   the same Sync leg `pg-backup.sh` uses (the worker writes the script; you
   place the Sync credentials on that box through a 0600 file).
3. **The wallet:** on a hardware wallet you hold, create a new BIP84 account
   for Stored; in the BTCPay store → Wallets → BTC → "Connect an existing
   wallet" → import the **account xpub** (watch-only; never the seed, never a
   private key); confirm the first address on the device matches BTCPay's.
   Write the seed to steel (two copies), and the xpub + derivation path
   `m/84'/0'/0'` into the F-14 escrow envelope. Set the store's
   "Invoice expires after" to 30 min, "Payment invalid after" 1440 min,
   "Consider the invoice settled" at 1 confirmation (the code overrides per
   invoice), payment tolerance 0 %.
4. **Keys into prod:** in the store → Settings → Webhooks → create one for
   `https://stored.cloud/webhook/btcpay` (or `https://stored.ge/…` until
   WP-DOMAIN-1 lands), all invoice events, automatic redelivery on; copy the
   secret. Account → API keys → create a key with permissions
   `btcpay.store.cancreateinvoice`, `btcpay.store.canviewinvoices`,
   `btcpay.store.canmodifyinvoices` for **that store only**. Put
   `BTCPAY_URL=https://pay.stored.cloud`, `BTCPAY_STORE_ID=…`,
   `BTCPAY_API_KEY=…`, `BTCPAY_WEBHOOK_SECRET=…` into
   `/opt/vaultaire/configs/.env` the 0600 way (write the lines locally, scp,
   `sudo install -m 600`, `sudo vaultaire-switch restart`; never through a
   tool result or a command line) — or as systemd credentials when the F-14
   credential path is in place. Flip `pay_btcpay` for your own tenant first.
5. **Stripe (the R10 list, unchanged, plus two):** endpoint pinned to API
   version 2023-08-16 with the six event types, `STRIPE_SECRET_KEY` and
   `STRIPE_WEBHOOK_SECRET` together, the six `STRIPE_PRICE_*`, Customer
   Portal: quantity updates **off**, cancellation allowed, **reactivation
   off** (new — it is the one way a customer defeats the switch guard);
   Billing → Manage failed payments → cancel after retries **within 14 days**
   (new — the retry ladder must end inside our grace, F-26). Optional: turn on
   the stablecoin payment method if you want USDC through Stripe.
6. **Counsel / accountant:** the eight questions in `docs/PAYMENTS.md` § 6.
   Until Q1, Q2 and Q4 are answered, `pay_btcpay` stays on the canary tenant
   only (F-30).
7. **Terms and privacy policy:** the refund policy (F-28), "no chargebacks on
   crypto payments", payment records kept 7 years and anonymised at erasure,
   `payment_events` 90 days — the worker drafts the sentences in WP-PAY-3, you
   approve them.

## Shared preamble (paste at the top of every prompt)

```
Ground rules (every Stage 1a prompt)
- Commits: never a Co-Authored-By line naming Claude or AI. A Claude-Session: trailer is fine.
  Commit format: type(scope): description [WP-PAY-N].
- Merging: one PR through the merge queue: gh pr merge --auto, never --admin or --squash.
  Docs-only pushes don't deploy. Rebase (gh pr update-branch) when BEHIND.
- Prod is read-only: journalctl -u 'vaultaire@*' and grep "level":"error", never -p err;
  keys read only inside 0600 scripts and never printed.
- Databases: never the shared vaultaire dev DB. make test-db TEST_DB=vaultaire_<topic> and
  DATABASE_URL=postgres://viera@localhost:5432/vaultaire_<topic>?sslmode=disable for every
  go test; one go test process per DB at a time. Run make test-db after every pulled migration.
- TDD: red first, at the handlers; Arrange/Act/Assert; require for fatal, assert otherwise.
- Framing: build now, dark behind flags; never "wait for launch".
- Cite docs/ARCHITECTURE_CONTRACT.md (line 12: metadata has clocks; line 13: the tenant is
  the unit of disclosure) and docs/surfaces/DEVELOPER.md in the PR body. Read docs/PAYMENTS.md
  first; it is the design of record and the PR must say where it departs from it.
- Update the per-directory CLAUDE.md of every directory touched; a build note
  docs/reviews/WP-PAY-N.md with Decisions / What was built / Before → after / Local live
  proof / Prod proof / Residuals; flip the row in the plan's WP-PAY table (docs-only commit
  at the end); docs/STATUS.md names the next migration.
```

---

## Prompt WP-PAY-0 — one product: delete the legacy packs and the metered reporter (= WP-R10-4)

```
Stage 1a, WP-PAY-0 (= WP-R10-4, decision F-29): one product — delete the pack path and the metered reporter.

Continue from: WP-PAY planning merged (docs/PAYMENTS.md). Most recent code PR on main: check
gh pr list --state merged --limit 5. No migration. One PR. Lane: Developer surface.

Part 0 — verify, cite file:line
1. registerStripePlans (internal/api/server.go ~1099) registers five STRIPE_PRICE_VAULT*/STANDARD
   packs; HandleUpgrade (dashboard/handlers/billing.go ~66) sells them through
   StripeService.CreateCheckoutSession (billing/stripe.go ~130). Count prod tenants on a pack
   plan: tenants.plan NOT IN ('starter','house') — expected 0 (read-only, 0600 script).
2. MeteredReporter (billing/metered.go) is constructed only when STRIPE_METER_STORAGE and
   STRIPE_METER_EGRESS are set (server.go ~554); prod has neither. AccruedCents is called from
   admin_costs.go ~242, billing.go ~246, admin_revenue.go ~281/356/394.
3. HouseFromSubscription (billing/house.go ~497) returns false for a pack subscription and
   the dashboard's liveOther path (billing_house.go ~124-160) sends such a tenant to support.
4. CancelSubscription (stripe.go ~189) is used by the deletion runner for ANY subscription —
   it stays.

What to build
1. Delete: registerStripePlans + the Plan registry (RegisterPlan/GetPlan/Plans),
   CreateCheckoutSession (pack), HandleUpgrade and its route, MeteredReporter + metered.go +
   its tests, STRIPE_METER_* wiring, SetEmailSender on the reporter, AccruedCents /
   MeteredRatePerTB and every caller (the "≈ $X this month" estimate on the billing page and
   the metered branches of admin revenue/costs — those pages show houses only, priced by
   billing.QuoteHouse at the tenant's house_period).
2. Keep: HandleManageBilling (the portal), CancelSubscription, everything house.
3. A tenant whose tenants.plan is a pack id (none on prod) is shown the liveOther message.
4. CLAUDE.md of internal/billing and the root CLAUDE.md env table: remove STRIPE_PRICE_VAULT3…,
   STRIPE_METER_*; docs/CONFIG.md the same. spending_cap_cents and metered_usage_reports stay
   in the schema until WP-PAY-7 (D-12) — say so in the build note.

Tests (red first)
- TestNoMeteredSymbols: a structural test that the billing package exports no
  MeteredReporter / AccruedCents (reflect or `go doc` parse) — or simply the build after deletion.
- TestBillingPage_NoAccruedEstimate: the customer billing page renders without the estimate.
- TestAdminRevenue_HousesOnly: a house tenant and a free tenant → MRR = QuoteHouse only.
- TestServer_BootWithoutMeterEnv: no "metered billing dormant" log line; no reporter started.
- Every existing house/webhook test stays green.

Key files
- internal/api/server.go:521-560 (Stripe wiring), :1096-1110 (registerStripePlans)
- internal/billing/{metered.go,metered_test.go,stripe.go:61-80,130-158}
- internal/dashboard/handlers/{billing.go:42-116,246; admin_revenue.go:176-240,281,356,394; admin_costs.go:242}
- internal/dashboard/router.go (the /dashboard/billing/upgrade route)

Architecture notes
- This is the Stripe-shaped half of the surface WP-PAY-2's provider interface would otherwise
  carry; deleting first is the point (F-29). Nothing customer-visible changes on prod.
- Prod proof: deploy; /dashboard/billing and /admin/revenue render; boot log has no metered line.
- Rollback: revert the PR.
```

---

## Prompt WP-PAY-1 — the entitlement record (migration 093)

```
Stage 1a, WP-PAY-1: the entitlement record — paid_through in our DB, a payments ledger, the lapse job.

Continue from WP-PAY-0 (merged as #NNN) — if it has not merged, build on main anyway; nothing
here depends on it. Lane: Developer surface. Migration: 093 (confirm with
ls internal/database/migrations | tail -1 and gh pr list --state open — another PR may have
claimed it; take the next free number and say so). One PR, two commits (migration + package;
webhook + job).

Read first: docs/PAYMENTS.md § 1 (the record), § 2.4 (claim contract), § 8 (what is provable);
plan § WP-PAY row 1; internal/billing/CLAUDE.md "Webhook delivery contract";
internal/usage/CLAUDE.md (lock order: the total before the floors).

Part 0 — verify, cite file:line
1. applyHouse (billing/webhook.go ~441-490) grants on active/trialing/past_due and clears on
   canceled/unpaid/incomplete_expired/paused; isCurrentSubscription ~490.
2. No code writes subscriptions.current_period_end (018) or billing_charges (019).
3. SetHouse (usage/floor.go ~92) opens its own transaction; ClearHouse ~131 locks
   tenant_quotas FOR UPDATE before deleting floors (deadlock lesson, Prompt 7 Part 0).
4. The jobs scheduler: jobSpec (api/jobs.go ~98-114) with Hour/Minute for a daily job;
   registration in api/jobs_wiring.go; job_runs.result holds a job's structured result.
5. account.EraseRows (account/erase.go) and the export sections
   (api/account_export_sections.go) are schema-walked by TestErasedTablesCoverSchema and
   TestExportSectionsCoverSchema — a new table fails both until listed.
6. Flags: api/flags_wiring.go ~79-88 registers every flag with its default.
7. Prod (read-only): tenants with stripe_subscription_id, tenant_floor_quotas rows — expected
   0 and 0 (2026-10-18: 0/0), so the backfill is a no-op.

What to build
1. Migration 093_payments.sql exactly as docs/PAYMENTS.md § 1.2 (entitlements, payments,
   payment_events, invoice_counters; CHECKs; the partial index on paid_through). Additive.
2. Package internal/payments:
   - Entitlement, Order (= billing.HouseOrder's shape; billing may alias it), Renewal.
   - func Lock(ctx, tx, tenantID) — pg_advisory_xact_lock(hashtextextended('entitlement:'||$1,0)).
   - func Grant(ctx, tx, tenantID, Order, paidThrough time.Time, provider, providerRef string,
     renewal Renewal) error — upsert entitlements (lapsed_at = NULL) + usage.SetHouseTx in the
     same tx; idempotent.
   - func Extend(ctx, tx, tenantID, by Period) (newPaidThrough, error) — GREATEST(paid_through,
     now()) + 1 month | 1 year (Postgres interval arithmetic; document the month-end rule:
     '2026-01-31' + 1 month = '2026-02-28', accepted).
   - func StopRenewal(ctx, tx, tenantID) — renewal = 'prepaid'.
   - Ledger: RecordPending / Settle / Expire / Invalidate / Credit on payments rows, keyed by
     (provider, provider_id); Settle assigns invoice_number from invoice_counters under
     FOR UPDATE (gapless per year).
   - Status(ctx, db, tenantID) — the read model the dashboard will use in WP-PAY-2.
   - Grace: ENTITLEMENT_GRACE env (Go duration, default 14d = 336h; 1h–60d; a rejected value
     is logged at Warn and the default kept — the house rule for knobs).
3. usage.SetHouseTx(ctx, tx, tenantID, House) = the body of SetHouse on a caller's tx;
   SetHouse calls it.
4. The Stripe webhook dual-writes: checkout.session.completed and
   customer.subscription.{created,updated} with active/trialing → Grant(items,
   paid_through = sub.CurrentPeriodEnd, 'stripe', sub.ID, auto) inside applyHouse's path (the
   claim transaction is held; Grant takes a tx — pass the claim tx or open a second one and
   document the choice; the per-customer advisory lock already serialises the customer).
   invoice.payment_succeeded → ledger row settled (amount_paid, period from the invoice
   lines' period) + Extend/Grant to current_period_end. Terminal statuses KEEP clearing as
   today (that branch moves behind the flag in WP-PAY-2) but also set renewal='prepaid'.
   invoice.payment_failed → a ledger row status 'failed' (add the value to the status set).
5. The entitlement_lapse job (api/entitlement_lapse.go): jobSpec{Name:"entitlement_lapse",
   Hour:7, Minute:30, MaxRunTime: 10m}. Each run: (a) backfill — tenants with floor rows and no
   entitlement row get one (provider 'stripe' when tenants.stripe_subscription_id is set,
   paid_through = the fetched current_period_end when Stripe is configured, else now()+grace
   with a Warn; else provider 'manual'); (b) candidates = lapsed_at IS NULL AND paid_through
   + grace < now(); skip a row whose provider was unready at any point in the last 24 h (a
   readiness history: payments.ReadyHistory, fed by WP-PAY-2's providers; until then every
   provider counts as ready — say so); with flag entitlement_lapse OFF only count
   (result.would_clear, one Info line per run); ON: ClearHouse, lapsed_at = now(), audit
   billing.house_lapsed {tenant, paid_through, provider}; (c) reconcile: every granted row's
   floors must equal its house; heal with SetHouseTx and one Error line per healed tenant.
   Result JSON: {backfilled, candidates, would_clear, cleared, skipped_unready, healed}.
   Register in jobs_wiring.go; admin GET /api/v1/admin/jobs lists it; POST …/run works.
6. Flag entitlement_lapse registered default false.
7. EraseRows: entitlements DELETE; payments UPDATE SET tenant_id = NULL, metadata =
   metadata - 'addresses' - 'txids' (define the metadata keys now: provider ids under
   metadata.ids, addresses under metadata.addresses, txids under metadata.txids), note = NULL;
   payment_events UPDATE tenant_id = NULL; invoice_counters is infrastructure (not tenant).
   Export: entitlements and payments exported (keyset paged, no secret column);
   payment_events listed as "payment-provider event bodies (90-day retention)";
   invoice_counters "infrastructure". Retention job: payment_events 90 d; payments never
   (accounting record) — add the sentence to the privacy policy + DPA text in the same PR
   (docs/legal or the landing sources — find where retention.go's periods are stated).
8. Metrics: vaultaire_entitlements_expiring{window} (read from the table on scrape),
   vaultaire_entitlements_lapsed_total, vaultaire_payments_total{provider,kind,outcome}.

Tests (red first, own test DB)
- TestGrant_Idempotent / TestGrant_ExtendsFromMaxOfPaidThroughAndNow (running vs lapsed row)
- TestExtend_MonthEndRule
- TestSettle_InvoiceNumbersGaplessUnderConcurrency (10 goroutines, one year, 1..10)
- TestStripeWebhook_WritesEntitlement_PaidThroughIsCurrentPeriodEnd (signed fixture, fetched
  sub with CurrentPeriodEnd)
- TestStripeWebhook_PaymentSucceeded_LedgerRowAndExtend
- TestStripeWebhook_Canceled_StillClearsWhileFlagOff_SetsPrepaid
- TestLapseJob_FlagOff_CountsOnly / TestLapseJob_ClearsPastGrace_UsedBytesUntouched /
  TestLapseJob_SkipsUnreadyProvider / TestLapseJob_ReconcileHealsFloors /
  TestLapseJob_Backfill_FromFloorRows
- TestSQLLiteralsMatchSchema, TestErasedTablesCoverSchema, TestExportSectionsCoverSchema green
  with the four tables; TestEraseRows_PaymentsAnonymisedNotDeleted
- The job must only touch its own tenant's rows in tests (shared DB): scope with an onlyTenants
  hook as the deletion runner's onlyDue does.

Key files
- internal/billing/webhook.go:243-290 (checkout), 303-375 (invoices), 441-490 (applyHouse)
- internal/usage/floor.go:92 (SetHouse), 131 (ClearHouse); internal/usage/ledger_tx.go (lock order)
- internal/api/jobs.go:98-114 (jobSpec), jobs_wiring.go:60-145 (registration pattern),
  deletion_runner.go (onlyDue scoping), retention.go:79 (periods table)
- internal/account/erase.go:118-265; internal/api/account_export_sections.go:60-95, 371-378
- internal/api/flags_wiring.go:79-88

Architecture notes
- Two-slot: the old build ignores the new tables; the new build writes both. Nothing dropped.
- The lapse job is the ONLY writer of lapsed_at; the webhook never clears a row the job owns
  once WP-PAY-2 flips the branch. Billing never deletes data: ClearHouse is unchanged.
- Live proof: /verify on a private migrated DB; the full suite via DATABASE_URL on a private
  DB. Prod proof: migration applied by the deploy, 0 rows, entitlement_lapse run ok with
  would_clear=0 (admin POST …/run), the three gauges present on /metrics. Report red/green.
- Rollback: the flag (default OFF); the tables are additive.
ultrathink
```

---

## Prompt WP-PAY-2 — the provider interface, the dashboard off stripe-go, provider flags

```
Stage 1a, WP-PAY-2: payments.Provider — Stripe becomes one provider; the dashboard stops importing stripe-go; pay_stripe / pay_btcpay flags; provider-off semantics.

Continue from WP-PAY-1 (merged as #NNN, migration 093 deployed, entitlement_lapse flag OFF).
Lane: Developer surface. No migration. One PR (two commits: interface + Stripe adapter;
dashboard + runner + flags).

Read first: docs/PAYMENTS.md § 2 (interface, switches, double-billing guard), § 1.4 (Stripe
under the model); internal/billing/CLAUDE.md; internal/dashboard/CLAUDE.md.

Part 0 — verify, cite file:line
1. HouseBilling (dashboard/handlers/billing_house.go ~32-39) returns *stripe.Subscription;
   HandleManageBilling (billing.go ~120) takes *billing.StripeService; Deps.Stripe
   (dashboard/router.go ~48, ~320, ~714). HandleUpgrade is gone (WP-PAY-0) — if not, delete it here.
2. Customer creation sites: api/server.go ~1065 and dashboard/router.go ~714 (registration),
   billing_house.go ~290 (lazy). HasLiveSubscription ~house.go:456 is the Stripe R10-05 guard.
3. deletion_runner.cancelSubscription (~509-545) defers when r.Stripe == nil and a
   subscription id is on the row.
4. flags_wiring.go registers flags with defaults; flags.Service.Enabled(key, tenant).
5. go list -deps ./internal/dashboard/... | grep stripe-go → non-empty today (the red test).

What to build
1. internal/payments/provider.go: the Provider interface exactly as docs/PAYMENTS.md § 2.2;
   Registry{Register(p), For(ctx, tenantID, name) (Provider, error)} returning
   ErrProviderDisabled when flag pay_<name> is off for the tenant and ErrProviderUnavailable
   when Ready() is false; Ready history recorded every 60 s into payments.ReadyHistory (the
   lapse job reads it); gauge vaultaire_payment_provider_ready{provider}.
2. billing.StripeService implements Provider: Name "stripe"; Renewal auto; Ready = key set ∧
   HouseCheckoutReady; StartCheckout = GetCustomerID-or-CreateCustomer (lazy, the ONLY
   creation site) + HasLiveSubscription + CreateHouseCheckout (purpose in metadata);
   Resize = UpdateHouseSubscription; StopRenewal/ResumeRenewal = cancel_at_period_end
   true/false (new; Subscription.Update); Cancel = CancelSubscription; ManageURL =
   CreateBillingPortalSession; Webhook = the WebhookHandler.
3. Delete the two registration-time CreateCustomer calls (server.go, router.go). A tenant
   gets a Stripe customer when it first buys.
4. Dashboard: HouseBilling → an interface over payments (Status(ctx, tenant) payments.Status;
   StartCheckout; Resize; ManageURL); the house page renders from payments.Status (house,
   paid_through, renewal, provider, lapsed); HandleManageBilling takes payments.Provider;
   Deps.Stripe → Deps.Payments *payments.Registry. Structural test
   TestDashboardImportsNoStripe.
5. Provider-off semantics (§ 2.3): checkout/resize POST → 409 provider_disabled with the
   page message; the webhook route is mounted whenever the secrets are set (flag never
   unmounts); the billing page lists providers that are on; the deletion runner calls
   Provider.Cancel for the row's provider and defers ONLY when renewal='auto' and the
   provider is unconfigured (a prepaid row has nothing to cancel).
6. With entitlement_lapse ON: applyHouse's terminal branch (canceled/unpaid/incomplete_expired/
   paused, and customer.subscription.deleted) sets renewal='prepaid' and does NOT ClearHouse
   (the date and the lapse job decide); with the flag OFF today's behaviour. past_due → no
   change either way (it already was a grant; now it is simply nothing).
7. Flags: pay_stripe default = STRIPE_SECRET_KEY set; pay_btcpay default false (registered
   now so the admin page shows it; no provider behind it yet).
8. The double-billing guard (§ 2.5): payments.StartCheckout(ctx, registry, tenant, provider,
   Checkout) — under Lock: refuse 409 other_provider_live when the row's provider ≠ this one
   and renewal='auto'; then the provider's own StartCheckout.

Tests (red first)
- TestDashboardImportsNoStripe (go list -deps, or a go/packages walk)
- TestRegister_CreatesNoStripeCustomer (both registration paths)
- TestCheckout_LazyCustomer_CreatedOnce
- TestCheckout_ProviderFlagOff_409 / TestResize_ProviderFlagOff_409
- TestWebhookRoute_MountedWhileFlagOff (secrets set, flag off → 200 on a signed no-op event)
- TestCheckout_OtherProviderLive_409 (row provider 'btcpay' renewal 'auto' is impossible, so
  seed provider 'stripe' renewal 'auto' and start a 'manual'/'fake' provider checkout)
- TestApplyHouse_TerminalStatus_FlagOn_SetsPrepaidKeepsHouse /
  TestApplyHouse_TerminalStatus_FlagOff_Clears
- TestDeletionRunner_PrepaidRow_NoCancelNeeded / TestDeletionRunner_AutoRowUnconfigured_Defers
- TestStripeProvider_StopRenewal_SetsCancelAtPeriodEnd (params assertion, as house_test does)

Key files
- internal/dashboard/handlers/billing_house.go:31-60, 120-200, 208-330; billing.go:118-160
- internal/dashboard/router.go:48, 310-330, 708-726
- internal/billing/stripe.go:84-130, 160-230, 276-300; house.go:456-600; webhook.go:441-521
- internal/api/server.go:520-560, 815-830, 855-870, 1060-1070; deletion_runner.go:505-545
- internal/api/flags_wiring.go:79-88; internal/flags/service.go:92-120

Architecture notes
- The import direction is the deliverable: payments ← billing (stripe), dashboard → payments.
  Moving internal/billing under internal/payments/stripe is WP-PAY-7's cosmetic step.
- Nothing on prod has a Stripe key: every behaviour change ships dark. Live proof: Stripe test
  mode (owner's test keys, stripe listen) in a lab + the signed fixtures; prod proof: deploy,
  register a tenant → the log shows no Stripe call; /dashboard/billing renders the free-tier
  sentence; /admin/flags shows pay_stripe / pay_btcpay.
- Rollback: flags; the webhook branch is behind entitlement_lapse.
ultrathink
```

---

## Prompt WP-PAY-3 — receipts, reminders, the billing page, admin tooling, metrics and rules

```
Stage 1a, WP-PAY-3: what a prepaid customer sees and what the operator can do by hand.

Continue from WP-PAY-2 (merged as #NNN). Lane: Developer surface. No migration
(invoice_counters is in 093). One PR, three commits (receipts + reminders; billing page;
admin + metrics + rules).

Read first: docs/PAYMENTS.md § 3.8 (renewal UX, receipts, retention), § 7 (metrics, audit,
admin), internal/email/CLAUDE.md (prod sends through Cloudflare Email Service SMTP),
internal/audit/audit.go (Entry), deploy/monitoring/README.md.

Part 0 — verify, cite file:line
1. email.Sender (email/email.go ~21) and the templates dir; which flows send today.
2. audit.Record(ctx, db, Entry) and the Action naming (event_type = the part before the dot).
3. The dashboard tenant detail template (dashboard/templates/admin/tenant_detail.html) and the
   customer billing template (templates/customer/billing.html).
4. Prometheus rule files are installed by hand on the box (deploy/monitoring/README.md); the
   generic JobStale rule exists in vaultaire-jobs.yml.

What to build
1. Receipts: payments.Settle already numbers; add the receipt page
   GET /dashboard/billing/receipts/{id} (print-ready HTML: issuer from internal/hosts legal
   data, tenant e-mail, the house lines from the snapshot, USD amount, crypto amount + rate +
   provider invoice id when present, paid date, invoice_number); the receipts list on the
   billing page; receipt e-mail payment_receipt.html on every settle.
2. Reminders: job payment_reminders (hourly; MaxRunTime 5m) for renewal='prepaid' rows with
   lapsed_at IS NULL: steps T-14d, T-3d, T+1d, T+(grace-1)d; each step once per period
   (entitlements.reminders_sent JSONB keyed by step and paid_through — add the column in a
   small additive migration if 093 lacks it, else a table; say which); templates
   renewal_reminder.html (step-aware copy) and house_lapsed.html (sent by the lapse job).
   Metric vaultaire_payment_reminders_total{step,outcome}.
3. Billing page: the entitlement sentence ("Your house is paid through <date>; it renews
   automatically with Stripe" / "…; nobody will renew it — renew below" / "…; in grace until
   <date>"), buttons Renew now / Add a year (StartCheckout purpose=renew on the row's provider,
   or any provider that is on when the row is prepaid), Resize (existing), receipts.
4. Admin (admin JWT, chi route group /api/v1/admin/tenants/{id}): GET payments (ledger +
   entitlement), POST payments {amount_cents, period, purpose:'manual', note} → ledger row
   provider 'manual' settled + Grant/Extend, audit billing.payment_recorded_manual; POST
   entitlement {paid_through?, order?, reason (required)} → audit billing.paid_through_adjusted;
   POST refunds {payment_id, amount_cents, provider_ref, shorten_paid_through bool, note} →
   ledger row kind 'refund' refund_of, audit billing.refund_recorded. The admin tenant page
   gets a Billing panel calling them (CSRF as the other admin forms).
5. Audit rows for every grant (billing.house_granted — in payments.Grant, so every provider
   gets it) and every lapse (already in WP-PAY-1; confirm).
6. Metrics (payments/metrics.go) as docs/PAYMENTS.md § 7; rules
   deploy/monitoring/vaultaire-payments.yml: PaymentProviderDown, PaymentWebhookFailing,
   DoublePeriodDetected (the metric exists from WP-PAY-5; define the rule now, it reads 0),
   EntitlementLapseSkippedUnready; promtool check rules in the test (the repo's pattern).
7. Privacy policy / terms sentences drafted (F-28 refund policy, no chargebacks, records 7 y,
   payment_events 90 d) as a separate commit for the owner's approval.

Tests (red first)
- TestReceiptPage_RendersFromLedger / TestReceiptPage_OtherTenant_404
- TestReminders_EachStepOnce / TestReminders_PrepaidOnly / TestReminders_NotAfterLapse
- TestBillingPage_Sentence (auto, prepaid, grace, lapsed, free)
- TestAdminManualPayment_GrantsAndAudits / TestAdminPaidThroughAdjust_RequiresReason /
  TestAdminRefund_ShortensWhenAsked / TestAdminPaymentsRoutes_AdminOnly
- TestPaymentsRules_Parse (promtool)
- The schema tests if a column is added.

Key files
- internal/email/{email.go:21,templates/}; internal/audit/audit.go:33-62,98
- internal/dashboard/templates/customer/billing.html; templates/admin/tenant_detail.html
- internal/api/server.go:1000 (the admin route group); admin_jobs.go (handler pattern)
- deploy/monitoring/vaultaire-jobs.yml (rule style), README.md (install steps)

Architecture notes
- Every admin money action is audited with actor, subject tenant and reason; none is possible
  without the reason field. The receipt is rendered from the ledger row, never stored twice.
- Prod proof: a manual payment on tenant zero (the owner's own tenant) grants a house, the
  receipt renders, the reminders job runs ok; the rule file installed on the box [OPS].
- Rollback: admin routes are admin-only; the reminders job has no effect on prepaid rows that
  do not exist on prod.
```

---

## Prompt WP-PAY-4 — the BTCPay provider (lab first)

```
Stage 1a, WP-PAY-4: BTCPay Server as the second provider — the Greenfield client, the webhook, the invoice state table. Dark behind pay_btcpay.

Continue from WP-PAY-3 (merged as #NNN). Lane: Developer surface. No migration. Two PRs:
(A) the lab spike's fixtures + the client + the webhook verifier; (B) the provider, the
state table, readiness, wiring.

Read first: docs/PAYMENTS.md § 3 entire (API, states, money, coins, hosting, UX), § 2.4-2.5;
the BTCPay Greenfield OpenAPI (https://docs.btcpayserver.org/API/Greenfield/v1/ and the
templates under BTCPayServer/wwwroot/swagger/v1/ in the btcpayserver repository):
swagger.template.webhooks.json, swagger.template.invoices.json, swagger.template.stores.json.

Part 0 — the lab spike (local only; delete everything after)
1. Start Docker (colima start). Clone btcpayserver-docker; generate a regtest compose with
   BTCPAYGEN_CRYPTO1=btc NBITCOIN_NETWORK=regtest BTCPAYGEN_REVERSEPROXY=none
   BTCPAYGEN_LIGHTNING=none (./build.sh), run it, create a store, a Greenfield API key with the
   three store permissions, a webhook to a local listener (ngrok-free: the listener runs in the
   same Docker network, or `docker compose exec` a curl sidecar).
2. Create invoices and drive every row of docs/PAYMENTS.md § 3.3 with bitcoin-cli on regtest
   (pay exact / under / over / late / never; mine 0, 1, 2 blocks; mark Settled/Invalid by hand;
   a redelivery). Capture every webhook delivery's raw body + BTCPay-Sig header into
   internal/payments/btcpay/testdata/<event>_<case>.json and the fetched invoice +
   payment-methods JSON beside it. Verify: the header is exactly sha256=<hex> of
   HMAC-SHA256(secret, body); the SpeedPolicy enum names (HighSpeed/MediumSpeed/
   LowMediumSpeed/LowSpeed) and their confirmation counts; the status/additionalStatus pairs
   seen; whether a Settled invoice ever changes state afterwards (a 1-block reorg on regtest
   via invalidateblock). Write the corrections into docs/PAYMENTS.md § 3.3 in PR (A).
3. Name in the build note exactly what ran, the versions, and that the containers, volumes and
   clone were deleted.

What to build (A)
1. internal/payments/btcpay/client.go: CreateInvoice, GetInvoice, GetPaymentMethods,
   MarkStatus, CreateRefund (the exact Greenfield shapes), API key auth header
   `Authorization: token <key>`, TunedHTTPClient-style timeouts (10 s), one retry on a
   fresh connection for GETs only.
2. webhook.go: VerifySig(secret, header, body) constant-time; body cap 1 MiB; event types
   parsed; dedup key COALESCE(originalDeliveryId, deliveryId); payments.ClaimEvent (provider
   'btcpay') held across the handler; tenant resolution = OUR pending ledger row for invoiceId
   (metadata.tenant_id must match, else 200 + Warn + counted foreign — never applied);
   fetch the invoice and payment-methods; apply the fetched status.

What to build (B)
3. provider.go implementing payments.Provider: Name "btcpay", Renewal prepaid; Ready = GET
   /api/v1/health + GET the store, every 60 s, false after two failures; StartCheckout =
   ledger row pending → CreateInvoice(amount − credit, USD, metadata, checkout{
   expirationMinutes 30, monitoringMinutes 1440, speedPolicy by amount (< $200 MediumSpeed,
   else LowMediumSpeed), redirectURL, redirectAutomatically}, receipt enabled) → write the
   invoice id onto the row → return checkoutLink; Resize = the top-up invoice (the pro-rata
   formula of § 3.5, printed in metadata); StopRenewal/ResumeRenewal/Cancel no-ops; ManageURL "".
4. states.go — the table of § 3.3 as code: Settled → Settle + Grant/Extend (+ credit on
   overPaid); Expired(partiallyPaid) → expired_partial + credit; ReceivedPayment
   afterExpiration with totalPaid ≥ due → MarkStatus(Settled) (audit billing.invoice_marked,
   reason paid_late_in_full); Processing/ReceivedPayment/PaymentSettled → status only;
   Invalid → invalid. Every transition idempotent on (invoice id, fetched status).
5. Wiring: env BTCPAY_URL, BTCPAY_STORE_ID, BTCPAY_API_KEY, BTCPAY_WEBHOOK_SECRET (all four or
   the provider is not registered — one Error line naming the missing ones); route
   POST /webhook/btcpay mounted only then; the registry's pay_btcpay flag gates checkout.
   CONFIG.md + root CLAUDE.md env table rows.
6. Metrics: vaultaire_payment_webhook_total{provider="btcpay",event,outcome},
   vaultaire_payments_total{provider="btcpay",…}, the readiness gauge.

Tests (red first; fixtures from the spike)
- TestBTCPaySig_Valid / _WrongSecret_400 / _MissingHeader_400 / _ConstantTime (no early return)
- TestBTCPayWebhook_Redelivery_IsDuplicate (originalDeliveryId)
- TestBTCPayWebhook_UnknownInvoice_200NoApply / _MetadataTenantMismatch_200NoApply
- TestBTCPayWebhook_FetchError_5xx / _Fetch404_200
- One test per § 3.3 row: Settled_Grants_ExtendsFromPaidThrough, Settled_OverPaid_Credit,
  ExpiredPartial_Credit_NoGrant, PaidLateInFull_MarksSettled, Processing_NoGrant,
  Invalid_NoGrant, Created_Noop
- TestBTCPay_StartCheckout_PendingRowBeforeInvoice / _SubtractsCredit / _SpeedPolicyByAmount
- TestBTCPay_Resize_TopUpAmount (table of cases incl. annual)
- TestBTCPay_Ready_FalseAfterTwoFailures_TrueAfterOne
- TestServer_BTCPayRouteAbsentWithoutSecrets

Key files
- internal/billing/webhook.go:80-203 (the claim shape to mirror), internal/payments/* (WP-PAY-1/2)
- internal/api/server.go:815-830 (webhook route mounting), backend_probes.go (probe cadence pattern)
- internal/drivers/transport.go (TunedHTTPClient)

Architecture notes
- Grant on Settled only; never on Processing. A reorg after Settled is accepted risk at the
  chosen confirmations (documented). Payment tolerance 0.
- The provider never holds a key: the store is watch-only (the owner's click-list).
- Nothing can be proven on prod until the F-23 box and the owner's items 2–4 exist: PR (B)
  ships dark (no secrets → no route; flag OFF). Then: the owner flips pay_btcpay for tenant
  zero and pays ONE mainnet invoice (small, own money) — the first real proof; the build note
  records invoice id, confirmations, time to Settled, and the ledger row.
- Rollback: flag OFF; unset the secrets to unmount the route.
ultrathink
```

---

## Prompt WP-PAY-5 — switching providers, both ways, per tenant and in bulk

```
Stage 1a, WP-PAY-5: switch a customer from Stripe to crypto and back without a gap or an overlap; the owner's bulk stop.

Continue from WP-PAY-4 (merged as #NNN; BTCPay dark). Lane: Developer surface. No migration
(pending_provider and next_* are in 093). One PR.

Read first: docs/PAYMENTS.md § 4 (the cutover), § 2.5 (the guard and the detector).

Part 0 — verify, cite file:line
1. stripe-go v75: CheckoutSessionSubscriptionDataParams has TrialEnd and BillingCycleAnchor
   (checkout_session.go ~1640, ~1660 in the module cache); SubscriptionParams has
   CancelAtPeriodEnd.
2. StopRenewal/ResumeRenewal exist on the Stripe provider (WP-PAY-2).
3. The webhook's customer.subscription.updated handler sees cancel_at_period_end on the
   fetched subscription.

What to build
1. payments.SwitchToPrepaid(ctx, tenant, to provider): under Lock — provider.StopRenewal,
   renewal='prepaid', pending_provider=to, audit billing.provider_switch_requested; the
   billing page button "Switch to crypto at renewal"; from T-14d the page and the reminders
   offer StartCheckout(purpose='switch') on the pending provider; Settled → provider=to,
   pending_provider cleared (payments.Grant already does GREATEST(paid_through, now()) so
   paying early extends from paid_through).
2. The portal race: on a fetched subscription with cancel_at_period_end=false for a row with
   renewal='prepaid' and provider='stripe' → renewal='auto', pending_provider cleared, every
   pending btcpay ledger row for the tenant MarkStatus(Invalid) (audit billing.invoice_marked
   reason portal_uncancel); if a btcpay row already settled covering dates Stripe also bills →
   the detector (3).
3. The double-period detector: in payments.Settle (every provider): if another provider's
   settled row for the tenant has period_end > this row's period_start →
   vaultaire_payments_double_period_total++, audit billing.double_period {both rows}, an
   operator e-mail (EMAIL_FROM → the support address), and the grant still happens (paid is
   paid; the refund is by hand).
4. SwitchToCard: StartCheckout on Stripe with subscription_data.trial_end = paid_through when
   ≥ 48 h away (and metadata purpose=switch); otherwise start now and say on the page that
   the remaining prepaid days are forfeited; on checkout.session.completed with a trialing
   subscription → provider='stripe', renewal='auto', paid_through unchanged (= trial end).
5. Bulk: POST /api/v1/admin/payments/stripe/stop-renewals {dry_run: true default} → lists every
   renewal='auto' provider='stripe' row with paid_through; dry_run=false → StopRenewal each,
   renewal='prepaid', one audit row each (reason bulk, actor), one e-mail each (template
   renewal_stopped.html); POST …/resume-renewals the reverse for rows whose paid_through is in
   the future and pending_provider IS NULL. 202 + a job_runs-style result; 409 while running.
6. The billing page copy for every state; the reminders include the switch offer when
   pending_provider is set.

Tests (red first)
- TestSwitchToCrypto_StopsRenewal_KeepsPaidThrough / _EarlyPayment_ExtendsFromPaidThrough /
  _NoPayment_GraceThenLapse
- TestPortalUncancel_RestoresAuto_InvalidatesPendingInvoice
- TestDoublePeriod_Detected_Audited_StillGranted
- TestSwitchToCard_TrialEndIsPaidThrough / _Inside48h_StartsNow
- TestBulkStop_DryRun_WritesNothing / _PerTenantAuditAndMail / TestBulkResume_OnlyFutureUnpending
- TestBulk_409WhileRunning

Key files
- internal/billing/house.go:425-600 (checkout params, UpdateHouseSubscription), webhook.go:463-521
- internal/payments/{entitlement.go,switch.go (new)}, btcpay/provider.go (MarkStatus)
- internal/api/admin_payments.go (WP-PAY-3), admin_jobs.go (the 202/409 pattern)

Architecture notes
- No gap, no overlap: paid_through is never shortened by a switch; Extend appends. The only
  overlap possible is the portal race, detected and refunded by hand; the click-list turns
  portal reactivation off.
- Lab: Stripe test mode + BTCPay regtest together (the WP-PAY-4 lab stack); prod proof: the
  bulk dry run on 0 subscribers answers an empty list (admin JWT, read-only).
- Rollback: flags; the bulk action is dry-run by default and never cancels a period.
ultrathink
```

---

## Prompt WP-PAY-7 — the contract half (admin pages on the ledger, D-12 drops)

```
Stage 1a, WP-PAY-7: readers move to entitlements + payments; the Stripe columns and the three dead tables drop (D-12). Two deploys after WP-PAY-2.

Part 0 — verify
1. Both slots run a build ≥ WP-PAY-2 (vaultaire-switch status; /version on both ports).
2. Every reader of tenants.{stripe_subscription_id,subscription_status,house_period,
   deletion_stripe_cancelled_at}: grep as in docs/PAYMENTS.md § 0 row 3 — the list must be
   exactly the admin handlers, the export sections, the deletion runner, the retention table,
   erase.go. subscriptions / billing_charges / metered_usage_reports have no writer.
3. A fresh pg-backup exists (the drop is the one irreversible step).

What to build
1. admin_revenue / admin_costs / admin_support / tenants / admin / admin_stats / onboarding
   read entitlements + payments (MRR = sum of QuoteHouse over granted rows; revenue by month =
   payments.amount_cents settled; churn = lapses) — WP-R10-5 closed.
2. The deletion runner reads the entitlement (provider, renewal, provider_ref) instead of the
   tenant columns; the stamp becomes entitlements.cancelled_by_runner_at (additive column in
   the same migration) — or a ledger row kind 'adjustment' purpose 'deletion_cancel'; pick one.
3. Export sections and erase.go drop the old columns/tables; TestExportSectionsCoverSchema
   and TestErasedTablesCoverSchema stay green across the drop.
4. Migration <next>: ALTER TABLE tenants DROP COLUMN IF EXISTS … (the four), DROP TABLE IF
   EXISTS subscriptions, billing_charges, metered_usage_reports; stripe_events.tenant_id stays.
   stripe_customer_id stays (the provider's customer handle).
5. Cosmetic: internal/billing → internal/payments/stripe (gofmt-only move, separate commit).

Tests: the schema tests after the drop; TestAdminRevenue_FromLedger; every WP-PAY test green.
Prod proof: the pages render from the ledger; `\d tenants` has no dropped column; both slots
healthy after the switch. Rollback: none for the drop — hence the backup first; everything
else reverts.
```
