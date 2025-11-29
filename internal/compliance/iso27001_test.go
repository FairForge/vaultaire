// internal/compliance/iso27001_test.go
package compliance

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestISO27001Service_NewService(t *testing.T) {
	svc := NewISO27001Service()
	require.NotNil(t, svc)

	controls := svc.ListControls(nil)
	assert.Greater(t, len(controls), 20, "Should have baseline controls")
}

func TestISO27001Service_GetControl(t *testing.T) {
	svc := NewISO27001Service()

	t.Run("existing control", func(t *testing.T) {
		control, err := svc.GetControl("A.5.15")
		require.NoError(t, err)
		assert.Equal(t, "A.5.15", control.ID)
		assert.Equal(t, "Access control", control.Name)
		assert.Contains(t, control.SOC2Mapping, "CC6.1")
	})

	t.Run("non-existent control", func(t *testing.T) {
		_, err := svc.GetControl("A.99.99")
		assert.ErrorIs(t, err, ErrNotFound)
	})
}

func TestISO27001Service_ListControls(t *testing.T) {
	svc := NewISO27001Service()

	t.Run("all controls", func(t *testing.T) {
		controls := svc.ListControls(nil)
		assert.Greater(t, len(controls), 0)
	})

	t.Run("organizational controls only", func(t *testing.T) {
		category := ISOCategoryOrganizational
		controls := svc.ListControls(&category)

		for _, c := range controls {
			assert.Equal(t, ISOCategoryOrganizational, c.Category)
		}
	})

	t.Run("technological controls only", func(t *testing.T) {
		category := ISOCategoryTechnological
		controls := svc.ListControls(&category)

		assert.Greater(t, len(controls), 5)
		for _, c := range controls {
			assert.Equal(t, ISOCategoryTechnological, c.Category)
		}
	})
}

func TestISO27001Service_ListControlsBySOC2(t *testing.T) {
	svc := NewISO27001Service()

	t.Run("find controls mapped to CC6.1", func(t *testing.T) {
		controls := svc.ListControlsBySOC2("CC6.1")
		assert.Greater(t, len(controls), 0, "Should find controls mapped to CC6.1")

		// Verify all returned controls have CC6.1 in mapping
		for _, c := range controls {
			found := false
			for _, m := range c.SOC2Mapping {
				if m == "CC6.1" {
					found = true
					break
				}
			}
			assert.True(t, found, "Control %s should map to CC6.1", c.ID)
		}
	})

	t.Run("no controls for unmapped SOC2", func(t *testing.T) {
		controls := svc.ListControlsBySOC2("INVALID")
		assert.Len(t, controls, 0)
	})
}

func TestISO27001Service_UpdateControlStatus(t *testing.T) {
	svc := NewISO27001Service()
	ctx := context.Background()

	t.Run("update existing control", func(t *testing.T) {
		err := svc.UpdateControlStatus(ctx, "A.5.7", ControlStatusImplemented, "Threat intel feed configured")
		require.NoError(t, err)

		control, _ := svc.GetControl("A.5.7")
		assert.Equal(t, ControlStatusImplemented, control.Status)
		assert.Contains(t, control.Notes, "Threat intel")
		assert.NotNil(t, control.LastAudit)
	})

	t.Run("non-existent control", func(t *testing.T) {
		err := svc.UpdateControlStatus(ctx, "INVALID", ControlStatusImplemented, "")
		assert.ErrorIs(t, err, ErrNotFound)
	})
}

func TestISO27001Service_AddEvidence(t *testing.T) {
	svc := NewISO27001Service()
	ctx := context.Background()

	evidence := ISOEvidence{
		Type:        EvidenceTypeConfiguration,
		Title:       "RBAC Configuration Export",
		Description: "Export of role-based access control settings",
		DocumentRef: "ISMS-EVD-001",
		Location:    "/evidence/rbac-config.json",
	}

	t.Run("add evidence to control", func(t *testing.T) {
		err := svc.AddEvidence(ctx, "A.5.15", evidence)
		require.NoError(t, err)

		control, _ := svc.GetControl("A.5.15")
		require.Len(t, control.Evidence, 1)
		assert.Equal(t, "RBAC Configuration Export", control.Evidence[0].Title)
	})

	t.Run("non-existent control", func(t *testing.T) {
		err := svc.AddEvidence(ctx, "INVALID", evidence)
		assert.ErrorIs(t, err, ErrNotFound)
	})
}

func TestISO27001Service_GenerateSoA(t *testing.T) {
	svc := NewISO27001Service()
	ctx := context.Background()

	soa, err := svc.GenerateSoA(ctx, "Security Officer")
	require.NoError(t, err)

	assert.Equal(t, "Security Officer", soa.ApprovedBy)
	assert.Equal(t, "1.0", soa.Version)
	assert.Greater(t, len(soa.Controls), 20)
	assert.NotNil(t, soa.ApprovedDate)

	// Verify SoA entries
	for _, entry := range soa.Controls {
		assert.NotEmpty(t, entry.ControlID)
		assert.NotEmpty(t, entry.ControlName)
		assert.True(t, entry.Applicable, "All initialized controls should be applicable")
	}
}

