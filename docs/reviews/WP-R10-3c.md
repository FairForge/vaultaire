# WP-R10-3c — an account erasure reaches every byte

**Worker session, 2026-10-02. PR #554, branch `wp/R10-3c-erasure-sweep`, from `main` @ #553.** Closes
WP-R10-3 post-merge finding PM-3 (P1, erasure claim) and the erasure half of R13-10. No
migration. **Opens WP-R8-7** (below) — found on the way, proven, not built here.

## The gap, reproduced first

The runner walked `object_head_cache` and deleted each object on its recorded backend. Bytes
with no head row were never reached, and `EraseRows` then removed the last rows that named them.

Red first, in the runner's fixture (`TestErasureSweep_BlobsWithNoHeadRowAreErased`, run on
`main` @ #552 before any code): a versioning-enabled bucket, a key with a live version row and a
delete marker on top (no head row), and a blob whose version row went with `DELETE ?versionId` →
outcome `erased`, both files still on disk.

Live, on the `main` build (two servers, see "Live proof"): `objects_deleted=1`, `job_runs ok`,
the user row gone — and `one.txt` still on the local backend, `two.txt` still behind the iDrive
driver.

## What was built

### The sweep (`internal/api/erasure_sweep.go`)

Stage **b2** of `eraseTenant`: after the head-row walk and the multipart abort found nothing left,
before `account.EraseRows`. It does not ask the tables where the bytes are; it asks **every
registered backend** what it holds for the tenant and deletes it.

| Driver can… | How it is swept |
|---|---|
| list a tenant (`engine.TenantWalker`) | one walk of the tenant's own prefix — every container, including ones no table remembers; each object is removed through `TenantObject.Remove`, a closure bound to **the key the listing returned** |
| only `List` a container | bucket by bucket (`buckets` ∪ the distinct bucket names of nine more tables), `List(<tenant>_<bucket>)` then `deleteCopyOn` for each name — through the driver, never `engine.Delete` (it falls back to the primary) |

Rules:

