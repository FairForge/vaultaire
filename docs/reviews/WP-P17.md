# Stage 1a — Prompt 17: Part 0 (WP-P16 residuals), WP-R4-2 (the bucket state machine), WP-SSEC-1 (SSE-C complete, migration 096)

Date: 2026-10-20. Prod at the start: `4429cd2` (slot 8001). Branches `stage1a-p17-*`.
Prompt: Part 0 = RP17-1, RP17-2, RP17-10, RP18-1, RP18-2 + the prod read; Part A = WP-R4-2 (R4-10); Part B = WP-SSEC-1 (plan 5.14.8a; migration 096, WP-PAY-1 → 097).

**PRs:** Part 0 #794 (`cdc6c9d`), Part A #795 (`b284a4c`), Part B #796 (migration 096 + the flag, `06c80a6`), #797 (the 0x05 codec, `6282034`), #798 (the API, `6439e91`) — every one deployed and proven on prod the same night; this docs PR last. **Next migration: 097** (WP-PAY-1 moves from 096 to 097).

## Decisions

| # | Decision | Why |
|---|---|---|
| D-P17-1 | The Object Lock headers come from the key's `object_locks` row LEFT JOINed into the ONE query HEAD, GET and `?versionId` already make — never a second round trip. | Non-negotiable 2: HEAD is one read of the cache. `object_locks` has no version column (it is what `checkObjectLock` enforces on every version), so a version names the key's lock. |
| D-P17-2 | A row with no retention but a hold (or `legal_hold = FALSE` after a hold was lifted) sends `x-amz-object-lock-legal-hold` alone; a row is the only thing that makes a header appear. | AWS sends no lock header for an object that never had a configuration, never an empty value. |
| D-P17-3 | `materialize` is two fixed buffers (a 1 MiB probe, then the one 64 MiB + 1 spool) and the temp file — not "spill at 1 MiB". | The drivers that use it keep the 64 MiB in-memory shape they were measured with (Geyser, the fixed-bucket uploader, permafrost); the cost was the doubling, not the threshold. A body under 1 MiB never takes the spool. |
| D-P17-4 | The fleet re-check runs INSIDE the probe (synchronous, 10 s of the probe's 15 s) after the accounts' probes have answered, never changing the probe's verdict. | Deterministic (a test can assert the verdict after one `HealthCheck`), bounded, one at a time (`fleetChecking`); a goroutine would have needed its own lifecycle. |
| D-P17-5 | A fleet verdict is logged once per CHANGE; the write-failure Warn and the `accepted` Warn stay unconditional. | An hourly `match` line is noise; a failed marker write is an error path every time. |
| D-P17-6 | The webhook envelope's `created_at` is the `events` row's, carried as `recordedEvent` through every delivery job; the retry job reads `COALESCE(e.created_at, o.created_at)`. | One event, one body (RP18-2). The signature's `t` dates the request. |

| D-P17-7 | The bucket state machine checks the second factor BEFORE the state rules, and reads the three flags in one query whose failure is the RP16-5 503. | The prompt's order ("the MFA check stays before it"); a 503 flag read still wins. |
| D-P17-8 | `MfaDelete=Enabled` is gated on the EFFECTIVE versioning state: the request's `Status` when it carries one, else the bucket's. | AWS: "you must include `Status` whenever you use `MfaDelete`" — `Status=Enabled` + `MfaDelete=Enabled` on an unversioned bucket is the documented request. |
| D-P17-9 | The two Object Lock refusals carry the sentences AWS answers ("An Object Lock configuration is present on this bucket, so the versioning state cannot be changed.", "Versioning must be 'Enabled' on the bucket to apply a Object Lock configuration"); the MFA-on-unversioned one is ours (AWS's API reference does not print its text). | Conformance where the text is known; said plainly where it is not. |
| D-P17-10 | The TOTP replay guard is the dashboard's `ConsumeTOTPCode`, keyed by the tenant's user (= tenant + code in effect: one user per tenant). | One guard, one window, the R12-38 lesson kept (every accepted code inside the window remembered, not the latest). |
| D-P17-11 | No dashboard one-step "versioning + lock": the dashboard writes none of the bucket flags today (its compliance page reads them), so there is no third path to keep honest. | Nothing to build; noted so no one looks for it. |
| D-P17-12 | A part is sealed at UploadPart as one complete 0x04 stream of its own, bound to the part number (`StreamIdentity.Part`); Complete concatenates the sealed parts behind a self-describing 0x05 table and needs no key. | The S3 shape: Create and every UploadPart carry the key, Complete does not, and "we never store your key" forbids keeping it on the upload row. |
| D-P17-13 | A 0x05 object has **no `body_md5`** (NULL) and its integrity on a whole GET is every segment's GCM tag, not the A.1/A.4 digest. | The parts are sealed one by one as they arrive, in any order, in parallel: no whole plaintext digest can exist without the key at Complete. The prompt's "the whole-body `body_md5` taken as the plaintext streams in" cannot hold for parts; the per-part plaintext MD5 (the ETag) is taken that way. |
| D-P17-14 | `multipart_parts.sealed_md5` (096) is the MD5 of a sealed part's STAGED bytes, re-checked by Complete's staged reader at the sealed length. | The staged-part corruption class (WP-R3-1): without it a part changed at rest between UploadPart and Complete would be committed and found only at a GET. Not a layout column (the layout is the blob's own header) and not anything that unwraps. |
| D-P17-15 | The row value of a multipart SSE-C object is `AES256-SSE-C-v3-mp`; `storedBlobLength` answers 0 = unknown for it and no reader sizes a read by it (`storedBlobKnown`). | The stored length is the parts' lengths' function, which live in the blob; a wrong "expected size" would make the multi-bridge Sync driver pick no version. The vault parity job skips such rows (RP19 residual) rather than protect 0 bytes. |
| D-P17-16 | A 0x05 Range is read span by span: the 6-byte lead, the part table, then per part its 29-byte header and its segments, chained one stored read at a time; the parallel sealed reader (`parallel_get`) takes the single stream for 0x05 objects. | Correct first; the parallel reader assumes one header and one segment grid. Throughput measured on prod in § Prod proof. |
| D-P17-17 | A keyed CopyObject destination is always one 0x04 stream, whatever the source (a 0x05 source included); the copy of an object onto its own key with a new key is the SSE-C rotation. | AWS: a copy is a single-part object. |
| D-P17-18 | The `ssec_required` refusal counter is the EXISTING `vaultaire_generated_object_refused_total{kind,reason}` with the new reason `ssec_required` (pre-created at 0), not a second near-identical metric. | One counter for "a generated object was not written", one label for why. |
| D-P17-19 | Complete of an upload created WITHOUT a key into a bucket flagged after the Create is 400 (the placement is told `SSEC` from the upload row). | The flag governs writes from now on; a Complete is the write. |
| D-P17-20 | Dashboard: the flag is one checkbox on the bucket settings page; public + required is refused with a flash and nothing written. No one-step "make versioned + locked" — the dashboard writes none of the lock/versioning flags today. | B.5's "one toggle with the honest copy"; A.2's dashboard note (no third path exists). |

## What was built

### Part 0 (one PR)

- **RP17-1** `object_headers.go`: `LockMode`, `LockRetainUntil`, `LegalHold` on `objectHeaders`, written on HEAD / 200 / 206 (never on a 304); `objectLockColumns` + `lockHeadersOf`; the three queries (`handleHeadObject`, `headVersion`, `HandleGet`) alias their table and LEFT JOIN `object_locks`.
- **RP17-2** `drivers/geyser.go`: `materialize` = `materializeProbe` (1 MiB) + `materializeSpool` (the one 64 MiB + 1 buffer) + the file; two rows in `data_path_allocations_test.go`.
- **RP17-10** `dispatchWebhooks` deleted; the R11 SSRF guard test calls `deliverToEndpoints` with the endpoint it used to mock.
- **RP18-1** `drivers/onedrive_fleet.go`: `recheckFleet` from `HealthCheck` (after `unknown`: the next probe; else hourly; 10 s; one at a time), `FleetState()`, verdict/accept/time on the driver, log on change (`fleetVerdictChanged`); `cmd/vaultaire` unchanged but for the comment (the accept value boot passes is the one the re-checks use); a bounds row in `data_path_bounds_test.go`.
- **RP18-2** `events.go`: `recordedEvent` (RETURNING `created_at`), `submitEventDelivery(ctx, db, logger, ev)`, `webhookJob(…, ev)`, `deliverToEndpoints(…, ev)`; `object_delete_shared.go` carries it per pending key; `webhooks_routes.go`'s test event returns its `created_at`; the retry SELECT reads `e.created_at`.

### Part A — WP-R4-2 (#795)

- `s3_versioning.go` `handlePutBucketVersioning`: one read of the flags (503 on failure), MFA first, then A.1 (Suspended on a lock bucket 409) and A.3 (MfaDelete=Enabled needs effective versioning Enabled; Disabled needs the header — it did), one UPDATE for both elements.
- `s3_lock.go` `handlePutObjectLockConfiguration`: A.2 — enabling needs versioning Enabled (409), the lock never goes off (as before).
- `s3_mfa_delete.go`: A.4 — `validateMFAHeader` consumes the code through `auth.ConsumeTOTPCode`; the three refusal sentences.
- Tests: `s3_bucket_state_test.go`; `TestMFADelete_EnableRequiresObjectLock` became `TestMFADelete_EnableNeedsVersioningNotTheLock` (the rule inverted); three tests that enabled the lock on an unversioned bucket now enable versioning first.
- AWS pages checked (§ Decisions D-P17-9 and `internal/api/CLAUDE.md` § MFA Delete): the PutBucketVersioning and PutObjectLockConfiguration API references, the user guide's Configuring Object Lock and Configuring MFA delete pages.

### Part B — WP-SSEC-1 (three PRs)

- **#796 (migration 096 + B.5):** `096_ssec_multipart_and_required.sql` (`multipart_uploads.ssec_key_md5`, `multipart_parts.sealed_md5`, `buckets.ssec_required`, all additive); `bucketEncryptionSettings`; `refuseSSECRequired` in the placement (PUT, Complete), Create's and CopyObject's own reads; the generated-object refusal (`refusedSSECRequired`, Error line, the existing counter); `generatedTargetAllowed` for log/inventory targets (409); management `PATCH /api/v1/manage/buckets/{name}` `ssec_required` (409 `bucket_public`), GET/PATCH report it; the dashboard checkbox; OpenAPI; a keyed CopyObject destination was 501 until #798 (it used to land plaintext under a request that asked for a key).
- **#797 (the codec):** `StreamIdentity.Part` in the 0x04 AAD; format 0x05 (`ssec_multipart.go`: header, parser with every absurd table refused before allocation, `MultipartOpener`, `PlanMultipartRange`, `SSECMultipartSealedLength`).
- **#798 (the API):** Create records the key MD5 (SSE-S3 + SSE-C 400; the response carries the SSE-C headers); `uploadPartSeal` + `partBody.seal` (sealed on the staging disk, plaintext MD5 = ETag, `sealed_md5` on the row; a different key / none / a key on a plain upload 400 `InvalidArgument`, nothing staged; a sealed part needs a length, 411); Complete (sealed parts re-hashed at their sealed length, the 0x05 header first, placement `SSEC`, row `AES256-SSE-C-v3-mp` + `-N` ETag + key MD5, `body_md5` NULL, the SSE-C headers on the answer; a part sealed on a plain upload or the reverse asserted); reads (`newSSECOpener`, `openMultipartRange` + `spanChain`, `storedBlobKnown`, the key-MD5 header on HEAD/GET of every SSE-C object); copies (`copySourceRow.SSECKeyMD5`/`SSECKey`, `ParseSSECCopySourceHeaders`, 400/403, `openCopySource` through `openSealed`; the keyed CopyObject destination sealed as one 0x04; UploadPartCopy sealed under the upload's key); `docs/API.md`, `internal/api/CLAUDE.md`, `internal/crypto/CLAUDE.md`.

## Before → after

| Test | Red (how) | Green |
|---|---|---|
| `TestObjectLockHeaders_AnObjectUnderTheBucketDefaultRetentionNamesItOnHeadGetAnd206` | against the branch base (= `origin/main`): mode `""`, hold `""`, date `""` on HEAD | the three on HEAD, 200 and 206 |
| `TestObjectLockHeaders_ALegalHoldIsON` | base: `""` | `ON`, no mode/date |
| `TestObjectLockHeaders_NoneWithoutARow` | guard (green on base) | — |
| `TestObjectLockHeaders_AVersionCarriesTheKeysLock` | base: `""` on HEAD `?versionId` | COMPLIANCE + date + OFF |
| `TestMaterialize_SpoolsInOneFixedBufferThenSpills` | base: 268,512,856 B allocated for a 400 MiB body (bound 70 MiB) | 64 MiB + 1 MiB + small |
| `TestOneDrivePut_AnUnknownLengthUploadsFromTheSpooledBodyWithoutASecondCopy` (the P16 test, the spool subtraction removed) | base: 339,837,768 B (bound 140 MiB) | under 140 MiB |
| `TestFleetFingerprint_AnUnknownBootVerdictIsRecheckedFromTheProbe` | new symbols: red by mutation (the `recheckFleet` call removed from `HealthCheck`): verdict stays `unknown`, gauge 0, no Error line | mismatch found at the next probe, gauge 1, one Error line, no re-read inside the hour |
| `TestFleetFingerprint_TheHourlyRecheckLogsOnlyAChangeOfVerdict` | same mutation: "the markers were re-read" fails | re-read after an hour, no second line |
| `TestWebhookOutbox_ARetrySendsTheSameBodyAsTheFirstAttempt` | base: bodies differ (the retry crosses a second boundary by a sleep of ≤ 1.1 s) and `created_at` ≠ the row's | byte-equal, `created_at` = the row's |

| **Part A** `TestBucketState_SuspendingVersioningOnALockBucketIs409` | `origin/main` (the message constants stubbed): 200 | 409, the row untouched |
| `TestBucketState_TheMFACheckComesBeforeTheLockRefusal` | main: 403 then 200 | 403 then 409 |
| `TestBucketState_EnablingTheLockNeedsVersioningEnabled` | main: 200 on the unversioned and the Suspended bucket, the lock on | 409 × 2, the lock off; 200 on the versioned one |
| `TestBucketState_MFADeleteIsGatedOnVersioningNotOnTheLock` | main: 409 on the versioned no-lock bucket ("gated on the lock"), 200 on the unversioned | 200 / 409 |
| `TestBucketState_StatusEnabledAndMFADeleteEnabledInOneRequest` | main: 409 | 200, both written |
| `TestBucketState_DisablingMFADeleteNeedsTheHeaderToo` | guard (green on main) | — |
| `TestBucketState_AnMFACodeIsAcceptedOnce` | main: 204, 204 | 204, 403, then 204 with the next step's code |
| `TestBucketState_TheDeleteFlavoursOnALockBucketAreUnchanged` | guard (green on main through `handleDeleteObject`) | — |
| **Part B** `TestSSECRequired_*` (7) | `origin/main` (constants stubbed): PUT/Create/CopyObject 200, the generated object written, log/inventory targets 200, GET bucket without the field, PATCH on a public bucket 200 | 400 × 3, refused + counted, 409 × 2, reported, 409 |
| `TestHandleUpdateBucketSettings_RequireCustomerKey` | main: the flag not written, public+required flipped the visibility | written; refused with a flash, nothing changed |
| `TestSSECMultipart_*` (crypto, 6) | new symbols: red by mutation — the part label dropped from the AAD fails the part-number test, the sealed-length check dropped fails the absurd-header test | round trip, identity, headers, ranges, tamper, empty part |
| `TestSSECMultipart_*` (api, 5) | new symbols: red by mutation — `uploadPartSeal` bypassed (no key check, no seal): all five fail | the aws-cli-shaped round trip (HEAD, GET, wrong/no key, ranges), the key rules, the tamper cut + the Complete refusal, UploadPartCopy from 0x04 and 0x05, CopyObject 0x05→0x04 + the rotation |
| `TestUploadPartCopy_SSECSource_NeedsItsKey`, `TestQuotaAccounting_CopyEncryptedSourceRejected` | the old 501 expectations | 400 without the source key |

## Prod proof

### Part A on prod (`b284a4c`)

, b284a4c, 2026-10-20 02:5x UTC, bench tenant via s3.stored.ge):
- A locked bucket (`--object-lock-enabled-for-bucket`) → `put-bucket-versioning Status=Suspended`: `InvalidBucketState: An Object Lock configuration is present on this bucket, so the versioning state cannot be changed.`; `get-bucket-versioning` still `Enabled`.
- A plain bucket with versioning Enabled → `put-object-lock-configuration ObjectLockEnabled=Enabled`: 200, `get-object-lock-configuration` `Enabled`, `get-bucket-versioning` still `Enabled`.
- A plain UNVERSIONED bucket → `InvalidBucketState: Versioning must be 'Enabled' on the bucket to apply a Object Lock configuration`; `get-object-lock-configuration` reports none.
- `MFADelete=Enabled` on the unversioned bucket with a header the bench tenant cannot mint: `AccessDenied` — the MFA check comes first (the 409 half is the handler tests'). Three buckets removed after.

### Part 0 on prod (`cdc6c9d`)

, cdc6c9d, 02:21 UTC):
- A locked bucket with a GOVERNANCE 1-day default: `head-object` → `ObjectLockMode GOVERNANCE`, `ObjectLockRetainUntilDate 2026-10-21T02:21:51+00:00`, `ObjectLockLegalHoldStatus OFF`; after `put-object-legal-hold ON` → `ON`; `get-object --range bytes=0-3` (206, `bytes 0-3/15`) carries all three.
- RP18-1 fired for real at the deploy's boot: `permafrost fleet fingerprint: a marker could not be read — no verdict; re-checked at the next probe` (tenant-2's marker download failed in the boot budget) then, 2 s later from the probe, `permafrost fleet fingerprint state=match fingerprint=06efac2e47795187 accounts=3 written=0`.
- 0 `"level":"error"` lines after the deploy; the 2026-10-20 `chunk_orphans` run started 02:16:57 UTC — the scheduler's hourly tick is boot (00:07) + the BootDelay, not 01:30 sharp (a note, not a finding: the day's run happens within the hour).

