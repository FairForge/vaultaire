package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/dashboard"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/adminread"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
)

// K4c-1 (Stage 1a Prompt 33): an admin read of customer metadata writes to
// the transparency log — before the data is read, fail closed — and the
// tenant can find its own entries; the public cannot link them.

// TestAdminReadRoutes_AllClassified walks the whole router: every route of
// every method of the API's admin groups and of the dashboard's /admin
// pages has a classification (Stage 1a Prompt 34a, RP43-6: a route of
// another method than GET is a logged read — through the gate — or a
// mutation with its reason, so a POST that answers rows cannot ship
// unclassified), and every classification names a route that exists.
func TestAdminReadRoutes_AllClassified(t *testing.T) {
	srv := NewServer(&config.Config{Server: config.ServerConfig{Port: 0}}, zap.NewNop(),
		engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "local"}), nil, nil)
	dash, dashWrites := dashboard.AdminReadClassification(), dashboard.AdminWriteClassification()
	seen := map[string]bool{}
	require.NoError(t, chi.Walk(srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.ReplaceAll(route, "/*/", "/")
		key := route
		if method != http.MethodGet {
			key = method + " " + route
		}
		switch {
		case strings.HasPrefix(route, "/api/v1/admin/"), strings.HasPrefix(route, "/api/compliance/"):
			_, read := apiAdminReads[key]
			_, write := apiAdminWrites[key]
			assert.True(t, read != write, "%s %s: classified in exactly one of apiAdminReads (log/exempt) and apiAdminWrites (mutation)", method, route)
			if read {
				_, wrapped := adminReadWrapped.Load(key)
				assert.True(t, wrapped, "%s %s is not registered through adminRead", method, route)
			}
			if write {
				assert.True(t, strings.HasPrefix(apiAdminWrites[key], "mutation: "), "%s: a mutation states its reason", key)
			}
		case route == "/admin" || strings.HasPrefix(route, "/admin/"):
			if method == http.MethodGet {
				_, ok := dash[route]
				assert.True(t, ok, "GET %s is not classified in the dashboard's adminReadRoutes", route)
				assert.True(t, dashboard.AdminReadWrapped(route), "GET %s is not registered through adminGet", route)
			} else {
				assert.True(t, strings.HasPrefix(dashWrites[key], "mutation: "), "%s is not classified in the dashboard's adminWriteRoutes", key)
			}
		default:
			return nil
		}
		seen[key] = true
		return nil
	}))
	for p := range apiAdminReads {
		assert.True(t, seen[p], "apiAdminReads names %s, which is no route", p)
	}
	for p := range apiAdminWrites {
		assert.True(t, seen[p], "apiAdminWrites names %s, which is no route", p)
	}
	for p := range dash {
		if p == "/admin/flags" || p == "/admin/backends" {
			continue // mounted only with a flags service / an engine — checked in the dashboard package
		}
		assert.True(t, seen[p], "adminReadRoutes names %s, which is no GET route", p)
	}
	for p := range dashWrites {
		if strings.Contains(p, "/admin/flags/") || strings.Contains(p, "/admin/backends/") {
			continue // mounted only with a flags service / an engine — checked in the dashboard package
		}
		assert.True(t, seen[p], "adminWriteRoutes names %s, which is no route", p)
	}
	assert.Greater(t, len(seen), 75, "the walk saw the admin routes of every method")
}

// readFixture is the admin money routes' fixture with a log, the recorder
// and the flags; the tenant has translog_admin_reads ON (its own row).
type readFixture struct {
	*adminPayFixture
	fl *flags.Service
}

func setupReadFixture(t *testing.T, store *logstore.Store) *readFixture {
	t.Helper()
	f := setupAdminPayFixture(t)
	fl := flags.New(f.db, zap.NewNop())
	registerFlags(fl)
	f.server.flags = fl
	f.server.translog = store
	f.server.adminReads = adminread.New(f.db, store)
	require.NoError(t, fl.Set(context.Background(), flagTranslogAdminReads, f.tenant, true, "test"))
	t.Cleanup(func() {
		_ = fl.Unset(context.Background(), flagTranslogAdminReads, f.tenant)
		_, _ = f.db.Exec(`DELETE FROM translog_subjects WHERE tenant_id = $1`, f.tenant)
		_, _ = f.db.Exec(`DELETE FROM audit_logs WHERE user_id = $1`, f.adminID)
	})
	return &readFixture{adminPayFixture: f, fl: fl}
}

