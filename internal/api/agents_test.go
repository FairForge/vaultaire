package api

// WP-AGENT-1 (Stage 1a Prompt 55 PR A; docs/ACCOUNTS.md § Agents; F-41): an
// agent grant IS a management key with a person behind it — a live key on
// the account and a test key on its sandbox, 30 days, renewable, never
// widened; the request log names the key, the audit trail the grant on
// behalf of its member.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/flags"
)

// withAgents turns the agent_grants flag on for the fixture's tenant.
func (f *platformFixture) withAgents() *platformFixture {
	f.t.Helper()
	if f.s.flags == nil {
		fl := flags.New(f.db, zap.NewNop())
		registerFlags(fl)
		f.s.flags = fl
	}
	require.NoError(f.t, f.s.flags.Set(context.Background(), flagAgentGrants, f.user.TenantID, true, "test"))
	require.NoError(f.t, f.s.flags.Refresh(context.Background()))
	f.t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM feature_flags WHERE tenant_id = $1`, f.user.TenantID)
		_, _ = f.db.Exec(`DELETE FROM agent_grants WHERE tenant_id = $1`, f.user.TenantID)
		_, _ = f.db.Exec(`DELETE FROM management_keys WHERE tenant_id = $1 OR tenant_id IN (SELECT id FROM tenants WHERE sandbox_of = $1)`, f.user.TenantID)
	})
	return f
}

func (f *platformFixture) agentGrant(o auth.AgentGrantOptions) (*auth.AgentGrant, auth.AgentGrantSecrets) {
	f.t.Helper()
	if o.GrantedBy == "" {
		o.GrantedBy = f.user.ID
	}
	g, sec, err := auth.MintAgentGrant(context.Background(), f.db, f.user.TenantID, o)
	require.NoError(f.t, err)
	return g, sec
}

func TestAgent_GrantExpires(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents()
	g, sec := f.agentGrant(auth.AgentGrantOptions{AgentName: "Claude", ClientID: "mcp_test"})
	assert.True(t, strings.HasPrefix(sec.Live, auth.ManagementKeyLivePrefix), "the grant's key")
	assert.True(t, strings.HasPrefix(sec.Test, auth.ManagementKeyTestPrefix), "and its rehearsal key on the sandbox")
	require.NotNil(t, g.ExpiresAt)
	assert.WithinDuration(t, time.Now().Add(30*24*time.Hour), *g.ExpiresAt, time.Minute, "30 days by default")

	code, out := f.call(http.MethodGet, "/api/v1/manage/buckets", sec.Live, nil)
	require.Equal(t, http.StatusOK, code, out)
	code, out = f.call(http.MethodGet, "/api/v1/manage/buckets", sec.Test, nil)
	require.Equal(t, http.StatusOK, code, "the test key acts on the sandbox: %v", out)

	// Past its lifetime it is refused at once.
	_, err := f.db.Exec(`UPDATE management_keys SET expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, g.ManagementKeyID)
	require.NoError(t, err)
	code, out = f.call(http.MethodGet, "/api/v1/manage/buckets", sec.Live, nil)
	assert.Equal(t, http.StatusUnauthorized, code, out)

	// The agent cannot renew itself; the person can, and it widens nothing.
	code, out = f.call(http.MethodPost, "/api/v1/manage/agents/"+g.ID+"/renew", sec.Test, nil)
	assert.Equal(t, http.StatusForbidden, code, out)
	assert.Equal(t, "person_required", errCode(out))
	code, out = f.call(http.MethodPost, "/api/v1/manage/agents/"+g.ID+"/renew", f.jwt, map[string]any{"scopes": []string{"platform"}})
	require.Equal(t, http.StatusOK, code, out)
	code, out = f.call(http.MethodGet, "/api/v1/manage/buckets", sec.Live, nil)
	require.Equal(t, http.StatusOK, code, "renewed: %v", out)
	for _, id := range []string{g.ManagementKeyID, g.TestManagementKeyID} {
		var scopes, ceiling string
		var expires time.Time
		require.NoError(t, f.db.QueryRow(`SELECT array_to_string(scopes, ','), COALESCE(s3_ceiling::text, ''), expires_at FROM management_keys WHERE id = $1`, id).
			Scan(&scopes, &ceiling, &expires))
		assert.Equal(t, auth.ScopeManageWrite, scopes, "a renewal never widens the scope")
		assert.NotContains(t, ceiling, `"DeleteBucket"`, "nor the ceiling")
		assert.WithinDuration(t, time.Now().Add(30*24*time.Hour), expires, time.Minute, "a fresh 30 days, both keys")
	}

	// Revoked: both keys at once.
	code, out = f.call(http.MethodDelete, "/api/v1/manage/agents/"+g.ID, f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	for _, k := range []string{sec.Live, sec.Test} {
		code, _ = f.call(http.MethodGet, "/api/v1/manage/buckets", k, nil)
		assert.Equal(t, http.StatusUnauthorized, code)
	}

	// Dark: with the flag off a grant's key authenticates nothing.
	_, sec2 := f.agentGrant(auth.AgentGrantOptions{AgentName: "other"})
	require.NoError(t, f.s.flags.Set(context.Background(), flagAgentGrants, f.user.TenantID, false, "test"))
	require.NoError(t, f.s.flags.Refresh(context.Background()))
	code, _ = f.call(http.MethodGet, "/api/v1/manage/buckets", sec2.Live, nil)
	assert.Equal(t, http.StatusUnauthorized, code)
}

