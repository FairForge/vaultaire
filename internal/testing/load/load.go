// Package load provides utilities for load testing.
package load

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config configures a load test.
type Config struct {
	// Target URL
	URL string

	// HTTP method
	Method string

	// Request headers
	Headers map[string]string

	// Request body
	Body []byte

	// Number of concurrent workers
	Concurrency int

	// Total number of requests (0 = unlimited, use Duration)
	Requests int

	// Test duration (0 = unlimited, use Requests)
	Duration time.Duration

	// Rate limit (requests per second, 0 = unlimited)
	RateLimit int

	// Timeout per request
	Timeout time.Duration

	// Custom request generator
	RequestFunc func() (*http.Request, error)
}

// Result contains load test results.
type Result struct {
	TotalRequests  int64
	SuccessCount   int64
	FailureCount   int64
	TotalDuration  time.Duration
	RequestsPerSec float64
	AvgLatency     time.Duration
	MinLatency     time.Duration
	MaxLatency     time.Duration
	P50Latency     time.Duration
	P90Latency     time.Duration
	P95Latency     time.Duration
	P99Latency     time.Duration
	StatusCodes    map[int]int64
	Errors         map[string]int64
	BytesReceived  int64
	BytesSent      int64
	latencies      []time.Duration
	mu             sync.Mutex
}

// Runner executes load tests.
type Runner struct {
	client *http.Client
	config Config
}

