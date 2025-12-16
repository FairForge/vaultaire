// Package loadtest provides infrastructure for load, stress, and chaos testing.
package loadtest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// SpikeConfig defines parameters for spike testing.
type SpikeConfig struct {
	Name           string
	BaselineRPS    int           // Normal traffic level
	SpikeRPS       int           // Traffic during spike
	BaselinePeriod time.Duration // Duration at baseline before spike
	SpikeDuration  time.Duration // How long the spike lasts
	RecoveryPeriod time.Duration // Duration to monitor after spike
	NumSpikes      int           // Number of spikes to generate
	MaxConcurrency int           // Maximum concurrent workers
	Timeout        time.Duration // Per-request timeout
}

// DefaultSpikeConfig returns sensible defaults for spike testing.
func DefaultSpikeConfig(name string) *SpikeConfig {
	return &SpikeConfig{
		Name:           name,
		BaselineRPS:    50,
		SpikeRPS:       500,
		BaselinePeriod: 10 * time.Second,
		SpikeDuration:  5 * time.Second,
		RecoveryPeriod: 10 * time.Second,
		NumSpikes:      3,
		MaxConcurrency: 200,
		Timeout:        30 * time.Second,
	}
}

// SpikeState represents the current state of spike testing.
type SpikeState string

const (
	SpikeStateBaseline SpikeState = "baseline"
	SpikeStateSpike    SpikeState = "spike"
	SpikeStateRecovery SpikeState = "recovery"
	SpikeStateComplete SpikeState = "complete"
)

// SpikeMetrics captures metrics for a specific period.
type SpikeMetrics struct {
	State       SpikeState
	SpikeNumber int
	StartTime   time.Time
	EndTime     time.Time
	Duration    time.Duration
	TargetRPS   int
	ActualRPS   float64
	Requests    int64
	Successes   int64
	Failures    int64
	ErrorRate   float64
	AvgLatency  time.Duration
	P95Latency  time.Duration
	P99Latency  time.Duration
	MaxLatency  time.Duration
}

// SpikeResult captures the complete spike test results.
type SpikeResult struct {
	*Summary
	Config          *SpikeConfig
	SpikeMetrics    []SpikeMetrics
	RecoveryTimes   []time.Duration // Time to recover after each spike
	MaxErrorRate    float64         // Highest error rate during any spike
	MaxLatency      time.Duration   // Highest latency observed
	SystemRecovered bool            // Did system recover to baseline after all spikes?
}

// SpikeTester executes spike load tests.
type SpikeTester struct {
	config     *SpikeConfig
	workerFunc WorkerFunc

	// Metrics
	currentRPS    atomic.Int64
	totalRequests atomic.Int64
	successCount  atomic.Int64
	failureCount  atomic.Int64

	// State
	mu            sync.RWMutex
	state         SpikeState
	currentSpike  int
	running       bool
	startTime     time.Time
	latencies     []time.Duration
	spikeMetrics  []SpikeMetrics
	periodStart   time.Time
	periodMetrics *periodCollector
}

// periodCollector tracks metrics for the current period.
type periodCollector struct {
	requests  atomic.Int64
	successes atomic.Int64
	failures  atomic.Int64
	latencies []time.Duration
	mu        sync.Mutex
}

func newPeriodCollector() *periodCollector {
	return &periodCollector{
		latencies: make([]time.Duration, 0, 1000),
	}
}

func (p *periodCollector) record(result Result) {
	p.requests.Add(1)
	if result.Error != nil {
		p.failures.Add(1)
	} else {
		p.successes.Add(1)
	}
	p.mu.Lock()
	p.latencies = append(p.latencies, result.Duration)
	p.mu.Unlock()
}

func (p *periodCollector) snapshot() (requests, successes, failures int64, latencies []time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	latencies = make([]time.Duration, len(p.latencies))
	copy(latencies, p.latencies)
	return p.requests.Load(), p.successes.Load(), p.failures.Load(), latencies
}

func (p *periodCollector) reset() {
	p.requests.Store(0)
	p.successes.Store(0)
	p.failures.Store(0)
	p.mu.Lock()
	p.latencies = p.latencies[:0]
	p.mu.Unlock()
}

// NewSpikeTester creates a new spike testing instance.
func NewSpikeTester(config *SpikeConfig, workerFunc WorkerFunc) *SpikeTester {
	if config == nil {
		config = DefaultSpikeConfig("default-spike")
	}

	return &SpikeTester{
		config:        config,
		workerFunc:    workerFunc,
		state:         SpikeStateBaseline,
		spikeMetrics:  make([]SpikeMetrics, 0),
		latencies:     make([]time.Duration, 0, 10000),
		periodMetrics: newPeriodCollector(),
	}
}

