// internal/ha/orchestrator_test.go
package ha

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHAOrchestrator_RegisterBackend(t *testing.T) {
	orch := NewHAOrchestrator()
	defer orch.Stop()

	orch.RegisterBackend("lyve", BackendConfig{
		Primary:       true,
		HealthCheck:   func() error { return nil },
		CheckInterval: 10 * time.Second,
	})

	status := orch.GetBackendStatus("lyve")
	assert.Equal(t, StateHealthy, status.State)
}

func TestHAOrchestrator_HealthTransitions(t *testing.T) {
	orch := NewHAOrchestrator()
	defer orch.Stop()

	orch.RegisterBackend("lyve", BackendConfig{
		Primary:          true,
		FailureThreshold: 3,
	})

	// Initially healthy
	assert.Equal(t, StateHealthy, orch.GetBackendStatus("lyve").State)

	// Report failures - should transition to degraded then failed
	orch.ReportHealthCheck("lyve", false, 0, nil)
	assert.Equal(t, StateHealthy, orch.GetBackendStatus("lyve").State) // 1 failure

	orch.ReportHealthCheck("lyve", false, 0, nil)
	assert.Equal(t, StateDegraded, orch.GetBackendStatus("lyve").State) // 2 failures

	orch.ReportHealthCheck("lyve", false, 0, nil)
	assert.Equal(t, StateFailed, orch.GetBackendStatus("lyve").State) // 3 failures
}

func TestHAOrchestrator_AutomaticFailover(t *testing.T) {
	orch := NewHAOrchestrator()
	defer orch.Stop()

	// Register primary and secondary
	orch.RegisterBackend("lyve", BackendConfig{
		Primary:          true,
		FailureThreshold: 2,
	})
	orch.RegisterBackend("quotaless", BackendConfig{
		Primary: false, // Secondary
	})

	// Configure failover
	orch.ConfigureFailover("lyve", FailoverRule{
		SecondaryBackend: "quotaless",
		AutoFailover:     true,
		FailoverDelay:    10 * time.Millisecond,
	})

	// Fail primary
	orch.ReportHealthCheck("lyve", false, 0, nil)
	orch.ReportHealthCheck("lyve", false, 0, nil)

	// Wait for failover
	time.Sleep(50 * time.Millisecond)

	// Check active backend changed
	active := orch.GetActiveBackend("lyve")
	assert.Equal(t, "quotaless", active)
}

func TestHAOrchestrator_RecoveryDetection(t *testing.T) {
	orch := NewHAOrchestrator()
	defer orch.Stop()

	orch.RegisterBackend("lyve", BackendConfig{
		Primary:           true,
		FailureThreshold:  2,
		RecoveryThreshold: 2,
	})

	// Fail the backend
	orch.ReportHealthCheck("lyve", false, 0, nil)
	orch.ReportHealthCheck("lyve", false, 0, nil)
	assert.Equal(t, StateFailed, orch.GetBackendStatus("lyve").State)

	// Report recovery
	orch.ReportHealthCheck("lyve", true, 10*time.Millisecond, nil)
	assert.Equal(t, StateRecovering, orch.GetBackendStatus("lyve").State)

	orch.ReportHealthCheck("lyve", true, 10*time.Millisecond, nil)
	assert.Equal(t, StateHealthy, orch.GetBackendStatus("lyve").State)
}

func TestHAOrchestrator_EventNotifications(t *testing.T) {
	orch := NewHAOrchestrator()
	defer orch.Stop()

	var events []HAEvent
	var mu sync.Mutex
	eventReceived := make(chan struct{}, 10)

	// Subscribe to events
	orch.Subscribe(func(event HAEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		select {
		case eventReceived <- struct{}{}:
		default:
		}
	})

	orch.RegisterBackend("lyve", BackendConfig{
		Primary:          true,
		FailureThreshold: 1,
	})

	// Wait for registration event
	select {
	case <-eventReceived:
	case <-time.After(100 * time.Millisecond):
	}

	// Trigger failure
	orch.ReportHealthCheck("lyve", false, 0, nil)

	// Wait for failure event
	select {
	case <-eventReceived:
	case <-time.After(100 * time.Millisecond):
	}

	mu.Lock()
	defer mu.Unlock()

	require.GreaterOrEqual(t, len(events), 1, "should have at least 1 event")

	// Find the failed event
	var foundFailed bool
	for _, e := range events {
		if e.Type == EventBackendFailed {
			foundFailed = true
			break
		}
	}
	assert.True(t, foundFailed, "should have received backend failed event")
}

func TestHAOrchestrator_CircuitBreaker(t *testing.T) {
	orch := NewHAOrchestrator()
	defer orch.Stop()

	orch.RegisterBackend("lyve", BackendConfig{
		Primary:          true,
		FailureThreshold: 3,
		CircuitBreaker:   true,
	})

	// Trip the circuit breaker
	for i := 0; i < 5; i++ {
		orch.ReportHealthCheck("lyve", false, 0, nil)
	}

	// Circuit should be open
	assert.True(t, orch.IsCircuitOpen("lyve"))
}

func TestHAOrchestrator_GetHealthyBackends(t *testing.T) {
	orch := NewHAOrchestrator()
	defer orch.Stop()

	orch.RegisterBackend("lyve", BackendConfig{Primary: true})
	orch.RegisterBackend("quotaless", BackendConfig{Primary: false})
	orch.RegisterBackend("onedrive", BackendConfig{Primary: false, FailureThreshold: 1})

	// Fail one backend
	orch.ReportHealthCheck("onedrive", false, 0, nil)

	healthy := orch.GetHealthyBackends()
	assert.Len(t, healthy, 2)
	assert.Contains(t, healthy, "lyve")
	assert.Contains(t, healthy, "quotaless")
}

func TestHAOrchestrator_OverallStatus(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*HAOrchestrator)
		expected SystemStatus
	}{
		{
			name: "all healthy",
			setup: func(o *HAOrchestrator) {
				o.RegisterBackend("lyve", BackendConfig{Primary: true})
				o.RegisterBackend("quotaless", BackendConfig{Primary: false})
			},
			expected: SystemHealthy,
		},
		{
			name: "one degraded",
			setup: func(o *HAOrchestrator) {
				o.RegisterBackend("lyve", BackendConfig{Primary: true, FailureThreshold: 3})
				o.RegisterBackend("quotaless", BackendConfig{Primary: false})
				o.ReportHealthCheck("lyve", false, 0, nil)
				o.ReportHealthCheck("lyve", false, 0, nil)
			},
			expected: SystemDegraded,
		},
		{
			name: "primary failed with secondary",
			setup: func(o *HAOrchestrator) {
				o.RegisterBackend("lyve", BackendConfig{Primary: true, FailureThreshold: 1})
				o.RegisterBackend("quotaless", BackendConfig{Primary: false})
				o.ReportHealthCheck("lyve", false, 0, nil)
			},
			expected: SystemDegraded,
		},
		{
			name: "all failed",
			setup: func(o *HAOrchestrator) {
				o.RegisterBackend("lyve", BackendConfig{Primary: true, FailureThreshold: 1})
				o.ReportHealthCheck("lyve", false, 0, nil)
			},
			expected: SystemCritical,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orch := NewHAOrchestrator()
			defer orch.Stop()
			tt.setup(orch)
			assert.Equal(t, tt.expected, orch.GetSystemStatus())
		})
	}
}
