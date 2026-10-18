# WP-P9: Stage 1a Prompt 9

2026-10-17. Four parts:

- **Part 0**: the WP-P8 post-merge probe — the deadline every byte-carrying request carries, read off its context, and a stalling-backend lab at aws-cli's default timeouts.
- **Part A**: WP-R6-3. A backend that does not answer: per-class first-byte bounds, the small-call hedge on a fresh connection, `/health` on two failed probes, `vaultaire_driver_op_seconds`.
- **Part B**: WP-R7-6 (the DNS cache asks again after `DRIVER_DNS_TTL` and rotates its addresses) and WP-R7-7 (the Lyve lock guard).
- **Part C**: RP8-1. Dedup GC: a backend that cannot be asked is one Warn per run, never one Error per chunk.

PRs:

- **#745** `b8edd24`: Part 0. Tests, plus the lab tool `cmd/tools/stall-s3`; no product code.
- **#746** `bb430d8`: Part A.
- **#747** `e4b2460`: Part C.
- **#748** `1e87be4`: Part B.
- **#749** `2903d84`: the post-merge review's one fix — a dial never waits on a DNS refresh (§ Post-merge review).

No migration; the next number stays **093**.

Most of this note was drafted by the Prompt 9 worker session. Its docs PR was never opened; Stage 1a Prompt 10 Part 0 assembled it from those drafts, added the post-merge review and the 02:30 UTC `dedup_gc` proof, and landed it.

## Decisions

1. **The knob is `DRIVER_HEDGE_AFTER`, not `HEDGE_AFTER`.**
   - `CHUNK_GET_HEDGE_AFTER` and `LARGE_GET_HEDGE_AFTER` already exist, so a bare name would be ambiguous.
   - It also takes `0`/`off`, so an operator can turn the hedge off without a deploy.
2. **An answer is any HTTP response.** The first one is taken: a 404 is the object's verdict, a 5xx the backend's.
   - A transport error or a header timeout is no answer, so the other attempt is awaited.
   - If neither attempt answers, the first attempt's error is returned.
