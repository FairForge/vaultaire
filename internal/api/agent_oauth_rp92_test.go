package api

// RP92-4 / RP92-5 (Stage 1a Prompt 57 Part 0): the OAuth caps are keyed on
// the peer address, and claude.ai — the one client the owner proved — comes
// from Anthropic's egress, not the user's. A registration repeated from the
// same network is the SAME client (no new row, nothing spent); the token
// limiter is per (client, network); and the per-day cap of clients lives in
// SQL (agent_oauth_clients.created_ip, migration 124) — an IPv6 peer counts
// by its /64, a flood or a restart resets nothing, and a refused body
// spends nothing.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	dashmw "github.com/FairForge/vaultaire/internal/dashboard/middleware"
)

// oauthPeer is a peer for one test run: an address no other run shares
// (the day's cap is rows in the shared test database).
func oauthPeer4() string {
	return fmt.Sprintf("198.18.%d.%d", rand.IntN(256), 1+rand.IntN(254))
}

func oauthNet6() string {
	return fmt.Sprintf("2001:db8:%x:%x", rand.IntN(0xffff), rand.IntN(0xffff))
}

func oauthRegister(t *testing.T, s *Server, peer, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(body))
	if strings.Contains(peer, ":") {
		r.RemoteAddr = "[" + peer + "]:40000"
	} else {
		r.RemoteAddr = peer + ":40000"
	}
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, r)
	return w
}

func registration(name string) string {
	return fmt.Sprintf(`{"client_name":%q,"redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`, name)
}

