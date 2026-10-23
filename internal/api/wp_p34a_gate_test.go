package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/dashboard"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/email"
	"github.com/FairForge/vaultaire/internal/payments"
	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
)

// Stage 1a Prompt 34a, Part 0a (RP44-1): the admin-read gate and the handler
// read ONE subject — whatever the handler reads is what the log holds — and
// a subject that names no tenant never lets the handler run unrecorded.

// recorderCall is one call of the recorder and its answer.
type recorderCall struct {
	scope, tenant string
	err           error
}

// spyRecorder wraps the API server's real recorder and keeps its calls.
type spyRecorder struct {
	s     *Server
	mu    sync.Mutex
	calls []recorderCall
}

func (r *spyRecorder) RecordAdminRead(ctx context.Context, actor, scope, tenant string) error {
	err := r.s.RecordAdminRead(ctx, actor, scope, tenant)
	r.mu.Lock()
	r.calls = append(r.calls, recorderCall{scope, tenant, err})
	r.mu.Unlock()
	return err
}

func (r *spyRecorder) take() []recorderCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

// gateFixture is the admin-read fixture (tenant with translog_admin_reads
// ON) plus marked records of that tenant on every subject page, the
// production dashboard router over the database with the spy as its
// recorder, and the API's admin routes with the spy as its hook.
type gateFixture struct {
	*readFixture
	spy        *spyRecorder
	dash, apiR chi.Router
	newAdmin   func() *http.Cookie
	marker     string // in the tenant's audit row, its abuse report and its e-mail
	user       string // a user of the tenant (tenants.email = users.email)
	abuseID    int64
}

func setupGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	f := setupReadFixture(t, logstore.New(setupJobsFixture(t).db, nil))
	ctx := context.Background()
	g := &gateFixture{readFixture: f, spy: &spyRecorder{s: f.server}, marker: "p34a-" + uuid.NewString()[:8]}

	// The tenant's records, each carrying the marker: an audit row, an
	// abuse report, a house (the payments page), a user of its own.
	g.user = uuid.NewString()
	_, err := f.db.Exec(`INSERT INTO users (id, email, password_hash, role) VALUES ($1, $2, 'x', 'user')`, g.user, f.tenant+"@test.local")
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO audit_logs (tenant_id, user_id, event_type, action, resource, result, severity, timestamp)
		VALUES ($1, $2, 'admin', 'test.p34a', $3, 'success', 'info', now())`, f.tenant, g.user, g.marker)
	require.NoError(t, err)
	require.NoError(t, f.db.QueryRow(`INSERT INTO abuse_reports (reporter_email, tenant_id, report_type, description)
		VALUES ('r@test.local', $1, 'spam', $2) RETURNING id`, f.tenant, g.marker).Scan(&g.abuseID))
	tx, err := f.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, payments.Grant(ctx, tx, f.tenant, payments.Order{Std: 1, Period: payments.PeriodAnnual},
		time.Now().Add(30*24*time.Hour), "manual", "", payments.RenewalPrepaid))
	require.NoError(t, tx.Commit())
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM abuse_reports WHERE id = $1`, g.abuseID)
		_, _ = f.db.Exec(`DELETE FROM users WHERE id = $1`, g.user)
		_, _ = f.db.Exec(`DELETE FROM audit_logs WHERE resource = $1`, g.marker)
	})

	// The production dashboard router, the API's recorder behind the spy.
	logger := zap.NewNop()
	authSvc := auth.NewAuthService(nil, nil)
	sessions := dashauth.NewMemoryStore()
	g.dash = chi.NewRouter()
	dashboard.RegisterRoutes(g.dash, dashboard.Deps{DB: f.db, Auth: authSvc, Sessions: sessions, Logger: logger,
		Email: email.NewSender(logger), BaseURL: "http://localhost:8000", AdminReads: g.spy})
	// Every page view is a new operator, so no read is coalesced into
	// another's entry: a logged read is a NEW entry.
	g.newAdmin = func() *http.Cookie {
		u, err := authSvc.CreateUser(ctx, "root-"+uuid.NewString()[:8]+"@stored.ge", "securepass123")
		require.NoError(t, err)
		require.NoError(t, authSvc.EnableMFA(ctx, u.ID, "JBSWY3DPEHPK3PXP", nil))
		tok, err := sessions.Create(ctx, dashauth.SessionData{UserID: u.ID, TenantID: "t-admin", Email: u.Email, Role: "admin"}, time.Hour)
		require.NoError(t, err)
		return &http.Cookie{Name: dashauth.SessionCookieName, Value: tok}
	}

	// The API's subject routes, as server.go registers them.
	f.server.adminReadHook = g.spy.RecordAdminRead
	g.apiR = chi.NewRouter()
	g.apiR.Route("/api/v1/admin", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), userIDKey, req.Header.Get("X-Test-User"))))
			})
		})
		r.Get("/audit", f.server.requireAdmin(f.server.adminRead("/api/v1/admin/audit", f.server.handleAdminAuditList)))
		f.server.registerAdminPaymentRoutes(r)
	})
	return g
}