3. **Per-class bounds are one transport clone per class.** That gives four pools per driver, as the prompt allowed: "a small set of pre-built clients … the connection pool must stay shared per class".
   - `requestClass` reads the class off the request the SDK built.
   - `RestoreObject` (Geyser's `POST ?restore`) is classed with the Complete (10 min): the tape gateway's answer to it has not been measured.
4. **SDK attempts: 2 on a request, 3 on a job.**
   - The SDK takes this decision per call only, so drivers pass `sdkCallOptions(ctx)`.
   - The scheduler marks every run with `common.WithJob`.
5. **`vaultaire_driver_op_seconds` is observed in the engine**, at every driver call it makes; the engine interface is unchanged.
   - Not observed: the canary's direct `Exists`/`Put`, the parallel GET's ranges and the console probe. None of them goes through the engine.
   - A PUT is timed without the time spent inside the source's `Read`, so a slow uploader cannot fire `DriverOpSlow`.
6. **`/health` debounce lives in `BackendHealthChecker.UpdateHealth`**, not in the probe loop. Every reader therefore sees one state: `/health`, the gauge, the dashboard and `/health/ready`.
7. **Target-only is declared by the driver**, through the optional interface `engine.TargetOnlyDeclarer`.
   - The engine's name list stays; the Driver interface is unchanged.
   - `refusedPrimary` refuses any driver that declares itself target-only. Geyser never declares, so it can still be auto-detected as primary.
8. **The Lyve lock check is a boot call, not part of the constructor.** `NewLyveDriver` is used by tests and tools with fake credentials, so it must stay free of I/O.
9. **Permafrost is a residual** (RP9-1). It takes no code in this prompt; the reasons are in the residuals table.

## What was built

### Part 0: the WP-P8 post-merge probe (#745)

Tests and one lab tool; no product code.

- **`put_deadline_bounds_test.go`.** An SDK middleware reads the deadline each operation's context carries at the Initialize step. No test waits the 15 s it proves. Through `Put`, at the default `FIXED_BUCKET_PUT_TIMEOUT`, on the Lyve driver and the fixed-bucket driver:
  - a 40 MiB body uploads as parts of 16, 16 and 8 MiB;
  - the 16 MiB parts carry **15 s**, the 8 MiB part the 10 s floor;
  - a 2 MiB chunk's PutObject carries **10 s**;
  - Create and Complete carry no deadline of ours.
- **The sync bridge keeps `putDeadline`'s whole base** (2 min for a 2 MiB PUT). On the WebDAV fake, scaled down, a PUT the bridge answers 700 ms late is taken on its first attempt. A mutation (`webdav.go` on `s3PutDeadline`) turns the test red.
- **`cmd/tools/stall-s3`.** An in-memory, path-style S3 with the calls the fixed-bucket driver makes, aws-chunked decoded.
  - It holds the chosen requests: a PUT after reading its body, a GET, HEAD or DELETE from its first byte (`-method`, added in #748). It then answers nothing until the client gives up.
  - It is re-armed over `/_stall/arm`.
  - The lab runs `STORAGE_MODE=wasabi` against it. The `s3` driver's endpoint is fixed in code (`us.s3compat.cloud`), so the lab cannot run behind it (RP9-6).

### Part A: WP-R6-3 (#746)

1. **Per-class first-byte bounds** (`transport.go` `WithOpTimeouts`): one clone of the transport, so one pool, per request class.
   - `requestClass` reads the class off the SDK's request.
   - `s3OpTimeouts`:

     | Request | Bound |
     |---|---|
     | HEAD, DELETE, a listing page, bucket and multipart bookkeeping | 15 s |
     | a GET's first byte | 25 s |
     | a PUT's answer after its body (the per-size deadline fires first) | 25 s |
     | Complete, server-side copy, and (since #748) Geyser's restore | 10 min |

   - `geyserOpTimeouts`: GET and PUT 5 min (tape), metadata 15 s.
   - SDK attempts: 2 on a request path, 3 on a background job (`sdkCallOptions`, `common.WithJob` set on every scheduled run).
2. **The hedge** (`hedge.go`).
   - Sent once more on the driver's fresh client after `DRIVER_HEDGE_AFTER` (2 s): HeadBucket (the probe), HeadObject/Exists (the canary too), a listing page (`listPages` walks the pages itself) and DeleteObject.
   - The first answer wins; the loser's context is cancelled.
   - On `idrive` (+ regions, relabelled by `SetBackendName`), `wasabi`, `lyve`, `r2` and `s3`. Not on Geyser.
   - Series: `vaultaire_driver_hedges_total{driver,op}`, `vaultaire_driver_hedges_won_total`.
3. **`/health` on two consecutive failed probes** (`BackendHealthChecker.UpdateHealth`). One success heals.
   - Every failure is still counted (`vaultaire_backend_probe_failures_total`), and its error shown in `last_error` and in the Warn line.
   - Six existing tests encoded "one failure flips health" and now take two.
4. **`vaultaire_driver_op_seconds{driver,op}`**: an engine histogram, buckets 0.05 … 120 s. Ops: head, get_first_byte, put, delete, list, probe.
   - A PUT is timed less the time spent inside the source's `Read` (`sizeTrackingReader.waited`).
   - **`s3_access_log` holding only error rows is by design.** R13-14 / R9-04 record successes only for buckets with server access logging enabled (`shouldRecord`); errors are recorded for every bucket, because the admin support page reads them.
5. **The invariant.** RP8-9 is written in `internal/drivers/CLAUDE.md` with its exceptions, and held by `data_path_bounds_test.go`:
   - every S3-class client's per-class bounds, read from the real constructors;
   - every `TunedHTTPClient(...)` call and every `context.WithTimeout` in the package, listed with what bounds it.
6. `deploy/monitoring/vaultaire-drivers.yml`: DriverHedgesWinning (warning) and DriverOpSlow (warning on the primary); a README row. `DRIVER_HEDGE_AFTER` is in CLAUDE.md.

### Part B: WP-R7-6 and WP-R7-7 (#748)

1. **The DNS cache** (`dns_cache.go`).
   - An answer expires after `DRIVER_DNS_TTL` (5 min). There is one lookup per host per TTL (`singleflight`), and a failed refresh keeps the stale answer.
   - The dials rotate over the answer, and a dial that fails on one address tries the next in the same call.
   - Series: `vaultaire_driver_dns_refreshes_total{host,outcome}`. `ConfigureDriverDNS` sets the TTL at boot, before the first dial.
2. **The Lyve lock guard.** `LyveDriver.CheckBucketLock` runs once at boot from `cmd/vaultaire`, with a 20 s timeout.
   - **Locked** (ObjectLockEnabled, or a default retention):
     - an Error line;
     - `TargetOnly()` true through the new optional interface `engine.TargetOnlyDeclarer`: never a fail-over destination (`buildWriteCandidateList`), never the primary (`CheckPrimaryEligible`, and `refusedPrimary` makes `STORAGE_MODE=lyve` a boot Fatal with the reason);
     - `vaultaire_lyve_bucket_lock_enabled{region}` 1, which pages (`LyveBucketLockEnabled`).
   - **`ObjectLockConfigurationNotFoundError`**: no lock, the gauge 0.
   - **A 403**: `ErrLyveLockUnreadable`, a Warn and no guard.
   - **Anything else**: a Warn.
3. **Geyser's `POST ?restore`** is classed with the Complete (10 min), so Part A's 15 s metadata bound no longer applies to it. The tape gateway's answer time is not measured, and I would not tighten an unmeasured bound.
4. `stall-s3 -method`.

### Part C: RP8-1 (#747)

- `sweep` asks `engine.CanAsk` once per backend per run. A backend with an open breaker, or with no driver, is skipped before its first call.
- A failing backend is asked per candidate; its breaker opens by itself.
- Either way there is one Warn per backend after the loop: `not_swept`, `how` (skipped | asked), `class` and the first error.
- The counts are kept in three places:
  - `DedupGCResult.NotSwept` in `job_runs.result` (`not_swept`);
  - the job's note ("793 chunk(s) not swept (idrive: 793 — the backend could not be asked)");
  - `vaultaire_dedup_gc_unswept_chunks{backend}`, read from the table (`dedupGCCollector`, 1 min cache).
- The run stays `ok`. A leaked blob (row gone, delete failed) stays an Error line.

## Before → after

"Before" is `origin/main` `72d20fd` at the start, which carries #742.

| # | Item | On `72d20fd` | Now | Red test |
|---|---|---|---|---|
| 0.1 | #742 on a stall, on prod | proven by the meta session (18:08) | proven again 18:14 UTC; see Prod proof | — (prod read) |
| 0.2 | Stall lab at aws-cli's default timeouts | — | every shape first-attempt, ≤ 15.7 s; the control reproduces 60 s and a 400 to nobody | — (the lab) |
| 0.3 | UploadPart deadline through Lyve / fixed-bucket; sync unchanged | held; untested end to end | held: 16 MiB → 15 s, 8 MiB → 10 s, 2 MiB → 10 s; sync 2 min | `Test{Lyve,FixedBucket}UploadPart_ASixteenMiBPartGetsFifteenSecondsAtTheDefault`, `TestSyncPut_ASmallPutKeepsTheBridgesWholeBase` (a probe: green on main; the mutation turns it red) |
| 0.4 | The bound invariant | fixed-bucket and lyve 60 s per class; r2, s3compat and s3 unbounded; fresh clients unbounded; Geyser meta 5 min | every data-path bound ≤ 30 s except the listed exceptions | `TestDataPathBounds_NothingWaitsMoreThanThirtySeconds` (29 violations), `TestDataPathBounds_EveryClientAndTimeoutIsListed` |
| 0.5 | Prod read before Part A | — | 0 Error lines since 17:10; dedup_gc "793 not swept"; 18/18 jobs `ok`; lyve put retries 1; client-body errors 0 | — |
| A.1 | Per-operation first-byte bounds | 60 s (or none) for every class | 15 s / 25 s / 25 s / 10 min; Geyser 5 min; SDK 2 attempts (3 on a job) | `TestOpTimeouts_EachClassWaitsItsOwnBound`, `TestRequestClass_TheSDKsShapes`, `TestSDKAttempts_TwoOnARequestThreeOnAJob`, `TestJob_ARunsContextIsMarkedAsAJobs` |
| A.2 | The hedge | none | probe, head, list page, delete hedged at 2 s on a fresh connection; never PUT or GET | `TestHedge_*` (9; red with the hedge removed) |
| A.3 | `/health` on N failures | one failed probe = degraded | two in a row = degraded; one success heals | `TestHealth_OneFailedProbeIsNotAnOutage`, `TestHealth_ADeadBackendIsUnhealthyAtItsSecondProbe` |
| A.4 | Latency you can see | no histogram | `vaultaire_driver_op_seconds{driver,op}` | `TestDriverOpSeconds_*` (4) |
| A.5 | Cut-over proof and rules | — | before/after on prod (below); `vaultaire-drivers.yml` | — |
| B.1 | DNS cache TTL and rotation | cached for ever, `addrs[0]`, no retry on another address | 5 min TTL, singleflight, stale on a failed refresh, round-robin, next address on a dial error | `TestDNSCache_*` (5), `TestDriverDNSTTL_EnvRule` |
| B.2 | Lyve lock guard | none: a locked bucket would take fail-over writes | locked = target-only, a gauge and a page; 403 = Warn | `TestLyveLockGuard_*` (5), `TestEngine_ADeclaredTargetOnlyBackend*` (3 + control), `TestRefusedPrimary_ABackendThatDeclaresItselfTargetOnly` |
| C | dedup GC's error spam | one Error line per chunk, an open breaker asked anyway | one Warn per backend per run; `not_swept` in the result and on the gauge; an open breaker never called | `TestDedupGC_AnUnreachableBackendIsOneWarnPerRunNotOneErrorPerChunk`, `…OpenBreakerIsSkippedBeforeItsFirstCall`, `…TheRunsResultCarriesNotSweptPerBackend`, `…TheUnsweptGaugeIsReadFromTheTable` |

## Local live proof

### Part 0: the stalling-backend lab (Mac)

Setup:

- `stall-s3` on 127.0.0.1:9555.
- A lab server built from `72d20fd` (= `main` with #742), `STORAGE_MODE=wasabi`, `WASABI_ENDPOINT=http://127.0.0.1:9555`, `FIXED_BUCKET_PUT_TIMEOUT` at its default. The fixed-bucket driver uploads through the same `deadlinePutClient` as `lyve`.
- A private migrated database (dropped after), `chunking` on for the lab tenant.
- `MULTIPART_STAGING_MIN_FREE=off`: the Mac's free disk is below the guard's floor.
- aws-cli 2.28.19, fresh random bytes per run (a re-upload deduplicates and stores no chunk). One held PUT per run.

| Run | aws-cli default timeouts | `--cli-read-timeout 20` | First client attempt | `client_body_errors` |
|---|---|---|---|---|
| baseline: 90 MB chunked / 60 MB whole / 100 MiB multipart, nothing held | 0.9 s / 0.8 s / 1.0 s | — | yes | 0 |
| 90 MB chunked PUT, a chunk PUT held | **10.6 s** | 10.5 s | yes, md5 equal | 0 |
| 60 MB whole PUT, part 4 (9.7 MB) held | **10.8 s** | 10.7 s | yes | 0 |
| 60 MB whole PUT, part 2 (16 MiB) held | **15.7 s** | 15.7 s | yes | 0 |
| 100 MiB `aws s3 cp` (multipart → Complete → chunked), a chunk PUT held | **10.8 s** | 10.8 s | yes | 0 |
| control, `FIXED_BUCKET_PUT_TIMEOUT=off`, 90 MB chunked | **61.9 s** | — | **no**: aws-cli's 60 s read timeout, one CLI retry | **0 → 1** (`request body not received … reason=gone`) |
| control, deadline off, 100 MiB multipart | 62.6 s | — | yes (the Complete is a detached long op) | 0 |

`vaultaire_driver_put_retries_total{driver="wasabi"}` rose by one per held PUT.

**The prompt's "≤ 12 s extra" holds for a chunk and for a part under about 40 MiB.** A held 16 MiB part costs its 15 s deadline (Part 0.3's own number), so a whole PUT between 16 and 64 MiB that stalls on a full part is 15 s late. It is still on the client's first attempt.

### Part A (Mac, the #748 tree: A + B)

The same lab, with `stall-s3 -method`.

| Held | `main` (`72d20fd`) | Part A |
|---|---|---|
| a DELETE (`aws s3 rm`) | (a 60 s header bound) | **2.41 s**, hedged: `hedges_total{wasabi,delete}` 1, `won` 1 |
| a GET's first byte (5 MB `aws s3 cp` download) | **60.6 s**: aws-cli's read timeout and the server's 60 s header timeout fired within 5 ms; the server logged `engine get failed` (an Error line, a 5xx to nobody), the CLI's retry got the bytes | **26.5 s**: cut at the 25 s bound, the SDK's retry served it on the client's first attempt, md5 equal |
| the canary's HEAD (the probe) | a 15 s probe failure | hedged and answered: `hedges_won_total{wasabi,head_object}` 1, no probe failure |
| one probe (the bucket HEAD and its hedge) | `/health` degraded | `vaultaire_backend_probe_failures_total` +1, `/health` **healthy**, gauge 1 |
| two probes in a row | — | 19:26:25 one failure: `healthy` (failures 4); 19:26:56 the second: **`degraded`**, `probe failing on wasabi`, gauge 0 (failures 5); 19:27:11 one success: `healthy` again |


## Prod proof

### Part 0: the prod read before Part A (`72d20fd`)

- 0 `"level":"error"` lines since 17:10 UTC.
- `dedup_gc`'s last row: `793 chunk(s) not swept (see the log)`.
- 18 of 18 jobs `ok`.
- `vaultaire_driver_put_retries_total{driver="lyve"}` 1; `vaultaire_client_body_errors_total` 0.
- **#742 on a stall, again (18:14 UTC):** every UploadPart of one 60 MB PUT stalled on Lyve us-east-1, one of them again on its fresh-connection retry. The PUT answered 503 at 32 s and the client's retry succeeded. At 19:13–19:19, 150 of 150 were clean. **Lyve us-east-1's stalls come in episodes** (RP9-3).

### Part A, before and after (Wasabi and Lyve, probe-shaped calls)

| Window | Lyve | Wasabi pooled | Wasabi hedged |
|---|---|---|---|
| before, 18:26–18:36 (`72d20fd`, a client-side probe) | pooled and hedged 300/300 each, p99 183 / 372 ms | 299/300, one 15 s failure, p50 0.9 s | 300/300, max 5.3 s, 2 won |
| after, 19:59–20:41 (`1e87be4`) | — | 0 probe failures, mean probe 0.33 s over 85 probes | `hedges_total{wasabi,head_bucket}` 4, won 3; `head_object` 1 of 1 won (on the earlier slot) |

- 0 Error lines.
- **Wasabi's probe history:** 765 of 767 failures since 10-06 were single; so were 199 of the 199 in the 2.7-day window the prompt cites. Two in a row removes all of them from `/health`.
- After each slot switch `/health` read `healthy` for about 30 s before the dead iDrive probes went `degraded`: RP9-9, fixed in Stage 1a Prompt 10 Part 0.3.

### Part B

- Boot line `lyve: bucket has no object lock` (us-east-1); `vaultaire_lyve_bucket_lock_enabled{region="us-east-1"} 0`.
- Lyve's data key (not the probe pair) answers `ObjectLockConfigurationNotFoundError` on both `stored-*` buckets.
- `vaultaire_driver_dns_refreshes_total` is present per host.
- Wasabi's name resolves to 14 IPs. The pooled connection is now on .107, not the .101 that was pinned from boot.

### Part C

See § Post-merge review: the first run after #747 is the 2026-10-18 02:30 UTC run.

## Residuals

| # | Sev | Item | Where it goes |
|---|---|---|---|
| RP9-1 | P3 | Permafrost's Graph calls have no per-class bound and no ctx-aware bounded retries: throttling and the `/content` redirects are unmeasured | WP-R7-4 (Permafrost conformance) |
| RP9-2 | [YOU] | Advisory GO-2026-5932 (`x/crypto/openpgp`; no fix, not called) posts a code-scanning review thread on every PR, and `required_review_thread_resolution` blocks auto-merge until it is resolved | dismiss the alert in the repo's code scanning |
| RP8-11 | P3 | A chunk blob cut by its deadline after the backend stored it has no row | WP-R8-2 (the orphan sweep) |
| RP9-3 | owner | Lyve us-east-1 (prod's Lyve region) stalls in episodes, including on fresh connections; us-west-1 was clean the same minutes | owner question: the Lyve region |
| RP9-4 | P3 | A GET's first-byte bound 25 s × 2 SDK attempts ≈ HAProxy's 50 s | the bound invariant's next revision |
| RP9-5 | note | The Sync bridge's bounds are above 30 s (its own rule, the listed exception) | none |
| RP9-6 | P3 | The `s3` driver's endpoint is hardcoded (`us.s3compat.cloud`), so no lab can run behind it | WP-R7-3 (delete `quotaless.go` + `S3Driver`) |
| RP9-7 | note | `VAULTAIRE_TUNED_TRANSPORT=false` drops every bound (`http.DefaultClient`) | documented in `internal/drivers/CLAUDE.md` |
| RP9-8 | P3 | The lab-tag wrapper hides `TargetOnly` | the next driver-wrapper change |
| RP9-9 | P2 | `/health` reads `healthy` for about 30 s after a slot start with dead backends (`RegisterBackend` seeds healthy; two failures needed) | **Stage 1a Prompt 10 Part 0.3** |
| — | note | A hedged DELETE on a versioned backend bucket would leave two delete markers; no backend bucket of ours is versioned | none |
| — | [YOU] | `deploy/monitoring/vaultaire-drivers.yml` is installed by hand | the owner's box install |
| — | note | A held 16 MiB part costs its 15 s deadline, more than the prompt's "≤ 12 s"; it is still the client's first attempt | none |

## Gates

- `go test -race ./...` on a private DB from each commit's tree (`fulltest-A/B/C`): every package `ok`, 40 of 40.
- golangci-lint v2.14.0: 0. `make gosec`: 0.
- Structure tests green, among them `TestDataPathBounds_NothingWaitsMoreThanThirtySeconds` and `TestDataPathBounds_EveryClientAndTimeoutIsListed` (new), `TestDriverWritePaths_WrapTheirErrorsWithW`, `TestEngineCalls_CarryTheTenant`, `TestChunkBlobCalls_GoThroughTheChunkStore`, the ledger tests, `TestSQLLiteralsMatchSchema`.
- `build-and-test` passed on every PR; CI and Deploy on every merge commit.

## Post-merge review (2026-10-17)

The meta session read #745–#748 against the code and prod (`1e87be4`, 95 min after the deploy).

**Every claim holds.**

- 0 Error lines.
- No call was cut by the new per-class bounds (no "awaiting response headers").
- `vaultaire_driver_hedges_won_total{driver="wasabi",op="head_bucket"}`: 6 of 7 hedges won live.
- `/health` is `degraded` only at two failures in a row.
- `vaultaire_lyve_bucket_lock_enabled{region="us-east-1"} 0`.
- `requestClass` puts every op where the table says: an object-lock GET is Meta, UploadPart is Put, a copy is Complete, and DeleteObjects is unused by the drivers.

**The one gap (P2), fixed by #749.**

- `dnsDialCache.entry` ran the refresh under `context.WithoutCancel`, and every dial waited in `singleflight.Do`: a 100 ms dial waited a 3 s hung lookup.
- So the hedge "at 2 s on a fresh connection", and the probe's 15 s budget, were bounded by the resolver's timeouts (about 10 s on the box), not by their callers.
- The fix is stale-while-revalidate: a dial past the TTL starts the refresh and takes the stale answer, and only a host with no answer waits, within its own context. `dnsLookupTimeout` (5 s) bounds the lookup.
- Stage 1a Prompt 10 Part 0.2 proved it on a lab with the real hedged HEAD of the fixed-bucket driver at the default `DRIVER_HEDGE_AFTER` (2 s). The resolver answered once and then hung 10 s, and the server held the first HEAD:

  | Tree | Hedged HEAD answered in (two TTL rounds) | `refreshes_total{outcome="error"}` |
  |---|---|---|
  | `1e87be4` | **10.001 s, 10.002 s**: the hedge waited the resolver out | +1 per TTL |
  | `2903d84` (#749) | **2.003 s, 2.003 s** | +1 per TTL |

  Prod read on `2903d84`: see `docs/reviews/WP-P10.md` § Prod proof.

**Handed to Stage 1a Prompt 10 Part 0:**

- RP9-9 (item 3);
- one budget for the dial's address loop (item 4): `net.Dialer.Timeout` is per address, so the worst case is n × 10 s;
- Geyser's 15 s metadata bound, unmeasured on tape (item 5).

RP9-1 stays open with its home in WP-R7-4.

### Part C on prod: the 2026-10-18 `dedup_gc` run (scheduled 02:30 UTC)

Read only, on `83f3d89` (slot 8000).

- **When it ran:** the run started at 03:05:20 UTC and took 0.22 s. That is not a delay of the job. The slot booted at about 00:05, and the scheduler runs a daily job at the first hourly check after its scheduled time (WP-R13-3). The 2026-10-17 run started at 03:20 for the same reason.
- **`job_runs`:**
  - outcome `ok`, `last_error` = `1049 chunk(s) not swept (idrive: 1049 — the backend could not be asked) (see the log); they stay candidates`;
  - `result` = `{"failed": 1049, "deleted": 0, "not_swept": {"idrive": 1049}, "reconciled": 0, "bytes_reclaimed": 0}`.
- **Journal:**
  - one Warn `dedup gc: chunks not swept — their backend could not be asked; they stay candidates`, with `backend=idrive`, `not_swept=1049`, `how=asked`, `class=backend_unavailable`, and the first error (`exists _global/_chunks/…`);
  - one Info `dedup gc run`;
  - **0** `"level":"error"` lines from 02:20 UTC to the read at 03:07. Before #747, the same run wrote one Error line per chunk.
- **Gauge:** `vaultaire_dedup_gc_unswept_chunks{backend="idrive"}` = **1049**, read from the box's Prometheus.
- **1049, not 793:** more chunk rows on the dead iDrive account have passed the 7-day grace since the 10-17 run. Every candidate is still counted, and none is lost: they stay rows, and they stay candidates until the account answers.
