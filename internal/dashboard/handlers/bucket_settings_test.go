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

	"github.com/FairForge/vaultaire/internal/hosts"
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
			`<span class="tier">{{.TierPreference}}</span>` +
			`{{if .DataResidency}}<span class="residency">{{.DataResidency}}</span>{{end}}` +
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
	useProdHosts(t)
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db, "test-dash-s1")
	defer cleanupDashBucketData(t, db, "test-dash-s1")

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
	assert.Contains(t, body, "https://cdn.stored.cloud/settings-co/public-bucket")
	assert.Contains(t, body, "https://example.com")
	assert.Contains(t, body, "7200")
}

func TestHandleBucketSettings_CDNHiddenForPrivate(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db, "test-dash-s2")
	defer cleanupDashBucketData(t, db, "test-dash-s2")

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
	assert.NotContains(t, body, hosts.Get().CDNBase())
}

func TestHandleUpdateBucketSettings_ToggleToPublic(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db, "test-dash-s3")
	defer cleanupDashBucketData(t, db, "test-dash-s3")

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
	cleanupDashBucketData(t, db, "test-dash-s4")
	defer cleanupDashBucketData(t, db, "test-dash-s4")

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
	cleanupDashBucketData(t, db, "test-dash-s5")
	defer cleanupDashBucketData(t, db, "test-dash-s5")

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
	cleanupDashBucketData(t, db, "test-dash-s6")
	defer cleanupDashBucketData(t, db, "test-dash-s6")

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
	cleanupDashBucketData(t, db, "test-dash-s7")
	defer cleanupDashBucketData(t, db, "test-dash-s7")

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

func TestHandleBucketSettings_TierPreference(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db, "test-dash-tp1")
	defer cleanupDashBucketData(t, db, "test-dash-tp1")

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, slug)
		VALUES ('test-dash-tp1', 'Tier Co', 'tier@test.com', 'VK-tp1', 'SK-tp1', 'tier-co')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, tier_preference)
		VALUES ('test-dash-tp1', 'archive-bucket', 'private', 'archive')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	tmpl := testBucketSettingsTemplate(t)
	handler := HandleBucketSettings(tmpl, db, zap.NewNop())

	req := injectSessionWithTenant(
		httptest.NewRequest("GET", "/dashboard/buckets/archive-bucket/settings", nil),
		"test-dash-tp1")
	req = injectBucketRoute(req, "archive-bucket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `<span class="tier">archive</span>`)
}

func TestHandleUpdateBucketSettings_TierPreference(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db, "test-dash-tp2")
	defer cleanupDashBucketData(t, db, "test-dash-tp2")

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, slug)
		VALUES ('test-dash-tp2', 'Update Tier Co', 'utier@test.com', 'VK-tp2', 'SK-tp2', 'utier-co')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, storage_used_bytes, tier)
		VALUES ('test-dash-tp2', 1099511627776, 0, 'starter')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, tier_preference)
		VALUES ('test-dash-tp2', 'tier-bucket', 'private', 'auto')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	tmpl := testBucketSettingsTemplate(t)
	handler := HandleUpdateBucketSettings(tmpl, db, zap.NewNop())

	form := url.Values{
		"visibility":      {"private"},
		"tier_preference": {"performance"},
	}
	req := injectSessionWithTenant(
		httptest.NewRequest("POST", "/dashboard/buckets/tier-bucket/settings",
			strings.NewReader(form.Encode())),
		"test-dash-tp2")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = injectBucketRoute(req, "tier-bucket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)

	var pref string
	err = db.QueryRow(`SELECT tier_preference FROM buckets WHERE tenant_id = 'test-dash-tp2' AND name = 'tier-bucket'`).Scan(&pref)
	require.NoError(t, err)
	assert.Equal(t, "performance", pref)
}

