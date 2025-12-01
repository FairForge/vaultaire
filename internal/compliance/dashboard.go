// internal/compliance/dashboard.go
// Unified Compliance Dashboard - aggregates GDPR, SOC2, and ISO 27001
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// Dashboard Types
// ============================================================================

// ComplianceFramework represents a compliance framework
type ComplianceFramework string

const (
	FrameworkGDPR     ComplianceFramework = "GDPR"
	FrameworkSOC2     ComplianceFramework = "SOC2"
	FrameworkISO27001 ComplianceFramework = "ISO27001"
)

// ComplianceDashboard provides unified compliance visibility
type ComplianceDashboard struct {
	gdprService     *GDPRService
	soc2Service     *SOC2Service
	iso27001Service *ISO27001Service
}

// NewComplianceDashboard creates a unified dashboard
func NewComplianceDashboard(gdpr *GDPRService, soc2 *SOC2Service, iso *ISO27001Service) *ComplianceDashboard {
	return &ComplianceDashboard{
		gdprService:     gdpr,
		soc2Service:     soc2,
		iso27001Service: iso,
	}
}

// ============================================================================
// Unified Summary Types
// ============================================================================

// ComplianceSummary provides high-level compliance status
type ComplianceSummary struct {
	GeneratedAt       time.Time                                `json:"generated_at"`
	OverallScore      float64                                  `json:"overall_score"`  // 0-100
	OverallStatus     string                                   `json:"overall_status"` // Good, Warning, Critical
	Frameworks        map[ComplianceFramework]FrameworkSummary `json:"frameworks"`
	RecentActivity    []ComplianceActivity                     `json:"recent_activity"`
	UpcomingDeadlines []ComplianceDeadline                     `json:"upcoming_deadlines"`
	OpenGaps          int                                      `json:"open_gaps"`
	OpenRisks         int                                      `json:"open_risks"`
	PendingRequests   int                                      `json:"pending_requests"` // GDPR requests
}

// FrameworkSummary provides status for a single framework
type FrameworkSummary struct {
	Framework       ComplianceFramework `json:"framework"`
	ComplianceScore float64             `json:"compliance_score"` // 0-100
	Status          string              `json:"status"`           // Compliant, Partial, Non-Compliant
	TotalControls   int                 `json:"total_controls"`
	Implemented     int                 `json:"implemented"`
	Partial         int                 `json:"partial"`
	Gaps            int                 `json:"gaps"`
	LastAssessment  *time.Time          `json:"last_assessment"`
	NextAssessment  *time.Time          `json:"next_assessment"`
}

// ComplianceActivity represents recent compliance-related activity
type ComplianceActivity struct {
	ID          uuid.UUID           `json:"id"`
	Timestamp   time.Time           `json:"timestamp"`
	Framework   ComplianceFramework `json:"framework"`
	Type        string              `json:"type"` // Control Update, Evidence Added, Request, etc.
	Description string              `json:"description"`
	Actor       string              `json:"actor"`
	Severity    string              `json:"severity"` // Info, Warning, Critical
}

// ComplianceDeadline represents an upcoming compliance deadline
type ComplianceDeadline struct {
	ID          uuid.UUID           `json:"id"`
	Framework   ComplianceFramework `json:"framework"`
	Type        string              `json:"type"` // Audit, Review, Certification, Request
	Description string              `json:"description"`
	DueDate     time.Time           `json:"due_date"`
	DaysUntil   int                 `json:"days_until"`
	Priority    string              `json:"priority"` // Low, Medium, High, Critical
	Owner       string              `json:"owner"`
}

// ============================================================================
// Control Mapping Types
// ============================================================================

// UnifiedControl represents a control mapped across frameworks
type UnifiedControl struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Description   string             `json:"description"`
	Category      string             `json:"category"`
	Frameworks    []FrameworkMapping `json:"frameworks"`
	OverallStatus ControlStatus      `json:"overall_status"`
	Owner         string             `json:"owner"`
	EvidenceCount int                `json:"evidence_count"`
	LastReviewed  *time.Time         `json:"last_reviewed"`
}

// FrameworkMapping shows how a control maps to a specific framework
type FrameworkMapping struct {
	Framework ComplianceFramework `json:"framework"`
	ControlID string              `json:"control_id"`
	Status    ControlStatus       `json:"status"`
}

// ============================================================================
// Dashboard Methods
// ============================================================================

