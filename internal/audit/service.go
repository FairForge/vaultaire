package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/FairForge/vaultaire/internal/database"
	"github.com/google/uuid"
)

// AuditService handles audit logging
type AuditService struct {
	db *database.Postgres
}

// NewAuditService creates a new audit service
func NewAuditService(db *database.Postgres) *AuditService {
	return &AuditService{db: db}
}

// LogEvent logs an audit event to the database
func (s *AuditService) LogEvent(ctx context.Context, event *AuditEvent) error {
	if event.ID == uuid.Nil {
		event.ID = uuid.New()
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	if event.Severity == "" {
		event.Severity = SeverityInfo
	}

	// Convert metadata to JSON
	var metadataJSON []byte
	var err error
	if event.Metadata != nil {
		metadataJSON, err = json.Marshal(event.Metadata)
		if err != nil {
			return fmt.Errorf("marshal metadata: %w", err)
		}
	}

	query := `
		INSERT INTO audit_logs (
			id, timestamp, user_id, tenant_id, event_type, action,
			resource, result, severity, ip, user_agent, duration_ms,
			error_msg, metadata, performed_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
	`

	_, err = s.db.ExecContext(ctx, query,
		event.ID,
		event.Timestamp,
		nullUUID(event.UserID),
		nullString(event.TenantID),
		event.EventType,
		event.Action,
		nullString(event.Resource),
		event.Result,
		event.Severity,
		nullString(event.IP),
		nullString(event.UserAgent),
		nullInt64(event.Duration.Milliseconds()),
		nullString(event.ErrorMsg),
		nullBytes(metadataJSON),
		nullUUID(event.PerformedBy),
	)

	return err
}

// Query retrieves audit logs based on query parameters
func (s *AuditService) Query(ctx context.Context, query *AuditQuery) ([]*AuditEvent, error) {
	if query.Limit <= 0 {
		query.Limit = 100
	}
	if query.Limit > 1000 {
		query.Limit = 1000
	}

	sqlQuery := `
		SELECT id, timestamp, user_id, tenant_id, event_type, action,
		       resource, result, severity, ip, user_agent, duration_ms,
		       error_msg, metadata, performed_by
		FROM audit_logs
		WHERE 1=1
	`
	args := []interface{}{}
	argIdx := 1

	if query.UserID != nil {
		sqlQuery += fmt.Sprintf(" AND user_id = $%d", argIdx)
		args = append(args, *query.UserID)
		argIdx++
	}
	if query.TenantID != nil {
		sqlQuery += fmt.Sprintf(" AND tenant_id = $%d", argIdx)
		args = append(args, *query.TenantID)
		argIdx++
	}
	if query.EventType != nil {
		sqlQuery += fmt.Sprintf(" AND event_type = $%d", argIdx)
		args = append(args, *query.EventType)
		argIdx++
	}
	if query.Resource != nil {
		sqlQuery += fmt.Sprintf(" AND resource LIKE $%d", argIdx)
		args = append(args, "%"+*query.Resource+"%")
		argIdx++
	}
	if query.Result != nil {
		sqlQuery += fmt.Sprintf(" AND result = $%d", argIdx)
		args = append(args, *query.Result)
		argIdx++
	}
	if query.Severity != nil {
		sqlQuery += fmt.Sprintf(" AND severity = $%d", argIdx)
		args = append(args, *query.Severity)
		argIdx++
	}
	if query.StartTime != nil {
		sqlQuery += fmt.Sprintf(" AND timestamp >= $%d", argIdx)
		args = append(args, *query.StartTime)
		argIdx++
	}
	if query.EndTime != nil {
		sqlQuery += fmt.Sprintf(" AND timestamp <= $%d", argIdx)
		args = append(args, *query.EndTime)
		argIdx++
	}

	sqlQuery += " ORDER BY timestamp DESC"
	sqlQuery += fmt.Sprintf(" LIMIT $%d OFFSET $%d", argIdx, argIdx+1)
	args = append(args, query.Limit, query.Offset)

	rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("query audit logs: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			// Log error but don't fail the query
			_ = err
		}
	}()

	return s.scanAuditEvents(rows)
}

