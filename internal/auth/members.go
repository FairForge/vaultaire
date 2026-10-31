// internal/auth/members.go
//
// Members and roles (WP-TEAM-1, Stage 1a Prompt 52; migration 118;
// docs/ACCOUNTS.md: a team is ONE shared tenant with members, a person may
// act on several tenants). A member is a (tenant, user, role) row of
// tenant_members. Nothing about a tenant's data changes — S3 keys stay
// tenant-keyed — only the management surfaces (the management API, the
// platform API, the dashboard) learn who acts and authorise each write by
// the acting member's role through Can, the one rule.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Role is a member's role on a tenant.
type Role string

// The four roles, most powerful first.
const (
	RoleOwner     Role = "owner"
	RoleAdmin     Role = "admin"
	RoleDeveloper Role = "developer"
	RoleViewer    Role = "viewer"
)

// ParseRole reads a role name; ok=false for anything else.
func ParseRole(s string) (Role, bool) {
	switch r := Role(s); r {
	case RoleOwner, RoleAdmin, RoleDeveloper, RoleViewer:
		return r, true
	}
	return "", false
}

// Action is what a management write does, named per route.
type Action string

// The actions (plan § WP-STRIPE, the WP-TEAM-1 row: a viewer reads; a
// developer makes keys and buckets; an admin invites and manages members,
// never the owner; the owner alone deletes the account, changes billing or
// exports the account).
const (
	// ActionRead: every GET of the management surfaces.
	ActionRead Action = "read"
	// ActionBucketWrite: create, change or delete a bucket, its tier or
	// residency, a direct upload into it.
	ActionBucketWrite Action = "bucket.write"
	// ActionKeyWrite: create or revoke an S3 key of the tenant.
	ActionKeyWrite Action = "key.write"
	// ActionWebhookWrite: create, change, test, rotate or delete a webhook
	// endpoint.
	ActionWebhookWrite Action = "webhook.write"
	// ActionSandboxReset: empty the tenant's test-mode sandbox.
	ActionSandboxReset Action = "sandbox.reset"
	// ActionChildKey: mint or revoke a platform child's key, or its STS
	// credentials ("a platform's member acts on its children").
	ActionChildKey Action = "platform.child_key"
	// ActionChildWrite: create or schedule the erasure of a platform child.
	ActionChildWrite Action = "platform.child_write"
	// ActionMemberManage: invite, change the role of, or remove a member
	// (never the owner).
	ActionMemberManage Action = "member.manage"
	// ActionAccountExport: the GDPR export of the account.
	ActionAccountExport Action = "account.export"
	// ActionAccountDelete: schedule or cancel the account's erasure.
	ActionAccountDelete Action = "account.delete"
	// ActionBilling: buy, resize or cancel the house; switch the provider.
	ActionBilling Action = "billing.write"
	// ActionManagementKey: mint or revoke a management key (WP-KEY-1) —
	// an admin's or the owner's, and a person's only (never a key's).
	ActionManagementKey Action = "management_key.write"
)

// roleActions is the action table — THE rule every management and
// dashboard write calls through Can. A role not in it can do nothing.
var roleActions = map[Role]map[Action]bool{
	RoleViewer: {ActionRead: true},
	RoleDeveloper: {ActionRead: true, ActionBucketWrite: true, ActionKeyWrite: true, ActionWebhookWrite: true,
		ActionSandboxReset: true, ActionChildKey: true},
	RoleAdmin: {ActionRead: true, ActionBucketWrite: true, ActionKeyWrite: true, ActionWebhookWrite: true,
		ActionSandboxReset: true, ActionChildKey: true, ActionChildWrite: true, ActionMemberManage: true,
		ActionManagementKey: true},
	RoleOwner: {ActionRead: true, ActionBucketWrite: true, ActionKeyWrite: true, ActionWebhookWrite: true,
		ActionSandboxReset: true, ActionChildKey: true, ActionChildWrite: true, ActionMemberManage: true,
		ActionAccountExport: true, ActionAccountDelete: true, ActionBilling: true, ActionManagementKey: true},
}

// Can reports whether a member of role may do action.
func Can(role Role, action Action) bool { return roleActions[role][action] }

// Actions lists every action the table names (tests walk it).
func Actions() []Action {
	return []Action{ActionRead, ActionBucketWrite, ActionKeyWrite, ActionWebhookWrite, ActionSandboxReset, ActionChildKey,
		ActionChildWrite, ActionMemberManage, ActionAccountExport, ActionAccountDelete, ActionBilling, ActionManagementKey}
}

// ErrNotMember: the user may not act on the tenant.
var ErrNotMember = errors.New("not a member of this tenant")

// MemberRole is the role userID holds on tenantID: its tenant_members row.
// A user with no row who registered the tenant (users ↔ tenants by e-mail,
// the registration contract migration 118's backfill applied) is its owner
// — an account the previous build registered during the deploy overlap has
// no row yet. Anyone else is ErrNotMember. A child or a sandbox has no
// member: its platform's members act on it through the platform.
func MemberRole(ctx context.Context, db queryRower, tenantID, userID string) (Role, error) {
	var role string
	err := db.QueryRowContext(ctx,
		`SELECT role FROM tenant_members WHERE tenant_id = $1 AND user_id::text = $2`, tenantID, userID).Scan(&role)
	switch {
	case err == nil:
		if r, ok := ParseRole(role); ok {
			return r, nil
		}
		return "", fmt.Errorf("member role of %s on %s: unknown role %q", userID, tenantID, role)
	case !errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("member role of %s on %s: %w", userID, tenantID, err)
	}
	var one int
	err = db.QueryRowContext(ctx, `
		SELECT 1 FROM tenants t JOIN users u ON u.email = t.email
		 WHERE t.id = $1 AND u.id::text = $2 AND t.parent_tenant_id IS NULL AND t.sandbox_of IS NULL`,
		tenantID, userID).Scan(&one)
	switch {
	case err == nil:
		return RoleOwner, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrNotMember
	default:
		return "", fmt.Errorf("member role of %s on %s: %w", userID, tenantID, err)
	}
}
