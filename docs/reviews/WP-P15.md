# WP-P15: Stage 1a Prompt 15

2026-10-19. Three parts:

- **Part 0**: the WP-P14 residuals (RP15-1 the A.4 tamper half on prod, RP16-1 on prod, RP15-7 the switch re-install, RP15-9 one docs clause) and the prod read.
- **Part A**: WP-R3-4 R4-17, MFA Delete on DeleteObjects.
- **Part B**: WP-R2-7, the key length and the bucket name on every object operation.

PRs:

- **#780**: Part A.
- **#781**: Part B.
- **#782**: test-only — the two WP-P14 tests that failed three CI runs today wait for their condition instead of sleeping; CI `max_connections` 1000.
- this PR: Part 0's clause (RP15-9 in the root CLAUDE.md `job_runs` entry) and this note, docs only.

Start sha: `origin/main` `75afd31` (#779). **No migration** (095 stays reserved for WP-PAY-1). Engine lane; the surfaces are the developer surface's S3 contract (docs/surfaces/DEVELOPER.md) and `docs/ARCHITECTURE_CONTRACT.md` end state 2 (a refusal is a refusal on every path that reaches the bytes).

## Decisions

1. **One MFA check per batch.** `mfa_delete_enabled` is a bucket column and the code is one header, so `deleteObjects` calls the single DELETE's `checkMFADelete` once, after the bucket-exists check and before `longOpBegin`: a refusal is a real 403 `AccessDenied` (as the single DELETE answers), never an `<Error>` inside a committed 200. On an MFA-Delete bucket the batch now needs the header like the single DELETE does — stricter than AWS, which asks it only for a version-id delete or a versioning change (docs/API.md says so). A bucket without MFA Delete needs nothing.
2. **The gate runs once, in the router.** `requestNameRefused` (`internal/api/s3_request_names.go`) runs right after `systemBucketRefused`, before any handler, so every object operation answers the same and none can be forgotten: a key over **1,024 bytes** (S3's limit) is 400 `KeyTooLongError`; a bucket name `validateBucketName` refuses is 400 `InvalidBucketName`.
3. **The 857-byte write limit stays a separate rule.** The writes' `blobKeyTooLong` (the blob address `<key>@<26>` must fit the backends) keeps refusing 858–1,024-byte keys on PUT/Complete/Copy/direct upload with its own message; the new gate is S3's limit on every op. A 1,024-byte key is not refused by the gate (a GET of it is 404).
4. **What the bucket gate does not touch.** A `_` system name (`tenant.IsSystemBucket`) keeps NoSuchBucket from `systemBucketRefused` (the export bucket stays invisible, and its GetObject/HeadObject still work); CreateBucket and DeleteBucket keep their own checks and answers. Prod before the gate: 5 buckets, 0 names it refuses, longest key 38 bytes.
5. **A batch key over 1,024 bytes is a per-key error.** `DeleteObjects` names it `KeyTooLongError` in `<Error>` and deletes the other keys (S3's shape); the batch is not refused.

## What was built

### Part 0 (prod, no code)

- **0.1 RP15-1.** There was no unnamed P14 stray: `stored-us-east-1` held 117 blobs for the bench tenant, 116 named by `retired_blobs` (115 `version_deleted`, 1 `overwritten`) including both 200 MB `p14-ver` blobs (`mp200.bin@cvdex…`, `@tvst6…`, due 10-20 03:53). The A.4 proof was re-run on the exact driver address (below).
- **0.2 RP16-1** proven on prod (below).
- **0.3 RP15-7.** `/usr/local/sbin/vaultaire-switch` re-installed from the repo (the diff was exactly `chunk_puts_in_flight`), sha `4ac55a02d16a9b53` = the repo file; `status` OK.
- **0.4 RP15-9.** One clause in CLAUDE.md's `job_runs` entry.

### Part A (#780)

`internal/api/s3_batch.go`: the `checkMFADelete` call above. Tests `internal/api/s3_mfa_delete_batch_test.go`. CLAUDE.md (api) and docs/API.md updated.

### Part B (#781)

`internal/api/s3_request_names.go` (`maxS3KeyBytes`, `requestNameRefused`, `keyTooLongHint`), its call in `s3.go`, the per-key check in `s3_batch.go`. Tests `internal/api/s3_request_names_test.go` (real SigV4). CLAUDE.md (api) and docs/API.md (`KeyTooLongError` in the 400 row; "Names are checked before anything else").

## Before → after

| # | Item | Before (`75afd31`) | After | Test |
|---|---|---|---|---|
| A.a | DeleteObjects on an MFA-Delete bucket, no `x-amz-mfa` | **200, every key Deleted** (R4-17) | 403 AccessDenied, rows intact | `TestMFADelete_BatchWithoutHeaderIsRefusedOnAnMFABucket` (red: 200 `<DeleteResult>`) |
| A.b | an invalid code | 200, deleted | 403 | `TestMFADelete_BatchWithAnInvalidCodeIsRefused` |
| A.c | MFA bucket, no MFA on the account | 200, deleted | 403 | `TestMFADelete_BatchOnAnMFABucketWithNoMFAOnTheAccountIsRefused` |
| A.d | a valid code | 200 | 200, deleted | `TestMFADelete_BatchWithAValidCodeDeletes` |
| A.e | the refusal under a 1 ns keep-alive threshold | a committed 200 | a real 403 (decided before `longOpBegin`) | `TestMFADelete_BatchRefusalPrecedesTheKeepAlive` (red: committed 200) |
| A.f | a bucket without MFA Delete | 200 | 200, no header needed | `TestMFADelete_BatchOnANonMFABucketNeedsNoHeader` |
| B.a | a 1,100-byte key: GET / HEAD / DELETE / tagging / retention | 404 NoSuchKey / 404 / **204** / 404 / 404 | 400 `KeyTooLongError` on each | `TestKeyLength_AnObjectOpWithAKeyOver1024BytesIs400` |
| B.b | a 1,024-byte key | 404 | 404 (not the gate's) | `TestKeyLength_A1024ByteKeyIsNotRefusedByTheGate` |
| B.c | a 900-byte PUT | 400 (the 857 rule) | 400, the 857 rule's message | `TestKeyLength_WritesKeepTheStricter857Refusal` |
| B.d | object ops on `Locked`, `ab`, 64 × `a`, `a..b` | GET 404 NoSuchKey, PUT/LIST 404 NoSuchBucket, DELETE **204** | 400 `InvalidBucketName` on each | `TestBucketName_AnObjectOpOnAMalformedNameIs400` |
| B.e | a `_` system name | NoSuchBucket | NoSuchBucket | `TestBucketName_ASystemBucketNameIsStillNoSuchBucket` |
| B.f | GetObject/HeadObject of an `_exports` key | 200 | 200 | `TestBucketName_SystemBucketReadsStillWork` |
| B.g | a valid bucket round trip | 200 | 200 | `TestBucketName_AValidBucketIsUntouched` |
| B.h | DeleteObjects with one key over 1,024 bytes | Deleted (a no-op on a key that cannot exist) | that key `<Error>` `KeyTooLongError`, the others Deleted | `TestDeleteObjects_AKeyOver1024BytesIsAPerKeyError` |

**An existing test changed for intended behaviour:** `s3_r4_review_test.go` named its missing bucket from the tenant id (an invalid name, now 400); it uses `no-such-bucket-r4`.

## Prod proof

### Part 0.1: A.4 altered bytes on a `-N` row (2026-10-19, `75afd31`)

Bucket `p15-a4v-1792417074` (versioned, via `s3.stored.ge`): a 200,000,000-byte multipart upload, ETag `545fe20e…-24`, `body_md5` `ce50243c…` = `md5sum` of the file, backend `lyve`. The blob at `t-tenant-14a623b16b3f7012/tenant-14a623b16b3f7012_p15-a4v-1792417074/mp200.bin@vyw7ame902bb5s4tqv7520de0c` was overwritten with the backend's pair by other bytes of the same length. A whole GET: aws-cli `IncompleteRead(199999488 bytes read, 512 more expected)` — the bytes received are a prefix of the tampered blob, the last 512 withheld; `vaultaire_object_integrity_failures_total{path="plain",kind="md5"}` 0 → 1; one Error line (`bytes_sent` 199,999,999). Cleaned up: versions deleted, the bucket removed, the tampered blob queued `version_deleted` (due 10-20 13:38). A first attempt's bucket (`p15-a4-1792417057`, unversioned, chunked by the box's 1 GB multipart threshold — no `-N` row) was removed too.

### Part 0.2: RP16-1 (#779) on prod

Bucket `p15-rp16-1792417188`, versioned. Two 20 MB `-3` versions; `DELETE ?versionId=<current>` → the head row's `body_md5` is v1's (`bcf6b8ab…`), a whole GET byte-equal to v1, the integrity counter unchanged. SSE-C: two versions under two keys; after deleting v2, key 1 HEAD 200 (1,000,000) and GET byte-equal, key 2 HEAD 403 and GET `AccessDenied` ("The provided SSE-C key does not match…"). 0 Error lines; bucket removed.

### Part 0.5: the prod read (`75afd31`)

19/19 jobs `ok`. Alerts: `BackendProbeFailing` × 11 (the dead iDrive key) + `ObjectIntegrityFailure` × 1 (the deliberate 0.1 cut, clears within the hour). Error lines since 04:15: 1, the deliberate one. `retired_blobs`: 0 due (346 chunked rows due from 15:53, 121 lyve `version_deleted`, 1 `overwritten`). `routing_truth`: idrive rows 501, verdicts 0, errors 10 (unchanged).

### Parts A and B on prod

Deploys: #780 `a484692` 15:24 UTC, #782 `7ae486c` 16:51, #781 `f47ef3a` 16:58 (slot 8000). All as the bench tenant through `s3.stored.ge` with aws-cli (`AWS_MAX_ATTEMPTS=1`), every bucket created for the proof and removed after it.

**Part A (`a484692`), bucket `p15-mfa-1792425142`:** three objects; `mfa_delete_enabled` set on that one row by SQL. DeleteObjects of the three without `x-amz-mfa` → `AccessDenied`, 3 head rows kept. The flag cleared → the same batch answers 3 Deleted, 0 rows.

**Part B (`f47ef3a`), bucket `p15-names-1792429107`:**

| Request | Answer |
|---|---|
| GET / DELETE / PUT of a 1,100-byte key | 400 `KeyTooLongError` each; HEAD 400 (no body) |
| GET of a 1,024-byte key | 404 `NoSuchKey` (not the gate's) |
| GET on `Locked`, `ab`, `a..b`, 64 × `a` | 400 `InvalidBucketName` each |
| DeleteObjects of `a`, the 1,100-byte key, `b` | Deleted `a`, `b`; `<Error>` `KeyTooLongError` on the long key |
| PUT / GET / DELETE of `rt.txt` | ETag `eff5bc1e…`, GET byte-equal, DELETE 204 |

After the deploy: 0 Error lines since 16:58; alerts `BackendProbeFailing` × 4 + `DriverHedgesWinning` × 1 (the 0.1 `ObjectIntegrityFailure` has cleared).

## Residuals

| # | Sev | Item | Where it goes |
|---|---|---|---|
| RP16-3 | note | **Two July bench blobs no row names** on Lyve: `vbench-user1-vaultaire/bench/sustained/w2-336` (`stored-us-east-1`) and `w2-690` (`stored-us-west-1`), 16 MiB each. No collector names them; left in place. | operator (delete with the backend's pair) or the bench tenant's erasure |
| RP16-4 | P3 | **CreateBucket ignores `x-amz-bucket-object-lock-enabled`**: `create-bucket --object-lock-enabled-for-bucket` makes a bucket without lock; only PutObjectLockConfiguration enables it. | a conformance WP |
| RP16-5 | P3 | **`checkMFADelete` fails open on a DB error** (the read of `mfa_delete_enabled` failing reads as "off"), on the single DELETE and now the batch. Pre-existing; a 503 would be the fail-closed answer. | WP-R3-4 remainder |
| RP16-6 | note | The integrity Error line's field `recorded_etag` carries the `body_md5` digest on a `-N` row; `expected_md5` would read right. | a one-line rename |
| RP16-7 | note | `shellcheck` is not installed on the box, so `vaultaire-switch_test.sh` reports FAILED there on that check alone (every functional case passes; the whole test passes on the Mac). | operator (`apt install shellcheck`) |
| RP16-8 | note | The Mac's homebrew golangci-lint (v1.64.8, built with go1.25) cannot lint a go1.26 module, so the pre-commit hook fails; v2.14.0 (CI's) was used. | the hook config could pin v2 |
| RP16-9 | P3 | **The test DB pools are unbounded.** 66 `sql.Open` sites in the tests, none with `SetMaxOpenConns`; CI's Postgres at 400 connections still answered `too many clients` to `auth` and `database` when `api`, `auth`, `dashboard`, `dashboard/handlers` and `drivers` converged (#781's first run). #782 raises CI to 1000; the fix is one `testutil` opener with a bound and a `t.Cleanup` close, swept over every site. | a test-hygiene WP |
| RP16-10 | note | **Why today's merges took hours** (the owner's question): a PR is two 7–11 min CI runs (the PR's and the merge group's), serialized by the queue; two #777/#774 tests failed three runs on timing (fixed by #782); a failed merge-group run ejects the PR and clears auto-merge, and nothing re-arms it — #780 sat ejected 85 min because the session's watcher polled the PR's state, not its queue membership (the watcher now re-arms and re-runs, `scratchpad/queuewatch.sh`); and a GitHub Actions incident (15:49 UTC, "critical") held #781's third run queued with no runner. | done; a `gh` alias or a workflow that re-arms auto-merge after an ejection would close the human gap for good |
| RP15-6 | note | **Owner:** the unit's memory lines (`GOMEMLIMIT=40GiB`, `MemoryHigh=48G`, `MemoryMax=56G`) and `MULTIPART_STAGING_DIR=/opt/vaultaire/data/multipart` (`docs/DEPLOY.md`). | owner |
| RP15-1, RP15-2, RP15-7, RP15-9, RP16-1 (prod) | — | closed here | — |
| WP-R3-4 (R4-17), WP-R2-7 | — | closed by #780, #781 | — |

## Gates

- `/verify` of Part A and Part B against a local server on the private migrated DB `vaultaire_p15` with aws-cli (A: a batch without the code 403 with 3 rows kept, with a valid code all 3 deleted; B: every row of the table above).
- The full `go test -race ./...` on `vaultaire_p15` (DATABASE_URL): 40 packages ok, exit 0.
- golangci-lint v2.14.0: 0. `build-and-test` green on both PRs. CI failures on the way, none in the diffs: Postgres `too many clients` (auth, database) on #781's first run (RP16-9); `TestChunkedPut_WorkerFailureReleasesRefs` in #780's first merge group and `TestChunkPutBudget_AWriteDegradesToFewerChunksInFlightRatherThanRefusing` in #781's second run — both again on the main push after #780 merged, and the budget test a fourth time on #781's third run — fixed in #782 (both tests 5 × `-race` and 3 × `GOMAXPROCS=1` clean on `vaultaire_p15`; #781's merge group, built on #782's main, was the first run of the day to pass with the fix under load).
