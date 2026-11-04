package api

// RP90-3 (Stage 1a Prompt 56 Part 0): /oauth/register and /oauth/token are
// public whatever the flag (RFC 7591 § 5 asks the server to limit them):
// per-IP limits on both (10 a minute, dashmw's login-limiter shape), a cap
// of clients registered per IP per day, and a retention rule for clients
// unused 30 days and codes after 24 hours.

import (
	"context"
	"fmt"
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

func TestOAuth_RegisterAndTokenAreRateLimited(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents()
	f.s.registerAgentOAuthRoutes()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM agent_oauth_clients WHERE client_name LIKE 'rp90-3%'`) })
	// Each registration a NEW client: since RP94-6 an identical one is the
	// same client and spends no token of the minute.
	n := 0
	register := func(ip string) *httptest.ResponseRecorder {
		n++
		r := httptest.NewRequest(http.MethodPost, "/oauth/register",
			strings.NewReader(fmt.Sprintf(`{"client_name":"rp90-3-%d","redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`, n)))
		r.RemoteAddr = ip + ":40000"
		w := httptest.NewRecorder()
		f.s.router.ServeHTTP(w, r)
		return w
	}
	token := func(ip string) *httptest.ResponseRecorder {
		form := url.Values{"grant_type": {"authorization_code"}, "code": {"nope"}, "client_id": {"x"}, "redirect_uri": {"https://x"}, "code_verifier": {"v"}}
		r := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = ip + ":40000"
		w := httptest.NewRecorder()
		f.s.router.ServeHTTP(w, r)
		return w
	}

	for i := 0; i < oauthPerMinute; i++ {
		require.Equal(t, http.StatusCreated, register("198.51.100.7").Code, "registration %d is within the limit", i+1)
	}
	w := register("198.51.100.7")
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "the 11th registration in a minute from one IP")
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
	assert.Contains(t, w.Body.String(), `"error"`, "an OAuth-shaped error")
	assert.Equal(t, http.StatusCreated, register("198.51.100.8").Code, "per IP: another address is not limited")

	for i := 0; i < oauthPerMinute; i++ {
		assert.NotEqual(t, http.StatusTooManyRequests, token("198.51.100.9").Code, "token %d is within the limit", i+1)
	}
	assert.Equal(t, http.StatusTooManyRequests, token("198.51.100.9").Code, "the 11th token request in a minute from one IP")

	// The day's cap counts clients, whatever the minute's pace — distinct
	// ones: an identical registration is the same client (RP92-4,
	// TestOAuth_ClaudeAIsSharedEgressIsNotOneClient).
	f.s.oauthRegisterLimiter = dashmw.NewRateLimiter(time.Minute, 1000)
	registerNamed := func(ip, name string) int {
		r := httptest.NewRequest(http.MethodPost, "/oauth/register",
			strings.NewReader(`{"client_name":"`+name+`","redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`))
		r.RemoteAddr = ip + ":40000"
		w := httptest.NewRecorder()
		f.s.router.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < oauthClientsPerIPPerDay; i++ {
		require.Equal(t, http.StatusCreated, registerNamed("198.51.100.10", fmt.Sprintf("rp90-3 %d", i)), "client %d of the day", i+1)
	}
	assert.Equal(t, http.StatusTooManyRequests, registerNamed("198.51.100.10", "rp90-3 over"), "one IP registers at most the day's cap of clients")
}

func TestRetention_OAuthClientsAndCodes(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents()
	ctx := context.Background()
	tag := uuid.NewString()[:8]
	old := time.Now().Add(-31 * 24 * time.Hour)
	ids := map[string]string{"stale": "rp903-stale-" + tag, "used": "rp903-used-" + tag, "granted": "rp903-granted-" + tag, "fresh": "rp903-fresh-" + tag}
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = f.db.Exec(`DELETE FROM agent_oauth_clients WHERE client_id = $1`, id)
		}
	})
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := f.db.Exec(q, args...)
		require.NoError(t, err, q)
	}
	for _, id := range ids {
		exec(`INSERT INTO agent_oauth_clients (client_id, client_name, redirect_uris, created_at) VALUES ($1, 'rp90-3', '{https://x/cb}', $2)`, id, old)
	}
	exec(`UPDATE agent_oauth_clients SET created_at = NOW() WHERE client_id = $1`, ids["fresh"])
	exec(`UPDATE agent_oauth_clients SET last_used_at = NOW() - interval '2 days' WHERE client_id = $1`, ids["used"])
	// A client a live grant names is kept however old: its connector renews through it.
	f.agentGrant(auth.AgentGrantOptions{AgentName: "Claude", ClientID: ids["granted"]})
	exec(`INSERT INTO agent_oauth_codes (code_hash, client_id, tenant_id, user_id, redirect_uri, code_challenge, created_at, expires_at)
		VALUES ($1, $2, $3, $4, 'https://x/cb', 'c', NOW() - interval '25 hours', NOW() - interval '24 hours'),
		       ($5, $2, $3, $4, 'https://x/cb', 'c', NOW(), NOW() + interval '10 minutes')`,
		strings.Repeat("a", 56)+tag, ids["fresh"], f.user.TenantID, f.user.ID, strings.Repeat("b", 56)+tag)

	j := NewRetentionJob(f.db, zap.NewNop())
	for _, p := range defaultRetentionPolicies() {
		if p.Table == "agent_oauth_clients" || p.Table == "agent_oauth_codes" {
			_, err := j.prune(ctx, p)
			require.NoError(t, err, p.Table)
		}
	}
	left := map[string]bool{}
	rows, err := f.db.Query(`SELECT client_id FROM agent_oauth_clients WHERE client_id = ANY($1)`, "{"+strings.Join([]string{ids["stale"], ids["used"], ids["granted"], ids["fresh"]}, ",")+"}")
	require.NoError(t, err)
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		left[id] = true
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	assert.False(t, left[ids["stale"]], "a client unused 30 days goes")
	assert.True(t, left[ids["used"]], "a client used this month stays")
	assert.True(t, left[ids["granted"]], "a client a live grant names stays")
	assert.True(t, left[ids["fresh"]], "a new client stays")
	var codes int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM agent_oauth_codes WHERE client_id = $1`, ids["fresh"]).Scan(&codes))
	assert.Equal(t, 1, codes, "a code goes 24 hours after it was made; the fresh one stays")
}
