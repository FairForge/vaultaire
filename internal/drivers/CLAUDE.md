# internal/drivers

Storage backend implementations for the Vaultaire engine. Every driver implements `engine.Driver` (defined in `internal/engine/interface.go`).

## Driver Interface Contract

```go
type Driver interface {
    Name() string
    Get(ctx context.Context, container, artifact string) (io.ReadCloser, error)
    Put(ctx context.Context, container, artifact string, data io.Reader, opts ...PutOption) error
    Delete(ctx context.Context, container, artifact string) error
    List(ctx context.Context, container string, prefix string) ([]string, error)
    Exists(ctx context.Context, container, artifact string) (bool, error)
    HealthCheck(ctx context.Context) error
}
```

`driver.go` re-exports `engine.Driver` as a local type alias and defines `PutOption` functional options (`WithContentType`, `WithUserMetadata`).

## Driver Implementations

### Wired in `cmd/vaultaire/main.go` (production)

| Driver | File | Constructor | Backend | Key Config |
|--------|------|-------------|---------|------------|
| `LocalDriver` | `local.go` | `NewLocalDriver(basePath, logger)` | Local filesystem | `DATA_PATH` env var; reader/file pools; transactions; buffered writes; multipart uploads; file locking; sparse files; xattrs; directory indexing |
| `S3CompatDriver` | `s3compat.go` | `NewS3CompatDriver(accessKey, secretKey, logger)` | S3-compatible (maps to Quotaless endpoint) | Hardcoded `us.s3compat.cloud:8000`; optional `S3COMPAT_INSECURE_TLS` for self-signed certs |
| `LyveDriver` | `lyve.go` | `NewLyveDriver(accessKey, secretKey, tenantID, region, logger)` | Seagate Lyve Cloud | Auto-builds `s3.{region}.global.lyve.seagate.com` endpoint; tenant isolation via context key |
| `QuotalessDriver` | `quotaless.go` | `NewQuotalessDriver(accessKey, secretKey, endpoint, logger)` | Quotaless | Embeds `*S3Driver`; 50 MB chunks; 100 MB multipart cutoff; static vs dynamic endpoint detection |
| `GeyserDriver` | `geyser.go` | `NewGeyserDriver(accessKey, secretKey, bucket, tenantID, logger, ...GeyserOption)` | Spectra Logic Vail (LTO-9 tape) | Default LA endpoint; `WithGeyserEndpoint` for London; 64 MB spill threshold (RAM vs temp file); generous HTTP timeouts for tape ops |

| `IDriveDriver` | `idrive.go` | `NewIDriveDriver(accessKey, secretKey, endpoint, region, logger)` | iDrive E2 | Fixed-bucket + key-prefix pattern (like Geyser); `IDRIVE_BUCKET` env var (default `vaultaire`); materialize for Content-Length; ContentLength passthrough skips materialize when size known; `EgressTracker` field (not wired) |
| `OneDriveDriver` | `onedrive.go` | `NewOneDriveFleetDriver(logger)` | Microsoft OneDrive (Graph API) | Multi-tenant fleet (TENANT_N_* env vars, N=1..15); raw HTTP + azidentity (no Graph SDK); dual transport (HTTP/2 API, HTTP/1.1 CDN); 60MB chunked uploads; streaming uploads (ContentLength passthrough); parallel byte-range downloads (adaptive 1/2/4/8 streams); fleet-wide TLS cache + DNS cache; token refresh mutex; RateLimit header tracking; pooled 1MB drain buffers |

### iDrive Region Registry (Phase 5.14.7)

`idrive_regions.go` — static map of iDrive e2 region identifiers to S3-compatible endpoints. Used by `cmd/vaultaire/main.go` to register one `IDriveDriver` per region (`idrive-{region}`), enabling per-bucket data residency.

Helpers: `IsValidRegion(region)`, `IsEURegion(region)` (true for `eu-*`), `RegionDisplayName(region)`. Called from S3 API (`CreateBucket` validation), dashboard (`HandleCreateBucket`, `HandleBucketSettings`), and engine adapter (`bucketRegionDriver` routing).

### Not wired in main.go (scaffolds / future)

| Driver | File | Constructor | Backend | Status |
|--------|------|-------------|---------|--------|
| `S3Driver` | `s3.go` | `NewS3Driver(endpoint, accessKey, secretKey, region, logger)` | Generic AWS S3 | Used as base for `QuotalessDriver` (embedded); not directly wired |

## Shared Utilities

### Transport

| File | Type | Purpose |
|------|------|---------|
| `transport.go` | `TunedHTTPClient` | Shared HTTP client factory with connection pooling (200 conns), 4MB I/O buffers, DNS caching, TLS session resumption. Used by all S3-compatible drivers (Lyve, Geyser, iDrive, S3, S3compat). Options: `WithInsecureTLS()`, `WithResponseHeaderTimeout()`, `WithHTTP1Only()`. Disable via `VAULTAIRE_TUNED_TRANSPORT=false`. OneDrive has its own transports (odGraphTransport, odCDNTransport). |

