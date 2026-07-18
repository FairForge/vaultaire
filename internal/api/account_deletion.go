package api

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.uber.org/zap"
)

const deletionGracePeriod = 30 * 24 * time.Hour // 30 days

type DeletionStatus struct {
	Scheduled   bool      `json:"scheduled"`
	ScheduledAt time.Time `json:"scheduled_at,omitempty"`
	Reason      string    `json:"reason,omitempty"`
}

type AccountDeletionService struct {
	db     *sql.DB
	logger *zap.Logger
}

func NewAccountDeletionService(db *sql.DB, logger *zap.Logger) *AccountDeletionService {
	return &AccountDeletionService{db: db, logger: logger}
}

func (s *AccountDeletionService) ScheduleDeletion(ctx context.Context, userID, tenantID, reason string) (time.Time, error) {
	if s.db == nil {
		return time.Time{}, fmt.Errorf("database unavailable")
	}

	// If already scheduled, return the existing date.
	var existing sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT deletion_scheduled_at FROM users WHERE id = $1`, userID).Scan(&existing)
	if err != nil {
		return time.Time{}, fmt.Errorf("check existing deletion: %w", err)
	}
	if existing.Valid {
		return existing.Time, nil
	}

	scheduledAt := time.Now().Add(deletionGracePeriod)

	_, err = s.db.ExecContext(ctx,
		`UPDATE users SET deletion_scheduled_at = $1, deletion_reason = $2, status = 'pending_deletion' WHERE id = $3`,
		scheduledAt, reason, userID)
	if err != nil {
		return time.Time{}, fmt.Errorf("schedule deletion: %w", err)
	}

	s.logger.Info("account deletion scheduled",
		zap.String("user_id", userID),
		zap.String("tenant_id", tenantID),
		zap.Time("scheduled_at", scheduledAt))

	return scheduledAt, nil
}

func (s *AccountDeletionService) CancelDeletion(ctx context.Context, userID string) error {
	if s.db == nil {
		return fmt.Errorf("database unavailable")
	}

	result, err := s.db.ExecContext(ctx,
		`UPDATE users SET deletion_scheduled_at = NULL, deletion_reason = NULL, status = 'active' WHERE id = $1 AND deletion_scheduled_at IS NOT NULL`,
		userID)
	if err != nil {
		return fmt.Errorf("cancel deletion: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("no pending deletion to cancel")
	}

	s.logger.Info("account deletion cancelled", zap.String("user_id", userID))
	return nil
}

func (s *AccountDeletionService) GetDeletionStatus(ctx context.Context, userID string) (*DeletionStatus, error) {
	if s.db == nil {
		return &DeletionStatus{}, nil
	}

	var scheduledAt sql.NullTime
	var reason sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT deletion_scheduled_at, deletion_reason FROM users WHERE id = $1`, userID).
		Scan(&scheduledAt, &reason)
	if err != nil {
		return nil, fmt.Errorf("get deletion status: %w", err)
	}

	status := &DeletionStatus{}
	if scheduledAt.Valid {
		status.Scheduled = true
		status.ScheduledAt = scheduledAt.Time
	}
	if reason.Valid {
		status.Reason = reason.String
	}
	return status, nil
}

func (s *AccountDeletionService) ExecuteDeletion(ctx context.Context, userID, tenantID string) error {
	if s.db == nil {
		return fmt.Errorf("database unavailable")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin deletion tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Order is FK-safe: children before parents. webhook_endpoints goes
	// before events so deliveries cascade away via webhook_id before the
	// events they reference are deleted (works even on schemas that predate
	// migration 056's ON DELETE CASCADE on webhook_deliveries.event_id).
	// quota_usage_events goes before tenant_quotas for the same reason.
	// tenant_chunk_refs/object_metadata have UUID tenant_id columns while
	// tenant IDs are strings ("tenant-..." or UUID) — compare as text.
	deletes := []struct {
		query string
		arg   string
	}{
		{`DELETE FROM webhook_endpoints WHERE tenant_id = $1`, tenantID},
		{`DELETE FROM events WHERE tenant_id = $1`, tenantID},
		{`DELETE FROM object_head_cache WHERE tenant_id = $1`, tenantID},
		{`DELETE FROM object_versions WHERE tenant_id = $1`, tenantID},
		{`DELETE FROM object_locks WHERE tenant_id = $1`, tenantID},
		{`DELETE FROM object_locations WHERE tenant_id = $1`, tenantID},
		{`DELETE FROM tenant_chunk_refs WHERE tenant_id::text = $1`, tenantID},
		{`DELETE FROM object_metadata WHERE tenant_id::text = $1`, tenantID},
		{`DELETE FROM artifacts WHERE tenant_id = $1`, tenantID},
		{`DELETE FROM buckets WHERE tenant_id = $1`, tenantID},
		{`DELETE FROM sts_tokens WHERE tenant_id = $1`, tenantID},
		{`DELETE FROM api_keys WHERE user_id = $1`, userID},
		{`DELETE FROM quota_usage_events WHERE tenant_id = $1`, tenantID},
		{`DELETE FROM tenant_quotas WHERE tenant_id = $1`, tenantID},
		{`DELETE FROM tenant_encryption_keys WHERE tenant_id = $1`, tenantID},
		{`DELETE FROM dashboard_sessions WHERE user_id = $1`, userID},
		{`DELETE FROM user_mfa WHERE user_id = $1`, userID},
		{`DELETE FROM oauth_accounts WHERE user_id::text = $1`, userID},
		{`DELETE FROM user_activities WHERE user_id = $1`, userID},
		{`DELETE FROM bandwidth_usage_daily WHERE tenant_id = $1`, tenantID},
		{`DELETE FROM account_exports WHERE user_id = $1`, userID},
		{`DELETE FROM users WHERE id = $1`, userID},
		{`DELETE FROM tenants WHERE id = $1`, tenantID},
	}

	for _, d := range deletes {
		if _, err := tx.ExecContext(ctx, d.query, d.arg); err != nil {
			return fmt.Errorf("delete from %s: %w", d.query, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit deletion: %w", err)
	}

	s.logger.Info("account deleted",
		zap.String("user_id", userID),
		zap.String("tenant_id", tenantID))

	return nil
}
