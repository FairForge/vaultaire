package handlers

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func testAnalyticsTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("base").Parse(
		`{{define "base"}}` +
			`{{block "nav" .}}{{end}}` +
			`{{block "content" .}}{{end}}` +
			`{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Analytics{{end}}` +
			`{{define "content"}}` +
			`<h1>{{.BucketName}}</h1>` +
			`<span class="dl24">{{.Downloads24h}}</span>` +
			`<span class="bw24">{{.Bandwidth24h}}</span>` +
			`<span class="dl7">{{.Downloads7d}}</span>` +
			`<span class="dl30">{{.Downloads30d}}</span>` +
			`<span class="hasdata">{{.HasData}}</span>` +
			`{{if .HasBudget}}<span class="budget">{{.BudgetPct}}%</span>{{end}}` +
			`{{range .TopObjects}}<span class="top">{{.Key}}</span>{{end}}` +
			`{{range .Countries}}<span class="geo">{{.Code}}</span>{{end}}` +
			`{{end}}`))
	return tmpl
}

func TestHandleBucketAnalytics_NilDB(t *testing.T) {
	// Arrange
	tmpl := testAnalyticsTemplate(t)
	handler := HandleBucketAnalytics(tmpl, nil, zap.NewNop())

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "test-bucket")

	sd := &dashauth.SessionData{
		UserID: "user-1", TenantID: "tenant-1",
		Email: "test@stored.ge", Role: "user",
	}

	req := httptest.NewRequest("GET", "/dashboard/buckets/test-bucket/analytics", nil)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = context.WithValue(ctx, dashauth.SessionKey, sd)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(w, req)

	// Assert — renders with defaults, no crash
	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "test-bucket")
	assert.Contains(t, body, `<span class="dl24">0</span>`)
	assert.Contains(t, body, `<span class="bw24">0 B</span>`)
	assert.Contains(t, body, `<span class="hasdata">false</span>`)
}

func TestHandleBucketAnalytics_NoSession(t *testing.T) {
	tmpl := testAnalyticsTemplate(t)
	handler := HandleBucketAnalytics(tmpl, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/buckets/test-bucket/analytics", nil)
	w := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(w, req)

	// Assert — redirects to login
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleBucketAnalytics_EmptyBucketName(t *testing.T) {
	tmpl := testAnalyticsTemplate(t)
	handler := HandleBucketAnalytics(tmpl, nil, zap.NewNop())

	rctx := chi.NewRouteContext()
	// No "name" param set

	sd := &dashauth.SessionData{
		UserID: "user-1", TenantID: "tenant-1",
		Email: "test@stored.ge", Role: "user",
	}

	req := httptest.NewRequest("GET", "/dashboard/buckets//analytics", nil)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = context.WithValue(ctx, dashauth.SessionKey, sd)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(w, req)

	// Assert — redirects to bucket list
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/buckets", w.Header().Get("Location"))
}

func TestSetAnalyticsDefaults(t *testing.T) {
	data := map[string]any{}

	setAnalyticsDefaults(data)

	assert.Equal(t, int64(0), data["Downloads24h"])
	assert.Equal(t, int64(0), data["Downloads7d"])
	assert.Equal(t, int64(0), data["Downloads30d"])
	assert.Equal(t, "0 B", data["Bandwidth24h"])
	assert.Equal(t, "0 B", data["Bandwidth7d"])
	assert.Equal(t, "0 B", data["Bandwidth30d"])
	assert.Equal(t, false, data["HasData"])
	assert.Equal(t, false, data["HasBudget"])
	assert.Nil(t, data["TopObjects"])
	assert.Nil(t, data["Countries"])
}
