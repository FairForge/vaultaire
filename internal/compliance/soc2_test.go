// internal/compliance/soc2_test.go
package compliance

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSOC2Service_NewService(t *testing.T) {
	svc := NewSOC2Service()
	require.NotNil(t, svc)

	// Verify baseline controls are loaded
	controls := svc.ListControls(nil)
	assert.Greater(t, len(controls), 20, "Should have baseline controls loaded")
}

func TestSOC2Service_GetControl(t *testing.T) {
	svc := NewSOC2Service()

	t.Run("existing control", func(t *testing.T) {
		control, err := svc.GetControl("CC6.1")
		require.NoError(t, err)
		assert.Equal(t, "CC6.1", control.ID)
		assert.Equal(t, TSCSecurity, control.Category)
		assert.Equal(t, "Access Control Implementation", control.Name)
	})

	t.Run("non-existent control", func(t *testing.T) {
		_, err := svc.GetControl("INVALID")
		assert.ErrorIs(t, err, ErrNotFound)
	})
}

func TestSOC2Service_ListControls(t *testing.T) {
	svc := NewSOC2Service()

	t.Run("all controls", func(t *testing.T) {
		controls := svc.ListControls(nil)
		assert.Greater(t, len(controls), 0)
	})

	t.Run("security controls only", func(t *testing.T) {
		category := TSCSecurity
		controls := svc.ListControls(&category)

		for _, c := range controls {
			assert.Equal(t, TSCSecurity, c.Category)
		}
	})

	t.Run("privacy controls only", func(t *testing.T) {
		category := TSCPrivacy
		controls := svc.ListControls(&category)

		for _, c := range controls {
			assert.Equal(t, TSCPrivacy, c.Category)
		}
	})
}

func TestSOC2Service_UpdateControlStatus(t *testing.T) {
	svc := NewSOC2Service()
	ctx := context.Background()

	t.Run("update existing control", func(t *testing.T) {
		err := svc.UpdateControlStatus(ctx, "CC3.2", ControlStatusImplemented, "Completed fraud assessment")
		require.NoError(t, err)

		control, _ := svc.GetControl("CC3.2")
		assert.Equal(t, ControlStatusImplemented, control.Status)
		assert.Contains(t, control.Notes, "fraud assessment")
		assert.NotNil(t, control.LastReviewed)
	})

	t.Run("non-existent control", func(t *testing.T) {
		err := svc.UpdateControlStatus(ctx, "INVALID", ControlStatusImplemented, "")
		assert.ErrorIs(t, err, ErrNotFound)
	})
}

func TestSOC2Service_AddEvidence(t *testing.T) {
	svc := NewSOC2Service()
	ctx := context.Background()

	evidence := ControlEvidence{
		Type:        EvidenceTypeLog,
		Title:       "Access Control Audit Log",
		Description: "30-day audit log export showing access patterns",
		Location:    "/evidence/2024-11/access-audit.json",
		CollectedBy: "security-team",
	}

	t.Run("add evidence to control", func(t *testing.T) {
		err := svc.AddEvidence(ctx, "CC6.1", evidence)
		require.NoError(t, err)

		control, _ := svc.GetControl("CC6.1")
		require.Len(t, control.Evidence, 1)
		assert.Equal(t, "Access Control Audit Log", control.Evidence[0].Title)
		assert.NotEqual(t, "", control.Evidence[0].ID.String())
	})

	t.Run("non-existent control", func(t *testing.T) {
		err := svc.AddEvidence(ctx, "INVALID", evidence)
		assert.ErrorIs(t, err, ErrNotFound)
	})
}

func TestSOC2Service_RecordTest(t *testing.T) {
	svc := NewSOC2Service()
	ctx := context.Background()

	test := ControlTest{
		TestName:   "Authentication Verification",
		TestMethod: "Automated",
		TestedBy:   "security-scanner",
		Passed:     true,
		Findings:   "All authentication endpoints require valid credentials",
	}

	t.Run("record passing test", func(t *testing.T) {
		err := svc.RecordTest(ctx, "CC6.1", test)
		require.NoError(t, err)

		control, _ := svc.GetControl("CC6.1")
		require.Len(t, control.TestResults, 1)
		assert.True(t, control.TestResults[0].Passed)
		assert.Equal(t, ControlStatusTested, control.Status)
	})

	t.Run("record failing test", func(t *testing.T) {
		failingTest := ControlTest{
			TestName:    "Vendor Assessment Check",
			TestMethod:  "Manual",
			TestedBy:    "security-team",
			Passed:      false,
			Findings:    "Missing vendor security questionnaire",
			Remediation: "Request VSQ from Geyser Data",
		}

		err := svc.RecordTest(ctx, "CC9.2", failingTest)
		require.NoError(t, err)

		control, _ := svc.GetControl("CC9.2")
		require.Greater(t, len(control.TestResults), 0)
		lastTest := control.TestResults[len(control.TestResults)-1]
		assert.False(t, lastTest.Passed)
	})
}

