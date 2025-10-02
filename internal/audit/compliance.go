package audit

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SOC2Report represents a SOC2 compliance report
type SOC2Report struct {
	Summary        string
	TotalEvents    int64
	AccessEvents   int64
	SecurityEvents int64
	FailedAttempts int64
	GeneratedAt    time.Time
	StartDate      time.Time
	EndDate        time.Time
}

// GDPRReport represents a GDPR compliance report for a user
type GDPRReport struct {
	UserID        uuid.UUID
	TotalAccesses int64
	DataExports   []time.Time
	DataDeletions []time.Time
	ConsentEvents []time.Time
	GeneratedAt   time.Time
	StartDate     time.Time
	EndDate       time.Time
}

// AccessReport represents user access patterns
type AccessReport struct {
	UserID            uuid.UUID
	TotalAccesses     int64
	AccessedResources []string
	UniqueResources   int64
	DownloadCount     int64
	UploadCount       int64
	GeneratedAt       time.Time
	StartDate         time.Time
	EndDate           time.Time
}

// SecurityReport represents security incidents
type SecurityReport struct {
	TotalIncidents      int64
	CriticalIncidents   int64
	IncidentsByType     map[EventType]int64
	IncidentsBySeverity map[Severity]int64
	GeneratedAt         time.Time
	StartDate           time.Time
	EndDate             time.Time
}

// GenerateSOC2Report generates a SOC2 compliance report
func (s *AuditService) GenerateSOC2Report(ctx context.Context, start, end time.Time) (*SOC2Report, error) {
	report := &SOC2Report{
		GeneratedAt: time.Now(),
		StartDate:   start,
		EndDate:     end,
	}

	// Total events
	query := `SELECT COUNT(*) FROM audit_logs WHERE timestamp >= $1 AND timestamp <= $2`
	err := s.db.QueryRowContext(ctx, query, start, end).Scan(&report.TotalEvents)
	if err != nil {
		return nil, fmt.Errorf("count total events: %w", err)
	}

	// Access events
	query = `SELECT COUNT(*) FROM audit_logs WHERE timestamp >= $1 AND timestamp <= $2 AND event_type IN ($3, $4, $5)`
	err = s.db.QueryRowContext(ctx, query, start, end, EventTypeLogin, EventTypeFileDownload, EventTypeFileList).Scan(&report.AccessEvents)
	if err != nil {
		return nil, fmt.Errorf("count access events: %w", err)
	}

	// Security events
	query = `SELECT COUNT(*) FROM audit_logs WHERE timestamp >= $1 AND timestamp <= $2 AND event_type IN ($3, $4, $5)`
	err = s.db.QueryRowContext(ctx, query, start, end, EventTypeSecurityAlert, EventTypeAccessDenied, EventTypeSuspiciousActivity).Scan(&report.SecurityEvents)
	if err != nil {
		return nil, fmt.Errorf("count security events: %w", err)
	}

	// Failed attempts
	query = `SELECT COUNT(*) FROM audit_logs WHERE timestamp >= $1 AND timestamp <= $2 AND result = $3`
	err = s.db.QueryRowContext(ctx, query, start, end, ResultFailure).Scan(&report.FailedAttempts)
	if err != nil {
		return nil, fmt.Errorf("count failed attempts: %w", err)
	}

	report.Summary = fmt.Sprintf("SOC2 Compliance Report: %d total events, %d security incidents, %d failed attempts",
		report.TotalEvents, report.SecurityEvents, report.FailedAttempts)

	return report, nil
}

