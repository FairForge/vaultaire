package account

// RP84-1 (Stage 1a Prompt 53, Part 0): a key is the tenant's (D-STRIPE-40),
// so the tenant's erasure takes every key of the tenant, whoever made it,
// and a member's own erasure leaves the keys it made on another tenant —
// handed to that tenant's owner.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/testutil"
)

type teamKeyFixture struct {
	t               *testing.T
	db              *sql.DB
	svc             *Service
	owner, member   string // users.id
	ownerEmail      string
	memberEmail     string
	team, personal  string // tenants.id: the owner's team, the member's own
	keyRowID, keyID string // the key the member made on the team
	ownerKeyID      string // the owner's primary on the team
	memberOwnKeyID  string // the member's key on its own tenant
}

func setupTeamKeyFixture(t *testing.T) *teamKeyFixture {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	s := uuid.NewString()[:8]
	f := &teamKeyFixture{t: t, db: db, svc: NewService(db, zap.NewNop()),
		owner: uuid.NewString(), member: uuid.NewString(),
		ownerEmail: "team-owner-" + s + "@example.com", memberEmail: "team-dev-" + s + "@example.com",
		team: "tenant-team-" + s, personal: "tenant-own-" + s,
		keyRowID: uuid.NewString(), keyID: "VKTEAM" + s, ownerKeyID: "VKOWNR" + s, memberOwnKeyID: "VKMEMB" + s}
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := db.ExecContext(ctx, q, args...)
		require.NoError(t, err, q)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM api_keys WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM tenant_members WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM audit_logs WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM tenant_quotas WHERE tenant_id IN ($1, $2)`,
			`DELETE FROM tenants WHERE id IN ($1, $2)`,
		} {
			_, _ = db.ExecContext(ctx, q, f.team, f.personal)
		}
		_, _ = db.ExecContext(ctx, `DELETE FROM users WHERE id IN ($1, $2)`, f.owner, f.member)
	})
	for _, u := range []struct{ id, email string }{{f.owner, f.ownerEmail}, {f.member, f.memberEmail}} {
		exec(`INSERT INTO users (id, email, password_hash, status) VALUES ($1, $2, 'x', 'active')`, u.id, u.email)
	}
	exec(`INSERT INTO tenants (id, name, email) VALUES ($1, 'Team', $2)`, f.team, f.ownerEmail)
	exec(`INSERT INTO tenants (id, name, email) VALUES ($1, 'Own', $2)`, f.personal, f.memberEmail)
	exec(`INSERT INTO tenant_quotas (tenant_id) VALUES ($1), ($2)`, f.team, f.personal)
	exec(`INSERT INTO tenant_members (tenant_id, user_id, role) VALUES ($1, $2, 'owner')`, f.team, f.owner)
	exec(`INSERT INTO tenant_members (tenant_id, user_id, role, invited_by) VALUES ($1, $2, 'developer', $3)`, f.team, f.member, f.owner)
	exec(`INSERT INTO tenant_members (tenant_id, user_id, role) VALUES ($1, $2, 'owner')`, f.personal, f.member)
	exec(`INSERT INTO api_keys (id, user_id, tenant_id, name, key_id, secret_key, secret_hash, is_primary)
	      VALUES ($1, $2, $3, 'owner primary', $4, 'sk-owner', 'h', TRUE)`, uuid.NewString(), f.owner, f.team, f.ownerKeyID)
	exec(`INSERT INTO api_keys (id, user_id, tenant_id, name, key_id, secret_key, secret_hash)
	      VALUES ($1, $2, $3, 'team ci', $4, 'sk-team', 'h')`, f.keyRowID, f.member, f.team, f.keyID)
	exec(`INSERT INTO api_keys (id, user_id, tenant_id, name, key_id, secret_key, secret_hash, is_primary)
	      VALUES ($1, $2, $3, 'own primary', $4, 'sk-own', 'h', TRUE)`, uuid.NewString(), f.member, f.personal, f.memberOwnKeyID)
	return f
}

// due makes userID's account past its erasure date.
func (f *teamKeyFixture) due(userID string) {
	f.t.Helper()
	_, err := f.db.Exec(`UPDATE users SET status = 'pending_deletion', deletion_scheduled_at = $2 WHERE id = $1`,
		userID, time.Now().Add(-time.Hour))
	require.NoError(f.t, err)
}

func (f *teamKeyFixture) lookup(keyID string) (*auth.Credential, error) {
	return auth.NewAuth(f.db, zap.NewNop(), nil).LookupCredential(context.Background(), keyID)
}

func TestErase_TheTeamsErasureTakesEveryKeyOfTheTenant(t *testing.T) {
	f := setupTeamKeyFixture(t)
	cred, err := f.lookup(f.keyID)
	require.NoError(t, err)
	require.Equal(t, f.team, cred.TenantID)

	f.due(f.owner)
	_, err = f.svc.EraseRows(context.Background(), f.owner, f.team, f.ownerEmail, time.Now())
	require.NoError(t, err)

	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE tenant_id = $1`, f.team).Scan(&n))
	assert.Zero(t, n, "every key of the erased tenant is gone, the members' included")
	_, err = f.lookup(f.keyID)
	assert.ErrorIs(t, err, auth.ErrUnknownAccessKey, "the developer's key no longer resolves to the erased tenant")
	_, err = f.lookup(f.memberOwnKeyID)
	assert.NoError(t, err, "the member's own tenant is untouched")
}

func TestErase_AMembersErasureKeepsTheTeamsKeys(t *testing.T) {
	f := setupTeamKeyFixture(t)
	f.due(f.member)
	_, err := f.svc.EraseRows(context.Background(), f.member, f.personal, f.memberEmail, time.Now())
	require.NoError(t, err)

	cred, err := f.lookup(f.keyID)
	require.NoError(t, err, "the team's key survives a member's own erasure")
	assert.Equal(t, f.team, cred.TenantID)
	var maker string
	require.NoError(t, f.db.QueryRow(`SELECT user_id::text FROM api_keys WHERE key_id = $1`, f.keyID).Scan(&maker))
	assert.Equal(t, f.owner, maker, "the key now names the team's owner")

	var action, resource string
	var userID sql.NullString
	require.NoError(t, f.db.QueryRow(`SELECT action, resource, user_id::text FROM audit_logs WHERE tenant_id = $1 AND action = 'key.maker_erased'`,
		f.team).Scan(&action, &resource, &userID))
	assert.Equal(t, "key:"+f.keyRowID, resource)
	assert.False(t, userID.Valid, "the row names no erased person")

	_, err = f.lookup(f.memberOwnKeyID)
	assert.ErrorIs(t, err, auth.ErrUnknownAccessKey, "the member's own tenant's keys went with it")
}
