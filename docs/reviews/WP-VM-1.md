# WP-VM-1: a chunked object can be a version

Stage 1a Prompt 7, 2026-10-14. Spec: `WP-R3-1.md` decision 2. Closes PM-3
of `WP-R2-1.md`, R61-5 and R61-8 of `WP-R6-1.md`, R85-2 of `WP-R8-5.md`.

PRs:
- **#726**: Part 0, the WP-R10-8 post-merge probe (`WP-R10-8.md` § Post-merge).
- **#728**: Parts A–C, migration **092**.
- **#729**: Part D, the `versioned_chunking` flag (default OFF).

Next migration: **093**.

## Decisions

1. **A manifest's address lives in the existing `object_key` column.**
   - A chunked write puts its manifest at `newBlobAddress(key)` = `<key>@<26 base32>`, the whole-blob scheme.
   - That address goes in `object_key` of `object_metadata` and `tenant_chunk_refs`.
   - Their unique constraints are untouched: `UNIQUE(tenant, bucket, object_key)` and `(…, chunk_index)` are the ones the previous build's `ON CONFLICT` names.
   - `VARCHAR(1024)` holds the longest accepted key (857) plus the 27-byte suffix.
   - The head row's and the version row's `storage_key` name the address; `backend_name` stays `chunked`.
   - A manifest written before 092 stays at its key and is read through `COALESCE(storage_key, object_key)` for ever.
   - 092 therefore needs no manifest column. It adds R61-8's version attributes and an index for the orphan pass.
2. **The two-slot window (≤ 15 min) and a rollback.**
   - The previous build reads a manifest by key. On it, an addressed chunked object is a 500, never wrong bytes.
   - Its overwrite or delete releases nothing by key and drops the head row, so the addressed manifest is orphaned. Its chunks stay pinned and the bill stays exact.
   - The collector's **orphan pass** queues an addressed manifest that no row names after the grace; the next run releases it.
   - The previous build's collector skips `chunked` rows, as it does any unregistered backend: a note, never a failed run.
3. **A deduplicated version is billed at its logical size** (the C2 rule). Three 256 MiB versions differing by 1 MiB bill 768 MiB while writing 265 MiB.
4. **`versioned_chunking` is dark** (default OFF), per tenant first. It goes global after a canary tenant has run on it for a while. The rest is the owner's call.
5. **Customer-visible:**
   - a Suspended DELETE now answers `x-amz-delete-marker: true`, `x-amz-version-id: null`, and the null version's bytes go (R61-5);
   - HEAD sends `x-amz-version-id` (R85-2);
   - a version's GET, HEAD and copy carry its own metadata and headers (R61-8);
   - the `x-amz-copy-source-if-*` conditions answer 412 where AWS does.

## What was built

- **A — the address.**
  - The chunked PUT, Complete and the chunked copy write the manifest at a fresh address.
  - Every reader resolves it through the row: GET, Range, the read window, CDN, copy source and routing truth.
  - A direct upload over a chunked object leaked its manifest before (a nil releaser). That is fixed by the same design.
- **B — retire, never release at the flip.**
  - `commitBlobWrite` and every delete keep a displaced manifest while a head or version row names it.
  - Otherwise they retire it (`retired_blobs`, backend `chunked`, the 24 h blob grace). The deletes are DELETE, DeleteObjects, the marker, `?versionId`, the Suspended delete, the deletion runner and the export purge.
  - The collector releases a manifest under the key's lock after `manifestNamedBy` re-checks it: refs deleted, GCI counts decremented. That is not a bill. Dedup GC then reclaims as before.
  - `anchorNullVersion` anchors a chunked head row: **PM-3 closed**.
  - Retained versions are matched by address **and kind** (`usage.VersionRetained`); the `is_chunked` short-circuit is gone.
  - `EraseRows` locks the tenant's refs before its set-based release. A collector release racing it would otherwise decrement a shared chunk twice; dedup GC's recount would heal it, but the lock is cheaper.
