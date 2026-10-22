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
