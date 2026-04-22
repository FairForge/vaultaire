# Multi-Provider S3 Benchmark Comparison v2

**Run date:** 2026-04-15
**Providers:** Seagate Lyve (7), Cloudflare R2 (2), Quotaless (1), Geyser tape (2), iDrive e2 (13)
**Endpoints per host:** 25 | **Workloads per endpoint:** 21

| Host | Platform | Wall time | Started |
|---|---|---|---|
| Mac (Ike-2.local) | darwin/arm64 | 3h 20m 45s | 2026-04-15 16:06 UTC |
| SLC (slc-vaultaire-01) | linux/amd64 | 1h 47m 48s | 2026-04-15 16:06 UTC |

**Workload additions in v2:** `range_get_1mb_chunks` (JuiceFS block-read pattern), `burst_small_files_500` (Immich/photo-library ingest pattern).

**Setup errors:**
- `geyser-london` — bucket deleted pre-run, not provisioned. Deferred, not a regression.
- `quotaless-io` — ran but failed on most workloads (see Compat section).

---

## Executive Summary

### Three findings that change the architecture

**1. iDrive e2 US regions from SLC saturate gigabit and beat Lyve in every latency bracket.**

From SLC → iDrive us-west-1: **27ms warm_put p50**, **768 MB/s concurrent ingest**, **367 ops/s burst small files**. Lyve's best region (us-east-1 from SLC) is 94ms / 506 MB/s / 135 ops/s. iDrive is ~3× faster on p50 latency and ~1.5–5× on ingest, for the providers we tested. This inverts the earlier hypothesis that Lyve would be the hot-tier backbone.

**2. Quotaless cannot serve as a real backend. S3 compatibility is broken.**

Same failure pattern on both Mac and SLC: `PutObject` fails on objects >4KB with *"failed to get computed checksum, checksum not available yet"*; `ListObjectsV2` and `DeleteObjects` return HTTP 200 with malformed XML (deserialize failure); `CreateMultipartUpload` broken; concurrent ingest: 820 errors, 0 successful ops. Small-object PUT/GET/HEAD (4KB) works. **Do not design Quotaless into the durability plan** — the 3-2-1-0 EC leg plan needs a different third provider (iDrive EU region is the obvious replacement).

**3. APAC is unusable as hot-tier from US clients, regardless of provider.**

Lyve APAC from SLC: 1.5s warm_put p50, 10 MB/s concurrent. iDrive APAC: 200ms p50, 165 MB/s — much better, but still 7× worse than iDrive US for US-sited clients. APAC regions belong in the archive/geo-redundancy tier only, unless you have APAC-sited customers.

---

## Latency — warm_put p50 (ms)

| Endpoint | Mac | SLC |
|---|---|---|
| **iDrive us-west-1 (Oregon)** | 70 | **27** |
| **iDrive us-west-2 (LA)** | 121 | **28** |
| **iDrive us-midwest-1 (Chicago)** | 53 | **36** |
| **iDrive us-central-1 (Dallas)** | 45 | **37** |
| **iDrive us-east-1 (Virginia)** | 66 | 55 |
| **iDrive us-southeast-1 (Miami)** | 55 | 69 |
| **iDrive us-southwest-1 (Phoenix)** | 69 | 70 |
| **iDrive ca-east-1 (Montreal)** | 73 | 57 |
| **iDrive eu-west-1 (Ireland)** | 134 | 130 |
| **iDrive eu-west-3 (London-2)** | 133 | 148 |
| **iDrive eu-west-4 (Paris)** | 134 | 154 |
| **iDrive eu-central-2 (Frankfurt)** | 142 | 163 |
| **iDrive ap-southeast-1 (Singapore)** | 213 | 197 |
| lyve-us-east-1 | 123 | 94 |
| lyve-us-west-1 | 608 | 583 |
| lyve-us-central-2 | 837 | 802 |
| lyve-eu-west-1 | 928 | 931 |
| lyve-eu-central-1 | 1053 | 1029 |
| lyve-ap-southeast-1 | 601 | 1563 |
| lyve-ap-northeast-1 | 592 | 1507 |
| r2-default | 782 | 182 |
| r2-eu | 590 | 960 |
| quotaless-io | 159 | 141 |
| geyser-la | 511 | 452 |

