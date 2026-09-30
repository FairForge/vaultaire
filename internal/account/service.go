// Package account is the one account-deletion state machine (WP-R10-3,
// R12-22 / WP-R12-7, R11-13 / D-16).
//
// Before this package the dashboard ran its own UPDATE on users (it could
// not import internal/api — an import cycle) while the management API and
// DELETE /api/v1/user went through api.AccountDeletionService: two
// implementations of one flow with different messages, no reason cap, and
// an ExecuteDeletion nothing ever called (R10-08). The dashboard, the
// management API and the user API now call Schedule / Cancel / Status here,
// and the daily runner in internal/api (deletion_runner.go) calls ListDue
// and EraseRows once the objects are gone from every backend.
//
// The grace period is the AWS shape (D-16): scheduling changes nothing but
// the users row — login, S3 and the management API keep working so the
// customer can export, migrate and cancel; the erasure happens on the date.
package account

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"
)

// GracePeriod is how long after a request the erasure runs.
const GracePeriod = 30 * 24 * time.Hour

// MaxReasonLen caps the free-text reason (R12-22: it was unbounded TEXT on
// the dashboard path).
const MaxReasonLen = 500

// StatusPending is users.status while a deletion is scheduled.
const StatusPending = "pending_deletion"

var (
	// ErrNoPendingDeletion is returned by Cancel when nothing is scheduled.
	ErrNoPendingDeletion = errors.New("no pending deletion to cancel")
	// ErrNotPending is returned by EraseRows when the user is no longer
	// scheduled (a cancel landed first) — the erasure must not proceed.
	ErrNotPending = errors.New("account is not pending deletion")
	// ErrObjectsRemain is returned by EraseRows while object_head_cache
	// still lists objects for the tenant: the runner's walk (stage b) has
	// to finish first, or bytes would be orphaned on a backend.
	ErrObjectsRemain = errors.New("objects remain in the index")
	// ErrNoDatabase is returned when the service has no database.
	ErrNoDatabase = errors.New("database unavailable")
)

// Status is what the APIs report about a user's deletion.
type Status struct {
	Scheduled   bool      `json:"scheduled"`
	ScheduledAt time.Time `json:"scheduled_at,omitempty"`
	Reason      string    `json:"reason,omitempty"`
}

// Service holds the state machine.
type Service struct {
	db     *sql.DB
	logger *zap.Logger
	now    func() time.Time
}

// NewService builds the service; a nil db makes every call fail with
// ErrNoDatabase (the binary runs without Postgres in local mode).
func NewService(db *sql.DB, logger *zap.Logger) *Service {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Service{db: db, logger: logger, now: time.Now}
}

// CapReason truncates a reason to MaxReasonLen runes (never mid-rune).
func CapReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) <= MaxReasonLen {
		return reason
	}
	runes := []rune(reason)
	return string(runes[:MaxReasonLen])
}

// Schedule marks the user for erasure GracePeriod from now. Scheduling
// twice returns the existing date (idempotent). The tenant id is logged
// only; the runner joins users → tenants by e-mail (the registration
// contract, R5-16) when the date comes.
func (s *Service) Schedule(ctx context.Context, userID, tenantID, reason string) (time.Time, error) {
	if s.db == nil {
		return time.Time{}, ErrNoDatabase
	}
	reason = CapReason(reason)
	if reason == "" {
		reason = "User requested deletion"
	}
	scheduledAt := s.now().Add(GracePeriod)

	// One statement: the WHERE keeps an existing schedule (two clicks, two
	// APIs, a dashboard and a script — the first date wins) and the
	// RETURNING reports it either way.
	var got time.Time
	err := s.db.QueryRowContext(ctx, `
		UPDATE users
		   SET deletion_scheduled_at = COALESCE(deletion_scheduled_at, $1),
		       deletion_reason       = COALESCE(deletion_reason, $2),
		       status                = CASE WHEN deletion_scheduled_at IS NULL THEN $3 ELSE status END
		 WHERE id = $4
		 RETURNING deletion_scheduled_at`,
		scheduledAt, reason, StatusPending, userID).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("schedule deletion: user %s not found", userID)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("schedule deletion: %w", err)
	}
	s.logger.Info("account deletion scheduled",
		zap.String("user_id", userID), zap.String("tenant_id", tenantID), zap.Time("scheduled_at", got))
	return got, nil
}