// dashGet is one admin page view as a fresh operator.
func (g *gateFixture) dashGet(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.AddCookie(g.newAdmin())
	w := httptest.NewRecorder()
	g.dash.ServeHTTP(w, req)
	return w
}

func (g *gateFixture) apiGet(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("X-Test-User", g.adminID)
	w := httptest.NewRecorder()
	g.apiR.ServeHTTP(w, req)
	return w
}

// logged reports whether the calls recorded the tenant's read of scope,
// answered with no error, and the tenant can find an entry of that scope.
func (g *gateFixture) logged(t *testing.T, calls []recorderCall, scope string) bool {
	t.Helper()
	for _, c := range calls {
		if c.scope == scope && c.tenant == g.tenant && c.err == nil {
			for _, e := range g.tenantEntries(t) {
				if e.Scope == scope {
					return true
				}
			}
		}
	}
	return false
}

// The review's probe: the audit page and its export, filtered to the tenant
// by a padded or re-cased id. Each either logs the tenant's read or does not
// show the tenant's row.
func TestAdminRead_AuditFilterWhitespace_Logged(t *testing.T) {
	g := setupGateFixture(t)
	for _, v := range []string{g.tenant, " " + g.tenant, g.tenant + " ", "\t" + g.tenant, strings.ToUpper(g.tenant)} {
		for _, page := range []struct{ path, scope string }{
			{"/admin/audit", translog.ScopeTenantAudit},
			{"/admin/audit/export", translog.ScopeTenantAudit}, // its own scope since RP44-4: accepted below too
		} {
			// Act
			w := g.dashGet(t, page.path+"?tenant="+url.QueryEscape(v))
			calls := g.spy.take()

			// Assert
			shown := strings.Contains(w.Body.String(), g.marker)
			if !shown {
				continue
			}
			ok := g.logged(t, calls, page.scope) || g.logged(t, calls, "tenant_audit_export")
			assert.True(t, ok, "%s?tenant=%q showed the tenant's row (%d) and the log has no read of it: %+v", page.path, v, w.Code, calls)
		}
	}
}

// Every subject route, every way of writing its parameter: what the
// handler serves of the tenant is what was logged against the tenant.
func TestAdminRead_EverySubjectResolverAgreesWithItsHandler(t *testing.T) {
	g := setupGateFixture(t)
	id, aid := g.tenant, strconv.FormatInt(g.abuseID, 10)
	esc := url.PathEscape
	pathVariants := func(base, v, suffix string) []string {
		return []string{base + v + suffix, base + "%20" + v + suffix, base + v + "%20" + suffix, base + "%09" + v + suffix,
			base + "%00" + v + suffix, base + strings.ToUpper(v) + suffix, base + v + suffix + "/", base + esc(v) + suffix}
	}
	queryVariants := func(base, param, v string) []string {
		q := func(s string) string { return url.QueryEscape(s) }
		return []string{base + "?" + param + "=" + q(v), base + "?" + param + "=" + q(" "+v), base + "?" + param + "=" + q(v+" "),
			base + "?" + param + "=" + q("\t"+v), base + "?" + param + "=%00" + q(v), base + "?" + param + "=" + q(strings.ToUpper(v)),
			base + "?" + param + "=" + q(v) + "&" + param + "=other", base + "?" + param + "=other&" + param + "=" + q(v),
			base + "?" + param + "=" + q(v) + "%0A"}
	}
	type route struct {
		api     bool
		targets []string
		scopes  []string // the scope(s) the tenant's read is logged as
		shows   func(w *httptest.ResponseRecorder) bool
	}
	email := g.tenant + "@test.local"
	inBody := func(s string) func(*httptest.ResponseRecorder) bool {
		return func(w *httptest.ResponseRecorder) bool { return strings.Contains(w.Body.String(), s) }
	}
	routes := []route{
		{targets: pathVariants("/admin/tenants/", id, ""), scopes: []string{translog.ScopeTenantDetail}, shows: inBody(email)},
		{targets: pathVariants("/admin/support/", id, ""), scopes: []string{translog.ScopeSupportCase}, shows: inBody(email)},
		{targets: pathVariants("/admin/abuse/", aid, ""), scopes: []string{translog.ScopeAbuseCase}, shows: inBody(g.marker)},
		{targets: queryVariants("/admin/audit", "tenant", id), scopes: []string{translog.ScopeTenantAudit}, shows: inBody(g.marker)},
		{targets: queryVariants("/admin/audit", "user", g.user), scopes: []string{translog.ScopeTenantAudit}, shows: inBody(g.marker)},
		{targets: queryVariants("/admin/audit/export", "tenant", id), scopes: []string{translog.ScopeTenantAudit, "tenant_audit_export"}, shows: inBody(g.marker)},
		{targets: queryVariants("/admin/audit/export", "user", g.user), scopes: []string{translog.ScopeTenantAudit, "tenant_audit_export"}, shows: inBody(g.marker)},
		{targets: queryVariants("/admin/support", "q", id), scopes: []string{translog.ScopeSupportCase}, shows: inBody(email)},
		{api: true, targets: pathVariants("/api/v1/admin/tenants/", id, "/payments"), scopes: []string{translog.ScopeTenantPayments},
			shows: func(w *httptest.ResponseRecorder) bool {
				return w.Code == http.StatusOK && strings.Contains(w.Body.String(), `"provider":"manual"`)
			}},
		{api: true, targets: queryVariants("/api/v1/admin/audit", "tenant_id", id), scopes: []string{translog.ScopeTenantAudit}, shows: inBody(g.marker)},
		{api: true, targets: queryVariants("/api/v1/admin/audit", "user_id", g.user), scopes: []string{translog.ScopeTenantAudit}, shows: inBody(g.marker)},
	}
	for _, rt := range routes {
		for _, target := range rt.targets {
			// Act
			var w *httptest.ResponseRecorder
			if rt.api {
				w = g.apiGet(t, target)
			} else {
				w = g.dashGet(t, target)
			}
			calls := g.spy.take()

			// Assert: shown ⇒ logged against the tenant.
			if !rt.shows(w) {
				continue
			}
			ok := false
			for _, s := range rt.scopes {
				ok = ok || g.logged(t, calls, s)
			}
			assert.True(t, ok, "GET %s served the tenant's records (%d) and the log has no read of the tenant: %+v", target, w.Code, calls)
		}
	}
}