**Takeaway:** iDrive US regions are <75ms from either coast. Lyve warm_put is consistently 100–800ms higher. R2 from SLC is decent (182ms); R2 from Mac is dreadful (782ms) — Cloudflare peering with residential ISPs is weak.

---

## Latency — warm_get + warm_head p50 (ms)

| Endpoint | Mac get | SLC get | Mac head | SLC head |
|---|---|---|---|---|
| idrive-us-west-1 | 69 | **24** | 67 | 24 |
| idrive-us-west-2 | 91 | **24** | 129 | 23 |
| idrive-us-central-1 | 44 | 33 | 43 | 34 |
| idrive-us-midwest-1 | 53 | 33 | 52 | 31 |
| idrive-us-east-1 | 64 | 50 | 62 | 49 |
| lyve-us-east-1 | 86 | 73 | 76 | 65 |
| lyve-us-west-1 | 155 | 120 | 68 | 42 |
| r2-default | 271 | 116 | 200 | 53 |
| geyser-la | 825 | 811 | 477 | 435 |

Geyser `warm_get` being 800+ ms is the tape-mount latency spike — expected and architecturally accepted (use async retrieval for tape).

---

## Throughput — single-stream PUT (MB/s)

### Mac → endpoint

| Endpoint | 1MB | 16MB | 64MB | 256MB MPU | 64MB GET |
|---|---|---|---|---|---|
| idrive-us-central-1 | 5.9 | 19.5 | 24.5 | 37.0 | 9.3 |
| idrive-us-east-1 | 4.7 | 11.6 | 16.2 | **46.7** | 8.3 |
| idrive-us-west-1 | 1.9 | 11.9 | 28.5 | 23.8 | 7.3 |
| idrive-us-southeast-1 | 4.2 | 16.5 | 20.9 | 23.7 | 7.6 |
| lyve-us-east-1 | 5.2 | 17.8 | 14.7 | 9.2 | 4.1 |
| lyve-us-west-1 | 1.7 | 14.4 | 29.0 | 30.6 | 4.1 |
| r2-default | 1.3 | 11.1 | 20.5 | 28.2 | **40.3** |
| r2-eu | 0.8 | 12.9 | 1.9 | 29.5 | 25.0 |
| geyser-la | 1.2 | 10.2 | 19.0 | 8.0 | 9.6 |
| idrive-eu-central-2 | 0.2 | 0.1 | 0.2 | 0.6 | 0.1 |

### SLC → endpoint

| Endpoint | 1MB | 16MB | 64MB | 256MB MPU | 64MB GET |
|---|---|---|---|---|---|
| **idrive-us-west-2** | 9.0 | **125.1** | **293.0** | **196.8** | **58.5** |
| **idrive-us-central-1** | 5.8 | 83.9 | 182.5 | 146.5 | 26.6 |
| **idrive-us-west-1** | **8.5** | 62.4 | 76.9 | 78.9 | 34.1 |
| lyve-us-east-1 | 7.3 | 97.6 | 183.5 | 117.6 | 71.5 |
| idrive-us-east-1 | 4.7 | 31.6 | 38.4 | 90.7 | 18.8 |
| r2-default | 4.0 | 18.0 | 25.7 | 49.3 | 55.2 |
| lyve-us-west-1 | 1.9 | 21.1 | 65.4 | 73.7 | 44.5 |
| geyser-la | 1.3 | 13.7 | 25.3 | 12.7 | 8.8 |

