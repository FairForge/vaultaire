// internal/auth/memberships.go
//
// A user in several tenants (WP-TEAM-1 PR C, Stage 1a Prompt 52; plan
// 42.2: home and work). The sign-in lists the user's tenants and mints a
// JWT for ONE — the active tenant — and the dashboard's switcher moves the
// session between them. Every management surface then acts on the active
// tenant only.
package auth

import (
	"context"
	"database/sql"
	"fmt"
)

// Membership is one tenant a user may act on.
type Membership struct {
	TenantID string `json:"id"`
	Role     Role   `json:"role"`
	Name     string `json:"name,omitempty"`
}

// Memberships lists the tenants userID is a member of, the one it owns
// first, then by when it joined. A tenant the user registered with no
// member row yet (the deploy overlap) is listed as owned.
func Memberships(ctx context.Context, db *sql.DB, userID string) ([]Membership, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT tenant_id, role, name, at FROM (
		SELECT m.tenant_id, m.role, COALESCE(t.name, '') AS name, m.accepted_at AS at
		  FROM tenant_members m JOIN tenants t ON t.id = m.tenant_id
		 WHERE m.user_id::text = $1
		UNION ALL
		SELECT t.id, 'owner', COALESCE(t.name, ''), t.created_at
		  FROM tenants t JOIN users u ON u.email = t.email
		 WHERE u.id::text = $1 AND t.parent_tenant_id IS NULL AND t.sandbox_of IS NULL
		   AND NOT EXISTS (SELECT 1 FROM tenant_members x WHERE x.tenant_id = t.id AND x.user_id = u.id)
		) x ORDER BY role = 'owner' DESC, at, tenant_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("memberships of %s: %w", userID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Membership
	for rows.Next() {
		var m Membership
		var role string
		var at sql.NullTime
		if err := rows.Scan(&m.TenantID, &role, &m.Name, &at); err != nil {
			return nil, fmt.Errorf("memberships of %s: %w", userID, err)
		}
		m.Role = Role(role)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ActiveTenant picks the tenant a sign-in acts on: want when the user is a
// member of it ("" = no choice: the owned tenant, else the first joined).
// ErrNotMember when want names a tenant the user may not act on, or the
// user is a member of none.
func ActiveTenant(ctx context.Context, db *sql.DB, userID, want string) (Membership, []Membership, error) {
	all, err := Memberships(ctx, db, userID)
	if err != nil {
		return Membership{}, nil, err
	}
	if len(all) == 0 {
		return Membership{}, all, ErrNotMember
	}
	if want == "" {
		return all[0], all, nil
	}
	for _, m := range all {
		if m.TenantID == want {
			return m, all, nil
		}
	}
	return Membership{}, all, ErrNotMember
}
