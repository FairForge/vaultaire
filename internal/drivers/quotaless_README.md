# Quotaless Storage

## Overview

Privacy-focused cloud storage compatible with S3 (Minio gateway), WebDAV, and rclone.
Operating since 2022. Single anycast IP (130.78.218.170) in London (AS204044 Packet Star
Networks). All endpoints (srv1-8, us, nl, sg, io) resolve to the same IP — likely anycast
routing with virtual host separation, not physically distinct servers.

## Pricing

- Recurring: €60/month (flat service fee, regardless of storage amount)
- One-time: €20 per 10TB (storage capacity pack)
- Bandwidth: unmetered up to 10 Gbit/s
- No data usage fees, no API call charges, no egress fees

Effective cost at scale:
| Storage | Setup (once) | Monthly | €/TB/mo (year 2+) | vs iDrive |
|---------|-------------|---------|-------------------|-----------|
| 10 TB   | €20         | €60     | €6.00             | 1.8x more |
| 18 TB   | €40         | €60     | €3.33             | breakeven |
| 50 TB   | €100        | €60     | €1.20             | 2.8x less |
| 100 TB  | €200        | €60     | €0.60             | 5.5x less |
| 500 TB  | €1,000      | €60     | €0.12             | 28x less  |

## Accounts

### teamviera (current/active)
- Username: teamviera
- Password: in password manager
- Pydio Cells: https://drive.quotaless.cloud
- Infinite Scale: https://cp.quotaless.cloud
- S3 Access Key: in `.env.bench` (QUOTALESS_ACCESS_KEY)
- S3 Secret Key: `gatewaysecret` (fixed for all accounts)
- S3 Bucket: `data`
- S3 Key prefix: `personal-files/`

### isaac (old account)
- Username: isaac
- Credentials: in password manager

## Endpoints

All resolve to 130.78.218.170 (London). Performance difference is endpoint
behavior, not geographic routing.

### S3 (port 8000)

| Endpoint | Type | Multipart | no_head | Performance from SLC |
|----------|------|-----------|---------|---------------------|
| `srv1.quotaless.cloud:8000` | Specific | YES | Optional | **Best: 145 MB/s 8w DL** |
| `srv2-8.quotaless.cloud:8000` | Specific | YES | Optional | Same |
| `us.quotaless.cloud:8000` | Static | YES | Optional | Good |
| `nl.quotaless.cloud:8000` | Static | YES | Optional | Good |
| `sg.quotaless.cloud:8000` | Static | YES | Optional | Good |
| `io.quotaless.cloud:8000` | Dynamic LB | NO | Required | **Half speed: 76 MB/s** |

Key findings:
- srv1 is 2x faster than io for concurrent downloads (LB adds overhead)
- Cross-server consistency is INSTANT (12/12 tests pass)
- All endpoints are functional; prefer srv1 for max throughput

### WebDAV (port 8080)

| Endpoint | Notes |
|----------|-------|
| `us/nl/sg.quotaless.cloud:8080/webdav` | OwnCloud vendor, static |
| `io.quotaless.cloud:8080/webdav` | Dynamic |
| `srv1-8.quotaless.cloud:8080/webdav` | Specific server |
| `rclone.io:2052` | Async proxy (files appear later) |

WebDAV throughput is comparable to S3 (~5-7 MB/s single stream).
Files uploaded via WebDAV are NOT accessible via S3 and vice versa.

## S3 Client Configuration (CRITICAL)

### The problem

AWS SDK v2's default middleware is INCOMPATIBLE with Quotaless's Minio gateway:
- Flexible checksum headers cause data corruption on download
- Streaming payload signing causes connection resets
- ListObjectsV2 returns malformed XML
- CreateMultipartUpload returns malformed XML

### The solution: Raw HTTP + SigV4 with UNSIGNED-PAYLOAD

Zero new dependencies. Use `aws/signer/v4` directly:

```go
import v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

signer := v4.NewSigner()

// CRITICAL: Set x-amz-content-sha256 BEFORE signing
req.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")
signer.SignHTTP(ctx, creds, req, "UNSIGNED-PAYLOAD", "s3", "us-east-1", time.Now())
```

The key insight: the v4 signer does NOT add `x-amz-content-sha256` automatically.
The S3 SDK middleware normally handles this. When using the signer directly,
you MUST set the header yourself before calling SignHTTP.

Reference implementation: `cmd/quotaless-bench-v2/main.go`

### Transport tuning

```go
&http.Transport{
    MaxIdleConns:        200,
    MaxIdleConnsPerHost: 200,
    IdleConnTimeout:     90 * time.Second,
    ReadBufferSize:      256 * 1024,
    WriteBufferSize:     256 * 1024,
    DisableCompression:  true,
    ForceAttemptHTTP2:   true,
    DialContext: (&net.Dialer{
        Timeout:   10 * time.Second,
        KeepAlive: 30 * time.Second,
    }).DialContext,
}
```

