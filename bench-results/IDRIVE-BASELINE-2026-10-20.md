# iDrive e2 baseline — the original reseller account, every use (2026-10-20)

What this is: the first measurement of iDrive e2 as it is actually used by prod after the
account came back — the **original reseller account** (b3c3c9f2, partner@fairforge.io),
paid again on 2026-10-20 (invoice 105170: Yearly 1 TB, term to 2027-10-20), its managed
user isaacv17@gmail.com with one read-write key per active region
(`.private/idrive-keys-2026-10-20.env`), and prod on `STORAGE_MODE=idrive` since
21:58 UTC the same evening. Every number below is from the SLC box between 22:58 and
23:38 UTC. It is the baseline the next iDrive measurement is compared with.

Comparable earlier programs: `TIERS-BACKENDS-CLOUDFLARE-2026-10-14.md` (§2.2 the Standard
tier on a lab instance with a different iDrive account as primary, §3 the raw backends —
quoted below where the same cell exists) and `TIERS-AND-LAYERS-2026-10-12.md` (prod on
Lyve). The tools are the same: `cmd/tools/tierbench` and `ingestbench` for the customer
path (every GET sha256-verified), `cmd/tools/bench-compare` for the raw endpoints (the
tuned transport, 200 idle connections, 4 MB buffers). Raw lines: `raw-2026-10-20/`.

**Prod state while measuring.** Build `374e5826` (deployed 18:27 UTC). Primary **iDrive
us-central-1 (Dallas)**. Registered: idrive + 12 region drivers (13 regions, Phoenix and
Paris new to prod), lyve, wasabi (dormant), r2, geyser, permafrost (3 accounts), sync
(5 bridges), local. `/health` 20/20 before, during and after. Flags: `chunking` default
ON, `parallel_get` ON for the bench tenant for the run (row removed after), `vault_parity`
global ON. The bench tenant: tier `bench`, 1 TB quota, nothing stored before or after.

**Budget.** The account holds a 1 TB block with 3 TB of free egress a month, so the program
was sized to stay well inside it: the vendor's own counters at 23:40 UTC said **40.1 GB
uploaded and 4.3 GB downloaded today, 196 MB held** before the cleanup (the download
counter lags ours — our reads were ~35 GB — recheck tomorrow). After the cleanup every
bench bucket is gone (13 raw, 4 through prod); what the account still holds is the 41-byte
canary plus **13 GB / 3,100 blobs the overwrite cell displaced**, queued in `retired_blobs`
as `overwritten` and due for the collector at 23:38 UTC on 2026-10-21 — the product's 24 h
retirement (WP-R2-1 Part C) working as designed, not a leak.

---

## 1. Findings that change something

1. **Dallas as the primary is right.** Through prod, Standard on iDrive beats Standard on
   Lyve (10-12/10-14) on every line: small ops 30–35 ms vs 57–105 ms, 256 MiB multipart
   215 in / 146 out vs 100 / 116, 1 GiB multipart 253 / 175 vs 140 / 133, 32-worker
   ingest **958 MB/s vs 753**. It also matches or beats the 10-14 lab instance on the
   other account (36 / 30 ms, 123 / 182 MB/s, 875 MB/s), so the account swap changed nothing
   in the data plane.
2. **A whole-object single-connection read from a far region is still the lottery, and
   Paris is the worst.** Raw from SLC: Paris 64 MB GET **1.1 MB/s (61 s)**, 1 MiB ranges
   0.7 MB/s; Frankfurt 3.1 MB/s, Milan 3.6 MB/s, London 12 MB/s — while every EU
   multipart write runs 58–102 MB/s and Tokyo reads 32 MB/s. Through prod a 256 MiB
   multipart object in a Paris-pinned bucket read back at **3.1 MB/s (86 s)** with
   `parallel_get` on. Residency buckets remain correct and slow from SLC; the EU hub
   (plan 41.x) stands, and Paris should not be offered as "fast" anywhere.
3. **Through Cloudflare the gap to the origin is small today**: 4 KiB ops +12 ms, a 64 MB
   read 141 MB/s (the origin 117), 32-worker ingest 578 MB/s (origin 958). On 10-14 the
   same cells on Lyve were 396 ms, 19 MB/s and 256 MB/s — Cloudflare's own variance
   (the 10-12 finding about Smart Tiered Cache) rather than ours; one more sample, not a
   trend.
