// internal/engine/disaster_recovery_test.go
package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDisasterRecovery_DetectFailure(t *testing.T) {
	dr := NewDisasterRecovery()

	// Simulate backend failure
	dr.ReportFailure("backend1", "Connection timeout")

	status := dr.GetBackendStatus("backend1")
	assert.Equal(t, "FAILED", status.State)
	assert.Contains(t, status.LastError, "Connection timeout")
}

func TestDisasterRecovery_AutomaticFailover(t *testing.T) {
	dr := NewDisasterRecovery()

	// Configure failover with short delay
	dr.ConfigureFailover("backend1", FailoverConfig{
		Primary:      "backend1",
		Secondary:    "backend2",
		AutoFailover: true,
		FailoverTime: 100 * time.Millisecond, // Short delay for testing
	})

	// Trigger failure
	dr.ReportFailure("backend1", "Disk failure")

	// Wait for failover to complete
	time.Sleep(200 * time.Millisecond)

	// Check that failover occurred
	active := dr.GetActiveBackend("backend1")
	assert.Equal(t, "backend2", active)
}

func TestDisasterRecovery_RecoveryPlan(t *testing.T) {
	dr := NewDisasterRecovery()

	// Create recovery plan
	plan := dr.CreateRecoveryPlan("backend1", RecoveryScenario{
		FailureType: "COMPLETE_FAILURE",
		DataLoss:    true,
	})

	assert.NotNil(t, plan)
	assert.Greater(t, len(plan.Steps), 0)
	assert.Contains(t, plan.Steps[0].Description, "failover")
}
