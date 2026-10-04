# Vaultaire-path benchmark and test program — 2026-10-04

Everything here went **through Vaultaire**: its drivers from the SLC box to the
real backends, its S3 API on the production origin (`s3.stored.ge`, the direct
host) and through Cloudflare (`stored.ge`, what a customer gets), the load gate
(`tests/load`), the S3 conformance drive (`cmd/tools/validate`), the four tiers,
and the erasure layouts (`cmd/tools/erasure-bench`) with Reed-Solomon **and**
RaptorQ over the same legs. The 2026-10-03 codec bench
(`ERASURE-2026-10-03-codecs.md`) measured codecs alone; this one is the product.

State under test: prod build `a60564393cec` (PR #569 — Wasabi as the interim
Standard primary, `STORAGE_MODE=wasabi`, iDrive registered for its rows), the
`chunking` flag off, Cloudflare in front of `stored.ge` on the plan it had on
2026-10-04 (100 MB request body limit). Box: `slc-vaultaire-01`, 32 cores,
idle. Tenant: the load-test tenant (tier `bench`, 1.1 TB quota), a 30-day
VLT_ key minted for the run. Raw logs: `~/vaultaire-bench/results-20261004/`
on the box.

## 1. Findings that change something

| # | Finding | Severity | Status |
|---|---------|----------|--------|
| F1 | **Every aws-sdk-go-v2 client gets 403 `SignatureDoesNotMatch` through `stored.ge`** — rclone 1.71, the load gate, bench-compare, a 40-line SDK probe. The SDK signs `accept-encoding: identity`; Cloudflare rewrites the header on the way to the origin. Same key, same request through `s3.stored.ge`: 200. aws-cli (does not sign the header) passes both ways. | **P1** (rclone is the documented client) | fixed, PR #571: the verifier also tries the canonical request with `accept-encoding: identity` when the header is signed. Re-measured after deploy in §6 |
| F2 | **Cloudflare's 100 MB body limit**: a single 105 MB `PutObject` through `stored.ge` uploads for 70 s and then gets **413**; the same PUT to the origin succeeds in 17.5 s; multipart (8 MB parts) through Cloudflare succeeds. | P1 (docs + plan) | docs must say "parts ≤ 100 MB or use `s3.stored.ge`"; a Business plan ($200/mo) raises it to 200 MB, Enterprise to 500 MB+ |
| F3 | **Load-gate `ConcurrentGet` fails on the origin with Wasabi as primary**: p99 1.303 s against the 500 ms gate (100 readers × 1 MB; p50 107 ms). August 2026 on iDrive: p99 180 ms. The other four gates pass. | P2 | Wasabi first-byte + a tail; see F4. Gate stays red until the primary changes or the gate is re-based |
| F4 | **Wasabi us-west-1 stalls**: single PUTs that normally take ~1.1 s occasionally take 30–62 s (1 of 30 sequential 16 MiB PUTs; a 60.3 s raw 16 MB PUT, a 61.9 s 128 MiB shard PUT, a 15 s 4 KB PUT via the origin, burst p99 5.2 s). 30 PUTs to **us-central-1** and 30 to Lyve: none above 2.1 s. | P2 | 100-PUT probe per region in §7; candidates: `WASABI_REGION=us-central-1` or a per-PUT timeout + retry in the driver |
| F5 | **Whole-object PUT through Vaultaire is 4× slower than raw** for a single 64 MB stream (14.6 MB/s vs 64.6 MB/s to Wasabi; 47.7 MB/s with HTTP/1.1 forced); multipart is not (166–214 MB/s vs 155–298 raw). Range GET of 1 MB chunks: p50 267 ms vs 70 ms raw. | P2 | the single-stream PUT path (ETag stream + materialize) and the range path deserve a profile |
| F6 | **iDrive is down account-wide**: the us-west-1 regional key pair answers **503 Service Unavailable** on every call (the prod key answers 403). | [YOU] 0 | account repair, not code |
| F7 | Vaultaire's own metadata paths are where it beats every backend: HEAD 3,973 ops/s (object_head_cache) vs 37 raw; `ListObjectsV2` 2 ms vs 156; multipart abort 11 ms vs 439. | — | as designed |
| F8 | The Cloudflare API token on disk is invalid (rotated as asked), so Sippy and Cache Reserve could not be exercised. | [YOU] | a new scoped token |

## 2. Raw backends vs the Vaultaire origin (bench-compare, SLC, same box, same hour)

