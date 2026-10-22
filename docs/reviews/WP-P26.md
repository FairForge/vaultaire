# Stage 1a — Prompt 26: Part 0 (RP31-1…7 on WP-PAY-5) and keystone K3 (the bucket mode column + modes.json, Phase 41.0)

*2026-10-22. PRs: #873 (Part 0), #872 (K3 probe), #874 (K3 PR 2, **migration 102**), #875 (K3 PR 3, readers), this docs PR. Next migration: **103**. No rule-file changes in this prompt.*

## Step 0

- #869 (WP-PAY-5) was merged (03:04 UTC) and deployed `70a72d7` on slot 8001. Prod read 03:15 UTC: 0 `"level":"error"`, 1 warn (the expected `BLOMP_ALLOW_UNSEALED` boot Warn); `renewal_stops` ok (last success 03:09:06); `vaultaire_payment_renewal_stops_pending 0`; `vaultaire_payments_double_period_total` 0 for btcpay / manual / stripe.
- Its two rule files installed on the box: `vaultaire-payments.yml` 40fe3ce0 → **3ab8e79b**, `vaultaire-jobs.yml` dd5c6965 → **9147a86a**. `promtool check rules` (10 + 9 rules) and `promtool test rules` (both test files, copied into one dir) SUCCESS; Prometheus reloaded; **101 rules loaded**.
- `docs/reviews/WP-P25.md` came with #871 (merged 03:30). Its post-merge section (the RP31 table) is added by this PR.
- Migration 102 was free (101 = `sia_promotions`).

## Part 0 (#873) — the WP-P25 review's findings

| Item | Sev | Fix | Red test (mutation) |
|---|---|---|---|
| RP31-1 a | P2 proven | `InvalidateOpenInvoices` fetches each invoice and marks it Invalid only when provably unpaid (`unpaidForInvalid`: New or Expired, `paidAmount` 0). Processing / partly paid invoices are left to settle and counted `invalidate_skipped_paid` (0 from init). A failed fetch marks nothing | `TestPortalUncancel_ProcessingInvoice_LeftToSettle` (rule → true) |
| RP31-1 b | P2 | The `StatusInvalid` branch credits `paidAmount` (`partial:<invoice>`) on a row that never settled | `TestInvoiceInvalid_WithPaidAmount_Credited` (credit → 0) |
| RP31-2 | P3 proven | The detector skips `resize` rows on either side, and a period starting inside `MinDeferredStart` of the other's end. The latter is `Settled.Forfeit`, audited `billing.prepaid_forfeited` {days, until}; no counter, no mail | `TestDoublePeriod_ResizeTopUpNeverMatches`, `TestSwitchToCard_Inside48h_NoDoublePeriod` (resize clause dropped; window → 0) |
| RP31-3 | P3 | The renewal_stop row is read inside the grant transaction after `payments.Lock` (`grantStripe` → `stripeGrant.away`); the re-assert's Stripe call runs after the commit | `TestStripeEvent_ImplicitSwitchCommitsWhileWaiting_NeverTakesTheHouse` (red on main: provider `stripe`; mutation: drop the first Lock) |
| RP31-4 | P3 | `StopRenewalRef` on past_due / unpaid / incomplete cancels now (`invoice_now=false`, `prorate=false`; stripe-go v75.11.0 `subscription.go:633-643`, `subscription/client.go:68-77`) → `ErrRefGone` (row gone) | `TestImplicitSwitch_FromPastDue_CancelsNow` (red on main ×3) |
| RP31-5 | P3 | The `crypto_invoice_open` 409 page offers "Cancel the bitcoin invoice and continue": the provider withdraws only a provably unpaid invoice, marks it Invalid, closes our row under the lock and audits `customer_cancel`; the same request then continues into the refused handler. A paid one keeps the 409 with its date | `TestStripeCheckout_AbandonedUnpaidInvoice_CancelThenProceeds`, `…_ProcessingInvoice_Still409` |
| RP31-6 | P4 | `SwitchToPrepaid`: the row is committed first, Stripe is called after; a failed stop reverts the row under the lock, only if unchanged (`revertSwitch`) | `TestSwitchToPrepaid_CommitFails_StripeNeverStopped` (a deferred constraint trigger fails the COMMIT; red on main), `…_StripeRefuses_RowReverted` |
| RP31-7 | note | With the flag OFF, the clear at Stripe's deletion is pinned; the page (`endsAtDate`) and the reminders (`NoGrace`) say "Your house ends on <date> unless the bitcoin payment arrives" / "unless you renew" | `TestSwitchToBitcoin_FlagOff_UnpaidSwitchClearedAtStripeDeletion` (pins), `TestReminders_PendingBitcoinSwitch_FlagOff_SaysEndsUnlessPaid` (red on main) |
| RP31-8 | carried | Not built (a downsize at period end needs the `next_*` scheduled change, § 1.2; well over ~40 lines). The owner decision line is in PAYMENTS.md § 2.3 | — |

