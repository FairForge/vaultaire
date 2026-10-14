# The best mix per tier — every backend, every workload, the Cloudflare product beside it (2026-10-14)

What this is: one measurement program, run from the production box on 2026-10-14, that puts
every backend Stored can use side by side under the same workloads, then measures each
tier the way a customer sees it (through `s3.stored.ge` direct and through `stored.ge`
behind Cloudflare), then measures the parity legs that sit behind the tiers, and ends with
the mix recommended per tier and per use case, with the Cloudflare product that belongs in
front of it. The numbers for the Standard tier on iDrive come from a **lab instance** of the
same build on the box (port 8100, its own database, the NEW iDrive reseller account as
primary) so production was not switched.

Earlier programs this supersedes where it repeats them: `TIERS-AND-LAYERS-2026-10-12.md`
(prod on Lyve, customer path), `SYNC-WORKLOADS-2026-10-07.md` (the Sync bridges),
`VAULTAIRE-PATH-2026-10-04.md` (the Cloudflare rounds), `ERASURE-2026-09-25-all-backends.md`.
Where today did not repeat a measurement, the earlier number is quoted and dated.

**Prod state while measuring.** Build `9fc27050` (PR #721, the rclone multi-stream fix,
deployed 05:29 UTC). Primary **Lyve** (`STORAGE_MODE=lyve` since 2026-10-11). Registered:
lyve, wasabi (dormant), r2, geyser, permafrost (3 OneDrive accounts), sync (5 bridges),
local, idrive + 10 regions on the OLD reseller account (every key object-dead since
2026-10-10, `/health` degraded 7/18). Flags: `vault_parity` global ON (leg order
sync → permafrost → lyve), `parallel_get` ON for the bench tenant only, `chunking` default ON.

**The new iDrive account.** Reseller API key supplied by the owner on 2026-10-14 (7-day key —
rotate it in the reseller console). Created through the reseller API: user `bench@stored.cloud`
(2,000 GB quota), all 11 active regions enabled, one read-write key per region. Key file:
`.private/idrive-keys-2026-10-14.env` (mode 600, gitignored). Regions on this account:
us-west-4 (Oregon), us-west-2 (Los Angeles), us-east-1 (Virginia), us-central-1 (Dallas),
us-midwest-1 (Chicago), us-southeast-1 (Miami), eu-west-1 (Ireland), eu-west-3 (London),
eu-central-1 (Frankfurt), eu-south-1 (Milan), ap-northeast-1 (Tokyo). All HDD-backed.

**Vantage points.** (1) The SLC box itself = a customer VPS in the same datacenter, zero
network distance; it measures ceilings. (2) Hostbrr, a shared cPanel account in New York =
a US customer 50 ms away. (3) Maxko, a shared cPanel account in Sisak, Croatia = an EU
customer 166 ms away. (2) and (3) run rclone 1.75 and curl, the clients customers actually use.

**Tools.** `cmd/tools/bench-compare` (raw backends, one fixed workload list), `tierbench`
(customer path, every GET sha256-verified), `ingestbench` (N writers), `webdav-bench` (the
Sync bridges through the real driver), `onedrive-bench` (the fleet through the real driver),
`erasure-bench` (Reed–Solomon layouts across legs, rebuild from parity). Raw lines in
`raw-2026-10-14/` beside this file and in `~/vaultaire-bench/raw-20261014/` on the box.

---

## 1. Findings that change something

1. **Standard on iDrive beats Standard on Lyve on every customer-path line** (§2.2 vs
   §2.1): small ops 30–36 ms against 57–105, multipart reads 182 against 116 MB/s, ingest
   875 against 753 MB/s, chunked 256 MiB 160/127 against 128/114. The new reseller account
   is the better Standard primary the day it is on annual prepay. → §7.1.
2. **Sync is the slowest parity leg and holds every parity shard.** Rebuilding a Vault
   object with Geyser gone: Lyve 150–235 MB/s, iDrive 115–124, OneDrive 66–69, Sync 12–62;
   writing the parity: 1.3 s on any S3 leg, 7–21 s on Sync (§4.3). Flip the leg order to
   Lyve now. → §7.2.
3. **Geyser serves from its landing zone at 60–76 MB/s as a data leg and 20–25 MB/s as a
   single Vault read**, and a cold object stalls a client indefinitely (the raw bench sat
   30 min on one range read; a 100-key listing takes 30 s). Tape is a floor, never a serving
   leg — but the ingest copy for Standard is usable, not just survivable, while the copy is
   recent. → §7.3.
4. **The warm Cloudflare cache is the fastest thing in the product** — 217–313 MB/s at
   60–140 ms to first byte from the box, 102 MB/s from Croatia — and 4–10× any S3 read of
   the same object. Public is the tier where Cloudflare earns its place. Two gaps: 4 KiB
   objects are never cached (`DYNAMIC`) and an overwritten public key serves stale until
   the max-age expires. → §7.5.
5. **Cloudflare in front of the S3 API costs a real client 20–60 ms per op and nothing on
   throughput** (10-12, confirmed from New York and Croatia today: 45–50 MB/s single
   stream, network-bound) — while the box's own path through the edge was pathological
   today (a 156 s 8 MB read, a 1 GiB read cut with EOF, 7.8 s range reads). The box is not
   a customer; re-run on a quiet box before calling it a Cloudflare fact. → §7.7.
6. **Residency is correct and slow**: a bucket pinned to Frankfurt answers small ops 100–130
   ms slower from SLC and read a 256 MiB multipart object at 4 MB/s on one connection
   (chunked: 36 MB/s). `parallel_get` does not cover region drivers yet, and every byte
   still transits SLC. → §7.6.
7. **Every S3 backend is a per-connection lottery**: iDrive 7.9–103 MB/s and Lyve 1–84
   MB/s on single 64 MB reads, 37–241 MB/s on 10-10. Parallel ranges (the chunked path,
   `parallel_get`, multipart) are what make throughput repeatable, on every leg.
8. **Multi-stream download gains nothing from a shared-host client**: rclone's four streams
   were equal to one from Croatia and slower from New York (16 vs 45 MB/s). The #721 fix
   was about correctness (403 → 200), not speed; the speed is the customer's uplink.
