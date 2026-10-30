package api

// RP84 (Stage 1a Prompt 53, Part 0): the review of WP-TEAM-1's findings on
// the members surfaces — no role on no tenant (RP84-2), the invitation
// caps (RP84-3), the keys a removed member made (RP84-4), one pending
// invitation per address (RP84-5).

import (
	"context"
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
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
)

// RP84-2: a JWT on NO tenant is no owner. It used to be one on the API
// (the dashboard already made it a viewer): a key on tenant ” answered 201.
// The invitation's accept still works for it — the invitation's address
// decides, not a membership it does not have yet.
func TestMembers_ATenantlessSignInIsNoOwner(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	f.s.registerUserAPIRoutes()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM tenant_invitations WHERE tenant_id = $1`, f.user.TenantID) })
	u, jwt := f.invitee("tenantless-" + uuid.NewString()[:8] + "@stored.ge")

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/user/apikeys"},
		{http.MethodPost, "/api/v1/manage/keys"},
		{http.MethodDelete, "/api/v1/user/"},
	} {
		code, out := f.call(c.method, c.path, jwt, map[string]any{"name": "nope", "reason": "x"})
		assert.Equal(t, http.StatusForbidden, code, "%s %s: %v", c.method, c.path, out)
		assert.Equal(t, "not_a_member", errCode(out), "%s %s", c.method, c.path)
	}
	var keys int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE user_id = $1`, u.ID).Scan(&keys))
	assert.Zero(t, keys, "no key was written on tenant ''")

	_, token, err := auth.CreateInvitation(context.Background(), f.db, f.user.TenantID, f.user.ID, u.Email, auth.RoleViewer, time.Now())
	require.NoError(t, err)
	code, out := f.call(http.MethodPost, "/api/v1/manage/invitations/"+token+"/accept", jwt, nil)
	assert.Equal(t, http.StatusOK, code, "the accept is the invitation's address's: %v", out)
	assert.Equal(t, "viewer", out["role"])
}

// RP84-2: the key generators never fall back to a cached tenant: "" is
// refused.
func TestMembers_KeyGeneratorsRefuseNoTenant(t *testing.T) {
	f := setupPlatformFixture(t)
	u, _ := f.invitee("nokeys-" + uuid.NewString()[:8] + "@stored.ge")
	_, err := f.s.auth.GenerateAPIKeyForTenant(context.Background(), "", u.ID, "k", nil)
	assert.ErrorIs(t, err, auth.ErrNoTenant)
	_, err = f.s.auth.GenerateTestKeyForTenant(context.Background(), "", u.ID, "k", nil)
	assert.ErrorIs(t, err, auth.ErrNoTenant)
}

