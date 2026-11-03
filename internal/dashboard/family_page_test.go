package dashboard

// WP-FAMILY-1 PR C (Stage 1a Prompt 56): the Family page. The organizer
// sees usage and caps — never a member's bucket or object — and invites
// (a fresh authenticator code), caps and removes; an invitee follows the
// join link (a sign-in returns to it) and joins with their own account; a
// member leaves. Every write goes through dashboardAction.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/email"
	"github.com/FairForge/vaultaire/internal/flags"
)

func TestDashboard_FamilyPage(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	logger := zap.NewNop()
	authSvc := auth.NewAuthService(nil, db)
	newPerson := func(prefix string) *auth.User {
		u, _, _, err := authSvc.CreateUserWithTenant(ctx, prefix+"-"+uuid.NewString()[:8]+"@family.example", "Str0ngPassw0rd!", "x")
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = db.Exec(`UPDATE tenants SET parent_tenant_id = NULL, child_kind = NULL, pool_root_id = id WHERE id = $1`, u.TenantID)
			for _, q := range []string{`DELETE FROM tenant_invitations WHERE tenant_id = $1`, `DELETE FROM feature_flags WHERE tenant_id = $1`,
				`DELETE FROM audit_logs WHERE tenant_id = $1`, `DELETE FROM object_head_cache WHERE tenant_id = $1`, `DELETE FROM buckets WHERE tenant_id = $1`} {
				_, _ = db.Exec(q, u.TenantID)
			}
		})
		return u
	}
	org, mem := newPerson("famorg"), newPerson("fammem")
	const secret = "JBSWY3DPEHPK3PXP"
	require.NoError(t, authSvc.EnableMFA(ctx, org.ID, secret, nil))
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM user_mfa WHERE user_id = $1`, org.ID) })
	fl := flags.New(db, logger)
	fl.Register("family", false)
	require.NoError(t, fl.Set(ctx, "family", org.TenantID, true, "test"))
	require.NoError(t, fl.Refresh(ctx))

	sessions := dashauth.NewMemoryStore()
	r := chi.NewRouter()
	RegisterRoutes(r, Deps{DB: db, Auth: authSvc, MFA: auth.NewMFAService("stored.ge"), Sessions: sessions, Logger: logger,
		Email: email.NewSender(logger), BaseURL: "http://localhost:8000", Flags: fl})
	cookie := func(u *auth.User) *http.Cookie {
		tok, err := sessions.Create(ctx, dashauth.SessionData{UserID: u.ID, TenantID: u.TenantID, Email: u.Email, Role: "user"}, time.Hour)
		require.NoError(t, err)
		return &http.Cookie{Name: dashauth.SessionCookieName, Value: tok}
	}
	orgC, memC := cookie(org), cookie(mem)
	csrfRe := regexp.MustCompile(`name="csrf_token" value="([0-9a-f]{64})"`)
	get := func(c *http.Cookie, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if c != nil {
			req.AddCookie(c)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	post := func(c *http.Cookie, path string, form url.Values) *httptest.ResponseRecorder {
		page := get(c, "/dashboard/family")
		m := csrfRe.FindStringSubmatch(page.Body.String())
		require.NotNil(t, m, "a CSRF token on the page: %s", page.Body.String())
		form.Set("csrf_token", m[1])
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(c)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	kind := func(tenant string) string {
		var k *string
		require.NoError(t, db.QueryRow(`SELECT child_kind FROM tenants WHERE id = $1`, tenant).Scan(&k))
		if k == nil {
			return ""
		}
		return *k
	}

	// No flag on the member's own account: no page.
	assert.Equal(t, http.StatusNotFound, get(memC, "/dashboard/family").Code)

	// The organizer invites with a fresh code; without one, nothing is sent.
	w := post(orgC, "/dashboard/family/invite", url.Values{"email": {mem.Email}, "totp_code": {"000000"}})
	require.Equal(t, http.StatusSeeOther, w.Code)
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM tenant_invitations WHERE tenant_id = $1`, org.TenantID).Scan(&n))
	assert.Zero(t, n, "no code, no invitation")
	code, err := totp.GenerateCode(secret, time.Now())
	require.NoError(t, err)
	w = post(orgC, "/dashboard/family/invite", url.Values{"email": {mem.Email}, "totp_code": {code}})
	require.Equal(t, http.StatusSeeOther, w.Code)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM tenant_invitations WHERE tenant_id = $1 AND kind = 'family'`, org.TenantID).Scan(&n))
	require.Equal(t, 1, n)

	// The link's token (the mail is a LogSender here): re-issued through the
	// one function, which replaces the pending invitation (the resend).
	_, token, err := auth.InviteFamilyMember(ctx, db, org.TenantID, org.ID, mem.Email, time.Now())
	require.NoError(t, err)
	join := "/dashboard/family/join/" + token

	// Signed out: to the sign-in, which returns to the link.
	w = get(nil, join)
	require.Equal(t, http.StatusSeeOther, w.Code)
	var back *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == dashauth.ReturnToCookie {
			back = c
		}
	}
	require.NotNil(t, back, "the join link is remembered for after the sign-in")

	// Signed in: the page says who invites and what they see.
	w = get(memC, join)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), org.Email)
	assert.Contains(t, w.Body.String(), "never your files")
	m := csrfRe.FindStringSubmatch(w.Body.String())
	require.NotNil(t, m)
	req := httptest.NewRequest(http.MethodPost, join, strings.NewReader(url.Values{"csrf_token": {m[1]}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(memC)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusSeeOther, w.Code, w.Body.String())
	assert.Equal(t, "sponsored", kind(mem.TenantID))

	// The member's library: the organizer's page shows bytes, never names.
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name) VALUES ($1, 'wedding-album')`, mem.TenantID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag) VALUES ($1, 'wedding-album', 'vows.mp4', 2048, 'e')`, mem.TenantID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE tenant_quotas SET storage_used_bytes = 2048 WHERE tenant_id = $1`, mem.TenantID)
	require.NoError(t, err)
	w = get(orgC, "/dashboard/family")
	require.Equal(t, http.StatusOK, w.Code)
	page := w.Body.String()
	assert.Contains(t, page, mem.Email)
	assert.Contains(t, page, "2 KB")
	assert.NotContains(t, page, "wedding-album")
	assert.NotContains(t, page, "vows.mp4")

	// A cap, then the removal: the member keeps its account.
	w = post(orgC, "/dashboard/family/members/"+mem.TenantID+"/cap", url.Values{"cap_gib": {"1"}})
	require.Equal(t, http.StatusSeeOther, w.Code)
	var limit int64
	require.NoError(t, db.QueryRow(`SELECT storage_limit_bytes FROM tenant_quotas WHERE tenant_id = $1`, mem.TenantID).Scan(&limit))
	assert.Equal(t, int64(1<<30), limit)
	w = get(memC, "/dashboard/family")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "You are in")
	assert.Contains(t, w.Body.String(), "Leave the family")
	w = post(orgC, "/dashboard/family/members/"+mem.TenantID+"/remove", url.Values{})
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "", kind(mem.TenantID), "detached")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, mem.TenantID).Scan(&n))
	assert.Equal(t, 1, n, "its library untouched")

	// Join again, then leave on its own.
	_, token, err = auth.InviteFamilyMember(ctx, db, org.TenantID, org.ID, mem.Email, time.Now())
	require.NoError(t, err)
	_, err = auth.AcceptFamilyInvitation(ctx, db, token, mem.ID, mem.Email, time.Now())
	require.NoError(t, err)
	w = post(memC, "/dashboard/family/leave", url.Values{})
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "", kind(mem.TenantID), "left")
}