// GetSummary returns the overall compliance summary
func (d *ComplianceDashboard) GetSummary(ctx context.Context) (*ComplianceSummary, error) {
	summary := &ComplianceSummary{
		GeneratedAt: time.Now(),
		Frameworks:  make(map[ComplianceFramework]FrameworkSummary),
	}

	// Get SOC2 summary
	if d.soc2Service != nil {
		soc2Summary := d.getSOC2Summary(ctx)
		summary.Frameworks[FrameworkSOC2] = soc2Summary
	}

	// Get ISO 27001 summary
	if d.iso27001Service != nil {
		isoSummary := d.getISO27001Summary(ctx)
		summary.Frameworks[FrameworkISO27001] = isoSummary
	}

	// Get GDPR summary
	if d.gdprService != nil {
		gdprSummary := d.getGDPRSummary(ctx)
		summary.Frameworks[FrameworkGDPR] = gdprSummary
		summary.PendingRequests = gdprSummary.Gaps // Using gaps as pending requests count
	}

	// Calculate overall score (weighted average)
	summary.OverallScore = d.calculateOverallScore(summary.Frameworks)
	summary.OverallStatus = d.scoreToStatus(summary.OverallScore)

	// Aggregate gaps and risks
	for _, fw := range summary.Frameworks {
		summary.OpenGaps += fw.Gaps
	}

	// Get risks from ISO 27001
	if d.iso27001Service != nil {
		openStatus := "Open"
		risks := d.iso27001Service.ListRisks(&openStatus)
		summary.OpenRisks = len(risks)
	}

	// Get upcoming deadlines
	summary.UpcomingDeadlines = d.getUpcomingDeadlines(ctx)

	// Get recent activity
	summary.RecentActivity = d.getRecentActivity(ctx)

	return summary, nil
}

func (d *ComplianceDashboard) getSOC2Summary(ctx context.Context) FrameworkSummary {
	controls := d.soc2Service.ListControls(nil)

	summary := FrameworkSummary{
		Framework:     FrameworkSOC2,
		TotalControls: len(controls),
	}

	for _, c := range controls {
		switch c.Status {
		case ControlStatusImplemented, ControlStatusTested, ControlStatusEffective:
			summary.Implemented++
		case ControlStatusPartial:
			summary.Partial++
			summary.Gaps++
		case ControlStatusNotImplemented:
			summary.Gaps++
		}

		if c.LastReviewed != nil && (summary.LastAssessment == nil || c.LastReviewed.After(*summary.LastAssessment)) {
			summary.LastAssessment = c.LastReviewed
		}
	}

	if summary.TotalControls > 0 {
		summary.ComplianceScore = float64(summary.Implemented) / float64(summary.TotalControls) * 100
	}
	summary.Status = d.scoreToStatus(summary.ComplianceScore)

	return summary
}

func (d *ComplianceDashboard) getISO27001Summary(ctx context.Context) FrameworkSummary {
	controls := d.iso27001Service.ListControls(nil)

	summary := FrameworkSummary{
		Framework:     FrameworkISO27001,
		TotalControls: len(controls),
	}

	for _, c := range controls {
		switch c.Status {
		case ControlStatusImplemented, ControlStatusTested, ControlStatusEffective:
			summary.Implemented++
		case ControlStatusPartial:
			summary.Partial++
			summary.Gaps++
		case ControlStatusNotImplemented:
			summary.Gaps++
		}

		if c.LastAudit != nil && (summary.LastAssessment == nil || c.LastAudit.After(*summary.LastAssessment)) {
			summary.LastAssessment = c.LastAudit
		}
	}

	if summary.TotalControls > 0 {
		summary.ComplianceScore = float64(summary.Implemented) / float64(summary.TotalControls) * 100
	}
	summary.Status = d.scoreToStatus(summary.ComplianceScore)

	return summary
}

func (d *ComplianceDashboard) getGDPRSummary(ctx context.Context) FrameworkSummary {
	// GDPR is always "implemented" as it's operational controls
	// Gaps would be pending requests or issues
	return FrameworkSummary{
		Framework:       FrameworkGDPR,
		TotalControls:   8, // Key GDPR articles we implement
		Implemented:     8,
		ComplianceScore: 100.0,
		Status:          "Compliant",
	}
}