// RP84-3 (b): invitations are bounded per tenant — at most 20 pending and
// at most 50 invitation mails a day — counted from rows (both slots and a
// restart agree), on the API (429 invitation_rate_limited) and the
// dashboard (a flash, no mail).
func TestMembers_InvitationsAreCapped(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	mail := &captureSender{}
	f.s.emailSender, f.s.baseURL = mail, "https://stored.example"
	rl, _ := f.s.jsonAPIMiddleware()
	rl.mu.Lock()
	rl.rps, rl.burst = 1000, 1000 // the request limiter is not the bound under test
	rl.mu.Unlock()
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM tenant_invitations WHERE tenant_id = $1`, f.user.TenantID)
		_, _ = f.db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, f.user.TenantID)
	})
	invite := func(addr string) (int, map[string]any) {
		return f.call(http.MethodPost, "/api/v1/manage/members", f.jwt, map[string]any{"email": addr, "role": "viewer"})
	}
	addr := func(i int) string { return "cap-" + uuid.NewString()[:6] + "-" + string(rune('a'+i%26)) + "@stored.ge" }

	var first string
	for i := 0; i < auth.MaxPendingInvitations; i++ {
		a := addr(i)
		if i == 0 {
			first = a
		}
		code, out := invite(a)
		require.Equal(t, http.StatusCreated, code, "invitation %d: %v", i, out)
	}
	code, out := invite(addr(99))
	assert.Equal(t, http.StatusTooManyRequests, code, out)
	assert.Equal(t, "invitation_rate_limited", errCode(out), "the 21st pending invitation")
	code, out = invite(first)
	assert.Equal(t, http.StatusCreated, code, "re-inviting a pending address replaces it, adding none: %v", out)

	// The day's mails: revoke to stay under the pending cap, then send on.
	_, err := f.db.Exec(`DELETE FROM tenant_invitations WHERE tenant_id = $1`, f.user.TenantID)
	require.NoError(t, err)
	var sent int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'member.invited'`,
		f.user.TenantID).Scan(&sent))
	for i := sent; i < auth.MaxInvitationsPerDay; i++ {
		if i%auth.MaxPendingInvitations == 0 {
			_, err := f.db.Exec(`DELETE FROM tenant_invitations WHERE tenant_id = $1`, f.user.TenantID)
			require.NoError(t, err)
		}
		code, out := invite(addr(i))
		require.Equal(t, http.StatusCreated, code, "mail %d: %v", i, out)
	}
	_, err = f.db.Exec(`DELETE FROM tenant_invitations WHERE tenant_id = $1`, f.user.TenantID)
	require.NoError(t, err)
	mails := len(mail.to)
	code, out = invite(addr(7))
	assert.Equal(t, http.StatusTooManyRequests, code, out)
	assert.Equal(t, "invitation_rate_limited", errCode(out), "the 51st mail of the day")
	assert.Equal(t, mails, len(mail.to), "no mail was sent")

	// The dashboard answers the same refusal with a flash, and sends nothing.
	req := httptest.NewRequest(http.MethodPost, "/dashboard/members", strings.NewReader(url.Values{
		"email": {addr(8)}, "role": {"viewer"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(dashauth.WithSession(req.Context(), &dashauth.SessionData{UserID: f.user.ID, TenantID: f.user.TenantID, Email: f.user.Email}))
	w := httptest.NewRecorder()
	handlers.HandleInviteMember(f.db, mail, "https://stored.example", zap.NewNop())(w, req)
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Contains(t, flashOf(w), "invitations", "the page says why")
	assert.Equal(t, mails, len(mail.to), "the dashboard sent nothing either")
	var pending int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM tenant_invitations WHERE tenant_id = $1`, f.user.TenantID).Scan(&pending))
	assert.Zero(t, pending, "no row was written past the cap")
}

// flashOf is the flash message a dashboard handler set.
func flashOf(w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == "flash" {
			if v, err := url.ParseQuery(c.Value); err == nil {
				for _, m := range v {
					return strings.Join(m, " ")
				}
			}
		}
	}
	return ""
}

// RP84-5: one pending invitation per address — a new one replaces it (the
// first link dies), the accept answers the role the member really has, and
// an address of any case that is a member is refused.
func TestMembers_ANewInvitationReplacesThePendingOne(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	ctx := context.Background()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM tenant_invitations WHERE tenant_id = $1`, f.user.TenantID) })
	address := "Twice-" + uuid.NewString()[:8] + "@Stored.ge"
	_, oldToken, err := auth.CreateInvitation(ctx, f.db, f.user.TenantID, f.user.ID, address, auth.RoleViewer, time.Now())
	require.NoError(t, err)
	_, newToken, err := auth.CreateInvitation(ctx, f.db, f.user.TenantID, f.user.ID, address, auth.RoleAdmin, time.Now())
	require.NoError(t, err)
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM tenant_invitations WHERE tenant_id = $1 AND accepted_at IS NULL`, f.user.TenantID).Scan(&n))
	assert.Equal(t, 1, n, "one pending invitation for the address")
	_, err = auth.InvitationByToken(ctx, f.db, oldToken, time.Now())
	assert.ErrorIs(t, err, auth.ErrInvitationNotFound, "the replaced link is dead")

	_, jwt := f.invitee(address) // registered with its mixed case
	code, out := f.call(http.MethodPost, "/api/v1/manage/invitations/"+newToken+"/accept", jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, "admin", out["role"])

	_, _, err = auth.CreateInvitation(ctx, f.db, f.user.TenantID, f.user.ID, address, auth.RoleViewer, time.Now())
	assert.ErrorIs(t, err, auth.ErrAlreadyMember, "a member of a mixed-case address is a member")

	// An accept that finds the person already a member answers the row's
	// role, never the invitation's.
	other := "already-" + uuid.NewString()[:8] + "@stored.ge"
	_, token, err := auth.CreateInvitation(ctx, f.db, f.user.TenantID, f.user.ID, other, auth.RoleAdmin, time.Now())
	require.NoError(t, err)
	v, vjwt := f.invitee(other)
	_, err = f.db.Exec(`INSERT INTO tenant_members (tenant_id, user_id, role) VALUES ($1, $2, 'viewer')`, f.user.TenantID, v.ID)
	require.NoError(t, err)
	code, out = f.call(http.MethodPost, "/api/v1/manage/invitations/"+token+"/accept", vjwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, "viewer", out["role"], "the member's actual role")
}

// RP84-4: a removed member keeps no secret silently — the removal names the
// keys it made (they are the tenant's, D-STRIPE-40, and stay live unless
// asked), and revoke_keys: true revokes them and their STS tokens in the
// removal's own transaction, with one audit row.
func TestMembers_RemovalNamesTheKeysTheMemberMade(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	ctx := context.Background()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM sts_tokens WHERE tenant_id = $1`, f.user.TenantID) })
	_, err := f.db.Exec(`UPDATE tenant_quotas SET tier = 'standard' WHERE tenant_id = $1`, f.user.TenantID)
	require.NoError(t, err) // past the free tier's key cap
	dev, devJWT := f.addMember(auth.RoleDeveloper)
	var made []string
	for _, name := range []string{"ci", "backup"} {
		code, out := f.call(http.MethodPost, "/api/v1/manage/keys", devJWT, map[string]any{"name": name})
		require.Equal(t, http.StatusCreated, code, out)
		made = append(made, out["id"].(string))
	}
	var parentKeyID string
	require.NoError(t, f.db.QueryRow(`SELECT key_id FROM api_keys WHERE id = $1`, made[0]).Scan(&parentKeyID))
	_, err = f.db.Exec(`INSERT INTO sts_tokens (access_key, secret_key, tenant_id, parent_key_id, expires_at)
		VALUES ($1, 'sk', $2, $3, NOW() + INTERVAL '1 hour')`, "ASIARM"+uuid.NewString()[:12], f.user.TenantID, parentKeyID)
	require.NoError(t, err)

	keyIDs := func(out map[string]any) []string {
		var ids []string
		list, _ := out["keys_made"].([]any)
		for _, k := range list {
			m := k.(map[string]any)
			assert.Contains(t, m, "name")
			assert.Contains(t, m, "last_used")
			ids = append(ids, m["id"].(string))
		}
		return ids
	}

	// Removed without revoke: named, still live.
	code, out := f.call(http.MethodDelete, "/api/v1/manage/members/"+dev.ID, f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.ElementsMatch(t, made, keyIDs(out), "the answer names every key the member made")
	assert.Equal(t, false, out["keys_revoked"])
	_, err = f.s.auth.GetOwnedAPIKey(ctx, f.user.TenantID, made[0])
	assert.NoError(t, err, "the tenant's keys stay live unless asked")

	// Back, then removed with revoke_keys: revoked, their tokens gone.
	_, err = f.db.Exec(`INSERT INTO tenant_members (tenant_id, user_id, role) VALUES ($1, $2, 'developer')`, f.user.TenantID, dev.ID)
	require.NoError(t, err)
	code, out = f.call(http.MethodDelete, "/api/v1/manage/members/"+dev.ID, f.jwt, map[string]any{"revoke_keys": true})
	require.Equal(t, http.StatusOK, code, out)
	assert.ElementsMatch(t, made, keyIDs(out))
	assert.Equal(t, true, out["keys_revoked"])
	var live, tokens, rows int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE tenant_id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		f.user.TenantID, dev.ID).Scan(&live))
	assert.Zero(t, live, "every key the member made is revoked")
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM sts_tokens WHERE parent_key_id = $1`, parentKeyID).Scan(&tokens))
	assert.Zero(t, tokens, "their STS tokens with them")
	for _, id := range made {
		_, err := f.s.auth.GetOwnedAPIKey(ctx, f.user.TenantID, id)
		assert.ErrorIs(t, err, auth.ErrKeyRevoked, "this process's key cache knows it")
	}
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'member.keys_revoked'`,
		f.user.TenantID).Scan(&rows))
	assert.Equal(t, 1, rows, "one audit row for the revocation")
}

