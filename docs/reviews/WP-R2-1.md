# WP-R2-1: write-new-key-then-swap

Stage 1a, Prompt 2. Spec: `SYNTHESIS.md` (WP-R2-1 row = R2-03/05/15, R3-15,
R13-06, WP-R3-4 driver, R7-12), `R2-object-path.md:118`. Order of record: the
FULL-PLAN TRACK (`docs/IMPLEMENTATION_PLAN.md` Stage 1) and `docs/STATUS.md`
item 7.

The work ships in two halves because of the two-slot deploy. The previous
build keeps serving the new schema for up to 15 min (`LONG_OP_TIMEOUT`,
900 s). A reader that does not know the new column must never meet a row that
needs it. So:

- **Part B, "expand" (Prompt 2B, this PR, migration 086):** every reader of
  whole-object bytes honours the blob's own address. Deployed before any
  writer sets one.
- **Part C, writers (Prompt 2C):** a write goes to `<key>@<version>` and the
  head-row transaction flips the pointer. Appends here and flips the
  SYNTHESIS row.

Part B is inert on prod: after 086, every `storage_key` is NULL (counts below).

## The bug, seen first

Every whole-object writer writes the backend blob **in place** at
`<container>/<key>` and only then runs the head-row transaction:

- plain PUT (`HandlePut`)
- CopyObject (`s3_copy.go`)
- CompleteMultipartUpload (`s3_multipart.go`)
- generated objects (`background_put.go`)
- direct-upload complete (`direct_upload.go`)

A key therefore has exactly one blob. The consequences on record:

