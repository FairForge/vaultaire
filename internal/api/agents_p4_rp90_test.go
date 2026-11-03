package api

// RP90-11…17 (Stage 1a Prompt 56, the P4 PR).

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
)

// RP90-11: the grant's cap never replaces a smaller one on the key.
func TestAgent_MarkKeepsTheSmallerCap(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents()
	grantCap := int64(10 << 20)
	_, sec := f.agentGrant(auth.AgentGrantOptions{AgentName: "Claude", StorageLimit: &grantCap, EgressLimit: &grantCap})
	code, out := f.call(http.MethodPost, "/api/v1/manage/keys", sec.Live, map[string]any{"name": "agent's"})
	require.Equal(t, http.StatusCreated, code, out)
	id := out["id"].(string)
	_, err := f.db.Exec(`UPDATE api_keys SET storage_limit_bytes = 1024, egress_limit_bytes = 2048 WHERE id = $1`, id)
	require.NoError(t, err)
	k, err := auth.LookupManagementKey(context.Background(), f.db, sec.Live)
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), agentGrantCtxKey{}, k)
	require.NoError(t, f.s.markAgentKey(ctx, f.user.TenantID, id))
	var storage, egress int64
	require.NoError(t, f.db.QueryRow(`SELECT storage_limit_bytes, egress_limit_bytes FROM api_keys WHERE id = $1`, id).Scan(&storage, &egress))
	assert.Equal(t, int64(1024), storage, "LEAST, never a replacement")
	assert.Equal(t, int64(2048), egress)
}

// RP90-12: a loopback redirect matches any port (RFC 8252 § 7.3).
func TestOAuth_LoopbackRedirectAnyPort(t *testing.T) {
	c := &auth.OAuthClient{RedirectURIs: []string{"http://127.0.0.1:8080/callback", "http://localhost/cb", "https://claude.ai/api/mcp/auth_callback", "http://[::1]:1/cb"}}
	for _, ok := range []string{"http://127.0.0.1:8080/callback", "http://127.0.0.1:53111/callback", "http://localhost:6001/cb", "http://[::1]:44000/cb",
		"https://claude.ai/api/mcp/auth_callback"} {
		assert.True(t, c.Allows(ok), ok)
	}
	for _, no := range []string{"http://127.0.0.1:53111/other", "http://127.0.0.2:8080/callback", "https://127.0.0.1:8080/callback",
		"https://claude.ai:8443/api/mcp/auth_callback", "http://localhost:6001/cb?x=1", "http://evil.example:8080/callback",
		"http://localhost.evil.example:6001/cb", "http://user@127.0.0.1:9/callback"} {
		assert.False(t, c.Allows(no), no)
	}
}

// RP90-13: an agent's S3 key stops when the account's agent_grants flag is off.
func TestAgent_S3KeyRefusedWhileTheFlagIsOff(t *testing.T) {
	f := setupEdgeFixture(t)
	_, ak, sk := f.agentS3Key()
	w := f.serve(f.signedAs(ak, sk, http.MethodPut, "/"+f.bucket+"/on.txt", nil, []byte("on"), nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, f.fl.Set(context.Background(), flagAgentGrants, f.tenantID, false, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	w = f.serve(f.signedAs(ak, sk, http.MethodPut, "/"+f.bucket+"/off.txt", nil, []byte("off"), nil))
	assert.Equal(t, http.StatusForbidden, w.Code, "the flag off stops the agent's S3 keys as its management key")
	w = f.serve(f.signedAs(f.ak, f.sk, http.MethodPut, "/"+f.bucket+"/person.txt", nil, []byte("p"), nil))
	assert.Equal(t, http.StatusOK, w.Code, "the person's key is untouched")
}

// RP90-16: the grants list names members: a person's.
func TestAgent_ListNeedsAPerson(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents()
	_, sec := f.agentGrant(auth.AgentGrantOptions{AgentName: "Claude"})
	code, out := f.call(http.MethodGet, "/api/v1/manage/agents", sec.Live, nil)
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "person_required", errCode(out))
	code, _ = f.call(http.MethodGet, "/api/v1/manage/agents", f.jwt, nil)
	assert.Equal(t, http.StatusOK, code)
}

// RP90-17: a suspended account claims nothing.
func TestClaim_SuspendedClaimerRefused(t *testing.T) {
	partner := setupPlatformFixture(t).withManagement().withApps(true, true)
	partner.s.registerPartnerRoutes()
	cleanupDescendants(partner)
	key := partner.partner()
	claimer := setupPlatformFixture(t).withManagement().withApps(true, false)
	claimer.s = partner.s
	require.NoError(t, partner.s.flags.Refresh(context.Background()))
	jwt, err := partner.s.auth.GenerateJWT(claimer.user)
	require.NoError(t, err)
	claimer.jwt = jwt
	cleanupDescendants(claimer)
	token := claimToken(partner.claimable(key, "suspended"))
	_, err = claimer.db.Exec(`UPDATE tenants SET suspended_at = NOW() WHERE id = $1`, claimer.user.TenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = claimer.db.Exec(`UPDATE tenants SET suspended_at = NULL WHERE id = $1`, claimer.user.TenantID)
	})
	code, out := claimer.call(http.MethodPost, "/api/v1/manage/claims/"+token, claimer.jwt, nil)
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "account_suspended", errCode(out))
}
