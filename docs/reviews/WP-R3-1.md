# WP-R3-1: multipart through the chunk pipeline

Stage 1a, Prompt 3 (2026-10-13). Lane: Engine. Part 0 of the same prompt, the
post-merge review of WP-R2-1 C1/C2, is `docs/reviews/WP-R2-1.md` § "Post-merge
review (2026-10-13)" (PR #702, deployed `5880398`). Spec:
`docs/reviews/SYNTHESIS.md` (WP-R3-1 row, R3-07), `docs/reviews/R3-multipart-copy-batch.md:78-92`
(the multipart-vs-chunked table) and `:136`. The owner pulled this ahead of
WP-R6-1 because multipart was the one write path that stored plaintext in an
`sse_enabled` bucket. No migration (the last one stays 088; the next is 089).

## The bug, seen first

On `5880398` (red tests, before numbers in the PR):

- Every CompleteMultipartUpload wrote ONE whole blob, `is_chunked = FALSE`,
  `encryption_algorithm = ''`: plaintext, with or without a master key, into an
  `sse_enabled` bucket too. The same bytes in one PUT were chunked and
  chunk-encrypted, sealed, or refused 413/503. Every aws-cli / rclone / restic
  upload over 8 MiB is multipart, so in practice every large customer object
  was whole, plaintext, never deduplicated and never compressed.
- Local live proof on `origin/main`: two 200 MiB multipart uploads into an
  `sse_enabled` bucket on a server WITH the master key → `is_chunked = f`,
  `encryption_algorithm = ''`.
- A staged part that changed at rest (same size: the stat check passes) was
  stored under the ETag of the bytes the client sent (test: 200, GET served
  the corrupted bytes).
- A chunked object behind a public bucket's `/cdn` link answered 404 (the CDN
  asked the engine for a whole blob at `<key>`).

## What was built

**R1: one placement decision** (`internal/api/placement.go`,
`decidePlacement`). HandlePut and CompleteMultipartUpload both call it; a
CopyObject of a chunked source asks it for the destination. The rule: **a
Complete is placed exactly as a single PUT of the same bytes and class.**

