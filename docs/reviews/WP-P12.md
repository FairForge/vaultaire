# WP-P12: Stage 1a Prompt 12

2026-10-18. Three parts:

- **Part 0**: four follow-ups of the 2026-10-18 reviews (RP11-1, RP11-1b, RP11-2, RP11-3) and the prod read.
- **Part A**: WP-R3-2. UploadPartCopy: a server-side copy of a large object works.
- **Part B**: WP-R8-2. A chunk blob has a lifecycle:
  - B.1: no lock and no pooled connection is held across the backend PUT.
  - B.2: blobs no index row names are found and erased.

PRs:

- **#761**: Part 0, deployed `cc5dc08`.
- **#762**: Part A, deployed `9b693fc`.
- **#763**: Part B.1, migration 093, deployed `734f611`.
- **#764**: Part B.2, deployed `e9181dd`.
- this PR: docs only.

Start sha: `origin/main` `bdd30d1`. **Migration 093** is taken by B.1. STATUS item 8 had reserved it for WP-PAY-1; WP-PAY-1 now takes **094**, and this PR moves that reservation in STATUS, `docs/PAYMENTS.md`, the plan and `WP-PAY-PROMPTS.md`. **The next migration is 094.**

## Decisions

1. **Which subscription status writes are guarded (RP11-1 / 1b).**
   - Every write of `tenants.subscription_status` and `plan` is now one guarded `UPDATE … WHERE stripe_subscription_id = <the event's>`.
   - The invoice events take the subscription id from `invoice.subscription`. An invoice never adopts a subscription, and a one-off invoice (no subscription) writes nothing.
   - `customer.subscription.{created,updated}` adopt the id when the tenant has none, so a `created` that arrives before `checkout.session.completed` still lands. A `canceled` or `incomplete_expired` subscription is never adopted: without that rule, a stale `updated` for a deleted subscription would re-point the tenant at it.
   - **On a stale event, `plan` does nothing** and neither does the status. `applyHouse` is unchanged: it grants without the guard and clears with it, as the post-merge review found right.
2. **A bandwidth delta that fails is kept, at the risk of counting it twice (RP11-3).**
   - Each upsert gets its own 10 s deadline. A failed row is merged into the next flush, and dropped after 5 failed flushes with one Warn that names its bytes.
   - The risk: a statement whose context ran out after Postgres had already committed it is counted twice when it is retried. That needs a commit and a client timeout in the same instant, which is rarer than the drop it replaces. Residual RP12-1.
3. **One copy-source resolver, one opener, one part stager (WP-R3-2).**
   - `s3_copy_source.go` holds what was inside `copyObject`:
     - `resolveCopySource`: the head row or the `?versionId` row, scoped to the tenant, plus the `x-amz-copy-source-if-*` conditions;
     - `openCopySource`: opens any kind of source, whole or a range;
     - `writeCopySourceOpenError` and `copySourceReadFailed`.
   - `stageUploadPart` is the tail `handleUploadPart` had. UploadPart and UploadPartCopy both call it, so the cap, the staging guard, the reservation, the MD5 and the one-transaction install are the same for both.
4. **What the refactor did to CopyObject: nothing it can observe.**
   - Its tests are unchanged and green (`s3_copy_test.go`, `copy_sized_test.go`, `s3_chunked_copy_test.go`, `s3_copy_long_op_test.go`, the R61-8 and WP-R8-5 copy tests).
   - Error mapping is kept case by case, in the same order (size drift → region → all backends → source failure → 500).
   - What it gained:
     - a ranged-open path in the shared opener. CopyObject never asks for a range.
     - the opener now gives `openSealed` a `ranged` source. `openSealed` only uses it for a ranged request, so a whole copy reads exactly as before.
   - What it lost: nothing.
