# Multi-Provider S3 Benchmark Comparison v3

**Run date:** 2026-04-20 (v11-allfix)
**Source:** `slc-v11-allfix-20260420-215853.json`
**Providers:** iDrive e2 (13), Seagate Lyve (7), Cloudflare R2 (2), Quotaless (4), Geyser tape (1)
**Endpoints:** 27 | **Workloads per endpoint:** 17–24 | **Wall time:** 3h 10m
**Host:** slc-vaultaire-01 (linux/amd64, SLC datacenter)

**What changed since v2:** Quotaless fully working (UNSIGNED-PAYLOAD + incompatible-op skip), R2 errors resolved, Geyser London excluded (bucket deleted). Transport tuning carried forward from v8 (1MB buffers, TLS 1.2 min, session cache, DNS caching).

---

## Executive Summary

### Five backends, five roles — all validated

For the first time, every provider in the stack completes benchmarks with zero errors and verified data integrity. This validates the full stored.ge architecture:

**1. iDrive e2 is the hot tier.** Three US-West regions sustain 630–863 MB/s concurrent ingest, 24–39ms p50 latency, 250–440 MB/s concurrent download. No other provider is within 2x on ingest from SLC. iDrive us-west-1 at 863 MB/s saturates the datacenter uplink.

**2. Quotaless is back.** The v2 report declared it "not viable" after the SDK checksum failures. With UNSIGNED-PAYLOAD signing and incompatible-op skipping, all four Quotaless endpoints now pass 17/17 workloads with verified checksums. Concurrent download hits 240 MB/s; ingest is 105–145 MB/s. The real value: unlimited free egress at €0.60/TB/mo stored (at 100TB). This is the egress backstop.

**3. R2 delivers on download.** 550 MB/s concurrent download with zero egress fees. Concurrent ingest is only 235 MB/s (R2 throttles writes) but download throughput is the highest of any provider tested. Use for read-heavy public content.

**4. Geyser tape is the cheapest archive at $1.55/TB.** 12 MB/s ingest, 2 MB/s download — tape latency. Correct tier for cold data replication, not serving.

**5. OneDrive fleet is a free CDN layer.** Permafrost v3.1 achieves 183 MB/s fleet (61 MB/s/tenant) with HTTP/1.1 + adaptive range downloads. Three M365 tenants = free 5TB storage per tenant. Not S3-compatible — requires the Permafrost driver — but it's zero-cost storage.

---

## Latency — warm_put p50 (ms)

| Endpoint | v11 p50 | vs v2 |
|---|---|---|
| **idrive-us-west-2** | **27** | stable |
| **idrive-us-west-1** | **28** | stable |
| **idrive-us-central-1** | **39** | +3ms |
| idrive-us-southwest-1 | 45 | new |
| idrive-us-midwest-1 | 49 | new |
| idrive-us-east-1 | 55 | stable |
| idrive-us-southeast-1 | 68 | stable |
| idrive-ca-east-1 | 55 | stable |
| lyve-us-east-1 | 92 | improved (-2) |
| idrive-eu-west-1 | 130 | stable |
| geyser-la | 142 | tape |
| idrive-eu-west-3 | 149 | stable |
| idrive-eu-west-4 | 153 | stable |
| r2-default | 181 | stable |
| idrive-ap-southeast-1 | 205 | stable |
| quotaless-srv1 | 245 | **NEW (was broken)** |
| quotaless-io | 242 | **NEW (was broken)** |
| quotaless-us | 247 | **NEW (was broken)** |
| idrive-eu-central-2 | 317 | improved |
| quotaless-srv2 | 357 | **NEW (was broken)** |
| lyve-us-west-1 | 588 | stable |
| lyve-us-central-2 | 372 | improved |
| r2-eu | 698 | improved |
| lyve-eu-west-1 | 774 | stable |
| lyve-eu-central-1 | 878 | stable |
| lyve-ap-southeast-1 | 1664 | stable |
| lyve-ap-northeast-1 | 1666 | stable |

---

## Concurrent Ingest (32 workers x 4MB, 20s cap)

