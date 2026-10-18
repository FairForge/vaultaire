# WP-P10: Stage 1a Prompt 10

2026-10-17 → 18. Two parts:

- **Part 0**: the WP-P9 post-merge probe.
- **Part A**: the region gate. A region that cannot take a write is never offered.

PRs:

- **755**: the WP-P9 note itself (`docs/reviews/WP-P9.md`), docs only. The Prompt 9 worker never opened it.
- **#751**: Part 0, items 0.3 (RP9-9) and 0.4 (the dial loop's budget).
- **#753**: Part A, the region gate.
- **#754**: Part A's follow-up, found by its own prod proof: a region that has not yet answered a probe is not offered.
- **this PR**: this note, the SYNTHESIS row and the STATUS sentence.

No migration; the next number stays **093**.

## Decisions

1. **WP-P9 landed as its own docs PR,** not inside the Part 0 code PR. Its last proof was the 02:30 UTC `dedup_gc` run, and a code PR waiting four hours for it would have held Part 0's deploy, which RP9-9's proof needs.
2. **RP9-9: "no success yet" is `LastSuccess` zero**, a new field on `BackendHealthState`. A backend that has never answered a probe gets no benefit of the doubt. Once it has answered, the two-in-a-row rule applies unchanged.
   - Two existing tests whose subject is something else (the canary's 403, the probe's own bound) now give their backend a prior success. That is the 2026-10-11 shape: a working key that died.
3. **The dial loop's budget is shared out the way `net.Dialer` shares its own.** Each attempt gets the time left divided by the addresses left, with a 2 s floor (`attemptContext`).
   - The prompt's literal "the remaining budget handed to each attempt" would let a blackholed first address spend the whole 10 s, so the good second address would never be asked. The split keeps the one total and still reaches it.
   - The red test asserts both: within the budget, and connected on the fourth address.
4. **Geyser's 15 s metadata bound stays.** The histogram had nothing to say (item 5), so the bound was measured directly on prod, read-only: answers come in under 0.7 s. A minutes-long listing was a no-answer episode, which a bound plus a retry beats.
5. **The region gate's "down" uses the probe state exactly as `/health` does**: two failures after a success, one before any. It also treats an OPEN breaker as down. Half-open is not down: like `engine.CanAsk`, it lets the next call through.
   - A region driver with no probe state at all is judged on registration alone (tests, a dev box).
   - The prompt's nil `Deps.RegionAvailable` is that same rule on the dashboard.
6. **The default region is always offered**, even when the `idrive` probe is failing. Its buckets are served by the primary through the engine, and on prod the primary is `lyve`.
7. **An idempotent re-create of an owned bucket in a down region is not refused.** It creates nothing, and the prompt keeps existing pinned buckets unchanged. Unknown and not-enabled regions are refused as before, owned or not.
8. **The metric's `region` label is never caller input.** An unknown id is counted as `region="other"`, and the Info line truncates it to 64 bytes.
9. **The management envelope got a new error type, `unavailable_error` → 503.** The type table had no 503, and `api_error` (500) would mislead a client into treating a retryable refusal as our bug. The code is `region_unavailable_now`, as the prompt names it.

## What was built

### Part 0: the WP-P9 post-merge probe

1. **The WP-P9 note.** It was assembled from the Prompt 9 worker's drafts. Its residual table (RP9-1 … RP9-9) was recovered from that session's log rather than reconstructed, and it adds the post-merge review section, the #749 lab and the 02:30 UTC `dedup_gc` proof.
2. **The proof of #749** (lab and prod; see the proofs below).
3. **RP9-9** (`health_handlers.go`): `LastSuccess`. A backend with no success yet is unhealthy at its first failed probe, so `/health`'s `reason` and `vaultaire_backend_health` follow at once.
4. **One budget for the dial's address loop** (`dns_cache.go`):
   - `dialer(dial, budget)`: the budget is the dialer's Timeout (10 s), passed by `cachedDialContext`.
   - `attemptContext` gives each address its share.
   - Both `WithTimeout` sites are listed in `data_path_bounds_test.go`.
5. **Geyser's metadata bound**: measured; no change (see Prod proof).
6. **The prod read** before Part A.

### Part A: the region gate (#753, #754)