// scanAuditEvents is a helper that scans rows into AuditEvent structs
func (s *AuditService) scanAuditEvents(rows *sql.Rows) ([]*AuditEvent, error) {
	var events []*AuditEvent
	for rows.Next() {
		event := &AuditEvent{}
		var userID, performedBy sql.NullString
		var tenantID, resource, ip, userAgent, errorMsg sql.NullString
		var durationMs sql.NullInt64
		var metadataJSON []byte

		err := rows.Scan(
			&event.ID,
			&event.Timestamp,
			&userID,
			&tenantID,
			&event.EventType,
			&event.Action,
			&resource,
			&event.Result,
			&event.Severity,
			&ip,
			&userAgent,
			&durationMs,
			&errorMsg,
			&metadataJSON,
			&performedBy,
		)
		if err != nil {
			return nil, fmt.Errorf("scan audit log: %w", err)
		}

		// Convert nullable fields
		if userID.Valid {
			event.UserID, _ = uuid.Parse(userID.String)
		}
		if performedBy.Valid {
			event.PerformedBy, _ = uuid.Parse(performedBy.String)
		}
		event.TenantID = tenantID.String
		event.Resource = resource.String
		event.IP = ip.String
		event.UserAgent = userAgent.String
		event.ErrorMsg = errorMsg.String
		if durationMs.Valid {
			event.Duration = time.Duration(durationMs.Int64) * time.Millisecond
		}
		if metadataJSON != nil {
			if err := json.Unmarshal(metadataJSON, &event.Metadata); err != nil {
				// Log unmarshal error but continue - metadata is optional
				event.Metadata = nil
			}
		}

		events = append(events, event)
	}

	return events, rows.Err()
}

// Helper functions for NULL handling
func nullUUID(u uuid.UUID) interface{} {
	if u == uuid.Nil {
		return nil
	}
	return u
}

func nullString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64(i int64) interface{} {
	if i == 0 {
		return nil
	}
	return i
}

func nullBytes(b []byte) interface{} {
	if b == nil {
		return nil
	}
	return b
}

// Search searches audit events with filters
func (s *AuditService) Search(ctx context.Context, filters *SearchFilters, limit, offset int) ([]*AuditEvent, error) {
	query := `
		SELECT id, timestamp, user_id, tenant_id, event_type, action,
		       resource, result, severity, ip, user_agent, duration_ms,
		       error_msg, metadata, performed_by
		FROM audit_logs
		WHERE 1=1
	`
	args := []interface{}{}
	argCount := 1

	if filters.TenantID != "" {
		query += fmt.Sprintf(" AND tenant_id = $%d", argCount)
		args = append(args, filters.TenantID)
		argCount++
	}

	if filters.UserID != nil {
		query += fmt.Sprintf(" AND user_id = $%d", argCount)
		args = append(args, filters.UserID.String())
		argCount++
	}

	if filters.EventType != "" {
		query += fmt.Sprintf(" AND event_type = $%d", argCount)
		args = append(args, filters.EventType)
		argCount++
	}

	query += " ORDER BY timestamp DESC"
	query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", argCount, argCount+1)
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search events: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			_ = err
		}
	}()

	return s.scanAuditEvents(rows)
}

// GetEventByID retrieves a single event by ID
func (s *AuditService) GetEventByID(ctx context.Context, id string) (*AuditEvent, error) {
	query := `
		SELECT id, timestamp, user_id, tenant_id, event_type, action,
		       resource, result, severity, ip, user_agent, duration_ms,
		       error_msg, metadata, performed_by
		FROM audit_logs
		WHERE id = $1
	`

	rows, err := s.db.QueryContext(ctx, query, id)
	if err != nil {
		return nil, fmt.Errorf("get event by id: %w", err)
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

	if len(events) == 0 {
		return nil, fmt.Errorf("event not found")
	}

	return events[0], nil
}