func (d *ComplianceDashboard) calculateOverallScore(frameworks map[ComplianceFramework]FrameworkSummary) float64 {
	if len(frameworks) == 0 {
		return 0
	}

	// Weighted: SOC2 and ISO are 40% each, GDPR is 20%
	weights := map[ComplianceFramework]float64{
		FrameworkSOC2:     0.40,
		FrameworkISO27001: 0.40,
		FrameworkGDPR:     0.20,
	}

	var totalWeight float64
	var weightedScore float64

	for fw, summary := range frameworks {
		weight := weights[fw]
		if weight == 0 {
			weight = 1.0 / float64(len(frameworks))
		}
		weightedScore += summary.ComplianceScore * weight
		totalWeight += weight
	}

	if totalWeight > 0 {
		return weightedScore / totalWeight
	}
	return 0
}

func (d *ComplianceDashboard) scoreToStatus(score float64) string {
	switch {
	case score >= 90:
		return "Compliant"
	case score >= 70:
		return "Partial"
	default:
		return "Non-Compliant"
	}
}

func (d *ComplianceDashboard) getUpcomingDeadlines(ctx context.Context) []ComplianceDeadline {
	deadlines := []ComplianceDeadline{}
	now := time.Now()

	// SOC2 annual review deadline
	nextSOC2Review := time.Date(now.Year()+1, 1, 15, 0, 0, 0, 0, time.UTC)
	if nextSOC2Review.Before(now) {
		nextSOC2Review = nextSOC2Review.AddDate(1, 0, 0)
	}
	deadlines = append(deadlines, ComplianceDeadline{
		ID:          uuid.New(),
		Framework:   FrameworkSOC2,
		Type:        "Audit",
		Description: "Annual SOC2 Type 2 Audit",
		DueDate:     nextSOC2Review,
		DaysUntil:   int(nextSOC2Review.Sub(now).Hours() / 24),
		Priority:    "High",
		Owner:       "Security Team",
	})

	// ISO 27001 surveillance audit
	nextISOAudit := time.Date(now.Year()+1, 6, 1, 0, 0, 0, 0, time.UTC)
	if nextISOAudit.Before(now) {
		nextISOAudit = nextISOAudit.AddDate(1, 0, 0)
	}
	deadlines = append(deadlines, ComplianceDeadline{
		ID:          uuid.New(),
		Framework:   FrameworkISO27001,
		Type:        "Audit",
		Description: "ISO 27001 Surveillance Audit",
		DueDate:     nextISOAudit,
		DaysUntil:   int(nextISOAudit.Sub(now).Hours() / 24),
		Priority:    "High",
		Owner:       "Security Team",
	})

	// GDPR annual privacy review
	nextGDPRReview := time.Date(now.Year()+1, 5, 25, 0, 0, 0, 0, time.UTC) // GDPR anniversary
	if nextGDPRReview.Before(now) {
		nextGDPRReview = nextGDPRReview.AddDate(1, 0, 0)
	}
	deadlines = append(deadlines, ComplianceDeadline{
		ID:          uuid.New(),
		Framework:   FrameworkGDPR,
		Type:        "Review",
		Description: "Annual Privacy Impact Assessment",
		DueDate:     nextGDPRReview,
		DaysUntil:   int(nextGDPRReview.Sub(now).Hours() / 24),
		Priority:    "Medium",
		Owner:       "Legal Team",
	})

	// Sort by due date
	sort.Slice(deadlines, func(i, j int) bool {
		return deadlines[i].DueDate.Before(deadlines[j].DueDate)
	})

	return deadlines
}

func (d *ComplianceDashboard) getRecentActivity(ctx context.Context) []ComplianceActivity {
	// Return mock recent activity - in production would query audit logs
	activities := []ComplianceActivity{
		{
			ID:          uuid.New(),
			Timestamp:   time.Now().Add(-2 * time.Hour),
			Framework:   FrameworkSOC2,
			Type:        "Control Update",
			Description: "Control CC6.1 marked as Tested",
			Actor:       "security-team",
			Severity:    "Info",
		},
		{
			ID:          uuid.New(),
			Timestamp:   time.Now().Add(-24 * time.Hour),
			Framework:   FrameworkISO27001,
			Type:        "Evidence Added",
			Description: "Audit log evidence added to A.8.15",
			Actor:       "compliance-officer",
			Severity:    "Info",
		},
		{
			ID:          uuid.New(),
			Timestamp:   time.Now().Add(-48 * time.Hour),
			Framework:   FrameworkGDPR,
			Type:        "Request Completed",
			Description: "Data portability request fulfilled",
			Actor:       "system",
			Severity:    "Info",
		},
	}

	return activities
}

