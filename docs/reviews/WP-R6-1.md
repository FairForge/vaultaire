# WP-R6-1: location-authoritative reads and deletes

Stage 1a Prompt 5, 2026-10-13. Closes R6-07, R6-09, R6-14, R6-15, R6-22,
R6-24, R8-19, the WP-R9-5 half (the `object_locations` touch), the two
#677 bypasses, `wasabi` target-only and PM-6 of WP-R2-1. Spec:
`SYNTHESIS.md` WP-R6-1 / the R6-22 residual, `R6-engine.md`,
`WP-R2-1.md` PM-6, `WP-R8-1.md` (the bypasses).

PRs: **#713** (Part 0: the WP-R8-5 post-merge probe, R85-1, R85-9, R85-10;
`docs/reviews/WP-R8-5.md` § Post-merge), **#714** (Part A: candidates,
copies, PM-6, the tenant guard; migration **090**), **#715** (Part B: one
truth, the bypasses, `wasabi`). Next migration: **091**.

## The rule

The row that describes an object — `object_head_cache.backend_name`, a
version row's, the Smart ledger's — is the one authority for where its
bytes are. A read asks that backend and no other. A miss there is a miss
(404). A backend that cannot be asked — failure, open breaker, no driver by
that name — is 503 + Retry-After. Never a guess on another backend. A
delete removes every copy the tables know of, and fails while one may
remain. An engine call whose context names no tenant is a bug, and it
fails loudly.

The caller names the backend on the context of the call
(`engine.WithRecordedBackend`; `WithKnownCopies` for a delete,
`internal/engine/recorded_backend.go`). The engine remembers nothing:
`objectBackends`, `HintBackend` and the `LocationStore` are gone.

## Prerequisite of record: routing truth (read on prod first, 2026-10-13)

- `routing_truth` last `ok` 05:47 UTC, `idrive`: 501 rows, 10 sampled, 10
  `error` (the dead key; no `missing`, no `unknown_backend`).
- `object_head_cache`: **501 rows, all on `idrive`** (a registered
  driver), one tenant (f429, 2,048,010 B); `object_versions` 0;
  `smart_demotions` 0; `retired_blobs` 4.
- `vaultaire_routing_unknown_backend_rows`: no series, i.e. 0 rows on an
  unregistered name.

