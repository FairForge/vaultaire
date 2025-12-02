// internal/ha/rto_rpo_test.go
package ha

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRTORPOConfig_Validation(t *testing.T) {
	t.Run("valid config passes validation", func(t *testing.T) {
		config := RTORPOConfig{
			RTO:              time.Minute * 5,
			RPO:              time.Minute * 1,
			Tier:             TierStandard,
			AlertThreshold:   0.8, // Alert at 80% of target
			EscalationPolicy: "ops-team",
		}

		err := config.Validate()
		assert.NoError(t, err)
	})

	t.Run("rejects zero RTO", func(t *testing.T) {
		config := RTORPOConfig{
			RTO: 0,
			RPO: time.Minute,
		}

		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "RTO")
	})

	t.Run("rejects zero RPO", func(t *testing.T) {
		config := RTORPOConfig{
			RTO: time.Minute,
			RPO: 0,
		}

		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "RPO")
	})

	t.Run("rejects RPO greater than RTO", func(t *testing.T) {
		config := RTORPOConfig{
			RTO: time.Minute,
			RPO: time.Minute * 10, // RPO shouldn't exceed RTO typically
		}

		err := config.Validate()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "RPO should not exceed RTO")
	})
}

func TestServiceTier_Defaults(t *testing.T) {
	tests := []struct {
		tier        ServiceTier
		expectedRTO time.Duration
		expectedRPO time.Duration
	}{
		{TierCritical, time.Minute * 1, time.Second * 30},
		{TierStandard, time.Minute * 15, time.Minute * 5},
		{TierBestEffort, time.Hour * 4, time.Hour * 1},
	}

	for _, tt := range tests {
		t.Run(string(tt.tier), func(t *testing.T) {
			defaults := GetTierDefaults(tt.tier)
			assert.Equal(t, tt.expectedRTO, defaults.RTO)
			assert.Equal(t, tt.expectedRPO, defaults.RPO)
		})
	}
}

func TestRTORPOTracker_NewTracker(t *testing.T) {
	t.Run("creates tracker with config", func(t *testing.T) {
		config := RTORPOConfig{
			RTO:            time.Minute * 5,
			RPO:            time.Minute * 1,
			Tier:           TierStandard,
			AlertThreshold: 0.8,
		}

		tracker, err := NewRTORPOTracker(config)
		require.NoError(t, err)
		assert.NotNil(t, tracker)
		assert.Equal(t, TierStandard, tracker.Tier())
	})

	t.Run("rejects invalid config", func(t *testing.T) {
		config := RTORPOConfig{RTO: 0, RPO: 0}

		_, err := NewRTORPOTracker(config)
		assert.Error(t, err)
	})
}

