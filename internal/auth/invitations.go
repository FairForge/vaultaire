// internal/auth/invitations.go
//
// Invitations (WP-TEAM-1 PR B, Stage 1a Prompt 52; migration 118). An
// owner or admin invites an e-mail address to the tenant with a role; the
// address receives a single-use link that lives 7 days. Only the SHA-256
// of the link's token is stored. Accepting it as the signed-in user of that
// address makes the member row; a person with no account registers through
// the link (RegisterInvitedUser) and gets NO tenant of their own — the
// registration's tenant step is the inviting tenant's member row.
//
// `kind` is 'member' here; WP-FAMILY-1 adds 'family' (docs/ACCOUNTS.md: a
// family is a pool of personal tenants, never one tenant with members).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// InvitationTTL is how long an invitation's link is good for.
const InvitationTTL = 7 * 24 * time.Hour

// The invitation caps (RP84-3): a tenant holds at most MaxPendingInvitations
// usable invitations, and sends at most MaxInvitationsPerDay invitation
// mails in any 24 hours — counted from rows (the pending invitations; the
// member.invited audit rows, written in the invitation's own transaction),
// so both slots and a restart agree. One account could otherwise spend the
// sending quota every reset, verification and receipt mail shares.
const (
	MaxPendingInvitations = 20
	MaxInvitationsPerDay  = 50
)

// The refusals of the invitation flow.
var (
	ErrInvitationNotFound      = errors.New("no such invitation")
	ErrInvitationExpired       = errors.New("the invitation has expired")
	ErrInvitationUsed          = errors.New("the invitation has already been used")
	ErrInvitationEmailMismatch = errors.New("the invitation was sent to another address")
	ErrAlreadyMember           = errors.New("the address is already a member")
	ErrInviteRole              = errors.New("an invitation's role is admin, developer or viewer")
	ErrOwnerMember             = errors.New("the owner cannot be removed or given another role")
	ErrInvitationRateLimited   = errors.New("too many invitations: at most 20 pending and 50 sent a day per account")
)

// Invitation is one tenant_invitations row (never its token).
type Invitation struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"tenant_id"`
	Kind       string     `json:"kind"`
	Email      string     `json:"email"`
	Role       Role       `json:"role"`
	InvitedBy  string     `json:"invited_by,omitempty"`
	ExpiresAt  time.Time  `json:"expires_at"`
	AcceptedAt *time.Time `json:"accepted_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Member is one tenant_members row with the member's e-mail.
type Member struct {
	UserID     string    `json:"user_id"`
	Email      string    `json:"email"`
	Role       Role      `json:"role"`
	InvitedBy  string    `json:"invited_by,omitempty"`
	AcceptedAt time.Time `json:"accepted_at"`
}

// HashInvitationToken is what the row stores of a token.
func HashInvitationToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// invitableRole reports whether an invitation may carry role.
func invitableRole(role Role) bool {
	return role == RoleAdmin || role == RoleDeveloper || role == RoleViewer
}

