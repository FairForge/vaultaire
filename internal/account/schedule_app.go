// internal/account/schedule_app.go
//
// RP90-8 (Stage 1a Prompt 56 Part 0): ONE deletion of a self-serve app
// (WP-APP-1) for the API and the dashboard. The app and every end user under
// it are scheduled through scheduleTenantTx (ScheduleTenant's statement), and
// BOTH key kinds of the whole subtree — the app's and its users' management
// keys, every S3 key (the primary too: nothing of an app scheduled for
// erasure authenticates) — are revoked in the same transaction, so "its keys
// stopped now" is true the moment the answer is written. The STS tokens die
// with their parents (auth.LookupCredential). A schedule has no cancel path
// (D-16's 30-day grace is the erasure's, not a way back).
//
// It lives here, not in internal/auth, beside ScheduleTenant: this package's
// tests import auth, so auth importing account would be a cycle.
package account

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/audit"
)

// ErrNotAnApp: no app of that parent has the id.
var ErrNotAnApp = errors.New("not an app of this account")

// RevokedKey is one S3 key a schedule revoked (for the caller's key cache).
type RevokedKey struct{ TenantID, ID string }

// AppSchedule is what ScheduleApp did.
type AppSchedule struct {
	ScheduledAt time.Time
	EndUsers    []string
	S3Keys      []RevokedKey
	MgmtKeys    int64
}

// scheduleTenantTx is ScheduleTenant's statement on q.
func (s *Service) scheduleTenantTx(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, tenantID string) (time.Time, error) {
	var got time.Time
	err := q.QueryRowContext(ctx, `
		UPDATE tenants SET deletion_scheduled_at = COALESCE(deletion_scheduled_at, $1)
		 WHERE id = $2 AND parent_tenant_id IS NOT NULL
		 RETURNING deletion_scheduled_at`, s.now().Add(GracePeriod), tenantID).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("schedule deletion of tenant %s: %w", tenantID, ErrNotAChild)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("schedule deletion of tenant %s: %w", tenantID, err)
	}
	return got, nil
}

// ScheduleApp schedules parent's app appID and its end users for erasure
// and revokes every key of the subtree, in ONE transaction, with one
// tenant.deletion_scheduled audit row per end user (the app's own row is
// the caller's app.deletion_scheduled).
func (s *Service) ScheduleApp(ctx context.Context, parentID, appID string) (AppSchedule, error) {
	var out AppSchedule
	if s.db == nil {
		return out, ErrNoDatabase
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("schedule app %s: %w", appID, err)
	}
	defer func() { _ = tx.Rollback() }()
	var isApp bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1 AND parent_tenant_id = $2 AND child_kind = 'app' FOR UPDATE)`,
		appID, parentID).Scan(&isApp); err != nil {
		return out, fmt.Errorf("schedule app %s: %w", appID, err)
	}
	if !isApp {
		return out, ErrNotAnApp
	}
	if out.ScheduledAt, err = s.scheduleTenantTx(ctx, tx, appID); err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM tenants WHERE parent_tenant_id = $1 ORDER BY id`, appID)
	if err != nil {
		return out, fmt.Errorf("schedule app %s: its users: %w", appID, err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return out, fmt.Errorf("schedule app %s: its users: %w", appID, err)
		}
		out.EndUsers = append(out.EndUsers, id)
	}
	if err := rows.Close(); err != nil {
		return out, fmt.Errorf("schedule app %s: its users: %w", appID, err)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("schedule app %s: its users: %w", appID, err)
	}
	for _, u := range out.EndUsers {
		at, err := s.scheduleTenantTx(ctx, tx, u)
		if err != nil {
			return out, err
		}
		if err := audit.RecordTx(ctx, tx, audit.Entry{TenantID: u, Action: "tenant.deletion_scheduled", Resource: "tenant:" + u,
			Metadata: map[string]any{"scheduled_at": at, "with_app": appID}}); err != nil {
			return out, fmt.Errorf("schedule app %s: %w", appID, err)
		}
	}
	// The subtree: the app, its users, and any sandbox of either.
	const subtree = `(SELECT id FROM tenants WHERE id = $1 OR parent_tenant_id = $1
		UNION SELECT id FROM tenants WHERE sandbox_of IN (SELECT id FROM tenants WHERE id = $1 OR parent_tenant_id = $1))`
	res, err := tx.ExecContext(ctx, `UPDATE management_keys SET revoked_at = NOW() WHERE revoked_at IS NULL AND tenant_id IN `+subtree, appID)
	if err != nil {
		return out, fmt.Errorf("schedule app %s: revoke management keys: %w", appID, err)
	}
	out.MgmtKeys, _ = res.RowsAffected()
	keys, err := tx.QueryContext(ctx, `UPDATE api_keys SET revoked_at = NOW() WHERE revoked_at IS NULL AND tenant_id IN `+subtree+`
		RETURNING tenant_id, id::text`, appID)
	if err != nil {
		return out, fmt.Errorf("schedule app %s: revoke S3 keys: %w", appID, err)
	}
	for keys.Next() {
		var k RevokedKey
		if err := keys.Scan(&k.TenantID, &k.ID); err != nil {
			_ = keys.Close()
			return out, fmt.Errorf("schedule app %s: revoke S3 keys: %w", appID, err)
		}
		out.S3Keys = append(out.S3Keys, k)
	}
	if err := keys.Close(); err != nil {
		return out, fmt.Errorf("schedule app %s: revoke S3 keys: %w", appID, err)
	}
	if err := keys.Err(); err != nil {
		return out, fmt.Errorf("schedule app %s: revoke S3 keys: %w", appID, err)
	}
	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("schedule app %s: %w", appID, err)
	}
	s.logger.Info("app deletion scheduled", zap.String("tenant_id", appID), zap.Int("end_users", len(out.EndUsers)),
		zap.Int("s3_keys_revoked", len(out.S3Keys)), zap.Int64("management_keys_revoked", out.MgmtKeys), zap.Time("scheduled_at", out.ScheduledAt))
	return out, nil
}
