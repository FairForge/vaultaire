package compliance

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// BreachPgStore implements BreachDatabase using PostgreSQL.
type BreachPgStore struct {
	db *sql.DB
}

// NewBreachPgStore returns a PostgreSQL-backed BreachDatabase.
func NewBreachPgStore(db *sql.DB) *BreachPgStore {
	return &BreachPgStore{db: db}
}

func (s *BreachPgStore) CreateBreach(ctx context.Context, breach *BreachRecord) error {
	categories, err := json.Marshal(breach.DataCategories)
	if err != nil {
		return fmt.Errorf("marshal data_categories: %w", err)
	}

	var metadata []byte
	if breach.Metadata != nil {
		metadata, err = json.Marshal(breach.Metadata)
		if err != nil {
			return fmt.Errorf("marshal metadata: %w", err)
		}
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO breach_records (
			id, breach_type, severity, status, detected_at, reported_at,
			affected_user_count, affected_record_count, data_categories,
			description, root_cause, consequences, mitigation,
			notified_authority, notified_subjects, authority_notified_at,
			subjects_notified_at, deadline_at, metadata, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9,
			$10, $11, $12, $13,
			$14, $15, $16,
			$17, $18, $19, $20, $21
		)`,
		breach.ID, breach.BreachType, breach.Severity, breach.Status, breach.DetectedAt, breach.ReportedAt,
		breach.AffectedUserCount, breach.AffectedRecordCount, categories,
		breach.Description, breach.RootCause, breach.Consequences, breach.Mitigation,
		breach.NotifiedAuthority, breach.NotifiedSubjects, breach.AuthorityNotifiedAt,
		breach.SubjectsNotifiedAt, breach.DeadlineAt, metadata, breach.CreatedAt, breach.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert breach_records: %w", err)
	}
	return nil
}

func (s *BreachPgStore) GetBreach(ctx context.Context, breachID uuid.UUID) (*BreachRecord, error) {
	var b BreachRecord
	var categories, metadata []byte

	err := s.db.QueryRowContext(ctx, `
		SELECT id, breach_type, severity, status, detected_at, reported_at,
			affected_user_count, affected_record_count, data_categories,
			description, root_cause, consequences, mitigation,
			notified_authority, notified_subjects, authority_notified_at,
			subjects_notified_at, deadline_at, metadata, created_at, updated_at
		FROM breach_records WHERE id = $1`, breachID).Scan(
		&b.ID, &b.BreachType, &b.Severity, &b.Status, &b.DetectedAt, &b.ReportedAt,
		&b.AffectedUserCount, &b.AffectedRecordCount, &categories,
		&b.Description, &b.RootCause, &b.Consequences, &b.Mitigation,
		&b.NotifiedAuthority, &b.NotifiedSubjects, &b.AuthorityNotifiedAt,
		&b.SubjectsNotifiedAt, &b.DeadlineAt, &metadata, &b.CreatedAt, &b.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query breach_records: %w", err)
	}

	if categories != nil {
		_ = json.Unmarshal(categories, &b.DataCategories)
	}
	if metadata != nil {
		_ = json.Unmarshal(metadata, &b.Metadata)
	}

	return &b, nil
}

func (s *BreachPgStore) UpdateBreach(ctx context.Context, breach *BreachRecord) error {
	categories, err := json.Marshal(breach.DataCategories)
	if err != nil {
		return fmt.Errorf("marshal data_categories: %w", err)
	}

	var metadata []byte
	if breach.Metadata != nil {
		metadata, err = json.Marshal(breach.Metadata)
		if err != nil {
			return fmt.Errorf("marshal metadata: %w", err)
		}
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE breach_records SET
			breach_type = $2, severity = $3, status = $4,
			reported_at = $5, affected_user_count = $6, affected_record_count = $7,
			data_categories = $8, description = $9, root_cause = $10,
			consequences = $11, mitigation = $12,
			notified_authority = $13, notified_subjects = $14,
			authority_notified_at = $15, subjects_notified_at = $16,
			metadata = $17, updated_at = $18
		WHERE id = $1`,
		breach.ID, breach.BreachType, breach.Severity, breach.Status,
		breach.ReportedAt, breach.AffectedUserCount, breach.AffectedRecordCount,
		categories, breach.Description, breach.RootCause,
		breach.Consequences, breach.Mitigation,
		breach.NotifiedAuthority, breach.NotifiedSubjects,
		breach.AuthorityNotifiedAt, breach.SubjectsNotifiedAt,
		metadata, breach.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("update breach_records: %w", err)
	}
	return nil
}