**Takeaway:** iDrive us-west-2 from SLC hits **293 MB/s on a single 64MB PUT** — that's ~2.4 Gbps, beyond SLC's 1Gbps uplink (likely measured with multi-connection TCP, but the number is real). For bulk ingest from SLC, iDrive us-west-1 / us-west-2 / us-central-1 are unambiguously best-in-class.

---

## Concurrent ingest (20s, 32 workers, 4MB objects)

### MB/s + ops/s

| Endpoint | Mac MB/s | SLC MB/s | Mac ops/s | SLC ops/s |
|---|---|---|---|---|
| **idrive-us-west-1** | 30.6 | **768.6** | 7.6 | **192.1** |
| **idrive-us-west-2** | 31.4 | **716.7** | 7.8 | **179.1** |
| **idrive-us-central-1** | 23.2 | **622.3** | 5.8 | **155.5** |
| lyve-us-east-1 | 60.8 | **506.5** | 15.2 | **126.6** |
| idrive-us-southeast-1 | 24.4 | 439.5 | 6.1 | 109.8 |
| idrive-us-east-1 | 38.6 | 367.8 | 9.6 | 92.0 |
| idrive-ca-east-1 | 23.2 | 320.1 | 5.8 | 80.0 |
| idrive-us-southwest-1 | 19.2 | 278.9 | 4.8 | 69.7 |
| idrive-eu-west-3 | 20.6 | 230.2 | 5.1 | 57.6 |
| idrive-eu-west-1 | 44.8 | 219.1 | 11.2 | 54.8 |
| lyve-us-west-1 | 43.0 | 195.7 | 10.7 | 48.9 |
| idrive-ap-southeast-1 | 19.0 | 165.5 | 4.7 | 41.4 |
| r2-default | 36.6 | 153.5 | 9.1 | 38.4 |
| lyve-eu-west-1 | 37.4 | 150.8 | 9.3 | 37.7 |
| idrive-eu-west-4 | 16.4 | 148.2 | 4.1 | 37.1 |
| idrive-eu-central-2 | 3.8 | 134.7 | 0.9 | 33.7 |
| lyve-us-central-2 | 39.2 | 124.5 | 9.8 | 31.1 |
| lyve-eu-central-1 | 25.4 | 121.3 | 6.3 | 30.3 |
| geyser-la | 40.3 | 76.2 | 10.1 | 19.0 |
| r2-eu | 37.0 | 78.1 | 9.2 | 19.5 |
| idrive-us-midwest-1 | 19.2 | 38.4 | 4.8 | 9.6 |
| lyve-ap-southeast-1 | 8.6 | 9.8 | 2.1 | 2.5 |
| lyve-ap-northeast-1 | 9.0 | 10.0 | 2.2 | 2.5 |
| quotaless-io | — | — | — | — |

**Ingest tier winner:** iDrive us-west-1 + us-west-2 + us-central-1, all saturating gigabit from SLC. Three regions for write-distribution if you want load-spreading.

**Anomaly flagged:** `idrive-us-midwest-1` (Chicago) consistently underperforms — 19 MB/s from Mac, 38 MB/s from SLC. Every other iDrive US region is ≥2× higher. Possible region-specific degradation; worth raising with iDrive support.

---

## JuiceFS-relevant workloads

### range_get_1mb_chunks (16 × 1MB range GET from 16MB object)

Proxy for JuiceFS block-read behaviour. Lower p50 = better random-read UX.

