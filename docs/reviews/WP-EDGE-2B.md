# WP-EDGE-2 Part B — the Worker 0.3.0 as the codec's data-plane host (Stage 1a Prompt 46b)

*Plan 40.10 slice 2, the HOST half: the Cloudflare Worker `stored-s3-edge`
(`edge/s3-ingest`) becomes what Part A's Go harness played — it chunks,
hashes, asks the index, seals and stores every codec write, and commits the
ordered chunk list. Part 0 = the RP68 review's fixes
(`docs/reviews/WP-EDGE-2A-REVIEW.md`). Prompt: `docs/reviews/WP-EDGE-PROMPTS.md`
§ Stage 1a Prompt 46b (the Opus edition was the session's). Session
2026-10-27. No migration (next: 112). The post-merge review is RP70.*

Contract lines cited: **4** (keys only through the one hierarchy — the Worker
holds the admit's PUBLIC key and per-chunk random keys it makes; nothing of
the hierarchy travels), **5** (no global convergent key — the identity stays
per tenant and per version; `dedup-measure` measures what a cross-tenant index
would buy, it builds none), **6** (dedup by a keyed index — the Worker sends
sha256 over the service-token channel, the control plane keys it in memory;
`TestIndexHoldsNoPlainHash` unchanged and green).

## What was built

| PR | What | Tests (red first) |
|---|---|---|
| **#1002** Part 0 (merged) | RP68-1 the edge's rule (`placementRequest.EdgeCodec`; the grant's threshold = `EDGE_MIN_BYTES`; a 4–64 MiB PUT and every part of a codec tenant are the codec's); a part of an upload already on the edge is never `small` (found here); RP68-2 a hit waits on a young claim (25 ms → 1 s, 60 s); RP68-6 a hit's size is its row's; RP68-3 the HEAD sample (`engine.ObjectHeader` on the edge writers, the first stored chunk and every 32nd, `head_mismatch`) + `routing_truth` OPENS `AES256-EW` chunks; RP68-5 `chunk_move` verifies a codec row by MD5; RP68-4 the outcomes by stage (index hit/miss; commit referenced/stored/head_mismatch/failed; reaper claims_dropped) | `TestEdgeCodecAdmit_EdgeRuleCoversWhatTheOriginStoresWhole`, `TestEdgeCodecMultipart_SmallLastPartStaysOnTheEdge`, `TestEdgeMultipart_SmallLastPartOfAnEdgeUploadStaysOnTheEdge`, `TestEdgeCodecCommit_HitWaitsForAYoungClaim`, `…_HitOfADroppedClaimFailsAtOnce`, `…_HitSizeMustBeTheRows`, `…_HeadSampleOfStoredChunks`, `TestRoutingTruth_CodecChunkOpenedNotJustRead`, `TestChunkMove_CodecRowVerifiesByMD5`, `TestEdgeHeadObject_ETagAndSizeAtTheDriversKey` |
| **#1003** Part 0b (the lab's finding) | one upload is one path UNDER THE LOCK: the first edge or codec part sees a staged reservation in flight (`reserved_bytes`, no part row) and passes through (`staged_parts`); a staged reservation or install on a codec upload is refused (`errUploadCodec`) | `TestEdgeCodecMultipart_StagedTailInFlightKeepsTheUploadStaged`, `TestStagedPart_RefusedUnderTheLockOnceTheUploadIsCodec` |
| **#1005** PR A, the Worker 0.3.0 (deployed 14:23:57 UTC) | `src/codec.js` (the cutter `TransformStream` over `chunker.wasm`, sha256, the index batches, the bound, the PUTs, the commit, the pure-JS MD5), `src/seal.js` (the 0x06 sealer over WebCrypto + `storedE2EE.sealTo`), `src/index.js` (the codec branch, lazy wasm loading, `CODEC_TAMPER` refused on production, a pass-through's reason logged); `make edge-wasm` (every module by the client_build recipe); `wrangler.toml` (`find_additional_modules`, `base_dir = "src"`, the module rules, `limits.cpu_ms = 300000`, `ENVIRONMENT = "production"`); closed-list clients `edge-codec`, `edge-seal`, `edge-wasm-exec` and their 0.3.0 manifest rows; `lab/codec-proof.sh` | `test/codec.test.js` (fake control plane + fake backend over HTTP: index batches ≤ 64, one PUT per miss at its sealed length, the commit names the backend's ETags, hits, repeats, back-pressure, failure → edge_error, short body), `test/seal.test.js` + `TestEdgeCodec_ExportVectorForNode` / `TestEdgeCodec_NodeSealedChunkOpens` (the Go opener opens the node-sealed blob), the golden boundaries through the cutter |
| **#1004** PR B | `cmd/tools/dedup-measure` — the F-3 number (`cross_tenant_only_fraction`) | `TestMeasure_PerTenantAndCrossTenant` (40 MiB, 32 per-tenant unique, 16 global, 0.4 exactly), `TestMeasure_TopLevelFileIsAnError` |
| PR C (this) | this note, STATUS, plan 40.10 + the three § 40.5 sentences, CLAUDE.md, Prompt 47 refreshed, the ledger | — |

## Step 0 — the probes

| # | Question | Answer |
|---|---|---|
| 0.1 | RB-9's capture at the 0.3.0 deploy | **Captured** (§ The deploy and RB-9): the failing first request reached the origin without the two signed conditional headers `range` and `if-none-match`; RB3-9. |
| 0.2 | A multi-module `no_bundle` Worker: is every file byte-identical? | **Yes.** `wrangler deploy --dry-run --outdir` with `find_additional_modules = true`, `base_dir = "src"` and two rules (`ESModule` for `*.js` + `wasm/*.js`, `CompiledWasm` for `wasm/*.wasm`) uploads six modules, each byte-identical to its source: `index.js` `f40f2779…`, `codec.js` `a6f44edd…`, `seal.js` `e7d66022…`, `wasm/chunker.wasm` 4,908,879 B `c5b1467a…` (= the edge-chunker 0.1.0 row), `wasm/e2ee.wasm` `bc06f7a6…` (= the sdk-wasm 0.2.0 row), `wasm/wasm_exec.js` 16,992 B `0c949f49…`; total 9,182 KiB, **2,621 KiB gzipped** (the Paid limit is 10 MB compressed). Two findings: modules must sit under `base_dir` (a sibling `wasm/` is not collected; a `../wasm/` specifier is invalid in workerd), and `make sdk-wasm` builds DIFFERENT e2ee bytes (`6f83d9bd…`, no `-buildvcs=false` / pinned toolchain) — the Worker uses `scripts/build-client.sh sdk-wasm` only. **Decision: one manifest row per module file** (D-EDGE-23). |
| 0.3 | WebCrypto AES-GCM and the e2ee.wasm load in workerd (`wrangler dev`, local) | AES-256-GCM over 64 × 1 MiB segments (AAD 1186 B): **16–19 ms** (≈ 3.5 GB/s); SHA-256 of 64 MiB: 29–32 ms; the first `import()` of the three modules 7 ms; Go runtime boot: chunker **31 ms**, e2ee **23 ms** (warm 2–7 ms); `storedE2EE.sealTo` of a 32-byte key: **0.75–1.1 ms**, 1169 bytes. |
| 0.4 | The chunker's rate in workerd | GoldenBody 100 MiB in 1,050–1,104 ms ≈ **93–97 MB/s**, 51 chunks (= the golden vectors). The chunker is the codec's CPU: ≈ 11 s per GiB → `limits.cpu_ms = 300000`. Through the cutter `TransformStream` in node: 118 MB/s. |
| 0.5 | `encodeBody: "manual"` for RP66-1's residual | **Not run** — the Opus edition of the prompt carried no step for it. Owed (RB3-8). |

The pure-JS MD5 (`codec.js`, the commit's `body_md5`): 341 MB/s in node — not the bound.

## The lab proof (`edge/s3-ingest/lab/codec-proof.sh`)

Topology (Part A's, D-EDGE-7): the origin built from the branch on
`127.0.0.1:8099`, a private migrated DB `vaultaire_edgelab`, `STORAGE_MODE=idrive`
on the bench us-central-1 pair (bucket `vaultaire`), a LAB master key file,
`EDGE_SERVICE_TOKEN_FILE`, `EDGE_TRUST_ANY_PEER=true`, `EDGE_MIN_BYTES` default
(4 MiB); flags `edge_ingest`, `edge_codec`, `chunking` ON for the lab tenant
through the admin API; the Worker in `wrangler@4.151.3 dev --local` with the
origin as upstream; aws-cli 2.28.19 through it as an HTTP proxy
(`request_checksum_calculation = when_required`). Every throughput number is
the Mac's uplink to iDrive (and shared that hour with the 40.10 SPEED bench
session), not an edge number.

| | Case | Result |
|---|---|---|
| (a) | `aws s3 cp` of GoldenBody(20261026, 100 MiB) | admitted codec; **51 chunks = the golden count**; head row `chunked` / `AES256-EW` / 104,857,600 / ETag = the body's MD5; read back through the origin's chunked GET **MD5-equal** (`e9aff7ea…`); 39 index calls p50 8 / p95 43 ms, peak in flight 4, 14.1 s (Mac uplink) |
| (b) | the same file again | **0 PUTs**, 51 hits (index) and 51 referenced (commit), `stored` unchanged at 51; 13 index calls; 2.9 s |
| (c) | the Worker's refs vs the harness's | the 51 `tenant_chunk_refs` offsets of (a) equal the golden boundaries the harness's chunker cuts (`chunker_golden.json`) — same count, offsets, sizes |
| (d) | aws-cli multipart | **64 MB parts**, 150 MiB: codec, `chunked`, `-3`, MD5-equal. **8 MiB parts**, 48 MiB: codec, `chunked`, `-6`, MD5-equal. **8 MiB parts with a 2 MiB tail**, 50 MiB: the first run FAILED at Complete — the tail (under `EDGE_MIN_BYTES`, the origin's per D-EDGE-5) staged beside six codec parts (aws-cli sends every part at once); fixed by #1003; the re-run: the whole upload ONE staged path, `idrive`, `-7`, MD5-equal (consistent, but through SLC — RB3-2) |
| (e) | 300 MiB of one 4 MiB block repeated, 8 MiB parts (parallel parts sharing chunks) | **0 × 503** in the aws-cli debug log; MD5-equal; `failed` 0 — RP68-2's wait carried every shared hit |
| (f) | the host dies mid-upload | killing aws-cli (`-9`) at 50 % abandons nothing in the lab — over loopback aws-cli had handed every part body to the dev proxy and workerd ran each invocation to its commit (25/25 committed). The HOST killed instead (`f_host`: the wrangler tree at 13:26:52 UTC after 10 parts): 8 admits and 25 claims pending. The reaper's run at 13:48:55 UTC (the admits' expiry 13:46:52 + its 5-minute cadence): `expired` 8, **`claims_dropped` 8**, `retired` 0, `released_bytes` 67,108,864 (the eight 8 MiB parts' reservations); pending claims left: **0** (RP68-4's outcome name, live) |
| (g) | tampered Workers (`CODEC_TAMPER`, refused on production) | **etag**: the commit refused **400 InvalidRequest** (`head_mismatch`: the backend answers another ETag), no head row, `head_mismatch` 1. **key** (sealed under another public key): **the commit is ACCEPTED** — a HEAD sample sees the wrong-key blob's own ETag and size; the origin's GET then fails closed (`the chunk key does not unwrap`, 500) and `routing_truth`'s codec open would mark it `unreadable` (pinned by `TestRoutingTruth_CodecChunkOpenedNotJustRead`). RP68-3(b)'s expectation does not hold for a HEAD sample: RB3-1 |
| (h) | throughput, 1 GiB, 64 MB parts | codec **26.2 MB/s** (41 s), 399 index calls p50 **60 ms** p95 **199 ms**, peak in flight 4 per request; the 0.2.1 mover path (edge_codec OFF, the bucket unsealed for the run — an SSE bucket's write is the origin's, `placement_whole_sealed`) **33.9 MB/s**: the codec ≈ 77 % of the mover on this link. workerd process RSS during the codec run **32–66 MB** (step 11: under 100 MB, the bound stays 4) |

Lab objects on iDrive are under `t-_global/_global/_chunks/<lab tenant>/` and
`t-<lab tenant>/…`; deleted at the lab's teardown (§ Teardown).

## Decisions

- **D-EDGE-22 — an index batch goes out at 64 chunks, at the end of the body,
  or when the reader is about to wait on the bound.** The prompt's "chunks
  queued until 64" and "at most 4 chunks between hash and PUT" cannot both
  hold literally — a batch would wait for chunks the bound forbids; flushing
  when the semaphore is spent is the only non-deadlocking reading. Measured:
  under back-pressure the batches are ≤ 4 (≈ 400 calls per GiB, p50 60 ms) —
  RP68-7's batching of claims per call is now worth doing (RB3-3).
- **D-EDGE-23 — one `client_build` row per module file.** `index.js` →
  `edge-ingest`, `codec.js` → `edge-codec`, `seal.js` → `edge-seal`,
  `wasm_exec.js` → `edge-wasm-exec` (new closed-list clients — the prompt
  named only the last; the rule it set makes the other two necessary),
  `chunker.wasm` → `edge-chunker` 0.1.0, `e2ee.wasm` → `sdk-wasm` 0.2.0 (the
  existing rows, same bytes). The MD5 lives in `codec.js` (one module fewer).
- **D-EDGE-24 — the wasm is loaded by dynamic `import()` at the first codec
  request**, from `src/wasm/` under `base_dir`; `node --test` keeps importing
  `src/index.js`, and a request that is not the codec's loads nothing.
- **D-EDGE-25 — the 0x06 DEK's HKDF is computed over HMAC**: node's
  (OpenSSL's) HKDF refuses an `info` over 1024 bytes and the DEK's info is
  27 + 1186 bytes. RFC 5869 by hand — PRK = HMAC(32 zero bytes, chunk key) (Go's
  nil salt), OKM = HMAC(PRK, info ‖ 0x01) — byte-identical to
  `hkdf.New(sha256.New, key, nil, info)`; the Go opener proves it. No
  derivation changed.
- **D-EDGE-26 — one upload is one path, under the lock** (#1003): a staged
  reservation in flight makes the upload staged for the first edge/codec part;
  a staged part on a codec upload is refused under the lock. D-EDGE-5 is
  unchanged (an UploadPart under `EDGE_MIN_BYTES` on an upload not yet on
  the edge is still the origin's) — RB3-2 and the open question.
- **D-EDGE-27 — the commit's sample answers what it cannot check with a
  503**: a HEAD that cannot be made (the backend could not be asked) is the
  host's retry (`errEdgeChunkUnverified`), a blob missing or of another ETag
  or size is the 400; the codec admit requires the chunk store's backend to
  be an `engine.ObjectHeader` (else the origin takes the write).
- **D-EDGE-28 — `CODEC_TAMPER` is a lab switch in the deployable source**,
  refused at the first request when `ENVIRONMENT = "production"`
  (`wrangler.toml` sets it; the refusal passes every request to the origin).
- **A part of an upload already on the edge is never `small`** (#1002): the
  stager refuses its parts, so D-EDGE-5's literal answer for such a part was
  always a client 400.

## Residuals (RP70's input)

- **RB3-1 (P2) — a host that seals with the wrong key passes the commit.**
  The HEAD sample (RP68-3a) checks ETag and size, which a wrong-key blob has
  of its own; the object is then unreadable (GET 500, fail-closed) until
  `routing_truth`'s daily codec open finds it, and a later dedup hit on its
  chunks inherits the damage. Fix: the commit GETs and OPENS the first
  stored chunk (≤ 16 MiB + overhead, under the tenant's codec seed, the keyed
  hash checked) instead of only HEADing it — a design change, the open
  question below.
- **RB3-2 (P3) — a codec tenant's multipart upload with a tail part under
  4 MiB.** aws-cli sends every part at once: when the tail reaches the
  origin's stager first, the whole upload becomes one staged path through SLC
  (consistent, #1003); when a codec part marks the upload first, the tail is
  refused 400 (aws-cli does not retry a 400 — the upload fails, nothing
  corrupt). The fix is to exempt UploadPart from `EDGE_MIN_BYTES` — D-EDGE-5's
  owner (the open question).
- **RB3-3 (P3) — index calls per ≤ 4 chunks under back-pressure**: 399 calls
  for 1 GiB, p50 60 ms / p95 199 ms against the local origin. RP68-7 (one
  transaction per call for its claims) now has its number.
- **RB3-4 (P4) — the lab's throughput is the Mac's uplink** (26.2 vs 33.9
  MB/s, ≈ 77 %); the edge's number is the deploy's and the SPEED bench's.
- **RB3-5 (P4) — an empty UploadPart of an edge/codec upload** passes through
  (`empty`) and is refused by the stager (one path) — S3 allows a 0-byte last
  part.
- **RB3-6 (P4) — the memory number is the local workerd process's RSS**
  (32–66 MB for 1 GiB), not an edge isolate's: the deployed Worker's
  `Exceeded memory` count in the dashboard is the check after the deploy.
- **RB3-7 (P4) — the 0.3.0 bundle is 2.6 MB gzipped**: a cold isolate's
  first codec request pays ≈ 55 ms of Go runtime boot.
- **RB3-8 (P4) — Step 0.5 (`encodeBody: "manual"`, RP66-1's residual) not
  run.**
- **RB3-9 (P3) — RB-9's cause: the first pass-through after a content
  deploy loses the signed `range` and `if-none-match`** (§ The deploy and
  RB-9). Fix: `cache: "no-store"` on the pass-through fetch; prove with the
  capture at the next content deploy.
- RB2-1 … RB2-5 stand as RP68 left them; RB2-3 is closed by RP68-5 (#1002).

## Open questions (for the owner)

1. **D-EDGE-5 for parts (RB3-2):** may an UploadPart under `EDGE_MIN_BYTES`
   be the edge's (every part of an edge tenant asked about)? It removes the
   staged-tail race for aws-cli's default 8 MiB parts; the cost is two JSON
   round trips per tail part. Not changed here (a D-EDGE decision).
2. **RB3-1:** the commit's sample by GET + open (catches a wrong-key host at
   the first commit) instead of HEAD (catches a lying ETag only). ≈ one
   chunk's read per commit.

## The deploy and RB-9

**Deployed 2026-10-27 14:23:57 UTC** — the Worker 0.3.0 from main `0954fa72`
(the server already on `a373d11f`, which contains it), version
`7c1cd872-db3b-48e4-988c-9920df9c92cf`, 9,182.59 KiB / 2,620.63 KiB gzipped,
after the 40.10 SPEED bench session finished its prod-path section (it asked
for the hold). `edge_codec` stays OFF on prod (no key version until F-14): the
write and read paths are 0.2.1's; the new line is the pass-through's reason in
the Worker's log. The deployed script downloaded back
(`…/workers/scripts/stored-s3-edge/content/v2`): every module byte-identical
to its manifest row — `index.js` `f40f2779…`, `codec.js` `a6f44edd…`,
`seal.js` `e7d66022…`, `wasm/chunker.wasm` `c5b1467a…`, `wasm/e2ee.wasm`
`bc06f7a6…`, `wasm/wasm_exec.js` `0c949f49…`; and each is a `client_build`
entry of prod's log (edge-ingest / edge-codec / edge-seal / edge-wasm-exec
0.3.0 = entries 85–88, appended at the server's boot; edge-chunker 0.1.0 =
80; sdk-wasm 0.2.0 earlier). `translog-verify -client` itself cannot run
against prod yet (no signing key, `translog_public` OFF — the ceremony's).
`/health` healthy, 21/21 backends.

**RB-9 — captured, root cause narrowed to the pass-through's headers.**
`tcpdump -i lo -s0 -U tcp port 8000` (the active slot) armed on the box
before the deploy; the signed pass-through probe (aws-cli `get-object` with
`--range bytes=0-1 --if-none-match '"abc"'`, the bench key, a nonexistent
bucket) fired 3 s after the deploy returned (14:24:00) and again 20 s later:

| | the first request after the deploy | 20 s later |
|---|---|---|
| answer | **403 SignatureDoesNotMatch** | 404 NoSuchKey (auth passed) |
| `SignedHeaders` (identical) | `host;if-none-match;range;x-amz-checksum-mode;x-amz-content-sha256;x-amz-date` | the same |
| `range: bytes=0-1` | **absent** | present |
| `if-none-match: "abc"` | **absent** | present |
| every other header (host, x-amz-date, x-amz-content-sha256, x-amz-checksum-mode, authorization, the cf-* set, x-forwarded-*) | present | present (order differs) |

The origin refused correctly: two headers the client signed never reached it.
The Worker reached admit (a pass-through decision) and then `fetch(request)`
— on the first request after a content-changing deploy, the subrequest
arrived without the conditional pair; 20 s later with it. A GET carrying
`Range` / `If-None-Match` is exactly what Cloudflare's cache layer answers
itself (it fetches the whole object to fill the cache and strips the
conditional headers from the origin request) until it learns the URL is not
cacheable — the read path already sends `cache: "no-store"` for this reason
(D-EDGE-11's link). **Proposed fix (RB3-9):** the pass-through
`fetch(request, { cache: "no-store" })` for every request the edge passes on,
then a content deploy with the capture armed again; not made here (the
deploy is this session's last step, and a Worker change needs its own test
and deploy). Both pcaps removed from the box — this capture's and Part A's
`/tmp/rb9.pcap` + `/tmp/rb9-tcpdump.log` of 02:42 UTC, which WP-EDGE-2A.md
said were cleaned and were not.

## Teardown

The lab torn down at 14:05 UTC: the Worker and the origin stopped (by their
process trees — the lab's first `stop_worker` killed only the listener and
left wrangler parents respawning workerd; fixed in the script), every blob of
the lab tenant deleted from iDrive (`t-_global/_global/_chunks/tenant-ec056962e7d55c55/`
1,400 chunk blobs, 2,860,854,695 bytes; `t-tenant-ec056962e7d55c55/` 2 whole
objects, 1,126,172,529 bytes; left: 0 and 0; no open multipart upload), the
private DB `vaultaire_edgelab` dropped, the lab master key, token and
credentials removed. The boot canary `t-_probe/_probe/<the Mac's hostname>`
stays (every lab origin writes it; shared with earlier labs).