func (s *BreachPgStore) ListBreaches(ctx context.Context, filters map[string]interface{}) ([]*BreachRecord, error) {
	query := `SELECT id, breach_type, severity, status, detected_at, reported_at,
		affected_user_count, affected_record_count, data_categories,
		description, root_cause, consequences, mitigation,
		notified_authority, notified_subjects, authority_notified_at,
		subjects_notified_at, deadline_at, metadata, created_at, updated_at
		FROM breach_records WHERE 1=1`

	var args []interface{}
	argIdx := 1

	if severity, ok := filters["severity"].(string); ok && severity != "" {
		// #nosec G202 -- builds a $N placeholder only; the value is parameterized via args
		query += fmt.Sprintf(" AND severity = $%d", argIdx)
		args = append(args, severity)
		argIdx++
	}
	if status, ok := filters["status"].(string); ok && status != "" {
		// #nosec G202 -- builds a $N placeholder only; the value is parameterized via args
		query += fmt.Sprintf(" AND status = $%d", argIdx)
		args = append(args, status)
	}

	query += " ORDER BY detected_at DESC LIMIT 100"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query breach_records: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var breaches []*BreachRecord
	for rows.Next() {
		var b BreachRecord
		var categories, metadata []byte
		if err := rows.Scan(
			&b.ID, &b.BreachType, &b.Severity, &b.Status, &b.DetectedAt, &b.ReportedAt,
			&b.AffectedUserCount, &b.AffectedRecordCount, &categories,
			&b.Description, &b.RootCause, &b.Consequences, &b.Mitigation,
			&b.NotifiedAuthority, &b.NotifiedSubjects, &b.AuthorityNotifiedAt,
			&b.SubjectsNotifiedAt, &b.DeadlineAt, &metadata, &b.CreatedAt, &b.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan breach_records: %w", err)
		}
		if categories != nil {
			_ = json.Unmarshal(categories, &b.DataCategories)
		}
		if metadata != nil {
			_ = json.Unmarshal(metadata, &b.Metadata)
		}
		breaches = append(breaches, &b)
	}

	return breaches, nil
}

