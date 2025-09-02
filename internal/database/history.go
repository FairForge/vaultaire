package database

import (
	"context"
	"database/sql"
	"time"
)

type HistoryStore struct {
	db *sql.DB // Changed from undefined DB to *sql.DB
}

func NewHistoryStore(db *sql.DB) *HistoryStore {
	return &HistoryStore{db: db}
}

func (h *HistoryStore) RecordChange(ctx context.Context, tenantID, container, artifact, operation string) error {
	query := `
        INSERT INTO change_history (tenant_id, container, artifact, operation, timestamp)
        VALUES ($1, $2, $3, $4, $5)
    `
	_, err := h.db.ExecContext(ctx, query, tenantID, container, artifact, operation, time.Now())
	return err
}

func (h *HistoryStore) GetHistory(ctx context.Context, tenantID, container, artifact string) ([]ChangeRecord, error) {
	query := `
        SELECT id, tenant_id, container, artifact, operation, timestamp
        FROM change_history
        WHERE tenant_id = $1 AND container = $2 AND artifact = $3
        ORDER BY timestamp DESC
    `
	rows, err := h.db.QueryContext(ctx, query, tenantID, container, artifact)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var records []ChangeRecord
	for rows.Next() {
		var r ChangeRecord
		err := rows.Scan(&r.ID, &r.TenantID, &r.Container, &r.Artifact, &r.Operation, &r.Timestamp)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

type ChangeRecord struct {
	ID        int64
	TenantID  string
	Container string
	Artifact  string
	Operation string
	Timestamp time.Time
}
