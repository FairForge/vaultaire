package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

func testFlagsTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("admin").Parse(
		`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Flags{{end}}` +
			`{{define "content"}}` +
			`{{range .Flags}}` +
			`<span class="flag">{{.Key}}</span>` +
			`<span class="enabled">{{.Enabled}}</span>` +
			`{{range .Overrides}}<span class="override">{{.TenantID}}={{.Enabled}}</span>{{end}}` +
			`{{end}}` +
			`{{end}}`))
	return tmpl
}

func flagsSessionCtx(req *http.Request, role string) *http.Request {
	sd := &dashauth.SessionData{
		UserID: "u-1", TenantID: "t-1", Email: "admin@flags.test", Role: role,
	}
	return req.WithContext(context.WithValue(req.Context(), dashauth.SessionKey, sd))
}

func openHandlersFlagsDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())

	key := fmt.Sprintf("test-dash-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM feature_flags WHERE flag_key = $1", key)
	})
	return db, key
}

func TestHandleAdminFlags_RendersResolvedFlags(t *testing.T) {
	// Nil-DB service — the page must render from defaults alone.
	svc := flags.New(nil, zap.NewNop())
	svc.Register("chunking", true)

	req := flagsSessionCtx(httptest.NewRequest("GET", "/admin/flags", nil), "admin")
	rec := httptest.NewRecorder()
	HandleAdminFlags(testFlagsTemplate(t), svc, zap.NewNop())(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `<span class="flag">chunking</span>`)
	assert.Contains(t, body, `<span class="enabled">true</span>`)
}

func TestHandleAdminFlags_RedirectsWithoutSession(t *testing.T) {
	svc := flags.New(nil, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/flags", nil)
	rec := httptest.NewRecorder()
	HandleAdminFlags(testFlagsTemplate(t), svc, zap.NewNop())(rec, req)
	assert.Equal(t, http.StatusSeeOther, rec.Code)
}

func TestHandleAdminFlagSet_GlobalToggleRoundTrip(t *testing.T) {
	db, key := openHandlersFlagsDB(t)
	svc := flags.New(db, zap.NewNop())
	svc.Register(key, false)
	require.NoError(t, svc.Refresh(context.Background()))

	router := chi.NewRouter()
	router.Post("/admin/flags/{key}/set", HandleAdminFlagSet(svc, zap.NewNop()))
	router.Post("/admin/flags/{key}/clear", HandleAdminFlagClear(svc, zap.NewNop()))

	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = flagsSessionCtx(req, "admin")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// Toggle the global state on.
	rec := post("/admin/flags/"+key+"/set", url.Values{"enabled": {"true"}})
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.True(t, svc.Enabled(key, "any"))

	// Session email is recorded as updated_by.
	var updatedBy string
	require.NoError(t, db.QueryRow(`
		SELECT COALESCE(updated_by, '') FROM feature_flags
		WHERE flag_key = $1 AND tenant_id = '*'`, key).Scan(&updatedBy))
	assert.Equal(t, "admin@flags.test", updatedBy)

	// Per-tenant override off, then clear it.
	rec = post("/admin/flags/"+key+"/set", url.Values{"enabled": {"false"}, "tenant_id": {"tenant-dash"}})
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.False(t, svc.Enabled(key, "tenant-dash"))
	assert.True(t, svc.Enabled(key, "other"))

	rec = post("/admin/flags/"+key+"/clear", url.Values{"tenant_id": {"tenant-dash"}})
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.True(t, svc.Enabled(key, "tenant-dash"), "cleared override reverts to global")
}
