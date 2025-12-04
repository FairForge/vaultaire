// internal/reporting/report.go
package reporting

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Report types
const (
	ReportTypeUsage       = "usage"
	ReportTypeBilling     = "billing"
	ReportTypeCompliance  = "compliance"
	ReportTypeSecurity    = "security"
	ReportTypePerformance = "performance"
)

// Export formats
const (
	FormatJSON = "json"
	FormatCSV  = "csv"
	FormatPDF  = "pdf"
	FormatXLSX = "xlsx"
)

// Severity levels
const (
	SeverityLow      = "low"
	SeverityMedium   = "medium"
	SeverityHigh     = "high"
	SeverityCritical = "critical"
)

// ReportConfig configures a report
type ReportConfig struct {
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	Format    string            `json:"format"`
	TenantID  string            `json:"tenant_id"`
	StartDate time.Time         `json:"start_date"`
	EndDate   time.Time         `json:"end_date"`
	GroupBy   string            `json:"group_by"`
	Filters   map[string]string `json:"filters"`
}

// Validate checks configuration
func (c *ReportConfig) Validate() error {
	if c.Name == "" {
		return errors.New("report: name is required")
	}
	if !c.StartDate.IsZero() && !c.EndDate.IsZero() {
		if c.EndDate.Before(c.StartDate) {
			return errors.New("report: end date must be after start date")
		}
	}
	return nil
}

// Report represents a generated report
type Report struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Type      string      `json:"type"`
	TenantID  string      `json:"tenant_id"`
	CreatedAt time.Time   `json:"created_at"`
	StartDate time.Time   `json:"start_date"`
	EndDate   time.Time   `json:"end_date"`
	Data      interface{} `json:"data"`
}

// UsageRecord represents a usage event
type UsageRecord struct {
	TenantID  string    `json:"tenant_id"`
	Operation string    `json:"operation"`
	Bytes     int64     `json:"bytes"`
	Timestamp time.Time `json:"timestamp"`
	Resource  string    `json:"resource"`
}

// AuditEvent represents an audit event
type AuditEvent struct {
	TenantID  string    `json:"tenant_id"`
	Action    string    `json:"action"`
	Resource  string    `json:"resource"`
	UserID    string    `json:"user_id"`
	Timestamp time.Time `json:"timestamp"`
	Success   bool      `json:"success"`
	Details   string    `json:"details"`
}

// SecurityEvent represents a security event
type SecurityEvent struct {
	TenantID  string    `json:"tenant_id"`
	EventType string    `json:"event_type"`
	Source    string    `json:"source"`
	Severity  string    `json:"severity"`
	Timestamp time.Time `json:"timestamp"`
	Details   string    `json:"details"`
}

// GeneratorConfig configures the report generator
type GeneratorConfig struct {
	StorageCostPerGB   float64
	BandwidthCostPerGB float64
	RequestCostPer1000 float64
}

// ReportGenerator generates reports
type ReportGenerator struct {
	config         *GeneratorConfig
	usageRecords   []*UsageRecord
	auditEvents    []*AuditEvent
	securityEvents []*SecurityEvent
	mu             sync.RWMutex
}

// NewReportGenerator creates a report generator
func NewReportGenerator(config *GeneratorConfig) *ReportGenerator {
	if config == nil {
		config = &GeneratorConfig{
			StorageCostPerGB:   0.023,
			BandwidthCostPerGB: 0.09,
			RequestCostPer1000: 0.005,
		}
	}
	return &ReportGenerator{
		config:         config,
		usageRecords:   make([]*UsageRecord, 0),
		auditEvents:    make([]*AuditEvent, 0),
		securityEvents: make([]*SecurityEvent, 0),
	}
}

// RecordUsage records a usage event
func (g *ReportGenerator) RecordUsage(record *UsageRecord) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.usageRecords = append(g.usageRecords, record)
}

// RecordAuditEvent records an audit event
func (g *ReportGenerator) RecordAuditEvent(event *AuditEvent) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.auditEvents = append(g.auditEvents, event)
}

// RecordSecurityEvent records a security event
func (g *ReportGenerator) RecordSecurityEvent(event *SecurityEvent) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.securityEvents = append(g.securityEvents, event)
}