9. **Three bench credentials were dead** — the Wasabi partner pair, the B2 bench bucket, the
   old iDrive pairs — and the bench env file printed every secret it held into every log
   that sourced it. Fixed on the box (§7.8).

---

## 2. The tiers, as a customer sees them

### 2.1 Through production (primary Lyve), origin vs Cloudflare

`tierbench` from the box, 17:22–17:4x UTC, bench tenant, every GET sha256-verified ("ok").
`std` = Standard on Lyve (`tb-std`), `arch` = Vault on Geyser (`tb-arch`, tier archive),
`pub` = Public on R2 (`tb-pub`). `origin` = `https://s3.stored.ge` (HAProxy + TLS, no
Cloudflare), `cf` = `https://stored.ge` (through the Cloudflare proxy). p50 / p95 for small
ops; MB/s and TTFB for transfers. The `cf` columns were measured while the second raw pass
was also running from the box and, as on 10-12, the box's own path to the Cloudflare
colos does not represent a customer — the remote vantages in §2.3 do.

| Workload | std-origin | std-cf | arch-origin | arch-cf |
|---|---|---|---|---|
| small_put_4k | 105 / 245 ms | 396 / 856 ms | 76 / 194 ms | 387 / 727 ms |
| small_get_4k | 57 / 70 ms | 533 / 1,149 ms | 33 / 34 ms | 321 / 532 ms |
| small_head_4k (head cache) | < 1 ms | 281 / 308 ms | 32 ms (a Geyser call) | 307 / 328 ms |
| list 30 keys | 2 ms | 480 ms | 2 ms | 290 ms |
| small_delete | 63 / 75 ms | 379 / 778 ms | 94 / 283 ms | **6.7 / 9.0 s** |
| single_put_8mb | 16 MB/s | 2.3 MB/s | 13 MB/s | 2.6 MB/s |
| get_single_8mb | 16 MB/s, ttfb 165 ms | **156 s** (one stalled read) | 24 MB/s, ttfb 98 ms | 5.4 MB/s, ttfb 329 ms |
| single_put_64mb | 43 MB/s | 19 MB/s | 55 MB/s | 33 MB/s |
| get_single_64mb | 52 MB/s, ttfb 1.25 s | 19 MB/s, ttfb 881 ms | 22 MB/s, ttfb 97 ms | 26 MB/s, ttfb 343 ms |
| single_put_256mb (chunked) | 450 MB/s† | n/a (> 100 MB via CF) | — | — |
| get_single_256mb | 114 MB/s, ttfb 510 ms | — | — | — |
| mpu_put_256mb (16 × 16 MiB, 8 wide) | 100 MB/s | 39 MB/s | 57 MB/s | 33 MB/s |
| get_mpu_256mb | 116 MB/s, ttfb 392 ms | 33 MB/s, ttfb 844 ms | 23 MB/s, ttfb 66 ms | **3.6 MB/s** (75 s) |
| mpu_put_1024mb | 140 MB/s | 91 MB/s | — | — |
| get_mpu_1024mb | 133 MB/s, ttfb 516 ms | **ERROR unexpected EOF** | — | — |
| range_get_1mb × 20 | 240 / 382 ms | **7.8 / 29.9 s** | 281 / 435 ms | 692 / 1,296 ms |
| archive_head / RestoreObject | — | — | class GLACIER; restore → 403 `InvalidObjectState` (still in the landing zone, directly readable — the 10-12 result) | same |
| 32 workers × 4 MiB PUT, 30 s | **753 MB/s**, 180 ops/s, p50 123 ms | 256 MB/s, 61 ops/s, p50 433 ms | — | — |

† the 256 MiB single PUT is the chunked path and the bench reuses its data seed, so the
chunks already existed from an earlier run: that number is the dedup hit rate, not the
upload speed (the 10-12 fresh-chunk figure was 128 MB/s).

Reading it, against 10-12 (same shape, `std` 119/61 ms small ops, 38–57 MB/s whole
reads, 124 MB/s chunked):

- **The origin path is the same as two days ago or better**: Lyve small ops ~60–105 ms,
  whole 256 MiB reads 114–116 MB/s, a 1 GiB multipart 140 in / 133 out, 753 MB/s with 32
  writers. #721 changed nothing on the origin, as intended.
- **Vault on the origin reads at 22–24 MB/s whole and 23 MB/s multipart** from the landing
  zone — the parallel-range driver's ceiling for a single object that is still on disk at
  Geyser (10-12: 20–21). That is the number a customer sees before eviction; after eviction
  it is a restore.
- **The box → Cloudflare path was bad today**: a stalled 8 MB read (156 s), a 1 GiB read cut
  with `unexpected EOF`, range reads at 7.8 s p50, deletes on the archive bucket at 6.7 s.
  10-12 saw the same shape milder (range p50 248 ms, 5–10× slower small ops). The second raw
  pass was saturating the box's uplink to iDrive at the same time, which is part of it; the
  remote vantages below, which are what customers see, did not reproduce any of it. The
  EOF is worth one re-run on a quiet box before it is called a Cloudflare fact (§7).

### 2.1a Public on R2, through `/cdn` (first pass answered 404: the bucket row was
`public`, the handler wants `public-read`; re-run as `pub2`)

`tb-pub` on R2, bench tenant slug `bench`, CDN URL `https://stored.ge/cdn/bench/tb-pub/<key>`,
fetched twice unauthenticated: pass 1 = the Cloudflare miss (origin → R2 → edge), pass 2 =
the warm hit. 17:52–17:54 UTC from the box.