func cleanupOAuthClients(t *testing.T, f *platformFixture, prefix string) {
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM agent_oauth_clients WHERE client_name LIKE $1`, prefix+"%")
	})
}

func TestOAuth_ClaudeAIsSharedEgressIsNotOneClient(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents()
	f.s.registerAgentOAuthRoutes()
	// The minute's pace is not what this test is about.
	f.s.oauthRegisterLimiter = dashmw.NewRateLimiter(time.Minute, 1000)
	tag := "rp92-4-" + uuid.NewString()[:8]
	cleanupOAuthClients(t, f, tag)

	// 30 distinct registrations from one address in a day: 20 rows, then 429.
	peer := oauthPeer4()
	for i := 0; i < 30; i++ {
		w := oauthRegister(t, f.s, peer, registration(fmt.Sprintf("%s-distinct-%d", tag, i)))
		if i < oauthClientsPerIPPerDay {
			require.Equal(t, http.StatusCreated, w.Code, "client %d of the day: %s", i+1, w.Body)
		} else {
			assert.Equal(t, http.StatusTooManyRequests, w.Code, "client %d of the day", i+1)
		}
	}
	assert.Equal(t, oauthClientsPerIPPerDay, countRows(t, f.db, `SELECT COUNT(*) FROM agent_oauth_clients WHERE client_name LIKE $1`, tag+"-distinct-%"))

	// The same registration 30 times (every claude.ai user adding the
	// connector): one row, 30 × 201, one client_id.
	same := registration(tag + "-Claude")
	egress := oauthPeer4()
	var first string
	for i := 0; i < 30; i++ {
		w := oauthRegister(t, f.s, egress, same)
		require.Equal(t, http.StatusCreated, w.Code, "registration %d: %s", i+1, w.Body)
		var out map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		if i == 0 {
			first = out["client_id"].(string)
		}
		assert.Equal(t, first, out["client_id"], "registration %d is the same client", i+1)
	}
	// A client_name differing only in case and spaces is the same client.
	w := oauthRegister(t, f.s, egress, registration("  "+strings.ToUpper(tag)+"-CLAUDE "))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, first, out["client_id"], "normalized client_name")
	assert.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(*) FROM agent_oauth_clients WHERE lower(client_name) = lower($1)`, tag+"-Claude"), "one row")
	// The capped address's own first registration of it is a new client: refused.
	assert.Equal(t, http.StatusTooManyRequests, oauthRegister(t, f.s, peer, same).Code, "identical, but not from this address")

	// 11 token exchanges of 11 clients from one address in a minute: 200 each.
	const redirect = "https://claude.ai/api/mcp/auth_callback"
	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM agent_oauth_codes WHERE tenant_id = $1`, f.user.TenantID) })
	tokenPeer := oauthPeer4()
	for i := 0; i < oauthPerMinute+1; i++ {
		c, err := auth.RegisterOAuthClient(context.Background(), f.db, fmt.Sprintf("%s-token-%d", tag, i), []string{redirect}, oauthPeer4())
		require.NoError(t, err)
		code, err := auth.IssueAgentCode(context.Background(), f.db, auth.AgentConsent{
			AuthorizationRequest: auth.AuthorizationRequest{ClientID: c.ID, RedirectURI: redirect, CodeChallenge: challenge,
				CodeChallengeMethod: "S256", ResponseType: "code"},
			TenantID: f.user.TenantID, UserID: f.user.ID, AgentName: "Claude"})
		require.NoError(t, err)
		form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {c.ID}, "redirect_uri": {redirect}, "code_verifier": {verifier}}
		r := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = tokenPeer + ":40000"
		w := httptest.NewRecorder()
		f.s.router.ServeHTTP(w, r)
		assert.Equal(t, http.StatusOK, w.Code, "exchange %d (its own client) from one address: %s", i+1, w.Body)
	}
}

func TestOAuth_RegisterCapSurvivesAFloodAndARestart(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents()
	f.s.registerAgentOAuthRoutes()
	f.s.oauthRegisterLimiter = dashmw.NewRateLimiter(time.Minute, 1000)
	tag := "rp92-5-" + uuid.NewString()[:8]
	cleanupOAuthClients(t, f, tag)

	// The day's clients of one /64, from twenty of its addresses.
	net6 := oauthNet6()
	firstAddr := net6 + "::1"
	for i := 0; i < oauthClientsPerIPPerDay; i++ {
		addr := fmt.Sprintf("%s:%x::%x", net6, i, i+2)
		if i == 0 {
			addr = firstAddr
		}
		w := oauthRegister(t, f.s, addr, registration(fmt.Sprintf("%s-%d", tag, i)))
		require.Equal(t, http.StatusCreated, w.Code, "client %d: %s", i+1, w.Body)
	}

	// A flood: 20,001 addresses of the same /64, and 20,001 other networks,
	// through the in-memory limiters — no counter of the first is reset.
	for i := 0; i <= 20000; i++ {
		f.s.oauthRegisterLimiter.Allow(fmt.Sprintf("%s:%x:%x::1", net6, i>>16, i&0xffff))
		f.s.oauthRegisterLimiter.Allow(fmt.Sprintf("2001:db8:ffff:%x::1", i))
	}
	w := oauthRegister(t, f.s, firstAddr, registration(tag+"-21st"))
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "the /64's 21st client of the day: %s", w.Body)
	w = oauthRegister(t, f.s, net6+":abcd::9", registration(tag+"-21st-b"))
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "another address of the same /64: the same network")

	// A restart (a slot switch): a new Server on the same database.
	f2 := setupPlatformFixture(t).withManagement().withAgents()
	f2.s.registerAgentOAuthRoutes()
	w = oauthRegister(t, f2.s, firstAddr, registration(tag+"-after-restart"))
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "the cap is the database's, not the process's: %s", w.Body)
	assert.Equal(t, oauthClientsPerIPPerDay, countRows(t, f.db, `SELECT COUNT(*) FROM agent_oauth_clients WHERE client_name LIKE $1`, tag+"%"))
}

func TestOAuth_AMalformedRegistrationSpendsNothing(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents()
	f.s.registerAgentOAuthRoutes()
	f.s.oauthRegisterLimiter = dashmw.NewRateLimiter(time.Minute, 1000)
	tag := "rp92-5m-" + uuid.NewString()[:8]
	cleanupOAuthClients(t, f, tag)
	peer := oauthPeer4()

	for i := 0; i < 3*oauthClientsPerIPPerDay; i++ {
		body := `{"client_name":"` + tag + `","redirect_uris":["javascript:alert(1)"]}`
		if i%2 == 1 {
			body = `not json`
		}
		w := oauthRegister(t, f.s, peer, body)
		require.Equal(t, http.StatusBadRequest, w.Code, "refused body %d", i+1)
	}
	for i := 0; i < oauthClientsPerIPPerDay; i++ {
		w := oauthRegister(t, f.s, peer, registration(fmt.Sprintf("%s-%d", tag, i)))
		assert.Equal(t, http.StatusCreated, w.Code, "client %d after %d refused bodies: %s", i+1, 3*oauthClientsPerIPPerDay, w.Body)
	}
}

// RP92-5: the registering address is kept only as long as the day's cap
// needs it — blanked two days after the registration, the client kept.
func TestRetention_OAuthClientIPScrubbedAfterTwoDays(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents()
	tag := "rp92-5r-" + uuid.NewString()[:8]
	cleanupOAuthClients(t, f, tag)
	_, err := f.db.Exec(`INSERT INTO agent_oauth_clients (client_id, client_name, redirect_uris, created_at, created_ip)
		VALUES ($1, $1, '{https://x/cb}', NOW() - interval '3 days', '198.18.0.9'),
		       ($2, $2, '{https://x/cb}', NOW() - interval '1 day' + interval '1 hour', '198.18.0.9')`, tag+"-old", tag+"-new")
	require.NoError(t, err)
	j := NewRetentionJob(f.db, zap.NewNop())
	n := 0
	for _, p := range defaultRetentionPolicies() {
		if p.Table == "agent_oauth_clients" && p.Action == retentionScrubColumns {
			n++
			_, err := j.prune(context.Background(), p)
			require.NoError(t, err)
		}
	}
	require.Equal(t, 1, n, "one scrub policy for the clients' address")
	assert.Equal(t, 0, countRows(t, f.db, `SELECT COUNT(created_ip) FROM agent_oauth_clients WHERE client_id = $1`, tag+"-old"), "blanked after two days")
	assert.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(*) FROM agent_oauth_clients WHERE client_id = $1`, tag+"-old"), "the client is kept")
	assert.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(created_ip) FROM agent_oauth_clients WHERE client_id = $1`, tag+"-new"), "a day's registration keeps it")
}
