// Package loadtest provides infrastructure for load, stress, and chaos testing.
package loadtest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// StressConfig defines parameters for stress testing.
type StressConfig struct {
	Name             string
	InitialRPS       int           // Starting requests per second
	MaxRPS           int           // Maximum RPS to reach
	RampUpRate       int           // RPS increase per interval
	RampInterval     time.Duration // How often to increase RPS
	HoldDuration     time.Duration // How long to hold at max RPS
	MaxConcurrency   int           // Maximum concurrent workers
	Timeout          time.Duration // Per-request timeout
	FailureThreshold float64       // Stop if error rate exceeds this (0.0-1.0)
	LatencyThreshold time.Duration // Stop if p99 latency exceeds this
}

// DefaultStressConfig returns sensible defaults for stress testing.
func DefaultStressConfig(name string) *StressConfig {
	return &StressConfig{
		Name:             name,
		InitialRPS:       10,
		MaxRPS:           1000,
		RampUpRate:       50,
		RampInterval:     10 * time.Second,
		HoldDuration:     30 * time.Second,
		MaxConcurrency:   500,
		Timeout:          30 * time.Second,
		FailureThreshold: 0.10, // 10% error rate
		LatencyThreshold: 5 * time.Second,
	}
}

// StressPhase represents a phase of the stress test.
type StressPhase string

const (
	PhaseRampUp   StressPhase = "ramp_up"
	PhaseHold     StressPhase = "hold"
	PhaseComplete StressPhase = "complete"
	PhaseStopped  StressPhase = "stopped"
)

// StressResult captures stress test outcomes with phase details.
type StressResult struct {
	*Summary
	Phase            StressPhase
	MaxRPSAchieved   int
	BreakingPointRPS int    // RPS where errors started exceeding threshold
	StopReason       string // Why the test stopped
	PhaseResults     []PhaseResult
}

// PhaseResult captures metrics for each stress phase.
type PhaseResult struct {
	Phase      StressPhase
	TargetRPS  int
	ActualRPS  float64
	Duration   time.Duration
	Requests   int64
	Successes  int64
	Failures   int64
	ErrorRate  float64
	AvgLatency time.Duration
	P99Latency time.Duration
}

// StressTester executes stress tests with progressive load increases.
type StressTester struct {
	config     *StressConfig
	workerFunc WorkerFunc

	// Metrics
	currentRPS    atomic.Int64
	totalRequests atomic.Int64
	successCount  atomic.Int64
	failureCount  atomic.Int64

	// State
	mu           sync.RWMutex
	phase        StressPhase
	running      bool
	startTime    time.Time
	latencies    []time.Duration
	phaseResults []PhaseResult
	stopReason   string
}

// NewStressTester creates a new stress testing instance.
func NewStressTester(config *StressConfig, workerFunc WorkerFunc) *StressTester {
	if config == nil {
		config = DefaultStressConfig("default-stress")
	}

	return &StressTester{
		config:       config,
		workerFunc:   workerFunc,
		phase:        PhaseRampUp,
		latencies:    make([]time.Duration, 0, 100000),
		phaseResults: make([]PhaseResult, 0),
	}
}