Single values are one run; latency rows are p50 / p99.

| Workload | Lyve us-east-1 raw | Wasabi us-west-1 raw | **Vaultaire origin (→ Wasabi)** |
|---|---|---|---|
| cold dial + PUT 1 KB | 287 / 405 ms | 153 / 1201 ms | 109 / 206 ms |
| warm PUT 4 KB | 84 / 437 ms, 9 ops/s | 36 / 123 ms, 23 ops/s | 50 / 788 ms, 15 ops/s |
| warm GET 4 KB | 57 / 250 ms | 28 / 359 ms | 39 / 108 ms |
| warm HEAD 4 KB | 55 / 78 ms, 18 ops/s | 25 / 39 ms, 37 ops/s | **3,973 ops/s** (head cache) |
| list 100 | 293 ms | 156 ms | **2 ms** |
| PUT 1 MB | 5.9 MB/s | 9.4 MB/s | 8.6 MB/s |
| PUT 16 MB | 67 MB/s | 1.3 MB/s (one 60.3 s stall) | 38.7 MB/s |
| GET 16 MB | 35 MB/s | 25 MB/s | 52.6 MB/s |
| PUT 64 MB single stream | 73.7 MB/s | 64.6 MB/s | **14.6 MB/s** (F5) |
| GET 64 MB | 73.4 MB/s | 69.9 MB/s | 62.9 MB/s |
| multipart 256 MB, 4 parts | 121 MB/s | 155 MB/s | 166 MB/s |
| multipart 256 MB, 16 parts | 166 MB/s | 298 MB/s | 214 MB/s |
| concurrent ingest 20 s | 660 MB/s, 165 ops/s | 807 MB/s, 202 ops/s | 643 MB/s, 161 ops/s |
| concurrent download 20 s | 468 MB/s, p99 950 ms | 502 MB/s, p99 783 ms | 472 MB/s, p99 532 ms |
| range GET 1 MB chunks | 11.8 MB/s, p50 65 ms | 13.2 MB/s, p50 70 ms | 3.6 MB/s, p50 267 ms (F5) |
| burst 500 small files | 148 ops/s, p99 279 ms | 32 ops/s, p99 5.2 s | 197 ops/s, p99 99 ms |
| sustained upload 60 s | **508 MB/s** steady | 133 MB/s, p99 7.7 s, 56–178 MB/s swings | 150 MB/s, p99 583 ms |
| integrity / consistency (6 checks) | all ✓ | all ✓ | all ✓ |
| rate-limit escalation to 128 workers | 0 errors | 0 errors | 0 errors |

Reading: through Vaultaire, parallel work costs almost nothing (ingest 643 vs
807, download 472 vs 502, multipart equal) and metadata is two orders faster.
Single-stream whole-object PUT and small range GETs carry a real overhead
(F5). Lyve sustains 4× Wasabi's upload rate from this box and has no tail.

The Cloudflare column is empty in this run: F1 refused the bucket creation.

## 3. Load gate (`tests/load`, origin, Wasabi primary)

| Test | Result | Throughput | p50 | p99 | Gate | August 2026 (iDrive) |
|---|---|---|---|---|---|---|
| ConcurrentPut (100 × 1 MB) | 0 5xx | 238 MB/s, 227 ops/s | 357 ms | 440 ms | p99 < 2 s ✓ | 168 MB/s, p99 622 ms |
| ConcurrentGet (100 readers) | 0 5xx | 47 MB/s, 45 ops/s | 107 ms | **1.303 s** | p99 < 500 ms **✗** | 36 MB/s, p99 180 ms |
| Multipart (50 × 100 MB) | 0 5xx | 676 MB/s | 7.3 s | 7.8 s | 0 5xx ✓ | 729 MB/s |
| MixedReadWrite (100, 70/30) | 0 5xx | 133 MB/s | 67 ms | 515 ms | p99 < 2 s ✓ | 72 MB/s, p99 302 ms |
| ManagementBurst (50 rapid) | 40 × 429, 0 5xx | — | 2 ms | 2 ms | 429s appear ✓ | same |

Through Cloudflare the gate could not run at all (F1); it re-runs after the
fix deploys (§6).

## 4. S3 conformance (`cmd/tools/validate`, origin): 22 / 22 PASS