| Workload | pub-origin (S3 API, direct) | pub-cf (S3 API through Cloudflare) | `/cdn` miss → hit |
|---|---|---|---|
| small_put_4k | 216 / 306 ms | 526 / 1,270 ms | — |
| small_get_4k | 123 / 222 ms | 272 / 837 ms | 4 KiB: 0.52 s → 0.23 s, both `DYNAMIC` (never cached) |
| single_put_8mb | 11 MB/s | 1.9 MB/s | — |
| get_single_8mb | 31 MB/s, ttfb 130 ms | 10 MB/s, ttfb 337 ms | 11 MB/s (0.77 s) → **72 MB/s, ttfb 61 ms** |
| get_single_64mb | 52 MB/s, ttfb 110 ms | 31 MB/s, ttfb 367 ms | 19 MB/s (3.6 s) → **217–276 MB/s, ttfb 64–81 ms** |
| mpu_put_256mb | 36 MB/s | 21 MB/s | — |
| get_mpu_256mb | 78 MB/s, ttfb 148 ms | 37 MB/s, ttfb 438 ms | 15–22 MB/s (12–18 s) → **298–313 MB/s, ttfb 72–141 ms** |
| range_get_1mb | — | 348 / 711 ms | — |

Reading it: R2 writes from SLC are slow (11–36 MB/s, 10-12: 5–45) and an S3 read of a
public object through the API is 31–78 MB/s; the same object through the warm edge is
217–313 MB/s at 60–140 ms to first byte, 4–10× the API. The miss costs one origin read at
R2 speed (11–22 MB/s here) and the 4 KiB object is never cached (`DYNAMIC` — small
responses without cache headers; a `Cache-Control` on `/cdn` responses would fix it).
From Croatia the warm hit ran at 102 MB/s (§2.3). Public is the tier where Cloudflare
earns its place; everything else only passes through it.

### 2.2 Standard on iDrive (the lab instance, primary = the new account, Dallas)

Same build, same box, port 8100, loopback (no HAProxy, no TLS — compare with the
`std-origin` column above minus ~5–10 ms), `parallel_get` on for the lab tenant.
`lab-std` = Standard on iDrive us-central-1 (Dallas, the prod convention); `lab-eu` = a
bucket pinned to iDrive eu-central-1 (Frankfurt) — the residency feature, served through
SLC; `lab-arch` = Vault on the shared Geyser bucket; `lab-pub` = Public on the shared R2
bucket through the S3 API (the lab's `/cdn` route answered 404 — not chased; the `/cdn`
numbers are in §2.1a).

| Workload | Standard on iDrive Dallas | EU bucket on iDrive Frankfurt | Vault (Geyser) via the lab | Public (R2) via the S3 API |
|---|---|---|---|---|
| small_put_4k | **36 / 39 ms** | 159 / 216 ms | 62 / 185 ms | 34 / 36 ms |
| small_get_4k | **30 / 31 ms** | 137 / 180 ms | 33 / 34 ms | 29 / 31 ms |
| small_delete | 31 / 33 ms | 140 / 215 ms | 113 / 217 ms | 31 / 31 ms |
| single_put_8mb | 26 MB/s | 4.8 MB/s | 22 MB/s | 69 MB/s |
| get_single_8mb | 2.2 MB/s† | 5.8 MB/s | 7.0 MB/s | 4.2 MB/s† |
| single_put_64mb | 96 MB/s | 25 MB/s | 62 MB/s | 96 MB/s |
| get_single_64mb | 43 MB/s, ttfb 1.13 s | 15 MB/s, ttfb 1.2 s | 25 MB/s, ttfb 35 ms | 83 MB/s, ttfb 339 ms |
| single_put_256mb (chunked, fresh) | 160 MB/s | 98 MB/s | — | — |
| get_single_256mb | 127 MB/s, ttfb 245 ms | 36 MB/s, ttfb 3.4 s | — | — |
| mpu_put_256mb | 123 MB/s | 91 MB/s | 67 MB/s | 273 MB/s |
| get_mpu_256mb | **182 MB/s**, ttfb 315 ms | **4.1 MB/s** (66 s), ttfb 3.0 s | 20 MB/s, ttfb 36 ms | 97 MB/s, ttfb 464 ms |
| mpu_put_1024mb / get | 127 / 144 MB/s | — | — | — |
| range_get_1mb | 195 / 247 ms | 264 / 271 ms | 280 / 525 ms | 117 / 250 ms |
| 32 workers × 4 MiB PUT | **875 MB/s**, 209 ops/s, p50 133 ms | — | — | — |
| 8 workers × 64 MiB PUT | 778 MB/s, p50 655 ms | — | — | — |

† one slow single-connection read each on iDrive and R2 (3.8 s and 2.0 s for 8 MB): the
same per-connection variance the raw pass shows; the next size up read at 43–83 MB/s.

Reading it:

- **Standard on iDrive Dallas beats Standard on Lyve on every small-op line** (30–36 ms
  vs 57–105 ms on the origin) and on multipart reads (182 vs 116 MB/s) and writes (123 vs
  100); the ingest ceiling is higher too (875 vs 753 MB/s). The chunked 256 MiB path is
  160 in / 127 out against Lyve's 128 / 114 on 10-12. Single-connection whole reads are
  the same lottery on both (43 vs 52 MB/s here, 37–241 MB/s on 10-10).
- **The EU bucket is correct and slow from here**: Frankfurt adds ~100–130 ms to every
  small op from SLC and a whole-object multipart read came back at 4 MB/s on one
  connection (the chunked read at 36 MB/s). A European customer reading a Frankfurt bucket
  goes SLC → Frankfurt → SLC → Europe. Residency works; it is not a performance feature
  until there is a hub in Europe or the parallel reader covers region drivers.
- **Vault and Public behave the same through the lab as through prod** (Geyser 20–25 MB/s
  reads; R2 83–97 MB/s reads, 273 MB/s multipart in), so the tier numbers in §2.1 are the
  product's, not Lyve's.

### 2.3 From outside: New York (Hostbrr) and Croatia (Maxko), rclone 1.75

