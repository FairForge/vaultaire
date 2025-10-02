package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// FailedLoginAlert represents a failed login pattern
type FailedLoginAlert struct {
	UserID    uuid.UUID
	IP        string
	Count     int64
	FirstSeen time.Time
	LastSeen  time.Time
}

// SuspiciousActivityAlert represents suspicious behavior
type SuspiciousActivityAlert struct {
	UserID      uuid.UUID
	Description string
	EventCount  int64
	UniqueIPs   int64
	FirstSeen   time.Time
	LastSeen    time.Time
}

// UnusualAccessAlert represents unusual access patterns
type UnusualAccessAlert struct {
	UserID    uuid.UUID
	Resource  string
	IP        string
	Timestamp time.Time
	Reason    string
}

// CriticalAlert represents a critical severity event
type CriticalAlert struct {
	Event     *AuditEvent
	Timestamp time.Time
}

// AlertSummary provides an overview of alerts
type AlertSummary struct {
	TotalAlerts    int64
	CriticalAlerts int64
	WarningAlerts  int64
	FailedLogins   int64
	SecurityEvents int64
}

// DetectFailedLoginAttempts detects multiple failed login attempts
func (s *AuditService) DetectFailedLoginAttempts(ctx context.Context, timeWindow time.Duration, threshold int) ([]FailedLoginAlert, error) {
	cutoff := time.Now().Add(-timeWindow)

	query := `
		SELECT user_id, ip, COUNT(*) as count,
		       MIN(timestamp) as first_seen,
		       MAX(timestamp) as last_seen
		FROM audit_logs
		WHERE event_type = $1
		  AND result = $2
		  AND timestamp >= $3
		  AND user_id IS NOT NULL
		GROUP BY user_id, ip
		HAVING COUNT(*) >= $4
		ORDER BY count DESC
	`

	rows, err := s.db.QueryContext(ctx, query, EventTypeLogin, ResultFailure, cutoff, threshold)
	if err != nil {
		return nil, fmt.Errorf("query failed logins: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	var alerts []FailedLoginAlert
	for rows.Next() {
		var alert FailedLoginAlert
		var userIDStr string
		if err := rows.Scan(&userIDStr, &alert.IP, &alert.Count, &alert.FirstSeen, &alert.LastSeen); err != nil {
			return nil, fmt.Errorf("scan failed login alert: %w", err)
		}
		alert.UserID, _ = uuid.Parse(userIDStr)
		alerts = append(alerts, alert)
	}

	return alerts, rows.Err()
}

// DetectSuspiciousActivity detects suspicious patterns
func (s *AuditService) DetectSuspiciousActivity(ctx context.Context, timeWindow time.Duration) ([]SuspiciousActivityAlert, error) {
	cutoff := time.Now().Add(-timeWindow)

	// Detect users accessing from many different IPs
	query := `
		SELECT user_id,
		       COUNT(*) as event_count,
		       COUNT(DISTINCT ip) as unique_ips,
		       MIN(timestamp) as first_seen,
		       MAX(timestamp) as last_seen
		FROM audit_logs
		WHERE timestamp >= $1
		  AND user_id IS NOT NULL
		  AND ip IS NOT NULL
		GROUP BY user_id
		HAVING COUNT(DISTINCT ip) >= 3
		ORDER BY unique_ips DESC
	`

	rows, err := s.db.QueryContext(ctx, query, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query suspicious activity: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	var alerts []SuspiciousActivityAlert
	for rows.Next() {
		var alert SuspiciousActivityAlert
		var userIDStr string
		if err := rows.Scan(&userIDStr, &alert.EventCount, &alert.UniqueIPs, &alert.FirstSeen, &alert.LastSeen); err != nil {
			return nil, fmt.Errorf("scan suspicious activity: %w", err)
		}
		alert.UserID, _ = uuid.Parse(userIDStr)
		alert.Description = fmt.Sprintf("Access from %d different IPs in %s", alert.UniqueIPs, timeWindow)
		alerts = append(alerts, alert)
	}

	return alerts, rows.Err()
}

// DetectUnusualAccessPatterns detects unusual access patterns
func (s *AuditService) DetectUnusualAccessPatterns(ctx context.Context, timeWindow time.Duration) ([]UnusualAccessAlert, error) {
	cutoff := time.Now().Add(-timeWindow)

	// Detect access during unusual hours (e.g., 2-5 AM)
	query := `
		SELECT user_id, resource, ip, timestamp
		FROM audit_logs
		WHERE timestamp >= $1
		  AND EXTRACT(HOUR FROM timestamp) BETWEEN 2 AND 5
		  AND user_id IS NOT NULL
		ORDER BY timestamp DESC
		LIMIT 100
	`

	rows, err := s.db.QueryContext(ctx, query, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query unusual access: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	var alerts []UnusualAccessAlert
	for rows.Next() {
		var alert UnusualAccessAlert
		var userIDStr string
		var resourceNull, ipNull string
		if err := rows.Scan(&userIDStr, &resourceNull, &ipNull, &alert.Timestamp); err != nil {
			return nil, fmt.Errorf("scan unusual access: %w", err)
		}
		alert.UserID, _ = uuid.Parse(userIDStr)
		alert.Resource = resourceNull
		alert.IP = ipNull
		alert.Reason = "Access during unusual hours (2-5 AM)"
		alerts = append(alerts, alert)
	}

	return alerts, rows.Err()
}

// GetCriticalAlerts returns recent critical severity events
func (s *AuditService) GetCriticalAlerts(ctx context.Context, timeWindow time.Duration) ([]CriticalAlert, error) {
	cutoff := time.Now().Add(-timeWindow)

	query := `
		SELECT id, timestamp, user_id, tenant_id, event_type, action,
		       resource, result, severity, ip, user_agent, duration_ms,
		       error_msg, metadata, performed_by
		FROM audit_logs
		WHERE severity = $1
		  AND timestamp >= $2
		ORDER BY timestamp DESC
		LIMIT 100
	`

	rows, err := s.db.QueryContext(ctx, query, SeverityCritical, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query critical alerts: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	events, err := s.scanAuditEvents(rows)
	if err != nil {
		return nil, err
	}

	var alerts []CriticalAlert
	for _, event := range events {
		alerts = append(alerts, CriticalAlert{
			Event:     event,
			Timestamp: event.Timestamp,
		})
	}

	return alerts, nil
}

// GetAlertSummary returns a summary of alerts
func (s *AuditService) GetAlertSummary(ctx context.Context, timeWindow time.Duration) (*AlertSummary, error) {
	cutoff := time.Now().Add(-timeWindow)
	summary := &AlertSummary{}

	// Critical alerts
	query := `SELECT COUNT(*) FROM audit_logs WHERE severity = $1 AND timestamp >= $2`
	if err := s.db.QueryRowContext(ctx, query, SeverityCritical, cutoff).Scan(&summary.CriticalAlerts); err != nil {
		return nil, fmt.Errorf("count critical alerts: %w", err)
	}

	// Warning alerts
	query = `SELECT COUNT(*) FROM audit_logs WHERE severity = $1 AND timestamp >= $2`
	if err := s.db.QueryRowContext(ctx, query, SeverityWarning, cutoff).Scan(&summary.WarningAlerts); err != nil {
		return nil, fmt.Errorf("count warning alerts: %w", err)
	}

	// Failed logins
	query = `SELECT COUNT(*) FROM audit_logs WHERE event_type = $1 AND result = $2 AND timestamp >= $3`
	if err := s.db.QueryRowContext(ctx, query, EventTypeLogin, ResultFailure, cutoff).Scan(&summary.FailedLogins); err != nil {
		return nil, fmt.Errorf("count failed logins: %w", err)
	}

	// Security events
	query = `SELECT COUNT(*) FROM audit_logs WHERE event_type IN ($1, $2, $3) AND timestamp >= $4`
	if err := s.db.QueryRowContext(ctx, query, EventTypeSecurityAlert, EventTypeAccessDenied, EventTypeSuspiciousActivity, cutoff).Scan(&summary.SecurityEvents); err != nil {
		return nil, fmt.Errorf("count security events: %w", err)
	}

	summary.TotalAlerts = summary.CriticalAlerts + summary.WarningAlerts

	return summary, nil
}
