# Stage 1a — Prompt 23 (= Prompt 22 re-issued): Part 0 (RP26-1/2) and WP-PAY-7 (the contract: the admin pages and the deletion runner on the ledger, the webhook off the tenants billing columns, the D-12 drops)

Date: 2026-10-21. Prod at the start: `9195b74549e5` (slot 8000, active since 12:44:58 UTC), `STORAGE_MODE=idrive`, 0 rows in `entitlements` / `payments`, 3 tenants. Branches `stage1a-p23-{part0,readers,writers,drop,g202}`, worktrees `../vaultaire-p23{,w}`; private test DBs `vaultaire_p23` (migrated through 100) and `vaultaire_p23pre` (through 099). Other sessions merged and deployed beside this prompt all day (sia / blomp / host tuning, #825–#835): every PR was rebased onto `origin/main` read at that moment.

Prompt: Part 0 = RP26-1 (P3) + RP26-2 (P4) + the prod read; WP-PAY-7 = the plan row (readers → writers + `charge.refunded` → the drop → docs). Design of record: `docs/PAYMENTS.md` § 1.2, § 1.6 step 4, § 2.5, § 7. Lane: Developer surface (`docs/surfaces/DEVELOPER.md`); `docs/ARCHITECTURE_CONTRACT.md` line 12 — every column we hold has a stated purpose, and these had none left.

**PRs:** Part 0 #832 (`b93ab3c`), readers #834 (`165d756`), writers + `charge.refunded` #836 (`8545d7c`, carrying the G202 fix — #840 closed as superseded), migration 100 #839 (`d40040f`); this docs PR last. **Migration 100. Next migration: 101.**

## Decisions

| # | Decision | Why |
|---|---|---|
| D-P23-1 | A terminal Stripe status (canceled / unpaid / incomplete_expired / paused, `customer.subscription.deleted`) on a GRANTED entitlement whose `paid_through` is after now **never revokes**, `entitlement_lapse` ON or OFF: `payments.EndByStatus` stops the renewal under the entitlement lock (`billing.renewal_stopped`, one Warn naming the kept date); `paid_through <= now`, a lapsed row or no row → `Revoke` as before. With the flag OFF nothing clears such a house after its date: the lapse job counts it `would_clear` after grace and clears it when the flag flips. | RP26-1: since RP24-1 a Renew of a manually-paid house opens a $0 trialing subscription; cancelling the trial in the portal revoked the year paid by hand. Paid time is taken only by an operator refund/adjustment, never by a status. |
| D-P23-2 | The deletion runner's inputs are the record only: the entitlement's `provider_ref` plus every Stripe subscription the webhook recorded as an invalid house — a `payment_events` row `invalid_house:<sub>` (`payments.RecordInvalidHouse`), kept by the retention job until the erasure anonymises it. Its stamp is a `payment_events` row `deletion_cancel:<ref>` written only after the provider answered and checked before calling, cancelled through `payments.RefCanceller.CancelRef`. | Item 6 + RP22-3 (D-P20-3): the invalid house's subscription used to live on `tenants.stripe_subscription_id`, which is dropped. Keying the stamp by the reference makes the 2026-10-01 rule structural: a new subscription is a new reference, so an old stamp can never skip it. |
| D-P23-3 | The webhook writes **no** tenants column but `stripe_customer_id` — `plan` included. `tenants.plan` is the operator's label (a legacy pack label, the admin tier picker); every reader that showed "house" derives it from the record (`houseStateOf`: active / in grace / lapsed / free). A legacy pack's subscription events write nothing (no adoption, no downgrade). | RP21-3 asked for readers that do not trust a lying `plan`; not writing it at all removes the lie at the source. Prod has 0 packs and 0 houses. |
| D-P23-4 | `charge.refunded` records each succeeded refund (keyed by its `re_…` id) and does **not** shorten `paid_through`; an unknown charge, a redelivery, an amount past the charge are 200 with a log line. | Whether a refund takes paid time is the operator's call (the admin refund form's checkbox, WP-PAY-3); a webhook storm must never come from our ledger disagreeing with Stripe. |
| D-P23-5 | **The first DROP in the set:** because `deploy.yml` re-applies every migration file, the statements that created the dropped objects (018, 019, 043, 056, 066, 072) were removed in the same change as 100. Rule for every future contract: *empty the creator, then drop.* | Left in place, each deploy would re-add four `tenants` columns for 100 to drop again; a dropped column keeps its `pg_attribute` slot, so `tenants` would have crept toward Postgres' 1,600-column cap by four slots per deploy. `TestMigration100_Idempotent` is red with the old files (4 → 12 dropped slots over two re-runs) and green with the change. |
| D-P23-6 | The PR 3 build was made **drop-safe** before the drop: the `entitlement_lapse` pre-097 backfill (and `SubscriptionPeriodEnd`) moved from PR 4 into PR 3, and the `subscriptions` erase rule became a no-op there. | Found by running the PR 3 build's tests against the post-100 schema: `EraseRows`' `DELETE FROM subscriptions` and the backfill's read would have failed in the window between the migration and the slot switch. After the move every product path of that build passes on both schemas. |
| D-P23-7 | The live-house predicate is written as a literal wherever SQL joins on it (no shared SQL constant concatenated). | gosec G202 (#840): the Security workflow is not a required check, and the merge queue took #834 at the head before the fix. |

## What was built

### Part 0 (#832)
- **RP26-1** — `payments.EndByStatus` (entitlement.go) + `WebhookHandler.keptByDate`; `applyHouse`'s flag-OFF branch and `handleSubscriptionDeleted`'s house branch call it; `clearHouseTx` gone. Four older tests that cleared a house paid 30 days AHEAD moved their arrange to a date in the past (`pastHouse`).
- **RP26-2** — `vendorView` matches the reseller user with `strings.EqualFold`.

### PR 2 — readers (#834, + #840)
`internal/payments/live.go` (`LiveHouses`, `CountLiveHouses`, `HouseMonthOf`); admin revenue (houses as bought, the dead `subscriptions` churn reads replaced by first grants / lapses), overview, funnel, costs, support search, tenants list/detail (`houseStateOf`; the detail card is "House" + the entitlement sentence); the billing page's plan card (RP21-3) and `loadHouse` (no tenants fallback — `payments.Status` only); onboarding; the deletion runner (D-P23-2); the export's tenants section without the four columns.

### PR 3 — writers + `charge.refunded` (#836)
Every tenants billing-column write gone (`SaveSubscription`, `writeInvoiceStatus`, `GetSubscription`, `CancelSubscription` deleted); `isCurrentSubscription` = the entitlement's `provider_ref` only, failing CLOSED for a tenant with no row; `webhook_refund.go` (D-P23-4; `payments.ChargeOf`, `payments.ErrRefundRecorded`); D-P23-6.

### PR 4 — migration 100 (#839)
`100_contract_drops.sql` (D-P23-5); the creators emptied; the erase / export entries of the three tables; schema tests.

## Before → after

| Probe | Before | After |
|---|---|---|
| Manual year → Renew by card ($0 trial) → trial cancelled in the portal, flag OFF | floors cleared, row lapsed: the paid year gone | renewal stopped, house kept to its date, one Warn |
| Billing page of a lapsed house (`tenants.plan` still `house`) | plan "house", status "Active" | no house, "Ended" |
| Admin revenue / overview with a granted entitlement and no tenants columns | absent / counted 0 | priced from the row / counted |
| A tenants row saying `house`/`active` with no entitlement | priced and counted | neither |
| Deletion runner, a crash between Stripe's answer and the stamp | stamp on `tenants`; a stale stamp only safe by the "cancel every run" rule | stamp per reference in `payment_events`; one idempotent repeat, then no call |
| `charge.refunded` | ignored (RP23-4) | one refund row per refund id, counted under the charge's provider, audited |
| A clearing event for a tenant with no entitlement row | the tenants fallback treated it as current and revoked floors | ignored (fails closed) |
| A deploy after the drop | (would re-add + re-drop four columns, +4 dead slots per deploy) | the dropped objects stay absent; 0 new slots |

## Local proof

Full `go test -race ./...` green for every PR on a private DB (`vaultaire_p23`, then `vaultaire_p23pre` for the pre-drop schema). Lint (golangci-lint 2.14.0) 0 issues; gosec (the workflow's command) 0 issues after #840's fix. Red → green (new tests): `TestSubscriptionDeleted_FlagOff_FuturePaidThrough_KeepsHouse`, `TestTerminalStatus_FlagOff_FuturePaidThrough_KeepsHouse` (×4 statuses), `TestTerminalStatus_FlagOff_PastDate_ClearsAsBefore` (guard, green before), `TestVendorView_ResellerUserCaseInsensitive`, `TestAdminRevenue_FromLedger`, `TestAdminOverview_ActiveSubsFromEntitlements`, `TestBillingPage_LapsedHouseNotLive`, `TestBillingPage_ReadsTheRecordOnly`, `TestDeletionRunner_CancelsFromEntitlement_StampIdempotent`, `TestCancelSubscription_RecordedInvalidHouse_CancelsOnce`, `TestDeletionRunner_ProviderErrorWritesNoStamp`, `TestWebhook_WritesNoTenantsBillingColumns` (source guard — sqlmock would have to model every `Grant` query), `TestNoSQLNamesTheDroppedTenantsColumns`, `TestIsCurrentSubscription_NoRow_IgnoresClearingEvent`, `TestChargeRefunded_RecordsOnce`, `_UnknownCharge200NoApply` (guard, green before: the event was unhandled), `_CountedByChargesProvider`, `TestLapseJob_FloorsWithoutRow_Untouched`, `TestMigration100_Idempotent` (red with the old creators: `tenants` 4 → 12 dropped slots). ~35 older tests changed their ARRANGE only (houses seeded through `payments.Grant`, sqlmock expectations of the new queries, assertions moved from the tenants status columns to the record — the guarantees unchanged).

**The two-slot proof (D-P23-6):** the PR 3 build's tests run against the post-100 schema — before the move, `EraseRows` and the lapse backfill failed; after it, every product path green.

## Prod proof

Read-only box scripts throughout (journal, `psql` as postgres, `/metrics` on the active port). Expected noise excluded: the `BLOMP_ALLOW_UNSEALED` boot Warn, bench tenants' traffic.

| When (UTC) | Build | What was read |
|---|---|---|
| ~14:00, before any PR | `9195b74549e5` | 59 error / 51 warn lines since 12:44:58 — **all bench traffic** (blomp PUTs `request body already consumed`, the host-tuning session's `tuning-bench` chunked GETs on Lyve, bench-bucket HEAD misses); 0 billing lines. `backend_capacity` (ran 09:21:05) ok, vendor view off (RP25-1). Every job ok. 0 entitlements / payments / floor rows / `plan='house'`; the objects 100 drops held **0 rows** (0 subscription ids, statuses, periods, stamps, caps; 0 rows in the three tables) — the backfill owed nothing. |
| Deploy 14:32:15 → 14:35:37 | `b93ab3c` (#832) | (Part 0) |
| Deploy 15:25:09 → 15:27:09, then 15:26:09 → 15:28:49 | `165d756` (#834), then `45e6e07` (#837, another session) | (readers) |
| Deploy 15:46:10 → 15:47:50 | `26291b2` (contains #836) | since the 15:47:35 switch: **0 error lines**, 1 Warn (the expected Blomp one); every job ok; the payment / webhook / entitlement series present at 0. Slot 8001 stopped. |
| 15:51:13–15:51:20 | — | **`pg-backup.sh` by hand** (gate c, owner's go for gate b): `vaultaire_20261021_155113`, 14,098,065 B, 106 tables, off-box copy on Sync size-verified, the three backup timestamps written. |
| Deploy 16:04:37 → 16:06:34 | `d40040f` (#839, migration 100) | the four `tenants` columns and `spending_cap_cents` **absent**, the three tables **absent**; dropped-column slots `tenants` 4, `tenant_quotas` 1 (exactly one drop); **0 error / 0 warn lines** on the new slot (besides the Blomp Warn); the old slot logged no "column/relation does not exist" in the window; every job ok; `/health` 200. |

Benchmarks: the owner gave the go for the drop at ~15:50; the four deploy windows above are the ones to discard from bench samples.

## Residuals (RP27-*)

| # | Sev | Item | Where it goes |
|---|---|---|---|
| RP27-1 | note | The next deploy after `d40040f` should leave `pg_attribute` dropped slots at `tenants` 4 / `tenant_quotas` 1 — the prod confirmation of D-P23-5 (the test proves it locally). | the next review's prod read |
| RP27-2 | note | A Stripe subscription that is no house (a legacy pack bought before WP-PAY-1) is now invisible to the record: not adopted, not cancelled by the deletion runner. Prod has none (no Stripe key ever set). If Stripe goes live with an imported customer base, list subscriptions by customer once and convert or cancel by hand. | owner / WP-PAY-5 |
| RP27-3 | note | With `entitlement_lapse` OFF, a house kept by D-P23-1 past its date is cleared by nobody until the flag flips (`would_clear` counts it). The flag flip is already the plan's next step. | WP-PAY flag flip |
| RP27-4 | note | `tenants.plan` is now only an operator label (D-P23-3); the admin "Plan" column shows `house` from the record. A later WP could rename it `tier_label` or fold it into `tenant_quotas.tier`. | note |
| RP27-5 | note | The merge queue took #834 at its earlier head while gosec (not required) was red: making the Security workflow's gosec a required check would have held it. | owner (branch protection) |
| RP26-3 | carried | $0 promo sale unnumbered — owner decision. | owner |
| RP25-1, RP25-2, RP23-1, RP23-2, RP20-7, RB-3 | carried | as before (reseller key, F-31 plan rows, street address, § 3.9 sentences, …, remove `BLOMP_ALLOW_UNSEALED` before customer data). | owner |

Closed here: RP26-1, RP26-2, RP21-3, RP23-4, WP-R10-5 (reading revenue from the ledger), WP-PAY-7.

## Post-merge review (2026-10-21, meta-review)

Adversarial pass over #832, #834, #836, #839 (and the docs #843) at `f660265`. **Every claim holds; no P0/P1.** One P3 proven, the rest notes.

**Verified.** `payments.EndByStatus` takes the entitlement lock, keeps a granted row with `paid_through > now` (renewal stopped, audited once through `StopRenewalChanged`), revokes otherwise — and both callers (`applyHouse`'s flag-OFF clearing branch, `handleSubscriptionDeleted`) reach it only after `isCurrentSubscription`, which now fails CLOSED (`current != "" && current == subID`) and is never consulted on a granting status (the first grant of a tenant with no row still lands). `currentSubscription` reads `entitlements.provider_ref` of a `stripe` row only. The deletion runner's targets are the entitlement's reference plus every `invalid_house` row (deduplicated against it); `DeletionCancelled` is checked before the call and `MarkDeletionCancelled` written only after the provider answered; `cancelRef` still GETs first and treats `canceled` as success; an unconfigured provider defers only an `auto` target. The retention policy keeps `invalid_house` rows while they name a tenant and the erasure nulls `payment_events.tenant_id`. The only `GrantRecorded` callers are the Stripe webhook (×2) and `RecordManualPayment`'s no-row branch; `Extend` and `AdjustEntitlement` never rewrite `provider`/`provider_ref`, so no admin path can orphan a live Stripe subscription from the record. Checkout's second guard (`HasLiveSubscription`) lists the customer's subscriptions at Stripe, so it never depended on the dropped columns. Migration 100 + the emptied creators: no migration file and no non-test Go names a dropped object (grep of `internal/`, `cmd/`); `TestMigration100_Idempotent` stands. No commit of the five PRs carries an AI `Co-Authored-By` trailer.

**Prod read-only 16:30 UTC** (`d40040fd8f92` on slot 8001, `/health` 200): the four `tenants` columns and `spending_cap_cents` absent, the three tables absent; dropped slots `tenants` 4, `tenant_quotas` 1 — **RP27-1 stays open: no deploy has run since the drop** (the next one must leave 4 / 1); journal since the 16:06:34 switch **0 error / 2 warn** (both `HEAD: object not in metadata cache`, bench traffic), no "does not exist" line; 0 entitlements / payments / payment_events, 3 tenants (plans `starter` ×2, `vault100` ×1); `entitlement_lapse`, `payment_reminders`, `account_deletion`, `retention`, `backend_capacity`, `routing_truth` all `ok`; every payment series present at 0, `payment_provider_ready{stripe} 0`. `feature_flags`: 5 rows, none for `entitlement_lapse` or `pay_*`. Firing: `BackendPlanUnknown` ×4 (geyser, lyve, r2, wasabi — RP25-2, the owner's F-31 rows) and `RoutingTruthMissing` / `RoutingTruthMissingJump` `{idrive}` (the 501 head rows of the 2026-10-04 account, CLAUDE.md § Entry Point — known).

**Installed by this review:** `vaultaire-routing.yml` from #837 (another session's WP-ROUTING-READ) was not on the box (`a7d13808…` there, `f9b8e49c…` in the repo). `promtool test rules` SUCCESS in one directory on the box, backup `vaultaire-routing.yml.bak-20261021-rev23`, installed `f9b8e49c9e980e3cbeae85dbd1ef82dede7f8b5c7eb105bb768d10105b9eca6d`, Prometheus reloaded; the group now carries `RoutingTruthUnreadable` and `RoutingTruthChunksUnreadable`. `vaultaire-payments.yml` matches (`e1ed3ac4…`).

### Findings

| # | Sev | Finding | Proof | Where it goes |
|---|---|---|---|---|
| RP28-1 | **P3** | **A Stripe refund is recorded twice when the operator also uses the admin refund form** — the WP-PAY-3 workflow ("refund in Stripe, record it here, tick *shorten* if paid time goes") is the one `internal/billing/CLAUDE.md` still names for shortening. Since #836 `charge.refunded` records the refund under its `re_…` id; the admin form (`HandleAdminRefund` admin_billing.go:158, `handleAdminRefund` admin_payments.go:218) posts with `provider_ref` usually blank → `RecordRefund` (admin.go:300) keys it `refund:<uuid>`, a different `(provider, provider_id)`, so `Settle` sees it as new. Either order doubles it; a full refund is caught (`ErrRefundExceeds`, the second side refused — and if that side was the form, its *shorten* never happens), a partial one is not. Every refund row takes a credit-note number from the gapless, never-pruned `invoice_counters` series, so the wrong row cannot be withdrawn, only countered. | Throwaway test on `vaultaire_rev23` (deleted): a $10.00 Stripe charge, `RecordRefund(500, ProviderRef "re_rev23", actor stripe-webhook)` then `RecordRefund(500, ProviderRef "", actor admin-1)` → second call `err=nil`, credit note `ST-2026-000003`, **2 refund rows summing 1000, the charge marked `refunded`** for a $5 refund. No money moves (Stripe refunded once) and prod has no Stripe key, hence P3 — but it must be closed before Stripe goes live. | Prompt 24 Part 0 |
| RP28-2 | note | `charge.refunded` drops a refund it cannot place, for good: (a) a charge whose `invoice.paid` is not in the ledger yet (Stripe does not order deliveries; an `invoice.paid` answered 500 and retrying) is acknowledged 200 "ledger does not know" and no later event names that refund again; (b) a refund whose status is `pending` at delivery is skipped and `charge.refund.updated` is not handled, so it is never recorded when it succeeds. | Code: `webhook_refund.go:88-95` (ErrNoSuchPayment → 200), `:106` (status filter); `handle` has no `charge.refund.updated` case. | Prompt 24 Part 0 (small: when `recordInvoicePaid` newly settles a charge, record that charge's succeeded refunds; handle `charge.refund.updated` through the same path) |
| RP28-3 | note | `HouseMonthOf` churn: a re-grant clears `lapsed_at` (`GrantRecorded` sets it NULL), so a house that lapses and returns within one month counts neither as churned nor new, and a row lapsed before the month but re-granted in it counts as live at the month's start. Admin statistics only. | `live.go` HouseMonthOf; `entitlement.go:200`. | note (fix with an audit-row or ledger-based count when the revenue page is next touched) |
| RP28-4 | note | Stage a cancels only what the record names. `tenants.stripe_customer_id` is kept, and listing the customer's subscriptions at Stripe (what `HasLiveSubscription` already does) would also stop a legacy pack (RP27-2) and the two-tab race in which two Checkout sessions created before either completed both become subscriptions. | `deletion_runner.go` cancelSubscription; `house.go:433`. | WP-PAY-5 (its bulk stop-renewals action lists by customer anyway) |
| RP28-5 | note | Restating RP27-3 as a **gate**: with `entitlement_lapse` OFF and D-P23-1, a Stripe customer who cancels immediately keeps their floors until the flag flips — nothing else clears them. Prod holds 0 entitlements, so flipping the flag now (dry run read `would_clear = 0`, then the global row) costs nothing and must precede `STRIPE_SECRET_KEY`. | `EndByStatus`; `entitlement_lapse.go` count-only branch. | owner (flag flip before the Stripe click-list) |

RP27-1 (slots after the next deploy) carried to the next review's prod read. RP27-2 → covered by RP28-4. RP27-4/RP27-5 stand.
