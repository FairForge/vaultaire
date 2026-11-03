package account

// RP90-2 (Stage 1a Prompt 56 Part 0): an S3 key an agent grant minted dies
// with its grant on EVERY path (D-STRIPE-78) — a revoke, the member's
// removal, the person's erasure — and a key whose grant's management key
// is revoked never authenticates, whatever its own row says.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
)

// agentKeyOnTeam mints a grant on the team for the member (granted_by the
// member) and an S3 key of that grant as POST /keys makes it: user_id the
// grant's actor (the team's owner), agent_grant_id the grant.
func (f *teamKeyFixture) agentKeyOnTeam() (grantID, keyID string) {
	f.t.Helper()
	g, _, err := auth.MintAgentGrant(context.Background(), f.db, f.team, auth.AgentGrantOptions{AgentName: "Claude", GrantedBy: f.member})
	require.NoError(f.t, err)
	f.t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM api_keys WHERE agent_grant_id = $1::uuid`, g.ID)
		_, _ = f.db.Exec(`DELETE FROM agent_grants WHERE tenant_id = $1`, f.team)
		_, _ = f.db.Exec(`DELETE FROM management_keys WHERE tenant_id = $1 OR tenant_id IN (SELECT id FROM tenants WHERE sandbox_of = $1)`, f.team)
		_, _ = f.db.Exec(`DELETE FROM tenant_quotas WHERE tenant_id IN (SELECT id FROM tenants WHERE sandbox_of = $1)`, f.team)
		_, _ = f.db.Exec(`DELETE FROM tenants WHERE sandbox_of = $1`, f.team)
	})
	keyID = "VKAGNT" + uuid.NewString()[:8]
	_, err = f.db.Exec(`INSERT INTO api_keys (user_id, tenant_id, name, key_id, secret_key, secret_hash, agent_grant_id)
		VALUES ($1, $2, 'agent s3', $3, 'sk-agent', 'h', $4::uuid)`, f.owner, f.team, keyID, g.ID)
	require.NoError(f.t, err)
	return g.ID, keyID
}

func (f *teamKeyFixture) keyRevoked(keyID string) bool {
	f.t.Helper()
	var revoked bool
	require.NoError(f.t, f.db.QueryRow(`SELECT revoked_at IS NOT NULL FROM api_keys WHERE key_id = $1`, keyID).Scan(&revoked))
	return revoked
}

func TestAgent_KeysDieWithTheGrantOnEveryPath(t *testing.T) {
	ctx := context.Background()

	t.Run("revoke", func(t *testing.T) {
		f := setupTeamKeyFixture(t)
		grant, key := f.agentKeyOnTeam()
		_, err := f.lookup(key)
		require.NoError(t, err, "a live grant's key authenticates")
		// The revoke alone — no per-key RevokeAPIKey call after it (the one
		// that could fail and leave the key live).
		_, err = auth.RevokeAgentGrant(ctx, f.db, f.team, grant)
		require.NoError(t, err)
		assert.True(t, f.keyRevoked(key), "revoked in the grant's own revoke")
		_, err = f.lookup(key)
		assert.Error(t, err)
	})

	t.Run("the grant's management key revoked", func(t *testing.T) {
		f := setupTeamKeyFixture(t)
		grant, key := f.agentKeyOnTeam()
		_, err := f.db.Exec(`UPDATE management_keys SET revoked_at = NOW() WHERE id = (SELECT management_key_id FROM agent_grants WHERE id = $1::uuid)`, grant)
		require.NoError(t, err)
		require.False(t, f.keyRevoked(key))
		_, err = f.lookup(key)
		assert.ErrorIs(t, err, auth.ErrAccessKeyRevoked, "the lookup reads the grant: a dead grant's key never authenticates")
	})

	t.Run("member removal", func(t *testing.T) {
		f := setupTeamKeyFixture(t)
		_, key := f.agentKeyOnTeam()
		_, err := auth.RemoveMember(ctx, f.db, f.team, f.member, false)
		require.NoError(t, err)
		assert.True(t, f.keyRevoked(key), "the member left: the agent acting for them stops, keys and all")
		_, err = f.lookup(key)
		assert.Error(t, err)
	})

	t.Run("the person's erasure", func(t *testing.T) {
		f := setupTeamKeyFixture(t)
		_, key := f.agentKeyOnTeam()
		f.due(f.member)
		_, err := f.svc.EraseRows(ctx, f.member, f.personal, f.memberEmail, time.Now())
		require.NoError(t, err)
		assert.True(t, f.keyRevoked(key), "the person erased: their agent's S3 keys on the team are revoked")
		_, err = f.lookup(key)
		assert.Error(t, err)
		_, err = f.lookup(f.keyID)
		assert.NoError(t, err, "the team's own keys are untouched (RP84-1)")
	})
}
