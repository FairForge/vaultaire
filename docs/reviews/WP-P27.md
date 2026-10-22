# Stage 1a — Prompt 27: Part 0 (the WP-P26 review's RP33-1…11)

*2026-10-22. PRs: #881 (Part 0b, payments), #882 (Part 0a, bucket modes), this docs PR. **No migration** (Prompt 28 / K4 took 103 in parallel). **Next migration: 104.** No rule-file changes.*

Two sentences carry this prompt. **A bucket's mode tells the truth about the bytes already in it** (0a). **Unconfirmed bitcoin is never value** (0b).

## Step 0

- #878 (the WP-P26 review) and #844 (the WP-P23 post-merge review) were MERGED before work started.
- Prod baseline: build `a2d388f` on slot 8000, with 0 `"level":"error"` in the previous 2 h. Every bucket was `standard` (7 rows). `modes_copy` was set only for tenant zero (`tenant-4e1b43062db9d685`), and `pay_btcpay` had no rows, so it was OFF.

## Part 0a (#882) — bucket modes

| Item | Sev | Fix | Red test on origin/main `8ab12e7d` | Mutation → caught by |
|---|---|---|---|---|
| RP33-1 a | P2 proven | `modes.SetBucket` takes `pg_advisory_xact_lock(hashtext('bucket-mode'), hashtext(tenant/bucket))`. On an `ssec_required` false→true change it then counts, **in a separate statement after the lock** (READ COMMITTED), three kinds of readable data: head rows whose `encryption_algorithm` is not SSE-C, version rows with bytes that are not SSE-C, and active uploads with `ssec_key_md5 = ''`. Any non-zero count returns `*modes.ReadableObjectsError`. The management PATCH answers it with **409 `bucket_has_readable_objects`** and `error.readable {readable_objects, readable_versions, uploads_without_key}`. The dashboard shows a flash: copy the data into a new bucket with your key | `TestPrivateMode_RefusedOverReadableObject` (`expected: 409 actual: 200`, `mode = private` ×4); `TestSetBucket_PrivateRefusedOverEachReadableKind` ("An error is expected but got nil" ×5); `TestPrivateMode_SSECObjectsOnly_Allowed` (a guard that passes on main); `TestHandleUpdateBucketSettings_PrivateOverReadable_FlashesCopyAdvice` | M1 no readable check; M5 versions clause; M6 uploads clause; M9 PATCH mapping; M11 flash; M12 delete markers counted |
| RP33-1 b | P2 | `commitBlobWrite` first takes the same key with `pg_advisory_xact_lock_shared` and re-reads `ssec_required`. After the upsert, a row that is not SSE-C in a bucket that now requires a key gets `errSSECRequiredAtCommit`. That rolls back, queues the blob `orphan_write`, and sends the client the same 400 as at the door (`refusedAtCommit`). It applies on every writer: PUT and Complete (whole and chunked), CopyObject (whole and chunked), generated objects (`errGeneratedSSECRequired`, counted) and direct-upload complete (400 `ssec_required`). Lock order (written in `blob_flip.go`): bucket-mode → key lock → head row → version → upload row → `tenant_quotas` → floors | `TestPrivateMode_FlipDuringCommit_NeverPrivateOverPlaintext` (**"Private over plaintext (PUT 200, door <nil>)"**); `TestPrivateMode_UnkeyedCommitAfterFlip_Refused` (PutObject, CopyObject; red only because the hook is absent on main); `TestBlobRowWritersOnlyThroughTheFlip` (an AST pin: every head-row INSERT is inside a blobWrite `Upsert:` closure, and every version row with bytes is written by `writeVersionRow`) | M2 door without the lock; M3 flip without the shared lock; M4 no re-check; M10 a head-row INSERT in `cdn.go` |
| RP33-1 c | P2 | `POST /api/v1/manage/buckets` accepts `"mode": "standard" \| "private"` (`modes.DeveloperModes()`); any other value, `public` included, is 400 `invalid_mode`. A Private create inserts the row and turns it Private through the door **in one transaction** (`createPrivateBucketRow`); a Standard create is the single INSERT it was before. The OpenAPI enum and its text are generated from modes.json. `TestBucketModeWritersOnlyThroughTheDoor` now also pins the door's callers | `TestCreateBucket_ModePrivate` (`expected "private" actual "standard"`; `public` → 201) | M8 create ignores private |
| RP33-4 | P3 | The door writes one `bucket.mode_changed` audit row {bucket, from, to, visibility, ssec_required}, with actor, IP and user agent, **inside its own transaction** (new `audit.RecordTx`). The row commits with the change or not at all. A refused change writes no row; a save that changes nothing writes none | `TestBucketModeChange_Audited`, `…_RefusedLeavesNoAuditRow` (`"[]" should have 4 item(s), but has 0`) | M7 no audit |
| RP33-5 | P3 | `GET ?acl` of a Public bucket adds AWS's canned public-read grant, AllUsers READ (`xsi:type="Group"`, the `…/global/AllUsers` URI), beside the owner's FULL_CONTROL. Object `?acl` of an object in a Public bucket does the same (D-P27-1) | `TestGetBucketAcl_PublicBucket_SaysAllUsersRead` (`should have 2 item(s), but has 1`) | M13 public ignored; M15 the group grant on every bucket |
| RP33-6 | P3 | **`docs/BACKEND_ENCRYPTION.md`**: one row per driver `main.go` can register (13), giving the transport, the vendor's at-rest statement (URL, read 2026-10-22) and whether a Standard object can land there. The table shows Standard's pre-key sentence is false on backends Standard reaches **by default**: `geyser` (the encryption add-on is off on our account), `idrive` (at rest unconfirmed), and our own NVMe for multipart parts. It is also false on `blomp`, which is flag-gated, plain HTTP and unsealed on prod. modes.json `without_master_key` now says: "Not encrypted by us at rest yet: your files sit on our storage providers' disks and tapes as we send them, encrypted there only where the provider does so, and we can read them." | `TestBackendEncryptionDoc_EveryDriverHasARow` (the doc was absent) | M14 the blomp row dropped |
| RP33-10 | note | The settings page of a Public bucket says that turning public access off stops new downloads at once, but CDN copies may be served for up to `cache_max_age` + 10 min (no purge). The Customer-key card says the requirement can be turned on only while the bucket holds nothing we can read | — | — |

