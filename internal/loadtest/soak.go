// Package loadtest provides infrastructure for load, stress, and chaos testing.
package loadtest

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// SoakConfig defines parameters for soak/endurance testing.
type SoakConfig struct {
	Name               string
	Duration           time.Duration // Total test duration (hours)
	TargetRPS          int           // Sustained requests per second
	MaxConcurrency     int           // Maximum concurrent workers
	Timeout            time.Duration // Per-request timeout
	SampleInterval     time.Duration // How often to collect metrics
	MemoryThreshold    uint64        // Alert if memory exceeds this (bytes)
	GoroutineThreshold int           // Alert if goroutines exceed this
	ErrorRateThreshold float64       // Alert if error rate exceeds this
	LatencyThreshold   time.Duration // Alert if p99 exceeds this
}

// DefaultSoakConfig returns sensible defaults for soak testing.
func DefaultSoakConfig(name string) *SoakConfig {
	return &SoakConfig{
		Name:               name,
		Duration:           1 * time.Hour,
		TargetRPS:          100,
		MaxConcurrency:     50,
		Timeout:            30 * time.Second,
		SampleInterval:     1 * time.Minute,
		MemoryThreshold:    2 * 1024 * 1024 * 1024, // 2GB
		GoroutineThreshold: 10000,
		ErrorRateThreshold: 0.01, // 1%
		LatencyThreshold:   5 * time.Second,
	}
}

// ResourceSample captures system resource usage at a point in time.
type ResourceSample struct {
	Timestamp    time.Time
	HeapAlloc    uint64 // Bytes allocated on heap
	HeapSys      uint64 // Bytes obtained from system
	HeapObjects  uint64 // Number of allocated objects
	NumGoroutine int    // Number of goroutines
	NumGC        uint32 // Number of completed GC cycles
	GCPauseNs    uint64 // Total GC pause time in nanoseconds
	Requests     int64  // Requests in this sample period
	Errors       int64  // Errors in this sample period
	ErrorRate    float64
	AvgLatency   time.Duration
	P99Latency   time.Duration
}

// SoakAlert represents a threshold violation during soak testing.
type SoakAlert struct {
	Timestamp time.Time
	AlertType string
	Message   string
	Value     interface{}
	Threshold interface{}
}

// SoakResult captures the complete soak test results.
type SoakResult struct {
	*Summary
	Config           *SoakConfig
	ResourceSamples  []ResourceSample
	Alerts           []SoakAlert
	PeakMemory       uint64
	PeakGoroutines   int
	MemoryGrowth     float64       // Percentage growth from start to end
	GoroutineGrowth  float64       // Percentage growth from start to end
	Stable           bool          // Did system remain stable throughout?
	DegradationPoint time.Duration // When degradation was first detected (0 if never)
}

// SoakTester executes extended duration soak tests.
type SoakTester struct {
	config     *SoakConfig
	workerFunc WorkerFunc

	// Metrics
	totalRequests  atomic.Int64
	successCount   atomic.Int64
	failureCount   atomic.Int64
	periodRequests atomic.Int64
	periodFailures atomic.Int64

	// State
	mu              sync.RWMutex
	running         bool
	startTime       time.Time
	latencies       []time.Duration
	periodLatencies []time.Duration
	samples         []ResourceSample
	alerts          []SoakAlert
}

// NewSoakTester creates a new soak testing instance.
func NewSoakTester(config *SoakConfig, workerFunc WorkerFunc) *SoakTester {
	if config == nil {
		config = DefaultSoakConfig("default-soak")
	}

	return &SoakTester{
		config:          config,
		workerFunc:      workerFunc,
		latencies:       make([]time.Duration, 0, 100000),
		periodLatencies: make([]time.Duration, 0, 10000),
		samples:         make([]ResourceSample, 0),
		alerts:          make([]SoakAlert, 0),
	}
}

