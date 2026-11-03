// internal/auth/family.go
//
// A family (WP-FAMILY-1, Stage 1a Prompt 56; docs/ACCOUNTS.md § A family;
// F-40). A family is a POOL of personal tenants, never one tenant: the
// organizer's root pays, and each member is a SPONSORED child — a person's
// own account with its own login, keys, recovery and library, which the
// organizer never mints a key for, reads, sees the request log of, or
// erases (the edges table). Invitations are WP-TEAM-1's rows with
// kind = 'family'; accepting one attaches the invitee's OWN root under the
// organizer and moves its bytes into the pool (usage.MovePoolTx), in one
// transaction. An account that pays for its own plan joins only after it
// cancels (F-40's recommendation: no proration machinery).
//
// Six people (F-40's draft — a constant until the owner decides): the
// organizer and at most five members, pending invitations counted.
package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/usage"
)

// MaxFamilyPeople is a family's size, the organizer included (F-40's draft:
// "up to 6 people"). Members + pending invitations ≤ MaxFamilyPeople − 1.
const MaxFamilyPeople = 6

// familyInvitationRole is the role column of a family invitation: the
// table's constraint asks for one, a family grants none (a member is no
// member of the organizer's tenant).
const familyInvitationRole = RoleViewer

// The refusals of the family flow.
var (
	// ErrFamilyFull: the family already has MaxFamilyPeople people, pending
	// invitations counted.
	ErrFamilyFull = errors.New("the family is full: six people, the organizer included")
	// ErrMemberPaysOwnPlan: the invitee's account has a live paid plan of its
	// own (F-40): it joins after cancelling it.
	ErrMemberPaysOwnPlan = errors.New("this account pays for its own plan: cancel it, then join the family")
	// ErrNotFamilyOrganizer: the tenant cannot organize a family (it is a
	// child, a sandbox, or a member of a family itself).
	ErrNotFamilyOrganizer = errors.New("only a personal account that is no family member organizes a family")
	// ErrCannotJoinFamily: the joining account is not a plain personal
	// account (a child, a sandbox, a platform or partner, an account with
	// apps or children of its own, already in a family, or the organizer's).
	ErrCannotJoinFamily = errors.New("only a personal account with no apps or child accounts of its own joins a family")
)