// Generate generates a report
func (g *ReportGenerator) Generate(ctx context.Context, config *ReportConfig) (*Report, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	report := &Report{
		ID:        uuid.New().String(),
		Name:      config.Name,
		Type:      config.Type,
		TenantID:  config.TenantID,
		CreatedAt: time.Now().UTC(),
		StartDate: config.StartDate,
		EndDate:   config.EndDate,
	}

	var err error
	switch config.Type {
	case ReportTypeUsage:
		report.Data, err = g.generateUsageReport(config)
	case ReportTypeBilling:
		report.Data, err = g.generateBillingReport(config)
	case ReportTypeCompliance:
		report.Data, err = g.generateComplianceReport(config)
	case ReportTypeSecurity:
		report.Data, err = g.generateSecurityReport(config)
	default:
		report.Data, err = g.generateUsageReport(config)
	}

	if err != nil {
		return nil, err
	}

	return report, nil
}

func (g *ReportGenerator) generateUsageReport(config *ReportConfig) (map[string]interface{}, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	filtered := g.filterUsageRecords(config)

	var totalBytes int64
	var totalRequests int64
	byOperation := make(map[string]int64)
	byDay := make(map[string]int64)

	for _, r := range filtered {
		totalBytes += r.Bytes
		totalRequests++
		byOperation[r.Operation] += r.Bytes
		day := r.Timestamp.Format("2006-01-02")
		byDay[day] += r.Bytes
	}

	data := map[string]interface{}{
		"total_bytes":    totalBytes,
		"total_requests": totalRequests,
	}

	if config.GroupBy == "operation" {
		data["by_operation"] = byOperation
	}
	if config.GroupBy == "day" {
		data["by_day"] = byDay
	}

	return data, nil
}

func (g *ReportGenerator) generateBillingReport(config *ReportConfig) (map[string]interface{}, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	filtered := g.filterUsageRecords(config)

	var totalBytes int64
	var totalRequests int64

	for _, r := range filtered {
		totalBytes += r.Bytes
		totalRequests++
	}

	storageGB := float64(totalBytes) / (1024 * 1024 * 1024)
	bandwidthGB := storageGB * 0.1 // Assume 10% egress
	storageCost := storageGB * g.config.StorageCostPerGB
	bandwidthCost := bandwidthGB * g.config.BandwidthCostPerGB
	requestCost := float64(totalRequests) / 1000 * g.config.RequestCostPer1000

	return map[string]interface{}{
		"total_storage_gb":   storageGB,
		"total_bandwidth_gb": bandwidthGB,
		"total_requests":     totalRequests,
		"storage_cost":       storageCost,
		"bandwidth_cost":     bandwidthCost,
		"request_cost":       requestCost,
		"estimated_cost":     storageCost + bandwidthCost + requestCost,
	}, nil
}

func (g *ReportGenerator) generateComplianceReport(config *ReportConfig) (map[string]interface{}, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	filtered := g.filterAuditEvents(config)

	var successCount, failureCount int
	for _, e := range filtered {
		if e.Success {
			successCount++
		} else {
			failureCount++
		}
	}

	status := "compliant"
	if failureCount > successCount/10 { // More than 10% failures
		status = "review_required"
	}

	return map[string]interface{}{
		"audit_events":      filtered,
		"total_events":      len(filtered),
		"successful_events": successCount,
		"failed_events":     failureCount,
		"compliance_status": status,
	}, nil
}

func (g *ReportGenerator) generateSecurityReport(config *ReportConfig) (map[string]interface{}, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	filtered := g.filterSecurityEvents(config)

	bySeverity := make(map[string]int)
	byType := make(map[string]int)

	for _, e := range filtered {
		bySeverity[e.Severity]++
		byType[e.EventType]++
	}

	return map[string]interface{}{
		"security_events": filtered,
		"total_events":    len(filtered),
		"threat_summary": map[string]interface{}{
			"by_severity": bySeverity,
			"by_type":     byType,
		},
	}, nil
}

func (g *ReportGenerator) filterUsageRecords(config *ReportConfig) []*UsageRecord {
	var filtered []*UsageRecord

	for _, r := range g.usageRecords {
		if config.TenantID != "" && r.TenantID != config.TenantID {
			continue
		}
		if !config.StartDate.IsZero() && r.Timestamp.Before(config.StartDate) {
			continue
		}
		if !config.EndDate.IsZero() && r.Timestamp.After(config.EndDate) {
			continue
		}
		if op, ok := config.Filters["operation"]; ok && r.Operation != op {
			continue
		}
		filtered = append(filtered, r)
	}

	return filtered
}