// Run executes the spike test.
func (s *SpikeTester) Run(ctx context.Context) (*SpikeResult, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil, fmt.Errorf("spike test already running")
	}
	s.running = true
	s.startTime = time.Now()
	s.state = SpikeStateBaseline
	s.currentSpike = 0
	s.periodStart = time.Now()
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	// Results collection
	results := make(chan Result, s.config.MaxConcurrency*10)
	collectorDone := make(chan struct{})
	go s.collectResults(results, collectorDone)

	// Worker management
	var wg sync.WaitGroup
	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()

	semaphore := make(chan struct{}, s.config.MaxConcurrency)
	workerID := atomic.Int64{}

	currentRPS := s.config.BaselineRPS
	s.currentRPS.Store(int64(currentRPS))

	requestTicker := time.NewTicker(time.Second / time.Duration(currentRPS))
	defer requestTicker.Stop()

	// State transition timer
	stateTimer := time.NewTimer(s.config.BaselinePeriod)
	defer stateTimer.Stop()

	recoveryTimes := make([]time.Duration, 0)
	var spikeEndTime time.Time

	for {
		select {
		case <-workerCtx.Done():
			goto cleanup

		case <-stateTimer.C:
			// Record metrics for completed period
			s.recordPeriodMetrics(currentRPS)

			// Transition state
			s.mu.Lock()
			switch s.state {
			case SpikeStateBaseline:
				s.currentSpike++
				if s.currentSpike > s.config.NumSpikes {
					s.state = SpikeStateComplete
					s.mu.Unlock()
					goto cleanup
				}
				s.state = SpikeStateSpike
				currentRPS = s.config.SpikeRPS
				stateTimer.Reset(s.config.SpikeDuration)

			case SpikeStateSpike:
				spikeEndTime = time.Now()
				s.state = SpikeStateRecovery
				currentRPS = s.config.BaselineRPS
				stateTimer.Reset(s.config.RecoveryPeriod)

			case SpikeStateRecovery:
				// Calculate recovery time (simplified: time since spike ended)
				recoveryTimes = append(recoveryTimes, time.Since(spikeEndTime))

				// Check if more spikes needed
				if s.currentSpike >= s.config.NumSpikes {
					s.state = SpikeStateComplete
					s.mu.Unlock()
					goto cleanup
				}
				s.state = SpikeStateBaseline
				stateTimer.Reset(s.config.BaselinePeriod)
			}
			s.mu.Unlock()

			s.currentRPS.Store(int64(currentRPS))
			s.periodStart = time.Now()
			s.periodMetrics.reset()
			requestTicker.Reset(time.Second / time.Duration(currentRPS))

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

	return s.buildResult(recoveryTimes), nil
}

// collectResults aggregates results from workers.
func (s *SpikeTester) collectResults(results chan Result, done chan struct{}) {
	defer close(done)

	for result := range results {
		s.totalRequests.Add(1)

		if result.Error != nil {
			s.failureCount.Add(1)
		} else {
			s.successCount.Add(1)
		}

		s.mu.Lock()
		s.latencies = append(s.latencies, result.Duration)
		s.mu.Unlock()

		s.periodMetrics.record(result)
	}
}

// recordPeriodMetrics saves metrics for the current period.
func (s *SpikeTester) recordPeriodMetrics(targetRPS int) {
	requests, successes, failures, latencies := s.periodMetrics.snapshot()

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	duration := now.Sub(s.periodStart)

	var errorRate float64
	if requests > 0 {
		errorRate = float64(failures) / float64(requests)
	}

	var actualRPS float64
	if duration.Seconds() > 0 {
		actualRPS = float64(requests) / duration.Seconds()
	}

	var avgLatency, p95Latency, p99Latency, maxLatency time.Duration
	if len(latencies) > 0 {
		_, maxLatency, avgLatency, _, p95Latency, p99Latency = calculatePercentiles(latencies)
	}

	metrics := SpikeMetrics{
		State:       s.state,
		SpikeNumber: s.currentSpike,
		StartTime:   s.periodStart,
		EndTime:     now,
		Duration:    duration,
		TargetRPS:   targetRPS,
		ActualRPS:   actualRPS,
		Requests:    requests,
		Successes:   successes,
		Failures:    failures,
		ErrorRate:   errorRate,
		AvgLatency:  avgLatency,
		P95Latency:  p95Latency,
		P99Latency:  p99Latency,
		MaxLatency:  maxLatency,
	}

	s.spikeMetrics = append(s.spikeMetrics, metrics)
}

// buildResult creates the final spike test result.
func (s *SpikeTester) buildResult(recoveryTimes []time.Duration) *SpikeResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	endTime := time.Now()
	total := s.totalRequests.Load()
	successes := s.successCount.Load()
	failures := s.failureCount.Load()

	summary := &Summary{
		TestName:      s.config.Name,
		TestType:      TestTypeSpike,
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

	// Find max error rate and latency during spikes
	var maxErrorRate float64
	var maxLatency time.Duration
	for _, m := range s.spikeMetrics {
		if m.State == SpikeStateSpike {
			if m.ErrorRate > maxErrorRate {
				maxErrorRate = m.ErrorRate
			}
			if m.MaxLatency > maxLatency {
				maxLatency = m.MaxLatency
			}
		}
	}

	// Check if system recovered (last recovery period had low error rate)
	systemRecovered := true
	for _, m := range s.spikeMetrics {
		if m.State == SpikeStateRecovery && m.ErrorRate > 0.05 {
			systemRecovered = false
			break
		}
	}

	return &SpikeResult{
		Summary:         summary,
		Config:          s.config,
		SpikeMetrics:    s.spikeMetrics,
		RecoveryTimes:   recoveryTimes,
		MaxErrorRate:    maxErrorRate,
		MaxLatency:      maxLatency,
		SystemRecovered: systemRecovered,
	}
}

// State returns the current spike test state.
func (s *SpikeTester) State() SpikeState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// CurrentRPS returns the current target RPS.
func (s *SpikeTester) CurrentRPS() int {
	return int(s.currentRPS.Load())
}

// CurrentSpike returns which spike iteration we're on.
func (s *SpikeTester) CurrentSpike() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentSpike
}
