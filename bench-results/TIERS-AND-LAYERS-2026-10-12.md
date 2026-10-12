# How Stored is running — tiers, backends and the layers above, 2026-10-12

A customer's-eye measurement of production (`a05bf41`, 14:20–15:20 UTC) with the
bench tenant's ordinary S3 key, plus the raw backends beside it, plus the things a
customer stacks on top (a JuiceFS mount, restic, the public CDN path, Cloudflare in
front of private reads). Follows `VAULTAIRE-PATH-2026-10-04.md`; numbers from that
program are quoted where today did not repeat them.

**Prod state while measuring.** Primary = **Lyve** (`STORAGE_MODE=lyve` since
2026-10-11 12:45, every iDrive key answers 403 on object calls); live: lyve, wasabi,
r2, geyser, permafrost (3 OneDrive accounts), sync (5 bridges), local. `/health`
degraded 7/18 with the 11 iDrive probes named. Flags: `parallel_get` ON for the bench
tenant only (it covers idrive/wasabi, not lyve), `vault_parity` global ON, `chunking`
default ON. So **Performance and Standard are the same thing today** (both = the
primary) and Resilient also = Lyve. This is what customers get today; it is not the
launch config.

**Vantage points.** (1) a client on the SLC box itself = a customer VPS in the same
datacenter, zero network distance — it measures Vaultaire's ceilings; through
`https://s3.stored.ge` (HAProxy + TLS, the direct origin), `https://stored.ge`
(through Cloudflare) and `http://127.0.0.1:8000` (loopback, what HAProxy/TLS cost).
(2) this Mac at home = a remote customer, uplink ≈ 25–40 MB/s, 66 ms RTT to SLC — it
measures latency, not throughput. The other VPS hosts (NYC, LA, dev) were all
unreachable; a real second-datacenter vantage is the one thing this program lacks.

Tool: `cmd/tools/tierbench` (new) — one bucket, one endpoint, every GET sha256-verified
against what was PUT; `-cdn` fetches each object twice unauthenticated; `-restore`
asks the archive restore state. Raw lines in `raw-2026-10-12/`.

## 1. Findings that change something