func TestAgent_RequestLogNamesTheAgent(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents()
	tr := NewS3AccessLogTracker(f.db)
	tr.SetRequestLog(func(string) bool { return true })
	f.s.accessLogTracker = tr
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, f.user.TenantID) })
	g, sec := f.agentGrant(auth.AgentGrantOptions{AgentName: "Claude"})

	code, out := f.call(http.MethodPost, "/api/v1/manage/keys", sec.Live, map[string]any{"name": "agent's s3 key"})
	require.Equal(t, http.StatusCreated, code, out)
	keyID := out["id"].(string)
	code, out = f.call(http.MethodPut, "/api/v1/manage/keys/"+keyID+"/caps", sec.Live, map[string]any{"storage_limit_bytes": 1 << 20, "egress_limit_bytes": nil})
	require.Equal(t, http.StatusOK, code, out)
	tr.Flush()

	var logged string
	require.NoError(t, f.db.QueryRow(`SELECT key_id FROM request_log WHERE tenant_id = $1 AND op = 'POST /api/v1/manage/keys'
		ORDER BY logged_at DESC LIMIT 1`, f.user.TenantID).Scan(&logged))
	assert.Equal(t, "mk:"+g.ManagementKeyID, logged, "the request log names the key")

	page, err := audit.List(context.Background(), f.db, audit.Filter{TenantID: f.user.TenantID, Action: "key.caps_set", Limit: 5})
	require.NoError(t, err)
	require.NotEmpty(t, page.Rows)
	assert.Equal(t, "agent:"+g.ID, page.Rows[0].Principal, "the audit names the grant")
	assert.Equal(t, f.user.ID, page.Rows[0].PerformedBy, "on behalf of the member who granted it")
}

