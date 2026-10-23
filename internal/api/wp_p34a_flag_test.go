package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/adminread"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
	"github.com/FairForge/vaultaire/internal/translog/logstore/logstoretest"
)

// Stage 1a Prompt 34a, Part 0a (RP44-2): the admin-read record's own switch.
// A per-tenant row can only turn translog_admin_reads ON, a per-tenant OFF
// under a global ON is refused, and every write of a translog_* flag row is
// a log entry appended BEFORE the row — refused, with nothing written, when
// the append fails.
//
// "Global ON" is modelled by a flags service of the test's own whose
// in-code default is ON (no '*' row is ever written on the shared database).

// useGuardedFlags gives the server fl with the server's write guard on it
// (the production wiring of NewServer).
func useGuardedFlags(s *Server, fl *flags.Service) { s.useFlags(fl) }

// globalOnFlags is a flags service over the shared database whose
// translog_admin_reads is ON globally by its default.
func globalOnFlags(t *testing.T, db *sql.DB) *flags.Service {
	t.Helper()
	fl := flags.New(db, zap.NewNop())
	registerFlags(fl)
	fl.Register(flagTranslogAdminReads, true)
	require.NoError(t, fl.Refresh(context.Background()))
	return fl
}

// flagRouter mounts the admin flag API as server.go does, the caller's
// admin identity in the context.
func flagRouter(s *Server, adminID string) chi.Router {
	r := chi.NewRouter()
	r.Route("/api/v1/admin", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx := context.WithValue(req.Context(), userIDKey, adminID)
				next.ServeHTTP(w, req.WithContext(context.WithValue(ctx, emailKey, testAdminEmail)))
			})
		})
		r.Put("/flags/{key}", s.handleAdminFlagSet)
		r.Delete("/flags/{key}", s.handleAdminFlagUnset)
	})
	return r
}

func putFlag(t *testing.T, r chi.Router, key, tenant string, enabled bool) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"enabled": enabled, "tenant_id": tenant})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/v1/admin/flags/"+key, bytes.NewReader(body)))
	return w
}

// dashFlagSet is the dashboard's POST /admin/flags/{key}/set (or clear).
func dashFlagSet(t *testing.T, fl *flags.Service, key, tenant string, enabled, clear bool) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"enabled": {map[bool]string{true: "true", false: "false"}[enabled]}, "tenant_id": {tenant}}
	req := httptest.NewRequest(http.MethodPost, "/admin/flags/"+key+"/set", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rc := chi.NewRouteContext()
	rc.URLParams.Add("key", key)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rc)
	ctx = context.WithValue(ctx, dashauth.SessionKey, &dashauth.SessionData{UserID: uuid.NewString(), Email: "root@stored.ge", Role: "admin"})
	w := httptest.NewRecorder()
	h := handlers.HandleAdminFlagSet(fl, zap.NewNop())
	if clear {
		h = handlers.HandleAdminFlagClear(fl, zap.NewNop())
	}
	h(w, req.WithContext(ctx))
	return w
}

func flagRow(t *testing.T, db *sql.DB, key, tenant string) (exists, enabled bool) {
	t.Helper()
	err := db.QueryRow(`SELECT enabled FROM feature_flags WHERE flag_key = $1 AND tenant_id = $2`, key, tenant).Scan(&enabled)
	if err == sql.ErrNoRows {
		return false, false
	}
	require.NoError(t, err)
	return true, enabled
}

// privateTranslog is a log of the test's own (migrations 103 and 104 on a
// throwaway database): the test may hold the log's lock.
func privateTranslog(t *testing.T) *sql.DB {
	t.Helper()
	db := logstoretest.PrivateLog(t)
	_, self, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(self), "..", "database", "migrations", "104_translog_subjects.sql")) // #nosec G304 -- the repository's migration
	require.NoError(t, err)
	_, err = db.Exec(string(raw))
	require.NoError(t, err)
	return db
}

// policyEntries is the tenant's log_policy entries as its own view lists them.
func policyEntries(t *testing.T, s *Server, tenant string) []map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/manage/account/transparency", nil)
	req = req.WithContext(context.WithValue(context.WithValue(req.Context(), tenantIDKey, tenant), userIDKey, "u"))
	w := httptest.NewRecorder()
	s.handleMgmtAccountTransparency(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var out struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	var policy []map[string]any
	for _, e := range out.Data {
		if e["kind"] == translog.KindLogPolicy {
			policy = append(policy, e)
		}
	}
	return policy
}