func TestISO27001Service_CreateRiskAssessment(t *testing.T) {
	svc := NewISO27001Service()
	ctx := context.Background()

	risk := &ISMSRiskAssessment{
		AssetID:         "ASSET-001",
		AssetName:       "Customer Data Storage",
		ThreatSource:    "External Attacker",
		Vulnerability:   "API endpoint without rate limiting",
		ExistingControl: "Basic authentication",
		Likelihood:      3,
		Impact:          4,
		Treatment:       "Mitigate",
		TreatmentPlan:   "Implement rate limiting via internal/ratelimit",
		Owner:           "Engineering Team",
	}

	err := svc.CreateRiskAssessment(ctx, risk)
	require.NoError(t, err)

	assert.NotEqual(t, "", risk.ID.String())
	assert.Equal(t, 12, risk.RiskScore)
	assert.Equal(t, "High", risk.RiskLevel)
	assert.Equal(t, "Open", risk.Status)
}

func TestISO27001Service_ListRisks(t *testing.T) {
	svc := NewISO27001Service()
	ctx := context.Background()

	// Create some risks
	risk1 := &ISMSRiskAssessment{
		AssetName:  "API Gateway",
		Likelihood: 2,
		Impact:     3,
		Treatment:  "Mitigate",
	}
	risk2 := &ISMSRiskAssessment{
		AssetName:  "Database",
		Likelihood: 1,
		Impact:     5,
		Treatment:  "Accept",
	}

	_ = svc.CreateRiskAssessment(ctx, risk1)
	_ = svc.CreateRiskAssessment(ctx, risk2)

	t.Run("list all risks", func(t *testing.T) {
		risks := svc.ListRisks(nil)
		assert.Len(t, risks, 2)
	})

	t.Run("list by status", func(t *testing.T) {
		status := "Open"
		risks := svc.ListRisks(&status)
		assert.Len(t, risks, 2)

		closedStatus := "Closed"
		closedRisks := svc.ListRisks(&closedStatus)
		assert.Len(t, closedRisks, 0)
	})
}

func TestCalculateRiskLevel(t *testing.T) {
	tests := []struct {
		score    int
		expected string
	}{
		{1, "Low"},
		{5, "Low"},
		{6, "Medium"},
		{11, "Medium"},
		{12, "High"},
		{19, "High"},
		{20, "Critical"},
		{25, "Critical"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := calculateRiskLevel(tt.score)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestISO27001Service_GenerateReport(t *testing.T) {
	svc := NewISO27001Service()
	ctx := context.Background()

	// Generate SoA first
	_, _ = svc.GenerateSoA(ctx, "Approver")

	t.Run("without SoA", func(t *testing.T) {
		report, err := svc.GenerateReport(ctx, false)
		require.NoError(t, err)

		assert.Greater(t, report.TotalControls, 20)
		assert.Greater(t, report.ImplementedCount, 0)
		assert.Greater(t, report.CompliancePercent, 50.0)
		assert.Greater(t, report.SOC2Overlap, 0, "Should have SOC2 mapped controls")
		assert.Nil(t, report.SoA)
	})

	t.Run("with SoA", func(t *testing.T) {
		report, err := svc.GenerateReport(ctx, true)
		require.NoError(t, err)

		assert.NotNil(t, report.SoA)
	})

	t.Run("category breakdown", func(t *testing.T) {
		report, err := svc.GenerateReport(ctx, false)
		require.NoError(t, err)

		assert.Contains(t, report.ByCategory, string(ISOCategoryOrganizational))
		assert.Contains(t, report.ByCategory, string(ISOCategoryTechnological))
	})
}

func TestISO27001Service_ExportToJSON(t *testing.T) {
	svc := NewISO27001Service()
	ctx := context.Background()

	report, _ := svc.GenerateReport(ctx, false)
	jsonData, err := svc.ExportToJSON(report)
	require.NoError(t, err)

	assert.Contains(t, string(jsonData), "total_controls")
	assert.Contains(t, string(jsonData), "compliance_percent")
	assert.Contains(t, string(jsonData), "soc2_overlap_count")
}

func TestISOControlAttributes(t *testing.T) {
	svc := NewISO27001Service()

	// Verify controls have proper attributes
	control, _ := svc.GetControl("A.8.24") // Cryptography control

	assert.Contains(t, control.ControlTypes, AttrPreventive)
	assert.Contains(t, control.SecurityProps, AttrConfidentiality)
	assert.Contains(t, control.CyberConcepts, AttrProtect)
	assert.Contains(t, control.OperationalCaps, AttrInfoProtection)
}