### Resilience & Orchestration

| File | Type | Purpose |
|------|------|---------|
| `health.go` | `HealthChecker` | Registers named health checks, runs them concurrently with timeout, returns `HealthReport` (healthy/degraded/unhealthy) |
| `circuit_breaker.go` | `CircuitBreaker` | Closed/Open/HalfOpen states; configurable failure/success thresholds and reset timeout |
| `retry.go` | `RetryPolicy`, `RetryableDriver` | Exponential backoff with jitter; wraps any `Driver` with automatic retries |
| `fallback.go` | `FallbackDriver` | Wraps primary + secondary `Driver`; tries primary first, falls back on failure |
| `regional_failover.go` | `RegionalFailover` | Automatic failover between two `RegionDriver` instances with health monitoring |
| `throttle.go` | `ThrottledDriver` | Wraps any `Driver` with `rate.Limiter`-based bandwidth throttling |
| `parallel.go` | `ParallelDriver` | Worker-pool concurrency for bulk `Put`/`Get` operations |
| `queue.go` | `RequestQueue` | Bounded work queue with worker pool; `ErrQueueFull` / `ErrQueueClosed` |

### Bandwidth & Cost

| File | Type | Purpose |
|------|------|---------|
| `egress_tracker.go` | `EgressTracker` | Per-tenant byte + cost tracking; default rate $0.009/GB (iDrive E2) |
| `egress_predictor.go` | `EgressPredictor` | Daily/monthly usage prediction with quota alerts (Info/Warning/Critical) |
| `bandwidth_quota.go` | `BandwidthQuota` | Monthly egress quotas per tenant with automatic reset |
| `cost_advisor.go` | `CostAdvisor` | Analyzes file access patterns; recommends compression, dedup, tier migration |

### Caching & Streaming

| File | Type | Purpose |
|------|------|---------|
| `smart_cache.go` | `SmartCache` | LRU cache with tenant isolation; tracks hits/misses/evictions |
| `compression.go` | `CompressionDriver` | Wraps any `Driver`; gzip-compresses on Put, decompresses on Get |
| `reader_pool.go` | `ReaderPool` | `sync.Pool`-based reader reuse |
| `parallel_stream.go` | `StreamManager` | Manages concurrent download streams |
| `parallel_chunks.go` | `ChunkReader` | Parallel chunk reading for large objects |
| `chunked_transfer.go` | `ChunkedTransfer` | Chunked upload/download management |
| `resumable.go` | `ResumableUpload` | Upload checkpoint + resume via temp file metadata |

### S3 Protocol

| File | Type | Purpose |
|------|------|---------|
| `s3_auth.go` | `S3Signer` | AWS Signature V4 implementation for raw HTTP signing |
| `s3_iam.go` | `PolicyEvaluator`, `IAMPolicy`, `STSToken` | IAM policy parsing and action/resource evaluation |

### Local Driver Extras

| File | Purpose |
|------|---------|
| `locking_unix.go` | `flock`-based file locking (`FileLock`) |
| `sparse_unix.go`, `sparse_fallback.go` | Sparse file support (SEEK_HOLE/SEEK_DATA on Unix, fallback on others) |
| `xattr_unix.go`, `xattr_other.go` | Extended attributes (platform-conditional) |
| `watch.go` | `Watcher` interface + `WatchEvent` types (Create/Modify/Delete/Rename) |

### Extensibility

| File | Type | Purpose |
|------|------|---------|
| `capabilities.go` | `CapabilityChecker` | Interface for drivers to declare capabilities (streaming, range read, multipart, versioning, encryption, replication, watch, atomic) |
| `conflict.go` | `ConflictDetector`, `ConflictResolver` | Version-based conflict detection for concurrent writes |
| `wasm.go` | `WASMPlugin` | WASM plugin execution via wazero (future compute-at-edge) |
| `webhook.go` | `WebhookDispatcher` | Dispatches `WatchEvent`s to registered webhook URLs |

### Geyser Admin

| File | Type | Purpose |
|------|------|---------|
| `geyser_admin.go` | `GeyserAdminClient` | Console API client for bucket provisioning, airgap, billing, keepalive. Reverse-engineered; requires manual session cookie setup. |

### Test Helpers

| File | Purpose |
|------|---------|
| `test_helpers.go` | `mustClose`, `mustCopy`, `mustWrite` convenience functions for tests |
| `conformance_test.go` | Cross-driver conformance test suite |

## Existing READMEs

- `idrive_README.md` -- iDrive E2 integration + reseller API reference
- `onedrive_README.md` -- OneDrive integration + dual-transport pattern (HTTP/2 for API, HTTP/1.1 for CDN)
- `quotaless_README.md` -- Quotaless backend ops manual
- `pixeldrain_README.md` -- Pixeldrain benchmarks (CDN option, not a storage tier)