**A2 decision: the fan-out fallback goes.** A row on an unregistered name
is 503 with an Error line (the engine's `ErrBackendNotRegistered`) plus
`vaultaire_routing_unknown_backend_reads_total{op,backend}` (WP-R7-5's,
unchanged). Nothing for the owner to run first.

## Before → after

Measured on `origin/main` `9f3d0c1` with the same tests (a throwaway file
in a separate worktree at `origin/main`): 18 registered backends (prod's
count), fixed-bucket drivers at the handlers.

| # | Case | Before | After |
|---|---|---|---|
| A1 | GET of an object its recorded backend lost | **18 backend calls**, 404 | **1**, 404 |
| A1 | ... with a stale copy on another backend | **200 with the stale copy** (206 for a Range) | 404 |
| A1 | recorded backend's breaker open | **200 with another backend's stale copy** | 503, 0 calls |
| A2 | row on an unregistered backend | GET 200 (the primary's copy); DELETE **204, row dropped** | 503, row kept |
| A3 | DELETE, a second known copy (Smart ledger hot copy; a `retired_blobs` copy at the same address) | 204, **both copies left** | 204, every copy removed by the DELETE |
| A3 | ... the second copy's backend failing | **204, head row gone**, copy left | 503, row and bill kept; the retry removes both |
| A4 | DELETE racing a PUT (PM-6) | the **PUT's head row deleted**, its blob leaked | 204; the PUT's row, bytes, bill and retention intact |
| A5 | engine call with no tenant | stored as tenant **`"default"`** | `engine.ErrNoTenant`, 0 driver calls, counter +1 |
| B1 | `object_locations` | written per Put in a goroutine (prod: **7,693 rows — 7,684 chunk rows**, 9 object rows), read on a cold route, `last_accessed` touched in a goroutine | not read, written or touched by the engine |
| B2 | `objectBackends` | unbounded `sync.Map`, one entry per object since boot | gone: no per-object state |
| B3a | generated object into a flagged tenant's `sync`-tier bucket | the **primary** | `sync` (the PUT's gate) |
| B3b | region-pinned write, 6 failing puts | the driver asked 6 times, **breaker closed**, no 503; a broken body a backend failure; no tenant accepted | breaker opens at 5, then 503 without a call; broken body `ErrCallerAborted`; `ErrNoTenant` |
| B4 | `wasabi` | a write fall-over destination and a read fan-out candidate | neither, unless target / primary / recorded |

## What was built

**Part A (#714)**
- **A1** `CoreEngine.Get`/`GetRange` read from `preferredBackend`: the
  context's recorded backend, else the primary. `readCandidates` returns
  `[target]` or `ErrAllBackendsUnavailable` wrapping
  `ErrBackendNotRegistered`. Where each caller's candidate comes from:

  | Caller | Candidate |
  |---|---|
  | S3 GET: whole, Range, `parallel_get`, sealed (`openSealedGet`) | the head row's `backend_name`; a version row's for `?versionId` |
  | Smart promotion | `OnRead`'s answer (hot or cold), recorded like the head row's |
  | vault parity fallback's data ranges | the head row's, on `Open`'s context (the shards come from the leg drivers, no expected size) |
  | CopyObject source, `/cdn` | the head row's |
  | the parity job's source read | the candidate's `backend_name` (now selected) |
  | chunked objects | not this path: each chunk's GCI `backend_id` through `GetOn` (unchanged) |
  | the deletion runner, the export purge | the head row's (or the ledger's hot copy) |
  | key re-encrypt, routing truth, Smart demotion/promotion, the collector, the erasure sweep | the driver of the recorded backend directly (unchanged) |

- **A3** `Delete` attempts the recorded backend plus `KnownCopies`. The
  API's `knownBlobCopies` (`object_delete_shared.go`, one query) supplies
  an unsettled ledger row's hot and cold backends at the same address,
  and a `retired_blobs` row's backend at the same address. Migration
  **090** indexes `retired_blobs (container, storage_key)`. A miss is fine;
  any failure is the delete's. The Smart ledger's `settle` still runs after
  the row (it closes the ledger), and the daily reclaim remains the
  backstop.
- **A4** `deleteHeadRowIfAt`: `DELETE ... AND COALESCE(storage_key, '') =
  $read`. When a write committed between the blob delete and the row
  delete, the DELETE is a no-op ordered **before** the write. It answers
  204; the write's row, bytes, retention (the lock row is not deleted) and
  parity stay. A DELETE that read no row deletes none. An Info line marks
  it. The deletion runner and the export purge keep the unconditional
  delete; the erasure's own walk follows.
- **A5** `requireTenant` in `Put`, `Get`, `GetRange`, `Delete` and the `*On`
  calls: `ErrNoTenant` (wraps `ErrInvalidInput`), an Error line, and
  `vaultaire_engine_no_tenant_refusals_total{op}`. `common.GetTenantID`
  answers `""`, never `"default"`. `TestEngineCalls_CarryTheTenant` lists
  every engine storage call site in `internal/api` with where its tenant
  comes from; a new one fails until it is listed. `api`'s `TestMain`
  fails the package on any refusal a test did not declare
  (`expectNoTenantRefusals`). The probes and the chunk container are no
  exception in practice: they name their reserved tenants (`_probe`,
  `_global`).

**Part B (#715)**
- **B1** `routing.go` deleted. The table stays (additive) with its old
  rows; its readers are the chunk store's legacy-uploader fallback, the
  chunk move and the erasure sweep's bucket sources. The dashboard's carbon
  badge and admin cost view read the tables of record (whole objects by
  `backend_name`, chunks by `backend_id`).
- **B2** `objectBackends` and `HintBackend` deleted. An LRU would have been
  a stale second copy of what the per-request value already carries, and
  prod's map held at least every chunk stored since the last deploy.
- **B3a** `generatedObjectWriter.allowSync` = `syncPlacementGate(s.flags)`
  for the report writer. The GDPR export's `_exports` is never a sync
  bucket.
- **B3b** `placeObject`'s region branch uses `engine.PutOn`, which now tags
  a source failure `ErrCallerAborted` (as `Put` does) and records the
  backend used.
- **B4** `dormantBackends` (`wasabi`): out of `buildWriteCandidateList`
  unless target or primary, and not in `CheckPrimaryEligible`'s refusal.
  With `STORAGE_MODE=wasabi` every write is wasabi's, as before.

## Tests

Red first, measured against `origin/main` (the table above):
- engine: `TestGet_RecordedBackendMiss_IsOneCallNot18`,
  `TestGet_NoRecordedBackend_AsksThePrimaryOnly`,
  `TestGet_RecordedBackendBreakerOpen_503NoFanOut`,
  `TestGet_RecordedBackendFails_503NoFanOut`,
  `TestGet_StaleCopyOnAnotherBackend_NeverServed`,
  `TestGet_UnregisteredRecordedBackend_503NoFallback`,
  `TestDelete_RemovesEveryKnownCopy`, `TestDelete_OneCopyFails_IsAFailure`,
  `TestEngine_NoTenantContext_Refused`, `TestObjectBackends_Bounded`,
  `TestWasabi_NotACandidateUnlessRecorded`.
- handlers: `TestGet_RecordedBackendMiss_IsOneCallNot18`,
  `TestGet_StaleCopyOnAnotherBackend_NeverServedAtTheHandler`,
  `TestGet_RecordedBackendBreakerOpen_503NoFanOut`,
  `TestGet_UnregisteredRecordedBackend_503`, `TestDelete_RemovesEveryKnownCopy`,
  `TestDelete_OneCopyFails_IsAFailure`, `TestDelete_RacingPut_KeepsThePutsBlob`,
  `TestEngineCalls_CarryTheTenant`, `TestReadPath_DoesNotTouchObjectLocations`,
  `TestPlaceObject_RegionDriver_BreakerAndStrictRules`,
  `TestGeneratedObject_SyncTierBucket_FollowsTheSyncGate`.

Assertions whose meaning changed (also listed in each PR body):
- The engine's read fall-over is gone: `…FailoverOnError` became
  `…RecordedBackendDown_NoFailover`, and tape no longer keeps a read
  fall-over.
- An unregistered name is 503 everywhere.
- A demoted object's DELETE whose hot copy fails is 503 (it was 204 plus
  a reclaim).
- Tenant-less calls are refused by the engine before the driver.
- Every `HintBackend` became `WithRecordedBackend`.
- `routing_test.go` was removed with the store.
- Many fixtures now name their tenant. Two dedup GC checks had passed by
  accident (a tenant-less `Get` of a chunk); they now use `ChunkContext`.

## Live proof (local)

Lab build (`-tags lab`: `cmd/vaultaire/lab_on.go`;
`VAULTAIRE_LAB_LOCAL_DRIVERS=lab2=<dir>` registers a second local-backed
driver, `VAULTAIRE_LAB_LOG_CALLS=1` logs every backend call),
`STORAGE_MODE=local`, a private DB through 090, aws-cli 2:

| Step | Result |
|---|---|
| PUT into a missing bucket | `NoSuchBucket` |
| the recorded blob moved to `lab2` (a stale copy on another backend), GET | `NoSuchKey`; **1 backend call** (`local get`); the stale copy not served |
| DELETE of an object with a `retired_blobs` row naming `lab2` at its address | 204; one delete on `local`, one on `lab2`; both files gone |
| keyless `x-amz-server-side-encryption: AES256` PUT | 503 + **one Warn line** |
| `/metrics` | `vaultaire_engine_no_tenant_refusals_total` 0 for every op; `vaultaire_errors_total` 0 |

The one Error line in the log is the boot shared-store check flagging the
two lab local drivers (a lab artifact). Server stopped, DB dropped.

## Prod proof

2026-10-13. Prod has no master key (0 sealed rows). Deploys: #713 at 21:25
UTC (`e3f95e0`), #714 at ~21:53 (`bfe9d55`, migration 090's index
present), #715 at 22:11 (`7cc4bea`). Bench tenant
`tenant-14a623b16b3f7012`, aws-cli 2.35.21 on the SLC box (the bench keys
written on the box from stdin, removed after), via stored.ge (Cloudflare).

