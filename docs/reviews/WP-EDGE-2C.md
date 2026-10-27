# WP-EDGE-2 Part C — the Worker 0.4.0: aws-chunked at the edge, the windowed reader, the RP70 fixes, the origin's uploader (Stage 1a Prompt 46c)

*Plan 40.10, slices 1 and 2 widened.*
- *Part 0 = the RP70 review's fixes (`docs/reviews/WP-EDGE-2B-REVIEW.md`).*
- *Then RE-2's close: aws-cli's DEFAULT upload moves at the edge.*
- *RB-1's close: large reads fan in.*
- *The SPEED bench's uploader constants (`bench-results/SPEED-2026-10-27.md` §§ 1, 4, 8).*

*Prompt: `docs/reviews/WP-EDGE-PROMPTS.md` § Stage 1a Prompt 46c (the Opus edition was the session's). Session 2026-10-27. No migration (next: 112). The post-merge review is RP72.*

Contract lines cited:
- **4** (keys only through the one hierarchy). The commit opens a stored chunk under the codec seed `TenantKeys.EdgeCodecPrivateKey` derives from the tenant's key version. A header signature is computed on SLC from the backend key and travels like a presigned URL. The Worker holds no key but the admit's public one and its per-chunk random keys.
- **5** (no global convergent key). Claims, index keys and codec keys stay per tenant and per version.
- **6** (dedup by a keyed index). The index call keys every hash in memory as before, now in one transaction; the codec decodes aws-chunked before the chunker, so the keyed index sees decoded bytes.

## What was built

| PR | What | Tests (red first) |
|---|---|---|
| **#1010** Part 0 (merged `7874b1bd`, deployed) | RP70-2 / D-EDGE-29 (every UploadPart of an edge tenant is the edge's); RP70-3 (the first stored chunk OPENED at commit); RP70-4 (one transaction per index call); RP70-1 (the bound in bytes, the seal streamed); RP70-6; RP70-7; RP70-5 (`passThrough`, `cache: "no-store"`); lab cases `p0_parts`, `p0_bytes` | `TestEdgeCodecMultipart_EveryPartIsTheEdges`, `TestEdgeCodecCommit_FirstStoredChunkIsOpened`, `TestEdgeIndex_OneTransactionPerCall`, node: the byte semaphore, back-pressure in bytes on 16 MiB chunks, `sealChunkStream` = `sealChunk`'s bytes, hits released at once, the 4xx relay, `test/passthrough.test.js` |
| **#1012** PR A (merged `ccd98dd8`) | aws-chunked at the edge: `EdgeWriter.SignPut` / `SignUploadPart` / `EdgeStreaming()`, the admit on the decoded length, `headers` + the plain URL, D-EDGE-30/33; the codec's decoder `src/awschunked.js` (client `edge-awschunked`), D-EDGE-31; `XAmzContentChecksumMismatch`; the signer's unescaped canonical path; `lab/awschunkedput.go`; docs/API.md | `TestEdgeSignPut_HeaderSignedStreamingTrailer` (the SDK's own signer reproduces it, a key with `@`), `TestEdgeAdmit_AWSChunkedIsHeaderSigned`, `…OnAnUnboundedBackend`, `TestEdgeCodecCommit_ChecksumMismatchIsTheClients400`, node `test/awschunked.test.js` |
| **#1013** PR B | the windowed reader, D-EDGE-32: `EdgePresign.Window`, the admit's `window` (only with `features: ["window"]`), `bytes_sent`; the Worker's `serveWindow`, one window budget per isolate; the #1011 merge-group flake fixed | `TestEdgeAdmitGet_WindowForLargeReads`, `TestEdgeCommitGet_WindowMetersWhatWasSent`, `TestEdgePresignGet_Window`, node: order / in-flight / cut / fallback / shared budget / no hang |
| **#1011** PR C | the S3-class uploader at 32 MiB × 8, `S3SinglePutMaxBytes` with it, the allocation row | `TestS3Upload_ThirtyTwoMiBByEightAndTheSinglePutCutoffMovesWithIt`, `TestDataPathBounds_…` (the row names the constants), `…AThirtyTwoMiBPartGetsThirtySecondsAtTheDefault` |
| PR D (this) | this note, STATUS, plan 40.10, CLAUDE.md, docs/API.md, the README (0.4.0), the 0.4.0 manifest rows, Prompt 47 refreshed, 46c DONE in the chain | — |

**[YOU]** `GEYSER_GET_CONCURRENCY=64` in prod's `.env`, then `sudo vaultaire-switch restart`. It gives +81 % on a landing-zone Vault read (the SPEED bench § 8: 47 MB/s for 4 GiB against 26 at 32); it holds up to 64 × 8 MiB per Vault GET against `GOMEMLIMIT=40GiB`. This session did not touch prod's `.env`.

## Step 0 — the probes (run before PR A)

| # | Question | Answer |
|---|---|---|
| 0.1 | `encodeBody: "manual"` + an un-decoded subrequest for a stored-`Content-Encoding` row (RB3-8, RP66-1's residual): servable at the edge? (a throwaway Worker `sb-p46c-probe` on the bench account; a 7,000,000-byte text stored gzip-encoded — 20,683 B, `Content-Encoding: gzip` — on iDrive us-central-1 and Lyve us-west-1; a presigned GET with `cache: "no-store"`, the backend's body passed on) | **No.** The runtime DECODES the subrequest's body before the Worker sees it: at the Worker the backend's `Content-Length` is already gone, on both backends, with or without `accept-encoding: identity` on the subrequest. `encodeBody: "manual"` only governs the Worker's own response, so the client never gets the stored bytes. It gets the 7 MB plaintext with no `Content-Encoding` when it asks for identity, and Cloudflare's own re-compression (57–62 KB of other bytes, labelled gzip) when it asks for gzip — on both backends, `manual` or default. A row with a stored `Content-Encoding` stays the origin's, as RP66-1 decided. RB3-8 is closed by this answer. |
| 0.2 | The CRC64NVME table implementation's rate in workerd, and CRC32's (slicing-by-8 over two uint32 halves, `src/awschunked.js`; 64 MiB per call, `cpuTime` from `wrangler tail`) | **CRC64NVME 225–350 MB/s per CPU-second, CRC32 255–528** — three calls each. These are floors: each call also allocates and fills its 64 MiB. In node on the M1: 707 / 926 MB/s. That is well above the chunker's ~95 MB/s, so the decoder adds ~20–40 % to the codec's CPU per byte. |
| 0.3 | How far may a HEADER-signed `STREAMING-UNSIGNED-PAYLOAD-TRAILER` PUT's `x-amz-date` drift? (aws-sdk-go-v2's own `v4.NewSigner().SignHTTP`, an aws-chunked body with a CRC32 trailer sent `Transfer-Encoding: chunked`, `t-_lab/p46c-clock/*`, deleted after) | **Lyve us-west-1: ±15 minutes exactly** — −14 and +14 min 200, −15.5 / −16 / −20 / −60 and +16 min 403 `RequestTimeTooSkewed`. **iDrive us-central-1: NO clock check** — every offset 200, −24 h and **−7 days** included, while a wrong secret is 403 `SignatureDoesNotMatch` (the signature IS verified; the date is not). A PRESIGNED PUT is bounded on both (`X-Amz-Expires=2` used after 6 s: 403 on iDrive and Lyve). So a header signature is a 15-minute bearer on Lyve, as the prompt assumed, and a **non-expiring** bearer on iDrive for "PUT any bytes to this one key" — D-EDGE-33. Both backends store the decoded body (ETag = the MD5 of the decoded bytes). |
| 0.4 | The streamed-seal PUT: does `fetch` with a `FixedLengthStream` body of 16 MiB reach the backend at pass-through speed? (`sb-p46c-probe` from SLC: `/pass` forwards the body to a presigned PUT; `/seal` reads it, cuts 1 MiB segments and PUTs the 0x06 stream of the Worker 0.4.0's `sealChunkStream` through a `FixedLengthStream(16,778,658)`; three runs per backend) | **Yes — no cost of its own.** Both backends answered 200 to all six streamed-seal PUTs. Once the plaintext is in the Worker, the sealed 16 MiB reaches the backend in 175–651 ms on iDrive and 102–691 ms on Lyve: 24–164 MB/s on the Worker → backend leg. The pass-through's PUT, which also waits on the client's stream, took 477–717 ms and 696–1,019 ms. Client totals are 0.68–1.27 s for pass and 1.09–1.84 s for seal; the probe reads the whole body before sealing, so seal is serial, and the pipeline overlaps the two. The codec's per-stream rate is set by the chunker's CPU (~95 MB/s) and the client, not by the streamed PUT. |

## Decisions

- **D-EDGE-29 — every UploadPart of an edge tenant is the edge's, whatever
  its size** (RP70-2, decided by the review). `EDGE_MIN_BYTES` gates
  PutObject only; an empty PutObject stays the origin's (`empty`), an empty
  UploadPart is the edge's too (RB3-5) — a codec part of 0 bytes commits with
  an empty chunk list (`part_chunks = '[]'`, size 0) and Complete reads it
  back. `edgeUploadPart` (#1002's "a part of an upload already on the edge is
  never small") is deleted: the gate now never asks. A small tail costs two
  JSON round trips; the upload never splits between the stager and the edge.
  Tests: `TestEdgeCodecMultipart_EveryPartIsTheEdges`,
  `TestEdgeAdmit_SmallBodiesPassThrough` (its part case flipped).

- **D-EDGE-30 — a Lyve-bound aws-chunked write is admitted on its length.**
  Lyve does not verify the trailer. Such a write with no Content-MD5 counts
  `vaultaire_data_plane_admits_total{outcome="admitted_unverified"}`, a
  pre-created label. With a Content-MD5, the COMMIT compares the backend's
  ETag (the decoded body's MD5) with it, so the write is verified wherever
  it goes and counts `admitted` (`TestEdgeAdmit_AWSChunkedIsHeaderSigned`).
  `docs/API.md` says so. The origin verifies no trailer either (RE-6), so
  the edge is no worse than `stored.ge` for these writes.
- **D-EDGE-31 — the codec decodes aws-chunked framing and VERIFIES the
  trailer.** `src/awschunked.js` holds the framing state machine and the
  slicing-by-8 CRC64NVME / CRC32. Only those two trailers are admitted: any
  other trailer, signed chunks (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`), or a
  missing decoded length stays the origin's. A mismatch is committed
  `checksum_mismatch`: the client's 400 `XAmzContentChecksumMismatch`, the
  reservation given back, every claim dropped. A framing error is
  `edge_error` and a 503. On the mover path, iDrive's own
  `XAmzContentChecksumMismatch` is relayed as that code (it used to map to
  BadDigest). New closed-list client: `edge-awschunked`.
- **D-EDGE-32 — the windowed reader.**
  - A GET (or a Range) of ≥ 64 MiB is read as 16 MiB ranges, ≤ 6 in
    flight, written in order to a `FixedLengthStream` of the span. The
    link signs if-match where provable (D-EDGE-10) and NO range
    (`engine.EdgePresign.Window`; only a window may sign neither); under
    64 MiB, D-EDGE-11's exact-span link.
  - The first range decides as Part B's single GET does (fallback, no byte
    sent). A later range of another status, length or ETag than the
    first's CUTS the body.
  - ONE commit when the stream ends, not two: `served` with `bytes_sent`,
    or `error` with the bytes sent before the cut. The control plane meters
    what the client received and charges the backend's breaker.
  - Two safety additions beyond the prompt:
    1. **Features.** The admit answers a window only when the description
       names `features: ["window"]`. A 0.3.0 Worker would GET the range-less
       link whole, receive a 200, fall back and charge the breaker on every
       large GET between the server's deploy and the Worker's.
    2. **One window budget per isolate.** `WINDOW_ISOLATE_BYTES` = 6 × 16
       MiB, shared FIFO by every windowed read of an isolate: two concurrent
       reads at 96 MiB each would pass the 128 MB isolate.
  - A read longer than the admit's 20 minutes is reaped and metered at the
    admitted size, and its late commit is 410 (D-EDGE-14's trade).
- **D-EDGE-33 — a header-signed aws-chunked PutObject only where the backend
  bounds the signature's life** (Step 0.3).
  - Lyve: PutObject and UploadPart, ±15 min.
  - iDrive: UploadPart only. A part's signature dies with its upload:
    Complete names every part by the ETag its commit recorded, and a dead
    upload id is NoSuchUpload.
  - An iDrive-bound aws-chunked PutObject stays the origin's
    (`aws_chunked_unbounded`). aws-cli's multipart threshold is 8 MB, so
    that is a 4–8 MB single PUT; the lab's (vi) row shows it served.
  - A backend no probe measured (wasabi) offers nothing
    (`aws_chunked_backend`).
  - The minted signature uses S3's rule of an unescaped canonical path
    (`DisableURIPathEscaping`): the lab's first run was 403 on every part
    without it.
- **RP70-3 — the first stored chunk is OPENED at commit.** One ranged read
  (`chunkStore.getRange` → `engine.VersionedRangeGetter.GetRangeInfo`, which
  every edge writer has) of `min(sealed size, WrappedChunkSealedLength(1 MiB)
  + 1)` bytes: the header, the first segment and ONE more byte — the
  prompt's `StreamHeaderWrapped + 1 MiB + 16` is not enough for a chunk of
  more than one segment, because `OpenWrappedChunk`'s full-mode opener reads
  one byte past a segment to know it is not the last (without it the first
  segment is opened with the LAST flag in its nonce and fails). For a
  one-segment chunk the read is exactly the prompt's number (pinned by
  `TestEdgeCodecCommit_HeadSampleOfStoredChunks`); for a longer one +1
  (`TestEdgeCodecCommit_FirstStoredChunkIsOpened`). The read also compares
  the backend's ETag and the blob's whole size (what the HEAD covered), so
  the (g) `etag` case stays refused at chunk 0. Every 32nd stored chunk after
  it is still HEADed.
- **RP70-4 — one transaction per index call** (`claimEdgeChunks`): the admit
  row FOR UPDATE (still pending; concurrent calls of one admit merge their
  claims in turn), then per chunk in keyed-hash order the advisory lock, the
  row FOR UPDATE, the claim; the claims recorded on the admit in the same
  transaction; one commit; then the links presigned (no DB work). A link that
  cannot be made drops every claim of the call. `claimEdgeChunk` and
  `recordEdgeCodecClaims` are deleted (nothing else called them).
- **RP70-7 — a control plane's own 4xx at commit** (its JSON error: the
  commit was malformed) is the client's 400 InvalidRequest naming that
  error, never retried; a 200 is relayed whatever status it carries.
- **RP70-1 — the bound in BYTES, the seal streamed.** `CODEC_IN_FLIGHT_BYTES`
  = 48 MiB of chunk plaintext; FIFO; a chunk larger than the bound is
  admitted alone. To make "release each segment as it is consumed" TRUE (not
  only accounting), the cutter now hands a chunk as its 1 MiB segments, each
  its own buffer (`Pending.cut` copied once before too), the chunk is hashed
  over them with `crypto.DigestStream` (workerd; node's tests concatenate),
  and `sealChunkStream` seals a segment when the PUT reads it (highWaterMark
  0), drops its plaintext and releases its bytes. The PUT body goes through a
  `FixedLengthStream(sealed_length)` in workerd (a runtime without one — the
  node tests — sends the signed content-length with the stream).
  `sealChunk` (whole) stays for the Go cross-implementation vector; both
  produce identical bytes under an injected random (`test/seal.test.js`).
  The codec log line's field is `peak_in_flight_bytes`.
- **RP70-1's premise, corrected**: an all-zero run does NOT cut 16 MiB
  chunks — the frozen chunker cuts zeros at its 1 MiB minimum (and they
  dedup to one chunk); max-size chunks come from runs of a constant
  NON-zero byte (0xff, …) and other boundary-free content. The node test and
  the lab use 16 MiB regions of distinct constant non-zero bytes.
- **RP70-5 — `passThrough(request)`** = `fetch(request, {cache: "no-store"})`
  at all seven sites (kind pass, no env, the refused tamper, the admit's
  failure / non-200, `!admit.edge`, the codec modules failing to load, the
  read fallback), pinned by `test/passthrough.test.js`.

## Lab rows (Part 0, `lab/codec-proof.sh`, wrangler 4.151.3 dev --local, origin on :8099, iDrive us-central-1 bench pair)

| | Case | Result |
|---|---|---|
| (i) | `p0_parts`: 50 MiB at aws-cli's default 8 MiB parts (7 parts, a 2 MiB tail), ten uploads | parts admitted **+70 of 70**, `pass_through` +0, `pass_through_small` +0, `staged_parts` lines 0, failed runs or 400s in the aws-cli debug logs **0**; ten head rows, all `chunked`; read back MD5-equal (`cd5ec232…`) |
| (ii) | `p0_bytes` at 64 MB parts: 256 MiB of zeros; 256 MiB of sixteen 16 MiB constant runs (the chunker's maximum); a 1 GiB sparse image (`dd seek`, six 3 MiB random islands) | the Worker's `peak_in_flight_bytes`: zeros **2 MiB** (256 chunks of 1 MiB, 1 miss); runs **48 MiB** (16 chunks of 16 MiB, 16 misses — the bound, reached and held); sparse **5 MiB** (1,019 chunks, 15 misses); all three MD5-equal. workerd RSS (both processes, summed, sampled every 0.5 s): 539 / 912 / 750 MiB against ~64 idle — see RB4-1 |
| (iii) | `g key` (sealed under another public key, `CODEC_TAMPER=key`) | **refused at the first commit**: `head_mismatch` — "the first stored chunk does not open under the tenant's codec key: … the chunk key does not unwrap"; no head row; `vaultaire_edge_codec_chunks_total{outcome="head_mismatch"}` 1 (Part B's (g) key case was ACCEPTED) |

## Lab rows (PR A: aws-chunked at the edge)

aws-cli sends a trailer only over TLS: botocore's `httpchecksum.py` uses
trailers "over TLS" only. Against the lab's plain-HTTP origin it sends a
`x-amz-checksum-crc64nvme` HEADER and a plain body, so `lab/awschunkedput.go`
drives aws-cli's TLS shape: the SPEED bench's capture — 1 MiB chunks, a
CRC64NVME trailer, `Transfer-Encoding: chunked`, 8 MiB × 10 for a whole
upload. aws-cli's own default over TLS is the prod proof (§ The deploy).

| | Case | Result |
|---|---|---|
| (iv) | `pa_mover`: 100 MiB in aws-cli's aws-chunked shape, `edge_codec` OFF (the mover) | **13/13 parts 200**, every one header-signed. Admit counter: +13 `admitted` (iDrive verifies the trailer), 0 `admitted_unverified`, 0 pass-through. 0 parts staged on the origin. Complete `cc3de890…-13`; row on `idrive`, 104,857,600 bytes; MD5-equal. aws-cli itself with its DEFAULT config over the lab's HTTP: 13 parts admitted too, with a header checksum and a plain body, no trailer. |
| (v) | `pa_codec`: the same shape, `edge_codec` ON | 13/13 parts admitted codec, 13 codec requests, 0 mismatches; row `chunked` / `AES256-EW` `-13`; MD5-equal — the cutter and the MD5 saw decoded bytes |
| (vi) | `pa_trailer`: a 6 MiB body, the trailer's CRC with one bit flipped, the length right | codec PutObject: **400 XAmzContentChecksumMismatch** — the Worker's decoder, relayed from the control plane — and no row; the intact body 200 and a row. Mover UploadPart (`edge_codec` OFF): **400 XAmzContentChecksumMismatch** — iDrive's verification, relayed by the commit; the intact part 200. An aws-chunked PutObject bound for iDrive: passed to the origin (`aws_chunked_unbounded`, D-EDGE-33), which stored it — 200. |
| — | the first `pa_mover` run | every header-signed part **403 SignatureDoesNotMatch**: the minted signature escaped the blob address's `@` a second time (`%2540`). S3 signs the path as sent — the SDK's S3 signer sets `DisableURIPathEscaping` — and the bare `v4.NewSigner()` did not. Fixed and pinned: `TestEdgeSignPut_HeaderSignedStreamingTrailer` now signs `dir/a b@…`. Step 0.3's probe keys had no `@`, which is why it missed this. |

## Lab rows (PR B: the windowed reader)

A 2 GiB GoldenBody object, uploaded through the mover at 64 MB parts (row `idrive`, `-32`, so `if-match` is provable). It is read by `aws s3api get-object` (ONE GetObject) through wrangler dev, three times per shape, from the Mac (a home line, the dev server as an HTTP proxy). The single stream is the same origin built with the window threshold raised — a lab build, never committed.

| Shape | MB/s (3 runs) | workerd process CPU, s per GiB | Bytes |
|---|---|---|---|
| single stream (Part B's exact-span link) | 11.6 / 10.8 / 11.5 | 5.9 / 6.5 / 6.5 | MD5-equal ×3 |
| window 16 MiB × 6 (128 ranges, `served`, `bytes_sent` 2,147,483,648) | 16.4 / **19.9** / 16.6 | 4.3 / 4.1 / 4.4 | MD5-equal ×3 |

- The window gains **1.44× at the median and 1.73× best-to-best**: short of the 1.8× target on this link.
- The Mac's home line and the dev server's proxy are in both paths. The bench's 2.0× was the edge from SLC; the prod proof (§ The deploy) measures the edge.
- The CPU column is the whole local workerd process, proxy included. The single stream, where JavaScript touches no byte, costs MORE process CPU than the window, so the reader's own cost cannot be separated locally. Its edge number is the deployed Worker's `cpuTime`.

## Residuals

- **RB4-1 (P3) — the local memory number is not the plaintext bound.** The
  same single 64 MiB PUT of four 16 MiB runs, three times each, through
  wrangler dev: 0.3.0 (main) peaks at ~400 MiB of workerd RSS, this build at
  ~480 MiB, both from ~64–200 MiB idle and neither giving it back — the Go
  wasm heaps (the chunker's and e2ee's linear memories only grow) and the dev
  proxy's request buffers, which the plaintext bound does not govern and the
  local process cannot separate from an isolate. The codec's own held
  plaintext is ≤ 48 MiB by construction and measured; whether an EDGE
  isolate stays under 128 MiB with four concurrent 64 MiB codec parts is the
  deployed Worker's `Exceeded memory` count once `edge_codec` is on for a
  tenant (RB3-6 stands).

- **RB4-2 (P3) — iDrive's header signatures do not expire** (Step 0.3).
  An UploadPart's signature is a bearer for that part until its upload
  ends. A replay can only make Complete fail InvalidPart (the recorded
  ETag), never change a committed object. The PutObject case is closed by
  D-EDGE-33. A signature travels the service channel and lives in the
  Worker's memory for the request; nothing logs it.
- **RB4-3 (P3) — Lyve-bound aws-chunked writes are unverified unless the
  client sends Content-MD5** (D-EDGE-30). Tenant zero's `lightroom-backups`
  is on Lyve, so its default aws-cli uploads through the edge count
  `admitted_unverified`. The fix is the codec path (it verifies, D-EDGE-31),
  once `edge_codec` is on after F-14.
- **RB4-4 (P3) — the window's lab gain was 1.44× (median), short of 1.8×**,
  on a home line through the dev proxy. The edge number is the prod proof's.
- **RB4-5 (P3) — the codec's 48 MiB and the window's 96 MiB are separate
  budgets.** An isolate running a max-size codec write and a full window at
  once could pass 128 MB. One isolate-wide byte budget for both is the fix,
  once `edge_codec` is on for any tenant (it is OFF on prod).
- **RB4-6 (P4) — a 16–32 MiB object the origin wrote before PR C** was a
  backend multipart; its edge GET's If-Match of the MD5 is a 412, and the
  origin serves it (no breaker charge). PR C's constant moved the cutoff.
- **RB4-7 (P4) — aws-cli sends a trailer only over TLS** (botocore), so the
  lab's aws-cli runs over plain HTTP send a header checksum. The lab drives
  the TLS shape with `lab/awschunkedput.go`; the real default is the prod
  proof's.

## Open questions (for the owner)

1. **D-EDGE-33**: should an iDrive-bound aws-chunked PutObject be
   header-signed anyway (a non-expiring bearer for one fresh blob address
   until the key rotates), to move 4–8 MB single PUTs off SLC? Today: no.
2. **D-EDGE-30**: keep admitting Lyve-bound aws-chunked writes unverified
   (counted), or send them to the origin? Today: admitted. The origin
   verifies no trailer either (RE-6).
3. **RP70-1's premise**: all-zero runs cut at the 1 MiB minimum; max-size
   chunks come from constant non-zero runs. No action — a correction for
   the review record.

## The manifest rows and the deploy order

- The four new 0.4.0 rows name `7c3c1a2a` (#1013's merge on main, the final bytes of every module):
  - `edge-ingest` `d7489599…`
  - `edge-codec` `a0262b07…`
  - `edge-seal` `b211fcd4…`
  - `edge-awschunked` `2ff68eb5…`
- `scripts/repro-check-clients.sh` rebuilt each of them OK.
- `chunker.wasm`, `e2ee.wasm` and `wasm_exec.js` are byte-identical to their existing rows: `edge-chunker` 0.1.0, `sdk-wasm` 0.2.0, `edge-wasm-exec` 0.3.0.
- The rows reach prod's log at the server's boot after this PR deploys.
- The Worker 0.4.0 deploys AFTER that, with RB-9's capture armed. Its result and the prod proof are this note's follow-up section (the docs PR after the deploy).

## The deploy and RB-9

**Deployed 2026-10-27 21:32:50 UTC**: the Worker 0.4.0 from main `ce74b6d7`, version `0eddafac-424e-46ec-90ab-291e4ad8b900`, 9,210.53 KiB / 2,630.57 KiB gzipped. The server was already on `ce74b6d7`, which holds the rows.
- The rows were appended at that server's boot: **entries 95–98** (`edge-ingest`, `edge-codec`, `edge-seal`, `edge-awschunked` 0.4.0).
- `make edge-wasm` in a clean checkout, and `wrangler deploy --dry-run --outdir` before the deploy: all seven modules were byte-identical to their rows.
- The deployed script, downloaded back (`…/workers/scripts/stored-s3-edge/content/v2`), was byte-identical module for module:
  - `index.js` `d7489599…`
  - `codec.js` `a0262b07…`
  - `seal.js` `b211fcd4…`
  - `awschunked.js` `2ff68eb5…`
  - `wasm/chunker.wasm` `c5b1467a…`
  - `wasm/e2ee.wasm` `bc06f7a6…`
  - `wasm/wasm_exec.js` `0c949f49…`
- `edge_codec` stays OFF on prod (no key version until F-14). `/health` healthy, 21/21.

**RB-9 — fixed.**
- The capture was armed exactly as Part B's: `tcpdump -i lo -s0 -U tcp port 8001` (the active slot) before the deploy.
- The signed pass-through probe: aws-cli `get-object` with `--range bytes=0-1 --if-none-match '"abc"'`, the bench key, a nonexistent bucket. It fired at 21:32:52, about 2 s after the deploy returned, and again 20 s later.
- **Both answered NoSuchKey: the signature verified, no 403.** Part B's first probe after its content deploy was 403 SignatureDoesNotMatch.
- The capture shows both requests reaching the origin WITH `range: bytes=0-1` and `if-none-match: "abc"`. `fetch(request, {cache: "no-store"})` (RP70-5) is the fix.
- The pcap, its log and the scripts were removed from the box; tcpdump stopped.

## The prod proof (tenant zero, aws-cli's DEFAULT settings)

The Lightroom zip `lightroom-backups/old-lightroom-catalogs/LRViera23_2025-04-03 2051.zip` (242,911,992 B, on Lyve) was read from `stored.ge` (53.7 MB/s, MD5 `26c0d8ca…`). It was then uploaded to `lightroom-backups/p46c-proof/LRViera23.zip` through `s3.stored.cloud` with the Mac's `stored` profile, which carries only `region`: aws-cli 2.28.19's defaults.

| | Result |
|---|---|
| upload | aws-cli sent all **29 parts aws-chunked**, `x-amz-trailer: x-amz-checksum-crc64nvme`, Transfer-Encoding chunked. **29/29 admitted header-signed** (`admitted_unverified` +29: the bucket is on Lyve — D-EDGE-30, RB4-3), 29 committed. **0 PUT lines on the origin**: its log for the key holds only Create and Complete (two POSTs). 28.4 MB/s on the Mac's uplink. The row: 242,911,992 bytes, ETag `347535c4…-29` |
| download | `aws s3api get-object` (ONE GetObject: the windowed reader, 15 ranges) three times: **67.4 / 58.7 / 39.5 MB/s, MD5-equal** (`26c0d8ca…`). `vaultaire_data_plane_admits_total{op="get",outcome="admitted"}` +3; three commits each metering exactly 242,911,992 bytes. The same GET through `stored.ge`: 57.2 MB/s — the Mac's downlink is the limit on both paths |
| the edge's own numbers (`wrangler tail`, one more windowed GET) | **cpuTime 262 ms** for 242.9 MB ≈ **1.16 s per GiB** (target < 1.2); Worker wall 2,942 ms ≈ 83 MB/s from Lyve into the Worker |

The proof object was deleted after, with no upload left open.

## Teardown

- The lab (Part 0, PR A and PR B cases) was torn down after PR B's runs: the Worker and the origin stopped; 196 blobs of the lab tenant deleted from iDrive us-central-1 (`t-<lab tenant>/` 8, `t-_global/_global/_chunks/<lab tenant>/` 188) and its 3 open backend uploads aborted; `t-_lab/` empty on iDrive and Lyve; the DB `vaultaire_edgelab` dropped; the lab key, token and credentials removed.
- The Step 0 probe Worker `sb-p46c-probe` was deleted from the bench account; its six objects were deleted; `~/p46c-probe` was removed from SLC.
