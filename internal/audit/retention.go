package audit

import (
	"context"
	"fmt"
	"time"
)

// RetentionPolicy defines how long to keep audit logs
type RetentionPolicy struct {
	EventType EventType
	Severity  Severity
	Duration  time.Duration
}

// DefaultRetentionPolicies returns the default retention policies
func DefaultRetentionPolicies() []RetentionPolicy {
	return []RetentionPolicy{
		// Compliance events - 1 year
		{
			EventType: EventTypeAccessDenied,
			Severity:  "",
			Duration:  365 * 24 * time.Hour,
		},
		{
			EventType: EventTypeSecurityAlert,
			Severity:  "",
			Duration:  365 * 24 * time.Hour,
		},
		{
			EventType: EventTypeSuspiciousActivity,
			Severity:  "",
			Duration:  365 * 24 * time.Hour,
		},
		// Critical severity - 90 days
		{
			EventType: "",
			Severity:  SeverityCritical,
			Duration:  90 * 24 * time.Hour,
		},
		// Error severity - 60 days
		{
			EventType: "",
			Severity:  SeverityError,
			Duration:  60 * 24 * time.Hour,
		},
		// Default - 30 days
		{
			EventType: "",
			Severity:  "",
			Duration:  30 * 24 * time.Hour,
		},
	}
}

// CleanupOldLogs removes logs older than the specified retention period
func (s *AuditService) CleanupOldLogs(ctx context.Context, retention time.Duration) (int64, error) {
	cutoff := time.Now().Add(-retention)

	query := `
		DELETE FROM audit_logs
		WHERE timestamp < $1
	`

	result, err := s.db.ExecContext(ctx, query, cutoff)
	if err != nil {
		return 0, fmt.Errorf("cleanup old logs: %w", err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}

	return deleted, nil
}

// CleanupOldLogsByType removes logs of a specific type older than retention period
func (s *AuditService) CleanupOldLogsByType(ctx context.Context, eventType EventType, retention time.Duration) (int64, error) {
	cutoff := time.Now().Add(-retention)

	query := `
		DELETE FROM audit_logs
		WHERE timestamp < $1 AND event_type = $2
	`

	result, err := s.db.ExecContext(ctx, query, cutoff, eventType)
	if err != nil {
		return 0, fmt.Errorf("cleanup old logs by type: %w", err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}

	return deleted, nil
}

// CleanupOldLogsBySeverity removes logs of a specific severity older than retention period
func (s *AuditService) CleanupOldLogsBySeverity(ctx context.Context, severity Severity, retention time.Duration) (int64, error) {
	cutoff := time.Now().Add(-retention)

	query := `
		DELETE FROM audit_logs
		WHERE timestamp < $1 AND severity = $2
	`

	result, err := s.db.ExecContext(ctx, query, cutoff, severity)
	if err != nil {
		return 0, fmt.Errorf("cleanup old logs by severity: %w", err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}

	return deleted, nil
}

// ApplyRetentionPolicies applies multiple retention policies in order
func (s *AuditService) ApplyRetentionPolicies(ctx context.Context, policies []RetentionPolicy) (int64, error) {
	var totalDeleted int64

	// Apply policies from most specific to least specific
	for _, policy := range policies {
		var deleted int64
		var err error

		switch {
		case policy.EventType != "" && policy.Severity != "":
			// Both event type and severity
			deleted, err = s.cleanupByTypeAndSeverity(ctx, policy.EventType, policy.Severity, policy.Duration)
		case policy.EventType != "":
			// Event type only
			deleted, err = s.CleanupOldLogsByType(ctx, policy.EventType, policy.Duration)
		case policy.Severity != "":
			// Severity only
			deleted, err = s.CleanupOldLogsBySeverity(ctx, policy.Severity, policy.Duration)
		default:
			// Default policy (all remaining logs)
			deleted, err = s.CleanupOldLogs(ctx, policy.Duration)
		}

		if err != nil {
			return totalDeleted, fmt.Errorf("apply policy: %w", err)
		}

		totalDeleted += deleted
	}

	return totalDeleted, nil
}

// cleanupByTypeAndSeverity removes logs matching both type and severity
func (s *AuditService) cleanupByTypeAndSeverity(ctx context.Context, eventType EventType, severity Severity, retention time.Duration) (int64, error) {
	cutoff := time.Now().Add(-retention)

	query := `
		DELETE FROM audit_logs
		WHERE timestamp < $1 AND event_type = $2 AND severity = $3
	`

	result, err := s.db.ExecContext(ctx, query, cutoff, eventType, severity)
	if err != nil {
		return 0, fmt.Errorf("cleanup by type and severity: %w", err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}

	return deleted, nil
}
