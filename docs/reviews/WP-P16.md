# WP-P16: Stage 1a Prompt 16

2026-10-19 → 10-20. Three parts:

- **Part 0**: the WP-P15 residuals — RP16-5 (MFA Delete fails closed), RP16-6 (the integrity line's field), RP16-4 (CreateBucket's Object Lock header), RP16-3 (the two July bench blobs, prod), and the prod read.
- **Part A**: WP-R7-4, permafrost conformance (R7-06…09, R7-18, and RP9-1 from WP-P9).
- **Part B**: WP-R11-3, webhook delivery for real (migration 095).

PRs:

- **#789**: Part 0.
- **#790**: Part A.
- **#791**: Part B.
- this PR: this note, SYNTHESIS rows, `docs/STATUS.md` (next migration 096; WP-PAY-1 moves to 096), `docs/reviews/WP-PAY-PROMPTS.md`, docs only.

Start sha: `origin/main` `23a61a5` (#784). Prod at start `f47ef3a`. Engine lane; the contract pages are `docs/ARCHITECTURE_CONTRACT.md` and `docs/surfaces/DEVELOPER.md`.

## Decisions

1. **MFA Delete fails closed with 503, not 500.** An unreadable `mfa_delete_enabled` is "unknown", and the request may succeed once the database answers: `503 ServiceUnavailable` + `Retry-After: 5`, logged at Error. `sql.ErrNoRows` stays "no row → not enabled" (the bucket-existence answers are the handlers' own). The versioning-suspend path's pre-read, which discarded its error, is gone — it calls `checkMFADelete`, which reads the flag itself.
2. **Object Lock at creation = lock + versioning Enabled** (AWS's shape). A malformed header is 400 `InvalidArgument`. Re-creating a bucket you own that has no lock, with the header, is **409 `InvalidBucketState`** — never a 200 that leaves it unlocked (AWS us-east-1 answers 200 and ignores it; other regions 409 `BucketAlreadyOwnedByYou`). Only the S3 entry point takes the header; the management API and the dashboard pass no options.
3. **The integrity line logs both** `etag` (the row's) and `expected_md5` (the digest the body was checked against — a `-N` row's `body_md5`).
4. **Permafrost's fingerprint lives on the fleet, not in the database.** No generic key/value table exists (`feature_flags` has no value column; reusing `job_runs` would invent a phantom job), so Part A takes no migration: every account carries `vaultaire/_fleet/fingerprint.json` (its index, the ordered fleet as 16-hex hashes of `TENANT_N_ID`+`TENANT_N_USER`, the digest). The marker travels with the data it describes: a database restore cannot lose it, a fleet moved to another box keeps it. Accepting a new order is `PERMAFROST_FLEET_ACCEPT=<the digest the Error line names>` — safe if left set (it accepts only that order, and does nothing once the markers match).
5. **The erasure sweep was not missing nested keys** (the prompt's premise, checked): `List` returned the container's direct children and `Delete` of a folder name removed the folder item — Graph deletes a folder with its subtree (`TestOneDriveSweepShape_…` documented it). The contract fix (R7-06: `List` returns keys) would have regressed that: deleting every key leaves the folder skeleton, whose names are the customer's key prefixes. So the sweep now removes the container's folder tree after its keys (`RemoveContainerTree`, `containerTreeRemover` in `api/erasure_sweep.go`), only when every delete of that container succeeded and `List` is empty; a failure defers the tenant.
6. **A large GET holds the window, not one range.** The prompt's "at most one buffered range ahead": with 8 MiB ranges and `odAdaptiveStreams` (2/4/8) in flight through `internal/parfetch`, a GET holds at most streams × 8 MiB (64 MiB) — the Geyser bound — and never the object. No hedge and no retry of a failed range (parfetch's `HedgeAfter` 0): a failed range fails the stream, as before.
7. **Retry bound = the S3-class drivers'** (`requestRetryMaxAttempts` 2 / `jobRetryMaxAttempts` 3, the constants in `hedge.go`); a wait never exceeds 30 s and a wait that would outlast the caller's deadline is not waited for — the call fails at once with the throttle error. Upload fragments: 1 s then 2 s between attempts, the same bound. RP9-1 (WP-P9 assigned it here): header bounds 15 s Graph / 25 s CDN range / **2 min** upload fragment — the exception, because the last fragment commits the file (like an S3 Complete).
8. **The probe reports the worst account.** One dead account of three fails permafrost's probe (and so its breaker) — 1/N of the objects is unreachable, which the rotating probe saw a third of the time.
9. **`odIsNotFound` = 404 `itemNotFound` or a 404 with no Graph body.** A 404 `ResourceNotFound` (the drive or the user gone) is the account's failure, never an object miss.
10. **The outbox row is the durable intent; attempt 1 is the pool job.** One `webhook_outbox` row per (event, endpoint) is written where the pool job is enqueued (`submitEventDelivery`, a DeleteObjects batch's `deliverInOrder`, one `unnest` per batch); the prompt's columns plus `state` (`pending | delivered | failed | skipped`) and `updated_at`; unique on (event_id, endpoint_id); `endpoint_id` `ON DELETE SET NULL`, `event_id` `ON DELETE CASCADE`.
11. **A lease, not an in-flight flag.** While attempt 1 is queued or running, `next_attempt_at` = now + 15 min: a crashed process's attempt is retried, a queued one is not doubled. A duplicate past the lease is at-least-once (docs/API.md: dedupe on `X-Event-ID`).
12. **One statement per finished attempt** (a CTE): the outbox row moves and the `webhook_deliveries` row is written with `retry_count` = attempt − 1 and `next_retry_at` — B.4 denormalised, no join on read.
13. **Five attempts, four waits: 1/5/25/125 s ±20 %.** The prompt's 625 s step would follow the fifth attempt, which is the last. The `webhook_retry` job runs every 30 s, so a due row goes out within 30 s of its time.
14. **A drop is not an attempt.** Overloaded, shutdown and lookup-failed jobs still write their failed `webhook_deliveries` row, then the outbox row is due again 30 s later with `attempts` unchanged (created if the event had none).
15. **Skipped.** An endpoint disabled while its job is queued (the job's re-validation), disabled / gone / no longer matching when the retry job reads it, or deleted (the API delete marks its rows `skipped` in the DELETE's transaction).
16. **The signature is comma-joined**: `X-Webhook-Signature: t=<unix>,v1=<hex HMAC-SHA256(secret, "<t>." + body)>,sha256=<legacy HMAC(body)>`, the legacy element for one release (docs/API.md and the OpenAPI say so, with the 5-minute tolerance). A receiver that compares the WHOLE header with `sha256=<hex>` breaks either way; one that reads the elements keeps working. Prod has **0 webhook endpoints and 0 deliveries** (2026-10-20), so no receiver exists to break.
17. **A 3xx is a failed attempt** (it was recorded `delivered`; the client refuses redirects), so it is now retried.
18. **The metric**: `delivered` counts only deliveries on a retry (attempt ≥ 2), `retry` each failure that schedules another attempt, `exhausted` the fifth failure.

## What was built

### Part 0 (#789)

- `internal/api/s3_mfa_delete.go`: `*mfaDeleteUnknownError`, `writeMFADeleteRefusal`, `logMFADeleteUnknown`; the three callers (`s3.go`, `s3_batch.go`, `s3_versioning.go`). Tests `s3_mfa_delete_failclosed_test.go` (a `database/sql` driver that fails only statements naming `mfa_delete_enabled`).
- `internal/api/bucket_create.go` (`bucketCreateOptions`, `createBucketRegistryWith`, `bucketCreateLockMismatch`), `s3_buckets.go` (the header). Tests `s3_create_bucket_lock_test.go`.
- `cdn.go`, `s3_engine_adapter.go`: the integrity line's fields; `object_digests_test.go` pins them on a `-N` row.
- test-only: `management_test.go` (the INSERT's two new args); `chunk_store_structure_test.go` skips `.claude/` (an agent worktree inside the checkout is a second copy of the repo, and the walker read it).
- docs/API.md, internal/api/CLAUDE.md.

### Part A (#790)

- `internal/drivers/onedrive.go`: `downloadRanges` through `parfetch` + `fetchRange` + `odRangeWindow`; the unknown-length `Put` uploads from the spool (`chunkedUpload` deleted); `odChunkPool`/`newODChunkBuffer`, `odFragment` (the fenced fragment body); `odAttempts`, `odWait`, `retryWait`, `odTenant.wait`/`sleep`, the rewritten `graphDo` + `trackRateLimit`, `putChunk`; `odError` + `newODError` + `odIsNotFound` (by code); the three transports' header bounds; `List` (recursive) + `walkFiles` + `listChildren` (`children` now built on it), `RemoveContainerTree`; `HealthCheck` + `probe` + `logProbeChange` (`pickTenant` and the driver's rotation index deleted).
- `internal/drivers/onedrive_fleet.go` (new): the marker, `CheckFleetFingerprint`, the gauge (on `Collectors()`).
- `cmd/vaultaire/main.go`: `checkPermafrostFleet` (20 s, `PERMAFROST_FLEET_ACCEPT`).
- `internal/api/erasure_sweep.go`: `containerTreeRemover`, called after a container's deletes all succeeded.
- `deploy/monitoring/vaultaire-drivers.yml` (`PermafrostFleetMismatch`) + `deploy/monitoring/tests/vaultaire-drivers_test.yml`.
- Tests: `onedrive_retry_test.go`, `onedrive_stream_test.go`, `onedrive_fleet_test.go` (drivers), `TestErasureSweep_AFolderTree*` (api); `TestOneDriveSweepShape_…` rewritten for the key shape; the `pickTenant` test deleted with it; two rows in `data_path_allocations_test.go`.
- Docs: `onedrive_README.md` (throttling as built, the driver status, § Fleet fingerprint with the procedure), drivers `CLAUDE.md` (the row, the bound-invariant exception, the tenant-walk note), api `CLAUDE.md` (the sweep), root `CLAUDE.md` (`TENANT_N_*`, `PERMAFROST_FLEET_ACCEPT`, the deletion paragraph).

### Part B (#791)

- New: `internal/database/migrations/095_webhook_outbox.sql`; `internal/api/webhook_outbox.go` (`recordWebhookIntents`, `finishWebhookAttempt`, `skipOutboxRows`, `WebhookRetrier` = the `webhook_retry` job, the metric); `internal/api/webhook_outbox_test.go`; `deploy/monitoring/tests/vaultaire-jobs_test.yml`.
- Changed: `events.go` (the intent, `webhookSignature`, only a 2xx delivers), `event_delivery.go` (a drop makes the row due again; `skipOutboxRows` on re-validation), `object_delete_shared.go` (the batch's intents), `webhooks_routes.go` (rotate-secret, the transactional delete, the deliveries columns), `jobs_wiring.go`, `server.go`, `prom_metrics.go`, `retention.go` (the outbox's 30 d), `account_export_sections.go` (excluded with its reason), `internal/account/erase.go`, `internal/docs/openapi.go`, `deploy/monitoring/vaultaire-jobs.yml` (`webhook_retry` in the stale list).
- Docs: root `CLAUDE.md`, `internal/api/CLAUDE.md`, `internal/database/CLAUDE.md`, docs/API.md, `deploy/monitoring/README.md`.
- Tests changed for intended behaviour: `events_test.go` (the delete and deliveries mocks), `r11_review_test.go`, `jobs_runners_test.go`, `retention_test.go`.
- Built by a forked agent in its own worktree; reviewed here before this note (the CTE's backoff index reads the pre-update `attempts`; the lease covers a crashed attempt; the delete skips in its own transaction; the intent INSERT runs only when an endpoint is owed).

### Part 0 prod work

- **0.4 RP16-3.** The two blobs were under the bench tenant's prefix, not at the bucket root: `t-tenant-14a623b16b3f7012/tenant-14a623b16b3f7012_vbench-user1-vaultaire/bench/sustained/w2-336` on `stored-us-east-1` (16,777,216 B, LastModified **2026-10-12 14:46** — not July: the us-east-1 copy is Lyve's replication of the July blob) and `…/w2-690` on `stored-us-west-1` (16,777,216 B, 2026-07-30 04:36). Re-checked first: 0 head rows, 0 version rows, 0 `retired_blobs` rows, 0 active uploads, and no `buckets` row named `vbench-user1-vaultaire`. Deleted with the Lyve pair (aws-cli, `AWS_MAX_ATTEMPTS=1`) 2026-10-19 ~23:00 UTC; HEAD of each afterwards: gone; a listing of the bucket's prefix on both: none.
- **0.5 the prod read** (2026-10-19 22:49 UTC, `f47ef3a`): 19/19 jobs `ok`. `chunk_orphans` (01:44 run): every backend `complete`, 0 found / 0 erased / 0 failed — lyve listed 38,576 (all named), wasabi 1,318, **idrive 1,049 — the dead iDrive key still LISTS** (the same 1,049 chunks `dedup_gc` cannot sweep: List and HeadBucket pass, object calls 403). `routing_truth` (06:26 run): idrive 10 of 501 sampled, 10 errors (10 consecutive: the rest not asked), 0 verdicts. Alerts: `BackendProbeFailing` × 11 (dead iDrive) + `RoutingTruthNoVerdict` × 1 — correct, not silenced. 0 Error lines since 18:00 UTC. The 2026-10-20 01:30 run had not happened when this note was merged (00:50 UTC); the read above is the 10-19 run, the first with prod listing history.

## Before → after

| # | Item | Before | After | Test |
|---|---|---|---|---|
| 0.1a | DELETE on an MFA-Delete bucket whose flag read fails | **204, deleted** (read as "off") | 503 ServiceUnavailable + Retry-After, row kept | `TestMFADelete_FlagReadFailureIs503OnDelete` (red: 204) |
| 0.1b | DeleteObjects, same | **200, every key Deleted** | 503, 0 deleted | `TestMFADelete_FlagReadFailureIs503OnTheBatch` (red: 200) |
| 0.1c | versioning suspend, same | **200, Suspended** | 503, still Enabled | `TestMFADelete_FlagReadFailureIs503OnVersioningSuspend` (red: 200 Suspended) |
| 0.1d | a bucket with no row | nil (ungated) | nil (ungated) | `TestMFADelete_ABucketWithNoRowStaysUngated` |
| 0.2 | the integrity Error line on a `-N` row | `recorded_etag` = the body_md5 | `etag` (the `-N` ETag) + `expected_md5` | `TestBodyDigest_AWholeGetOfAMultipartObjectIsVerified` (pins the fields) |
| 0.3a | `create-bucket` with `x-amz-bucket-object-lock-enabled: true` | **a bucket without lock**, versioning disabled | lock on, versioning Enabled; PutObjectLockConfiguration sets a rule with no enable step; a config without lock is 409 | `TestCreateBucket_ObjectLockHeaderCreatesALockedVersionedBucket` (red) |
| 0.3b | the header `yes` | a bucket created | 400 InvalidArgument, no row | `TestCreateBucket_AMalformedObjectLockHeaderIs400AndCreatesNothing` (red) |
| 0.3c | re-create of an owned unlocked bucket with the header | 200, still unlocked | 409 InvalidBucketState, unchanged; a plain re-create stays 200 | `TestCreateBucket_ReCreatingAnUnlockedOwnedBucketAskingForLockIs409` (red) |
| 0.3d | `FALSE` / absent | plain | plain | `TestCreateBucket_ObjectLockHeaderFalseOrAbsentIsAPlainBucket` |
| A.1a | `Get` of 128 MiB, the reader has taken 1 MiB | **134,217,728 B fetched** (every range read whole) | ≤ the 64 MiB window + 1 MiB; the stream byte-equal (sha256) | `TestOneDriveGet_RangesStreamThroughABoundedWindow` (red: 134,217,728 > 68,157,440) |
| A.1b | a range answered 200 (the whole object) | **spliced into the stream, no error** | the stream fails | `TestOneDriveGet_ARangeAnsweredWithTheWholeObjectIsAnError` (red: nil error) |
| A.1c | unknown-length `Put` of 400 MiB | **924,887,624 B allocated** (spool + `io.ReadAll` of it) | the driver's own ≤ 128 MiB beyond the spool (one pooled 60 MiB buffer); 7 fragments, every byte verified | `TestOneDrivePut_AnUnknownLengthUploadsFromTheSpooledBodyWithoutASecondCopy` (red: 924,887,624 ≥ 134,217,728) |
| A.1d | known-length `Put`, small `Get` | streamed / one GET | same | `TestOneDrivePut_AKnownLengthStreamsToo`, `TestOneDriveGet_ASmallObjectIsOneStream` |
| A.2a | a Graph call answering 503 for good, request path | **5 attempts, 30.6 s of sleeps** | 2 attempts, 1 wait | `TestGraphDo_AFailingCallIsTriedTwiceOnARequestPath` (red: 5) |
| A.2b | the same under `common.WithJob` | 5 | 3 | `TestGraphDo_AJobGetsThreeAttempts` |
| A.2c | 429 `Retry-After: 120`, caller deadline 2 s | **slept 120 s** (the test hung past 25 s) | fails at once (< 1 s), 1 attempt | `TestGraphDo_ARetryAfterPastTheDeadlineIsNotWaitedFor` (red: timeout) |
| A.2d | `Retry-After: 120`, no deadline | 120 s | 30 s (`odMaxRetryWait`) | `TestGraphDo_ARetryAfterIsCappedWithNoDeadline` |
| A.2e | the caller cancels during a 10 s wait | **slept on** (the test hung past 25 s) | returns `context.Canceled` at once, no further attempt | `TestGraphDo_AWaitEndsWhenTheCallerGoes` (red: timeout) |
| A.2f | the caller already gone | **5 attempts, 21 s** | 1 attempt, no wait | `TestGraphDo_ACallerAlreadyGoneIsNeverRetried` (red: 5) |
| A.2g | an upload fragment answering 500 / 503 | 3 attempts, `time.Sleep` 1/2/4 s | 2 on a request path, 3 under a job, waits on ctx (a cancel ends a 1 s wait at once) | `TestPutChunk_RetriesAreBoundedAndWaitOnCtx`, `TestPutChunk_ACallerGoneMidWaitStopsAtOnce` |
| A.2h | the transports' response-header bounds | none (0) | 15 s Graph, 25 s CDN, 2 min upload | `TestOneDriveTransports_AreBoundedByRequestClass` |
| A.3a | `List` of a container holding `k.txt`, `photos/a.jpg`, `photos/2024/b.jpg`, … on two accounts | **the direct children** — folder names `photos`, `other` in place of keys; for prefix `photos/2`: `nil` | every key relative to the container, once, the union; a prefix walks only the folders that can hold it | `TestOneDriveList_IsRecursiveRelativeToTheContainerAndTheFleetsUnion` (red: elements differ; the prefix listing nil) |
| A.3b | a nested folder with a second page | the page never read | both pages | `TestOneDriveList_FollowsEveryPageOfANestedFolder` (red) |
| A.3c | a nested folder answering 403 | **listing "complete"** | the listing fails | `TestOneDriveList_AFolderThatFailsFailsTheListing` (red: nil error) |
| A.3d | `RemoveContainerTree` emptied / with a key left | — | the container folder deleted on every account / `ErrDirNotEmpty`, nothing deleted | `TestOneDriveRemoveContainerTree_*` |
| A.3e | the erasure sweep on a folder-tree backend | the folder names would stay (with the new List) | the keys, then the folder tree; a failure defers the tenant | `TestErasureSweep_AFolderTreeBackendLosesItsKeysThenItsFolders`, `TestErasureSweep_AFolderTreeThatCannotBeRemovedDefers` (red without the hook) |
| A.4a | first boot, no markers | nothing recorded | a marker per account, state `recorded`, gauge 0 | `TestFleetFingerprint_FirstBootRecordsAMarkerOnEveryAccount` |
| A.4b | `TENANT_2` removed (3 → 2) | **silent remap** | `mismatch`, one Error line (`size 3 → 2`, `index 1: …`, the fingerprint), gauge 1, nothing rewritten | `TestFleetFingerprint_ARemovedAccountIsAMismatchNamedInOneErrorLine` |
| A.4c | an appended account | silent remap | `mismatch` | `TestFleetFingerprint_AnAppendedAccountIsAMismatch` |
| A.4d | `PERMAFROST_FLEET_ACCEPT` = the digest / another value | — | `accepted`, rewritten / still `mismatch`; left set afterwards: `match` | `TestFleetFingerprint_AcceptingTheDigestRewritesTheMarkers` |
| A.4e | a marker unreadable / one missing | — | `unknown`, nothing written, no Error / `repaired` | `TestFleetFingerprint_AnUnreadableMarkerIsNoVerdict`, `…AMissingMarkerOnAMatchingFleetIsRewritten` |
| A.4f | the gauge | — | on `drivers.Collectors()`, 0 from registration; rule `PermafrostFleetMismatch` | `TestFleetFingerprint_TheMismatchGaugeExistsAtZero`, `deploy/monitoring/tests/vaultaire-drivers_test.yml` |
| A.5a | 3 accounts, one answering 401 | **nil on 2 probes of 3** (one rotating account) | an error naming it on every probe | `TestOneDriveHealthCheck_ADeadAccountFailsEveryProbeAndIsNamed` (red: nil) |
| A.5b | one account hung, probe budget 300 ms | nil (another account asked) | the hung account named, < 2 s | `TestOneDriveHealthCheck_AHungAccountIsBoundedByTheProbe` (red: nil) |
| A.5c | a 404 `ResourceNotFound` (the drive gone) | a miss | the account's failure, never a miss | `TestODError_A404ThatIsNotAnItemMissIsNotAMiss` |
| A.5d | a 400 whose message holds "409" creating the root folder | **"exists"** (text match) | an error; a 409 `nameAlreadyExists` still looks the folder up | `TestEnsureRootFolder_OnlyAConflictIsTheFolderExisting` (red: nil error) |
| A.5e | a miss on every account | `engine.NotFoundError` | same (a guard: it already held, Prompt 2b.5 F1.1) | `TestOneDriveGet_AMissEverywhereIsATypedEngineMiss` (passed on the old driver) |
| B.1a | a receiver that answers 500, 500, 200 | 1 attempt, the event lost | delivered on attempt 3, `delivered_at` set | `TestWebhookOutbox_AReceiverThat500sTwiceThen200sGetsTheEvent` |
| B.1b | a receiver down for good | 1 attempt | `failed` at attempt 5, the waits exactly 1/5/25/125 s (injected clock and jitter, no sleeps) | `…AReceiverDownForGoodGoesTerminalAtTheFifthAttempt` |
| B.1c | a pool drop | a failed deliveries row only | the row due at +30 s, `attempts` 0; a dropped retry stays due | `…APoolDropLeavesTheRowDueWithoutSpendingAnAttempt` |
| B.1d | an attempt that never finished (a crash) | — | retried when its lease ends | `…AnAttemptThatNeverFinishedIsRetriedWhenItsLeaseEnds` |
| B.1e | an endpoint disabled or deleted | — | `skipped`, no POST | `…ADisabledOrDeletedEndpointsRowsAreSkipped`, `…AnEndpointDisabledWhileItsFirstAttemptIsQueuedEndsSkipped` |
| B.1f | retention | — | terminal rows over 30 d pruned, pending rows kept; `TestRetention_PoliciesMatchThePolicyPage` names the table | `…RetentionPrunesTerminalRowsAfter30DaysOnly` |
| B.2 | the signature | `sha256=<HMAC(body)>`, replayable for ever | `t,v1,sha256`; a 6-minute-old `t` fails; `t` is under v1 | `TestWebhookSignature_IsTimestampedAndKeepsTheLegacyElement` |
| B.3 | rotate-secret | no endpoint | 200 + the new secret once; the old one fails, the new one verifies; audit `webhook.secret_rotated`; another tenant's or a missing id 404; a queued job signs with the new one | `TestRotateWebhookSecret_*` (2) |
| B.4 | `retry_count` in `GET …/deliveries` | always 0 | 0…n with `next_retry_at` | `TestListDeliveries_RetryCountIsTheAttemptsFromTheOutbox` |
| B.m | `vaultaire_webhook_retries_total` | — | 3 series at 0 from boot | `TestWebhookRetries_EveryOutcomeExistsAtZeroFromBoot` |
| B.r | `webhook_retry` in `PeriodicJobStale` | — | promtool SUCCESS; FAILED against `main`'s rule file (no alert) | `deploy/monitoring/tests/vaultaire-jobs_test.yml` |

Part B's red: against `23a61a5` the new tests do not compile (new symbols), so each was shown red by a mutation of the code it covers — no intent row; an attempt's end not moving the outbox; no backoff; a drop spending an attempt; no retention entry; `skipOutboxRows` removed; the legacy signature; no generation bump on rotate; `retry_count` hard-coded 0 — each applied, seen failing, restored. A first run of the existing `TestEventDeliveries_ADroppedRowNamingAGoneWebhookOrEventCostsNoOtherRow` caught a bug in the first version ("ON CONFLICT DO UPDATE cannot affect row a second time" when a batch names a pair twice) → `DISTINCT ON`.

## Prod proof

Deploys: #790 `255192e` 2026-10-19 23:42 UTC (slot 8001), #789 `73856db` 23:48 (slot 8000), #791 `4429cd2` 2026-10-20 00:07 (slot 8001, Deploy 00:09). Every bucket below was created for the proof as the bench tenant (`tenant-14a623b16b3f7012`, key `VLT_LF4A…`) through `s3.stored.ge` with aws-cli (`AWS_MAX_ATTEMPTS=1`) and removed after it.

### Part A on prod (`255192e`)

- **The fingerprint at boot.** Slot 8001's boot: `permafrost fleet fingerprint` `state=recorded fingerprint=06efac2e47795187 accounts=3 written=3` (3.3 s after the fleet was initialised), then `permafrost account probe: up` for `tenant-1`, `tenant-2`, `tenant-3` — the first probe names every account. Slot 8000's boot (`73856db`, 6 min later): `state=match written=0`. A third check from the probe tool against the prod env: `{State:match Digest:06efac2e47795187}`. `/metrics`: `vaultaire_permafrost_fleet_mismatch 0`, `vaultaire_backend_health{backend="permafrost"} 1`.
- **The rule installed.** `vaultaire-drivers.yml` from `main` → `/etc/prometheus/rules/` (`promtool check rules`: 4 rules; `promtool test rules vaultaire-drivers_test.yml`: SUCCESS), Prometheus reloaded (13 groups), `PermafrostFleetMismatch` health `ok`, state `inactive`. **sha256 of all 12 `deploy/monitoring/vaultaire-*.yml` = the box's files** (RP16-11's check).
- **The driver against the prod fleet** (a throwaway probe built from the branch, the real `NewOneDriveFleetDriver` with the prod `.env`, not committed): `HealthCheck` 3 accounts, nil, 398 ms. Four keys written under the bench tenant's container `…_p16-permafrost` (`top.txt`, `photos/a.jpg`, `photos/2024/b.jpg`, `photos/2024/deep/c.jpg`): `List` → **4 keys (3 nested)** in 3.1 s; prefix `photos/2024/` → the 2 under it. **1,100 MiB** written (`Put` with a length, 73 MB/s) and read back (`Get`, 8 MiB ranges, 69.6 MB/s), sha256 equal; the process's RSS **85 MiB before the read, 247 MiB at its peak** (sampled every 100 ms) — the 64 MiB window plus GC slack, never the object (the old driver held every range of it). Deleted; then the cleanup the erasure sweep now runs: 4 deletes, `RemoveContainerTree` nil, `List` 0, and the tenant's top-level folders no longer include `…_p16-permafrost`.
- **Found on the way:** the bench tenant's permafrost tree held two folders no row names — `…__parity` (an empty container folder of the parity job; left alone) and `…_vbench-user1-vaultaire` (an empty July `bench/…` folder skeleton, no file — the shape the new sweep step removes). The skeleton was removed with `RemoveContainerTree` (List 0 before, nil, gone from the listing after). No head row, version row or parity row names anything on permafrost (`object_head_cache`: 0 rows on `permafrost`/`onedrive`; `vault_parity`: 0 rows — the parity leg has been `sync` since 2026-10-07).

### Part 0 on prod (`73856db`)

| Request | Answer |
|---|---|
| `create-bucket --object-lock-enabled-for-bucket` (`p16-lock-1792453830`) | 200; `get-bucket-versioning` **Enabled**; `get-object-lock-configuration` **`ObjectLockEnabled: Enabled`** (before: no lock, versioning off — RP16-4) |
| `put-object-lock-configuration` GOVERNANCE 1 day (no separate enable) | 200; read back with the rule |
| `put-object k.txt` | ETag `401b30e3…`, version `68fbbd54-…`; `get-object-retention` GOVERNANCE until 2026-10-20 23:50 |
| `delete-object k.txt` | 403 AccessDenied "Object is protected by Object Lock." (our rule: every delete flavour) |
| a SigV4-signed `PUT /p16-bad-…` with `x-amz-bucket-object-lock-enabled: yes` (curl `--aws-sigv4`) | **400 InvalidArgument** "x-amz-bucket-object-lock-enabled must be true or false."; HEAD bucket 404 |
| cleanup: `delete-object --version-id … --bypass-governance-retention`, `delete-bucket` | 0 versions, 0 markers, bucket removed |
| a plain bucket: PUT, DELETE (0.1's regression check: no MFA Delete, the flag readable) | DELETE 204, HEAD 404; bucket removed |

0.1's 503 needs the flag read to fail, which prod cannot be made to do: it is proven by the handler tests on a driver that fails only that statement. 0.2 is a log field: pinned by the handler test.

After both deploys (23:40 → 23:58 UTC): **0 Error lines**; alerts `BackendProbeFailing` × 11 (the dead iDrive key; 4 of them re-pending on the new slot's fresh series), `RoutingTruthNoVerdict` × 1 (correct), `DriverHedgesWinning{driver="wasabi"}` × 1 — the dormant Wasabi driver's probe, 3 HeadBuckets hedged and all 3 won by the hedge (the 2026-10-17 edge-flow loss, not these changes).

### Part B on prod (`4429cd2`, slot 8001)

- Before the first event: `webhook_outbox` 0 rows; the `webhook_retry` job's first run 00:09:52 `ok` `{"due":0,"skipped":0,"submitted":0}`; `vaultaire_webhook_retries_total{delivered,exhausted,retry}` all 0 on `/metrics`.
- **A listener the SSRF gate allows**: `http://38.147.105.54:18791/hook` — the box's own public address (a public IP, so `webhookClient` dials it; UFW keeps the port unreachable from outside, the delivery comes from the box). Bucket `p16-wh-1792455004` through `s3.stored.ge`; webhook `6d6df004-…` for `object.created`, created through `/api/v1/webhooks` after a real `/auth/login` as the bench user (no minted token).
- PUT `a.bin` 00:10:11 → delivered on attempt 1; `v1` and the legacy element verify; `t` 0.7 s old on arrival.
- **Listener killed 00:10:38; PUT `b.bin` 00:10:39:** attempt 1 00:10:39 `connect: connection refused` (next due 00:10:40); attempt 2 00:10:52 failed (next 00:10:56); attempt 3 00:11:22 failed (next 00:11:48). **Listener back 00:11:39 → attempt 4 delivered 00:11:52**, status 200, the signature verified. (The retry job's 30 s tick is why each attempt goes out up to 30 s after its due time.)
- `GET /api/v1/webhooks/{id}/deliveries`: `retry_count` 0, 1, 2 (each with its `next_retry_at`), then 3 delivered (`next_retry_at` null); `total_count` 5.
- **rotate-secret** 00:12:10 → a new `whsec_`, audit row `webhook.secret_rotated`; PUT `c.bin` verified under the new secret and failed under the old (`v1` and legacy both false).
- After: `retry` 3, `delivered` 1, `exhausted` 0; `vaultaire_job_runs_total{job_name="webhook_retry",outcome="ok"}` 6. Cleanup: the webhook deleted through the API (its 3 delivered outbox rows keep, `endpoint_id` NULL, for the 30 d retention), objects and bucket removed (0 head rows, 0 buckets, 0 endpoints left), the listener stopped, the proof's temp files (secrets, the JWT header) removed. 0 Error lines from 00:07.
- **Rules:** `vaultaire-jobs.yml` installed (sha `df9c16fa…` = the repo; backup `.bak-20261020`), `promtool check rules` 9 rules SUCCESS, Prometheus reloaded (13 groups), `PeriodicJobStale` watches `webhook_retry`, health ok. sha256 of every `deploy/monitoring/vaultaire-*.yml` on `origin/main` = the box (12 of 12) — checked after Part A's install too.

## Residuals

| # | Sev | Item | Where it goes |
|---|---|---|---|
| RP17-1 | P3 | **HEAD/GET never return the Object Lock headers.** `x-amz-object-lock-mode`, `x-amz-object-lock-retain-until-date`, `x-amz-object-lock-legal-hold` are accepted on PUT (`s3_lock.go`) but never sent back; aws-cli `head-object` shows `None` for an object under a GOVERNANCE default retention (seen in Part 0's proof; `get-object-retention` answers right). | a conformance WP (WP-R2-4's header writer is the one place) |
| RP17-2 | P3 | **`materialize` allocates ~256 MiB (512 under `-race`) to spool a body into its 64 MiB in-memory buffer** (bytes.Buffer growth), on every unknown-length PUT of the drivers that use it (Geyser, iDrive/Wasabi, permafrost). Rare on the data path (an S3 PUT carries its length), so no budget row; measured in Part A's test, which subtracts it. | a small driver fix (grow in fixed steps, or spill sooner) |
| RP17-3 | note | **No fleet rebalance tool.** The fingerprint names a changed `TENANT_N` order and `PERMAFROST_FLEET_ACCEPT` accepts one, but moving each object to its new home is by hand (`onedrive_README.md` § Fleet fingerprint step 2); the fleet walk keeps reads working meanwhile. Nothing routes to permafrost today (0 head rows, 0 parity rows). | before the fleet grows past 3 |
| RP17-4 | note | **A failed permafrost range is not retried** (parfetch `HedgeAfter` 0, as before): one CDN error fails the GET. And the ranges are now fetched as the reader consumes them, so a GET that outlives Graph's pre-authenticated `downloadUrl` (about an hour) fails mid-stream where the old driver had fetched every range up front (holding the object). A reader at the parity rebuild's speed (~70 MB/s on prod) reads 250 GB in that hour. The vault parity rebuild is the only reader. | WP-VAULT-1 follow-up if the parity leg moves back to permafrost |
| RP17-5 | note | **The rotate-secret answer is cached with its new `whsec_`** for 24 h in `idempotency_cache` when the caller sends an `Idempotency-Key` (create-webhook already did the same). | the idempotency middleware should not cache secret-bearing bodies |
| RP17-6 | note | **The legacy `sha256=` element** goes one release after #791; the comma-joined header changes what a whole-header comparer sees (no receiver exists today). | the release after |
| RP17-7 | ~~note~~ | ~~`webhook_retry` adds a "job run completed" Info line every 30 s (~2,900 a day).~~ **Withdrawn (post-merge review, RP18-3): the scheduler already logs a 0-row interval run at Debug; prod shows 3 lines in the proof hour, 0 since.** | — |
| RP17-8 | note | **A shared `vaultaire_test` with parallel branches**: a branch's migration applied to the shared local DB makes the other branches' schema-walk tests fail (`TestExportSectionsCoverSchema` saw `webhook_outbox` on Part 0's and Part A's runs); `TestRoutingRuleFile_MatchesTheExportedSeries` counts global NULL `backend_name` rows, and `internal/dashboard/handlers/r9_schema_test.go` leaks two (`test-r9-dash-*`). CI is unaffected (one branch per DB). | RP16-9's test-hygiene WP |
| RP17-9 | note | **A new merge-queue shape (RP16-10):** #789's auto-merge, armed while its checks ran, was never enqueued when they went green (CLEAN, armed, not queued) — the watcher only re-armed a cleared auto-merge. Disabling and re-arming queued it at once; the watcher (`scratchpad/queuewatch.sh`) now does that after 3 polls. | done; the `gh` alias / workflow of RP16-10 would cover it |
| RP17-10 | note | `dispatchWebhooks` (events.go) was unreachable before #791 (deadcode reports it) and still is. | dead-code sweep |
| RP16-3, RP16-4, RP16-5, RP16-6 | — | closed here (Part 0) | — |
| WP-R7-4 (R7-06/07/08/09/18, RP9-1), WP-R11-3 (R11-11, R13-15) | — | closed by #790, #791 | — |

Still open from WP-P15, unchanged: RP16-7 (shellcheck on the box), RP16-8 (the hook's golangci-lint v1), RP16-9 (the test pools), RP15-6 (owner: the unit's memory lines, `MULTIPART_STAGING_DIR`).

## Gates

- **Part 0:** golangci-lint v2.14.0 0; `go test -race ./internal/api` ok except `TestExportSectionsCoverSchema`, which saw Part B's `webhook_outbox` on the shared local DB (RP17-8); red runs of every new test with the fix stashed (204 / 200 / Suspended; lock ignored, no 400, no 409); CI `build-and-test` green, merge group green.
- **Part A:** golangci-lint 0; gosec (the Security workflow's flags) 0 on `internal/drivers`, `internal/api`, `cmd/vaultaire`; `go test -race ./internal/drivers ./cmd/vaultaire` ok (drivers 120 s); `go test -race ./internal/api` ok except RP17-8's test; **every behaviour test run against `origin/main`'s driver in a scratch worktree** (the tests that need new symbols dropped there): the red results in the table, two of them a 25 s test timeout (the old sleeps); the api sweep tests red with the hook stashed; CI green; the promtool test SUCCESS on the box.
- **Part B:** golangci-lint 0, gosec clean, no new dead code; full `go test -race ./...` on a private migrated `vaultaire_p16b`: 40 packages ok (`internal/api` after removing RP17-8's two leaked rows from that private DB); after the rebase onto #790: api, drivers, account, database, docs ok; CI green twice, merge group green, Deploy green.

## Post-merge review (2026-10-20, meta session)

Reviewed on `origin/main` `4429cd2` (#789 `73856db`, #790 `255192e`, #791 `4429cd2`), prod `4429cd2` on slot 8001, after the docs PR. CI on `main` green for all three merge commits (CI, Deploy, Security). The 12 rule files on the box = `origin/main` (sha256, 12 of 12 — the RP16-11 check). Prod read 01:05 UTC: `/health` degraded for the 11 dead iDrive probes only, 0 `"level":"error"` lines since the 00:07 deploy, 1,410 warn lines = the 11 iDrive canaries × 128 probes, alerts = `BackendProbeFailing` × 11 + `RoutingTruthNoVerdict` (correct), 19/19 jobs `ok`, `webhook_retry` `{"due":0}`, `webhook_outbox` 3 `delivered` (the proof's), 0 endpoints, `vaultaire_permafrost_fleet_mismatch 0`, `vaultaire_webhook_retries_total` retry 3 / delivered 1 / exhausted 0. The docs PR #792 sat CLEAN with auto-merge armed and never enqueued (RP17-9's shape) — disabled and re-armed, queued at once.

**Verdict: every claim holds. No P0/P1/P2.** What was checked against the diffs:

- **#789.** `checkMFADelete`'s three callers all answer `*mfaDeleteUnknownError` with 503 + `Retry-After: 5`; `sql.ErrNoRows` stays nil; the versioning path's error-discarding pre-read is gone. `x-amz-bucket-object-lock-enabled` is read only by the S3 `CreateBucket` — the management API and the dashboard both create through `createBucketRegistry` (WP-R12-10) with empty options, so no entry point can create a half-locked bucket; an owned unlocked bucket re-created with the header is `bucketCreateLockMismatch` → 409, an owned locked one 200. `PutBucketVersioning Suspended` on a lock bucket is still accepted (R4-10, WP-R4-2 — not this PR's scope).
- **#790.** `walkFiles` prunes by `HasPrefix(dir, prefix) || HasPrefix(prefix, dir)` (a prefix inside a folder descends it, a sibling is skipped); a 404 of a folder gone mid-walk is nothing, any other error fails the listing. `RemoveContainerTree` lists first and refuses `ErrDirNotEmpty`. `ListDir`/`RemoveEmptyDir` (the parity reconcile's) are untouched. `graphDo`: a body without `GetBody` is never re-sent; `retryWait` returns at once past the deadline or over 30 s; `putChunk` re-sends the same `Content-Range` from the same buffer, fenced after each attempt. `HealthCheck` probes every account under the probe's ctx and names each failing one. The fingerprint marker sits under `vaultaire/_fleet/`, outside every `t-<tenant>/` tree, so no sweep, walk or reconcile can see it. `fetchRange` refuses anything but a 206 with the exact `Content-Range`.
- **#791.** The CTE's backoff index reads the pre-update `attempts` (Postgres SET semantics): attempt 1 → 1 s … attempt 4 → 125 s, the fifth failure terminal; the `[5]` element is never evaluated (CASE). `RunOnce` leases the rows in the same transaction it takes them (`FOR UPDATE OF o SKIP LOCKED`), so a 30 s tick cannot double a queued retry; a drop makes the row due in 30 s with `attempts` unchanged (`ON CONFLICT … WHERE state = 'pending'`). `revalidateEndpoints` filters by the job's own endpoint ids, so a retry POSTs only to its endpoint. The retry's body is built from `events.data`, which is the `payload` attempt 1 was built from (`recordEvent`). `webhook_outbox` is erased by tenant, excluded from the export, pruned at 30 d (terminal rows only). `vaultaire_webhook_retries_total` is pre-created at 0 for all three outcomes.

### Findings

| # | Sev | Item | Where it goes |
|---|---|---|---|
| RP18-1 | P3 | **The fleet fingerprint is checked at boot only.** `checkPermafrostFleet` runs once in `main`; a marker unreadable at boot (Graph down, a 20 s budget) is `unknown` with the gauge at 0 and nothing re-checks for the slot's lifetime — a changed `TENANT_N` order under a Graph outage at deploy time is invisible until the next deploy. Re-run the check from the probe after an `unknown` (and hourly), one Error line per change of verdict. | Prompt 17 Part 0 |
| RP18-2 | note | **A retry's envelope carries a new `created_at`.** `deliverToEndpoints` stamps `time.Now()` into the body, so attempt 1 and every retry of one `X-Event-ID` send different bodies (and different `v1` signatures). Harmless for a receiver that dedupes on the id; use the `events` row's `created_at` so one event is one body. | Prompt 17 Part 0 |
| RP18-3 | correction | **RP17-7 is not an issue.** The scheduler logs an `ok` interval run with 0 rows and no note at Debug (`jobs.go`, since WP-R13-3): prod shows 3 `webhook_retry` "job run completed" lines in the proof hour (the runs that submitted) and none since. Closed. | — |
| RP18-4 | note | `handlePutBucketVersioning` gates `MfaDelete=Enabled` on `object_lock_enabled`; AWS gates it on versioning. R4-10, already in WP-R4-2 with "Suspended accepted on a lock bucket" and "lock enabled on an unversioned bucket" (`handlePutObjectLockConfiguration` writes `object_lock_enabled` without touching versioning). | WP-R4-2 = Prompt 17 Part A |
| RP18-5 | note | The sweep's `RemoveContainerTree` is one more full `List` walk plus one folder delete per account under a single `SweepDeleteTimeout`; a very large tree defers the tenant until the walk fits. Fine at today's sizes (0 rows route to permafrost). | with RP17-3 (the rebalance tool) |

RP17-1, RP17-2, RP17-10 → Prompt 17 Part 0; RP17-5 → WP-R11-2; RP17-3/4/6/8/9 stand as filed.