func (g *ReportGenerator) filterAuditEvents(config *ReportConfig) []*AuditEvent {
	var filtered []*AuditEvent

	for _, e := range g.auditEvents {
		if config.TenantID != "" && e.TenantID != config.TenantID {
			continue
		}
		if !config.StartDate.IsZero() && e.Timestamp.Before(config.StartDate) {
			continue
		}
		if !config.EndDate.IsZero() && e.Timestamp.After(config.EndDate) {
			continue
		}
		filtered = append(filtered, e)
	}

	return filtered
}

func (g *ReportGenerator) filterSecurityEvents(config *ReportConfig) []*SecurityEvent {
	var filtered []*SecurityEvent

	for _, e := range g.securityEvents {
		if config.TenantID != "" && e.TenantID != config.TenantID {
			continue
		}
		if !config.StartDate.IsZero() && e.Timestamp.Before(config.StartDate) {
			continue
		}
		if !config.EndDate.IsZero() && e.Timestamp.After(config.EndDate) {
			continue
		}
		filtered = append(filtered, e)
	}

	return filtered
}

// Export exports a report to the specified format
func (g *ReportGenerator) Export(report *Report, format string) ([]byte, error) {
	switch format {
	case FormatJSON:
		return json.MarshalIndent(report, "", "  ")
	case FormatCSV:
		return g.exportCSV(report)
	case FormatPDF:
		return nil, errors.New("PDF export not implemented")
	case FormatXLSX:
		return nil, errors.New("XLSX export not implemented")
	default:
		return json.MarshalIndent(report, "", "  ")
	}
}

func (g *ReportGenerator) exportCSV(report *Report) ([]byte, error) {
	var buf strings.Builder
	w := csv.NewWriter(&buf)

	// Write header
	if err := w.Write([]string{"Field", "Value"}); err != nil {
		return nil, err
	}

	// Write basic info
	_ = w.Write([]string{"Report ID", report.ID})
	_ = w.Write([]string{"Name", report.Name})
	_ = w.Write([]string{"Type", report.Type})
	_ = w.Write([]string{"Created", report.CreatedAt.Format(time.RFC3339)})

	// Write data if it's a map
	if data, ok := report.Data.(map[string]interface{}); ok {
		for k, v := range data {
			_ = w.Write([]string{k, fmt.Sprintf("%v", v)})
		}
	}

	w.Flush()
	return []byte(buf.String()), w.Error()
}

// ReportSchedule defines a scheduled report
type ReportSchedule struct {
	ID         string        `json:"id"`
	Config     *ReportConfig `json:"config"`
	Cron       string        `json:"cron"`
	TenantID   string        `json:"tenant_id"`
	Enabled    bool          `json:"enabled"`
	LastRun    time.Time     `json:"last_run"`
	NextRun    time.Time     `json:"next_run"`
	Recipients []string      `json:"recipients"`
}

// ReportScheduler schedules reports
type ReportScheduler struct {
	generator *ReportGenerator
	schedules map[string]*ReportSchedule
	mu        sync.RWMutex
}

// NewReportScheduler creates a report scheduler
func NewReportScheduler(generator *ReportGenerator) *ReportScheduler {
	return &ReportScheduler{
		generator: generator,
		schedules: make(map[string]*ReportSchedule),
	}
}

// Schedule adds a report schedule
func (s *ReportScheduler) Schedule(schedule *ReportSchedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schedules[schedule.ID] = schedule
	return nil
}

// Unschedule removes a report schedule
func (s *ReportScheduler) Unschedule(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.schedules, id)
	return nil
}

// ListSchedules returns schedules for a tenant
func (s *ReportScheduler) ListSchedules(tenantID string) []*ReportSchedule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*ReportSchedule
	for _, sched := range s.schedules {
		if sched.TenantID == tenantID {
			result = append(result, sched)
		}
	}
	return result
}

// GetSchedule returns a schedule by ID
func (s *ReportScheduler) GetSchedule(id string) *ReportSchedule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.schedules[id]
}
