# RP70 — the post-merge review of WP-EDGE-2 Part B (the Worker 0.3.0 as the codec's host)

*Reviewed 2026-10-27 (session RP70): #1002 (Part 0: the RP68 fixes), #1003
(one upload is one path under the lock), #1005 (the Worker 0.3.0:
`src/codec.js`, `src/seal.js`, the multi-module deploy, the lab proof),
#1004 (`cmd/tools/dedup-measure`), #1007 (the closeout note
`docs/reviews/WP-EDGE-2B.md`), read adversarially from the note's
§ Decisions D-EDGE-22…28, § Residuals RB3-1…9 and its two open questions,
against the merged diffs, the node tests, the lab proof's eight cases and
the SPEED bench (`bench-results/SPEED-2026-10-27.md`, #1008), which measured
the same path the same day. Prod state: the Worker 0.3.0 is deployed with
`edge_codec` OFF everywhere (no key version until F-14), so the write and
read paths customers touch are 0.2.1's with one new log line.*

## Verdict

**HOLDS at P2.** The pipeline is what the harness specified: the frozen
chunker cuts the golden boundaries through a `TransformStream`, the sealer
reproduces `SealWrappedChunk` byte-compatibly (the Go opener opens the
node-sealed blob, the HKDF-by-HMAC is RFC 5869 to the letter with Go's nil
salt), the index is asked per batch, misses are sealed and stored at the
signed length, hits are referenced, the commit carries the ordered list and
the body's MD5, and every failure after the body is read is `edge_error` and
a 503, never a silent fallback. The control-plane fixes land as RP68 asked.
Three P2s stand between this and a flag that can be turned on for a tenant:
a memory bound counted in chunks rather than bytes (RP70-1), the small-tail
race for uploads of ten parts or fewer (RP70-2, the note's RB3-2, more
frequent than written), and the wrong-key host the HEAD sample cannot see
(RP70-3, the note's RB3-1, confirmed with a cost-bounded fix). None of them
touches the format or the control plane's decisions. They are Prompt 46c's
Part 0.

## What was checked and holds

| Area | Checked | Holds |
|---|---|---|
| The cutter | `chunkStream`: every piece to `storedChunker.push`, each completed length cut from the bytes held (copied once), `finish` at flush, the held-bytes assertion, `free` in every exit; the golden 51 boundaries through it in node | yes |
| The sealer | `seal.js`: random 32-byte key, `sealTo(pub, key, "stored/edge-codec/chunk-key/v1", null)` = 1169 bytes asserted, the header `[0x06][0x03] ‖ version ‖ wrap ‖ 1 MiB ‖ 7 random`, DEK = HKDF(key, no salt, "stored/edge-codec/stream-v1" ‖ header) by HMAC (PRK = HMAC(32 zeros, key), OKM = HMAC(PRK, info ‖ 0x01) — exactly `hkdf.New(sha256.New, key, nil, info)`), nonce = prefix ‖ i u32 BE ‖ last byte, AAD = header; the keys zeroed after use; `TestEdgeCodec_NodeSealedChunkOpens` opens it in Go | yes |
| The pipeline's order | `emit` → hash → `seen` (a repeat is a hit the commit settles after its first occurrence — the commit runs in body order and `referenceEdgeChunk` needs a ready row, which the stored first occurrence has become) → batch → `storeBatch` per miss: seal, PUT with the signed headers minus `content-length` (the body's own), the backend's ETag kept; the commit's `chunks` in body order with `body_md5` | yes |
| Back-pressure (D-EDGE-22) | the semaphore is acquired before a chunk is hashed and released after its PUT or its hit; a batch is flushed before the reader would wait on the bound; peak ≤ 4 asserted in node with 50 ms PUTs | yes — in chunks; RP70-1 is the byte count |
| The 503 rule | `runCodec` throws after `commitEdgeError`; `serveCodec` answers `slowDown`; before the body is read (the modules failed to load) the admit is failed and `fetch(request)` passes the untouched request on | yes |
| `commitOK` | one retry of a lost answer after 2 s, idempotent on the admit id; 404/410 = no answer; `null` → 503 | yes |
| `CODEC_TAMPER` (D-EDGE-28) | refused at the first request on `ENVIRONMENT = "production"` (the whole Worker then passes through); only `etag` / `key`; the node test pins it | yes |
| Logs | counts, sizes, ids, latencies — no hash, no key material (`TestIndexHoldsNoPlainHash` covers the origin's half) | yes |
| The deploy (D-EDGE-23/24) | six modules each byte-identical to its manifest row, the rows in prod's log (entries 85–88), `make edge-wasm` by the client_build recipe, `repro-check-clients` OK; `limits.cpu_ms = 300000` for the chunker's ~11 s per GiB | yes |
| RP68-1 (#1002) | `placementRequest.EdgeCodec` lifts the size term only; the grant's threshold = `EDGE_MIN_BYTES`; a PUT the origin stores whole is re-decided once with the flag on; an SSE bucket's write becomes codec with keys and passes through without them (`placement_whole_sealed`); a part of an upload already on the edge is never `small` | yes |
| RP68-2 (#1002) | `referenceEdgeChunk` polls a young claim 25 ms → 1 s for ≤ 60 s on the commit's clock, a dropped or aged claim is `errEdgeChunkVanished` at once; the lab's case (e): 0 × 503 on 300 MiB of one repeated block | yes |
| RP68-3 (#1002) | the first stored chunk and every 32nd HEADed through `chunkStore.head` → `engine.ObjectHeader` on the two edge writers, ETag + size compared, a missing blob is `head_mismatch`, a backend that cannot be asked is the host's 503; `routing_truth` opens `AES256-EW` chunks (`unreadable`); the lab's (g) etag case refused | yes — RP70-3 for what a HEAD cannot see |
| RP68-4/5/6 | the outcomes by stage; the reaper's `claims_dropped` (live in the lab's (f)); `chunk_move` verifies codec rows by MD5; a hit's size is its row's | yes |
| #1003 | `uploadHasStagedParts` counts a staged reservation in flight; `reserveUploadPart` refuses a codec upload; `settleUploadPartWith` refuses under the lock when the upload became codec meanwhile and gives the reservation back | yes — RP70-2 is what the lock cannot decide |
| `dedup-measure` | per-tenant unique vs global unique over the frozen chunker; the test's 0.4 exactly; `-dir` or a bucket with credentials from the environment; never writes; not in the product's dependency graph | yes |
| The bench agrees | the SPEED bench's section 9 ran this Worker on prod (bench tenant, `edge_ingest` on, codec off): 864 parts admitted, 0 on SLC, MD5-exact; its section 10 measured the codec's cost class (JS touching bytes ≈ 0.75 s CPU per 64 MiB) which the lab's (h) confirms at 26 MB/s per stream on a home line | yes |

## Findings

| # | Sev | Finding | Where | Fix (Prompt 46c Part 0 unless said) |
|---|---|---|---|---|
| **RP70-1** | **P2** | **The in-flight bound counts chunks, not bytes, and the sealed copy exists whole.** `CODEC_IN_FLIGHT = 4` holds up to four chunks between hash and PUT; each is up to 16 MiB of plaintext (`ChunkMaxSize`) plus its whole sealed copy (`sealChunk` allocates `out` of the full sealed length before the PUT starts, and `it.data` is nulled only after `sealChunk` returns) — 4 × 32 MiB = 128 MiB, the isolate's entire limit, before the chunker's own Go heap, the pending pieces and e2ee.wasm. Chunk sizes are content-defined: a body with no boundary inside 16 MiB — runs of zeros, sparse VM images, encrypted archives whose fingerprint never hits — cuts 16 MiB chunks throughout. The lab's (e) repeated a 4 MiB block (boundaries inside it) and (h) saw 32–66 MB of process RSS on a random body; no case had max-size chunks. Such an object fails with `Exceeded memory` on every attempt, and the client's retry fails the same way. | `codec.js:117` (`CODEC_IN_FLIGHT`), `:239–258` (`emit`), `seal.js:83–104` (`out` whole); RB3-6 | Bound by BYTES: a `Semaphore` of plaintext bytes (48 MiB) acquired per chunk size, and stream the sealed body — `fetch(url, {body: readable})` where the readable yields the header then each segment's ciphertext as it is encrypted under a `FixedLengthStream(sealed_length)`, so no sealed copy exists whole and the plaintext is released segment by segment. Lab cases: 256 MiB of zeros, a 1 GiB sparse VM image; the peak reported by the semaphore in bytes; the dashboard's `Exceeded memory` count after the next content deploy (RB3-6). |
| **RP70-2** | **P2** | **The small-tail race (RB3-2) hits every upload of ten parts or fewer.** aws-cli sends its first ten parts at once. The tail under `EDGE_MIN_BYTES` is the origin's (D-EDGE-5) unless the upload is already on the edge (#1002); for a file of ≤ 10 parts — 8–80 MiB at the 8 MiB default — the tail's admit can run before any codec part has marked the upload, so it passes through as `small`. Then (#1003) either its stager reservation lands first and the WHOLE upload is one staged path through SLC, or a codec part marks first and the tail's install is refused 400 — which aws-cli does not retry: the upload fails. Two outcomes, neither the edge, decided by a race. With `edge_codec` ON this is the common file size of every aws-cli user. | `edge_ingest.go:528–535` (`edgeReasonSmall` for a part), `edge_multipart.go` (`edgeUploadPart`), the note's open question 1 | **D-EDGE-29: every UploadPart of an edge tenant is the edge's whatever its size** — `EDGE_MIN_BYTES` gates PutObject only (a part is never alone: its upload is the unit the floor was meant for). The tail then takes two JSON round trips and becomes a codec part of ≤ 4 MiB (one or two chunks). Lab: a 50 MiB file at 8 MiB parts ten times, every run edge, never 400, never staged. |
| **RP70-3** | **P2** | **A host that seals under the wrong key passes the commit (RB3-1, confirmed).** The HEAD sample compares the backend's ETag and size to the host's report; a blob sealed under another public key has an ETag and size of its own that match. The object commits, reads 500 (`the chunk key does not unwrap`), and every later dedup hit on those chunks inherits it; `routing_truth` finds it within a day. The note asked for a GET + open of the first stored chunk; at 8 MiB parts that is up to 16 MiB back through SLC per 8 MiB committed — a quarter of the bytes, which defeats the plane. | `edge_codec_control.go` `headEdgeChunk`; the note's open question 2 | **Open the header and the FIRST SEGMENT, not the chunk:** one ranged GET of `StreamHeaderWrapped + 1 MiB + 16` bytes of the first stored chunk, the wrap opened under the tenant's codec seed, the first segment's GCM tag verified (`OpenWrappedChunk` does both in `prime`). ≤ 1.05 MiB per commit whatever the part size — 1.6 % of a 64 MiB part, 13 % of an 8 MiB one, which the client-settings page already steers away from. It proves the host sealed under the right key and the stream is well-formed; the keyed-hash binding of the whole plaintext stays with the read path and `routing_truth`. The ETag + size HEAD stays for the sampled rest. Lab: the (g) `key` case refused at the first commit. |
| RP70-4 | P3 | **Index calls per ≤ 4 chunks** (D-EDGE-22 + RB3-3): under back-pressure a batch is at most the bound, ≈ 400 calls per GiB at p50 60 ms, each a transaction, an advisory lock and a presign per chunk on the origin. RP70-1's byte bound raises the batch to ~24 average chunks by itself; the origin's half is RP68-7. | `edge_codec_control.go:277–322` | One transaction per index call for its claims (the locks taken in hash order, the inserts batched); the Worker side needs nothing beyond RP70-1. Measure: calls per GiB and p50 before and after. |
| RP70-5 | P3 | **RB-9's fix is one line and not yet made**: the first pass-through after a content deploy loses `range` and `if-none-match` to Cloudflare's cache layer (the capture is conclusive). | `index.js` `fetch(request)` at every pass-through | `fetch(request, { cache: "no-store" })` for every request the Worker passes on (PUTs included; harmless); the capture armed at the next content deploy proves it. |
| RP70-6 | P4 | A hit keeps its plaintext referenced (`it.data`) until the batch's `Promise.all` settles; a batch of 64 hits in the pre-RP70-1 shape could hold 64 chunks. | `codec.js` `storeBatch` | `it.data = null` on the hit branch; moot once the bound is in bytes. |
| RP70-7 | P4 | A commit the control plane answers with 400 (a malformed list, a size mismatch) returns `null` from `commitOK` and the client gets a 503 with Retry-After — the retry will fail the same way, so the client should see the 400. | `codec.js` `commitOK` | Relay a 4xx answer's XML (it is the S3 error the control plane wrote) instead of `null`. |
| RP70-8 | P4 | An object the edge stores chunked that the origin would store whole (4–64 MiB, RP68-1) is a chunked source for CopyObject: a copy into a versioned bucket is 501 (`s3_copy.go`'s rule), and a versioned bucket's own writes under `standard_sealed_default` are the origin's unless `versioned_chunking` is on (placement: `!versioned`). A property, for Prompt 48's design. | `placement.go:171`, `s3_copy.go` | Say it in Prompt 48 (b); the versioned bucket's path under sealed-default is a decision there. |
| RP70-9 | note | `CODEC_TAMPER` ships in the deployable source (D-EDGE-28). The refusal is pinned and runs per request; accepted, as the note decided. | `index.js` `codecTamper` | — |

## The residuals, re-checked

- **RB3-1 → RP70-3** (P2, fix bounded to ~1 MiB per commit). **RB3-2 → RP70-2** (P2: more frequent than "a tail under 4 MiB", it is every ≤ 10-part upload). **RB3-3 → RP70-4.** **RB3-9 → RP70-5.**
- **RB3-4** (the lab's throughput was the Mac's uplink): the SPEED bench answers it — 26–29 MB/s per stream in workerd for any JS-touching shape (its section 10), so the codec's per-stream rate is the pipeline's, not the Mac's, and aggregate scales with parts in flight as the mover's does.
- **RB3-5** (an empty last part passes through and the stager refuses it): stands; D-EDGE-29 makes an empty part the edge's too (a part of 0 bytes commits with an empty chunk list — add the case).
- **RB3-6** (local RSS only): RP70-1's lab cases and the dashboard count.
- **RB3-7** (2.6 MB bundle, ~55 ms Go boot per cold isolate): stands, informational.
- **RB3-8** (`encodeBody: "manual"` not run — RP66-1's residual): still owed; a Step 0 probe in 46c.
- **RB2-1…5** stand as RP68 left them.

## What the SPEED bench adds to the next prompt

The bench (#1008) measured this Worker and its backends the same day. Its "What changes" are inputs here, not findings: **aws-chunked bodies can be admitted with no decoder** when the control plane mints a header-signed streaming-trailer request (iDrive verifies the trailer, Lyve does not; iDrive refuses the query-signed form) — RE-2's close; **reads double with a windowed reader** (16 MiB ranges, ≤ 6 in flight) at ~1 s of CPU per GiB — RB-1's close; **the origin's uploader goes to 32 MiB × 8** (+29 % Lyve, +42 % iDrive; `S3SinglePutMaxBytes` moves with it); **`GEYSER_GET_CONCURRENCY` 64** is an owner `.env` change; **the second copy at the edge is a tee** and RS is the node's — Prompt 48's input. The node (Prompt 47) is not a speed project any more: its case is Cloudflare-independence, RS and GLACIER off SLC, and BYO hosts.

## For Prompt 46c, Part 0 — in this order

1. **RP70-2** (D-EDGE-29) — a one-line gate change with the largest customer-facing effect; first.
2. **RP70-1** — the byte bound and the streamed seal; the lab's zero-run and VM-image cases.
3. **RP70-3** — the first-segment open at commit.
4. **RP70-5** — the no-store pass-through, and the RB-9 capture at the deploy that ships it.
5. **RP70-4**, **RP70-6**, **RP70-7**.
