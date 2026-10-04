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
| F9 | **Uploads through Cloudflare were capped at ≈1.2 MB/s per stream.** Root cause (found in round 2, §10): **HAProxy's HTTP/2 flow-control window**, 65,535 bytes by default; Cloudflare speaks HTTP/2 to the origin, so every proxied upload ran at window ÷ RTT (65,535 B ÷ 64 ms ≈ 1 MB/s — measured 0.93 MB/s over h2 and 10 MB/s over h1.1 to the same origin from the same Mac). Fixed on the box: `tune.h2.initial-window-size 4194304`, `tune.h2.max-frame-size 1048576`, graceful reload. After: 10.4 MB/s via Cloudflare from the Mac (its uplink), 44 MB/s from the Dallas edge to the origin for 64 MiB. | was P1, **fixed 2026-10-04** | `docs/DEPLOY.md` records the lines; the config is not in the repo |
| F3 | **Load-gate `ConcurrentGet` fails on the origin with Wasabi as primary**: p99 1.303 s against the 500 ms gate (100 readers × 1 MB; p50 107 ms). August 2026 on iDrive: p99 180 ms. The other four gates pass. | P2 | Wasabi first-byte + a tail; see F4. Gate stays red until the primary changes or the gate is re-based |
| F4 | **Wasabi PUTs stall, in both regions**: 100 sequential 16 MiB PUTs from SLC — us-west-1: median 1.17 s, 4 stalls (16, 16, 30, 30 s); us-central-1: median 1.34 s, 4 stalls (9, 9, 62, **123 s**). 30 PUTs to Lyve us-east-1: median 1.15 s, max 1.31 s, no stall. The stalls also hit the product: a 15 s 4 KB PUT via the origin, a 7.8 s 8 MB tier PUT, a 61.9 s and a 32.8 s 128 MiB shard PUT, burst p99 5.2 s. | **P1** while Wasabi is the primary | ~4 % of PUTs stall 9–123 s on the account from this box, region-independent. Options: a per-PUT deadline + retry in the fixed-bucket driver (the erasure bench's hedge shape), or Lyve as the interim primary (§8) |
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

After the F1 fix deployed (build 3977d8b, 02:13 UTC): rclone 1.71 and the Go
SDK list and head through `stored.ge`. The same bench-compare run then went
through Cloudflare and to the origin back-to-back (the origin numbers in this
pass are lower than §2's: the erasure stage and the stall probe were running
on the box at the same time, which is why §2 is the origin reference):

| Workload | through Cloudflare | origin, same minutes |
|---|---|---|
| cold dial + PUT 1 KB | 448 / 568 ms | 96 / 150 ms |
| warm PUT 4 KB | 349 ms p50, 2 ops/s, p99 7.9 s | 47 ms, 11 ops/s |
| warm GET 4 KB | 123 / 203 ms | 34 ms / 7.4 s (one F4 tail) |
| warm HEAD 4 KB | 81 ms, 12 ops/s | 3,408 ops/s |
| list 100 | 171 ms | 2 ms |
| PUT 1 MB | **0.8 MB/s**, p50 1.39 s | 9.3 MB/s |
| PUT 16 MB | **1.2 MB/s**, p50 12.3 s | 54.8 MB/s |
| PUT 64 MB single | **1.3 MB/s** (47.8 s) | 67.7 MB/s |
| multipart 256 MB, 4 / 16 parts | 5.0 / 15.2 MB/s | 31.6 / 26.2 MB/s (contended) |
| GET 16 MB / 64 MB | 42.7 / 63.2 MB/s | 39.1 / 14.6 MB/s (contended) |
| concurrent ingest 20 s | 26.6 MB/s, p50 3.4 s | 80.7 MB/s |
| concurrent download 20 s | 57 MB/s, p99 2.1 s | 230 MB/s |
| sustained upload 60 s | **4.3 MB/s** | 126 MB/s |
| integrity / consistency | all ✓ | all ✓ |

Load gate through Cloudflare on the fixed build: ConcurrentPut 46 MB/s, p99
**2.26 s (gate 2 s ✗)**; ConcurrentGet 83 MB/s, p99 556 ms (gate 500 ms ✗);
Multipart 208 MB/s, p50 **21.7 s** (origin 7.3 s) ✓; MixedReadWrite 43 MB/s,
p99 2.43 s ✗; ManagementBurst 40 × 429 ✓. Finding F9: a per-stream upload cap
of about 1.2 MB/s on the Cloudflare leg, independent of HTTP version and
vantage point. Reads through Cloudflare cost a hop (+~80 ms per op) and are
otherwise fine; a public-bucket HIT is the one place Cloudflare is faster
than the origin.

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

Stage 2b, every leg live (R2 bench bucket recreated, B2 pointed at an
existing bucket). 64 MiB, 3 runs each; reads are the slowest of the k winners:

| Shape | Codec | PUT all (wall) | READ first-k | decode | lose b2 | lose lyve | lose r2 | lose wasabi |
|---|---|---|---|---|---|---|---|---|
| 4+2 wasabi:2 lyve:2 r2:1 b2:1 | RS | 1.0–1.6 s | 0.56–0.87 s (74–115 MB/s) | 30–46 ms | 0.44–0.57 s | 0.58–1.1 s | 0.40–0.60 s | 0.55–0.93 s |
| same | RaptorQ | 1.1–2.0 s | 0.51–0.90 s (72–125 MB/s) | 122–155 ms | 0.62–0.75 s | 0.61–1.1 s | 0.48–0.58 s | 0.45–1.2 s |
| 6+4 wasabi:3 lyve:3 r2:2 permafrost:2 | RS | 3.5–3.8 s (permafrost slowest) | 0.40–0.45 s (143–159 MB/s) | 34–42 ms | — | 0.97–1.1 s | 0.41–0.46 s | 1.2–1.3 s |
| same | RaptorQ | 2.9–3.6 s | 0.63–0.71 s (91–101 MB/s) | 132–162 ms | — | 1.4–1.7 s | 0.53–0.56 s | 1.1–1.5 s |
| 4+2, **sync gate = wasabi+lyve**, r2/b2 async | RS | commit **0.48–0.64 s (100–134 MB/s)**, fully protected 2.4–7.3 s | 0.51–0.60 s | 33–41 ms | 0.36–0.48 s | 0.58–1.2 s | 0.41–0.63 s | 0.66–1.6 s |
| 4+2, hedged PUT (2× leg median, min 3 s) | RS | 1.0–2.0 s, 0 hedges fired | 0.46–0.58 s | 30–38 ms | 0.31–0.60 s | 0.57–1.1 s | 0.41–0.56 s | 0.49–1.3 s |

512 MiB, 4+2 over wasabi:2 lyve:2 r2:2, 2 runs each:

| Codec | PUT all | READ first-4 | decode | lose lyve | lose r2 | lose wasabi |
|---|---|---|---|---|---|---|
| RS | 9.7 s / **32.8 s** (a Wasabi stall) | 2.2–3.9 s (131–238 MB/s) | 0.24–0.34 s | **41.0 s / 73.9 s** (R2 + Wasabi shards only: R2 delivered 128 MiB shards at 3–6 MB/s) | 2.4–18.2 s | 2.3–2.4 s |
| RaptorQ | 10.3–12.9 s (R2 slowest) | 3.9–4.3 s (120–130 MB/s) | 1.1–1.4 s | 5.5–7.0 s | 3.0–4.8 s | 2.8–3.1 s |

256 MiB, 4+2, two-vendor and one-vendor shapes (no single-vendor loss
survivable; shown for the PUT/READ baseline): wasabi:6 RS — PUT 2.0 s (129
MB/s), READ 1.2 s (207–222 MB/s); wasabi:3 lyve:3 RS — PUT 1.8–2.0 s, READ
1.7–1.9 s (132–147 MB/s); same with RaptorQ — READ 1.7–1.8 s of which decode
0.54–0.60 s; one PUT hit a 15.9 s Wasabi stall.

What stage 2b says: (1) losing any one of four vendors costs at most a
second on a 64 MiB read with RS; (2) the **sync-gate shape is the product
shape** — commit on the two fast legs in ~0.5 s and let the slow legs land
behind (R2 from SLC is the slow leg every time, B2 second); (3) RaptorQ costs
100–130 ms more per 64 MiB and ~1 s more per 512 MiB than RS at every shape,
and nothing in a fixed k-of-n layout uses its ratelessness — Phase 11 stays
RS; (4) at 512 MiB the degraded read depends entirely on which legs survive:
Lyve + Wasabi 2.3 s, R2 + Wasabi 41–74 s — the layout must never let a read
fall back to R2-only shards from this box; (5) the hedge never fired because
no leg exceeded 2× its median within 3 s — the F4 stalls were in other runs.
The Lyve 100-PUT stall probe is void (a scripting error signed it with the
Wasabi key); the 30-PUT Lyve run stands.

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

- Smart demotion end-to-end on a canary tenant (needs knob changes or 14 days).
- Archive restore timing once Geyser has moved the object to tape.
- Sippy / Cache Reserve / Workers edge reassembly: a new Cloudflare token.
- F5 profile of the single-stream PUT and range-GET paths.
- F3/F4 decision: a PUT deadline + retry in the fixed-bucket driver, or Lyve as the interim primary (both regions of Wasabi stall).


## 10. Round 2 (2026-10-04, 02:40–14:40 UTC): Sippy on prod, the edge Worker, the upload cap solved, iDrive back

New credentials arrived mid-day (a Cloudflare account token, R2 S3 keys, a B2
master key, a new iDrive reseller account). Everything below ran on the
production origin; Worker and R2 resources live in the bench Cloudflare
account (`vt-edge-ec.isaacv17.workers.dev`, buckets `vt-sippy-vaultaire`,
`vt-ecbench`).

### 10.1 R2 Sippy with the production origin as source

`PUT /r2/buckets/vt-sippy-vaultaire/sippy` with `bucketUrl =
https://s3.stored.ge/sippy-src-20261004` and a Vaultaire tenant key, 7-day
expiry rule. Reads through a presigned R2 URL from the Mac:

| Object | Cold (R2 → Vaultaire → Wasabi) | Warm (R2) | Bytes |
|---|---|---|---|
| 1 MB | TTFB **8.5 s**, 9.3 s total | 0.29 s / 0.40 s | identical |
| 16 MB | TTFB 4.3 s, 6.3 s total | 0.24–0.30 s / 0.7–0.8 s | identical |
| 64 MB | TTFB 0.62 s, 2.8 s total | 0.28–0.36 s / 1.8 s | identical |

Sippy works against Vaultaire unmodified (the earlier proof was a tunnel to a
laptop). The first cold read paid an 8 s first-contact cost that the next two
did not; the 64 MB cold read was faster than the 16 MB one. Copies appeared in
R2 after the first read. This is the public-bucket plan (40.1(a)) proven on
prod; the open item stays WP-R2-1 / 34.3 because Sippy never propagates deletes.

### 10.2 k-of-n reassembly at the edge (Workers + WASM), Reed-Solomon and RaptorQ

A Worker (`scratchpad/edge/vt-edge-ec`, Rust → wasm32 codec with a plain C
ABI: `reed-solomon-erasure` and `raptorq`, 289 KB) fetches all n shards —
Wasabi and Lyve via 7-day presigned URLs, R2 via the binding, B2 via a
presigned URL — keeps the first k whose headers arrive, cancels the rest,
decodes in WASM and streams the payload. Shards are the ones `erasure-bench
-keep` wrote from SLC (4+2 over wasabi:2, lyve:2, r2:1, b2:1). Output hashes
were checked against the concatenated systematic data shards (RS) and across
different winning subsets (RaptorQ: identical output from four different
shard sets, which rules out a wrong decode). The B2 leg was dead for the whole
round (free-tier download cap exhausted by the morning's erasure runs), so
"lose a whole vendor" leaves 3 of 4 and fails by construction; parity
reconstruction was forced with `drop=<slot>` instead.

| Request | From | Status | TTFB | Total | Winners | Notes |
|---|---|---|---|---|---|---|
| RS 64 MiB, all data shards | Mac | 200 | 0.76–1.09 s | 2.0–2.3 s | wasabi ×2 + lyve ×2 | shards into the edge in 240–374 ms; hash = reference |
| RS 64 MiB, `drop=1` (parity path) | Mac | 200 | 1.57 s | 2.9 s | lyve ×2, wasabi, **r2 parity** | hash = reference |
| RS 64 MiB | SLC | 200 | 1.07–1.96 s | 1.7–2.6 s | mixed, R2 won a race once | hash = reference |
| RS 64 MiB, `drop=1` | SLC | 200 | 0.80 s | 1.45 s | lyve ×2, wasabi, r2 | hash = reference |
| RS 64 MiB, edge cache HIT | Mac / SLC | 200 | **0.11 s / 0.14 s** | 1.4–1.6 s / 1.1 s | — | `caches.default`, `x-edge-cache: HIT` |
| RaptorQ 16 MiB | Mac / SLC | 200 | 0.84–1.29 s | 1.1–1.6 s | mixed, incl. `drop=0` | same hash from four different winner sets |
| RaptorQ 64 MiB | Mac | **500** | — | — | — | k × 16 MiB + 64 MiB output > the 128 MB isolate |
| RS 512 MiB | SLC | **500** | — | — | — | 6 × 128 MiB shards do not fit either |

What this says: (1) edge reassembly of a 64 MiB object from two vendors is a
2 s read from anywhere, 1.5 s from the box, and a 0.1 s first byte once the
edge has it; (2) the parity path costs ~0.6 s more because R2 is the slow
shard; (3) the 128 MB isolate bounds a whole-block decode at about 64 MiB for
RS and 16 MiB for RaptorQ — larger objects need **stripe-interleaved shards**
so the Worker can decode stripe by stripe (the Worker has that mode; the bench
encoder writes contiguous shards, so it was not exercised); (4) RaptorQ at the
edge works and interoperates (Go encoder, Rust decoder, both RFC 6330) but
brings nothing over RS here either; (5) `Date.now()` is frozen during CPU work
in Workers, so in-Worker decode timings are meaningless — only the
client-side TTFB minus the fetch marks tell the story.

### 10.3 The upload cap, explained and fixed

A Worker that generates 16 MiB at the edge and PUTs it to a presigned origin
URL took 19.2 s (0.87 MB/s); 64 MiB hit HAProxy's 50 s client timeout and
returned 502. Uploading the same bytes from the box to a Worker ran at 16–18
MB/s, so the client → Cloudflare leg was never the problem. HAProxy's log
showed Cloudflare arriving over **HTTP/2.0**. From the Mac, `curl --http2`
to the origin uploaded at 0.93 MB/s and `curl --http1.1` at 10 MB/s; RTT 64
ms; 65,535 ÷ 0.064 = 1.02 MB/s. That is HAProxy's default
`tune.h2.initial-window-size`. After `tune.h2.initial-window-size 4194304`
and `tune.h2.max-frame-size 1048576` with a graceful reload: Mac via
Cloudflare 10.4 MB/s (uplink-bound), Mac h2 direct 9.1 MB/s, edge → origin
16 MiB in 1.2 s and 64 MiB in 1.5 s (44 MB/s), box → Cloudflare 26–30 MB/s.
Finding F1 and F9 were both "Cloudflare" and both turned out to be ours:
one in the SigV4 verifier, one in HAProxy. Stage 4 (load gate and
bench-compare through Cloudflare after the fix) is appended in §11.

### 10.4 iDrive: a new reseller account, provisioned by API

The reseller key opened an empty account (the one holding the 2,037 old rows
is a different account; Isaac will hand it over later). Through the reseller
API: user `prod@stored.ge` (25 TB quota), all 11 active regions enabled
(`enable_user_region` takes `region`; `create_access_key` takes `storage_dn`
and `name`), one read-write key per region, bucket `vaultaire` created in
us-central-1 and a PUT/GET round trip verified. The pairs are installed on
prod (primary + 10 regional drivers); boot provisioned the regional buckets;
health is 17/17 with `idrive` and `idrive-<region>` all closed. Key file:
`.private/idrive-keys-2026-10-04.env` (gitignored). The primary stays
`wasabi` until the owner chooses; the 2,037 rows on the old account remain
unreadable until that account returns or the bench tenants are erased
(WP-R7-5).

### 10.5 Housekeeping

B2: the free-tier download cap is exhausted for the day (`AccessDenied: download
bandwidth or transaction (Class B) cap exceeded`) — raise it in the Caps &
Alerts page before the next B2 leg run. A scoped B2 application key
(`vaultaire-bench-20261004`) replaced the master key in the bench env; the
master key should not be used for S3 at all. The Cloudflare token, R2 keys, B2
master key and iDrive reseller key were pasted in chat and should be rotated
when this round is over.
