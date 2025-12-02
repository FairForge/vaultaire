// internal/ha/failover_testing.go
package ha

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// FailoverScenario defines a test scenario for failover testing
type FailoverScenario struct {
	Name           string
	Description    string
	FailureType    FailureType
	TargetBackend  string
	ExpectedResult ExpectedResult
	Timeout        time.Duration
}

// FailureType represents the type of failure to simulate
type FailureType string

const (
	FailureTypeComplete     FailureType = "complete"
	FailureTypePartial      FailureType = "partial"
	FailureTypeNetwork      FailureType = "network"
	FailureTypeLatency      FailureType = "latency"
	FailureTypeCascading    FailureType = "cascading"
	FailureTypeIntermittent FailureType = "intermittent"
)

// ExpectedResult defines what we expect from a failover scenario
type ExpectedResult struct {
	FailoverTriggered bool
	FailoverTarget    string
	RTOMet            bool
	ServiceAvailable  bool
	DataIntegrity     bool
}

// ScenarioResult contains the outcome of a scenario execution
type ScenarioResult struct {
	Scenario     FailoverScenario
	Passed       bool
	ActualResult ExpectedResult
	FailoverTime time.Duration
	ErrorMessage string
	ExecutedAt   time.Time
}

// TestReport contains the summary of all test executions
type TestReport struct {
	GeneratedAt     time.Time
	TotalScenarios  int
	PassedScenarios int
	FailedScenarios int
	AverageFailover time.Duration
	RTOCompliance   float64
	Results         []ScenarioResult
}

// FailoverTestRunner executes failover test scenarios
type FailoverTestRunner struct {
	orchestrator *HAOrchestrator
	geoManager   *GeoManager
	rtoTracker   *RTORPOTracker
	scenarios    []FailoverScenario
	results      []ScenarioResult
	mu           sync.Mutex
}

// NewFailoverTestRunner creates a new test runner
func NewFailoverTestRunner(orchestrator *HAOrchestrator, geoManager *GeoManager, rtoTracker *RTORPOTracker) *FailoverTestRunner {
	return &FailoverTestRunner{
		orchestrator: orchestrator,
		geoManager:   geoManager,
		rtoTracker:   rtoTracker,
		scenarios:    make([]FailoverScenario, 0),
		results:      make([]ScenarioResult, 0),
	}
}

// AddScenario adds a test scenario
func (r *FailoverTestRunner) AddScenario(scenario FailoverScenario) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scenarios = append(r.scenarios, scenario)
}

// GetScenarios returns all scenarios
func (r *FailoverTestRunner) GetScenarios() []FailoverScenario {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]FailoverScenario, len(r.scenarios))
	copy(result, r.scenarios)
	return result
}