- `internal/api/region_gate.go`: `regionCreateState` decides `offered | unknown | not_enabled | down`. `createBucketRegistry` consults it for S3 CreateBucket, the management API and the dashboard creator.
- S3: 503 `ServiceUnavailable` + `Retry-After: 60`, with the message "Region <id> is temporarily unavailable on this deployment: its storage is not answering…".
- Management: 503 `unavailable_error` / `region_unavailable_now`, `param: region`, `Retry-After: 60`.
- Dashboard:
  - `Deps.RegionAvailable` = `regionAvailableNow`.
  - The picker shows a registered region the gate refuses as `disabled … (temporarily unavailable)`; one with no driver stays `(not enabled)`.
  - The form's refusal is `BucketCreateRegionDown`: "Region <name> is temporarily unavailable…".
- **#754**: "healthy by the probe" means a probe said so.
  - The prod proof of #753 rendered the picker about a minute after the slot switch. It offered us-west-2 and us-west-4, both dead, because their staggered first probes had not run yet and `RegisterBackend` seeds `Healthy`.
  - The gate now asks `backendProvenUp`: healthy AND `LastSuccess` set.
  - A working region is therefore unavailable until its first probe answers (at most one probe interval after boot). A create in that window gets a retryable 503, never a dead bucket.
- `vaultaire_region_create_refused_total{region,reason}` and one Info line per refusal (`bucket create refused: region`: tenant, region, reason). No rule: the region driver's own `BackendProbeFailing` already pages.
- Docs: `internal/api`, `internal/dashboard/handlers` and `internal/drivers` CLAUDE.md, `docs/API.md` (regions and the error table), the OpenAPI 503, and `deploy/monitoring/README.md`.

## Before → after

