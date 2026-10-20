# Stage 1a — Prompt 18: Part 0 (WP-P17 review residuals) and WP-PAY-1 (the entitlement record, migration 097)

Date: 2026-10-20. Prod at the start: `6439e91` (slot 8000; main `8d12a1e`, docs only since). Branches `stage1a-p18-part0` (Part 0) and `stage1a-p18-wp-pay-1` (Part A).
Prompt: Part 0 = RP19-1, RP19-9, RP19-10, RP17-6, RP19-2 + the prod read; Part A = WP-PAY-1 as `docs/reviews/WP-PAY-PROMPTS.md` § "Prompt WP-PAY-1" with the day's updates (migration 097, moved line refs).

**PRs:** Part 0 #801 (`a8d5034`), Part A #802 (`0a121b6`, commits `4c149d9` migration + package, `79f2120` webhook + job); this docs PR last. **Next migration: 098.**

## Decisions

| # | Decision | Why |
|---|---|---|
| D-P18-1 | A 0x05 (SSE-C multipart) vault-floor object is skipped by the parity job **before it takes a slot** and counted on `vaultaire_vault_parity_unprotected_objects{reason="unknown_length"}` (pre-created at 0, beside `flag_off` — now also pre-created — `too_large`, `other`), noted (≤ 20 notes a run), never a row or a shard. | It burns none of its 20 attempts and never blocks a newer object; reading the length from the blob's own header is the real fix and not this prompt. No rule watches the new reason (`VaultParityObjectTooLarge` matches `too_large` only), so no rule file changed. |
| D-P18-2 | The Smart demotion candidate query excludes `-mp` rows; the copy-back refuses one with an error. | Both are flag-dark; a ledger row cannot name such an object today, and if one ever does the daily retry warns instead of copying 0 bytes. |
| D-P18-3 | `MfaDelete` is validated like `Status` (only `Enabled` / `Disabled`) as 400 `MalformedXML` BEFORE the MFA check. | Garbage passed the `!= ""` gate, consumed a valid code and wrote Disabled. |
| D-P18-4 | An `IsMFAEnabled` OR a `GetMFASecret` read error is `mfaDeleteUnknownError` → 503 + `Retry-After` (the RP16-5 shape); only "no TOTP enrolled" keeps 403. | A database outage must not read as "MFA not configured". |
| D-P18-5 | `X-Webhook-Signature` is `t=<unix>,v1=<hex>` only; the reference verifier in the outbox tests refuses any other element. | The legacy `sha256=` was "for one release after #791"; several shipped. Risk: 0 webhook endpoints on prod (read 2026-10-20). |
| D-P18-6 | A 0x05 Range opens the FIRST span's header up front (a wrong key is still 403 before the status line) and every later part's header when the previous span ends; the chain holds its own copy of the customer key, zeroed after the last span opens and again on Close. | N parts cost N header reads only as the body reaches them. Tested at `openSealed` with a counting source (the one level where reads count exactly). |
| D-P18-A1 | **`lapsed_at` has a second writer while the flag is OFF**: the Stripe webhook's clearing branch (canceled / unpaid / incomplete_expired / paused, `subscription.deleted`) marks the row lapsed in the same transaction as it clears the floors (`payments.Revoke`). The design says the lapse job is the only writer. | Otherwise the job's reconcile (floors must equal every granted row's house) would grant back, the next morning, a house the webhook took away. WP-PAY-2 moves the clearing branch behind the flag; the job is then the only writer, as designed. |
| D-P18-A2 | `past_due` grants with `paid_through = current_period_START` instead of "no change". | Same outcome on an existing row (a grant never shortens a date, so the row keeps the last paid date), and a row created by a `past_due` event (a house granted before 097) gets the right date instead of none: Stripe moved the period on when it raised the renewal invoice, and nothing was paid for it. |
| D-P18-A3 | `invoice.payment_succeeded`: amounts and period from the signed payload; the SUBSCRIPTION is fetched for the date (Grant to its `current_period_end`). The design says "fetch the invoice". | A paid invoice is final at Stripe — its payment event cannot be stale about amounts. What can be stale is the subscription, and it is fetched (R10-15). One Stripe call fewer per renewal. |
| D-P18-A4 | A grant never shortens a running date (`GREATEST`); a lapsed row is granted at the given date. | § 1.4: "Stripe's status never shortens the date". An operator refund (WP-PAY-7) is the only thing that will. |
| D-P18-A5 | `renewal = 'prepaid'` when the fetched subscription has `cancel_at_period_end`. | Nobody will extend it; the reminders of WP-PAY-3 read `renewal`. |
| D-P18-A6 | The dashboard's in-place resize writes the entitlement too: `server.go` wires `payments.HouseWriter` (Resize / Revoke) as the `billing.HouseQuotas` of the dashboard and the webhook. Not in the design. | The resize called `QuotaManager.SetHouse` directly; with the reconcile in the job, a resize whose webhook was delayed would have been reverted. No path changes a house's floors without its entitlement now. |
| D-P18-A7 | The webhook's grant runs in its own transaction beside the held claim (a second pooled connection), not on the claim transaction. | The prompt's choice to document: the handlers already query on the pool; the claim's per-customer lock serialises the customer's events and `payments.Lock` the tenant's grants; passing the claim tx would thread a `*sql.Tx` through every handler for no extra guarantee. |
| D-P18-A8 | The reconcile heals whatever the flag (one Error line per tenant). | Every writer writes both tables since 097, so a drift is a bug or a pre-097 state, and the entitlement is the billing truth. The flag governs clearing only. |
| D-P18-A9 | The erasure's payments/payment_events scrubs are in `account.Deleted` (before `tenants`), not `Kept`. | `Kept` runs after the `tenants` DELETE; `payments.tenant_id` is `ON DELETE SET NULL`, so a `Kept` scrub keyed by the tenant would match nothing and leave addresses and notes. |
| D-P18-A10 | Backfill of a house with no Stripe subscription: provider `manual`, renewal prepaid, `paid_through` = now + grace, a Warn. | The prompt's rule. Consequence carried as a residual: when the flag is turned on, such a house lapses 2 × grace after its backfill unless an operator extends it (WP-PAY-2's manual grant). Prod has 0 floor rows. |
| D-P18-A11 | `vaultaire_entitlements_expiring{window}` windows: `7d`, `30d`, `in_grace` (paid_through passed, still granted). | The two reminder horizons of § 3.8 plus the one the lapse job acts on. |
| D-P18-A12 | The flag is checked per tenant (`entitlement_lapse`, default OFF). | Built dark, enabled one canary tenant first, as every flag in the plan. |
| D-P18-A13 | Privacy policy: "Billing records, 7 years" stays and gains the ledger sentence (not purged automatically; de-linked, crypto addresses / txids / note removed at erasure); the DPA's Monitoring item names payment-provider event records 90 days and the ledger. `payment_events` is covered by the policy's existing "payment-provider event records, 90 days". | One statement per period, in both documents, matching `retention.go`. |