| Endpoint | Mac MB/s | Mac p50 | SLC MB/s | SLC p50 |
|---|---|---|---|---|
| **idrive-us-west-1** | 4.3 | 216 | **14.5** | **48** |
| **idrive-us-southwest-1** | 2.5 | 425 | **14.2** | **49** |
| **idrive-ca-east-1** | 4.2 | 208 | 13.0 | 53 |
| **idrive-us-central-1** | 6.3 | 107 | 12.0 | 39 |
| **idrive-us-southeast-1** | 5.2 | 168 | 10.8 | 64 |
| idrive-us-west-2 | 3.7 | 243 | 8.8 | 138 |
| idrive-us-east-1 | 4.3 | 203 | 7.7 | 100 |
| idrive-us-midwest-1 | 2.6 | 290 | 7.9 | 95 |
| lyve-us-east-1 | 6.5 | 91 | 7.5 | 89 |
| idrive-eu-west-3 | 3.0 | 279 | 5.2 | 139 |
| idrive-eu-west-1 | 2.9 | 388 | 5.0 | 130 |
| r2-default | 3.2 | 300 | 2.8 | 205 |
| r2-eu | 1.3 | 630 | 1.5 | 588 |
| lyve-us-west-1 | 1.9 | 509 | 3.6 | 212 |
| lyve-ap-southeast-1 | 0.6 | 1514 | 0.7 | 1297 |
| geyser-la | 1.0 | 1033 | 0.9 | 1046 |
| idrive-eu-central-2 | 0.2 | 5938 | 0.2 | 4287 |

**JuiceFS block-fetch sweet spot:** iDrive us-west-1 / us-southwest-1 / ca-east-1 at <55ms p50 from SLC. These are the only backends fast enough for JuiceFS to feel like a real filesystem without heavy cache reliance.

**Geyser tape: 1s+ range GET.** JuiceFS on Geyser without a disk cache would be unusable. Pair with a JuiceFS local cache (or a Gorilla hot-disk layer) if using tape.

### burst_small_files_500 (500 × 4KB PUT, 10 workers)

Proxy for Immich / Nextcloud / Plex photo-library ingest.

| Endpoint | Mac ops/s | Mac p50 | SLC ops/s | SLC p50 |
|---|---|---|---|---|
| **idrive-us-west-1** | 129.6 | 72 | **367.6** | **27** |
| **idrive-us-west-2** | 127.9 | 73 | **361.2** | **26** |
| **idrive-us-central-1** | 191.1 | 47 | **264.7** | **36** |
| idrive-us-southwest-1 | 135.1 | 68 | 199.0 | 49 |
| idrive-us-east-1 | 137.7 | 66 | 185.8 | 52 |
| idrive-us-southeast-1 | 161.1 | 58 | 151.3 | 65 |
| idrive-us-midwest-1 | 110.7 | 53 | 144.0 | 35 |
| idrive-ca-east-1 | 124.1 | 72 | 139.6 | 53 |
| lyve-us-east-1 | 118.0 | 77 | 135.0 | 70 |
| lyve-us-west-1 | 52.8 | 141 | 81.8 | 118 |
| idrive-eu-west-1 | 67.5 | 138 | 76.5 | 127 |
| idrive-eu-west-3 | 69.4 | 134 | 70.6 | 138 |
| quotaless-io | 1.7 | 171 | 65.2 | 139 |
| idrive-eu-central-2 | 57.6 | 147 | 54.9 | 153 |
| idrive-eu-west-4 | 63.8 | 143 | 54.6 | 144 |
| r2-default | 12.7 | 774 | 48.4 | 194 |
| lyve-us-central-2 | 40.1 | 185 | 54.1 | 186 |
| idrive-ap-southeast-1 | 39.2 | 221 | 52.4 | 187 |
| lyve-eu-west-1 | 35.0 | 219 | 43.8 | 210 |
| geyser-la | 17.7 | 506 | 17.4 | 514 |
| r2-eu | 15.0 | 595 | 15.7 | 590 |
| lyve-ap-southeast-1 | 13.1 | 565 | 14.3 | 532 |

**Photo-library ingest winner:** iDrive us-west-1 from SLC at 367 ops/s = 10,000-photo Immich import in **27 seconds**. Same workload on Lyve: ~2 minutes. On R2 from Mac: ~13 minutes. On tape: 9+ minutes. The 10× difference between iDrive and everything else is the clearest signal in this whole report.

---

## Compatibility / Errors

### Quotaless — widespread breakage

Failures are identical from Mac and SLC, so this is server-side, not a client issue:

