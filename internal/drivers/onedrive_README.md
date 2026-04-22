# OneDrive / Permafrost Fleet

## Overview

OneDrive fleet storage via Microsoft Graph API. NOT a performance or cost tier —
it's a **durability layer** for async background replication from iDrive/Quotaless/Geyser.
Customers never read from OneDrive directly. Value proposition: "15+ independent
copies across Microsoft's infrastructure" at $0 cost.

## Pricing

- Cost: **$0** (Microsoft 365 Business Basic, 1TB per user expandable to 5TB)
- Free for 3 years per tenant (existing subscriptions)
- 15 tenants × 5TB = **75TB free storage**
- No egress fees, no API call charges

## Tier Architecture

| Tier | Backend | Cost | Role |
|------|---------|------|------|
| Performance | iDrive E2 | $3.30/TB | Default hot, <50TB |
| Bulk/Egress | Quotaless | ~€0.60/TB | >50TB, free egress |
| Archive | Geyser | $1.55/TB | Cold tape, compliance |
| Deep Archive | Vault18 | $1/TB | 7+ year retention |
| **Durability** | **OneDrive fleet** | **$0** | **Background replication, 15 copies** |
| CDN | R2 / Pixeldrain | $15 / €4 | Public content (on-demand) |

## Azure App Registration

- Client ID: see `.env.bench` (`AZURE_CLIENT_ID`)
- Auth: Client credentials (service principal), not user-delegated
- Permissions: `Files.ReadWrite.All`, `User.Read.All`, `Sites.Read.All`
- Scopes: `https://graph.microsoft.com/.default`

## Tenant Fleet

| Tenant | Domain | UPN | Status |
|--------|--------|-----|--------|
| tenant-1 | (see .env.bench) | (see .env.bench) | Active |
| tenant-2 | (see .env.bench) | (see .env.bench) | **Check: client secret may be expired** |
| tenant-3 | (see .env.bench) | (see .env.bench) | Active |
| tenant-4..15 | (planned) | — | Not yet provisioned |

Credentials: `TENANT_N_ID`, `TENANT_N_CLIENT_ID`, `TENANT_N_SECRET`, `TENANT_N_USER`
env vars in `.env.bench` (gitignored).

## Microsoft Graph API

### Endpoints

| Operation | Method | Path | RU Cost |
|-----------|--------|------|---------|
| List drives | GET | `/users/{upn}/drives` | 1 |
| Create folder | POST | `/drives/{id}/items/root/children` | 2 |
| Upload (<250MB) | PUT | `/drives/{id}/items/{parentId}:/{name}:/content` | 2 |
| Upload (>4MB) | POST+PUT | `.../createUploadSession` + chunked PUT | 2 |
| Download | GET | `/drives/{id}/items/{id}/content` | 1 |
| Download URL | GET | item metadata → `@microsoft.graph.downloadUrl` | 1 |
| Delete | DELETE | `/drives/{id}/items/{id}` | 2 |
| List children | GET | `/drives/{id}/items/{id}/children` | 2 |
| Delta query (with token) | GET | `/drives/{id}/root/delta` | **1** |
| Delta query (no token) | GET | `/drives/{id}/root/delta` | 2 |
| Batch (up to 20) | POST | `/$batch` | per-request |

### Upload Strategy

| File size | Method | Notes |
|-----------|--------|-------|
| < 4 MB | Simple PUT | Single request, path-based addressing |
| 4 MB - 250 MB | Simple PUT | Works on personal OneDrive (NOT Business/SharePoint) |
| >= 4 MB (Business) | Upload session | Chunked, resumable |
| >= 250 MB | Upload session | Required for all account types |

**Chunked upload rules:**
- Chunk size MUST be a multiple of 320 KiB (327,680 bytes)
- Recommended: 5-10 MB per chunk (max 60 MB per PUT)
- No `Authorization` header on chunk PUTs — the upload URL is pre-authenticated
- On 404: session expired, restart entire upload
- On 416: query upload status with GET to find missing bytes

### Throttling Limits

Per-app (0-1,000 licenses):

| Scope | Limit |
|-------|-------|
| Resource units per minute | **1,250** |
| Resource units per 24 hours | 1,200,000 |
| Tenant RU per 5 minutes | 18,750 |
| Requests per user per 5 min | 3,000 |
| Ingress per user per hour | 50 GB |
| Egress per user per hour | 100 GB |
| App ingress+egress per hour | 400 GB |

**RateLimit headers** (returned when usage >= 80% of limit):
- `RateLimit-Limit`: resource unit cap for current window
- `RateLimit-Remaining`: remaining resource units
- `RateLimit-Reset`: seconds until quota refills

