# WP-P13: Stage 1a Prompt 13

2026-10-18. Three parts:

- **Part 0**: the four follow-ups of the WP-P12 post-merge review (RP13-1..4) and the prod read.
- **Part A**: WP-R8-3, body integrity on the whole-object path:
  - A.1: the MD5 tee and the cut;
  - A.2: the identity-bound AAD, format 0x04.
- **Part B**: WP-R2-4 (+ R2-21, R2-22, R2-23, R8-14, R8-15 = WP-R8-6). One header writer for HEAD, 200, 206 and 304.

PRs:

- **#768**: Part 0, deployed `6d1a8ab`.
- **#769**: A.1, deployed `fb51d0f`.
- **#770**: A.2, deployed `2c3583a`.
- **#771**: Part B, deployed `2942a9b`.
- this PR: docs only.

Start sha: `origin/main` `c50ed25`. **No migration.** A.4 was not taken (its design is residual RP14-6), so 094 stays reserved for WP-PAY-1. **The next migration is 094.**

## Decisions

1. **When a failed chunk PUT keeps its claim (RP13-1): by what the backends said.**
   - `engine.WithPutOutcome` records every backend attempt that ended **without an answer** (`engine.PutAnsweredOrUnsent` is the rule). Nothing was committed when the attempt:
     - got an HTTP status > 0 or an S3 error code;
     - got a `PermissionError`;
     - was refused before sending (`ErrInvalidInput`, `ErrChunkAddress`);
     - failed to connect (a refused dial or a DNS failure);
     - was never made (an open breaker, or a request already gone).
   - Everything else may have left the whole body with the backend: a deadline, a reset, a cancel, an unknown error.
   - The chunk store keeps the claim on no answer (a Warn naming the backends) and drops it on an answer.
   - The shapes, pinned through the real fixed-bucket driver, aws-sdk-go-v2 and the engine against the stalling fake S3 (`drivers/put_outcome_test.go`):

     | backend | engine error (abridged) | outcome |
     |---|---|---|
     | stalls every attempt | `… PutObject …: no answer within 200ms, twice (after one retry on a fresh connection): context deadline exceeded` | unanswered |
     | stalls; the caller's bound ends it | `… https response error StatusCode: 0 … canceled, context deadline exceeded` | unanswered |
     | stalls; the request is cancelled | `… StatusCode: 0 … canceled, context canceled` | unanswered |
     | answers 500 / 403 | `… StatusCode: 500 … api error InternalError` / `StatusCode: 403 … AccessDenied` | answered |
     | nothing listening | `… StatusCode: 0 … request send failed … connect: connection refused` | unsent |

     The SDK reports `StatusCode: 0` when no answer came, which is why the rule needs a status above 0.
   - **The cancel case (c).** A cancel can land after the request went out: the client left, or a sibling chunk failed and the pool cancelled the rest. The backend may then hold the whole body, so **the claim stays**. A cancel before any attempt drops it (no backend was asked).
   - **The cost:** a client that retries an upload whose chunk PUT got no answer waits up to the window (3 min) for that chunk. Residual RP14-1 has the cheaper alternative.
2. **A zero `LastModified` is never old (RP13-2).** Such a key is counted `unknown_age` and reported in the dry run with its zero time. It is never erased, and each run logs one Warn per backend.
3. **The shutdown's flush is `FinalFlush` (RP13-3).** `Server.flushTrackers` calls it. A row that fails there is dropped at once, with the Warn `bandwidth delta dropped at shutdown` naming its bytes. The flusher goroutine's own "final flush on ctx.Done" runs on `context.Background()` and never fires, so it stays the ordinary `Flush`.
4. **RP13-4: the chunked copy references through `IncrementRefTx`.** A ready row, exactly one, or the install fails whole (500, nothing installed).
   - Before, a pending row under a source chunk was referenced and the copy answered **200**.
   - "The way a vanished chunk fails" does not exist. `tenant_chunk_refs_scope_hash_fkey` refuses the delete of a row a manifest names, so a pending row is the only not-ready shape a manifest chunk can take.