4. **Oregon (us-west-4) is the fastest raw endpoint from SLC** again: 754 MB/s ingest,
   348 MB/s 256 MiB multipart, 105 MB/s 64 MB GET, 27–30 ms small ops — but it also
   produced the run's one stall (1 MiB PUTs at p50 4.2 s for 20 s). LA repeated its 10-14
   oddity: 30 MB/s concurrent ingest with p95 12.6 s while everything else on it is fast.
   Virginia is 50 ms from SLC; Miami 60 ms. Phoenix (new) is a 34–38 ms region with
   136 MB/s multipart writes and 7–11 MB/s single-stream reads.
5. **Consistency is perfect everywhere**: 20/20 read-after-write, 10/10 overwrite, 10/10
   list-after-write on all 13 regions; every sha256 verified; 0 retries. Dallas took 128
   concurrent small writers at 1,426 ops/s with 0 errors and a 60 s sustained upload at
   **478 MB/s** (28 GB — the cell was skipped for the other regions, see §6).
6. **Prod under the load**: 0 error lines, 0 client-body errors, 20/20 healthy, no breaker
   moved. The ingest cells ran against the live primary with the bench tenant alone.
7. **Open question for the next prompt:** after the run every `vaultaire_large_get_streams_total`
   and `_fallbacks_total` series on the active slot read 0 — including `path="chunked"`,
   though the chunked 256 MiB reads certainly happened — so whether the parallel reader
   took the Paris and Frankfurt multipart reads cannot be told from the counters. Either
   the counters are not wired to these paths or the flag row was not seen; a lab check
   with the flag on and the counter watched is the first step before blaming the backend
   alone for the 3 MB/s.
8. **Tool gotchas, fixed in the run log (§6) and worth a small PR**: `bench-compare`
   treats iDrive as a fixed-bucket endpoint and prints "✓ bucket" without a HeadBucket, so
   a bucket that does not exist gives a run of NoSuchBucket errors with no obvious cause;
   `ingestbench` caps a run at 1 GiB by default (`-max-mb`), which ends a 32-worker run in
   1.9 s; the raw list ends with a 60 s sustained upload that writes 28 GB per endpoint.

---

## 2. The customer path through production

`tierbench`, the bench tenant, HTTP/1.1 (what aws-cli and rclone use). `std-origin` =
`s3.stored.ge` direct (HAProxy, TLS); `std-cf` = `stored.ge` behind Cloudflare (bodies
over 100 MB skipped: Cloudflare Free refuses them); the three region columns are buckets
created with a `LocationConstraint` and served through SLC by their `idrive-<region>`
driver, via the origin. The last two columns are the earlier measurements for the same
cells: prod on **Lyve** via the origin (2026-10-14 §2.1) and the 10-14 **lab** on iDrive
Dallas with the other account (§2.2). Small ops: p50 / p95.

