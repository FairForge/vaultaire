# Stage 1a — Prompt 21: Part 0 (WP-P20 review residuals) and WP-CAP-1 (vendor capacity: the plan record, the backend_capacity job, the capacity / renewal / egress alerts)

Date: 2026-10-20/21. Prod at the start: `5cb3a5770b9d` (slot 8000), `STORAGE_MODE=idrive` (the revived original reseller account, invoice 105170), 0 rows in `entitlements` / `payments`, 3 tenants. Branches `stage1a-p21-part0`, `stage1a-p21-cap1` (stacked, rebased onto main after #818 merged), `stage1a-p21-toolchain`; worktrees `../vaultaire-p21{,b}`; private test DB `vaultaire_p21`.
Prompt: Part 0 = RP24-1 (P2) + RP24-2 (P3) + RP24-3/4/5 + the prod read; Part A = WP-CAP-1 A/B/C as the plan § WP-CAP (added by #814). Design of record: the plan rows; `docs/PAYMENTS.md` § 4.2 for RP24-1.

**PRs:** Part 0 #818 (`12b90d1`), WP-CAP-1 #820 (`e62ff68`), toolchain #821 (unplanned, see D-P21-4), promtool-test fix #822 (see D-P21-A9); this docs PR last. **Migration 099. Next migration: 100.**

## Decisions

| # | Decision | Why |
|---|---|---|
| D-P21-1 | **D-P20-A5 is amended:** the Renew button shows only inside the reminder window — within `payments.ReminderLead` (14 d) of the date, in grace, or lapsed. A granted prepaid row outside the window shows the sentence and no button; "Add a year" by card before that is WP-PAY-5's switch. | RP24-1: shown at any time, a renew on 2026-11-01 of a house prepaid to 2027-10-20 paid a year's price for 12 days. Inside the window the forfeit is bounded by the window itself. |
| D-P21-2 | `payments.Checkout.StartAt` (zero = now) carries the date the first charge may land; `payments.MinDeferredStart` = 48 h (Stripe's `trial_end` minimum, § 4.2). Stripe sets `SubscriptionData.TrialEnd` only when StartAt ≥ 48 h out; the handler sets the forfeit flash when a granted row's date is inside 48 h. The interface stays provider-neutral: BTCPay will read StartAt its own way (an invoice extending from paid_through). | RP24-1's fix as the review spelled it. |
| D-P21-3 | A $0 invoice settles **unnumbered**: `Payment.Unnumbered` makes `Settle` write `invoice_number` NULL and spend nothing from the gapless counter; the receipt mail gates on `settled.InvoiceNumber != ""`. The row IS settled (`status = 'settled'`, amount 0) so a redelivery is a replay, and the receipts list (numbered rows) never shows it. | The trial's `subscription_create` invoice is real at Stripe but no money moved: a numbered $0 receipt would be noise and the counter is gapless per year. |
| D-P21-4 | The Go toolchain moved 1.26.9 → 1.26.10 in its own PR (#821): GO-2026-6809 (crypto/x509 fails to distrust compromised intermediates, fixed in 1.26.10) made govulncheck fail the Security workflow on every PR and is reachable from every TLS client in the binary. | Out of the prompt's scope but in its lap: the failing check landed mid-prompt and `go-version-file: go.mod` means one line moves CI, gosec and prod's build. |
| D-P21-A1 | `backend_plans` is keyed by the registered driver name and the PUT refuses an unregistered one (404): a plan row nothing probes would never alert honestly, and a typo'd name must fail loudly. The row is an upsert — one row per backend, `updated_by` from the JWT, never the body. | The plan row A. |
| D-P21-A2 | The sums are per backend AND kind (`objects`, `versions`, `chunks`, `multipart`, `parity`, `packs`), all in SQL; `retired_blobs` is reported as a ROW COUNT (`retired_blobs_rows`), not bytes — the table records no size (086), and inventing one by asking backends would be a walk. Chunked head rows are never counted as objects (their bytes are the chunk blobs, at the compressed size the vendor bills); multipart holdings are attributed to the engine primary and the field's comment says so. | The prompt's column facts; the plan row B amended for `retired_blobs`. |
| D-P21-A3 | The vendor's `storage_used` is read as decimal GB (× 1e9) and the interpretation is **asserted against our own stored total**: `unit_checked` only when both are over 100 MiB and within 4× of each other; until then the run carries the note "unit unverified". The raw GB value rides in the result either way. | The plan's "units asserted against a known object on the first run, a note until then" — our stored total IS the known object(s), and the assertion re-runs every day instead of once. |
| D-P21-A4 | A reseller call that fails, a key not set, or the user missing from `/users` is a NOTE on an `ok` run, never a failed run; the reseller key is read by the master key's precedence (credential `idrive_reseller_api_key` → `IDRIVE_RESELLER_API_KEY_FILE` → env) and a set-but-unusable source refuses the boot. | The plan row B; the account-admin API has answered 5xx on dead regions; a typo must never mean "vendor view silently off". |
| D-P21-A5 | The reseller client decodes answers bare or unwrapped from a `data`/`users`/`stats` envelope. | The doc shows bare lists, the live console wraps some answers in `data` (the 2026-10-20 probe: `create_access_key` answers `data.access_key`); both shapes must parse. |
| D-P21-A6 | `vaultaire_backend_egress_free_bytes` = the plan row's `egress_free_multiple` × the last run's stored TOTAL (not the vendor's own number): the allowance iDrive bills against is 3× what is stored, and our stored total is the conservative side while the unit of the vendor's number is unverified. No series while the multiple is NULL (unmetered) or before the first run. | The plan row B's "(3× the stored total, from the plan row)". |
| D-P21-A7 | `BackendPlanUnknown` reads `vaultaire_backend_health{backend=~"idrive\|lyve\|geyser\|wasabi\|r2"} unless on(backend) vaultaire_backend_plan_bytes` — the paid backends by name; `local` and `sync` are not bought by the TB. `for: 6h` so a deploy's scrape gap cannot fire it. | The plan row C: firing until the owner enters the rows is the point. |
| D-P21-A8 | The renewal countdown rules read `term_end − time()` and keep firing past zero (the value goes negative): an expired term is the worst case of "renews in 7 days", not a resolved one. | The Aug-27 shape ran 12 days past the date unnoticed. |
| D-P21-A9 | promtool unit-test input series sample at **1 m intervals**: a 1 h-spaced series falls out of Prometheus's 5-minute staleness window between samples, so a `for:`-gated alert never goes pending and the test sees `got:[]`. Found by the box's promtool on the first install (#822 fixed the test file; the rule file was right). | A lesson for every future `tests/*_test.yml`. |

## What was built

### Part 0 (#818)

- **RP24-1** — `payments.Checkout.StartAt` + `payments.MinDeferredStart` (provider.go); `payments.ReminderLead` (reminders.go, used by `stepAt` and `renewOffer`); `houseCheckoutParams`/`CreateHouseCheckout` take `startAt` and set `SubscriptionData.TrialEnd` when ≥ 48 h out (house.go); `StripeService.StartCheckout` passes `c.StartAt` (provider.go); `Payment.Unnumbered` + `Settle` with `invoice_number = NULLIF(...)` (ledger.go); `recordInvoicePaid` marks a $0 invoice unnumbered and mails no receipt for an unnumbered row (webhook_ledger.go); `renewOffer` gates on the window; `HandleRenewHouse` sets StartAt for a granted row and the forfeit flash inside 48 h (billing_house.go).
- **RP24-2** — `PaymentRemindersRunner.Flags` (`payments.FlagSource`, a constructor argument; nil = off, as everywhere): `t+1d` and `last-day` go only where `entitlement_lapse` is on for the tenant; withheld = `skipped_flag_off` in the result + a note; not marked sent (the flag turning on inside the window sends it). Wired with `s.flags` (server.go).
- **RP24-3** — the manual Extend audits the ROW's `provider`/`renewal`; `RecordRefund` counts `CountPayment(<the charge's provider>, KindRefund, settled)`; both refund series pre-created at 0 (metrics.go).
- **RP24-4** — the `AdjustEntitlement` comment and `internal/payments/CLAUDE.md` now say RecordRefund shortens with its own guarded UPDATE under the entitlement lock (it never called AdjustEntitlement); "the one writer that may shorten" became "one of the two".
- **RP24-5** — a `provider` form value is accepted only when `svc.Enabled` lists it: a flash + one Warn (no user string in the log), never `Registry.For`'s Error.

### Part A — WP-CAP-1 (#820)

1. **Migration 099** `099_backend_plans.sql` (one row per backend, CHECKs on capacity and the term order). The three schema tests pass: no tenant/user column, so the table is outside the erasure walk and the export — said in `internal/account/schema_test.go`'s comment beside `payment_provider_readiness`; `exportExcludedTables` does NOT name it (the walk never sees it, as the prompt predicted).
2. **The plan record** — `internal/api/backend_plan.go`: `BackendPlan`, `readBackendPlan`/`upsertBackendPlan`/`listBackendPlans`, `GET/PUT /api/v1/admin/backends/{backend}/plan` (`registerAdminBackendPlanRoutes`, requireAdmin; 404 unregistered; PUT validates and audits `backend.plan_set` with the admin in `performed_by`); OpenAPI entries + `BackendPlan` schema (the drift guard passes).
3. **The measure** — `internal/api/backend_capacity.go`: `BackendCapacityRunner` (daily 08:30 UTC, +7 m catch-up, 10 min ceiling, registered in jobs_wiring; `POST /api/v1/admin/jobs/backend_capacity/run` for free), the five GROUP-BY sums + the multipart scalar + the retired-rows count, the vendor view (D-P21-A3/A4). `internal/idrive/reseller.go`: `LoadResellerKey`, `ResellerClient.Users/UsageStats` (15 s per request, the error envelope's text never the key). `cmd` wiring in `NewServer` (fatal on an unusable key source; `IDRIVE_RESELLER_USER` names the sub-account).
4. **The gauges** — `internal/api/metrics_capacity.go`: `capacityCollector` (15 s cache, failed read keeps the last values) exports the seven series from `job_runs.result` + `backend_plans` on scrape (D-P21-A6).
5. **The alerts** — `deploy/monitoring/vaultaire-capacity.yml` (11 rules, D-P21-A7/A8), `tests/vaultaire-capacity_test.yml` (promtool: Capacity80/90 incl. the vendor-only branch, Renewal7d, PlanUnknown), the README row; `backend_capacity` joined `vaultaire-jobs.yml`'s JobStale list (pinned by `TestJobRuleFile_…`, which derives the list from the registered jobs).
6. **The System page** — `loadBackendCapacity` (admin_system.go) + the Vendor Capacity card (system.html): bought / held by our rows / held by the vendor / free egress left / renews in N days (+ "no card" badge) / overage; backends the run saw with no plan row get a "no plan row" badge. The daily-jobs sentence names `entitlement_lapse 07:30, backend_capacity 08:30`.
7. **Docs** — root CLAUDE.md (the `backend_plans` table entry, the job list, the two env rows), `internal/api/CLAUDE.md`, `internal/idrive/CLAUDE.md` (new), `deploy/monitoring/README.md`.

### #821 — toolchain go1.26.10 (D-P21-4)

## Before → after

| Probe | Before | After |
|---|---|---|
| "Add a year" on a house prepaid to 2027-10-20, clicked 2026-11-01 | a Stripe subscription billed TODAY; paid through 2027-11-01 — a year's price for 12 days, the remainder forfeited, the anniversary moved | no button (outside the window); inside it, `trial_end = paid_through`: the charge lands on the paid date, the date is kept, the row flips to stripe/auto |
| The trial's $0 `subscription_create` invoice | a gapless receipt number spent, a $0 receipt mailed | settled unnumbered, no mail; the next paid invoice takes the next number |
| A `t+1d` reminder with `entitlement_lapse` OFF (prod) | "in grace, N days left" announcing an ending the job only counts | withheld, `skipped_flag_off: 1`, a note |
| A wire recorded on a Stripe `auto` row | audit says the house became manual/prepaid | audit says what the row is (stripe/auto) |
| A refund of a Stripe charge | counted `provider="manual"` | counted `provider="stripe"` |
| POST /dashboard/billing/renew with `provider='; DROP...` | `ErrUnknownProvider` logged at Error with the string | a flash, one Warn, no user string in the log |
| The iDrive renewal date | in a console behind a reCAPTCHA; nothing of ours knew it existed (12 days unnoticed in 2026-09) | a `backend_plans` row, the countdown gauges, Renewal30/14/7d + NoPayment alerts, the System card |
| Bytes per backend | no gauge anywhere | `vaultaire_backend_stored_bytes{backend,kind}` daily, and iDrive's own meter beside it |
| A registered paid backend with no plan | silence | `BackendPlanUnknown` warns until the owner enters the row |

## Local proof

Full `-race` suites green on `vaultaire_p21`: `internal/{payments,billing,dashboard/...,api,account,docs,idrive}`. Lint (golangci-lint 2.14.0) 0 issues; gosec 0 issues. Red tests as the prompt named them (all new): TestRenew_GrantedPrepaidRow_StripeTrialEndIsPaidThrough, TestRenew_Inside48h_StartsNow, TestRenew_TrialingSubscription_KeepsPaidThroughFlipsProvider, TestRecordInvoicePaid_ZeroAmount_NoReceiptNumberNoMail, TestBillingPage_RenewOnlyInsideTheWindow, TestReminders_GraceStepsOnlyWhereTheLapseJobWouldAct, TestManualPayment_ExtendAuditsTheRowsProviderAndRenewal, TestRefund_CountedByTheChargesProvider, TestRenew_UnknownProviderForm_IsAFlashNotAnErrorLine, TestBackendPlan_PutValidatesTermAndCapacity, TestBackendPlan_UnknownBackend404, TestBackendPlan_AuditRow, TestBackendCapacity_SumsEveryHolderOnce, TestBackendCapacity_VendorFailureIsANote, TestBackendCapacity_GaugesReadFromJobRuns, TestResellerClient_UsersAndUsageStats, TestLoadResellerKey_Precedence, TestJobsWiring_BackendCapacity0830, TestCapacityRules_ParseAndNameExportedSeries.

## Prod proof

### Part 0 (after #818 deployed — `12b90d194f15`, slot 8001, active since 02:19:17 UTC 2026-10-21)

Read at 02:45 UTC (box script, read-only): **0 `"level":"error"` and 0 warn lines** since the switch; **every job's last outcome `ok`** (22 jobs); `entitlements` 0, `payments` 0, `payment_provider_readiness` 1 (`stripe`, "STRIPE_SECRET_KEY is not set"), 3 tenants; the payment series as before plus the two **new refund pairs at 0** (`vaultaire_payments_total{kind="refund",provider="manual"|"stripe"}` — RP24-3's pre-creation on the deployed build); the 13 rule files on the box sha256-checked (`vaultaire-jobs.yml a73e3494`, `vaultaire-payments.yml e1ed3ac4` — the review's install, unchanged); 2 alerts: `RoutingTruthNoVerdict`, `DriverHedgesWinning`.

### Part A (after #820 deployed — `e62ff68a5537`, slot 8000, active since 02:53:47 UTC 2026-10-21)

Box script (the admin JWT minted inside from `JWT_SECRET` — note: `users` has no `tenant_id` column, the tenant comes from a join on the e-mail): **migration 099 applied** (backend_plans, 15 columns); `PUT /api/v1/admin/backends/idrive/plan` with the invoice facts → 200, the GET round-trips every field (`capacity_bytes` 1e12, `egress_free_multiple` 3, overage 5.12, egress 0.01, term 2026-10-20 → 2027-10-20, `payment_on_file` true, `invoice_ref` "invoice 105170"), **audit row** `backend.plan_set` with the admin's UUID in `performed_by`; `POST /api/v1/admin/jobs/backend_capacity/run` → 202, the run **`ok`** with the note `idrive 2.3 GiB, lyve 74.5 GiB, wasabi 2.3 GiB; vendor view off: IDRIVE_RESELLER_API_KEY or IDRIVE_RESELLER_USER not set` and the result: `idrive` {objects 2,048,010 B — the dead May rows + the canary, as the plan predicted — chunks 2,462,056,448 B}, `lyve` {chunks 80,027,080,625 B — the chunk rows}, `wasabi` {chunks 2,430,000,000 B — rows recorded in its interim day}; `retired_blobs_rows` {idrive 3,100, lyve 1, chunked 4}. **The gauges on `/metrics`**: `vaultaire_backend_plan_bytes{idrive} 1e+12`, `_plan_term_end_timestamp_seconds 1.8239904e+09`, `_plan_payment_on_file 1`, `_egress_free_bytes 7.392e+09` (3 × the stored total), `_stored_bytes{backend,kind}` for all three backends × six kinds. **0 error lines** since the switch.

**Rules installed by this prompt** (copied with their tests into ONE directory on the box, the P20 lesson): `promtool test rules` — `vaultaire-jobs_test.yml` SUCCESS, `vaultaire-capacity_test.yml` FAILED twice on the 1 h series (D-P21-A9) and SUCCESS after #822's fix; `install -m 0644`; `promtool check rules` → **11 + 9 rules**; `systemctl reload prometheus`; group `vaultaire-capacity` in `/api/v1/rules`. Hashes: `/etc/prometheus/rules/vaultaire-capacity.yml f935ad16…`, `vaultaire-jobs.yml 1b2fc21c…` (previous kept as `vaultaire-jobs.yml.bak-20261021`). **Alerts after the install**: `BackendPlanUnknown` pending for exactly `geyser`, `lyve`, `r2`, `wasabi` (idrive has its row — the rule works, and firing until the owner enters the rest is the point, RP25-2); `RoutingTruthNoVerdict{idrive}` still firing (pre-existing — the 05:30 run clears it on the revived account); `BackendCapacity80` silent at 0.002 of 1 TB. **The System card** renders from `loadBackendCapacity` (proven by the handler against the real template; the live admin page needs MFA — the P20 pattern).

## Residuals (RP25-*)

| # | Sev | Item | Where it goes |
|---|---|---|---|
| RP25-1 | note | The vendor view is off until the owner rotates the reseller key (it went through chat on 2026-10-20) and hands it to the box as the systemd credential `idrive_reseller_api_key` + `IDRIVE_RESELLER_USER` in the env — the [OPS]/owner block. | owner |
| RP25-2 | note | `BackendPlanUnknown` fires for `lyve`, `geyser`, `wasabi`, `r2` until their rows are entered (F-31 recommends entering all five the day this deploys). | owner (F-31) |
| RP25-3 | note | `vaultaire_backend_egress_free_bytes` uses OUR stored total; while the vendor-unit check cannot run (nothing stored) the EgressFree80 rule is effectively "any vendor egress fires once something is stored but free = 3×~0" — harmless on prod today (no vendor view yet), worth re-reading when the vendor view turns on. | note |
| RP23-1 | carried | street address (owner). | owner |
| RP23-2 | carried | § 3.9 policy sentences (owner, F-28). | owner |
| RP23-4 | carried | `charge.refunded` webhook. | WP-PAY-7 |
| RP23-5 | carried | grace < 2 d overlaps the grace steps. | note |
| RP24-6 | carried | receipt mail inside the webhook's answer (revisit with a real sender). | note |
| RP21-3, RP22-6, RP20-7 | carried | as before. | WP-PAY-7 / WP-PAY-5 / owner |

## [OPS] / owner — the vendor view and the plan rows

- In the iDrive console: turn on the e-mail notifications for disk utilization and renewal (both empty on 2026-09-20), enable 2FA, **rotate the reseller API key** (it went through chat on 2026-10-20) and seal the new one on the box: `systemd-creds encrypt --with-key=tpm2+host --name=idrive_reseller_api_key - /etc/credstore.encrypted/idrive_reseller_api_key` + `LoadCredentialEncrypted=` in the unit (the master-key ceremony's pattern, docs/ENCRYPTION.md); set `IDRIVE_RESELLER_USER=<the prod sub-account e-mail>` in `.env`; `sudo vaultaire-switch restart`.
- Enter the remaining plan rows (F-31): `geyser` (100 TB minimum, $155/mo), `lyve` (promo, no cap — a far `term_end`), `wasabi` (dormant), `r2` (free tier) — `BackendPlanUnknown` stops firing as each lands.
- Keep benchmarks against iDrive under a few hundred GB (3× stored free egress, then $0.01/GB).

## Post-merge review (2026-10-21)

Adversarial pass over #818 and #820 (non-test diffs read against the claims; CI green on main
through #822, the cancelled run on `e62ff68` was merge-queue supersession). **Every claim holds —
no P0/P1.** Verified directly: the renew window is enforced on BOTH entry points (`renewOffer`
runs again inside `HandleRenewHouse`, and an out-of-window POST would carry `StartAt` anyway, so
it defers rather than forfeits); `Settle` spends no counter number for an `Unnumbered` row and the
receipt mail gates on `InvoiceNumber != ""`; the capacity sums exclude the `chunked` sentinel and
NULL backends everywhere, retained versions use the one billing rule, and the reseller key travels
only in the `token` header with the error envelope's text never echoing it; the egress-free gauge
on prod is exactly 3 × the stored total.

| # | Sev | Finding |
|---|---|---|
| RP26-1 | **P3** | A Renew's trialing subscription is reachable with **$0 moved** (RP24-1), and `payments.Revoke` (entitlement.go:323) clears the floors and marks lapsed **regardless of a future `paid_through`**: a customer with a manually-paid house who clicks Renew and then cancels the trialing subscription in the Stripe portal, with `entitlement_lapse` OFF (prod default), loses the paid year today. Before RP24-1 this branch always followed a real charge. Dark on prod (no Stripe key); data untouched; → Prompt 22 Part 0: a terminal status on a GRANTED row with `paid_through > now` stops the renewal instead of revoking, flag OFF or ON — paid time is never taken by a status. |
| RP26-2 | P4 | `vendorView` matches the reseller user case-sensitively (`users[i].Email == r.ResellerUser`, backend_capacity.go): an e-mail iDrive returns in different case is "not in /users" forever. One `strings.EqualFold`. → Prompt 22 Part 0. |
| RP26-3 | note | **Any** `AmountPaid == 0` invoice settles unnumbered — including a 100 %-promo-code real sale (`AllowPromotionCodes` is on; the LET code). A $0 sale then has no receipt at all. Owner decision: acceptable (nothing was paid), or number-but-don't-mail. |

Rules on the box were installed by the worker itself this prompt (`vaultaire-capacity.yml
f935ad16`, `vaultaire-jobs.yml 1b2fc21c`) — nothing left as [OPS] except the owner's reseller key
(RP25-1) and the four plan rows (RP25-2).
