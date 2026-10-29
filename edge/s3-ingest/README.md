# stored-s3-edge — the mover (WP-EDGE-1 Part A + Part B) and the codec host (WP-EDGE-2 Part B + Part C)

The Cloudflare Worker on `s3.stored.cloud` (plan 40.10 slice 1). Every S3
request arrives here. A signed **PutObject** or **UploadPart** of a tenant
with the `edge_ingest` flag, whose placement is a whole unsealed write of
at least `EDGE_MIN_BYTES`, is **admitted** by the control plane (`POST
/api/v1/edge/admit` on `VAULTAIRE_ORIGIN`: SigV4 verified on SLC, quota
reserved, a blob address minted, a presigned backend link that signs
`content-length` and the client's `Content-MD5`) and its body is streamed
by this Worker straight to the backend — never through SLC — and reported
at **commit** (`POST /api/v1/edge/commit`: the backend's answer, nothing of
the bytes), where the control plane writes the head row, the version row
and the bill exactly as a PUT through SLC would. A signed **GetObject**
(Part B: plain, or one Range) of such a tenant whose head row is a whole
unsealed object on an edge backend of at least `EDGE_MIN_BYTES` is admitted
the same way and streamed FROM the backend on a presigned ranged GET: the
backend's body is the client's response body, under the ORIGIN's response
headers the admit answered, and the commit is made at the backend's
headers (the Worker never sees the end of a stream it does not touch) —
`served` meters the bytes once, as the origin would have; any other
backend answer (Lyve's HEAD 200 / GET 500 of 2026-10-21, a 412, a 403) is
`fallback` and the origin serves the request itself (`fetch(request)` — a
GET has no body to lose), with its failover path. Everything else — HEAD
(cache-only on SLC), listings, deletes, Initiate / Complete / Abort, the
management API, a tenant without the flag, a sealed, chunked, SSE-C, tape,
public-bucket, versioned-by-id, conditional or multi-range read, a copy or
sub-resource write, a small body or range, a tenant past its egress
allowance with the throttle on (the origin paces it) — passes through to
the origin untouched (`fetch(request)`: the route's origin is SLC, the Host
stays, the signature still verifies there).

What it is not: a privacy feature (it moves Standard plaintext and Private
ciphertext alike and holds no key — contract lines 1, 4, 8), a verifier
(the control plane verifies every signature — Architecture Decision 8), a
hasher or a buffer: **no JavaScript touches the bytes** — the subrequest's
body is the request's own (`bench-results/EDGE-RELAY-2026-10-26.md`: any JS
in the byte path halves a stream to ~25 MB/s; pass-through runs at 42–54
MB/s per stream and ~150 MB/s at eight). Integrity is the backend's own
plus the client's `Content-MD5` signed into the link.

## 0.3.0 — the codec pipeline (WP-EDGE-2 Part B)

An admit that answers `codec` (and no `url`) is a write of a tenant with the
`edge_codec` flag: at the edge every PutObject of at least `EDGE_MIN_BYTES`
and every part of a multipart upload, wherever the bucket allows chunking
(RP68-1). The Worker is then the data-plane host Part A's Go harness played
(`internal/api/edge_codec_harness_test.go` is the oracle):

1. **cut** — the body goes through a `TransformStream` whose transform feeds
   `storedChunker.push` (`src/wasm/chunker.wasm`, the frozen Go chunker:
   boundaries byte-identical to the origin's, pinned by the golden vectors in
   node and Go); every completed chunk is cut from the bytes held. The body's
   MD5 (a pure-JS MD5 in `src/codec.js`) is taken on the way.
2. **hash** — `crypto.subtle.digest("SHA-256", chunk)`.
3. **ask** — `POST /api/v1/edge/index` with ≤ 64 `{hash, size}` per call; the
   control plane keys each hash in memory and answers hits and misses (a
   presigned PUT per miss at the chunk's one address, signing the sealed
   length). A batch goes out at 64 chunks, at the end, or when the reader is
   about to wait on the bound (D-EDGE-22).
4. **seal** — each miss as one 0x06 blob (`src/seal.js`: a random chunk key
   wrapped with `storedE2EE.sealTo` from `src/wasm/e2ee.wasm`, 1 MiB AES-256-GCM
   segments over WebCrypto; the Go opener opens what it seals —
   `TestEdgeCodec_NodeSealedChunkOpens`) and PUT it on its link.
5. **commit** — `{admit_id, outcome: "ok", chunks: [{hash, size, sealed_size,
   backend_etag, status: stored|hit}] in body order, body_md5}`; the answer is
   the client's, relayed as every commit's.

**The 503 rule.** Before the body is read (the modules do not load), the
admit is committed `edge_error` and the request passes to the origin. Once it
is read, any failure — an index call, a chunk PUT, a short body, a refused
commit answer — is a commit `{admit_id, outcome: "edge_error"}` (the control
plane gives back the reservation and drops the claims) and a **503 SlowDown +
Retry-After 30**; the request is never passed on (its body is gone).

**The memory bound (0.4.0).** At most `CODEC_IN_FLIGHT_BYTES` = 48 MiB of
chunk plaintext is held between a chunk's cut and its last segment's seal: a
FIFO byte semaphore the reader awaits, so the body is read no faster than
chunks leave. A chunk larger than the bound is admitted alone. A chunk is
held as its 1 MiB segments, each its own buffer, hashed with
`crypto.DigestStream`. `sealChunkStream` seals a segment when the PUT reads
it, drops its plaintext and releases its bytes; the PUT body goes through a
`FixedLengthStream` of the sealed length, so no sealed copy exists whole. A
hit gives its bytes back at once. (0.3.0 bounded 4 CHUNKS — RP70-1.) Measured: peak held 4 in node with every PUT delayed 50 ms
(`test/codec.test.js`); the workerd process 32–66 MB RSS for a 1 GiB upload
in `wrangler dev` (WP-EDGE-2B.md § Step 0). `limits.cpu_ms = 300000`: the
chunker costs ≈ 11 s of CPU per GiB.

## 0.5.2 — the mirrored PUT keeps its Content-Length (Stage 1a Prompt 49, a 0.5.1 regression)

- 0.5.1's counting streams had no known length, so workerd sent both PUTs of a mirrored write chunked with no `Content-Length`. iDrive answered **411** to every mirrored whole PUT: on prod 2026-10-28 23:58 UTC, three of tenant zero's test PUTs and nothing else; 0.5.1 was live 23:57:58–23:59:49 and was rolled back to 0.5.0 (`ebb3decb`). Node's `fetch` sends the header for a stream body anyway, which is why the tests passed.
- Both bodies now go through a `FixedLengthStream` of the admitted length where the runtime has one (the codec's chunk PUTs already did). The fix and the lag bound were checked in workerd itself (`wrangler dev --local`, a Node sink that answers 411 to a body with no length):
  - before the fix: chunked, 411;
  - after: `content-length` on both, 200;
  - a 64 MiB PUT with a mirror reading at 2 MB/s: 0.74 s with the mirror cut at 1.5 MB (0.5.0's `putTwo`: 34.75 s, held to the mirror's pace).
- Manifest row `edge-codec` 0.5.2 at `f134825a` (#1042); `edge-ingest` is byte-identical to its 0.5.1 row (`index.js` did not change), the other modules to theirs.
- 0.5.1 is never to be deployed again: its manifest rows stay in the log, and 0.5.2's rows follow.

## 0.5.1 — the mirror never slows the write (Stage 1a Prompt 49 Part 0, RP76-2)

- `putTwo` still tees with the runtime's `tee()`, but the mirror's branch goes through a 16 MiB + 2 MiB byte-length buffer (`TransformStream` + `ByteLengthQueuingStrategy`) and the bytes each fetch takes are counted: the mirror is cut (its fetch aborted, its branch cancelled — the tee then feeds the primary alone) when the primary leads it by more than `MIRROR_MAX_LAG` (16 MiB), or when the primary has answered and the mirror has not within `MIRROR_AFTER_MS` (5 s). The 30 s + body at 2 MiB/s deadline stays the outer bound. A mirrored write runs at the primary's pace (0.5.0 ran at the slower leg's until the deadline — RP76-2). The check runs on every counted read of the primary's branch (the lag changes only there), not on a timer.
- A cut mirror rides the commit as `mirror_cut: true` (a whole PUT's, and each mirrored codec chunk's); the control plane counts it in `vaultaire_blob_copies_total{outcome="cut"}` and the copy is owed (`pending`, the job's).
- Manifest rows `edge-ingest` / `edge-codec` 0.5.1 at `606001be` (#1038); `edge-seal`, `edge-awschunked` and the wasm modules are byte-identical to their 0.5.0/0.4.0/0.3.0 rows.
- `test/mirror.test.js` holds both shapes: Node's tee (buffers the slower branch) and a workerd-style paced tee (`pacedTee`, the faster branch waits) — a mirror that never answers returns within 5 s of the primary's answer, a mirror at a tenth of the pace is cut at 16 MiB of lag with the primary at its own pace.

## 0.5.0 — the mirror and the public catalogue (Stage 1a Prompt 48, WP-EDGE-4; folds 0.4.1)

- **The mirror** (`edge_mirror`): an admit with `urls: [placement, leg]` (and a codec miss with two `urls`) sends the body to both with the runtime's own `tee()` — no JavaScript reads the bytes; workerd paces the fast branch to the slow one (Step 0.1: 1 GiB teed to a 20 MB/s leg held ≤ 44 MiB). The primary decides the write; the mirror is aborted past 30 s + the body at 2 MiB/s, or when the primary fails, and its failure is only a missing ETag (`backend_etags` on the commit; the copy is then the control plane's `blob_copies` job's). `src/codec.js` `putTwo`.
- **The read's mirror fallback**: a read the primary does not answer as admitted tries `urls[1]` (with `mirror_send_headers`) before the origin; the commit says `served_url: 1`.
- **The public catalogue** (plan 10.3): with `CATALOGUE_URL` set (a `[vars]` entry: where the control plane's `catalogue_filter` job publishes `latest` and `<version>.bloom`), the filter is loaded once per isolate (`e2ee.wasm` `catalogueLoad`; refreshed hourly; unreachable = no filter, never a failed write) and a chunk whose plain hash it holds is sent `public: true`. Unset on prod.
- RS k+m is never the Worker's (D-EDGE-51): `ec.wasm` (`make ec-wasm`, client `edge-ec`) is for SDK clients — **remove `src/wasm/ec.wasm` before a deploy** (the module rules would upload it).
- Manifest rows at `eb9e20e1`: `edge-ingest` 0.5.0, `edge-codec` 0.5.0, `sdk-wasm` 0.3.0 (e2ee.wasm with the catalogue exports), `edge-chunker` 0.1.1 (the same code; its bytes moved with the module's dependency set since 0.1.0); `edge-seal`, `edge-awschunked` and `edge-wasm-exec` are byte-identical to their rows. 0.4.1's changes ship here (it was never deployed).

## 0.4.1 — the RP72 fixes (Stage 1a Prompt 47 Part 0, #1018)

- A control plane's 401/403 at a codec commit is the Worker's 503 SlowDown (the service channel's, transient), not the client's 400 (RP72-3).
- The windowed reader stops at `request.signal` (the client's disconnect): no range is launched after it, the ranges in flight are cancelled, and the commit is `served` with `bytes_sent` and `backend_code client_disconnected` — never a breaker charge (RP72-5).
- ONE isolate-wide byte budget (`ISOLATE_BYTES` 96 MiB, `src/codec.js`): the window takes its ranges from it and each codec write's 48 MiB is a child of it (RP72-6).
- Manifest rows `edge-ingest` / `edge-codec` 0.4.1 at `048ee7f1`; `edge-seal`, `edge-awschunked` and the wasm modules are byte-identical to their 0.4.0 rows. **Not deployed yet** (WP-EDGE-3 RB5-1): deploy after the server that holds the rows has booted, with RB-9's capture armed as for 0.4.0.

## 0.4.0 — aws-chunked, the windowed reader, the RP70 fixes (WP-EDGE-2 Part C)

- **aws-cli's default upload is the edge's.** That is `x-amz-content-sha256:
  STREAMING-UNSIGNED-PAYLOAD-TRAILER`, an aws-chunked body with a CRC64NVME
  or CRC32 trailer and `Transfer-Encoding: chunked`. The admit answers
  `headers` + the plain `url`: a HEADER-signed backend request the control
  plane mints (the backend's secret never leaves SLC). The mover forwards
  the body untouched under those headers (`moveInit`, chunked when the
  client sent no length). iDrive verifies the trailer; Lyve does not
  (`admitted_unverified`, D-EDGE-30). An iDrive-bound PutObject stays the
  origin's: iDrive checks no `x-amz-date`, so a header signature never
  expires there (D-EDGE-33).
- **The codec decodes the framing.** `src/awschunked.js` (client
  `edge-awschunked`) carries slicing-by-8 CRC64NVME and CRC32. The codec
  decodes the framing in its byte path, VERIFIES the trailer, and commits a
  mismatch as `checksum_mismatch`, which the client gets as a 400
  `XAmzContentChecksumMismatch` (D-EDGE-31). Signed chunks
  (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`) stay the origin's.
- **The windowed reader** (`serveWindow`). A GET of ≥ 64 MiB is answered
  `window {range_bytes, in_flight, start}` and a link that signs if-match
  (when provable) and no range — only to a Worker whose description says
  `features: ["window"]`.
  - Each 16 MiB range is fetched (≤ 6 at once) and written in order to a
    `FixedLengthStream`.
  - The first range decides served vs fallback.
  - A later failure CUTS the body.
  - One commit when the stream ends carries `bytes_sent`.
  - All windowed reads of an isolate share one budget of 6 × 16 MiB
    (`WINDOW_ISOLATE_BYTES`).
  - D-EDGE-32.
- **Every pass-through is `fetch(request, {cache: "no-store"})`**
  (`passThrough`, RP70-5): RB-9's cause.
- **A control plane's own 4xx at commit** is the client's 400, never a 503.
- **The admit**: every UploadPart of an edge tenant is the edge's, whatever
  its size, an empty last part included (D-EDGE-29).

**The modules and their manifest rows.** Each file is published as itself
(`find_additional_modules`, `base_dir = "src"`) and each is a `client_build`
row (`internal/translog/clientbuilds/manifest.json`, built by
`scripts/build-client.sh`, rebuilt by the Deploy job's
`scripts/repro-check-clients.sh`):

| module | client | built by |
|---|---|---|
| `src/index.js` | `edge-ingest` | the file |
| `src/codec.js` | `edge-codec` | the file |
| `src/seal.js` | `edge-seal` | the file |
| `src/awschunked.js` | `edge-awschunked` | the file (0.4.0) |
| `src/wasm/chunker.wasm` | `edge-chunker` | `sdk/chunkerwasm`, go.mod's toolchain |
| `src/wasm/e2ee.wasm` | `sdk-wasm` (its existing row) | `sdk/wasm`, go.mod's toolchain |
| `src/wasm/wasm_exec.js` | `edge-wasm-exec` | the toolchain's `lib/wasm/wasm_exec.js` |

`make edge-wasm` builds the three wasm-side files into `src/wasm/`
(gitignored) with the recipe — never `make sdk-wasm`, whose bytes differ.
The wasm is loaded with dynamic `import()` at the first codec request, so
`node --test` still imports `src/index.js` and every other request costs
nothing new.

**`CODEC_TAMPER`** (`etag` | `key`) is the lab's tampered-host switch
(`lab/codec-proof.sh` case g); the Worker refuses it outright when
`ENVIRONMENT = "production"` (wrangler.toml) and passes every request to the
origin.

## Files

- `src/index.js` — the Worker's entry module, published as-is
  (`no_bundle = true`): `sha256(src/index.js)` is the transparency log's
  `client_build` row for client `edge-ingest`
  (`internal/translog/clientbuilds/manifest.json`), and the deployed script
  downloads back byte-identical (`GET /accounts/{id}/workers/scripts/stored-s3-edge/content/v2`,
  the `index.js` part). The pure parts (`classify`, `describe`,
  `relayResponse`, `s3ErrorXML`, `backendErrorCode`) are exported for the
  tests.
- `test/*.test.js` — `node --test` (`make test-edge`; CI runs it): the
  classifier (the SDK's `x-id=` query is not a sub-resource — rclone's
  library; an aws-chunked body is the origin's; a signed GET is the edge's,
  a HEAD or a presigned URL never), the descriptor (every header
  lower-cased, hop-by-hop dropped, the client's Authorization INSIDE the
  JSON), the error relay, the read half (`readResponse`: the backend's body
  under the origin's headers with the client's status; `readFetchInit`:
  exactly the signed headers, `cache: "no-store"`; `readSucceeded` /
  `readCommit`: served at `expect_status` with the backend's Content-Length
  equal to the admitted span (0.2.1, RP66-2), fallback on anything else).
- `src/codec.js`, `src/seal.js`, `src/wasm/` — 0.3.0's codec (above).
- `lab/codec-proof.sh` — the codec's lab proof (`up` / `run <case>` / `down`);
  `lab/goldenbody.go` writes `crypto.GoldenBody` bodies for it.
- `wrangler.toml` — name `stored-s3-edge`, route `s3.stored.cloud/*` on the
  stored.cloud zone of the PRODUCTION account, observability on, vars
  `VAULTAIRE_ORIGIN` and `ENVIRONMENT = "production"`, the module rules and
  `limits.cpu_ms`.

## Configuration

| | |
|---|---|
| `VAULTAIRE_ORIGIN` (var) | where admit and commit go: `https://stored.ge` — the SLC host whose certificate is valid, NOT `s3.stored.cloud` (a call to the route's own host would loop into this Worker) |
| `VAULTAIRE_EDGE_TOKEN` (secret) | the `EDGE_SERVICE_TOKEN` of the box's `.env`, set with `wrangler secret put VAULTAIRE_EDGE_TOKEN < file` — never on a command line |

The control plane accepts the token only from a Cloudflare peer
(`clientip.PeerIsCloudflare`) unless `EDGE_TRUST_ANY_PEER=true` (labs).

## The wire

Admit body: `{op: "put"|"part", method, host, path (raw), query (raw),
headers: {lower-cased name: value}, client_ip (CF-Connecting-IP),
request_id}`. Answers (always HTTP 200): `{edge:false, reason}` →
`fetch(request)`; `{edge:false, refusal:{status, headers, body}}` → relayed
to the client verbatim (the S3 XML the origin would have sent);
`{edge:true, admit_id, url, method, send_headers, backend, storage_key,
expect:{md5}, expires_at}` → the move. An admit that
does not answer within 10 s, or answers 5xx, is a pass-through (the origin
is the fallback, never a dropped upload; one log line).

Commit body: `{admit_id, outcome: ok|backend_error|edge_error,
backend_status, backend_code (the backend's XML <Code>; the subrequest's
error on edge_error), backend_etag, duration_ms}` — the backend's answer,
nothing of the bytes. The answer is the client's: `{status, headers,
body}`. A lost answer is retried once after 2 s (idempotent on the admit
id); still none → the client gets 503 SlowDown + Retry-After 30 and the
control plane's reaper settles the admit.

A read (Part B): the admit answers `{edge:true, admit_id, url, method:
"GET", send_headers (exactly the signed headers: `range` always — the
exact byte span, the whole object's for a whole read — and `if-match`
where the row's digest is provably the backend's ETag), expect_status (206:
what the backend must answer), status (the client's: 200, or 206 for its
own Range), response_headers (the ORIGIN's set: ETag, Content-Type,
Content-Length, Content-Range, x-amz-storage-class, x-amz-meta-*, the
version id, the lock headers …)}`. The Worker fetches the link with
`cache: "no-store"` and, at the backend's headers, answers the client with
the backend's body under `response_headers` and `status`, and commits
`{admit_id, outcome: served|fallback|error, backend_status,
backend_content_length, duration_ms}` in the background
(`ctx.waitUntil`; one retry). A backend status other than `expect_status`,
a Content-Length at the backend's headers that is not the admit's
`content_length` (0.2.1, RP66-2 — the one integrity property this path can
check; the origin's integrity reader then decides), or no answer, is a
fallback: the client's request goes to the origin unchanged. A mid-body
backend failure is a cut body, as at the origin.

aws-chunked bodies (`x-amz-content-sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER`,
aws-cli's default over TLS) are the edge's since 0.4.0: the mover forwards
them under the admit's `headers`, and the codec decodes them (above). Signed
chunks pass through to the origin.

## Deploy

```
cd edge/s3-ingest
make -C ../.. edge-wasm                  # the wasm modules, by the client_build recipe (0.3.0)
npx -y wrangler@4.151.3 deploy          # CLOUDFLARE_API_TOKEN = the user token, CLOUDFLARE_ACCOUNT_ID = the production account
npx -y wrangler@4.151.3 secret put VAULTAIRE_EDGE_TOKEN < /path/to/token   # once, and after a rotation
```

Then the transparency-log row: `shasum -a 256 src/index.js` must equal the
manifest row's `sha256` for the deployed version (the Deploy job rebuilds
every row with `scripts/repro-check-clients.sh`; `translog-verify -client
<downloaded index.js>` verifies a download against the log).

## The lab (docs/reviews/WP-EDGE-1A.md, WP-EDGE-1B.md)

`wrangler dev --local --port 8787 --local-upstream 127.0.0.1:8099` against
a local origin on `:8099`, with `.dev.vars` holding `VAULTAIRE_ORIGIN` and
`VAULTAIRE_EDGE_TOKEN` (the origin booted with the same token and
`EDGE_TRUST_ANY_PEER=true`). Clients use the dev server as an **HTTP
proxy** (`HTTP_PROXY=http://127.0.0.1:8787`, endpoint
`http://127.0.0.1:8099`): the Host they sign is the origin's, the
pass-through reaches it, and the Worker sees the request as the edge would.
Go clients must set the proxy explicitly (`http.ProxyURL`) — Go never
proxies a loopback destination from the environment.

The 100 MB request limit is the zone plan's (Free), answered by Cloudflare
before this Worker runs: keep parts at 64 MiB (`docs/API.md`'s rclone
snippet). Nothing here works around it.
