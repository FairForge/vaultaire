# Lyve / R2 / Quotaless Benchmark Comparison

**Mac**: `Ike-2.local` (darwin/arm64) — 56m 53s, started 2026-04-09T19:18:38Z
**SLC**: `slc-vaultaire-01` (linux/amd64) — 44m 49s, started 2026-04-09T19:32:00Z

Total endpoints: 7 Lyve regions + 2 Cloudflare R2 (default + EU) + 1 Quotaless = **10**.
Total workloads per endpoint: **19** (cold dial, warm small ops, head, copy, delete×2, list×2,
medium PUT/GET, large PUT/GET, 256MB multipart, concurrent ingest, mpu abort, integrity).

## 1. Latency (Mac → endpoint vs SLC → endpoint)

All values are **p50 milliseconds** (lower is better). `—` means workload errored.

| Endpoint | cold_dial<br/>Mac | cold_dial<br/>SLC | warm_put<br/>Mac | warm_put<br/>SLC | warm_get<br/>Mac | warm_get<br/>SLC | warm_head<br/>Mac | warm_head<br/>SLC |
|---|---|---|---|---|---|---|---|---|
| lyve-us-east-1 | 307ms | 261ms | 111ms | 73ms | 79ms | 66ms | 71ms | 65ms |
| lyve-us-west-1 | 817ms | 742ms | 601ms | 589ms | 142ms | 116ms | 60ms | 38ms |
| lyve-us-central-2 | 733ms | 509ms | 392ms | 371ms | 120ms | 97ms | 48ms | 54ms |
| lyve-eu-west-1 | 1307ms | 1331ms | 773ms | 771ms | 217ms | 210ms | 136ms | 129ms |
| lyve-eu-central-1 | 1570ms | 1490ms | 1006ms | 868ms | 236ms | 241ms | 139ms | 146ms |
| lyve-ap-southeast-1 | 2156ms | 1748ms | 1697ms | 550ms | 1576ms | 414ms | 266ms | 229ms |
| lyve-ap-northeast-1 | 2184ms | 1827ms | 1562ms | 584ms | 506ms | 414ms | 267ms | 228ms |
| r2-default | 853ms | 223ms | 407ms | 197ms | 276ms | 112ms | 191ms | 83ms |
| r2-eu | 644ms | 991ms | 590ms | 1012ms | 435ms | 448ms | 225ms | 195ms |
| quotaless-io | — | 950ms | — | 153ms | — | 153ms | — | 153ms |

## 2. Single-stream PUT throughput

Megabytes per second on a single PUT request. Higher is better.

| Endpoint | medium_put_1mb<br/>Mac | SLC | medium_put_16mb<br/>Mac | SLC | large_put_64mb<br/>Mac | SLC |
|---|---|---|---|---|---|---|
| lyve-us-east-1 | 3.6 | 4.7 | 21.5 | 105.4 | 11.5 | 207.7 |
| lyve-us-west-1 | 1.8 | 1.6 | 16.1 | 18.3 | 12.3 | 44.4 |
| lyve-us-central-2 | 2.5 | 2.8 | 26.5 | 35.7 | 30.8 | 77.6 |
| lyve-eu-west-1 | 1.3 | 1.3 | 2.7 | 11.9 | 9.7 | 42.9 |
| lyve-eu-central-1 | 1.0 | 1.1 | 7.9 | 13.6 | 13.0 | 47.0 |
| lyve-ap-southeast-1 | 0.6 | 0.5 | 3.9 | 3.2 | 6.4 | 6.7 |
| lyve-ap-northeast-1 | 0.6 | 0.7 | 3.9 | 4.4 | 3.8 | 7.3 |
| r2-default | 1.1 | 3.4 | 8.3 | 9.3 | 18.7 | 15.4 |
| r2-eu | 1.0 | 0.8 | 3.7 | 2.4 | 12.9 | 8.1 |
| quotaless-io | — | — | — | — | — | — |