### Part B on prod

**The flag (B.5) on `6282034`, 03:05 UTC, bench tenant via s3.stored.ge.** The flag was set by SQL on the box (the management API needs a user JWT and the bench tenant has no password; the PATCH route and the 409 for a public bucket are the handler tests'). With it on: an unkeyed `put-object`, `create-multipart-upload` and `copy-object` each answer `InvalidRequest: Requests specifying Server Side Encryption with Customer provided keys must provide an appropriate secret key. This bucket requires SSE-C: send …`; a keyed `put-object` is 200 with `SSECustomerAlgorithm AES256`; the object written before the flag still reads plaintext; the row of the keyed object is `AES256-SSE-C-v3`. (`put-bucket-logging` with the flagged bucket as its own target hit the self-reference rule first — the flagged-target 409 is the handler test's.) Flag off, bucket removed, 0 error lines. The dashboard toggle: `make dash-shots` on the fixture added to `TestPages_WriteScreenshotFixtures` → `bucket-settings-{light,dark,phone}.png` (the Customer key card, checkbox on, the copy below it); kept in the session scratchpad, not committed.

**SSE-C multipart (B.1–B.4) on `6439e91`, 03:32 UTC**, aws-cli 2.28 with a private config (`multipart_threshold = multipart_chunksize = 8MB`), `AWS_MAX_ATTEMPTS=1`:

