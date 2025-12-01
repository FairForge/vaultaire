// internal/compliance/dashboard_test.go
package compliance

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComplianceDashboard_NewDashboard(t *testing.T) {
	soc2 := NewSOC2Service()
	iso := NewISO27001Service()

	// GDPR service requires DB, so we pass nil - dashboard handles this gracefully
	dashboard := NewComplianceDashboard(nil, soc2, iso)
	require.NotNil(t, dashboard)
}

func TestComplianceDashboard_GetSummary(t *testing.T) {
	soc2 := NewSOC2Service()
	iso := NewISO27001Service()
	dashboard := NewComplianceDashboard(nil, soc2, iso)

	ctx := context.Background()
	summary, err := dashboard.GetSummary(ctx)
	require.NoError(t, err)

	t.Run("has SOC2 and ISO frameworks", func(t *testing.T) {
		assert.Contains(t, summary.Frameworks, FrameworkSOC2)
		assert.Contains(t, summary.Frameworks, FrameworkISO27001)
	})

	t.Run("GDPR excluded when service is nil", func(t *testing.T) {
		_, hasGDPR := summary.Frameworks[FrameworkGDPR]
		assert.False(t, hasGDPR, "GDPR should not be present when service is nil")
	})

	t.Run("calculates overall score", func(t *testing.T) {
		assert.Greater(t, summary.OverallScore, 0.0)
		assert.LessOrEqual(t, summary.OverallScore, 100.0)
	})

	t.Run("has overall status", func(t *testing.T) {
		assert.Contains(t, []string{"Compliant", "Partial", "Non-Compliant"}, summary.OverallStatus)
	})

	t.Run("has upcoming deadlines", func(t *testing.T) {
		assert.Greater(t, len(summary.UpcomingDeadlines), 0)
		// Deadlines should be sorted by date
		for i := 1; i < len(summary.UpcomingDeadlines); i++ {
			assert.True(t, summary.UpcomingDeadlines[i-1].DueDate.Before(summary.UpcomingDeadlines[i].DueDate) ||
				summary.UpcomingDeadlines[i-1].DueDate.Equal(summary.UpcomingDeadlines[i].DueDate))
		}
	})

	t.Run("has recent activity", func(t *testing.T) {
		assert.Greater(t, len(summary.RecentActivity), 0)
	})

	t.Run("tracks gaps and risks", func(t *testing.T) {
		assert.GreaterOrEqual(t, summary.OpenGaps, 0)
		assert.GreaterOrEqual(t, summary.OpenRisks, 0)
	})
}

func TestComplianceDashboard_FrameworkSummaries(t *testing.T) {
	soc2 := NewSOC2Service()
	iso := NewISO27001Service()
	dashboard := NewComplianceDashboard(nil, soc2, iso)

	ctx := context.Background()
	summary, err := dashboard.GetSummary(ctx)
	require.NoError(t, err)

	t.Run("SOC2 summary is accurate", func(t *testing.T) {
		soc2Summary := summary.Frameworks[FrameworkSOC2]
		assert.Equal(t, FrameworkSOC2, soc2Summary.Framework)
		assert.Greater(t, soc2Summary.TotalControls, 20)
		assert.Greater(t, soc2Summary.Implemented, 0)
		assert.Greater(t, soc2Summary.ComplianceScore, 50.0)
	})

	t.Run("ISO 27001 summary is accurate", func(t *testing.T) {
		isoSummary := summary.Frameworks[FrameworkISO27001]
		assert.Equal(t, FrameworkISO27001, isoSummary.Framework)
		assert.Greater(t, isoSummary.TotalControls, 20)
		assert.Greater(t, isoSummary.Implemented, 0)
	})
}

func TestComplianceDashboard_GetUnifiedControls(t *testing.T) {
	soc2 := NewSOC2Service()
	iso := NewISO27001Service()
	dashboard := NewComplianceDashboard(nil, soc2, iso)

	ctx := context.Background()
	controls, err := dashboard.GetUnifiedControls(ctx)
	require.NoError(t, err)

	t.Run("has controls from both frameworks", func(t *testing.T) {
		assert.Greater(t, len(controls), 40) // Combined controls
	})

	t.Run("controls have framework mappings", func(t *testing.T) {
		for _, c := range controls {
			assert.Greater(t, len(c.Frameworks), 0)
			assert.NotEmpty(t, c.ID)
			assert.NotEmpty(t, c.Name)
		}
	})

	t.Run("ISO controls have SOC2 cross-mappings", func(t *testing.T) {
		hasCrossMapping := false
		for _, c := range controls {
			if len(c.Frameworks) > 1 {
				hasCrossMapping = true
				break
			}
		}
		assert.True(t, hasCrossMapping, "Should have cross-framework mappings")
	})
}