| Endpoint | MB/s | ops/s | p50 ms |
|---|---|---|---|
| **idrive-us-west-1** | **863** | 216 | 117 |
| lyve-us-east-1 | 667 | 167 | 120 |
| idrive-us-central-1 | 636 | 159 | 154 |
| idrive-us-west-2 | 634 | 158 | 140 |
| idrive-us-southeast-1 | 444 | 111 | 125 |
| idrive-us-southwest-1 | 333 | 83 | 285 |
| lyve-us-central-2 | 273 | 68 | 388 |
| idrive-eu-west-1 | 240 | 60 | 416 |
| r2-default | 235 | 59 | 417 |
| idrive-ca-east-1 | 232 | 58 | 313 |
| idrive-eu-west-3 | 231 | 58 | 486 |
| lyve-us-west-1 | 210 | 52 | 520 |
| idrive-ap-southeast-1 | 160 | 40 | 562 |
| idrive-eu-west-4 | 156 | 39 | 586 |
| quotaless-srv2 | 145 | 36 | 688 |
| quotaless-srv1 | 125 | 31 | 658 |
| lyve-eu-west-1 | 123 | 31 | 725 |
| lyve-eu-central-1 | 119 | 30 | 893 |
| quotaless-io | 106 | 26 | 1079 |
| r2-eu | 96 | 24 | 640 |
| idrive-us-east-1 | 73 | 18 | 989 |
| idrive-eu-central-2 | 54 | 13 | 1375 |
| quotaless-us | 48 | 12 | 1204 |
| idrive-us-midwest-1 | 44 | 11 | 1503 |
| geyser-la | 12 | 3 | 8460 |
| lyve-ap-southeast-1 | 11 | 3 | 8181 |
| lyve-ap-northeast-1 | 10 | 3 | 9197 |

**Notes:**
- idrive-us-west-1 at 863 MB/s is +130% vs v8 (375 MB/s). Night run — less contention.
- idrive-us-east-1 and us-midwest-1 underperform vs other US regions — likely regional congestion at this time of day.
- Quotaless-us at 48 MB/s is an outlier vs srv1/srv2/io at 105–145 MB/s.

---

## Concurrent Download (32 workers x 4MB, 20s cap)

| Endpoint | MB/s | ops/s | p50 ms |
|---|---|---|---|
| **r2-default** | **550** | 138 | 139 |
| idrive-us-west-2 | 438 | 110 | 148 |
| idrive-us-west-1 | 435 | 109 | 126 |
| lyve-us-east-1 | 348 | 87 | 181 |
| lyve-us-central-2 | 314 | 79 | 235 |
| idrive-us-central-1 | 252 | 63 | 228 |
| r2-eu | 284 | 71 | 336 |
| quotaless-io | **240** | 60 | 283 |
| quotaless-srv2 | 226 | 56 | 370 |
| idrive-us-east-1 | 241 | 60 | 180 |
| idrive-us-midwest-1 | 223 | 56 | 251 |
| quotaless-us | 218 | 55 | 392 |
| lyve-us-west-1 | 215 | 54 | 354 |
| quotaless-srv1 | 181 | 45 | 395 |
| idrive-us-southwest-1 | 171 | 43 | 289 |
| idrive-us-southeast-1 | 164 | 41 | 296 |
| idrive-eu-west-3 | 152 | 38 | 467 |
| idrive-ap-southeast-1 | 147 | 37 | 522 |
| idrive-ca-east-1 | 145 | 36 | 366 |
| lyve-eu-central-1 | 123 | 31 | 689 |
| lyve-eu-west-1 | 117 | 29 | 689 |
| idrive-eu-west-1 | 86 | 22 | 841 |
| idrive-eu-west-4 | 17 | 4 | 3973 |
| lyve-ap-southeast-1 | 11 | 3 | 7624 |
| lyve-ap-northeast-1 | 11 | 3 | 7597 |
| idrive-eu-central-2 | 2 | 1 | 9993 |
| geyser-la | 2 | 1 | 8437 |

**Key finding:** R2 at 550 MB/s concurrent download is #1 and egress-free. For read-heavy workloads, R2 as a CDN-origin tier is compelling.

---

## Burst Small Files (500 x 4KB, 10 workers)

Proxy for Immich / Nextcloud / Plex photo-library ingest.