| step | result |
|---|---|
| `aws s3 cp` 20 MB `--sse-c AES256 --sse-c-key fileb://key` (3 parts) | 2.38 s; `head-object` with the key: `ETag "…-3"`, `ContentLength 20000000`, `SSECustomerAlgorithm AES256`, `SSECustomerKeyMD5` |
| `head-object` without the key / with another key | 400 / 403 |
| `aws s3 cp` back with the key | byte-equal (`cmp`) |
| `get-object --range bytes=8388600-8388620` (across the part-1/part-2 boundary) | `bytes 8388600-8388620/20000000`, byte-equal with `dd` of the source |
| `copy-object` onto its own key: source key 1, destination key 2, `--metadata-directive REPLACE` (the rotation) | 200; the download with key 2 byte-equal; `head-object` with key 1 403 |
| `aws s3 cp s3://…/20m.bin s3://…/copy.bin --sse-c-copy-source … --sse-c …` (a 0x05 source → a new key) | 200, removed in the cleanup |
| `aws s3 cp` 200 MB (25 parts) with the key | upload 2.65 s, download 2.37 s (≈ 84 MB/s through Cloudflare on the single-stream reader — `parallel_get` takes the single stream for a 0x05 object), byte-equal |
| the active slot's RSS | 252 MB before; 447 MB after the 200 MB upload (the parts stage on prod's tmpfs `/tmp`, RP15-6's owner item); **flat at 446.5–446.7 MB through the 200 MB GET** (six samples 2 s apart) |
| journal | 0 `"level":"error"` lines in the 30 min around the proof |

