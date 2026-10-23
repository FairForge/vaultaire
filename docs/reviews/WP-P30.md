# Stage 1a — Prompt 30: Part 0 only (the WP-P27/WP-P28 review's RP37)

*2026-10-23. PRs: #902 (Part 0a, payments), #904 (Part 0b, the transparency log), #899 (Part 0c, the Private sentence and the ops leftovers), and this docs PR. **No migration.** **Next migration: 104.** No `sdk/` file and no recovery file was touched (Prompt 29 / 31 ran beside this one).*

Three sentences carry this prompt:
- **Only confirmed bitcoin is value, and money paid later is never lost** (0a).
- **A `server_build` entry can be recomputed by anyone, and the log holds no free text** (0b).
- **The Private sentence is true for both ways a bucket is Private** (0c).

## Step 0

- #898 (this review) was in the merge queue at the start. It merged before #902 was queued.
- Prod baseline (2026-10-22 ~23:50 UTC):
  - build `b58a53357b63` (go1.26.10), with 0 `"level":"error"` in the previous 2 h;
  - `pay_btcpay`: no row (OFF), and 0 BTCPay rows in `payments`;
  - `translog_entries` = 9, all `server_build`, **0 `key_ceremony`**, 0 checkpoints, and no `translog_public` row;
  - the 16 rule files on the box match `deploy/monitoring/` hash for hash.

## Part 0a (#902) — payments

| Item | Sev | Fix | Red test on origin/main `b58a5335` | Mutation → caught by |
|---|---|---|---|---|
| RP37-1 + RP37-2 | P2 proven + P3 proven | The overpaid credit of a Settled invoice is `max(0, confirmed − amount)`. It is written on **every** delivery whose row is settled, not only on the new settle: `SetCredit` is absolute under `overpaid:<invoice>`, so a replay re-asserts it and a payment that arrives or confirms later raises it. Order inside the branch: the grant and `Settle` exactly as before, then the `partial:` credit zeroed once (new settle only), then the overpaid credit. A refunded row is never credited again (D-P30-1) | `TestSettled_UnconfirmedExtraPayment_NoCredit` (`Should be zero, but was 500`); `TestSettled_ExtraPaymentConfirmsLater_CreditedOnTheLaterDelivery` (`expected: 500 actual: 0`); guards green on main: `TestSettled_Replay_CreditUnchanged`, `TestSettled_RefundedRow_NoOverpaidCredit` | M1 overpaid from `paid` → `…UnconfirmedExtraPayment…`, `…ConfirmsLater…`; M2 overpaid only on the new settle → `…ConfirmsLater…`; M3 refunded credited → `…RefundedRow…` |
| D-P30-1 (spent credit) | RP34-3's class | `payments.SetCredit` never takes back credit a checkout already spent. A lowering that would take the tenant's raw balance below 0 keeps the spent part on the row and records `metadata.spent_kept_cents` and `recomputed_cents`. `btcpay.logKept` then writes an Error line naming the credit row, on all three credit branches. A later raise drops the marks | `TestSetCredit_LoweredBelowWhatWasSpent_KeepsTheSpentPartAndSaysSo` (raw balance `-500`; row `expected: 500 actual: 0`; `spent_kept_cents` nil) | M6 no floor → the same test |
| RP37-6b | P4 | The Settled grant gate reads `confirmed`, not `paid`, for a row not yet settled. A hand mark over a Processing payment is `settled_unpaid`, counted and paged by the existing `BTCPayPaymentNotApplied`. The `InvoicePaymentSettled` of its confirmation grants it | `TestSettled_HandMarkedOverProcessing_NotGranted` (`Should be false` — granted; status `settled`; `settled_unpaid` `expected: 1 actual: 0`) | M4 gate on `paid` → the same test |
| RP37-6a | P4 | `paidAmount > 0` with no `payments[]` in any method produces one Error line naming the invoice and `vaultaire_btcpay_unconfirmable_total` (unlabelled, 0 from registration). New rule `BTCPayPaymentUnconfirmable` (page) in `vaultaire-payments.yml`, with its promtool test. No credit is invented | `TestConfirmedCents_NoPaymentsArray_ErrorLineAndCounter` (`vaultaire_btcpay_unconfirmable_total is not exported`) | M5 no signal → the same test |
| RP37-3 | P3 | `payments.EndByStatusOf` is used by both Stripe terminal-event call sites (`webhook.go`). Its condition: the entitlement still names the ended subscription, is `prepaid` with a pending provider, and a `renewal_stop` row of reason `switch` names the ref. Then the end is the customer's own switch and nothing is ended (`Ended.Switched`, an Info line). `EndByStatus` itself and every other path are unchanged (D-P30-2). The cancel-now path also **voids the subscription's open invoices** (`voidOpenInvoices`: `invoice.List(subscription, status=open)` + `invoice.VoidInvoice`). A void that did not finish is retried: the next pass finds the subscription `canceled` and voids what is left before closing the row `gone` | `TestSwitchToPrepaid_FromPastDue_PaidThroughInThePast_HouseKept` (`Should be true` — house gone; `lapsed_at` set); `TestCancelNow_VoidsTheOpenPastDueInvoice` (`voided … actual: []string(nil)`); `TestCancelNow_VoidFailed_FinishedByTheNextPass` | M7 no switch keep → `…HouseKept`; M8 no void on cancel-now → `…VoidsTheOpenPastDueInvoice`; M9 no void on canceled → `…FinishedByTheNextPass` |
| RP37-5 | P4 | `StopPendingRenewals` → `stopOne`. Under `payments.Lock(tenant)`, held across the provider call (`stopCallTimeout`, 15 s), it re-reads the row (`RenewalStopOf`) and the entitlement. A row withdrawn or closed meanwhile, or one whose subscription the house renews by again, is skipped with no call (`StopRun.SkippedWithdrawn`). A permanent refusal (`ErrProviderRefused`) of a `switch` row reverts the switch through `revertSwitch` (the "only if unchanged" guard; the stop is withdrawn with it), with an Error line and `StopRun.Reverted`. `MarkRenewalStop` now takes a `*sql.Tx` or a `*sql.DB` | `TestStopPendingRenewals_RowWithdrawnMidRun_NoStripeCall` (`refs ["sub_a","sub_b"]`, `Stopped 2`); `TestStopPendingRenewals_PermanentRefusal_RevertsTheSwitch` (`renewal "prepaid"`, pending `btcpay`, the stop row still there) | M10 no re-read skip → `…RowWithdrawnMidRun…`; M11 no revert → `…PermanentRefusal…` |
| RP37-7 | note | `internal/billing/CLAUDE.md` § Environment now carries the restricted-key recipe. It is derived from the stripe-go packages the code imports, and it includes **"Customer portal: read"** (= RP34-4) and Invoices Write (the void) | — | — |

