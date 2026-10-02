# WP-R8-7 — one address for chunk blobs

**Worker session, 2026-10-02. PR #556, branch `wp/R8-7-chunk-address`, from `main` @ #555.** Closes
WP-R10-3c F-1 (P1; P0 the day two customers upload the same large file) and SYNTHESIS table A row
WP-R8-7. No migration.

## The bug, seen first

`chunkContainer` (`_global`) was meant to be shared and tenant-independent, and on the local driver it
was: the local driver keys by container. Prod's primary is iDrive, a fixed-bucket backend: the driver
keys every object `t-<tenant>/<container>/<artifact>` with the tenant taken from the **context of the
call**, and S3 requests carry it. A chunk was therefore stored at `t-<uploader>/_global/_chunks/<hash>`.

Red first — `TestChunkBlobs_HaveOneAddressOnAFixedBucketBackend`, written by WP-R10-3c and kept
skipped; run on `main` @ #555 with the `Skip` removed: tenant B's GET of a deduplicated upload answers
**500**. Live, on the `main` build (two servers, "Live proof" below): tenant B's GET is `InternalError`,
the 33 blobs of a 70 MB object are all under `t-<tenant 1>/`.

Why nothing saw it: every chunk test in `internal/api` ran on the local driver, and built its context
with `tenant.WithTenant` alone — a context production never has.

## What was built

### The one address (`internal/engine/chunk_address.go`, `internal/api/chunk_store.go`)

A chunk blob's address is three things: its index row's `backend_id`, the container
`engine.ChunkContainer` (`_global`), and the tenant **`engine.ChunkAddressTenant` (`_global`) in the
driver context** — `t-_global/_global/_chunks/<hash>` on a fixed-bucket backend, `_global/_chunks/<hash>`
on a container-keyed one (unchanged there). `engine.ChunkContext(ctx)` is the ONE helper that builds the
context; `chunkStore` (`put`, `get`, `exists`, `delete`) is the one caller of it in the product, and
`TestChunkBlobCalls_GoThroughTheChunkStore` reads the source tree and fails on any storage call that
names the chunk container outside `chunk_store.go` / `chunk_move.go` (or any `"_global"` literal
outside the two constants). `cmd/tools/dedup-migrate` (frozen, D-14) uses the helper too.