func TestAdminReadFlag_PerTenantOffUnderGlobalOn_Refused(t *testing.T) {
	// Arrange: translog_admin_reads ON globally.
	f := setupReadFixture(t, logstore.New(setupJobsFixture(t).db, nil))
	require.NoError(t, f.fl.Unset(context.Background(), flagTranslogAdminReads, f.tenant))
	fl := globalOnFlags(t, f.db)
	useGuardedFlags(f.server, fl)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM feature_flags WHERE tenant_id = $1`, f.tenant) })

	// Act: the API and the dashboard each try to exempt the tenant.
	api := putFlag(t, flagRouter(f.server, f.adminID), flagTranslogAdminReads, f.tenant, false)
	dash := dashFlagSet(t, fl, flagTranslogAdminReads, f.tenant, false, false)

	// Assert: refused, no row.
	assert.Equal(t, http.StatusConflict, api.Code, api.Body.String())
	assert.Contains(t, api.Body.String(), "flag_not_exemptible")
	assert.Equal(t, http.StatusConflict, dash.Code, dash.Body.String())
	exists, _ := flagRow(t, f.db, flagTranslogAdminReads, f.tenant)
	assert.False(t, exists, "a per-tenant OFF row under a global ON was written")
	assert.True(t, fl.Enabled(flagTranslogAdminReads, f.tenant))
}

func TestAdminReadFlag_PerTenantOffUnderGlobalOn_ReadStillLogged(t *testing.T) {
	// Arrange: global ON, and the tenant's OFF row written behind the door
	// (an old row, or SQL on the box).
	f := setupReadFixture(t, logstore.New(setupJobsFixture(t).db, nil))
	_, err := f.db.Exec(`UPDATE feature_flags SET enabled = false WHERE flag_key = $1 AND tenant_id = $2`, flagTranslogAdminReads, f.tenant)
	require.NoError(t, err)
	useGuardedFlags(f.server, globalOnFlags(t, f.db))

	// Act
	w, out := f.call(t, f.adminID, "GET", "/api/v1/admin/tenants/"+f.tenant+"/payments", nil)

	// Assert: served AND logged — ON wins.
	require.Equal(t, http.StatusOK, w.Code, out)
	found := f.tenantEntries(t)
	require.Len(t, found, 1, "the per-tenant OFF row exempted the tenant's read")
	assert.Equal(t, translog.ScopeTenantPayments, found[0].Scope)
}

func TestTranslogFlagChange_AppendsBeforeTheRow(t *testing.T) {
	// Arrange: a log of the test's own, whose lock the test holds.
	f := setupReadFixture(t, logstore.New(setupJobsFixture(t).db, nil))
	priv := privateTranslog(t)
	store := logstore.New(priv, nil)
	f.server.translog, f.server.adminReads = store, adminread.New(priv, store)
	require.NoError(t, f.fl.Unset(context.Background(), flagTranslogAdminReads, f.tenant))
	useGuardedFlags(f.server, f.fl)
	conn, err := priv.Conn(context.Background())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.ExecContext(context.Background(), `SELECT pg_advisory_lock(hashtextextended('translog', 0))`)
	require.NoError(t, err)

	// Act: the flip waits on the log.
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- putFlag(t, flagRouter(f.server, f.adminID), flagTranslogAdminReads, f.tenant, true) }()
	time.Sleep(400 * time.Millisecond)
	exists, _ := flagRow(t, f.db, flagTranslogAdminReads, f.tenant)
	_, err = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtextextended('translog', 0))`)
	require.NoError(t, err)
	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the flag write never finished")
	}

	// Assert: no row while the entry could not be appended; then both.
	assert.False(t, exists, "the row was written before its log entry")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	exists, enabled := flagRow(t, f.db, flagTranslogAdminReads, f.tenant)
	assert.True(t, exists && enabled)
	var n int
	require.NoError(t, priv.QueryRow(`SELECT COUNT(*) FROM translog_entries WHERE kind = 'log_policy'`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestTranslogFlagChange_LogFrozen_Refused_NothingWritten(t *testing.T) {
	// Arrange: the log frozen behind its high-water (RP42-1).
	db := setupJobsFixture(t).db
	frozen := logstore.New(db, nil).WithHighwater(frozenHighwater(t))
	f := setupReadFixture(t, logstore.New(db, nil))
	require.NoError(t, f.fl.Unset(context.Background(), flagTranslogAdminReads, f.tenant))
	f.server.translog, f.server.adminReads = frozen, adminread.New(db, frozen)
	useGuardedFlags(f.server, f.fl)
	rt := flagRouter(f.server, f.adminID)

	for _, key := range []string{flagTranslogAdminReads, flagTranslogCustomerView} {
		// Act: set through the API and the dashboard, clear through both.
		set := putFlag(t, rt, key, f.tenant, true)
		dash := dashFlagSet(t, f.fl, key, f.tenant, true, false)

		// Assert: 503, nothing written.
		assert.Equal(t, http.StatusServiceUnavailable, set.Code, set.Body.String())
		assert.Contains(t, set.Body.String(), "log_frozen")
		assert.Equal(t, http.StatusServiceUnavailable, dash.Code, dash.Body.String())
		exists, _ := flagRow(t, db, key, f.tenant)
		assert.False(t, exists, "%s: a row was written although its entry was refused", key)
	}

	// A row that exists is not removed either.
	_, err := db.Exec(`INSERT INTO feature_flags (flag_key, tenant_id, enabled, updated_by) VALUES ($1, $2, true, 'test')`, flagTranslogCustomerView, f.tenant)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM feature_flags WHERE tenant_id = $1`, f.tenant) })
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/flags/"+flagTranslogCustomerView+"?tenant_id="+f.tenant, nil)
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	clear := dashFlagSet(t, f.fl, flagTranslogCustomerView, f.tenant, false, true)
	assert.Equal(t, http.StatusServiceUnavailable, clear.Code)
	exists, _ := flagRow(t, db, flagTranslogCustomerView, f.tenant)
	assert.True(t, exists, "the row was removed although its entry was refused")
}

func TestTranslogFlagChange_TenantFindsItsOwn(t *testing.T) {
	// Arrange: two tenants; A's customer view on.
	ctx := context.Background()
	a := setupReadFixture(t, logstore.New(setupJobsFixture(t).db, nil))
	b := setupReadFixture(t, a.server.translog)
	useGuardedFlags(a.server, a.fl)
	useGuardedFlags(b.server, a.fl)
	for _, f := range []*readFixture{a, b} {
		require.NoError(t, a.fl.Set(ctx, flagTranslogCustomerView, f.tenant, true, "test"))
		tenant := f.tenant
		t.Cleanup(func() { _ = a.fl.Unset(ctx, flagTranslogCustomerView, tenant) })
	}
	rt := flagRouter(a.server, a.adminID)

	// Act: A's admin-read switch set OFF→ON again and cleared, through the API.
	require.Equal(t, http.StatusOK, putFlag(t, rt, flagTranslogAdminReads, a.tenant, true).Code)
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/v1/admin/flags/"+flagTranslogAdminReads+"?tenant_id="+a.tenant, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Assert: A finds both changes of its own switch; B none of them.
	var mine []map[string]any
	for _, e := range policyEntries(t, a.server, a.tenant) {
		if e["flag"] == flagTranslogAdminReads {
			mine = append(mine, e)
		}
	}
	require.Len(t, mine, 2, "A's switch changes in its own view")
	assert.Equal(t, "set", mine[0]["action"])
	assert.Equal(t, true, mine[0]["enabled"])
	assert.Equal(t, "clear", mine[1]["action"])
	for _, e := range policyEntries(t, b.server, b.tenant) {
		assert.NotEqual(t, flagTranslogAdminReads, e["flag"], "B sees A's switch")
	}
}

// RP43-6: a flag write answers the flag and the one row it wrote — never the
// other tenants' rows, which are GET /flags's flag_list read.
func TestFlagWrite_AnswersOnlyTheRowItWrote(t *testing.T) {
	f := setupReadFixture(t, logstore.New(setupJobsFixture(t).db, nil))
	useGuardedFlags(f.server, f.fl)
	other := setupReadFixture(t, f.server.translog) // another tenant with its own row of the flag
	rt := flagRouter(f.server, f.adminID)

	// Act
	w := putFlag(t, rt, flagTranslogAdminReads, f.tenant, true)

	// Assert
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), f.tenant)
	assert.NotContains(t, w.Body.String(), other.tenant, "the write answered another tenant's row")
	del := httptest.NewRecorder()
	rt.ServeHTTP(del, httptest.NewRequest(http.MethodDelete, "/api/v1/admin/flags/"+flagTranslogAdminReads+"?tenant_id="+f.tenant, nil))
	require.Equal(t, http.StatusOK, del.Code)
	assert.NotContains(t, del.Body.String(), other.tenant)
}