func TestSOC2Service_GenerateAssessment(t *testing.T) {
	svc := NewSOC2Service()
	ctx := context.Background()

	t.Run("Type 1 assessment - Security only", func(t *testing.T) {
		assessment, err := svc.GenerateAssessment(ctx, "Type1", []TrustServiceCategory{TSCSecurity})
		require.NoError(t, err)

		assert.Equal(t, "Type1", assessment.Type)
		assert.Greater(t, len(assessment.Controls), 10)
		assert.Greater(t, assessment.CompletionPct, 50.0)

		// All controls should be Security category
		for _, c := range assessment.Controls {
			assert.Equal(t, TSCSecurity, c.Category)
		}
	})

	t.Run("Type 2 assessment - Multiple categories", func(t *testing.T) {
		categories := []TrustServiceCategory{TSCSecurity, TSCPrivacy, TSCConfidentiality}
		assessment, err := svc.GenerateAssessment(ctx, "Type2", categories)
		require.NoError(t, err)

		assert.Equal(t, "Type2", assessment.Type)
		assert.Len(t, assessment.Categories, 3)

		// Should have controls from multiple categories
		categoryFound := make(map[TrustServiceCategory]bool)
		for _, c := range assessment.Controls {
			categoryFound[c.Category] = true
		}
		assert.True(t, categoryFound[TSCSecurity])
	})

	t.Run("assessment identifies gaps", func(t *testing.T) {
		assessment, err := svc.GenerateAssessment(ctx, "Type1", []TrustServiceCategory{TSCSecurity})
		require.NoError(t, err)

		// Should identify partial/not implemented controls as gaps
		hasGaps := len(assessment.Gaps) > 0
		assert.True(t, hasGaps, "Should identify compliance gaps")
	})
}

func TestSOC2Service_RunAutomatedChecks(t *testing.T) {
	svc := NewSOC2Service()
	ctx := context.Background()

	checks, err := svc.RunAutomatedChecks(ctx)
	require.NoError(t, err)

	assert.Greater(t, len(checks), 3, "Should have multiple automated checks")

	// Verify check structure
	for _, check := range checks {
		assert.NotEmpty(t, check.ControlID)
		assert.NotEmpty(t, check.CheckName)
		assert.False(t, check.CheckedAt.IsZero())
	}
}

func TestSOC2Service_GenerateReportData(t *testing.T) {
	svc := NewSOC2Service()
	ctx := context.Background()

	data, err := svc.GenerateReportData(ctx, "Type1")
	require.NoError(t, err)

	assert.NotNil(t, data.Assessment)
	assert.Greater(t, len(data.AutomatedChecks), 0)
	assert.False(t, data.GeneratedAt.IsZero())
}

func TestSOC2Service_ExportToJSON(t *testing.T) {
	svc := NewSOC2Service()
	ctx := context.Background()

	data, err := svc.GenerateReportData(ctx, "Type1")
	require.NoError(t, err)

	jsonData, err := svc.ExportToJSON(data)
	require.NoError(t, err)

	assert.Contains(t, string(jsonData), "assessment")
	assert.Contains(t, string(jsonData), "automated_checks")
	assert.Contains(t, string(jsonData), "CC6.1")
}

func TestHashEvidence(t *testing.T) {
	content := []byte("test evidence content")
	hash := HashEvidence(content)

	assert.Len(t, hash, 64) // SHA256 hex = 64 chars

	// Same content = same hash
	hash2 := HashEvidence(content)
	assert.Equal(t, hash, hash2)

	// Different content = different hash
	hash3 := HashEvidence([]byte("different content"))
	assert.NotEqual(t, hash, hash3)
}

func TestControlStatus_Progression(t *testing.T) {
	// Verify status progression logic
	statuses := []ControlStatus{
		ControlStatusNotImplemented,
		ControlStatusPartial,
		ControlStatusImplemented,
		ControlStatusTested,
		ControlStatusEffective,
	}

	// Just verify all statuses are distinct
	seen := make(map[ControlStatus]bool)
	for _, s := range statuses {
		assert.False(t, seen[s], "Status %s should be unique", s)
		seen[s] = true
	}
}
