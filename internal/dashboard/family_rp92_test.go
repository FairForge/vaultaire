package dashboard

// RP92-2 / RP92-6 on the dashboard (Stage 1a Prompt 57 Part 0): the Family
// page is the billing role's — a viewer of the organizer's team sees no
// member's address — and the join page names the account that joins: the
// person's own, never the team the session acts on.

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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/email"
	"github.com/FairForge/vaultaire/internal/flags"
)

func TestDashboard_FamilyViewerAndTheOwnAccount(t *testing.T) {
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
				`DELETE FROM audit_logs WHERE tenant_id = $1`, `DELETE FROM tenant_members WHERE tenant_id = $1 AND role <> 'owner'`} {
				_, _ = db.Exec(q, u.TenantID)
			}
		})
		return u
	}
	org, viewer, joiner := newPerson("famorg"), newPerson("famview"), newPerson("famjoin")
	fl := flags.New(db, logger)
	fl.Register("family", false)
	require.NoError(t, fl.Set(ctx, "family", org.TenantID, true, "test"))
	require.NoError(t, fl.Refresh(ctx))
	// The viewer, and the joiner, are viewers of the organizer's team.
	for _, u := range []*auth.User{viewer, joiner} {
		_, err := db.Exec(`INSERT INTO tenant_members (tenant_id, user_id, role) VALUES ($1, $2, 'viewer')`, org.TenantID, u.ID)
		require.NoError(t, err)
	}

	sessions := dashauth.NewMemoryStore()
	r := chi.NewRouter()
	RegisterRoutes(r, Deps{DB: db, Auth: authSvc, MFA: auth.NewMFAService("stored.ge"), Sessions: sessions, Logger: logger,
		Email: email.NewSender(logger), BaseURL: "http://localhost:8000", Flags: fl})
	onOrg := func(u *auth.User) *http.Cookie {
		tok, err := sessions.Create(ctx, dashauth.SessionData{UserID: u.ID, TenantID: org.TenantID, Email: u.Email, Role: "user"}, time.Hour)
		require.NoError(t, err)
		return &http.Cookie{Name: dashauth.SessionCookieName, Value: tok}
	}
	get := func(c *http.Cookie, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(c)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// A viewer of the organizer's team: no family page.
	w := get(onOrg(viewer), "/dashboard/family")
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), joiner.Email)

	// The joiner acts on the organizer's team; the page names THEIR account,
	// and joining attaches it.
	_, token, err := auth.InviteFamilyMember(ctx, db, org.TenantID, org.ID, joiner.Email, time.Now())
	require.NoError(t, err)
	c := onOrg(joiner)
	w = get(c, "/dashboard/family/join/"+token)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "Join with "+joiner.Email, "the page names the account that joins")
	m := regexp.MustCompile(`name="csrf_token" value="([0-9a-f]{64})"`).FindStringSubmatch(w.Body.String())
	require.NotNil(t, m)
	req := httptest.NewRequest(http.MethodPost, "/dashboard/family/join/"+token, strings.NewReader(url.Values{"csrf_token": {m[1]}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(c)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusSeeOther, w.Code, w.Body.String())
	var kind *string
	require.NoError(t, db.QueryRow(`SELECT child_kind FROM tenants WHERE id = $1`, joiner.TenantID).Scan(&kind))
	require.NotNil(t, kind)
	assert.Equal(t, "sponsored", *kind, "their own account joined")
	require.NoError(t, db.QueryRow(`SELECT child_kind FROM tenants WHERE id = $1`, org.TenantID).Scan(&kind))
	assert.Nil(t, kind, "never the team the session acts on")
}