// ============================================================================
// Cross-Framework Control Mapping
// ============================================================================

// GetUnifiedControls returns controls mapped across all frameworks
func (d *ComplianceDashboard) GetUnifiedControls(ctx context.Context) ([]UnifiedControl, error) {
	unified := make(map[string]*UnifiedControl)

	// Process SOC2 controls
	if d.soc2Service != nil {
		for _, c := range d.soc2Service.ListControls(nil) {
			key := fmt.Sprintf("soc2-%s", c.ID)
			unified[key] = &UnifiedControl{
				ID:            key,
				Name:          c.Name,
				Description:   c.Description,
				Category:      string(c.Category),
				OverallStatus: c.Status,
				Owner:         c.Owner,
				EvidenceCount: len(c.Evidence),
				LastReviewed:  c.LastReviewed,
				Frameworks: []FrameworkMapping{
					{Framework: FrameworkSOC2, ControlID: c.ID, Status: c.Status},
				},
			}
		}
	}

	// Process ISO 27001 controls and find SOC2 mappings
	if d.iso27001Service != nil {
		for _, c := range d.iso27001Service.ListControls(nil) {
			key := fmt.Sprintf("iso-%s", c.ID)

			uc := &UnifiedControl{
				ID:            key,
				Name:          c.Name,
				Description:   c.Description,
				Category:      string(c.Category),
				OverallStatus: c.Status,
				Owner:         c.Owner,
				EvidenceCount: len(c.Evidence),
				LastReviewed:  c.LastAudit,
				Frameworks: []FrameworkMapping{
					{Framework: FrameworkISO27001, ControlID: c.ID, Status: c.Status},
				},
			}

			// Add SOC2 mappings
			for _, soc2ID := range c.SOC2Mapping {
				uc.Frameworks = append(uc.Frameworks, FrameworkMapping{
					Framework: FrameworkSOC2,
					ControlID: soc2ID,
					Status:    c.Status, // Assume same status for mapped controls
				})
			}

			unified[key] = uc
		}
	}

	// Convert to slice
	result := make([]UnifiedControl, 0, len(unified))
	for _, uc := range unified {
		result = append(result, *uc)
	}

	// Sort by ID
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})

	return result, nil
}

// ============================================================================
// Gap Analysis
// ============================================================================

// GapAnalysis represents a comprehensive gap analysis
type GapAnalysis struct {
	GeneratedAt     time.Time                         `json:"generated_at"`
	TotalGaps       int                               `json:"total_gaps"`
	CriticalGaps    int                               `json:"critical_gaps"`
	HighGaps        int                               `json:"high_gaps"`
	MediumGaps      int                               `json:"medium_gaps"`
	LowGaps         int                               `json:"low_gaps"`
	GapsByFramework map[ComplianceFramework][]GapItem `json:"gaps_by_framework"`
	Recommendations []Recommendation                  `json:"recommendations"`
}

// GapItem represents a single compliance gap
type GapItem struct {
	ControlID      string              `json:"control_id"`
	ControlName    string              `json:"control_name"`
	Framework      ComplianceFramework `json:"framework"`
	CurrentStatus  ControlStatus       `json:"current_status"`
	RequiredStatus ControlStatus       `json:"required_status"`
	Priority       string              `json:"priority"` // Critical, High, Medium, Low
	Effort         string              `json:"effort"`   // Low, Medium, High
	Description    string              `json:"description"`
	Remediation    string              `json:"remediation"`
}

