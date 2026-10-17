# WP-R8-5: streaming SSE (keystone K1, = WP-R3-3)

Stage 1a Prompt 4, 2026-10-13. The last code item before the F-14
master-key ceremony: with it no placement refuses an object for its size,
so the ceremony no longer turns large Vault, Object Lock, RESILIENT,
PUBLIC, SYNC and region-pinned uploads into 413. Spec: `SYNTHESIS.md`
WP-R8-5 / WP-R3-3 / WP-R2-6, `docs/reviews/R8-crypto-dedup.md` R8-12.

PRs: **#706** (Part 0, the worker probe of WP-R3-1, no P0/P1), **Part A**
(the codec + SSE-S3 everywhere, migration 089), **Part B** (SSE-C on the
same codec).

## The bug, seen first

One AES-GCM over the whole object (`0x02`) forced a buffer on every path:
the PUT `io.ReadAll`'d the body, Complete and CopyObject held the
plaintext and the ciphertext (2 × size, 512 MiB at the cap, per request,
no bound — the WP-R3-1 post-merge P2), the GET read the whole sealed blob
before its status line, `/cdn` too, and `key_reencrypt` read whole blobs.
Hence the 256 MiB cap (`crypto.MaxEncryptableSize`) and its refusals.
Measured on `origin/main` (`869bef6`) at the handlers, before any change:

| Case | Before |
|------|--------|
| PUT GLACIER into an `sse_enabled` versioned bucket over the cap (lowered to 4 KiB) | **413** |
| CompleteMultipartUpload, versioned `sse_enabled`, over the cap | **413** |
| `Range: bytes=100-200` of a sealed object | 206 after **1 whole-blob GET** (0 ranged reads), decrypted whole |
| CopyObject of a sealed (SSE-S3) source | **501** |
| CreateMultipartUpload with `x-amz-server-side-encryption: AES256` into a plain bucket, then Complete | 200, no response header, **stored plaintext** (PM-3) |
| PUT with `AES256` on a process with no master key | 200, **stored plaintext** (Part 0 W-4) |
| vault parity of a sealed vault object | `partial`, `encode parity: the object is longer than its recorded 307200 bytes` — **no sealed vault object ever had a parity copy** |
| Smart demotion of a sealed object | the cold copy declared **3,000 bytes for a 4,121-byte blob** (an S3-class cold backend keeps the first 3,000: an object that no longer opens) |

After the ceremony every new bucket is `sse_enabled`: every row of that
table would have been the normal case.

## What was built

**The codec** (`internal/crypto/stream.go`): the STREAM construction of
Hoang, Reyhanitabar, Rogaway and Vizár (CRYPTO 2015) in the nonce-prefix
shape of Tink's `AES-GCM-HKDF-STREAMING` and age's payload. Blob format
**`0x03`**:

```
header    = [0x03][kind u8][kind material][segment_size u32 BE][nonce_prefix 7]
kind 0x01 SSE-S3: [key_version u32 BE][ML-KEM-768 ct 1088]   (header 1105 bytes)
kind 0x02 SSE-C:  [salt 16]                                   (header 29 bytes)
segment_i = AES-256-GCM(DEK, nonce_prefix ‖ i u32 BE ‖ last u8, aad = header, plaintext_i)
```

