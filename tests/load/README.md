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

Paste before/after numbers here after running:

```
(pending — fill in after first run against local/SLC server)
```