**How the red runs were done.** I used a detached origin/main worktree holding only the new test files plus symbol-only stubs, so they compile: a no-op `blobCommitStartHook`, and `modes.ErrReadableObjects` / `ReadableObjectsError` / `SSECAlgorithms` / `ActionModeChanged` with no behaviour. All 15 mutations turned their named tests red.

**Departures (0a).**
- **D-P27-1:** GET **object**?acl in a Public bucket also carries AllUsers READ (the brief named only the bucket).
- **D-P27-2:** only `status = 'active' AND ssec_key_md5 = ''` uploads block the flip. An active SSE-C upload does not.
- **D-P27-3:** version rows from before 087 have no algorithm and count as readable. That is the safe side: such a bucket goes Private by copying.
- **D-P27-4:** modes.json Standard's pre-key sentence is rewritten, as RP33-6 asks once the table shows it false.

**Two sqlmock fixes found by the full race run.**
- `TestManagementCreateBucket`: a Standard create keeps its single INSERT.
- `TestPresignedURL_Integration`: the mock now expects the shared mode lock and the `ssec_required` read.

## Part 0b (#881) — payments

| Item | Sev | Fix | Red test on origin/main `8ab12e7d` | Mutation |
|---|---|---|---|---|
| RP33-2 a | P2 proven | The Invalid credit counts only confirmed money (`btcpay.confirmedCents`) | `TestInvoiceInvalid_UnconfirmedPayment_NoCredit` (p27 lab capture), `…_MarkedOverAnUnconfirmedPayment_NoCredit` (`Should be zero, but was 1000`) | M1 Invalid credits `paid` |
| RP33-2 b | P2 | The Expired partial credit counts only confirmed money | `TestInvoiceExpired_UnconfirmedPartial_NoCredit` (`… was 500`) | M2 |
| RP33-2 c | P2 | The paid-late mark is set only on confirmed money. A seen-but-unconfirmed payment waits for its `InvoicePaymentSettled` | `TestInvoiceExpired_PaidLateUnconfirmed_NotMarkedSettled`, `TestPaymentSettled_ExpiredPaidLate_MarksSettled`, `TestLab27_ExpiredPaidLate_UnminedThenMined` | M3 |
| RP33-2 formula | — | `confirmed = max(0, min(paidAmount − Σ value×rate of payments neither Settled nor Invalid, Σ value×rate of Settled payments))`, using each method's `rate`, rounded half up once per sum | `TestConfirmedCents` | M4 drop the Settled bound |
| RP33-3 | P3 proven | `SwitchToPrepaid` writes a pending `renewal_stop` (reason `switch`) **in the switch's own transaction**, then stops Stripe by reference after the commit under `Detached(ctx)`. Only `payments.ErrProviderRefused` (a Stripe 4xx other than 404/408/409/429) reverts it, and the revert deletes the stop row. Anything else leaves the stop owed to the `renewal_stops` job | `TestSwitchToPrepaid_ClientGoneDuringStop_JobStopsStripe`; guard `TestRecordRenewalStop_StoppedNeverClosesAPendingStop`; `TestRefusedOr_PermanentOnlyForA4xx` | M5 request ctx; M6 no row in the switch tx; M7 revert on any error; M8 `stopped` overwrites `pending`; M10 |
| RP33-7 | P3 | A switch from a past_due / unpaid / incomplete subscription cancels it now, through RP31-4's path by reference | `TestSwitchToPrepaid_FromPastDue_CancelsNow` (`"[]" should have 1 item(s)`) | M9 |
| RP33-8 a | P4 | A crash between the commit and the stop leaves a durable owed stop (same row as RP33-3) | `TestSwitchToPrepaid_CrashAfterCommit_JobStopsStripe` | M6 |
| RP33-8 b | P4 | A cancel that BTCPay accepted but our transaction did not close tells the customer the invoice **was cancelled** | `TestCancelUnpaidInvoice_MarkedThenCloseFails_SaysCancelPending`, `handlers.TestCancelCryptoInvoice_MarkedButNotClosed_SaysCancelled` | M12, M13 |
| RP33-9 | note | The hourly price assert also reads the default customer-portal configuration (`billingportal/configuration.List(is_default=true)`). If it allows plan changes, checkout closes with the configuration id in the error | `TestVerifyHousePrices_PortalAllowsPlanChanges_Unready` (`An error is expected but got nil`) | M14 |
| RP33-11 | note | **Answered by the lab: yes.** A payment mined after the invoice went Invalid produced an `InvoicePaymentSettled` delivery. The invoice stays Invalid, the payment becomes Settled, and the code credits 1000 cents with no grant | lab capture `internal/payments/btcpay/testdata/p27/` | — |
| regression | — | A portal un-cancel after a durable switch withdraws the stop | `TestPortalUncancel_AfterDurableSwitch_WithdrawsTheStop` | M11 |
| revert | — | A refused stop leaves no stop row | `TestSwitchToPrepaid_StripeRefuses_RowReverted` | M15 |

