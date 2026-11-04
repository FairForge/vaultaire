package api

// RP94-6: the per-MINUTE register limiter was spent before the idempotent
// match (RP92-4 fixed the per-day cap and /oauth/token), so the 11th
// claude.ai user connecting inside a minute — every one from Anthropic's
// egress, every registration identical — was 429'd. The minute's token is
// spent only on a NEW client row now.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dashmw "github.com/FairForge/vaultaire/internal/dashboard/middleware"
)

func TestOAuth_ElevenIdenticalRegistrationsInAMinuteAreOneClient(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withAgents()
	f.s.registerAgentOAuthRoutes()
	f.s.oauthRegisterLimiter = dashmw.NewRateLimiter(time.Minute, 10) // the minute's ten
	tag := "rp94-6-" + uuid.NewString()[:8]
	cleanupOAuthClients(t, f, tag)

	egress := oauthPeer4()
	same := registration(tag + "-Claude")
	var first string
	for i := 0; i < 25; i++ {
		w := oauthRegister(t, f.s, egress, same)
		require.Equal(t, http.StatusCreated, w.Code, "identical registration %d: %s", i+1, w.Body)
		var out map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		if i == 0 {
			first = out["client_id"].(string)
		}
		assert.Equal(t, first, out["client_id"])
	}
	assert.Equal(t, 1, countRows(t, f.db, `SELECT COUNT(*) FROM agent_oauth_clients WHERE client_name LIKE $1`, tag+"%"))

	// New clients still spend the minute: nine more fit (the first was one),
	// the tenth new one is 429.
	for i := 0; i < 10; i++ {
		w := oauthRegister(t, f.s, egress, registration(fmt.Sprintf("%s-new-%d", tag, i)))
		if i < 9 {
			require.Equal(t, http.StatusCreated, w.Code, "new client %d: %s", i+1, w.Body)
		} else {
			assert.Equal(t, http.StatusTooManyRequests, w.Code, "the minute's eleventh new client")
		}
	}
}