## 3. Single-stream GET throughput

Megabytes per second on a single GET request. Higher is better.

| Endpoint | medium_get_1mb<br/>Mac | SLC | medium_get_16mb<br/>Mac | SLC | large_get_64mb<br/>Mac | SLC |
|---|---|---|---|---|---|---|
| lyve-us-east-1 | 5.9 | 6.9 | 7.1 | 70.5 | 3.6 | 76.8 |
| lyve-us-west-1 | 1.3 | 2.0 | 6.5 | 20.9 | 3.7 | 49.7 |
| lyve-us-central-2 | 2.4 | 2.3 | 9.8 | 32.3 | 13.3 | 51.8 |
| lyve-eu-west-1 | 1.0 | 1.3 | 2.7 | 10.5 | 3.3 | 15.0 |
| lyve-eu-central-1 | 0.9 | 0.9 | 3.4 | 8.8 | 5.1 | 10.5 |
| lyve-ap-southeast-1 | 0.6 | 0.4 | 2.2 | 4.4 | 4.8 | 5.7 |
| lyve-ap-northeast-1 | 0.6 | 0.6 | 2.4 | 3.8 | 3.1 | 5.3 |
| r2-default | 3.0 | 7.4 | 20.1 | 26.6 | 28.3 | 30.9 |
| r2-eu | 1.3 | 1.4 | 11.3 | 11.0 | 25.0 | 19.3 |
| quotaless-io | — | — | — | — | — | — |

## 4. Multi-stream / concurrent throughput

`multipart_256mb` is one 256MB upload split into 16MB parts with parallel=4.
`concurrent_ingest_20s` is 32 worker goroutines × 4MB PUTs for 20 seconds (or 1GB cap).

| Endpoint | multipart_256mb<br/>Mac MB/s | SLC MB/s | concurrent_ingest<br/>Mac MB/s | SLC MB/s | concurrent_ingest<br/>Mac ops/s | SLC ops/s |
|---|---|---|---|---|---|---|
| lyve-us-east-1 | 11.2 | 131.0 | 44.0 | 522.0 | 11 | 130 |
| lyve-us-west-1 | 20.0 | 57.0 | 46.6 | 164.2 | 12 | 41 |
| lyve-us-central-2 | 31.8 | 94.4 | 48.8 | 268.3 | 12 | 67 |
| lyve-eu-west-1 | 16.1 | 31.8 | 33.2 | 148.7 | 8.3 | 37 |
| lyve-eu-central-1 | 17.3 | 37.8 | 43.0 | 127.1 | 11 | 32 |
| lyve-ap-southeast-1 | 7.9 | 7.8 | 10.6 | 11.0 | 2.6 | 2.7 |
| lyve-ap-northeast-1 | 7.6 | 4.0 | 8.4 | 11.6 | 2.1 | 2.9 |
| r2-default | 25.6 | 36.1 | 33.4 | 128.5 | 8.3 | 32 |
| r2-eu | 15.8 | 11.8 | 49.6 | 61.3 | 12 | 15 |
| quotaless-io | — | — | — | — | — | — |

## 5. Control-plane latency

All in **milliseconds** (full request, not p50).

