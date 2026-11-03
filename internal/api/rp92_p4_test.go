package api

// RP92-7 … RP92-12 (Stage 1a Prompt 57 Part 0, the P4s).

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/auth"
)

// RP92-7: RFC 8252 § 7.3's any-port rule is for loopback IP LITERALS; § 8.3
// says `localhost` is NOT RECOMMENDED (DNS rebinding) — a registered
// localhost URI matches exactly, as any other.
func TestOAuth_LocalhostIsNoAnyPortLoopback(t *testing.T) {
	c := &auth.OAuthClient{RedirectURIs: []string{"http://localhost:6001/cb", "http://127.0.0.1:8080/cb", "http://[::1]:1/cb"}}
	assert.True(t, c.Allows("http://localhost:6001/cb"), "exactly as registered")
	assert.False(t, c.Allows("http://localhost:6002/cb"), "localhost: no other port")
	assert.True(t, c.Allows("http://127.0.0.1:53111/cb"), "an IPv4 literal: any port")
	assert.True(t, c.Allows("http://[::1]:44000/cb"), "an IPv6 literal: any port")
}

// RP92-9: a re-invite of an address racing that address's join never
// deadlocks (Invite locks the address's invitation rows before the
// organizer's tenant row, the join's order).
func TestFamily_ReinviteRacingAJoinNeverDeadlocks(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withFamily()
	ctx := context.Background()
	deadlocked := func(err error) bool {
		var pqErr *pq.Error
		return errors.As(err, &pqErr) && pqErr.Code == "40P01"
	}
	// 20 rounds: two invitations each stays under the day's 50 per account
	// (MaxInvitationsPerDay); the deadlock showed in the first round or two.
	for i := 0; i < 20; i++ {
		p := f.person("famrace")
		tok, err := f.invite(p.user.Email)
		require.NoError(t, err)
		var wg sync.WaitGroup
		var invErr, joinErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, invErr = auth.InviteFamilyMember(ctx, f.db, f.user.TenantID, f.user.ID, p.user.Email, time.Now())
		}()
		go func() {
			defer wg.Done()
			_, joinErr = auth.AcceptFamilyInvitation(ctx, f.db, tok, p.user.ID, p.user.Email, time.Now())
		}()
		wg.Wait()
		require.False(t, deadlocked(invErr), "round %d: the re-invite deadlocked: %v", i, invErr)
		require.False(t, deadlocked(joinErr), "round %d: the join deadlocked: %v", i, joinErr)
		// Leave the family as it was for the next round (six people at most).
		_, _ = account.NewService(f.db, zap.NewNop()).LeaveFamily(ctx, p.user.TenantID, 5<<30)
		_, _ = f.db.Exec(`DELETE FROM tenant_invitations WHERE tenant_id = $1`, f.user.TenantID)
	}
}

// RP92-10: leaving is behind the family flag on the API as on the dashboard.
func TestFamily_LeaveIsBehindTheFlag(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withFamily()
	m := f.familyOf(4096)
	require.NoError(t, f.s.flags.Set(context.Background(), flagFamily, f.user.TenantID, false, "test"))
	require.NoError(t, f.s.flags.Refresh(context.Background()))
	code, out := f.call(http.MethodPost, "/api/v1/manage/family/leave", m.jwt, nil)
	assert.Equal(t, http.StatusNotFound, code, "flag off: no route, as every family route: %v", out)
	assert.Equal(t, "sponsored", f.childKind(m.user.TenantID), "still in the family")
}