func TestComplianceDashboard_GetGapAnalysis(t *testing.T) {
	soc2 := NewSOC2Service()
	iso := NewISO27001Service()
	dashboard := NewComplianceDashboard(nil, soc2, iso)

	ctx := context.Background()
	analysis, err := dashboard.GetGapAnalysis(ctx)
	require.NoError(t, err)

	t.Run("identifies gaps", func(t *testing.T) {
		assert.GreaterOrEqual(t, analysis.TotalGaps, 0)
	})

	t.Run("categorizes gaps by priority", func(t *testing.T) {
		totalCategorized := analysis.CriticalGaps + analysis.HighGaps + analysis.MediumGaps + analysis.LowGaps
		assert.Equal(t, analysis.TotalGaps, totalCategorized)
	})

	t.Run("gaps organized by framework", func(t *testing.T) {
		if analysis.TotalGaps > 0 {
			totalInFrameworks := 0
			for _, gaps := range analysis.GapsByFramework {
				totalInFrameworks += len(gaps)
			}
			assert.Equal(t, analysis.TotalGaps, totalInFrameworks)
		}
	})

	t.Run("provides recommendations", func(t *testing.T) {
		assert.Greater(t, len(analysis.Recommendations), 0)
		for _, rec := range analysis.Recommendations {
			assert.NotEmpty(t, rec.Title)
			assert.NotEmpty(t, rec.Description)
			assert.Contains(t, []string{"Critical", "High", "Medium", "Low"}, rec.Priority)
		}
	})
}

func TestComplianceDashboard_ExportJSON(t *testing.T) {
	soc2 := NewSOC2Service()
	iso := NewISO27001Service()
	dashboard := NewComplianceDashboard(nil, soc2, iso)

	ctx := context.Background()

	t.Run("export summary JSON", func(t *testing.T) {
		data, err := dashboard.ExportSummaryJSON(ctx)
		require.NoError(t, err)

		assert.Contains(t, string(data), "overall_score")
		assert.Contains(t, string(data), "frameworks")
		assert.Contains(t, string(data), "SOC2")
		assert.Contains(t, string(data), "ISO27001")
	})

	t.Run("export gap analysis JSON", func(t *testing.T) {
		data, err := dashboard.ExportGapAnalysisJSON(ctx)
		require.NoError(t, err)

		assert.Contains(t, string(data), "total_gaps")
		assert.Contains(t, string(data), "recommendations")
	})
}

func TestScoreToStatus(t *testing.T) {
	dashboard := NewComplianceDashboard(nil, nil, nil)

	tests := []struct {
		score    float64
		expected string
	}{
		{100, "Compliant"},
		{95, "Compliant"},
		{90, "Compliant"},
		{89, "Partial"},
		{75, "Partial"},
		{70, "Partial"},
		{69, "Non-Compliant"},
		{50, "Non-Compliant"},
		{0, "Non-Compliant"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := dashboard.scoreToStatus(tt.score)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestComplianceDeadlines(t *testing.T) {
	dashboard := NewComplianceDashboard(nil, NewSOC2Service(), NewISO27001Service())

	ctx := context.Background()
	summary, err := dashboard.GetSummary(ctx)
	require.NoError(t, err)

	t.Run("deadlines have required fields", func(t *testing.T) {
		for _, deadline := range summary.UpcomingDeadlines {
			assert.NotEmpty(t, deadline.ID)
			assert.NotEmpty(t, deadline.Framework)
			assert.NotEmpty(t, deadline.Type)
			assert.NotEmpty(t, deadline.Description)
			assert.False(t, deadline.DueDate.IsZero())
			assert.GreaterOrEqual(t, deadline.DaysUntil, 0)
		}
	})

	t.Run("has different framework deadlines", func(t *testing.T) {
		frameworks := make(map[ComplianceFramework]bool)
		for _, d := range summary.UpcomingDeadlines {
			frameworks[d.Framework] = true
		}
		assert.Greater(t, len(frameworks), 1, "Should have deadlines from multiple frameworks")
	})
}