- **Failure is not partial success.** A listing or a delete that fails (a miss is not a failure),
  a backend whose circuit breaker is `open`, a deadline, a shutdown → the tenant is **deferred**
  (the runner's existing outcome): not reported erased, no `account.erased` row, the run is an
  `error`, the hourly check retries. The sweep keeps no cursor — what is gone is not listed. The
  other backends are still swept in the same run (progress).
- **A cancel wins** until `EraseRows`: the user row is re-read before every backend and every
  `SweepCancelEvery` (500) objects.
- **Bounded per call**: 30 s per delete, 60 s per listing page of a walk, 5 min per `List` of one
  container; a backend is given up for the run after 25 failed deletes. `half-open` is "try"
  (these calls go straight to the driver, and a target-only backend with no traffic would stay
  half-open for ever).
- **It runs in the job only.** No request path calls it.
- **Once more after the erase** (stage c2): when the rows are gone the credentials are too, so a
  second pass picks up bytes that a write in flight put down after its container was listed.
  Nothing can defer any more; a failure is an `Error` line and a `note` in the erasure record.
- **Counts**: `swept` per backend, `swept_after_erase`, `sweep_failures`, `chunk_blobs_left`,
  `swept_backends`, `unswept_backends` in the `account.erased` audit row and the `account erased`
  log line; `vaultaire_account_deletion_swept_total{backend,result=deleted|failed|shared_left}`
  on the server registry, every series created at 0 for every registered backend.

### `engine.TenantWalker` (`internal/engine/interface.go`, `internal/drivers/tenant_walk.go`)

Optional driver interface: `WalkTenant(ctx, tenantID, fn func(TenantObject) error)`. Implemented
by every driver that keys under the tenant:

| Driver (registered as) | Key shape | Sweep | Boundary | Pages | Notes |
|---|---|---|---|---|---|
| idrive (`idrive`, every `idrive-<region>` — separate stores, each swept) | `t-<T>/<C>/<A>`, fixed bucket | walk `t-<T>/` | the `/` after the id | own loop | prod primary |
| lyve | same, `stored-<region>` | walk | same | own loop | 204 on a missing delete |
| geyser | same | walk | same | own loop | see "Geyser" below |
| r2 | same | walk | same | own loop | |
| s3compat (`s3`), quotaless | `<root>/<T>_<bucket>/<A>` | walk `<root>/<T>_` | the `_` after the id + the id rules | own loop | dormant; not in prod |
| local | `<base>/<T>_<bucket>/<A>` | walk dirs `<T>_*` | same | — | registered in prod; the walk also hands out the `.tmp-*` and `.meta` files `List` hides, and reports walk errors `List` swallows |
| permafrost | folder tree `vaultaire/t-<T>/<C>/…` on every fleet account | **`List` + `Delete` per bucket** | folder identity | Graph `nextLink` | see "Permafrost" below |
| `S3Driver` (only inside quotaless today) | container = a real bucket | `List` + `Delete` per bucket | exact bucket | SDK paginator | cannot walk; `NoSuchBucket` is an error, so it would defer — not registered anywhere |

The walk runs **its own page loop**, not `s3ListPaginator`: that one stops on a repeated
continuation token, which for a sweep would report a truncated listing as a finished one.

## The dangerous half

A sweep that lists the wrong prefix deletes another customer's data. What stands in front of it:

1. **Tenant id.** Refused unless it matches `^[A-Za-z0-9][A-Za-z0-9-]*$` — checked in the plan
   and again in the function that lists. `""` collapses a prefix to everyone's; `/` makes the
   "tenant" a container of another tenant (`t-a/b/`); `_` ends the tenant part of a container
   name (`a_b` + bucket `c` = tenant `a` + bucket `b_c` on a container-keyed backend). The
   drivers refuse `""` and `/` themselves, before any request, and never fall back to
   `"default"` in a walk. Prod: 5 of 5 tenant ids match `^tenant-[0-9a-f]+$`.
2. **Other tenants' ids.** One query: if another tenant's id starts with this id plus `_` or
   `/`, the sweep is refused (the tenant is deferred, loudly).
3. **Bucket names.** An empty name is refused in the plan and again where the listing is built
   (`<tenant>_` is the prefix of every container of the tenant). Names come from the tenant's own
   rows and always sit behind `<tenant>_`, so a bucket name cannot reach another tenant.
4. **The listing's boundary.** `t-<T>/` (fixed bucket), `<T>_` (container-keyed), a trailing `/`
   on every container listing. Proven per driver against a fake endpoint that pages
   (`tenant_walk_test.go`): neighbours `tenant-abc` and `tenant-a` beside `tenant-ab`, buckets
   `X_b` and `X_bc`, a key returned from outside the prefix (the walk aborts and never hands it
   out), a page that fails, a token that does not advance.
5. **The delete.** Bound to the listed key (`Remove`), or — fallback — a name that came out of the
   `List` of the tenant's own container, deleted through the driver with the tenant in the context.

`TestErasureSweep_NeighboursAndTheChunkContainerSurvive`: four backends of three shapes, two
buckets whose names share a prefix, a container no table remembers, two neighbour tenants (one id
a prefix of the erased one, one extended from it) with buckets of the same names — every byte of
the erased tenant goes, every neighbour byte stays.

### The chunk container

Chunk blobs live in `_global`. The sweep cannot delete them:

- container-keyed drivers list `<T>_…` — `_global` does not start with a tenant id (ids cannot
  be empty or start with `_`);
- the per-bucket fallback builds `<T>_<bucket>`; the plan drops `object_locations` rows whose
  "bucket" is `_global`, and the listing refuses a container equal to it;
- on a **fixed-bucket** backend the walk *does* see them — `t-<T>/_global/…` is inside the
  tenant's prefix (see WP-R8-7) — and the sweep skips the container, counts it
  (`chunk_blobs_left`, `result="shared_left"`) and logs it.

Proven in the neighbours test (a chunk blob under the erased tenant's prefix, under `t-default`,
under a neighbour, and `_global` on two local backends: all survive; `ChunkBlobsLeft == 1`) and
by mutation: with the skip removed the test fails.

### Permafrost — decision: sweep what it can list

Its `List` returns the direct children of the container folder on every fleet account, and its
`Delete` resolves the name on every account and deletes the item. So the fallback lists the top
level and deletes each entry; an entry that is a folder (keys with a `/`) is deleted as an item —
Graph removes the subtree with it. Every request stays under `t-<T>/<C>`
(`TestOneDriveSweepShape_…`, against the Graph stub). A listing error defers.

What that leaves, said plainly:
- buckets no table remembers are not reached there (no tenant walk);
- OneDrive deletes are **soft** (the item goes to the account's recycle bin) — true of the
  head-row walk too, since #529;
- a Graph error whose text contains `404` reads as "nothing here" (the driver's convention;
  WP-R7-4);
- the subtree delete is Graph's documented behaviour, **not exercised live**.

No storage class routes to permafrost and it is excluded from failover writes, so no customer
object lands there through the product. Prod has 25 head rows on it (bench data).

### Geyser

Read, not guessed: `internal/drivers/geyser_README.md` — "Safe on archived objects: `DELETE` (so
restic prune works)"; versioning is "leave suspended on prod" because an enabled bucket leaves a
delete marker per delete. The driver's `Delete` is a plain `DeleteObject`; the walk's `Remove` is
the same call on the listed key. Not tested against the real endpoint (prod is read-only for
workers), and when Geyser frees the tape is Geyser's.

## Concurrent writer — what happens, and what the sweep does not change

| Write | What happens |
|---|---|
| bytes land **before** their container is listed, the row commits later | the sweep deletes the blob (it has no head row); the row then commits; `EraseRows` refuses (`ErrObjectsRemain`); deferred; the next run deletes the row and erases. Tested. The customer's PUT answered 200 on the day their account is erased. |
| bytes land **after** the listing, no row yet at the erase | found by the pass after the erase. Tested (`swept_after_erase`). |
| bytes land after that pass — a request authenticated before the erase and still streaming | **the WP-R10-3 residual, unchanged**: an orphan blob, possibly an orphan head row. The sweep narrows the window from "after the walk" to "after the last pass"; it does not close it. Closing it means refusing the tenant's writes from the start of the erasure (a `tenants` state the S3 auth path reads) and waiting out in-flight requests — not built. |

## Adversarial pass — what it found in my own code

1. **`s3ListPaginator` would have reported a truncated listing as finished.** The first version
   of the walk used it. It stops on a repeated continuation token — right for a customer listing,
   wrong here. Own page loop; a token that does not advance is an error. Test with a fake that
   repeats its token.
2. **The first sweep's work was invisible to its own test.** With the sweep stage removed the
   marker test stayed green — the pass after the erase deleted the blobs. The tests now assert
   `swept_after_erase == 0`: found *before* the row erase, while a failure can still defer.
3. **The chunk count doubled** (both passes counted the same blobs). Counted in the first only.
4. **Attacker.** A tenant chooses bucket and key names, not its id. Bucket names sit behind
   `<T>_`; keys are handed back by the listing and removed by the listed key (no rebuild, no path
   resolution — a key with `..` is an opaque S3 key, and the local walk removes the path it
   walked). The id rules above are for ids nobody should be able to mint; the collision query is
   the check that nobody did.
5. **Flaky backend.** Page 3 fails → error, deferred, resumes (driver and runner tests). A delete
   that hangs → cut at its deadline, three strikes at `SweepMaxFailures = 3` in the test, 25 in
   prod; without the per-call deadline the test times out (run and seen).
6. **Deploy mid-sweep.** The job's context is cancelled: the walk stops, the tenant is deferred,
   the scheduler records `interrupted` (never the day's success), no erasure record; the next run
   starts over. Tested through the scheduler. A stop **between** the row erase and the last pass:
   the account is erased, the record's `note` says the pass did not complete.
7. **The other entry point.** `POST /api/v1/admin/account-deletion` starts the same job
   (`TestErasureSweep_TheAdminTriggerSweepsToo`). Nothing else calls `EraseRows`.
8. **"Every" against the path that returns first.** "Every registered backend": a backend with an
   open breaker is skipped *and the tenant deferred*; one that vanished between plan and sweep
   cannot exist (drivers are registered at boot). "Every byte": not the chunk container (by
   rule), not an unregistered backend (cannot), not buckets no table remembers on a backend that
   cannot walk — each is counted or named in the record.
9. **A `List` error read as a miss.** `isObjectMissingErr` matches the text "not found"; a
   `List` error is never passed through it. With the error swallowed the test fails.
10. **Shared test database.** The fixture's runs are scoped (`onlyDue`), its tenants are its own,
    the collision test inserts and removes its own `tenants` row.

Every test written with the code was checked by removing the behaviour and watching it fail
(18 mutations: the chunk skip, both id checks, both empty-bucket checks, the `object_locations`
filter, walk and `List` errors swallowed, the per-delete deadline, failed deletes not deferring,
the breaker check, both cancel checks, the pass after the erase, the sweep stage itself, the
unregistered-backend record, the zero series, the failed-last-pass note) — all red.

## Tests

- `internal/drivers/tenant_walk_test.go` — six S3-class drivers on a paging fake + local: the
  tenant only, every page, exact removal, neighbours intact, `X_b` vs `X_bc`, refused ids with
  zero requests, three ways a listing cannot be completed, the chunk container reported as its
  own container. `onedrive_sweep_test.go` — the shape the fallback uses on permafrost.
- `internal/api/erasure_sweep_test.go` — 18 tests on the runner's fixture plus a fixed-bucket
  backend with fault switches and a list-only backend: the gap, the admin trigger, neighbours and
  chunks, refused ids and an id collision, an empty bucket name, the plan's sources, the schema
  walk (`TestSweepBucketSourcesCoverSchema`: a tenant table with a bucket column is a source or
  excluded with a reason), listing failure + resume (walk and fallback), hang + refuse, open
  breaker, cancel before and during, shutdown → `interrupted` → resume, a write during the sweep,
  the pass after the erase (found; failed and recorded), an unregistered backend, the zero series.
- `go test -race ./...`, `make lint` (0 issues), `make gosec` (0 issues).

## Live proof

Two servers on private databases, aws-cli. **B** is the "iDrive account": a second Vaultaire
(local disk) with a bucket `vaultaire`. **A** is the server under test: `STORAGE_MODE=local` plus
`IDRIVE_*` pointed at B — the real `IDriveDriver` over HTTP with SigV4. Two tenants on A, each
with a bucket `photos` holding `one.txt` (local), `two.txt` (`--storage-class STANDARD` → iDrive)
and `kept.txt`. T1: versioning enabled, both re-put, both deleted (`{"marker":true}`), deletion
scheduled through `DELETE /api/v1/manage/account`, backdated by SQL, `POST
/api/v1/admin/account-deletion` → 202.

| | `main` @ #553 | this branch |
|---|---|---|
| `job_runs` | `ok`, rows 1 | `ok`, rows 1 |
| `account.erased` | `objects_deleted=1` | `objects_deleted=1 swept={"local": 1, "idrive": 1} swept_after_erase=0 chunk_blobs_left=0` |
| T1 on the local backend | **`<T1>_photos/one.txt` still there** | nothing |
| T1 behind the iDrive driver | **`t-<T1>/<T1>_photos/two.txt` still there** | nothing |
| T2 (same bucket name) | untouched | untouched; `one.txt` and `two.txt` still read |
| T1's key | AccessDenied | AccessDenied |

## Prod (read-only, 2026-10-02 — the sweep was never run there)

- Registered drivers: `local`, `idrive` (primary, `STORAGE_MODE=idrive`), `lyve`, `geyser`, `r2`,
  `permafrost`. No region driver, no `s3`, no `quotaless`.
- `object_versions`: 205,604 rows, 3 tenants, **1** delete marker; live version rows whose key has
  no head row: **1** (`idrive`) — the one key the walk would miss today. 3 of 26 buckets are
  versioning-enabled.
- No bucket name is known only outside `buckets`. No tenant id or bucket name contains `_` or `/`.
- Head rows: `idrive` 2,037 (1 chunked), `onedrive` 2,613, `local` 451, `(null)` 62, `r2` 29,
  `permafrost` 25, `lyve` 6, `geyser` 2. `smart_demotions` 0, `multipart_uploads` 0.
- Accounts pending deletion: **0**. `account_deletion` last ran `ok`, 0 rows.

**An unregistered backend cannot be swept at all.** Head rows on one (`onedrive`, 2,613 in prod)
already defer the tenant daily in the walk; rows of other tables that name one do not defer —
the erasure goes through and the backends are listed in the record (`unswept_backends`) and in a
`Warn` line. Prod's `onedrive` and NULL-backend rows are WP-R7-5's.

## Legal copy, re-read against the code

The six sentences (#529: `privacy.html:41`, `terms.html:73`, `dpa.html:106`, `baa.html:94`,
`gdpr.html:27`, `:33`) say the deletion job erases the objects on the date.

- **Whole objects, including earlier versions and deleted keys: true after this PR**, on every
  backend a customer object can be on (`idrive`, `idrive-<region>`, `lyve`, `geyser`, `r2`) and on
  `local`. No customer backend is "not sweepable". Versioning is metadata-only (WP-R2-1): an
  earlier version's bytes are the blob at the key.
- **Chunked objects (over 64 MB, Standard, unversioned bucket, `chunking` on — the default): not
  true.** By design their blocks are released on the date and collected by dedup GC after its
  7-day grace; on a fixed-bucket backend they are not collected at all (WP-R8-7). **Not changed
  here**: the sentence that would be true by design ("blocks of large objects are purged within
  eight days") is false on prod's primary until WP-R8-7. This is the PM-3 treatment — the copy
  stays, the WP gates it — and the row is in SYNTHESIS.
- Seen next to them, not one of the six: `gdpr.html:37` ("Object Lock exemption … erasure requests
  are deferred until the retention period concludes") contradicts the runner, which erases locked
  objects with their owner's account (`locked_erased`, since #529). One of the two has to change;
  that is a decision, not a fix (below).

## Found on the way

### WP-R8-7 — chunk blobs have no single address on a fixed-bucket backend (P1; P0 once two customers share a chunk)

`chunkContainer`'s comment says chunks "must live outside any tenant's namespace". On iDrive,
Lyve, Geyser and R2 they do not: the driver keys `t-<tenant>/<container>/<artifact>` with the
tenant from the request context, and S3 requests carry it (`s3.go` sets `common.TenantIDKey`). A
chunk is stored at `t-<uploader>/_global/_chunks/<hash>`.

Proven with a fixed-bucket-shaped primary and the request context as `s3.go` builds it (kept as
`TestChunkBlobs_HaveOneAddressOnAFixedBucketBackend`, **skipped** — remove the `Skip` to see it):

1. Tenant A uploads; tenant B uploads the same bytes → **PUT 200** (dedup hit, nothing stored);
   **B's GET → 500**. B's read looks under `t-<B>/_global/`. B's object was never stored anywhere
   B can read.
2. Both delete; dedup GC (job context, no tenant) deletes the GCI rows and addresses the blob at
   `t-default/_global/…`: on S3 a delete of a missing key is 204 → "deleted". **The blob stays
   under `t-<A>/_global/` for ever** — after an ordinary DELETE and after an account erasure.

Why nothing saw it: every test in `internal/api` runs on the local driver, which keys by
container only, and the chunk tests build their context with `tenant.WithTenant` alone.

Prod today: `chunking` is ON (code default, no flag row); 1 chunked object (256 MB, 127 chunks,
one tenant); 0 hashes shared by two tenants; dedup GC has deleted 0 rows. Nothing is broken yet;
the first two customers who upload the same large file break it.

Not built here: it changes where every chunked PUT and GET looks, and prod's 127 blobs need a
read fallback or a move. Suggested shape: chunk Put/Get/Delete run with one fixed tenant in the
context (what GC already does), reads fall back to the uploader's prefix for blobs written before,
an operator copy of `t-*/_global/*` to the one address, then the fallback goes. The sweep's
`chunk_blobs_left` is the count of what an erased tenant leaves under its prefix until then.

**Until it is built: turn the `chunking` flag off** (global row, `/admin/flags`). Off = plain
whole-object PUTs; reads of existing chunked objects keep working. [YOU].

### Smaller

- **P3 — an `Error` line per erased tenant**: the in-process bandwidth tracker still holds the
  tenant's counters and its next flush fails on `bandwidth_usage_daily_tenant_id_fkey`
  (`bandwidth.go:241`). Seen in the live proof on both builds. The tracker should drop the tenant
  with `auth.Evict`.
- **CI flake, second sighting**: `internal/dashboard/handlers` `TestHouse_*` failed at
  `testDashDB` with `pq: sorry, too many clients already` on #553 (which touched only
  `internal/account`); passed on `gh run rerun --failed`. Two in one day is a pattern: CI's
  Postgres has `max_connections=100` and every package opens its own pools.

## Decisions taken

- A tenant walk as an optional driver interface, the per-bucket sweep as the fallback (the brief's
  "complete form").
- `Remove` is bound to the listed key; the pair is for the caller's decision, not for the delete.
- Everything under the tenant's prefix is deleted except the chunk container — including
  containers that are not `<T>_<bucket>` shaped: nothing but that tenant's requests writes there.
- Only an `open` breaker defers; `half-open` is tried.
- A pass after the erase, which cannot defer.
- Permafrost: swept by `List` + `Delete`, with the limits above.
- `local`'s `List` is left as it is (it swallows walk errors and hides temp files); the walk does
  neither.
- Log and rollup tables are not sources of bucket names (a tenant could name 100,000 buckets in
  requests that 404); the tenant walk is what reaches forgotten buckets.
- The legal copy is not weakened to a sentence that is itself not yet true.

## Not done

- WP-R8-7.
- The write-during-erasure residual (refuse the tenant's writes from the start of the erasure).
- Forgotten buckets on a backend that cannot walk (permafrost; `S3Driver`).
- `S3Driver`: `NoSuchBucket` on `List` defers. Unregistered everywhere; say if AWS-direct returns.
- An alert on `vaultaire_account_deletion_swept_total{result="failed"}`: `AccountDeletionDeferred`
  already fires on the deferral it causes.
- Prod's 62 NULL-backend rows and the `onedrive` / `local` rows: WP-R7-5. The sweep makes the
  NULL-backend case safe at erasure for registered backends (it does not need the row to name the
  backend); it does nothing for `onedrive`.

## What I could not prove

- The real iDrive, Lyve, Geyser and R2 endpoints. The iDrive driver was exercised live against a
  second Vaultaire speaking S3 (ListObjectsV2 with a prefix, DeleteObject, SigV4); the other three
  share its helper and were tested against a paging fake. Geyser's answer for a tape object is the
  README's, not mine.
- Permafrost against Graph.
- A sweep of a tenant with millions of chunk blobs under its prefix (the walk lists them to skip
  them: ~500 pages per TB at 2 MiB chunks, twice).
- The residual window is reasoned from the code, not measured.

## [YOU]

1. **Turn the `chunking` flag off** until WP-R8-7 (`/admin/flags` → `chunking` → global off).
2. **Decide `gdpr.html:37`**: does Object Lock defer an account erasure (the copy) or not (the
   code since #529)? Recommended: the code — say in the paragraph that a lock protects against
   deleting an object, not against closing the account.
3. Nothing to install. `deploy/monitoring` is unchanged.

## Post-merge review (plan driver, 2026-10-02)

Read against `main` @ #554: `erasure_sweep.go` and `drivers/tenant_walk.go` in full, every
driver's `getTenantID` fallback, and the uses of the id `"default"` across `internal/`. Not
re-read: the runner's wiring of the two passes, the tests beyond the two refused-id ones, the
permafrost fallback against Graph (nobody can run that here).

**WP-R8-7 is real.** The skipped test was run with the `Skip` removed: tenant B's GET of a
deduplicated upload answers 500. `IDriveDriver.getTenantID` / `buildKey` (`t-<tenant>/…`, tenant
from the context, `"default"` when there is none) are what the test's double imitates. It is
the next prompt, ahead of the queue.

| ID | Sev | Where | What | Status |
|----|-----|-------|------|--------|
| PM-1 | P3 (hardening; the blast radius is other tenants' bytes) | `erasure_sweep.go` `checkSweepTenant`, `sweepTenant` | `"default"` passed the id pattern. It is the id every driver and `common.GetTenantID` fall back to when a context carries no tenant, so on a fixed-bucket backend `t-default/` holds whatever any tenant's context-less path wrote — and is where dedup GC addresses chunk blobs. Registration never mints that id; a `tenants` row with it (a dev database, a hand-made row) would have been swept like any other. | **fixed here**: `sweepableTenantID` refuses the fallback id in the plan and in the function that lists. Red-first: `TestErasureSweep_RefusesTheFallbackTenantID` (on `main`: one walk, one list). |

Clean on this read: the walk's own page loop (a token that does not advance is an error, not
the end); `Remove` bound to the listed key; the prefix boundaries (`t-<T>/`, `<T>_`); the
collision query; the chunk-container skip; an open breaker defers; the sweep keeps no cursor.

Noted, not changed:
- The legal sentence is still not true for one case, and the note says so honestly: the blocks
  of a chunked object (over 64 MB) stay under the erased tenant's prefix until WP-R8-7.
- The write-during-erasure residual is narrower, not closed.
- A decision the worker surfaced and that is not yet in the decisions table: `gdpr.html:37`
  says Object Lock defers an erasure; the runner erases locked objects with their owner's
  account. Recorded as **D-26** below in SYNTHESIS.
