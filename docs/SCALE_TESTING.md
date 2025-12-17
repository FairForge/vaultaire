# Scale Testing Guide

This document describes the scale testing infrastructure for Vaultaire and best practices for performance validation.

## Overview

Vaultaire includes a comprehensive load testing package (`internal/loadtest`) that supports multiple testing strategies:

| Test Type | Purpose                 | Duration   | When to Use                     |
| --------- | ----------------------- | ---------- | ------------------------------- |
| Load      | Baseline performance    | 5-30 min   | Before releases, after changes  |
| Stress    | Find breaking points    | 10-60 min  | Capacity planning, new features |
| Spike     | Traffic burst handling  | 5-15 min   | Event preparation, resilience   |
| Soak      | Resource leak detection | 4-24 hours | Before major releases           |
| Chaos     | Failure resilience      | 10-30 min  | Resilience validation           |

## Running Tests

### Prerequisites

```bash
# Ensure test environment is isolated
export VAULTAIRE_ENV=test

# Start the service under test
go run ./cmd/server
```

### Basic Load Test

```go
package main

import (
    "context"
    "time"

    "github.com/FairForge/vaultaire/internal/loadtest"
)

func main() {
    config := loadtest.DefaultConfig("s3-api-test")
    config.Duration = 10 * time.Minute
    config.TargetRPS = 100
    config.MaxConcurrency = 50

    worker := func(ctx context.Context, id int) loadtest.Result {
        start := time.Now()
        // Perform S3 operation
        err := performS3Put(ctx)
        return loadtest.Result{
            StartTime: start,
            Duration:  time.Since(start),
            Error:     err,
        }
    }

    framework := loadtest.NewFramework(config, worker)
    summary, err := framework.Run(context.Background())

    // Analyze results
    analyzer := loadtest.NewBottleneckAnalyzer(nil)
    analysis := analyzer.AnalyzeLoadTest(summary)

    fmt.Println(analysis.GenerateReport())
}
```

## Test Strategies

### 1. Load Testing

**Purpose**: Establish baseline performance metrics

**Configuration**:

- `TargetRPS`: Expected production traffic
- `Duration`: 10+ minutes for stable metrics
- `MaxConcurrency`: Match production connection limits

**Key Metrics**:

- Requests per second achieved
- P95 and P99 latency
- Error rate

### 2. Stress Testing

**Purpose**: Find system breaking points

**Configuration**:

- `StartRPS`: 10% of expected load
- `MaxRPS`: 5-10x expected load
- `RampUpRate`: Gradual increase (10-20 RPS per interval)
- `FailureThreshold`: Acceptable error rate (typically 5%)
- `LatencyThreshold`: Maximum acceptable P99 latency

**Key Metrics**:

- Breaking point RPS
- Latency at breaking point
- Error types at failure

### 3. Spike Testing

**Purpose**: Validate handling of sudden traffic bursts

**Configuration**:

- `BaselineRPS`: Normal traffic level
- `SpikeRPS`: 5-10x baseline
- `SpikeDuration`: 1-5 minutes
- `RecoveryPeriod`: Time to monitor after spike

**Key Metrics**:

- Recovery time
- Error rate during spike
- Post-spike stability

### 4. Soak Testing

**Purpose**: Detect memory leaks and resource exhaustion

**Configuration**:

- `Duration`: 4-24 hours
- `TargetRPS`: Sustainable load (50-70% of breaking point)
- `MemoryThreshold`: Alert threshold (e.g., 2GB)
- `GoroutineThreshold`: Maximum goroutine count

**Key Metrics**:

- Memory growth percentage
- Goroutine growth
- GC frequency
- Long-term error rate stability

### 5. Chaos Testing

**Purpose**: Validate system resilience to failures

**Chaos Types**:

- `ChaosLatency`: Add artificial delays
- `ChaosError`: Inject random errors
- `ChaosTimeout`: Simulate timeouts
- `ChaosPartition`: Simulate network partitions

**Key Metrics**:

- Resilience score (0-100)
- Recovery time
- Error handling effectiveness

## Performance Baselines

### Creating Baselines

```go
manager := loadtest.NewBaselineManager("./baselines")

// After a load test
baseline := manager.CreateBaseline(
    "v1.2.0",           // Name
    "Release 1.2.0",    // Description
    "production",       // Environment
    "1.2.0",           // Version
    summary,           // Test summary
)

manager.SaveToFile("v1.2.0")
```