**How the red runs were done.** The new test files ran against `b58a5335`'s code. The only stubs were the two new `StopRun` fields (`SkippedWithdrawn`, `Reverted`), so that the payments tests compile. All 11 mutations turned their named tests red (`git checkout` restore after each, on a committed tree).

**Stripe's documentation (RP37-3's open question), read 2026-10-22:**
- docs.stripe.com/billing/subscriptions/cancel: *"When you cancel a subscription, all `open` and `draft` invoices for that subscription have their `auto_advance` property set to `false`. This pauses automatic collection for these invoices and prevents automatic reminder emails from sending. You can still manually attempt to collect payment and send emails."*
- docs.stripe.com/api/subscriptions/cancel: *"By default, upon subscription cancellation, Stripe stops automatic collection of all finalized invoices for the customer. This is intended to prevent unexpected payment attempts after the customer has canceled a subscription. However, you can resume automatic collection of the invoices manually after subscription cancellation to have us proceed."*
- docs.stripe.com/api/invoices/void: *"Mark a finalized invoice as void. This cannot be undone. … maintains a papertrail where the invoice can still be found."*

So Smart Retries stop at the cancel, but the open past_due invoice **stays open and payable by hand**: the dunning mails link to it. Neither page says whether the hosted page accepts a payment, so we void it. A card payment after the customer chose bitcoin would be the double charge § 4.4 forbids.

**Departures (0a).**
- **D-P30-1:** a refunded row is never credited again. A later payment to a refunded invoice is the refunding operator's to look at; the overpaid credit row stays exactly as it was. A credit recomputed below what checkout already spent keeps the spent part, records it on the row and in an Error line, and never leaves a silently negative balance. **This closes RP34-3's "silently" half, not its economics:** the double use (a `partial:` credit spent at checkout, then the same money settles its own invoice late) is now visible and recorded. Whether to claw it back stays an operator decision.
- **D-P30-2:** RP37-3 is decided as "the deletion webhook sees the switch's own stop and ends nothing" (`EndByStatusOf`), not as a grace recorded on the entitlement.
  - The house ends at the earlier of the bitcoin invoice's settlement (which extends it from now, since `paid_through` is past) and `paid_through` + `ENTITLEMENT_GRACE`, enforced by the `entitlement_lapse` job.
  - With that flag OFF (the default), the house is counted `would_clear` like every other kept house (RP26-1). That is the existing flag-OFF contract, not a new hole.