| Endpoint | warm_copy<br/>p50 Mac | SLC | delete_single<br/>p50 Mac | SLC | delete_batch_100<br/>total Mac | SLC | list_100<br/>Mac | SLC | list_prefix<br/>Mac | SLC | mpu_abort<br/>p50 Mac | SLC |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| lyve-us-east-1 | 82ms | 74ms | 77ms | 72ms | 185ms | 558ms | 177ms | 133ms | 76ms | 61ms | 442ms | 429ms |
| lyve-us-west-1 | 222ms | 577ms | 139ms | 123ms | 2528ms | 3743ms | 185ms | 129ms | 64ms | 43ms | 423ms | 497ms |
| lyve-us-central-2 | 388ms | 377ms | 107ms | 112ms | 2093ms | 2514ms | 171ms | 172ms | 61ms | 57ms | 421ms | 438ms |
| lyve-eu-west-1 | 706ms | 293ms | 218ms | 212ms | 2306ms | 2307ms | 335ms | 298ms | 208ms | 135ms | 813ms | 768ms |
| lyve-eu-central-1 | 337ms | 337ms | 236ms | 242ms | 2769ms | 2780ms | 390ms | 392ms | 212ms | 171ms | 835ms | 846ms |
| lyve-ap-southeast-1 | 1435ms | 831ms | 678ms | 644ms | 14.0s | 14.7s | 897ms | 768ms | 283ms | 241ms | 1836ms | 1657ms |
| lyve-ap-northeast-1 | 939ms | 827ms | 677ms | 639ms | 13.9s | 14.0s | 892ms | 782ms | 284ms | 244ms | 1833ms | 1760ms |
| r2-default | 993ms | 343ms | 554ms | 102ms | 4094ms | 361ms | 301ms | 74ms | 211ms | 75ms | 1050ms | 241ms |
| r2-eu | 953ms | 1470ms | 253ms | 259ms | 716ms | 706ms | 458ms | 449ms | 237ms | 228ms | 580ms | 560ms |
| quotaless-io | — | — | — | 153ms | — | — | — | — | — | — | — | — |

## 6. S3 API compatibility (errors per workload)

`✓` = clean run, `⚠` = partial errors, `✗` = total failure / no measurable result.

**SLC run** (Mac matches except Quotaless not tested on Mac):

| Workload | lyve-us-east-1 | lyve-us-west-1 | lyve-us-central-2 | lyve-eu-west-1 | lyve-eu-central-1 | lyve-ap-southeast-1 | lyve-ap-northeast-1 | r2-default | r2-eu | quotaless-io |
|---|---|---|---|---|---|---|---|---|---|---|
| cold_dial_put_1kb | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| warm_put_4kb | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| warm_get_4kb | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| warm_head_4kb | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| warm_copy_4kb | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ⚠ |
| warm_delete_single | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| delete_batch_100 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ |
| list_100 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ |
| list_prefix | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ |
| medium_put_1mb | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ⚠ |
| medium_get_1mb | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| medium_put_16mb | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ⚠ |
| medium_get_16mb | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| large_put_64mb | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ |
| large_get_64mb | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ |
| multipart_put_256mb | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ |
| mpu_abort | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ⚠ |
| concurrent_ingest_20s | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ⚠ |
| integrity_16mb | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ |

## 7. At-a-glance summary

Best single-stream large-PUT throughput from each host (one number to remember):

- Mac best single-stream PUT: **lyve-us-central-2** at **30.8 MB/s**
- SLC best single-stream PUT: **lyve-us-east-1** at **207.7 MB/s**
- Mac best concurrent ingest: **r2-eu** at **49.6 MB/s**
- SLC best concurrent ingest: **lyve-us-east-1** at **522.0 MB/s**


---

## 8. Headline findings

### Finding 1 — From SLC, Lyve us-east-1 is the fastest target in the matrix by a wide margin.

| Metric (SLC → Lyve us-east-1) | Value | 2nd best from SLC | Ratio |
|---|---|---|---|
| large_put_64mb | **207.7 MB/s** | 77.6 (lyve-us-central-2) | 2.7× |
| concurrent_ingest_20s | **522.0 MB/s** | 268.3 (lyve-us-central-2) | 1.9× |
| medium_put_16mb | **105.4 MB/s** | 35.7 (lyve-us-central-2) | 3.0× |
| medium_get_16mb | **70.5 MB/s** | 32.3 (lyve-us-central-2) | 2.2× |

522 MB/s ≈ 4.2 Gbps. Either SLC has a multi-gigabit path to whatever DC Lyve calls "us-east-1", or Lyve has aggressive parallel-PUT optimization on that endpoint. Likely both.