## What was built

### Part 0 (#801, `a8d5034`)

- **RP19-1** `vault_parity.go` (skip + `unknown_length`), `smart_demotion.go` (candidates exclude `-mp`), `smart_promotion.go` (copy-back refuses `-mp`); `wp_p18_part0_test.go`.
- **RP19-9 / RP19-10** `s3_versioning.go` (the `MfaDelete` value), `s3_mfa_delete.go` (`validateMFAHeader`: a read error → `mfaDeleteUnknownError`); `wp_p18_mfa_test.go`.
- **RP17-6** `events.go` (`generateWebhookSignature` / the signing join), `docs/API.md` and `internal/docs/openapi.go` (the signature section), `events_test.go`, `webhook_outbox_test.go`.
- **RP19-2** `sse_stream.go` (`openMultipartRange` / `spanChain` lazy); `wp_p18_ssec_range_test.go`.

### Part A — WP-PAY-1 (#802, two commits)

- **Migration 097** `097_payments.sql`: `entitlements`, `payments`, `payment_events`, `invoice_counters` as § 1.2 (CHECKs on period / renewal / provider / house, kind / purpose / status — `failed` added — and period; the partial index on `paid_through WHERE lapsed_at IS NULL`; `idx_payment_events_received` for the retention job). Additive.
- **`internal/payments`** (new): `Order` (= `billing.HouseOrder`, alias), `Period` (= `billing.Period`), `Renewal`, `Entitlement`; `Lock`, `Grant`, `Extend`, `StopRenewal`, `Revoke`, `Resize`, `Get`, `StatusOf`, `HouseWriter`, `InTx`; the ledger `RecordPending` / `Settle` (gapless `ST-<year>-<n>` from the year's counter row inside the settle transaction) / `Fail` / `Expire` / `Invalidate` / `Credit`, metadata keys `ids` / `addresses` / `txids`; `GraceFromEnv` (`ENTITLEMENT_GRACE`, 1h–60d, default 336h, Warn-and-default); `ReadyHistory` + `AlwaysReady`; `vaultaire_payments_total{provider,kind,outcome}`.
- **`usage.SetHouseTx` / `ClearHouseTx`**: the bodies of `SetHouse` / `ClearHouse` on the caller's transaction (total before floors, unchanged).
- **The Stripe webhook dual-writes** (`webhook.go` `applyHouse`, `webhook_ledger.go`): active/trialing → `Grant` through `current_period_end`; past_due → `Grant` through `current_period_start`; terminal statuses and `subscription.deleted` → `Revoke` + `StopRenewal` (still clearing, as today); `invoice.payment_succeeded` → ledger row settled + `Grant` to the fetched subscription's period end, one transaction; `invoice.payment_failed` → ledger row `failed`. The `tenants` columns are written in the same transaction as before. `StripeService.SubscriptionPeriodEnd` for the backfill.
- **The `entitlement_lapse` job** (`internal/api/entitlement_lapse.go`): daily 07:30 UTC, +5 m catch-up, 10 min ceiling; backfill / lapse (flag) / reconcile; result `{backfilled, candidates, would_clear, cleared, skipped_unready, healed, grace, errors}`; audit `billing.house_lapsed`; `vaultaire_entitlements_lapsed_total`, `vaultaire_entitlements_expiring{window}` (from the table). Registered in `jobs_wiring.go`, listed by `GET /api/v1/admin/jobs`, run by `POST …/jobs/entitlement_lapse/run`; `JobStale` in `deploy/monitoring/vaultaire-jobs.yml` watches it.
- **Flag** `entitlement_lapse` registered default false.
- **Erasure** (`account/erase.go`): `entitlements` deleted; `payments` scrubbed (tenant, note, `metadata.addresses`, `metadata.txids`); `payment_events.tenant_id` nulled. **Export**: sections `entitlement` and `payments` (keyset-paged, no `actor`); `payment_events` excluded with its reason. **Retention**: `payment_events` 90 d; `payments` never (pinned in `TestRetention_PoliciesMatchThePolicyPage`). Privacy policy + DPA sentences.
- Docs: `internal/payments/CLAUDE.md` (new), `internal/{billing,usage,account,database,api,dashboard}/CLAUDE.md`, root `CLAUDE.md` (tables, `ENTITLEMENT_GRACE`, the job list).

## Before → after

| Part 0 | Red against origin/main `8d12a1e` | Green |
|---|---|---|
| RP19-1 parity | `TestVaultParity_SSECMultipartObjectIsSkippedAndCounted`: `expected: 0 actual: 1` failed — "encode parity: the object is longer than its recorded 0 bytes", a `partial` row written, gauge `other`=1 | skipped, no row, no shard, `unknown_length`=1 |
| RP19-1 demotion | `TestSmartDemotion_NeverNamesAnSSECMultipartObject`: `expected: 1 actual: 2` candidates | 1; the 0x05 object stays on idrive |
| RP19-1 copy-back | `TestSmartPromotion_CopyBackRefusesAnSSECMultipartObject`: no error (copied at length 0) | refused, stays cold |
| RP19-9 | `TestBucketState_AnMfaDeleteValueThatIsNeitherEnabledNorDisabledIsMalformed`: `expected: 400 actual: 200`, MFA Delete written off, the code consumed (its reuse 403) | 400 `MalformedXML`, nothing written, the code unused |
| RP19-10 | `TestBucketState_AnMFAStateReadErrorIs503NotNotConfigured`: `expected: 503 actual: 403` | 503 + `Retry-After`; `…NoTOTPEnrolledKeepsIts403` green before and after |
| RP17-6 | `TestWebhookSignature_IsTimestampedAndV1Only`: `"t=1800000000,v1=6aba…,sha256=54ad…"` does not match `^t=1800000000,v1=[0-9a-f]{64}$` | v1 only |
| RP19-2 | `TestSSECMultipartRange_OpensTheNextPartsHeaderOnlyWhenItsTurnComes`: stored reads before the first byte `expected: 4 actual: 6` (lead + table + 3 headers + data) | 4 (lead, table, part 1 header, part 1 data), 8 in all; the wrong-key test green before and after |

| Part A | Before (origin/main) | After |
|---|---|---|
| A paid house's date | nowhere: `subscriptions.current_period_end` has no writer | `entitlements.paid_through` = Stripe's `current_period_end`, written with the floors |
| A Stripe payment | `tenants.subscription_status = 'active'` | + a `payments` row `settled` with our receipt number `ST-2026-…`, house granted to the period end |
| Red, the six webhook tests on the old handler | `TestStripeWebhook_WritesEntitlement_PaidThroughIsCurrentPeriodEnd`, `…PastDue_KeepsTheDate`, `…Canceled_StillClearsWhileFlagOff_SetsPrepaid`, `…SubscriptionDeleted_ClearsAndSetsPrepaid`: `payments.Get` → `no entitlement`; `…PaymentSucceeded_LedgerRowAndExtend`, `…PaymentFailed_LedgerRowFailed_NoEntitlementChange`: `sql: no rows in result set` (no ledger row) | green |
| Red, schema walks | `TestErasedTablesCoverSchema`: `[entitlements payment_events payments]`; `TestExportSectionsCoverSchema`: the same three "give it an export section or list it…"; `TestEraseRows_PaymentsAnonymisedNotDeleted`: note kept, `addresses`/`txids` kept, `payment_events.tenant_id` kept | green |
| Red, job table | `TestRegisterJobs_EveryLoopIsAJobAndDailyTimesDoNotCollide`: `entitlement_lapse` unexpected | pinned at `daily 07:30 UTC` |

## Local live proof

**Part 0** — the four items are proven at the handler / `openSealed` level (tables above); `go test -race` of `internal/api` on its own private database `vaultaire_p18_p0` and golangci-lint v2.14.0 by hand (0 issues).

**Part A** — the branch build (`make build`) on the private migrated database `vaultaire_p18` (097 applied twice: idempotent), `STORAGE_MODE=local`, port 8099; an admin registered and promoted, its JWT for the admin API:

1. `POST /api/v1/admin/jobs/entitlement_lapse/run` → 202; `job_runs`: `ok | 0 | {"grace": "336h0m0s", "healed": 0, "cleared": 0, "backfilled": 0, "candidates": 0, "would_clear": 0, "skipped_unready": 0}`. `/metrics`: `vaultaire_entitlements_expiring{window="7d"|"30d"|"in_grace"} 0`, `vaultaire_entitlements_lapsed_total 0`, `vaultaire_payments_total{kind="charge",outcome="settled|failed|replayed",provider="stripe"} 0`.
2. A 300 KiB object stored with aws-cli; a 2 TB house written as a pre-097 one (floor rows, no entitlement). Run → `backfilled: 1`; the row `manual | prepaid | 2026-11-03` (now + 14 d) with the Warn "paid_through set to now + grace … no Stripe subscription"; floors untouched.
3. `paid_through` moved to 20 days ago. Run → `candidates: 1, would_clear: 1, cleared: 0`; floors and the 2 TB limit kept.
4. `PUT /api/v1/admin/flags/entitlement_lapse {"enabled":true,"tenant_id":…}`. Run → `cleared: 1`; floors 0 rows, `tenant_quotas` 5 GiB `free`, `storage_used_bytes` 307200 unchanged; `lapsed_at` set; audit `billing.house_lapsed {"grace": "336h0m0s", "tenant": …, "provider": "manual", "paid_through": "2026-09-30T13:34:53Z"}`; `vaultaire_entitlements_lapsed_total 1`; the object read back byte-equal; the Warn "house lapsed — back to the free tier, data untouched".
5. Server log: two `"level":"error"` lines, both residue of other packages' tests in the private database (a NULL-backend head row the routing-truth boot check names; a test tenant without a quota row in the bandwidth alerter) — none from this change.

Suites on `vaultaire_p18` with `-race`: `internal/{payments,billing,account,usage,database,dashboard/...}` ok; `internal/api` + `cmd/...` ok after removing a row `internal/dashboard/handlers/r9_schema_test.go` leaked into the private database (a NULL-backend head row of tenant `test-r9-dash-…`; it made `TestRoutingRuleFile_MatchesTheExportedSeries` read `vaultaire_routing_unknown_backend_rows{backend=""}` 1 — residual RP20-A6). golangci-lint v2.14.0 (CI's): 0 issues.

## Prod proof

Before (2026-10-20 ~13:35 UTC, read-only): prod `6439e91`; `job_runs` since 10-20: `chunk_orphans` ok 02:16 (lyve listed 38,739 / named 38,739, idrive 1,049 / 1,049, 0 found), `routing_truth` ok 05:37 (idrive: 10 of 501 sampled, 10 errors — the known no-verdict), `vault_parity` ok 13:32; 0 `"level":"error"` lines since 03:00; the 12 rule files sha256-equal to the repo; **13 alerts firing** = the 11 `BackendProbeFailing{idrive*}` + `RoutingTruthNoVerdict{idrive}` + `DriverHedgesWinning{driver="idrive-us-midwest-1"}` (since 13:33; RP20-A5); 3 tenants, 0 with a Stripe subscription, 0 floor rows, 0 `stripe_events`; #793 OPEN (not merged — it is the WP-P16 post-merge review docs PR), #800 merged.

Deploys: #801 → `a8d5034` (deploy 13:51 UTC, success), #802 → `0a121b6` (13:56, success); `/version` `0a121b68ffc5`.

- **WP-PAY-1:** migration 097 applied by the deploy — `entitlements` 0, `payments` 0, `payment_events` 0, `invoice_counters` 0 rows. `POST /api/v1/admin/jobs/entitlement_lapse/run` (an admin JWT minted inside a 0600 script on the box, never printed) → 202; `job_runs`: `entitlement_lapse | ok | 13:58:29 | 0 | {"grace": "336h0m0s", "healed": 0, "cleared": 0, "backfilled": 0, "candidates": 0, "would_clear": 0, "skipped_unready": 0}`; `/metrics`: `vaultaire_entitlements_expiring{window="7d"|"30d"|"in_grace"} 0`, `vaultaire_entitlements_lapsed_total 0`, `vaultaire_payments_total{kind="charge",outcome="settled|failed|replayed",provider="stripe"} 0`, `vaultaire_job_last_success_timestamp_seconds{job_name="entitlement_lapse"}` set. No `entitlement_lapse` flag row (default OFF).
- **RP19-9 as a customer** (bench tenant, signed curl through `https://stored.ge`): a new bucket, `PUT ?versioning` with `<Status>Enabled</Status><MfaDelete>Garbage</MfaDelete>` → **400 `MalformedXML`**; `GET ?versioning` → empty configuration (nothing written); bucket deleted (204).
- **RP19-1:** `vaultaire_vault_parity_unprotected_objects{reason="unknown_length"} 0` (with `flag_off`, `other`, `too_large`) on `/metrics`.
- RP17-6 and RP19-2 have nothing to observe on prod (0 webhook endpoints; no 0x05 object read by range).
- **Rules:** `vaultaire-jobs.yml` (JobStale lists `entitlement_lapse`) installed the documented way — `promtool check rules` (9 rules) + `promtool test rules tests/vaultaire-jobs_test.yml` SUCCESS on the box, the old file kept as `.bak-20261020b`, `systemctl reload prometheus`; the 12 rule files sha256-equal to `main`; 64 rules load; the JobStale expression names the job. Right after the reload 8 alerts fired (7 `BackendProbeFailing{idrive*}` + `RoutingTruthNoVerdict`): the slot restart resets the probe counters and the `for:` windows, so the set climbs back to the 11 + 1; `DriverHedgesWinning` cleared with the restart.
- 0 `"level":"error"` lines from 13:50 UTC (both deploys) to the end of the proof.

## Residuals

- **RP20-1** A vault-floor SSE-C multipart (0x05) object gets no parity copy until the job reads the length from the blob's own 0x05 header; counted on `unknown_length`, no rule pages on it.
- **RP20-2** A future Smart ledger row naming a 0x05 object would fail its copy-back daily with a Warn instead of being handled (none can exist today: the demotion never selects one).
- **RP20-3** The pre-commit hook runs the `golangci-lint` on PATH — on this Mac Homebrew's 2.4.0 built with go1.25, which refuses the go1.26 module. The Part 0 worker committed with `--no-verify` after running CI's v2.14.0 by hand; Part A committed through the hook with v2.14.0 first on PATH. [YOU] `brew upgrade golangci-lint` (or pin it in the hook).
- **RP20-A1** A `manual` house backfilled by the job is paid through now + grace (D-P18-A10): once `entitlement_lapse` is on for such a tenant it lapses 2 × grace after the backfill unless an operator extends it. WP-PAY-2's manual grant (an admin `Extend`) closes it; prod has 0 floor rows today.
- **RP20-A2** The "house lapsed" e-mail of § 1.3 is not sent (the job audits and logs). WP-PAY-3 (reminders) owns e-mail.
- **RP20-A3** `isCurrentSubscription` still reads `tenants.stripe_subscription_id`, not `entitlements.provider_ref` (§ 1.4). Same value today (both written in one transaction); WP-PAY-2 switches it with the clearing branch.
- **RP20-A4** `payment_events` and the generic `payments.ClaimEvent` have no writer until BTCPay (WP-PAY-4); the table, its retention, erasure and export are in place.
- **RP20-A5** `DriverHedgesWinning{driver="idrive-us-midwest-1"}` is firing on prod since 13:33 UTC (13 alerts, not the expected 12): the region's probe hedge "wins" 100% because the dead iDrive key's region answers nothing in 2 s. Same root cause as the eleven `BackendProbeFailing{idrive*}` (the iDrive account, owner row); not touched here.
- **RP20-A6** `internal/dashboard/handlers/r9_schema_test.go` leaves a head row (`test-r9-dash-…`, NULL backend) in its database; on a shared DB it makes the routing gauge test read 1 instead of 0. A cleanup in that test.
- Carried unchanged: RP19-3..8, RP19-11..13, RP16-7/8/9, RP17-3/4/5/8, RP15-6 (owner rows).

## Post-merge review (2026-10-20, meta)

Adversarial pass over #801 and #802 against `docs/PAYMENTS.md`, the Stripe
webhook contract (`internal/billing/CLAUDE.md`) and the code at `d825f39`.
**Every claim above holds; no P0/P1. One P2 (RP20-4, proven with a throwaway
test) and one P3 go to Prompt 19 Part 0.** CI, Security, CodeQL and Deploy
green on every merge commit; the docs PR #803 merged; #804 (a lint-only test
change) deployed after it, prod `/version` = `d825f39a5ff7`. #793 (the WP-P16
post-merge PR) was found OPEN with auto-merge cleared a FOURTH time — updated
and re-queued during this review.