- **D-P30-3:** the stop job holds the tenant's entitlement lock across the Stripe call, bounded to 15 s. Every writer that withdraws a stop (the portal race, `revertSwitch`) takes the same lock, so none can slip between the read and the call. A Stripe webhook for that tenant waits at most those 15 s.
- **D-P30-4:** the canceled branch of `StopRenewalRef` also voids the subscription's open invoices. Every caller of `StopRenewalRef` is a switch context (`SwitchToPrepaid`, the job, `afterSwitchedAway`), so a void there is always right.

## Part 0b (#904) — the transparency log's honesty

| Item | Sev | Fix | Red test on origin/main `b58a5335` | Mutation → caught by |
|---|---|---|---|---|
| RP37-9 | P3 proven on prod | **One recipe**, `scripts/build-release.sh`, used by `deploy.yml`, `make build` and the CI smoke test. It sets `GOTOOLCHAIN` from go.mod's `toolchain` (go1.26.10), `CGO_ENABLED=0`, `-trimpath`, `-buildvcs=false` and `GOAMD64=v1`. `BuildSHA` = 12 hex of the commit; `BuildDate` = the **commit time** in UTC, so `/version`'s `build` reads as the commit time (`CLAUDE.md`, `docs/DEPLOY.md`). The check is `scripts/repro-check.sh` (`make repro-check`): two builds from `git archive HEAD` into fresh directories, each with its own GOCACHE/GOPATH/GOMODCACHE, 3 s apart, compared with each other and with the artifact. `docs/TRANSPARENCY.md` § Recompute a `server_build` entry | `repro-check` on deploy.yml:57's old recipe: `7b4ff90f…` ≠ `d5af225a…`, "the two builds differ", exit 1. New recipe: `0d39eb7e…` twice | M9 drop `-trimpath` → `3f4eb348…` ≠ `66896dd9…`, exit 1 |
| RP37-10 | P3 proven | `KeyCeremony` holds only structured fields. `action` comes from a closed list: `key_generated`, `key_installed`, `key_rewrapped`, `key_rotated`, `key_retired`, `escrow_split`, `escrow_tested`, `realm_enrolled`, `log_restored`. `key_role` ∈ {`master_key`, `translog_signing_key`, `recovery_realm_key`}. Then `key_fingerprint`, `participants`, `shares`/`threshold`, `signed_size`/`restored_size`/`new_origin` (for `log_restored` only), and a `reference` that is empty or `https://github.com/FairForge/vaultaire/` + `pull/<n>` \| `commit/<40 hex>` \| `blob/<40 hex>/docs/<NAME>.md`. **`statement` is removed**; `ParseKeyCeremony`, `Decode` and the route all decode strictly (`DisallowUnknownFields`). `entries.go:18`'s claim — no entry can name a tenant, a person or an e-mail — is now true as written (the one exception is D-P30-13) | `TestCeremonyEntry_RefusesFreeText`: main `accepted "Installed by Isaac Viera with Jane Doe of 12 Main Street"`, `accepted "Installed by isaacv17 at gmail dot com"`, `accepted "Restored for 4e1b43062db9d685"` | M1 no `DisallowUnknownFields`, M2 reference loosened to `^https://` → `RefusesFreeText`; M8 lenient route decoder → `TestCeremonyRoute_InvalidEntry_CodeNotBurned` |
| RP37-11 | P3 | `logstore.WithHighwater(TRANSLOG_HIGHWATER_FILE)`, default `/opt/vaultaire/data/translog/highwater` (absolute only). It holds the C2SP checkpoint body, written 0600 and atomically (temp file, fsync, rename, directory fsync) **after** the checkpoint row. Every pass proves the log extends both the newest DB checkpoint and the file's, even with nothing new appended (D-P30-15). `vaultaire_translog_checkpoint_refusals_total{reason=rewritten\|behind_highwater\|highwater_unreadable}` is pre-created. Rule `TranslogCheckpointRefused` (`increase[30m] > 0`, **page**) has a promtool test. An absent file is "first run" (a note); an unreadable or malformed one is a refusal. The restore procedure is in `docs/TRANSPARENCY.md` § After a database restore, with a pointer in `docs/DEPLOY.md` § Backups | `TestCheckpoint_DatabaseRestoredBehindHighwater_RefusesToSign`: `expected: 0 actual: 1 — the key signed a second tree at size 5 (signed=true)`; `TestCheckpoint_HighwaterUnreadable_Refuses` (red by absence) | M3 no behind check, M4 file not written, M6 no proof against the file → `…RestoredBehind…`; M5 unreadable treated as absent → `…HighwaterUnreadable…`; M11 refusal not counted → both; M10 severity warning → `TestTranslogRules_ParseAndNameExportedSeries` |
| RP37-13 | P4 | `logstore/logstoretest.PrivateLog(t)` gives a test its own log: a throwaway database holding only migration 103, dropped at cleanup, and not linked into `cmd/vaultaire` (`go list -deps` count 0). The two signing tests use it and assert exactly. The rest assert only on their own entries | Main's test binaries, `-count=20 -race`, beside another process appending + signing in a loop: **api 6/20 FAIL, logstore 2/20 FAIL**, `note has no verifiable signatures` | The same proof after the fix: **20/20 PASS** each (the loop signed 6,375 checkpoints across both runs) |
| RP37-14 | P4 | The entry is decoded strictly and validated (`Encode`) **before** the MFA check and the TOTP consumption. An `Idempotency-Key` (≤ 256) becomes `RetryableCeremony(entry, sha256(actor‖key))`, the log's own dedup inside Append's transaction. A replay answers 201 with the same body and `Idempotency-Replayed: true`, needs no fresh code and appends nothing; the same key with another entry is 409 (D-P30-11). The route has its own admin gate, which **audits** a non-admin's 403; the comment is now true | `TestCeremonyRoute_InvalidEntry_CodeNotBurned` (`expected: 201 actual: 401 — mfa_code: wrong, expired or already used`); `TestCeremonyRoute_IdempotentRetry_OneEntry` (`expected: 201 actual: 401`) | M7 append without the dedup key → `…IdempotentRetry…`; M12 non-admin audit removed → `TestTranslogCeremonyRoute_AdminMFAAudited` |

