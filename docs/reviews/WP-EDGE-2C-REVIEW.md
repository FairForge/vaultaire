# RP72 — the post-merge review of WP-EDGE-2 Part C (the Worker 0.4.0: aws-chunked at the edge, the windowed reader, the RP70 fixes, the origin's uploader)

*Reviewed 2026-10-27 (session RP72): #1010 (Part 0: the RP70 fixes), #1012
(aws-chunked at the edge), #1013 (the windowed reader), #1011 (the
uploader at 32 MiB × 8), #1014 and #1015 (the closeout
`docs/reviews/WP-EDGE-2C.md`, the 0.4.0 deploy, RB-9's capture, the prod
proof), read adversarially from the note's § Step 0, § Decisions
D-EDGE-29…33, § Residuals RB4-1…7 and its three open questions, against the
merged diffs, the node and Go tests, the lab tables and the prod proof. Prod
state: the Worker 0.4.0 is live for every tenant with `edge_ingest` on
(tenant zero and the bench tenant); `edge_codec` is OFF everywhere.*

## Verdict

**HOLDS at P3 — the first review of this chain with no P2.** Every RP70 fix
landed as asked and two of them better than asked: the seal is streamed
segment by segment (no sealed copy exists whole), and the first stored chunk
is opened, which caught the wrong-key host at its first commit in the lab.
RB-9 is fixed and proven by the capture. aws-cli's default upload moves at
the edge with no byte on SLC, proven on prod with tenant zero's own bucket;
the windowed reader reads at the client's line. Three P3s and four P4s
remain, all narrow, and two of the note's open questions get an answer
below. The three P3s are Prompt 47's Part 0.

## What was checked and holds