- **C — every reader.**
  - GET and HEAD `?versionId` of a chunked version: Range, the read window, If-*, SSE headers, version id.
  - Copy from a chunked version: a manifest copy where a PUT would chunk, re-stored whole elsewhere.
  - ListObjectVersions.
  - Routing truth samples chunked versions through their manifests.
  - The sentinel on `object_versions` is neither an unknown backend nor an unregistered one in the sweep report.
  - Verified unchanged: Smart demotion skips chunked rows and versioned buckets; `vault_parity` skips chunked rows; the export lists versions as before; key rotation moves chunks per hash, and a chunked non-current version reads after a rotation (test).
- **R61-5.** A Suspended DELETE replaces the null version with a null marker in one transaction under the key's locks. Its bytes are retired and released. An Enabled-era current version keeps its bytes behind the marker. DeleteObjects does the same.
- **R61-8.** Every version write copies the head row's attributes onto the version row (`copyHeadAttrsToVersionSQL`). GET and HEAD `?versionId`, the copy and the head repoint after a version delete use them. The four copy-source conditions are decided on the source (`evaluateCopySourceConditions`).
- **R85-2.** HEAD names the current version, and a delete marker on its 404.
- **D.** `decidePlacement` chunks into Enabled and Suspended buckets only with the tenant's flag. Reads never depend on it. A chunked copy into a versioned bucket answers its version id.

## Before → after

Before = the red commits' tests on `origin/main` `e209950`: **19/19 red**.

