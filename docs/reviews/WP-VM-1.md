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
