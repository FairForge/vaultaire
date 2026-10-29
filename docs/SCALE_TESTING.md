# Scale Testing

> **The launch gate is `tests/load/README.md`.** Five authenticated scenarios
> with pass/fail thresholds, run by hand against a live instance (CI skips
> them). Everything else on this page — the nightly k6 job — is
> informational. The `internal/loadtest` library and its `cmd/tools/loadtest`
> harness were deleted (decision D-7, WP-R0-1, Stage 1a Prompt 52E): git has
> them.

## The gates (`tests/load/`)

`go test ./tests/load/ -v -timeout 10m` against a running server. Env:
`VAULTAIRE_LOAD_ENDPOINT` (default `http://localhost:8000`),
`VAULTAIRE_LOAD_ACCESS_KEY` / `_SECRET_KEY` (fall back to
`VAULTAIRE_BENCH_*`), `VAULTAIRE_LOAD_BUCKET` (default `load-test`), and
`VAULTAIRE_LOAD_EMAIL` / `_PASSWORD` for the management burst. The load
tenant needs a `tenant_quotas` row with headroom (the multipart scenario alone
writes 5 GB; the free tier is 5 GB).

| Test | Gate |
|------|------|
| `TestLoad_ConcurrentPut` (100 × 1 MB) | 0 5xx, p99 < 2 s |
| `TestLoad_ConcurrentGet` (100 readers) | 0 5xx, p99 < 500 ms |
| `TestLoad_Multipart` (50 × 100 MB) | 0 5xx |
| `TestLoad_MixedReadWrite` (100 workers, 70/30) | 0 5xx, p99 < 2 s |
| `TestLoad_ManagementBurst` (50 rapid) | 429s appear, 0 5xx |
| all | goroutine growth < 50 |

`TestLoad_Uploader_PartSize` checks the multipart uploader's part sizing.
Last production run: 2026-08-03 on the production host, all gates pass; the
table, the 6.2 GB RSS transient during 50 concurrent multipart completes and
the rerun recipe are in `bench-results/LOADTEST-2026-08-03.md`. The three
production bugs the first run found (breaker tripped by 404s, idempotent
re-create 403, quota-exceeded 500) are recorded in the README.

Run it before a release that touches the write path, the quota path, the
breaker or the DB pool. Numbers on a laptop are local-disk bound; the gates
validate correctness and concurrency behaviour, not production throughput.

## Nightly workflow — removed (Review R15)

`nightly.yml` and the `tests/{benchmarks,k6,chaos}` suites it ran were deleted:
every one of them hit the server **unauthenticated** (403 on every PUT — the
benchmark step had been failing on `upload failed: 403` and the k6 error
threshold tripping on every run), and every step was `continue-on-error`, so
the workflow reported green for months while measuring nothing. The gates are
`tests/load/` (SigV4, env-gated) and the in-process synthetic customer check
(`SYNTHETIC_CHECK_*`, R13).

## Running a server for a manual test

```bash
make build && JWT_SECRET=dev PORT=8000 ./bin/vaultaire      # or: go run ./cmd/vaultaire
```

No `VAULTAIRE_ENV`, no config file: everything is env vars (`docs/CONFIG.md`).
With no backend credentials the primary is the `local` driver under
`DATA_PATH` (default `/tmp/vaultaire-data`). With a database, register a
tenant (`POST /auth/register`) and use the returned key pair; without one the
server serves S3 on the no-DB fallback path and HEAD answers 503.

There is no `cmd/loadtest`, no `compare` sub-command and no pre-merge load
workflow; a "load-test on every PR" job would need a running server and is
not what CI does.

## SLA targets

These are the targets (they lived in the deleted `internal/loadtest`'s
`DefaultStorageSLA()`); the launch copy promises nothing beyond them ("no contractual SLA, target 99.5 %"
in the FAQ):

| Objective | Target | Priority |
|-----------|--------|----------|
| Availability (30 d) | ≥ 99.9 % | critical |
| p50 latency | ≤ 50 ms | medium |
| p95 latency | ≤ 200 ms | high |
| p99 latency | ≤ 500 ms | critical |
| Error rate | ≤ 0.1 % | critical |
| Throughput | ≥ 100 RPS per instance | high |

`loadtest.NewSLAValidator(sla).Validate(summary)` returns per-objective
results with `GenerateReport()` and `GetFailedCritical()`; custom SLAs are
built with `CreateCustomSLA` and the `NewLatencySLO` / `NewErrorRateSLO` /
`NewThroughputSLO` constructors. `DefaultAnalysisConfig()` holds the
bottleneck thresholds (`AvgLatencyThreshold`, `ErrorRateThreshold`,
`MemoryGrowthThreshold`, `GoroutineThreshold`, `GCPauseThreshold`).