Mutation check: each of the 10 mutations turns its named test red.

## K3 — the bucket mode

**Probe (#872, `docs/reviews/K3-PROBE.md`).** Writers W1–W5 (three creates, all Standard; the dashboard settings UPDATE, the only `public-read` writer; the management PATCH). No S3 path writes either column: `PUT ?acl` refuses public-read. Readers R1–R12; the **name trap** (`visibility = 'private'` printed "Private" = Standard mode) is in R1/R2, and R12 is llms.txt promising `?acl (public-read)`. **Prod truth:** no master key loaded, 0 `tenant_key_versions`, 503 head rows and 34,234 chunk rows unsealed, 7 buckets all Standard. "Encrypted at rest with keys we hold" is false on prod today.

**PR 2 (#874, migration 102).** `buckets.mode` GENERATED STORED from `visibility` + `ssec_required` (the recommended shape: it cannot drift, and the old slot never writes it). `modes.json` (three modes; Standard's sentence carries `requires_master_key` + `without_master_key`), `internal/modes` (`Validate`, `Render`, `FromColumns`, `SetMasterKeyLoaded` from `NewServer`). **The one write door** `modes.SetBucket`: W4 and W5 collapse onto it, and the 096 rule sits in its statement. Export `mode` + `ssec_required`. Tests: `TestModesJSON_ThreeModesNoForbiddenWords`, `TestBucketMode_DerivedFromVisibilityAndSSEC`, `TestModeNamesOnlyFromModesJSON`, `TestBucketModeWritersOnlyThroughTheDoor` (each mutation-checked).

**PR 3 (#875, readers).** Management API `mode` (additive). OpenAPI: `ManagedBucket.mode` enum + text and PutObject's four `x-amz-server-side-encryption*` headers, generated; without the key it says the AES256 write is refused (503) and never "keys we hold". Flag **`modes_copy`** (default OFF): the dashboard bucket badge and the settings Mode card per tenant, and the privacy page's per-mode table when on globally. llms.txt: `?acl` is owner-only, plus the modes line. Tests `TestPrivacyModesTable_NoMasterKey_NeverSaysKeysWeHold` (+ `_MasterKeyLoaded_`, `_FlagOff_NoTable`), `TestBucketLabels_ModesCopy_StandardBucketIsNeverCalledPrivate`, `TestManagementBucket_ReportsMode`, `TestOpenAPI_ModeTextFromModesJSON`, `TestLLMSTxt_…`, `TestFeaturesWithoutADeclaredMode_Count` (= **2**, docs/STATUS.md).

## Departures

- D-P26-1: `recordInvoicePaid` checks switched-away only for an active or trialing subscription (the granting branch); before, a past_due one also re-asserted.
- D-P26-2: the forfeit window applies to any provider pair (a renew inside 48 h is the same designed forfeit), not only the switch to card.
- D-P26-3: the RP31-5 cancel continues the refused request in-process (`next[cancel_then]` with the same form), not a redirect: the customer clicks once.
- D-P26-4: modes.json sentences are objects `{text, requires_master_key, without_master_key}` rather than plain strings, so the marker sits on the sentence it qualifies.
- D-P26-5: the names test cannot ban a bare "Standard" label: it is also the downstairs tier's name. It bans `>Private<` / `>Public<` labels, the sentences, "<name> mode", "keys we hold" and "zero-knowledge", with a counted allow-list.
- D-P26-6: the probe's R4 (the compliance page's "Encryption" ✓/✗ from `sse_enabled`) is left as it is: true on prod (✗ everywhere), and it is not a mode.

## Prod proof

All reads on the box (sudo password on stdin; journal JSON; `psql` as postgres).

- **Part 0, `314a2ff`** (slot 8000, 04:06 UTC): 0 `"level":"error"`, 1 warn (the expected Blomp boot Warn); `vaultaire_payment_webhook_total{btcpay,invalidate_skipped_paid} 0` exported; `vaultaire_payment_renewal_stops_pending 0`; `vaultaire_payments_double_period_total` 0 × 3; `renewal_stops` ok. `pay_btcpay` stays OFF (no `BTCPAY_*`).
- **Migration 102, `b24a1b8`** (slot 8001, 04:12 UTC): `buckets.mode` exists, `is_generated = ALWAYS`, expression = the CASE; `SELECT mode, count(*) FROM buckets GROUP BY 1` → **`standard | 7`**; rows disagreeing with the CASE: **0**.
- **Readers, `66bdf1a`** (slot 8000, 04:49 UTC):
  - `/openapi.json`: the `ManagedBucket.mode` enum is `private, standard, public`. "keys we hold" appears **nowhere** (no master key). The AES256 header text says the write is refused (503).
  - `/llms.txt`: `?acl (owner-only: a public-read ACL is refused …)` + the generated bucket-modes line.
  - `/legal/privacy`: **no** modes table (`modes_copy` is not on globally).
- **Tenant zero** (`tenant-4e1b43062db9d685`, slug `stored`): `modes_copy` set **for that tenant only** through `PUT /api/v1/admin/flags/modes_copy` (200, audited `flag.set`; JWT minted on the box, 10 min, never printed). The global flag stays OFF.
  - Management API on a throwaway empty bucket: create → `mode: standard`; GET → `standard`, `ssec_required false`; PATCH `{"ssec_required": true}` → `mode: private` (row `private | t | private`: written through the door); the list reports `private`.
  - The bucket was deleted (200); tenant zero is back to **0 buckets**.
  - Not proven on prod: the dashboard labels themselves (a session is needed; the template tests render the real templates).
- Journal 04:06 → 04:53 UTC across three switches: **0 errors**. 5 warns = 3 Blomp boot Warns + 2 S3 `HEAD: object not in metadata cache` (a client HEAD of an absent key).

## Residuals (RP32-n; the review continues the numbering)

| # | Sev | What | Where |
|---|---|---|---|
| RP32-1 | P3 | `SwitchToPrepaid` from a past_due Stripe house still uses `cancel_at_period_end` (`StopRenewal`, not RP31-4's ref path), so dunning may retry | next payments prompt |
| RP32-2 | P4 | `invalidate_skipped_paid` and the customer cancel's refusals live on the webhook counter (RP31-11's class) | note |
| RP32-3 | P3 | `modes_copy` is OFF everywhere: the legacy labels (2 files) still print "Private" for Standard until the owner flips it and a follow-up removes the legacy branches | owner flip → cleanup PR |
| RP32-5 | P3 | CI flake (another lane's code): `internal/drivers` `TestSiaWriteBudget_PacingIsNeverAStalledS3d` failed twice on the docs-only #872 (`PutObject … no answer within 50ms, twice`): a 50 ms deadline under CI load. #872 was rebased to re-run it | the Sia lane |
| RP32-4 | note | Private is offered only through SSE-C per bucket; the SDK key core (K2) is the other path in modes.json's `how` sentence ("encrypted on your device"), true for client-side tools today | K2 |

## Owner items (not done here)

- `pay_btcpay` stays OFF and no `BTCPAY_*` in `.env` until RP31-1 is on prod (it is now, with #873; the flip is still yours).
- Flip `modes_copy` globally **after reading the privacy table** (a legal page): `POST /api/v1/admin/flags` `modes_copy` on `'*'`. Per tenant first if you want to see the labels.
- RP31-8: confirm the § 2.3 decision line (a downsize while the provider's flag is off, at period end) or change it.
- WP-P24 click-list items 3–4; the Stripe test-mode key (RP31-9).

## Post-merge review (2026-10-22, meta-review)

State: prod `a2d388f` (#877 on top of #873/#874/#875), slot 8000, 0 `"level":"error"` in the 90 min before the read; `buckets`: `standard | private | f | 7`; `modes_copy` only on `tenant-4e1b43062db9d685`; the 16 rule files on the box match `deploy/monitoring/` hash for hash. No AI co-author trailers. #844 (WP-P23 post-merge) was CLEAN and unarmed for a day; it is in the queue now.

**What holds.** The generated column (cannot drift; the old slot's UPDATEs keep it right); the door's single statement (a concurrent PATCH is re-checked by EvalPlanQual on the row, so public + ssec cannot both land); Complete re-reads `ssec_required` through `decidePlacement`, so an upload created unkeyed before the flip is refused at Complete; direct uploads are public-only. Part 0: RP31-1 a (a failed fetch marks nothing, partly paid New/Expired skipped), RP31-3 (row read after `payments.Lock`, `ErrRefGone` handled by both callers), RP31-4 (cancel now, `invoice_now=false`, `prorate=false`; the following `deleted` no longer names the current subscription), RP31-5 (CSRF, tenant-scoped `OpenInvoiceOf`, the continuation limited to three fixed handlers), RP31-2 / D-P26-2 (only overlaps ≤ 48 h are excluded; a full-period double charge is still caught), `revertSwitch`'s "only if unchanged" guard.

| # | Sev | Finding | Evidence |
|---|---|---|---|
| **RP33-1** | **P2, proven** | A bucket that already holds readable objects can be made **Private**. `PATCH {"ssec_required": true}` (and the dashboard checkbox) flips `buckets.mode` to `private` whatever the bucket holds ("objects written before the flag stay as they were", `management_routes.go`); modes.json then tells the customer "Only you hold the key: we store your files encrypted and cannot read them", which is false for every object written before. Also a race: a PUT that passed `decidePlacement` before the flip commits its unkeyed head row after it. It departs from the plan too (line 1699: the mode is "set at creation through the SDK/API, shown — not toggled"; the departure is not in D-P26-n) | Throwaway test (deleted): PUT `plain.txt` unkeyed → PATCH → `mode=private` → GET with no key **200 "readable-by-us"** |
| **RP33-2** | **P2** | Unconfirmed bitcoin becomes value. `paidAmount` counts unconfirmed payments (Processing = paid in full, unconfirmed). (a) #873's Invalid branch credits it, and PAYMENTS.md § 3.3 line 492 itself says Invalid = "the monitoring window of 24 h passed without confirmation: a double-spend, an RBF replacement, a dropped transaction" → pay with a low-fee transaction, wait 24 h, collect the credit, RBF the coins back. (b) The Expired branch (WP-PAY-4) credits an unconfirmed partial the same way, and (c) marks an Expired invoice Settled when an unconfirmed late payment reaches the amount → a grant on money that may never confirm. Dark (`pay_btcpay` OFF); a gate on the owner's flip | Code + § 3.3; no lab capture of a confirmed-vs-unconfirmed Invalid |
| **RP33-3** | P3, proven | `SwitchToPrepaid` runs the Stripe stop and `revertSwitch` under the request's context: a client that leaves during the Stripe call fails both, so the row stays `prepaid` / pending `btcpay` while Stripe renews on auto. A later settle records the stop as `stopped` and nobody stops Stripe → card charged for the period bitcoin paid (RP31-6's own state) | Throwaway `TestReview_SwitchToPrepaid_ClientGoneDuringStop_RowNotReverted`: `expected "auto" actual "prepaid"` |
| RP33-4 | P3 | The one door writes no audit row: a bucket made Public (readable by the world) or taken out of Private leaves no trace in `audit_logs` | `internal/modes/bucket.go`, `bucket_settings.go` |
| RP33-5 | P3 | No S3 reader tells the truth about a Public bucket: `GET ?acl` answers owner-only FULL_CONTROL for a `public-read` bucket, and there is no `?policyStatus`. S3 tooling and scanners read a public bucket as private | `s3_acl.go handleGetBucketAcl` |
| RP33-6 | P3 | Standard's pre-key sentence is not true on every backend. "Encrypted in transit" is false for `blomp` (plain-HTTP storage URL; `BLOMP_ALLOW_UNSEALED=true` on prod; 2 head rows there today). "Under the providers' own encryption" is unverified for Geyser tape, Blomp and `local`. It must be true per backend before the owner flips `modes_copy` globally | K3-PROBE "the vendor's statement, not verified by us" |
| RP33-7 | P3 | = RP32-1, confirmed: `SwitchToPrepaid` from a past_due house uses `cancel_at_period_end`; dunning success → `grantStripe` `away` with the stop row `stopped` → full-period overlap, refund by hand | `billing/provider.go:162` |
| RP33-8 | P4 | (a) Process death between the commit and the Stripe stop in `SwitchToPrepaid`: same end state as RP33-3 if the bitcoin settles first. (b) `CancelUnpaidInvoice`: BTCPay marked Invalid, then our transaction fails → "Nothing was changed" (the webhook closes the row later) | reasoned |
| RP33-9 | note | Stripe `billing_reason=subscription_update` is filed `resize` and skipped by the detector; a portal configured for plan/interval changes would bill a full period the detector never sees. Pin the portal configuration or count it | `webhook_ledger.go:32` |
| RP33-10 | note | Public → Standard leaves edge copies at Cloudflare for up to `cache_max_age_secs` (≤ 86,400) + 600 s (`cdn.go:125`, no purge); per-object SSE-C in a Standard bucket is understated by a bucket-level label | |
| RP33-11 | note | "Money paid after a mark is credited" (btcpay/CLAUDE.md) is unverified: BTCPay may stop monitoring an Invalid invoice | no lab capture |

RP32-2…5 carried as the worker wrote them. Next migration **103**. → Prompt 27 = Part 0 (RP33-1…8) + keystone K4 (the transparency-log library).