// The connect flow: a client registers (RFC 7591), the authorization
// endpoint sends the person to the dashboard's consent, the consent's code
// is exchanged ONCE (PKCE S256) for the agent grant's key.
func TestAgent_ConsentMintsOneGrant(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents()
	f.s.registerAgentOAuthRoutes()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM agent_oauth_codes WHERE tenant_id = $1`, f.user.TenantID) })
	const redirect = "https://claude.ai/api/mcp/auth_callback"

	code, out := f.call(http.MethodPost, "/oauth/register", "", map[string]any{"client_name": "Claude", "redirect_uris": []string{"javascript:alert(1)"}})
	assert.Equal(t, http.StatusBadRequest, code, out)
	code, out = f.call(http.MethodPost, "/oauth/register", "", map[string]any{"client_name": "Claude", "redirect_uris": []string{redirect}})
	require.Equal(t, http.StatusCreated, code, out)
	clientID := out["client_id"].(string)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM agent_oauth_clients WHERE client_id = $1`, clientID) })

	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "state": {"st"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	w := httptest.NewRecorder()
	f.s.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil))
	require.Equal(t, http.StatusFound, w.Code, w.Body.String())
	assert.True(t, strings.HasPrefix(w.Header().Get("Location"), "/dashboard/agents/connect?"), "the consent is the dashboard's")
	bad := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"https://evil.example/cb"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	w = httptest.NewRecorder()
	f.s.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+bad.Encode(), nil))
	assert.Equal(t, http.StatusBadRequest, w.Code, "an unregistered redirect is never followed")
	// RFC 9700 § 4.11.2: registration is open, so a client's redirect is
	// anyone's — a malformed request is answered here, never sent there.
	plain := url.Values{"response_type": {"token"}, "client_id": {clientID}, "redirect_uri": {redirect}, "state": {"st"}}
	w = httptest.NewRecorder()
	f.s.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+plain.Encode(), nil))
	assert.Equal(t, http.StatusBadRequest, w.Code, "a malformed request is never redirected")
	assert.Empty(t, w.Header().Get("Location"), "no automatic redirect to a self-registered address")

	consent := func() string {
		c, err := auth.IssueAgentCode(context.Background(), f.db, auth.AgentConsent{
			AuthorizationRequest: auth.AuthorizationRequest{ClientID: clientID, RedirectURI: redirect, CodeChallenge: challenge,
				CodeChallengeMethod: "S256", ResponseType: "code"},
			TenantID: f.user.TenantID, UserID: f.user.ID, AgentName: "Claude"})
		require.NoError(t, err)
		return c
	}
	token := func(c, v string) (int, map[string]any) {
		form := url.Values{"grant_type": {"authorization_code"}, "code": {c}, "client_id": {clientID}, "redirect_uri": {redirect}, "code_verifier": {v}}
		r := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		f.s.router.ServeHTTP(w, r)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	// A wrong verifier burns the code.
	c1 := consent()
	code, out = token(c1, strings.Repeat("x", 50))
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "invalid_grant", out["error"])
	code, out = token(c1, verifier)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "invalid_grant", out["error"], "a code is spent by its first exchange, right or wrong")

	c2 := consent()
	code, out = token(c2, verifier)
	require.Equal(t, http.StatusOK, code, out)
	live := out["access_token"].(string)
	assert.True(t, strings.HasPrefix(live, auth.ManagementKeyLivePrefix))
	assert.True(t, strings.HasPrefix(out["stored_test_token"].(string), auth.ManagementKeyTestPrefix))
	code, out = token(c2, verifier)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "invalid_grant", out["error"], "exchanged once")

	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM agent_grants WHERE tenant_id = $1`, f.user.TenantID).Scan(&n))
	assert.Equal(t, 1, n, "one consent, one grant")
	code, out = f.call(http.MethodGet, "/api/v1/manage/buckets", live, nil)
	assert.Equal(t, http.StatusOK, code, out)
	code, out = f.call(http.MethodGet, "/api/v1/manage/agents", f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	data := out["data"].([]any)
	require.Len(t, data, 1)
	assert.Equal(t, "Claude", data[0].(map[string]any)["agent_name"])
	assert.Equal(t, clientID, data[0].(map[string]any)["client_id"])
}

// An S3 key an agent mints is its grant's: the grant's caps, an expiry no
// later than the grant's, and revoked with it.
func TestAgent_ItsKeysCarryTheGrant(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents()
	limit := int64(5 << 20)
	g, sec := f.agentGrant(auth.AgentGrantOptions{AgentName: "Claude", StorageLimit: &limit, Lifetime: 24 * time.Hour})
	code, out := f.call(http.MethodPost, "/api/v1/manage/keys", sec.Live, map[string]any{"name": "agent's"})
	require.Equal(t, http.StatusCreated, code, out)
	id := out["id"].(string)
	var grant string
	var storage int64
	var expires time.Time
	var perms string
	require.NoError(t, f.db.QueryRow(`SELECT agent_grant_id::text, storage_limit_bytes, expires_at, permissions::text FROM api_keys WHERE id = $1`, id).
		Scan(&grant, &storage, &expires, &perms))
	assert.Equal(t, g.ID, grant)
	assert.Equal(t, limit, storage, "the grant's caps are its S3 keys' caps")
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), expires, time.Minute, "never outlives the grant")
	assert.NotContains(t, perms, `"DeleteBucket"`, "the agent ceiling")

	code, out = f.call(http.MethodDelete, "/api/v1/manage/agents/"+g.ID, f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	var revoked bool
	require.NoError(t, f.db.QueryRow(`SELECT revoked_at IS NOT NULL FROM api_keys WHERE id = $1`, id).Scan(&revoked))
	assert.True(t, revoked, "revoked with its grant")
}