// GenerateGDPRReport generates a GDPR compliance report for a specific user
func (s *AuditService) GenerateGDPRReport(ctx context.Context, userID uuid.UUID, start, end time.Time) (*GDPRReport, error) {
	report := &GDPRReport{
		UserID:      userID,
		GeneratedAt: time.Now(),
		StartDate:   start,
		EndDate:     end,
	}

	// Total accesses
	query := `SELECT COUNT(*) FROM audit_logs WHERE user_id = $1 AND timestamp >= $2 AND timestamp <= $3`
	err := s.db.QueryRowContext(ctx, query, userID, start, end).Scan(&report.TotalAccesses)
	if err != nil {
		return nil, fmt.Errorf("count total accesses: %w", err)
	}

	// Data exports
	query = `SELECT timestamp FROM audit_logs WHERE user_id = $1 AND event_type = $2 AND timestamp >= $3 AND timestamp <= $4 ORDER BY timestamp DESC`
	rows, err := s.db.QueryContext(ctx, query, userID, EventTypeDataExport, start, end)
	if err != nil {
		return nil, fmt.Errorf("query data exports: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	for rows.Next() {
		var ts time.Time
		if err := rows.Scan(&ts); err != nil {
			return nil, fmt.Errorf("scan timestamp: %w", err)
		}
		report.DataExports = append(report.DataExports, ts)
	}

	return report, nil
}

// GenerateAccessReport generates an access report for a user
func (s *AuditService) GenerateAccessReport(ctx context.Context, userID uuid.UUID, start, end time.Time) (*AccessReport, error) {
	report := &AccessReport{
		UserID:      userID,
		GeneratedAt: time.Now(),
		StartDate:   start,
		EndDate:     end,
	}

	// Total accesses
	query := `SELECT COUNT(*) FROM audit_logs WHERE user_id = $1 AND timestamp >= $2 AND timestamp <= $3`
	err := s.db.QueryRowContext(ctx, query, userID, start, end).Scan(&report.TotalAccesses)
	if err != nil {
		return nil, fmt.Errorf("count total accesses: %w", err)
	}

	// Accessed resources
	query = `SELECT DISTINCT resource FROM audit_logs WHERE user_id = $1 AND timestamp >= $2 AND timestamp <= $3 AND resource IS NOT NULL`
	rows, err := s.db.QueryContext(ctx, query, userID, start, end)
	if err != nil {
		return nil, fmt.Errorf("query resources: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	for rows.Next() {
		var resource string
		if err := rows.Scan(&resource); err != nil {
			return nil, fmt.Errorf("scan resource: %w", err)
		}
		report.AccessedResources = append(report.AccessedResources, resource)
	}

	report.UniqueResources = int64(len(report.AccessedResources))

	// Download count
	query = `SELECT COUNT(*) FROM audit_logs WHERE user_id = $1 AND event_type = $2 AND timestamp >= $3 AND timestamp <= $4`
	err = s.db.QueryRowContext(ctx, query, userID, EventTypeFileDownload, start, end).Scan(&report.DownloadCount)
	if err != nil {
		return nil, fmt.Errorf("count downloads: %w", err)
	}

	// Upload count
	err = s.db.QueryRowContext(ctx, query, userID, EventTypeFileUpload, start, end).Scan(&report.UploadCount)
	if err != nil {
		return nil, fmt.Errorf("count uploads: %w", err)
	}

	return report, nil
}

// GenerateSecurityReport generates a security incident report
func (s *AuditService) GenerateSecurityReport(ctx context.Context, start, end time.Time) (*SecurityReport, error) {
	report := &SecurityReport{
		GeneratedAt:         time.Now(),
		StartDate:           start,
		EndDate:             end,
		IncidentsByType:     make(map[EventType]int64),
		IncidentsBySeverity: make(map[Severity]int64),
	}

	// Total incidents
	query := `SELECT COUNT(*) FROM audit_logs WHERE timestamp >= $1 AND timestamp <= $2 AND severity IN ($3, $4)`
	err := s.db.QueryRowContext(ctx, query, start, end, SeverityError, SeverityCritical).Scan(&report.TotalIncidents)
	if err != nil {
		return nil, fmt.Errorf("count total incidents: %w", err)
	}

	// Critical incidents
	query = `SELECT COUNT(*) FROM audit_logs WHERE timestamp >= $1 AND timestamp <= $2 AND severity = $3`
	err = s.db.QueryRowContext(ctx, query, start, end, SeverityCritical).Scan(&report.CriticalIncidents)
	if err != nil {
		return nil, fmt.Errorf("count critical incidents: %w", err)
	}

	// Incidents by type
	query = `SELECT event_type, COUNT(*) FROM audit_logs WHERE timestamp >= $1 AND timestamp <= $2 AND severity IN ($3, $4) GROUP BY event_type`
	rows, err := s.db.QueryContext(ctx, query, start, end, SeverityError, SeverityCritical)
	if err != nil {
		return nil, fmt.Errorf("query incidents by type: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	for rows.Next() {
		var eventType EventType
		var count int64
		if err := rows.Scan(&eventType, &count); err != nil {
			return nil, fmt.Errorf("scan incident type: %w", err)
		}
		report.IncidentsByType[eventType] = count
	}

	return report, nil
}

// ExportToCSV exports an access report to CSV format
func (r *AccessReport) ExportToCSV() (string, error) {
	buf := new(bytes.Buffer)
	writer := csv.NewWriter(buf)

	// Write header
	if err := writer.Write([]string{"Timestamp", "Resource", "Action"}); err != nil {
		return "", fmt.Errorf("write CSV header: %w", err)
	}

	// Write data
	for _, resource := range r.AccessedResources {
		if err := writer.Write([]string{
			r.GeneratedAt.Format(time.RFC3339),
			resource,
			"ACCESS",
		}); err != nil {
			return "", fmt.Errorf("write CSV row: %w", err)
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		return "", fmt.Errorf("flush CSV writer: %w", err)
	}

	return buf.String(), nil
}
