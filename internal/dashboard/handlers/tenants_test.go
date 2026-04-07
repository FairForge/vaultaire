package handlers

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// --- Test helpers ---

func testAdminTenantsTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("admin").Parse(
		`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Tenants{{end}}` +
			`{{define "content"}}` +
			`<h1>Tenants</h1>` +
			`<span class="email">{{.Email}}</span>` +
			`<span class="search">{{.SearchQuery}}</span>` +
			`{{if .Tenants}}{{range .Tenants}}` +
			`<span class="tenant">{{.Name}} {{.Email}} {{.Plan}} {{.Status}}</span>` +
			`{{end}}{{else}}` +
			`<p class="empty">No tenants found.</p>` +
			`{{end}}` +
			`{{end}}`))
	return tmpl
}

func testAdminTenantDetailTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("admin").Parse(
		`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Tenant Detail{{end}}` +
			`{{define "content"}}` +
			`<h1>{{.TenantName}}</h1>` +
			`<span class="email">{{.TenantEmail}}</span>` +
			`<span class="plan">{{.Plan}}</span>` +
			`<span class="storage">{{.StorageUsedFmt}} / {{.StorageLimitFmt}}</span>` +
			`<span class="suspended">{{.IsSuspended}}</span>` +
			`{{end}}`))
	return tmpl
}

func adminCtx(t *testing.T) context.Context {
	t.Helper()
	store := dashauth.NewMemoryStore()
	token, err := store.Create(context.Background(), dashauth.SessionData{
		UserID: "admin-1", TenantID: "t-admin", Email: "admin@stored.ge", Role: "admin",
	}, time.Hour)
	require.NoError(t, err)
	sd, err := store.Get(context.Background(), token)
	require.NoError(t, err)
	return context.WithValue(context.Background(), dashauth.SessionKey, sd)
}

func withChiParam(ctx context.Context, key, val string) context.Context {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, val)
	return context.WithValue(ctx, chi.RouteCtxKey, rctx)
}

// --- Tenant list tests ---

func TestHandleTenantList_NoSession(t *testing.T) {
	handler := HandleTenantList(testAdminTenantsTemplate(t), nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/admin/tenants", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleTenantList_NoDB(t *testing.T) {
	handler := HandleTenantList(testAdminTenantsTemplate(t), nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/admin/tenants", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "Tenants")
	assert.Contains(t, body, "No tenants found.")
}

func TestHandleTenantList_WithSearch(t *testing.T) {
	handler := HandleTenantList(testAdminTenantsTemplate(t), nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/admin/tenants?q=test", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "test")
}

// --- Tenant detail tests ---

func TestHandleTenantDetail_NoSession(t *testing.T) {
	handler := HandleTenantDetail(testAdminTenantDetailTemplate(t), nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/admin/tenants/t-1", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleTenantDetail_EmptyID(t *testing.T) {
	handler := HandleTenantDetail(testAdminTenantDetailTemplate(t), nil, zap.NewNop())

	ctx := withChiParam(adminCtx(t), "id", "")
	req := httptest.NewRequest("GET", "/admin/tenants/", nil)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestHandleTenantDetail_NoDB(t *testing.T) {
	handler := HandleTenantDetail(testAdminTenantDetailTemplate(t), nil, zap.NewNop())

	ctx := withChiParam(adminCtx(t), "id", "nonexistent")
	req := httptest.NewRequest("GET", "/admin/tenants/nonexistent", nil)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// --- Suspend/Enable tests ---

func TestHandleSuspendTenant_NoSession(t *testing.T) {
	handler := HandleSuspendTenant(nil, zap.NewNop())

	req := httptest.NewRequest("POST", "/admin/tenants/t-1/suspend", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
}

func TestHandleSuspendTenant_NoDB(t *testing.T) {
	handler := HandleSuspendTenant(nil, zap.NewNop())

	ctx := withChiParam(adminCtx(t), "id", "t-1")
	req := httptest.NewRequest("POST", "/admin/tenants/t-1/suspend", nil)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestHandleEnableTenant_NoSession(t *testing.T) {
	handler := HandleEnableTenant(nil, zap.NewNop())

	req := httptest.NewRequest("POST", "/admin/tenants/t-1/enable", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
}

func TestHandleEnableTenant_NoDB(t *testing.T) {
	handler := HandleEnableTenant(nil, zap.NewNop())

	ctx := withChiParam(adminCtx(t), "id", "t-1")
	req := httptest.NewRequest("POST", "/admin/tenants/t-1/enable", nil)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// --- Quota update tests ---

func TestHandleUpdateQuota_NoSession(t *testing.T) {
	handler := HandleUpdateQuota(nil, zap.NewNop())

	req := httptest.NewRequest("POST", "/admin/tenants/t-1/quota", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
}

func TestHandleUpdateQuota_InvalidInput(t *testing.T) {
	handler := HandleUpdateQuota(nil, zap.NewNop())

	ctx := withChiParam(adminCtx(t), "id", "t-1")
	body := strings.NewReader("storage_limit=notanumber")
	req := httptest.NewRequest("POST", "/admin/tenants/t-1/quota", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid storage limit")
}

func TestHandleUpdateQuota_NoDB(t *testing.T) {
	handler := HandleUpdateQuota(nil, zap.NewNop())

	ctx := withChiParam(adminCtx(t), "id", "t-1")
	body := strings.NewReader("storage_limit=100")
	req := httptest.NewRequest("POST", "/admin/tenants/t-1/quota", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// --- Tier change tests ---

func TestHandleChangeTier_NoSession(t *testing.T) {
	handler := HandleChangeTier(nil, zap.NewNop())

	req := httptest.NewRequest("POST", "/admin/tenants/t-1/tier", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
}

func TestHandleChangeTier_InvalidTier(t *testing.T) {
	handler := HandleChangeTier(nil, zap.NewNop())

	ctx := withChiParam(adminCtx(t), "id", "t-1")
	body := strings.NewReader("tier=bogus")
	req := httptest.NewRequest("POST", "/admin/tenants/t-1/tier", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid tier")
}

func TestHandleChangeTier_NoDB(t *testing.T) {
	handler := HandleChangeTier(nil, zap.NewNop())

	ctx := withChiParam(adminCtx(t), "id", "t-1")
	body := strings.NewReader("tier=vault3")
	req := httptest.NewRequest("POST", "/admin/tenants/t-1/tier", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
