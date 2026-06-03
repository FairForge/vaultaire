# Load Tests — Pre-Launch Gate (Phase 5.15.2)

Authenticated S3 + management API load tests that enforce pass/fail gates.
Env-gated: CI skips these (no running server). Run manually against a live instance.

## Setup

1. Start Vaultaire with a real database:
   ```bash
   PORT=8000 go run ./cmd/vaultaire
   ```

2. Ensure the load test tenant has a `tenant_quotas` row — registration creates
   `users → tenants → api_keys → tenant_quotas` in order. A bench tenant created
   out-of-band often misses the quota row, causing every PUT to 500:
   ```sql
   INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, storage_used_bytes, tier)
   VALUES ('<tenant-id>', 107374182400, 0, 'standard')
   ON CONFLICT (tenant_id) DO NOTHING;
   ```

3. Create the load-test bucket (or let the first test create it via `CreateBucket`).

## Environment Variables

| Variable | Default | Required |
|----------|---------|----------|
| `VAULTAIRE_LOAD_ENDPOINT` | `http://localhost:8000` | No |
| `VAULTAIRE_LOAD_ACCESS_KEY` | falls back to `VAULTAIRE_BENCH_ACCESS_KEY` | Yes |
| `VAULTAIRE_LOAD_SECRET_KEY` | falls back to `VAULTAIRE_BENCH_SECRET_KEY` | Yes |
| `VAULTAIRE_LOAD_BUCKET` | `load-test` | No |
| `VAULTAIRE_LOAD_EMAIL` | — | For management burst test |
| `VAULTAIRE_LOAD_PASSWORD` | — | For management burst test |

## Running

```bash
# All load tests (timeout generous for multipart)
go test ./tests/load/ -v -timeout 10m

# Single scenario
go test ./tests/load/ -v -run TestLoad_ConcurrentPut -timeout 5m
```

## Pass Gates

| Test | Gate |
|------|------|
| ConcurrentPut (100 × 1 MB) | 0 5xx, p99 < 2s |
| ConcurrentGet (100 readers) | 0 5xx, p99 < 500ms |
| Multipart (50 × 100 MB) | 0 5xx |
| MixedReadWrite (100 workers) | 0 5xx, p99 < 2s |
| ManagementBurst (50 rapid) | 429s appear, 0 5xx |
| All tests | goroutine growth < 50 |

## Results

**First run — 2026-06-02, local server (macOS, local-disk backend, PostgreSQL).**
The run surfaced three production bugs (all fixed in the same change) before going green:

| Bug | Symptom | Fix |
|-----|---------|-----|
| **Circuit breaker tripped on benign not-found** (critical) | A burst of 404/not-found deletes recorded 5 "failures" in 60s → breaker opened → **every** request 503'd for 30s. On a single-backend deploy = self-inflicted total outage. | `engine/failover.go`: `isBackendFailure()` — only genuine backend failures (timeout, conn refused, 5xx) trip the breaker; not-found/quota/permission/invalid-input never do. |
| **CreateBucket 403 on idempotent re-create** | A free-tier tenant at its 1-bucket limit got `QuotaExceeded` when re-creating a bucket it already owned → broke `aws s3 mb`/terraform/rclone ensure-bucket. | `api/s3_buckets.go`: skip the quota gate when the tenant already owns the bucket. |
| **Quota-exceeded PUT → 500** | Engine-level quota exhaustion mapped to `InternalError` (500) instead of `QuotaExceeded` (403). | `api/s3_engine_adapter.go`: map `engine.ErrQuotaExceeded` → 403. |

Harness bug also fixed: `patternReader` was not seekable → SigV4 PUT failed client-side (`request stream is not seekable`); now implements `io.ReadSeeker`.

**Clean run after fixes (fresh tenant, quota headroom):** all gates pass, 0 breaker trips.

| Test | Throughput | p99 | Status |
|------|-----------|-----|--------|
| ConcurrentPut (100 × 1 MB) | 116 MB/s | 901 ms | 100/100 ✓ |
| ConcurrentGet (100 readers) | 237 MB/s | 440 ms | 100/100 ✓ |
| Multipart (50 × 100 MB) | 219 MB/s | — | 50/50 ✓ |
| MixedReadWrite (100, 70/30) | 145 MB/s | 722 ms | 100/100 ✓ |
| ManagementBurst (50 rapid) | — | 9 ms | 10×200 + 40×429 ✓ |

Notes:
- Numbers are **local-disk bound** (single SSD, synchronous writes). Prod backends (iDrive ~836 MB/s) and the write path will differ; this run validates *correctness and concurrency behavior*, not prod throughput.
- The DB pool was already bumped to 50/25 (Phase 5.15.2) — no pool exhaustion observed at 100 concurrency.
- **Setup gotcha:** the load tenant needs storage-quota headroom (free tier = 5 GB; the Multipart scenario alone writes 5 GB). Bump `tenant_quotas.storage_limit_bytes` for the test tenant, or use a paid-tier tenant. Do not hand-edit the quota row while the server is running — the QuotaManager upsert can race it.