### rclone config (for manual/backup use)

```ini
[quotaless]
type = s3
provider = Minio
access_key_id = <QUOTALESS_ACCESS_KEY>
secret_access_key = gatewaysecret
endpoint = https://srv1.quotaless.cloud:8000
acl = bucket-owner-full-control
no_check_bucket = true
upload_cutoff = 100M
chunk_size = 50M
encoding = Slash,InvalidUtf8,Dot,Percent,Ctl
use_multipart_uploads = true
no_head = false
```

For dynamic endpoint (io.), set `use_multipart_uploads = false` and `no_head = true`.

## Benchmark Results (2026-04-18, SLC datacenter)

### Raw HTTP + SigV4 client (production path)

| Workload | Throughput | Notes |
|----------|-----------|-------|
| **Download ceiling** | **393 MB/s** | 64 workers × 4MB, plateau |
| **Upload ceiling** | **235 MB/s** | 16 workers × 16MB, degrades at 32w |
| **Sustained 60s** | **180 MB/s** | 8w × 16MB, zero throttling |
| Range parallel 256MB | 131 MB/s | 16 ranges |
| Range parallel 64MB | 47.5 MB/s | 16 ranges |
| Concurrent GET 32w×16MB | 385 MB/s | |
| Concurrent GET 16w×16MB | 294 MB/s | |
| Concurrent GET 8w×16MB | 145 MB/s | |
| Concurrent GET 8w×1MB | 45 MB/s | |
| Concurrent PUT 16w×16MB | 235 MB/s | |
| Concurrent PUT 8w×1MB | 21 MB/s | |
| Concurrent PUT 4w×16MB | 48 MB/s | |
| Single PUT 64MB | 25 MB/s | |
| Single GET 64MB | 24 MB/s | |
| Single PUT 256MB | 13 MB/s | |
| Single GET 256MB | 26 MB/s | |
| Integrity 16MB | ✓ SHA256 match | |
| Integrity 1MB | ✓ SHA256 match | |
| Integrity 1KB | ✓ SHA256 match | |

### Download scaling curve (SLC → London, 150ms RTT)

| Workers | File size | Throughput | Efficiency |
|---------|----------|-----------|------------|
| 1 | 64MB | 24 MB/s | baseline |
| 4 | 64MB range | 24 MB/s | 1.0x |
| 8 | 16MB | 145 MB/s | 6.0x |
| 8 | 64MB range | 43 MB/s | 1.8x |
| 16 | 16MB | 294 MB/s | 12.2x |
| 32 | 16MB | 385 MB/s | 16.0x |
| 64 | 4MB | 393 MB/s | 16.4x ← ceiling |

Ceiling is ~393 MB/s (3.1 Gbit/s) — likely transatlantic transit capacity.
EU users would see higher throughput (lower RTT, same server).

### Upload scaling curve

| Workers | File size | Throughput |
|---------|----------|-----------|
| 4 | 1MB | 11 MB/s |
| 8 | 1MB | 21 MB/s |
| 4 | 16MB | 48 MB/s |
| 16 | 16MB | 235 MB/s ← sweet spot |
| 32 | 4MB | 8.5 MB/s ← DEGRADES (errors) |

Upload degrades past 16 workers — likely server-side connection limit.

### Cross-server consistency

Write to srv1, immediately read from all others:
srv1 ✓ | srv2 ✓ | us ✓ | nl ✓ | sg ✓ | io ✓
Tested from both SLC and Mac: 12/12 pass. Replication is instant.

### Endpoint comparison (concurrent 8w×16MB GET)

| Endpoint | Throughput | Why |
|----------|-----------|-----|
| srv1 | 145 MB/s | Direct to server |
| io | 76 MB/s | Load balancer overhead halves speed |

Always use srv1 (or specific server) for maximum throughput.

### rclone vs raw HTTP (same workload, same endpoint)

| Client | Concurrent 8w×1MB | Single 64MB PUT |
|--------|------------------|-----------------|
| rclone subprocess | 3.3 MB/s | 8.9 MB/s |
| Raw HTTP + SigV4 | 21.7 MB/s | 24.7 MB/s |
| **Improvement** | **6.6x** | **2.8x** |

Raw HTTP eliminates subprocess fork/exec overhead and enables connection reuse.

## Best Practices

### For the Vaultaire production driver

