# WP-EDGE-2 Part A — slice 2 against the LAB master key: the codec and the control plane (Stage 1a Prompt 46)

*Plan 40.10 slice 2, the CODEC half: a PutObject (or an UploadPart) admitted
at the edge of a tenant with the `edge_codec` flag whose placement is CHUNKED
is chunked, keyed-hashed and SEALED by the data-plane host — in Part A a Go
test harness that plays the Worker; Part B (Prompt 46b) makes it the Worker
0.3.0. Part 0 = the WP-EDGE-1 Part B review's RP66-1/2/3/5/6/7. Prompt:
`docs/reviews/WP-EDGE-PROMPTS.md` (the 2026-10-27 rewrite; RP66-8's two
corrections are in). Session 2026-10-26/27, the owner away ("build
everything now"). Next migration: 112. The post-merge review is RP68.*

Contract lines cited: **4** (keys only through the one hierarchy: the index
key and the codec X-Wing pair are HKDF of the tenant key VERSION's stored
chunk key — `stored/index/v1`, `stored/edge-codec/v1` — no new stored key, no
new column; deleting the version's row, the crypto-shred, takes both), **5**
(no global convergent key: the index key is per tenant and per version, so
nothing converges across tenants by construction — the cross-tenant forms
stay 10.3's filter and F-37's pool, neither here), **6** (dedup by a keyed
index: a codec chunk's index identity is HMAC-SHA256(index key,
sha256(chunk)); `TestIndexHoldsNoPlainHash` sweeps the rows AND the server's
observed logs for every real chunk hash of an upload).

## Part 0 — the RP66 fixes (#994, merged; the Worker 0.2.1 deployed)

| # | Fix | Test (red on main first) |
|---|---|---|
| RP66-1 (P2) | A head row with a stored `Content-Encoding` passes through (`passThrough("content_encoding")`): the Workers runtime's `encodeBody: "automatic"` re-encodes a Response body to match the header it is given, while the fixed-bucket backend's answer carries the already-encoded bytes with no encoding header — the edge would gzip the gzip under the row's Content-Length. | a `TestEdgeAdmitGet_PassThroughMatrix` row |
| RP66-2 (P3) | The GET admit answers `content_length` (the admitted span); the Worker 0.2.1's `readSucceeded` requires the backend's Content-Length to equal it (no header = no check) — a mismatch is a `fallback`, the body cancelled, the origin's integrity path decides. Every `served` edge read counts in `vaultaire_object_integrity_unverified_total{path="edge",reason="unverified"}` (pre-created): the path is unverified by design beyond the length, and the counter says so. Bit-rot of equal length stays with the backend's own checksums — a decision. | the matrix's baseline + the node length-mismatch test + `TestEdgeCommitGet_ServedTouchesLastAccessedAndCountsUnverified` |
| RP66-3 (P3) | A served edge read refreshes `object_head_cache.last_accessed` with the origin's own detached UPDATE — an object read daily through the edge no longer looks idle to `smart_demotion`. A fallback touches nothing (the origin's own serve does). | the same test |
| RP66-5 (P4) | The response's `x-amz-request-id` is the Worker's `request_id` — what the admit row, the access-log row and the Worker's log line carry. | the same test |
| RP66-6 (P4) | The reaper purges terminal `get` rows after 24 h (aws-cli writes one per 8 MiB range; nothing later reads them — the access log has the bytes); writes keep 7 d. One DELETE, the interval by op. | `TestEdgeReaper_TerminalGetRowsPurgedAt24h` |
| RP66-7 (P4) | A backend answer without a Content-Length is a Debug line, metered as admitted (folded into RP66-2's branch). | covered by the clamp assertions |

**Worker 0.2.1** = sha256 `4714c398…` of `src/index.js` at `a2cc249d`, the
manifest row beside it; deployed on the production account 2026-10-27
02:31:24 UTC (version `419ef4f0`), the deployed script's content byte-checked
by the manifest row (the repro Deploy step rebuilds it).

## Step 0 — the probes

| # | Question | Answer |
|---|---|---|
| 0.1 | WASM chunker boundaries = Go's on the 100 MiB seed body; throughput in node | **Byte-identical**: `GoldenBody(seed 20261026, 100 MiB)` cuts into 51 chunks at the same offsets in Go (`TestChunker_GoldenBoundaries`) and through the WASM module's push API in uneven 64 KiB–8 MiB slices (`edge/s3-ingest/test/chunker.test.js`, under `make test-edge`, which builds the wasm first). Throughput in node 24.6: 100 MiB in 815 ms ≈ **123 MB/s** (workerd is Part B's measurement). Boundaries reset per instance: each multipart part is chunked alone, so cross-part dedup is by chunk content, never position. |
| 0.2 | The X-Wing KEM from sdk/wasm standalone in node | **Works**: `sealTo` of a 32-byte key = **1169 bytes**, **0.75 ms per wrap** (200 iterations, warm), opens back exact; the module is 4.3 MB (1.2 MB gzipped). Part B is buildable. |
| 0.3 | The claim/takeover window against a host that dies mid-part | **Holds** (`TestEdgeCodecIndex_HitMissClaimTakeover`): the dead host's claims stay `pending`; a second host racing the same misses inside the window is answered HITS (nothing referenced) and its commit's `IncrementRef` finds no ready row → 503, the host retries; past the window a third admit's index TAKES THE CLAIMS OVER, stores, commits; a fourth upload then hits for real. The window is `edgeAdmitExpiry` (20 min), not chunk_claim.go's 3 min — D-EDGE-20. |
| 0.4 | Migration 111's shape | **Decided**: `global_content_index.keyed_hash TEXT` + partial unique `(dedup_scope, keyed_hash) WHERE keyed_hash IS NOT NULL`; a codec row carries the keyed hash hex in BOTH `plaintext_hash` (the table's identity, so every claim, reference, advisory lock, GC sweep and reconcile keyed on `(dedup_scope, plaintext_hash)` works untouched) and `keyed_hash` (the typed marker: set = this row's identity is keyed, no column of it is sha256(chunk)). `edge_admits.codec BOOLEAN`; `multipart_uploads.codec BOOLEAN` (one upload is one path needs the upload marked, not only its parts); `multipart_parts.part_chunks JSONB` (the part's ordered chunk list `[{h,s,z,e,v}]`). Additive. |
| 0.5 | RB-9's capture at the 0.2.1 deploy | **Reproduced, narrowed, headers still owed.** Deploy 1 (02:31:24Z, ~4 min quiet before, NEW script bytes): the FIRST signed pass-through GET (If-None-Match + Range, the bench key, a probe bucket that does not exist) answered **403 SignatureDoesNotMatch** at 02:31:36 — and the journal shows the 0.2.1 Worker DID call `/api/v1/edge/admit` (200, a pass-through decision) **235 ms before the same request's `fetch(request)` hit the origin with a signature it refused**; every later probe verified (NoSuchKey). So the new version handled the request — the corruption is between the Worker's pass-through fetch and the origin, on the first request only. Deploy 2 (02:35:20Z, continuous traffic before, identical bytes): not reproduced. Deploy 3 (02:41:59Z, ~6.5 min quiet, identical bytes, tcpdump armed on the box): not reproduced. Verdict: the 403 struck only the deploy whose SCRIPT BYTES changed — consistent with Cloudflare not rotating isolates for identical content — as every historical occurrence (0.1.0, 0.2.0, 0.2.1) followed a content-changing deploy. One failed request per content deploy, self-healing, the client's retry succeeds. The origin runs `zap.NewProduction` (no auth Debug to flip); the headers capture is tcpdump between HAProxy and the slot, armed BEFORE the next content-changing deploy — the Worker 0.3.0, Prompt 46b's Step 0. The probe wrote nothing anywhere (the bucket never existed); tcpdump and its files cleaned off the box. |

## What was built

- **A (#995, the codec):** `internal/crypto/stream.go` grows format **0x06**
  (`StreamFormatWrapped`, kind 0x03 `StreamKindWrapped`, header 1186 bytes =
  `[0x06][0x03][key_version u32][wrapped chunk key 1169][segment_size u32][nonce_prefix 7]`;
  `MaxStreamHeader` 1105 → 1186; `ParseStreamHeader` refuses a kind that is
  not its format's). `edge_codec.go`: a RANDOM 32-byte key per chunk (the
  host seals before it knows whether the chunk dedups; a dedup hit reuses the
  first writer's blob, openable because its header wraps its own key), the
  0x04 construction's 1 MiB segments under HKDF(chunk key, info ‖ header),
  the key wrapped with HPKE (sdk/go/e2ee's 0x03 sealed box: RFC 9180 base
  mode, X-Wing, HKDF-SHA256, ChaCha20-Poly1305); AAD = the header alone (a
  chunk has no bucket/key identity; the keyed-hash check after the open binds
  the content, as the plaintext-hash check binds AES256-CE). The encryption
  name is **`AES256-EW`** (`crypto.EdgeChunkEncryptionAlgo`).
  `WrappedChunkSealedLength` is a pure function of the size (the chunk link's
  Content-Length). On `TenantKeys`: `IndexKey`, `EdgeCodecPublicKey`,
  `EdgeCodecPrivateKey` (per version, cached as every version is). The frozen
  chunker compiled to WASM (`sdk/chunkerwasm` → client **`edge-chunker`**,
  built by `scripts/build-client.sh`, linted by `make lint-wasm`, reproducible
  — built twice byte-identical; manifest row 0.1.0 at `155b4ae5`). Golden:
  a golden-OPENS fixture + deterministic segment vectors + the tamper matrix
  + `TestKeyedChunkHash_Golden` + `TestChunker_GoldenBoundaries` (Go + node).
- **B (#997, the control plane; migration 111):** the admit's codec branch
  (`edgeAdmitCodecPut` / `edgeAdmitCodecPart`): with the flag and a key
  version the admit answers `{edge:true, codec:{version, kem_public_key,
  chunk_threshold, segment_size}}` and NO presigned body PUT; with the flag
  and NO key version (prod until F-14) **Part A's whole PUT** (Debug
  `no_key` — never a refusal, never a plaintext chunked write at the edge).
  **`POST /api/v1/edge/index`** (the service-token gate): per plain sha256
  the control plane computes the keyed hash under the admit's version's
  index key IN MEMORY (D-EDGE-16 as RP66-8 rewrote it) and answers hit
  (ready row — referenced at COMMIT, never at index time) / hit (a YOUNG
  pending claim: another host is storing it; the commit's `IncrementRef` is
  the check) / miss (a claim taken, chunk_claim.go's shape, takeover after
  `edgeAdmitExpiry` — D-EDGE-20; a presigned PUT into `_global` at the ONE
  address `_chunks/<tenant>/<keyed hex>.k<version>` signing the SEALED
  length; no Content-MD5 on a chunk link — D-EDGE-17). The claims are
  recorded on the admit row (`attrs.codec_claims`) so the expiry drops them.
  **The commit**: a stored chunk's claim flips ready with
  `ciphertext_hash` = the backend's MD5 ETag of the sealed bytes and one
  reference; a hit takes its reference (0 rows = `errEdgeChunkVanished` →
  503, the host retries the part); then the manifest at the admit's address
  and the chunked head row in ONE transaction through `commitBlobWrite` —
  **storeChunkedObject's rows** (the harness compares them row by row); the
  PUT's ETag is the host's whole-body MD5 (the one place Part A's "no JS in
  the byte path" reverses, on purpose: the host read every byte to chunk
  it). A failed commit gives every reference back, drops every claim,
  releases the reservation. **Multipart**: the first codec part marks the
  upload (`multipart_uploads.codec`, under the upload row's lock, no staged
  and no edge-whole part); each part's commit writes its ordered chunk list
  (`multipart_parts.part_chunks`) with its references taken (a replaced part
  number releases its previous list's); Complete concatenates the lists into
  one manifest at the upload's recorded address (the `-N` ETag as the staged
  path's); an ABORTED codec upload's part references are released by the
  abort inline and by the multipart reaper's net (part_chunks NULLed with
  the release, once). One upload is one path: a staged part on a codec
  upload is 400 (`codecUploadRefusal`), an edge-whole part on one is 400;
  a first codec-eligible part on an upload holding staged parts passes
  through to the origin's stager (D-EDGE-21: kinder than D-EDGE-3's literal
  400, same invariant). **The read path**: the chunked GET's
  `fetchAndVerifyChunk` opens an `AES256-EW` chunk — the stored bytes
  checked against the recorded ciphertext MD5, the 0x06 open under the
  RECORDED version's codec seed, the plaintext bound by its keyed hash
  (`openEdgeCodecChunk`). The `key_reencrypt` chunk pass skips `AES256-EW`
  rows (RB2-1 below). Flag **`edge_codec`** (default OFF, per tenant);
  metrics `vaultaire_edge_codec_chunks_total{outcome=hit|miss|stored|failed}`,
  `_bytes_total{kind=plaintext|sealed}`, `_index_calls_total` (pre-created).
- **C (this PR):** this note, STATUS, plan 40.10, CLAUDE.md rows,
  ENCRYPTION.md, Prompt 46b in `WP-EDGE-PROMPTS.md`, the RB-9 line of
  `WP-EDGE-1B.md`.

## The harness proof (`edge_codec_harness_test.go`, the migrated test DB, the lab master key)

The Go "Worker": chunks with the Go chunker, hashes, calls `/index` over the
handler, seals the misses with `SealWrappedChunk` under the admit's public
key, PUTs them where the links point (the Part A fake backend), commits with
the chunk list and the body's MD5. Proven: the end-to-end read-back (the
ORIGIN's own chunked GET serves the object MD5-equal); the rows equal a
control upload of the SAME bytes through the origin's chunked path — same
size, same ETag (both the body's MD5), same backend sentinel, floor, chunk
count and offsets (the frozen chunker), different identities (keyed against
plain) and seals (AES256-EW against AES256-CE) on purpose; a second upload
of the same bytes = **100 % hits, 0 PUTs**, both manifests holding their
references; the no-key gate; the race/takeover ladder (Step 0.3); a failed
commit and an expiry drop everything (nothing queued for the blob collector
— a codec admit holds no whole blob); the multipart concatenation read back
byte-exact with the `-N` ETag, the one-path refusals, the abort's reference
release. This harness is Part B's oracle: the Worker 0.3.0 must produce the
same rows.

## Decisions

- **D-EDGE-16 (rewritten, RP66-8).** The data-plane host learns NO
  long-lived key. It sends sha256(chunk) over the service-token channel and
  the CONTROL PLANE applies the tenant index key in memory for the call —
  the stored prompt's per-admit keyed hash was impossible
  (HMAC(K_tenant, h) is not computable from HMAC(K_admit, h) without h),
  and the host already holds the plaintext: the hash adds nothing to what
  it knows. The index stores keyed hashes only; no log line or metric label
  of the path carries a plain hash (`TestIndexHoldsNoPlainHash` sweeps the
  rows and the observed logs).
- **D-EDGE-17.** No Content-MD5 on a chunk link — the host would have to
  seal every chunk before asking the index. The link signs the SEALED
  length (a pure function of the plaintext size); the backend's MD5 ETag of
  the sealed bytes is recorded at commit as the row's `ciphertext_hash` and
  every chunked GET verifies the stored blob against it, as today.
- **D-EDGE-18.** The prompt's "tenant's ML-KEM-768 public key" is realised
  as a per-version **X-Wing** pair derived from the version's stored chunk
  key (HKDF `stored/edge-codec/v1` → the 32-byte X-Wing seed; the KEM inside
  X-Wing is ML-KEM-768 + X25519 — sdk/go/e2ee's suite, one HPKE
  implementation). No new stored key or column (contract 4); a rotation is a
  new index key AND a new codec pair, so old codec chunks stop deduping new
  uploads while their blobs stay readable under their recorded version;
  the crypto-shred covers both.
- **D-EDGE-19.** The 0x06 golden is a golden-OPENS fixture plus
  deterministic segment vectors: `crypto/hpke` has no seeded sender, so the
  stored prompt's "fixed KEM ephemeral trick" is not expressible with the
  stdlib API (the sdk's own `TestSealedBox_GoldenOpens` pattern).
- **D-EDGE-20.** An edge chunk claim's takeover window is the admit's whole
  life (`edgeAdmitExpiry`, 20 min), not chunk_claim.go's 3 minutes: a chunk
  link lives 15 minutes and may still write the ONE address — a claim taken
  over while a live link can write it would let a dead host's late PUT land
  over the taker's bytes, leaving a `ciphertext_hash` describing other bytes
  (the read would fail closed, but the object would be broken until
  re-uploaded). A young pending claim is answered as a HIT with nothing
  referenced; the commit validates (`IncrementRef` 0 rows = retry).
- **D-EDGE-21.** One upload is one path, with D-EDGE-3's refusals pointed
  the right way: a staged part on a codec upload and an edge-whole part on a
  codec upload are 400; a FIRST codec-eligible part on an upload that
  already holds staged parts passes through to the origin's stager instead
  of 400 — the upload stays one (staged) path and the client is not bricked.
- **IsStreamFormat grew by 0x06** — every reader that switches on the format
  byte, by grep: `stream.go` `ParseStreamHeader`/`newStreamHeaderFormat`
  (parse + refuse a mismatched kind/format pair), `sse_s3.go` `Open`
  (`streamAEADFor` refuses kind ≠ SSE-S3), `SSEBlobKeyVersion` (kind 0x03
  errors), `DecryptBytes` (refuses every streaming format),
  `api/s3_large_get.go`'s sealed gate (row-gated to SSE algorithms; the kind
  switch refuses), `api/sse_stream.go` (`== StreamFormat` only). None can
  serve a 0x06 blob as something else (`TestWrappedChunk_IsNotAnSSEBlob`).

## Residuals (RP68's input)

- **RB2-1 (P3) — the codec re-home after a key rotation is a pass of its
  own.** `key_reencrypt`'s chunk pass skips `AES256-EW` rows (their re-home
  changes the index identity: new keyed hash, new address, every manifest
  ref re-pointed). Old-version codec chunks stay readable (the row records
  the version) and stop deduping new uploads (D-EDGE-18).
- **RB2-2 (P3) — a multipart codec part's references are counted only in
  `ref_count` until Complete inserts the manifest rows.** dedup GC's
  reconcile recomputes `ref_count` from `tenant_chunk_refs` for rows idle
  past its 7-day grace: an upload still active past ~7 days whose early
  chunks go untouched could have those references reconciled away before
  Complete. The 48 h abandon reaper bounds it today; the clean fix is
  staging `tenant_chunk_refs` rows at part commit (re-pointed at Complete).
- **RB2-3 (P4) — `chunk_move` and `routing_truth`'s chunk verification
  assume a sha256 `ciphertext_hash`.** An `AES256-EW` row's is an MD5; the
  chunk move would mis-verify it (the move is operator-run and prod has no
  codec rows); the routing-truth read probe reads bytes, not the hash.
- **RB2-4 (P4) — the account erasure of a tenant with ACTIVE codec uploads**
  releases manifest references set-based (F5, `tenant_chunk_refs`) but not
  `part_chunks` references; the tenant-scoped rows are erased with the
  scope either way, so nothing outlives the tenant — only the counts drift
  until the rows go.
- **RB2-5 (P4) — `SealWrappedChunk` is unreachable from `cmd/vaultaire`**
  (deadcode): by design — the PRODUCT only OPENS 0x06; the sealer is the
  data-plane host's (the harness, then the WASM/Worker). Linking
  sdk/go/e2ee into the binary also makes deadcode list that package's other
  functions; informational.
- **RB-9 — refined, still open** (Step 0.5 above): one 403 per
  content-changing Worker deploy, first request only, admit reached, the
  pass-through's signature refused; the headers capture is armed before the
  0.3.0 deploy (Prompt 46b Step 0).
