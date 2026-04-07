package handlers

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testAPIKeysTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("base").Parse(
		`{{define "base"}}` +
			`{{block "nav" .}}{{end}}` +
			`{{block "content" .}}{{end}}` +
			`{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}API Keys{{end}}` +
			`{{define "content"}}` +
			`<h1>API Keys</h1>` +
			`<span class="email">{{.Email}}</span>` +
			`{{if .NewKey}}<span class="new-key">{{.NewKey.Key}}</span><span class="new-secret">{{.NewKey.Secret}}</span>{{end}}` +
			`{{if .GenerateError}}<span class="error">{{.GenerateError}}</span>{{end}}` +
			`{{range .Keys}}<span class="key">{{.KeyID}} {{.Status}}</span>{{end}}` +
			`{{end}}`))
	return tmpl
}

func setupAuthWithUser(t *testing.T) (*auth.AuthService, *dashauth.SessionData) {
	t.Helper()
	authSvc := auth.NewAuthService(nil, nil)
	user, _ := authSvc.CreateUser(context.Background(), "keys@stored.ge", "securepass123")
	sd := &dashauth.SessionData{
		UserID:   user.ID,
		TenantID: "tenant-test",
		Email:    user.Email,
		Role:     "user",
	}
	return authSvc, sd
}

func TestHandleAPIKeys_ListEmpty(t *testing.T) {
	tmpl := testAPIKeysTemplate(t)
	authSvc, sd := setupAuthWithUser(t)
	handler := HandleAPIKeys(tmpl, authSvc, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/apikeys", nil)
	ctx := context.WithValue(req.Context(), dashauth.SessionKey, sd)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "keys@stored.ge")
}

func TestHandleAPIKeys_ListWithKeys(t *testing.T) {
	tmpl := testAPIKeysTemplate(t)
	authSvc, sd := setupAuthWithUser(t)

	key, err := authSvc.GenerateAPIKey(context.Background(), sd.UserID, "test-key")
	require.NoError(t, err)

	handler := HandleAPIKeys(tmpl, authSvc, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/apikeys", nil)
	ctx := context.WithValue(req.Context(), dashauth.SessionKey, sd)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, key.Key)
	assert.Contains(t, body, "active")
}

func TestHandleGenerateKey(t *testing.T) {
	tmpl := testAPIKeysTemplate(t)
	authSvc, sd := setupAuthWithUser(t)
	handler := HandleGenerateKey(tmpl, authSvc, zap.NewNop())

	form := url.Values{"name": {"my-rclone-key"}}
	req := httptest.NewRequest("POST", "/dashboard/apikeys",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx := context.WithValue(req.Context(), dashauth.SessionKey, sd)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "new-key")
	assert.Contains(t, body, "new-secret")

	keys, _ := authSvc.ListAPIKeys(context.Background(), sd.UserID)
	// CreateUser already generates one key, so we should have 2 now.
	require.Len(t, keys, 2)
	// Find the one we just created.
	var found bool
	for _, k := range keys {
		if k.Name == "my-rclone-key" {
			found = true
		}
	}
	assert.True(t, found, "expected to find my-rclone-key")
}

func TestHandleGenerateKey_DefaultName(t *testing.T) {
	tmpl := testAPIKeysTemplate(t)
	authSvc, sd := setupAuthWithUser(t)
	handler := HandleGenerateKey(tmpl, authSvc, zap.NewNop())

	form := url.Values{"name": {""}}
	req := httptest.NewRequest("POST", "/dashboard/apikeys",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx := context.WithValue(req.Context(), dashauth.SessionKey, sd)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	keys, _ := authSvc.ListAPIKeys(context.Background(), sd.UserID)
	require.Len(t, keys, 2) // 1 from CreateUser + 1 from Generate
}

func TestHandleRevokeKey(t *testing.T) {
	authSvc, sd := setupAuthWithUser(t)

	key, err := authSvc.GenerateAPIKey(context.Background(), sd.UserID, "revoke-me")
	require.NoError(t, err)

	handler := HandleRevokeKey(authSvc, zap.NewNop())

	req := httptest.NewRequest("POST", "/dashboard/apikeys/"+key.ID+"/revoke", nil)
	ctx := context.WithValue(req.Context(), dashauth.SessionKey, sd)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", key.ID)
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/dashboard/apikeys", w.Header().Get("Location"))

	keys, _ := authSvc.ListAPIKeys(context.Background(), sd.UserID)
	require.Len(t, keys, 2) // 1 from CreateUser + 1 generated
	var revokedFound bool
	for _, k := range keys {
		if k.ID == key.ID && k.RevokedAt != nil {
			revokedFound = true
		}
	}
	assert.True(t, revokedFound, "expected key to be revoked")
}

func TestHandleAPIKeys_NoSession(t *testing.T) {
	tmpl := testAPIKeysTemplate(t)
	authSvc := auth.NewAuthService(nil, nil)
	handler := HandleAPIKeys(tmpl, authSvc, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/apikeys", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}