func TestHandleUpdateBucketSettings_InvalidTier(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db, "test-dash-tp3")
	defer cleanupDashBucketData(t, db, "test-dash-tp3")

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, slug)
		VALUES ('test-dash-tp3', 'Bad Tier Co', 'btier@test.com', 'VK-tp3', 'SK-tp3', 'btier-co')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, storage_used_bytes, tier)
		VALUES ('test-dash-tp3', 1099511627776, 0, 'starter')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, tier_preference)
		VALUES ('test-dash-tp3', 'bad-tier-bucket', 'private', 'archive')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	tmpl := testBucketSettingsTemplate(t)
	handler := HandleUpdateBucketSettings(tmpl, db, zap.NewNop())

	form := url.Values{
		"visibility":      {"private"},
		"tier_preference": {"bogus"},
	}
	req := injectSessionWithTenant(
		httptest.NewRequest("POST", "/dashboard/buckets/bad-tier-bucket/settings",
			strings.NewReader(form.Encode())),
		"test-dash-tp3")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = injectBucketRoute(req, "bad-tier-bucket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)

	var pref string
	err = db.QueryRow(`SELECT tier_preference FROM buckets WHERE tenant_id = 'test-dash-tp3' AND name = 'bad-tier-bucket'`).Scan(&pref)
	require.NoError(t, err)
	assert.Equal(t, "auto", pref, "invalid tier should fall back to auto")
}

func TestHandleUpdateBucketSettings_BucketNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db, "test-dash-s8")
	defer cleanupDashBucketData(t, db, "test-dash-s8")

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

func TestHandleBucketSettings_DataResidency(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db, "test-dash-dr1")
	defer cleanupDashBucketData(t, db, "test-dash-dr1")

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, slug)
		VALUES ('test-dash-dr1', 'EU Co', 'eu@test.com', 'VK-dr1', 'SK-dr1', 'eu-co')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, region, data_residency)
		VALUES ('test-dash-dr1', 'eu-bucket', 'private', 'eu-west-1', 'eu')
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	tmpl := testBucketSettingsTemplate(t)
	handler := HandleBucketSettings(tmpl, db, zap.NewNop())

	req := injectSessionWithTenant(
		httptest.NewRequest("GET", "/dashboard/buckets/eu-bucket/settings", nil),
		"test-dash-dr1")
	req = injectBucketRoute(req, "eu-bucket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `<span class="residency">eu</span>`)
}

// Review R12 (mass assignment): the form offers auto/performance/standard/
// archive; "resilient" (the Lyve tier) reached the row from a crafted POST on
// a free tenant. It now falls back to auto like any other unknown value.
func TestHandleUpdateBucketSettings_ResilientNotOfferedByTheForm(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db, "test-dash-tp4")
	defer cleanupDashBucketData(t, db, "test-dash-tp4")

	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key, slug)
		VALUES ('test-dash-tp4', 'Res Co', 'res@test.com', 'VK-tp4', 'SK-tp4', 'res-co') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, storage_used_bytes, tier)
		VALUES ('test-dash-tp4', 5368709120, 0, 'free') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, tier_preference)
		VALUES ('test-dash-tp4', 'res-bucket', 'private', 'standard') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)

	handler := HandleUpdateBucketSettings(testBucketSettingsTemplate(t), db, zap.NewNop())
	form := url.Values{"visibility": {"private"}, "tier_preference": {"resilient"}}
	req := injectSessionWithTenant(httptest.NewRequest("POST", "/dashboard/buckets/res-bucket/settings",
		strings.NewReader(form.Encode())), "test-dash-tp4")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = injectBucketRoute(req, "res-bucket")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusSeeOther, w.Code)

	var pref string
	require.NoError(t, db.QueryRow(`SELECT tier_preference FROM buckets WHERE tenant_id = 'test-dash-tp4' AND name = 'res-bucket'`).Scan(&pref))
	assert.Equal(t, "auto", pref, "a value the form does not offer is not stored")
}

// useProdHosts installs the WP-DOMAIN-1 prod host set for one test (the CDN
// URLs come from internal/hosts) and restores the previous one.
func useProdHosts(t *testing.T) {
	t.Helper()
	prev := hosts.Get()
	t.Cleanup(func() { hosts.Set(prev) })
	hosts.Set(hosts.Hosts{Canonical: "https://stored.cloud", S3: "https://s3.stored.cloud", CDN: "https://cdn.stored.cloud", BaseURL: "https://stored.cloud"})
}