// tenantEntries is the tenant's admin_read entries, found the way the
// customer endpoint finds them.
func (f *readFixture) tenantEntries(t *testing.T) []adminread.Found {
	t.Helper()
	found, err := f.server.adminReads.Find(context.Background(), f.tenant)
	require.NoError(t, err)
	return found
}

// The API's one-tenant read (the payments ledger): the entry is appended
// before the handler reads anything, names no one, and the operator is in
// audit_logs with the entry's index. (The dashboard's tenant page is the
// same gate: internal/dashboard TestAdminRead_TenantDetail_AppendsBeforeServing.)
func TestAdminRead_TenantPayments_AppendsBeforeServing(t *testing.T) {
	// Arrange: the route's handler wrapped by a probe that counts the
	// tenant's entries when it runs.
	f := setupReadFixture(t, logstore.New(setupJobsFixture(t).db, nil))
	var atServe int
	probe := f.server.adminRead("/api/v1/admin/tenants/{tenant_id}/payments", func(w http.ResponseWriter, r *http.Request) {
		atServe = len(f.tenantEntries(t))
		f.server.handleAdminTenantPayments(w, r)
	})
	rt := chi.NewRouter()
	rt.Get("/api/v1/admin/tenants/{tenant_id}/payments", func(w http.ResponseWriter, r *http.Request) {
		probe(w, r.WithContext(context.WithValue(r.Context(), userIDKey, f.adminID)))
	})

	// Act
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/tenants/"+f.tenant+"/payments", nil))

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, atServe, "appended BEFORE the read")
	found := f.tenantEntries(t)
	require.Len(t, found, 1)
	assert.Equal(t, translog.ScopeTenantPayments, found[0].Scope)
	var body string
	require.NoError(t, f.db.QueryRow(`SELECT convert_from(body, 'UTF8') FROM translog_entries WHERE idx = $1`, found[0].Index).Scan(&body))
	for _, who := range []string{f.tenant, f.adminID, "@test.local"} {
		assert.NotContains(t, body, who)
	}
	var meta string
	require.NoError(t, f.db.QueryRow(`SELECT metadata::text FROM audit_logs WHERE user_id = $1 AND action = 'admin.read'`, f.adminID).Scan(&meta))
	assert.Contains(t, meta, `"scope": "tenant_payments"`)
	assert.Contains(t, meta, `"translog_index"`)

	// A refresh within the hour appends nothing (and no second audit row).
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/tenants/"+f.tenant+"/payments", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Len(t, f.tenantEntries(t), 1)
	var audits int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE user_id = $1 AND action = 'admin.read'`, f.adminID).Scan(&audits))
	assert.Equal(t, 1, audits)
}

func TestAdminRead_LogFrozen_ReadRefused(t *testing.T) {
	// Arrange: the log frozen behind its high-water (RP42-1).
	db := setupJobsFixture(t).db
	f := setupReadFixture(t, logstore.New(db, nil).WithHighwater(frozenHighwater(t)))

	// Act: the API's payments read, through the production registration.
	w, out := f.call(t, f.adminID, "GET", "/api/v1/admin/tenants/"+f.tenant+"/payments", nil)

	// Assert: refused, nothing read, nothing appended.
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "log_frozen", out["error"])
	assert.Nil(t, out["entitlement"], "the payments were not read")
	assert.Empty(t, f.tenantEntries(t))

	// Any other failure of the recorder refuses too.
	f.server.adminReadHook = func(context.Context, string, string, string) error { return errors.New("the database is down") }
	w, out = f.call(t, f.adminID, "GET", "/api/v1/admin/tenants/"+f.tenant+"/payments", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "admin_read_not_logged", out["error"])
	f.server.adminReadHook = nil

	// Flag OFF for the tenant: today's behaviour — no entry, no refusal.
	require.NoError(t, f.fl.Unset(context.Background(), flagTranslogAdminReads, f.tenant))
	w, _ = f.call(t, f.adminID, "GET", "/api/v1/admin/tenants/"+f.tenant+"/payments", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, f.tenantEntries(t))
}

func TestCustomerTransparency_FindsOwnEntriesOnly(t *testing.T) {
	// Arrange: tenants A and B, read by the admin (A twice, B once).
	ctx := context.Background()
	a := setupReadFixture(t, logstore.New(setupJobsFixture(t).db, nil))
	b := setupReadFixture(t, a.server.translog)
	for _, f := range []*readFixture{a, b} {
		require.NoError(t, a.fl.Set(ctx, flagTranslogCustomerView, f.tenant, true, "test"))
		tenant := f.tenant
		t.Cleanup(func() { _ = a.fl.Unset(ctx, flagTranslogCustomerView, tenant) })
	}
	require.NoError(t, a.server.RecordAdminRead(ctx, a.adminID, translog.ScopeTenantDetail, a.tenant))
	require.NoError(t, a.server.RecordAdminRead(ctx, a.adminID, translog.ScopeTenantPayments, a.tenant))
	require.NoError(t, a.server.RecordAdminRead(ctx, a.adminID, translog.ScopeSupportCase, b.tenant))
	get := func(tenant string) (int, []adminread.Found) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/manage/account/transparency", nil)
		req = req.WithContext(context.WithValue(context.WithValue(req.Context(), tenantIDKey, tenant), userIDKey, "u"))
		w := httptest.NewRecorder()
		a.server.handleMgmtAccountTransparency(w, req)
		var out struct {
			Data   []adminread.Found `json:"data"`
			Origin string            `json:"origin"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code == http.StatusOK {
			assert.Equal(t, translog.Origin, out.Origin)
		}
		return w.Code, out.Data
	}

	// Act + Assert: each tenant sees its own entries only.
	code, mine := get(a.tenant)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, mine, 2)
	assert.Equal(t, []string{translog.ScopeTenantDetail, translog.ScopeTenantPayments}, []string{mine[0].Scope, mine[1].Scope})
	code, theirs := get(b.tenant)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, theirs, 1)
	assert.Equal(t, translog.ScopeSupportCase, theirs[0].Scope)
	assert.NotEqual(t, mine[0].Index, theirs[0].Index)

	// Dark for a tenant without translog_customer_view; empty for one never read.
	code, _ = get("tenant-" + uuid.NewString())
	assert.Equal(t, http.StatusNotFound, code)
	require.NoError(t, a.fl.Set(ctx, flagTranslogCustomerView, "never-read-"+a.tenant, true, "test"))
	t.Cleanup(func() { _ = a.fl.Unset(ctx, flagTranslogCustomerView, "never-read-"+a.tenant) })
	code, none := get("never-read-" + a.tenant)
	assert.Equal(t, http.StatusOK, code)
	assert.Empty(t, none)
}

// The exempt API admin reads are exactly these (the inventory's verdicts).
func TestAdminReadRoutes_APIExemptAreTheAggregates(t *testing.T) {
	var logged []string
	for p, r := range apiAdminReads {
		if r.exempt == "" {
			logged = append(logged, p)
		}
	}
	assert.ElementsMatch(t, []string{"/api/v1/admin/tenants/{tenant_id}/payments", "/api/v1/admin/audit", "/api/v1/admin/jobs",
		"/api/v1/admin/flags",
		// RP43-6 (Stage 1a Prompt 34a): the routes of another method that answer customer data.
		"POST /api/v1/admin/routing-truth/resolve-null", "POST /api/v1/admin/chunk-orphans", "POST /api/v1/admin/chunk-move",
		"POST /api/v1/admin/tenants/{tenant_id}/payments", "POST /api/v1/admin/tenants/{tenant_id}/entitlement",
		"POST /api/v1/admin/tenants/{tenant_id}/refunds", "POST /api/v1/admin/payments/stripe/stop-renewals",
		"POST /api/v1/admin/payments/stripe/resume-renewals",
	}, logged, "every other admin route is an aggregate, infrastructure, scaffolding or a mutation")
}
