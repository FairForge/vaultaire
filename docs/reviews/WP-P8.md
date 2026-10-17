# WP-P8: Stage 1a Prompt 8

2026-10-17. Four parts:

- **Part 0**: the WP-VM-1 post-merge probe.
- **Part A**: WP-R2-3. A request body the client did not finish is the client's error (R2-09 P1, R108-9).
- **Part B**: `parallel_get` for sealed objects and region-pinned buckets.
- **Part C**: the rest of WP-R3-5, multipart staging on the data disk.

PRs:

- **#733**: Part 0. Tests only: every probe holds.
- **#734**: Part A.
- **#736**: Part B.
- **#737**: Part C.

Deployed: #733 `9fb91f0`, #734 `24be833`, #737 `5946aff`, #736 `314320c` (prod since 16:35 UTC, 2026-10-17).
- this PR: docs only.

**No migration.** The next number stays **093**.

## Decisions

1. **The two-slot window of an addressing migration** (RVM-1, measured with the real previous binary):
   - The owner chose **a checklist line in `docs/DEPLOY.md`, no guard** (2026-10-17).
   - Prod had 0 legacy chunked rows at the 092 switch and has 0 now; that window has closed.
   - The guard belongs one deploy ahead of the *next* migration that gives an artefact an address.
2. **One client-body sentinel.**
   - Prompt 2b.2 C1 already built most of WP-R2-3 under the name `engine.ErrCallerAborted`:
     - `engine.Put` / `PutOn` tag a source read error;
     - `isBackendFailure` says false for it;
     - the failover walk stops on it;
     - `bodyReadErrorCode` answers `IncompleteBody`.
   - It *is* the `ErrClientBody` the prompt names. A second name for the same fact would split every check in two.
3. **The answer is 400 `IncompleteBody`** for every body the client did not finish, aws-chunked framing included.
   - `MalformedPOSTRequest` is AWS's code for a browser-form POST and is not used.
   - A body below Sync's minimum rate keeps **408 `RequestTimeout`**.
   - A client that went away is answered the same 400. Nobody reads it, but the access log and the 5xx count see a 4xx.
4. **No alert** on `vaultaire_client_body_errors_total`. It is client traffic; `deploy/monitoring/README.md` says when to read it.
5. **A sealed parallel read derives its key once.**
   - `crypto.StreamRangeOpener` binds the AEAD, so an SSE-C key is still zeroed at open.
   - A part is whole segments.
   - The window holds plaintext only, so the process budget counts what it holds.
6. **A new metric, not a relabelled one.**
   - `vaultaire_large_get_parallel_reads_total{backend,path}` names the backend a parallel read took (registered drivers only).
   - The existing series keep their labels and gain `path="sealed"`.
7. **Region-pinned buckets needed no code.**
   - The reader already covered them (finding B1 below).
   - What changed is that a read now says which backend it took.
8. **The staging directory is one per process**, set at boot.
   - A rejected value is a Warn and the default, never a boot failure.
   - The prod choice, `/opt/vaultaire/data/multipart`, is the owner's `.env` line. It is not set by this prompt.

## What was built

### Part 0: the WP-VM-1 post-merge probe (#733)

| # | Probe | Verdict |
|---|---|---|
| 1a | Two-slot window, a real `e209950` on lab port 8301 beside `main` on 8302, one private DB through 092: a legacy chunked object (manifest at `<key>`, written by the OLD slot), overwritten chunked on the NEW slot, then read on the OLD slot | **200, the old bytes under the new ETag**. Same size: all 80 MiB of A under B's ETag (md5 of A). Smaller: the first 70 MiB of A under C's ETag (`wrote more than the declared Content-Length` in the server log only). RVM-1 as corrected; checklist line added |
| 1b | An object chunked on the NEW slot, overwritten and deleted on the OLD slot | holds. See the steps below |
| 2 | A Suspended plain DELETE of a chunked object under COMPLIANCE, GOVERNANCE (no bypass) and legal hold. Shapes: a pre-versioning chunked object, and one written Suspended under the flag. Paths: DELETE and DeleteObjects | holds: AccessDenied. Unchanged: the head row, the key's version rows, the retired queue, the manifest and the bill |
| 3 | A chunked copy into an Enabled bucket under `versioned_chunking` (2 PUTs + 3 manifest copies), then DELETE of every version | holds. See the steps below |
| 4 | `TestLedger_RandomSequence_*` | gains a Suspended bucket in every scenario (chunked writes into it, R61-5 deletes from it) and the manifest copy as an op of its own; 10 runs × 4 scenarios × 60 steps green |
| 5 | Prod, read-only | below; one correction to the meta review |