**Lab** (BTCPay 2.4.5 regtest, torn down after):
- A payment left unmined past a 2-minute monitoring window turned the invoice **`Invalid/None`**, with `paidAmount` 10.00 and the payment still Processing. **No `InvoiceInvalid` webhook** was delivered (RP34-1).
- `Expired/PaidLate` stayed unmined until mining. Then `InvoicePaymentSettled` arrived and the payment was Settled.

**Departures (0b).**
- **D-P27-5:** the formula subtracts the unconfirmed part of BTCPay's net `paidAmount`, capped by the Settled value. It does not sum Settled values alone, which would count a second payment's network fee as value.
- **D-P27-6:** a transient Stripe failure no longer fails the switch. The switch answers success and the stop stays owed to the job.
- **D-P27-7:** two upserts changed meaning.
  - A `stopped` record never closes a `pending` row.
  - The Stripe webhook treats a stop row for the subscription the entitlement still names as owed, not as "switched away", so a portal un-cancel still withdraws the switch.
- **D-P27-8:** four older btcpay tests used an unconfirmed payment and now use the confirmed capture of the same invoice. Two tests that used `"stripe: 500"` as a refusal now use `ErrProviderRefused`.

## Checks

- `make lint`: 0 on both branches.
- gosec with the workflow's flags: 0. `gosec@latest` cannot read Go 1.26 export data on the Mac, so it was run through `go run …@master` (RP34-6).
- `go test -race ./...`: green against a private migrated database (`vaultaire_test_p27a`).
- CI: green, merge group included.
- No Co-Authored-By or AI trailer in any commit or merge commit (`git log` checked before each merge).