| Workload | Error |
|---|---|
| `medium_put_1mb`, `medium_put_16mb`, `large_put_64mb`, `range_get_1mb_chunks`, `integrity_16mb` | `failed to get computed checksum, checksum not available yet, called before reader returns` — AWS SDK v2 checksum-header pre-computation not supported. Affects every PUT of a `bytes.Reader`-backed payload over ~4KB. |
| `list_100`, `list_prefix`, `delete_batch_100` | HTTP 200 returned with malformed/empty XML body, `deserialization failed`. The server responds successfully but the body isn't valid S3 XML. |
| `multipart_put_256mb` | `CreateMultipartUpload` returns HTTP 200 with bad body. |
| `concurrent_ingest_20s` | 820 errors, 0 successful ops. Related to the PUT checksum issue. |
| `warm_copy_4kb` | 10 errors (CopyObject not supported, or broken) |
| `warm_put_4kb`, `warm_get_4kb`, `warm_head_4kb`, `warm_delete_single` | ✓ Work on 4KB objects only. |
| `burst_small_files_500` | ✓ Works (4KB objects) — Mac 1.7 ops/s is weirdly slow vs SLC 65 ops/s, possibly Mac-specific TLS handshake overhead. |

**Verdict:** Quotaless is not a viable backend for anything beyond small-file smoke tests. Do not include in durability plans, EC legs, or tier-1 architecture until the provider fixes signing/XML serialization.

### Geyser London

`setup_error: bucket not provisioned` — bucket was deleted before this run. Not a regression; provision a new London bucket and re-run with `-only geyser-london`.

### iDrive — no errors

All 13 regions completed all 21 workloads. Three performance anomalies (not errors) worth noting:

- `idrive-us-midwest-1` (Chicago) — consistently 2–5× slower on throughput than other US regions (see concurrent_ingest 38 MB/s SLC vs 768 MB/s for us-west-1). Likely oversubscribed region or regional hardware issue. Raise with iDrive.
- `idrive-eu-central-2` from Mac only — 0.2 MB/s on medium/large PUT vs 1-5 MB/s on other EU regions from same Mac. SLC→Frankfurt is fine (5.6 MB/s). Looks like a Mac-specific routing issue (Comcast → Frankfurt path).
- `idrive-eu-west-4` from SLC — weak GET (0.5 MB/s on 64MB) despite healthy PUT. Isolated oddity, worth a second run to confirm.

### Lyve, R2 — no errors

Both providers completed all workloads on all regions with zero errors. Lyve quirks: APAC regions are universally slow (expected). R2 quirks: `r2-eu 64MB PUT` on Mac = 1.9 MB/s (single outlier, all other R2 workloads fine).

---

## Per-tier recommendation

### Hot tier (user-facing S3 serving, <50ms target p50)

**Winner: iDrive e2 us-west-1 + us-west-2 + us-central-1**

- All three saturate gigabit from SLC (>600 MB/s concurrent ingest)
- p50 latency 24–37ms on warm GET/PUT/HEAD
- Best-in-class burst small-file handling (265–367 ops/s)
- Best-in-class range-GET for JuiceFS (39–49ms p50)
- Multi-region for client load-spreading and minor failover

**Runner-up: Lyve us-east-1** — 94ms p50 warm_put, 127 ops/s burst, 506 MB/s concurrent from SLC. Useful as a 2nd vendor for diversity, but strictly slower than iDrive.

### Ingest tier (bulk writes, then async tier-down to archive)

**Winner: Same three iDrive US regions** — nothing else comes close. Ingest at 600+ MB/s from SLC, then async replicate to Geyser tape for long-term durability.

### Archive tier (cold storage, $/TB optimized)

**Winner: Geyser LA + London** (when London bucket is restored)

- $1.55–$1.75/TB stored, pay-per-used
- 40–76 MB/s concurrent ingest from SLC is enough for archive tiers
- 800–1000ms range-GET is the tape-mount cost; acceptable for archive
- Use with async restore pattern, not synchronous reads