1 MiB segments (`StreamSegmentSize`, written into each header); n = max(1,
ceil(size / S)) — an empty plaintext is one empty final segment, never a
bare header; stored length = `len(header) + size + 16n`, a pure function of
the size (the backend PUT carries it). DEK: SSE-S3 = HKDF-SHA256(the
ML-KEM shared secret of a fresh encapsulation, info
`vaultaire-sse-stream-v3` ‖ header); SSE-C = HKDF-SHA256(the customer key,
the blob's random salt, `vaultaire-sse-c-stream-v3` ‖ header). The file
comment argues each tamper class and `stream_test.go` tests each one
(modified segment, modified tag, reordered, dropped, duplicated, truncated
at a boundary / inside the final segment / to the header, extended by a byte
and by a partial segment, every header field, an absurd segment size refused
before any allocation): the opener returns exactly the plaintext of the
verified segments before the bad one, never a byte of it. Its constructors
verify the header and the FIRST segment before they return, so a caller's
status line always follows a verified segment. `PlanStreamRange` maps a
plaintext range onto the segments it spans. At most one plaintext and one
ciphertext segment per stream (`TestStream_OneGiB_BoundedMemory`: sealing
and opening 1 GiB allocates < 32 MiB in total, with no per-byte growth).

**SSE-S3 everywhere** (Part A, #708; `internal/api/sse_stream.go`):

| Path | Before | After |
|---|---|---|
| PUT (`HandlePut`) | `io.ReadAll` + one GCM | `SSEService.Seal` over the body, MD5 tee before it, backend PUT of `SealedLength(size)`; a short/long body fails the sealer's read (engine: `ErrCallerAborted` → 400, no breaker charge); a Content-MD5 mismatch retires the written blob |
| CompleteMultipartUpload whole branch (**WP-R3-3**) | `sseSealingReader`: plaintext + ciphertext, 2 × size | `newSSEWriteSealer` over the staged parts; the tenant key fetched before the keep-alive can commit |
| CopyObject | 413 over 256 MiB; **501 for any sealed source** | a plaintext source sealed at any size; a sealed source opened as it streams and sealed again for its destination (SSE-C sources: still 501); a source with no recorded size into an `sse_enabled` bucket must be empty (`exactSizeReader{want: 0}`) |
| generated objects (export, access log, inventory) | read the spool into memory; `too_large_for_sse` refusal | sealed from the spool file at any size (`refusedSSEUnavailable` stays); the dashboard's "export too large to encrypt" state is gone |
| `key_reencrypt` whole pass | `io.ReadAll` (≤ 256 MiB + overhead) | header check on a peek, open → MD5 tee → seal under the newest version → PUT, streamed; a `0x01`/`0x02` blob is re-sealed as `0x03` by the same pass; ETag checked after the write (a mismatch queues the new blob as an orphan) |
| S3 GET | the whole blob read and decrypted before the status line; Range sliced the decrypted buffer | the status line after a verified first segment; a later failure cuts the connection (`TestGet_SealedObject_SegmentTamperedAfterStatusLine_CutsConnection`: exactly the verified segments went out); a Range reads the header (one ranged GET of 1105 bytes) and the segments it spans (one more) — `TestGet_SealedObject_RangeBySegment` counts the backend reads |
| `/cdn` (`openSSEBlob`) | whole-buffer | streaming, Range by segment (`writeRangeBody`) |
| vault parity | layout sized by the PLAINTEXT, every sealed vault object failed ("longer than its recorded bytes") | shards over the STORED bytes (`storedBlobLength`); the GET's parity fallback rebuilds the blob and opens it (`TestVaultParity_SealedObject_RebuiltThenOpened`) |
| Smart demotion / promotion | copied a sealed blob declaring the plaintext size (an S3 backend kept the first `size` bytes) | the blob's stored length (`TestSmartDemotion_SealedObject_CopiedAtItsStoredLength`) |
| placement | `placeWholeSealed` ≤ 256 MiB, 413 above; `AES256` header without a key → plaintext 200 | `placeWholeSealed` at any size > 0; the header without a key → **503** like an `sse_enabled` bucket (Part 0 W-4) |
| CreateMultipartUpload | the `AES256` header dropped (PM-3) | kept on `multipart_uploads.sse_requested` (**migration 089**), placed at Complete as on a PUT; Create and Complete answer `x-amz-server-side-encryption: AES256` when the object is sealed or chunk-encrypted (a Complete's headers are delivered when it answers before the keep-alive commits) |

`crypto.MaxEncryptableSize`, `maxSSEObjectSize`, `refuseTooLargeToSeal`,
`sseSealingReader`, `errCopyTooLargeForSSE`, `openSSEBlob`,
`errGeneratedTooLargeForSSE` and `SSEService.EncryptBytes` are gone
(`TestSSEPaths_NeverBufferTheObject` checks the names and walks the source).
`vaultaire_sse_blobs_opened_total{format}` (0x01/0x02/0x03, on the server's
registry — the live proof found it first on the default registry, which
`/metrics` does not serve; fixed with `TestSSEBlobsOpened_OnTheServersRegistry`).

**SSE-C** (Part B, #709): kind `0x02` of the same codec; one customer key
over many objects never repeats a (key, nonce) — each blob has its own salt,
hence its own DEK. The row decides the format: new value
**`AES256-SSE-C-v3`**; `AES256-SSE-C` rows are the old `[nonce 12][ct+tag]`
format, opened whole by the bounded `SSECOpenLegacy`. Telling them apart by
the first byte would be wrong one time in 256 (an old blob starts with a
random nonce byte), so the row is the discriminator
(`TestSSEC_MagicCollision_OldBlobStartingWith0x03`: an old blob whose nonce
starts `0x03 0x02`, the streaming SSE-C magic, opens as the old format,
whole and ranged). Every check is `crypto.IsSSEC`. The PUT seals as it
streams (the key zeroed as soon as the DEK is derived); the GET shares the
sealed branch with SSE-S3 (`blobOpener`): `ErrSSECKeyMismatch` → 403 at the
first segment, before the status line, as before. Multipart with SSE-C
stays 501 — it is **WP-SSEC-1**'s (the key on Create, every UploadPart and
Complete); an SSE-C copy source stays 501.

**Which path a Vault object takes:** a `vault`-floor object lives on Geyser
(tape). A whole GET is Geyser's 8-stream ranged `Get`; a Range of a sealed
object is two `engine.GetRange` calls (Geyser implements `RangeGetter`); an
object on tape answers `ErrArchived` to either, which keeps the restore
semantics (never the parity fallback). `parallel_get` stays off for sealed
objects (decision 2).

**Not changed:** HEAD (cache-only, non-negotiable 2); the deletion runner and
the erasure sweep (they delete stored bytes, never read them); routing
truth (`Exists`); the chunk pipeline (per-chunk AES256-CE, unchanged).

## Tests

How red was shown: the "before" table above was measured on `origin/main`'s
code (`869bef6`) at the handlers, with a throwaway test file in a separate
worktree — the same cases the tests below assert the other way. Several
handler tests here were written alongside the code rather than strictly
before it; their red state is that table, not a failing run of the test
file itself (it does not compile against `origin/main`: it uses the new
API). Two were genuinely red first: `TestSSEBlobsOpened_OnTheServersRegistry`
(found by the live proof) and the demotion/parity stored-length tests (red
on `origin/main` as the table shows). The codec is new code, its tests
written with it.

- `internal/crypto/stream_test.go`: round trips at 0, 1, S-1, S, S+1, 3S,
  3S+17; the length function; the sealer holds the source to its size (short,
  long, an error delivered with the last bytes); `TestStream_OneGiB_BoundedMemory`
  (1 GiB through `-short`, sha256 equal, < 32 MiB allocated); one test per
  tamper class; ranged open at the first byte, across a boundary, inside a
  segment, the last byte, the whole last segment, the whole object; a shifted
  or short span fails; SSE-S3 seal/open with the version in the header,
  after a rotation, a moved version field fails; the 0x01/0x02 compatibility
  opener and the metric hook.
- `internal/crypto/ssec_test.go`: SSE-C round trip, range, wrong key at the
  first segment; same key + same bytes → different blobs (salt); the old
  format and the magic collision.
- `internal/api/sse_stream_test.go`: `TestPut_SSEBucket_OverTheOldCap_SealedStreaming`,
  `TestPut_Sealed_ShortBodyAndBadDigest`, `TestGet_SealedObject_RangeBySegment`,
  `_HeaderTamperedBeforeStatusLine_Clean500`,
  `_SegmentTamperedAfterStatusLine_CutsConnection`,
  `TestOldBlobs_0x01_0x02_StillOpen`,
  `TestCopy_WholeSource_OverTheOldCap_IntoSSEBucket_Sealed` (and a sealed
  source copied, and into a plain bucket),
  `TestCreateMultipart_SSEHeader_PersistedAndHonouredAtComplete`,
  `TestPut_SSEHeader_NoKey_Refused503`, `TestKeyReencrypt_ReSealsAs0x03_Streaming`,
  `TestVaultParity_SealedObject_RebuiltThenOpened`,
  `TestCDN_SealedPublicObject_StreamsAndRanges`,
  `TestSmartDemotion_SealedObject_CopiedAtItsStoredLength`,
  `TestSSEBlobsOpened_OnTheServersRegistry`, `TestSSEPaths_NeverBufferTheObject`
  (source walk), and Part B's `TestSSEC_Put_Get_Range_OverTheOldCap`,
  `TestSSEC_OldBlob_StillOpens`, `TestSSEC_MagicCollision_OldBlobStartingWith0x03`.
- **Assertions whose meaning changed** (listed in each PR body):
  `TestComplete_SSEBucket_NonChunkableOver256MiB_Refused413` →
  `_SealedStreaming` (versioned, GLACIER, DEEP_ARCHIVE);
  `TestPlacement_PutAndCompleteAgree` gains the over-the-old-cap rows for
  every non-chunkable class, the `AES256` header kept on Create and the
  keyless header; the oversize tests (413) → the body is read (a short one is
  400 IncompleteBody); `TestQuotaAccounting_CopyEncryptedSourceRejected`: no
  master key → 500 (fail closed; it was 501), SSE-C → 501;
  `TestGeneratedObject_TooLargeIsRefusedNeverPlaintext` → `_OverTheOldCap_Sealed`;
  `requireSSEBlob` checks 0x03 and the length function; the Part 0 W-4 probe
  expects 503; the SSE-C row value is `AES256-SSE-C-v3`.

## Live proof (local, 2026-10-13)

A private database through 089, `STORAGE_MODE=local` (no local cold driver
exists: GLACIER resolves to the primary there, its floor is `vault`), the
master key in `$CREDENTIALS_DIRECTORY/encryption_master_key`, aws-cli 2
defaults (8 MiB parts), random files with a plaintext marker at every MiB,
the process RSS sampled every second (`ps -o rss=`), peak over the idle value
just before each step.

| Step | Result |
|---|---|
| `origin/main` binary: 20 MiB sealed `put-object` + 20 MiB SSE-C `put-object` | blob `0x02`, 20 MiB + 1,121 B; old SSE-C, 20 MiB + 28 B; 0 markers |
| **Part A binary:** 1 GiB `put-object` into a versioned `sse_enabled` bucket | `0x03`, **1,073,759,313 B = the length function**, 0 markers; RSS **+2.9 MiB**, 3.75 s |
| GET of it | sha256 equal, RSS **+2.1 MiB**, 1.0–1.2 s (plain 1 GiB GET: 1.0–1.3 s — aws-cli is the bound locally); Ranges `0-0`, `1048570-1048585` (across a boundary), the last byte, the whole last segment, 200 MB in the middle: all equal |
| 300 MiB GLACIER multipart into a non-versioned `sse_enabled` bucket (was 413) | sealed whole, 314,578,705 B = the length function, floor `vault`, `-38` ETag, sha256 equal; RSS +2.5 MiB |
| 300 MiB multipart into the versioned `sse_enabled` bucket, then an overwrite (was 413) | both versions sealed, key version 1; `GET`/`HEAD`/`Range ?versionId=<old>` equal, `AES256` |
| 300 MiB plaintext source `copy-object` into the `sse_enabled` bucket (was 413) | sealed, 0 markers in the blob, the source's ETag; a copy of that sealed copy (was 501): sha256 equal |
| the two `origin/main` objects | the `0x02` object equal (whole and a Range), the old SSE-C object equal |
| rotate the tenant key (`POST …/encryption-key/rotate`), run `key_reencrypt` | `ok`, **7 blobs, 2.67 GB re-sealed under v2 in 6.6 s**, the `0x02` one included; `vaultaire_sse_blobs_opened_total` `0x02`=1, `0x03`=6. RSS **+70 MiB**: the 20 MiB `0x02` blob in the bounded compatibility opener (`io.ReadAll` + the plaintext) — measured apart: re-sealing a 64 MiB `0x03` object allocates 4 MiB in total, adding a 20 MiB `0x02` blob makes it 74 MiB |
| no master key (the same data) | PUT and CreateMultipartUpload: **503** for the `sse_enabled` bucket and for the `AES256` header; sealed GET and Range: clean **500**, nothing written |
| **Part B binary:** 1 GiB SSE-C `put-object` (was 413) | blob `03 02…`, **1,073,758,237 B** = 29 + size + 16 × 1024, 0 markers; RSS **+3.2 MiB**, 4.24 s |
| GET / Ranges / wrong key | sha256 equal (RSS +0, 1.35 s); the four boundary Ranges equal; a wrong key 403 "does not match" |
| the old SSE-C object (written by `origin/main`) | equal, whole and ranged |

**Vault parity:** the local server has one backend, so a parity rebuild
cannot be shown live; `TestVaultParity_SealedObject_RebuiltThenOpened`
drives it with two local drivers (Geyser down, served from the parity leg,
whole and ranged). **A GDPR export over 256 MiB** is impractical to build;
`TestGeneratedObject_OverTheOldCap_Sealed` seals an export of dozens of
64-byte segments instead.

## Prod proof

2026-10-13, regression only (prod has no master key: 0 sealed rows). #708
deployed 18:49 UTC (`/version` `b4a8ed2af6b0`), #709 at ~19:05 UTC
(`fbd71ef242d9`); migration 089's column present; the bench tenant
`tenant-14a623b16b3f7012`, aws-cli 2.35.21 on the SLC box (the default
profile: 64 MiB parts, 8 concurrent), bucket `wpr85-proof`.

| Step | Result |
|---|---|
| 1 GiB random, `aws s3 cp` up / down via s3.stored.ge (origin) | up 77.7 / 87.0 / 86.3 MB/s; down 96.7 / 91.9 / 85.2 MB/s; sha256 equal every time; head row `is_chunked`, ETag `…-16`, encryption none |
| the same via stored.ge (Cloudflare) | up 99.8 / 87.2 / 92.0 MB/s; down 61.8 / 103.2 (**reset by the peer mid-body**, see below) / 91.8 MB/s; the others sha256 equal |
| single-stream `curl` of a presigned GET of the chunked 1 GiB object | **138 / 154 MB/s** via the origin, **119 / 124 MB/s** via Cloudflare |
| `bytes=500000000-500999999` of the chunked object | equal |
| 20 MiB `put-object` / GET / `bytes=1048570-1048585` via stored.ge | equal (whole plaintext object, no key) |
| `put-object --server-side-encryption AES256` and `create-multipart-upload --server-side-encryption AES256` (no master key) | **503** `ServiceUnavailable` + the "asks for server-side encryption … key is not available" suggestion — was a 200 that stored plaintext (W-4) |
| **after #709:** 300 MiB **SSE-C** `put-object` via the origin (413 on `b4a8ed2`: over 256 MiB) | 200, 65.7 MB/s; row `AES256-SSE-C-v3` on `lyve`, not chunked |
| its GET / a 1 MB Range at 200 MB / a wrong key / HEAD without the key | equal (52.5 MB/s single stream from Lyve) / equal / **403** "does not match" / **403** |

**Throughput, honestly:** the aws-cli numbers are below #704's (up 161–167,
down 228–252 MB/s via the origin, the same profile) for objects stored by
the same, unchanged chunk pipeline. The server side is not the difference:
the per-stream median of chunked GETs on prod was **42 MB/s in #704's window
(13:05 UTC) and 44 / 42 MB/s during this proof** (Prometheus,
`vaultaire_large_get_throughput_bytes_per_second`), and one curl stream of
the same object ran 119–154 MB/s, faster than aws-cli's 8-way aggregate.
The Lyve probe latency was ~1.0 s all day (0.97–1.20 s). This WP changes no
chunked path (#708/#709 touch sealed whole objects only); the aws-cli
aggregate gap is recorded, not explained (client side or Lyve concurrency at
this hour), and the re-measurement belongs to the bench script, not to this
WP.

**One reset:** a 1 GiB download via stored.ge was cut — the server log shows
its write to HAProxy failing (`connection reset by peer` /
`broken pipe` on `127.0.0.1:8001`), i.e. the client-side connection went
away; no backend read failed and no cut was the server's. The retry and the
other downloads were byte-equal.

**`/health` and the canary through both switches:** `degraded` as before —
the 11 iDrive probes, plus the dormant `wasabi` driver's probe, which fails
on and off with `StatusCode: 0` since at least 17:48 UTC on the previous
build `869bef6` (R85-3; not this WP); the Lyve primary's breaker closed;
active slot :8000 after #708, :8001 after #709.

**`job_runs`:** `dedup_gc` last `ok` 02:44 UTC (the 5,076 ref-0 chunk rows of
10-13 are due on 10-20); `vault_parity` `ok`; `key_reencrypt` not registered
(no master key) — as before.

**Error lines** (18:49 → 19:20 UTC): 3 + 3 "asking for SSE-S3 refused" (this
proof's deliberate keyless requests, aws-cli retrying each 503 twice — R85-9),
and the four lines of the one client-side reset (2 × "insert event: context
canceled", 2 × "failed to stream chunk to client"). Nothing else.

**Cleanup:** every object and the bucket deleted; the bench tenant at 0
objects, 0 buckets, usage 0, 0 chunk refs, 0 active uploads; the box's temp
files and the copied credentials removed. A PUT before the bucket existed
(the proof's `mb` failed on the box profile's location constraint) was
accepted — R85-1; those objects were deleted, the bucket created properly,
the proof re-run in it.

## Gates

For each of #706, #708 and #709: `make lint` with golangci-lint **v2.14.0**
(0 issues); gosec with the Makefile flags (0 issues; #706 is test + docs);
`make test-integration` on a private database (`vaultaire_test_r85`, every
migration through 089): green, 40 packages, `-race` — which includes
`TestSQLLiteralsMatchSchema`, `TestExportSectionsCoverSchema`,
`TestSweepBucketSourcesCoverSchema`, the `internal/account` schema walk,
`TestChunkBlobCalls_GoThroughTheChunkStore`,
`TestWholeObjectReads_GoThroughTheBlobAddress`,
`TestChunkedWrites_GoThroughTheChunkedCore` and `job_rules_test.go`. CI on
the PRs and on the merge commits green. No `Co-Authored-By` trailer on any
commit.

## Decisions (to report; not built here)

1. **The ceremony gate (F-14).** With #708 and #709 deployed, **every code
   precondition of the on-box ceremony is met**: no placement refuses an
   object for its size, nothing is stored plaintext where encryption is
   asked, every encryption path streams, sealed objects copy, keep their
   parity copy and survive Smart demotion. What stays the owner's
   (`docs/ENCRYPTION.md` § the on-box ceremony): the dedicated service user
   and `LoadCredentialEncrypted=` (`deploy/systemd/vaultaire@.service:16`
   is still `User=user1`, the credential lines `:27-28` commented out);
   `apt install libtss2-rc0 ssss age` (`systemd-creds has-tpm2` must say
   `yes`); three age recipients with offline identities; step 0 (clear the
   test data, #701). Two product consequences of "every new bucket
   `sse_enabled`" to accept knowingly: **direct upload refuses such buckets**
   (`direct_upload.go:98` — a client-side upload is never sealed by the
   server; direct uploads exist only for public-read buckets, which are
   `sse_enabled` too after the ceremony, so the feature is off for new
   buckets until a direct-upload variant seals client-side), and **public
   objects are sealed** (A5 below).
2. **`parallel_get` by segment for sealed objects: S (≈ 1 day).** The pieces
   exist: `internal/parfetch` and `openParallelGet` read a whole object as
   16 MiB ranges in flight; the codec opens any run of segments. The work is
   a part planner aligned to segments (16 MiB of plaintext = 16 segments =
   16 × 1,048,592 stored bytes), the header read once and shared, each part
   opened with `OpenRange` (its own first-segment verification), and the
   same ETag/size consistency check across parts. Worth it after the
   ceremony, when sealed objects exist: today a sealed object on the Lyve
   primary reads single-stream (38–70 MB/s per the 2026-10-12 bench) where
   `parallel_get` reads ~135 MB/s.
3. **WP-R2-6's remaining scope (A4).** Per-request peaks after this WP: an
   SSE stream (S3 or C, read or write) ≤ 2 MiB + the header (measured live:
   +2–3 MiB RSS for 1 GiB); the chunked PUT pool and the chunked GET window
   unchanged from R8-12's table (≈ 72 MiB typical / ≤ 576 MiB worst per
   chunked PUT; 64 MiB per chunked GET under the 2 GiB process budget); the
   compatibility openers read a pre-WP-R8-5 blob whole (≤ 256 MiB; prod holds
   none); the Sync driver holds ≤ 8 MiB PUT bodies for its retries. WP-R2-6
   still has to bound the NUMBER of requests holding those (semaphores sized
   from `GOMEMLIMIT`), chiefly for the chunked PUT pool. SYNTHESIS row
   re-scoped.
4. **Order after this:** WP-R6-1 (location-authoritative reads, the two #677
   bypasses, PM-6 of the WP-R2-1 post-merge review), then WP-R10-8
   (per-part reservations), then version-aware manifests (WP-R3-1 decision 2,
   L). **Agreed**, with one item to fold in first, small: **a PUT or a
   multipart upload into a bucket that does not exist is accepted** (found by
   the prod proof — residual R85-1 below); it fits WP-R6-1's "the row is the
   authority" theme or a one-PR fix before it.
5. **Carried unchanged:** the 11 iDrive criticals; `wasabi` target-only (its
   probe flaps — R85-3); the two #677 bypasses; tenant f429's 2 MB on iDrive;
   the Sync.com sub-processor sentence; gey48; the D-12 drop; PM-4 of WP-R2-1
   (`whole_skipped` gauge); PM-8 (`LIKE` escape in the management objects
   list); MFA-Delete on batch (R4-17).

**A5, the public-read question: seal public objects (keep the invariant).**
Every new bucket is `sse_enabled` with a key, including one later made
public; PUBLIC-class objects on R2 are then sealed. Nothing at the edge reads
R2 raw (Sippy pulls through the S3 API, the Workers through `/cdn` —
`docs/CLOUDFLARE.md`), and `/cdn` now opens a sealed object as it streams.
The cost per `/cdn` hit is one ML-KEM decapsulation plus one AES-GCM pass
over the bytes served — measured locally the sealed 1 GiB GET ran at the
plain GET's speed (1.0–1.35 s each, aws-cli bound); a Range costs one extra
backend request (the header). The edge caches the plaintext as before. The
one-sentence invariant stays true: with the key on, everything a new bucket
stores is encrypted at rest.

## The invariant

**No encryption path holds a whole object in memory**, outside the two
bounded compatibility openers for blobs written before this WP (SSE-S3
`0x01`/`0x02` and old-format SSE-C, ≤ 256 MiB by construction; prod holds
none); and **no size is refused for encryption anywhere**: SSE-S3 and SSE-C
seal at any size, a multipart upload is placed as a PUT of the same bytes,
a copy and a generated object seal as they stream.

## Not done / residuals

| # | Sev | What | Hand-off |
|---|-----|------|----------|
| R85-1 | **P2** (pre-existing, found by the prod proof) | A PUT and a multipart upload into a bucket with no `buckets` row are accepted and stored (`origin/main` has no existence check on PUT/Create/Complete; only CopyObject's destination has one): the objects are billed and readable but invisible to ListBuckets, and the bucket's settings (`sse_enabled`, versioning, tier) are absent — after the ceremony such an object is not sealed. The prod proof's `mb` failed (`InvalidLocationConstraint`, the box's aws-cli profile) and the uploads landed anyway; deleted, the bucket created properly, the proof re-run. | **Fixed** in WP-R6-1 Part 0 (§ Post-merge below): `NoSuchBucket` on PUT/Create/Complete, the CopyObject rule reused |
| R85-2 | P3 (pre-existing) | HEAD without `?versionId` in a versioned bucket answers no `x-amz-version-id` (seen live) | **closed 2026-10-14 (WP-VM-1, #728)**: HEAD names the current version |
| R85-3 | note | The dormant `wasabi` driver's signed HeadBucket probe fails on and off with `StatusCode: 0` (connection level) since at least 17:48 UTC on `869bef6`; `/health` names it with the 11 iDrive probes when it does | Owner (Wasabi account), not this WP |
| R85-4 | P3 | The 0x01/0x02 compatibility opener `io.ReadAll`s a legacy blob (up to ~2.5 × its size in transient memory); bounded, prod holds none | Remove the opener once a `0x01`/`0x02` blob can no longer exist (after the ceremony + a rotation, or never needed) |
| R85-5 | P3 | `parallel_get` stays off for sealed objects | Decision 2 |
| R85-6 | note | No format-only re-seal pass is scheduled: a `0x01`/`0x02` blob is re-sealed as `0x03` when its tenant's key rotates (prod holds none) | — |
| R85-7 | note | Multipart with SSE-C stays 501; an SSE-C copy source stays 501 | WP-SSEC-1 |
| R85-9 | P3 | The keyless refusal of a request asking `AES256` logs at **Error** (`placement.go`, Create): any client can write Error lines at will (aws-cli retries each 503 twice: 3 lines a request). The bucket case is an operator condition (Error is right); the header case should be Warn and `markClientRefusal` | **Fixed** in WP-R6-1 Part 0 (§ Post-merge below) |
| R85-8 | note | Customer copy (WP-R14-6), read, not edited: `internal/api/docs_faq.md:121-126` names no size limit and says SSE-S3 is "not yet enabled in production" — still true; it must change the day the ceremony runs ("on by default for every bucket"); `landing.src.html:641` names SSE-C and client-side encryption — accurate | WP-R14-6, with the ceremony |

## Post-merge (worker probe)

2026-10-13, Part 0 of Stage 1a Prompt 5 (WP-R6-1), on `origin/main`
`9f3d0c1` (= prod `fbd71ef` + docs), at the handlers, on a private database
through 089. What the note above did not drive. Tests:
`internal/api/wp_r8_5_probe_test.go`. No P0/P1.

| # | Probe | Result | Verdict |
|---|---|---|---|
| 1a | A sealed blob **shorter** than the length function says (cut inside the last segment; the last segment dropped; cut at the end of the first segment; inside the first; the header only, an interrupted write) — whole GET, a Range in the last segment, CopyObject, `/cdn` | Whole GET: a cut **after** the status line when the first segment verifies (5,120 / 4,096 of 6,044 bytes went out, exactly the verified segments), a clean **500 before** it when it does not. Range in the last segment: a clean **500** every time (the ranged read of the spanned segments fails before the status line). `/cdn`: the same pair. CopyObject: no destination row ever; a **500** when the first segment fails, but a **`ServiceUnavailable` (503, at Warn) when a later one does** | Holds — never a short 200, never a byte past the truth. The 503 is **R85-10**, fixed here |
| 1b | A sealed blob **longer** than the length function (one byte appended; a whole stale segment appended; the old, longer file's tail left behind — a non-atomic overwrite) | Whole GET: a cut after the status line **before the final segment** every time (5,120 of 5,197 bytes: the opener holds the final segment until it has seen the end, and the end is not there). A Range in the last segment: **206, byte-equal** — it reads exactly the segments the length function names; the bytes after them are never read. `/cdn`: the same. CopyObject: no destination row (`ServiceUnavailable` before R85-10, `InternalError` after) | Holds. The whole GET of such a blob is always cut, never served: **R85-11** (P3) below |
| 2 | `key_reencrypt` racing a whole GET of the same sealed object: the GET paused after 1,500 stored bytes, the key rotated, the job re-sealed the object at a new address and retired the old one, the collector ran, then the GET resumed | The GET finished **byte-equal** from the old address; the old address is retired `reencrypted` with **> 23 h** of grace left (`retiredBlobGrace` = 24 h); the collector's run left it in place; the next GET reads the new address | Holds |
| 3 | SSE-C under `?versionId`: an old version in the old format (`AES256-SSE-C` on its rows, `[nonce][ct+tag]` blob) and a new one (`AES256-SSE-C-v3`) of the same key, different customer keys | Each version opens by **its own row's** format, whole and a Range; a wrong key on either, whole or ranged, and each version's key on the other, are **403 before the status line**, no plaintext byte. (The flip carries the head row's algorithm onto the version it displaces: a pre-#709 object has the old value on both rows, as the fixture now writes it) | Holds |
| 4 | A sealed Complete answered after the keep-alive committed (`instantCommit`) | The body is right (`CompleteMultipartUploadResult`, the multipart ETag); `x-amz-server-side-encryption` is **absent** (the 200's headers went out before the operation set it); the object is sealed; HEAD and GET report `AES256`. AWS's `CompleteMultipartUploadResult` has **no** `ServerSideEncryption` element: boto3, the Go SDK v2 and the JS SDK v3 fill their `ServerSideEncryption` field from the response **header** only, so after a commit the SDK reports none for a sealed object. aws-cli does not read it; a client that asserts encryption on the Complete response (a compliance-minded backup tool) would see a false negative; HEAD is the truth | Documented, **R85-12** (P3) |
| 5 | **R85-1** (P2): PUT, CreateMultipartUpload, CompleteMultipartUpload into a bucket with no `buckets` row | Before: stored and billed (the 10-13 prod proof's PUTs landed in a bucket that did not exist); Create 200 + an upload row; Complete of an upload whose bucket was deleted since Create 200 + a head row. After: **404 `NoSuchBucket`** with the near-name suggestion (`writeNoSuchBucket`) — the PUT before any body byte or reservation, Create before any upload row or staging directory, Complete before the keep-alive can commit (the upload stays for Abort / the reaper). The rule is CopyObject's (`longOpBucketExists`: a row, or objects recorded under the name — a bucket older than the registry keeps working). Generated objects (GDPR export, access logs, inventory) already refused a target with no row (`errTargetBucketGone`); `_exports` has its row; unchanged | **Fixed** (this PR) |
| 6 | **R85-9** (P3): the keyless refusal of a request asking `AES256` | Before: an Error line per request (PUT, Create) and a 5xx on `vaultaire_errors_total`. After: **Warn** + `markClientRefusal` (out of the 5xx count). The `sse_enabled`-bucket case (an operator condition) stays Error and counts | **Fixed** (this PR) |

### Findings

| # | Sev | Finding | Disposition |
|---|---|---|---|
| R85-10 | P3 | CopyObject of a sealed source that does not open after its first segment (truncated, corrupt) answered `ServiceUnavailable` (503, Retry-After) and logged at Warn ("the source could not be read", the transient-backend case of Prompt 2b.3 D1.6): an integrity failure a client would retry for ever and no operator would see. Nothing was stored | **Fixed** (this PR): `crypto.ErrStreamCorrupt` / `errSealedUnreadable` in the source error → 500 + an Error line, as the GET |
| R85-11 | P3 | A sealed blob longer than its length function is always cut on a whole GET (after the status line, before the final segment). Correct (never past the truth) but never served, and detected only at the end of the stream; the backend's own length (a GET's Content-Length) would tell before the status line | Residual: a pre-status length check where the driver reports one; low value (the multi-bridge Sync driver already picks the copy of the expected size) |
| R85-12 | P3 | A sealed Complete answered after the keep-alive committed carries no `x-amz-server-side-encryption` header (SDKs read it from the header only) although the placement is decided before `longOpBegin` | Residual: hand the header to the keep-alive at `longOpBegin` so the committed 200 carries it |