Probe 1b, step by step:

1. The OLD slot's overwrite writes its own manifest at `<key>`.
2. The OLD slot's delete releases that manifest by key.
3. The addressed manifest is left with no row naming it.
4. Inside the grace, the collector queues nothing.
5. After the grace, the orphan pass queues it (`orphan_manifests: 1`).
6. The next run releases it (`due 1, deleted 1`).

Throughout, `tenant_quotas` equals the billed bytes, and the GCI ref counts equal the refs (161 / 161, 0 mismatched, 0 negative).

Probe 3: the ledger equals `BilledBytes` after each of the 10 steps. 5 manifests are retired, the first collector run releases 5 and the second releases 0. A live object sharing the chunks keeps exactly its ref counts, so nothing is released twice.

### Part A: WP-R2-3 (#734)

- **`clientBody`** (`internal/api/client_body.go`) is the body as every PUT path reads it:
  - every read error wraps `ErrCallerAborted`, so the chunker, the sealer, the MD5 tee and the driver all hand the handler an error it can tell from the backend's;
  - it remembers that it failed, for a driver that drops the chain.
- **`answerClientBody`** is the one answer for whole, sealed, chunked, chunk-encrypted and UploadPart:
  - 400 / 408;
  - one Info line;
  - `vaultaire_client_body_errors_total{op=PutObject|UploadPart,reason=short|gone|framing|too_slow}`.
  - A signed-payload hash mismatch keeps `XAmzContentSHA256Mismatch`.
- **The chunked PUT** (R108-9) answered 500, with an Error line and a 5xx count, to a body that broke off. The same happened to a client that went away while the store workers saw only `context canceled`.
- **`storeChunkedObject`** reports a cancelled request context. The streaming chunker stops *without an error* when the context ends: after a half-close, Go's server cancels it once the body is read. The write then measured 0 bytes and blamed the client's declared length (IncompleteBody). That race also turned a **backend failure** into a 400 from time to time.
- **aws-chunked framing is strict** (`errAWSChunkedFraming`). Before:
  - a chunk size that is not hex was a 500;
  - a chunk not followed by CRLF was re-synced on the next line (the bytes were wrong);
  - a stream that ended inside a chunk or before its terminal chunk ended "cleanly".