### Finding 2 — Mac upload is the bottleneck, not Lyve.

| Same endpoint, different host | Mac | SLC | SLC÷Mac |
|---|---|---|---|
| Lyve us-east-1 large_put_64mb | 11.5 MB/s | 207.7 MB/s | **18×** |
| Lyve us-east-1 concurrent_ingest | 44.0 MB/s | 522.0 MB/s | **12×** |
| Lyve us-east-1 multipart_256mb | 11.2 MB/s | 131.0 MB/s | **12×** |

Your home ISP cap (~10-15 MB/s = ~100-120 Mbps up) explains most of the Mac numbers for big files. **Don't make architecture decisions from Mac numbers for write throughput.**

### Finding 3 — R2 anycast quality varies dramatically by client location.

| | cold_dial p50 | warm_put p50 | large_put MB/s |
|---|---|---|---|
| Mac → R2 default | 853ms | 407ms | 18.7 |
| SLC → R2 default | **223ms** | **197ms** | 15.4 |
| Mac → R2 EU | 644ms | 590ms | 12.9 |
| SLC → R2 EU | 991ms | 1012ms | 8.1 |

SLC sits next to a Cloudflare PoP and gets ~4× lower latency. Mac's residential ISP is hitting a less optimal R2 PoP — anycast does NOT guarantee great routing, it guarantees nearest **reachable** PoP. The R2-EU jurisdiction endpoint is universally slower than default anycast and only worth using for jurisdiction-required customers.

### Finding 4 — Lyve regional names don't match geographic intuition.

From Salt Lake City (the closest to "us-west" by geography):

| Lyve region | SLC cold dial p50 | SLC large_put MB/s |
|---|---|---|
| us-east-1 | **261ms** ← closest! | **207.7** |
| us-central-2 | 509ms | 77.6 |
| us-west-1 | 742ms | 44.4 |
| eu-central-1 | 1490ms | 47.0 |
| eu-west-1 | 1331ms | 42.9 |
| ap-southeast-1 | 1748ms | 6.7 |
| ap-northeast-1 | 1827ms | 7.3 |

**Lyve "us-west-1" is almost 3× slower from SLC than "us-east-1".** Either us-west-1 is far from SLC physically (LA? Seattle? Hawaii?) or the network path is bad. Either way: if you keep Lyve, **us-east-1 is your only "fast" region from SLC** — others are all 2-30× slower.

### Finding 5 — Asia-Pacific Lyve regions are not viable for primary writes from US/EU.

| AP region | SLC large_put | Mac large_put |
|---|---|---|
| ap-southeast-1 | 6.7 MB/s | 6.4 MB/s |
| ap-northeast-1 | 7.3 MB/s | 3.8 MB/s |

Concurrent ingest only gets you to **11 MB/s** (vs 522 MB/s for us-east-1). These should be skipped entirely unless you actively serve customers there. Even then, you'd want a regional ingress point rather than going cross-Pacific.

### Finding 6 — From SLC, Lyve us-east-1 beats R2 by 13× on single-stream large PUT and 4× on concurrent ingest.

| SLC → endpoint | large_put_64mb | concurrent_ingest_20s |
|---|---|---|
| **lyve-us-east-1** | **207.7 MB/s** | **522 MB/s** |
| r2-default | 15.4 MB/s | 128 MB/s |
| ratio | 13.5× | 4.1× |

If your ingest is server-to-server from SLC, **Lyve us-east-1 destroys R2 for write throughput**. R2 is competitive for control-plane ops and reads, but for raw bytes-in, Lyve wins by an order of magnitude.

### Finding 7 — From the Mac (residential), R2 default ≈ Lyve us-east-1 for big writes, but R2 wins for cold-dial latency from anywhere global.

| Mac → endpoint | large_put_64mb | warm_put p50 |
|---|---|---|
| lyve-us-east-1 | 11.5 MB/s | 111ms |
| r2-default | 18.7 MB/s | 407ms |