// InviteFamilyMember invites email to organizer's family and returns the
// invitation and its token (shown once: in the e-mailed link). It counts
// the family — the organizer, its sponsored members and the pending family
// invitations of other addresses — under the organizer's tenant row FOR
// UPDATE: ErrFamilyFull past MaxFamilyPeople. A pending invitation of the
// same address is replaced (the resend), as a member invitation's is. The
// row, and its family.invited audit row, in one transaction.
func InviteFamilyMember(ctx context.Context, db *sql.DB, organizer, invitedBy, email string, now time.Time) (Invitation, string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") || len(email) > 254 || strings.ContainsAny(email, "\r\n") {
		return Invitation{}, "", fmt.Errorf("invalid email address")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Invitation{}, "", fmt.Errorf("invite %s to the family: %w", email, err)
	}
	defer func() { _ = tx.Rollback() }()
	var root bool
	var ownEmail string
	err = tx.QueryRowContext(ctx, `SELECT parent_tenant_id IS NULL AND sandbox_of IS NULL AND provisional_until IS NULL, lower(email)
		  FROM tenants WHERE id = $1 FOR UPDATE`, organizer).Scan(&root, &ownEmail)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Invitation{}, "", ErrTenantNotFound
	case err != nil:
		return Invitation{}, "", fmt.Errorf("invite %s to the family: %w", email, err)
	case !root:
		return Invitation{}, "", ErrNotFamilyOrganizer
	case email == ownEmail:
		return Invitation{}, "", ErrAlreadyMember
	}
	var members, pending, sentToday int
	var already bool
	if err := tx.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM tenants c WHERE c.parent_tenant_id = $1 AND c.child_kind = 'sponsored'),
		       (SELECT COUNT(*) FROM tenant_invitations i
		         WHERE i.tenant_id = $1 AND i.kind = 'family' AND i.accepted_at IS NULL AND i.expires_at > $3 AND lower(i.email) <> $2),
		       (SELECT COUNT(*) FROM audit_logs a
		         WHERE a.tenant_id = $1 AND a.action IN ('member.invited', 'family.invited')
		           AND a.timestamp > $3::timestamptz - INTERVAL '24 hours'),
		       EXISTS (SELECT 1 FROM tenants c WHERE c.parent_tenant_id = $1 AND c.child_kind = 'sponsored' AND lower(c.email) = $2)`,
		organizer, email, now).Scan(&members, &pending, &sentToday, &already); err != nil {
		return Invitation{}, "", fmt.Errorf("invite %s to the family: %w", email, err)
	}
	if already {
		return Invitation{}, "", ErrAlreadyMember
	}
	if 1+members+pending >= MaxFamilyPeople {
		return Invitation{}, "", ErrFamilyFull
	}
	if sentToday >= MaxInvitationsPerDay {
		return Invitation{}, "", ErrInvitationRateLimited
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Invitation{}, "", fmt.Errorf("invite %s to the family: %w", email, err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	inv := Invitation{ID: uuid.NewString(), TenantID: organizer, Kind: "family", Email: email, Role: familyInvitationRole,
		InvitedBy: invitedBy, CreatedAt: now.UTC(), ExpiresAt: now.UTC().Add(InvitationTTL)}
	var by any
	if _, err := uuid.Parse(invitedBy); err == nil {
		by = invitedBy
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tenant_invitations WHERE tenant_id = $1 AND lower(email) = $2 AND accepted_at IS NULL`,
		organizer, email); err != nil {
		return Invitation{}, "", fmt.Errorf("invite %s to the family: replace the pending one: %w", email, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenant_invitations (id, tenant_id, kind, email, role, token_hash, invited_by, expires_at, created_at)
		VALUES ($1, $2, 'family', $3, $4, $5, $6, $7, $8)`,
		inv.ID, organizer, email, string(inv.Role), HashInvitationToken(token), by, inv.ExpiresAt, inv.CreatedAt); err != nil {
		return Invitation{}, "", fmt.Errorf("invite %s to the family: %w", email, err)
	}
	if err := audit.RecordTx(ctx, tx, audit.Entry{TenantID: organizer, Action: "family.invited", Resource: "invitation:" + inv.ID,
		Metadata: map[string]any{"email": inv.Email}}); err != nil {
		return Invitation{}, "", fmt.Errorf("invite %s to the family: %w", email, err)
	}
	if err := tx.Commit(); err != nil {
		return Invitation{}, "", fmt.Errorf("invite %s to the family: %w", email, err)
	}
	return inv, token, nil
}

// FamilyInvitationByToken reads the family invitation a link's token names,
// checked as usable at now (the join page shows it before anyone signs in).
func FamilyInvitationByToken(ctx context.Context, db *sql.DB, token string, now time.Time) (Invitation, error) {
	inv, err := scanInvitation(db.QueryRowContext(ctx,
		`SELECT `+invitationColumns+` FROM tenant_invitations WHERE token_hash = $1 AND kind = 'family'`, HashInvitationToken(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, ErrInvitationNotFound
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("read family invitation: %w", err)
	}
	return inv, checkFamilyInvitation(inv, now)
}

func checkFamilyInvitation(inv Invitation, now time.Time) error {
	switch {
	case inv.Kind != "family":
		return ErrInvitationNotFound
	case inv.AcceptedAt != nil:
		return ErrInvitationUsed
	case !now.Before(inv.ExpiresAt):
		return ErrInvitationExpired
	}
	return nil
}

// AcceptFamilyInvitation spends the family invitation for the signed-in
// user userID, whose e-mail must be the invited address, attaching their
// OWN root tenant (memberTenant: the account they act on, which they own)
// to the organizer — in ONE transaction: the invitation locked, both
// tenants' rows locked in id order, the checks (F-40's live entitlement,
// a plain personal account, the family's size), the edge (parent, kind
// 'sponsored', the organizer's pool), usage.MovePoolTx (the bytes into the
// pool; the pool's lock order), the member's own cap cleared (0: the pool
// decides, docs/ACCOUNTS.md "caps inside the pool"), the invitation spent,
// and an audit row on each tenant.
func AcceptFamilyInvitation(ctx context.Context, db *sql.DB, token, userID, userEmail, memberTenant string, now time.Time) (Invitation, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Invitation{}, fmt.Errorf("join the family: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	inv, err := scanInvitation(tx.QueryRowContext(ctx,
		`SELECT `+invitationColumns+` FROM tenant_invitations WHERE token_hash = $1 FOR UPDATE`, HashInvitationToken(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, ErrInvitationNotFound
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("join the family: %w", err)
	}
	if err := checkFamilyInvitation(inv, now); err != nil {
		return inv, err
	}
	if !strings.EqualFold(strings.TrimSpace(userEmail), inv.Email) {
		return inv, ErrInvitationEmailMismatch
	}
	organizer := inv.TenantID
	if memberTenant == organizer {
		return inv, ErrCannotJoinFamily
	}
	// Both tenants' rows, in id order (two joins of one family, or a join
	// racing the organizer's own invitation, never deadlock).
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM tenants WHERE id IN ($1, $2) ORDER BY id FOR UPDATE`, organizer, memberTenant); err != nil {
		return inv, fmt.Errorf("join the family: lock: %w", err)
	}
	var orgRoot bool
	if err := tx.QueryRowContext(ctx, `SELECT parent_tenant_id IS NULL AND sandbox_of IS NULL FROM tenants WHERE id = $1`, organizer).
		Scan(&orgRoot); err != nil {
		return inv, fmt.Errorf("join the family: the organizer: %w", err)
	}
	if !orgRoot {
		return inv, ErrNotFamilyOrganizer
	}
	var plain, owns, pays bool
	err = tx.QueryRowContext(ctx, `
		SELECT t.parent_tenant_id IS NULL AND t.sandbox_of IS NULL AND t.provisional_until IS NULL
		       AND NOT t.platform AND NOT t.partner
		       AND NOT EXISTS (SELECT 1 FROM tenants c WHERE c.parent_tenant_id = t.id),
		       EXISTS (SELECT 1 FROM tenant_members m WHERE m.tenant_id = t.id AND m.user_id::text = $2 AND m.role = 'owner')
		         OR EXISTS (SELECT 1 FROM users u WHERE u.id::text = $2 AND lower(u.email) = lower(t.email)),
		       EXISTS (SELECT 1 FROM entitlements e WHERE e.tenant_id = t.id AND e.lapsed_at IS NULL)
		  FROM tenants t WHERE t.id = $1`, memberTenant, userID).Scan(&plain, &owns, &pays)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return inv, ErrTenantNotFound
	case err != nil:
		return inv, fmt.Errorf("join the family: the account: %w", err)
	case !owns || !plain:
		return inv, ErrCannotJoinFamily
	case pays:
		return inv, ErrMemberPaysOwnPlan
	}
	var members int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenants WHERE parent_tenant_id = $1 AND child_kind = 'sponsored'`, organizer).
		Scan(&members); err != nil {
		return inv, fmt.Errorf("join the family: %w", err)
	}
	if 1+members+1 > MaxFamilyPeople {
		return inv, ErrFamilyFull
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tenants SET parent_tenant_id = $2, child_kind = 'sponsored', pool_root_id = $2 WHERE id = $1`,
		memberTenant, organizer); err != nil {
		return inv, fmt.Errorf("join the family: the edge: %w", err)
	}
	if _, err := usage.MovePoolTx(ctx, tx, memberTenant, memberTenant, organizer); err != nil {
		return inv, fmt.Errorf("join the family: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tenant_quotas SET storage_limit_bytes = 0, updated_at = NOW() WHERE tenant_id = $1`, memberTenant); err != nil {
		return inv, fmt.Errorf("join the family: the pool decides: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tenant_invitations SET accepted_at = $2 WHERE id = $1`, inv.ID, now); err != nil {
		return inv, fmt.Errorf("join the family: %w", err)
	}
	at := now
	inv.AcceptedAt = &at
	for _, e := range []audit.Entry{
		{TenantID: organizer, Action: "family.member_joined", Resource: "tenant:" + memberTenant,
			Metadata: map[string]any{"invitation": inv.ID}},
		{UserID: userID, TenantID: memberTenant, Action: "family.joined", Resource: "tenant:" + organizer,
			Metadata: map[string]any{"invitation": inv.ID}},
	} {
		if err := audit.RecordTx(ctx, tx, e); err != nil {
			return inv, fmt.Errorf("join the family: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return inv, fmt.Errorf("join the family: %w", err)
	}
	return inv, nil
}

// FamilyMember is one member as its organizer sees it: usage and caps, and
// nothing else (no bucket, object, key or request — docs/ACCOUNTS.md).
type FamilyMember struct {
	ID           string    `json:"id"`
	Label        string    `json:"label"`
	StorageUsed  int64     `json:"storage_used_bytes"`
	StorageLimit *int64    `json:"storage_limit_bytes"`
	JoinedAt     time.Time `json:"joined_at"`
}

// FamilyView is the family page of a tenant: its role (organizer, member,
// or none), and for an organizer its members, its pending invitations and
// its pool; for a member, the organizer's label.
type FamilyView struct {
	Role        string         `json:"role"`
	Organizer   string         `json:"organizer,omitempty"`
	Members     []FamilyMember `json:"members,omitempty"`
	Invitations []Invitation   `json:"invitations,omitempty"`
	PoolUsed    int64          `json:"pool_used_bytes"`
	PoolLimit   int64          `json:"pool_limit_bytes"`
}

// Family reads tenantID's family view.
func Family(ctx context.Context, db *sql.DB, tenantID string) (FamilyView, error) {
	var v FamilyView
	var kind, parent sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT child_kind, parent_tenant_id FROM tenants WHERE id = $1`, tenantID).Scan(&kind, &parent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return v, ErrTenantNotFound
		}
		return v, fmt.Errorf("read the family of %s: %w", tenantID, err)
	}
	if kind.String == string(ChildSponsored) {
		v.Role = "member"
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(name, ''), email) FROM tenants WHERE id = $1`, parent.String).Scan(&v.Organizer); err != nil {
			return v, fmt.Errorf("read the family of %s: %w", tenantID, err)
		}
		return v, nil
	}
	v.Role = "organizer"
	if err := db.QueryRowContext(ctx, `SELECT pool_used_bytes, storage_limit_bytes FROM tenant_quotas WHERE tenant_id = $1`, tenantID).
		Scan(&v.PoolUsed, &v.PoolLimit); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return v, fmt.Errorf("read the family of %s: %w", tenantID, err)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT t.id, t.email, COALESCE(q.storage_used_bytes, 0), COALESCE(q.storage_limit_bytes, 0),
		       COALESCE((SELECT MAX(i.accepted_at) FROM tenant_invitations i
		                  WHERE i.tenant_id = $1 AND i.kind = 'family' AND lower(i.email) = lower(t.email)), t.created_at)
		  FROM tenants t LEFT JOIN tenant_quotas q ON q.tenant_id = t.id
		 WHERE t.parent_tenant_id = $1 AND t.child_kind = 'sponsored'
		 ORDER BY t.created_at, t.id`, tenantID)
	if err != nil {
		return v, fmt.Errorf("read the family of %s: %w", tenantID, err)
	}
	defer func() { _ = rows.Close() }()
	v.Members = []FamilyMember{}
	for rows.Next() {
		var m FamilyMember
		var limit int64
		if err := rows.Scan(&m.ID, &m.Label, &m.StorageUsed, &limit, &m.JoinedAt); err != nil {
			return v, fmt.Errorf("read the family of %s: %w", tenantID, err)
		}
		if limit > 0 {
			l := limit
			m.StorageLimit = &l
		}
		v.Members = append(v.Members, m)
	}
	if err := rows.Err(); err != nil {
		return v, fmt.Errorf("read the family of %s: %w", tenantID, err)
	}
	inv, err := db.QueryContext(ctx, `SELECT `+invitationColumns+` FROM tenant_invitations
		 WHERE tenant_id = $1 AND kind = 'family' AND accepted_at IS NULL AND expires_at > NOW() ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return v, fmt.Errorf("read the family invitations of %s: %w", tenantID, err)
	}
	defer func() { _ = inv.Close() }()
	v.Invitations = []Invitation{}
	for inv.Next() {
		i, err := scanInvitation(inv)
		if err != nil {
			return v, fmt.Errorf("read the family invitations of %s: %w", tenantID, err)
		}
		v.Invitations = append(v.Invitations, i)
	}
	return v, inv.Err()
}
