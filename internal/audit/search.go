package audit

import (
	"context"
	"fmt"
	"time"
)

// MetadataFilter filters by metadata key-value pairs
type MetadataFilter struct {
	Key   string
	Value string
}

// ComplexFilter combines multiple filter conditions
type ComplexFilter struct {
	EventType EventType
	Severity  Severity
	Result    Result
	TenantID  string
}

// DateRangeFilter filters by time range
type DateRangeFilter struct {
	Start time.Time
	End   time.Time
}

// SearchResources performs full-text search on resource paths
func (s *AuditService) SearchResources(ctx context.Context, searchTerm string, limit int) ([]*AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	query := `
		SELECT id, timestamp, user_id, tenant_id, event_type, action,
		       resource, result, severity, ip, user_agent, duration_ms,
		       error_msg, metadata, performed_by
		FROM audit_logs
		WHERE resource ILIKE $1
		ORDER BY timestamp DESC
		LIMIT $2
	`

	rows, err := s.db.QueryContext(ctx, query, "%"+searchTerm+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("search resources: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	return s.scanAuditEvents(rows)
}

// SearchByMetadata searches logs with specific metadata
func (s *AuditService) SearchByMetadata(ctx context.Context, filter *MetadataFilter, limit int) ([]*AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	// PostgreSQL JSONB query with explicit casting
	query := `
		SELECT id, timestamp, user_id, tenant_id, event_type, action,
		       resource, result, severity, ip, user_agent, duration_ms,
		       error_msg, metadata, performed_by
		FROM audit_logs
		WHERE metadata @> jsonb_build_object($1::text, $2::text)
		ORDER BY timestamp DESC
		LIMIT $3
	`

	rows, err := s.db.QueryContext(ctx, query, filter.Key, filter.Value, limit)
	if err != nil {
		return nil, fmt.Errorf("search by metadata: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	return s.scanAuditEvents(rows)
}

// SearchComplex performs search with multiple AND conditions
func (s *AuditService) SearchComplex(ctx context.Context, filter *ComplexFilter, limit int) ([]*AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	query := `
		SELECT id, timestamp, user_id, tenant_id, event_type, action,
		       resource, result, severity, ip, user_agent, duration_ms,
		       error_msg, metadata, performed_by
		FROM audit_logs
		WHERE 1=1
	`
	args := []interface{}{}
	argIdx := 1

	if filter.EventType != "" {
		query += fmt.Sprintf(" AND event_type = $%d", argIdx)
		args = append(args, filter.EventType)
		argIdx++
	}
	if filter.Severity != "" {
		query += fmt.Sprintf(" AND severity = $%d", argIdx)
		args = append(args, filter.Severity)
		argIdx++
	}
	if filter.Result != "" {
		query += fmt.Sprintf(" AND result = $%d", argIdx)
		args = append(args, filter.Result)
		argIdx++
	}
	if filter.TenantID != "" {
		query += fmt.Sprintf(" AND tenant_id = $%d", argIdx)
		args = append(args, filter.TenantID)
		argIdx++
	}

	query += " ORDER BY timestamp DESC"
	query += fmt.Sprintf(" LIMIT $%d", argIdx)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search complex: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	return s.scanAuditEvents(rows)
}

// SearchDateRange searches logs within a date range
func (s *AuditService) SearchDateRange(ctx context.Context, filter *DateRangeFilter, limit int) ([]*AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	query := `
		SELECT id, timestamp, user_id, tenant_id, event_type, action,
		       resource, result, severity, ip, user_agent, duration_ms,
		       error_msg, metadata, performed_by
		FROM audit_logs
		WHERE timestamp >= $1 AND timestamp <= $2
		ORDER BY timestamp DESC
		LIMIT $3
	`

	rows, err := s.db.QueryContext(ctx, query, filter.Start, filter.End, limit)
	if err != nil {
		return nil, fmt.Errorf("search date range: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	return s.scanAuditEvents(rows)
}