1. **Whole objects on the Lyve primary download single-stream at 38–70 MB/s; the
   chunked path does 124 MB/s.** A 1 GiB multipart upload (stored whole — multipart
   is never chunked) reads back at 38 MB/s via the origin and 70 via Cloudflare; a
   256 MiB *single* PUT (chunked above 64 MiB) reads back at 124 MB/s from the same
   backend through `parfetch`. `aws s3 cp` and rclone upload anything big as
   multipart, so this is the download speed most customers of the Standard tier see
   for large files today. The whole-object parallel reader (`parallel_get`, #666)
   is implemented for idrive/wasabi only. → **Extend `parallel_get` to `lyve`**
   (the GetRangeInfo copy noted on 2026-10-11) and flip it for the bench tenant
   first. Expected: 130–215 MB/s like the local measurement.
2. **A transient Sync-bridge refusal became a customer-visible 400 "InvalidArgument,
   check your key", with no log line.** The first 1 GiB multipart complete on the
   Sync tier answered 400 after 13 s inside the keep-alive; the same complete
   replayed with aws-cli succeeded (26 s, 64 parts, striped onto the bridges), and
   three fresh 1 GiB uploads then succeeded 3/3 (complete 21–33 s). The complete
   handler maps every `engine.ErrInvalidInput` to 400 with `refusedKeySuggestion`
   and logs nothing (`s3_multipart.go` ~748); the WebDAV driver wraps a bridge
   MKCOL/PUT 400 in that same error. For our own encoded stripe names a bridge
   refusal is transient, not the customer's fault. → Log the cause at Warn on this
   branch; on the `sync` backend treat a refusal of a *generated* name (stripe
   pieces, parity shards, packs) as retryable/503, never 400.
3. **The public `/cdn` miss path through Cloudflare is erratic: 0.8 s to 78 s for a
   fresh 8 MiB object, while HAProxy-direct serves the same object in 0.23–0.48 s
   every time.** Reproduced from SLC and from the Mac (52 s, 78 s, 20 s, 11 s, 7 s,
   3 s, 0.8 s across 10 fresh objects; the 20 s one went through the SLC colo, the
   rest PHX). Hits are fine (11–36 ms TTFB, 58–297 MB/s). The origin logged its side
   of the slow requests at 0.25 s, so the time is inside Cloudflare's edge→origin
   fetch; Smart Tiered Cache is ON for the zone (2026-10-04) and the extra hop is
   the first suspect. → A/B with tiered cache off for `/cdn/*` (zone change, owner
   call), and pre-warm (DO copier / Sippy) stays the answer for anything a visitor
   must not wait for. Also: **a key without a file extension is never cached**
   (`cf-cache-status: DYNAMIC` on a 4 KiB `…/small/000` from both vantages) — the
   known extension rule; a cache rule for `/cdn/*` or the docs must say it.
4. **The concurrent-ingest tail on the origin is Vaultaire's, not iDrive's.** 20 s of
   1 MiB PUTs from 16 workers: raw Lyve 748 MB/s p99 420 ms; via the origin 103 MB/s
   p99 2.4 s (10-04 on iDrive: 53 MB/s p99 11.5 s, blamed on the h2 window then).
   Sustained 16 MiB uploads are fine (336 MB/s vs 610 raw), the load gate's 100 × 1 MB
   passes with p99 616 ms, and loopback 4 KiB PUTs are 68 ms p50 — so the cost is
   per-object work under concurrency (quota + head row + placement per PUT), not
   bandwidth. Worth a profile before launch; it is the shape of a backup client
   with many parallel small files.
5. **Archive restore is still untimed.** Every archive object written today was
   readable at once from Geyser's landing zone (20–27 MB/s via the origin, Geyser's
   own ceiling); RestoreObject answers `InvalidObjectState: directly readable
   … tape ~13 days after upload`; the 10-04 probe objects (8 days old) are still
   readable, one of them says `RestoreAlreadyInProgress`. HEAD of a vault object
   costs a Geyser round trip (32 ms) by design. Vault parity works: all four
   archive objects had their 4 parity shards on Sync 5–22 s after landing, and the
   4+4 geyser+sync shape rebuilds a 256 MiB object from the parity alone in 2.3 s.
