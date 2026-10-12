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
4. ~~The concurrent-ingest tail on the origin is Vaultaire's~~ — **withdrawn the same
   day, see §8.2: it is the bench client's HTTP/2 transport; the origin ingests at
   614–859 MB/s with p99 0.7 s from a plain HTTP/1.1 client.** The original text: 20 s of
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
  refusals of generated names) are code → PR #693, same day (§8.7); finding 3
  needs the zone A/B (owner); finding 4 is withdrawn (§8.2).
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

## 8. Second round, same day: "can we do all?"

Everything in §7 that did not need the owner, a second datacenter or the tape
clock was run between 17:50 and 18:40 UTC. Prod moved twice meanwhile
(`c74f70e` at 15:56, `3aece84` at 18:01 — both copy/docs-and-WP-R8-1 builds, no
data-path change); which build a number is from is stated where it matters.
Raw lines: `raw-2026-10-12/round2-*` (the SLC run log, the Mac tools and apps logs, ingest JSON lines from SLC, the Mac and the local instance, the smoke run, the presigned transcript, the local pprof top).

### 8.1 Smoke on the new build (`c74f70e`, Standard, origin, from SLC)

4 KiB PUT p50 76 ms (p95 494), GET 57, HEAD 0, DELETE 65; 256 MiB single PUT
214 MB/s, its GET 94 MB/s (chunked); multipart 256 MiB 124 MB/s up / 64 down;
multipart 1 GiB 227 MB/s up / **46.5 MB/s down** (the finding-1 shape, still
there before #693); 1 MiB ranges p50 67 ms; 0 mismatches. Same picture as the
morning, slightly better on the small PUT.

### 8.2 The concurrent-ingest tail is the bench client's, not Vaultaire's (finding 4 retracted)

`cmd/tools/ingestbench` (new: N workers × fixed-size PUTs, per-PUT latency) with
a plain HTTP/1.1 client, bench tenant, 4 MiB objects, 1 GiB per run:

| path (from SLC) | workers | MB/s | ops/s | p50 | p99 |
|---|---|---|---|---|---|
| origin `s3.stored.ge` (HAProxy + TLS) | 2 | 72 | 17 | 101 ms | 334 ms |
| origin | 8 | 318 | 76 | 94 | 302 |
| origin | 32 | **614** | 147 | 108 | **705** |
| origin | 64 | **859** | 205 | 212 | 709 |
| loopback `:8001` | 8 | 282 | 67 | 97 | 433 |
| loopback | 32 | 687 | 164 | 113 | 668 |
| loopback | 64 | 887 | 212 | 202 | 689 |
| origin, 64 KiB objects | 32 | 29 | **441** | 66 | 200 |
| origin, 32 workers, **HTTP/2 allowed** (one connection, 32 streams) | 32 | **114** | 27 | 1277 | **2307** |
| origin, HTTP/1.1 again, same minute | 32 | 791 | 189 | 134 | 407 |
| bench-compare `concurrent_ingest_20s`, same minute | 32 | 94 | 24 | 1304 | 2559 |
| origin, HTTP/2, 1 / 4 / 16 workers | 1 / 4 / 16 | 32 / 111 / 168 | | 122 / 120 / 417 | 186 / 461 / 990 |
| `stored.ge` (Cloudflare), HTTP/2 / HTTP/1.1 | 32 | 181 / 321 | | 631 / 274 | 972 / 976 |

So the origin ingests 4 MiB objects at 600–860 MB/s with a p99 of 0.7 s and
441 small PUTs/s, and HAProxy + TLS cost nothing measurable against loopback.
The 103 MB/s / p99 2.4 s of §3 came from bench-compare's own client (its
transport allows HTTP/2: one connection, 32 streams, one flow-control window —
the same shape the 10-04 report blamed on iDrive). Run back to back with the same tool: HTTP/2 allowed 114 MB/s p99 2.3 s,
HTTP/1.1 791 MB/s p99 0.4 s, bench-compare in between 94 MB/s p99 2.6 s. The
h2 path is HAProxy's per-stream flow control (`tune.h2.initial-window-size
262144`, set on 2026-10-04 for the Cloudflare→origin hop): one stream from the
box does 32 MB/s, 4 streams 111, 16 streams 168, and 32 streams on one
connection fall back to 114. From the Mac at 66 ms the origin barely cares
(1 stream 8.5 vs 8.2 MB/s, 8 streams 28 vs 33) but the Cloudflare host does:
8 streams 13.8 MB/s with two 21 s stalls on h2 vs 51 MB/s on HTTP/1.1. Go
clients multiplex every request onto one h2 connection per host, and rclone
and restic leave HTTP/2 on by default — so this is a real customer shape for
many-small-files uploads, and the fix is an HAProxy knob (a larger
`tune.h2.fe.initial-window-size`, HAProxy 3.2 has the frontend-only form) plus
a docs line ("`--disable-http2` for bulk uploads"), not a Vaultaire change. The
morning's finding 4 is withdrawn: there is no per-PUT serialisation to profile
on prod.

**The local profile agrees.** The product binary with `net/http/pprof` patched
in (never committed), local driver, a private migrated DB, two freshly
registered tenants, 32 workers × 4 MiB: CPU is 22 % of one core over the run,
three quarters of it the disk `write(2)` and 12 % the MD5 of the ETag;
`reserveQuota` (one `SELECT … FOR UPDATE` on the tenant's quota row per PUT)
held no goroutine at the sampled instant and the head-row upsert held 6 of 30.
Per-PUT service time is ~90 ms at 8 workers (p99 177 ms, 369 MB/s on the Mac's
SSD); more workers only queue (32: p50 378 ms, 64: p50 709 ms, same MB/s).

### 8.3 Two tenants at once: shared capacity, no cross-tenant lock

Prod has no second key I may use (signups are closed; `bench2`'s secret is not
stored — `api_keys.secret_key` is empty and the `tenants` mirror is stale; a
mint is the owner's), so this ran on the local instance above: tenants A and B
each with 32 workers get 167 + 167 MB/s where A alone got 306–356, each at the
p50 of a 64-worker solo run (747 vs 709 ms); with 16 + 16 workers 183 + 182 MB/s
at p99 0.75–0.8 s. The box is one pool; a tenant's quota row lock never shows
up. For the prod column with a real second tenant the same two commands run
from `~/vaultaire-bench` the moment a key exists.

### 8.4 Striped Sync objects: every range is right, each costs 1.3–3.3 s to first byte

1 GiB multipart to `sync-lab` (4 × 256 MiB pieces on the bridges, complete
25 s), then `GetObject` with `Range` through the origin, bytes compared to the
source:

| range | bytes | time | result |
|---|---|---|---|
| first MiB of piece 0 | 1 MiB | 4.9 s | ok |
| 2 bytes across the 256 MiB seam | 2 | 3.3 s | ok |
| first MiB of piece 1 | 1 MiB | 1.9 s | ok |
| 1 MiB either side of the seam | 2 MiB | 1.5 s | ok |
| 2 bytes across the 512 MiB and 768 MiB seams | 2 + 2 | 1.6 / 3.4 s | ok |
| 200 MiB → 600 MiB (three pieces) | 400 MiB | 9.8 s (43 MB/s) | ok |
| last byte, last MiB, `bytes=-1048576` | 1 / 1 MiB / 1 MiB | 3.2 / 1.3 / 1.6 s | ok |
| past the end | — | — | 416 RangeNotSatisfiable |
| whole object | 1 GiB | 8.9 s (121 MB/s) | ok |

`Content-Range` is exact on every answer. The time is the bridge's open of a
piece, not Vaultaire: a 2-byte read pays the same 1.6–3.3 s as a megabyte.

### 8.5 Presigned browser uploads: both hosts, with the one difference known

A boto3 "backend" set `?cors` (one origin, PUT/GET/HEAD, ExposeHeaders ETag) and
signed URLs; a curl "browser" sent the preflight and the uploads with `Origin`:

| step | `stored.ge` (Cloudflare) | `s3.stored.ge` (origin) |
|---|---|---|
| preflight PUT, allowed origin | 200, rules echoed, max-age 3000 | 200, same |
| preflight, foreign origin | 403 | 403 |
| presigned single PUT 8 MiB | 200, 1.0 s | 200, 1.0 s |
| presigned single PUT 150 MiB | **413** in 0.3 s | 200, 4.5 s |
| presigned UploadPart × 3 (16 MiB) | 200, **ETag exposed** | 200, ETag exposed |
| CompleteMultipartUpload (backend) | ok, 4.5 s total | ok, 5.1 s |
| presigned GET 48 MiB with Origin | 200, bytes equal | 200, bytes equal |

A browser can do single PUTs up to 100 MB and multipart of any size through
`stored.ge`, and anything through `s3.stored.ge`. Nothing to fix; the docs'
"multipart above 100 MB on the Cloudflare host" rule is confirmed.

### 8.6 Customer tools nobody had pointed at Stored

All from the Mac (66 ms RTT, 25–40 MB/s link) unless noted; dataset 500 small
files (4–256 KiB) + 3 × 32 MiB, 161 MiB.

| tool | result |
|---|---|
| **rclone 1.71** sync / re-sync no-op / check / copy down / delete, origin and `stored.ge` | all pass, 503 files, byte-identical; sync 11 s, no-op 3 s, check 1 s, download 7 s |
| **rclone mount 1.60** on the box, `--vfs-cache-mode writes` | 256 MiB write lands in S3 8.9 s after `close`; 200 × 10 KiB created in 0.2 s, all 200 in S3 after 10 s; cold read 256 MiB 69 MB/s, sha equal; 200 small reads 13 s (66 ms each, the HEAD+GET pair); 10 MiB at +100 MiB 0.73 s |
| **s5cmd 2.3** cp 500 small (32 workers) / 3 × 32 MiB / ls / cp down / rm | all pass, byte-identical; 4 s / 4 s / 1 s / 5 s / 3 s |
| **mc** (MinIO client) mirror / stat / ls -r / mirror down / rm | all pass, byte-identical; 12 s / 1 s / 7 s / 4 s |
| **Duplicati** (docker, `s3-client=aws`, path-style, **`use-ssl=true`**) | backup 256 MiB + 300 files 28 s, incremental 4 s, `test all` with full remote verification 11 s "no errors", restore 301 files 12 s byte-identical; 16 keys, 274 MB |
| Duplicati with its default (plain HTTP) | fails: HAProxy answers the http listing with a 302 to https and the AWS SDK reports "Error Code Found" — a docs line ("tick Use SSL") |
| **Nextcloud 31** `files_external` amazons3, path-style, region us-east-1 | mount verifies; WebDAV PUT 32 MiB 15 s, GET 2 s byte-identical; **100 × 8 KiB through Nextcloud 65–118 s** (0.65–1.2 s per file — Nextcloud's own stat/write/stat per file over S3, 102 keys landed) |
| **Cyberduck 9.5** (`duck`) | list buckets works; **every bucket-scoped operation fails "Cannot read bucket location"** — `GET ?location` answers the bucket's region column (`us-central-1`, an iDrive id) in the body and `x-amz-bucket-region`, and Cyberduck refuses a region it cannot map (`--region` does not override it) |

The Cyberduck one is a product decision: answer the SigV4 signing region
(`us-east-1`, what every other S3-compatible answers) on `?location` and keep
the placement region on the dashboard / a Stored header, or keep the truthful
answer and lose Cyberduck, Mountain Duck and anything on their S3 library.
Recommendation: the former — nothing signs against `us-central-1`, and the
bucket's placement is already in the dashboard and the management API.

### 8.7 Lyve parallel GET after #693

`a77f7d5` went live at 18:22 UTC (the merge queue deployed it 19 minutes after
the PR opened); the bench tenant already had `parallel_get` ON. Same tierbench
shape as §8.1, three vantages, within the next four minutes:

| whole-object GET (Lyve, bench tenant) | before #693 (§8.1 / §2) | after, origin | after, loopback | after, Cloudflare |
|---|---|---|---|---|
| multipart 1 GiB | 38–46 MB/s | **135 MB/s** (7.9 s, TTFB 0.95 s) | 86 MB/s | **93 MB/s** |
| multipart 256 MiB | 57–64 MB/s | **99 MB/s** | 107 MB/s | 81 MB/s |
| single 256 MiB (chunked path, unchanged) | 94–124 MB/s | 82 MB/s | 91 MB/s | — |
| 1 MiB ranges of the 1 GiB object, p50 | 67 ms | 119 ms | 107 ms | 164 ms |

`vaultaire_large_get_streams_total{path="ranged",outcome="ok"}` went 0 → 7 with
0 errors and 0 fallbacks; the server-side throughput histogram puts the seven
streams at 64–150 MB/s (mean 95). Finding 1 is closed: a multipart upload on the
Standard tier now reads back at the chunked path's speed or better, ~3× what the
morning measured. The 1 MiB ranges got ~50 ms slower in this run (119 vs 67 ms
p50) — one run, the box's Lyve path at that minute; worth a glance in the next
bench rather than a change now. One side note from the Cloudflare pass: a
256 MiB *single* PUT through `stored.ge` answered **502**, not 413, in this run
(the morning's 256 MiB single via CF was skipped by `-max-single-mb 100`); the
100 MB rule stands either way.

### 8.8 What changed in the open list

- Finding 1 and 2: PR #693 (`GetRangeInfo` on Lyve; a bridge's 400 on a
  generated name is retried, then 503 + Retry-After, and every `ErrInvalidInput`
  the complete/put handlers answer is logged with bucket, key, upload id).
- Finding 4: withdrawn (§8.2). bench-compare's transport should disable HTTP/2
  like tierbench and ingestbench do, or its ingest numbers stay a client
  artefact.
- New: GetBucketLocation vs Cyberduck (§8.6); Duplicati's "Use SSL" docs line;
  the Nextcloud per-file cost is Nextcloud's, worth a sentence in the docs.
- Still open: the zone A/B (owner), a real second tenant on prod (a key),
  the second-datacenter client, the tape clock (2026-10-17), iDrive.

**Cleanup:** `tb-smoke-1012`, `tb-ingest-1012`, `tb-ingest-1012c`,
`tb-presign-1012`, `tb-tools-1012`, `tb-duck-1012`, `tb-nc-1012`, `tb-dup-1012`
and `sync-lab/range-1012/` deleted; the local profiling database dropped and the
patched binary discarded; the Nextcloud and Duplicati containers removed; the
`claude+contention` sign-up never happened (403, signups closed).