The clients customers run, from shared hosting accounts on other networks, against
`stored.ge` through Cloudflare (rclone's default) — the real customer path. Maxko ran
17:52–17:54 UTC (the box's second raw pass still running; irrelevant to this path).

| Workload | Maxko, Sisak HR (166 ms RTT) | Hostbrr, New York (50 ms) |
|---|---|---|
| 256 MiB multipart PUT (32 MiB parts, 8 wide) | 40.5 s = 6.6 MB/s (yesterday 32 MB/s with 64 MiB parts, 4 wide — the account's uplink varies by the hour) | 5.4 s = **50 MB/s** |
| 256 MiB GET, 4 streams (the #721 path) | 6.8 s = 40 MB/s, byte-identical | 16.6 s = 16 MB/s, byte-identical |
| 256 MiB GET, single stream | 5.7 s = 47 MB/s | 6.0 s = **45 MB/s** |
| 64 × 1 MiB PUT, 16 transfers | 7.2 s = 8.9 files/s | 4.5 s = 14 files/s |
| 64 × 1 MiB GET, 16 transfers | 4.1 s = 15.6 files/s | 2.1 s = 30 files/s |
| `/cdn` 64 MiB, first → second fetch | miss: ttfb 0.57 s, 5.6 s total (12 MB/s); hit: ttfb 0.15 s, 0.66 s total (**102 MB/s**) | 0.54 s then 1.9 s (ttfb 0.25 / 0.31 s)§ |
| presigned GET, time to first byte | 0.61 s | 2.26 s (yesterday 0.52 s) |

§ the New York "first fetch" was already a hit: the same key had been fetched by an
earlier, failed run of the script, the object was purged and re-uploaded, and the edge
served the cached copy of the old object (same URL, cache TTL not expired). That is
how a CDN works, and it is the reason a public object must get a new key when its
content changes, or the bucket's cache max-age must be short.

Reading it: from both continents the download side is network-bound at 40–47 MB/s on a
single stream; rclone's four streams gained nothing from Croatia and lost from New York
(16 MB/s — four 64 MiB ranges competing on one shared-host uplink). Small files run at
9–30 per second. The warm CDN hit at 102 MB/s from Croatia is faster than anything
through the S3 API — the Public tier is the right answer for anything read more than
once. The slow Croatian upload is the account, not Stored: the same box did 32 MB/s
yesterday, and New York uploaded at 50 MB/s.

---

## 3. The backends, raw, from the box

`bench-compare`, one process, each endpoint in turn, the tuned transport (200 idle
connections, 4 MB buffers). p50 for small ops; MB/s for transfers; "concurrent" = 20 s
of 16 parallel 4 MB objects. Lyve and Geyser from the first pass (`bench-compare.json.txt`);
iDrive, Wasabi, R2 and B2 from the second pass (`bench-compare-2.json.txt`,
`bench-compare-b2.json.txt`) — the first pass was killed after 30 minutes stuck on a Geyser
range read of a cold object (see §1).

| Backend | 4 KiB PUT / GET / HEAD p50 | 64 MB PUT / GET | 256 MB multipart (16 parts, h1) | 16×4 MB ingest / download | 1 MiB range p50 | list 100 |
|---|---|---|---|---|---|---|
| Lyve us-west-1 (prod's primary) | 165 / 45 / 42 ms | 137 / 84 MB/s | — | **754 / 617 MB/s** | 49 ms | 240 ms |
| Lyve us-east-1 | 88 / 58 / 56 ms | 75 / 1.0† MB/s | — | 670 / 360 MB/s | 66 ms | 271 ms |
| Lyve eu-west-1 | 136 / 133 / 132 ms | 33 / 22 MB/s | — | 352 / 204 MB/s | 145 ms | 620 ms |
| Geyser LA (tape landing zone) | 57 / 31 / 30 ms | 24 / 3.5 MB/s | 101 MB/s | 26 / 3.6 MB/s | stuck > 30 min (killed) | **31.5 s** |
| **iDrive us-central-1 (Dallas, new account, the prod convention)** | **30 / 27 / 26 ms** | 106 / 103 MB/s | 254 MB/s | **835 / 351 MB/s** | 32 ms | 154 ms |
| iDrive us-west-2 (Los Angeles) | 28 / 24 / 24 ms | 54 / 17 MB/s | 102 MB/s | 36‡ / 277 MB/s | 450 ms‡ | 95 ms |
| iDrive us-west-4 (Oregon) | 30 / 28 / 27 ms | 108 / 80 MB/s | **311 MB/s** | 74‡ / 55‡ MB/s | 42 ms | 107 ms |
| iDrive us-east-1 (Virginia) | 52 / 47 / 46 ms | 63 / 7.9 MB/s | 173 MB/s | 263 / 279 MB/s | 93 ms | 184 ms |
| iDrive us-midwest-1 (Chicago) | 38 / 34 / 34 ms | 97 / 92 MB/s | 240 MB/s | 58‡ / 4.5‡ MB/s | 411 ms‡ | 145 ms |
| iDrive us-southeast-1 (Miami) | 63 / 58 / 57 ms | 51 / 47 MB/s | 178 MB/s | 443 / 182 MB/s | 281 ms | 227 ms |
| iDrive eu-west-1 (Ireland) | 130 / 120 / 119 ms | 27 / 33 MB/s | 108 MB/s | 337 / 169 MB/s | 122 ms | 593 ms |
| iDrive eu-west-3 (London) | 135 / 124 / 123 ms | 26 / 11 MB/s | 110 MB/s | 335 / 192 MB/s | 133 ms | 6.7 s |
| iDrive eu-central-1 (Frankfurt) | 142 / 132 / 131 ms | 25 / EOF† MB/s | 95 MB/s | 278 / 115 MB/s | 392 ms | 523 ms |
| iDrive eu-south-1 (Milan) | 151 / 144 / 138 ms | 20 / 12 MB/s | 98 MB/s | 280 / 157 MB/s | 145 ms | 557 ms |
| iDrive ap-northeast-1 (Tokyo) | 119 / 120 / 108 ms | 28 / 32 MB/s | 95 MB/s | 365 / 97 MB/s | 114 ms | 435 ms |
| Wasabi us-west-1 (partner account, prod's pair; the bench pair answers 403 to everything) | 50 / 36 / 32 ms, but 4 KiB PUT p99 **14.8 s** (4 ops/s) | 50 / 37 MB/s | 164 MB/s | 281 / **506 MB/s** | 129 ms | 342 ms |
| R2 (default jurisdiction) — raw pass impossible: the bench token is bucket-scoped and answers 401 to CreateBucket; the numbers are through the product (lab, §2.2, and 10-12) | 34 / 29 ms via the product | 96 / 83 MB/s via the product (10-12 raw: 5–45 MB/s writes) | 273 MB/s via the product | — | 117 ms | — |
| Backblaze B2 us-east-005 (bucket `fairforge-bench-cold`; `vt-ecbench-b2` is gone and the scoped key cannot create one) | **242** / 69 / 58 ms (3 PUT/s) | 27 / 42 MB/s | 66 MB/s | 150 / **484 MB/s** | 104 ms | 252 ms |
| Pixeldrain (REST, not S3; `pixeldrain-bench`) | 1 KiB PUT 0.6 ops/s, GET 2.3 ops/s | 100 MB: 24 / 24 MB/s; 1 GB: 106 / 69 MB/s | — | large concurrent **358 / 466 MB/s**; small 3.9 / 23 ops/s | 16 ranges of 1 GB: 87 MB/s | 985 ops/s (cached) |

Pixeldrain is a CDN with an upload API, not an object store: one small object costs
0.5–2 s, a gigabyte streams at 70–110 MB/s per connection and 358–466 MB/s in parallel,
and it rebuilt a Vault object from parity at 36 MB/s (§4.3). It stays what the 2026-04
evaluation said — a public-delivery option — and the Cloudflare cache has since taken
that role for free.

† one 65 s read of a 64 MB object on us-east-1 while the same endpoint streamed 360 MB/s
concurrently: Lyve's per-connection speed varies by the minute (also seen 2026-10-10,
37–241 MB/s on iDrive). Single-stream numbers on any S3 backend are one sample.

Geyser as a raw backend: 100 MB/s multipart in, 24 MB/s single PUT, and reads that
depend entirely on whether the object is still in the landing zone (3.5 MB/s here, a
cold object blocked the bench). A 100-key listing takes 30 s. None of this is news; it is
why Vault reads go through the parallel-range driver and why tape is never a hot leg.

---

## 4. The parity legs

### 4.1 Sync.com, one bridge and five

Through the real driver (`drivers.NewWebDAVDriver`, the multi-bridge driver for the
five-bridge rows), every read sha256-verified, 0 stalls, 0 retries, 0 mismatches.
`webdav-one.json.txt` / `webdav-five.json.txt`.

| Workload | one bridge (4918) | five bridges (the prod `sync` backend) |
|---|---|---|
| 4 KiB PUT, c1 / c8 / c32 | 1.5 / 8.8 / 7.2 ops/s; p50 0.49 s / 0.56 s / **5.8 s** | — |
| 4 KiB GET, c1 / c8 / c32 | 1.3 / 11 / 10 ops/s; p50 0.61 / 0.69 / 2.7 s | — |
| 4 KiB Exists | 4,170 ops/s, p50 7 ms (the driver's PROPFIND cache) | — |
| 1 MiB PUT / GET, c8 | 8.3 / 10 MB/s | — |
| 16 MiB PUT / GET, c1 | 3.1 / 13 MB/s | 2.4 / 13 MB/s |
| 16 MiB PUT / GET, c4 | 15 / 49 MB/s | 13 / 49 MB/s |
| 256 MiB PUT / GET, c1 | 22 / 44 MB/s | 23 / 40 MB/s |
| 256 MiB PUT / GET, c4 | 56 / 149 MB/s | 73 / 150 MB/s |
| 1 GiB PUT / GET, c1 | — | 25 / 49 MB/s |
| 1 GiB PUT / GET, c4 | — | **94 / 174 MB/s** |
| GET time to first byte | 1.27 s | 1.26–1.42 s, every size |
| Range reads in a 256 MiB object | 64 KiB: 0.8 ops/s; 4 MiB: 1.1 ops/s (4.5 MB/s) | — |
| Parity 4+4, 64 MiB: write m / read m / degraded k | — | 11 / 51 / 56 MB/s |
| Parity 4+4, 256 MiB | — | 44 / 109 / 108 MB/s |
| Parity 4+4, 1 GiB | — | 36 / **20** / 126 MB/s (read-m p50 52 s) |
| 300 × 1 MiB shards, flat folder, c16 | 10 / 10 MB/s | 14 / 19 MB/s |
| 300 × 1 MiB shards, fan-out folders, c16 | 1.7 / 5.5 MB/s | 3.5 / 8.6 MB/s |
| Cross-bridge visibility, 64 KiB, 10 rounds | — | new: p50 2.4 s, p95 **25 s**; overwrite: p50 31 s; delete: p50 30 s, p95 40 s |

Reading it: Sync is a sequential-throughput leg with a fixed ~1.3 s per request. Four
streams of 256 MiB or 1 GiB saturate it at 73–94 MB/s in and 150–174 MB/s out, the same
as the 2026-10-07 numbers. Anything small is slow (8–14 ops/s at best, 5.8 s p50 for 4 KiB
writes at 32 concurrent) and a 1 GiB object's four 256 MiB parity shards read back at
20 MB/s with a 52 s p50, so parity for large objects wants smaller shards or the striped
path. A second bridge sees a write after 2.4 s typically and 25–40 s at the tail, which is
why the driver routes mutable reads to the writing bridge only.

### 4.2 OneDrive fleet (3 accounts)

Through the real driver (`onedrive-bench`, FNV placement, 0 errors, post-delete listing clean).

| Workload | PUT | GET | DELETE |
|---|---|---|---|
| 12 × 64 MB, c4 | 49 MB/s | 98 MB/s | 197 MB/s |
| 40 × 4 MB, c8 | 17 MB/s | 39 MB/s | 29 MB/s |

Reading it: a 64 MB object costs an upload session (≈ 1.3 s of fixed overhead); below
~16 MB the fleet is request-bound. Three accounts give half the Sync fleet's ingest and
two-thirds of its reads. It is a leg for large, rarely read copies, which is exactly
the parity role, and it is free. Microsoft's terms are the risk, not the numbers.

### 4.3 Erasure layouts: write once, rebuild from each leg

`erasure-bench`, Reed–Solomon 4+4 over a 256 MiB payload (64 MiB shards; 10+6 for the
fleet shape), 2 runs per layout, every rebuild sha256-verified. "write" = all 8 shards
landed (payload rate; wire rate is 2×); "read" = the fastest 4 shards win; "lose X" = the
object rebuilt with every shard on leg X unavailable, i.e. the speed at which a tier
survives losing that leg. `erasure.txt`. The Backblaze rows are from the second pass
(`erasure-b2.txt`) — the first pass pointed at a bench bucket that no longer exists.

| Layout (data → parity) | Write, run 1 / 2 | Read (fastest 4) | Lose the data leg → rebuilt from parity | Lose the parity leg → read from data |
|---|---|---|---|---|
| Geyser → Sync (today's Vault) | 12 / 37 MB/s (Sync-bound: 21 s, 7 s) | 5 MB/s‡ / 101 | **62 / 12 MB/s** | 60 / 76 |
| Geyser → OneDrive | 47 / 51 | 80 / 94 | **66 / 69** | 70 / 73 |
| Geyser → Lyve | 161 / 198 | 167 / 210 | **150 / 235** | 72 / 61 |
| Geyser → iDrive (new account) | 174 / 198 | 114 / 112 | **115 / 124** | 68 / 63 |
| Geyser → B2 | 42 / 106 (B2-bound: 6.0 s, 2.4 s) | 128 / 135 | **158 / 154** | 63 / 64 |
| iDrive → Lyve | 190 / 234 | 275 / 231 | 255 / 200 | 35 / 43 |
| Lyve → iDrive | 154 / 234 | 194 / 226 | 176 / 137 | 320 / 28 |
| iDrive → B2 | 46 / 99 (B2-bound: 5.5 s, 2.6 s) | 145 / 178 | **126 / 124** | 161 / 24 |
| iDrive 6 + Lyve 6 + B2 4, RS 10+6 (the fleet shape, 1.6× overhead) | 61 / 74 (B2-bound) | 218 / 278 | lose B2 223 / 276; lose iDrive 181 / 198; **lose Lyve 200 / 43** | — |
| Geyser → Pixeldrain | 30 MB/s (Pixeldrain-bound, 8.6 s) | 24 | **36** | 21 |

‡ the first read of the first layout waited 48 s on Geyser (one cold shard); the same
read on run 2 took 2.5 s.

Reading it, for the Vault parity home:

- **Rebuilding a Vault object with Geyser gone** is the only number that matters for the
  parity leg, and it ranks **Lyve 150–235 MB/s > B2 154–158 > iDrive 115–124 >
  OneDrive 66–69 > Pixeldrain 36 > Sync 12–62**. Sync is the slowest leg and the one
  holding every parity shard today. B2's writes are the slow side (2.4–6 s for four 64 MiB
  shards against 0.5–1.4 s on iDrive and Lyve) and its small ops are the slowest of any
  S3 backend here (3 PUT/s, §3), which does not matter for parity shards.
- **Writing parity to Sync costs the write**: 7–21 s for 256 MiB against 1.3–1.5 s on any
  S3 leg. That is the 20 MB/s the `vault_parity` job's size estimate assumes.
- **Geyser as a data leg reads at 60–76 MB/s** when its shards are in the landing zone,
  on every layout; the cold-shard case is a restore.
- **iDrive ↔ Lyve either way is a 137–320 MB/s mirror** when both are up and a
  28–43 MB/s single-connection read when one is down and the other serves alone — the
  per-connection variance again, not a leg property (the same layout gave 320 and 28 in
  two consecutive runs).

---

## 5. Cloudflare, product by product, where it belongs

Everything here was verified on the real zone or the lab account between 2026-10-03 and
2026-10-14 (`hearth/docs/cloudflare/verified.md`, `docs/CLOUDFLARE.md`); the numbers in
§2 add today's customer-path measurements.

| Product | Plan | Pairs with | What it does for the tier | What it must not do |
|---|---|---|---|---|
| The zone (proxy, WAF, rate limits, Bot Fight Mode) | Free today | every tier's S3 API on `stored.ge` / `s3.stored.cloud` | hides the origin, absorbs scanners (1,090 auth probes in 9 h on 2026-10-11), rate-limits sign-in | add latency it does not need to: 20–60 ms per op from a real client (10-12); refuses single bodies > 100 MB (413) — multipart with 64 MB parts is fine, 128 MB parts are not; rewrites `Accept-Encoding` toward the origin (the #721 bug) and gzips error bodies unless `no-transform` |
| Cache + `/cdn` | Free | **Public** | a warm hit is a real CDN: 11–36 ms TTFB, 207–297 MB/s to a box, 20–58 MB/s to a home line (10-12) | the miss path is erratic (0.8–78 s on 10-12) — Public objects should be pre-warmed or served through Workers Cache |
| R2 | pay-as-you-go, $15.36/TB-mo, $0 egress | **Public** only | the only leg whose egress is free; the origin every Cloudflare cache pattern can pull from | never a tier: 3.7× iDrive per TB; writes from SLC 5–45 MB/s (10-12) |
| Sippy / the DO puller | Workers Paid | **Public**, large objects | lazy promotion of a private object into R2 on first view: ≤ 200 MiB Sippy, > 512 MiB the DO copier (10-09) | Sippy reads the origin twice for 200–650 MiB |
| Workers Cache gateway | Workers Paid | **Public**, **Standard presigned reads** | HIT TTFB 0.16–0.27 s, 56–72 MB/s hits, range slicing on a hit; > 512 MB bypasses | never HEAD a presigned URL from a Worker (a cold cache turns it into a GET) |
| Custom Hostnames (Cloudflare for SaaS) | 100 free, then $0.10 | **Public** buckets on a customer's own domain; white-label tenancy | `cdn.customer.com` → `/cdn/<slug>/<bucket>` with Cloudflare's certificate | the zone token cannot read them today (needs the permission) |
| Workers Paid ($5/mo) | — | the control plane: dashboards, status page, Hearth-CF apps, presign minting at the edge, the Forge-CF provisioner | 10 M requests and the D1/DO/Queues allowances cover ~15 apps | never the storage plane; 128 MB and 30 s CPU per request |
| Access | Free ≤ 50 users | admin, Grafana, the own box's management ports | proven as a gate 2026-10-04 | — |
| Tunnel | Free | the own box / a VPS as an origin without a public port | — | **not for the S3 origin**: a quick tunnel did 56/75 MB/s vs 109/168 proxied and 515 direct, and collapses the client IP to 127.0.0.1 (breaks `internal/clientip`) |
| Email Service | — | transactional mail from stored.cloud (live since 2026-10-12) | — | — |
| Logpush → a Stored bucket | needs the Logs permission on the token | observability, and a demo of the product eating its own logs | — | untested |
| Containers | Workers Paid, $6.48/GiB-mo awake | the always-on helpers (museum, OnlyOffice) **only if they sleep** | — | 5–6× a rented box per GiB of RAM awake |
| Smart Placement, Cache Reserve, Argo | — | — | Smart Placement: no gain measured (10-04); Cache Reserve refused on Free (API 1135) | — |

Two settings that matter and are easy to get wrong: `origin_max_http_version` must be **1**
on every zone that fronts uploads (the h2 window caps a single PUT at 3–5 MB/s; stored.cloud
still had 2 on 2026-10-10), and the S3 host must never be an alias-redirect target (a SigV4
signature covers the Host).

---

## 6. The best mix per tier and per use case

The rule under every row: two homes for every byte, the hot home is the cheapest disk
that measured well, Cloudflare is the edge and the control plane and never a tier.
"Now" = the launch configuration this report supports; "next" = when the trigger in the
right-hand column arrives.

| Tier | Use cases | Primary, now | Second copy, now | Cloudflare product | Next, and the trigger |
|---|---|---|---|---|---|
| **Standard** | developers, app storage, restic / rclone / Kopia backups, anything read often | **iDrive us-central-1** (§2.2: 30–36 ms small ops, 182 MB/s multipart reads, 875 MB/s ingest — ahead of Lyve on every line) once the account is on annual prepay and the canary watches it; **Lyve** until then and as the free underwriter | **Geyser at ingest** (free to 100 TB; the data-loss floor, §4.3: 60–76 MB/s reads from the landing zone) and Lyve while it is $0 | the zone only (WAF, rate limits, hidden origin); `origin_max_http_version = 1`; no cache; the S3 host never an alias target | own box as primary at ~400 TB stored; the StorageBox or a second box as the live mirror; Lyve becomes the mirror when the promo ends |
| **Standard, EU residency** | EU customers who need the bucket in Europe | **iDrive eu-central-1 / eu-west-1** (pinned bucket, §2.2) | a second iDrive EU region or tape | the zone | a hub in Europe before residency is sold as fast: today a Frankfurt bucket reads whole objects at 4 MB/s through SLC |
| **Performance** | low-latency, regional, enterprise RFPs | the **iDrive region nearest the customer** (Dallas / LA / Oregon 24–30 ms from SLC, 240–311 MB/s multipart) with `parallel_get` | Lyve | the zone; Workers Cache gateway for presigned read-heavy apps (0.16–0.27 s TTFB on a hit) | own box for US customers, iDrive regions for breadth |
| **Smart** | mixed data that cools | same as Standard, 15 % hot | tape at ingest — the demotion becomes a reclaim | the zone | the flag flips on after the ingest copy ships |
| **Vault** | archives, compliance, Object Lock, 7-year retention | **Geyser tape** (22–25 MB/s reads from the landing zone, restore after eviction) | **RS parity on Lyve** — rebuild 150–235 MB/s, the fastest leg measured (§4.3); iDrive 115–124 next; Sync 12–62 last, and it holds every shard today | the zone | parity home → the StorageBox or own disk when Lyve charges; London as the second tape site at volume |
| **Public** | websites, downloads, media, open datasets, anything read more than once | **R2** | the authoritative object on the Standard primary | **the cache on `/cdn`** (warm: 217–313 MB/s, 60–140 ms TTFB from the box, 102 MB/s from Croatia); Sippy / the DO puller for lazy promotion; **Custom Hostnames** for a customer's own domain; Workers Cache for ranges | pre-warm on publish; `Cache-Control` on `/cdn` so 4 KiB objects stop being `DYNAMIC` |
| **Pin-hot add-on** | customer pays to never cool | Standard primary, exempt from demotion | Standard's second copy | the zone | — |
| **Sync tier** | own data only | Sync.com | none | — | retire into the cheap disk leg |
| **Internal** | database dumps, exports, probes, benches | Sync.com (dumps today) | **the 1 TB StorageBox** in the same metro | Access for the admin paths; Logpush into a Stored bucket once the token allows | own box |

And the legs that are not in the mix, with the measurement that puts them there:

- **Sync.com** — 1.3 s per request, 8–14 small ops/s, 20 MB/s reading a gigabyte's parity,
  25–40 s cross-bridge visibility at the tail, resale forbidden by its terms. Own data and
  today's parity shards until the re-protect moves them.
- **OneDrive** — 49 in / 98 out MB/s through the driver, a sound 66–69 MB/s parity rebuild,
  and Microsoft's per-user terms. A third copy of own data at most.
- **Wasabi** — the bench pair answered 403 to everything (prod's pair is re-measured in
  §3); throttling seen under load on 2026-10-04; 90-day minimum when paid. Dormant switch
  target only.
- **Pixeldrain** — 0.6 small ops/s; a CDN, and the Cloudflare cache does that for free.
- **Backblaze B2** — a real candidate for the parity home: rebuilt a Vault object alone at
  154–158 MB/s (second only to Lyve), free egress through Cloudflare, $6/TB, a name every
  sub-processor list already carries. Slow small ops (3 PUT/s) rule it out as a hot tier.
  If Lyve starts charging before the cheap disk leg exists, this is where parity goes.
- **Quotaless** — account dead since 2026-05; remove.

---

## 7. What to change

1. **Put the new iDrive account on annual prepay and make it the Standard primary.** It
   measured faster than Lyve on every customer-path line (§2.2) and the prod iDrive keys are
   object-dead anyway; the swap is the per-region pairs into prod `.env` (`deploy/scripts/
   idrive-region-env.sh .private/idrive-keys-2026-10-14.env`), `STORAGE_MODE=idrive`,
   `vaultaire-switch restart`. Rows still on Lyve stay readable (keep `LYVE_*` set). Owner
   decision — the account's payment is the thing that has failed three times.
2. **Move the Vault parity home to Lyve first** (`VAULT_PARITY_LEGS=lyve,permafrost,sync`)
   today, the cheap disk leg later: it rebuilds 2–20× faster than Sync and writes parity in
   1.3 s instead of 7–21 s; a re-protect job moves the shards already on Sync.
3. **Build the ingest copy to tape for Standard and Smart** (the plan change from
   2026-10-14): every write followed by a copy to Geyser, rows record both homes, reads fall
   back. The erasure rows show Geyser serving 60–76 MB/s from the landing zone, so the
   fallback is usable, not just survivable, for recent objects.
4. **Add the cheap disk leg driver** (SFTP, or a SabreDAV shim behind the WebDAV driver) and
   buy the 1 TB StorageBox for the dumps now; size up when there are terabytes to mirror.
5. **Two small fixes on `/cdn`:** a `Cache-Control` header so small objects are cached
   (today `DYNAMIC`), and documentation that an overwritten public object is served stale
   until the bucket's max-age expires — new key on new content.
6. **`parallel_get` must cover the region drivers** before residency is sold as fast; a
   Frankfurt bucket read whole from SLC at 4 MB/s on one connection.
7. **Re-run the Cloudflare pass from a quiet box** before the `unexpected EOF` on a 1 GiB
   read and the 156 s 8 MB read are called Cloudflare facts; the remote vantages did not
   reproduce them. If they recur, it is a `stored.ge` zone setting, not a product bug.
8. **Rotate the reseller API key** (pasted in chat, 7-day) and refresh the dead bench pairs
   in `.env.bench` on the box (Wasabi 403, B2's bucket gone, the old iDrive pairs). The
   bare `export` lines in that file were neutralised today: they printed every credential
   into any log that sourced the file.

---

## 8. Cost of the mix

Per stored TB-month unless stated; list prices on 2026-10-14; the Lyve promo end date is
still not in writing.

| Leg | Cost | Role in the mix |
|---|---|---|
| Lyve | $0 under the promo; ~$7.99 modelled after | Standard primary while free, then the second copy |
| iDrive e2 reseller (new account) | $2.06 year 1 annual, $4.13 year 2+, $4.46 monthly; 3× stored egress free | regions, Performance, residency; annual only |
| Geyser tape | $0 marginal to the 100 TB minimum already paid, $1.55 after | Vault floor; Smart cold half; the disaster copy for Standard |
| Hostbrr StorageBox 16 TB (Utah) | $1.11 with the 40 % code yearly, $1.33 monthly | the cheap disk leg: parity home, Standard mirror, dumps (SFTP driver needed) |
| Wasabi (partner account) | $0 today, $6.99 + 90-day minimum when paid | dormant emergency primary only |
| Backblaze B2 | $6, free egress via Cloudflare | the "boring name" second copy if a disclosable cloud is wanted |
| R2 | $15.36, $0 egress | Public only |
| Sync.com | fixed subscription | own data; today's parity home (see §4.1) |
| OneDrive fleet | $0 sunk | tertiary copy of own data at most |
| Own box (R130 → STOR-OGD) | $69/mo for 20 TB → $729/mo for ~560 TB ≈ $1.30–2.10 usable | the Standard primary at scale, unmetered egress |
| Workers Paid + zone | $5/mo + $0 | the control plane and the edge |

---

## 9. Run log

- 16:0x UTC reseller API: user, 11 regions, 11 keys.
- 16:2x bench-compare launched (16 endpoints: lyve ×3, geyser, wasabi, r2, idrive ×11 on the
  new account — `vaultaire-bench` bucket created in every region first).
- 16:3x onedrive-bench (12 × 64 MB c4; 40 × 4 MB c8) then webdav-bench one bridge
  (small, large, range, parity) then five bridges (large, parity, crossbridge).
- 16:5x lab instance up on 8100 (`vaultaire_lab` DB, 89 migrations, 15/15 healthy once the
  `vaultaire-lab` bucket existed in Dallas — the primary driver does not create its bucket,
  the region drivers do). Tenant `lab2` (tier standard, 2 TB), buckets `lab-std`, `lab-arch`
  (archive), `lab-pub` (public), `lab-eu` (pinned eu-central-1); `parallel_get` on for it.
- prod bench tenant: `tb-std`, `tb-arch` (archive), `tb-pub` (public) — tierbench origin + cf,
  queued behind the raw bench so Lyve is not measured twice at once; then the lab; then B2
  raw + 9 erasure layouts; then the two remote vantages.
- 17:21 the first raw pass killed after 30 min on a Geyser range read; the prod run started;
  a second raw pass (iDrive ×11, Wasabi) ran beside it, R2 had no endpoint in the bench env.
- 17:22–17:4x prod tiers; 17:4x–17:49 lab tiers; 17:51–17:57 nine erasure layouts (the B2
  ones against a bucket that no longer existed); 17:52–17:54 Maxko vantage; 17:53 Public
  re-run (`pub2`, the row had `public` where the handler wants `public-read`); 17:57–18:07
  Pixeldrain raw + leg; 18:01–18:03 Hostbrr vantage (second attempt — the first died on a
  missing `/usr/bin/time`); 18:08–18:14 B2 raw + three layouts on `fairforge-bench-cold`;
  18:14–18:20 Wasabi with prod's pair (the bench pair is dead) and R2 (its token cannot
  create a bucket).
- Cleanup: lab stopped and `vaultaire_lab` dropped; `tb-*` and `lab-*` buckets deleted;
  Maxko throwaway subdomain and files removed, webroot clean; Hostbrr vantage files removed.
  Kept: the iDrive bench user, its `vaultaire-bench` buckets, the key file, `~/vaultaire-bench/
  bin-20261014` and `raw-20261014` on the box (the `run-*.out` files there contain the env
  dumps described in §7.8 — never copy them).
