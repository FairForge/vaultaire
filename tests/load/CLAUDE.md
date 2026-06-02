# tests/load

Pre-launch load testing gate (Phase 5.15.2). Env-gated — all tests skip when `VAULTAIRE_LOAD_ACCESS_KEY` is unset, so CI stays green.

## Files

| File | Purpose |
|------|---------|
| `harness.go` | S3 client (aws-sdk-go-v2, SigV4, path-style), metrics collector, pattern data generator, JWT helper, goroutine leak checker |
| `s3_load_test.go` | Five scenarios: ConcurrentPut, ConcurrentGet, Multipart, MixedReadWrite, ManagementBurst |
| `README.md` | Setup instructions, env vars, pass gates, results placeholder |

## Key Design Decisions

- **Go harness, not k6/hey** — reuses `aws-sdk-go-v2` (already a dep), lives with the code, SigV4 auth built-in.
- **Env-gated skip** — mirrors the same pattern used in integration tests. No server = `t.Skip`.
- **Pattern reader** — repeating 4KB pattern avoids heap allocation for large payloads (100 MB multipart).
- **Gate assertions inside `metrics.report()`** — zero 5xx and p99 thresholds. ManagementBurst has custom 429 assertions.
- **Cleanup is best-effort** — `deleteObjects` logs errors but doesn't fail the test.

## Running

```bash
VAULTAIRE_LOAD_ACCESS_KEY=... VAULTAIRE_LOAD_SECRET_KEY=... go test ./tests/load/ -v -timeout 10m
```