// Run executes the soak test.
func (s *SoakTester) Run(ctx context.Context) (*SoakResult, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil, fmt.Errorf("soak test already running")
	}
	s.running = true
	s.startTime = time.Now()
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	// Create test context with duration
	testCtx, cancel := context.WithTimeout(ctx, s.config.Duration)
	defer cancel()

	// Results collection
	results := make(chan Result, s.config.MaxConcurrency*10)
	collectorDone := make(chan struct{})
	go s.collectResults(results, collectorDone)

	// Worker management
	var wg sync.WaitGroup
	workerCtx, workerCancel := context.WithCancel(testCtx)
	defer workerCancel()

	semaphore := make(chan struct{}, s.config.MaxConcurrency)
	workerID := atomic.Int64{}

	// Request ticker
	requestTicker := time.NewTicker(time.Second / time.Duration(s.config.TargetRPS))
	defer requestTicker.Stop()

	// Sample ticker for resource monitoring
	sampleTicker := time.NewTicker(s.config.SampleInterval)
	defer sampleTicker.Stop()

	// Take initial sample
	s.takeSample()

	for {
		select {
		case <-workerCtx.Done():
			goto cleanup

		case <-sampleTicker.C:
			s.takeSample()
			s.checkThresholds()
			s.resetPeriodMetrics()

		case <-requestTicker.C:
			select {
			case semaphore <- struct{}{}:
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					defer func() { <-semaphore }()

					result := s.workerFunc(workerCtx, id)
					select {
					case results <- result:
					case <-workerCtx.Done():
					}
				}(int(workerID.Add(1)))
			default:
				// At max concurrency
			}
		}
	}

cleanup:
	workerCancel()
	wg.Wait()
	close(results)
	<-collectorDone

	// Take final sample
	s.takeSample()

	return s.buildResult(), nil
}

// collectResults aggregates results from workers.
func (s *SoakTester) collectResults(results chan Result, done chan struct{}) {
	defer close(done)

	for result := range results {
		s.totalRequests.Add(1)
		s.periodRequests.Add(1)

		if result.Error != nil {
			s.failureCount.Add(1)
			s.periodFailures.Add(1)
		} else {
			s.successCount.Add(1)
		}

		s.mu.Lock()
		s.latencies = append(s.latencies, result.Duration)
		s.periodLatencies = append(s.periodLatencies, result.Duration)
		s.mu.Unlock()
	}
}

// takeSample captures current resource usage.
func (s *SoakTester) takeSample() {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	s.mu.Lock()
	defer s.mu.Unlock()

	periodReqs := s.periodRequests.Load()
	periodFails := s.periodFailures.Load()

	var errorRate float64
	if periodReqs > 0 {
		errorRate = float64(periodFails) / float64(periodReqs)
	}

	var avgLatency, p99Latency time.Duration
	if len(s.periodLatencies) > 0 {
		_, _, avgLatency, _, _, p99Latency = calculatePercentiles(s.periodLatencies)
	}

	sample := ResourceSample{
		Timestamp:    time.Now(),
		HeapAlloc:    memStats.HeapAlloc,
		HeapSys:      memStats.HeapSys,
		HeapObjects:  memStats.HeapObjects,
		NumGoroutine: runtime.NumGoroutine(),
		NumGC:        memStats.NumGC,
		GCPauseNs:    memStats.PauseTotalNs,
		Requests:     periodReqs,
		Errors:       periodFails,
		ErrorRate:    errorRate,
		AvgLatency:   avgLatency,
		P99Latency:   p99Latency,
	}

	s.samples = append(s.samples, sample)
}

// checkThresholds verifies current metrics against configured thresholds.
func (s *SoakTester) checkThresholds() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.samples) == 0 {
		return
	}

	latest := s.samples[len(s.samples)-1]

	// Memory threshold
	if latest.HeapAlloc > s.config.MemoryThreshold {
		s.alerts = append(s.alerts, SoakAlert{
			Timestamp: time.Now(),
			AlertType: "memory",
			Message:   fmt.Sprintf("heap allocation %d exceeds threshold %d", latest.HeapAlloc, s.config.MemoryThreshold),
			Value:     latest.HeapAlloc,
			Threshold: s.config.MemoryThreshold,
		})
	}

	// Goroutine threshold
	if latest.NumGoroutine > s.config.GoroutineThreshold {
		s.alerts = append(s.alerts, SoakAlert{
			Timestamp: time.Now(),
			AlertType: "goroutines",
			Message:   fmt.Sprintf("goroutine count %d exceeds threshold %d", latest.NumGoroutine, s.config.GoroutineThreshold),
			Value:     latest.NumGoroutine,
			Threshold: s.config.GoroutineThreshold,
		})
	}

	// Error rate threshold
	if latest.ErrorRate > s.config.ErrorRateThreshold {
		s.alerts = append(s.alerts, SoakAlert{
			Timestamp: time.Now(),
			AlertType: "error_rate",
			Message:   fmt.Sprintf("error rate %.4f exceeds threshold %.4f", latest.ErrorRate, s.config.ErrorRateThreshold),
			Value:     latest.ErrorRate,
			Threshold: s.config.ErrorRateThreshold,
		})
	}

	// Latency threshold
	if latest.P99Latency > s.config.LatencyThreshold {
		s.alerts = append(s.alerts, SoakAlert{
			Timestamp: time.Now(),
			AlertType: "latency",
			Message:   fmt.Sprintf("p99 latency %v exceeds threshold %v", latest.P99Latency, s.config.LatencyThreshold),
			Value:     latest.P99Latency,
			Threshold: s.config.LatencyThreshold,
		})
	}
}