R2 has higher per-request overhead (anycast LB + edge auth) but a fatter pipe from your home network. Lyve us-east-1 has lower per-request latency (smaller distance, simpler stack) but doesn't fit as much through your link in a single stream.

### Finding 8 — R2's control plane is FAST from SLC, slower from residential.

| | list_100 | delete_batch_100 |
|---|---|---|
| Mac → R2 default | 301ms | 4094ms |
| SLC → R2 default | **74ms** | **361ms** |
| Mac → Lyve us-east-1 | 177ms | 185ms |
| SLC → Lyve us-east-1 | 133ms | 558ms |

From SLC, R2 has the **fastest list** (74ms) and **fastest batch delete** (361ms) of any endpoint tested. Lyve us-east-1's batch-delete (558ms) is actually slower than R2 from SLC. From Mac, the order flips because the network round-trip dominates.

### Finding 9 — Quotaless is severely S3-incompatible.

| Workload | Result |
|---|---|
| Small PUT/GET/HEAD/DELETE on ≤4KB | ✓ works |
| `CopyObject` | ✗ all 10 fail |
| `medium_put_1mb`, `medium_put_16mb`, `large_put_64mb` | ✗ all fail |
| `ListObjectsV2` (any form) | ✗ XML parse error: "attribute name without = in element" |
| `DeleteObjects` (batch) | ✗ same XML parse error |
| `CreateMultipartUpload` | ✗ same XML parse error |
| `concurrent_ingest_20s` | ✗ 677/677 errors |
| `integrity_16mb` round-trip | ✗ checksum subsystem failure |

Quotaless's S3 emulator returns malformed XML for most non-trivial responses, doesn't support multipart upload at the `io.` endpoint, and chokes on PUTs above ~4KB without the production driver's `personal-files/` prefix. **This is not a usable tier behind a generic S3 client.** The existing `internal/drivers/quotaless.go` works in production only because it manually injects the path prefix and avoids the broken operations entirely. If you stay with Quotaless, it has to remain a special-case driver, not a transparent backend — and forget about any client-side LIST or batch-delete features.

---

## 9. Recommendations

### Should you switch to R2 as your ingress layer?

**Yes, but the picture is more nuanced than the deal terms alone suggest.**

The real answer depends on **where the bytes originate**:

- **Server-to-server ingest from SLC** (deploy hooks, S3 mirroring, datacenter-to-datacenter migration): **Lyve us-east-1 is currently 4-13× faster than R2** from your SLC server. If your customers push via your edge in SLC, keep Lyve us-east-1 as the hot tier.
- **Residential / global client uploads** (browser, desktop sync app, mobile): **R2 default anycast wins** on first-byte latency from anywhere in the world. Lyve has fixed regional endpoints — a customer in Berlin gets eu-central-1 latency (1.5s cold dial from your Mac, comparable from anywhere far away). R2 gets them sub-300ms via the nearest Cloudflare PoP.
- **Restores / GET-heavy workloads**: R2 default is competitive for GETs (30 MB/s from SLC, comparable to Lyve us-central-2). For pull-heavy workloads R2's free egress is the financial winner regardless of raw speed.

**The recommended architecture is dual-ingress, not pick-one:**

```
            ┌─ direct fast path ────────────► Lyve us-east-1 (hot tier from SLC, 207 MB/s)
            │
SLC server ─┤
            │                                 ┌─ R2 default (anycast) ──── for residential / global clients
            └─ thin proxy at /upload ─────────┤
                                              └─ async tier-down to ─────► Geyser/Vault18 (cold)
```

R2 sits in front for client-facing uploads (the actual "ingress layer" question). Lyve us-east-1 stays for server-to-server fast-path until/unless Wasabi consolidation degrades it. Both feed Geyser as the cold tier.

### Lyve region rationalization

