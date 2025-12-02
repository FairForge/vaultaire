// internal/ha/failover_test_framework_test.go
package ha

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFailoverTestRunner_NewRunner(t *testing.T) {
	t.Run("creates runner with components", func(t *testing.T) {
		geoConfig := DefaultGeoConfig()
		geoManager, err := NewGeoManager(geoConfig)
		require.NoError(t, err)

		orchestrator := NewHAOrchestrator()

		rtoConfig := GetTierDefaults(TierStandard)
		rtoTracker, err := NewRTORPOTracker(rtoConfig)
		require.NoError(t, err)

		runner := NewFailoverTestRunner(orchestrator, geoManager, rtoTracker)
		assert.NotNil(t, runner)
	})
}

func TestFailoverTestRunner_AddScenario(t *testing.T) {
	t.Run("adds scenario to runner", func(t *testing.T) {
		runner := createTestRunner(t)

		scenario := FailoverScenario{
			Name:          "primary-failure",
			Description:   "Test failover when primary backend fails",
			FailureType:   FailureTypeComplete,
			TargetBackend: "primary",
			ExpectedResult: ExpectedResult{
				FailoverTriggered: true,
				FailoverTarget:    "secondary",
				RTOMet:            true,
				ServiceAvailable:  true,
			},
			Timeout: time.Second * 30,
		}

		runner.AddScenario(scenario)
		assert.Len(t, runner.GetScenarios(), 1)
	})
}

func TestFailoverTestRunner_CompleteFailure(t *testing.T) {
	t.Run("detects and handles complete backend failure", func(t *testing.T) {
		runner := createTestRunner(t)

		// Register backends
		runner.orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})
		runner.orchestrator.RegisterBackend("secondary", BackendConfig{Primary: false})

		scenario := FailoverScenario{
			Name:          "complete-failure-test",
			FailureType:   FailureTypeComplete,
			TargetBackend: "primary",
			ExpectedResult: ExpectedResult{
				FailoverTriggered: true,
				ServiceAvailable:  true,
			},
			Timeout: time.Second * 5,
		}

		result := runner.ExecuteScenario(context.Background(), scenario)

		assert.True(t, result.Passed, "Scenario should pass: %s", result.ErrorMessage)
		assert.True(t, result.ActualResult.FailoverTriggered)
		assert.True(t, result.ActualResult.ServiceAvailable)
	})
}

func TestFailoverTestRunner_PartialFailure(t *testing.T) {
	t.Run("handles degraded backend appropriately", func(t *testing.T) {
		runner := createTestRunner(t)

		runner.orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})
		runner.orchestrator.RegisterBackend("secondary", BackendConfig{Primary: false})

		scenario := FailoverScenario{
			Name:          "partial-failure-test",
			FailureType:   FailureTypePartial,
			TargetBackend: "primary",
			ExpectedResult: ExpectedResult{
				FailoverTriggered: false,
				ServiceAvailable:  true,
			},
			Timeout: time.Second * 5,
		}

		result := runner.ExecuteScenario(context.Background(), scenario)

		assert.True(t, result.Passed, "Scenario should pass: %s", result.ErrorMessage)
		assert.True(t, result.ActualResult.ServiceAvailable)
	})
}

func TestFailoverTestRunner_CascadingFailure(t *testing.T) {
	t.Run("handles multiple backend failures", func(t *testing.T) {
		runner := createTestRunner(t)

		runner.orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})
		runner.orchestrator.RegisterBackend("secondary", BackendConfig{Primary: false})
		runner.orchestrator.RegisterBackend("tertiary", BackendConfig{Primary: false})

		scenario := FailoverScenario{
			Name:          "cascading-failure-test",
			FailureType:   FailureTypeCascading,
			TargetBackend: "primary,secondary",
			ExpectedResult: ExpectedResult{
				FailoverTriggered: true,
				FailoverTarget:    "tertiary",
				ServiceAvailable:  true,
			},
			Timeout: time.Second * 10,
		}

		result := runner.ExecuteScenario(context.Background(), scenario)

		assert.True(t, result.Passed, "Scenario should pass: %s", result.ErrorMessage)
	})
}

func TestFailoverTestRunner_IntermittentFailure(t *testing.T) {
	t.Run("handles flapping backend without thrashing", func(t *testing.T) {
		runner := createTestRunner(t)

		runner.orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})
		runner.orchestrator.RegisterBackend("secondary", BackendConfig{Primary: false})

		scenario := FailoverScenario{
			Name:          "intermittent-failure-test",
			FailureType:   FailureTypeIntermittent,
			TargetBackend: "primary",
			ExpectedResult: ExpectedResult{
				ServiceAvailable: true,
			},
			Timeout: time.Second * 10,
		}

		result := runner.ExecuteScenario(context.Background(), scenario)

		assert.True(t, result.Passed, "Scenario should pass: %s", result.ErrorMessage)
		assert.True(t, result.ActualResult.ServiceAvailable)
	})
}

func TestFailoverTestRunner_RTOCompliance(t *testing.T) {
	t.Run("measures failover time against RTO", func(t *testing.T) {
		runner := createTestRunner(t)

		runner.orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})
		runner.orchestrator.RegisterBackend("secondary", BackendConfig{Primary: false})

		scenario := FailoverScenario{
			Name:          "rto-compliance-test",
			FailureType:   FailureTypeComplete,
			TargetBackend: "primary",
			ExpectedResult: ExpectedResult{
				FailoverTriggered: true,
				RTOMet:            true,
				ServiceAvailable:  true,
			},
			Timeout: time.Second * 5,
		}

		result := runner.ExecuteScenario(context.Background(), scenario)

		assert.True(t, result.Passed, "Scenario should pass: %s", result.ErrorMessage)
		assert.True(t, result.ActualResult.RTOMet, "RTO should be met")
		assert.Less(t, result.FailoverTime, time.Second*5, "Failover should complete within 5 seconds")
	})
}

