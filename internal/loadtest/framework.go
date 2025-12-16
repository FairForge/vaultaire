// Package loadtest provides infrastructure for load, stress, and chaos testing.
package loadtest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// TestType defines the kind of load test being run.
type TestType string

const (
	TestTypeLoad   TestType = "load"   // Sustained normal load
	TestTypeStress TestType = "stress" // Beyond normal capacity
	TestTypeSpike  TestType = "spike"  // Sudden traffic bursts
	TestTypeSoak   TestType = "soak"   // Extended duration
)

// Config defines load test parameters.
type Config struct {
	Name             string
	Type             TestType
	Duration         time.Duration
	RampUpDuration   time.Duration
	RampDownDuration time.Duration
	TargetRPS        int           // Requests per second
	MaxConcurrency   int           // Max concurrent workers
	Timeout          time.Duration // Per-request timeout
}

// DefaultConfig returns sensible defaults for load testing.
func DefaultConfig(name string, testType TestType) *Config {
	return &Config{
		Name:             name,
		Type:             testType,
		Duration:         5 * time.Minute,
		RampUpDuration:   30 * time.Second,
		RampDownDuration: 15 * time.Second,
		TargetRPS:        100,
		MaxConcurrency:   50,
		Timeout:          30 * time.Second,
	}
}

// Result captures metrics from a single request.
type Result struct {
	StartTime  time.Time
	Duration   time.Duration
	StatusCode int
	BytesSent  int64
	BytesRecv  int64
	Error      error
	Labels     map[string]string
}

// Summary aggregates results from a load test run.
type Summary struct {
	TestName       string
	TestType       TestType
	StartTime      time.Time
	EndTime        time.Time
	TotalRequests  int64
	SuccessCount   int64
	FailureCount   int64
	TotalBytes     int64
	MinLatency     time.Duration
	MaxLatency     time.Duration
	AvgLatency     time.Duration
	P50Latency     time.Duration
	P95Latency     time.Duration
	P99Latency     time.Duration
	RequestsPerSec float64
	ErrorRate      float64
	Errors         map[string]int64
}

// WorkerFunc is the function each worker executes.
// It should perform one unit of work and return a Result.
type WorkerFunc func(ctx context.Context, workerID int) Result

// Framework orchestrates load test execution.
type Framework struct {
	config     *Config
	workerFunc WorkerFunc
	results    chan Result

	// Metrics (atomic for thread safety)
	totalRequests atomic.Int64
	successCount  atomic.Int64
	failureCount  atomic.Int64
	totalBytes    atomic.Int64

	// State
	mu        sync.RWMutex
	running   bool
	startTime time.Time
	latencies []time.Duration
	errors    map[string]int64
}

// New creates a new load testing framework.
func New(config *Config, workerFunc WorkerFunc) *Framework {
	if config == nil {
		config = DefaultConfig("default", TestTypeLoad)
	}

	return &Framework{
		config:     config,
		workerFunc: workerFunc,
		results:    make(chan Result, config.MaxConcurrency*10),
		latencies:  make([]time.Duration, 0, 10000),
		errors:     make(map[string]int64),
	}
}

// Run executes the load test and returns a summary.
func (f *Framework) Run(ctx context.Context) (*Summary, error) {
	f.mu.Lock()
	if f.running {
		f.mu.Unlock()
		return nil, fmt.Errorf("load test already running")
	}
	f.running = true
	f.startTime = time.Now()
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		f.running = false
		f.mu.Unlock()
	}()

	// Create cancellable context with test duration
	testCtx, cancel := context.WithTimeout(ctx, f.config.Duration)
	defer cancel()

	// Start result collector
	collectorDone := make(chan struct{})
	go f.collectResults(collectorDone)

	// Start workers
	var wg sync.WaitGroup
	workerCtx, workerCancel := context.WithCancel(testCtx)
	defer workerCancel()

	// Rate limiter for target RPS
	ticker := time.NewTicker(time.Second / time.Duration(f.config.TargetRPS))
	defer ticker.Stop()

	// Worker pool
	semaphore := make(chan struct{}, f.config.MaxConcurrency)
	workerID := atomic.Int64{}

	// Main loop - spawn workers at target rate
	for {
		select {
		case <-workerCtx.Done():
			goto cleanup
		case <-ticker.C:
			select {
			case semaphore <- struct{}{}:
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					defer func() { <-semaphore }()

					result := f.workerFunc(workerCtx, id)
					select {
					case f.results <- result:
					case <-workerCtx.Done():
					}
				}(int(workerID.Add(1)))
			default:
				// At max concurrency, skip this tick
			}
		}
	}

