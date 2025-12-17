// Package loadtest provides infrastructure for load, stress, and chaos testing.
package loadtest

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// ChaosType defines the type of chaos to inject.
type ChaosType string

const (
	ChaosLatency            ChaosType = "latency"   // Add artificial latency
	ChaosError              ChaosType = "error"     // Inject errors
	ChaosTimeout            ChaosType = "timeout"   // Cause timeouts
	ChaosPartition          ChaosType = "partition" // Simulate network partition
	ChaosResourceExhaustion ChaosType = "resource"  // Exhaust resources
)

// ChaosConfig defines parameters for chaos testing.
type ChaosConfig struct {
	Name           string
	Duration       time.Duration
	TargetRPS      int
	MaxConcurrency int
	Timeout        time.Duration

	// Chaos parameters
	ChaosTypes       []ChaosType   // Types of chaos to inject
	ChaosProbability float64       // Probability of chaos per request (0.0-1.0)
	ChaosInterval    time.Duration // How often chaos events occur
	ChaosDuration    time.Duration // How long each chaos event lasts

	// Latency chaos
	LatencyMin time.Duration // Minimum added latency
	LatencyMax time.Duration // Maximum added latency

	// Error chaos
	ErrorTypes []error // Errors to inject

	// Recovery
	RecoveryPeriod time.Duration // Observation period after chaos
}

// DefaultChaosConfig returns sensible defaults for chaos testing.
func DefaultChaosConfig(name string) *ChaosConfig {
	return &ChaosConfig{
		Name:             name,
		Duration:         5 * time.Minute,
		TargetRPS:        50,
		MaxConcurrency:   30,
		Timeout:          30 * time.Second,
		ChaosTypes:       []ChaosType{ChaosLatency, ChaosError},
		ChaosProbability: 0.1, // 10% of requests
		ChaosInterval:    30 * time.Second,
		ChaosDuration:    10 * time.Second,
		LatencyMin:       100 * time.Millisecond,
		LatencyMax:       500 * time.Millisecond,
		ErrorTypes:       []error{fmt.Errorf("chaos: simulated failure")},
		RecoveryPeriod:   30 * time.Second,
	}
}

// ChaosEvent represents a chaos injection event.
type ChaosEvent struct {
	Type      ChaosType
	StartTime time.Time
	EndTime   time.Time
	Duration  time.Duration
	Affected  int64 // Number of requests affected
}

// ChaosResult captures chaos test outcomes.
type ChaosResult struct {
	*Summary
	Config          *ChaosConfig
	ChaosEvents     []ChaosEvent
	RecoveryMetrics RecoveryMetrics
	ResilienceScore float64 // 0-100, higher is better
}

// RecoveryMetrics captures how well the system recovered from chaos.
type RecoveryMetrics struct {
	PreChaosRPS          float64
	DuringChaosRPS       float64
	PostChaosRPS         float64
	PreChaosErrorRate    float64
	DuringChaosErrorRate float64
	PostChaosErrorRate   float64
	RecoveryTime         time.Duration // Time to return to baseline
	FullyRecovered       bool
}

// ChaosTester executes chaos engineering tests.
type ChaosTester struct {
	config     *ChaosConfig
	workerFunc WorkerFunc
	rng        *rand.Rand

	// Metrics
	totalRequests atomic.Int64
	successCount  atomic.Int64
	failureCount  atomic.Int64
	chaosAffected atomic.Int64

	// State
	mu           sync.RWMutex
	running      bool
	startTime    time.Time
	chaosActive  bool
	currentChaos ChaosType
	chaosStart   time.Time
	latencies    []time.Duration
	events       []ChaosEvent

	// Phase tracking for recovery metrics
	preChaosRequests    atomic.Int64
	preChaosErrors      atomic.Int64
	duringChaosRequests atomic.Int64
	duringChaosErrors   atomic.Int64
	postChaosRequests   atomic.Int64
	postChaosErrors     atomic.Int64
	phase               string // "pre", "during", "post"
}