5. **UploadPartCopy's permission is UploadPart's.**
   - `auth.OperationAllowed` lets a key that may `UploadPart` run `UploadPartCopy`. aws-cli and rclone switch to it on their own, so a scoped backup key must keep working.
   - `UploadPartCopy` is also grantable on its own (`S3Operations`).
   - The SOURCE is gated as a copy's: `copySourceScopeDenied` (R5-02) now covers both entry points.
6. **A failed source read is 503, and a malformed range is 400 `InvalidArgument`, as on AWS.**
   - A source read that fails, ends early or runs on is **503 + Retry-After**, with no part row, no temp file and the reservation given back. Never 400 `IncompleteBody`: the body is a backend's, not the client's (D1.6).
   - A source that cannot be opened is answered exactly as CopyObject answers it (503 / archived / 404 / 500).
   - A range that is malformed, outside the source or longer than 5 GiB is 400 `InvalidArgument`. A whole source over 5 GiB is 400 `InvalidRequest`.
7. **A pending chunk row holds no reference (B.1).**
   - The claim inserts the row at `ref_count` 0, and the writer's reference is added by the ready flip. So:
     - dedup GC's reconcile never sees a pending row's count disagree with `tenant_chunk_refs`;
     - an abandoned claim is recognisable (`ref_count` 0);
     - a failed PUT can drop its claim without touching anyone's reference.
   - A failed PUT **deletes** its own claim (only at ref 0), so the next writer stores the chunk at once instead of waiting out the window. A crash leaves the claim for the takeover or for B.2.
8. **`chunkPutBound` = 90 s, `chunkPendingTakeover` = 2 × 90 s = 3 min.**
   - A chunk is at most 16 MiB. Its per-attempt deadline is `s3PutDeadline`'s 15 s, and with one retry on a fresh connection that is 30 s per backend; the engine fails over to at most two more general-purpose backends: 90 s.
   - The claimer's PUT runs under that bound from after its claim. A backend that commits a PUT the client already gave up on (RP8-11) does so within the second bound. So a taker's bytes are never overwritten by the claimer's late ones.