// Run executes the stress test.
func (s *StressTester) Run(ctx context.Context) (*StressResult, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil, fmt.Errorf("stress test already running")
	}
	s.running = true
	s.startTime = time.Now()
	s.phase = PhaseRampUp
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

	currentRPS := s.config.InitialRPS
	s.currentRPS.Store(int64(currentRPS))

	// Ramp up phase
	rampTicker := time.NewTicker(s.config.RampInterval)
	defer rampTicker.Stop()

	requestTicker := time.NewTicker(time.Second / time.Duration(currentRPS))
	defer requestTicker.Stop()

	holdStart := time.Time{}
	maxRPSAchieved := currentRPS
	breakingPointRPS := 0

	for {
		select {
		case <-workerCtx.Done():
			s.setStopReason("context cancelled")
			goto cleanup

		case <-rampTicker.C:
			// Check thresholds before ramping
			errorRate := s.getErrorRate()
			p99 := s.getP99Latency()

			// Record phase result
			s.recordPhaseResult(currentRPS)

			if errorRate > s.config.FailureThreshold {
				if breakingPointRPS == 0 {
					breakingPointRPS = currentRPS
				}
				s.setStopReason(fmt.Sprintf("error rate %.2f%% exceeded threshold %.2f%%",
					errorRate*100, s.config.FailureThreshold*100))
				goto cleanup
			}

			if p99 > s.config.LatencyThreshold {
				if breakingPointRPS == 0 {
					breakingPointRPS = currentRPS
				}
				s.setStopReason(fmt.Sprintf("p99 latency %v exceeded threshold %v",
					p99, s.config.LatencyThreshold))
				goto cleanup
			}

			// Ramp up or transition to hold
			s.mu.Lock()
			switch s.phase {
			case PhaseRampUp:
				if currentRPS >= s.config.MaxRPS {
					s.phase = PhaseHold
					holdStart = time.Now()
					currentRPS = s.config.MaxRPS
				} else {
					currentRPS += s.config.RampUpRate
					if currentRPS > s.config.MaxRPS {
						currentRPS = s.config.MaxRPS
					}
				}
			case PhaseHold:
				if time.Since(holdStart) >= s.config.HoldDuration {
					s.phase = PhaseComplete
					s.mu.Unlock()
					s.setStopReason("completed successfully")
					goto cleanup
				}
			}
			s.mu.Unlock()

			if currentRPS > maxRPSAchieved {
				maxRPSAchieved = currentRPS
			}
			s.currentRPS.Store(int64(currentRPS))

			// Update request ticker for new RPS
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

	// Record final phase
	s.recordPhaseResult(currentRPS)

	return s.buildResult(maxRPSAchieved, breakingPointRPS), nil
}

// collectResults aggregates results from workers.
func (s *StressTester) collectResults(results chan Result, done chan struct{}) {
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
	}
}

// recordPhaseResult saves metrics for the current phase.
func (s *StressTester) recordPhaseResult(targetRPS int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	total := s.totalRequests.Load()
	successes := s.successCount.Load()
	failures := s.failureCount.Load()

	var errorRate float64
	if total > 0 {
		errorRate = float64(failures) / float64(total)
	}

	var avgLatency, p99Latency time.Duration
	if len(s.latencies) > 0 {
		_, _, avgLatency, _, _, p99Latency = calculatePercentiles(s.latencies)
	}

	elapsed := time.Since(s.startTime)
	var actualRPS float64
	if elapsed.Seconds() > 0 {
		actualRPS = float64(total) / elapsed.Seconds()
	}

	result := PhaseResult{
		Phase:      s.phase,
		TargetRPS:  targetRPS,
		ActualRPS:  actualRPS,
		Duration:   elapsed,
		Requests:   total,
		Successes:  successes,
		Failures:   failures,
		ErrorRate:  errorRate,
		AvgLatency: avgLatency,
		P99Latency: p99Latency,
	}

	s.phaseResults = append(s.phaseResults, result)
}

// buildResult creates the final stress test result.
func (s *StressTester) buildResult(maxRPS, breakingRPS int) *StressResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	endTime := time.Now()
	total := s.totalRequests.Load()
	successes := s.successCount.Load()
	failures := s.failureCount.Load()

	summary := &Summary{
		TestName:      s.config.Name,
		TestType:      TestTypeStress,
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

	return &StressResult{
		Summary:          summary,
		Phase:            s.phase,
		MaxRPSAchieved:   maxRPS,
		BreakingPointRPS: breakingRPS,
		StopReason:       s.stopReason,
		PhaseResults:     s.phaseResults,
	}
}

// getErrorRate returns the current error rate.
func (s *StressTester) getErrorRate() float64 {
	total := s.totalRequests.Load()
	if total == 0 {
		return 0
	}
	return float64(s.failureCount.Load()) / float64(total)
}

// getP99Latency returns the current p99 latency.
func (s *StressTester) getP99Latency() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.latencies) == 0 {
		return 0
	}
	_, _, _, _, _, p99 := calculatePercentiles(s.latencies)
	return p99
}

// setStopReason records why the test stopped.
func (s *StressTester) setStopReason(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopReason == "" {
		s.stopReason = reason
	}
}

// CurrentRPS returns the current target RPS.
func (s *StressTester) CurrentRPS() int {
	return int(s.currentRPS.Load())
}

// Phase returns the current test phase.
func (s *StressTester) Phase() StressPhase {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.phase
}