| Workload | Dallas, origin | Dallas, Cloudflare | Frankfurt (eu-central-1) | Phoenix (us-southwest-1) | Paris (eu-west-4) | Lyve origin 10-14 | iDrive lab 10-14 |
|---|---|---|---|---|---|---|---|
| small_put_4k | **35 / 36 ms** | 47 / 76 ms | 154 / 241 ms | 43 / 45 ms | 153 / 295 ms | 105 / 245 ms | 36 / 39 ms |
| small_get_4k | **30 / 31 ms** | 38 / 41 ms | 138 / 147 ms | 37 / 52 ms | 137 / 226 ms | 57 / 70 ms | 30 / 31 ms |
| small_head_4k (head cache) | 1 ms | 9 / 13 ms | 1 ms | < 1 ms | < 1 ms | < 1 ms | — |
| list 30 keys | < 5 ms | 10 ms | < 5 ms | < 5 ms | < 5 ms | 2 ms | — |
| small_delete | 31 / 34 ms | 40 / 47 ms | 140 / 233 ms | 38 / 39 ms | 138 / 183 ms | 63 / 75 ms | 31 / 33 ms |
| single_put_8mb | 26 MB/s | 51 MB/s | 4.8 MB/s | 21 MB/s | 4.5 MB/s | 16 MB/s | 26 MB/s |
| get_single_8mb | 30 MB/s, ttfb 101 ms | 30 MB/s, ttfb 70 ms | 5.7 MB/s, ttfb 413 ms | 3.7 MB/s, ttfb 124 ms | 1.0 MB/s, ttfb 413 ms | 16 MB/s, ttfb 165 ms | 2.2 MB/s† |
| single_put_64mb | 80 MB/s | 79 MB/s | 25 MB/s | 56 MB/s | 21 MB/s | 43 MB/s | 96 MB/s |
| get_single_64mb | **117 MB/s**, ttfb 496 ms | **141 MB/s**, ttfb 194 ms | 24 MB/s, ttfb 1.8 s | 36 MB/s, ttfb 1.2 s | **2.4 MB/s**, ttfb 16.5 s | 52 MB/s, ttfb 1.25 s | 43 MB/s, ttfb 1.13 s |
| single_put_256mb (chunked) | 188 MB/s | n/a (> 100 MB) | 74 MB/s | — | — | 450 MB/s‡ | 160 MB/s |
| get_single_256mb (chunked) | 112 MB/s, ttfb 614 ms | — | 16 MB/s, ttfb 1.6 s | — | — | 114 MB/s, ttfb 510 ms | 127 MB/s, ttfb 245 ms |
| mpu_put_256mb (16 × 16 MiB, 8 wide) | **215 MB/s** | **216 MB/s** | 67 MB/s | 128 MB/s | 62 MB/s | 100 MB/s | 123 MB/s |
| get_mpu_256mb | **146 MB/s**, ttfb 451 ms | 108 MB/s, ttfb 406 ms | 33 MB/s, ttfb 4.7 s | 54 MB/s, ttfb 1.4 s | **3.1 MB/s** (86 s), ttfb 4.7 s | 116 MB/s, ttfb 392 ms | 182 MB/s, ttfb 315 ms |
| mpu_put_1024mb (64 parts) | **253 MB/s** (complete 2.2 s) | — | — | — | — | 140 MB/s | 127 MB/s |
| get_mpu_1024mb | **175 MB/s**, ttfb 517 ms | — | — | — | — | 133 MB/s, ttfb 516 ms | 144 MB/s |
| range_get_1mb (p50 / p95) | 118 / 388 ms | 168 / 501 ms | 379 / 390 ms | 129 / 145 ms | **2.3 / 3.0 s** | 240 / 382 ms | 195 / 247 ms |

† one slow single-connection read on the lab that day. ‡ the chunked PUT on Lyve was
measured with a different chunk concurrency.

Reading it: the Dallas column is the product's Standard tier today. Small ops are the
backend's own RTT plus ~5 ms (raw Dallas 27–30 ms, §4), multipart in both directions runs
at 150–250 MB/s on one client with 8 parts in flight, and the chunked single PUT at 188
MB/s. The region columns are the backend's RTT and per-connection speed seen from SLC: a
European customer reading a European bucket goes SLC → Europe → SLC → Europe, which is
why they exist for residency and not for speed until there is a hub there.

## 3. Ingest, many writers

`ingestbench`, 20 s each, the bench tenant, one bucket, objects deleted after. The 10-14
columns are the same cells on prod-on-Lyve via the origin and on the lab instance.

| Cell | Today, Dallas primary | Lyve origin 10-14 | iDrive lab 10-14 |
|---|---|---|---|
| 32 workers × 4 MiB, origin | **958 MB/s**, 229 ops/s, p50 121 ms, p99 472 ms, max 1.7 s | 753 MB/s, 180 ops/s, p50 123 ms | 875 MB/s, 209 ops/s, p50 133 ms |
| 8 workers × 64 MiB, origin | **724 MB/s**, 10.8 ops/s, p50 650 ms, p99 1.5 s | — | 778 MB/s, p50 655 ms |
| 32 workers × 4 MiB, Cloudflare | **578 MB/s**, 138 ops/s, p50 217 ms, p99 473 ms | 256 MB/s, 61 ops/s, p50 433 ms | — |
| 16 workers × 4 MiB cycling 8 keys (the overwrite path, WP-R10-8) | **674 MB/s**, 161 ops/s, p50 93 ms, p99 202 ms | — | — |