### Throttle Handling

On **429** (Too Many Requests) or **503** (Server Too Busy):
1. Read `Retry-After` header (seconds to wait)
2. If both `Retry-After` and `RateLimit-Reset` present, honor the **greater** value
3. Pause ALL requests to that tenant (not just the failed one) — throttled requests still consume RU
4. Use decorrelated jitter for backoff (not fixed exponential)
5. Cap backoff at 60 seconds

**User-Agent decoration** (prioritized by Microsoft):
```
ISV|FairForge|Vaultaire/1.0
```

### Delta Queries (Efficient Listing)

Delta queries cost only **1 RU with a token** (vs 2 RU for regular list). Use for:
- Sync/scan operations
- Detecting changes since last check
- Store `@odata.deltaLink` for incremental sync

### Batch Operations

Up to 20 requests per batch (`POST /$batch`). Each sub-request evaluated
individually for RU cost. Useful for bulk deletes or metadata fetches.

## Transport Optimization — Three Generations

### v1 (SDK + tuned transport) — 2026-04-18

Single transport, Graph SDK for everything. Applied 256KB buffers, 200 connection pool, HTTP/2, compression off.

```go
http.DefaultTransport = &http.Transport{
    MaxIdleConns:        200,           // connection pool size
    MaxIdleConnsPerHost: 200,           // per-host pool (default: 2)
    ReadBufferSize:      256 * 1024,    // 256KB (64x fewer syscalls vs default 4KB)
    WriteBufferSize:     256 * 1024,
    DisableCompression:  true,
    ForceAttemptHTTP2:   true,
    ...
}
```

**Result**: +69% per-tenant upload (11→18.62 MB/s), +158% large file throughput. Downloads unchanged at ~12 MB/s per tenant.

### v2 (raw HTTP, no Graph SDK) — 2026-04-19

Bypass the Graph SDK entirely. Use `azidentity` for auth only. Raw HTTP for all data operations. Binary drops from 44MB → 10MB (-77%).

- 1MB read/write buffers
- http2.ConfigureTransport() for explicit HTTP/2 config
- Chunked uploads for >4MB files (10MB aligned to 320KiB)
- Pre-warming (eliminates TLS handshake from timing)
- User-Agent: `ISV|FairForge|Vaultaire/1.0`
- Decorrelated jitter backoff: `sleep = min(cap, random(base, prev*3))`

**Result**: 100MB uploads +62% to 98 MB/s fleet. Worker scaling shifted — SDK topped out at 25, raw HTTP scales to 100+ workers. Downloads **still stuck at 12 MB/s fleet ceiling**.

### v3 (HTTP/1.1 for CDN + Range requests) — 2026-04-19

**Breakthrough: Go's HTTP/2 has a known flow-control bug** (github issues #54330, #47840, #63520) where slow streams block fast streams on the same multiplexed connection. Rclone's OneDrive backend documented a **66x speed improvement** by forcing HTTP/1.1 for bulk downloads.

**The fix: dual transport architecture.**

```go
// HTTP/2 for Graph API JSON calls (small, latency-sensitive)
func graphTransport() *http.Transport {
    t := &http.Transport{
        MaxIdleConns:        200,
        MaxIdleConnsPerHost: 200,
        ReadBufferSize:      1 << 20,    // 1MB
        WriteBufferSize:     1 << 20,
        DisableCompression:  true,
        ForceAttemptHTTP2:   true,
        ...
    }
    _ = http2.ConfigureTransport(t)
    return t
}

// HTTP/1.1 for CDN bulk downloads (bypasses HTTP/2 flow-control bug)
func cdnTransport() *http.Transport {
    return &http.Transport{
        // Empty TLSNextProto disables HTTP/2 ALPN negotiation
        TLSNextProto:     make(map[string]func(string, *tls.Conn) http.RoundTripper),
        MaxConnsPerHost:  64,
        ReadBufferSize:   4 * 1024 * 1024,  // 4MB — bigger for bulk data
        WriteBufferSize:  4 * 1024 * 1024,
        DisableCompression: true,
        ...
    }
}

// HTTP/2 for uploads (multiplexing helps write path)
func uploadTransport() *http.Transport {
    // Same as graphTransport but with 4MB buffers
}
```

**Range requests for single-file parallelism:**