| Case | Before | After |
|---|---|---|
| a chunked write's manifest | at the key (`storage_key` NULL) | at its address |
| overwrite / delete of a chunked object | the manifest released in the transaction: a GET in flight lost its chunks to the next GC | retired, released by the collector after 24 h |
| PM-3: Enabled overwrite / marker over a pre-versioning chunked object | `GET ?versionId=null` **404**, bytes released | 200, the bytes, billed as a retained version |
| GET / HEAD / Range / If-* of a chunked version; ListObjectVersions | 404; not listed | 200 / 206 / 304 / 412; listed (size, STANDARD) |
| copy of a chunked version (restore onto its key; into an unversioned bucket) | 404 | restored whole; manifest copy |
| R61-8: GET of a non-current version | no metadata | its own |
| R61-8: copy-source conditions | 200 | 412 where AWS answers it |
| R85-2: HEAD on a versioned bucket | no `x-amz-version-id` | the current id |
| R61-5: Suspended DELETE | the null version kept, live and billed | null marker; bytes retired and released |
| a manifest no row names (the previous build's delete) | leaked | found, released |
| Object Lock: a locked chunked version | — | DELETE 403; a retired row for it is kept by the collector |
| versioned PUT / Complete / copy with the flag (Part D) | whole | chunked, three versions sharing chunks |

The randomised ledger test (`TestLedger_RandomSequence_…`) now covers:
- chunked writes in Off, Enabled, Suspended and a mid-run `→Enabled→Suspended`;
- collector runs;
- every earlier writer and deleter.

The ledger equals `BilledBytes` after every step: 10 runs × 4 scenarios × 60 steps.

## Local live proof

Lab build (`-tags lab`, `VAULTAIRE_LAB_LOG_CALLS=1`), `STORAGE_MODE=local`, a private DB through 092, aws-cli 2.28.

| Step | State |
|---|---|
| 1. bucket unversioned, PUT 80 MB → chunked | head `k@h77c…`, 37 refs, used 80 MB |
| 2. Enabled, overwrite with 20 MB and 30 MB (flag off: whole) | the 80 MB object is version `null`, backend `chunked`, its manifest kept; used 130 MB |
| 3. DELETE → marker | marker latest; nothing retired; used 130 MB |
| 4. GET each `?versionId` | md5 of A, B, C all match |
| 5. Range 10–11 MB on `null` | md5 matches the source slice; HEAD `?versionId=null` 80000000, the ETag, STANDARD |
| 6. `copy-object --copy-source k?versionId=null` onto `k` | current = A (md5), `CopySourceVersionId null`, HEAD names the new version; used 210 MB |
| 7. DELETE every version | used 0; retired: 1 manifest (`version_deleted`), 3 blobs; refs still 37; 37 chunk blobs on disk |
| 8a. collector (grace backdated) | refs 0; 37 GCI rows at ref 0, marked |
| 8b. dedup_gc (grace backdated) | GCI rows 0; chunk blobs on disk 0; whole blobs 0 |
| R61-5: Suspended, a pre-versioning chunked 80 MB + a null-version 20 MB, plain DELETE of both | two `null` delete markers (latest), 0 live versions, GET 404 ×2, used 0, both retired (`deleted`) |

### The point of it: three 256 MiB versions, 1 MiB apart (local lab, flag off vs on)

| | whole (flag off) | chunked (flag on) |
|---|---|---|
| backend bytes written | 768 MiB | **265 MiB** |
| chunk blobs created | 0 | 125 (V2 and V3 added 2 each) |
| PUT MB/s | 208 / 222 / 220 | 161 / 164 / 165 |
| GET `?versionId` MB/s (md5 ok) | 432 / 424 / 437 | 473 / 472 / 465 |
| bill | 768 MiB | 768 MiB (logical, C2) |

## Prod proof

2026-10-15, bench tenant `tenant-14a623b16b3f7012`, prod `e367364` (#729) on the Lyve primary, aws-cli 2.35 on the SLC box through stored.ge (Cloudflare). The bench keys were written to the box from stdin (0600) and removed after.

**The flag.** The owner could not sign in to the dashboard and asked for the bypass. So:
- a 15-minute admin JWT was minted for the admin account from the stored JWT secret;
- the **audited** flags API was called with it;
- `audit_logs`: `flag.set` at 01:43:03 UTC and `flag.unset` at 01:44:59 UTC, both for `tenant-14a623b16b3f7012`.

The rows name the admin account as the actor, because the token was minted for it. The token was deleted afterwards. No raw SQL wrote to prod.

**A first attempt** with `put-object` of 100 MiB was refused by Cloudflare with **413**: its plan caps a request body at 100 MB, and 100 MiB is 104.9 MB. Nothing was stored. The retry used `aws s3 cp` (multipart, as a client sends that size). A Complete is placed as a PUT (WP-R3-1), so it chunked.

| Step | Result |
|---|---|
| three 100 MiB versions, 1 MiB apart (`aws s3 cp`) | 34.3 / 41.2 / 32.7 MB/s; head and 3 versions `chunked`, 3 manifests, 174 refs; **+60 chunk rows on Lyve** (V2 and V3 about 2 each); used 300 MiB |
| GET `?versionId` × 3 | md5 match ×3 (33.9 / 56.5 / 45.8 MB/s) |
| copy of V1 onto the key (a manifest copy: the flag is on) | current md5 = V1; a 4th chunked version; used 400 MiB |
| DELETE → marker | used 400 MiB (every version keeps its bill) |
| DELETE every version | **used 0**; 4 manifests retired (`version_deleted`) |
| bucket deleted, flag unset | the bench tenant: 0 objects, 0 versions, 0 buckets, 0 active uploads, used 0 |

The 4 retired manifests are released by the collector after 24 h. Their chunks are then dedup GC's after its **7-day grace**: about 60 chunk rows on Lyve, now inside the 7,437 Lyve GCI rows (7,377 of them already marked from earlier work).

## Residuals

| # | Sev | Item | Where it goes |
|---|---|---|---|
| RVM-1 | note | The two-slot window and a rollback: the previous build 500s on an addressed chunked object and orphans its manifest on overwrite or delete. The orphan pass heals it. | none |
| RVM-2 | P3 | Per-version tags are not kept: GetObjectTagging `?versionId` answers the key's tags. | with tagging, if a client needs it |
| RVM-3 | P3 | A chunked PUT is slower than a whole PUT locally (161–165 vs 208–222 MB/s at 256 MiB). | `CHUNK_PUT_CONCURRENCY`, the PUT perf pass (R108-1) |
| RVM-4 | note | Object Lock stays per key: a retained key refuses every write and delete, so a locked chunked version cannot be displaced at all. | per-version retention |
| RVM-5 | P3 | A self-copy of a chunked object without REPLACE is InvalidRequest (as AWS); a whole self-copy is accepted (pre-existing). | the copy's next pass |
| RVM-6 | note | A version row a chunked write made before WP-VM-1 (only the race case) carries backend `''` and no address: no bytes retained. Prod: 0. | none |
| RVM-7 | P3 | The GDPR export lists versions without their 092 attributes. | Phase 43 inventory |
| RVM-8 | note | Cloudflare refuses a single PUT over 100 MB (413) before the origin sees all of it. Clients send multipart above that, and the origin reserves and compensates. | docs (client guidance) |

## Gates

- `make test-integration` (as `go test -race -p 4 -timeout 40m ./...` on a private DB, because the machine was loaded): every package ok, for #728 and #729.
- golangci-lint v2.14.0: 0. gosec with the workflow's flags: 0.
- Structure tests in that run: `TestLedger_NoQuotaStatementAfterACommit`, `TestLedger_QuotaStatementIsLast`, `TestLedger_LockOrder_*`, `TestEngineCalls_CarryTheTenant`, `TestExportSectionsCoverSchema`, the account schema walk, `TestChunkBlobCalls_GoThroughTheChunkStore`.

## Post-merge review (2026-10-17, the meta session)

Read: the product diffs of #726, #728 and #729 against the note, the vendor
semantics where a claim cites them, and prod read-only (journal and the
tables, 2026-10-14 21:16 → 2026-10-17 14:30 UTC). **Every claim holds. No P0
and no P1.** Three P3 corrections are recorded below; nothing needed code.

| # | Claim probed | How | Verdict |
|---|---|---|---|
| M1 | "retired, never released at the flip" on EVERY delete path (the pattern that found most earlier gaps: the same logic on the other entry points) | `DeleteObjectChunksTx` has exactly ONE caller left — the collector's `collectManifest`; DELETE, DeleteObjects, the marker, `?versionId`, the Suspended delete, the deletion runner and the export purge all reach `deleteHeadRowTx` / `deleteVersion`, which retire through `retireRowBytes`; `HandleDelete`'s old chunked branch releases nothing itself | holds |
| M2 | Kind-strict naming: a legacy manifest at `<key>` and a whole blob at `<key>` are never confused | `manifestNamedBy` / `versionKeepsManifest` require `is_chunked` / backend `chunked`; `blobNamedBy` / `versionKeepsBlob` exclude them; `usage.VersionRetained` matches address AND kind, NULL head row short-circuits | holds |
| M3 | The orphan pass cannot queue a live manifest | a manifest's rows appear only at the flip's COMMIT (no in-flight window); the shape check (`retiredBlobKey`) and the `COALESCE(storage_key, object_key)` head check exclude a legacy manifest at the key, a key that contains `@`, and a key that is itself address-shaped; `updated_at < now − 24 h`; the collector re-checks under the key's lock before releasing | holds |
| M4 | Lock order, collector ↔ `EraseRows` | both take `tenant_chunk_refs` rows, then `global_content_index` rows; the erase's `FOR UPDATE` on the tenant's refs serialises them with no cycle | holds |
| M5 | R61-5: the head row's bytes go unless an Enabled-era version names them | the null row becomes the marker BEFORE `deleteHeadRowTx` asks `versionKeeps*`, so only a non-null live version can keep them; a retained null version is retired and released in the same transaction, the bill last | holds |
| M6 | R61-8: the copy-source conditions follow AWS's two pairs | `evaluateCopySourceConditions`: If-Match present decides alone (an If-Unmodified-Since that fails is ignored, as AWS); If-None-Match present decides alone; otherwise the date headers | holds (AWS documents only the two combinations; the undocumented ones are decided the same way) |
| M7 | #726 P8 on the chunked Complete | `markCompleted` runs as `InTx` inside the flip's upsert: a lost Complete rolls the manifest and head row back, the dedup-hit chunks' refs are compensated, `answerConcurrentComplete` compares the head ETag | holds |
| M8 | #726 reaper orphan sweep cannot remove a live upload's staging | a directory is created after its row; the sweep removes only directories with no row AND a 1 h old mtime; a part landing in a directory whose row an Abort just removed is the one race, and it fails that part only | holds |
| M9 | Prod (read-only) | collector run 2026-10-16 02:33 UTC: `due 4, deleted 4` (the proof's manifests), one earlier `due 1, deleted 1`; now `retired_blobs` 0 rows, chunked head rows 0, chunked versions 0, `object_metadata` 0, `tenant_chunk_refs` 0; `global_content_index` 8,785 rows all at `ref_count 0` (dedup GC's 7-day grace; 793 on the dead iDrive key unsweepable, unchanged cause); all 18 jobs `ok`; the only `level:error` lines since the deploy are the three Cloudflare-cut proof PUTs (R108-9); `wasabi` probe failing (the dead partner pair, 194 warnings) — target-only, breaker closed, nothing routes to it | clean |

### Corrections (P3, recorded; no code)

| # | Item | Where it goes |
|---|---|---|
| RVM-1 (corrected) | "On the previous build an addressed chunked object is a 500, never wrong bytes" holds for an object chunked on the new build — but NOT for a chunked object written before 092 and overwritten chunked on the new slot during the ≤ 15 min two-slot window: the legacy manifest at `<key>` is retired (24 h grace), and the old slot's `GetObjectChunks(key)` serves ITS chunks under the new head row's size and ETag (a short or cut body, or old bytes under a new ETag). 087 had the same shape for whole blobs at `<key>`. Prod had 0 legacy chunked rows at the switch, so nothing was affected. **Pattern for every "the artefact gets an address" migration: the old slot's by-key reader does not find nothing — it finds the retired artefact.** The fix, if ever needed again, is to keep the old slot's reader from resolving a retired row (a `retired_blobs` check in the OLD build, shipped one deploy earlier) | **done** — measured with the real `e209950` binary (WP-P8 Part 0, 2026-10-17: 200 with the old bytes under the new ETag); owner decision: the checklist line in `docs/DEPLOY.md`, no guard |
| RVM-5 (extended) | A self-copy that names the CURRENT version (`?versionId=<the current id>`, or `null` on a key with no version row) without REPLACE is accepted (`src.VersionID != ""` reads as a restore); AWS answers InvalidRequest. A harmless rewrite: a new manifest address, the old one retired | the copy's next pass, with RVM-5 |
| RVM-9 | `latestVersion` adds up to two DB reads to every HEAD on a versioned bucket (the versioning status, the `is_latest` row). DB-only still (non-negotiable 2); a note for the HEAD latency budget | WP-R9-5 (the per-GET touch coalescing) |

### Handed to Stage 1a Prompt 8, Part 0

1. The two-slot window with a REAL previous binary (`e209950` built from git on a lab port): a legacy chunked object, overwritten chunked on the new slot, read on the old slot — record what the client sees (RVM-1 corrected), and the orphan pass healing the old slot's overwrite and delete of an addressed object.
2. A Suspended plain DELETE of a chunked object under Object Lock (retention and legal hold): 403 before the null row is touched, nothing retired.
3. The chunked copy into an Enabled bucket under the flag, then every version deleted: `tenant_quotas` = `BilledBytes` after each step, the collector releasing each manifest once.
4. `TestLedger_RandomSequence_*` with the Suspended delete and the manifest copy in its alphabet.