func TestRTORPOTracker_RecordRecovery(t *testing.T) {
	t.Run("records successful recovery within RTO", func(t *testing.T) {
		config := RTORPOConfig{
			RTO:            time.Minute * 5,
			RPO:            time.Minute * 1,
			Tier:           TierStandard,
			AlertThreshold: 0.8,
		}

		tracker, err := NewRTORPOTracker(config)
		require.NoError(t, err)

		// Simulate a recovery that took 2 minutes (within 5 min RTO)
		event := RecoveryEvent{
			IncidentID:   "inc-001",
			BackendName:  "primary",
			FailureTime:  time.Now().Add(-time.Minute * 2),
			RecoveryTime: time.Now(),
			DataLoss:     time.Second * 30,
			Successful:   true,
		}

		result := tracker.RecordRecovery(event)
		assert.True(t, result.RTOMet)
		assert.True(t, result.RPOMet)
		assert.InDelta(t, float64(time.Minute*2), float64(result.ActualRTO), float64(time.Second))
		assert.Equal(t, time.Second*30, result.ActualRPO)
	})

	t.Run("detects RTO breach", func(t *testing.T) {
		config := RTORPOConfig{
			RTO:            time.Minute * 5,
			RPO:            time.Minute * 1,
			Tier:           TierStandard,
			AlertThreshold: 0.8,
		}

		tracker, err := NewRTORPOTracker(config)
		require.NoError(t, err)

		// Simulate a recovery that took 10 minutes (exceeds 5 min RTO)
		event := RecoveryEvent{
			IncidentID:   "inc-002",
			BackendName:  "primary",
			FailureTime:  time.Now().Add(-time.Minute * 10),
			RecoveryTime: time.Now(),
			DataLoss:     time.Second * 30,
			Successful:   true,
		}

		result := tracker.RecordRecovery(event)
		assert.False(t, result.RTOMet)
		assert.True(t, result.RPOMet)
		assert.InDelta(t, float64(time.Minute*10), float64(result.ActualRTO), float64(time.Second))
	})

	t.Run("detects RPO breach", func(t *testing.T) {
		config := RTORPOConfig{
			RTO:            time.Minute * 5,
			RPO:            time.Minute * 1,
			Tier:           TierStandard,
			AlertThreshold: 0.8,
		}

		tracker, err := NewRTORPOTracker(config)
		require.NoError(t, err)

		// Simulate recovery with 3 minutes of data loss (exceeds 1 min RPO)
		event := RecoveryEvent{
			IncidentID:   "inc-003",
			BackendName:  "primary",
			FailureTime:  time.Now().Add(-time.Minute * 2),
			RecoveryTime: time.Now(),
			DataLoss:     time.Minute * 3,
			Successful:   true,
		}

		result := tracker.RecordRecovery(event)
		assert.True(t, result.RTOMet)
		assert.False(t, result.RPOMet)
		assert.Equal(t, time.Minute*3, result.ActualRPO)
	})
}
func TestRTORPOTracker_GetMetrics(t *testing.T) {
	t.Run("calculates metrics from history", func(t *testing.T) {
		config := RTORPOConfig{
			RTO:            time.Minute * 5,
			RPO:            time.Minute * 1,
			Tier:           TierStandard,
			AlertThreshold: 0.8,
		}

		tracker, err := NewRTORPOTracker(config)
		require.NoError(t, err)

		// Record multiple recoveries
		now := time.Now()
		events := []RecoveryEvent{
			{IncidentID: "1", FailureTime: now.Add(-time.Minute * 2), RecoveryTime: now, DataLoss: time.Second * 30, Successful: true},
			{IncidentID: "2", FailureTime: now.Add(-time.Minute * 3), RecoveryTime: now, DataLoss: time.Second * 45, Successful: true},
			{IncidentID: "3", FailureTime: now.Add(-time.Minute * 6), RecoveryTime: now, DataLoss: time.Second * 20, Successful: true}, // RTO breach
		}

		for _, e := range events {
			tracker.RecordRecovery(e)
		}

		metrics := tracker.GetMetrics()
		assert.Equal(t, 3, metrics.TotalIncidents)
		assert.Equal(t, 2, metrics.RTOCompliant)
		assert.Equal(t, 3, metrics.RPOCompliant)
		assert.InDelta(t, 66.67, metrics.RTOComplianceRate, 0.1)
		assert.Equal(t, float64(100), metrics.RPOComplianceRate)
	})

	t.Run("returns zero metrics for no history", func(t *testing.T) {
		config := RTORPOConfig{
			RTO:            time.Minute * 5,
			RPO:            time.Minute * 1,
			Tier:           TierStandard,
			AlertThreshold: 0.8,
		}

		tracker, err := NewRTORPOTracker(config)
		require.NoError(t, err)

		metrics := tracker.GetMetrics()
		assert.Equal(t, 0, metrics.TotalIncidents)
		assert.Equal(t, float64(100), metrics.RTOComplianceRate) // No incidents = 100% compliant
	})
}

func TestRTORPOTracker_CheckStatus(t *testing.T) {
	t.Run("healthy when no active incidents", func(t *testing.T) {
		config := RTORPOConfig{
			RTO:            time.Minute * 5,
			RPO:            time.Minute * 1,
			Tier:           TierStandard,
			AlertThreshold: 0.8,
		}

		tracker, err := NewRTORPOTracker(config)
		require.NoError(t, err)

		status := tracker.CheckStatus(context.Background())
		assert.Equal(t, StatusHealthy, status.Status)
		assert.False(t, status.RTOAtRisk)
		assert.False(t, status.RPOAtRisk)
	})

	t.Run("warns when approaching RTO threshold", func(t *testing.T) {
		config := RTORPOConfig{
			RTO:            time.Minute * 5,
			RPO:            time.Minute * 1,
			Tier:           TierStandard,
			AlertThreshold: 0.8, // Alert at 80% = 4 minutes
		}

		tracker, err := NewRTORPOTracker(config)
		require.NoError(t, err)

		// Start an incident 4.5 minutes ago (past 80% threshold)
		tracker.StartIncident("inc-warning", "primary", time.Now().Add(-time.Minute*4-time.Second*30))

		status := tracker.CheckStatus(context.Background())
		assert.Equal(t, StatusWarning, status.Status)
		assert.True(t, status.RTOAtRisk)
	})

	t.Run("critical when RTO breached", func(t *testing.T) {
		config := RTORPOConfig{
			RTO:            time.Minute * 5,
			RPO:            time.Minute * 1,
			Tier:           TierStandard,
			AlertThreshold: 0.8,
		}

		tracker, err := NewRTORPOTracker(config)
		require.NoError(t, err)

		// Start an incident 6 minutes ago (past RTO)
		tracker.StartIncident("inc-breach", "primary", time.Now().Add(-time.Minute*6))

		status := tracker.CheckStatus(context.Background())
		assert.Equal(t, StatusCritical, status.Status)
		assert.True(t, status.RTOAtRisk)
		assert.True(t, status.RTOBreached)
	})
}