5. **The tee withholds the object's last byte (A.1).** If the tee handed out every byte and only then cut, a Content-Length client would already hold a complete body. It would never see the cut. So the reader hashes as it streams and holds the final byte until the MD5 matches. On a mismatch the client is short by at least one byte:
   - aws-cli reports `IncompleteRead(… 1 more expected)`;
   - rclone sees an HTTP/2 `INTERNAL_ERROR`;
   - both retry.
6. **No kill switch for the cut.** Prod's whole rows were read before the deploy: 501, every one a 32-hex MD5 on `idrive`, and none an MD5-of-empty with a size. Every whole-object writer records the MD5 of the bytes it stores (for a sealed object, of the plaintext): PUT, CopyObject, generated objects, the R2 direct upload's single PUT and the re-encrypt, which checks it. The rule `ObjectIntegrityFailure` pages on the first cut. A false cut would show up there in minutes.
7. **Where the tee sits (A.1).** On the stream the client reads, whatever fetched the bytes:
   - `plain` (the engine's GET);
   - `parallel` (the ranged reader: its ETag-and-size identity check across ranges stays, and the tee adds content);
   - `sealed` (after the open);
   - `parity` (a rebuild);
   - `cdn`.

   A chunked object verifies every chunk (stored-blob hash, GCM tag, plaintext SHA-256) and is counted in neither metric.
8. **The identity is the KEY, never the address (A.2).**
   - A 0x04 segment's AAD is `header ‖ "vaultaire-sse-identity-v4" ‖ len‖tenant ‖ len‖bucket ‖ len‖key`.
   - The bucket and key are the object's: a retained version, a re-addressed blob (WP-R2-1) and a re-encrypted one keep them. So the verbatim movers need no change: Smart demotion, promotion and copy-back, the parity rebuild, `?versionId`.
   - The DEK derivation is unchanged. HKDF's info already covers the header, and the format byte is in the header, so rewriting 0x04 to 0x03 changes the DEK and fails.
   - The layout is the 0x03 one: header length, `SealedLength`, `MaxStreamHeader` and `storedBlobLength` are unchanged.
9. **Chunk encryption is not changed (A.2).**
   - A chunk is shared by every object of a tenant that holds its bytes (the dedup), so it has no bucket or key to bind.
   - What binds its content is the check after the open: the plaintext's SHA-256 must equal the index row's `plaintext_hash`. So a chunk blob swapped for another chunk's fails there.
10. **What the whole path still cannot verify (A.3).**
    - **A Range (206)**: only the whole body hashes to the ETag. The size is checked, and on the parallel path the ETag and total size are checked across ranges.
    - **A `-N` object**: its ETag is the MD5 of the parts' MD5s. Only the size is checked, until A.4.
    - **A row whose ETag is not an MD5**: none on prod today.
    - **What a backend would need to do to serve wrong bytes undetected now**: serve them to a Range, to a `-N` object or to a parallel range of a mismatching blob whose ETag it reports unchanged. On a whole single-part GET it would need bytes with the same MD5 as the original, a collision. MD5 detects accidents and swaps, but not a forger. A forger who can rewrite a sealed blob faces GCM with the identity in its AAD.
11. **Part B: what the one writer settles.**
    - **The version id.** `x-amz-version-id` is sent on a versioned or suspended bucket only: the requested version, the latest, or `null` for an object with no version row. A bucket that was never versioned names none, as AWS. GET used to send `null` everywhere.
    - **The default Cache-Control.** It is `private, no-cache`, on HEAD too.
    - **Overrides on HEAD.** `?response-content-disposition` applies to HEAD as to GET.
    - **The 304.** It carries ETag, Last-Modified, Cache-Control and Expires. The prompt said "ETag + Last-Modified and nothing else", but RFC 9110 §15.4.5 says a 304 MUST carry the Cache-Control and Expires a 200 would have. `conditional_test.go` already pinned the Cache-Control. Nothing else of the object goes in.
12. **Part B: what AWS does that we kept.**
    - **A delete marker's version id** → 405 `MethodNotAllowed`, with `x-amz-delete-marker: true`, the marker's id, its Last-Modified and `Allow: DELETE`, on GET and HEAD. It was 404.
    - **SSE-C without the key** → 400 `InvalidRequest`, with AWS's text, decided before any object header. It was 403, and HEAD had already set the size, ETag and Last-Modified.
13. **Part B: what we chose not to do.**
    - **HEAD with a wrong SSE-C key → 200.** HEAD is cache-only, and the key can only be checked against the blob. This is a deviation, recorded in `docs/API.md`. GET with a wrong key stays 403.
    - **`/cdn` keeps its own writer.** Its Cache-Control (the bucket's max-age), its disposition rule (HTML/SVG/XML are always attachments) and CORS belong to the public surface, and no S3 client compares a `/cdn` response with a HEAD.
    - **`x-amz-restore` stays HEAD-only.** HEAD's one backend call stays HEAD's. AWS sends it on a GET of a restored object too: a deviation, recorded.

## What was built

### Part 0 (#768)

- **`engine/put_outcome.go`** (new): `PutOutcome`, `WithPutOutcome`, `PutAnsweredOrUnsent`; `CoreEngine.Put` records each attempt. The `Driver` interface is unchanged.
- **`chunk_store.go`**: `put` returns the outcome.
- **`chunk_claim.go`**: keep or drop by the outcome; the header comment rewrites decision 8's sentence.
- **`chunkPutBound`** is now a field (`chunkPutBoundDefault` 90 s), so tests set it small.
- **`chunk_orphans.go`**: `UnknownAge` and the Warn.
- **`bandwidth.go`**: `FinalFlush` and `flush(final)`; `server.go` calls it from `flushTrackers`.
- **`s3_copy.go`**: the chunked copy references through `gci.IncrementRefTx`, checks one row, and refuses a pending one.

### A.1 (#769)

- **`object_integrity.go`** (new): `newIntegrityReader`, `integrityError`, `unverifiedReason`, and two counters registered with the large-GET collectors.
- **`s3_engine_adapter.go` `HandleGet`**: the whole 200 is armed, every 206 counted, and an integrity error cuts (`abortResponse`). The path label is plain, parallel, sealed or parity.
- **`cdn.go`**: the same on `/cdn`.
- **Rule** `ObjectIntegrityFailure` (critical) in `deploy/monitoring/vaultaire-backends.yml` and the README. `promtool check rules`: 18 rules.
- **Docs**: `changelog.md` (2026-10-19, the cut explained) and `internal/api/CLAUDE.md`.

### A.2 (#770)

- **`crypto/stream.go`**:
  - `StreamFormat` = 0x04, `StreamFormatV3` = 0x03, `IsStreamFormat`;
  - `StreamIdentity`, `ErrStreamIdentity`, `streamAAD`, `openAAD`;
  - `StreamHeader.Format`;
  - the sealer and opener take the AAD.
- **`sse_s3.go` and `ssec.go`**: every seal and open names the object; the `onOpen` label is the blob's own format.
- **API**:
  - `newSSEWriteSealer(…, id, …)`;
  - `sseS3Opener{svc, id}` and `sseCOpener{key, legacy, id}`;
  - the writers (PUT, Complete, CopyObject, `generatedObjectWriter`, `key_reencrypt_whole`) and every opener site;
  - `s3_large_get` reads either header byte.
- **Structure test** `TestSSEPaths_EveryOpenerAndSealerNamesTheObject`.
- **Docs**:
  - `internal/crypto/CLAUDE.md`: the format table (0x01–0x04, what each binds);
  - `docs/ENCRYPTION.md`: the format, the re-encrypt migrating formats, the ceremony's step 8;
  - `internal/api/CLAUDE.md`.

### Part B (#771)

- **`object_headers.go`** (new): `objectHeaders`, `write(w, mode, rng)` and `sseOf`.
- **`s3.go`**:
  - `handleHeadObject` and `headVersion` on the writer;
  - the SSE-C refusal comes first;
  - `headRange` and `headStatus` (HEAD + Range);
  - `writeDeleteMarkerVersion` (405).
- **`s3_engine_adapter.go`**:
  - `HandleGet` builds `oh` once;
  - the 304, the native 206, the `serveRange` fallback and the whole 200 write through it;
  - `handleChunkedGet(…, oh)`;
  - the SSE-C 400;
  - the marker 405;
  - the chunked PUT's SSE header (`chunkedResult.Encrypted`).
- **Docs**: `docs/API.md` (the header set, HEAD + Range, the deviations table), `changelog.md` and `internal/api/CLAUDE.md`.

## Before → after

| # | Item | Before (`c50ed25`) | After | Test |
|---|---|---|---|---|
| 0.1a | a chunk PUT that got no answer (deadline / the bound) | claim dropped; the next writer stored at once | claim kept (`pending`, its `pending_since`); the next writer waits the window, takes over, stores | `TestChunkClaim_APutWithNoAnswerKeepsItsClaimForTheWindow` (deadline, bound) |
| 0.1b | a chunk PUT answered 500 | dropped at once | dropped at once (kept) | `TestChunkClaim_APutTheBackendAnsweredWithAnErrorDropsItsClaimAtOnce` |
| 0.1c | the request cancelled while the backend holds the body | dropped | kept | `TestChunkClaim_ACancelAfterTheBodyWentOutKeepsTheClaim` |
| 0.1 | the error shapes of a stalled, answering, unreachable backend | — | pinned | `TestPutOutcome_*` (drivers) |
| 0.2 | a listed key with a zero `LastModified` | erased at first sighting | never erased; `unknown_age`, dry-run candidate, one Warn | `TestChunkOrphans_AKeyWithNoModificationTimeIsNeverErased` |
| 0.3 | the shutdown flush cannot write a row | kept "for the next flush" (none) | dropped, Warn `bandwidth delta dropped at shutdown` with its bytes | `TestBandwidthTracker_TheShutdownFlushDropsAFailedDeltaWithAWarnNamingItsBytes` |
| 0.4 | chunked copy of a manifest naming a pending row | **200**, the pending row referenced | 500, nothing installed, no reference | `TestChunkedCopy_ASourceChunkThatIsNotReadyIsNeverReferenced` |
| A.1a | whole plain GET of other bytes of the right length | 200, wrong bytes, clean end | cut before the last byte; Error line; `failures{plain,md5}` | `TestObjectIntegrity_APlainGetOfOtherBytesIsCutBeforeItsLastByte` |
| A.1b | the same on `/cdn` | 200 clean | cut; `failures{cdn,md5}` | `TestObjectIntegrity_ACDNGetOfOtherBytesIsCut` |
| A.1c | the same through `parallel_get` | 200 clean | cut; `failures{parallel,md5}` | `TestObjectIntegrity_AParallelGetOfOtherBytesIsCut` |
| A.1d | a Range, a `-N`, a chunked object | unverified, uncounted | served; `unverified{…,range\|multipart}` once per response; chunked uncounted | `…_ARangeAndAMultipartETagAreServedUnverifiedAndCounted`, `…_AChunkedObjectIsNeitherCheckedNorCounted` |
| A.1e | a sealed blob copied to another key's address | **200, A's bytes as B** | cut (A.1); refused 500 before the status line (A.2) | `TestObjectIntegrity_ASealedBlobCopiedToAnotherKeyIsNeverServedAsIt` |
| A.1f | a client that goes away | — | neither cut nor counted | `…_AClientThatLeftIsNeitherCutNorCounted` |
| A.1g | the egress record of a cut response | — | the bytes sent (size − 1) | in A.1a |
| A.1h | a matching GET | — | no counter, no line; 0.87 s CPU per GiB on the box | `…_AMatchingGetIsServedWholeAndCountsNothing`, `BenchmarkIntegrityReader_1GiB` |
| A.2a | the format of a new SSE-S3 blob | 0x03 | 0x04, same header length and `SealedLength` | `TestStreamV4_NewSSES3BlobsAreFormat0x04WithTheSameLayout` |
| A.2b | a blob of another key / bucket / split, whole, range, RangeOpener, GET, parallel sealed, `/cdn`, copy source | opened, served as the other key | `ErrStreamCorrupt` at the first segment; 500 before any byte | `TestStreamV4_ABlobOpensOnlyUnderItsOwnIdentity`, `TestSSEIdentity_ABlobCopiedToAnotherKeyIsRefusedBeforeTheStatusLine`, `…_AParallelSealedGetOfAnotherKeysBlobIsRefused`, `…_CDNOtherKeysBlob` |
| A.2c | first byte rewritten 0x04 → 0x03 | — | does not open | `TestStreamV4_AFormatByteRewrittenTo0x03DoesNotOpen` |
| A.2d | a 0x03 blob | opens | opens (GET whole and range), counted `0x03` | `TestStreamV4_A0x03BlobStillOpensAndIsCountedAsSuch`, `TestSSEIdentity_A0x03BlobStillOpensAndTheReencryptMigratesItTo0x04` |
| A.2e | SSE-C | AAD = header | the identity too | `TestStreamV4_SSECBindsTheIdentityToo` |
| A.2f | every writer | — | 0x04 for the destination; the re-encrypt migrates 0x03 → 0x04 | `TestSSEIdentity_EveryWriterSeals0x04ForTheDestination`, `…_A0x03BlobStillOpens…` |
| A.2g | a retained version (`?versionId`) | opens | opens (sealed for the key) | `TestSSEIdentity_ARetainedVersionOpensAsItsKey` |
| A.2h | a caller with an incomplete identity | — | refused (`ErrStreamIdentity`); the source walk finds none | `TestStreamV4_AnEmptyIdentityIsRefused`, `TestSSEPaths_EveryOpenerAndSealerNamesTheObject` |
| B.a | HEAD / 200 / 206 header sets | HEAD = 200 minus `Accept-Ranges` (prod, 6d1a8ab); 206 = 200 | one set | `TestHeaders_HeadGetAndRangeCarryTheSameObjectHeaders`, `…_ADefaultCacheControlIsTheSameOnHeadAndGet` |
| B.b | HEAD + `Range: bytes=0-1023` / an unsatisfiable range / a 0-byte object | 200 whole / 200 / 200 (prod) | 206 + range length / 416 `bytes */<size>` / 416 | `TestHeaders_HeadWithARangeAnswersAsAWSDoes` |
| B.c | HEAD `?versionId` of a non-current version; of a delete marker | its attributes; 404 | the same set as GET `?versionId`; **405** + `x-amz-delete-marker` | `…_HeadOfANonCurrentVersionCarriesItsOwnAttributes`, `…_AVersionIdNamingADeleteMarkerIs405` |
| B.d | SSE-C GET / HEAD without the key | 403; HEAD with ETag + Last-Modified set (prod) | **400 `InvalidRequest`**, AWS's text, no object header; wrong key: GET 403, HEAD 200 (deviation) | `…_AnSSECObjectWithoutItsKeyIs400WithNoObjectHeaders` |
| B.e | chunk-encrypted PUT / GET / Complete | no SSE header on PUT and GET | `AES256` on all three, as HEAD | `…_AChunkEncryptedObjectNamesItsEncryptionEverywhere` |
| B.f | 0-byte SSE-C PUT | 200 (already: WP-R8-5 fixed R2-23) | 200; HEAD 0; GET empty | `…_AnEmptySSECObjectIsStoredAndRead` |
| B.g | `/cdn` | its own headers | its own writer (decision 13) | — |
| B.h | 304 | ETag, Last-Modified (+ Cache-Control on GET) | ETag, Last-Modified, Cache-Control, Expires on GET and HEAD | `…_A304CarriesTheValidatorsAndTheCacheHeadersOnly` |
| B.i | parallel / sealed GET vs single stream | — | the same 200 and 206 sets | `…_TheParallelAndTheSealedGetAnswerThePlainGetsHeaders` |

## Prod proof

### Part 0.5: the prod read, before anything of this prompt deployed (2026-10-18 20:21 UTC, `425a7b9`, slot 8001)

- **Logs:** 0 `"level":"error"` lines since 17:00 UTC. There were 9,619 info and 4,449 warn: 4,440 `backend health check failed` (the dead iDrive key), 8 HEAD-not-in-cache and 1 put-deadline retry.
- **Jobs:** 19/19 `ok`. `dedup_gc`'s note is unchanged (1,049 not swept, idrive). `routing_truth` (06:08): 10 idrive errors.
- **`chunk_orphans`:** the 16:45 run's result was 13 found / 13 erased, every backend `complete`, cursors absent. **The 2026-10-19 01:30 run had not happened at read time.** Its read is in RP14-7.
- **`routing_truth`'s chunked sample: 0 objects.** The reason: prod has **0 chunked head rows and 0 chunked version rows**. The 737 manifests left in `tenant_chunk_refs` (32,123 refs, all the bench tenant's, created 2026-10-18 by Prompt 12's load runs) are all `retired_blobs` rows (`chunked`, `deleted`) due from 2026-10-19 04:08 UTC. The collector releases them then. Not a leak.
- **`global_content_index`:** 41,024 rows, all `ready` (lyve 38,530, wasabi 1,318, idrive 1,176). `vaultaire_chunk_orphan_bytes` 0 on all three.
- **UploadPartCopy:** every `vaultaire_s3_long_op_total{op="UploadPartCopy"}` series reads 0 on slot 8001.
- **Whole rows** (for A.1): 501, all a 32-hex MD5, all on `idrive`. None is an MD5-of-empty with a size, and there are no versions.

### Part 0 (#768, `6d1a8ab`, 21:06 UTC, slot 8000)

Bench tenant, `s3.stored.ge`, `AWS_MAX_ATTEMPTS=1`:

- A 90 MiB single-part PUT went through the chunked store (claim → PUT with the outcome option → ready), `is_chunked`.
- `copy-object` of it is a manifest copy through `IncrementRefTx`, with the same ETag.
- Both GETs are byte-equal. The bucket's refs are 92 rows, 0 pending. Deleted.
- 0 Error and 0 Warn lines in the run.
- Since the deploy: 0 `may still land` lines (no stall happened) and 0 `bandwidth delta dropped at shutdown` (the switch at 20:55 stopped a slot running the old code).

### A.1 (#769, `fb51d0f`, 21:11 UTC, slot 8001)

Bench tenant, `AWS_MAX_ATTEMPTS=1`. Each blob was overwritten on Lyve directly with the backend's own pair, read inside the script and never printed (`stored-us-…`, key `t-<tenant>/<tenant>_<bucket>/<storage_key>`), with random bytes of the same length.

- **Plain, 32 MiB, through `stored.ge`.**
  - aws-cli: `Connection broken: IncompleteRead(33554431 bytes read, 1 more expected)`. The 33,554,431 bytes received are a prefix of the tampered bytes.
  - rclone: `stream error: stream ID 5; INTERNAL_ERROR; received from peer` (Cloudflare's HTTP/2 reset).
  - Both are counted, `failures{plain,md5}` = 2.
  - Restored by a re-PUT through Stored: GET byte-equal. Deleted.
- **Parallel, 128 MiB.** A single-part PUT through `s3.stored.ge` into a versioned bucket (stays whole: `is_chunked` f, single MD5 ETag), with `parallel_get` on for the bench tenant only.
  - **How the flag was set:** a `feature_flags` row (`updated_by = wp-p13-proof`), removed at the end. The admin API needs an admin JWT, which needs the owner's MFA.
  - `vaultaire_large_get_parallel_reads_total{backend="lyve",path="ranged"}` went 0 → 1.
  - aws-cli: `IncompleteRead(134217727 bytes read, 1 more expected)`, prefix equal. `failures{parallel,md5}` = 1.
  - Restored and byte-equal. Every version deleted.
- **Error lines:** exactly the three deliberate ones, `object integrity: the body served does not match its row — the response is cut`, each naming the key, `backend=lyve`, the recorded ETag, the measured MD5, `reason=md5` and the path.
- **The rule:** `ObjectIntegrityFailure` is not installed on the box yet (RP14-3). It would fire on these three for an hour.

### A.2 (#770)

**Not provable on prod: there is no master key**, so no sealed write exists. The tests are the proof, and `docs/ENCRYPTION.md`'s ceremony gains step 8: after the key is set, A's blob is copied over B's address on the backend, and a GET of B must answer 500 before any byte.

### Part B (#771, `2942a9b`)

This run used one bench-tenant object carrying every attribute, in a versioned bucket. Requests went through `stored.ge` with curl SigV4 and with aws-cli. The same script ran before the deploy (21:07 UTC, `6d1a8ab`) and after it (21:36 UTC, `2942a9b`, slot 8001):

| | before (`6d1a8ab`) | after (`2942a9b`) |
|---|---|---|
| HEAD vs GET 200 object headers | differ: HEAD lacks `accept-ranges` | **equal** |
| GET 206 (`bytes=0-1023`) vs GET 200 | equal | equal |
| HEAD `Range: bytes=0-1023` | **200**, length 65536 | **206**, length 1024, `bytes 0-1023/65536`, headers equal to GET 200's |
| HEAD `Range: bytes=70000-` | **200** | **416** `bytes */65536` |
| SSE-C GET without the key | **403** `AccessDenied` | **400** `InvalidRequest`, AWS's message |
| SSE-C HEAD without the key | **403**, with `etag` and `last-modified` set | **400**, no object header |
| SSE-C GET / HEAD with the key | 200 byte-equal / 200 + algorithm | the same |
| 0-byte SSE-C PUT, HEAD, GET | 200, `content-length: 0`, 200 0 B | the same (R2-23 was already fixed) |
| `aws s3 sync` of 50 files, up then again; down then again | 0 / 0 changes | 0 / 0 changes |
| `rclone check` | not run (my connection-string quoting) | 0 differences, 50 matching files |

- The GET 200 set after the deploy: `accept-ranges`, `cache-control: max-age=60, public`, `content-disposition`, `content-language`, `content-type`, `etag`, `expires`, `last-modified`, `x-amz-meta-owner`, `x-amz-meta-project`, `x-amz-storage-class: STANDARD`, `x-amz-tagging-count: 2`, `x-amz-version-id`, `x-amz-website-redirect-location`.
- Every bucket and version written was deleted (53 versions). 0 Error lines in either run.
- **A.2 on prod (`2c3583a`, 21:20 UTC):** `vaultaire_sse_blobs_opened_total` exports the `0x04` label, and every format reads 0 (nothing is sealed on prod). 0 Error lines since the switch.

## Residuals

| # | Sev | Item | Where it goes |
|---|---|---|---|
| RP14-1 | P3 | **A cancelled chunk PUT keeps its claim, so a retry waits.** A chunk PUT cancelled by a sibling's failure or a client that left keeps its claim when the request had gone out (decision 1). The client's retry of that upload then waits up to the 3 min window for those chunks (≤ 8, the pool's workers). Cheaper: let an in-flight chunk PUT finish detached under `chunkPutBound`, so its outcome is known (ready, then released by the abort's compensation, or an answer that drops it). | the next chunk-store prompt |
| RP14-2 | P3 | **The outcome reads each backend's final error, not each SDK attempt's.** If the SDK's retryer saw a reset (body sent, no answer) and then a 500 on its next attempt, the attempt counts as answered and the claim is dropped. Needs a reset with the whole body sent, then an answer, then a late commit of the first. | none until seen; a per-attempt hook would need an SDK middleware |
| RP14-3 | note | `ObjectIntegrityFailure` is in `deploy/monitoring/vaultaire-backends.yml` but not installed on the box (rules are installed by hand, README § Installing a rules file). Install it after 22:12 UTC on 2026-10-18, an hour after the deliberate proof failures. | operator |
| RP14-4 | note | **What the whole path still cannot verify (A.3, decision 10):** a Range, a `-N` object, a row whose ETag is not an MD5 (none on prod). | A.4 below for `-N`; a Range stays size-checked |
| RP14-5 | note | **Deviations from AWS** (`docs/API.md`): HEAD with a wrong SSE-C key answers 200 (cache-only); GET does not send `x-amz-restore`. | keep; revisit if a client depends on either |
| RP14-6 | P3 | **A.4, a whole-body digest at write time, not taken.** The design:<br>• **Schema:** migration 094 (WP-PAY-1 → 095) adds `object_head_cache.body_md5 TEXT` and `object_versions.body_md5 TEXT`, NULL = not known.<br>• **Complete:** tees `stagedPartsReader` into an MD5 and writes it with the flip.<br>• **Other writers:** CopyObject whole writes the MD5 it already computes (it is the ETag for a single part); generated objects and PUT write the ETag; the R2 direct upload writes NULL for a multipart one (R2 gives no whole MD5). `key_reencrypt_whole` keeps the column.<br>• **Read:** `newIntegrityReader` takes `COALESCE(body_md5, single-part etag)`, so a `-N` object is verified like a single-part one. A Range still cannot be. | next prompt; it moves WP-PAY-1 to 095 as Prompt 12 did with 093 |
| RP14-7 | note | **Two scheduled runs fall after this session.** The 2026-10-19 01:30 `chunk_orphans` run (expected: 0 found, `unknown_age` absent) and the 05:30 `routing_truth` chunked sample (expected: 0 objects again, because prod has no live chunked object; after 04:08 the retired-manifest collector frees the 737 bench manifests). | the next session's prod read |
| RP14-8 | note | **The tee's CPU** is 0.87 s per GiB served on the box (1.24 GB/s per core) and 1.6 s on an M1 Pro. A whole GET at 450 MB/s costs about 35 % of one core. | none; measure if the box's CPU becomes a limit |
| RP13-1..4 | — | closed by #768 | — |
| WP-R8-3 (A.1, A.2), WP-R2-4, WP-R8-6 | — | closed by #769, #770, #771 | — |

## Gates

- `go test -race -count=1 ./...` on a private DB (`vaultaire_test_p13`, one process): every package ok on the Part 0, A.2 and Part B trees. For A.1, `./internal/api/...` was run (225 s).
  - The A.1 / B combined tree and the A.2 + B merge were checked locally (targeted packages) before #771 queued.
  - One early Part 0 run went to the shared `vaultaire_test` DB: the shell does not keep `DATABASE_URL` between calls. Its cleanups ran. Every run after that went through a wrapper that sets the DSN.
- golangci-lint v2.14.0: 0 on every tree. gosec with the workflow's flags: 0. `promtool check rules` on the box: 18 rules in `vaultaire-backends.yml`.
- Structure tests green: `TestSSEPaths_NeverBufferTheObject`, the new `TestSSEPaths_EveryOpenerAndSealerNamesTheObject`, `TestWholeObjectReads_GoThroughTheBlobAddress`, `TestEngineCalls_CarryTheTenant`, `TestChunkBlobCalls_GoThroughTheChunkStore`, `TestDataPathBounds_*`, `TestExactSizeReader_OnlyUserIsTheCopyPath`, `TestRegisterJobs_*`, `TestSQLLiteralsMatchSchema`, `TestEgressCopy_*`.
- **Existing tests changed for intended behaviour:** SSE-C without the key 403 → 400 in `TestGetObject_SSEC_RequiresKey`, `TestHeadObject_SSEC_RequiresKey`, `TestSSEC_AboveChunkThreshold_NotChunked` and `TestSSEC_Put_Get_Range_OverTheOldCap`; a marker's version id 404 → 405 in `TestVM1_R61_5_ASuspendedDeleteReplacesTheNullVersion_Whole`.
- **Other test changes:** `fixedBucketDriver.failPut` now returns an error carrying the HTTP status (an answer). The crypto codec tests pass the header as the AAD explicitly.
- `build-and-test` green on #768–#771 and on their merge groups. Deploy succeeded for each: `6d1a8ab`, `fb51d0f`, `2c3583a`, `2942a9b`. No flakes.
