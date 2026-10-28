# WP-EDGE-4 — the second copy at the edge (mirror; RS dark), `standard_sealed_default` (dark), the public-catalogue filter (Stage 1a Prompt 48)

*Plan 40.10 slice 3: "objects under 8 MiB mirrored on two backends; 8 MiB and up k data + m parity across backends … no vendor holding more than m shards, a read touching the k data shards only … mirror first (the owner's own backups are a single copy on Lyve today)"; plan 10.3: "the public-catalogue filter shipped to the client".*
- *Part 0 = the RP74 review's fixes (`docs/reviews/WP-EDGE-3-REVIEW.md`), RP74-2 first and deployed alone.*
- *Then the mirror (PR A), RS k+m at a Go host (PR B, dark), the seal-by-default switch (PR C, dark), the catalogue filter (PR D).*

*Prompt: `docs/reviews/WP-EDGE-PROMPTS.md` § Stage 1a Prompt 48 (the Opus edition was the session's). Session 2026-10-28. **Migrations 113** (`113_blob_copies.sql`, PR A) **and 114** (`114_public_catalogue.sql`, PR D); next: 115. The post-merge review is RP76.*

Contract lines cited:
- **4** (keys only through the one hierarchy). A mirror copy of a sealed chunk is the same ciphertext at the same address; RS shards and mirror copies of a Standard object carry what the primary carries. `standard_sealed_default` seals with the existing SSE-S3 and codec keys of the tenant's key versions. A node still holds only its token (and now its TLS certificate).
- **5** (no global convergent key; `_public` the only cross-tenant scope). The catalogue's `_public` scope holds only what an operator seeded with `cmd/tools/catalogue-seed`; the index never claims there, a tenant's chunk enters it never, and a commit may reference it only for a hash the index answered public.
- **6** (dedup by a keyed index). Unchanged for tenant chunks: a public chunk is referenced by its PLAIN hash because it is public content (anyone holding the artefact computes it); the filter is checked on the host, so the server learns only what was referenced.
- **8** (TLS on every data path): a node now terminates TLS itself (RP74-3).

## What was built

| PR | What | Tests (red first) |
|---|---|---|
| **#1027** Part 0a (merged `21d01c02`, deployed 13:28 UTC) | RP74-2: the edge-call gate refuses a token that is not 64 lower-case hex before any lookup; the registry cache is an LRU of 1,024 (negative entries included) | `TestEdgeCaller_AnUnknownTokenCostsNoQueryAndNoEntry` (10,000 tokens: 0 queries, 0 entries), `TestDataPlaneHostCache_LRUEvicts…` |
| **#1028** Part 0 (merged `6a19dbbf`, deployed 14:14 UTC) | RP74-1 DNS-only control plane (DEPLOY.md, `install.sh` + `cmd/tools/cf-range`, the node's Error line after two 404 heartbeats, the gate pinned on an `s3.` host); RP74-3 node TLS (`NODE_TLS_CERT`/`NODE_TLS_KEY` or `NODE_ACME_EMAIL`, a public bind refused without, StateDirectory + CAP_NET_BIND_SERVICE, :443); RP74-4 host=slc verifies (whole read through the integrity reader on the admit's `body_md5`; an aws-chunked move's trailer, `checksum_mismatch` the client's 400); RP74-5 one slc host; RP74-6 `dataplane.Host.Budget` (FIFO, 2 s → 503; node `NODE_CODEC_BUDGET_BYTES`, slc = the origin's chunk budget); RP74-7 `slcEnabled` needs the `slc` row up, `weight_unused`; RP74-8 doc; RP74-9 reserved by first segment; RP74-10 `/metrics` on `NODE_METRICS_ADDR`; RP74-11 a heartbeat updates its cached row; D-EDGE-45 the sealed chunk's one retry | `TestNode_*`, `TestNodeRole_RefusesAPublicBindWithoutTLS`, `TestNode_ServesTheOperatorsCertificate`, `TestCFRange_…`, `TestDataPlaneSLC_AWholeReadIsVerified`, `…AWrongTrailerOnAMoveIsTheClients400`, `…TwoWritesShareOneClient`, `…TheHostsBudgetIsTheOriginsChunkBudget`, `…TheSLCRowMustBeUp`, `TestByteBudget_*`, `TestHost_TwoCodecWritesUnderOneBudgetSerialise`, `TestHost_ASealedChunkPutWithNoAnswerIsRetriedOnceOnAFreshConnection`, `TestEdgeHeartbeat_UpdatesItsCachedRowAndFlushesNothing`, `TestNodeReservedSegments_AreTheRoutersReservedNames` |
| **#1031** PR A (merged `99651f8e`, deployed 15:05 UTC) | migration 113 `blob_copies`; `MIRROR_LEGS`, `edge_mirror`; the mirrored PUT (two links, one commit, the copy row in the flip's transaction), the codec's mirrored chunks, the Go host's tee, the Worker 0.5.0's tee; the read's mirror fallback; the `blob_copies` job (backfill, copy, orphans, stale); DELETE via the known copies; routing_truth's copy sample; capacity's `copies`; `deploy/edge/mirror-proof.sh` | `TestMirrorLegFor_…`, `TestEdgeMirror_*` (whole PUT, Go host tee, codec chunks, read fallback), `TestBlobCopiesJob_*` (pending copy + MD5 check, delete, backfill, chunk orphan), `TestRoutingTruth_SamplesMirrorCopies`, the capacity test's copies, `TestPutTwo_AStalledMirrorIsCutLooseAndThePrimaryFinishes`, Worker `test/mirror.test.js` + the codec's mirrored cases |
| **#1032** PR B (merged `aea0f79f`, deployed) | `internal/ec` (the WP-VAULT-1 codec lifted, held byte-identical by `TestEC_ParityIsVaultParitys`, goldens); `sdk/ecwasm` + client `edge-ec` + `make ec-wasm`; `edge_rs` (dark): K+M shard links to a host naming `FeatureRS`, the host's streaming encoder, the `rs` sentinel + shard rows, the origin's rebuild, stale shards on delete/overwrite | `TestEC_*`, `TestRSShardLegs_NoLegHoldsMoreThanM`, `TestEdgeRS_*` (any K rebuild with a whole leg lost, a range across a stripe boundary, delete and overwrite remove the shards, only where it can hold), node `test/ec.test.js` |
| **#1030** PR C (merged `3bd75434`, deployed) | `standard_sealed_default` (global only, default OFF, 409 `master_key_absent` / `tenant_keys_missing`), the placement's encryption requirement, `edgeCodecFor`, the BACKEND_ENCRYPTION.md cell | `TestStandardSealedDefault_*` (2/12/200 MiB sealed, no plaintext on the backend; 503 without a key; the codec implied), `TestBackendEncryptionDocMatchesFlagDefault`, the dashboard's 409 |
| **#1033** PR D (merged `eb9e20e1`, deployed 16:08 UTC) | migration 114; `cmd/tools/catalogue-seed`; `internal/catalogue` (versioned Bloom filter, goldens) + `sdk/wasm` `catalogueLoad/Contains/Free`; the `catalogue_filter` job; dedup GC and the chunk move never touch `_public`; the index's public path and the commit's `_public` references; the Worker loads `CATALOGUE_URL` | `TestFilter_*`, `TestSeed_RowsTheIndexAndOneBlobPerChunk`, `TestDedupGC_NeverCollectsAPublicChunk`, `TestCatalogueFilterJob_PublishesTheFilter`, `TestCatalogue_ASeededChunkIsReferencedWithoutAnUpload`, `TestCatalogue_AFalsePositiveUploadsAndNothingEntersThePublicScope`, Worker `test/catalogue.test.js` |
| **#1034** (merged `65ca7d5a`) | found by prod's first backfill runs: the copy pass stops 90 s before the run's deadline and a run out of time is a note, never an error; the copy pass copies 8 at once (a blob ≥ 1 GiB alone), up to 2,000 rows a run | `TestBlobCopiesJob_ARunOutOfTimeIsANoteNotAFailure`, `TestBlobCopiesJob_CopiesSeveralAtOnce` |
| **#1035** (merged `44d99f24`, deployed 16:50 UTC) | the Worker 0.5.0's manifest rows at `eb9e20e1`: `edge-ingest` / `edge-codec` 0.5.0, `sdk-wasm` 0.3.0, `edge-chunker` 0.1.1 — appended to the log at its boot as entries 115–118 | `TestClientBuildManifest_Valid` |
| PR E (this) | this note, DEPLOY/STATUS/plan/CLAUDE.md rows, BACKEND_ENCRYPTION.md, the ceremony checklist in the prompts file, the ledger row | — |

## Step 0 — the probes

### 0.1 — `ReadableStream.tee()` in workerd with two consumers of unequal speed
`wrangler dev --local` (4.151.3), a Worker teeing a 1 GiB request body into two `fetch` PUTs: one sink reading as fast as it can, one paced at 20 MB/s. **Both branches finished together at 51.1 s (1 GiB / 20 MB/s) and the workerd processes peaked at 44 MiB RSS**: workerd's tee is backpressured — the fast branch waits for the slow one; nothing buffers. So the Worker tees (D-EDGE-46), and the mirror is aborted when it falls behind its deadline, which also frees the primary.

### 0.2 — the RS codec's rate in WASM vs native
`internal/ec` 4+2 over 64 MiB in 8 MiB stripes: **native 13.0 GB/s** (`BenchmarkEncode_4plus2`, klauspost/reedsolomon's SIMD, the Mac M-series); **the same code as `ec.wasm` in node: ~0.69 GiB/s** (≈ 19× slower, copies across the JS boundary included). The SPEED bench's 26–29 MB/s per stream in a Worker was end-to-end (isolate, six connections, 48–96 shard PUTs) — the reason RS runs on a Go host (D-EDGE-51), not this codec's rate.

### 0.3 — the catalogue filter for 10 M chunks at 1 %
`catalogue.New(10_000_000, 0.01)`: **11.98 MB, k = 7**, built in 2.6 s; loaded into `e2ee.wasm` in 17 ms; **46.7 k checks/s** in node (≈ 21 µs a check, the JS↔Go call dominating — at ~1 MiB a chunk, never the bottleneck); measured false positives **1.01 %** over 200,000 absent hashes. 12 MB fits the 128 MB isolate beside the codec's 96 MiB budget only just: a catalogue of 10 M chunks is the ceiling for one filter in a Worker (RB6-7).

## Decisions

- **D-EDGE-45 — a sealed chunk's PUT is retried once on a fresh connection; a mover's never** (RB5-5 answered, RP74's open question 1). The chunk is in memory on the host (the request is replayable): no answer → sealed again and sent once more with keep-alives off; a backend's answer is never retried. A streaming body cannot be replayed: the client's 503 stands.
- **D-EDGE-46 — the Worker tees the mirror** (Step 0.1): `body.tee()` into two fetches, the mirror aborted past 30 s + the body at 2 MiB/s or when the primary fails; a failed or aborted mirror is only a missing ETag (the copy is the job's). No JavaScript reads the bytes.
- **D-EDGE-47 — the mirror leg is decided once, at the admit** (`edge_admits.attrs.mirror_leg`): the index's second links and the commit's copy rows read it from the row, so a breaker that opens between admit and commit never makes a row name a leg the bytes are not on.
- **D-EDGE-48 — a copy is the same blob at the same address on its leg**, `ready` only when the leg's ETag is the primary's MD5; every row carries the primary's MD5 so the job's repair copy is checked against it (a multipart `-N` object is checked by size). The repair copy streams through SLC (the 2× lane): rare by construction — the edge writes the copy when the leg answers.
- **D-EDGE-49 — an aws-chunked PUT and a multipart upload are mirrored by the job**, not at the admit: a header-signed request names one backend's signature, and a part would need its own backend upload on the leg. They commit with a `pending` row (an aws-chunked PUT) or are found by the backfill pass (a completed upload).
- **D-EDGE-50 — turning `edge_mirror` on IS the backfill**: the job enqueues a `pending` row for every whole object of a mirrored tenant without one (1,000 a run, ≤ 50 GiB of copies a run). Vault-floor objects are never mirrored (their second copy is the parity); origin-chunked objects are not (their chunks carry no copy rows; codec chunks do).
- **D-EDGE-51 — RS runs on a Go host only** (`dataplane.FeatureRS`: a node, host=slc). The prompt's step 9 had the Worker stripe through `ec.wasm`; its own refreshed SPEED inputs say "never in the Worker", and the plan 40.10 text agrees — `ec.wasm` is built (client `edge-ec`) for an SDK client and the Step 0.2 number, and the Worker is never answered shards.
- **D-EDGE-52 — RS geometry 4+2, 2 MiB pieces (8 MiB stripes), round robin over the placement and `MIRROR_LEGS`, data shards first, no leg over M** — so it needs three legs; the prompt's "two fake fast legs" cannot hold 4+2 with ≤ M each, and the lab fixture has three.
- **D-EDGE-53 — an RS object is a head row on the `rs` sentinel plus one `blob_copies` shard row per shard** (`<address>.s<i>` on its leg, the geometry on the row); only a whole PutObject of ≥ 8 MiB into an unversioned bucket. The origin GET rebuilds (lazy parity: a read touches the data shards unless one fails; any K; never wrong bytes); DELETE and an overwrite make the shards stale; `routing_truth` knows the sentinel and samples the shards as copies.
- **D-EDGE-54 — one codec, held by a test**: `internal/ec`'s parity is `vault_parity`'s byte for byte; `vault_parity`'s live path (ON globally on prod) is left as it is.
- **D-EDGE-55 — `standard_sealed_default` excludes Public and Private**: SSE-C writes are sealed by the customer's key already; a PUBLIC-class write is meant to be read. Its refusal is `flags.Refusal` (Code + Count), the API's and the dashboard's 409 alike.
- **D-EDGE-56 — the WASM filter is a handle**: `catalogueLoad(bytes) → handle`, `catalogueContains(handle, hash)` — a 12 MB filter is copied into Go once per isolate, never per chunk (the prompt's `catalogueContains(filter, hash)` would copy it per call).
- **D-EDGE-57 — the catalogue is published on `r2` under the reserved tenant `_catalogue`** (`catalogue/<version>.bloom`, then `catalogue/latest`), and the Worker reads `CATALOGUE_URL` (unset = no filter). Which public URL serves the `vaultaire-public` bucket is a deploy setting (RB6-6).
- **D-EDGE-58 — `_public` is never collected or moved**: dedup GC skips the scope (a seeded chunk has no tenant reference to keep it), the chunk move never walks it (seeded at its one address); a commit may reference `_public` only for a plain hash the index answered public on this admit (`attrs.codec_public`).

## The lab proofs

**Mirror** (`deploy/edge/mirror-proof.sh`, 2026-10-28, the Mac: a local control plane on a private migrated DB, the bench iDrive us-central-1 pair as the primary, the bench Lyve us-west-1 pair as the leg, `MIRROR_LEGS=lyve`, writes through host=slc):
- *whole*: a 12 MiB PUT — one commit (`admit put host=slc committed mirror_leg=lyve`), the row on idrive, the copy `ready` on lyve with the body's MD5 `e104cee7…`, `head-object` on each leg itself: 12,582,912 B on both.
- *fallback*: the primary's blob deleted on iDrive with the bench pair → the GET through the control plane MD5-equal from Lyve, `vaultaire_blob_copies_reads_total{outcome="served_mirror"} 1`.
- *backfill*: a 5 MiB object written with `edge_mirror` OFF had no copy; the flag ON and one job run: `enqueued 1, copied 1, copied_bytes 5242880`, the copy on Lyve.
- *delete*: `aws s3 rm` → the blob gone on both legs (iDrive and Lyve asked directly), the job: `orphaned 1, deleted 1`, no row left.
- *codec* (the lab master key, `edge_codec` ON): a 12 MiB codec PUT → 8 stored sealed chunks (`AES256-EW`), 8 copy rows `ready` on Lyve, 8 blobs under `t-_global/_global/_chunks/<tenant>/` on Lyve itself, read back MD5-equal.
- Lesson: with a master key the origin seals a plain PUT (the mover's case runs without it), and a tenant registered under a key cannot sign on a process without it (its S3 secret is wrapped) — the script's `LAB_KEY` restart.

**RS** (the fixture is the lab: three fake legs, `edge_rs` ON, a Go host with `FeatureRS`): a 20 MiB + 123 B body → 6 shards, two per leg; read back equal; a range across the 8 MiB stripe boundary; a whole leg lost (two shards) → equal; a second leg lost → never a 200; DELETE and an overwrite → the job removes all 12 stale shards.

**Catalogue** (the fixture): a 6 MiB seeded artefact → an index call marking every chunk public answers no miss and mints no link; the commit's manifest references `_public` for every chunk; the read is the body; a false positive is a miss in the tenant's scope; a "hit" the index never answered public cannot reference `_public`.

## The prod proof

`edge_mirror` is ON for tenant zero (`tenant-4e1b43062db9d685`) only. Its backfill was the first production use of `blob_copies`: every whole, non-empty head row of its three buckets got a mirror copy on `idrive` (the Lyve rows; the one `idrive` row's copy went to `lyve`). Verified on prod 2026-10-28 18:27 UTC by joining `object_head_cache` (whole, not chunked) to `blob_copies` (kind `mirror`) on the address:

| bucket | primary | head rows | ready copies (size equal) | no copy | bytes |
|---|---|---|---|---|---|
| lightroom-backups | lyve | 14,274 | 14,246 | 28 | 62 GB |
| mac-offload | lyve | 5 | 5 | 0 | 32 GB |
| tz-proof-1792873702 | idrive | 1 | 1 | 0 | 256 MB |

**Ready copies = non-empty whole head rows: 14,252 = 14,252**, each copy's size equal to its head row's. The 28 rows with no copy are all 0-byte objects (Lightroom `.lrcat-data` `*.log` and `LOCK` files), which the backfill skips by design (`size_bytes > 0`: an empty object is rebuilt from its head row alone); the 3 chunked objects of `tz-proof` are not mirrored (Part A mirrors whole objects and codec chunks). 46 copies needed a retry (32 on the second attempt, 11 on the third, 3 on the fourth): Lyve us-east-1 answered GET 500 or timed out on some `lightroom-backups` objects mid-backfill — the 2026-10-21 shape — and each was copied on a later run; none gave up. The job's last run is `ok`, `pending 0`, `enqueued 0`.

The first backfill run on prod ended `error` (the orphans pass ran into the run's deadline) and copied serially; #1034 reserved the deadline's tail, made the stops a note (`stopped`) and copied 8 objects at once (an object ≥ 1 GiB alone). After it, a run copied 2,000 objects (up to 36 GB) in about a minute — RB6-10.

## Residuals

- **RB6-1 — the Worker 0.5.0 is DEPLOYED** (2026-10-28 17:24 UTC, version `ebb3decb-dc20-42f8-95a1-ec3786a3c775`, from a clean checkout of `44d99f24` with every module's sha256 equal to its row — `index.js` `bf3c6ea5`, `codec.js` `301d89b8`, `e2ee.wasm` `4d2d94d8`, `chunker.wasm` `c48b60ca`, `seal.js`, `awschunked.js`, `wasm_exec.js` their 0.4.0/0.3.0 rows; `ec.wasm` left out); it folds 0.4.1 (RB5-1 closed). Smoke from the box with the bench tenant: a 6 MiB GET through `s3.stored.cloud` admitted and committed at the edge, MD5-equal; the PUT (aws-cli's aws-chunked body on an iDrive placement) passed through to the origin as D-EDGE-33 says. No RB-9 403 on the first requests after the deploy.
- **RB6-2 (P3) — a mirrored chunk the host wrote to the leg but never committed is not swept.** `chunk_orphans` walks the primary only; a whole PUT's leg address is queued (`queueEdgePutOrphans`), a codec chunk's is not. Bounded by the codec's claims (a claim that expires leaves at most that admit's chunks).
- **RB6-3 (P3) — RS is taught to GET, DELETE, overwrite, the job, routing_truth and capacity only.** CopyObject from an `rs` source, a version of one, Smart demotion, Sia promotion and key re-encryption would meet the sentinel: `edge_rs` is limited to whole PUTs into unversioned buckets, and must stay dark until each is taught or refused.
- **RB6-4 (P3) — an RS shard carries no per-shard digest the read checks**: a whole read is checked against the object's MD5 as it streams (a corrupt data shard cuts the body, never wrong bytes); a range is not checked.
- **RB6-5 (P4) — the mirror fallback is not on a windowed read** (≥ 64 MiB at the Worker): the origin serves.
- **RB6-6 (P3) — `CATALOGUE_URL` is unset on prod**: no public URL for `vaultaire-public` is chosen, no artefact is seeded on prod. The job publishes to `r2`; the Worker reads nothing until both.
- **RB6-7 (P4) — one filter ≤ ~10 M chunks in a Worker** (Step 0.3: 12 MB beside the 96 MiB codec budget in a 128 MB isolate).
- **RB6-8 (P4) — RP74-8's `clientip` hop for registered nodes is not built** (DEPLOY.md says an IP allow-list and a node do not mix).
- **RB6-9 (P4) — `vaultaire_blob_copies_pending` is set at the end of each run**: absent after a boot until the first (6 min).
- **RB6-10 (P3) — the job stops at fixed counts, not at its deadline**: a run enqueues ≤ 1,000 and copies ≤ 2,000, then idles ~9 min of its 10-min slot. Tenant zero's 14,252 objects took 8 runs where one run draining until its deadline (the tail reserve already guards it) would have done. Owed before a tenant of 10⁵–10⁶ objects is mirrored: loop while work is pending and time remains.

## Open questions (for the owner)

1. **The catalogue's first artefacts and the public URL**: which artefacts (an Ubuntu cloud image, a Debian netinst, the Go toolchain?) and which public hostname serves `vaultaire-public` for `CATALOGUE_URL`.
2. **A third fast leg** for RS 4+2 at 1.5× (today Lyve and iDrive): which, and when — until then `edge_rs` stays dark and the mirror (2×) is the second copy.