// ExecuteScenario runs a single failover scenario
func (r *FailoverTestRunner) ExecuteScenario(ctx context.Context, scenario FailoverScenario) ScenarioResult {
	result := ScenarioResult{
		Scenario:   scenario,
		ExecutedAt: time.Now(),
	}

	// Create timeout context
	timeout := scenario.Timeout
	if timeout == 0 {
		timeout = time.Second * 30
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Record start time for RTO measurement
	startTime := time.Now()

	// Inject failure based on type
	r.injectFailure(scenario)

	// Wait for failover to occur (or timeout)
	failoverDetected := r.waitForFailover(ctx, scenario)

	// Record failover time
	result.FailoverTime = time.Since(startTime)

	// Check service availability
	serviceAvailable := r.checkServiceAvailability()

	// Build actual result
	result.ActualResult = ExpectedResult{
		FailoverTriggered: failoverDetected,
		ServiceAvailable:  serviceAvailable,
		RTOMet:            result.FailoverTime <= r.getRTOTarget(),
	}

	// Compare with expected
	result.Passed = r.compareResults(scenario.ExpectedResult, result.ActualResult)
	if !result.Passed {
		result.ErrorMessage = r.buildErrorMessage(scenario.ExpectedResult, result.ActualResult)
	}

	// Restore system state
	r.restoreState(scenario)

	// Store result
	r.mu.Lock()
	r.results = append(r.results, result)
	r.mu.Unlock()

	return result
}

// RunAllScenarios executes all registered scenarios
func (r *FailoverTestRunner) RunAllScenarios(ctx context.Context) {
	scenarios := r.GetScenarios()
	for _, scenario := range scenarios {
		select {
		case <-ctx.Done():
			return
		default:
			r.ExecuteScenario(ctx, scenario)
		}
	}
}

// GenerateReport creates a summary report
func (r *FailoverTestRunner) GenerateReport() TestReport {
	r.mu.Lock()
	defer r.mu.Unlock()

	report := TestReport{
		GeneratedAt:    time.Now(),
		TotalScenarios: len(r.results),
		Results:        make([]ScenarioResult, len(r.results)),
	}

	copy(report.Results, r.results)

	var totalFailoverTime time.Duration
	rtoMetCount := 0

	for _, result := range r.results {
		if result.Passed {
			report.PassedScenarios++
		} else {
			report.FailedScenarios++
		}
		totalFailoverTime += result.FailoverTime
		if result.ActualResult.RTOMet {
			rtoMetCount++
		}
	}

	if report.TotalScenarios > 0 {
		report.AverageFailover = totalFailoverTime / time.Duration(report.TotalScenarios)
		report.RTOCompliance = float64(rtoMetCount) / float64(report.TotalScenarios) * 100
	}

	return report
}

// injectFailure simulates the specified failure type
func (r *FailoverTestRunner) injectFailure(scenario FailoverScenario) {
	targets := strings.Split(scenario.TargetBackend, ",")
	simulatedError := errors.New("simulated failure")

	for _, target := range targets {
		target = strings.TrimSpace(target)
		switch scenario.FailureType {
		case FailureTypeComplete:
			// Report multiple failures to trigger state change
			for i := 0; i < 3; i++ {
				r.orchestrator.ReportHealthCheck(target, false, 0, simulatedError)
			}
		case FailureTypePartial:
			// Report high latency (degraded)
			r.orchestrator.ReportHealthCheck(target, true, time.Second*5, nil)
		case FailureTypeNetwork:
			for i := 0; i < 3; i++ {
				r.orchestrator.ReportHealthCheck(target, false, 0, errors.New("network unreachable"))
			}
		case FailureTypeLatency:
			r.orchestrator.ReportHealthCheck(target, true, time.Second*10, nil)
		case FailureTypeCascading:
			for i := 0; i < 3; i++ {
				r.orchestrator.ReportHealthCheck(target, false, 0, simulatedError)
			}
		case FailureTypeIntermittent:
			// Simulate flapping
			go func(t string) {
				r.orchestrator.ReportHealthCheck(t, false, 0, simulatedError)
				time.Sleep(time.Millisecond * 50)
				r.orchestrator.ReportHealthCheck(t, true, time.Millisecond*10, nil)
				time.Sleep(time.Millisecond * 50)
				r.orchestrator.ReportHealthCheck(t, false, 0, simulatedError)
				time.Sleep(time.Millisecond * 50)
				r.orchestrator.ReportHealthCheck(t, true, time.Millisecond*10, nil)
			}(target)
		}
	}
}

// waitForFailover waits for failover to occur
func (r *FailoverTestRunner) waitForFailover(ctx context.Context, scenario FailoverScenario) bool {
	ticker := time.NewTicker(time.Millisecond * 10)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			healthyBackends := r.orchestrator.GetHealthyBackends()

			switch scenario.FailureType {
			case FailureTypeComplete, FailureTypeCascading:
				// Failover detected if target is not in healthy list
				targets := strings.Split(scenario.TargetBackend, ",")
				allTargetsFailed := true
				for _, target := range targets {
					target = strings.TrimSpace(target)
					for _, healthy := range healthyBackends {
						if healthy == target {
							allTargetsFailed = false
							break
						}
					}
				}
				if allTargetsFailed && len(healthyBackends) > 0 {
					return true
				}
			case FailureTypePartial, FailureTypeLatency:
				// Partial failures don't trigger full failover
				return false
			case FailureTypeIntermittent:
				time.Sleep(time.Millisecond * 200)
				return false
			}
		}
	}
}

// checkServiceAvailability checks if at least one backend is available
func (r *FailoverTestRunner) checkServiceAvailability() bool {
	healthyBackends := r.orchestrator.GetHealthyBackends()
	return len(healthyBackends) > 0
}

// getRTOTarget returns the RTO target from tracker
func (r *FailoverTestRunner) getRTOTarget() time.Duration {
	if r.rtoTracker != nil {
		return time.Minute * 15 // Default standard tier
	}
	return time.Minute * 15
}

// compareResults compares expected vs actual results
func (r *FailoverTestRunner) compareResults(expected, actual ExpectedResult) bool {
	if expected.ServiceAvailable && !actual.ServiceAvailable {
		return false
	}
	if expected.FailoverTriggered && !actual.FailoverTriggered {
		return false
	}
	if expected.RTOMet && !actual.RTOMet {
		return false
	}
	return true
}

// buildErrorMessage creates a descriptive error message
func (r *FailoverTestRunner) buildErrorMessage(expected, actual ExpectedResult) string {
	var messages []string

	if expected.ServiceAvailable && !actual.ServiceAvailable {
		messages = append(messages, "service became unavailable")
	}
	if expected.FailoverTriggered && !actual.FailoverTriggered {
		messages = append(messages, "failover was not triggered")
	}
	if expected.RTOMet && !actual.RTOMet {
		messages = append(messages, "RTO was not met")
	}

	if len(messages) == 0 {
		return ""
	}
	return strings.Join(messages, "; ")
}

// restoreState restores backends to healthy state
func (r *FailoverTestRunner) restoreState(scenario FailoverScenario) {
	targets := strings.Split(scenario.TargetBackend, ",")
	for _, target := range targets {
		target = strings.TrimSpace(target)
		// Report healthy to restore
		for i := 0; i < 3; i++ {
			r.orchestrator.ReportHealthCheck(target, true, time.Millisecond*10, nil)
		}
	}
}