// RP85-1 / RP85-2: the idempotency cache is keyed by the ACTING member too —
// the group's middleware runs before each route's role check, so a viewer
// presenting a developer's key got the developer's cached answer — and a
// replay says it is one (Idempotent-Replayed, Stripe's header) and, where a
// secret was blanked, secret_redacted.
func TestIdempotency_KeyedByTheActingMember(t *testing.T) {
	f := setupPlatformFixture(t).withManagement()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM idempotency_cache WHERE tenant_id = $1`, f.user.TenantID) })
	_, err := f.db.Exec(`UPDATE tenant_quotas SET tier = 'standard' WHERE tenant_id = $1`, f.user.TenantID)
	require.NoError(t, err)
	_, developer := f.addMember(auth.RoleDeveloper)
	_, viewer := f.addMember(auth.RoleViewer)
	key := "idem-" + uuid.NewString()
	post := func(jwt string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/manage/keys", strings.NewReader(`{"name":"ci"}`))
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		f.s.router.ServeHTTP(w, req)
		return w
	}

	first := post(developer)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	assert.Empty(t, first.Header().Get("Idempotent-Replayed"))

	w := post(viewer)
	assert.Equal(t, http.StatusForbidden, w.Code, "never the developer's cached answer: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "role_forbidden")
	assert.Empty(t, w.Header().Get("Idempotent-Replayed"))

	again := post(developer)
	require.Equal(t, http.StatusCreated, again.Code, again.Body.String())
	assert.Equal(t, "true", again.Header().Get("Idempotent-Replayed"))
	assert.Contains(t, again.Body.String(), `"secret_redacted":true`)
	var rows int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE tenant_id = $1 AND name = 'ci'`, f.user.TenantID).Scan(&rows))
	assert.Equal(t, 1, rows, "the replay made no second key")
}
