package handlers

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	_ "github.com/lib/pq"
)

func testBucketSettingsTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("base").Parse(
		`{{define "base"}}{{block "nav" .}}{{end}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Settings{{end}}` +
			`{{define "content"}}` +
			`<h1>{{.BucketName}} Settings</h1>` +
			`<span class="vis">{{.Visibility}}</span>` +
			`{{if .CDNBaseURL}}<span class="cdn">{{.CDNBaseURL}}</span>{{end}}` +
			`{{if .ArchiveRestricted}}<span class="archive">restricted</span>{{end}}` +
			`{{if .ArchiveReason}}<span class="reason">{{.ArchiveReason}}</span>{{end}}` +
			`<span class="cors">{{.CORSOrigins}}</span>` +
			`<span class="cache">{{.CacheMaxAge}}</span>` +
			`{{if .FlashSuccess}}<span class="flash-ok">{{.FlashSuccess}}</span>{{end}}` +
			`{{if .FlashError}}<span class="flash-err">{{.FlashError}}</span>{{end}}` +
			`{{end}}`))
	return tmpl
}

func injectBucketRoute(req *http.Request, name string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", name)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// --- Unit tests (no DB) ---

func TestHandleBucketSettings_NoSession(t *testing.T) {
	tmpl := testBucketSettingsTemplate(t)
	handler := HandleBucketSettings(tmpl, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/buckets/my-bucket/settings", nil)
	req = injectBucketRoute(req, "my-bucket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleBucketSettings_NoDB(t *testing.T) {
	tmpl := testBucketSettingsTemplate(t)
	handler := HandleBucketSettings(tmpl, nil, zap.NewNop())

	req := injectSession(httptest.NewRequest("GET", "/dashboard/buckets/my-bucket/settings", nil))
	req = injectBucketRoute(req, "my-bucket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "my-bucket Settings")
	assert.Contains(t, body, `<span class="vis">private</span>`)
}

func TestHandleBucketSettings_EmptyName(t *testing.T) {
	tmpl := testBucketSettingsTemplate(t)
	handler := HandleBucketSettings(tmpl, nil, zap.NewNop())

	req := injectSession(httptest.NewRequest("GET", "/dashboard/buckets//settings", nil))
	req = injectBucketRoute(req, "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/buckets", w.Header().Get("Location"))
}

func TestHandleUpdateBucketSettings_NoSession(t *testing.T) {
	tmpl := testBucketSettingsTemplate(t)
	handler := HandleUpdateBucketSettings(tmpl, nil, zap.NewNop())

	form := url.Values{"visibility": {"public-read"}}
	req := httptest.NewRequest("POST", "/dashboard/buckets/my-bucket/settings",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = injectBucketRoute(req, "my-bucket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleUpdateBucketSettings_InvalidVisibility(t *testing.T) {
	tmpl := testBucketSettingsTemplate(t)
	handler := HandleUpdateBucketSettings(tmpl, nil, zap.NewNop())

	form := url.Values{"visibility": {"invalid"}}
	req := injectSession(httptest.NewRequest("POST", "/dashboard/buckets/my-bucket/settings",
		strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = injectBucketRoute(req, "my-bucket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "flash-err")
}

// --- Integration tests (DB required) ---

func TestHandleBucketSettings_ReadsFromDB(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db)
	defer cleanupDashBucketData(t, db)

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, slug)
		VALUES ('test-dash-s1', 'Settings Co', 'settings@test.com', 'VK-s1', 'SK-s1', 'settings-co')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, cors_origins, cache_max_age_secs)
		VALUES ('test-dash-s1', 'public-bucket', 'public-read', 'https://example.com', 7200)
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	tmpl := testBucketSettingsTemplate(t)
	handler := HandleBucketSettings(tmpl, db, zap.NewNop())

	req := injectSessionWithTenant(
		httptest.NewRequest("GET", "/dashboard/buckets/public-bucket/settings", nil),
		"test-dash-s1")
	req = injectBucketRoute(req, "public-bucket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "public-bucket Settings")
	assert.Contains(t, body, `<span class="vis">public-read</span>`)
	assert.Contains(t, body, "cdn.stored.ge/settings-co/public-bucket")
	assert.Contains(t, body, "https://example.com")
	assert.Contains(t, body, "7200")
}

func TestHandleBucketSettings_CDNHiddenForPrivate(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db)
	defer cleanupDashBucketData(t, db)

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, slug)
		VALUES ('test-dash-s2', 'Private Co', 'private@test.com', 'VK-s2', 'SK-s2', 'private-co')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, visibility)
		VALUES ('test-dash-s2', 'secret-bucket', 'private')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	tmpl := testBucketSettingsTemplate(t)
	handler := HandleBucketSettings(tmpl, db, zap.NewNop())

	req := injectSessionWithTenant(
		httptest.NewRequest("GET", "/dashboard/buckets/secret-bucket/settings", nil),
		"test-dash-s2")
	req = injectBucketRoute(req, "secret-bucket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `<span class="vis">private</span>`)
	assert.NotContains(t, body, "cdn.stored.ge")
}

func TestHandleUpdateBucketSettings_ToggleToPublic(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db)
	defer cleanupDashBucketData(t, db)

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, slug)
		VALUES ('test-dash-s3', 'Toggle Co', 'toggle@test.com', 'VK-s3', 'SK-s3', 'toggle-co')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, storage_used_bytes, tier)
		VALUES ('test-dash-s3', 1099511627776, 0, 'starter')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, visibility)
		VALUES ('test-dash-s3', 'toggle-bucket', 'private')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	tmpl := testBucketSettingsTemplate(t)
	handler := HandleUpdateBucketSettings(tmpl, db, zap.NewNop())

	form := url.Values{
		"visibility":    {"public-read"},
		"cors_origins":  {"https://mysite.com"},
		"cache_max_age": {"1800"},
	}
	req := injectSessionWithTenant(
		httptest.NewRequest("POST", "/dashboard/buckets/toggle-bucket/settings",
			strings.NewReader(form.Encode())),
		"test-dash-s3")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = injectBucketRoute(req, "toggle-bucket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)

	var vis, cors string
	var cache int
	err = db.QueryRow(`SELECT visibility, cors_origins, cache_max_age_secs
		FROM buckets WHERE tenant_id = 'test-dash-s3' AND name = 'toggle-bucket'`).
		Scan(&vis, &cors, &cache)
	require.NoError(t, err)
	assert.Equal(t, "public-read", vis)
	assert.Equal(t, "https://mysite.com", cors)
	assert.Equal(t, 1800, cache)
}

func TestHandleUpdateBucketSettings_ArchiveTierBlocked(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db)
	defer cleanupDashBucketData(t, db)

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, slug)
		VALUES ('test-dash-s4', 'Archive Co', 'archive@test.com', 'VK-s4', 'SK-s4', 'archive-co')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, storage_used_bytes, tier)
		VALUES ('test-dash-s4', 1099511627776, 0, 'vault18')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, visibility)
		VALUES ('test-dash-s4', 'cold-bucket', 'private')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	tmpl := testBucketSettingsTemplate(t)
	handler := HandleUpdateBucketSettings(tmpl, db, zap.NewNop())

	form := url.Values{"visibility": {"public-read"}}
	req := injectSessionWithTenant(
		httptest.NewRequest("POST", "/dashboard/buckets/cold-bucket/settings",
			strings.NewReader(form.Encode())),
		"test-dash-s4")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = injectBucketRoute(req, "cold-bucket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)

	// Visibility should NOT have changed.
	var vis string
	err = db.QueryRow(`SELECT visibility FROM buckets WHERE tenant_id = 'test-dash-s4' AND name = 'cold-bucket'`).Scan(&vis)
	require.NoError(t, err)
	assert.Equal(t, "private", vis)
}