| Area | Checked | Holds |
|---|---|---|
| RP70-1 (bytes, streamed seal) | `Semaphore` in bytes, FIFO, a chunk over the bound admitted alone; `Pending.cut` hands a chunk as 1 MiB segment buffers; `sha256Hex` over `crypto.DigestStream` in workerd; `sealChunkStream` seals a segment when the PUT pulls it (highWaterMark 0), drops its plaintext and releases its bytes; the PUT body through `FixedLengthStream(sealed_length)`; the batch flushed before the reader would wait; the lab's 16 × 16 MiB constant runs held exactly 48 MiB; Step 0.4 measured no cost of its own | yes |
| RP70-2 (D-EDGE-29) | `EDGE_MIN_BYTES` and `empty` gate PutObject only; an empty part commits with an empty list; `edgeUploadPart` deleted; the lab's ten 50 MiB uploads at 8 MiB parts: 70/70 admitted, no 400, no staged tail | yes |
| RP70-3 (the open at commit) | `openEdgeChunkPrefix`: one ranged read of header + first segment + the opener's look-ahead byte through `chunkStore.getRange` → `GetRangeInfo`, ETag and whole size compared, `OpenWrappedChunk`'s `prime` under the admit version's codec seed; a stream error or EOF is `head_mismatch`, a backend that cannot be asked `unverified`; the (g) `key` tamper refused at chunk 0 | yes |
| RP70-4 (one transaction per call) | `claimEdgeChunks`: the admit row FOR UPDATE, the advisory locks in keyed-hash order (two calls sharing chunks cannot deadlock), the claims recorded in the same transaction, one commit, then the presigns; a link that cannot be made drops every claim of the call | yes |
| RP70-5/6/7 | `passThrough` at all seven sites, pinned; a hit releases its bytes at once; a control plane's own 4xx is the client's 400 (RP72-3 for the 401/403 case) | yes |
| aws-chunked, the mover | `classify` admits `STREAMING-UNSIGNED-PAYLOAD-TRAILER` with a decoded length and nothing else of the streaming family; `edgeStreamingOf` requires the aws-chunked encoding, a verified trailer name and a decoded length; `signStreaming` signs the four headers, the date and the host, never content-length, with S3's unescaped canonical path (the `@` lesson, pinned); `edgeHeaderSigns` refuses a signature that signs less or more; `moveInit` forwards the body untouched, chunked when the client sent no length; the commit compares the backend's ETag with a Content-MD5 when one was sent | yes |
| D-EDGE-33 | a PutObject is header-signed only where `SignatureMaxAge > 0` (Lyve); an iDrive-bound aws-chunked PutObject is `aws_chunked_unbounded` and the origin's; a part's signature dies with its upload; wasabi offers nothing | yes |
| aws-chunked, the codec | `awsChunkedDecoder`: the framing state machine with bounded lines, data spans passed as views, CRC64NVME (reflected 0x9A6C9329AC4BC9B5, slicing-by-8, big-endian trailer) and CRC32 pinned to the standard check values and the SDK's vectors; the decoded length compared; a mismatch is `checksum_mismatch` → the client's 400 with everything given back; a framing error an `edge_error` | yes |
| The windowed reader | the admit answers a window only with `features: ["window"]` (an older Worker gets Part B's link); the link signs if-match where provable and no range (`EdgePresign.Window`; a link signing neither is made only for a window); the first range decides as Part B's GET; a later range of another status, length or ETag cuts; one commit at the end with `bytes_sent`; the isolate's shared 96 MiB budget, given back before a cut read waits; `settleEdgeGet` meters `bytes_sent` and charges the breaker on a cut | yes |
| The uploader | 32 MiB × 8; `S3SinglePutMaxBytes` follows; the allocation row updated; the per-64 MiB deadline unchanged (a 32 MiB part gets 30 s) | yes — RP72-1 is what the moved cutoff does to older rows |
| The deploy | seven modules byte-identical to their rows (95–98 new), the repro check OK; RB-9's capture: both probes NoSuchKey, `range` and `if-none-match` present at the origin | yes |
| The prod proof | 29 of 29 default aws-cli parts admitted header-signed to Lyve, 0 PUT lines on the origin, MD5-equal; the windowed download MD5-equal three times, metered exactly; 1.16 s of CPU per GiB | yes — RB4-3 (all 29 `admitted_unverified`) |
| Step 0.1 (RB3-8) | `encodeBody: "manual"` cannot help: the runtime decodes the subrequest before the Worker sees it; RP66-1's pass-through stands and RB3-8 is closed | yes |

## Findings

| # | Sev | Finding | Where | Fix (Prompt 47 Part 0 unless said) |
|---|---|---|---|---|
| **RP72-1** | P3 | **Every edge read of a 16–32 MiB object written before #1011 goes through SLC, for ever.** The origin's ETag is always the body's MD5 (Architecture Decision 3), while the backend's ETag for such an object is the manager's `-2`. D-EDGE-10 signs If-Match on "one backend PutObject ≤ `S3SinglePutMaxBytes`"; the constant is now 32 MiB, so a 20 MiB row written before 2026-10-27 19:52 UTC is taken for a single PUT, the link signs its MD5, the backend answers 412 and the origin serves it — on every read, not once (RB4-6 says "the origin serves it" as if that ended it). No breaker charge, no error, just 2× SLC for those objects. | `edge_get.go` `edgeGetIfMatch`, `drivers.S3SinglePutMaxBytes` | Now: the rule takes the cutoff that was in force when the row was written — a row with `created_at` before the #1011 deploy uses 16 MiB (one dated constant, `s3SinglePutCutoffBefore`). Properly: record the backend's ETag on the head row at every write (RB-2 of Part B; a column in Prompt 47's migration 112) and sign If-Match from it, never by inference. |
| **RP72-2** | P3 | **A mover aws-chunked write's length is enforced by the backend's handling of `x-amz-decoded-content-length`, which nobody has measured on Lyve.** The signature covers the declared decoded length; a body that decodes to another length lands on a backend that does not enforce it with the row's `content_length` wrong (the reservation and the row say one thing, the blob another). Without a Content-MD5 nothing at commit compares a size. The bench's § 1 tested wrong checksums and bodies of the right length, not a wrong decoded length. | `edge_ingest.go` the aws-chunked admit; `settleEdgeAdmit` | Probe both backends with a decoded length off by one (±1 byte, both ways); `EdgeStreamingProps` gains `EnforcesDecodedLength`; where it is false the commit HEADs the object (one `ObjectHeader` call, which both edge writers have) and refuses a size that is not the admitted one (`IncompleteBody`, everything given back). |
| **RP72-3** | P3 | **A control plane's 401/403 at commit is answered to the client as "retrying unchanged will fail the same way".** `commitOK` maps every 4xx of the control plane's own to a 400 `InvalidRequest`; a 401/403 is the service channel's (the token, the peer rule), not the client's, and is transient from the client's view. | `codec.js` `commitOK` | 401/403 → the Worker's 503 SlowDown; 400 and 422 → the 400. One branch, one node test. |
| RP72-4 | P4 | A non-last UploadPart under 5 MiB on the edge-whole path (D-EDGE-29 admits it) is a real part of the backend's multipart; the backend's Complete refuses it (`EntityTooSmall`) where the origin's stager would have assembled anything. aws-cli, rclone and the SDKs never send one; a hand-rolled client could. | `edge_multipart.go`, `completeEdgeUpload` | Relay the backend's `EntityTooSmall` as the client's 400 at Complete (check it is not folded into a 500) and say in `docs/API.md` that the S3 minimum applies. |
| RP72-5 | P4 | A read whose client disconnects mid-window keeps fetching ranges until a write fails (the pump notices at the next `writer.write`), up to the window's 96 MiB more from the backend. | `index.js` `serveWindow` | Watch `request.signal` (workerd fires it on client disconnect) and stop launching ranges; what was sent is still metered. |
| RP72-6 | P4 | RB4-5 stands and is now two budgets in one isolate: a codec write's 48 MiB and a window's 96 MiB can meet in one isolate at 144 MiB. Moot while `edge_codec` is OFF; the fix is one isolate-wide byte budget before the flag goes on. | `codec.js`, `index.js` | One `Semaphore` for both, sized 96 MiB. |
| RP72-7 | P4 | Tenant zero's `lightroom-backups` is on Lyve: every default aws-cli upload to it is `admitted_unverified` (29 of 29 in the proof). rclone sends Content-MD5 and is verified; aws-cli is not until the codec path carries it. | the proof, D-EDGE-30 | `docs/API.md` already says it; add the rclone sentence and the per-backend table. An owner choice between iDrive for that bucket and waiting for F-14. |

## The residuals and open questions, re-checked

- **RB4-1** (local RSS is not the bound): stands; the isolate's own count after `edge_codec` goes on is the measure.
- **RB4-2** (iDrive's non-expiring header signatures on parts): stands as bounded by D-EDGE-33 and the reaper's 48 h abort.
- **RB4-3** → RP72-7. **RB4-4** (the window's lab gain): the prod proof's numbers are the client's downlink on both paths, and the Worker took the object from Lyve at 83 MB/s; the edge-to-edge gain is the bench's 2.0×. Closed. **RB4-5** → RP72-6. **RB4-6** → RP72-1 (it is permanent, not a one-off). **RB4-7** (aws-cli trailers over TLS only): the prod proof covered the real shape; closed.
- **Open question 1 (D-EDGE-33, iDrive-bound aws-chunked PutObject):** keep it at the origin. A 4–8 MB single PUT is what it costs; a non-expiring bearer for a fresh blob address is a standing hole for a saving of one small object per upload. If it matters later, the answer is a Content-MD5 the client sends (rclone) or the codec path, not a wider signature.
- **Open question 2 (D-EDGE-30, Lyve-bound unverified):** keep admitting and counting. The origin verified nothing for these writes either, and RP72-2's HEAD closes the length half; the checksum half is the codec's after F-14. Say the per-backend truth in the docs (RP72-7).
- **Open question 3:** recorded; the review's premise about zero runs was wrong and the note's correction stands (zeros cut at the 1 MiB minimum and dedup; constant non-zero runs are the max-size case).

## For Prompt 47, Part 0 — in this order

1. **RP72-1**: the dated cutoff now; `backend_etag` on the head row in migration 112 (with the data-plane hosts table) and If-Match from it.
2. **RP72-2**: the decoded-length probe on both backends; `EnforcesDecodedLength`; the HEAD at commit where it is false.
3. **RP72-3**, then **RP72-4**, **RP72-5**, **RP72-6** (one line each; RP72-6 stays dark with the flag).
