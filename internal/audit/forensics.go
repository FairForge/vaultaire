package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// UserSessionTimeline represents a reconstructed user session
type UserSessionTimeline struct {
	UserID      uuid.UUID
	Events      []*AuditEvent
	StartTime   time.Time
	EndTime     time.Time
	TotalEvents int
}

// IPActivity represents activity from an IP address
type IPActivity struct {
	IP             string
	EventCount     int64
	UniqueUsers    int64
	FailedAttempts int64
	FirstSeen      time.Time
	LastSeen       time.Time
}

// AttackPattern represents an analyzed attack pattern
type AttackPattern struct {
	IP             string
	FailedAttempts int64
	TargetedUsers  int64
	EventTypes     map[EventType]int64
	TimeSpan       time.Duration
	IsActive       bool
}

// IncidentReport represents a security incident summary
type IncidentReport struct {
	UserID      uuid.UUID
	EventCount  int64
	FirstEvent  time.Time
	LastEvent   time.Time
	EventTypes  map[EventType]int64
	IPAddresses []string
	Resources   []string
}

// ReconstructUserSession reconstructs a user's activity timeline
func (s *AuditService) ReconstructUserSession(ctx context.Context, userID uuid.UUID, start, end time.Time) ([]*AuditEvent, error) {
	query := `
		SELECT id, timestamp, user_id, tenant_id, event_type, action,
		       resource, result, severity, ip, user_agent, duration_ms,
		       error_msg, metadata, performed_by
		FROM audit_logs
		WHERE user_id = $1
		  AND timestamp >= $2
		  AND timestamp <= $3
		ORDER BY timestamp ASC
	`

	rows, err := s.db.QueryContext(ctx, query, userID, start, end)
	if err != nil {
		return nil, fmt.Errorf("query user session: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	return s.scanAuditEvents(rows)
}

// TraceIPActivity traces all activity from a specific IP address
func (s *AuditService) TraceIPActivity(ctx context.Context, ip string, timeWindow time.Duration) (*IPActivity, error) {
	cutoff := time.Now().Add(-timeWindow)
	activity := &IPActivity{IP: ip}

	// Event count
	query := `SELECT COUNT(*) FROM audit_logs WHERE ip = $1 AND timestamp >= $2`
	if err := s.db.QueryRowContext(ctx, query, ip, cutoff).Scan(&activity.EventCount); err != nil {
		return nil, fmt.Errorf("count events: %w", err)
	}

	// Unique users
	query = `SELECT COUNT(DISTINCT user_id) FROM audit_logs WHERE ip = $1 AND timestamp >= $2 AND user_id IS NOT NULL`
	if err := s.db.QueryRowContext(ctx, query, ip, cutoff).Scan(&activity.UniqueUsers); err != nil {
		return nil, fmt.Errorf("count unique users: %w", err)
	}

	// Failed attempts
	query = `SELECT COUNT(*) FROM audit_logs WHERE ip = $1 AND result = $2 AND timestamp >= $3`
	if err := s.db.QueryRowContext(ctx, query, ip, ResultFailure, cutoff).Scan(&activity.FailedAttempts); err != nil {
		return nil, fmt.Errorf("count failed attempts: %w", err)
	}

	// Time range
	query = `SELECT MIN(timestamp), MAX(timestamp) FROM audit_logs WHERE ip = $1 AND timestamp >= $2`
	if err := s.db.QueryRowContext(ctx, query, ip, cutoff).Scan(&activity.FirstSeen, &activity.LastSeen); err != nil {
		// Ignore errors if no events found
		activity.FirstSeen = time.Now()
		activity.LastSeen = time.Now()
	}

	return activity, nil
}

// FindRelatedEvents finds events related to a specific resource
func (s *AuditService) FindRelatedEvents(ctx context.Context, resource string, timeWindow time.Duration) ([]*AuditEvent, error) {
	cutoff := time.Now().Add(-timeWindow)

	query := `
		SELECT id, timestamp, user_id, tenant_id, event_type, action,
		       resource, result, severity, ip, user_agent, duration_ms,
		       error_msg, metadata, performed_by
		FROM audit_logs
		WHERE resource = $1
		  AND timestamp >= $2
		ORDER BY timestamp DESC
		LIMIT 100
	`

	rows, err := s.db.QueryContext(ctx, query, resource, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query related events: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	return s.scanAuditEvents(rows)
}

// AnalyzeAttackPattern analyzes attack patterns from an IP
func (s *AuditService) AnalyzeAttackPattern(ctx context.Context, ip string, timeWindow time.Duration) (*AttackPattern, error) {
	cutoff := time.Now().Add(-timeWindow)
	pattern := &AttackPattern{
		IP:         ip,
		EventTypes: make(map[EventType]int64),
	}

	// Failed attempts
	query := `SELECT COUNT(*) FROM audit_logs WHERE ip = $1 AND result = $2 AND timestamp >= $3`
	if err := s.db.QueryRowContext(ctx, query, ip, ResultFailure, cutoff).Scan(&pattern.FailedAttempts); err != nil {
		return nil, fmt.Errorf("count failed attempts: %w", err)
	}

	// Targeted users
	query = `SELECT COUNT(DISTINCT user_id) FROM audit_logs WHERE ip = $1 AND timestamp >= $2 AND user_id IS NOT NULL`
	if err := s.db.QueryRowContext(ctx, query, ip, cutoff).Scan(&pattern.TargetedUsers); err != nil {
		return nil, fmt.Errorf("count targeted users: %w", err)
	}

	// Event types distribution
	query = `
		SELECT event_type, COUNT(*)
		FROM audit_logs
		WHERE ip = $1 AND timestamp >= $2
		GROUP BY event_type
	`
	rows, err := s.db.QueryContext(ctx, query, ip, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query event types: %w", err)
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
			return nil, fmt.Errorf("scan event type: %w", err)
		}
		pattern.EventTypes[eventType] = count
	}

	pattern.TimeSpan = timeWindow
	pattern.IsActive = pattern.FailedAttempts > 5

	return pattern, nil
}

// GenerateIncidentReport generates a detailed incident report for a user
func (s *AuditService) GenerateIncidentReport(ctx context.Context, userID uuid.UUID, timeWindow time.Duration) (*IncidentReport, error) {
	cutoff := time.Now().Add(-timeWindow)
	report := &IncidentReport{
		UserID:     userID,
		EventTypes: make(map[EventType]int64),
	}

	// Total events
	query := `SELECT COUNT(*) FROM audit_logs WHERE user_id = $1 AND timestamp >= $2`
	if err := s.db.QueryRowContext(ctx, query, userID, cutoff).Scan(&report.EventCount); err != nil {
		return nil, fmt.Errorf("count events: %w", err)
	}

	// Time range
	query = `SELECT MIN(timestamp), MAX(timestamp) FROM audit_logs WHERE user_id = $1 AND timestamp >= $2`
	if err := s.db.QueryRowContext(ctx, query, userID, cutoff).Scan(&report.FirstEvent, &report.LastEvent); err != nil {
		// Set defaults if no events
		report.FirstEvent = time.Now()
		report.LastEvent = time.Now()
	}

	// Event types
	query = `
		SELECT event_type, COUNT(*)
		FROM audit_logs
		WHERE user_id = $1 AND timestamp >= $2
		GROUP BY event_type
	`
	rows, err := s.db.QueryContext(ctx, query, userID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query event types: %w", err)
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
			return nil, fmt.Errorf("scan event type: %w", err)
		}
		report.EventTypes[eventType] = count
	}

	// IP addresses
	query = `SELECT DISTINCT ip FROM audit_logs WHERE user_id = $1 AND timestamp >= $2 AND ip IS NOT NULL LIMIT 10`
	ipRows, err := s.db.QueryContext(ctx, query, userID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query ips: %w", err)
	}
	defer func() {
		if err := ipRows.Close(); err != nil {
			_ = err
		}
	}()

	for ipRows.Next() {
		var ip string
		if err := ipRows.Scan(&ip); err != nil {
			return nil, fmt.Errorf("scan ip: %w", err)
		}
		report.IPAddresses = append(report.IPAddresses, ip)
	}

	// Resources
	query = `SELECT DISTINCT resource FROM audit_logs WHERE user_id = $1 AND timestamp >= $2 AND resource IS NOT NULL LIMIT 10`
	resRows, err := s.db.QueryContext(ctx, query, userID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query resources: %w", err)
	}
	defer func() {
		if err := resRows.Close(); err != nil {
			_ = err
		}
	}()

	for resRows.Next() {
		var resource string
		if err := resRows.Scan(&resource); err != nil {
			return nil, fmt.Errorf("scan resource: %w", err)
		}
		report.Resources = append(report.Resources, resource)
	}

	return report, nil
}