## Prod proof

**Deploys.**
- **#881:** `6bbd3ed` on slot 8000, 14:47 UTC (Deploy run 39798406404). 0 `"level":"error"` after that switch.
- **#882:** merged as `c2a7fa69` at 16:58 UTC. The queue push started **no Deploy run** (the known shape), so `deploy.yml` was dispatched (run 39818719434: build and deploy-prod succeeded). Prod `/version` reads `c2a7fa698eb2` on slot 8000. **0 `"level":"error"` since the switch** (read at 17:02 UTC).

**The door on tenant zero** (`tenant-4e1b43062db9d685`), 17:01–17:02 UTC, against the active slot on the box. A management JWT was minted on the box (10 min, never printed). The tenant's primary key has no stored secret (it predates the SigV4 secret column: "regenerate this API key"), so a temporary key `p27-proof-temp` was created through `POST /api/v1/manage/keys` and deleted at the end. The tenant is on the free tier (1 bucket), so the two buckets ran one after the other.
1. **Non-empty bucket.** `p27-proof-nonempty-…` was created Standard (201) and one plaintext object was PUT (200).
   - `PATCH {"ssec_required":true}` → **409 `bucket_has_readable_objects`**, with `readable {readable_objects: 1, readable_versions: 0, uploads_without_key: 0}` and the copy advice.
   - Afterwards: `mode` still `standard`, the object still served (200), no audit row.
   - The object was deleted (204), then the bucket (200).
2. **Create with mode.** `POST {"name": "p27-proof-private-…", "mode": "private"}` → **201 `"mode": "private"`**, row `private`, `ssec_required = t`.
   - An unkeyed PUT into it answers **400 `InvalidRequest`**.
   - The bucket was deleted (200).
   - `"mode": "public"` → 400 `invalid_mode`.
3. **Audit.** Exactly one `bucket.mode_changed` row in the window: `{"from": "standard", "to": "private", "bucket": "p27-proof-private-…", "visibility": "private", "ssec_required": true}`. The refused PATCH wrote none.
4. **Cleanup.** 0 `p27-proof-%` buckets, 0 head rows, 0 live `p27-proof-temp` keys.
5. **Flags.** `pay_btcpay`: **0 rows (OFF)**. `modes_copy`: only `tenant-4e1b43062db9d685 = true` (global still OFF).

**A first attempt (17:01) is recorded for honesty.**
- The temporary key did not exist yet, so the S3 PUT was refused (no stored secret) and the "non-empty" bucket was in fact empty. The PATCH to Private on it was therefore **200, and correct**: an empty bucket may go Private. It wrote its audit row.
- The second create hit the 1-bucket free-tier limit (403 `free_tier_bucket_limit`).
- Everything from that attempt was deleted the same minute.

## Residuals (RP34-n)

