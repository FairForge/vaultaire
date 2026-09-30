# Scale Testing

> **The launch gate is `tests/load/README.md`.** Five authenticated scenarios
> with pass/fail thresholds, run by hand against a live instance (CI skips
> them). Everything else on this page — the nightly k6 job and the
> `internal/loadtest` library — is informational.

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

## Nightly (`.github/workflows/nightly.yml`)

07:00 UTC daily (and `workflow_dispatch`): PostgreSQL 15 service, migrations,
`go build`, server started with `JWT_SECRET` and `DATABASE_URL`, then

1. `go test -bench=. -benchtime=10s ./tests/benchmarks/`
2. `k6 run --quiet tests/k6/s3_basic_load.js` (other scripts in `tests/k6/`:
   `s3_realistic_load.js`, `tenant_isolation_load.js`, `resource_monitor.js`)
3. `./tests/chaos/basic_chaos_test.sh` and `go test ./tests/chaos/`

Every step is `continue-on-error`; results are uploaded as
`nightly-results-<run>` (`tests/benchmarks/baseline_results.txt`,
`tests/k6/*.json`). Nothing fails the workflow — read the artifact.

## Running a server for a manual test

```bash
make build && JWT_SECRET=dev PORT=8000 ./bin/vaultaire      # or: go run ./cmd/vaultaire
```

No `VAULTAIRE_ENV`, no config file: everything is env vars (`docs/CONFIG.md`).
With no backend credentials the primary is the `local` driver under
`DATA_PATH` (default `/tmp/vaultaire-data`). With a database, register a
tenant (`POST /auth/register`) and use the returned key pair; without one the
server serves S3 on the no-DB fallback path and HEAD answers 503.

## The `internal/loadtest` library

A Go package for writing your own load, stress, spike, soak and chaos runs
against any worker function. Signatures as of `internal/loadtest/framework.go`:

```go
type TestType string            // TestTypeLoad, TestTypeStress, TestTypeSpike, TestTypeSoak
type WorkerFunc func(ctx context.Context, workerID int) Result

func DefaultConfig(name string, testType TestType) *Config   // Duration, RampUp/RampDown, TargetRPS, MaxConcurrency, Timeout
func New(config *Config, workerFunc WorkerFunc) *Framework
func (f *Framework) Run(ctx context.Context) (*Summary, error)
```

```go
package main

import (
    "context"
    "fmt"
    "time"

    "github.com/FairForge/vaultaire/internal/loadtest"
)

func main() {
    cfg := loadtest.DefaultConfig("s3-put", loadtest.TestTypeLoad)
    cfg.Duration = 10 * time.Minute
    cfg.TargetRPS = 100
    cfg.MaxConcurrency = 50

    worker := func(ctx context.Context, id int) loadtest.Result {
        start := time.Now()
        err := performS3Put(ctx) // your SigV4 client
        return loadtest.Result{StartTime: start, Duration: time.Since(start), Error: err}
    }

    summary, err := loadtest.New(cfg, worker).Run(context.Background())
    if err != nil {
        panic(err)
    }
    analysis := loadtest.NewBottleneckAnalyzer(nil).AnalyzeLoadTest(summary)
    fmt.Println(analysis.GenerateReport())
}
```

The other testers follow the same shape — `DefaultStressConfig(name)` /
`NewStressTester(cfg, worker)` (ramps `StartRPS`→`MaxRPS`, stops at
`FailureThreshold` or `LatencyThreshold`), `DefaultSpikeConfig` /
`NewSpikeTester` (`BaselineRPS`, `SpikeRPS`, recovery time), `DefaultSoakConfig`
/ `NewSoakTester` (memory / goroutine / GC sampling with thresholds),
`DefaultChaosConfig` / `NewChaosTester` (latency, error, timeout and partition
injection with a resilience score). Each `Run` returns its own result type
that `BottleneckAnalyzer` (`AnalyzeStressTest`, `AnalyzeSoakTest`) and the
report generators accept.

Baselines and capacity models are library calls too, not a CLI:

```go
bm := loadtest.NewBaselineManager("./baselines")
bm.CreateBaseline("v1.2.0", "Release 1.2.0", "production", "1.2.0", summary)
_ = bm.SaveToFile("v1.2.0")
cmp, _ := bm.Compare("v1.2.0", newSummary)     // cmp.OverallStatus, cmp.Regressions, cmp.Differences
fmt.Println(cmp.GenerateReport())

model := loadtest.BuildModelFromStress("api", stressResult)   // or BuildModelFromSoak(name, soakResult, targetRPS)
p := loadtest.NewCapacityPlanner(model)
_ = p.EstimateCapacity(500)          // RequiredInstances, EstimatedLatency, Confidence
_ = p.CalculateHeadroom(currentRPS)  // Utilization, RiskLevel
_ = p.PlanForGrowth(100, 20, 12)     // 20 %/month for 12 months
```

Default baseline thresholds: RPS −10 %, avg latency +15 %, p95 +20 %, p99
+25 %, error rate +50 % relative (`BaselineManager.SetThreshold` to change).

There is no `cmd/loadtest`, no `compare` sub-command and no pre-merge load
workflow; a "load-test on every PR" job would need a running server and is
not what CI does.

## SLA targets

`loadtest.DefaultStorageSLA()` (`internal/loadtest/sla.go`) encodes the targets
the launch copy promises nothing beyond ("no contractual SLA, target 99.5 %"
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