All 12 mutations were caught; M4 and M12 were rewritten once to compile.

**Departures (0b).**
- **D-P30-10:** `entryVersion` stays 1. #884's `key_ceremony` v1 schema was replaced before any ceremony entry existed (Step 0: 0 on prod). `server_build` bodies are unchanged, so entries 0–8 still decode.
- **D-P30-11:** the ceremony route's idempotency is the log's own dedup key, not the `idempotency_cache` middleware. It commits atomically with the append, so a crash after the commit cannot produce a second append. It never expires. The key is scoped to the admin who sent it and lives only in `translog_entries.dedup_key`, never in the public body.
- **D-P30-12:** the reproducibility gate runs in **Deploy's build job** on every non-docs push to `main`: three identical hashes (two fresh builds + the artifact) before the upload. It does not run on PRs (≈ 5 min each), so a regression shows after the merge and fails that deploy.
- **D-P30-13:** a fixed-length hex field (`key_fingerprint`, a 40-hex commit) can still carry a deliberately padded hex string. A bare 16-hex tenant id fits no field, and every append is audited with its actor.
- **D-P30-14, the restore procedure, decided:**
  - If the restored database holds M entries and the high-water file says the key signed N > M, the log continues under a **new origin** (`stored.cloud/translog/v2`) with a new key.
  - Its first entry is `log_restored` {`signed_size` N, `restored_size` M, `new_origin: true`}.
  - The old key is never loaded again.
  - The origin can only be kept by a byte-exact re-append of the lost entries, and no tool does that today.
  - A lost box (no high-water file) is treated the same way, unless a witness holds a checkpoint the restored log extends.
  - **The owner confirms the cost: F-33.**
- **D-P30-15:** every pass re-proves both signed trees, not only when the log grew, so a restore refuses (and pages) before the first new entry.

## Part 0c (#899) — the Private sentence and the ops leftovers