- **R2-03.** `GET ?versionId=<non-current>` answers **501** (stop-gap #484). The
  version's bytes were overwritten.
- **R2-05 residue, R3-15.** A failure after the backend write has already
  destroyed the previous object.
- **R2-15.** Two concurrent PUTs to one key can leave the row describing the
  other body.
- **R13-06.** The Smart reclaim's stale-copy delete can remove a fresh PUT's
  bytes. `checkLostWrite` detects it; nothing prevents it.
- Edge caches need purge-on-overwrite.
- Versioned buckets bill only the latest object, while "retained" versions
  hold no bytes.

The fix is immutable blob keys. The precondition is that **no reader assumes
the key is the address.** On `origin/main` every reader did. With a row
pointed at `x@v1` by SQL and a stale blob left at `x`, on `8d69ed0` + 086:

```
GET x                       -> the stale bytes at <key> (Get, Range, parallel_get, /cdn)
CopyObject src=x            -> 503 (the stale blob's size differs from the row's)
Smart demotion of x         -> cold copy written at <key>, not at x@v1
reclaim (hot copy)          -> deleted the unrelated blob at <key>, left x@v1
vault parity of x           -> shards computed from <key>; GET from parity returned the wrong bytes
routing truth (2 rows at sk, 1 at key) -> present 1 / missing 2 (inverted)
DELETE x                    -> deleted <key>, left x@v1
GET ?versionId=v1 (row has storage_key) -> 501
TestWholeObjectReads_GoThroughTheBlobAddress -> 40 call sites on a raw key
```

## What was built (Part B)

**Migration 086** (`086_object_storage_key.sql`, additive, idempotent, re-run twice clean):

- `object_head_cache.storage_key TEXT`. NULL = the blob is at `<key>`: every
  row today, for ever. No backfill, and no blob ever moves.
- `object_versions.storage_key TEXT`. NULL = no bytes retained for this
  version. The exception is the newest live version behind a delete marker,
  which `versionBytesIntact` still serves from `<key>`.
- `object_versions.encryption_algorithm TEXT`. A reader of a non-current
  version must know whether to decrypt without the head row, which describes
  the CURRENT object. Not in the prompt's B1; this is the minimum B3 needs.
  See decisions.
- `smart_demotions.storage_key TEXT`. The address the demotion copied under.
  The ledger addresses the copies it made, never the head row of now. Also
  beyond B1, and needed for the ledger to be correct in the window when 2C
  rows exist.
- `CHECK (storage_key IS NULL OR storage_key <> '')` on all three
  (`NOT VALID`: no table scan at deploy).
- `retired_blobs` `(backend_id, container, storage_key, tenant_id, reason,
  retire_after)`, PK `(backend_id, container, storage_key)`. It is the
  `retired_chunk_blobs` shape, for whole objects, with indexes on
  `retire_after` and `tenant_id`.

The header records why the address is a per-row column and not a naming rule:

1. NULL says "written before the rule" for ever, with no flag day.
2. The address is a fact about the blob. A reader must not re-derive what a
   writer recorded (two derivations drift; R8-7 was that bug for chunks).
3. 2C picks the address and may need a different shape per backend (key-length
   limits below).
4. A demotion copies to another backend under the same address, which only
   works if the address is data.

**One resolver** (`internal/api/blob_address.go`): `objectBlobAddress(tenant,
bucket, key, storageKey) (container, artifact)` and `blobArtifact(key,
storageKey)`. Every reader goes through it, and the engine's routing map
(`HintBackend`) is seeded with the address. The full site list is at the end
of this note.

**B3, versions:**

- `GET ?versionId=<non-current>` serves the version whose row has a
  `storage_key`.
  - ETag, size, content type and Last-Modified (= the version's `created_at`)
    come from the version row, as do `x-amz-version-id`, conditionals and Range.
  - SSE-S3 decrypts through `DecryptBytes`, which opens under the key version
    in the blob's 0x02 header. SSE-C works with the request's key.
  - Egress meter and access log run as for any GET.
  - The head row's user metadata, tags and echo headers are **not** sent with
    an older version. They describe the current object, and no per-version
    copy of them exists.
- A version without an address keeps today's 501 text.
- `DELETE ?versionId` of a version with bytes queues its blob in
  `retired_blobs` (`version_deleted`, `retire_after = now + 24 h`). This
  happens in the same transaction as the row delete, under the key's version
  lock.
  - It is skipped when the head row or another version still names the blob.
    Deleting the current version must not strand the current object.
  - Object Lock is checked first, as today (R2-01).
  - Until 2C's collector ships, the rows wait. That costs storage, never data.
- `ListObjectVersions` is unchanged (sizes come from the rows).

**B4:**

- Routing truth asks the recorded backend for the address. The re-read after a
  miss compares the address too, so a repointed row is `changed`, not `missing`.
- `resolve-null` asks every driver for the address, and its UPDATE is guarded
  by it.
- The boot check reads backend NAMES only (no key), so it needed nothing. The
  daily job is what asks backends.
- The dashboard's object browser reads sizes and ETags from the rows and makes
  no backend call. Verified: no engine/driver read anywhere under
  `internal/dashboard`.

**The two-slot window, writer side.** This is not a reader, but required for
"inert": while 2C deploys, this build keeps serving and writing in place. A
write from this slot over a row that a 2C slot gave an address must not leave
the row naming the old blob.

- `atomicHeadUpsertReleasing` (every PUT / copy / complete / generated-object
  upsert) clears `storage_key` on the row it writes. In the same transaction it
  queues the displaced blob in `retired_blobs` (`overwritten`), unless a
  version row keeps it.
- `upsertWholeObjectHeadRow` (also direct upload) sets `storage_key = NULL`.
- The Suspended bucket's `null`-version rewrite (`writeVersionRow ON CONFLICT`)
  clears its address and `encryption_algorithm`.
- `dropDisplacedBlob` leaves an addressed displaced blob to the queue.
- The delete-marker branch queues the head row's blob unless a version keeps it.
- Plain DELETE and DeleteObjects leave a blob a version row keeps (a Suspended
  bucket's plain DELETE of a key whose bytes are also an Enabled-era version).

`object_write_shared.go` was "read only" in the prompt. Two clauses changed
there (`storage_key = NULL` in `upsertWholeObjectHeadRow` and in the
`null`-version upsert), plus `deleteVersion`'s retire. They are the minimum for
the window and for B3. 2C rewrites both upserts to record their own address.

## The same bug elsewhere: the audit

Every storage call in `internal/api` whose artifact is a key variable was
listed by `TestWholeObjectReads_GoThroughTheBlobAddress`: 40 on `origin/main`.
Each now goes through the resolver or is a listed writer site. Beyond the
prompt's list, the walk found:

- `s3.go` HEAD's `RestoreStatus` (the x-amz-restore passthrough).
- `s3_batch.go` DeleteObjects.
- `account_export.go` (the GDPR export object's delete).
- `smart_promotion.go` `submitRestore` / `requestRestore` (the auto-restore of
  a demoted object; `writeArchivedRead` / `OnArchivedRead` now carry the blob).
- `promote` and `PromotePending` (the copy-back reads the ledger's address).
- `cmd/tools/routing-truth` and `cmd/tools/dedup-migrate` (operator tools that
  read the head row's object; they now read `COALESCE(storage_key,
  object_key)`, and dedup-migrate clears the address when it chunks an object).

Not readers, left as they are:

- **The erasure sweep** walks the tenant's prefix, not rows. An addressed blob
  is inside the prefix and is swept. A legacy `<key>` blob that no row names
  any more (2C writes `x@v2`; the old `x` stays) is "bytes no row names" and is
  swept too.
- `retired_blobs` is a sweep source (its `container` minus the tenant part):
  on a driver without a tenant walk (permafrost), its bucket is listed even
  when no other row remembers it.
- **The chunked path** has one address by hash. Versioned buckets stay excluded
  from chunking.
- **The inventory CSV** reads size and etag from rows.
- **`ListObjectVersions`** reads sizes from rows.

## Adversarial pass on my own code

- **A version GET when the key's current state is a delete marker** (no head
  row): the version branch sets `cacheHit` itself, so the conditionals, Range
  and the Content-Length all run on the version.
- **A version GET of an object on cold storage:** the Smart promoter is not
  consulted for versions. Demotion skips versioned buckets, and `OnRead` keys
  on the head row's etag, so it would have done nothing anyway. The
  `last_accessed` touch is skipped too (it would mark the current object read).
- **Parity fallback for a version:** `Open` is bound to the head row's etag, so
  a version's etag never matches and the error is surfaced. Correct: parity
  protects the current object only.
- **Retire races.** `blobStillNamed` runs inside the writer's transaction,
  after its own upsert, under the per-key advisory lock every head writer
  holds. A 2C writer that names the same address afterwards cannot exist, since
  2C's addresses are unique per write.
- **Retire of an address on a backend that no row records** (`backend_name`
  NULL): `errUnaddressableBlob`, skipped (the collector could not address it).
  Left to the erasure sweep. Not possible for a 2C row.
- **The ledger in mixed state.** `routeOf` now returns the address. "Routes to"
  is `(backend, address)`, so a hot or cold copy at the ledger's address is
  stale whenever the key routes elsewhere, including the same backend at
  another address (2C's overwrite). For every legacy row both addresses are
  `<key>`, so the old behaviour is unchanged.
- **A version row keeping the ledger's address** (a bucket versioned after the
  demotion): `settle` deletes nothing. That leaks a copy (money) rather than
  risk a version's bytes.
- **`checkLostWrite`** now compares the address deleted with the address the
  key routes to. A 2C write at its own address can never be the bytes a
  stale-copy delete removed. For legacy rows it is the old check: R13-06 stays
  detected until 2C closes it.
- **Flip-back guard:** `UPDATE … AND COALESCE(storage_key,'') = <ledger's>`. A
  same-etag re-PUT that 2C gives a new address no longer flips the old
  ledger's copy in.
- **A false negative in my own structure test, found and fixed.** The first
  regex was greedy and missed `if err := x.Delete(ctx, c, key); err != nil …`
  (36 sites seen instead of 40). It now matches parentheses.
- **Mocks:** `s3_presign_test.go`'s sqlmock regex followed the probe query's
  new column.

## Tests

Red first. "Before" = the same tests on `origin/main` (`8d69ed0`) + migration
086, in a scratch worktree.

| Test | Before | After |
|------|--------|-------|
| `TestGet_ServesTheRowsStorageKeyNotTheObjectKey` (fixed-bucket shape) | stale `<key>` bytes | pass |
| `TestGet_Range_ServesTheRowsStorageKey` | `SSSS…` (stale) | pass |
| `TestParallelGet_ServesTheRowsStorageKey` (`parallel_get` on, `rangedDriver`, whole + Range) | stale | pass |
| `TestGetVersion_NonCurrentWithBytes_Served` (+ Range 206, If-None-Match 304 on the version's ETag, current unchanged) | 501 | pass |
| `TestGetVersion_SSES3VersionOpensUnderItsOwnKeyVersion` (v1 ciphertext, rotate, v2) | 501 | pass |
| `TestGetVersion_NonCurrentWithoutBytes_Still501` | pass (guard) | pass |
| `TestDeleteVersion_WithBytes_RetiresTheBlob` | no row | `version_deleted` row, backend + container right |
| `TestDeleteVersion_BlobTheHeadRowNames_NotRetired` | pass (guard) | pass |
| `TestPut_OverARowWithStorageKey_ClearsItAndRetiresTheDisplacedBlob` | row kept `p@v1`, nothing queued | pass |
| `TestPut_SuspendedNullVersionRewrite_ClearsTheVersionsAddress` | address kept | pass |
| `TestDelete_DeletesTheRowsStorageKey` | `x@v1` left | pass |
| `TestCDN_ServesTheRowsStorageKey` | stale | pass |
| `TestCopyObject_SourceIsTheRowsStorageKey` | 503 | pass |
| `TestSmartDemotion_CopiesUnderTheStorageKeyAndFlipBackKeepsIt` | cold copy at `<key>` | pass (ledger records `k@v1`; flip-back drops the cold `k@v1`) |
| `TestReclaim_DeletesTheHotCopyAtTheStorageKeyNeverTheObjectKey` | deleted `<key>`, kept `k@v1` | pass |
| `TestVaultParity_ProtectsAndRebuildsFromTheStorageKey` (one shard lost, data ranges needed) | wrong bytes | pass |
| `TestDeletionRunner_DeletesTheStorageKeyAndReleasesQuota` | n/a (signature) | pass; `<key>` left to the sweep |
| `TestRoutingTruth_AsksTheRecordedBackendForTheStorageKey` | present 1 / missing 2 | present 2 / missing 1 |
| `TestWholeObjectReads_GoThroughTheBlobAddress` | 40 sites | 0 (≥ 40 calls inspected; listed writer sites asserted to still exist) |

Gates run before the PR:

- `make lint` with golangci-lint v2.14.0: 0 issues.
- `make gosec` (go run, Makefile flags): 0 issues.
- `make test-integration` on `vaultaire_test_r21b` (through 086): result in
  the PR body.
- `TestSQLLiteralsMatchSchema`, `TestExportSectionsCoverSchema`,
  `TestSweepBucketSourcesCoverSchema`, `TestErasedTablesCoverSchema`,
  `TestChunkBlobCalls_GoThroughTheChunkStore`: all pass.
- `retired_blobs` is excluded from the export with a reason, deleted by
  `EraseRows` (by tenant), and a sweep bucket source.

## Live proof (local, 2026-10-12)

Setup:

- Two local builds of this branch on a private DB (`vaultaire_live_r21b`,
  migrated through 086).
- Server **B** (:8197, `STORAGE_MODE=local`) plays the tape. Server **A**
  (:8199, `STORAGE_MODE=local`) registers `geyser` with
  `GEYSER_ENDPOINT=http://127.0.0.1:8197` and B's keys. That gives A a real
  second, S3-signed cold backend.
- Driven with aws-cli.

**GET and Range.** PUT `x` (stale body), copy a fresh body aside as `x@v1`,
then `UPDATE … SET storage_key='x@v1', size, etag`:

```
before setting storage_key: STALE body written at the object key
after  (row names x@v1):    FRESH body that the row names via storage_key
range 0-4:                  FRESH
the blob at <key>:          STALE body written at the object key   (untouched)
```

**Versions.** Versioned bucket, v1 = 12 B, v2 = 20 B:

```
get-object --version-id v1 (no storage_key):  NotImplemented … non-current versions are listed but their data is not retained.
after UPDATE object_versions SET storage_key='k@<v1>':
  "dd8f100298ff923592ab35dc15788abc"  12  de237b3c-…   body "version one"
current object: "version two, longer"; list-object-versions: v2 20 True / v1 12 False
delete-object --version-id v1 -> retired_blobs: local | <tenant>_vers | k@<v1> | version_deleted | retire_after > now+23h
the blob stays on disk for the collector (2C)
```

**Smart demotion** (admin JWT, `smart_demotion` flag on for the tenant, row
aged 30 d by SQL):

```
dry run: candidates 1, idle_candidates 1, demoted 0
run (202): job_runs smart_demotion ok, rows 1
head row: geyser | x@v1     ledger: local -> geyser, storage_key x@v1, hot copy on record
on B: tape/t-<A tenant>/<A tenant>_proof/x@v1   (the cold copy, under the SAME address)
```

**Routing truth.** Run while `x` was on geyser:

- `geyser: 1 sampled, 1 present` (asked for `x@v1`).
- `local` showed 1 missing. That row was B's own copy of the tape object:
  both servers share the live DB, so A's routing truth asked A's disk about B's
  row. It is a harness artifact, not a finding.

**Read after the demotion, inside the grace:**

- GET `x` returned FRESH. The head row flipped back to `local | x@v1`.
- The cold copy at `x@v1` was removed from B.

## Prod (read-only)

**Before (2026-10-12 15:01 UTC).** psql as the app's DB user, read-only
session, SQL from a file:

- `/version` = `a05bf411c91d`, `/health` 200.
- `object_head_cache` 7,488 rows (7,487 whole).
- `object_versions` 205,604 rows (204,905 non-current live).
- `smart_demotions` 0.
- No `storage_key` columns, no `retired_blobs` (086 not yet applied).
- Longest object key: 60 bytes; longest leaf: 32 bytes.

**After the deploy** (#684 merged as `1f24e5a`; `/version` = `1f24e5a7394f`,
built 15:24:10 UTC, active slot 8001; checked 15:26 UTC):

- `object_head_cache`: 5,421 rows, **0** with a `storage_key`.
- `object_versions`: 205,604 rows, **0** with a `storage_key`, **0** with an
  `encryption_algorithm`.
- `smart_demotions` with a `storage_key`: **0**.
- `retired_blobs` depth: **0**.
- `/health` 200, three in a row (`degraded` as before: the 11 iDrive criticals,
  untouched).
- Errors since the switch: the three routing-truth boot findings every boot
  logs (WP-R7-5 rows, WP-R6-1). Nothing about storage keys or retirement.
- Canary: the iDrive regional probes keep failing as before. Primary `lyve`.
- `routing_truth`, `smart_demotion` and `vault_parity` jobs: last outcome `ok`.

The head rows fell from 7,488 to 5,421 between the two snapshots. The cause is
one tenant's own churn, not this deploy:

- `tenant-14a623b16b3f7012` logged 10,049 `object.created`, 10,388
  `object.deleted` and 8 `bucket.deleted` events between 14:26 and 15:17 UTC.
- That window ended before this build served (15:24).

**Not shown:**

- **No synthetic check runs on prod.** `SYNTHETIC_CHECK_URL` is not in the
  prod `.env`, and the active slot exports no `vaultaire_synthetic_check_*`
  series. "Synthetic green through the switch" cannot be claimed from prod. It
  stays a [YOU] row (R13-13 checklist item 4).
- The two-slot switch completed: `/version` moved to the new sha and `/health`
  stayed 200.

Nothing changes for any customer: every address column is NULL, so every
reader resolves `<key>` exactly as before.

## Decisions (to report; not built here)

**1. The separator and the key-length rule 2C will need.**
`<key>@<26-char id>` adds **27 bytes** to the leaf. `@` is not escaped by
Sync's `davName`, by S3 or by OneDrive. The limits per backend, from the
drivers' key shapes:

- Worst case: tenant UUID = 36, bucket = 63, so container
  `<tenant>_<bucket>` = 100.
- **Fixed-bucket S3** (idrive, idrive-<region>, wasabi, r2, lyve, geyser:
  `t-<tenant>/<container>/<key>`):
  - Limit is S3's 1,024 bytes for the whole key. The prefix is
    2 + 36 + 1 + 100 + 1 = 140 bytes.
  - Today a customer key is at most **884 bytes**; with the suffix,
    **857 bytes**.
  - With a 3-character bucket the prefix is 80, giving 944 / 917.
- **Sync** (`sync`, WebDAV): the limit is per mapped name, 248 UTF-16 units
  (measured 2026-10-07, `webdav.go` `webdavMaxNameUnits`), on the leaf + the
  2-char marker `%o` / `%s`.
  - Today the leaf is at most **246** mapped units; with the suffix, **219**.
  - `davName` triples `%`, reserved characters, a leading space or `~`, and a
    trailing `.` or space.
  - Striped pieces are named by sha256 and are unaffected.
- **Permafrost** (OneDrive fleet: `vaultaire/t-<tenant>/<container>/<key>`):
  - Microsoft's documented limit is 400 characters for the whole decoded path
    (and 255 per segment). It is documented, not re-measured here: no fleet
    credentials leave the box.
  - The prefix is 9 + 1 + 2 + 36 + 1 + 100 + 1 = 150, so a key is at most
    **250** characters today and **223** with the suffix.
- **Local**: the filesystem's 255-byte name limit; the leaf is at most 228 with
  the suffix.
- Prod's longest key is 60 bytes and its longest leaf 32, so nothing on prod is
  near any limit.

Recommendation for 2C:

- Enforce at PUT time, before any byte is read.
- 400 `KeyTooLongError` when `len(key) > 1024 − 140 − 27` on any backend.
- Refuse (or, for Sync, address by hash) a leaf whose mapped length plus 27
  exceeds the target's per-segment limit.
- Today's behaviour for an over-long Sync leaf (`errNameTooLong` →
  invalid input) already covers the read side.

**2. `object_versions.encryption_key_version` for the whole-object rotation gauge.**

- The 0x02 header holds the version, so reads do not need a column.
- The rotation backlog gauge (`keyRotationPendingSQL`) counts from
  `object_head_cache.encryption_key_version`. Once 2C retains versions, their
  blobs are SSE-S3 objects the gauge cannot see.
- **Recommend:** 2C adds `object_versions.encryption_key_version INT` in 087,
  written by the version write from the same value as the head row, and the
  gauge counts both tables.
- Related: this PR added `object_versions.encryption_algorithm` (086), because
  B3's reader needs to know *whether* to decrypt.

**3. Carried unchanged:**

- the 11 iDrive criticals (firing on purpose)
- `wasabi` target-only and the two #677 bypasses (→ WP-R6-1)
- the PG 16 drop on 2026-10-13
- the Sync.com sub-processor sentence
- gey48
- `tenant_encryption_keys` → the D-12 drop

## Not done / residuals (2C's)

- **No writer sets an address**, and there is no collector for
  `retired_blobs`. Both are 2C's.
- **Direct-upload complete** upserts outside `atomicHeadUpsertReleasing`. It
  clears the address, but a 2C blob it displaces is not queued, so the
  erasure sweep is its only remover.
  - Same for the old blob of a `null` version that this build rewrites in
    place over a 2C row.
  - Both are the two-slot window only.
- **Deleting the current version** with `DELETE ?versionId` leaves the head row
  describing that version (today's metadata-only behaviour). With bytes, the
  blob is kept because the head row names it. 2C must re-point the head row to
  the newly-latest version's address in the same transaction.
- **DeleteObjects is not versioning-aware** (no delete marker; pre-existing).
  This PR only stops it from deleting a blob that a version row keeps.
- **The CDN purge-on-overwrite need** goes away only when 2C writes new
  addresses.
- **R13-06** is detected, not prevented, until 2C.

## [YOU]

- Nothing to switch for this half: it ships inert.
- The prod synthetic check is not configured (`SYNTHETIC_CHECK_URL` and its
  tenant/keys). It is the customer-path signal 2C's deploy should be watched
  with.

---

## 2C's checklist — every site that reads whole-object bytes, and the function it goes through now

| Site | Reads/acts on | Through |
|------|---------------|---------|
| `HandleGet` — `engine.Get`, native `GetRange` (both), `openParallelGet`, SSE-S3/SSE-C decrypt, parity fallback, archived read | head row, or the version row for a non-current version | `objectBlobAddress(t.ID, bucket, key, storage_key)` → `blob`; `HintBackend(container, blob)` |
| `versionBytesIntact` (newest live version behind a marker, no address) | `<key>` | unchanged (NULL rule) |
| HEAD | — (cache-only) | — ; its `RestoreStatus` passthrough: `blobArtifact(req.Object, storage_key)` |
| `/cdn` (`cdn.go`) | head row | `objectBlobAddress` |
| CopyObject source (`s3_copy.go`) | head row | `blobArtifact(srcKey, storage_key)` |
| RestoreObject (`s3_restore.go` `objectRestorer`) | head row | `blobArtifact` (returned with the restorer) |
| Smart demotion copy + flip (`smart_demotion.go demote`) | candidate row | `objectBlobAddress`; ledger `storage_key` recorded; flip guarded by it |
| Smart `flipBack` / `promote` / `PromotePending` / auto-restore (`smart_promotion.go`) | ledger row | `objectBlobAddress(…, ledger storage_key)` |
| Ledger `routeOf` / `settle` / `deleteCopyOn` / `checkLostWrite` / `dropDisplacedBlob` (`smart_ledger.go`) | head row or newest live version; ledger | `keyRoute.blob`, `routesTo(backend, blob)` |
| Vault parity protect read + `Open` data ranges (`vault_parity.go`) | candidate row / caller's blob | `objectBlobAddress`; `Open(…, key, blob, …)` |
| DELETE (`HandleDelete`), DeleteObjects (`s3_batch.go`) | head row | `blobArtifact`; skipped when `versionKeepsBlob` |
| Delete marker branch | head row | `retireBlobNow(…, overwritten)` unless a version keeps it |
| `DELETE ?versionId` (`deleteVersion`) | version row | `retireBlob(…, version_deleted)` in the row's tx |
| Deletion runner (`deletion_runner.go deleteObject`) + hot copy | head row, ledger | `objectBlobAddress`, ledger `storage_key` |
| Export object delete (`account_export.go`) | head row | `blobArtifact` |
| Routing truth `checkWhole` + re-read (`routing_truth.go`) | head row | `objectBlobAddress`; re-read guarded by `storage_key` |
| `resolve-null` (`admin_routing.go`) | head row | `objectBlobAddress`; UPDATE guarded by `storage_key` |
| `cmd/tools/routing-truth`, `cmd/tools/dedup-migrate` | head row | `COALESCE(storage_key, object_key)` |
| Erasure sweep | tenant prefix | unaffected (sweeps addressed and orphaned `<key>` blobs alike) |
| Writers still in place at `<key>` (2C changes them): `placeObject` (PUT, copy, complete, generated objects), direct-upload over-quota delete, the chunked PUT/copy legacy-blob delete | — | listed in `TestWholeObjectReads_GoThroughTheBlobAddress` |

**Measured key-length limit per backend**, with the 27-byte `@<26>` suffix and
the worst-case container (UUID tenant, 63-character bucket):

| Backend | Customer key max |
|---------|------------------|
| Fixed-bucket S3 (idrive*, wasabi, r2, lyve, geyser) | 857 bytes |
| Sync | leaf of 219 mapped units |
| Permafrost | 223 characters (documented 400-character path limit) |
| Local | leaf of 228 bytes |