**Why `_global` and not `default`** (what GC's context resolved to): `default` was the address of every
driver call that forgot its tenant — the address of a bug, and such calls are refused now (below). An id
starting with `_` is nobody's: registration mints `tenant-<hex>`; the S3 front door refuses a credential
that resolves to a reserved id (`engine.IsReservedTenantID`: `""` or a leading `_`, 403 + Error log);
the chunked PUT path refuses it where it is load-bearing (the id is the dedup scope); the erasure sweep
refuses to list under any id that is not `^[A-Za-z0-9][A-Za-z0-9-]*$`. And even a tenant that had the
id could not reach the container: its objects live in `<id>_<bucket>`, never `_global`. Prod: 5 of 5
tenant ids are `tenant-<hex>`.

The engine refuses a write into the chunk container under any other context (`ErrChunkAddress`, wraps
`ErrInvalidInput`: a caller's bug, no breaker charge) — so a future call site that forgets the helper
fails loudly instead of storing a blob where nothing will look.

Reads, existence checks and deletes go to the row's backend only: `engine.GetOn / PutOn / DeleteOn /
ExistsOn` run one named backend through its circuit breaker. A chunk is on its `backend_id` and nowhere
else; `engine.Get`'s candidate walk asked every other registered backend after a miss (six round trips
per missing chunk on prod). The verdict is the backend's own: a miss is a miss, an unregistered name is
`ErrBackendNotRegistered` (never a miss), an open breaker is `ErrAllBackendsUnavailable`.

### Dedup scope: one address for tenant-scoped chunks too

Plaintext chunks are `_global` scope; encrypted chunks are tenant-scoped (R8 invariant 1), keyed
`_chunks/<tenant>/<hash>`. **Decision: they share the one address** (`t-_global/_global/_chunks/<tenant>/<hash>`).
The scope partitions the index, not the prefix. What it means:

- **GC**: one rule for every chunk — released when the last reference goes, deleted at the one address
  after the grace. No scope-dependent addressing anywhere.
- **Erasure**: an erased tenant's scoped chunks are released on the date (the manifest walk) and
  collected by GC, like the shared ones; the sweep never sees them (they are not under the tenant's
  prefix). The alternative — scoped chunks under the tenant, swept at erasure — would have made the
  erasure immediate for them but needed a second address rule, a sweep that deletes inside `_global`
  (the skip is what protects a shared chunk), and the GCI rows of a cancelled erasure would have
  pointed at deleted blobs (a dedup hit after the cancel = an unreadable object) unless the sweep also
  took GC's locked row delete. Not worth it for a configuration prod does not run (no master key).
- The legal sentence is the same for both kinds (below).

### Blobs written before: the read fallback

`chunkStore.get` reads the one address; when — and only when — the backend answers "not there", it looks
where a blob written before this PR would be: under the tenants whose manifests reference the chunk
(earliest reference first, `idx_tcr_hash`), then under the tenants the engine recorded a write of that
key for (`object_locations`, one row per `Put` keyed by the tenant in the context of the write — this
reaches a blob whose uploader has since deleted its object while another tenant still references the
chunk). **An error is never read as "not there"**: an error at the one address is returned as is; an
error at a legacy address does not end the search, but if nothing is found it is the result, not a
miss. Each legacy read is counted (`vaultaire_chunk_legacy_address_reads_total`, a plain counter at 0
from boot) and logged once per chunk (bounded set). Writes and deletes never fall back.

### Dedup GC deletes real blobs (`dedup_gc.go` `sweepOne`)

Under the chunk's advisory lock, BEFORE the row delete, GC now asks the backend for the blob at the one
address: a backend that cannot be asked (not registered, breaker open, an error) keeps the candidate for
the next run (the row is the only record of where the blob is; deleting it first is how a blob becomes
unknowable); a blob that is not at the one address but still at a legacy one keeps the row —
`sweepLegacyPending`, `legacy_pending` in the result and the job note "run the chunk move" (a delete
never touches a legacy address); a blob that is nowhere lets the row go. Then the row delete, then the
blob delete through the chunk store. The lock now guards the SAME key a concurrent first store writes:
before this PR the sweep and the store addressed different keys on a fixed-bucket backend, and the lock
protected nothing.

Proven on the fixed-bucket double: a chunk another tenant's manifest references is never deleted
(`sweepKept`, blob and row intact, the other tenant reads); a chunk whose last reference went is deleted
**from the backend**, not only from the index (0 rows, 0 blobs), and a third upload stores it again;
the sweep racing a new upload of the same bytes, 12 rounds, never leaves an unreadable object.

### The move (`chunk_move.go`, `POST /api/v1/admin/chunk-move`)

`?backend=<name>` (required) `&dry_run=false` (default true) `&tenant=<id>` (repeatable). Admin JWT; one
run at a time (409); on the detached admin context (a proxy timeout does not stop it; the result is also
the `chunk move` log line and an `admin.chunk_move` audit row). One backend per run, two passes:

1. every index row on the backend — is the blob at the one address and does it verify (size + plaintext
   hash after decompression; the ciphertext hash for an encrypted chunk)? If not, a copy that verifies is
   looked for under the tenants that could have written it (the referencing tenants, the recorded
   writers, the `tenant=` ids), copied with `PutOn`, **read back and verified**, and only then is every
   old copy deleted — under the chunk's lock, the row re-read under it;
2. every tenant's `_global` container is listed (`tenants` ∪ `object_locations` writers ∪ `tenant=`): a
   blob whose chunk has a row is moved as above (a tenant pass 1 did not know to ask); a blob with no
   row is an orphan — nothing can reference it, `tenant_chunk_refs` has a foreign key on the row — and
   is deleted; anything else there is counted `unrecognized` and left alone.

Never on a backend where the address does not depend on the tenant (local, s3compat): there the "old
copy" IS the blob. Decided from the driver's own key builder (`engine.KeyAddresser.ObjectKey`), once
per run and again per delete; a driver without it is refused with a note. Idempotent and resumable with
no cursor: done is found done, a run stopped anywhere — between a copy and its delete included — is
finished by the next one. `missing` is a verdict only after both passes. **Done = `at_address == rows`
and `orphans == 0`.**

### Erasure: `chunk_blobs_left` defers (`erasure_sweep.go`)

The sweep still skips the chunk container under a tenant's prefix (the skip is what protects a chunk
another tenant shares) and counts it. What a blob there means now: **written before this PR, not yet
moved**. The tenant is then **deferred** with "run the chunk move": after the row erase nothing would
name that prefix again, and GC deletes at the one address only — the blob would stay for ever. After the
move the count reads 0 and the next run of the erasure finishes (`TestErasureSweep_NeighboursAndTheChunkContainerSurvive`,
`TestChunkMove_LeavesNothingForTheErasureSweepToCount`). Prod: 0 accounts pending deletion.

### A driver call with no tenant is refused (`internal/drivers/tenant_ctx.go`)

A call on idrive, lyve, geyser, r2 or permafrost whose context names no tenant used to resolve to
`"default"`: a read that misses an object that exists, a write nothing finds, a delete that "succeeds"
on a key that was never there (an S3 delete of a missing key is 204 — exactly how GC "deleted" every
chunk). The audit below found nothing legitimate, so it is refused: `drivers.ErrNoTenant` (wraps
`engine.ErrInvalidInput`: no breaker charge, never a miss), an Error line naming backend and operation,
`vaultaire_driver_calls_without_tenant_total{backend}` at 0 for every registered backend from boot, rule
`DriverCallWithoutTenant`. Lyve and Geyser honour a driver-level default only when the constructor was
given one (the probe tools); `cmd/vaultaire/main.go` passed `"vaultaire"` to Geyser and now passes none.
Every driver but quotaless reports `ObjectKey` (`engine.KeyAddresser`).

## The same bug on every other path — the audit

Every caller of `engine.Get/Put/Delete/Exists/GetRange/List` and of a driver method outside the engine,
read for the tenant in its context against the tenant the object was written with:

| Path | Call | Tenant in the context | Same as the writer's? |
|---|---|---|---|
| S3 GET / HEAD-restore / GET range | `s3_engine_adapter.go:405,535`, `s3.go:793` | `r.Context()`: `handleS3Request` sets `common.TenantIDKey` after auth | yes |
| S3 PUT (plain, SSE), stale-blob delete after a chunked PUT | `object_write_shared.go` `placeObject`, `s3_engine_adapter.go:1427` | request | yes |
| S3 DELETE, `DeleteObjects` | `s3_engine_adapter.go:2062`, `s3_batch.go:158` | request | yes |
| ListObjects without a DB (`s3_list.go:445`), adapter list | request | yes |
| CopyObject source + destination, stale destination blob | `s3_copy.go:192,278,573` | request (same tenant: cross-tenant copy is deferred, 5.10.2) | yes |
| CompleteMultipartUpload | `s3_multipart.go:675` → `placeObject` | request | yes |
| RestoreObject | `s3_restore.go:105` | request | yes |
| CDN | `cdn.go:160,186` | `common.WithTenantID(tenantID)` from the slug row (the 2026-07-31 fix) | yes |
| Access-log delivery, inventory reports | `background_put.go:126,151` | `tctx = WithTenantID(tenantID)` (R13-02) | yes |
| Smart demotion copy / flip / cold delete | `smart_demotion.go:400-440` | `tctx` | yes |
| Smart promotion: restore, copy-back, cold delete | `smart_promotion.go:239,350,396-482` | `WithTenantID` | yes |
| `dropDisplacedBlob`, `deleteCopyOn`, `checkLostWrite` | `smart_ledger.go:150,204` | `WithTenantID(tenantID)` | yes |
| Deletion runner walk (`deleteOnBackend`), erasure sweep, pass after the erase | `deletion_runner.go:523,571`, `erasure_sweep.go:423`, `Remove` closures | `tctx`; the walk binds the listed key | yes |
| Dashboard restore / restore status | `dashboard/handlers/bucket_restore.go:86,133` | `context.WithValue(TenantIDKey, sd.TenantID)` | yes |
| Synthetic check | `synthetic_check.go` | real signed requests through the public endpoint | yes (it is a customer) |
| **Dedup GC** (`dedup_gc.go`) | **job context, no tenant → `t-default/`** | **fixed: `engine.ChunkContext`** |
| **Chunk store / fetch** (`storeChunkLocked`, `fetchAndVerifyChunk`) | **the uploader's request context** | **fixed: `engine.ChunkContext`** |
| **`cmd/tools/dedup-migrate`** (frozen, D-14) | `eng.Get/Put/Delete` with no tenant → `t-default/` | fixed: `WithTenantID(c.tenantID)` for the object, `ChunkContext` for the chunk |
| `engine.Replicator`, `DisasterRecovery`, … | driver calls with the caller's context | unreachable from the product (WP-R6-4) |
| Health probes | `HeadBucket` / `CustomerDetails` | no key | n/a |

Found: three paths wrote or read without the tenant the object was written with — the two chunk paths
and the frozen tool. Everything else carries the right tenant. Nothing legitimate calls a fixed-bucket
driver without one, hence the refusal rather than a log line alone.

## Adversarial pass — on my own code

1. **Attacker: a tenant that wants another tenant's chunk.** A tenant reaches a chunk through its own
   manifest only (`GetObjectChunks` by `tenant_id`), unchanged; the one address is unreachable through the
   object path (containers are `<id>_<bucket>`); a reserved credential is refused at the front door;
   the sweep refuses the id. A tenant that wants its own chunk kept after erasure: its manifest rows are
   released by the runner; the blob is at the one address, not under its prefix, so the sweep's skip
   does not keep it; GC deletes it after the grace unless another manifest names it — in which case it
   is that tenant's data too.
2. **Flaky backend.** The fallback never reads an error as "not there" (two tests, both red without the
   check). GC keeps the candidate when `Exists` errors or the backend is not registered. The move
   leaves everything for the next run on an unreadable old copy, an unreadable one address, an `Exists`
   that fails after the copy (copied, verified, old copy kept), and refuses to run while the backend's
   breaker is open — and five hard failures DO open it, as they should: my first flaky-backend test ran
   three failure shapes against one fixture and tripped the breaker halfway, which is how that rule got
   its own test.
3. **Concurrent writer.** Two first-stores of one chunk: `storeChunkLocked` is unchanged (R8-02, the
   loser references the winner's row) and both now address the same key. A GC sweep racing a new
   reference: 12 rounds of sweep-vs-PUT on the one address, every upload that answered 200 reads;
   the lock means something now (before, the two addressed different keys). The mover takes the same
   lock and re-reads the row under it; a row GC swept meanwhile is not resurrected
   (`TestChunkMove_ARowThatDedupGCSweptMeanwhileIsNotResurrected`).
4. **A deploy between the copy and the delete of the move.** The run stops with both copies in place,
   reads work (the one address answers first), the next run deletes the old copies and copies nothing
   (`TestChunkMove_StoppedBetweenTheCopyAndTheDelete_IsFinishedByTheNextRun`). Found by the mutation
   pass: nothing proved the mover keeps the old copy when the copy it just wrote does not read back —
   `TestChunkMove_ACopyThatDoesNotReadBackKeepsTheOldCopy` (a backend whose `Put` drops the bytes) was
   added and is red without the read-back.
5. **The same logic on the other entry point.** The erasure runner's walk releases a chunked object's
   manifest and never touches `_global` (unchanged); its sweep now defers on a legacy blob. The admin
   trigger of GC is the same `RunOnce`. `DeleteObjects` releases manifests like single DELETE. The
   frozen tool was the other writer of chunk blobs — fixed.
6. **"Always" / "never" against the path that returns first.** "A chunk has one address": true for
   every blob written from this deploy on; blobs written before are at a legacy address until the move
   (hence the fallback, the counter, GC's `legacy_pending`, the sweep's deferral). "GC deletes real
   blobs": at the one address; a legacy copy of a chunk whose row GC dropped before this PR is an
   orphan the move's listing pass deletes. "No tenant-less call": the double and the real drivers
   refuse; `local` and `s3compat` key by container and need none.
7. **Shared test database.** Every new test runs under a backend name of its own (its rows are the only
   rows on that backend), names the tenants the move may list (`tenantsOverride`), sweeps exactly its
   own chunks (never `RunOnce`), and cleans its rows — including the ones a DELETE left at ref 0.
8. **Double counting in my own report** (found by the dry-run test): the listing pass re-counted chunks
   the row pass had already failed on. Pass 1 now records every (tenant, key) it examined; pass 2 skips
   them, and `missing` is decided only after both passes.

Mutations run and seen red: error-at-one-address read as a miss; legacy hard error dropped; GC's
legacy guard removed; the reserved-id guard removed; the sweep's deferral removed; the engine's write
guard removed; a stray chunk call added (the structural test); the mover's verification removed; the
mover's read-back removed. The original red test, on `main` @ #555.

## Tests

- `internal/api/fixed_bucket_test.go` — `fixedBucketDriver` moved out of the sweep test and made the
  **primary of every chunk test** (`setupChunkingFixture`): prod's key shape, refusing tenant-less calls
  like the real drivers, `ObjectKey`, fault switches (`failGet`, `failGetUnder`, `failExists`,
  `failDelete`, `dropPut`). `s3Ctx` builds the request context the way `s3.go` does (`common.TenantIDKey`
  + `tenant.WithTenant`); 245 `tenant.WithTenant` call sites in the package's tests now use it.
- `internal/api/chunk_address_test.go` — the red test extended (both read; both delete; nothing inside
  the grace; GC past it deletes from the backend; a third upload stores again); never deletes a chunk
  another tenant references; sweep vs new reference ×12; a backend that cannot be asked keeps the row;
  the fallback with its counter and once-per-chunk log; an uploader that deleted its object; both
  error-is-not-a-miss cases; GC keeps the row of a chunk still at a legacy address and drops one that is
  nowhere; writes only at the one address; a tenant-less call refused with no breaker charge; the
  reserved id at the front door; the legal sentence pinned to the 7-day grace.
- `internal/api/chunk_move_test.go` — 16 tests (listed in `internal/api/CLAUDE.md`).
- `internal/api/chunk_store_structure_test.go` — the source scan.
- `internal/engine/chunk_address_test.go` — the context, the write guard, the `*On` verdicts, the
  breaker.
- `internal/drivers/tenant_ctx_test.go` — idrive/lyve/geyser/r2 refuse every op before a request
  leaves (a fake that counts requests), permafrost against the Graph stub, the configured default of
  tools, `ObjectKey` per driver.
- `internal/api/erasure_sweep_test.go` — the neighbours test: deferred with the legacy blob, erased
  after the move.
- `go test -race ./...` (all 34 packages), `make lint` (0 issues), `make gosec` (0 issues).

## Live proof

Two servers on private databases, aws-cli. **B** = the "iDrive account": a Vaultaire on local disk with
a bucket `vaultaire`. **A** = the server under test, `STORAGE_MODE=idrive` with `IDRIVE_*` pointed at B
(the real `IDriveDriver` over HTTP with SigV4). Two tenants on A, a 70 MB object (`s3api put-object`),
uploaded by both (the second is a dedup hit: 2 chunked rows, 33 index rows, 33 blobs).

| | `main` @ #555 | this branch (same databases, blobs where `main` wrote them) |
|---|---|---|
| blobs on B | 33 under `t-<tenant 1>/_global/` | — |
| tenant 1 GET | 200, identical | 200 |
| **tenant 2 GET** | **`InternalError` (500)** | **200, identical** (`vaultaire_chunk_legacy_address_reads_total` 33, 33 "legacy address" log lines — one per chunk) |
| `chunk-move` dry run | — | `rows 33, moved 33, legacy_deleted 33, missing 0, failed 0, orphans 0` |
| `chunk-move dry_run=false` | — | the same numbers; blobs on B: 33 under `t-_global/`, 0 elsewhere |
| second run | — | `at_address 33, moved 0, legacy_deleted 0` |
| both GET after | — | 200 identical, the legacy counter unchanged at 33 |
| both DELETE, `dedup-gc` inside the grace | — | 33 rows marked; `deleted 0`, 33 blobs |
| rows backdated 8 days, `dedup-gc` | — | **`deleted 33, bytes_reclaimed 70000000`; 0 rows, 0 blobs on B** |
| a third upload | — | 33 blobs under `t-_global/` again |
| `vaultaire_driver_calls_without_tenant_total` | — | 0 on both backends |

(70 MB → 33 chunks: the ≈2 MiB real average of R8-11, WP-R8-4.)

## Prod (read-only, 2026-10-02)

- `global_content_index`: **127 rows**, all `backend_id = idrive`, scope `_global`, plaintext,
  uncompressed, unmarked, keys `_chunks/<hash>`; 127 refs from ONE tenant (`tenant-f4294518dc0a7a8f`,
  "Bench2", the 256 MB `netbench` object of 2026-07-31); no hash shared by two tenants;
  `object_locations` holds that tenant's chunk writes (504 on idrive). `dedup_gc` last ran `ok` on
  2026-10-02 04:30, 0 rows. 0 accounts pending deletion. `chunking` flag: no `feature_flags` row (still
  ON by code default — [YOU] below).
- **The 127 blobs are not on prod's iDrive at all.** One signed HEAD through the server's own driver
  config (the uploader's prefix, the one address, `t-default/`): all three "not found"; a listing of
  `t-<tenant>/_global/`: 0 objects; the whole bucket `vaultaire`: **30 objects, 160 KB, one top-level
  prefix** (`t-tenant-cb05d8eeb2093573/`, the FairForge Demo tenant's `letshow` container), no `_global`
  anywhere. The explanation is the iDrive account change of 2026-09-21 (new reseller account): every
  head row on `idrive` older than that date — 1,230 of Bench2's, 140 of BenchTest's, the chunked
  object among them — names a bucket in the dead account. Only the demo tenant's 25 post-change rows
  (and 5 of its older ones) have bytes there.
- Consequently there is **nothing to move on prod**. The move's dry run will report `rows 127,
  missing 127` (no copy anywhere), the read fallback will never fire for these rows, and the `netbench`
  object has been unreadable since the account change whatever this PR does. The way out is the
  object's deletion (by its owner, or with the bench tenant's erasure): its 127 rows then go to ref 0
  and GC drops them a week later under the "nowhere" rule (`sweepDeleted` with a miss on the delete).
- **Found on the way, not this WP's:** 1,370 of prod's 2,037 `idrive` head rows (two bench tenants,
  pre-2026-09-21) point at the dead account — the objects 404 today. WP-R7-5 (routing-truth
  reconciliation, queue item 5) is where a sampled `Exists` job would have shown it.

## Legal copy

The six erasure sentences (`privacy.html:41`, `terms.html:73`, `dpa.html:106`, `baa.html:94`,
`gdpr.html:27`, `:33`) now end with the sentence that is true: *"Large objects are stored in
deduplicated blocks: the blocks of your objects are released on that date and deleted by the storage
collector once its seven-day safety period has passed, except a block that is byte-for-byte identical to
one another customer stores, which remains as that customer's data."* (the DPA says Controller.)
`TestErasureCopy_SaysWhatDedupGCDoes` pins the phrase to the runner's 7-day grace and daily schedule.
True by construction after this PR: GC deletes at the one address, where every block written from this
deploy on is, and a legacy block under a tenant's prefix defers the erasure until it is moved.

## Decisions taken

- The reserved tenant id `_global` is the address (not `default`); app-level guards, no DB constraint.
- Tenant-scoped (encrypted) chunks share the one address.
- Reads to the row's backend only (`engine.GetOn`), not the engine's candidate walk.
- GC asks the backend before the row delete; a legacy-only blob keeps the row.
- An erasure that finds a legacy chunk blob under the tenant's prefix is deferred, not reported erased.
- Tenant-less fixed-bucket driver calls are refused, not only counted.
- The move is an admin endpoint on the server's own drivers (dry run by default, synchronous on the
  detached context), not a CLI with a second copy of the driver wiring.
- Every chunk test runs on the fixed-bucket double; `s3Ctx` everywhere in the package's tests.

## Not done / what I could not prove

- The real iDrive, Lyve, Geyser and R2 endpoints with the new context: the iDrive driver was exercised
  live against a second Vaultaire speaking S3 (SigV4, HEAD, GET, PUT, DELETE, ListObjectsV2 with a
  prefix); the other three share its helpers and were tested against the request-counting fake.
  Permafrost against Graph: the stub only.
- The move on prod's real rows (nothing to move — above); its numbers come from the local proof.
- The fallback's `object_locations` query is a sequential scan (no index on `(bucket, object_key)`);
  it runs only after the referencing tenants missed, and only until the move. Acceptable for the
  transition; not for a permanent path.
- The write-during-erasure residual of WP-R10-3c is unchanged.
- An orphan chunk blob under an erased tenant's prefix is reached only by naming the tenant
  (`tenant=`): the move cannot list prefixes the database no longer knows.

## [YOU]

1. **Deploy, then run the move once per fixed-bucket backend** (admin JWT; idrive is the only backend with
   chunk rows on prod):
   `curl -s -X POST -H "Authorization: Bearer <admin JWT>" "https://stored.ge/api/v1/admin/chunk-move?backend=idrive" | jq .`
   — a dry run. Expected on today's prod: `rows 127, at_address 0, moved 0, missing 127, orphans 0` (the
   blobs are in the dead iDrive account). Then `…&dry_run=false` moves whatever a dry run says it
   would; done when `at_address == rows && orphans == 0`.
2. **Turn `chunking` back on** (`/admin/flags` → `chunking`; today there is no row, the code default is
   ON) once the move has run — nothing written from this deploy on needs it, so "after the deploy" is
   enough on prod; the order matters only on a box with legacy blobs that a dedup hit could reference
   before the move (uploader deletes → refs gone → `object_locations` still finds it; the move's
   listing pass is the last resort).
3. **When the read fallback can go**: `vaultaire_chunk_legacy_address_reads_total` at 0 for a week after
   the move on every backend (`curl -s https://stored.ge/metrics | grep chunk_legacy`; rule
   `ChunkLegacyAddressReads`, info) and a dry run reporting `at_address == rows`. Then a follow-up
   deletes `getLegacy` / `legacyTenants` / `legacyCopyExists` / `engine.LegacyChunkContext` / GC's
   `sweepLegacyPending` branch / the sweep's deferral (it keeps the skip and the count) / the `tenant=`
   parameter and the listing pass of the move.
4. **The dead `netbench` object** (Bench2, 256 MB, 127 rows): delete it as that tenant (`aws s3 rm`) or
   erase the bench tenants; GC drops the rows a week later. The same tenants' 1,370 other `idrive`
   rows are WP-R7-5's.
5. Install `deploy/monitoring/vaultaire-backends.yml` again (two new rules: `DriverCallWithoutTenant`,
   `ChunkLegacyAddressReads`).

## Post-merge review (plan driver, 2026-10-02)

Read in full against the note: `engine/chunk_address.go`, `api/chunk_store.go`, `api/dedup_gc.go`,
`api/chunk_move.go`, `drivers/tenant_ctx.go` + the five drivers, the sweep's deferral, the front-door
guard. Checked and held: a miss at the one address never charges the breaker (`isBackendFailure`
excludes `NotFoundError` and the SDK 404s — every legacy read starts with one); `object_locations`
has no constraint on `tenant_id`, so the `_global` writer row is fine; the move's listing pass skips
reserved ids, so `t-_global/_global/` is never listed and an in-flight first store (blob put before
the row insert, under the xact lock) is never an "orphan"; `HealthCheck` and `EnsureBucket` carry no
key, so the refusal cannot mark a backend down; the access-log and inventory writers carry a tenant.

**PM-1 (P1, fixed #558): region pinning did not reach the chunked path.** WP-R7-1 enforces a
bucket's region on the plain PUT, multipart complete and CopyObject (R3-08) — "never fall through to
the primary: the bucket promised a region" — but the chunked-path gate checked threshold, versioning,
tier, encryption and the flag only. An object above the threshold in an `eu-west-1` bucket had its
chunks at the one address on the primary in Dallas; with no driver for the region the plain path
refused the PUT while the chunked path accepted it (the red run put the chunk at
`t-_global/_global/…` on the primary from the pinned bucket). Fix: `chunkingDisabledByRegion` joins
the gate and `willChunkEncrypt` (whole objects through `placeObject`, or the 503); a chunked source
copied INTO a pinned bucket is 503 without the region driver and 501 with it (a manifest copy would
leave the chunks on the primary). `TestHandlePut_RegionPinnedBucketIsNeverChunked`,
`TestChunkedCopy_RegionPinnedDestination`. Prod impact today: none (one `eu-west-1` bucket, 0
objects, no regional keys). Same shape as R4-22 / R7-27: a rule enforced on one entry point and not
the other.

Noted, not changed: `bytes_reclaimed` counts a candidate whose blob was "nowhere" (the 127 dead rows
will report 256 MB reclaimed when GC drops them); `NewChunkerFromConfig` ignores a recorded
`Chunker.AverageBits` (WP-R8-4; only matters the day the average changes).