| Step | Result |
|---|---|
| after #713: `put-object` and `create-multipart-upload` into a missing bucket | **`NoSuchBucket`** both (was: stored, the 10-13 proof); 0 rows, 0 uploads, 0 buckets after |
| after #715: `put-object` into a missing bucket | `NoSuchBucket` |
| `create-bucket --region us-east-1` | created |
| 1 GiB random, `aws s3 cp` up ×2 | 10.6 s (**102 MB/s**), then 6.3 s (171 MB/s: the same bytes, every chunk deduplicated) |
| 1 GiB down ×2 | 7.3 s (**146 MB/s**), 6.5 s (**166 MB/s**); sha256 equal both times |
| single-stream `curl` of a presigned GET of the 1 GiB object | 55.0 MB/s, then 106.6 MB/s |
| 20 MiB `put-object` / GET / DELETE | 4.0 s / 3.6 s / 0.7 s wall from the client, equal; server-side 2.78 s / 2.94 s / 0.11 s |
| GET of the deleted key | `NoSuchKey`, **53 ms server-side**; 0 `backend failed, trying next` lines; `vaultaire_engine_fallover_total` unchanged (0) |
| `/metrics` after the proof | `vaultaire_engine_no_tenant_refusals_total` **0** for every op, `vaultaire_routing_unknown_backend_reads_total` 0, `vaultaire_driver_calls_without_tenant_total` 0, `vaultaire_errors_total` 0 |
| journal 22:12:30–22:14:30 | no Error line; one Warn: the AWS SDK's "Skipped validation of multipart checksum" (pre-existing) |