func TestFailoverTestRunner_RecoveryAfterFailover(t *testing.T) {
	t.Run("recovers original backend after failover", func(t *testing.T) {
		runner := createTestRunner(t)

		runner.orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})
		runner.orchestrator.RegisterBackend("secondary", BackendConfig{Primary: false})

		// Simulate failure using ReportHealthCheck
		for i := 0; i < 3; i++ {
			runner.orchestrator.ReportHealthCheck("primary", false, 0, errors.New("simulated failure"))
		}

		// Wait for state change
		time.Sleep(time.Millisecond * 100)

		// Simulate recovery
		for i := 0; i < 3; i++ {
			runner.orchestrator.ReportHealthCheck("primary", true, time.Millisecond*10, nil)
		}

		// Check that primary can be used again
		healthyBackends := runner.orchestrator.GetHealthyBackends()
		assert.Contains(t, healthyBackends, "primary")
	})
}

func TestFailoverTestRunner_GenerateReport(t *testing.T) {
	t.Run("generates test report", func(t *testing.T) {
		runner := createTestRunner(t)

		runner.orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})
		runner.orchestrator.RegisterBackend("secondary", BackendConfig{Primary: false})

		scenarios := []FailoverScenario{
			{
				Name:          "test-1",
				FailureType:   FailureTypeComplete,
				TargetBackend: "primary",
				ExpectedResult: ExpectedResult{
					FailoverTriggered: true,
					ServiceAvailable:  true,
				},
				Timeout: time.Second * 5,
			},
			{
				Name:          "test-2",
				FailureType:   FailureTypePartial,
				TargetBackend: "primary",
				ExpectedResult: ExpectedResult{
					ServiceAvailable: true,
				},
				Timeout: time.Second * 5,
			},
		}

		for _, s := range scenarios {
			runner.AddScenario(s)
		}

		ctx := context.Background()
		runner.RunAllScenarios(ctx)

		report := runner.GenerateReport()
		assert.Equal(t, 2, report.TotalScenarios)
		assert.NotEmpty(t, report.GeneratedAt)
	})
}

func TestFailoverTestRunner_ConcurrentFailures(t *testing.T) {
	t.Run("handles concurrent failure injections", func(t *testing.T) {
		runner := createTestRunner(t)

		runner.orchestrator.RegisterBackend("backend-1", BackendConfig{Primary: true})
		runner.orchestrator.RegisterBackend("backend-2", BackendConfig{Primary: false})
		runner.orchestrator.RegisterBackend("backend-3", BackendConfig{Primary: false})

		var wg sync.WaitGroup
		results := make(chan ScenarioResult, 3)

		scenarios := []FailoverScenario{
			{Name: "concurrent-1", FailureType: FailureTypeComplete, TargetBackend: "backend-1", Timeout: time.Second * 5, ExpectedResult: ExpectedResult{ServiceAvailable: true}},
			{Name: "concurrent-2", FailureType: FailureTypePartial, TargetBackend: "backend-2", Timeout: time.Second * 5, ExpectedResult: ExpectedResult{ServiceAvailable: true}},
			{Name: "concurrent-3", FailureType: FailureTypeLatency, TargetBackend: "backend-3", Timeout: time.Second * 5, ExpectedResult: ExpectedResult{ServiceAvailable: true}},
		}

		for _, s := range scenarios {
			wg.Add(1)
			go func(scenario FailoverScenario) {
				defer wg.Done()
				result := runner.ExecuteScenario(context.Background(), scenario)
				results <- result
			}(s)
		}

		wg.Wait()
		close(results)

		passedCount := 0
		for result := range results {
			if result.Passed {
				passedCount++
			}
		}

		assert.GreaterOrEqual(t, passedCount, 2, "At least 2 scenarios should pass")
	})
}

func TestFailoverTestRunner_TimeoutHandling(t *testing.T) {
	t.Run("respects scenario timeout", func(t *testing.T) {
		runner := createTestRunner(t)

		runner.orchestrator.RegisterBackend("primary", BackendConfig{Primary: true})

		scenario := FailoverScenario{
			Name:          "timeout-test",
			FailureType:   FailureTypeComplete,
			TargetBackend: "primary",
			ExpectedResult: ExpectedResult{
				ServiceAvailable: true,
			},
			Timeout: time.Millisecond * 100,
		}

		start := time.Now()
		result := runner.ExecuteScenario(context.Background(), scenario)
		elapsed := time.Since(start)

		assert.Less(t, elapsed, time.Second*2)
		assert.NotNil(t, result)
	})
}

// Helper function to create a test runner
func createTestRunner(t *testing.T) *FailoverTestRunner {
	geoConfig := DefaultGeoConfig()
	geoManager, err := NewGeoManager(geoConfig)
	require.NoError(t, err)

	orchestrator := NewHAOrchestrator()

	rtoConfig := GetTierDefaults(TierStandard)
	rtoTracker, err := NewRTORPOTracker(rtoConfig)
	require.NoError(t, err)

	return NewFailoverTestRunner(orchestrator, geoManager, rtoTracker)
}