| Item | Sev | Fix | Red test on origin/main `b58a5335` | Mutation → caught by |
|---|---|---|---|---|
| RP37-4 | P3 live | modes.json Private `customer` and `server_holds` are rewritten (texts below) so that both are true for SSE-C and for client-side encryption. `modes.UnqualifiedCannotRead` is enforced in `modes.Validate` (a bad file panics at load): "cannot/can't read" or "cannot/can't decrypt" must come with "at rest" or a client-side clause ("on your device", "client-side", "before they are uploaded") | `TestModesJSON_NoUnqualifiedCannotRead`: `Should be empty, but was [Your files as ciphertext we cannot decrypt — … Only you hold the key: we store your files encrypted and cannot read them, …]`; `Validate refuses an unqualified 'cannot read'`: `An error is expected but got nil.` | M1 "at rest" dropped → load panic in `TestModesJSON_NoUnqualifiedCannotRead` |
| RP37-8 | P4 | A new modes.json `deleted_bytes` sentence, rendered in the privacy page's Retention list as "Deleted and replaced files" behind `modes_copy` (`handlers.privacyDeletedBytes`); with the flag off nothing is shown | `TestPrivacyRetention_ModesCopy_SaysDeletedBytesOutliveTheDelete`, `TestModesJSON_DeletedBytesSentence` (build failure on main) | M2 template line dropped → `…SaysDeletedBytesOutliveTheDelete` |
| RP37-17 | note → P4 fixed | `keep_firing_for: 5m` on `BackendProbeFailing` and `PrimaryBackendProbeFailing`, so a slot switch's one healthy sample no longer resolves an outage | `promtool test rules vaultaire-backends_test.yml` replaying the real 2026-10-22 pattern: FAILED at 27m30s and 28m (`got:[]`) | M3 `keep_firing_for` removed → the same three evals FAILED |

**The Private texts.**
- **Customer, old:** "Only you hold the key: we store your files encrypted and cannot read them, and cannot recover them if the key is lost."
- **Customer, new:** "Only you keep the key: we store your files encrypted and never store the key, so we cannot read them at rest or recover them if the key is lost."
- **server_holds, old:** "Your files as ciphertext we cannot decrypt — the key arrives with each request and is never stored. We still see object names, sizes, counts, timestamps and when they are read."
- **server_holds, new:** "Your files as ciphertext. The key is never stored: with SSE-C it reaches our server with each request, which uses it for that request only; with client-side encryption it never reaches us. We still see object names, sizes, counts, timestamps and when they are read."
- **deleted_bytes (new):** "when you delete or overwrite a file, its stored bytes stay with our storage provider for up to a day before our collector erases them (the deduplicated blocks of a large file, up to seven days after no file uses them), and a copy that reached tape is not erased from the tape by a delete. Changing a bucket's mode does not re-encrypt or erase what was stored before the change."

**RP37-17, what really happened** (journal, Prometheus and the ntfy bridge on the box).
- **The probe did not fail for two hours straight.**
  - 17:38:43–17:42:43 UTC was a four-failure blip.
  - Then it was quiet for 1,496 s.
  - From 18:07:39 to 19:38:56 the signed HeadBucket failed every ~30 s: 184 failures, 188 in total.
- **`BackendProbeFailing{backend="geyser"}` fired and reached ntfy.**
  - Pending at 17:40, firing at 18:13:39 (ntfy 200).
  - **Resolved at 19:13:39 in the middle of the outage:** the slot switch at 19:10:39 exported `vaultaire_backend_health{geyser}` = 1 for one scrape, because `RegisterBackend` seeds every backend Healthy until its first probe.
  - Fired again at 19:16:24 and resolved at 19:41:24.
  - There were no silences.
- The premise in the review was therefore wrong (D-P30-23). The fix is `keep_firing_for`, not the `for:` clause and not the two-in-a-row rule.
- **No Vault write failed in the window:** `object_head_cache` has 0 rows on `geyser` and none on the vault floor, `vault_parity` has 0 rows, and `s3_access_log` for 17:38–19:40 holds 4 `HeadObject 404` and 0 statuses ≥ 500.