func TestHandleUpdateBucketSettings_ToggleToPrivate(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db)
	defer cleanupDashBucketData(t, db)

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, slug)
		VALUES ('test-dash-s5', 'Revoke Co', 'revoke@test.com', 'VK-s5', 'SK-s5', 'revoke-co')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, storage_used_bytes, tier)
		VALUES ('test-dash-s5', 1099511627776, 0, 'starter')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, visibility)
		VALUES ('test-dash-s5', 'was-public', 'public-read')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	tmpl := testBucketSettingsTemplate(t)
	handler := HandleUpdateBucketSettings(tmpl, db, zap.NewNop())

	form := url.Values{"visibility": {"private"}}
	req := injectSessionWithTenant(
		httptest.NewRequest("POST", "/dashboard/buckets/was-public/settings",
			strings.NewReader(form.Encode())),
		"test-dash-s5")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = injectBucketRoute(req, "was-public")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)

	var vis string
	err = db.QueryRow(`SELECT visibility FROM buckets WHERE tenant_id = 'test-dash-s5' AND name = 'was-public'`).Scan(&vis)
	require.NoError(t, err)
	assert.Equal(t, "private", vis)
}

func TestHandleBucketSettings_ArchiveTierShowsRestriction(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db)
	defer cleanupDashBucketData(t, db)

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, slug)
		VALUES ('test-dash-s6', 'Geyser Co', 'geyser@test.com', 'VK-s6', 'SK-s6', 'geyser-co')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, storage_used_bytes, tier)
		VALUES ('test-dash-s6', 1099511627776, 0, 'geyser')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, visibility)
		VALUES ('test-dash-s6', 'tape-bucket', 'private')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	tmpl := testBucketSettingsTemplate(t)
	handler := HandleBucketSettings(tmpl, db, zap.NewNop())

	req := injectSessionWithTenant(
		httptest.NewRequest("GET", "/dashboard/buckets/tape-bucket/settings", nil),
		"test-dash-s6")
	req = injectBucketRoute(req, "tape-bucket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "restricted")
	assert.Contains(t, body, "archive-tier")
}