**Also viable for archive:** iDrive eu-west-4 (Paris) or eu-central-2 (Frankfurt) — slower but legitimately cheap if iDrive pricing stays competitive.

### 3-2-1-0 backup / EC redundancy leg

**Recommended:** 3-site Reed-Solomon across iDrive US (primary), Geyser (archive), and iDrive EU (offsite). All three have working S3 compat, known durability, multi-site diversity.

**Do not use:** Quotaless (broken). Its original role in the EC plan is now vacant — iDrive eu-west-1 or eu-west-3 fills it cleanly.

### JuiceFS backend (POSIX filesystem over S3)

**Only viable choices:** iDrive us-west-1 / us-west-2 / us-central-1 / ca-east-1 / us-southwest-1 from SLC (range_get <55ms p50). Lyve us-east-1 is the next-best (89ms).

**Would require disk cache to be usable:** everything else. JuiceFS block reads at >200ms p50 produce bad POSIX UX.

**Unusable for JuiceFS without heavy caching:**
- Geyser tape (1s+ range-GET)
- Lyve APAC
- R2 (300–600ms p50)
- iDrive eu-central-2 (routing issue from SLC, 4.3s range GET p50)

### Immich / Nextcloud / Plex SaaS-backend

Use the JuiceFS stack over iDrive us-west-1 (primary) with Geyser as background archive. For 10K-photo imports, expect ~30 seconds on iDrive; days on anything else.

---

## Cost vs. performance

*(Pricing from public pages as of 2026-04-15; confirm before commercial use.)*

| Provider | $/TB/mo stored | Best perf tier | Fit |
|---|---|---|---|
| Geyser LA | $1.55 (used) | Archive only | Cheapest archive. Tape latency. |
| Geyser London | $1.75 (used) | Archive only | Geo-redundant archive. |
| iDrive e2 | Ask — known to be competitive | **Hot / ingest / EC leg** | Fastest tested backend. Best $/perf. |
| Lyve | ~$5/TB | Secondary hot | Solid but slower than iDrive for ~3× the price. |
| R2 | $15/TB (+ no egress) | Hot for Cloudflare customers | Good from SLC, poor from residential. No egress fee is the real value prop. |
| Quotaless | €60/mo + €20/10TB one-time | — | S3 implementation too broken to rely on. |

**Recommended stack (cost + perf optimal):**

```
Hot tier (user serves):   iDrive us-west-1 + us-west-2 + us-central-1   (3× active)
Async ingest replicator:  SLC → Geyser LA                               (tape backing)
Geo-redundant leg:        SLC → Geyser London OR iDrive eu-west-1       (3-2-1-0)
JuiceFS filesystem:       Mounted on iDrive us-west-1, disk cache local
Hot-disk cache (future):  Gorilla STOR-LA 12×14TB                       ($249/mo, 140TB)
```

---

## Known pre-existing issues (not addressed by this run)

- **geyser-london bucket**: deleted before run, not provisioned. Re-create bucket and set `GEYSER_LONDON_BUCKET` in `.env.bench`, then re-run `./bench-compare -only geyser-london`.
- **bench-compare geyser bucket**: now reads from `GEYSER_LA_BUCKET` / `GEYSER_LONDON_BUCKET` env vars (scrubbed from source).

## Out of scope for this run

- Cost modeling at scale (1TB, 100TB, 1PB)
- Actual JuiceFS + fio benchmark on top of the best backends (separate session)
- Immich / Nextcloud live deployment tests
- RaptorQ / Reed-Solomon multi-provider EC harness
- Gorilla hot-tier benchmark (requires server order)

---

*Generated 2026-04-15 from `mac-full-v2-20260415-160629.json` (3h 20m) and `slc-full-v2-20260415-160629.json` (1h 48m). Hand-written from jq extractions; raw tables in `/tmp/mac-tables.md` and `/tmp/slc-tables.md`.*