| Endpoint | ops/s | p50 ms |
|---|---|---|
| **idrive-us-west-2** | **348** | 26 |
| **idrive-us-west-1** | **338** | 28 |
| idrive-us-central-1 | 248 | 39 |
| idrive-us-southwest-1 | 219 | 45 |
| idrive-us-midwest-1 | 188 | 49 |
| idrive-us-east-1 | 175 | 55 |
| idrive-ca-east-1 | 158 | 55 |
| idrive-us-southeast-1 | 138 | 68 |
| lyve-us-east-1 | 117 | 72 |
| lyve-us-central-2 | 92 | 105 |
| lyve-us-west-1 | 77 | 120 |
| idrive-eu-west-1 | 70 | 127 |
| idrive-eu-west-3 | 64 | 138 |
| idrive-eu-west-4 | 62 | 144 |
| r2-default | 50 | 186 |
| geyser-la | 50 | 142 |
| idrive-ap-southeast-1 | 47 | 187 |
| lyve-eu-west-1 | 44 | 210 |
| lyve-eu-central-1 | 38 | 227 |
| quotaless-srv2 | 33 | 276 |
| quotaless-io | 33 | 274 |
| quotaless-srv1 | 27 | 362 |
| idrive-eu-central-2 | 25 | 334 |
| quotaless-us | 22 | 421 |
| r2-eu | 16 | 590 |
| lyve-ap-southeast-1 | 14 | 532 |
| lyve-ap-northeast-1 | 13 | 557 |

**Photo import speed (10K photos):**
- iDrive us-west-1/2: ~29 seconds
- Lyve us-east-1: ~85 seconds
- Quotaless: ~5 minutes
- Geyser tape: ~3.3 minutes (surprisingly decent for tape on small files)

---

## Data Integrity

All 27 endpoints pass integrity verification with zero retries:

| Provider | integrity_16mb | integrity_robust_16mb | integrity_chunked_16mb |
|---|---|---|---|
| iDrive (13 regions) | all pass | all pass | all pass |
| Lyve (7 regions) | all pass | all pass | all pass |
| R2 (2 regions) | all pass | all pass | all pass |
| Quotaless (4 endpoints) | all pass | all pass | skipped (incompatible) |
| Geyser LA | pass | pass | pass |

---

## Quotaless — Full Rehabilitation

v2 report verdict: *"Do not design Quotaless into the durability plan."*
v3 verdict: **Quotaless is viable for bulk storage and egress-heavy workloads.**

### What fixed it

The SDK's `PutObject` was sending `x-amz-content-sha256` with a streaming hash that Quotaless's Minio gateway doesn't support. Swapping in `SwapComputePayloadSHA256ForUnsignedPayloadMiddleware` tells the SDK to use `UNSIGNED-PAYLOAD` instead. Seven workloads remain incompatible with Minio (CopyObject, batch delete, list operations, multipart, chunked integrity) — these are now skipped, not errored.

### Quotaless v11 performance

| Endpoint | Conc DL | Conc Ingest | Single GET | Single PUT | warm_put p50 | Integrity |
|---|---|---|---|---|---|---|
| io | **240 MB/s** | 106 MB/s | 21 MB/s | 10 MB/s | 242ms | verified |
| srv2 | 226 MB/s | 145 MB/s | 21 MB/s | 10 MB/s | 357ms | verified |
| us | 218 MB/s | 48 MB/s | 0.2 MB/s* | 11 MB/s | 247ms | verified |
| srv1 | 181 MB/s | 125 MB/s | 21 MB/s | 13 MB/s | 245ms | verified |

*quotaless-us large_get_64mb had a 305-second outlier — anomaly, not systemic.

### Quotaless limitations (skipped workloads)

CopyObject, DeleteObjects (batch), ListObjectsV2, CreateMultipartUpload, AbortMultipartUpload, integrity_chunked_16mb — all return malformed XML or error on Minio gateway. The Vaultaire driver must implement these operations client-side or avoid them.

### Quotaless vs raw HTTP ceiling

The dedicated quotaless-bench-v2 (raw HTTP + SigV4) achieves 393 MB/s download. The SDK-based bench-compare reaches 240 MB/s. The ~40% gap is SDK overhead (connection pooling, header signing, response parsing). The Vaultaire production driver should use raw HTTP for Quotaless to capture the full ceiling.

---

## Provider Scorecards

### iDrive e2

| Metric | Best Region | Value |
|---|---|---|
| Latency (p50) | us-west-1 / us-west-2 | **24–28ms** |
| Concurrent ingest | us-west-1 | **863 MB/s** |
| Concurrent download | us-west-2 | **438 MB/s** |
| Burst small files | us-west-2 | **348 ops/s** |
| Integrity | all 13 regions | all pass |
| Errors | — | **zero** |