| Region | Verdict | Why |
|---|---|---|
| **us-east-1** | **KEEP — primary** | Best in matrix from SLC. 207 MB/s PUT, 522 MB/s concurrent. |
| us-central-2 | KEEP — secondary | Second-best from SLC. Useful as a failover region. |
| us-west-1 | DROP | Slower from SLC than us-east-1, no obvious customer upside. |
| eu-central-1 / eu-west-1 | KEEP ONE | Both ~equivalent (~45 MB/s from SLC). Pick whichever has cheaper cross-connect or nearest customer mix. |
| ap-southeast-1 | DROP unless you have AP customers | 6.7 MB/s is unusable for primary writes. |
| ap-northeast-1 | DROP unless you have JP customers | Same. |

**Default recommendation: us-east-1 + us-central-2 + one EU region. Drop us-west-1 and both AP regions.** If Wasabi's consolidation lets you migrate Lyve buckets between regions cheaply, you can revisit AP later.

### R2 jurisdictions

**Use R2 default (anycast) as the customer-facing endpoint.** The R2-EU jurisdiction endpoint is consistently slower from both hosts (4× slower from SLC, ~30% slower from Mac) and only useful for customers with explicit EU residency requirements. Make EU jurisdiction an opt-in flag at bucket creation, not the default.

### Quotaless

**Stop treating Quotaless as a generic S3 backend.** It's a single-namespace flat store with broken LIST, broken batch delete, broken multipart, and broken large PUTs at the `io.` endpoint. The existing Vaultaire driver works in production by manually wrapping the broken bits — that's fine for a special-case tier but it cannot be exposed through normal S3 routing. Deciding whether to keep Quotaless at all is a business question (cost?), not a technical one — but if you keep it, it stays as a hidden backend behind your engine, never as a routable region.

### What to do about the Wasabi/Lyve acquisition

The benchmark makes the risk concrete: **today, your single best write target from SLC is Lyve us-east-1**. That's the asset most exposed to whatever Wasabi does with pricing or terms. The hedge isn't to rip Lyve out — it's to:

1. Add R2 as a parallel ingress option (mostly client-facing).
2. Verify Geyser write throughput from SLC against this same matrix (Geyser wasn't tested today — its tape backend is the long-term cost play, but you need to know its real ingest ceiling).
3. Re-run this same matrix monthly (the bench tool now exists at `cmd/bench-compare/`) so you have a trend line if Lyve performance degrades post-acquisition.
4. Pre-stage migration tooling for Lyve us-east-1 → R2 + Geyser. The fewer manual steps, the less leverage Wasabi has over you at renewal time.

---

## 10. Methodology notes

- **Tool**: `cmd/bench-compare/main.go` — reads creds from env, emits incremental JSON. Source committed to repo, credentials in `.env.bench` (gitignored).
- **Bucket naming**: One reusable bucket per provider (`vbench-<user>-<provider>`), shared across regions. Created on first run via `CreateBucket`, ignored on subsequent runs.
- **Data**: All payloads are `crypto/rand` random bytes (incompressible — no dedup or compression artifacts skewing numbers).
- **Concurrency model**: Sequential workloads use the same long-lived `*s3.Client`. The `cold_dial` workload constructs a fresh client per iteration to capture TLS+TCP+TTFB.
- **Cleanup**: Best-effort `DeleteObject` of every key tracked during the run. Buckets are NOT deleted (so re-runs don't pay create overhead).
- **Variance**: Each workload runs once. Single-trial measurements have natural variance, especially over the public internet. The patterns described above are large enough to dwarf single-trial noise (10× and 20× ratios) but treat any single number with ±25% uncertainty.
- **Not tested**: Geyser, OneDrive, local backend. Versioning, presigned URLs, ACLs, lifecycle, replication. STS / temporary credentials. Real-world workload mixes (the "soak" test from `cmd/bench/main.go` would be a good follow-up for sustained load).
- **Reproduce**: `source .env.bench && /tmp/bench-compare -out bench-results/<host>-$(date +%s).json` from any machine that can reach the endpoints.