0 failures in 10,347 PUTs. The overwrite path (displaced blobs retired, the quota released
in the commit) runs at the same speed as fresh writes with a lower p50, as designed.

## 4. The backends, raw, from the box

`bench-compare`, each region in turn, one process, path-style, the tuned transport. p50
for small ops; MB/s for transfers; "concurrent" = 16 parallel 4 MB objects, capped by the
tool at 1 GiB per direction (1.5–3 s, not 20 s — the 10-14 numbers were capped the same
way); the Dallas row alone ran the full list. Columns: 4 KiB PUT / GET / HEAD; 16 MB
PUT / GET (one connection); 64 MB PUT / GET; 256 MB multipart (16 parts); concurrent
ingest / download; 1 MiB range reads p50 (and MB/s); ListObjects 100 keys.

| Region | 4 KiB PUT / GET / HEAD | 16 MB PUT / GET | 64 MB PUT / GET | 256 MB mpu | 16×4 MB in / out | 1 MiB range | list 100 |
|---|---|---|---|---|---|---|---|
| **us-central-1 Dallas (primary)** | **30 / 27 / 28 ms** | 79 / 70 MB/s | 88 / 75 MB/s | 275 MB/s | **690 / 176 MB/s** | 32 ms (15.8 MB/s) | 110 ms |
| us-west-2 Los Angeles | 28 / 24 / 24 ms | 88 / 17 MB/s | 99 / 57 MB/s | 82 MB/s | **30**‡ / 410 MB/s | 51 ms (15.7) | 95 ms |
| us-west-4 Oregon | 30 / 27 / 27 ms | 22‡ / 104 MB/s | 107 / 105 MB/s | **348 MB/s** | **754 / 374 MB/s** | 29 ms (25.1) | 112 ms |
| us-east-1 Virginia | 54 / 49 / 49 ms | 53 / 26 MB/s | 55 / 31 MB/s | 183 MB/s | 411 / 258 MB/s | 398 ms (2.5) | 192 ms |
| us-southwest-1 Phoenix (new) | 38 / 34 / 34 ms | 49 / 6.9 MB/s | 49 / 11 MB/s | 136 MB/s | — | 62 ms (12.9) | 134 ms |
| us-midwest-1 Chicago | 39 / 34 / 32 ms | 33 / 7.4 MB/s | 39 / 71 MB/s | 65 MB/s | — | 287 ms (2.9) | 170 ms |
| us-southeast-1 Miami | 63 / 59 / 57 ms | 34 / 29 MB/s | 55 / 16 MB/s | 152 MB/s | — | 227 ms (4.2) | 253 ms |
| eu-west-1 Ireland | 129 / 120 / 119 ms | 25 / 34 MB/s | 28 / 27 MB/s | 102 MB/s | — | 125 ms (5.6) | 593 ms |
| eu-west-3 London | 140 / 130 / 129 ms | 24 / 6.5 MB/s | 26 / 12 MB/s | 97 MB/s | — | 386 ms (2.7) | 516 ms |
| eu-west-4 Paris (new) | 157 / 145 / 145 ms | 17 / 7.4 MB/s | 17 / **1.1 MB/s** (61 s) | 58 MB/s | — | **1.15 s** (0.7) | 577 ms |
| eu-central-1 Frankfurt | 144 / 134 / 132 ms | 20 / 29 MB/s | 19 / **3.1 MB/s** (20 s) | 76 MB/s | — | 395 ms (2.4) | 531 ms |
| eu-south-1 Milan | 152 / 139 / 138 ms | 20 / 28 MB/s | 21 / **3.6 MB/s** (18 s) | 99 MB/s | — | 141 ms (4.4) | 692 ms |
| ap-northeast-1 Tokyo | 120 / 111 / 110 ms | 28 / 30 MB/s | 29 / 32 MB/s | 111 MB/s | — | 113 ms (6.4) | 548 ms |

