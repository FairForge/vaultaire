# Lyve Cloud, every region, raw from the box — 2026-10-21

**When:** 2026-10-21 01:35–01:57 UTC. **From:** `slc-vaultaire-01` (the prod box; 10 Gbps,
Salt Lake City). **What:** `bench-compare` (commit `5828310`, built 2026-10-20) against each
of the account's seven Lyve Cloud 2 regions through its own regional endpoint
`s3.<region>.global.lyve.seagate.com`, path-style, the tuned transport, the bench root key.
us-west-1 (the region prod's `lyve` driver homes in) ran every workload including the
60 s sustained upload; the other six ran everything but that cell. Raw logs and JSON:
`raw-2026-10-21-lyve/`. Companion: `IDRIVE-BASELINE-2026-10-20.md` (the same tool, the
same box, 13 iDrive regions, two hours earlier).

**Pre-flight, before the matrix reached any region:** a signed ListObjects, then a PUT, a
GET with `x-rstor-replication-status: true`, and a DELETE of one probe object through each
region's own endpoint. All seven answered 200, `X-Rstor-Sse-Encryption-Status: true`, and the
replication header named the bucket's own region (US-WEST-1 … AP-NORTHEAST-1) — so every row
below measures the region, not Lyve's cross-region proxy path (the 2026-07-28 trap). The
bench buckets are the pre-provisioned `vbench-lyve-<region>` set.

## 1. Findings

1. **Every region works, with zero errors on every cell.** Seven regions × ~30 workloads,
   no error, 0 retries; consistency 20/20 read-after-write, 10/10 overwrite, 10/10
   list-after-write everywhere; 128 concurrent writers clean on all seven (`max_clean=128`).
2. **us-west-1 is the Lyve region to keep.** 74 / 46 / 45 ms small ops, 232 MB/s multipart
   (16 parts), 803 / 447 MB/s with 32 writers / readers, **605 MB/s sustained for 60 s**
   (586–616 across the minute), 1,044 ops/s at 128 writers. Against iDrive Dallas the same
   night: iDrive wins every small-op line (30 / 27 / 28 ms) and the writer ladder (1,426
   ops/s); Lyve wins single-stream 64 MB (126 / 94 vs 88 / 75 MB/s), concurrent (803 / 447
   vs 690 / 176) and sustained (605 vs 478 MB/s). us-central-2 is within noise of
   us-west-1 (58 / 47 / 47 ms, 807 / 505 MB/s concurrent) and a fair second US home.
3. **Lyve's EU reads are healthy where iDrive's are not.** eu-west-1 and eu-central-1 read
   a 64 MB object single-stream at 21 and 19 MB/s and 16 MB at 26 / 24 MB/s; iDrive Paris,
   Frankfurt and Milan read the same at 1.1–3.6 MB/s two hours earlier. Small ops are the
   same class (143–154 ms vs 129–157 ms). For an EU residency bucket today, Lyve eu-west-1 is
   the better backend by an order of magnitude on reads.
4. **The two APAC regions enter through one door.** `s3.ap-southeast-1` and
   `s3.ap-northeast-1` both resolve to 134.204.251.1 (112 ms TCP from SLC), and every number
   is identical to the millisecond (337 / 222 / 221 ms, 23 / 32 MB/s, 1.0 s list). The
   buckets ARE homed in different regions (the replication header says so), so the data
   sits in Singapore or Tokyo, but a client in the US pays the same ingress plus an internal
   hop either way. Tokyo is no closer than Singapore from here; iDrive Tokyo answers in
   120 ms direct.
5. **us-west-1 has improved since 10-14**: 4 KiB PUT 165 → 74 ms, concurrent in 754 → 803
   MB/s, 64 MB single read 84 → 94 MB/s; us-east-1's 10-14 one-sample 65 s read of 64 MB
   did not recur (54 MB/s). The 2026-09-27 TCP-connect numbers still hold (us-west-1
   20 ms, AP 112 ms).
6. **Cold dial is expensive on Lyve**: 271–291 ms for a fresh TLS + 1 KiB PUT in the US,
   624–872 ms in Europe, 1.2 s in APAC — three to four times iDrive Dallas's 85 ms. The
   drivers pool connections, so this is a cost of the first request only.
7. **Prod unaffected:** 20 / 20 backends healthy and no error line in the window (the four
   log hits in the window were unauthenticated `robots.txt` / `favicon.ico` requests at
   Info level).

## 2. The seven regions, raw

p50 for small ops; MB/s for transfers; "32×4 MB" = 32 parallel 4 MB objects, capped by the
tool at 1 GiB per direction (1.3–4.5 s, not 20 s); multipart = 256 MB in 16 × 16 MB parts,
4 then 16 in flight; range = 16 × 1 MiB reads of a 16 MB object.

| Region | 4 KiB PUT / GET / HEAD | 16 MB PUT / GET | 64 MB PUT / GET | 256 MB mpu 4p / 16p | 32×4 MB in / out | 1 MiB range | list 100 |
|---|---|---|---|---|---|---|---|
| **us-west-1 (prod's Lyve home)** | 74 / 46 / 45 ms | 71 / 59 MB/s | **126 / 94 MB/s** | 163 / 232 MB/s | **803 / 447 MB/s** | 52 ms (18.2) | 157 ms |
| us-east-1 | 83 / 62 / 59 ms | 68 / 54 MB/s | 74 / 54 MB/s | 119 / 162 MB/s | 713 / 412 MB/s | 68 ms (11.4) | 309 ms |
| us-central-2 | **58 / 47 / 47 ms** | 90 / 83 MB/s | 109 / 91 MB/s | 154 / 206 MB/s | **807 / 505 MB/s** | 53 ms (15.4) | 206 ms |
| eu-west-1 | 143 / 141 / 140 ms | 31 / 26 MB/s | 32 / 21 MB/s | 58 / 75 MB/s | 352 / 211 MB/s | 147 ms (5.0) | 633 ms |
| eu-central-1 | 154 / 149 / 147 ms | 29 / 24 MB/s | 29 / 19 MB/s | 54 / 73 MB/s | 328 / 151 MB/s | 159 ms (4.4) | 690 ms |
| ap-southeast-1 | 337 / 222 / 221 ms | 23 / 32 MB/s | 31 / 40 MB/s | 34 / 56 MB/s | 230 / 312 MB/s | 228 ms (3.6) | 1.01 s |
| ap-northeast-1 | 337 / 222 / 221 ms | 23 / 32 MB/s | 31 / 41 MB/s | 41 / 62 MB/s | 228 / 222 MB/s | 227 ms (3.6) | 1.02 s |

The other cells, per region (cold dial = fresh client, TLS + 1 KiB PUT; copy = server-side
CopyObject; batch = DeleteObjects 100 keys; burst = 500 small files; ladder = 8 → 128
writers, ops/s at each step, every step 0 errors; HTTP/1.1 variants of the 64 MB PUT and
the 16-part multipart):

| Region | cold dial | copy | delete 1 / batch 100 | burst 500 | mpu abort | ladder 8 / 32 / 128 w | 64 MB h1 / mpu h1 | integrity 16 MB plain / robust / chunked |
|---|---|---|---|---|---|---|---|---|
| us-west-1 | 291 ms | 55 ms | 48 ms / 192 keys/s | 48 ms p50 | 382 ms | 100 / 218 / **1,044** ops/s | 136 / 233 MB/s | ✓ / ✓ 23 MB/s / ✓ 25 MB/s |
| us-east-1 | 286 ms | 70 ms | 62 ms / 216 keys/s | 62 ms | 426 ms | 58 / 102 / 245 | 75 / 163 | ✓ / ✓ 11 / ✓ 12 |
| us-central-2 | 271 ms | 69 ms | 66 ms / 123 keys/s | 57 ms | 415 ms | 91 / 273 / 793 | 116 / 221 | ✓ / ✓ 17 / ✓ 18 |
| eu-west-1 | 872 ms | 146 ms | 143 ms / 466 keys/s | 139 ms | 795 ms | 37 / 182 / 368 | 32 / 77 | ✓ / ✓ 4 / ✓ 4 |
| eu-central-1 | 624 ms | 153 ms | 152 ms / 413 keys/s | 152 ms | 846 ms | 36 / 163 / 338 | 30 / 73 | ✓ / ✓ 4 / ✓ 5 |
| ap-southeast-1 | 1.24 s | 442 ms | 437 ms / 12 keys/s | 335 ms | 1.72 s | 20 / 48 / 187 | 31 / 56 | ✓ / ✓ 4 / ✓ 4 |
| ap-northeast-1 | 1.15 s | 441 ms | 438 ms / 12 keys/s | 336 ms | 1.71 s | 20 / 48 / 169 | 31 / 60 | ✓ / ✓ 4 / ✓ 4 |

us-west-1 alone, the 60 s sustained upload (30 × 64 MB in flight): **605 MB/s**, p50 per
64 MB PUT 107 ms, 586 → 616 → 606 → 611 → 600 → 613 MB/s at 10 s marks.

## 3. Against the other runs

| Cell | Lyve us-west-1 today | iDrive Dallas 10-20 | iDrive Oregon 10-20 | Lyve us-west-1 10-14 |
|---|---|---|---|---|
| 4 KiB PUT / GET / HEAD | 74 / 46 / 45 ms | **30 / 27 / 28 ms** | 30 / 27 / 27 ms | 165 / 45 / 42 ms |
| 64 MB PUT / GET, one stream | **126 / 94 MB/s** | 88 / 75 MB/s | 107 / 105 MB/s | 137 / 84 MB/s |
| 256 MB multipart, 16 parts | 232 MB/s | 275 MB/s | **348 MB/s** | — |
| 32 × 4 MB in / out | **803 / 447 MB/s** | 690 / 176 MB/s | 754 / 374 MB/s | 754 / 617 MB/s |
| 128 writers | 1,044 ops/s | **1,426 ops/s** | — | — |
| 60 s sustained upload | **605 MB/s** | 478 MB/s | — | — |
| 1 MiB range p50 | 52 ms | **32 ms** | 29 ms | 49 ms |
| cold dial + 1 KiB PUT | 291 ms | **85 ms** | — | — |

| EU cell | Lyve eu-west-1 | Lyve eu-central-1 | iDrive Ireland 10-20 | iDrive Frankfurt 10-20 | iDrive Paris 10-20 |
|---|---|---|---|---|---|
| 4 KiB PUT / GET / HEAD | 143 / 141 / 140 ms | 154 / 149 / 147 ms | 129 / 120 / 119 ms | 144 / 134 / 132 ms | 157 / 145 / 145 ms |
| 64 MB PUT / GET | 32 / **21 MB/s** | 29 / **19 MB/s** | 28 / 27 MB/s | 19 / **3.1 MB/s** | 17 / **1.1 MB/s** |
| 256 MB multipart | 75 MB/s | 73 MB/s | 102 MB/s | 76 MB/s | 58 MB/s |
| list 100 | 633 ms | 690 ms | 593 ms | 531 ms | 577 ms |

## 4. Cost

Lyve is $0 under the promo with no egress or request fees, so this run cost nothing. Bytes
moved: us-west-1 41.7 GB (36 GB of it the sustained cell), each other region 3.7 GB; 63.7 GB
in all. Nothing was left behind: the tool's objects were deleted as it went, and the 15
`bench/sustained/*` objects (16 MB each) that the cut 10-14 run had left in six of the
buckets were deleted afterwards — every `vbench-lyve-<region>` bucket lists 0 objects.
The buckets stay (they are the homed bench set; re-creating one costs 3–8 s and a homing
check).

## 5. Run log

- 01:30–01:36 pre-flight from the Mac: ListObjects, PUT/GET/DELETE of a probe object per region,
  replication header checked; the box's bench key verified byte-identical to the local one.
- 01:35:05–01:37:15 us-west-1, every workload (130 s).
- 01:37:15–01:57:05 us-east-1 (89 s), us-central-2 (67 s), eu-west-1 (182 s),
  eu-central-1 (189 s), ap-southeast-1 (335 s), ap-northeast-1 (329 s), all but
  `sustained_upload_60s`.
- 01:58 prod journal checked (no error lines), `/health` 20 / 20, bench buckets emptied.

Tool notes: `bench-compare` reports `concurrent_*_20s` as a 1 GiB-capped burst (1.3–4.5 s),
so the "20 s" in the name is not what ran; the cut sustained cell of 10-14 left its in-flight
objects behind (cleaned today) — the cell should delete what it wrote on cancel.