// A subject that is no tenant: the handler never runs (404), and nothing is
// read; the recorder is not left to answer "nothing to record".
func TestAdminRead_UnresolvedSubject_HandlerNeverRuns(t *testing.T) {
	g := setupGateFixture(t)
	g.server.adminReadHook = nil
	ran := false
	probe := g.server.adminRead("/api/v1/admin/tenants/{tenant_id}/payments", func(w http.ResponseWriter, r *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	})
	rt := chi.NewRouter()
	rt.Get("/api/v1/admin/tenants/{tenant_id}/payments", func(w http.ResponseWriter, r *http.Request) {
		probe(w, r.WithContext(context.WithValue(r.Context(), userIDKey, g.adminID)))
	})
	for _, missing := range []string{"tenant-" + uuid.NewString()[:12], "%20" + g.tenant, strings.ToUpper(g.tenant)} {
		ran = false
		w := httptest.NewRecorder()

		// Act
		rt.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/tenants/"+missing+"/payments", nil))

		// Assert
		assert.Equal(t, http.StatusNotFound, w.Code, missing)
		assert.False(t, ran, "the handler ran for %s, which is no tenant", missing)
	}
	// The recorder itself never answers nil for a subject that is no tenant.
	err := g.server.RecordAdminRead(context.Background(), g.adminID, translog.ScopeTenantDetail, "tenant-"+uuid.NewString()[:12])
	assert.Error(t, err)
	assert.False(t, errors.Is(err, logstore.ErrLogFrozen))
}

// RP44-4: the one-tenant audit export is its own entry — an export within
// the hour of a view, by the same operator, is in the log, not coalesced
// into the view's entry.
func TestAdminRead_AuditExportIsItsOwnScope(t *testing.T) {
	g := setupGateFixture(t)
	admin := g.newAdmin()
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path+"?tenant="+g.tenant, nil)
		req.AddCookie(admin)
		w := httptest.NewRecorder()
		g.dash.ServeHTTP(w, req)
		return w
	}

	// Act: a view, then an export, in the same hour.
	require.Equal(t, http.StatusOK, get("/admin/audit").Code)
	w := get("/admin/audit/export")

	// Assert: two entries, the second the export's.
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), g.marker)
	found := g.tenantEntries(t)
	require.Len(t, found, 2, "the export was coalesced into the view")
	assert.Equal(t, []string{translog.ScopeTenantAudit, translog.ScopeTenantAuditCSV}, []string{found[0].Scope, found[1].Scope})
}

// RP43-4: a support search that finds exactly one tenant is that tenant's
// support case; one that finds several is the tenant list.
func TestAdminRead_SupportSearchOfOneTenant_IsItsSupportCase(t *testing.T) {
	g := setupGateFixture(t)

	// Act
	one := g.dashGet(t, "/admin/support?q="+url.QueryEscape(g.tenant))
	oneCalls := g.spy.take()
	many := g.dashGet(t, "/admin/support?q="+url.QueryEscape("@test.local"))
	manyCalls := g.spy.take()

	// Assert
	require.Equal(t, http.StatusOK, one.Code)
	assert.True(t, g.logged(t, oneCalls, translog.ScopeSupportCase), "%+v", oneCalls)
	require.Equal(t, http.StatusOK, many.Code)
	require.Len(t, manyCalls, 1)
	assert.Equal(t, recorderCall{scope: translog.ScopeTenantList}, manyCalls[0])
}