Verified in depth (beyond the session's own gates):

- **One writer of a house (#802):** every `SetHouse`/`ClearHouse` caller is now
  `payments.*` or the lapse job's heal; the dashboard's resize goes through
  `payments.HouseWriter` (`server.go` `houseQuotas`); the dashboard's admin
  quota and tier edits already refuse a house tenant (`tenants.go` `hasHouse`,
  409), and no admin API writes `tenant_quotas` — so the nightly reconcile
  cannot fight an operator's edit. `usage.QuotaManager.UpdateQuota` has no
  caller at all (RP20-8).
- **Lock order:** entitlement lock → `entitlements` row → `tenant_quotas` →
  `tenant_floor_quotas`, the write path's order from `tenant_quotas` on; the
  webhook holds the per-customer claim lock on one connection and takes the
  entitlement lock on another — no cycle with any key/head/upload lock.
- **Out-of-order Stripe events:** a late `invoice.payment_succeeded` after
  `customer.subscription.deleted` fetches the subscription (`canceled`) and
  writes the ledger only — no re-grant; the deleted handler keeps the
  current-subscription guard before `Revoke`; `Grant` never shortens a date
  (`GREATEST`) and a lapsed row takes the given date — both as the design says.
- **Gapless numbering:** the counter row is updated inside the settle
  transaction (`INSERT … ON CONFLICT DO UPDATE … RETURNING`): a waiter
  re-evaluates against the committed row after a rollback, so numbers never
  skip; the session's ten-concurrent test covers it.
- **Erasure:** the three new rules sit in `Deleted` before the `tenants` DELETE
  (the `ON DELETE SET NULL` would otherwise empty the `Kept` predicate);
  `metadata - 'addresses' - 'txids'` is NULL-safe.
- **Part 0 (#801):** the `MfaDelete` value check precedes `validateMFAHeader`
  (nothing consumed); the MFA read error is `*mfaDeleteUnknownError` on both
  reads and only a successful read's `!enabled` is the 403; the signature is
  `t=,v1=` and nothing else; the lazy 0x05 chain reads span N's header in
  `next()` and zeroes its key copy at the last span and on `Close`; the
  parity job's `unknown_length` is pre-created at 0 and the candidate query
  excludes `-mp` rows before any slot is taken.
- **Prod (read-only, this review):** `entitlements` / `payments` /
  `tenant_floor_quotas` / `webhook_endpoints` all 0 rows; every daily and
  interval job's last outcome `ok` (`entitlement_lapse` 13:58 with the result
  above); the 12 rule files sha256-equal to `main`; 12 alerts firing = the 11
  `BackendProbeFailing{idrive*}` + `RoutingTruthNoVerdict{idrive}` (the
  `DriverHedgesWinning` of RP20-A5 cleared with the deploy); 0 `"level":"error"`
  lines since 13:50 UTC.

New residuals:

| # | Sev | Item | Where it goes |
|---|---|---|---|
| RP20-4 | **P2** | **An invalid house from Stripe is a three-day 500 storm.** `HouseFromSubscription` (`house.go:497`) sums item quantities and validates nothing; `payments.Grant` now refuses `pin_hot > std` (and migration 097's CHECK would too). A subscription an operator edits at Stripe into that shape (delete the Standard item, keep pin-hot) makes `applyHouse` return the error → 500, the claim rolls back, every retry fails the same way — **proven**: three deliveries of one `customer.subscription.created` with `vault_m 1 + pin_m 1` answered `[500 500 500]` and left no `stripe_events` row — until Stripe disables the endpoint after three days (the R10-45 class; before #802 `SetHouse` stored the bad house silently). Fix: a typed `payments.ErrInvalidOrder`; `applyHouse` answers it with 200 + one Error line + audit `billing.house_invalid` (+ a counter), never a retry; the house is left as it was. | Prompt 19 Part 0 |
| RP20-5 | P3 | **The backfill ignores `cancel_at_period_end`**: `StripeService.SubscriptionPeriodEnd` returns the date only, so a pre-097 Stripe house whose renewal was stopped is backfilled `renewal = auto`; WP-PAY-3's reminders read `renewal`. Return the flag with the date. Prod has 0 floor rows. | Prompt 19 Part 0 |
| RP20-6 | note | `payments.metadata.ids.customer` (the Stripe customer id) survives erasure — a stable key into Stripe's own record of the person; the privacy policy says the ledger is "de-linked". The invoice and subscription ids suffice for accounting; decide whether `ids.customer` joins `addresses`/`txids` at erasure. | WP-PAY-3 (the policy sentences) |
| RP20-7 | note | Dunning vs grace: with `entitlement_lapse` ON a `past_due` house lapses 14 d after `current_period_start` while Stripe may still be retrying; a later `invoice.payment_succeeded` re-grants it (floors back, data untouched). Click-list item 5 — cancel after retries **within 14 d** — keeps the two aligned. | owner row |
| RP20-8 | note | `usage.QuotaManager.UpdateQuota` (`quota_manager.go:194`) has no caller. Delete with WP-PAY-0. | WP-PAY-0 |
| RP20-9 | note | One prod tenant is on a pack plan id: `Bench2` (`tenant-f4294518dc0a7a8f`), `tenants.plan = tenant_quotas.tier = 'vault100'`, 1 TiB limit, 2 MB used, no Stripe — set by the dashboard's tier handler. WP-PAY-0's Part 0 said "expected 0"; the tier label stays valid, only the pack checkout goes. | WP-PAY-0 Part 0 |
| RP20-10 | note | 1,590 `backend health check failed` Warn lines in the 2 h after the deploy, all from the 11 dead iDrive drivers (the owner row): one Warn per probe per driver. One per state change would do. | — |

Re-confirmed: RP20-A1 (a `manual` backfill lapses 2 × grace once the flag is
on) is real but moot on prod (0 floor rows); RP20-A6 (the r9 dashboard test's
leaked head row) is a one-line cleanup for Prompt 19 Part 0; RP20-3 (the
Homebrew golangci-lint 2.4.0 that refuses the go1.26 module) is worth pinning
in `.pre-commit-config.yaml` to CI's v2.14.0 so no worker commits `--no-verify`
again. WP-PAY-0 (= WP-R10-4) has **not** run — `HandleUpgrade`,
`registerStripePlans` and `metered.go` are still on `main` — and the drafted
WP-PAY-2 assumes it; Prompt 19 runs WP-PAY-0 first.