1. **Use srv1 endpoint** — 2x faster than io for downloads
2. **16 workers max for uploads** — degrades at 32 (connection limit)
3. **32 workers for downloads** — scales to ~385 MB/s
4. **16MB file operations** — best throughput per connection
5. **x-amz-content-sha256: UNSIGNED-PAYLOAD** — required for Minio compatibility
6. **Set header BEFORE v4.SignHTTP()** — signer doesn't add it automatically
7. **Key prefix: personal-files/** — all objects must be under this root
8. **Bucket: data** — fixed, don't try to create other buckets
9. **Retry with backoff on PUT** — occasional connection resets (not data corruption)
10. **Don't use AWS SDK v2 S3 client** — middleware is incompatible

### For multi-tenant Vaultaire

1. **Tenant isolation via key prefix**: `personal-files/{tenant_id}/{bucket}/{key}`
2. **Egress monitoring**: Track per-tenant download bytes for billing
3. **Automatic routing**: When tenant's iDrive egress exceeds 3x stored,
   replicate hot files to Quotaless for free egress
4. **Capacity planning**: Buy 10TB packs in advance of growth
5. **Multiple accounts**: Consider separate Quotaless accounts per
   large tenant for isolation (each with own €60/mo)

### For reliability

1. **POST-PUT verification** (optional): After PUT, GET and verify SHA256
2. **Retry on connection reset**: Up to 3 attempts with exponential backoff
3. **Health check**: TCP dial to srv1.quotaless.cloud:8000 (same pattern as other backends)
4. **Fallback**: If Quotaless fails, serve from iDrive mirror
5. **Monitor**: Track error rate, p95 latency, throughput per tenant

## Role in Vaultaire

### Primary use: Egress backstop

```
Tenant uploads 50TB → stored on iDrive ($3.30/TB)
Tenant downloads 300TB/mo (6x ratio)

WITHOUT Quotaless:
  iDrive egress overage: (300-150) × $0.01/GB = $1,500/mo
  Total cost: $165 storage + $1,500 egress = $1,665/mo
  Revenue: $199.50 → LOSS of $1,465/mo

WITH Quotaless backstop:
  Vaultaire detects >3x egress ratio
  Hot files auto-replicate to Quotaless
  Downloads served from Quotaless (free egress)
  Total cost: $165 iDrive + ~$65 Quotaless = $230/mo
  Revenue: $199.50 → manageable $30 loss (vs $1,465)
```

### Tier architecture

| Tier | Backend | $/TB | When |
|------|---------|------|------|
| Standard | iDrive E2 | $3.30 | Default, <50TB, latency-sensitive |
| **Bulk/Egress** | **Quotaless** | **€0.60** | **>50TB total OR high-egress tenants** |
| Archive | Geyser | $1.55 | Cold backups, compliance |
| Deep Archive | Vault18 | $1.00 | 7+ year retention |
| CDN/Public | R2 | $15.00 | Public buckets, global CDN |
| CDN/Specialty | Pixeldrain | €4.00 | Branded share links, FTPS |

### Smart routing logic

```
ON WRITE:
  → iDrive (default, fast)

ON READ:
  if tenant.egress_30d > tenant.stored * 3:
    if object not on Quotaless:
      background_replicate(object, quotaless)
    serve from Quotaless (free egress)
  else:
    serve from iDrive (fast)

ON COLD (no access 30 days):
  → migrate to Geyser (cheaper archive)

ON VERY COLD (no access 365 days):
  → migrate to Vault18 (cheapest)
```

## Benchmark Tools

| Tool | Purpose | Location |
|------|---------|----------|
| Raw HTTP bench | Production-path testing | `cmd/quotaless-bench-v2/main.go` |
| rclone bench | Compatibility testing | `cmd/quotaless-full-bench/bench.sh` |
| bench-compare | Cross-provider comparison | `cmd/bench-compare/main.go` |
| Quotaless driver bench | Legacy (uses broken S3Driver) | `cmd/quotaless-bench/main.go` |

### Bench results

| File | Description |
|------|-------------|
| `bench-results/quotaless-slc-ceiling.json` | Full ceiling test (SLC, raw HTTP) |
| `bench-results/quotaless-slc-raw-v2.json` | Standard workload (SLC, raw HTTP) |
| `bench-results/quotaless-mac-ceiling.json` | Mac residential test |
| `bench-results/slc-quotaless-large-20260418.json` | SDK v2 test (shows failures) |
| `bench-results/quotaless-*/results.csv` | rclone-based tests |

## What NOT to do

1. **Don't use aws-sdk-go-v2 S3 client** — checksum middleware corrupts data
2. **Don't use minio-go** — maintenance-only mode
3. **Don't use io. endpoint for throughput** — LB halves download speed
4. **Don't exceed 16 concurrent uploads** — degrades with errors at 32+
5. **Don't rely on HEAD for ETags** — ETags are unreliable via SDK
6. **Don't try multipart on io. endpoint** — not supported
7. **Don't create custom buckets** — use `data` bucket with key prefixes
8. **Don't forget the x-amz-content-sha256 header** — 403 without it