// CreateInvitation records an invitation of email to tenantID with role and
// returns it with its token (shown once: in the e-mailed link). An address
// that is already a member (in any case) is ErrAlreadyMember; past the caps
// it is ErrInvitationRateLimited. A pending invitation of the same address
// is REPLACED (RP84-5: one pending invitation per tenant and address — the
// first link dies, re-inviting is the resend). One transaction under the
// tenant's invitation lock: the checks, the row, and its member.invited
// audit row (the day's count).
func CreateInvitation(ctx context.Context, db *sql.DB, tenantID, invitedBy, email string, role Role, now time.Time) (Invitation, string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") || len(email) > 254 || strings.ContainsAny(email, "\r\n") {
		return Invitation{}, "", fmt.Errorf("invalid email address")
	}
	if !invitableRole(role) {
		return Invitation{}, "", ErrInviteRole
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Invitation{}, "", fmt.Errorf("invite %s: %w", email, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('tenant-invitations'), hashtext($1))`, tenantID); err != nil {
		return Invitation{}, "", fmt.Errorf("invite %s: %w", email, err)
	}
	var member bool
	var pending, sentToday int
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM tenant_members m JOIN users u ON u.id = m.user_id
		                WHERE m.tenant_id = $1 AND lower(u.email) = $2)
		    OR EXISTS (SELECT 1 FROM tenants t WHERE t.id = $1 AND lower(t.email) = $2),
		       (SELECT COUNT(*) FROM tenant_invitations i
		         WHERE i.tenant_id = $1 AND i.accepted_at IS NULL AND i.expires_at > $3 AND lower(i.email) <> $2),
		       (SELECT COUNT(*) FROM audit_logs a
		         WHERE a.tenant_id = $1 AND a.event_type = 'member' AND a.action = 'member.invited'
		           AND a.timestamp > $3::timestamptz - INTERVAL '24 hours')`,
		tenantID, email, now).Scan(&member, &pending, &sentToday); err != nil {
		return Invitation{}, "", fmt.Errorf("invite %s: %w", email, err)
	}
	if member {
		return Invitation{}, "", ErrAlreadyMember
	}
	if pending >= MaxPendingInvitations || sentToday >= MaxInvitationsPerDay {
		return Invitation{}, "", ErrInvitationRateLimited
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Invitation{}, "", fmt.Errorf("invite %s: %w", email, err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	inv := Invitation{ID: uuid.NewString(), TenantID: tenantID, Kind: "member", Email: email, Role: role,
		InvitedBy: invitedBy, CreatedAt: now.UTC(), ExpiresAt: now.UTC().Add(InvitationTTL)}
	var by any
	if _, err := uuid.Parse(invitedBy); err == nil {
		by = invitedBy
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM tenant_invitations WHERE tenant_id = $1 AND lower(email) = $2 AND accepted_at IS NULL`, tenantID, email); err != nil {
		return Invitation{}, "", fmt.Errorf("invite %s: replace the pending one: %w", email, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenant_invitations (id, tenant_id, kind, email, role, token_hash, invited_by, expires_at, created_at)
		VALUES ($1, $2, 'member', $3, $4, $5, $6, $7, $8)`,
		inv.ID, tenantID, email, string(role), HashInvitationToken(token), by, inv.ExpiresAt, inv.CreatedAt); err != nil {
		return Invitation{}, "", fmt.Errorf("invite %s: %w", email, err)
	}
	if err := audit.RecordTx(ctx, tx, audit.Entry{TenantID: tenantID, Action: "member.invited", Resource: "invitation:" + inv.ID,
		Metadata: map[string]any{"email": inv.Email, "role": string(inv.Role)}}); err != nil {
		return Invitation{}, "", fmt.Errorf("invite %s: %w", email, err)
	}
	if err := tx.Commit(); err != nil {
		return Invitation{}, "", fmt.Errorf("invite %s: %w", email, err)
	}
	return inv, token, nil
}

const invitationColumns = `id, tenant_id, kind, email, role, COALESCE(invited_by::text, ''), expires_at, accepted_at, created_at`

func scanInvitation(row interface{ Scan(...any) error }) (Invitation, error) {
	var inv Invitation
	var role string
	var accepted sql.NullTime
	if err := row.Scan(&inv.ID, &inv.TenantID, &inv.Kind, &inv.Email, &role, &inv.InvitedBy, &inv.ExpiresAt, &accepted, &inv.CreatedAt); err != nil {
		return Invitation{}, err
	}
	inv.Role = Role(role)
	if accepted.Valid {
		inv.AcceptedAt = &accepted.Time
	}
	return inv, nil
}

// checkInvitation is the one rule of a usable invitation at now.
func checkInvitation(inv Invitation, now time.Time) error {
	switch {
	case inv.Kind != "member":
		return ErrInvitationNotFound // WP-FAMILY-1's kind is not accepted here
	case inv.AcceptedAt != nil:
		return ErrInvitationUsed
	case !now.Before(inv.ExpiresAt):
		return ErrInvitationExpired
	}
	return nil
}

// InvitationByToken reads the invitation a link's token names, checked as
// usable at now (the accept page shows it before anyone signs in).
func InvitationByToken(ctx context.Context, db *sql.DB, token string, now time.Time) (Invitation, error) {
	inv, err := scanInvitation(db.QueryRowContext(ctx,
		`SELECT `+invitationColumns+` FROM tenant_invitations WHERE token_hash = $1`, HashInvitationToken(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, ErrInvitationNotFound
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("read invitation: %w", err)
	}
	return inv, checkInvitation(inv, now)
}

// acceptTx spends the invitation inside tx for userID (whose e-mail must be
// the invited address) and writes the member row and its audit row.
func acceptTx(ctx context.Context, tx *sql.Tx, token, userID, userEmail string, now time.Time) (Invitation, error) {
	inv, err := scanInvitation(tx.QueryRowContext(ctx,
		`SELECT `+invitationColumns+` FROM tenant_invitations WHERE token_hash = $1 FOR UPDATE`, HashInvitationToken(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, ErrInvitationNotFound
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("accept invitation: %w", err)
	}
	if err := checkInvitation(inv, now); err != nil {
		return inv, err
	}
	if !strings.EqualFold(strings.TrimSpace(userEmail), inv.Email) {
		return inv, ErrInvitationEmailMismatch
	}
	var by any
	if inv.InvitedBy != "" {
		by = inv.InvitedBy
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenant_members (tenant_id, user_id, role, invited_by, accepted_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, user_id) DO NOTHING`, inv.TenantID, userID, string(inv.Role), by, now); err != nil {
		return inv, fmt.Errorf("accept invitation: member: %w", err)
	}
	// The role the person HAS (RP84-5): an accept that found a member row
	// changed nothing, and answers that row's role.
	var have string
	if err := tx.QueryRowContext(ctx, `SELECT role FROM tenant_members WHERE tenant_id = $1 AND user_id = $2`,
		inv.TenantID, userID).Scan(&have); err != nil {
		return inv, fmt.Errorf("accept invitation: member: %w", err)
	}
	inv.Role = Role(have)
	if _, err := tx.ExecContext(ctx, `UPDATE tenant_invitations SET accepted_at = $2 WHERE id = $1`, inv.ID, now); err != nil {
		return inv, fmt.Errorf("accept invitation: %w", err)
	}
	at := now
	inv.AcceptedAt = &at
	if err := audit.RecordTx(ctx, tx, audit.Entry{UserID: userID, TenantID: inv.TenantID, Action: "member.joined",
		Resource: "user:" + userID, Metadata: map[string]any{"role": string(inv.Role), "invitation": inv.ID, "invited_by": inv.InvitedBy}}); err != nil {
		return inv, fmt.Errorf("accept invitation: %w", err)
	}
	return inv, nil
}

// AcceptInvitation spends the invitation for the signed-in user userID: one
// transaction (the row locked, single use), the member row, the audit row.
func AcceptInvitation(ctx context.Context, db *sql.DB, token, userID, userEmail string, now time.Time) (Invitation, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Invitation{}, fmt.Errorf("accept invitation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	inv, err := acceptTx(ctx, tx, token, userID, userEmail, now)
	if err != nil {
		return inv, err
	}
	if err := tx.Commit(); err != nil {
		return inv, fmt.Errorf("accept invitation: %w", err)
	}
	return inv, nil
}

// RegisterInvitedUser makes an account for the invited address through the
// link: a users row and the member row, in one transaction with the
// invitation spent — and NO tenant of its own (the inviting tenant is its
// tenant). Closed signups do not stop it: it adds a person to an existing
// account, no storage. The address is verified by the link. An address
// that already has an account signs in and accepts (ErrUserExists).
func (a *AuthService) RegisterInvitedUser(ctx context.Context, token, password string, now time.Time) (*User, Invitation, error) {
	if a.sqlDB == nil {
		return nil, Invitation{}, fmt.Errorf("register invited user: no database")
	}
	if len(password) < MinPasswordLength {
		return nil, Invitation{}, ErrPasswordTooShort
	}
	inv, err := InvitationByToken(ctx, a.sqlDB, token, now)
	if err != nil {
		return nil, inv, err
	}
	if existing, err := a.userByEmail(ctx, inv.Email); err != nil {
		return nil, inv, fmt.Errorf("register invited user: %w", err)
	} else if existing != nil {
		return nil, inv, ErrUserExists
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, inv, fmt.Errorf("hash password: %w", err)
	}
	// No TenantID: the person has no tenant of its own (RP84-2 — caching
	// the inviting tenant kept it after a removal); the tenant it acts on is
	// its JWT's or its session's, by membership.
	user := &User{ID: uuid.NewString(), Email: inv.Email, PasswordHash: string(hash), EmailVerified: true,
		CreatedAt: now, UpdatedAt: now}
	tx, err := a.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, inv, fmt.Errorf("register invited user: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, email_verified, created_at, updated_at)
		VALUES ($1, $2, $3, TRUE, $4, $4)
		ON CONFLICT (email) DO NOTHING`, user.ID, user.Email, user.PasswordHash, now)
	if err != nil {
		return nil, inv, fmt.Errorf("register invited user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, inv, ErrUserExists
	}
	if inv, err = acceptTx(ctx, tx, token, user.ID, user.Email, now); err != nil {
		return nil, inv, err
	}
	if err := tx.Commit(); err != nil {
		return nil, inv, fmt.Errorf("register invited user: %w", err)
	}
	a.cacheMu.Lock()
	a.users[user.Email] = user
	a.userIndex[user.ID] = user
	a.cacheMu.Unlock()
	return user, inv, nil
}

// ListMembers lists the tenant's members, the owner first. A tenant whose
// registering user has no row yet (the deploy overlap) lists it as owner.
func ListMembers(ctx context.Context, db *sql.DB, tenantID string) ([]Member, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.user_id::text, u.email, m.role, COALESCE(m.invited_by::text, ''), m.accepted_at
		  FROM tenant_members m JOIN users u ON u.id = m.user_id
		 WHERE m.tenant_id = $1
		UNION ALL
		SELECT u.id::text, u.email, 'owner', '', u.created_at
		  FROM tenants t JOIN users u ON u.email = t.email
		 WHERE t.id = $1 AND t.parent_tenant_id IS NULL AND t.sandbox_of IS NULL
		   AND NOT EXISTS (SELECT 1 FROM tenant_members x WHERE x.tenant_id = t.id AND x.role = 'owner')
		 ORDER BY 5`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list members of %s: %w", tenantID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Member
	for rows.Next() {
		var m Member
		var role string
		if err := rows.Scan(&m.UserID, &m.Email, &role, &m.InvitedBy, &m.AcceptedAt); err != nil {
			return nil, fmt.Errorf("list members of %s: %w", tenantID, err)
		}
		m.Role = Role(role)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListInvitations lists the tenant's invitations not yet accepted, newest
// first (expired ones included: the page shows them as expired).
func ListInvitations(ctx context.Context, db *sql.DB, tenantID string) ([]Invitation, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+invitationColumns+` FROM tenant_invitations
		 WHERE tenant_id = $1 AND accepted_at IS NULL ORDER BY created_at DESC, id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list invitations of %s: %w", tenantID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Invitation
	for rows.Next() {
		inv, err := scanInvitation(rows)
		if err != nil {
			return nil, fmt.Errorf("list invitations of %s: %w", tenantID, err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// RevokeInvitation deletes a pending invitation of the tenant.
func RevokeInvitation(ctx context.Context, db *sql.DB, tenantID, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return ErrInvitationNotFound
	}
	res, err := db.ExecContext(ctx, `DELETE FROM tenant_invitations WHERE tenant_id = $1 AND id = $2 AND accepted_at IS NULL`, tenantID, id)
	if err != nil {
		return fmt.Errorf("revoke invitation %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInvitationNotFound
	}
	return nil
}

// targetRole is the current role of userID on tenantID, for a change.
func targetRole(ctx context.Context, db *sql.DB, tenantID, userID string) (Role, error) {
	if _, err := uuid.Parse(userID); err != nil {
		return "", ErrNotMember
	}
	return MemberRole(ctx, db, tenantID, userID)
}

// SetMemberRole changes a member's role. The owner keeps its role, and no
// one is made owner here.
func SetMemberRole(ctx context.Context, db *sql.DB, tenantID, userID string, role Role) error {
	if !invitableRole(role) {
		return ErrInviteRole
	}
	cur, err := targetRole(ctx, db, tenantID, userID)
	if err != nil {
		return err
	}
	if cur == RoleOwner {
		return ErrOwnerMember
	}
	if _, err := db.ExecContext(ctx, `UPDATE tenant_members SET role = $3 WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, userID, string(role)); err != nil {
		return fmt.Errorf("set role of %s on %s: %w", userID, tenantID, err)
	}
	return nil
}

// RemoveMember removes a member's row — never a tenant, never the user's
// own account. The owner cannot be removed.
func RemoveMember(ctx context.Context, db *sql.DB, tenantID, userID string) error {
	cur, err := targetRole(ctx, db, tenantID, userID)
	if err != nil {
		return err
	}
	if cur == RoleOwner {
		return ErrOwnerMember
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM tenant_members WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID); err != nil {
		return fmt.Errorf("remove %s from %s: %w", userID, tenantID, err)
	}
	return nil
}