"Before" is `origin/main` `2903d84` at the start (#749 deployed 22:13 UTC 2026-10-17).

| # | Item | On `2903d84` | Now | Red test |
|---|---|---|---|---|
| 0.1 | The WP-P9 note | never opened: the SYNTHESIS row flips sat uncommitted on `docs/wp-p9` | landed (#755) with the post-merge review, the #749 lab and the `dedup_gc` proof | — (docs) |
| 0.2 | #749 | a hedged HEAD waited a hung refresh: **10.0 s** on `1e87be4` (lab) | **2.003 s** while the refresh hangs; one `error` refresh per TTL; prod: one `ok` refresh per host per 5 min for the hosts that dial, 0 errors, 0 `address(es) tried` lines, Wasabi hedges 9 of 12 won, every Wasabi probe ≤ 2.5 s | — (lab + prod read) |
| 0.3 | RP9-9: `/health` after a slot switch | `healthy` for ~30 s with the dead iDrive drivers; `backend_health` 1 until their second probe | unhealthy at the first failed probe of a backend that never answered one; prod: `degraded` naming `idrive` at slot uptime **0.6 s**, each region at its first staggered probe, all eleven by 28 s, never `healthy` | `TestHealth_ABackendWithNoSuccessYetIsUnhealthyAtItsFirstFailedProbe` |
| 0.4 | The dial's address loop | `net.Dialer.Timeout` 10 s per address: 4 lost SYNs = 40 s | one 10 s budget across the addresses, a share each (≥ 2 s), the caller's context if shorter | `TestDNSCache_TheAddressLoopHasOneBudget` (1.21 s vs 550 ms), `…EveryAddressLostIsOneBudgetNotOnePerAddress` |
| 0.5 | Geyser's 15 s metadata bound | unmeasured on tape | measured: HEAD 0.46–0.70 s, listing page headers 0.25–0.61 s; one no-answer episode (77–174 s pages); bound kept | — (prod measurement) |
| 0.6 | Prod read before Part A | — | 0 Error lines since 19:57; 18/18 jobs `ok`; client-body errors 6 (`PutObject/gone`); hedges 1/1 won (wasabi) since the 22:14 restart | — |
| A.1 | Availability | registered = offered | registered AND healthy by the probe AND breaker not open (default region always) | `TestRegionGate_ARegionWhoseBreakerIsOpenIsDown`, `…OneFailedProbeAfterASuccessIsStillOffered`, `…TheDefaultRegionIsAlwaysOffered` |
| A.2 | The refusal | a dead region's CreateBucket 200: a bucket nothing can be stored in | S3 503 + `Retry-After: 60`; management 503 `region_unavailable_now`; picker "(temporarily unavailable)"; unknown / not enabled still 400 | `TestRegionGate_S3CreateBucketInARegionDownByTheProbeIs503`, `…TheBucketRegionHeaderIsGatedToo`, `…ManagementCreateInARegionDownByTheProbeIs503`, `…UnknownAndNotEnabledStay400`, `…AnOwnedBucketInADownRegionIsUnchanged`, `TestHandleBuckets_ARegionDownByTheProbeIsDisabledAsTemporarilyUnavailable`, `TestHandleCreateBucket_ARegionDownIsRefusedWithTheSameWords` |
| A.3 | Observability | none | `vaultaire_region_create_refused_total{region,reason}`, one Info line | `TestRegionGate_ARefusalIsOneInfoLine` (+ the counter in the A.2 tests) |
| A.4 | A region not yet probed | offered (seeded healthy): the #753 prod proof saw us-west-2 and us-west-4 offered before their first probe | down until its first successful probe; prod at uptime 0.5 s: only `us-central-1` offered | `TestRegionGate_ARegionNotYetProbedIsNotOffered` (#754) |
| A.5 | Prod proof | — | S3 503 + `Retry-After: 60` (eu-west-1, ap-northeast-1), 400 for eu-west-4, the default-region control created and deleted; management and dashboard the same words; counters and Info lines per refusal | — |

## Local live proof

### Part 0.2: #749 on a lab (Mac)

The lab is a throwaway test in `internal/drivers`, never committed: the real `NewFixedBucketS3Driver` hedged `Exists` at the default `DRIVER_HEDGE_AFTER` (2 s), dialling through the shared `dnsDialCache`.

- The resolver answers the name once (127.0.0.1), then hangs 10 s on every lookup (the box's resolver timeout).
- `DRIVER_DNS_TTL` is 30 s on a fake clock.
- The server holds the first HEAD of each key until the client gives up. The pooled connection carries the first attempt; the hedge dials fresh.

| Tree | Round 1 | Round 2 | `refreshes_total{outcome="error"}` |
|---|---|---|---|
| `1e87be4` (before #749) | **10.001 s** | **10.002 s** | +1, +1 |
| `2903d84` (#749) | **2.003 s** | **2.003 s** | +1, +1 |

Before #749 the hedge's fresh dial waited the hung refresh out. With #749 it takes the stale answer at once while the refresh runs beside it, and the refresh is counted once per TTL either way.

### Part A

Covered by the handler tests (S3, management, dashboard with the real `buckets.html`). Prod is the live proof: a lab region driver would need a real region key pair.

## Prod proof

### Part 0.6: the prod read before anything of Prompt 10 deployed (`2903d84`, 22:16–22:35 UTC)

- `"level":"error"` since 2026-10-17 19:57 UTC: **0**.
- `job_runs`: 18 of 18 `ok`.
  - `dedup_gc` still reads `793 chunk(s) not swept (see the log)` from 03:20 UTC 2026-10-17, before #747.
  - `routing_truth` reads `idrive: 10 of 501 rows sampled … 10 error`: the dead key, expected.
- `vaultaire_client_body_errors_total`: 6 in 24 h, all `PutObject/gone` (clients that went away; 400s, no breaker charge).
- `vaultaire_dedup_gc_unswept_chunks`: no series yet. The collector reads `job_runs.result`, which no post-#747 run has written.
  - The first post-#747 run (2026-10-18, 03:05 UTC, the first hourly check after 02:30) wrote it: `not_swept {idrive: 1049}`, one Warn, 0 Error lines, gauge `{backend="idrive"}` 1049. The full proof is in `docs/reviews/WP-P9.md` § Part C on prod.
- `vaultaire_driver_hedges_total{wasabi,head_bucket}` 1, won 1, since the 22:14 restart. All other drivers 0.
- `vaultaire_driver_op_seconds` p99 per driver over 3 h, probes only (prod had no other engine traffic): wasabi 2.26 s (the hedged probe), sync 1.63 s, idrive-eu-west-3 1.67 s, idrive-us-west-2 1.26 s, permafrost 0.67 s, every other driver ≤ 0.25 s. No non-probe op had a sample.
- Enabled iDrive regions (boot line): ten region drivers, `ap-northeast-1 eu-central-1 eu-south-1 eu-west-1 eu-west-3 us-east-1 us-midwest-1 us-southeast-1 us-west-2 us-west-4`; `eu-west-4` and `us-southwest-1` have no pair. With the primary `idrive`, that is the "eleven dead iDrive drivers" of `/health`.

### Part 0.5: Geyser's metadata latency, measured (read-only, 22:22–22:34 UTC)

`vaultaire_driver_op_seconds{driver="geyser",op=~"head|delete|list"}` had **zero samples** in Prometheus since #746 (2.8 h of history, the histogram's whole life). The Geyser-touching jobs call the driver directly, not through the engine where the histogram observes, and none listed Geyser in that window. So there was nothing to read. The bound was measured instead, with aws-cli 2.35 from the box against `la1.geyserdata.com` on the prod bucket, read-only, keys inside a 0600 script, only timings printed:

| Call | Samples | Time to answer |
|---|---|---|
| HEAD of a sampled object (GLACIER or ONEZONE_IA, on tape) | 25 | **0.46–0.70 s** |
| HEAD of a missing key | 1 | 0.46 s (404) |
| a 1,000-key listing page, headers (`--debug`, first attempt) | 9 (3 walks × 3 pages, 22:33) | **0.25–0.61 s** |
| a 1,000-key listing page, total (first walk, 22:22–22:29, aws-cli defaults) | 3 | **162 s, 77 s, 174 s** |

The first walk's 77–174 s are consistent with aws-cli's 60 s read timeout firing and its retries succeeding (60 + 17, 60 + 60 + 42, 60 + 60 + 54), that is, with Geyser not answering some requests at all for several minutes. Its `--debug` was off, so this is inferred, not observed. Ten minutes later every page answered on its first attempt in under 0.65 s.

A 15 s bound is 20× every answered call. Against the no-answer episode, cutting at 15 s and retrying (2 SDK attempts on a request, 3 on a job) is what got aws-cli its answer, at 60 s per attempt instead of 15. **Meta stays 15 s.** Two things the measurement does not answer go to the residuals: a hedge for Geyser's metadata calls, and the histogram not seeing the jobs' direct driver calls.

### Part 0.2 and 0.3, after #751 deployed

#751 merged as `2d46142` and deployed 23:02 UTC. Prod has carried it since; later deploys: `e604bcc` (#750, Prompt 11) at 23:06, `d013b37` (#753) at 23:35, `83f3d89` (#754) at 00:01.

**RP9-9.** `/health` was polled every second on the new slot's own port (8001) from the moment it answered `/version`:

| UTC | Slot uptime | `/health` | `reason` |
|---|---|---|---|
| 23:02:17 | 0.6 s | `degraded`, 17 / 18 | probe failing on idrive |
| 23:02:20 | 3.7 s | `degraded`, 16 / 18 | … idrive-ap-northeast-1 |
| 23:02:31 | 14.8 s | `degraded`, 12 / 18 | … idrive-eu-west-3 |
| 23:02:45 | 28.0 s | `degraded`, 7 / 18 | all eleven iDrive names |
| 23:03:15 | 58.5 s | `degraded`, 7 / 18 | all eleven (unchanged) |

Each region was named at its first staggered probe; there was no `healthy` second. Before #751 the slot read `healthy` for ~30 s (the Prompt 9 note). The seven healthy backends are local, wasabi, geyser, r2, permafrost, sync and lyve.

**#749 over the next 85 minutes** (Prometheus, across the restarts, 22:17–23:42 UTC):

- `vaultaire_driver_dns_refreshes_total`: every outcome `ok`, 0 `error`. `la1.geyserdata.com` 16 (one per 5 min: it dials every probe); `s3.us-west-1.wasabisys.com` 6; four region hosts 2 each; the pooled hosts 0 (their connections stay up).
- `dial … address(es) tried`: 0 lines. `"level":"error"`: 0 lines.
- Hedges: `wasabi/head_bucket` 12 sent, 9 won; `wasabi/head_object` 1/1 won; `idrive-us-west-4/head_bucket` 1/1. `vaultaire_driver_op_seconds{driver="wasabi",op="probe"}`: the maximum is in the ≤ 2.5 s bucket, so every hedged probe answered within it.

### Part A, after #753 deployed

#753 merged as `d013b37`, deployed 23:35 UTC. The proof used the bench tenant through `https://stored.ge` (Cloudflare + HAProxy, the customer path), aws-cli 2.35 with `AWS_MAX_ATTEMPTS=1` so the 503 is seen once, not retried. Keys were read inside a 0600 script, never printed.

| Step | Result |
|---|---|
| `aws s3 mb s3://p10-gate-euw1-… --region eu-west-1` | `make_bucket failed … (ServiceUnavailable) … Region eu-west-1 is temporarily unavailable on this deployment: its storage is not answering. Retry later, or create the bucket in another region.` in 0.55 s |
| the raw answer (`--debug`) | `PUT /p10-gate-euw1b-… HTTP/1.1 503`, `retry-after: 60` |
| `create-bucket … LocationConstraint=ap-northeast-1` | 503 `ServiceUnavailable`, the same message |
| `create-bucket … LocationConstraint=eu-west-4` (no key pair) | 400 `InvalidLocationConstraint … Region eu-west-4 is not enabled on this deployment.` |
| control: `aws s3 mb` with no region | `make_bucket` ok, `get-bucket-location` = `us-central-1`; `aws s3 rb` ok |
| `aws s3 ls` | no `p10-gate` bucket: nothing refused was created, the control is gone |
| `vaultaire_region_create_refused_total` | `{down,eu-west-1}` 3 (two S3, one dashboard), `{down,ap-northeast-1}` 1, `{not_enabled,eu-west-4}` 1 |
| Info lines | one per refusal: `bucket create refused: region tenant-14a6… eu-west-1 down` (×2), `… ap-northeast-1 down`, `… eu-west-4 not_enabled` |
| dashboard picker (the load-test account) | eight regions `disabled … (temporarily unavailable)`, `us-southwest-1` and `eu-west-4` `(not enabled)`, `us-central-1` selected; **`us-west-2` and `us-west-4` offered** (see below) |
| dashboard form, `region=eu-west-1` | "Region EU West (Ireland) is temporarily unavailable: its storage is not answering. Try again in a minute, or pick another region." |

**The picker's two offered regions were dead.** `/health/backends` showed both unhealthy, `object canary HEAD … AccessDenied`. Their first probes ran at 23:36:23 and 23:36:26, just after the page was rendered (≈ 23:36:20); until then their state was the seeded `Healthy`. The gate trusted the seed, so → **#754**.

### #754, after it deployed (`83f3d89`, 00:01 UTC)

The picker was rendered through `https://stored.ge` the second the new build served:

| UTC | Slot uptime | Offered | "temporarily unavailable" | "not enabled" |
|---|---|---|---|---|
| 00:02:21 | **0.5 s** | `us-central-1` only | 10 | 2 |
| 00:03:01 | 40.6 s | `us-central-1` only | 10 | 2 |

`"level":"error"` since 22:13 UTC across the four deploys: 0.

## Residuals

| # | Sev | Item | Where it goes |
|---|---|---|---|
| RP9-1 | P3 | Permafrost's Graph calls: no per-class bound, no ctx-aware bounded retries (throttling and `/content` redirects unmeasured) | WP-R7-4 |
| RP8-11 | P3 | A chunk blob cut by its deadline after the backend stored it has no row | WP-R8-2 (the orphan sweep) |
| RP10-1 | owner | Prod's Lyve region is `us-east-1`, the region that stalled ~1–3 % of small PUTs on 2026-10-17 while `us-west-1` was clean (RP9-3) | owner question: move the Lyve buckets' home, or keep and rely on #740/#742's retry |
| RP10-2 | P3 | Geyser had a multi-minute no-answer episode on listings (inferred, 22:22–22:29 UTC). Its metadata calls are not hedged (the S3-class hedge excludes Geyser) | WP-R6-3 follow-up: hedge Geyser's HEAD and listing page, once a second episode is observed with `--debug` |
| RP10-3 | P3 | `vaultaire_driver_op_seconds` observes only engine calls; the jobs' direct driver calls (routing truth, the erasure walk, the parity reconcile, the canary) are not timed, so a backend those jobs alone touch (Geyser) has an empty histogram | the histogram at the driver layer (a transport round-tripper per class) |
| RP10-4 | closed | A region not yet probed was offered (the #753 prod proof) | **#754** |
| RP10-6 | note | A working region is refused (503, `Retry-After: 60`) until its first probe answers after a slot start: ≤ 28 s with the stagger | none: a retryable refusal, by design |
| RP10-5 | [YOU] | The owner's lever: unset the ten dead `IDRIVE_<REGION>_*` pairs, and the regions become `not_enabled` (400) instead of `down` (503) | both are fine; the gate matters when iDrive is repaired and one region lags |
| RP9-2 | [YOU] | GO-2026-5932's code-scanning thread blocks every PR's auto-merge | dismiss the alert |

## Gates

- Each code PR: `build-and-test` green on the PR and on the merge commit (`2d46142`, `d013b37`, `83f3d89`); Deploy green.
- Locally, on `vaultaire_test_p10`:
  - `go test -race` on the touched packages: `internal/api`, `internal/drivers`, `internal/dashboard/...`, `internal/docs`, all `ok`.
  - golangci-lint v2.14.0: 0.
  - gosec with the workflow's flags: 0 issues (376 files).
- Structure tests in those runs: `TestDataPathBounds_NothingWaitsMoreThanThirtySeconds`, `TestDataPathBounds_EveryClientAndTimeoutIsListed` (two new sites listed), `TestDriverWritePaths_WrapTheirErrorsWithW`, `TestEngineCalls_CarryTheTenant`, `TestChunkBlobCalls_GoThroughTheChunkStore`, `TestSQLLiteralsMatchSchema`.
- No migration.

## Owner notes

- **The region gate is what is live**, not unset pairs. `aws s3 mb --region eu-west-1` (or a `LocationConstraint`) now answers 503 with "temporarily unavailable" while iDrive's key is dead, and starts working on its own once that region's probe answers again.
- The LET readiness read's "dead regions in the picker" line is closed. The picker shows ten regions disabled "(temporarily unavailable)", two "(not enabled)", and `us-central-1` enabled. The prompt's "eleven" counts the primary `idrive` with the ten region drivers.

## Post-merge review (2026-10-18, meta session)

Read on `origin/main` `c9ce46c`, prod `83f3d89` (slot 8000). Every claim above holds.

- **The gate has no fourth door.** The three bucket-creation entry points (S3 `CreateBucket` with a `LocationConstraint` body or an `x-amz-bucket-region` header, the management `POST /buckets`, the dashboard form and picker) all pass `createBucketRegistry` → `regionCreateState`; no code path writes `buckets.region` after creation (`grep -rn "SET region\|UPDATE buckets.*region" internal/` is empty), so there is no second surface for the R4-22 class of gap.
- **The dial budget is the budget.** `dialer` wraps the loop's context in `WithTimeout(budget)` before any address is tried, and `attemptContext` takes `min(floor, left)`, so the 2 s floor can never push the loop past its 10 s total; a caller's shorter deadline still wins.
- **RP9-9 is the right rule.** `UpdateHealth`'s verdict is `ConsecutiveFailures < 2 && (ConsecutiveFailures == 0 || LastSuccess set)`: unhealthy at the first failure only while no probe has ever succeeded, two in a row after one has. The probe interval and the stagger are untouched, so a working region is offered at most 28 s after a slot start, as the note says.
- **Prod, 13:45–13:50 UTC:** 0 `"level":"error"` lines since 04:00; 18/18 jobs `ok` (`dedup_gc` 03:05 with `not_swept idrive: 1049`, `routing_truth` 06:08 with the ten `idrive` errors — both expected while the key is dead); `vaultaire_region_create_refused_total` has no series on the current slot (nothing refused since the 00:01 switch); `/health` `degraded` naming the eleven iDrive drivers. One observation, not a finding: `vaultaire_driver_put_retries_total{idrive-eu-west-3} 2`, `{idrive-us-midwest-1} 3` — the canary PUT on the dead regions sometimes gets no answer within its deadline and is retried on a fresh connection before the `AccessDenied` arrives; the retry is the #740 behaviour working, the regions stay unhealthy either way.

No P0/P1/P2 found; no fix PR. Residuals RP10-1/2/3/5 and RP9-2 stand as recorded.