// NewChaosTester creates a new chaos testing instance.
func NewChaosTester(config *ChaosConfig, workerFunc WorkerFunc) *ChaosTester {
	if config == nil {
		config = DefaultChaosConfig("default-chaos")
	}

	return &ChaosTester{
		config:     config,
		workerFunc: workerFunc,
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
		latencies:  make([]time.Duration, 0, 10000),
		events:     make([]ChaosEvent, 0),
		phase:      "pre",
	}
}

// Run executes the chaos test.
func (c *ChaosTester) Run(ctx context.Context) (*ChaosResult, error) {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return nil, fmt.Errorf("chaos test already running")
	}
	c.running = true
	c.startTime = time.Now()
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()

	// Create test context
	testCtx, cancel := context.WithTimeout(ctx, c.config.Duration)
	defer cancel()

	// Results collection
	results := make(chan Result, c.config.MaxConcurrency*10)
	collectorDone := make(chan struct{})
	go c.collectResults(results, collectorDone)

	// Worker management
	var wg sync.WaitGroup
	workerCtx, workerCancel := context.WithCancel(testCtx)
	defer workerCancel()

	semaphore := make(chan struct{}, c.config.MaxConcurrency)
	workerID := atomic.Int64{}

	// Request ticker
	requestTicker := time.NewTicker(time.Second / time.Duration(c.config.TargetRPS))
	defer requestTicker.Stop()

	// Chaos ticker
	chaosTicker := time.NewTicker(c.config.ChaosInterval)
	defer chaosTicker.Stop()

	for {
		select {
		case <-workerCtx.Done():
			goto cleanup

		case <-chaosTicker.C:
			c.handleChaosTick()

		case <-requestTicker.C:
			select {
			case semaphore <- struct{}{}:
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					defer func() { <-semaphore }()

					result := c.executeWithChaos(workerCtx, id)
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

	return c.buildResult(), nil
}

// handleChaosTick manages chaos state transitions.
func (c *ChaosTester) handleChaosTick() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.chaosActive {
		// Check if chaos should end
		if time.Since(c.chaosStart) >= c.config.ChaosDuration {
			c.endChaosEvent()
		}
	} else {
		// Maybe start new chaos
		if len(c.config.ChaosTypes) > 0 {
			c.startChaosEvent()
		}
	}
}

// startChaosEvent begins a new chaos injection.
func (c *ChaosTester) startChaosEvent() {
	chaosType := c.config.ChaosTypes[c.rng.Intn(len(c.config.ChaosTypes))]

	c.chaosActive = true
	c.currentChaos = chaosType
	c.chaosStart = time.Now()
	c.chaosAffected.Store(0)
	c.phase = "during"
}

// endChaosEvent completes the current chaos injection.
func (c *ChaosTester) endChaosEvent() {
	event := ChaosEvent{
		Type:      c.currentChaos,
		StartTime: c.chaosStart,
		EndTime:   time.Now(),
		Duration:  time.Since(c.chaosStart),
		Affected:  c.chaosAffected.Load(),
	}
	c.events = append(c.events, event)

	c.chaosActive = false
	c.currentChaos = ""
	c.phase = "post"
}

// executeWithChaos runs the worker with potential chaos injection.
func (c *ChaosTester) executeWithChaos(ctx context.Context, id int) Result {
	c.mu.RLock()
	chaosActive := c.chaosActive
	chaosType := c.currentChaos
	c.mu.RUnlock()

	// Decide if this request should be affected by chaos
	shouldInjectChaos := chaosActive && c.rng.Float64() < c.config.ChaosProbability

	if shouldInjectChaos {
		c.chaosAffected.Add(1)
		return c.injectChaos(ctx, id, chaosType)
	}

	return c.workerFunc(ctx, id)
}

// injectChaos applies the specified chaos to a request.
func (c *ChaosTester) injectChaos(ctx context.Context, id int, chaosType ChaosType) Result {
	start := time.Now()

	switch chaosType {
	case ChaosLatency:
		// Add artificial latency
		latency := c.config.LatencyMin + time.Duration(c.rng.Int63n(int64(c.config.LatencyMax-c.config.LatencyMin)))
		select {
		case <-time.After(latency):
		case <-ctx.Done():
			return Result{
				StartTime: start,
				Duration:  time.Since(start),
				Error:     ctx.Err(),
			}
		}
		// Still execute the actual request
		result := c.workerFunc(ctx, id)
		result.Duration += latency
		return result

	case ChaosError:
		// Inject an error
		var err error
		if len(c.config.ErrorTypes) > 0 {
			err = c.config.ErrorTypes[c.rng.Intn(len(c.config.ErrorTypes))]
		} else {
			err = fmt.Errorf("chaos: injected error")
		}
		return Result{
			StartTime: start,
			Duration:  time.Since(start),
			Error:     err,
		}

	case ChaosTimeout:
		// Simulate timeout by waiting until context deadline or configured timeout
		select {
		case <-time.After(c.config.Timeout):
		case <-ctx.Done():
		}
		return Result{
			StartTime: start,
			Duration:  time.Since(start),
			Error:     context.DeadlineExceeded,
		}

	case ChaosPartition:
		// Simulate network partition - immediate error
		return Result{
			StartTime: start,
			Duration:  time.Millisecond,
			Error:     fmt.Errorf("chaos: network partition"),
		}

	case ChaosResourceExhaustion:
		// Simulate resource exhaustion - slow response with possible error
		time.Sleep(c.config.LatencyMax)
		if c.rng.Float64() < 0.5 {
			return Result{
				StartTime: start,
				Duration:  time.Since(start),
				Error:     fmt.Errorf("chaos: resource exhaustion"),
			}
		}
		return c.workerFunc(ctx, id)

	default:
		return c.workerFunc(ctx, id)
	}
}

// collectResults aggregates results from workers.
func (c *ChaosTester) collectResults(results chan Result, done chan struct{}) {
	defer close(done)

	for result := range results {
		c.totalRequests.Add(1)

		// Track by phase
		c.mu.RLock()
		phase := c.phase
		c.mu.RUnlock()

		switch phase {
		case "pre":
			c.preChaosRequests.Add(1)
			if result.Error != nil {
				c.preChaosErrors.Add(1)
			}
		case "during":
			c.duringChaosRequests.Add(1)
			if result.Error != nil {
				c.duringChaosErrors.Add(1)
			}
		case "post":
			c.postChaosRequests.Add(1)
			if result.Error != nil {
				c.postChaosErrors.Add(1)
			}
		}

		if result.Error != nil {
			c.failureCount.Add(1)
		} else {
			c.successCount.Add(1)
		}

		c.mu.Lock()
		c.latencies = append(c.latencies, result.Duration)
		c.mu.Unlock()
	}
}

// buildResult creates the final chaos test result.
func (c *ChaosTester) buildResult() *ChaosResult {
	c.mu.RLock()
	defer c.mu.RUnlock()

	endTime := time.Now()
	total := c.totalRequests.Load()
	successes := c.successCount.Load()
	failures := c.failureCount.Load()

	summary := &Summary{
		TestName:      c.config.Name,
		TestType:      TestTypeLoad, // Chaos is a variant of load
		StartTime:     c.startTime,
		EndTime:       endTime,
		TotalRequests: total,
		SuccessCount:  successes,
		FailureCount:  failures,
		Errors:        make(map[string]int64),
	}

	duration := endTime.Sub(c.startTime).Seconds()
	if duration > 0 {
		summary.RequestsPerSec = float64(total) / duration
	}
	if total > 0 {
		summary.ErrorRate = float64(failures) / float64(total)
	}

	if len(c.latencies) > 0 {
		summary.MinLatency, summary.MaxLatency, summary.AvgLatency,
			summary.P50Latency, summary.P95Latency, summary.P99Latency = calculatePercentiles(c.latencies)
	}

	// Calculate recovery metrics
	recoveryMetrics := c.calculateRecoveryMetrics()

	// Calculate resilience score
	resilienceScore := c.calculateResilienceScore(recoveryMetrics)

	return &ChaosResult{
		Summary:         summary,
		Config:          c.config,
		ChaosEvents:     c.events,
		RecoveryMetrics: recoveryMetrics,
		ResilienceScore: resilienceScore,
	}
}

// calculateRecoveryMetrics computes recovery statistics.
func (c *ChaosTester) calculateRecoveryMetrics() RecoveryMetrics {
	preReqs := c.preChaosRequests.Load()
	preErrs := c.preChaosErrors.Load()
	duringReqs := c.duringChaosRequests.Load()
	duringErrs := c.duringChaosErrors.Load()
	postReqs := c.postChaosRequests.Load()
	postErrs := c.postChaosErrors.Load()

	var preErrorRate, duringErrorRate, postErrorRate float64
	if preReqs > 0 {
		preErrorRate = float64(preErrs) / float64(preReqs)
	}
	if duringReqs > 0 {
		duringErrorRate = float64(duringErrs) / float64(duringReqs)
	}
	if postReqs > 0 {
		postErrorRate = float64(postErrs) / float64(postReqs)
	}

	// Simplified RPS calculation (would need timestamps for accurate values)
	testDuration := time.Since(c.startTime).Seconds()
	var preRPS, duringRPS, postRPS float64
	if testDuration > 0 {
		totalReqs := float64(c.totalRequests.Load())
		avgRPS := totalReqs / testDuration
		preRPS = avgRPS
		duringRPS = avgRPS * (1 - c.config.ChaosProbability)
		postRPS = avgRPS
	}

	// Check if fully recovered
	fullyRecovered := postErrorRate <= preErrorRate*1.1 // Within 10% of baseline

	return RecoveryMetrics{
		PreChaosRPS:          preRPS,
		DuringChaosRPS:       duringRPS,
		PostChaosRPS:         postRPS,
		PreChaosErrorRate:    preErrorRate,
		DuringChaosErrorRate: duringErrorRate,
		PostChaosErrorRate:   postErrorRate,
		RecoveryTime:         c.config.RecoveryPeriod, // Simplified
		FullyRecovered:       fullyRecovered,
	}
}

// calculateResilienceScore computes an overall resilience rating.
func (c *ChaosTester) calculateResilienceScore(metrics RecoveryMetrics) float64 {
	var score float64 = 100

	// Deduct for high error rate during chaos (expected, but excessive is bad)
	if metrics.DuringChaosErrorRate > 0.5 {
		score -= 20
	} else if metrics.DuringChaosErrorRate > 0.3 {
		score -= 10
	}

	// Deduct for not recovering
	if !metrics.FullyRecovered {
		score -= 30
	}

	// Deduct for high post-chaos error rate
	if metrics.PostChaosErrorRate > 0.1 {
		score -= 20
	} else if metrics.PostChaosErrorRate > 0.05 {
		score -= 10
	}

	// Deduct for RPS degradation post-chaos
	if metrics.PreChaosRPS > 0 {
		rpsDegradation := (metrics.PreChaosRPS - metrics.PostChaosRPS) / metrics.PreChaosRPS
		if rpsDegradation > 0.2 {
			score -= 20
		} else if rpsDegradation > 0.1 {
			score -= 10
		}
	}

	if score < 0 {
		score = 0
	}

	return score
}

// IsChaosActive returns whether chaos is currently being injected.
func (c *ChaosTester) IsChaosActive() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.chaosActive
}

// CurrentChaos returns the type of chaos currently being injected.
func (c *ChaosTester) CurrentChaos() ChaosType {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentChaos
}

// Events returns the chaos events that have occurred.
func (c *ChaosTester) Events() []ChaosEvent {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make([]ChaosEvent, len(c.events))
	copy(result, c.events)
	return result
}