func TestRTORPOTracker_IncidentLifecycle(t *testing.T) {
	t.Run("tracks incident from start to resolution", func(t *testing.T) {
		config := RTORPOConfig{
			RTO:            time.Minute * 5,
			RPO:            time.Minute * 1,
			Tier:           TierStandard,
			AlertThreshold: 0.8,
		}

		tracker, err := NewRTORPOTracker(config)
		require.NoError(t, err)

		// Start incident
		incidentID := "inc-lifecycle"
		failureTime := time.Now().Add(-time.Minute * 2)
		tracker.StartIncident(incidentID, "primary", failureTime)

		// Check that incident is active
		assert.True(t, tracker.HasActiveIncident(incidentID))

		// Resolve incident
		result, err := tracker.ResolveIncident(incidentID, time.Second*30)
		require.NoError(t, err)
		assert.True(t, result.RTOMet)
		assert.True(t, result.RPOMet)

		// Check that incident is no longer active
		assert.False(t, tracker.HasActiveIncident(incidentID))
	})

	t.Run("returns error for unknown incident", func(t *testing.T) {
		config := RTORPOConfig{
			RTO:            time.Minute * 5,
			RPO:            time.Minute * 1,
			Tier:           TierStandard,
			AlertThreshold: 0.8,
		}

		tracker, err := NewRTORPOTracker(config)
		require.NoError(t, err)

		_, err = tracker.ResolveIncident("nonexistent", time.Second*30)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestRTORPOTracker_PerBackendTargets(t *testing.T) {
	t.Run("supports different targets per backend", func(t *testing.T) {
		tracker, err := NewRTORPOTrackerWithBackends(map[string]RTORPOConfig{
			"critical-db": {
				RTO:            time.Minute * 1,
				RPO:            time.Second * 30,
				Tier:           TierCritical,
				AlertThreshold: 0.8,
			},
			"archive-storage": {
				RTO:            time.Hour * 4,
				RPO:            time.Hour * 1,
				Tier:           TierBestEffort,
				AlertThreshold: 0.9,
			},
		})
		require.NoError(t, err)

		// Critical backend should have stricter targets
		criticalConfig := tracker.GetBackendConfig("critical-db")
		assert.Equal(t, time.Minute*1, criticalConfig.RTO)

		archiveConfig := tracker.GetBackendConfig("archive-storage")
		assert.Equal(t, time.Hour*4, archiveConfig.RTO)
	})
}

func TestGenerateSLAReport(t *testing.T) {
	t.Run("generates monthly SLA report", func(t *testing.T) {
		config := RTORPOConfig{
			RTO:            time.Minute * 5,
			RPO:            time.Minute * 1,
			Tier:           TierStandard,
			AlertThreshold: 0.8,
		}

		tracker, err := NewRTORPOTracker(config)
		require.NoError(t, err)

		// Record some events
		now := time.Now()
		events := []RecoveryEvent{
			{IncidentID: "1", FailureTime: now.Add(-time.Minute * 2), RecoveryTime: now, DataLoss: time.Second * 30, Successful: true},
			{IncidentID: "2", FailureTime: now.Add(-time.Minute * 3), RecoveryTime: now, DataLoss: time.Second * 45, Successful: true},
		}

		for _, e := range events {
			tracker.RecordRecovery(e)
		}

		report := tracker.GenerateSLAReport(now.Add(-time.Hour*24*30), now)
		assert.NotEmpty(t, report.GeneratedAt)
		assert.Equal(t, TierStandard, report.Tier)
		assert.Equal(t, 2, report.TotalIncidents)
		assert.InDelta(t, 100.0, report.RTOCompliancePercent, 0.1)
		assert.InDelta(t, 100.0, report.RPOCompliancePercent, 0.1)
	})
}
