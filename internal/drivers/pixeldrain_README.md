# Pixeldrain (not yet a driver)

Pixeldrain is NOT a Vaultaire storage tier. It's a CDN/cache layer with a
custom REST API. Documented here for cost comparison alongside the other
backends; no driver implementation exists.

## Pricing

| Plan         | Storage       | Egress       | Max file | Expiry    | Notes                          |
|--------------|---------------|--------------|----------|-----------|--------------------------------|
| Free         | unlimited     | 6 GB/day cap | 10 GB    | 60 days   | Throttled after cap            |
| Pro (€4/mo)  | unlimited     | 4 TB/30 days | 100 GB   | 120 days  | Subscription                   |
| **Prepaid**  | **€4/TB/mo**  | **€1/TB**    | **100 GB** | **never** | €0.01/day min, hotlinking OK |

Prepaid is pay-as-you-go from a deposited balance (€10 minimum deposit).
Storage charge = `TB_stored * €4 / 30.4375` per day.

## vs other Vaultaire-integrated backends (per TB/month)

| Provider     | Storage    | Egress        | Notes                              |
|--------------|------------|---------------|------------------------------------|
| iDrive E2    | $3.30/TB   | 3× free       | $0.01/GB beyond 3× stored          |
| Lyve Cloud   | $6.50/TB   | free          | 7 regions, S3-compatible           |
| Geyser       | $1.55/TB   | free          | Tape, LA + London                  |
| Quotaless    | ~$2/TB     | free          | Storj-based                        |
| Cloudflare R2| $15/TB     | free          | Global CDN included                |
| Pixeldrain Prepaid | €4/TB (~$4.30) | €1/TB (~$1.08) | 23 cache servers, 6 PoPs |

## API

Custom REST API (not S3-compatible). HTTP/1.1 only (no HTTP/2, no HTTP/3).

| Endpoint | Method | Purpose |
|---|---|---|
| `/api/file/{name}` | PUT | Upload (recommended over POST multipart) |
| `/api/file/{id}` | GET | Download (supports Range requests) |
| `/api/file/{id}/info` | GET | Metadata (comma-separated IDs, max 1000) |
| `/api/file/{id}` | DELETE | Remove |
| `/api/list` | POST | Create file collection (max 10K files) |
| `/api/list/{id}` | GET | Get list with all file metadata |
| `/api/user/files` | GET | List all user files (undocumented, works) |
| `/api/misc/speedtest` | GET | Raw bandwidth test (undocumented) |

Auth: HTTP Basic (empty username, API key as password).

Rate limit: **3000 requests/minute** (X-Ratelimit headers). Owner bypasses
per-file download rate limits. Hotlinking restricted unless Prepaid/Pro.

## Architecture

Global CDN: 23 caching servers in Portland, Querétaro, New York, São Paulo,
Frankfurt, Singapore. 1.6 Tbps aggregate capacity.

Two server IPs from DNS (23.175.1.x). SLC routes to Portland PoP (~10ms RTT).

## Performance (v6 benchmark, 2026-04-18)

### SLC datacenter (10 Gbps NIC, BBR, Linux amd64)

| Workload | Throughput | Notes |
|---|---|---|
| speedtest (raw network) | 404 MB/s | Single connection ceiling |
| **concurrent_upload_large** (64MB×16) | **581 MB/s** | Exceeds single-conn via parallelism |
| **concurrent_download_large** (64MB×16) | **235–465 MB/s** | Varies by CDN cache node |
| put_1gb (single stream) | 119 MB/s | cyclicReader eliminates rand overhead |
| range_parallel_1gb (16 chunks) | 158 MB/s | 1MB pooled buffers |
| put_100mb | 29 MB/s | |
| get_1gb (single stream) | 105 MB/s | Varies 34–116 by CDN node load |
| burst_small_100 (4KB×100) | 8.5 ops/s (16w), 13.8 ops/s (32w) | More workers helps small files |
| user_files_list (400 files) | 474ms | Fast enough for driver List() |
| list_roundtrip (100 files) | 1.5s | Create 892ms + GET 636ms |
| cache_behavior (10MB) | cold=2.1s warm=2.0s | No measurable cache latency difference |
| mixed_readwrite_20s | up=5.9 + dn=75.4 MB/s | Uploads/downloads coexist cleanly |
| **soak_upload_2m** | [211→224] MB/s | **STABLE — no throttling** |
| **soak_download_2m** | [715→1054] MB/s | **STABLE — ramps UP, peaks 1 GB/s** |

### Mac residential (ARM64, ~64 MB/s ceiling)

| Workload | Throughput |
|---|---|
| concurrent_upload_large (64MB×16) | 50–65 MB/s |
| concurrent_download_large (64MB×16) | 68–77 MB/s |
| put_1gb | 20–28 MB/s |
| get_1gb | 28–59 MB/s |
| soak_upload_2m | [30→105] MB/s STABLE |
| soak_download_2m | [58→109] MB/s STABLE |