// Cancel clears a scheduled deletion. A cancel that arrives while the
// runner holds the users row (the final erase transaction) waits for it
// and then finds no row: the account is gone and ErrNoPendingDeletion is
// returned — the runner's per-batch re-check is what makes an earlier
// cancel win (see deletion_runner.go).
func (s *Service) Cancel(ctx context.Context, userID string) error {
	if s.db == nil {
		return ErrNoDatabase
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE users SET deletion_scheduled_at = NULL, deletion_reason = NULL, status = 'active'
		 WHERE id = $1 AND deletion_scheduled_at IS NOT NULL`, userID)
	if err != nil {
		return fmt.Errorf("cancel deletion: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoPendingDeletion
	}
	s.logger.Info("account deletion cancelled", zap.String("user_id", userID))
	return nil
}

// GetStatus reports whether and when the user's deletion is scheduled.
func (s *Service) GetStatus(ctx context.Context, userID string) (*Status, error) {
	if s.db == nil {
		return &Status{}, nil
	}
	var at sql.NullTime
	var reason sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT deletion_scheduled_at, deletion_reason FROM users WHERE id = $1`, userID).Scan(&at, &reason)
	if err != nil {
		return nil, fmt.Errorf("get deletion status: %w", err)
	}
	st := &Status{}
	if at.Valid {
		st.Scheduled = true
		st.ScheduledAt = at.Time
	}
	if reason.Valid {
		st.Reason = reason.String
	}
	return st, nil
}

// Due is one account whose date has passed.
type Due struct {
	UserID      string
	TenantID    string
	Email       string
	ScheduledAt time.Time
}

// ListDue returns the accounts scheduled before now, oldest first. A user
// whose tenant row is already gone (a crash after the tenants DELETE cannot
// happen — the erase is one transaction — but a hand-deleted tenant can) is
// returned with an empty TenantID so the runner can still erase the user.
func (s *Service) ListDue(ctx context.Context, now time.Time, limit int) ([]Due, error) {
	if s.db == nil {
		return nil, ErrNoDatabase
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, COALESCE(t.id, ''), u.email, u.deletion_scheduled_at
		  FROM users u
		  LEFT JOIN tenants t ON t.email = u.email
		 WHERE u.status = $1 AND u.deletion_scheduled_at < $2
		 ORDER BY u.deletion_scheduled_at
		 LIMIT $3`, StatusPending, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list due deletions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Due
	for rows.Next() {
		var d Due
		if err := rows.Scan(&d.UserID, &d.TenantID, &d.Email, &d.ScheduledAt); err != nil {
			return nil, fmt.Errorf("scan due deletion: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// StillDue re-reads the user under FOR UPDATE in a short transaction and
// reports whether the erasure may proceed: status still pending and the
// date still in the past. A cancel that committed since the select makes
// it false; one in flight waits for the lock and then clears the schedule
// — the runner re-checks before every batch and before the row erase.
func (s *Service) StillDue(ctx context.Context, userID string, now time.Time) (bool, error) {
	if s.db == nil {
		return false, ErrNoDatabase
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin due check: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	due, err := lockedStillDue(ctx, tx, userID, now)
	if err != nil {
		return false, err
	}
	return due, tx.Commit()
}

func lockedStillDue(ctx context.Context, tx *sql.Tx, userID string, now time.Time) (bool, error) {
	var status string
	var at sql.NullTime
	err := tx.QueryRowContext(ctx,
		`SELECT status, deletion_scheduled_at FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&status, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("re-read user %s: %w", userID, err)
	}
	return status == StatusPending && at.Valid && at.Time.Before(now), nil
}
