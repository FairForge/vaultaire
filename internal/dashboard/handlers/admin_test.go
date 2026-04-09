package handlers

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testAdminTemplate(t *testing.T) *template.Template {
	t.Helper()
	// Minimal admin layout + content template for testing.
	tmpl := template.Must(template.New("admin").Parse(
		`{{define "admin"}}` +
			`{{block "content" .}}{{end}}` +
			`{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Admin{{end}}` +
			`{{define "content"}}` +
			`<h1>Admin Overview</h1>` +
			`<span class="email">{{.Email}}</span>` +
			`<span class="role">{{.Role}}</span>` +
			`{{end}}`))
	return tmpl
}

func TestHandleAdminOverview_AdminSession(t *testing.T) {
	// Arrange
	tmpl := testAdminTemplate(t)
	handler := HandleAdminOverview(tmpl, nil, zap.NewNop())

	store := dashauth.NewMemoryStore()
	token, err := store.Create(context.Background(), dashauth.SessionData{
		UserID:   "admin-1",
		TenantID: "tenant-1",
		Email:    "admin@stored.ge",
		Role:     "admin",
	}, time.Hour)
	require.NoError(t, err)

	sd, err := store.Get(context.Background(), token)
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/admin/", nil)
	ctx := context.WithValue(req.Context(), dashauth.SessionKey, sd)
	req = req.WithContext(ctx)

	// Act
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "Admin Overview")
	assert.Contains(t, body, "admin@stored.ge")
	assert.Contains(t, body, "admin")
}

func TestHandleAdminOverview_NoSession(t *testing.T) {
	// Arrange
	tmpl := testAdminTemplate(t)
	handler := HandleAdminOverview(tmpl, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/admin/", nil)

	// Act
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Assert — redirects to login when session missing
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}