// NewRunner creates a load test runner.
func NewRunner(config Config) *Runner {
	if config.Method == "" {
		config.Method = http.MethodGet
	}
	if config.Concurrency == 0 {
		config.Concurrency = 10
	}
	if config.Timeout == 0 {
		config.Timeout = 30 * time.Second
	}

	return &Runner{
		client: &http.Client{
			Timeout: config.Timeout,
			Transport: &http.Transport{
				MaxIdleConns:        config.Concurrency * 2,
				MaxIdleConnsPerHost: config.Concurrency * 2,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		config: config,
	}
}

// Run executes the load test.
func (r *Runner) Run(ctx context.Context) (*Result, error) {
	result := &Result{
		StatusCodes: make(map[int]int64),
		Errors:      make(map[string]int64),
		latencies:   make([]time.Duration, 0),
		MinLatency:  time.Hour, // Will be updated
	}

	// Create context with timeout if duration specified
	if r.config.Duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.config.Duration)
		defer cancel()
	}

	// Rate limiter
	var rateLimiter <-chan time.Time
	if r.config.RateLimit > 0 {
		ticker := time.NewTicker(time.Second / time.Duration(r.config.RateLimit))
		defer ticker.Stop()
		rateLimiter = ticker.C
	}

	// Request counter
	var requestCount int64

	// Worker pool
	var wg sync.WaitGroup
	requestChan := make(chan struct{}, r.config.Concurrency*2)

	// Start workers
	for i := 0; i < r.config.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range requestChan {
				r.executeRequest(ctx, result)
			}
		}()
	}

	// Send requests
	start := time.Now()

	go func() {
		defer close(requestChan)

		for {
			select {
			case <-ctx.Done():
				return
			default:
				// Check request limit
				if r.config.Requests > 0 {
					count := atomic.AddInt64(&requestCount, 1)
					if count > int64(r.config.Requests) {
						return
					}
				}

				// Rate limiting
				if rateLimiter != nil {
					select {
					case <-rateLimiter:
					case <-ctx.Done():
						return
					}
				}

				// Send to workers
				select {
				case requestChan <- struct{}{}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	wg.Wait()
	result.TotalDuration = time.Since(start)

	// Calculate statistics
	r.calculateStats(result)

	return result, nil
}

// executeRequest performs a single request and records metrics.
func (r *Runner) executeRequest(ctx context.Context, result *Result) {
	var req *http.Request
	var err error

	if r.config.RequestFunc != nil {
		req, err = r.config.RequestFunc()
	} else {
		req, err = http.NewRequestWithContext(ctx, r.config.Method, r.config.URL, nil)
		if r.config.Body != nil {
			req, err = http.NewRequestWithContext(ctx, r.config.Method, r.config.URL,
				strings.NewReader(string(r.config.Body)))
		}
	}

	if err != nil {
		result.mu.Lock()
		result.Errors[err.Error()]++
		atomic.AddInt64(&result.FailureCount, 1)
		result.mu.Unlock()
		return
	}

	// Set headers
	for k, v := range r.config.Headers {
		req.Header.Set(k, v)
	}

	// Track bytes sent
	if r.config.Body != nil {
		atomic.AddInt64(&result.BytesSent, int64(len(r.config.Body)))
	}

	// Execute request
	start := time.Now()
	resp, err := r.client.Do(req)
	latency := time.Since(start)

	atomic.AddInt64(&result.TotalRequests, 1)

	if err != nil {
		result.mu.Lock()
		errMsg := simplifyError(err)
		result.Errors[errMsg]++
		atomic.AddInt64(&result.FailureCount, 1)
		result.mu.Unlock()
		return
	}

	// Read and discard body to enable connection reuse
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	atomic.AddInt64(&result.BytesReceived, int64(len(bodyBytes)))

	// Record metrics
	result.mu.Lock()
	result.StatusCodes[resp.StatusCode]++
	result.latencies = append(result.latencies, latency)

	if latency < result.MinLatency {
		result.MinLatency = latency
	}
	if latency > result.MaxLatency {
		result.MaxLatency = latency
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		atomic.AddInt64(&result.SuccessCount, 1)
	} else {
		atomic.AddInt64(&result.FailureCount, 1)
	}
	result.mu.Unlock()
}

// simplifyError simplifies error messages for grouping.
func simplifyError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "context deadline exceeded") {
		return "timeout"
	}
	if strings.Contains(msg, "connection refused") {
		return "connection refused"
	}
	if strings.Contains(msg, "no such host") {
		return "dns error"
	}
	if len(msg) > 50 {
		return msg[:50] + "..."
	}
	return msg
}

// calculateStats computes final statistics.
func (r *Runner) calculateStats(result *Result) {
	result.mu.Lock()
	defer result.mu.Unlock()

	if len(result.latencies) == 0 {
		result.MinLatency = 0
		return
	}

	// Sort latencies for percentiles
	sort.Slice(result.latencies, func(i, j int) bool {
		return result.latencies[i] < result.latencies[j]
	})

	// Calculate average
	var total time.Duration
	for _, l := range result.latencies {
		total += l
	}
	result.AvgLatency = total / time.Duration(len(result.latencies))

	// Calculate percentiles
	result.P50Latency = percentile(result.latencies, 50)
	result.P90Latency = percentile(result.latencies, 90)
	result.P95Latency = percentile(result.latencies, 95)
	result.P99Latency = percentile(result.latencies, 99)

	// Calculate requests per second
	if result.TotalDuration > 0 {
		result.RequestsPerSec = float64(result.TotalRequests) / result.TotalDuration.Seconds()
	}
}

// percentile calculates the nth percentile.
func percentile(sorted []time.Duration, n int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := (n * len(sorted)) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// GenerateReport creates a formatted load test report.
func (r *Result) GenerateReport() string {
	report := "Load Test Report\n"
	report += "================\n\n"

	report += "Summary:\n"
	report += fmt.Sprintf("  Total Requests:  %d\n", r.TotalRequests)
	report += fmt.Sprintf("  Successful:      %d (%.1f%%)\n",
		r.SuccessCount, float64(r.SuccessCount)/float64(r.TotalRequests)*100)
	report += fmt.Sprintf("  Failed:          %d (%.1f%%)\n",
		r.FailureCount, float64(r.FailureCount)/float64(r.TotalRequests)*100)
	report += fmt.Sprintf("  Duration:        %v\n", r.TotalDuration.Round(time.Millisecond))
	report += fmt.Sprintf("  Requests/sec:    %.2f\n\n", r.RequestsPerSec)

	report += "Latency:\n"
	report += fmt.Sprintf("  Min:    %v\n", r.MinLatency.Round(time.Microsecond))
	report += fmt.Sprintf("  Avg:    %v\n", r.AvgLatency.Round(time.Microsecond))
	report += fmt.Sprintf("  P50:    %v\n", r.P50Latency.Round(time.Microsecond))
	report += fmt.Sprintf("  P90:    %v\n", r.P90Latency.Round(time.Microsecond))
	report += fmt.Sprintf("  P95:    %v\n", r.P95Latency.Round(time.Microsecond))
	report += fmt.Sprintf("  P99:    %v\n", r.P99Latency.Round(time.Microsecond))
	report += fmt.Sprintf("  Max:    %v\n\n", r.MaxLatency.Round(time.Microsecond))

	report += "Throughput:\n"
	report += fmt.Sprintf("  Bytes Sent:      %s\n", formatBytes(r.BytesSent))
	report += fmt.Sprintf("  Bytes Received:  %s\n\n", formatBytes(r.BytesReceived))

	if len(r.StatusCodes) > 0 {
		report += "Status Codes:\n"
		for code, count := range r.StatusCodes {
			report += fmt.Sprintf("  %d: %d\n", code, count)
		}
		report += "\n"
	}

	if len(r.Errors) > 0 {
		report += "Errors:\n"
		for err, count := range r.Errors {
			report += fmt.Sprintf("  %s: %d\n", err, count)
		}
	}

	return report
}

// formatBytes formats bytes as human-readable string.
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// Scenario represents a load test scenario.
type Scenario struct {
	Name       string
	Config     Config
	WarmupTime time.Duration
	RampUpTime time.Duration
	Validate   func(*Result) error
}

// ScenarioRunner runs multiple load test scenarios.
type ScenarioRunner struct {
	scenarios []Scenario
	results   map[string]*Result
	mu        sync.Mutex
}

// NewScenarioRunner creates a scenario runner.
func NewScenarioRunner() *ScenarioRunner {
	return &ScenarioRunner{
		scenarios: make([]Scenario, 0),
		results:   make(map[string]*Result),
	}
}

// AddScenario adds a scenario.
func (sr *ScenarioRunner) AddScenario(s Scenario) {
	sr.scenarios = append(sr.scenarios, s)
}

// Run executes all scenarios.
func (sr *ScenarioRunner) Run(ctx context.Context) error {
	for _, scenario := range sr.scenarios {
		// Warmup
		if scenario.WarmupTime > 0 {
			warmupConfig := scenario.Config
			warmupConfig.Duration = scenario.WarmupTime
			warmupRunner := NewRunner(warmupConfig)
			_, _ = warmupRunner.Run(ctx)
		}

		// Run scenario
		runner := NewRunner(scenario.Config)
		result, err := runner.Run(ctx)
		if err != nil {
			return fmt.Errorf("scenario %s failed: %w", scenario.Name, err)
		}

		sr.mu.Lock()
		sr.results[scenario.Name] = result
		sr.mu.Unlock()

		// Validate
		if scenario.Validate != nil {
			if err := scenario.Validate(result); err != nil {
				return fmt.Errorf("scenario %s validation failed: %w", scenario.Name, err)
			}
		}
	}

	return nil
}

// Results returns all scenario results.
func (sr *ScenarioRunner) Results() map[string]*Result {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	results := make(map[string]*Result)
	for k, v := range sr.results {
		results[k] = v
	}
	return results
}

// StressTest performs an increasing load stress test.
type StressTest struct {
	BaseConfig       Config
	StartConcurrency int
	MaxConcurrency   int
	StepSize         int
	StepDuration     time.Duration
	FailureThreshold float64 // Percentage of failures to stop
}

// RunStressTest executes a stress test with increasing load.
func RunStressTest(ctx context.Context, st StressTest) ([]StressResult, error) {
	results := make([]StressResult, 0)

	for concurrency := st.StartConcurrency; concurrency <= st.MaxConcurrency; concurrency += st.StepSize {
		config := st.BaseConfig
		config.Concurrency = concurrency
		config.Duration = st.StepDuration

		runner := NewRunner(config)
		result, err := runner.Run(ctx)
		if err != nil {
			return results, err
		}

		sr := StressResult{
			Concurrency: concurrency,
			Result:      result,
		}
		results = append(results, sr)

		// Check failure threshold
		if result.TotalRequests > 0 {
			failureRate := float64(result.FailureCount) / float64(result.TotalRequests) * 100
			if failureRate > st.FailureThreshold {
				sr.BreakingPoint = true
				break
			}
		}
	}

	return results, nil
}

// StressResult contains results for a stress test step.
type StressResult struct {
	Concurrency   int
	Result        *Result
	BreakingPoint bool
}

// AssertPerformance validates performance requirements.
func AssertPerformance(result *Result, maxP99 time.Duration, minRPS float64, maxFailureRate float64) error {
	if result.P99Latency > maxP99 {
		return fmt.Errorf("P99 latency %v exceeds maximum %v", result.P99Latency, maxP99)
	}

	if result.RequestsPerSec < minRPS {
		return fmt.Errorf("RPS %.2f below minimum %.2f", result.RequestsPerSec, minRPS)
	}

	if result.TotalRequests > 0 {
		failureRate := float64(result.FailureCount) / float64(result.TotalRequests) * 100
		if failureRate > maxFailureRate {
			return fmt.Errorf("failure rate %.2f%% exceeds maximum %.2f%%", failureRate, maxFailureRate)
		}
	}

	return nil
}