func (s *BreachPgStore) AddAffectedUsers(ctx context.Context, breachID uuid.UUID, userIDs []uuid.UUID) error {
	for _, uid := range userIDs {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO breach_affected_users (id, breach_id, user_id, created_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (breach_id, user_id) DO NOTHING`,
			uuid.New(), breachID, uid, time.Now(),
		)
		if err != nil {
			return fmt.Errorf("insert breach_affected_users: %w", err)
		}
	}
	return nil
}

func (s *BreachPgStore) GetAffectedUsers(ctx context.Context, breachID uuid.UUID) ([]*BreachAffectedUser, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, breach_id, user_id, notified, notified_at, method, created_at
		FROM breach_affected_users WHERE breach_id = $1`, breachID)
	if err != nil {
		return nil, fmt.Errorf("query breach_affected_users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var users []*BreachAffectedUser
	for rows.Next() {
		var u BreachAffectedUser
		if err := rows.Scan(&u.ID, &u.BreachID, &u.UserID, &u.Notified, &u.NotifiedAt, &u.Method, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan breach_affected_users: %w", err)
		}
		users = append(users, &u)
	}

	return users, nil
}

func (s *BreachPgStore) UpdateAffectedUser(ctx context.Context, affected *BreachAffectedUser) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE breach_affected_users SET notified = $2, notified_at = $3, method = $4
		WHERE id = $1`,
		affected.ID, affected.Notified, affected.NotifiedAt, affected.Method,
	)
	if err != nil {
		return fmt.Errorf("update breach_affected_users: %w", err)
	}
	return nil
}

func (s *BreachPgStore) CreateNotification(ctx context.Context, notification *BreachNotification) error {
	var metadata []byte
	var err error
	if notification.Metadata != nil {
		metadata, err = json.Marshal(notification.Metadata)
		if err != nil {
			return fmt.Errorf("marshal metadata: %w", err)
		}
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO breach_notifications (
			id, breach_id, notification_type, recipient, sent_at,
			method, status, content, metadata, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		notification.ID, notification.BreachID, notification.NotificationType,
		notification.Recipient, notification.SentAt,
		notification.Method, notification.Status, notification.Content,
		metadata, notification.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert breach_notifications: %w", err)
	}
	return nil
}

func (s *BreachPgStore) GetNotifications(ctx context.Context, breachID uuid.UUID) ([]*BreachNotification, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, breach_id, notification_type, recipient, sent_at,
			method, status, content, metadata, created_at
		FROM breach_notifications WHERE breach_id = $1 ORDER BY created_at DESC`, breachID)
	if err != nil {
		return nil, fmt.Errorf("query breach_notifications: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var notifications []*BreachNotification
	for rows.Next() {
		var n BreachNotification
		var metadata []byte
		if err := rows.Scan(
			&n.ID, &n.BreachID, &n.NotificationType, &n.Recipient, &n.SentAt,
			&n.Method, &n.Status, &n.Content, &metadata, &n.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan breach_notifications: %w", err)
		}
		if metadata != nil {
			_ = json.Unmarshal(metadata, &n.Metadata)
		}
		notifications = append(notifications, &n)
	}

	return notifications, nil
}

func (s *BreachPgStore) GetBreachStats(ctx context.Context) (*BreachStats, error) {
	stats := &BreachStats{
		BreachesByType:     make(map[string]int),
		BreachesBySeverity: make(map[string]int),
		BreachesByStatus:   make(map[string]int),
	}

	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM breach_records`).Scan(&stats.TotalBreaches)
	if err != nil {
		return nil, fmt.Errorf("count breach_records: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT breach_type, COUNT(*) FROM breach_records GROUP BY breach_type`)
	if err != nil {
		return nil, fmt.Errorf("query by type: %w", err)
	}
	for rows.Next() {
		var t string
		var c int
		if err := rows.Scan(&t, &c); err == nil {
			stats.BreachesByType[t] = c
		}
	}
	_ = rows.Close()

	rows, err = s.db.QueryContext(ctx, `SELECT severity, COUNT(*) FROM breach_records GROUP BY severity`)
	if err != nil {
		return nil, fmt.Errorf("query by severity: %w", err)
	}
	for rows.Next() {
		var t string
		var c int
		if err := rows.Scan(&t, &c); err == nil {
			stats.BreachesBySeverity[t] = c
		}
	}
	_ = rows.Close()

	rows, err = s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM breach_records GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("query by status: %w", err)
	}
	for rows.Next() {
		var t string
		var c int
		if err := rows.Scan(&t, &c); err == nil {
			stats.BreachesByStatus[t] = c
		}
	}
	_ = rows.Close()

	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM breach_records WHERE deadline_at > NOW() AND notified_authority = false`).Scan(&stats.WithinDeadline)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM breach_records WHERE deadline_at <= NOW() AND notified_authority = false`).Scan(&stats.MissedDeadline)

	var avgSeconds sql.NullFloat64
	_ = s.db.QueryRowContext(ctx, `SELECT AVG(EXTRACT(EPOCH FROM (COALESCE(reported_at, NOW()) - detected_at))) FROM breach_records`).Scan(&avgSeconds)
	if avgSeconds.Valid {
		stats.AverageResponseTime = time.Duration(avgSeconds.Float64) * time.Second
	}

	return stats, nil
}