Bucket removed after; `vaultaire-switch status`: slot 8000 active (`6439e91`); 12 of 12 rule files match the box (no rule changed in P17 — `sha256sum` of the set equal on both sides).

### Lab

Local server on the private DB (, `MULTIPART_STAGING_MIN_FREE=off` — the Mac's disk is under the default floor):
- aws-cli `cp` 20 MB `--sse-c AES256 --sse-c-key fileb://key` with an 8 MB threshold: ETag `…-3`, `head-object` shows `SSECustomerAlgorithm`/`SSECustomerKeyMD5`, the download byte-equal, wrong key 403, no key 400, `get-object --range bytes=8388600-8388620` (across the part-1/part-2 boundary) byte-equal, `copy-object` onto itself with a new key + `--metadata-directive REPLACE`: readable under the new key (now a 0x04 row), the old key 403.
- rclone (`RCLONE_S3_SSE_CUSTOMER_ALGORITHM/KEY_BASE64`, `chunk_size 8M`, `upload_cutoff 8M`) uploads a 20 MB object as a 0x05 row (`AES256-SSE-C-v3-mp`), reads it back byte-equal, and aws-cli reads rclone's upload byte-equal.

### Part 0 prod read (2026-10-20 01:39 UTC, slot 8001 `4429cd2`)

- `job_runs`: the last `chunk_orphans` run is **2026-10-19 01:44 UTC** (ok; lyve 38,576 listed = named, idrive 1,049, wasabi 1,318, every backend `complete`, 0 orphans; no cursor key — the result has none to resume). The 2026-10-20 01:30 run had not started at 01:39: the scheduler's hourly catch-up check runs it at its next tick (~02:10 UTC). Re-read below once it has.
- `routing_truth` 2026-10-19 06:26 UTC: idrive 10 of 501 rows sampled, 10 consecutive errors (the dead key), the rest not asked; every other backend 0 rows — as expected while `STORAGE_MODE=lyve` and the idrive rows wait for the account.
- Alerts: 12 firing = `BackendProbeFailing` × 11 (idrive + 10 regions) + `RoutingTruthNoVerdict{idrive}` — unchanged, correct, never silenced.
- 0 `"level":"error"` lines in `vaultaire@*` since 2026-10-20 01:00 UTC.
- Rule files: 12 of 12 `deploy/monitoring/vaultaire-*.yml` match `/etc/prometheus/rules/` (sha256).
- #793 (the WP-P16 post-merge docs PR) was open, green and not armed; armed and queued at 01:36 UTC.

## Residuals

| # | Sev | Item | Where it goes |
|---|---|---|---|
| RP19-1 | P3 | **The vault parity job and the Smart demotion ledger size a blob by `storedBlobLength`, which is 0 for a 0x05 object** (`AES256-SSE-C-v3-mp`): a vault-floor SSE-C multipart object would be "protected" at 0 bytes, a demoted one copied at 0. Neither path has such an object today (SSE-C multipart is a day old; the `vault` floor holds no SSE-C object). The job and the ledger should skip the row with a note until they read the length from the blob's own header. | a small follow-up before any vault-floor SSE-C multipart object exists |
| RP19-2 | P3 | **A 0x05 Range opens its spans up front** (every spanned part's 29-byte header read and DEK derived before the first byte): a range over many parts costs that many small ranged reads before it starts. Fine for aws-cli's ranges (one or two parts); a ranged read over a 10,000-part object would open 10,000 headers first. | bound the up-front spans (open the next part's header as the previous ends; the key copy would then outlive the constructor) |
| RP19-3 | note | `body_md5` is NULL for a 0x05 object (D-P17-13): the whole-200 digest check of WP-R8-3 does not apply; the GCM tags do. `vaultaire_object_integrity_unverified_total{reason="no_digest"}` counts such GETs beside the pre-094 rows. | documented; no action |
| RP19-4 | note | **The management API's `ssec_required` needs a user JWT**: the bench tenant has no password, so the prod proof set the flag by SQL. A bench user with a password (or a scoped-key route for bucket settings) would let the proof go through the API. | the bench tenant's next re-mint |
| RP19-5 | note | **Complete of an upload created without a key into a bucket flagged after the Create is 400** (D-P17-19). AWS would store it (its policy is evaluated per request too, so AWS answers the same for an unkeyed Complete… of a PUT-policy bucket the Create itself would have been refused). Documented in `docs/API.md`. | — |
| RP19-6 | note | **The scheduler's daily tick is boot + BootDelay, hourly** (the 2026-10-20 `chunk_orphans` run started 02:16:57, not at 01:30): a daily job runs within the hour after its time, never at it. Correct by design (WP-R13-3); the prompt's "01:30 run" is read at the next hourly tick. | — |
| RP19-7 | note | The `_exports` system bucket can never be flagged (the management API refuses system buckets); the GDPR export is therefore never refused for `ssec_required`. The `account_export` job's writer still carries the refusal path (kind `export`), pre-created at 0. | — |
| RP19-8 | note | **Three worktrees and a cherry-pick fight**: the API PR was built on main + the flag branch + the codec commit while #796/#797 were in the queue; the pre-commit hook lints the WHOLE tree including untracked files (a new file must be parked to commit a subset), and `git stash` without `--keep-index` empties the index. The rebase onto main dropped the duplicates cleanly. | the RP16-10 workflow note |
| RP17-3/4/5/6/8, RP16-7/8/9, RP15-6 | — | unchanged | — |

## Gates

- **Part 0 (#794):** golangci-lint v2.14.0 0; `go test -race` on api, drivers, cmd/vaultaire, crypto, engine against the private migrated `vaultaire_p17` (three sqlmock tests updated for the `RETURNING created_at` insert and the joined GET query); every new test red first (the table); `make deadcode` no longer names `dispatchWebhooks` (the 221 class-B functions of R15 stay); CI green, merge group green, deploy green, prod proof above.
- **Part A (#795):** lint 0; `go test -race` on the lock / versioning / MFA / batch tests + the SQL literal audit; every new test red against `origin/main` with the message constants stubbed; CI green; prod proof above.
- **Part B (#796, #797, #798):** lint 0 on every PR; the migration applied twice to the private DB; `go test -race` on api, crypto, dashboard (all three packages), docs, database — green (one leaked-row failure of `TestRoutingRuleFile_MatchesTheExportedSeries` on the private DB was RP17-8's `test-r9-dash-*` rows, removed); the OpenAPI drift guard and the export schema walk pass with the new column; #796's first CI run failed on two sqlmock expectations (the bucket read, the GET bucket) fixed in two follow-up commits; #797 rebuilt on main after #794's allocation rows conflicted; CI green on all three, merge group green, three deploys green; the lab and the prod proof above.
