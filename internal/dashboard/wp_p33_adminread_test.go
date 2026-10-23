package dashboard

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
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
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/adminread"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
)

// K4c-1 (Stage 1a Prompt 33): the dashboard's admin pages that read customer
// metadata append their admin_read entry before they are served, and refuse
// when that fails.

// failingRecorder is a recorder whose append always fails (a frozen log, the
// database down).
type failingRecorder struct{ calls int }

func (f *failingRecorder) RecordAdminRead(context.Context, string, string, string) error {
	f.calls++
	return errors.New("the log is down")
}

// storeRecorder records into a real adminread store (the flag is the API
// server's business; here every read is logged).
type storeRecorder struct{ s *adminread.Store }

func (r storeRecorder) RecordAdminRead(ctx context.Context, actor, scope, tenant string) error {
	_, _, err := r.s.Record(ctx, actor, scope, tenant)
	return err
}

// adminRouter is the production router with an engine and a flags service
// (so every admin page is mounted) and an admin signed in with MFA.
func adminRouter(t *testing.T, rec AdminReadRecorder) (chi.Router, *http.Cookie) {
	t.Helper()
	logger := zap.NewNop()
	authSvc := auth.NewAuthService(nil, nil)
	sessions := dashauth.NewMemoryStore()
	r := chi.NewRouter()
	RegisterRoutes(r, Deps{Auth: authSvc, Sessions: sessions, Logger: logger, Email: email.NewSender(logger), BaseURL: "http://localhost:8000",
		Engine: engine.NewEngine(nil, logger, &engine.Config{DefaultBackend: "local"}), Flags: flags.New(nil, logger), AdminReads: rec})
	ctx := context.Background()
	u, err := authSvc.CreateUser(ctx, "root-"+uuid.NewString()[:8]+"@stored.ge", "securepass123")
	require.NoError(t, err)
	require.NoError(t, authSvc.EnableMFA(ctx, u.ID, "JBSWY3DPEHPK3PXP", nil))
	tok, err := sessions.Create(ctx, dashauth.SessionData{UserID: u.ID, TenantID: "t-admin", Email: u.Email, Role: "admin"}, time.Hour)
	require.NoError(t, err)
	return r, &http.Cookie{Name: dashauth.SessionCookieName, Value: tok}
}

// Every logged admin page refuses its read when the recorder fails — the
// gate is on the route as the router registers it — and an exempt page is
// served without asking the recorder; every classified page is mounted.
func TestAdminRead_EveryLoggedPageFailsClosed(t *testing.T) {
	rec := &failingRecorder{}
	r, admin := adminRouter(t, rec)
	mounted := map[string]bool{}
	require.NoError(t, chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet && strings.HasPrefix(route, "/admin") {
			mounted[strings.ReplaceAll(route, "/*/", "/")] = true
		}
		return nil
	}))
	for pattern, rule := range adminReadRoutes {
		require.True(t, mounted[pattern], "%s is classified but not mounted", pattern)
		path := strings.ReplaceAll(pattern, "{id}", "1")
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(admin)
		w := httptest.NewRecorder()
		before := rec.calls

		r.ServeHTTP(w, req)

		if rule.exempt != "" {
			assert.NotEqual(t, http.StatusServiceUnavailable, w.Code, "exempt %s", pattern)
			assert.Equal(t, before, rec.calls, "exempt %s asked the recorder", pattern)
			continue
		}
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, "%s served although its entry failed", pattern)
		assert.Contains(t, w.Body.String(), "transparency log", pattern)
	}
	for route := range mounted {
		_, ok := adminReadRoutes[route]
		assert.True(t, ok, "GET %s is mounted but not classified", route)
	}
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	return db
}

func TestAdminRead_TenantDetail_AppendsBeforeServing(t *testing.T) {
	// Arrange: the tenant page's gate around a probe that counts the
	// tenant's entries when it runs.
	db := openDB(t)
	store := adminread.New(db, logstore.New(db, nil))
	tenant := "tl-read-" + uuid.NewString()[:12]
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM translog_subjects WHERE tenant_id = $1`, tenant) })
	var atServe int
	r := chi.NewRouter()
	r.Route("/admin", func(ar chi.Router) {
		ar.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				sd := &dashauth.SessionData{UserID: "admin-1", Role: "admin"}
				next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), dashauth.SessionKey, sd)))
			})
		})
		adminGet(ar, Deps{DB: db, AdminReads: storeRecorder{store}, Logger: zap.NewNop()}, "/tenants/{id}", func(w http.ResponseWriter, req *http.Request) {
			found, err := store.Find(req.Context(), tenant)
			require.NoError(t, err)
			atServe = len(found)
		})
	})

	// Act: the page, then a refresh.
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/tenants/"+tenant, nil))
		require.Equal(t, http.StatusOK, w.Code)
	}

	// Assert: there before the page read anything; one entry for both.
	assert.Equal(t, 1, atServe)
	found, err := store.Find(context.Background(), tenant)
	require.NoError(t, err)
	require.Len(t, found, 1, "a refresh is coalesced")
	assert.Equal(t, translog.ScopeTenantDetail, found[0].Scope)
	var body string
	require.NoError(t, db.QueryRow(`SELECT convert_from(body, 'UTF8') FROM translog_entries WHERE idx = $1`, found[0].Index).Scan(&body))
	assert.NotContains(t, body, tenant)
	assert.NotContains(t, body, "admin-1")
}

// The exempt pages are exactly these (the inventory's verdicts,
// docs/reviews/WP-P33.md § K4c-1): moving a page that shows a tenant's
// identity to exempt is a change of this list, reviewed — never silent.
func TestAdminReadRoutes_ExemptAreTheAggregates(t *testing.T) {
	var exempt []string
	for p, r := range adminReadRoutes {
		if r.exempt != "" {
			exempt = append(exempt, p)
		}
	}
	assert.ElementsMatch(t, []string{"/admin/stats", "/admin/notifications/count", "/admin/backends"}, exempt)
}

// A failed append refuses the page: the handler never runs (the 503 alone
// would hide a handler that ran after it).
func TestAdminRead_LogDown_PageHandlerNeverRuns(t *testing.T) {
	ran := false
	r := chi.NewRouter()
	r.Route("/admin", func(ar chi.Router) {
		adminGet(ar, Deps{AdminReads: &failingRecorder{}, Logger: zap.NewNop()}, "/tenants/{id}", func(http.ResponseWriter, *http.Request) { ran = true })
	})
	w := httptest.NewRecorder()

	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/tenants/t-1", nil))

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.False(t, ran)
}