CreateBucket, PUT 1 KB / 1 MB, GET + SHA-256, HEAD, ListObjectsV2, DELETE →
404, multipart 100 MB (35.0 s), range requests (the first took **15.5 s** —
one of the F4 stalls), presigned PUT/GET, Content-Type, Content-Disposition,
`x-amz-meta-*`, tagging, versioning lifecycle, overwrite/ETag, 10-way
concurrent PUT, **rclone compatibility (via the origin)**, dedup, cleanup.

## 5. The tiers, through the public endpoint

One bucket per tier (`PUT /api/v1/manage/buckets/{name}/tier`), 8 MB object
each, routing read back from `object_head_cache`:

| Tier | Backend recorded | Floor | Class reported | PUT 8 MB | Notes |
|---|---|---|---|---|---|
| performance | wasabi | standard | STANDARD | 1.6 s | the primary |
| standard | wasabi | standard | STANDARD | 7.8 s | same backend; one F4 tail |
| resilient | lyve | standard | STANDARD | 2.4 s | the Lyve tier |
| archive | geyser | vault | GLACIER | 2.1 s | GET succeeded immediately: Geyser's disk landing zone serves until the tape migration; `RestoreObject` answers `InvalidObjectState` "directly readable right now". The restore / 503 + Retry-After path is testable only once the object is on tape (hours) |
| smart (demotion) | — | — | — | — | not exercised: the job demotes objects idle 14 days and older than 3 days; fresh bench data never qualifies without changing prod knobs. The read-time promotion path is the archive path above |

## 6. Cloudflare: what it brings, measured

| Path | Measured |
|---|---|
| Public object via `/cdn/<slug>/<bucket>/key` through Cloudflare | first GET `cf-cache-status: MISS` TTFB 333 ms; then **HIT** TTFB 98–118 ms (origin direct: 288 ms). `cache-control: public, max-age=14400, stale-while-revalidate=600`. Egress on a HIT: $0 to us |
| Private presigned GET through Cloudflare | `cf-cache-status: BYPASS` every time (`cache-control: private, no-cache`), TTFB 215–430 ms vs origin — Cloudflare adds a hop and caches nothing; Cache Reserve for Standard reads (plan 40.1) is a design item, not a reality |
| 105 MB single PUT | **413 Content Too Large** after a 70 s upload (F2); multipart in 31.5 s; GET 105 MB 3.5 s via Cloudflare vs 3.7 s origin (from a Mac; link-bound) |
| Go SDK / rclone | 403 (F1) until PR #571 deploys — re-run below |
| Sippy, Cache Reserve, Argo, Workers reassembly | **not run**: the API token is invalid (F8). The 2026-10-03 Cloudflare session proved Sippy-from-Vaultaire locally |

After the F1 fix deploys: *(stage 3 pending — load gate through Cloudflare and
bench-compare `vaultaire-prod-cf` vs `vaultaire-prod-origin`, appended below
when done)*

## 7. Erasure layouts through Vaultaire's drivers: Reed-Solomon vs RaptorQ

`erasure-bench` now takes `-codec rs|raptorq`. RaptorQ (RFC 6330,
xssnick/raptorq, Go 1.25) deals symbols round-robin to the n shards so any k
shards hold K+4 symbols; same slots, legs and read shapes as RS. Legs are
Vaultaire's drivers: `wasabi` (fixed-bucket driver, us-west-1 bench bucket),
`lyve`, `r2`, `b2`, `permafrost`. The first pass ran with the R2 and B2 bench
buckets missing (404 on every shard, repaired mid-run), so those shapes were
effectively Wasabi + Lyve; stage 2b repeats them with all legs.

64 MiB payloads (first pass, wasabi:2 + lyve:2 carrying the reads):

| Shape | Codec | Encode | PUT all (wall) | READ first-k | of which decode | Degraded read (lose one leg) |
|---|---|---|---|---|---|---|
| 4+2 | RS | 3 ms | 0.9–1.5 s | 0.3–0.6 s (104–206 MB/s) | 30 ms | 0.31 s |
| 4+2 | RaptorQ T=32K | 117 ms | 3.1–8.4 s (incl. failed-leg retries) | 0.5–0.6 s (109–133 MB/s) | 127–158 ms | 0.48–0.53 s |
| 6+4 (+permafrost) | RS | 6 ms | 2.7–4.3 s (permafrost slowest 2.7–4.3 s) | 1.1 s (59 MB/s) | 40 ms | 0.9–1.0 s |
| 6+4 (+permafrost) | RaptorQ | 126 ms | 2.9–3.3 s | 0.48 s (132 MB/s) | 131 ms | 0.48–0.51 s |

