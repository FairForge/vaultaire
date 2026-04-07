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

func testOverviewTemplate(t *testing.T) *template.Template {
	t.Helper()
	// Minimal base + content template for testing.
	tmpl := template.Must(template.New("base").Parse(
		`{{define "base"}}` +
			`{{block "nav" .}}{{end}}` +
			`{{block "content" .}}{{end}}` +
			`{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Dashboard{{end}}` +
			`{{define "content"}}` +
			`<h1>Dashboard</h1>` +
			`<span class="storage">{{.StorageUsedFmt}} of {{.StorageLimitFmt}}</span>` +
			`<span class="bandwidth">{{.BandwidthTotalFmt}}</span>` +
			`<span class="buckets">{{.BucketCount}}</span>` +
			`<span class="objects">{{.ObjectCount}}</span>` +
			`<span class="apikeys">{{.APIKeyCount}}</span>` +
			`<span class="tier">{{.Tier}}</span>` +
			`<span class="email">{{.Email}}</span>` +
			`{{end}}`))
	return tmpl
}

func TestHandleOverview_NoDB(t *testing.T) {
	tmpl := testOverviewTemplate(t)
	handler := HandleOverview(tmpl, nil, zap.NewNop())

	// Create a session and inject it into the request context.
	store := dashauth.NewMemoryStore()
	token, err := store.Create(context.Background(), dashauth.SessionData{
		UserID:   "user-123",
		TenantID: "tenant-456",
		Email:    "test@stored.ge",
		Role:     "user",
	}, time.Hour)
	require.NoError(t, err)

	sd, err := store.Get(context.Background(), token)
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "/dashboard/", nil)
	ctx := context.WithValue(req.Context(), dashauth.SessionKey, sd)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "Dashboard")
	assert.Contains(t, body, "test@stored.ge")
	// Default values when no DB.
	assert.Contains(t, body, "0 B of 1 TB")
	assert.Contains(t, body, "starter")
}

func TestHandleOverview_NoSession(t *testing.T) {
	tmpl := testOverviewTemplate(t)
	handler := HandleOverview(tmpl, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should redirect to login.
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1 KB"},
		{1536, "1.5 KB"},
		{1048576, "1 MB"},
		{1073741824, "1 GB"},
		{1099511627776, "1 TB"},
		{5368709120, "5 GB"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatBytes(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRelativeTime(t *testing.T) {
	assert.Equal(t, "just now", relativeTime(time.Now()))
	assert.Equal(t, "5 minutes ago", relativeTime(time.Now().Add(-5*time.Minute)))
	assert.Equal(t, "1 hour ago", relativeTime(time.Now().Add(-1*time.Hour)))
	assert.Equal(t, "3 hours ago", relativeTime(time.Now().Add(-3*time.Hour)))
	assert.Equal(t, "2 days ago", relativeTime(time.Now().Add(-48*time.Hour)))
}

func TestAbsInt64(t *testing.T) {
	assert.Equal(t, int64(5), absInt64(5))
	assert.Equal(t, int64(5), absInt64(-5))
	assert.Equal(t, int64(0), absInt64(0))
}