### Comparing Against Baselines

```go
// After new test
comparison, err := manager.Compare("v1.2.0", newSummary)

if comparison.OverallStatus == loadtest.StatusRegression {
    fmt.Println("REGRESSION DETECTED")
    for _, metric := range comparison.Regressions {
        diff := comparison.Differences[metric]
        fmt.Printf("  %s: %.2f%% change\n", metric, diff.DeltaPct)
    }
}
```

### Threshold Configuration

Default thresholds (can be customized):

| Metric             | Threshold | Meaning                           |
| ------------------ | --------- | --------------------------------- |
| `requests_per_sec` | 10%       | Allow 10% RPS decrease            |
| `avg_latency_ms`   | 15%       | Allow 15% latency increase        |
| `p95_latency_ms`   | 20%       | Allow 20% P95 increase            |
| `p99_latency_ms`   | 25%       | Allow 25% P99 increase            |
| `error_rate`       | 50%       | Allow 50% relative error increase |

## Capacity Planning

### Building Capacity Models

```go
// From stress test results
model := loadtest.BuildModelFromStress("api-service", stressResult)

// Or from soak test results
model := loadtest.BuildModelFromSoak("api-service", soakResult, 100)
```

### Estimating Capacity

```go
planner := loadtest.NewCapacityPlanner(model)

// Estimate for specific RPS
estimate := planner.EstimateCapacity(500)
fmt.Printf("Need %d instances for 500 RPS\n", estimate.RequiredInstances)
fmt.Printf("Estimated latency: %v\n", estimate.EstimatedLatency)
fmt.Printf("Confidence: %.0f%%\n", estimate.Confidence * 100)

// Check current headroom
headroom := planner.CalculateHeadroom(currentRPS)
fmt.Printf("Utilization: %.0f%%\n", headroom.Utilization)
fmt.Printf("Risk level: %s\n", headroom.RiskLevel)
```

### Growth Planning

```go
// Plan for 20% monthly growth over 12 months
plans := planner.PlanForGrowth(100, 20, 12)

for i, plan := range plans {
    fmt.Printf("Month %d: %.0f RPS, %d instances\n",
        i+1, plan.TargetRPS, plan.RequiredInstances)
}
```

## Bottleneck Analysis

### Automatic Detection

```go
analyzer := loadtest.NewBottleneckAnalyzer(nil)

// Analyze different test types
analysis := analyzer.AnalyzeLoadTest(loadSummary)
analysis := analyzer.AnalyzeSoakTest(soakResult)
analysis := analyzer.AnalyzeStressTest(stressResult)

fmt.Printf("Health Score: %.0f/100\n", analysis.HealthScore)
fmt.Println(analysis.GenerateReport())
```

### Bottleneck Types

| Type       | Indicators        | Common Causes                           |
| ---------- | ----------------- | --------------------------------------- |
| Latency    | High P95/P99      | Database queries, lock contention       |
| Throughput | Low RPS           | CPU bottleneck, single-threaded code    |
| Memory     | High growth       | Leaks, large allocations                |
| Goroutine  | High count/growth | Leaked goroutines, missing cancellation |
| GC         | Frequent pauses   | Excessive allocations                   |

## CI/CD Integration

### Pre-merge Testing

```yaml
# .github/workflows/load-test.yml
load-test:
  runs-on: ubuntu-latest
  steps:
    - name: Run Load Test
      run: go test -v ./tests/load/... -tags=loadtest

    - name: Compare Baseline
      run: go run ./cmd/loadtest compare --baseline=main
```

### Release Testing

Run full test suite before releases:

1. Load test (30 min)
2. Stress test (find breaking point)
3. Soak test (4 hours minimum)
4. Compare against previous release baseline

## Performance SLAs

Recommended SLA targets for stored.ge:

| Metric       | Target             | Critical          |
| ------------ | ------------------ | ----------------- |
| Availability | 99.9%              | 99.5%             |
| P50 Latency  | < 50ms             | < 100ms           |
| P99 Latency  | < 500ms            | < 1s              |
| Error Rate   | < 0.1%             | < 1%              |
| Throughput   | > 100 RPS/instance | > 50 RPS/instance |

Configure tests to fail when SLAs are breached:

```go
config := loadtest.DefaultAnalysisConfig()
config.P99LatencyThreshold = 500 * time.Millisecond
config.ErrorRateThreshold = 0.001  // 0.1%
```