| # | Sev | What | Where |
|---|---|---|---|
| RP34-1 | P3 | BTCPay 2.4.5 marks an invoice Invalid at the end of the monitoring window **without** an `InvoiceInvalid` webhook. Our row stays `processing` until another delivery re-fetches it, and nothing does if the payment never confirms. A reconcile job could re-fetch open rows older than the window | payments lane |
| RP34-2 | P3 | A payment that confirms after BTCPay stopped watching the invoice may produce no event. The money is then not credited automatically, and support credits it by hand | with RP34-1 |
| RP34-3 | P4 | A `partial:` credit can be spent at checkout before a late settle zeroes it. This predates #881 and RP33-2 does not change it | note |
| RP34-4 | P3 | The RP33-9 portal assert closes checkout when the Stripe key cannot read portal configurations, so a restricted prod key needs billing-portal read | owner, with the Stripe env |
| RP34-5 | P4 | `SwitchToPrepaid` now waits up to 15 s for Stripe inside the dashboard request | note |
| RP34-6 | P4 | `make gosec` (`@latest`) cannot type-check Go 1.26 on the Mac, so `@master` was used locally. CI's gosec passes | tooling |
| RP34-7 | P4 | Two sessions shared one test database (`vaultaire_test_p27`): `cleanupDashBucketData` deletes every `test-dash-%` tenant, the other session's included. Each session needs its own DB (as the rule says), or the cleanup must be bounded to its own tenants | test hygiene |
| RP34-8 | P3 | Standard is not encrypted at rest on Geyser (the add-on is off), its at-rest status on iDrive is unconfirmed, and multipart parts sit on an unencrypted NVMe for up to 48 h (`docs/BACKEND_ENCRYPTION.md`). The master key (F-14) closes all three for Standard | owner (F-14) |
| RP34-9 | note | Root `CLAUDE.md` still said "Since 2026-10-11 prod runs `STORAGE_MODE=lyve`" in bold, present tense, above the paragraph that says `idrive` since 2026-10-20 21:58 UTC. Prod reads `idrive` (2026-10-22). The sentence is now past tense with its end date (this PR) | done here |
| RP34-10 | P4 | Per-object SSE-C in a Standard bucket is still understated by the bucket-level mode label (RP33-10's second clause; no reader shows a per-object mode) | the modes follow-up |
| RP34-11 | P3 | RP32-5's CI flake (another lane's code) took #882 out of the merge queue: `internal/drivers` `TestSiaWriteBudget_PacingIsNeverAStalledS3d` failed in the merge-group run at 16:36 UTC with `one PUT: the deadline timed s3d, not our wait` (expected 1, got 2). An earlier removal at 14:51 UTC was also `failed_checks`. Each costs a 20 min queue round | the Sia lane |
| RP34-12 | P4 | Tenant zero's primary key has no stored secret (`api_keys.secret_key` NULL), so it cannot sign SigV4 until it is rotated. The proof used a temporary key | owner: rotate the primary from the dashboard |

## Owner items (note only, not done here)

- **`pay_btcpay` stays OFF** until you flip it. RP33-2 is on prod now (#881, `6bbd3ed`), and no `BTCPAY_*` is in `.env`.
- **`modes_copy` global flip** only after reading the privacy page's per-mode table **and** `docs/BACKEND_ENCRYPTION.md`. Standard's sentence now says "not encrypted by us at rest yet".
- **F-32:** may the dashboard make an existing non-empty bucket Public (as today, now audited `bucket.mode_changed`), or only at creation?
- **RB-3:** set `BLOMP_ALLOW_UNSEALED` off before any customer.
- **RP31-8:** confirm or change the § 2.3 line in PAYMENTS.md.
- **WP-P24 click-list items 3–4.**
- From `docs/BACKEND_ENCRYPTION.md`:
  - Geyser's encryption add-on, or the master key.
  - Ask iDrive whether e2 encrypts at rest by default.
  - The box disk.
- RP34-4: billing-portal read on the restricted Stripe key.