// RP92-11: a join (or an invitation's accept) token never lands in the
// invitee's request log — the op names the route, the token is <token>.
func TestRequestLog_NeverHoldsAnInvitationToken(t *testing.T) {
	f := setupPlatformFixture(t).withManagement().withFamily()
	tr := NewS3AccessLogTracker(f.db)
	tr.SetRequestLog(func(string) bool { return true })
	f.s.accessLogTracker = tr
	p := f.person("famlog")
	tok, err := f.invite(p.user.Email)
	require.NoError(t, err)
	other := f.person("famlogother")

	// A refused join (another address), then a real one, then a bogus accept.
	code, _ := f.call(http.MethodPost, "/api/v1/manage/family/join/"+tok, other.jwt, nil)
	require.Equal(t, http.StatusForbidden, code)
	code, out := f.join(p, tok)
	require.Equal(t, http.StatusOK, code, out)
	f.call(http.MethodPost, "/api/v1/manage/invitations/"+tok+"/accept", other.jwt, nil)
	f.call(http.MethodPost, "/api/v1/manage/family/join/"+tok+"/x", other.jwt, nil) // no such route: the raw path
	tr.Flush()

	rows, err := f.db.Query(`SELECT op FROM request_log WHERE tenant_id = ANY($1)`, pq.Array([]string{p.user.TenantID, other.user.TenantID}))
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var op string
		require.NoError(t, rows.Scan(&op))
		n++
		assert.NotContains(t, op, tok, "a token in the request log")
	}
	require.NoError(t, rows.Err())
	assert.GreaterOrEqual(t, n, 3, "the requests were logged")
}

// RP92-12: invitations are not kept for ever — accepted or expired ones go
// 30 days later (retention), and an erased person's address leaves every
// invitation that names it, whatever tenant sent it.
func TestRetention_InvitationsAndTheErasedAddress(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	tag := uuid.NewString()[:8]
	ins := func(email string, expires time.Time, accepted *time.Time) string {
		var id string
		require.NoError(t, f.db.QueryRow(`INSERT INTO tenant_invitations (tenant_id, kind, email, role, token_hash, expires_at, accepted_at)
			VALUES ($1, 'member', $2, 'viewer', $3, $4, $5) RETURNING id`, f.user.TenantID, email, uuid.NewString(), expires, accepted).Scan(&id))
		return id
	}
	old := time.Now().Add(-31 * 24 * time.Hour)
	recent := time.Now().Add(-2 * 24 * time.Hour)
	expiredOld := ins("a-"+tag+"@x.example", old, nil)
	acceptedOld := ins("b-"+tag+"@x.example", old.Add(7*24*time.Hour), &old)
	expiredRecent := ins("c-"+tag+"@x.example", recent, nil)
	pending := ins("d-"+tag+"@x.example", time.Now().Add(24*time.Hour), nil)

	j := NewRetentionJob(f.db, zap.NewNop())
	n := 0
	for _, p := range defaultRetentionPolicies() {
		if p.Table == "tenant_invitations" {
			n++
			_, err := j.prune(context.Background(), p)
			require.NoError(t, err)
		}
	}
	require.Equal(t, 1, n, "one retention policy for invitations")
	left := func(id string) bool {
		return countRows(t, f.db, `SELECT COUNT(*) FROM tenant_invitations WHERE id = $1`, id) == 1
	}
	assert.False(t, left(expiredOld), "expired over 30 days ago")
	assert.False(t, left(acceptedOld), "accepted over 30 days ago")
	assert.True(t, left(expiredRecent), "expired this month: kept")
	assert.True(t, left(pending), "pending: kept")

	// The erasure: an invitation another tenant sent to the erased person.
	u, _, _, err := f.s.auth.CreateUserWithTenant(context.Background(), "erased-"+tag+"@family.example", "Str0ngPassw0rd!", "x")
	require.NoError(t, err)
	t.Cleanup(func() { cleanupPlatform(f.db, u.TenantID) })
	sent := ins(strings.ToUpper(u.Email), time.Now().Add(24*time.Hour), nil)
	_, err = f.db.Exec(`UPDATE users SET status = $2, deletion_scheduled_at = NOW() - interval '1 minute' WHERE id = $1`, u.ID, account.StatusPending)
	require.NoError(t, err)
	_, err = account.NewService(f.db, zap.NewNop()).EraseRows(context.Background(), u.ID, u.TenantID, u.Email, time.Now())
	require.NoError(t, err)
	assert.False(t, left(sent), "an invitation naming the erased address is erased with them")
	assert.True(t, left(pending), "another address's is not")
}