// resetPeriodMetrics clears per-period counters.
func (s *SoakTester) resetPeriodMetrics() {
	s.periodRequests.Store(0)
	s.periodFailures.Store(0)

	s.mu.Lock()
	s.periodLatencies = s.periodLatencies[:0]
	s.mu.Unlock()
}

// buildResult creates the final soak test result.
func (s *SoakTester) buildResult() *SoakResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	endTime := time.Now()
	total := s.totalRequests.Load()
	successes := s.successCount.Load()
	failures := s.failureCount.Load()

	summary := &Summary{
		TestName:      s.config.Name,
		TestType:      TestTypeSoak,
		StartTime:     s.startTime,
		EndTime:       endTime,
		TotalRequests: total,
		SuccessCount:  successes,
		FailureCount:  failures,
		Errors:        make(map[string]int64),
	}

	duration := endTime.Sub(s.startTime).Seconds()
	if duration > 0 {
		summary.RequestsPerSec = float64(total) / duration
	}
	if total > 0 {
		summary.ErrorRate = float64(failures) / float64(total)
	}

	if len(s.latencies) > 0 {
		summary.MinLatency, summary.MaxLatency, summary.AvgLatency,
			summary.P50Latency, summary.P95Latency, summary.P99Latency = calculatePercentiles(s.latencies)
	}

	// Calculate peak values and growth
	var peakMemory uint64
	var peakGoroutines int
	var firstMemory, lastMemory uint64
	var firstGoroutines, lastGoroutines int

	for i, sample := range s.samples {
		if sample.HeapAlloc > peakMemory {
			peakMemory = sample.HeapAlloc
		}
		if sample.NumGoroutine > peakGoroutines {
			peakGoroutines = sample.NumGoroutine
		}
		if i == 0 {
			firstMemory = sample.HeapAlloc
			firstGoroutines = sample.NumGoroutine
		}
		if i == len(s.samples)-1 {
			lastMemory = sample.HeapAlloc
			lastGoroutines = sample.NumGoroutine
		}
	}

	var memoryGrowth, goroutineGrowth float64
	if firstMemory > 0 {
		memoryGrowth = (float64(lastMemory) - float64(firstMemory)) / float64(firstMemory) * 100
	}
	if firstGoroutines > 0 {
		goroutineGrowth = (float64(lastGoroutines) - float64(firstGoroutines)) / float64(firstGoroutines) * 100
	}

	// Determine stability and degradation point
	stable := len(s.alerts) == 0
	var degradationPoint time.Duration
	if len(s.alerts) > 0 {
		degradationPoint = s.alerts[0].Timestamp.Sub(s.startTime)
	}

	return &SoakResult{
		Summary:          summary,
		Config:           s.config,
		ResourceSamples:  s.samples,
		Alerts:           s.alerts,
		PeakMemory:       peakMemory,
		PeakGoroutines:   peakGoroutines,
		MemoryGrowth:     memoryGrowth,
		GoroutineGrowth:  goroutineGrowth,
		Stable:           stable,
		DegradationPoint: degradationPoint,
	}
}

// Samples returns current resource samples (for monitoring during test).
func (s *SoakTester) Samples() []ResourceSample {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]ResourceSample, len(s.samples))
	copy(result, s.samples)
	return result
}

// Alerts returns current alerts (for monitoring during test).
func (s *SoakTester) Alerts() []SoakAlert {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]SoakAlert, len(s.alerts))
	copy(result, s.alerts)
	return result
}

// IsStable returns whether the test has encountered any alerts.
func (s *SoakTester) IsStable() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.alerts) == 0
}
