package api

// RP90-4/5 (Stage 1a Prompt 56 Part 0): an agent acts FOR a person — the
// person removed, the agent stops; and an agent caps only what its grant
// made (its own keys, the buckets it created), never a person's key.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
)

// member adds a developer member to the fixture's tenant.
func (f *platformFixture) member() string {
	f.t.Helper()
	id := uuid.NewString()
	_, err := f.db.Exec(`INSERT INTO users (id, email, password_hash, status) VALUES ($1, $2, 'x', 'active')`, id, "plat-dev-"+id[:8]+"@platform.example")
	require.NoError(f.t, err)
	_, err = f.db.Exec(`INSERT INTO tenant_members (tenant_id, user_id, role, invited_by) VALUES ($1, $2, 'developer', $3)`, f.user.TenantID, id, f.user.ID)
	require.NoError(f.t, err)
	f.t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM tenant_members WHERE user_id = $1`, id)
		_, _ = f.db.Exec(`UPDATE agent_grants SET granted_by = NULL WHERE granted_by = $1`, id)
		_, _ = f.db.Exec(`DELETE FROM audit_logs WHERE user_id = $1`, id)
		_, _ = f.db.Exec(`DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func TestAgent_RemovedMembersAgentStops(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents()
	dev := f.member()

	// A membership gone by any path: the agent's key is no one's any more.
	_, sec := f.agentGrant(auth.AgentGrantOptions{AgentName: "Claude", GrantedBy: dev})
	code, out := f.call(http.MethodGet, "/api/v1/manage/buckets", sec.Live, nil)
	require.Equal(t, http.StatusOK, code, out)
	_, err := f.db.Exec(`DELETE FROM tenant_members WHERE tenant_id = $1 AND user_id = $2`, f.user.TenantID, dev)
	require.NoError(t, err)
	code, out = f.call(http.MethodGet, "/api/v1/manage/buckets", sec.Live, nil)
	assert.Equal(t, http.StatusUnauthorized, code, "an agent whose person is no member acts for no one: %v", out)

	// The removal itself revokes the member's grants, revoke_keys or not.
	_, err = f.db.Exec(`INSERT INTO tenant_members (tenant_id, user_id, role, invited_by) VALUES ($1, $2, 'developer', $3)`, f.user.TenantID, dev, f.user.ID)
	require.NoError(t, err)
	g2, sec2 := f.agentGrant(auth.AgentGrantOptions{AgentName: "Claude 2", GrantedBy: dev})
	code, _ = f.call(http.MethodGet, "/api/v1/manage/buckets", sec2.Live, nil)
	require.Equal(t, http.StatusOK, code)
	_, err = auth.RemoveMember(context.Background(), f.db, f.user.TenantID, dev, false)
	require.NoError(t, err)
	got, err := auth.GetAgentGrant(context.Background(), f.db, f.user.TenantID, g2.ID)
	require.NoError(t, err)
	assert.NotNil(t, got.RevokedAt, "the grant is revoked with the membership")

	// The owner's own grant is untouched.
	_, own := f.agentGrant(auth.AgentGrantOptions{AgentName: "Mine"})
	code, _ = f.call(http.MethodGet, "/api/v1/manage/buckets", own.Live, nil)
	assert.Equal(t, http.StatusOK, code)
}

func TestAgent_CannotCapAPersonsKey(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents().withApprovals()
	_, sec := f.agentGrant(auth.AgentGrantOptions{AgentName: "Claude"})

	// A person's key and a person's bucket.
	code, out := f.call(http.MethodPost, "/api/v1/manage/keys", f.jwt, map[string]any{"name": "the person's"})
	require.Equal(t, http.StatusCreated, code, out)
	personKey := out["id"].(string)
	personBucket := f.bucket("rp905p")

	one := map[string]any{"storage_limit_bytes": 1, "egress_limit_bytes": 1}
	code, out = f.call(http.MethodPut, "/api/v1/manage/keys/"+personKey+"/caps", sec.Live, one)
	assert.Equal(t, http.StatusForbidden, code, "an agent never caps a person's key: %v", out)
	assert.Equal(t, "person_required", errCode(out))
	code, out = f.call(http.MethodPut, "/api/v1/manage/buckets/"+personBucket+"/caps", sec.Live, one)
	assert.Equal(t, http.StatusForbidden, code, "an agent never caps a bucket it did not create: %v", out)
	assert.Equal(t, "person_required", errCode(out))

	// Its own key and its own bucket: yes.
	code, out = f.call(http.MethodPost, "/api/v1/manage/keys", sec.Live, map[string]any{"name": "agent's"})
	require.Equal(t, http.StatusCreated, code, out)
	agentKey := out["id"].(string)
	code, out = f.call(http.MethodPut, "/api/v1/manage/keys/"+agentKey+"/caps", sec.Live, map[string]any{"storage_limit_bytes": 1 << 20})
	assert.Equal(t, http.StatusOK, code, "its own key: %v", out)
	ownBucket := "rp905a-" + strings.ToLower(uuid.NewString()[:8])
	code, out = f.call(http.MethodPost, "/api/v1/manage/buckets", sec.Live, map[string]any{"name": ownBucket})
	require.Equal(t, http.StatusCreated, code, out)
	code, out = f.call(http.MethodPut, "/api/v1/manage/buckets/"+ownBucket+"/caps", sec.Live, map[string]any{"storage_limit_bytes": 1 << 20})
	assert.Equal(t, http.StatusOK, code, "a bucket it created: %v", out)

	// The person caps anything.
	code, out = f.call(http.MethodPut, "/api/v1/manage/keys/"+personKey+"/caps", f.jwt, one)
	assert.Equal(t, http.StatusOK, code, out)
}