512 MiB payloads, 4+2 over wasabi:2, lyve:2, r2:2 (R2 live for the RaptorQ run):

| Codec | Encode | PUT all | READ first-4 | decode | lose lyve | lose r2 | lose wasabi |
|---|---|---|---|---|---|---|---|
| RS | 26–36 ms | 5.2 s / **61.9 s** (one F4 stall) | 3.9–5.0 s (103–131 MB/s) | 0.30 s | — (R2 leg absent) | 2.7–11.0 s | — |
| RaptorQ | 1.0–1.1 s | 11.3–17.5 s (R2 slowest 11.3–11.5 s) | 3.0–6.0 s (86–168 MB/s) | 1.0–1.3 s | 4.9–5.5 s | 2.7–5.5 s | 3.6 s |

Reading: the codec is never the bottleneck for RS (decode 0.3 s of a 4 s
read). RaptorQ decode is 1.0–1.3 s per 512 MiB — a quarter to a third of the
read — and its encode is 30–40× RS's; its only advantage, ratelessness, buys
nothing in a fixed k-of-n layout. The read wall is the slowest of the k
winning legs; "winners" were always Lyve + Wasabi, R2 lost every race from
SLC. The first-k race IS the straggler experiment: fetching all n and keeping
the first k turned a 10.7 s R2 fetch into a non-event in every RaptorQ run.

*(stage 2b — all legs live, sync-gate and hedged PUT shapes — appended when
done)*

## 8. Costs, with the measured facts folded in

Rates: iDrive $4.125/TB-mo (annual Y2+), Wasabi $7.99 list (90-day minimum
per object; the partner account is $0), Lyve $7.99 list ($0 under the promo
to ~mid-2028), Geyser $1.55 (+$155/mo floor already paid), R2 $15/TB-mo + $0
egress, B2 $6.95 + free Cloudflare egress, permafrost ~$0 (sunk licences).
Cloudflare: Free plan today (100 MB bodies), Pro $20/mo (100 MB), Business
$200/mo (200 MB), Enterprise (500 MB+); Cache Reserve $0.015/GB-mo +
$4.50/M writes + $0.36/M reads; Workers Paid $5/mo; Argo $5/mo + $0.10/GB.
COGS baseline from the 2026-09-22 outlook (`.private`, §2).

| Product | Price /quota-TB | COGS per stored TB today ($0 Wasabi/Lyve) | COGS if the hot copy were paid Wasabi | COGS on iDrive (plan) | Margin at 100 % fill: today / paid Wasabi / iDrive |
|---|---|---|---|---|---|
| Vault (Geyser) | 2.55 / 2.00 yr | 1.55 | 1.55 | 1.55 | 39 % / 39 % / 39 % |
| Smart (15 % hot + 85 % Geyser) | 4.99 / 4.49 yr | 1.32 | 2.52 | 1.94 | **74 % / 49 % / 61 %** |
| Performance (all hot) | 6.99 | 0.00 | 7.99 | 4.125 | **100 % / −14 % / 41 %** |
| Resilient (Lyve) | 7.99 | 0.00 | — | — | 100 % until the promo ends, then 0 % |
| Public buckets (R2 origin + Cloudflare cache) | — | 15.00 per stored TB, $0 per view | | | the §6 HIT path is the whole business case: pay per upload, never per view |

What the measurements add: (1) on a **paid** Wasabi the Performance tier is a
loss and Smart drops twelve points — the interim is only free; (2) Lyve is
equally free, has no tail (F4) and sustains 4× the upload rate from SLC
(§2) — on the numbers it is the better interim primary; (3) Cloudflare's
cache only pays for public objects (BYPASS on private), so Cache Reserve
would be an added cost until the plan's 40.1(b) makes Standard reads
cacheable; (4) Cloudflare Business ($200/mo) buys only the 200 MB body limit
— multipart already works, so the right spend is documentation, not the plan.

## 9. What is still open

- Stage 2b / stage 3 appended when their runs end.
- Smart demotion end-to-end on a canary tenant (needs knob changes or 14 days).
- Archive restore timing once Geyser has moved the object to tape.
- Sippy / Cache Reserve / Workers edge reassembly: a new Cloudflare token.
- F5 profile of the single-stream PUT and range-GET paths.
- F3/F4 decision: Wasabi region or driver timeout + retry, or Lyve as the interim primary.