9. **The orphan grace is measured by the blob's age, not by sightings (B.2).**
   - S3's `LastModified` is the blob's write time on every backend that holds chunks (`sync` holds none), so no table is needed and B.2 has no migration.
   - Sightings two runs apart (the parity reconcile's `vault_parity_orphans`) would be the answer for a backend whose modtime cannot be trusted. That is what would change the choice.
   - The grace is 24 h (`CHUNK_ORPHAN_GRACE`, 1h–7d). It is longer than the 90 s PUT bound, the 3 min takeover window, the 1 h re-encrypt retire window and a dedup GC run.
10. **`chunk_orphans` runs daily at 01:30 UTC**, between `inventory` (00:30) and `dedup_gc` (02:30). Orphans accrue slowly (a stall, a crash) and the grace is a day, so a daily walk is enough: about 40 list calls for prod's 41k chunks.
11. **A guard on the new deleter (added in B.2; the prompt did not ask for it).**
    - Each backend's orphans are collected over the whole listing first. They are erased only when they number at most max(100, 5 % of the keys listed).
    - More than that is what a key-mapping fault would look like (every live chunk unnamed). In that case the run erases nothing on that backend and says why in `not_swept`.
    - Reason: the boot catch-up run of a fresh deploy is this deleter's first prod exercise, and it runs before anyone can look at a dry run.
12. **The admin endpoint only reports.** `POST /api/v1/admin/chunk-orphans` answers `dry_run=false` with 400. The erasing run is the scheduler's (`POST /api/v1/admin/jobs/chunk_orphans/run`): it runs under the job lock and its cursor is recorded.

## What was built

### Part 0 (#761)

- **`webhook.go`**:
  - `writeInvoiceStatus` (the guarded invoice status write);
  - the guarded, adopting status write in `handleSubscriptionUpdated`;
  - every handler returns its tenant, and `ServeHTTP` writes `stripe_events.tenant_id` on the claim row inside the claim transaction, before the commit.
- **`bandwidth.go`**: `Flush` uses one deadline per statement. It keeps a pending map (`pendingTenant` / `pendingBackend`) merged into the next flush, and drops a row after `bandwidthFlushMaxAttempts` (5) with a Warn.

### Part A (#762)

- **The operation**: the parser names `auth.OpUploadPartCopy` from the header. It parses headers before the operation now, because the header names it.
- **Permission**: `auth.OperationAllowed`, plus the R5-02 gate on both copy entry points.
- **`s3_upload_part_copy.go`**:
  - range parsing (`bytes=first-last`, ≤ 5 GiB);
  - the resolver and the opener;
  - the source wrapped `exactSizeReader` → `cutReader` → `readErrReader`;
  - the stager;
  - `CopyPartResult`;
  - the keep-alive (`longOpUploadPartCopy`; `longOpBegin` runs after the reservation).
- **`chunked_read.go`**: `setRange`, so only the chunks a range spans are fetched.
- **Docs**: `docs/API.md`, `changelog.md`, `internal/api` and `internal/auth` CLAUDE.md, monitoring.

### Part B.1 (#763, migration 093)

- **`global_content_index.state`** (`pending` | `ready`, default `ready`), `pending_since`, and the partial index `idx_gci_pending`.
- **`chunk_claim.go`**: `storeChunkLocked` = `claimChunk` → `chunkStore.put` under `chunkPutBound` → `readyChunk`. A failure drops the claim (`dropChunkClaim`).
- **Readers that take `ready` rows only**:
  - `LookupChunk` / `LookupChunkFresh` / `LookupChunks` / `IncrementRef(Tx)` / the global stats (crypto);
  - dedup GC's reconcile, candidate scan and `sweepOne` DELETE;
  - `key_reencrypt`'s candidates (lifted into `candidates()`), re-read and swap;
  - the chunk move's row pass and re-read;
  - the rotation backlog count;
  - the dashboard cost and carbon byte sums.
- **Checks that count every state.** "Does a row name this address" (the retired-chunk collector, the chunk move's orphan pass) counts any row, pending ones included: the safe side.

### Part B.2 (#764)

- **`engine.PageLister`** (optional; the `Driver` interface is unchanged), and `drivers/list_page.go` on the fixed-bucket S3 driver (idrive, idrive-<region>, wasabi) and lyve.
- **`chunkStore.listPage`**.
- **`chunk_orphans.go`**:
  - the abandoned-claims pass;
  - the bounded listing pass from the cursor in `job_runs.result`;
  - the locked re-check and erase;
  - the guard;
  - `vaultaire_chunk_orphans_total{backend,outcome}` and `vaultaire_chunk_orphan_bytes{backend}` (a collector reading `job_runs.result`);
  - the admin dry run;
  - the OpenAPI entry, `CHUNK_ORPHAN_GRACE` in CLAUDE.md, `JobStale`.

## Before → after

| # | Item | Before (`bdd30d1`) | After | Test |
|---|---|---|---|---|
| 0.1 | stale `invoice.payment_succeeded` after `subscription.deleted` | row `active` | stays `canceled`, no id adopted | `TestWebhook_StalePaymentSucceededAfterDelete_DoesNotMarkTheTenantActive` |
| 0.1 | old subscription's `payment_failed` while on S2 | row `past_due` | stays `active` | `TestWebhook_StalePaymentFailedForAnOldSubscription_LeavesTheCurrentStatus` |
| 0.1b | stale `updated` for cancelled S1 while on S2 | row `canceled`, plan `vault3` | `active`, `house`, house kept | `TestWebhook_StaleUpdatedForAnOldSubscription_DoesNotOverwriteTheCurrentStatus` |
| 0.1b | `created` before checkout, none recorded | status written, id NOT set | status + id adopted | `TestWebhook_CreatedBeforeCheckout_AdoptsTheSubscription` |
| 0.1b | a cancelled subscription is never adopted | (already held) | held | `TestWebhook_CanceledSubscriptionIsNeverAdopted` |
| 0.2 | `stripe_events.tenant_id` | always NULL | the resolved tenant (NULL when not ours) | `TestWebhook_ClaimRowRecordsTheResolvedTenant` |
| 0.3 | a slow tenant upsert | the backend rows after it failed at once (shared 10 s context), counts dropped | each statement its own deadline; the failed delta re-sent next flush | `TestBandwidthTracker_ASlowStatementDoesNotFailTheOnesAfterIt` |
| 0.3 | a statement failing every flush | dropped silently (an Error) | kept 5 flushes, then one Warn naming the bytes | `TestBandwidthTracker_AStatementFailingEveryFlushIsDroppedWithAWarn` |
| A.1a | UploadPartCopy, whole plain source | 501 | the source's bytes, own MD5, `CopyPartResult` | `TestUploadPartCopy_WholePlainSource` |
| A.1b | `x-amz-copy-source-range` | 501 | exactly those bytes; malformed / past size / > 5 GiB → 400 | `TestUploadPartCopy_RangeIsExactlyThoseBytes`, `…_InvalidRanges_400NothingStaged`, `…_SourceOver5GiB` |
| A.1c | scoped key, UploadPart only, source bucket out of scope (real SigV4) | 501 (`NotImplemented`) | 403 `AccessDenied`; with GetObject + scope → copied | `TestUploadPartCopy_ScopedKeyNeedsGetObjectOnTheSource` |
| A.1d | sealed SSE-S3 source | 501 | plaintext part, whole and by segment | `TestUploadPartCopy_SealedSourceGivesPlaintext` |
| A.1e | chunked source | 501 | from the content index, whole and range | `TestUploadPartCopy_ChunkedSourceWholeAndRange` |
| A.1f | `?versionId` source | 501 | that version's bytes, `x-amz-copy-source-version-id` | `TestUploadPartCopy_SourceVersion` |
| A.1g | `x-amz-copy-source-if-match` mismatch | 501 | 412 | `TestUploadPartCopy_SourceConditions` |
| A.1h | SSE-C source | 501 | 501 (until WP-SSEC-1) | `TestUploadPartCopy_SSECSource_501` |
| A.1i | quota / in-flight cap / staging guard | 501 | 403 nothing staged / 413 / 503, on the range length | `TestUploadPartCopy_QuotaAndStagingGuards` |
| A.1j | source breaks off, short, long | 501 | 503 + Retry-After, no row, no temp file, reservation given back | `TestUploadPartCopy_SourceReadFails_503NoPart` |
| A.1k | Complete of copied parts; a part copied twice | 501 | byte-equal, `-3`, replaces | `TestUploadPartCopy_CompleteAssemblesTheSource` |
| A.1l | another tenant's bucket of the same name | 501 | 404 `NoSuchKey` | `TestUploadPartCopy_AnotherTenantsBucketOfTheSameName_404` |
| A.1m | `TestUploadPartCopy_RefusedNotImplemented` | 501 | inverted: the SOURCE is staged, never the empty body | `TestUploadPartCopy_StagesTheSourceNotTheEmptyBody` |
| B.1a | pooled connections held while chunk PUTs wait on the backend | **5 for 5 PUTs** | **0** | `TestChunkStore_NoConnectionHeldAcrossTheBackendPut` |
| B.1b | R8-02: two first-stores, different encodings, concurrent | one blob (held) | one blob, both read back | `TestChunkStore_ConcurrentFirstStoresWithDifferentEncodings_OneBlob` |
| B.1c | a writer that died after its claim | (no claim existed) | the next writer waits the window, takes it over, reads back | `TestChunkStore_AWriterThatDiedLeavesARowTheNextWriterTakesOver` |
| B.1c | claim taken over while its PUT ran | — | no flip; references the winner | `TestChunkStore_AClaimTakenOverWhileItsPutRanReferencesTheWinner` |
| B.1 | a failed PUT | (no claim existed) | its claim dropped | `TestChunkStore_AFailedPutDropsItsClaim` |
| B.1d | readers on a pending row | dedup hit, GET plan, IncrementRef, key re-encrypt and the chunk move all took it | none does; dedup GC keeps it | `TestChunkReaders_NeverSeeAPendingRow` |
| B.2a | an unnamed blob past the grace / young | found by nothing | erased, counted / left, counted | `TestChunkOrphans_AnOldBlobNoRowNamesIsErased_AYoungOneIsLeft` |
| B.2b | ready / young pending / abandoned pending | — | untouched / untouched / erased with the row | `TestChunkOrphans_ABlobARowNamesIsNeverTouched`, `…_AnAbandonedClaimWithNoBlobIsErased` |
| B.2c | a `retired_chunk_blobs` address | — | untouched | `TestChunkOrphans_ARetiredAddressIsTheCollectorsNotTheSweeps` |
| B.2d | a legacy address | — | never listed | `TestChunkOrphans_ALegacyAddressIsNeverListed` |
| B.2e | a store landing between the listing and the erase | — | kept by the locked re-check | `TestChunkOrphans_AStoreThatLandsBeforeTheEraseKeepsItsBlob` |
| B.2f | breaker open / no driver | — | `not_swept`, run ok | `TestChunkOrphans_ABackendThatCannotBeAskedIsSkippedNotFailed` |
| B.2g | the listing budget | — | 2 + 2 + 1 over three runs from the recorded cursor | `TestChunkOrphans_TheListingIsBoundedAndResumesFromItsCursor` |
| B.2h | dry run | — | reports, deletes nothing; the endpoint refuses `dry_run=false` | `TestChunkOrphans_ADryRunReportsAndDeletesNothing`, `…_TheAdminEndpointOnlyReports` |
| B.2 | the guard | — | too many orphans on one backend → nothing erased | `TestChunkOrphans_TooManyOrphansOnOneBackendErasesNothing` |

## Local live proof

- **Part A**: aws-cli 2 against a local server (`STORAGE_MODE=local`, a private migrated DB, the multipart staging floor off: the Mac's disk is 92 % full).
  - A 40 MiB plain object and a 90 MiB chunked object, each copied with `aws s3 cp s3://b/src s3://b/dst`: 5 and 12 UploadPartCopy parts, ETags `-5` / `-12`, downloads byte-equal.
  - `aws s3 mv` works.
  - A raw `upload-part-copy` with range 0–5242879 answers a `CopyPartResult`.
  - A range past the size answers 400 `InvalidArgument`.
- **Part B**: the handler-level tests above. B.1's connection count runs through the real `database/sql` pool (`db.Stats().InUse`).

## Prod proof

### Part 0.4: the prod read, before anything of this prompt deployed (2026-10-18 14:45 UTC, `83f3d89`, slot 8000)

- 0 `"level":"error"` lines in the 64,794 journal lines since 04:00 UTC. Of those: 33,194 info and 14,246 warn. The warns were 14,209 `backend health check failed` (iDrive), 22 HEAD-not-in-cache, 10 routing truth "could not be asked", and 5 put-deadline retries.
- 18/18 jobs `ok`:
  - `dedup_gc`'s note: 1,049 not swept (idrive);
  - `routing_truth`: 10 idrive errors.
- `global_content_index` by backend:

  | backend | rows | size | at ref 0 |
  |---|---|---|---|
  | idrive | 1,176 | 2,348 MB | all |
  | lyve | 24,399 | 47 GB | 7,439 |
  | wasabi | 1,318 | 2,655 MB | 172 |

  All rows are `_global` scope. `retired_chunk_blobs` is empty.
- `vaultaire_chunk_legacy_address_reads_total` 0. No `UploadPartCopy` long-op series.

### Part 0 (#761, `cc5dc08`)

- Prod has no `STRIPE_*`, so the webhook half is dark: its first live exercise is the owner's Stripe go-live.
- The bandwidth flusher: 0 `flush … bandwidth` Error lines across this prompt's two upload bursts (about 320 × 90 MB PUTs).

### Part A (#762; proven on `734f611`, which contains it)

Through `https://stored.ge` (Cloudflare), bench tenant, `AWS_MAX_ATTEMPTS=1`:

- **200 MB whole source.** It lives in a versioned bucket, where anything over 64 MiB is stored whole while `versioned_chunking` is off; there were no `chunked object stored` lines for that bucket.
  - `aws s3 cp s3://b/src s3://b/dst` with aws-cli's classic transfer client at 8 MB made **24 UploadPartCopy** calls in 2.8 s.
  - HEAD of src and dst: both `200000000 "10a0…-24"`.
  - dst downloaded byte-equal.
- **A non-current version as the source.**
  - v2 (30 MB) overwrote src.
  - `upload-part-copy --copy-source b/src?versionId=<v1> --copy-source-range bytes=0-5242879` answered `CopySourceVersionId` = v1 and a `CopyPartResult`.
  - After the complete, the object equals v1's first 5 MiB, not v2's.
- **100 MB chunked source**, aws-cli's default CRT client:
  - `aws s3 cp` server-side copy: 2 UploadPartCopy parts, 1.9 s, ETag `-2`. It equals the source's: same part boundaries, same part MD5s.
  - Byte-equal.
- **A raw `upload-part-copy`** of bytes 0–5242879: `CopyPartResult.ETag` = the MD5 of those source bytes.
- **A range past the end**: 400 `InvalidArgument` "range specified is not valid for source object of size: 100000000".
- **rclone** `copyto st:b/src st:b/rcl --s3-copy-cutoff 8M`: "Copied (server-side copy)", 12 parts, 3.5 s, byte-equal. rclone's default cutoff is 4.6 GiB, so below that it uses one CopyObject.
- **`vaultaire_s3_long_op_total{op="UploadPartCopy"}`**:
  - `ok` 40;
  - `error_before_commit` 2 (the deliberate range refusal, and a NoSuchKey from the first script run);
  - `error_after_commit` 0.
- **Not provable on prod**: sealed SSE-S3 sources, because prod has no master key. The tests cover them.
- **0 Error lines** since 16:09 UTC. Every bucket and object written was deleted (`p12*` buckets left: 0).
- **The first script run** sent the 200 MB source as ONE `put-object` and got Cloudflare's 502: bodies over 100 MB must be multipart or go to `s3.stored.ge` (`docs/CLOUDFLARE.md`). It also left one unfinished upload, which was aborted and its bucket deleted. A script error, not the product.

### Part B.1 (#763, migration 093, `734f611`)

Load: chunked 90 MB PUTs from 3 parallel streams for 3 minutes, direct to the active slot (`127.0.0.1:<port>`, `AWS_MAX_ATTEMPTS=1`), with `pg_stat_activity` and `pg_locks` sampled every 0.5 s (`p12conn.sh`).

| | before: `cc5dc08`, slot 8001, 15:24–15:31 UTC | after: `734f611`, slot 8001, 15:57–16:03 UTC |
|---|---|---|
| PUTs ok (first attempt) | 164 / 164 (~3.0 s each) | 159 / 159 (~3.0 s each) |
| GETs byte-equal | 164 / 164 | 159 / 159 |
| connections `idle in transaction` (mean / max) | **3.5 / 24** | **0.0 / 1** |
| advisory locks held (mean / max) | **3.6 / 24** | **0.1 / 4** |
| `active` (mean / max) | 0.1 / 9 | 0.2 / 5 |
| connections open (max) | 26 | 25 |

- **Every chunk store held an open transaction and the chunk's advisory lock across its backend PUT.** Up to 24 of the pool's 50 connections were waiting on Lyve with 3 uploads; the R8-09a arithmetic says 48 with 6.
- **After 093, nothing is held across a PUT.** The remaining locks are the claims' and the flips' millisecond transactions. Throughput is unchanged.
- **The "before" verify was cut by Part A's deploy.** #762's switch stopped slot 8001 at 15:31:15 during the verify, so 110 GETs got connection-refused and never reached the server, and the bucket was not deleted. All 164 objects were then re-verified on slot 8000 (GET MD5 = upload ETag): 164/164 equal, and the bucket deleted.
- 093 on prod: 40,968 rows, all `ready`, 0 `pending` after the load. 0 Error lines since 15:55.

### Part B.2 (#764, `e9181dd`)

**Before** (16:13 UTC, `734f611`, read-only): a hand-made dry run (`p12orph.sh`). It is the job's logic outside the job: `ListObjectsV2` of `t-_global/_global/_chunks/` on each backend with the prod key pairs (never printed), set against `global_content_index` and `retired_chunk_blobs`.

| backend | keys listed | named by a row | rows on it | orphans > 24 h | orphans < 24 h |
|---|---|---|---|---|---|
| lyve | 38,538 | 38,530 | 38,530 | **8 (13.9 MB)** | 0 |
| wasabi | 1,323 | 1,318 | 1,318 | **5 (10.8 MB)** | 0 |
| idrive | 1,049 | 1,049 | 1,176 | 0 | 0 |

- **The count is the proof of the finding: 13 blobs, 24.7 MB, billed by nothing.**
- All 13 have the `LastModified` of 2026-10-11 03:51 UTC: the 1 GiB chunked PUT that answered six 500s that night (Prompt 2b.5 F1.3). Chunk stores landed on both backends and their rows never committed.
- RP8-11 predicted orphans from the 2026-10-17 stall tests. **None were found.** A retry of the same chunk writes the same address and names it, and the stall tests' uploads were all retried to success. That is the likely reason; it was not traced blob by blob.
- The key mapping holds independently of the code: on lyve and wasabi, *every* row names a listed key.
- On idrive the dead key still lists (only Get/Put are refused). 127 of its 1,176 rows have no blob at the one address: residual RP12-5.

**After** (`e9181dd`, deployed 16:36 UTC, slot 8000). The boot catch-up is the first scheduled run: 16:45:01–16:45:27 UTC (26 s), `ok`, `not_swept` empty.

| backend | listed | named | orphans found | erased | bytes erased | kept / failed |
|---|---|---|---|---|---|---|
| lyve | 38,538 | 38,530 | 8 | **8** | 13,850,073 | 0 / 0 |
| wasabi | 1,323 | 1,318 | 5 | **5** | 10,848,360 | 0 / 0 |
| idrive | 1,049 | 1,049 | 0 | 0 | 0 | 0 / 0 |

- `job_runs.result`: `orphans_found` 13, `orphans_erased` 13, `bytes_erased` 24,698,433. Exactly the read-only "before".
- Metrics:
  - `vaultaire_chunk_orphans_total{outcome="erased"}`: lyve 8, wasabi 5;
  - `vaultaire_chunk_orphan_bytes`: 0 on all three;
  - `vaultaire_job_last_success_timestamp_seconds{job_name="chunk_orphans"}` set.
- Logs: one Warn per erased blob (13) and **0 Error lines** since the deploy.
- **The live chunks are intact.**
  - 50 random `ready` rows each on lyve and wasabi, HEAD at the one address: 50/50 and 50/50 present. The same sample before the run was also 50/50.
  - The next `routing_truth` chunked sample (05:30 UTC tomorrow) is the job's own confirmation.
- **The next dry run is empty.** The hand-made listing again: lyve 38,530 listed = 38,530 named, wasabi 1,318 = 1,318, idrive 1,049 = 1,049; 0 orphans past or within the grace.
- The guard did not trip: 13 orphans in about 41k keys, against a limit of max(100, 5 %).

## Residuals

| # | Sev | Item | Where it goes |
|---|---|---|---|
| RP12-1 | P3 | A bandwidth upsert whose context ran out after Postgres committed it is counted twice on its retry (RP11-3's fix trades a dropped delta for a rare double count) | none until it is seen. An idempotent flush needs a per-flush id |
| RP12-2 | P3 | A plain ranged copy source cannot detect a row that records fewer bytes than the blob holds: `GetRange` is cut to the range's length, so the excess past the range is never read. A whole copy still detects it (`exactSizeReader`) | none. Drift is caught by the whole-object reads |
| RP12-3 | note | UploadPartCopy of a sealed SSE-S3 source is proven by tests only (prod has no master key) | the F-14 ceremony's checklist: one `aws s3 cp` of a sealed object after the key is set |
| RP12-4 | note | **The B.1 switch overlap.** During a deploy of 093-aware code next to an older slot, the old slot can dedup-hit a `pending` row (its lookups do not filter by state) and reference a blob still landing. A GET in that window could fail. The claimer's ready flip completes it; a failed claim with a reference is kept for the takeover. Seen 0 times: 0 `pending` rows after the 15:55 switch. Both slots now run 093-aware code, so the window is closed for good | none |
| RP12-5 | P3 | 127 `idrive` chunk rows (1,176) have no blob at the one address. Probably legacy addresses: the chunk move never ran on idrive, whose key cannot Get. Unreadable either way while the key is dead | the iDrive account repair; then `POST /api/v1/admin/chunk-move?backend=idrive` (dry run first) |
| RP12-6 | note | The chunk orphan sweep lists only S3-class backends (`engine.PageLister`). Chunks are never on `sync` or permafrost (the primary is S3-class), so nothing is missed today | if a non-S3 backend ever becomes a primary: give it a `ListPage` |
| RP8-11 | — | closed by B.2 (no stall-test orphans existed; the 10-11 ones are erased) | — |
| RP11-1, 1b, 2, 3 | — | closed by #761 | — |

## Gates

- `go test -race -count=1 ./...` on a private DB (`vaultaire_test_p12`, one process): every package ok on each part's tree.
  - B.2's first full run caught two structure tests the new job and page listing must be named in (`TestRegisterJobs_EveryLoopIsAJobAndDailyTimesDoNotCollide`, `TestDataPathBounds_EveryClientAndTimeoutIsListed`). Both were fixed, and `api`/`drivers`/`engine` re-ran green.
- golangci-lint v2.14.0: 0 on every tree. gosec with the workflow's flags: 0.
- Structure tests green:
  - `TestEngineCalls_CarryTheTenant` (the copy's engine calls moved to `s3_copy_source.go:openCopySource`);
  - `TestExactSizeReader_OnlyUserIsTheCopyPath` (now `copyObject` and `uploadPartCopy`);
  - `TestChunkBlobCalls_GoThroughTheChunkStore` (the listing is `chunkStore.listPage`);
  - `TestWholeObjectReads_GoThroughTheBlobAddress`, `TestDataPathBounds_*`, `TestEngineRegistry_ReadOnlyUnderTheLock`, `TestS3ParserEmitsOnlyKnownOperations`, the ledger tests, `TestSQLLiteralsMatchSchema` (with 093).
- `build-and-test` green on #761, #762, #763 and #764, and on their merge commits. Deploy succeeded for each.
  - #762's PR showed a red `CodeQL` check: the repository-wide GO-2026-5932 alert on `go.mod` (x/crypto/openpgp, not called). There was no review thread to resolve, and the merge queue took the PR.