cleanup:
	// Wait for all workers to finish
	wg.Wait()
	close(f.results)
	<-collectorDone

	return f.buildSummary(), nil
}

// collectResults aggregates results from workers.
func (f *Framework) collectResults(done chan struct{}) {
	defer close(done)

	for result := range f.results {
		f.totalRequests.Add(1)
		f.totalBytes.Add(result.BytesSent + result.BytesRecv)

		if result.Error != nil {
			f.failureCount.Add(1)
			f.mu.Lock()
			errKey := result.Error.Error()
			if len(errKey) > 100 {
				errKey = errKey[:100]
			}
			f.errors[errKey]++
			f.mu.Unlock()
		} else {
			f.successCount.Add(1)
		}

		f.mu.Lock()
		f.latencies = append(f.latencies, result.Duration)
		f.mu.Unlock()
	}
}

// buildSummary creates the final summary from collected metrics.
func (f *Framework) buildSummary() *Summary {
	f.mu.RLock()
	defer f.mu.RUnlock()

	endTime := time.Now()
	duration := endTime.Sub(f.startTime).Seconds()
	total := f.totalRequests.Load()

	summary := &Summary{
		TestName:      f.config.Name,
		TestType:      f.config.Type,
		StartTime:     f.startTime,
		EndTime:       endTime,
		TotalRequests: total,
		SuccessCount:  f.successCount.Load(),
		FailureCount:  f.failureCount.Load(),
		TotalBytes:    f.totalBytes.Load(),
		Errors:        make(map[string]int64),
	}

	// Copy errors
	for k, v := range f.errors {
		summary.Errors[k] = v
	}

	// Calculate RPS and error rate
	if duration > 0 {
		summary.RequestsPerSec = float64(total) / duration
	}
	if total > 0 {
		summary.ErrorRate = float64(summary.FailureCount) / float64(total)
	}

	// Calculate latency percentiles
	if len(f.latencies) > 0 {
		summary.MinLatency, summary.MaxLatency, summary.AvgLatency,
			summary.P50Latency, summary.P95Latency, summary.P99Latency = calculatePercentiles(f.latencies)
	}

	return summary
}

// calculatePercentiles computes latency statistics.
func calculatePercentiles(latencies []time.Duration) (min, max, avg, p50, p95, p99 time.Duration) {
	if len(latencies) == 0 {
		return
	}

	// Sort for percentiles (copy to avoid modifying original)
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)

	// Simple bubble sort for small datasets, would use sort.Slice for production
	for i := 0; i < len(sorted)-1; i++ {
		for j := 0; j < len(sorted)-i-1; j++ {
			if sorted[j] > sorted[j+1] {
				sorted[j], sorted[j+1] = sorted[j+1], sorted[j]
			}
		}
	}

	min = sorted[0]
	max = sorted[len(sorted)-1]

	var total time.Duration
	for _, l := range sorted {
		total += l
	}
	avg = total / time.Duration(len(sorted))

	p50 = sorted[len(sorted)*50/100]
	p95 = sorted[len(sorted)*95/100]
	p99 = sorted[len(sorted)*99/100]

	return
}

// IsRunning returns whether a test is currently executing.
func (f *Framework) IsRunning() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.running
}

// CurrentStats returns real-time metrics during test execution.
func (f *Framework) CurrentStats() (total, success, failure int64, rps float64) {
	total = f.totalRequests.Load()
	success = f.successCount.Load()
	failure = f.failureCount.Load()

	f.mu.RLock()
	elapsed := time.Since(f.startTime).Seconds()
	f.mu.RUnlock()

	if elapsed > 0 {
		rps = float64(total) / elapsed
	}
	return
}