**Throughput, next to #704 and #710:**
- The 1 GiB aws-cli numbers (102 up, 146–166 down) sit between #710's ~90
  MB/s aggregate and #704's 161–250. The per-stream chunked path is
  unchanged by this WP: chunks are still read with `GetOn` from their
  index rows.
- The single curl stream (55 then 107 MB/s) is below #710's 119–154. This
  WP touches no part of that path either; the first, cold stream is the
  slow one.
- No earlier 20 MiB timing exists to compare with. The server-side 2.8–2.9 s
  is one whole-object stream to the Lyve primary, whose probe latency was
  ~1 s all day.

**One backend call on a miss, on prod:** prod exports no per-call counter.
Before WP-R6-1, a miss on Lyve walked on to the 11 object-dead iDrive
backends, each answering AccessDenied (a `trying next` Warn and a breaker
charge apiece). After it there are none, and the miss took 53 ms. No S3
GET miss happened on prod earlier on 10-13 to compare with: the day's five
`trying next` lines are dedup GC's chunk `ExistsOn` at 05:24. The exact
count is the tests' and the local live proof's (1 call).

**`/health` through the three switches:** `degraded`, the same as before:
the 11 iDrive probes, plus the dormant `wasabi` probe when it flaps
(R85-3). The Lyve primary's breaker stayed closed.

**CI on `main`:** #713's merge commit `e3f95e0` failed on
`TestVaultParity_ReconcileRunsWhenProtectUsesTheRun` (`context deadline
exceeded`), the known timing flake of 2026-10-13. Its `--failed` re-run was
cancelled when `bfe9d55`'s run superseded it, and `bfe9d55` (which contains
#713) and `7cc4bea` are green.