6. **R2 as the public-bucket backend is slow to write from SLC:** 8 MiB single PUT
   1.7–1.8 s (5 MB/s) and 4 KiB PUT 200 ms p50 — R2's write latency from Salt Lake,
   not Vaultaire (reads are 31–82 MB/s). Fine for a publish-once bucket; wrong for
   anything chatty. Direct-to-R2 presigned uploads (#574) stay the answer for apps.

Everything else held: load gate 5/5 on the origin, conformance 22/22 (rclone,
presigned, versioning, dedup), 0 byte mismatches in ~140 verified reads, no
fallover/truncation/unknown-backend counters moved, `/health` unchanged.

## 2. The tiers, as a customer sees them

p50 / p95 for small ops; MB/s for transfers (ttfb = time to first body byte).
`std` = Standard (today: Lyve), `arch` = Vault/archive (Geyser GLACIER, floor vault),
`sync` = the Sync.com bridges, `pub` = public-read (R2). `origin` = s3.stored.ge,
`cf` = stored.ge, `loop` = 127.0.0.1:8000, `mac-*` = this Mac at home.

| workload | std-origin | std-cf | std-loop | mac-std-origin | mac-std-cf | arch-origin | arch-cf | mac-arch-origin | sync-origin | sync-cf | mac-sync-origin | pub-origin | pub-cf | mac-pub-origin |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| small_put_4k | 119 / 248 ms | 83 / 155 ms | 68 / 102 ms | 132 / 170 ms | 197 / 463 ms | 47 / 108 ms | 185 / 380 ms | 120 / 269 ms | 1159 / 3128 ms | 2552 / 3221 ms | 1775 / 2823 ms | 201 / 288 ms | 327 / 406 ms | 250 / 295 ms |
| small_get_4k | 61 / 72 ms | 71 / 86 ms | 61 / 249 ms | 129 / 136 ms | 153 / 165 ms | 33 / 34 ms | 175 / 206 ms | 98 / 99 ms | 653 / 750 ms | 709 / 719 ms | 758 / 820 ms | 115 / 208 ms | 233 / 264 ms | 184 / 244 ms |
| small_head_4k | <1 ms | 11 / 20 ms | <1 ms | 66 / 76 ms | 85 / 92 ms | 32 / 32 ms | 160 / 172 ms | 94 / 95 ms | <1 ms | 12 / 18 ms | 65 / 66 ms | <1 ms | 131 / 145 ms | 66 / 68 ms |
| small_delete | 68 / 77 ms | 76 / 242 ms | 67 / 77 ms | 131 / 206 ms | 154 / 340 ms | 52 / 150 ms | 174 / 315 ms | 137 / 210 ms | 877 / 1560 ms | 1982 / 2269 ms | 1343 / 2156 ms | 96 / 143 ms | 260 / 288 ms | 171 / 176 ms |
| single_put_8mb | 16 MB/s | 31 MB/s | 52 MB/s | 12 MB/s | 9 MB/s | 11 MB/s | 6 MB/s | 9 MB/s | 7 MB/s | 3 MB/s | 4 MB/s | 5 MB/s | 5 MB/s | 7 MB/s |
| get_single_8mb | 16 MB/s, ttfb 158 ms | 17 MB/s, ttfb 81 ms | 17 MB/s, ttfb 62 ms | 11 MB/s, ttfb 154 ms | 13 MB/s, ttfb 150 ms | 7 MB/s, ttfb 64 ms | 4 MB/s, ttfb 214 ms | 9 MB/s, ttfb 152 ms | 8 MB/s, ttfb 1027 ms | 8 MB/s, ttfb 1078 ms | 5 MB/s, ttfb 1201 ms | 31 MB/s, ttfb 124 ms | 8 MB/s, ttfb 310 ms | 10 MB/s, ttfb 214 ms |
| single_put_64mb | 38 MB/s | 52 MB/s | 64 MB/s | 23 MB/s | 31 MB/s | 16 MB/s | 54 MB/s | — | 15 MB/s | 18 MB/s | 12 MB/s | 28 MB/s | 30 MB/s | 16 MB/s |
| get_single_64mb | 40 MB/s, ttfb 69 ms | 56 MB/s, ttfb 72 ms | 52 MB/s, ttfb 69 ms | 30 MB/s, ttfb 129 ms | 28 MB/s, ttfb 163 ms | 20 MB/s, ttfb 97 ms | 21 MB/s, ttfb 194 ms | — | 30 MB/s, ttfb 1389 ms | 25 MB/s, ttfb 1789 ms | 24 MB/s, ttfb 1350 ms | 75 MB/s, ttfb 107 ms | 37 MB/s, ttfb 289 ms | 32 MB/s, ttfb 247 ms |
| single_put_256mb (chunked) | 128 MB/s | n/a (>100 MB via CF) | 321 MB/s | — | — | 57 MB/s (whole) | — | — | 24 MB/s (whole) | — | — | — | — | — |
| get_single_256mb | 124 MB/s, ttfb 519 ms | — | 123 MB/s, ttfb 337 ms | — | — | 21 MB/s, ttfb 103 ms | — | — | 30 MB/s, ttfb 2771 ms | — | — | — | — | — |
| mpu_put_256mb (whole) | 132 MB/s | 76 MB/s | 128 MB/s | 29 MB/s | 26 MB/s | 63 MB/s | 30 MB/s | — | 19 MB/s | 23 MB/s | — | 45 MB/s | 34 MB/s | — |
| get_mpu_256mb | 57 MB/s, ttfb 63 ms | 54 MB/s, ttfb 387 ms | 37 MB/s, ttfb 92 ms | 44 MB/s, ttfb 145 ms | 24 MB/s, ttfb 176 ms | 21 MB/s, ttfb 68 ms | 27 MB/s, ttfb 200 ms | — | 42 MB/s, ttfb 1351 ms | 32 MB/s, ttfb 1421 ms | — | 82 MB/s, ttfb 184 ms | 36 MB/s, ttfb 319 ms | — |
| mpu_put_1024mb | 241 MB/s | 183 MB/s | 256 MB/s | — | — | — | — | — | 1st: 400 (finding 2); then 47, 47, 31 MB/s | — | — | — | — | — |
| get_mpu_1024mb | 38 MB/s, ttfb 109 ms | 70 MB/s, ttfb 195 ms | 49 MB/s, ttfb 73 ms | — | — | — | — | — | 44 MB/s, ttfb 2.9 s (striped, 4 pieces) | — | — | — | — | — |
| range_get_1mb | 65 / 80 ms | 248 / 664 ms | 68 / 79 ms | 148 / 157 ms | 184 / 210 ms | 284 / 379 ms | 380 / 487 ms | — | 915 / 990 ms | 954 / 1554 ms | 950 / 1027 ms | 75 / 90 ms | 263 / 309 ms | 158 / 196 ms |
| cdn_get_8mb (MISS / HIT) | | | | | | | | | | | | 77.9 s / 0.04 s (207 MB/s, ttfb 11 ms) | | 52.2 s / 0.42 s (20 MB/s, ttfb 36 ms) |
| cdn_get_64mb (MISS / HIT) | | | | | | | | | | | | 1.5 s / 0.23 s (297 MB/s, ttfb 12 ms) | | 2.5 s / 1.16 s (58 MB/s, ttfb 34 ms) |

Reading it:

- **HEAD is the head cache** (<1 ms on the box, 66 ms from home = the RTT) on every
  tier except Vault, where it is a Geyser call (32 ms). LIST is 1–7 ms.
- **Small writes cost the backend's PUT latency:** Lyve ~70–120 ms, Geyser ~50 ms,
  R2 ~200 ms, Sync ~1.2–2.5 s (several Sync API calls per file). From home add the
  66 ms RTT once.
- **Cloudflare in front of private S3 adds 20–60 ms per operation from a real
  client** (Mac: HEAD 66→85 ms, GET 129→153 ms) and gains nothing on throughput;
  the SLC→Cloudflare numbers (range p50 65→248 ms, bench-compare 5–10× slower) are
  the box's own path to the PHX/SLC colos and do not represent customers. Single
  PUTs above 100 MB are refused by the zone plan (413), multipart is fine.
- **Single-stream throughput per tier from the box:** Standard whole-object 38–57
  MB/s read / 38–132 write (multipart 16 × 16 MiB 8-wide 132–256 MB/s); chunked 124
  read / 128 write. Vault 20–27 read / 57–63 write. Sync 30–44 read (TTFB 1.4–2.9
  s) / 15–47 write. Public (R2) 31–82 read / 5–45 write.
- **From home** everything is link-bound (23–44 MB/s) — the box is not the limit for
  a residential customer; a VPS customer in another DC would see something between
  the two columns, which is the vantage still missing.

## 3. Raw backend vs Vaultaire (bench-compare, same box, same hour)

| workload | Lyve raw (us-west-1) | via s3.stored.ge | via stored.ge (from SLC) |
|---|---|---|---|
| warm_put_4kb p50 | 277 ms (4 ops/s) | 66 ms (12 ops/s) | 213 ms |
| warm_get_4kb p50 | 47 ms | 60 ms | 197 ms |
| warm_head_4kb | 43 ms | **0.3 ms (3,250 ops/s)** | 129 ms |
| list_100 | 136 ms | **2 ms** | 306 ms |
| large_put_64mb | 105 MB/s | 38 MB/s | 19 MB/s |
| large_get_64mb | 135 MB/s | 43 MB/s | 36 MB/s |
| multipart_put_256mb_16p | 232 MB/s | 188 MB/s | 72 MB/s |
| concurrent_ingest_20s | 748 MB/s, p99 420 ms | **103 MB/s, p99 2.4 s** | 171 MB/s, p99 1.9 s |
| concurrent_download_20s | 459 MB/s | 133 MB/s, p99 2.5 s | 137 MB/s |
| burst_small_files_500 | 218 ops/s | 128 ops/s | 50 ops/s |
| sustained_upload_60s | 610 MB/s | 336 MB/s | 81 MB/s |
| consistency (RAW / overwrite / list) | 20/20, 10/10, 10/10 | 20/20, 10/10, 10/10 | 20/20, 10/10, 10/10 |

Lyve raw small-op latency swings run to run (today's 277 ms PUT p50 vs 66 ms through
Vaultaire to the same backend is that swing, not a Vaultaire speed-up; the 10-04 note
"small-op workloads swing ±77 %" stands). What is stable: HEAD/LIST are 100× faster
through Vaultaire (the head cache), single-stream large transfers through Vaultaire
run at 35–60 % of raw, multipart at 80 %, and concurrency fans out poorly (finding 4).

**Load gate (origin, `tests/load`): 5/5 PASS** — ConcurrentPut 170 MB/s p99 616 ms;
ConcurrentGet 109 MB/s p99 119 ms; Multipart 50 × 100 MB 227 MB/s; Mixed 150 MB/s
p99 196 ms; ManagementBurst 40/50 429s, 0 5xx; goroutine growth ≤ 3.
**Conformance (`validate`, origin): 22/22 PASS** (multipart 100 MB 14.6 s, ranges,
presigned PUT/GET, versioning, tagging, rclone, dedup).

## 4. A VPS on top: JuiceFS, restic, objbench (Standard tier, from the box)

| | JuiceFS over Standard | JuiceFS over Sync |
|---|---|---|
| `juicefs bench` 1 thread: big write / big read | 452 MiB/s (write-back cache) / 46 MiB/s | 46 / 23 MiB/s |
| 4 threads: big write / big read | 635 / 161 MiB/s | 63 / 55 MiB/s |
| small file write (128 KiB) 1 / 4 threads | 14 / 42 files/s (71–95 ms each) | 0.4 / 3.5 files/s (1.2–2.3 s each) |
| small read / stat (cached metadata) | 5,000–12,900 / 29,000–90,000 files/s | same |
| cold sequential read 1 GiB (cache dropped) | 80 MB/s | 11 MB/s |
| warm read (local cache) | 17 GB/s | 15 GB/s |
| 200 × 10 KiB create + sync | 15 s | 413 s |
| 4 KiB write + fsync | 71 ms | 3.7 s |
| umount flush of 1 GiB | 2.1 s | 2.1 s |

JuiceFS writes 4 MiB blocks, so nothing is ever chunked and every block is one PUT —
the filesystem's speed is the tier's per-object PUT latency × its parallelism. On
Standard that is a usable general-purpose network disk for a VPS (media libraries,
Nextcloud/Immich data dirs, build artefacts, logs) as long as the hot working set
fits the local cache and nothing fsyncs per record (databases, mail spools, SQLite
stay on the VPS's own disk). Over Sync it is not a filesystem.

`juicefs objbench` on Standard (4 threads, 4 MiB blocks): upload 103 MiB/s, download
95 MiB/s, small objects put 52/s, get 59/s, delete 60/s, head 7,140/s, list 157,000/s.

**restic over Standard** (1 GiB file + 2,000 × 10 KiB + 200 × 1 MiB, S3 repo at the
origin): backup 17.2 s (≈ 70 MB/s), no-change incremental 1.3 s, restore of 2,205
files 6.1 s, byte-identical, `check` 1.3 s. A VPS backup target works out of the box
(restic/rclone/Borg via rclone all speak S3 to `s3.stored.ge`).

## 5. The Vault second copy, measured as a layout

`erasure-bench`, 256 MiB, RS 4+4, data on geyser, parity on the five Sync bridges —
the shape `vault_parity` writes: PUT all 8 shards 9.6 s (27 MB/s payload; Geyser
slowest 6.9 s, Sync 9.6 s); READ any-4 2.3 s (109 MB/s); **lose Geyser → rebuilt
from the four Sync parity shards in 2.3 s (112 MB/s)**; lose Sync → 3.0 s from Geyser.
And live: the four archive objects of §2 got `vault_parity` rows `complete` on
`{sync,sync,sync,sync}` 5, 6, 22 and 20 s after their head rows — the second copy is
real and a Geyser outage would be served from it.

## 6. What the layers buy, with these numbers

**A VPS + Stored (what a customer can run today).**
- Backup target: restic/rclone/Borg → `s3.stored.ge`, Standard tier: 70 MB/s in,
  200 MB/s restore, strong consistency, versioning + Object Lock available. Vault
  tier for the cold copies at 57 MB/s in; reads 20 MB/s from the landing zone, a
  restore wait once on tape.
- Network disk: JuiceFS (or rclone mount) over a Standard bucket: 80 MB/s cold,
  450+ MB/s cached writes, 14–42 small files/s. Put its metadata DB on the VPS;
  keep databases local.
- App storage: any S3 SDK; HEAD/LIST are ~1 ms at the origin (head cache), so
  metadata-heavy apps (galleries, sync clients) are far cheaper than on a raw
  vendor (43–136 ms).
- What to tell them: use `s3.stored.ge` for bulk and uploads (no 100 MB cap, ~20 ms
  less per op), `stored.ge` for browser traffic and presigned links; multipart
  with 16 MiB parts, 8 wide, for anything over 64 MiB; large single PUTs are
  chunked and (today) read back faster than multipart uploads (finding 1).

**+ Cloudflare.**
- Public buckets: `/cdn/<slug>/<bucket>/<key>` is a real CDN once warm — 11–36 ms
  TTFB, 58–300 MB/s hits, zero origin egress on hits, works for immutable keys with
  an extension. The miss is the problem (finding 3): erratic to 78 s; pre-warm with
  the DO copier (5 GiB in 39 s, 10-09) or Sippy (< 512 MiB, 10-09) for anything a
  visitor must not wait for, and set a cache rule so extension-less keys cache.
- Private S3 through the proxy: +20–60 ms per op, origin IP hidden, DDoS/WAF/rate
  limits, the 100 MB single-PUT cap. No speed-up. Uploads via the proxy are fine
  since the h2 fix (183 MB/s multipart 1 GiB from the box).
- Reads of private objects at the edge: Workers Cache with per-user partitions
  (10-05), DO puller instead of Sippy for big objects (10-09) — measured in the
  Cloudflare log, not repeated today.
- Workers/Containers next to it (Edge Box, Ente, EmDash): all measured 10-06/10-09;
  Stored is their S3 and the numbers in §2 are what they see from the edge, minus
  the home-uplink cap.

**+ JuiceFS as the product (a "network drive" tier).** Possible today on Standard
with the numbers in §4; the two things that would make it a product are a managed
metadata service (JuiceFS needs Redis/Postgres/TiKV per filesystem; sqlite on the
VPS is single-client) and the per-object PUT latency floor (70–120 ms on Lyve, so
14–42 small files/s per client). With iDrive back as the primary the 10-04 raw
numbers (31 ms small ops) would roughly triple the small-file rate.

## 7. Open after today

- Finding 1 (parallel_get on lyve) and finding 2 (unlogged 400 on complete; sync
  refusals of generated names) are code; finding 3 needs the zone A/B (owner);
  finding 4 needs a profile of the PUT path under concurrency.
- Archive restore timing: keep the hourly probe; the 10-04 objects hit ~13 days on
  2026-10-17.
- A second-datacenter client (any $5 VPS) for the real customer column.
- iDrive: every key still object-dead; Standard = Performance = Lyve until repaired.

**Cleanup:** every bucket created today deleted (tb-std/arch/sync/pub/jfs-1012,
load-1012-origin, validate-1012 + its versioned twin); the bench tenant holds only
what it held before (sync-lab, sync-pub, tier-archive-20261004, vault-bench-20261004,
upcap-20261004, the three pre-existing versioned validate buckets, vbench-*). restic
0.19.1 installed at `~/bin/restic` on SLC; results in
`~/vaultaire-bench/results-20261012/` and `raw-2026-10-12/` here.