- **The driver sweep** (R7's open item): `TestDriverWritePaths_WrapTheirErrorsWithW` reads every driver write path. No `fmt.Errorf` formats an error without `%w`: 0 violations today, and a `%w → %v` mutation in `s3upload.go` fails it.
- `deploy/monitoring/README.md`: the metric, and why no rule pages on it.

### Part B: `parallel_get` for sealed objects and region-pinned buckets (#736)

- **`openParallelSealed`** reads a sealed whole object as parallel ranges aligned to its segments. Eligibility:
  - SSE-S3 or SSE-C in the streaming format 0x03;
  - at least `LARGE_GET_PARALLEL_MIN_BYTES`;
  - on a backend with real ranged reads;
  - not vault floor, not chunked, not Geyser.
- How a read runs:
  - one ranged read for the header;
  - the key derived once (`SSEService.RangeOpener`, `SSECRangeOpener`);
  - parts of `LARGE_GET_PART_BYTES` worth of whole segments (at least one);
  - each part reads the ciphertext span of its segments and opens it as it streams.
- The plaintext path's rules:
  - the first part is read before any byte is sent. A failure falls back to the single sealed stream, which answers it; a wrong SSE-C key is still a 403 before the status line;
  - every range must report the stored length and one ETag (`rangeIdentity`);
  - the breaker sees one outcome, and bytes that do not open charge it nothing;
  - egress is counted once.
- `parallelSource` / `startParallel` are the gate and the start both paths share. The window is sized from the largest part.
- **B1, the finding.** The 2026-10-14 bench (`TIERS-BACKENDS-CLOUDFLARE-2026-10-14.md` §2.2, §7.6) read a Frankfurt-pinned bucket at 4.1 MB/s and concluded "`parallel_get` does not cover region drivers yet". It does, on `main`:
  - the row names `idrive-<region>`, its driver is the same `*IDriveDriver` as the primary, and `ParallelRangeSource` reads the row's backend. The red run proves it: the pinned bucket's *plaintext* case passes on `origin/main`;
  - the lab ran its fresh tenant with the flag **off**. No trace sets it in the lab DB (the scripts that set `parallel_get` target prod's bench tenant), and the lab log has no parallel-get line;
  - its fast Dallas 256 MiB reads (127 / 182 MB/s) were the **chunked** path, which a pinned bucket never takes (`chunkingDisabledByRegion`);
  - today Frankfurt read single-stream at 23–36 MB/s from SLC, so the 4.1 MB/s looks like one connection's bad luck (the bench's own §7).
- **`vaultaire_large_get_parallel_reads_total{backend,path}`** answers the question next time.

### Part C: `MULTIPART_STAGING_DIR` (#737)

- Default `/tmp/vaultaire-multipart`: nothing changes until the operator sets it.
- Validated once at boot:
  - absolute, cleaned;
  - created 0700;
  - a directory;
  - writable (a probe file created and removed).
  - A rejected value is a Warn and the default.
- One Info line, `multipart staging`, names the directory with `free_bytes`, `total_bytes` and `default`.
- Everything uses the one directory:
  - `multipartDir` / `partFilePath`, built only after `validUploadID`;
  - the guard's `statfs`;
  - the reaper's orphan sweep;
  - the deletion runner's staging removal.
- `deploy/systemd/vaultaire@.service` (a comment): both slots read the one `EnvironmentFile` as `user1`, so a staged part survives a slot switch. Keep it so: no `PrivateTmp=`, no per-slot value. The prod recipe is in `docs/DEPLOY.md` § Multipart staging.

## Before → after

Before = the red commits' tests on `origin/main` `5c383e8`:

- Part A: **22 / 25 red**. The 3 green are the controls: a well-framed aws-chunked body is stored, and a backend failure stays a 5xx, whole and chunked;
- Part B: **6 / 11 red**. The 5 green are the single-stream controls (flag off, under the threshold, vault floor), the tampered-segment cut (the single stream cuts too) and the pinned bucket's plaintext case (B1);
- Part C: does not compile (the setting does not exist);
- Part 0: tests only, green on `main` by design.

| Case | Before | After |
|---|---|---|
| chunked / chunk-encrypted PUT, client stops mid-body (half-closed or killed, a real socket) | **500**, an Error line, a 5xx | 400 IncompleteBody, counted, no Error, no 5xx |
| whole / sealed PUT, the same | 400 (since 2b.2 C1) | 400, counted |
| UploadPart, the same | 400 (since 2b.3 D1.8) | 400, counted; reservation 0, nothing staged |
| aws-chunked: chunk size not hex (PUT, chunked PUT, part) | **500** | 400, `reason="framing"` |
| aws-chunked: no CRLF after a chunk | bytes re-synced on the next line | 400 framing |
| aws-chunked: ends inside a chunk / before the terminal chunk | ended "cleanly" (a short body) | 400 framing |
| a backend failure on a complete chunked body | 503; **400** when the client had half-closed (the race above, seen while probing) | 503, counted, breaker charged, never the client's |
| 20 aborts in a minute (whole + chunked) | breaker closed; 10 × 500, 5 counted 5xx | breaker closed, 0 × 5xx, `CanAsk` ok |
| sealed (0x03) object ≥ 64 MiB, `parallel_get` on | one stream | parallel by segment (SSE-S3 and SSE-C; Range exact; wrong SSE-C key 403) |
| a tampered later segment / an overwrite mid-read (parallel) | — | the body is cut; the breaker is not charged |
| region-pinned bucket, `parallel_get` on | plaintext: parallel (already); sealed: one stream | both parallel; `parallel_reads{backend="idrive-<region>"}` |
| multipart staging directory | `/tmp/vaultaire-multipart`, fixed | `MULTIPART_STAGING_DIR`; the guard and the reaper follow it |

## Local live proof

### Part 0: the two-slot window (Mac)

- Setup: `e209950` and `main` built from git (`-tags lab`, `VAULTAIRE_LAB_LOG_CALLS=1`), ports 8301 / 8302, one DB through 092, one `DATA_PATH`, `STORAGE_MODE=local`.
- Steps and numbers: the table above (1a, 1b).

### Part B

On the Mac (iDrive new account: Dallas primary, Frankfurt pair; lab master key):

| Read | off | on | md5 |
|---|---|---|---|
| 1 GiB SSE-S3, Dallas | 10.5 / 12.3 MB/s | 17.2 / 18.4 MB/s | ✓ |
| 256 MiB sealed, Frankfurt | 7.2 / 5.7 | 7.6 / 9.1 | ✓ |
| 256 MiB plaintext, Frankfurt | 5.4 / 4.2 | 9.9 / 11.1 | ✓ |

The home link caps every row; correctness only. So the lab was re-run from the box.

On the SLC box (`~/vaultaire-bench/lab-p8`):

- Setup:
  - a user-space PostgreSQL 18 under `user1` on 127.0.0.1:55432 (the prod database was not touched; the lab role cannot create one);
  - port 8110, the B+C build;
  - the new iDrive account, Dallas + Frankfurt;
  - `chunking` off for the lab tenant (whole sealed objects);
  - one `s3api get-object` per read, so the server's own reader is what's measured.

| Read | `parallel_get` off | on | md5 |
|---|---|---|---|
| 1 GiB SSE-S3, Dallas (×3) | 219 / 166 / 176 MB/s | **254 / 310 / 232 MB/s** | ✓ ×6 |
| 256 MiB SSE-S3, Frankfurt-pinned (×2) | 36 / 23 | 24 / 55 | ✓ |
| 256 MiB plaintext, Frankfurt-pinned (×2) | 29 / 25 | 42 / 37 | ✓ |

- Metrics after: `vaultaire_large_get_parallel_reads_total{backend="idrive",path="sealed"} 3`, `{backend="idrive-eu-central-1",path="ranged"} 2`, `{…,path="sealed"} 2`.
- 0 fallbacks (budget, first part); no warn or error line in the lab log.
- The lab's objects were deleted through the lab, the user-space PG stopped and its data removed; logs kept.

### Part C

The same SLC lab, with `MULTIPART_STAGING_DIR=~/vaultaire-bench/lab-p8/staging`:

- the boot line: `multipart staging dir=/home/user1/vaultaire-bench/lab-p8/staging default=false free_bytes=3683071553536 total_bytes=3900832178176`;
- during the 1 GiB `aws s3 cp`: 16 parts in the staging directory and 0 directories under `/tmp/vaultaire-multipart`;
- the Complete read them back; the GETs' md5 above.

## Prod proof

### Part 0: the prod read, before anything was deployed

2026-10-17 14:44 UTC, `e367364`, read-only (psql as the app role, `journalctl`).

| Read | Value |
|---|---|
| `retired_blobs` | 0 rows; the `retired_blobs` job `ok` 14:42:57, `{"due":0,"deleted":0}` |
| head rows | 501, 0 chunked, 0 legacy chunked (`storage_key` NULL) |
| `object_versions` | 0 |
| `object_metadata` / `tenant_chunk_refs` | 0 / 0 |
| `global_content_index` | 8,785 rows, 0 with `ref_count > 0` |
| `dedup_gc` | `ok` 03:20:57, "793 chunk(s) not swept" (the dead iDrive key, unchanged cause) |
| `job_runs` | 18 / 18 `ok` |
| `level:error` since 2026-10-14 21:16 | **309**. **306** are `sweep chunk` lines from `dedup_gc` on the dead iDrive key (100 on each of 10-15, 10-16, 10-17 at 03:xx). **3** are the VM-1 proof's Cloudflare-cut PUTs (`chunked upload failed`, 10-15 01:43, R108-9) |
| warnings | 86,832; 86,648 `backend health check failed`: `idrive` and its 10 regions (≈ 7,858 each, dead keys), `wasabi` 199, `permafrost` 24 |

**Correction to the meta review** (WP-VM-1 § Post-merge, M9): "the only `level:error` lines since the deploy are the three Cloudflare-cut proof PUTs" missed the 306 nightly `sweep chunk` errors (RP8-1).

### Part A, after #734 deployed (`24be833`, 15:43 UTC)

2026-10-17 15:51 UTC, bench tenant `tenant-14a623b16b3f7012`:

- curl 8.18 `--aws-sigv4` on the SLC box, `--limit-rate 4M`, killed with SIGKILL at 2 s (the R2-09 live case);
- the keys read inside a 0600 script from the box's `.env.bench`, never printed.

| PUT | Route | Status | Latency |
|---|---|---|---|
| 20 MB (whole) | through Cloudflare | **400** | 1.97 s |
| 90 MB (chunked) | through Cloudflare | **400** | 2.05 s |
| 20 MB | origin (`--resolve stored.ge:443:127.0.0.1`) | **400** | 2.00 s |
| 90 MB | origin | **400** | 2.00 s |

- After the four cuts:
  - `vaultaire_client_body_errors_total{op="PutObject",reason="gone"} 4`;
  - `vaultaire_errors_total 0`;
  - 0 `level:error` lines;
  - the `lyve` breaker `closed`;
  - nothing kept: `used 0`, 0 head rows, 0 retired.
- A control 5 MB PUT + GET: md5 equal. The bucket was deleted.
- Before (R2-09, R108-9): 503 on the whole PUT before 2b.2 C1, and 500 on the chunked PUT (three on prod, 10-15).

### Part B

**Not provable on prod, by construction.** There are no sealed objects without the master key (F-14 ceremony not done), and no live region pair (every iDrive key answers AccessDenied to object calls since 2026-10-11). Proved on the SLC lab above.

### Parts B and C, after #736 deployed (`314320c`, 16:37 UTC)

The bench tenant has `parallel_get`, `vault_parity` and `sync_backend` on.

- **The boot line of the new slot** (8001; #737 and #736 both): `multipart staging dir=/tmp/vaultaire-multipart default=true free_bytes=64777670656 total_bytes=65049436160`. The default is unchanged (the tmpfs), no rejected value, 0 `level:error`.
- A 100 MiB `aws s3 cp` (2 × 64 MiB parts):
  - 2 parts staged in `/tmp/vaultaire-multipart` mid-upload, 0 directories left after;
  - the Complete was placed as a PUT (chunked, backend `chunked`);
  - a one-request GET: 61 MB/s, md5 equal;
  - deleted, the bucket deleted, `used 0`, 0 active uploads, 0 `level:error`.
- `vaultaire_client_body_errors_total` is exported. `vaultaire_large_get_parallel_reads_total` has no series until the first parallel read (a CounterVec prints nothing before one): no whole object on prod is a parallel candidate today, since every object over 64 MiB chunks.
- **What the check found** (RP8-8, not this prompt's): the Complete took **62.5 s** for 100 MiB, and a 90 MB chunked single PUT through the origin ran at **1.45 MB/s**; a 60 MB whole PUT ran at 28.7 MB/s.
  - The content index shows why. `first_seen_at` is the start of each chunk's store transaction: all 50 (then 45) chunk stores began within 1–2 s, and one of them took about **60 s**, the Lyve client's response-header bound.
  - Straight to Lyve from the box, 40 fresh-connection 2 MiB PUTs took 0.58–0.67 s each, with no stall.
  - Locally, `e367364` and `main` chunk-PUT 90 MB at 84–113 MB/s alike: no code change caused it.
  - The VM-1 proof (10-15) chunked the same shape at 33–41 MB/s.

## Residuals

| # | Sev | Item | Where it goes |
|---|---|---|---|
| RP8-1 | P3 | `dedup_gc` logs one Error line per chunk it cannot ask on a backend that is known dead (≈ 100 a night on the dead iDrive key: 306 since 10-14). Each is a real "not swept", already counted in the job's note. The meta review's "no errors" read missed them | dedup GC's next pass: one Warn per run per unreachable backend, not one Error per chunk; or the iDrive key repair |
| RP8-2 | P3 | `deploy/systemd/vaultaire@.service` on the box differs from the repo by the Part C comment (`deploy/**` is installed by hand) | the next unit change installs it |
| RP8-3 | note | The 10-14 bench report says `parallel_get` does not cover region drivers; it does (B1) | the report's next revision (§2.2, §7.6) |
| RP8-4 | P3 | A Frankfurt-pinned read gains little from parallel ranges from SLC (24–55 vs 23–36 MB/s): each range is one high-RTT connection | `LARGE_GET_PARALLEL_PARTS` per backend, or a hub in Europe (plan) |
| RP8-5 | [YOU] | `MULTIPART_STAGING_DIR` is not set on prod: parts still stage on the 61 GiB tmpfs | `docs/DEPLOY.md` § Multipart staging |
| RP8-6 | P3 | The aws-chunked decoder ignores trailing headers (`x-amz-checksum-*`): a trailer checksum is not verified | WP-R2-4 / the checksum work |
| RP8-7 | note | A sealed blob in the 0x01/0x02 format (before WP-R8-5) or an old-format SSE-C blob stays one stream. Prod holds none | none |
| RP8-8 | **P2** | On the Lyve primary a chunked write stalls about 60 s once per upload: one chunk PUT on a pooled connection hangs until the 60 s response-header bound, then retries. 100 MiB took 62.5 s (2026-10-17 16:37), 90 MB 62 s; fresh-connection PUTs show no stall. The fixed-bucket drivers have `FIXED_BUCKET_PUT_TIMEOUT` (a per-PUT deadline and one retry on a fresh connection); Lyve has only the 60 s bound (2b.5 F3.4). Likely the same edge-stall class as the Wasabi finding of 2026-10-17 | the hedged-PUT work (the Wasabi stall session's fix applied to `lyve`): a per-PUT deadline plus one retry on a fresh connection for the Lyve driver, or a hedge on the chunk store |
| RVM-1 | closed | The two-slot window of an addressing migration | the checklist line in `docs/DEPLOY.md` |

## Gates

- `go test -race -p 4 -timeout 40m ./...` on a private DB, from a clean worktree of each commit: every package ok for the Part A commit (`9153e3b`), Part B (`a15ea24`) and Part C (`afce011`).
  - One run beside other tests on the same DB failed on the adapter fixture's shared e-mail (two processes, one database). It was re-run alone: ok.
- golangci-lint v2.14.0: 0. gosec with the workflow's flags: 0, on the A tree and on the B+C tree.
- Structure tests in that run:
  - new: `TestDriverWritePaths_WrapTheirErrorsWithW`;
  - kept: `TestLedger_NoQuotaStatementAfterACommit`, `TestLedger_QuotaStatementIsLast`, `TestLedger_LockOrder_*`, `TestEngineCalls_CarryTheTenant`, `TestExportSectionsCoverSchema`, `TestChunkBlobCalls_GoThroughTheChunkStore`.
- `build-and-test` green on every PR and on each merge commit on `main`.
  - #736's first run failed one of its own new tests: `TestParallelSealed_SSES3_WholeObjectInParallelByteForByte` counted 9 ranged reads for 6, because a slow runner hedged three ranges. The test now runs without hedging (15cb27f) and passed 3× under `-race -cpu 2`.

## Post-merge review (2026-10-17, the meta session)

Read: the product diffs of #733, #734, #736, #737 and #740 against the note,
the vendor semantics where a claim cites them (the AWS SDK's `manager`
wrapping, Go's server contexts), and prod — read-only first (journal, metrics
and the tables from the 16:35 UTC deploy), then the worker's own `p8speed.sh`
run twice against `9eecdf4` at 17:29 UTC. **Every WP-P8 claim holds; no P0,
no P1 in #733–#737.** The one claim that did not hold is #740's, the Lyve fix
shipped after the note: measured live, fixed the same evening (#742).

| # | Claim probed | How | Verdict |
|---|---|---|---|
| M1 | WP-R2-3 covers EVERY request-body entry point (the pattern that found most earlier gaps: the same logic on the other entry points) | `newClientBody` wraps the body in `HandlePut` (whole, sealed, chunked, chunk-encrypted) and `handleUploadPart`; the management API's direct upload is JSON only (a presigned PUT lands in `HandlePut`); `s3_copy.go`'s `readErrReader` reads a backend source, not a client; no other handler reads `r.Body` into a write | holds |
| M2 | "never a breaker charge" survives a driver that drops the error chain | `isBackendFailure` is false on `ErrCallerAborted` AND on `context.Canceled`; the engine's own `sizeTrackingReader.sourceErr` re-tags a source failure whatever the driver returned (`engine.go:295`, `chunk_address.go:138`); the SDK's `manager` wraps with `%w` (`read upload data failed: %w`, `read multipart upload data failed, %w`, v1.23.14) | holds |
| M3 | `reason="gone"` can only be the client's side of the socket | the server never cancels a request context itself: `Start`'s `WithCancel` feeds the health goroutines only, there is no `http.TimeoutHandler`, `Shutdown` drains without cancelling; HAProxy's and Cloudflare's cuts (the 100 s origin timeout, R108-9) arrive as the client's close, which is what the 400-to-nobody is for | holds |
| M4 | A backend failure on a complete chunked body is never blamed on the client | `storeChunkedObject`'s `pctx` derives from the request context; `pool.join()`'s error is returned BEFORE the `ctx.Err()` check, so only a cancellation with no store error reads as the client's; `clientBodyFault` asks the REQUEST context, not `pctx` | holds (the "a backend failure on a complete chunked body → 503, counted, breaker charged" test row) |
| M5 | aws-chunked strictness has no new hole | `ParseInt(line, 16, 32)` + `size < 0` (a chunk over 2 GiB is framing — no SDK sends one); `p` is cut to `chunkLeft` before the read; the CRLF is read with `io.ReadFull` and compared; the terminal chunk's trailers are left unread and Go's server discards up to 256 KiB before reusing the connection (RP8-6 stands) | holds |
| M6 | The sealed parallel read cannot be tricked by a blob | vault floor excluded in `HandleGet`; Geyser excluded by `ParallelRangeSource`; `ParseStreamHeader` bounds `SegmentSize` (`minStreamSegment`..`maxStreamSegment`), so the part arithmetic never divides by zero; `rangeIdentity` is seeded by the header read and every part must match it; `errSealedUnreadable` / `errRangeIdentity` never reach the breaker; the SSE-C key is cleared after BOTH opens | holds |
| M7 | The staging directory cutover is safe | validated at boot, Warn + default on a bad value; the guard, the reaper, `multipartDir`, `partFilePath` and the deletion runner read the one `multipartStagingDir()`; `docs/DEPLOY.md` § Multipart staging names the one hazard (in-flight uploads keep their parts in the old directory → `InvalidPart`; do it when `active` uploads are few) | holds; note: the old directory's leftovers are swept by nobody until the tmpfs empties at boot (documented) |
| M8 | #740: "a stalled chunk PUT now costs the deadline once instead of the 60 s response-header timeout" | `putDeadline(base, size)` returns the whole base for ANY body under one unit, so a 2 MiB chunk's deadline was **60 s** — the same 60 s as `s3ObjectHTTPClient`'s `ResponseHeaderTimeout`, and aws-cli's default read timeout. **Live, 17:30:18 UTC, `p8speed.sh` run 2:** the 90 MB chunked PUT ran 61 s (1.47 MB/s); `put deadline passed, retrying once on a fresh connection … "deadline":60,"bytes":2482615`; 200 ms later `request body not received: the client's … reason=gone` (the CLI's 60 s read timeout, `cfg/retry-mode#standard` in its User-Agent); the CLI's own retry stored the object in 0.6 s (`dedup_ratio 36`); `s3_access_log` holds the 400 | **does not hold** — fixed by **#742** (`s3PutDeadline`: a body under 64 MiB gets the base scaled by its size, never under 10 s; a 2 MiB chunk 10 s, a 16 MiB part 15 s; only the deadline client, `sync` unchanged). Red first (the stub did not exist), green under `-race` |
| M9 | Prod, read-only, 16:35 → 17:45 UTC (`314320c` then `9eecdf4`) | 0 `level:error`; the two new CounterVecs exported at 0 on the fresh slot; every breaker closed; `/health` degraded only by the dead iDrive probes; after the probe `vaultaire_driver_put_retries_total{driver="lyve"} 1`, the probe's buckets and objects deleted | clean |
| M10 | CI on `main` | `build-and-test` green on every merge commit (#733–#740); the docs commit's CI run is `cancelled` by design (paths-ignore) | clean |

### Corrections and residuals

| # | Sev | Item | Where it goes |
|---|---|---|---|
| RP8-8 (re-rated) | **P1 as seen live**, fixed | A stalled chunk PUT on the Lyve primary (one per upload this afternoon) cost 60 s, and every client whose read timeout is ≤ 60 s — aws-cli's default — gave up in the same second: a 400 to nobody, the upload saved only by the client's own retry (instant through dedup). A client with no retry (a browser `fetch`, an SDK with retries off) failed the upload. #740 shortened nothing for a chunk; #742 does (10 s). The stall itself is Lyve us-east-1's | **#742** (`s3PutDeadline`); the hedge for small ops and the probe → Stage 1a Prompt 9 (WP-R6-3) |
| RP8-9 | P3 | Three 60 s timers raced on the data path today: the transport's `ResponseHeaderTimeout` (60 s), the PUT deadline (60 s for ≤ 64 MiB until #742) and aws-cli's read timeout (60 s). Any server-side wait that can reach 60 s is a client-visible failure, and nothing in the repo states that bound. A GET's first byte has no bound but the same 60 s header timeout | Prompt 9, WP-R6-3: every per-request bound on the data path well under 60 s, written down as the invariant with the numbers |
| RP8-10 | P3 | `s3_access_log` holds only error rows in the last two hours (6 × `PutObject` 400, 4 × `HeadObject` 404, no 200) and has no duration column; `/metrics` has no per-driver operation latency histogram — the 60 s mode was found by a probe, not by observability | Prompt 9, WP-R6-3: `vaultaire_driver_op_seconds{driver,op}`; confirm the access log's success-row rule is by design (per-bucket logging) |
| RP8-11 | P3 | A chunk PUT the deadline cut may still be committed by the backend after we gave up: a blob at `_chunks/<hash>` with no `global_content_index` row until the same chunk is uploaded again (then the row appears and the blob is overwritten in place). Dedup GC cannot see it; a cost leak, self-healing on re-upload, never a correctness issue | WP-R8-2 (the orphan sweep) |
| RP8-3 | done | The 10-14 bench report's "`parallel_get` does not cover region drivers" | corrected in place (this PR): findings 6, §7.6, recommendation 6 |

### Handed to Stage 1a Prompt 9, Part 0

1. **Prod proof of #742** once deployed: `p8speed.sh`-shaped chunked uploads until a stall is seen (about one in two this afternoon): the Warn line carries `"deadline":10`, the upload finishes on its first attempt about 10 s late, `vaultaire_driver_put_retries_total{driver="lyve"}` +1, and NO `request body not received` line.
2. **A lab with a stalling backend** (the `stallS3` fake's shape as an `httptest` server behind the `s3` driver, or a one-PUT-holding proxy in front of the lab's backend) and aws-cli at its DEFAULT timeouts: a chunked PUT, a whole PUT and a multipart upload each hit one stall and succeed on the first client attempt with 0 client-body errors; the same with `--cli-read-timeout 20`.
3. **The UploadPart path of the Lyve driver** (a 16 MiB part: 15 s) and the fixed-bucket drivers; the `sync` driver's `putDeadline` unchanged (a 2 MiB PUT to the bridge keeps its 2 min).
4. The RP8-9 invariant written and tested: no per-request bound on the data path above 30 s except CompleteMultipartUpload's 10 min and the per-size PUT deadline.