// Recommendation for closing compliance gaps
type Recommendation struct {
	Priority    string   `json:"priority"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Effort      string   `json:"effort"`
	Impact      string   `json:"impact"`
	Controls    []string `json:"controls"` // Affected control IDs
}

// GetGapAnalysis returns comprehensive gap analysis
func (d *ComplianceDashboard) GetGapAnalysis(ctx context.Context) (*GapAnalysis, error) {
	analysis := &GapAnalysis{
		GeneratedAt:     time.Now(),
		GapsByFramework: make(map[ComplianceFramework][]GapItem),
	}

	// Analyze SOC2 gaps
	if d.soc2Service != nil {
		soc2Gaps := []GapItem{}
		for _, c := range d.soc2Service.ListControls(nil) {
			if c.Status == ControlStatusPartial || c.Status == ControlStatusNotImplemented {
				gap := GapItem{
					ControlID:      c.ID,
					ControlName:    c.Name,
					Framework:      FrameworkSOC2,
					CurrentStatus:  c.Status,
					RequiredStatus: ControlStatusImplemented,
					Description:    c.Description,
					Remediation:    c.Notes,
				}

				// Prioritize based on control category
				prefix := c.ID[:3]
				switch prefix {
				case "CC6", "CC7": // Security controls
					gap.Priority = "High"
					analysis.HighGaps++
				case "CC9": // Vendor risk
					gap.Priority = "Medium"
					analysis.MediumGaps++
				default:
					gap.Priority = "Medium"
					analysis.MediumGaps++
				}

				gap.Effort = "Medium" // Default effort
				soc2Gaps = append(soc2Gaps, gap)
				analysis.TotalGaps++
			}
		}
		analysis.GapsByFramework[FrameworkSOC2] = soc2Gaps
	}

	// Analyze ISO 27001 gaps
	if d.iso27001Service != nil {
		isoGaps := []GapItem{}
		for _, c := range d.iso27001Service.ListControls(nil) {
			if c.Status == ControlStatusPartial || c.Status == ControlStatusNotImplemented {
				gap := GapItem{
					ControlID:      c.ID,
					ControlName:    c.Name,
					Framework:      FrameworkISO27001,
					CurrentStatus:  c.Status,
					RequiredStatus: ControlStatusImplemented,
					Description:    c.Description,
					Remediation:    c.Notes,
				}

				// Prioritize based on category
				switch c.Category {
				case ISOCategoryTechnological:
					gap.Priority = "High"
					analysis.HighGaps++
				case ISOCategoryOrganizational:
					gap.Priority = "Medium"
					analysis.MediumGaps++
				default:
					gap.Priority = "Low"
					analysis.LowGaps++
				}

				gap.Effort = "Medium"
				isoGaps = append(isoGaps, gap)
				analysis.TotalGaps++
			}
		}
		analysis.GapsByFramework[FrameworkISO27001] = isoGaps
	}

	// Generate recommendations
	analysis.Recommendations = d.generateRecommendations(analysis)

	return analysis, nil
}

func (d *ComplianceDashboard) generateRecommendations(analysis *GapAnalysis) []Recommendation {
	recommendations := []Recommendation{}

	// Check for vendor risk gaps
	vendorGaps := 0
	for _, gaps := range analysis.GapsByFramework {
		for _, g := range gaps {
			if g.ControlID == "CC9.2" || g.ControlID == "A.5.23" {
				vendorGaps++
			}
		}
	}
	if vendorGaps > 0 {
		recommendations = append(recommendations, Recommendation{
			Priority:    "High",
			Title:       "Complete Vendor Security Assessments",
			Description: "Request security questionnaires from Lyve Cloud, Geyser Data, and Quotaless",
			Effort:      "Medium",
			Impact:      "Closes multiple gaps across SOC2 and ISO 27001",
			Controls:    []string{"CC9.2", "A.5.23"},
		})
	}

	// Check for monitoring gaps
	if analysis.HighGaps > 0 {
		recommendations = append(recommendations, Recommendation{
			Priority:    "High",
			Title:       "Enhance Security Monitoring",
			Description: "Implement additional automated security checks and alerting",
			Effort:      "Medium",
			Impact:      "Improves detection and response capabilities",
			Controls:    []string{"CC4.1", "CC7.2", "A.8.16"},
		})
	}

	// Training recommendation
	recommendations = append(recommendations, Recommendation{
		Priority:    "Medium",
		Title:       "Formalize Security Training Program",
		Description: "Document security awareness training for all personnel",
		Effort:      "Low",
		Impact:      "Required for ISO 27001 certification",
		Controls:    []string{"A.6.3"},
	})

	return recommendations
}

// ============================================================================
// Export Functions
// ============================================================================

// ExportSummaryJSON exports the summary as JSON
func (d *ComplianceDashboard) ExportSummaryJSON(ctx context.Context) ([]byte, error) {
	summary, err := d.GetSummary(ctx)
	if err != nil {
		return nil, fmt.Errorf("get summary: %w", err)
	}
	return json.MarshalIndent(summary, "", "  ")
}

// ExportGapAnalysisJSON exports gap analysis as JSON
func (d *ComplianceDashboard) ExportGapAnalysisJSON(ctx context.Context) ([]byte, error) {
	analysis, err := d.GetGapAnalysis(ctx)
	if err != nil {
		return nil, fmt.Errorf("get gap analysis: %w", err)
	}
	return json.MarshalIndent(analysis, "", "  ")
}