‡ one-sample stalls: LA's concurrent ingest p50 1.4 s / p95 12.6 s (the same cell was 36
MB/s on 10-14 — LA has done this twice); Oregon's 1 MiB PUTs p50 4.2 s for one 20 s cell
while its 16 MB and 64 MB cells ran 100+ MB/s.

Dallas, the cells the other regions skipped: cold dial + 1 KiB PUT 85 ms p50; CopyObject
32 ms; DeleteObjects 100 keys 119 ms (836 keys/s); 500 small files burst 312 ops/s;
mpu_abort 55 ms; rate-limit escalation 8 → 128 writers 186 → **1,426 ops/s, 0 errors at
every step**; 60 s sustained upload (30 × 64 MB in flight) **478 MB/s** steady (448 → 496
across the minute); integrity 16 MB plain / robust / chunked all verified, 0 retries.

Against the 10-14 raw pass on the other account, Dallas is the same endpoint (30 / 27 /
26 ms, 254 MB/s multipart, 835 MB/s in then; 275 and 690 now — the concurrent cell is one
sample of 1.5 s). The region ordering is unchanged too: Oregon ≥ Dallas ≥ LA for a
client in SLC; Virginia and Miami 50–60 ms; Europe 120–160 ms; Tokyo 110 ms.

## 5. What a bench costs on this plan

The vendor's counters (reseller API `usage_stats`, 23:40 UTC): 40.1 GB uploaded, 4.3 GB
downloaded, 196 MB on disk before the cleanup, 6 deleted objects / 0.5 MB in the deleted
bucket. Ours: ~110 GB written (the 28 GB sustained cell, ~45 GB of ingest, the raw and
customer cells), ~35 GB read. Either the vendor's day is cut at a different hour or the
counters lag by hours; the WP-CAP-1 collector asserts the units on its first run. On a
1 TB block with 3 TB of free egress a month this program is affordable once; it is not a
nightly job. Left on the account after the cleanup: the canary and the 13 GB of displaced
blobs the `retired_blobs` job deletes on 2026-10-21 after 23:38 UTC (deletes are deferred
24 h by design, so a bench's bytes count against the block for a day).

## 6. Run log

- 22:58–23:04 UTC customer path (`tierbench` × 5, `ingestbench` × 3). The three ingest
  cells ended after 1.9 s: `-max-mb` defaults to 1 GiB. Rerun at the end with 16 GiB.
- 23:04 raw, first attempt: `bench-compare` printed "✓ bucket: vaultaire-bench" and every
  call answered NoSuchBucket — the tool's iDrive rows are `FixedBucket` entries (it never
  creates, only assumes). The name was taken by the 10-14 account in the same region, so
  "already exists" looked like success. A second attempt with `IDRIVE_BENCH_BUCKET` set to
  a fresh name failed the same way for the opposite reason: no bucket. Fix: create the
  bench bucket in all 13 regions by hand (aws-cli from the box), then run.
- 23:08–23:10 raw Dallas, every workload (2 m 04 s). The list ends with `sustained_upload_60s`
  (28 GB); the run was stopped once LA started it and the other twelve regions ran with
  `-skip-workload burst,integrity_robust,rate_limit,sustained` (the nine far regions also
  without the concurrent cells). Lesson: `pkill -f` with the script's name in the ssh
  command line kills the ssh shell itself — kill by pid.
- 23:11–23:36 raw: LA, Oregon, Virginia (3 m 36 s); the nine others (21 m 38 s).
- 23:36–23:38 ingest reruns (4 cells).
- Cleanup: 13 raw buckets emptied (Virginia held 369 objects from the cut run) and
  deleted; `idb-std`, `idb-eu`, `idb-phx`, `idb-par` deleted through stored.ge; the
  bench tenant's `parallel_get` row removed; the key file removed from the box. Prod
  `/health` 20/20, 0 error lines in the journal for the hour.

Files: `raw-2026-10-20/tier-*.log`, `tier.jsonl` (one line per workload), `ingest*.log`,
`ingest.jsonl`, `raw-dallas.{log,json}`, `raw-us.{log,json}`, `raw-rest.{log,json}`,
`run*.log`.