| Placement | When (the PUT's rule, unchanged) |
|---|---|
| chunked (encrypted per chunk when the process has the chunk encryptor) | content index present, size > 64 MiB, class chunkable (`storageClassDisablesChunking`: not RESILIENT, GLACIER, DEEP_ARCHIVE, PUBLIC, SYNC), bucket not versioned (Enabled or Suspended), not region-pinned, `chunking` flag on, no SSE-C |
| **503 + Retry-After** | the bucket is `sse_enabled` and the process has no SSE service (SSE-C excepted) — at **CreateMultipartUpload** and again at **Complete** (the key or the bucket's setting can change between them) |
| whole, SSE-S3 sealed | encryption required (`sse_enabled`, or the PUT's `x-amz-server-side-encryption: AES256`), not chunk-encrypted, size ≤ `maxSSEObjectSize` (256 MiB) |
| **413 EntityTooLarge** | encryption required, the SSE service exists, neither chunk-encrypted nor sealable |
| whole plaintext | otherwise |

The bucket's `sse_enabled` is read strictly (a failed read is a 500). Every
refusal comes before `longOpBegin`, so it is never a keep-alive 200
(`TestComplete_SSEBucket_NoKey_Refused503BeforeKeepAlive`, `_Over256MiB_Refused413`
run with `instantCommit`). The PUT's inline gates (versioning, tier, region,
flag, `willChunkEncrypt`, the 503, the 413 guard) are gone from
`s3_engine_adapter.go`: there is one copy of the rule.

**Memory peak of a sealed Complete:** the `sseSealingReader` now takes the
exact size and reads into one buffer of it: plaintext + ciphertext = 2 × size,
**512 MiB at the 256 MiB cap**, per Complete in flight (the PUT's documented
exception to streaming; no concurrency cap, as for PUT).

**R2: the reader-driven chunked core** (`internal/api/chunked_write.go`,
`storeChunkedObject(ctx, chunkedWrite) (chunkedResult, error)`): no `http`
types. Chunker → dedup lookups → store pool → `commitBlobWrite` with a nil
releaser and `ReplaceObjectManifestTx` inside (it releases the displaced
manifest itself; the flip must not release it twice) → head row
(`upsertChunkedObjectHeadRow`: every attribute column, `is_chunked`, no
address, no key version, `backend_name` = the sentinel, floor `standard`).
Inputs a caller decides: the body, the declared size, the attributes, an
`ETag` function called after the body and before the install (the PUT's
whole-stream MD5 + Content-MD5 check; Complete's `-N` + the staged-part check),
`DetachInstall` (Complete: once every chunk is stored, the install runs to the
end), and an `InTx` step (Complete marks its upload completed in the same
transaction). The handlers keep Object Lock, `x-amz-version-id`, the status
line and their event (`s3:ObjectCreated:Put` / `:CompleteMultipartUpload`).
`TestChunkedWrites_GoThroughTheChunkedCore` (new) fails if any file but the
core starts the store pool, or any but the core and the manifest copy installs
a manifest.

**R3: Complete's branches.**

- **The staged parts as one verified stream** (`multipart_staged.go`,
  `stagedPartsReader`, replacing the `io.Pipe` + goroutine): one part file
  open at a time, each re-hashed and checked against its UploadPart ETag
  **the moment its last byte is read, with that byte**. A consumer that reads
  exactly Content-Length and never asks again (an S3 SDK) still gets the
  error, so nothing is committed. Whole or chunked, a mismatch is a 500
  (`InternalError` + a suggestion), the reservation released, the upload still
  `active`; Complete also checks `verified()` after a whole write (an orphan
  is queued if a backend took the bytes without reading to the end). Also
  closes the same class on the whole path (it never checked).
- **A cut** (shutdown, `LONG_OP_DRAIN_BOUND`) during the chunk stores: the
  body is a `cutReader`, the stores run on `backendCallCtx`, the core's
  deferred compensator gives back every reference the Complete took
  (`DecrementRef` on a cancellation-immune context). Chunk blobs stored for it
  that no row references are dedup GC's after its **7-day grace**
  (`TestComplete_Chunked_LongOpCut_CompensatesRefs`: previous object intact,
  reservation released, the chunk's `ref_count` 0, upload `active`).
- **The upload row's address:** a chunked Complete chooses none
  (`completeBlobAddress` only for a whole placement). An address an earlier,
  whole attempt recorded stays on the row and names nothing; that attempt's
  orphan was queued and `blobNamedBy` keeps it only while the upload is
  active, so the collector removes it after the completion.
- **Replay** (`completedUploadResult` compares the `-N` ETag the key holds;
  `reassertVersionRow` never runs on a chunked key, versioned buckets stay
  whole): `TestComplete_Chunked_ReplayReturnsSameResult`, nothing charged.
- **Quota:** reserved for the total at Complete on the class's floor, as
  before; a chunkable class is the `standard` floor (`chunkedObjectFloor`) by
  construction. GLACIER multipart stays whole on the attic
  (`TestComplete_GlacierStaysWholeOnTheAttic`, the floors add up).
- **Displaced objects:** whole → chunked retires the blob (`overwritten`);
  chunked → whole releases the manifest (`TestComplete_Chunked_OverWholeObject_RetiresBlob`,
  `_WholeOverChunked_ReleasesManifest`).

**R4: what a chunked row means elsewhere.**

| Reader | A `-N` chunked row |
|---|---|
| GET / Range | the chunked GET (parfetch read-ahead, hedged; every chunk verified before its bytes go out). Exact bytes and ranges (tests, live) |
| `parallel_get` | does not apply (it is the whole-object ranged read); the chunked GET is already parallel. Measured on prod below |
| HEAD | the head row (size, `-N` ETag, `AES256` for `AES256-CE` rows) |
| CopyObject, chunked source | a manifest copy **only where a PUT of the bytes would chunk** (`decidePlacement` on the destination), reporting the **source's ETag** (no byte is read; chunk hashes are SHA-256, not MD5; a `-N` stays `-N`, an AWS deviation for a one-request copy); its head row now goes through `upsertChunkedObjectHeadRow` (tags by the tagging directive, every attribute: an overwrite used to keep the displaced object's cache headers and tags). Into a **versioned, region-pinned or non-chunkable** destination the source is read through the content index (`openChunkedObject`) and **re-stored whole** there, with a fresh MD5 ETag, the destination's floor and seal, and a version row in a versioned bucket. Before: 501 (versioned, pinned) or a manifest on the primary billed downstairs (archive tier; Part 0 PM-5) |
| **CDN** (`/cdn/...`) | **built here** (`chunked_read.go`): the CDN reads a chunked row through the content index, Range included (`chunkedObjectReader` is a ReadSeeker: a range fetches only the chunks it spans); every chunk backend is asked before the status line (503 when one cannot be); the CDN's own headers (`public` cache, its disposition: a `response-content-disposition` query cannot override it). Before: 404 for a row that exists. A chunk failing after the first byte cuts the connection (the CDN has no first-chunk prefetch like the S3 GET's: no clean 500, never bad bytes) |
| Smart demotion | skips chunked rows (`NOT is_chunked`): large Standard objects are now chunked, so the Smart tier can no longer move them to tape. The `smart_demotion` flag is OFF; the cost is the Smart tier's COGS saving on large objects, to weigh when it is turned on |
| Vault parity | vault-floor objects only; a chunked object is never vault-floor (asserted) |
| `routing_truth` | chunked rows are checked chunk by chunk; the head row's name is skipped |
| erasure / deletion runner | manifest released with the head row; the `_global` chunk container is never swept (`chunk_blobs_left`) |
| export | metadata rows only |
| `key_reencrypt` | chunks: the chunk pass; a sealed whole multipart object (`-N` ETag): the whole pass, which skips the MD5 check for `-N` ETags |
| `dedup_gc` | a chunk whose refs reach 0 after its 7-day grace |
| dashboard | lists the row (class from the floor: STANDARD); no backend shown |

**The `backend_name` sentinel** (the rule WP-R7-5 left here): **every chunked
head row records `chunked`** (`chunkedBackendSentinel`): the PUT (it used to
write the first stored chunk's backend, or `chunked` when every chunk was a
dedup hit), the Complete and the manifest copy (it wrote `''`). A chunked
version row records no backend. Existing rows keep what they have and read
exactly the same: every reader already skips a chunked row's name (routing
truth, the boot check, GET/CDN hints, demotion, parity, re-encrypt).

**R5: docs.** `internal/api/CLAUDE.md` (s3_multipart, the copy bullet, the
SSE section), `s3_long_op.go` and `s3_multipart.go` comments, `docs/ENCRYPTION.md`
(what is encrypted, the shred section), the root `CLAUDE.md` dedup-index line,
`docs/API.md` (the encryption paragraph and its 503/413 rule, large uploads,
the CDN line, the CopyObject line; two stale claims corrected: "without the
key `sse_enabled` has no effect" — it has been a 503 since WP-R8-1 A1 — and
"SSE-encrypted objects are not served by the CDN"), the changelog. `llms.txt`
says nothing that changes. **Customer copy sites that change (WP-R14-6, not
edited):** `internal/api/docs_faq.md:121-131` (the dedup privacy note now
covers multipart objects; "SSE-S3 not yet enabled" changes at the ceremony),
the landing's "SSE-C and client-side encryption" line
(`landing.src.html:641`) at the ceremony.

## Tests

Red first. "Before" = these tests on `3e4b95e` (Part 0) with the
`Server.chunkingThreshold` knob only (two stub symbols in a test file, never
committed).

| Test | Before | After |
|------|--------|-------|
| `TestComplete_ChunkableClass_StoresChunked_KeepsMultipartETag` | `is_chunked` false, `storage_key` `big.bin@567p…`, backend `fixed`, 0 chunk refs | pass |
| `TestComplete_SSEBucket_WithKey_NoPlaintextOnBackend` (chunked / small sealed / versioned sealed) | `encryption_algorithm` '' and the plaintext marker on the backend in all three | pass |
| `TestComplete_SSEBucket_NoKey_Refused503BeforeKeepAlive` | Create 200, Complete 200 | pass |
| `TestComplete_SSEBucket_NonChunkableOver256MiB_Refused413` | 200, stored plaintext | pass |
| `TestPlacement_PutAndCompleteAgree` (11 cases) | 8 disagreed (e.g. PUT `{200 chunked AES256-CE}` vs Complete `{200 whole ''}`; PUT 503 vs Complete 200) | pass |
| `TestComplete_StagedPartCorrupted_RefusedNothingCommitted` (chunked / whole) | 200, GET served the corrupted bytes | pass |
| `TestComplete_Chunked_ReplayReturnsSameResult` | not chunked | pass |
| `TestComplete_Chunked_OverWholeObject_RetiresBlob` / `_WholeOverChunked_ReleasesManifest` | not chunked | pass |
| `TestComplete_Chunked_LongOpCut_CompensatesRefs` | 200 (whole) | pass |
| `TestComplete_VersionedBucket_StaysWhole`, `TestComplete_GlacierStaysWholeOnTheAttic` | pass (pinned, unchanged) | pass |
| `TestComplete_Chunked_Dedup` | 2 blobs for the same bytes | 1 |
| `TestCDN_ChunkedPublicObject_Served` | 404 | pass |
| `TestCopy_ChunkedSource_IntoNonChunkableDestination_StoredWhole` | manifest / 501 | pass |
| `TestChunkedWrites_GoThroughTheChunkedCore` | — | pass |

**Assertions whose meaning changed:**
`TestChunkedCopy_VersionedDestinationRefused` became
`TestChunkedCopy_VersionedDestination_StoredWholeWithItsVersion` (501 → 200,
whole, a version that keeps its bytes); `TestChunkedCopy_RegionPinnedDestination`'s
"driver present" case (501 → 200, stored whole in the region's driver);
`setupChunkedCopyFixture` sets the server's threshold to the adapter's 1 KiB,
and `TestPlainCopyOverChunkedDest_ReleasesManifest`'s "plain" source is now
900 B (2 KiB is chunked at that threshold). Every other multipart, long-op,
quota, floor, strict-target, Smart-ledger, refused-name and blob-writer suite
is unchanged and green.

## Live proof (local, 2026-10-13)

This branch built, `STORAGE_MODE=local`, a private DB `vaultaire_live_r31`
through 088, a random lab key in `$CREDENTIALS_DIRECTORY/encryption_master_key`,
aws-cli with its default multipart (8 MiB parts), a registered tenant with a
house (standard + vault floors). New buckets were `sse_enabled` by default.
Test files: random bytes with the marker `R31-PLAINTEXT-MARKER-…` at every MiB.

| Step | Result |
|---|---|
| 200 MiB `aws s3 cp` into `enc` (sse) | head row `is_chunked t`, `AES256-CE`, backend `chunked`, floor standard, no address, ETag `abaf5d99…-25` = aws-cli's (computed locally from the 25 part MD5s); 102 chunks. `grep -rl` of the marker over `DATA_PATH` **0**, over `/tmp/vaultaire-multipart` 0. GET sha256 = the file's; `bytes=100000000-100999999` equal. HEAD: `-25`, 209715200, `AES256`. Usage 209,715,200 |
| the same file as `big2` | `DATA_PATH` 205,008 KiB before and after (**no new physical bytes**), dedup_ratio 0; GCI 102 rows, ref_count sum 204; usage 419,430,400 (logical, per object) |
| 20 MiB multipart into `enc` | whole, `ML-KEM-768+AES-256-GCM` key v1, ETag `43eeace7…-3`, blob `mid@gd2s9xjr…` 20,972,641 B (= 20 MiB + 1,121 B of seal), markers 0, GET equal; usage 440,401,920 |
| 12 MiB GLACIER multipart | whole, sealed, floor **vault**, ETag `-2`; floors standard 440,401,920 / vault 12,582,912, total 452,984,832 |
| 300 MiB into `res` (sse, tier resilient) | **413 EntityTooLarge** at Complete; nothing stored, usage unchanged, aws-cli aborted the upload, markers 0 |
| 200 MiB with the key unset (restart without the credential) | **503 at CreateMultipartUpload** ("multipart upload into an encrypted bucket refused…" in the log); no part staged |
| Complete wall time, 200 MiB fresh random, 25 parts (`s3api complete-multipart-upload` timed alone) | this branch **1.34–1.40 s** (chunked + compressed + encrypted; 1.26–1.27 s when the content is a dedup hit); `origin/main` **0.62–0.64 s**, stored whole and **plaintext** in the same sse bucket |

## Prod proof

2026-10-13, after the deploy of #703 (`/version` `08d4f24892d0`, build
12:53:36 UTC). The bench tenant `tenant-14a623b16b3f7012` (no buckets, usage
0 before), aws-cli on the SLC box (the box's config: 64 MiB parts, 8
concurrent), bucket `wpr31-proof`, deleted afterwards.

**Read-only counts before the deploy:** chunked head rows **0** (prod had never
chunked a multipart object); `global_content_index` 2,500 rows, all at
ref_count 0 (the 10-13 wipe's chunks, waiting for GC); `job_runs` `dedup_gc`
last `ok` 02:44 UTC ("149 chunk(s) not swept … they stay candidates").

| Step | Result |
|---|---|
| 1 GiB random, `aws s3 cp` via **stored.ge** | **119.3 MB/s** (9.0 s); head row `is_chunked t`, ETag `bc570d06…-16` = the md5-of-part-md5s of the file at 64 MiB parts (what this aws-cli computes), backend `chunked`, floor `standard`, no address, encryption `''` (prod has no key) |
| GET via stored.ge / via s3.stored.ge | **186.9 / 236.1 MB/s**, sha256 equal both (`fc8ddf00…`); `bytes=500000000-500999999` equal |
| the same bytes as a second key via s3.stored.ge | **330.1 MB/s**: every chunk a dedup hit. GCI 2,500 → **3,026** rows (+526 chunks for the first object, **0** for the second); the tenant's refs 1,052 = 2 × 526; usage 2,147,483,648 (logical, per object) |
| 4 more fresh 1 GiB objects (new random bytes each) | up via s3.stored.ge **161 / 167 MB/s**, via stored.ge **153 / 150 MB/s**; down via s3.stored.ge **228 / 252 MB/s**, via stored.ge **187 / 168 MB/s**; every download byte-equal |

**Next to the 2026-10-12 tiers bench** (`bench-results/TIERS-AND-LAYERS-2026-10-12.md`,
tierbench, Standard on Lyve; the clients differ, so the comparison is
approximate):

| 1 GiB multipart | 2026-10-12 (stored whole) | now (chunked) |
|---|---|---|
| upload via origin | 241 MB/s | 161–167 MB/s |
| upload via Cloudflare | 183 MB/s | 119–153 MB/s |
| download via origin | 38 MB/s single stream; 135 with `parallel_get` (§8.7, the bench tenant has it ON) | **228–252 MB/s** |
| download via Cloudflare | 70; 93 with `parallel_get` | **168–187 MB/s** |

Downloads of multipart objects are 1.7–2× the `parallel_get` numbers (the
chunked GET's hedged read-ahead). **Uploads of fresh content are ~30 % slower:**
Complete now stores ~526 chunks through the 8-worker store pool
(`CHUNK_PUT_CONCURRENCY`) instead of one streaming PUT; re-uploads of known
bytes are faster than before (330 MB/s). The knob to try first is
`CHUNK_PUT_CONCURRENCY` 16 (an owner `.env` change, measured with the same
script); not changed here.

**`/health` and the canary through the two-slot switch:** unchanged —
`degraded`, reason naming exactly the 11 iDrive probes, 7/18 healthy, the Lyve
primary's breaker closed (right after boot the count read 14/18 while the
probes had not all run); 0 error lines in the 25 minutes around the proof;
active slot :8000.

**Cleanup:** every object and the bucket deleted through stored.ge; the bench
tenant back to 0 objects, 0 buckets, usage 0, 0 chunk refs; the box's temp
files removed. `global_content_index` holds 5,076 rows at ref_count 0, whose
blobs `dedup_gc` deletes from Lyve after its 7-day grace.

## Gates

- `make test-integration` on a fresh private DB through 088: every package `ok` except one `internal/api` test whose precondition this WP changes on purpose (`TestRoutingTruth_ChunkedRowsAreNeverUnknownBackendRows` assumed a manifest copy stamps `''`; it now asserts the sentinel and plants a pre-WP `''` row); after the fix `go test -race ./internal/api/` ok (192 s). That run includes `TestSQLLiteralsMatchSchema`, `TestExportSectionsCoverSchema`, `TestSweepBucketSourcesCoverSchema`, the account schema walk, `TestChunkBlobCalls_GoThroughTheChunkStore`, `TestWholeObjectReads_GoThroughTheBlobAddress`, `TestChunkedWrites_GoThroughTheChunkedCore` (new) and `job_rules_test.go`.
- golangci-lint v2.14.0: 0 issues. gosec@latest with the Makefile's flags: 0 issues.
- No migration (088 stays the last; next 089).

## Decisions (to report; not built here)

1. **The ceremony gate needs WP-R8-5 (streaming SSE) first. Recommended.**
   With this WP, a multipart upload over 256 MiB into an `sse_enabled` bucket
   whose placement cannot chunk is 413. After the key, every new bucket
   defaults `sse_enabled`. The placements that cannot chunk are exactly where
   large objects live: GLACIER / DEEP_ARCHIVE (the Vault product), RESILIENT,
   PUBLIC, SYNC, region pins, and **every versioned bucket, so every Object
   Lock / WORM backup bucket** (Object Lock requires versioning: Veeam,
   restic and rclone with lock). The ceremony as planned would turn "stored
   plaintext" into "413" for the large archives Vault sells. The alternative,
   defaulting those buckets to `sse_enabled = false`, leaves the product that
   holds the most data plaintext at rest, contradicts "encrypted by default"
   (F-14, the privacy north star), and is fragile: the class is per request,
   a STANDARD bucket takes a GLACIER-classed object. WP-R8-5 is framed
   streaming AEAD (STREAM-style segments, e.g. 1 MiB, each with its own tag and
   the key version in a header), ranged reads by segment, whole-object
   re-encryption streaming, and WP-R3-3 (multipart SSE) becomes its use in
   Complete's whole branch. Size M. Order: **WP-R8-5 + WP-R3-3 → the F-14
   ceremony**. Persisting `x-amz-server-side-encryption` from
   CreateMultipartUpload (a column, 089) belongs in the same WP (below).
2. **Version-aware manifests** (versioned buckets chunked; Part 0's PM-3):
   the manifest becomes addressable like a blob. A `manifest_id` on
   `tenant_chunk_refs` / `object_metadata`, the head row's and the version row's
   `storage_key` naming it for a chunked object, the flip retiring a displaced
   manifest instead of releasing it (released when the last row naming it
   goes, by the collector, refs decremented), GET/HEAD/DELETE `?versionId`
   on chunked versions, `anchorNullVersion` for a chunked head, the erasure
   and export walks, the copy's manifest branch. Migration 089-or-later,
   additive; old rows (no `manifest_id`) read by key as today. Size **L**
   (≈ 1–1.5 weeks with the review).
3. **CDN chunked reads: built here** (`chunked_read.go`); WP-R2-2 keeps the
   rest of the CDN list.
4. **Per-part reservations (R3-14 / WP-R10-8): not next.** Order after this:
   **WP-R8-5 (+ WP-R3-3)** because the ceremony waits on it, then **WP-R6-1**
   (location-authoritative reads, the two #677 bypasses), then WP-R10-8. Until
   then `MULTIPART_MAX_UPLOAD_BYTES` (50 GiB per upload) and the 48 h reaper
   bound the unbilled staging.
5. **Carried unchanged:** the 11 iDrive criticals (prod on Lyve; keys
   object-dead since 2026-10-11), `wasabi` target-only, the two #677 bypasses
   (→ WP-R6-1), the dedicated service user + `LoadCredentialEncrypted=`
   (F-14 prerequisite; `deploy/systemd/vaultaire@.service` still `User=user1`),
   tenant f429's 2 MB on iDrive (deferred, un-scheduled), the Sync.com
   sub-processor sentence, gey48, the D-12 drop.

## The invariant

**With a master key, no byte of a multipart upload reaches a storage backend
unencrypted for an `sse_enabled` bucket**: it is chunk-encrypted, sealed, or
refused (413 when it can be neither, 503 without the key, at Create and at
Complete). The same holds for PUT, CopyObject (a whole source sealed or 413;
a chunked plaintext source into a chunkable sse bucket 501; a chunked source
into a non-chunkable one re-stored whole and sealed or 413) and generated
objects (sealed or refused). Named exceptions, none a storage backend write of
a completed object:

- **Staged parts** of an open multipart upload are plaintext on the box's own
  disk (`/tmp/vaultaire-multipart/<upload>/part-NNNNN`) between UploadPart and
  Complete/Abort/the reaper (≤ 48 h). WP-R3-5 moves the directory; encrypting
  staged parts belongs with WP-R8-5.
- **A direct upload presigned before the bucket was switched to
  `sse_enabled`** (15-min presign TTL): the client puts plaintext straight
  on R2; the complete removes it, and an upload never completed stays until
  the erasure sweep. The presign refuses an `sse_enabled` bucket.
- Objects written before the key (prod: wiped 2026-10-13 except tenant f429).

## Not done / residuals

- `x-amz-server-side-encryption: AES256` on CreateMultipartUpload into a bucket
  that is NOT `sse_enabled` is not recorded (no column), so such an upload is
  placed as if the header were absent (plaintext), where a PUT with the header
  is sealed. After the ceremony new buckets are `sse_enabled`; the column
  (089) goes with WP-R8-5.
- `x-amz-tagging` on CreateMultipartUpload is not persisted (pre-existing).
- A whole source copied into a chunkable destination stays whole (no dedup
  for such copies); never plaintext into an sse bucket.
- A manifest copy reports the source's ETag (above).
- A sealed Complete holds 2 × size in memory (512 MiB at the cap) per
  Complete in flight, with no concurrency cap (as PUT).
- Orphan chunk blobs of a cut or failed chunked Complete wait for dedup GC's
  7-day grace.
- The CDN cuts the connection on a chunk failure (no first-chunk prefetch).
- Two-slot window of this deploy: for ≤ 15 min the old slot writes multipart
  whole (plaintext; prod has no key and 0 sse buckets) and serves a chunked
  public object 404; both slots read both shapes.

# Post-merge review (2026-10-13)

By the plan driver, the same day, on `dcb7462`: the non-test diffs of #702
and #703 read against the sections above and against Part 0's table in
`docs/reviews/WP-R2-1.md`; CI on `main` green for `5880398`, `08d4f24` and
`dcb7462`; no `Co-Authored-By` trailer on either PR. **Every claim holds; no
P0/P1.** Checked and found right: `deleteBehindMarker` calls `retireBlobNow`
whatever `KeptByVersion` says, but `retireBlob` re-checks `blobStillNamed`
inside its transaction, so a blob a version keeps is never queued; the batch
checks Object Lock on both versioned flavours before the row change;
`anchorNullVersion` can only fire for a pre-versioning object (a Suspended
write writes a `null` version row, so "no version row at all" means exactly
that); every refusal of `decidePlacement` precedes `longOpBegin`;
`stagedPartsReader` reports a corrupt part with the part's last bytes, so a
consumer that reads exactly Content-Length still fails.

| # | Sev | Where | Scenario | Fix or hand-off |
|---|-----|-------|----------|-----------------|
| PM-1 | **P2** | `s3_multipart.go:787-795`, `s3_copy.go:875` (`sseSealingReader`), `s3_engine_adapter.go:1060` | A sealed Complete holds plaintext + ciphertext (512 MiB at the cap) per request with no bound — the PUT's documented exception, but multipart is the path every client takes, so one authenticated tenant with N concurrent 256 MiB Completes holds N × 512 MiB. | **WP-R8-5** (streaming SSE: the buffer goes; Prompt 4). WP-R2-6 keeps the chunked pool's bound. |
| PM-2 | P3 | `s3_copy.go:343` | A whole plaintext source > 256 MiB copied into a chunkable `sse_enabled` bucket is 413, where a PUT of the same bytes chunk-encrypts: the whole-source copy does not ask `decidePlacement`. | WP-R8-5 (the cap goes). |
| PM-3 | P3 | `s3_multipart.go:120` (Create), `placement.go` (`SSERequested`) | `x-amz-server-side-encryption: AES256` on CreateMultipartUpload is not persisted (the note's own residual), so Complete places such an upload into a non-sse bucket as plaintext where the PUT seals. | WP-R8-5: column 089 `multipart_uploads.sse_requested`. |
| PM-4 | P3, ceremony | `bucket_create.go:113` | Every new bucket is `sse_enabled` with a key, one later made public too: PUBLIC-class objects on R2 will be sealed and `/cdn` opens them on every hit (`cdn.go:356`, whole-buffer today). Nothing at the edge reads R2 raw (Sippy pulls through the S3 API), so it is a cost, not a break. | WP-R8-5 A5: decide "seal public objects" (keeps the one-sentence invariant) or not. |
| PM-5 | P3, ceremony | `direct_upload.go:98` | A direct upload refuses an `sse_enabled` bucket (the client puts plaintext on R2; the server never sees it), so after the ceremony every new bucket refuses direct uploads. | Product consequence for the F-14 ceremony note; not a code change. |
| PM-6 | note | `smart_demotion` | Large Standard objects are now chunked and the Smart tier skips chunked rows: its COGS saving on large objects is gone while the flag is OFF; to weigh when it is turned on (the note says so). | Listed. |

The next prompt (Stage 1a Prompt 4) is **WP-R8-5 streaming SSE** (= K1, =
WP-R3-3): decision 1 above, recommended by the worker and accepted — the
ceremony waits for it. Its Part 0 probes four things this review did not
drive at the handlers: two concurrent chunked Completes of one upload, a
chunked Complete racing the reaper, a sealed Complete's version row, and the
response header of a PUT asking `AES256` on a keyless process.

## Worker probe (WP-R8-5 Part 0, 2026-10-13)

By the WP-R8-5 worker on `59e2769`, at the handlers on a private database
through 088 (`internal/api/wp_r3_1_probe_test.go`, kept as regression tests).
**No P0/P1; nothing to fix in its own PR.**

| # | Probe | Result | Sev | Hand-off |
|---|-------|--------|-----|----------|
| W-1 | Two Completes of one chunked upload, both reading the last part when the first commits (a barrier in `stagedPartReadHook`) | Both 200 (the loser had its parts open; unlinking the staging directory does not cut an open file); `tenant_chunk_refs` for the key = one object's chunks, the sum of their GCI `ref_count` = the row count (1 each), usage = the size once, GET equal | holds | — |
| W-1b | The same without the barrier, 40 runs | 40 × `[200 200]`, refs and bill exact every time; a third Complete afterwards is a replay (200, usage unchanged) | holds | — |
| W-2 | The reaper's abort (`status = 'aborted'` + `RemoveAll`) between parts 2 and 3 of a chunked Complete over an existing object | 500 `InternalError` (`errStagedPartCorrupt`); the previous object still served, usage = the previous object only (reservation released), 0 `tenant_chunk_refs` for the tenant (compensated), the row stays `aborted`, a retry is 404 `NoSuchUpload` | holds | — |
| W-3 | A sealed Complete in an Enabled `sse_enabled` bucket, then an overwrite | The version row carries `ML-KEM-768+AES-256-GCM` and `encryption_key_version` 1; 0 plaintext markers on the backend; `GET ?versionId=<old>` equal with `x-amz-server-side-encryption: AES256`, `HEAD ?versionId` reports `AES256` | holds | — |
| W-4 | PUT with `x-amz-server-side-encryption: AES256` on a process with no master key, into a bucket that is not `sse_enabled`; CreateMultipartUpload with the same header | PUT 200, the object stored **plaintext** (`encryption_algorithm` '', markers on the backend); neither the PUT, the HEAD nor the Create echoes `x-amz-server-side-encryption` — so no lie in a header, but a client that asked for encryption is told nothing | **P2** | WP-R8-5 Part A: `decidePlacement` refuses `SSERequested` without an SSE service exactly like an `sse_enabled` bucket (503 + Retry-After), and so does Create (`TestPut_SSEHeader_NoKey_Refused503`) |

The Part 0 grep list (every reader of `SSEOverheadBytes`, `MaxEncryptableSize`,
`maxSSEObjectSize`, `EncryptBytes`, `DecryptBytes`, `SSECEncrypt`/`SSECDecrypt`,
`sseSealingReader`, `openSSEBlob`; `encryption_algorithm` readers are SQL
columns and listed in `docs/reviews/WP-R8-5.md`) is Part A's work list:

- `internal/crypto/sse_s3.go:26-28,58,85,100,116`, `ssec.go:17,22,49`;
- `internal/api/placement.go:25,45,157,160` (the seal-or-413 branch);
- `s3_engine_adapter.go:604` (SSE-C GET), `:637` (SSE-S3 GET), `:1009,1036` (SSE-C PUT), `:1075` (SSE-S3 PUT);
- `s3_multipart.go:784-796` (Complete's `sseSealingReader`);
- `s3_copy.go:343` (the 413), `:478-488` (the copy's sealer), `:870-940` (`sseSealingReader` itself);
- `background_put.go:56-58,99,110,234` (`maxSSEObjectSize`, generated objects);
- `cdn.go:122` (the "row inconsistent" cap), `:290`, `:354-361` (`openSSEBlob`);
- `key_reencrypt_whole.go:12,227,237,249`.