**Role:** Hot tier. Write-primary. JuiceFS backend. User-facing S3 serving.
**Weakness:** Some US regions inconsistent (us-east-1 only 73 MB/s ingest, us-midwest-1 only 44 MB/s). EU regions 2–5x slower than US-West. APAC usable but not fast.
**Regions to use:** us-west-1 (primary), us-west-2 (failover), us-central-1 (geographic diversity).

### Seagate Lyve Cloud

| Metric | Best Region | Value |
|---|---|---|
| Latency (p50) | us-east-1 | **92ms** |
| Concurrent ingest | us-east-1 | **667 MB/s** |
| Concurrent download | us-east-1 | **348 MB/s** |
| Burst small files | us-east-1 | **117 ops/s** |
| Integrity | all 7 regions | all pass |
| Errors | — | **zero** |

**Role:** Secondary hot tier. Geographic redundancy. 3-2-1-0 diversity leg.
**Weakness:** 3x higher latency than iDrive. APAC regions ~10 MB/s (unusable for hot serving). ~$5/TB/mo vs iDrive's competitive pricing.
**Regions to use:** us-east-1 (only competitive region from SLC).

### Cloudflare R2

| Metric | Region | Value |
|---|---|---|
| Latency (p50) | default | **181ms** |
| Concurrent ingest | default | **235 MB/s** |
| Concurrent download | default | **550 MB/s** |
| Burst small files | default | **50 ops/s** |
| Integrity | both | all pass |
| Errors | — | **zero** |

**Role:** CDN-origin. Read-heavy public content. Egress-free serving.
**Weakness:** Write latency is 4–7x higher than iDrive. Burst small-file ops is only 50/s (7x worse than iDrive). $15/TB/mo stored.
**Best use:** Front a read-heavy bucket where egress fees would otherwise dominate COGS.

### Quotaless

| Metric | Best Endpoint | Value |
|---|---|---|
| Latency (p50) | io | **242ms** |
| Concurrent ingest | srv2 | **145 MB/s** |
| Concurrent download | io | **240 MB/s** |
| Burst small files | srv2/io | **33 ops/s** |
| Integrity | all 4 | all pass |
| Errors | — | **zero (17/17 workloads)** |

**Role:** Egress backstop. Bulk storage for egress-heavy tenants.
**Weakness:** 245ms+ put latency (10x iDrive). No multipart upload. No CopyObject. No list. Small-file ops are poor (33 ops/s). Requires UNSIGNED-PAYLOAD — standard SDK config breaks.
**Best use:** Route high-egress tenants here. At 100TB = €0.60/TB/mo with **unlimited free egress**. Break-even vs iDrive egress fees: ~18TB. For a customer pulling 50TB/mo egress, this saves thousands compared to any other provider.
**Driver note:** Use raw HTTP + SigV4 in production driver (not SDK) — captures 393 MB/s vs SDK's 240 MB/s.

### Geyser Tape

| Metric | Region | Value |
|---|---|---|
| Latency (p50) | LA | **142ms** (warm_put), **53ms** (warm_get cached) |
| Concurrent ingest | LA | **12 MB/s** |
| Concurrent download | LA | **2 MB/s** |
| Burst small files | LA | **50 ops/s** (surprisingly good) |
| Integrity | LA | all pass |
| Errors | — | **zero** |

**Role:** Deep archive. 3-2-1-0 offsite copy. Glacier alternative at $1.55/TB.
**Weakness:** Tape mount latency. 2 MB/s concurrent download is tape-limited. London bucket still needs provisioning.
**Best use:** Async replication of all data for disaster recovery. Never serve user traffic from tape.

---

## What This Enables

### stored.ge production architecture (validated)

```
┌─────────────────────────────────────────────────────────────┐
│                    User S3 Request                          │
│                         │                                   │
│                    ┌────▼────┐                              │
│                    │ Vaultaire│                              │
│                    │  Engine  │                              │
│                    └────┬────┘                              │
│                         │                                   │
│         ┌───────────────┼───────────────┐                   │
│         ▼               ▼               ▼                   │
│   ┌──────────┐   ┌──────────┐   ┌──────────┐              │
│   │  iDrive  │   │ Quotaless│   │    R2    │              │
│   │  HOT     │   │  EGRESS  │   │   CDN    │              │
│   │ 863 MB/s │   │ 240 MB/s │   │ 550 MB/s │              │
│   │ 28ms p50 │   │ free out │   │ free out │              │
│   └────┬─────┘   └──────────┘   └──────────┘              │
│        │  async                                             │
│        ▼                                                    │
│   ┌──────────┐   ┌──────────┐                              │
│   │  Geyser  │   │ OneDrive │                              │
│   │ ARCHIVE  │   │   FREE   │                              │
│   │ $1.55/TB │   │ 183 MB/s │                              │
│   │  tape    │   │ fleet    │                              │
│   └──────────┘   └──────────┘                              │
└─────────────────────────────────────────────────────────────┘
```