func TestHandleUpdateBucketSettings_CacheMaxAgeClamped(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db)
	defer cleanupDashBucketData(t, db)

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, slug)
		VALUES ('test-dash-s7', 'Clamp Co', 'clamp@test.com', 'VK-s7', 'SK-s7', 'clamp-co')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, storage_used_bytes, tier)
		VALUES ('test-dash-s7', 1099511627776, 0, 'starter')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, visibility)
		VALUES ('test-dash-s7', 'clamp-bucket', 'private')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	tmpl := testBucketSettingsTemplate(t)
	handler := HandleUpdateBucketSettings(tmpl, db, zap.NewNop())

	form := url.Values{
		"visibility":    {"private"},
		"cache_max_age": {"999999"},
	}
	req := injectSessionWithTenant(
		httptest.NewRequest("POST", "/dashboard/buckets/clamp-bucket/settings",
			strings.NewReader(form.Encode())),
		"test-dash-s7")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = injectBucketRoute(req, "clamp-bucket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)

	var cache int
	err = db.QueryRow(`SELECT cache_max_age_secs FROM buckets WHERE tenant_id = 'test-dash-s7' AND name = 'clamp-bucket'`).Scan(&cache)
	require.NoError(t, err)
	assert.Equal(t, 86400, cache)
}

func TestHandleUpdateBucketSettings_BucketNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db)
	defer cleanupDashBucketData(t, db)

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, slug)
		VALUES ('test-dash-s8', 'Ghost Co', 'ghost@test.com', 'VK-s8', 'SK-s8', 'ghost-co')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	tmpl := testBucketSettingsTemplate(t)
	handler := HandleUpdateBucketSettings(tmpl, db, zap.NewNop())

	form := url.Values{"visibility": {"public-read"}}
	req := injectSessionWithTenant(
		httptest.NewRequest("POST", "/dashboard/buckets/nonexistent/settings",
			strings.NewReader(form.Encode())),
		"test-dash-s8")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = injectBucketRoute(req, "nonexistent")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
}