**Cleanup:** the object and the bucket deleted. The bench tenant has 0
heads, 0 uploads, 0 buckets, usage 0, 0 chunk refs. The box's
`~/r61proof` (credentials, files) is removed.

## Decisions (to report; not built here)

1. **When `object_locations` can be dropped.** Not before three things:
   (a) the chunk move has run on every backend that ever held a legacy
   chunk, since its legacy fallback is the last reader that needs the
   old rows to find a chunk's uploader; (b) the erasure sweep's bucket
   sources stop listing the table (an erased tenant's old containers are
   then found by the walk alone); (c) #715 has run on prod for 14 days
   (the rollback window). Then one migration drops the table (and
   `tiering_policies` / `tenant_cost_daily`, D-12) and nothing reads it.
2. **`idrive` rows under A1: 503.** The bytes exist and the backend cannot
   be asked; a 404 would tell a sync client to forget them. A GET asks
   iDrive alone (one AccessDenied), then 503 + Retry-After. Before, it
   walked the other 17 too, charging each object-dead region. The owner's
   iDrive repair (a working key) restores the reads with no data change. If
   the account is abandoned instead, the 501 rows (tenant f429, 2 MB) must
   be copied to Lyve and their `backend_name` flipped, or deleted with
   notice. `cmd/tools/routing-truth` is the plan tool.
3. **Order: agreed** — WP-R10-8 (per-part reservations) then version-aware
   manifests (WP-R3-1 decision 2, L). `parallel_get` by segment for sealed
   objects (WP-R8-5 decision 2, S) goes **right after the F-14 ceremony**:
   no sealed object exists before it, so its value starts that day. It
   fits between the two if the ceremony runs first.
4. **Carried unchanged:**
   - the 11 iDrive criticals;
   - tenant f429's 2 MB on iDrive;
   - the Sync.com sub-processor sentence;
   - gey48;
   - the D-12 drop;
   - PM-4 of WP-R2-1 (`whole_skipped` gauge);
   - PM-8 (`LIKE` escape in the management objects list);
   - MFA-Delete on batch (R4-17);
   - R85-2 (HEAD without `?versionId` omits `x-amz-version-id`);
   - R85-3 (`wasabi` probe flapping, the owner's Wasabi account);
   - the F-14 ceremony (owner).

## Not done / residuals

| # | Sev | Item | Where it goes |
|---|---|---|---|
| R61-1 | P3 | `keySuggestion` proposes the very key asked for ("Did you mean 'k.bin'?") when the row exists but its backend misses (seen in the live proof) | WP-R14 copy hygiene |
| R61-2 | note | `object_locations` keeps 7,693 dead rows until its drop (decision 1) | the drop migration |
| R61-3 | note | the known-copies lookup costs one query per deleted key (a 1,000-key DeleteObjects: 1,000) | if batches get slow: one query per batch |
| R61-4 | note | a superseded DELETE (PM-6) is logged at Info, with no counter | add one if it is ever seen on prod |

## The two questions

- **Can any read be served from a backend the object's row does not
  name?** No. Every S3/CDN/copy/parity read names the row's backend, and
  the engine asks it alone. The exceptions are documented addressing, not
  guesses:
  - chunks are read from their content-index row's backend;
  - a Smart-demoted object is read from the backend `OnRead` returns,
    which is the head row's cold backend or its flip back to hot;
  - a vault object whose backend failed is rebuilt from its parity row's
    legs;
  - a row with no backend on record (none on prod) is read from the
    primary.

  There is no A2 fallback: prod had 0 rows on an unregistered name.
- **Can any engine call run without a tenant?** No. The engine refuses it
  (`ErrNoTenant`; the counter has stayed 0 through the integration suite
  and on prod). The chunk container and the probes name their reserved
  tenants.