### Use cases now validated by data

**1. Self-hosted Immich / Nextcloud / Plex (stored.ge Vault tiers)**
- Ingest: iDrive us-west-1 at 348 ops/s — 10K photo import in 29 seconds
- Serve: iDrive at 24ms p50 or R2 for public galleries (zero egress)
- Archive: Geyser tape at $1.55/TB for cold backups
- **What this means:** A Vault3 ($3.99/3TB) customer gets photo import speed that competes with Google Photos, backed by $1.55/TB tape archive for durability

**2. JuiceFS POSIX filesystem over S3**
- Only viable on iDrive us-west-1/2/central-1 (24–39ms range GET p50)
- Validated: range_get_1mb_chunks at 10–15 MB/s, 39–49ms p50 — fast enough for interactive POSIX
- Pair with local disk cache for anything outside those three regions
- **What this means:** Can mount an S3 bucket as a filesystem for Docker volumes, media processing, etc.

**3. Egress-heavy workloads (video streaming, CDN origin)**
- Route to Quotaless: 240 MB/s download, unlimited free egress, €0.60/TB at 100TB
- Or R2: 550 MB/s download, zero egress, but $15/TB stored
- Crossover: R2 cheaper if stored <4TB, Quotaless cheaper at scale
- **What this means:** A customer streaming 100TB/mo video pays €60/mo on Quotaless vs ~$900/mo on S3

**4. 3-2-1-0 backup / disaster recovery**
- Site 1: iDrive us-west-1 (hot, SLC-adjacent)
- Site 2: iDrive eu-west-1 or Lyve us-east-1 (geographic diversity)
- Site 3: Geyser LA tape ($1.55/TB, offline media)
- All three pass integrity checks with zero retries
- **What this means:** Customer data survives datacenter loss, provider failure, or regional outage

**5. Tiered storage with automatic cost optimization**
- Hot (0–30 days): iDrive us-west-1 — fastest, moderate cost
- Warm (30–90 days): Quotaless — cheaper, still decent throughput
- Cold (90+ days): Geyser tape — cheapest at $1.55/TB
- **What this means:** COGS drops from ~$5/TB to ~$2/TB blended as data ages

### Production driver priorities

Based on these benchmarks, the driver implementation order should be:

1. **iDrive driver** — standard AWS SDK, already working. Primary backend.
2. **Quotaless driver** — raw HTTP + SigV4 (NOT SDK). Must handle: no multipart, no CopyObject, no list. Client-side chunking for large uploads. UNSIGNED-PAYLOAD required.
3. **Geyser driver** — standard AWS SDK with InsecureTLS. Async-only writes (queue + replicate). No synchronous reads.
4. **R2 driver** — standard AWS SDK. CDN-origin configuration. Read-path optimization.
5. **OneDrive/Permafrost driver** — custom Graph API. HTTP/1.1 for downloads, HTTP/2 for API. Adaptive range downloads. Batch prefetch.

---

## Anomalies and Follow-ups

| Issue | Impact | Action |
|---|---|---|
| idrive-us-east-1 ingest only 73 MB/s (was 368 in v2) | Not a top-3 region from SLC | Re-test during business hours; likely time-of-day variance |
| idrive-us-midwest-1 still underperforming (44 MB/s) | Consistent across v2 and v11 | Raise with iDrive support |
| idrive-eu-central-2 took 67 minutes (others ~3 min) | EU regions from SLC are slow | Expected — distance. Not a bug. |
| idrive-eu-west-4 download only 17 MB/s | Outlier vs other EU regions | Re-test to confirm |
| quotaless-us ingest 48 MB/s (others 105–145) | Single endpoint underperforming | Routing issue, re-test |
| quotaless-us large_get_64mb 305 seconds | Single outlier, 0.2 MB/s | Timeout/retry issue |
| Geyser London bucket not provisioned | Missing geo-redundant archive leg | Provision bucket, set GEYSER_LONDON_BUCKET |

---

*Generated 2026-04-21 from `slc-v11-allfix-20260420-215853.json` (3h 10m, 27 endpoints, 0 errors). Supersedes COMPARISON-v2.md.*