### Optimization history (v3 baseline → v7 tuned, SLC)

| Change | Impact |
|---|---|
| cyclicReader (memcpy vs crypto/rand) | put_1gb: 80→119 MB/s (+48%) |
| 1MB transport buffers (was 256KB) | put_100mb: 8.8→29 MB/s (+231%) |
| devNull + 1MB pooled copy buffers | range_parallel: 103→158 MB/s (+54%) |
| Multi-connection warmup | burst_small: 5.4→8.5 ops/s (+57%) |
| delete() body drain fix | Connection reuse: 400 deletes in 10s vs ~120s |
| **TCP buffers 16→64MB + txqueuelen 10000** | **concurrent_dl: 250→808 MB/s (+223%)** |
| **TCP tuning (soak download)** | **Peak 1054 MB/s (1 GB/s), was 506 MB/s** |

## Client transport config

```go
&http.Transport{
    MaxIdleConns:         200,
    MaxIdleConnsPerHost:  200,
    IdleConnTimeout:      90 * time.Second,
    TLSHandshakeTimeout: 10 * time.Second,
    ReadBufferSize:       1 << 20,  // 1MB
    WriteBufferSize:      1 << 20,  // 1MB
    DisableCompression:   true,
    ForceAttemptHTTP2:    true,     // falls back to HTTP/1.1 (pixeldrain is H1 only)
    TLSClientConfig: &tls.Config{
        MinVersion:         tls.VersionTLS12,
        ClientSessionCache: tls.NewLRUClientSessionCache(128),
    },
}
```

Key patterns:
- `devNull` writer (not `io.Discard`) forces CopyBuffer to use 1MB pooled buffer
- `cyclicReader` pre-generates 16MB random block, streams via memcpy
- `io.Copy(io.Discard, resp.Body)` before Close on DELETE for connection reuse
- 16 workers optimal for large files, 32 for small files

## SLC kernel tuning (applied 2026-04-18, persistent)

```
/etc/sysctl.d/99-vaultaire-network.conf:
  net.ipv4.tcp_rmem = 4096 87380 67108864   # was 16MB max → 64MB
  net.ipv4.tcp_wmem = 4096 65536 67108864   # was 16MB max → 64MB
  net.core.rmem_max = 67108864
  net.core.wmem_max = 67108864

/etc/systemd/system/vaultaire-txqueuelen.service:
  ip link set enp1s0f0 txqueuelen 10000     # was 1000
```

Also active: BBR congestion control (`net.ipv4.tcp_congestion_control=bbr`).

Impact: concurrent downloads jumped from 250 → 808 MB/s (+223%). Soak download
peaked at 1054 MB/s (1 GB/s). The 10 Gbps NIC was being throttled by 16MB
TCP window max — 16 parallel connections need 128MB aggregate window space.

## Soak test results (no throttling)

Upload 2 min sustained: `[211, 154, 190, 224] MB/s` — STABLE, slight ramp-up.
Download 2 min sustained: `[715, 1015, 1013, 1054] MB/s` — STABLE, ramps to 1 GB/s.

Pixeldrain does NOT throttle sustained bandwidth for Prepaid accounts.

## FTPS

Port 990 (implicit FTPS) and 21 (plain FTP) are live with Let's Encrypt TLS.
Auth uses same API key (USER=empty, PASS=api_key). Requires a "filesystem"
bucket to be provisioned through pixeldrain web UI — not available on basic
Prepaid without setup. Not benchmarked.

## Driver feasibility

If integrated as a CDN/cache layer:

| Driver method | Pixeldrain mapping | Notes |
|---|---|---|
| `Put` | `PUT /api/file/{name}` | Returns random ID, needs mapping table |
| `Get` | `GET /api/file/{id}` | Look up ID from mapping table |
| `Delete` | `DELETE /api/file/{id}` | Drain body before close |
| `List` | `GET /api/user/files` | Returns all files (474ms for 400) |
| `Exists` | `GET /api/file/{id}/info` | 404 = not found |

Blockers:
- **No user-controlled paths** — IDs are random, need DB mapping (container + path → pixeldrain_file_id)
- **100 GB max file** — large archives need chunking
- **List API as bucket abstraction** — one pixeldrain list per Vaultaire container (max 10K files)
- **Rate limit** — 3000 req/min, implement token bucket
- **No S3 compat** — cannot use existing S3 driver patterns

## Role in Vaultaire

**If integrated**: egress acceleration layer in front of Geyser / iDrive for
publicly-shared files that benefit from edge caching. NOT a primary backend
(slower and more expensive than iDrive for raw storage from US datacenters).

iDrive us-west-2 from SLC: 768 MB/s concurrent ingest, 27ms p50 latency.
Pixeldrain from SLC: 581 MB/s concurrent upload, ~1700ms p50 per-request.

Pixeldrain's value is **global CDN distribution** (6 PoPs), not raw speed.

See `cmd/pixeldrain-bench/` and `bench-results/*-pixeldrain-*.json`.