**Geyser LA1** (`stored3lib-632df558-…`; `GEYSER_ENDPOINT` is unset on prod, so LA1 is the driver's default):
- **Before:** three DeleteMarkers, `VersionId "null"`, `IsLatest`: `deltest-20261022/la1/m4` (20:55:26.733Z), `…/m6-never` (20:55:30.757Z), `…/m8` (20:55:39.783Z).
- **Delete:** `delete-object --version-id null` on each answered `{"DeleteMarker": true, "VersionId": "null"}`.
- **After:** `{"RequestCharged": null, "Prefix": "deltest-20261022/"}`, which is empty. Nothing else on Geyser was touched.

**Departures (0c).**
- **D-P30-20:** the Private label is not split. Both sentences are true for SSE-C and for client-side encryption. Contract line 1 ("keys exist only on the customer's devices") is still untrue of SSE-C Private, and that wording is the owner's.
- **D-P30-21:** the "cannot read" rule lives in `modes.Validate`, not only in a test.
- **D-P30-22:** RP37-8's sentence is in modes.json behind `modes_copy`, so the owner reads it before the global flip. It is not in the always-on policy text.
- **D-P30-23:** RP37-17's premise was wrong (above).

## Checks

- `make lint` and `make fmt` are clean on all three branches. The CI **Security gosec job** was green on #902, #904 and #899 before each was armed (RP35-6: the Mac's gosec is not trusted).
- Race runs:
  - `go test -race` on `./internal/payments/... ./internal/billing/... ./internal/dashboard/...` and the api payment/rule tests (DB `vaultaire_p30`);
  - `./internal/api/ ./internal/translog/... ./cmd/...` (DB `vaultaire_p30b`);
  - the modes and dashboard packages (DB `vaultaire_p30c`).
- RP37-13's proof is in the 0b table: `-count=20 -race` beside an appending + signing process, 20/20 after the fix and 6/20 + 2/20 failures before it.
- promtool on the box: `vaultaire-payments_test.yml`, `vaultaire-translog_test.yml` and `vaultaire-backends_test.yml` all SUCCESS.
- No `Co-Authored-By` or AI trailer in any commit of #899, #902 or #904.
- #902 was CLEAN and armed but not queued once; it was re-armed (`--disable-auto`, `--auto`).

## Prod proof

- **#899** merged as `aeb82d13`; its Deploy started by itself. 0 error lines in the 15 min after it. `curl -s https://stored.ge/openapi.json | grep -o 'Private: [^"]*'`:
  ```
  Private: Only you keep the key: we store your files encrypted and never store the key, so we cannot read them at rest or recover them if the key is lost. `standard` — Standard: Not encrypted by us at rest yet: …
  Private: Only you keep the key: we store your files encrypted and never store the key, so we cannot read them at rest or recover them if the key is lost.
  ```
  `vaultaire-backends.yml` was installed (`install -m 0644`, reload): sha256 `60ef3ec30a7178f7cea7e839cc07921a2a2d7f8912b649a3a3b1bff4c15ef5a8` (= repo); test file `0256a4df8856b16f177590a1103fcb61a49f686b4189d7be6ea83cdcc0d0ae45`. The rules API shows `keepFiringFor` 300 on both rules.
- **#902** merged as `4dd4f2ed`; Deploy run 39878300832 started by itself.
  - `/version` `{"build":"2026-10-23T00:28:39Z","version":"4dd4f2edfd30"}`, with 0 error lines since 00:28 UTC.
  - `pay_btcpay`: no row (OFF); 0 BTCPay rows (**nothing in the payments tables changed**); `vaultaire_btcpay_unconfirmable_total 0`.
  - `vaultaire-payments.yml` installed: sha256 `9cbdfbff8a8af3a5b452d7bfff6437f0a75e44044b50e1f79fb7c140229874e8` (= repo); test file `f5723343020fb76eb22f3000b753e3292d645e9938c078a049aa4466e82ab07f`; backup `.bak-20261023-p30`; `BTCPayPaymentUnconfirmable` health `ok`.
- **#904** merged as `17aaaef9`.
  - Deploy run 39879378307's "Reproducible build" step: build a = build b = artifact = `316213304c1cb248e6dcb31a4a1e9eab3a180553e4b004b719838a5f21ba1191`.
  - `/version` `{"build":"2026-10-23T00:28:49Z","version":"17aaaef90a68"}`. The `build` field is the commit time.
  - **Entry 12** = `server_build` `17aaaef90a68`, `binary_sha256` `316213304c…`, which equals slot 8001's binary.
  - **Recomputed on the Mac** from a fresh `git clone` at `17aaaef90a68` with `GOTOOLCHAIN=go1.26.10 GOOS=linux GOARCH=amd64 scripts/build-release.sh`: `316213304c1cb248e6dcb31a4a1e9eab3a180553e4b004b719838a5f21ba1191`. That is equal to the entry, the CI build and the deployed binary.
  - **Dedup on a double build:** `592ec32e` (#901) was deployed by its push and then deployed again by dispatch (run 39880078390). Both artifacts are `117077036ecd5a7946543e39416d814a8e74b0e1d2187286e1b2dd74b466d750`, and the second boot logged `server build already in the log index 13`: **nothing appended**.
  - `translog_entries` went 9 → 14:
    - idx 9 `245df639` (#900);
    - idx 10 `aeb82d13` (#899);
    - idx 11 `4dd4f2ed` (#902);
    - idx 12 `17aaaef9` (this PR's);
    - idx 13 `592ec32e`.
  - Entries 0–8 are untouched; they are history. **Entries 0–11 were built with the old recipe and cannot be recomputed by anyone** (two binaries each for `c2a7fa698eb2` and `d464454ea785`). Entry 12 is the first recomputable one.
  - Boot line: `translog high-water file path=/opt/vaultaire/data/translog/highwater`. The file does not exist yet, since there is no signing key and nothing was signed. The three refusal series are at 0; 0 error lines in 30 min.
  - `vaultaire-translog.yml` installed: sha256 `d8c30958b62510cd4c7a9b15b61235e420bed71590eb0279f0606a5857118572`; test file `8cfa20a38f06377db587b16468d7ab0f440000a08abad70d02d0157d1dd3b013`. Loaded: `TranslogCheckpointStale`, `TranslogCheckpointRefused`, `TranslogAppendFailing`.

## Residuals (RP38-n)

| # | Sev | What | Where |
|---|---|---|---|
| RP38-1 | P4 | A new slot exports `vaultaire_backend_health` = 1 for every backend before its first probe (`RegisterBackend` seeds Healthy). The rules now ride over it (`keep_firing_for`), but the gauge reports an unknown state as healthy; the app could export no sample until the first probe | engine / api |
| RP38-2 | note | `BackendProbeFlapping`'s `unless health == 0` lets the same one-sample blip raise a warning in a hard outage (19:11:09 UTC) | monitoring |
| RP38-3 | note | `GEYSER_ENDPOINT` is unset on prod, so LA1 is the implicit default | ops |
| RP38-4 | note | `translog_entries` 0–11 are unrecomputable (the old recipe); idx 12 (`17aaaef9`) is the first entry anyone can recompute | history |
| RP38-5 | P4 | Deleting the high-water file reopens "first run". It is not in the backups, so losing the box loses it too (D-P30-14). Only an external witness (K4b) closes this | K4b |
| RP38-6 | P4 | No tool re-appends lost entries byte for byte, which is the only way to keep the origin after a restore | K4b |
| RP38-7 | note | A deliberately padded hex value still fits the fixed-length hex fields (D-P30-13) | translog |
| RP38-8 | note | The reproducible-build gate runs at deploy, not on PRs (D-P30-12) | CI |
| RP38-9 | note | A BTCPay row refunded and then paid again keeps its money uncredited by design (D-P30-1); the operator who refunded decides | payments |

**Carried:**
- RP34-2, RP34-5, RP34-8 (owner, F-14), RP34-10, RP34-12 (owner: rotate tenant zero's primary).
- RP34-3: its "silently" half is closed by D-P30-1; the double use itself stays an operator decision.
- RP35-1…5.
- RP37-12 and RP37-15 are Prompt 31's (K2b; #900 pinned the sealed box's wire).
- RP37-16 (design notes).
- Prompt 29's RP36-*, as its note lists them.

## Owner items (note only, not done here)

- **F-33:** confirm the restore policy (D-P30-14): a database restore behind the high-water file means a new log origin `v2`, a new key and a re-pin.
- **The K4 signing-key ceremony** and publishing the verifier key. The ceremony commands in `docs/TRANSPARENCY.md` now use the new fields and an `Idempotency-Key`.
- **RB-3:** `BLOMP_ALLOW_UNSEALED` off before any customer.
- **F-32** (from WP-P27).
- **`modes_copy` global flip:** RP37-4 has shipped. First read the privacy page's new "Deleted and replaced files" sentence and the per-mode table.
- **Contract line 1** says Private = "keys exist only on the customer's devices", which SSE-C Private is not (D-P30-20). Keep the wording and add SSE-C, or reword it.
- **`la2rot2`:** does #892's test bucket on LA2 stay?
- `pay_btcpay` stays OFF until you flip it.