// WP-SSEC-1 B.5: the "require a customer key" toggle. On: unkeyed writes
// into the bucket are refused (the S3 API's 400); a public bucket cannot
// require a key, so the two together are refused with a flash and the row is
// left as it was.
func TestHandleUpdateBucketSettings_RequireCustomerKey(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	cleanupDashBucketData(t, db, "test-dash-s3")
	defer cleanupDashBucketData(t, db, "test-dash-s3")

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
	post := func(form url.Values) *httptest.ResponseRecorder {
		req := injectSessionWithTenant(
			httptest.NewRequest("POST", "/dashboard/buckets/toggle-bucket/settings", strings.NewReader(form.Encode())),
			"test-dash-s3")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = injectBucketRoute(req, "toggle-bucket")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	flag := func() (vis string, required bool) {
		require.NoError(t, db.QueryRow(`SELECT visibility, ssec_required FROM buckets WHERE tenant_id = 'test-dash-s3' AND name = 'toggle-bucket'`).Scan(&vis, &required))
		return vis, required
	}

	// On.
	w := post(url.Values{"visibility": {"private"}, "ssec_required": {"on"}})
	assert.Equal(t, http.StatusSeeOther, w.Code)
	vis, required := flag()
	assert.Equal(t, "private", vis)
	assert.True(t, required)

	// Public + required: refused, nothing changed.
	w = post(url.Values{"visibility": {"public-read"}, "ssec_required": {"on"}})
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Contains(t, w.Header().Get("Set-Cookie"), "flash")
	vis, required = flag()
	assert.Equal(t, "private", vis, "the visibility did not flip")
	assert.True(t, required)

	// Off (an unchecked checkbox sends nothing).
	w = post(url.Values{"visibility": {"private"}})
	assert.Equal(t, http.StatusSeeOther, w.Code)
	_, required = flag()
	assert.False(t, required)
}

// RP33-1: the settings form cannot make a bucket that holds readable files
// Private — the door refuses, the flash says to copy into a new bucket.
func TestHandleUpdateBucketSettings_PrivateOverReadable_FlashesCopyAdvice(t *testing.T) {
	if testing.Short() {
		t.Skip("requires database")
	}
	db := testDashDB(t)
	defer func() { _ = db.Close() }()
	const tenant = "test-dash-p27"
	clean := func() {
		_, _ = db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, tenant)
		_, _ = db.Exec(`DELETE FROM buckets WHERE tenant_id = $1`, tenant)
		_, _ = db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, tenant)
		_, _ = db.Exec(`DELETE FROM tenants WHERE id = $1`, tenant)
	}
	clean()
	defer clean() // before the deferred db.Close: a t.Cleanup ran on a closed handle and left the head row (RP20-A6)
	_, err := db.Exec(`INSERT INTO tenants (id, name, email, access_key, secret_key) VALUES ($1, 'P27', 'p27@test.com', 'VK-p27', 'SK-p27')`, tenant)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO buckets (tenant_id, name, visibility) VALUES ($1, 'has-plain', 'private')`, tenant)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, content_type)
		VALUES ($1, 'has-plain', 'plain.txt', 5, 'e', 'text/plain')`, tenant)
	require.NoError(t, err)

	form := url.Values{"visibility": {"private"}, "ssec_required": {"on"}}
	req := injectSessionWithTenant(httptest.NewRequest("POST", "/dashboard/buckets/has-plain/settings", strings.NewReader(form.Encode())), tenant)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	HandleUpdateBucketSettings(testBucketSettingsTemplate(t), db, zap.NewNop()).ServeHTTP(w, injectBucketRoute(req, "has-plain"))

	assert.Equal(t, http.StatusSeeOther, w.Code)
	var flash string
	for _, c := range w.Result().Cookies() {
		if c.Name == "flash" {
			v, _ := url.ParseQuery(c.Value)
			flash = v.Get("error")
		}
	}
	assert.Contains(t, flash, "1 file(s) that are not encrypted with your own key")
	assert.Contains(t, flash, "Create a new bucket")
	var mode string
	require.NoError(t, db.QueryRow(`SELECT mode FROM buckets WHERE tenant_id = $1 AND name = 'has-plain'`, tenant).Scan(&mode))
	assert.Equal(t, "standard", mode, "nothing written")
}
