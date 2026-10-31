// internal/auth/approvals.go
//
// Approvals (WP-AGENT-1 PR B; F-41; migration 122): what could lose or
// expose data is never in a default agent scope. An agent grant's key that
// reaches such an act is refused with a pending approval naming ONE action
// on ONE resource; the person decides it (a fresh TOTP code for a
// destructive act); an approved one is used ONCE, within 24 h of its
// request.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ApprovalAction is an act an agent asks the person for.
type ApprovalAction struct {
	// Says is the act in the person's words ("delete the bucket").
	Says string
	// Destructive: the approval needs a fresh TOTP code.
	Destructive bool
	// Role is the action the deciding member must be allowed to do
	// themselves.
	Role Action
}

// ApprovalActions are the acts an agent asks for (F-41's list: the rest of
// it — making anything public, Object Lock, exports, the erasure, members —
// no management key reaches at all).
var ApprovalActions = map[string]ApprovalAction{
	"bucket.delete": {Says: "delete the bucket", Destructive: true, Role: ActionBucketWrite},
	"key.revoke":    {Says: "revoke the key", Destructive: true, Role: ActionKeyWrite},
}

var (
	// ErrApprovalNotFound: no approval of the tenant has that id.
	ErrApprovalNotFound = errors.New("approval not found")
	// ErrApprovalDecided: the approval was approved or denied already.
	ErrApprovalDecided = errors.New("approval already decided")
	// ErrApprovalExpired: its 24 hours have passed.
	ErrApprovalExpired = errors.New("approval expired")
)

// Approval is one row as the API answers it.
type Approval struct {
	ID          string     `json:"id"`
	GrantID     string     `json:"grant_id"`
	AgentName   string     `json:"agent_name"`
	Action      string     `json:"action"`
	Resource    string     `json:"resource"`
	Says        string     `json:"says"`
	Destructive bool       `json:"destructive"`
	RequestedAt time.Time  `json:"requested_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	DecidedAt   *time.Time `json:"decided_at"`
	DecidedBy   string     `json:"decided_by,omitempty"`
	Decision    string     `json:"decision,omitempty"`
	UsedAt      *time.Time `json:"used_at"`
}

const approvalColumns = `a.id::text, a.grant_id::text, COALESCE(g.agent_name, ''), a.action, a.resource, a.requested_at, a.expires_at,
	a.decided_at, COALESCE(a.decided_by::text, ''), COALESCE(a.decision, ''), a.used_at`

func scanApproval(sc interface{ Scan(...any) error }) (*Approval, error) {
	var a Approval
	var decided, used sql.NullTime
	if err := sc.Scan(&a.ID, &a.GrantID, &a.AgentName, &a.Action, &a.Resource, &a.RequestedAt, &a.ExpiresAt,
		&decided, &a.DecidedBy, &a.Decision, &used); err != nil {
		return nil, err
	}
	if decided.Valid {
		a.DecidedAt = &decided.Time
	}
	if used.Valid {
		a.UsedAt = &used.Time
	}
	act := ApprovalActions[a.Action]
	a.Says, a.Destructive = act.Says, act.Destructive
	return &a, nil
}

// ConsumeApproval spends the grant's approved, unused, unexpired approval
// of action on resource (true), in ONE statement: two requests never both
// spend it.
func ConsumeApproval(ctx context.Context, db *sql.DB, grantID, action, resource string) (string, bool, error) {
	var id string
	err := db.QueryRowContext(ctx, `
		UPDATE approvals SET used_at = NOW()
		 WHERE id = (SELECT id FROM approvals
		              WHERE grant_id = $1::uuid AND action = $2 AND resource = $3 AND decision = 'approved'
		                AND used_at IS NULL AND expires_at > NOW()
		              ORDER BY decided_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		   AND used_at IS NULL
		RETURNING id::text`, grantID, action, resource).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("spend an approval of %s: %w", action, err)
	}
	return id, true, nil
}

// RequestApproval is the grant's pending approval of action on resource:
// the one already asked (and still within its 24 h), else a new one.
func RequestApproval(ctx context.Context, db *sql.DB, tenantID, grantID, action, resource string) (*Approval, error) {
	if _, ok := ApprovalActions[action]; !ok {
		return nil, fmt.Errorf("request an approval: unknown action %q", action)
	}
	var id string
	err := db.QueryRowContext(ctx, `SELECT id::text FROM approvals
		 WHERE grant_id = $1::uuid AND action = $2 AND resource = $3 AND decided_at IS NULL AND expires_at > NOW()
		 ORDER BY requested_at DESC LIMIT 1`, grantID, action, resource).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = db.QueryRowContext(ctx, `INSERT INTO approvals (tenant_id, grant_id, action, resource) VALUES ($1, $2::uuid, $3, $4)
			RETURNING id::text`, tenantID, grantID, action, resource).Scan(&id)
	}
	if err != nil {
		return nil, fmt.Errorf("request an approval of %s: %w", action, err)
	}
	return GetApproval(ctx, db, tenantID, id)
}

// GetApproval reads the tenant's approval id.
func GetApproval(ctx context.Context, db *sql.DB, tenantID, id string) (*Approval, error) {
	if !isUUID(id) {
		return nil, ErrApprovalNotFound
	}
	a, err := scanApproval(db.QueryRowContext(ctx, `SELECT `+approvalColumns+`
		  FROM approvals a LEFT JOIN agent_grants g ON g.id = a.grant_id WHERE a.tenant_id = $1 AND a.id = $2::uuid`, tenantID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrApprovalNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read approval %s: %w", id, err)
	}
	return a, nil
}

// ListApprovals lists the tenant's approvals of the last 30 days, the
// pending ones first (pendingOnly: those only).
func ListApprovals(ctx context.Context, db *sql.DB, tenantID string, pendingOnly bool) ([]*Approval, error) {
	q := `SELECT ` + approvalColumns + ` FROM approvals a LEFT JOIN agent_grants g ON g.id = a.grant_id
		 WHERE a.tenant_id = $1 AND a.requested_at > NOW() - INTERVAL '30 days'`
	if pendingOnly {
		q += ` AND a.decided_at IS NULL AND a.expires_at > NOW()`
	}
	rows, err := db.QueryContext(ctx, q+` ORDER BY (a.decided_at IS NULL AND a.expires_at > NOW()) DESC, a.requested_at DESC LIMIT 200`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list the approvals of %s: %w", tenantID, err)
	}
	defer func() { _ = rows.Close() }()
	out := []*Approval{}
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, fmt.Errorf("list the approvals of %s: %w", tenantID, err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DecideApproval records the person's decision ("approved" | "denied") on
// a pending, unexpired approval. The caller has checked the person, their
// role and (Destructive) their TOTP code.
func DecideApproval(ctx context.Context, db *sql.DB, tenantID, id, userID, decision string) (*Approval, error) {
	if decision != "approved" && decision != "denied" {
		return nil, fmt.Errorf("decide approval %s: decision %q", id, decision)
	}
	a, err := GetApproval(ctx, db, tenantID, id)
	if err != nil {
		return nil, err
	}
	res, err := db.ExecContext(ctx, `UPDATE approvals SET decided_at = NOW(), decided_by = $3::uuid, decision = $4
		 WHERE tenant_id = $1 AND id = $2::uuid AND decided_at IS NULL AND expires_at > NOW()`, tenantID, id, userID, decision)
	if err != nil {
		return nil, fmt.Errorf("decide approval %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if a.DecidedAt != nil {
			return nil, ErrApprovalDecided
		}
		return nil, ErrApprovalExpired
	}
	return GetApproval(ctx, db, tenantID, id)
}
