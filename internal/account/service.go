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
	// ErrChildrenRemain is returned by EraseRows while the tenant (a
	// platform) still has child tenants: they are erased first.
	ErrChildrenRemain = errors.New("child tenants remain")
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

// ErrNotAChild: ScheduleTenant was asked for a tenant that is not a
// platform's child (a user's account is scheduled through its user).
var ErrNotAChild = errors.New("not a child tenant")

// ScheduleTenant marks a tenant WITHOUT a user — a platform's child
// (tenants.parent_tenant_id, WP-PLAT-1) — for erasure GracePeriod from now,
// on tenants.deletion_scheduled_at (117). Scheduling twice returns the
// existing date. ErrNotAChild for any other tenant (a missing one too).
func (s *Service) ScheduleTenant(ctx context.Context, tenantID string) (time.Time, error) {
	if s.db == nil {
		return time.Time{}, ErrNoDatabase
	}
	var got time.Time
	err := s.db.QueryRowContext(ctx, `
		UPDATE tenants SET deletion_scheduled_at = COALESCE(deletion_scheduled_at, $1)
		 WHERE id = $2 AND parent_tenant_id IS NOT NULL
		 RETURNING deletion_scheduled_at`, s.now().Add(GracePeriod), tenantID).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("schedule deletion of tenant %s: %w", tenantID, ErrNotAChild)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("schedule deletion of tenant %s: %w", tenantID, err)
	}
	s.logger.Info("child tenant deletion scheduled", zap.String("tenant_id", tenantID), zap.Time("scheduled_at", got))
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
	// Child: a platform's child tenant (WP-PLAT-1) — no user: its
	// schedule is tenants.deletion_scheduled_at, and UserID is "".
	Child bool
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A platform's children whose own date has passed (WP-PLAT-1): erased
	// alone, by the same runner. A child of a platform due itself is
	// erased by the platform's cascade.
	if left := limit - len(out); left > 0 {
		crows, err := s.db.QueryContext(ctx, `
			SELECT t.id, t.email, t.deletion_scheduled_at
			  FROM tenants t
			 WHERE t.parent_tenant_id IS NOT NULL AND t.deletion_scheduled_at < $1
			 ORDER BY t.deletion_scheduled_at
			 LIMIT $2`, now, left)
		if err != nil {
			return nil, fmt.Errorf("list due child tenants: %w", err)
		}
		defer func() { _ = crows.Close() }()
		for crows.Next() {
			d := Due{Child: true}
			if err := crows.Scan(&d.TenantID, &d.Email, &d.ScheduledAt); err != nil {
				return nil, fmt.Errorf("scan due child tenant: %w", err)
			}
			out = append(out, d)
		}
		if err := crows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// StillDueTenant is StillDue for a child tenant (no user): the tenants
// row under FOR UPDATE in a short transaction, its date passed.
func (s *Service) StillDueTenant(ctx context.Context, tenantID string, now time.Time) (bool, error) {
	if s.db == nil {
		return false, ErrNoDatabase
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin due check: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	due, err := lockedTenantStillDue(ctx, tx, tenantID, now)
	if err != nil {
		return false, err
	}
	return due, tx.Commit()
}

// ScheduleChildrenNow makes every child of parent due now (the cascade of
// the parent's erasure, WP-PLAT-1): an earlier date of its own is kept.
// Returns the children's ids.
func (s *Service) ScheduleChildrenNow(ctx context.Context, parent string, now time.Time) ([]string, error) {
	if s.db == nil {
		return nil, ErrNoDatabase
	}
	// A sponsored child is never erased with its sponsor (DetachSponsored).
	rows, err := s.db.QueryContext(ctx, `
		UPDATE tenants SET deletion_scheduled_at = LEAST(COALESCE(deletion_scheduled_at, $2), $2)
		 WHERE parent_tenant_id = $1 AND COALESCE(child_kind, 'managed') <> 'sponsored'
		 RETURNING id`, parent, now)
	if err != nil {
		return nil, fmt.Errorf("schedule the children of %s: %w", parent, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func lockedTenantStillDue(ctx context.Context, tx *sql.Tx, tenantID string, now time.Time) (bool, error) {
	var at sql.NullTime
	err := tx.QueryRowContext(ctx,
		`SELECT deletion_scheduled_at FROM tenants WHERE id = $1 AND parent_tenant_id IS NOT NULL FOR UPDATE`, tenantID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("re-read tenant %s: %w", tenantID, err)
	}
	return at.Valid && !at.Time.After(now), nil
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

// DueShared reads the user under FOR SHARE inside the caller's transaction
// and reports whether the row exists and whether its erasure is due (status
// pending and the date passed). A writer holding FOR UPDATE — StillDue, the
// erase transaction — waits for the caller's transaction, so what the caller
// commits beside this read (an audit row) is committed against the state it
// read. The recovery-token mint uses it (RP54-1): no realm token is issued
// for an account whose erasure is owed, and the token's trail commits with
// the check.
func DueShared(ctx context.Context, tx *sql.Tx, userID string, now time.Time) (exists, due bool, err error) {
	var status string
	var at sql.NullTime
	err = tx.QueryRowContext(ctx,
		`SELECT status, deletion_scheduled_at FROM users WHERE id = $1 FOR SHARE`, userID).Scan(&status, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("read user %s: %w", userID, err)
	}
	return true, status == StatusPending && at.Valid && at.Time.Before(now), nil
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

// DetachSponsored detaches every SPONSORED child of parent (WP-ACCT-1,
// docs/ACCOUNTS.md: a person's own account a sponsor pays for — never
// erased with the sponsor): each, in its own transaction, leaves the pool —
// its bytes leave the root's pool counter (the root's quota row locked
// first, the ledger's order), it becomes its own root (pool_root_id), its
// pool's counting floor rows (limit 0) go, a cap of 0 becomes freeLimit (a
// tier of its own) — and its parent edge and kind are cleared, with an
// account.sponsor_detached audit row on it. Its data, keys and sign-in are
// untouched. Returns the detached ids.
func (s *Service) DetachSponsored(ctx context.Context, parent string, freeLimit int64) ([]string, error) {
	if s.db == nil {
		return nil, ErrNoDatabase
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM tenants WHERE parent_tenant_id = $1 AND child_kind = 'sponsored' ORDER BY id`, parent)
	if err != nil {
		return nil, fmt.Errorf("list the sponsored children of %s: %w", parent, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("list the sponsored children of %s: %w", parent, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("list the sponsored children of %s: %w", parent, err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the sponsored children of %s: %w", parent, err)
	}
	var done []string
	for _, id := range ids {
		if err := s.detachOne(ctx, id, freeLimit); err != nil {
			return done, err
		}
		done = append(done, id)
	}
	return done, nil
}

func (s *Service) detachOne(ctx context.Context, child string, freeLimit int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("detach %s: %w", child, err)
	}
	defer func() { _ = tx.Rollback() }()
	var root string
	if err := tx.QueryRowContext(ctx, `SELECT pool_root_id FROM tenants WHERE id = $1 FOR UPDATE`, child).Scan(&root); err != nil {
		return fmt.Errorf("detach %s: %w", child, err)
	}
	// The root's quota row first, then the child's (usage/ledger_tx.go).
	if root != child {
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM tenant_quotas WHERE tenant_id = $1 FOR UPDATE`, root); err != nil {
			return fmt.Errorf("detach %s: lock the pool: %w", child, err)
		}
	}
	var used int64
	err = tx.QueryRowContext(ctx, `SELECT storage_used_bytes FROM tenant_quotas WHERE tenant_id = $1 FOR UPDATE`, child).Scan(&used)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("detach %s: %w", child, err)
	default:
		if root != child {
			if _, err := tx.ExecContext(ctx, `UPDATE tenant_quotas SET pool_used_bytes = pool_used_bytes - $2, updated_at = NOW() WHERE tenant_id = $1`,
				root, used); err != nil {
				return fmt.Errorf("detach %s: the pool's counter: %w", child, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE tenant_quotas
			   SET pool_used_bytes = storage_used_bytes,
			       storage_limit_bytes = CASE WHEN storage_limit_bytes = 0 THEN $2 ELSE storage_limit_bytes END,
			       egress_cap_bytes = 0, updated_at = NOW()
			 WHERE tenant_id = $1`, child, freeLimit); err != nil {
			return fmt.Errorf("detach %s: its own pool: %w", child, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tenant_floor_quotas WHERE tenant_id = $1 AND storage_limit_bytes = 0`, child); err != nil {
			return fmt.Errorf("detach %s: the pool's floor rows: %w", child, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tenants SET parent_tenant_id = NULL, child_kind = NULL, pool_root_id = id WHERE id = $1`, child); err != nil {
		return fmt.Errorf("detach %s: %w", child, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_logs (tenant_id, event_type, action, resource, result, severity, metadata)
		VALUES ($1, 'account', 'account.sponsor_detached', $2, 'success', 'info',
		        '{"reason": "the sponsor''s account was erased; this account keeps its data and pays for itself"}'::jsonb)`, child, "tenant:"+child); err != nil {
		return fmt.Errorf("detach %s: audit: %w", child, err)
	}
	return tx.Commit()
}

// ErrStillAMember refuses ScheduleSignIn for a user who owns a tenant or is
// a member of one.
var ErrStillAMember = errors.New("the sign-in owns an account or is a member of one")

// ScheduleSignIn schedules the erasure of a sign-in that owns no tenant and
// is a member of none (RB10-1, WP-ACCT-1): Schedule, after checking under
// the users row's lock. On the date the runner erases the user rows only.
func (s *Service) ScheduleSignIn(ctx context.Context, userID, reason string) (time.Time, error) {
	if s.db == nil {
		return time.Time{}, ErrNoDatabase
	}
	var member bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM tenant_members m WHERE m.user_id::text = $1)
		    OR EXISTS (SELECT 1 FROM users u JOIN tenants t ON lower(t.email) = lower(u.email) WHERE u.id::text = $1)`,
		userID).Scan(&member); err != nil {
		return time.Time{}, fmt.Errorf("schedule a sign-in's deletion: %w", err)
	}
	if member {
		return time.Time{}, ErrStillAMember
	}
	return s.Schedule(ctx, userID, "", reason)
}