```go
// Split one file into N parallel byte-range downloads
func (a *authClient) downloadRanges(ctx context.Context, dlURL string, fileSize int64, streams int) (int64, error) {
    rangeSize := fileSize / int64(streams)
    var wg sync.WaitGroup
    var totalBytes atomic.Int64
    for i := 0; i < streams; i++ {
        start := int64(i) * rangeSize
        end := start + rangeSize - 1
        if i == streams-1 { end = fileSize - 1 }
        wg.Add(1)
        go func(s, e int64) {
            defer wg.Done()
            req, _ := http.NewRequest("GET", dlURL, nil)
            req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", s, e))
            resp, _ := a.cdnHTTP.Do(req)   // HTTP/1.1 transport
            defer resp.Body.Close()
            n, _ := io.Copy(io.Discard, resp.Body)
            totalBytes.Add(n)
        }(start, end)
    }
    wg.Wait()
    return totalBytes.Load(), nil
}
```

**60MB upload chunks** (up from 10MB — Microsoft's max, 35% faster).

**Batch metadata prefetch** via `POST /$batch` (up to 20 requests per call, 2.1x metadata speedup).

**v3 Results (SLC, 3 tenants, 2026-04-19):**

| Download strategy | Fleet MB/s | Per-tenant | vs v2 |
|---|---|---|---|
| HTTP/2 (v2 baseline) | 12.78 | 4.26 | — |
| HTTP/1.1 5w | 68.56 | 22.85 | **+437%** |
| HTTP/1.1 + Range(4) 5w | **214.46** | **71.49** | **+1,578%** |
| Batch + HTTP/1.1 10w | 125.56 | 41.85 | +883% |
| Batch + Range(4) + HTTP/1.1 5w | 182.55 | 60.85 | +1,329% |

**Single 100MB file with HTTP/1.1 + 2-stream Range**: 66 MB/s (single HTTP/2 was 9.25 MB/s — **7.1x speedup**).

**v3.1 Results (SLC, 3 tenants, 2026-04-20, ALL kernel tuning applied):**

| Download strategy | Fleet MB/s | Per-tenant | vs v2 |
|---|---|---|---|
| A: HTTP/2 5w (v2 baseline) | 97.53 | 32.51 | — |
| B: HTTP/1.1 5w | 117.12 | 39.04 | +20% |
| C: HTTP/1.1 range(4) 5w | 190.54 | 63.51 | +95% |
| **D: batch + HTTP/1.1 10w** | **200.83** | **66.94** | **+106%** |
| E: batch + range(4) + H1 5w | 88.18 | 29.39 | -10% |
| F: pipeline + adaptive + H1 5w | 161.69 | 53.90 | +66% |

**Key findings:**
- HTTP/2 single-stream: 49.70 MB/s — **72% faster than HTTP/1.1** (28.90 MB/s) for single files
- Range 8 streams: 108.47 MB/s peak (was 4-stream default, now 8-stream for >= 50MB files)
- Batch prefetch: 2.9x metadata speedup (758ms vs 2.2s for 10 URLs)
- Upload 60MB chunks: +28% vs 10MB (9.07 vs 6.53 MB/s)
- Worker counts aligned to tenant multiples (1,3,6,12,24) — avoids uneven distribution dip

## SLC TCP Tuning (Prerequisite)

Persistent in `/etc/sysctl.d/99-vaultaire.conf`:
```
net.ipv4.tcp_rmem = 4096 87380 67108864   # 64MB max (was 16MB)
net.ipv4.tcp_wmem = 4096 65536 67108864
net.core.rmem_max = 67108864
net.core.wmem_max = 67108864
net.ipv4.tcp_congestion_control = bbr
```

Verify with `sysctl net.ipv4.tcp_rmem` — should return `67108864` as max.

Without TCP tuning, v3 results drop ~30%. The 64MB buffers are required for the Range/parallel-download optimizations to reach full bandwidth.

### Additional TCP tuning (ALL APPLIED 2026-04-20)

From [Fornax's Guide To Ridiculously Fast Ethernet](https://docs.pixeldrain.com/posts/2024-03-07_network_optimizations/) (pixeldrain's 100Gbps optimization guide). All settings persistent in `/etc/sysctl.d/99-vaultaire.conf`.

```
net.ipv4.tcp_shrink_window = 1              # Cloudflare: prevent buffer bloat
net.ipv4.tcp_slow_start_after_idle = 0      # keep cwnd warm on idle connections
net.ipv4.tcp_fastopen = 3                   # client+server TFO
net.ipv4.tcp_mem = 12897485 16121856 19346227  # 40/50/60% of 123GB RAM
net.ipv4.tcp_tw_reuse = 1                   # reuse TIME_WAIT sockets for burst workloads
```

**Impact:** OneDrive 1-worker 22→101 MB/s (+348%), fleet 12→68 MB/s HTTP/2 (+473%), Lyve ingest 522→650 MB/s (+25%)

**Verify:** `ssh vaultaire-slc 'sysctl net.ipv4.tcp_shrink_window net.ipv4.tcp_slow_start_after_idle net.ipv4.tcp_fastopen net.ipv4.tcp_tw_reuse'`

## Benchmark Tools

6 tools in `cmd/permafrost-*/`, all gitignored.

| Tool | Purpose | Lines |
|------|---------|-------|
| `permafrost-benchmark` | Sequential upload/download (baseline) | ~120 |
| `permafrost-parallel` | Parallel upload (10 workers) | ~135 |
| `permafrost-fleet` | Multi-tenant concurrent (25 workers/tenant), uses Graph SDK | ~280 |
| `permafrost-stress` | Full stress: file size scaling, worker scaling, downloads, mixed, throttle (SDK) | ~536 |
| `permafrost-v2` | Raw HTTP (no SDK), chunked uploads, RU tracking, 7 tests | ~1036 |
| `permafrost-v3` | **Best** — HTTP/1.1 CDN + Range downloads, dual transport, 60MB chunks, batch prefetch, 6 tests | ~1067 |

### Build & Deploy

```bash
# Build for SLC
GOOS=linux GOARCH=amd64 go build -o permafrost-v3-linux ./cmd/permafrost-v3/
scp permafrost-v3-linux vaultaire-slc:/tmp/

# Run on SLC (source env vars first)
ssh vaultaire-slc 'cd /tmp && source .env.bench && ./permafrost-v3-linux'

# Quick local test (all tenants)
source .env.bench
go run ./cmd/permafrost-v3/

# Build+run any tool (substitute tool name)
GOOS=linux GOARCH=amd64 go build -o <tool>-linux ./cmd/<tool>/
scp <tool>-linux vaultaire-slc:/tmp/
ssh vaultaire-slc 'cd /tmp && source .env.bench && ./<tool>-linux'
```

### SLC `.env.bench` — Two Credential Sets

The SLC file at `/tmp/.env.bench` needs BOTH sets of credentials:

1. **S3 provider creds** (Lyve, R2, Quotaless, iDrive, Geyser) — for `bench-compare`
2. **OneDrive tenant creds** (`TENANT_1_ID`, `TENANT_1_CLIENT_ID`, `TENANT_1_SECRET`, `TENANT_1_USER`, etc.) — for `permafrost-*` tools

The Mac-local `.env.bench` has both. The SLC copy was originally created for S3 benchmarks only (April 2026) and the OneDrive tenant vars were appended on 2026-04-19. If SLC's `.env.bench` is recreated, re-sync the tenant vars:

```bash
# From Mac — append OneDrive tenant vars to SLC
grep "^export TENANT" .env.bench | ssh vaultaire-slc 'cat >> /tmp/.env.bench'

# Verify
ssh vaultaire-slc 'grep -c TENANT /tmp/.env.bench'  # should be 12 (4 vars × 3 tenants)
```

### Benchmark Tests (stress tool)

1. **File Size Scaling** — 1/4/10/50/100 MB files, 5 each, measures throughput vs file size
2. **Worker Count Scaling** — 10/25/50/75/100 workers, 50 × 1MB files, finds optimal concurrency
3. **Download Speed** — 1/10/50 MB files, measures restore performance
4. **Mixed Workload** — Simultaneous uploads + downloads across all tenants
5. **Throttle Stress** — 100 workers, 200 × 512KB files, watches for 429 errors

## Prior Benchmark Results

### March 6, 2026 — Pre-TCP-tuning

**Fleet benchmark (3 tenants, 25 workers/tenant):**

| Location | tenant-1 | tenant-2 | tenant-3 | Aggregate |
|----------|----------|----------|----------|-----------|
| MacBook (SLC ISP) | 14.68 | 8.82 | 16.07 | 39.56 MB/s |
| SLC server | 11.00 | 11.10 | 10.90 | 32.99 MB/s |

**Stress test (SLC server, 3 tenants):**

| Test | Result |
|------|--------|
| File size scaling | 8.19 -> 35.02 MB/s (1MB -> 100MB) |
| Best worker count | 25 workers, 30.30 MB/s fleet |
| Download (50MB) | **40.92 MB/s peak** (4x MacBook) |
| Mixed workload | 37.54 MB/s aggregate |
| Throttle stress | **0/200 failures**, ~400/1250 RU |

**15-tenant projections (linear scale):**
- Upload: ~175 MB/s / ~12.6 TB/day
- Download: ~450 MB/s / ~38.9 TB/day
- Storage: 75 TB free

### April 18, 2026 — Post-TCP-tuning + transport optimization (SLC, 2 tenants)

**File size scaling (per-tenant avg):**

| Size | MB/s | vs March |
|------|------|----------|
| 1 MB | 2.73 | ~same (latency-bound) |
| 4 MB | 11.13 | new test |
| 10 MB | 18.31 | new test |
| 50 MB | 28.79 | +146% |
| 100 MB | **30.21** | **+158%** |

**Fleet benchmark (25 workers/tenant):**

| Tenant | April MB/s | March MB/s | Change |
|--------|-----------|-----------|--------|
| tenant-1 | **18.62** | 11.00 | **+69%** |
| tenant-3 | **14.63** | 10.90 | **+34%** |
| Fleet (2 tenants) | **33.25** | 32.99 (3 tenants!) | matches with fewer tenants |

**Other results:**
- Downloads: 34-36 MB/s (50MB files) — comparable to March 40.92 peak
- Mixed workload: **47.99 MB/s** aggregate (+28% vs March 37.54)
- Throttle: 0/200 failures, ~2,297 RU/min (exceeded published limit, no throttle)
- Optimal workers: 25/tenant (confirmed again)

**15-tenant projections (updated):**
- Upload: **~249 MB/s / ~21.5 TB/day** (was ~175 MB/s, +42%)
- Download: ~530 MB/s / ~45.8 TB/day
- Storage: 75 TB free, cost: $0

## Production Driver Status

`internal/drivers/onedrive.go` — 191 lines, scaffold only. All methods return
"not implemented". Has `BatchOperation`, `SplitIntoBatches`, `DriveInfo` structs.

### Implementation priorities (when ready):

1. **Put** — Simple upload (<250MB) + upload session (>=250MB), path-based addressing
2. **Get** — Download via `@microsoft.graph.downloadUrl` (pre-authenticated, no redirect)
3. **List** — Delta queries with token (1 RU vs 2 RU)
4. **Delete** — Batch deletes (20 per batch request)
5. **Throttle handler** — `_graphFetch` wrapper tracking RateLimit headers, decorrelated jitter
6. **Multi-tenant orchestrator** — Health-weighted load balancing, proactive throttle avoidance (80% threshold)
7. **Token management** — Auto-refresh with mutex (prevent concurrent refresh storms)

### Patterns from SnapShelter (Node.js reference implementation)

Key patterns in `snapshelter/src/services/storage-providers/onedrive.js`:

- **`_graphFetch` wrapper**: Captures RateLimit-* headers on every response, handles 429/503
- **Token refresh mutex**: `_refreshPromise` prevents concurrent refresh attempts
- **Chunked uploads**: 10MB chunks aligned to 320KiB boundary, 3-attempt retry per chunk
- **Quota check throttle**: Only check once per 60 seconds during bulk operations
- **`@microsoft.graph.downloadUrl`**: Pre-authenticated URL from item metadata (no 302 redirect)
- **Conflict behavior**: `replace` for generated filenames (UUIDs)

## OneDrive Gotchas

1. **Personal vs Business**: `driveType` field from `/me/drive`. Personal allows 250MB simple upload; Business limits to 4MB
2. **No intermediate folder creation**: Must create folders explicitly before uploading
3. **File name restrictions**: `" * : < > ? / \ |` invalid. Business also restricts `# %`
4. **Path encoding**: Use `encodeURIComponent` per segment. Better: use item IDs after initial upload
5. **Upload session chunks**: MUST be multiples of 320 KiB. Non-aligned chunks silently fail
6. **Upload session auth**: Do NOT send `Authorization` header on chunk PUT requests
7. **Download URLs expire**: `@microsoft.graph.downloadUrl` valid ~1 hour, do not cache
8. **409 on folder create**: Expected when folder exists — not an error
9. **Tenant-2**: Client secret was expired as of March 2026. Rotate at Azure portal

## References

- [Graph API throttling](https://learn.microsoft.com/en-us/sharepoint/dev/general-development/how-to-avoid-getting-throttled-or-blocked-in-sharepoint-online)
- [Upload large files](https://learn.microsoft.com/en-us/graph/sdks/large-file-upload)
- [Delta queries](https://learn.microsoft.com/en-us/graph/delta-query-overview)
- [JSON batching](https://learn.microsoft.com/en-us/graph/json-batching)
- [driveItem: createUploadSession](https://learn.microsoft.com/en-us/graph/api/driveitem-createuploadsession)
- [Graph API error responses](https://learn.microsoft.com/en-us/graph/errors)
