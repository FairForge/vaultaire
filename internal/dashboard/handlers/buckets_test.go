package handlers

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testBucketsTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("base").Parse(
		`{{define "base"}}` +
			`{{block "nav" .}}{{end}}` +
			`{{block "content" .}}{{end}}` +
			`{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Buckets{{end}}` +
			`{{define "content"}}` +
			`<h1>Buckets</h1>` +
			`<span class="count">{{.BucketCount}}</span>` +
			`<span class="email">{{.Email}}</span>` +
			`{{if .CreateError}}<span class="error">{{.CreateError}}</span>{{end}}` +
			`{{if .CreateSuccess}}<span class="success">{{.CreateSuccess}}</span>{{end}}` +
			`{{range .Buckets}}<span class="bucket">{{.Name}}</span>{{end}}` +
			`{{end}}`))
	return tmpl
}

func testBucketObjectsTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("base").Parse(
		`{{define "base"}}` +
			`{{block "nav" .}}{{end}}` +
			`{{block "content" .}}{{end}}` +
			`{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Objects{{end}}` +
			`{{define "content"}}` +
			`<h1>{{.BucketName}}</h1>` +
			`<span class="count">{{.ObjectCount}}</span>` +
			`<span class="size">{{.TotalSizeFmt}}</span>` +
			`{{range .Objects}}<span class="object">{{.Display}}</span>{{end}}` +
			`{{range .Prefixes}}<span class="prefix">{{.Display}}</span>{{end}}` +
			`{{end}}`))
	return tmpl
}

func injectSession(req *http.Request) *http.Request {
	sd := &dashauth.SessionData{
		UserID:   "user-123",
		TenantID: "tenant-456",
		Email:    "test@stored.ge",
		Role:     "user",
	}
	ctx := context.WithValue(req.Context(), dashauth.SessionKey, sd)
	return req.WithContext(ctx)
}

func TestHandleBuckets_NoDB(t *testing.T) {
	tmpl := testBucketsTemplate(t)
	handler := HandleBuckets(tmpl, nil, "", zap.NewNop())

	req := injectSession(httptest.NewRequest("GET", "/dashboard/buckets", nil))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "Buckets")
	assert.Contains(t, body, "test@stored.ge")
	assert.Contains(t, body, `<span class="count">0</span>`)
}

func TestHandleBuckets_NoSession(t *testing.T) {
	tmpl := testBucketsTemplate(t)
	handler := HandleBuckets(tmpl, nil, "", zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/buckets", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleCreateBucket_NoDB(t *testing.T) {
	tmpDir := t.TempDir()
	tmpl := testBucketsTemplate(t)
	handler := HandleCreateBucket(tmpl, nil, tmpDir, zap.NewNop())

	form := url.Values{"name": {"my-test-bucket"}}
	req := injectSession(httptest.NewRequest("POST", "/dashboard/buckets",
		strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "my-test-bucket")

	// Directory should have been created.
	_, err := os.Stat(filepath.Join(tmpDir, "my-test-bucket"))
	require.NoError(t, err)
}

func TestHandleCreateBucket_InvalidName(t *testing.T) {
	tmpl := testBucketsTemplate(t)
	handler := HandleCreateBucket(tmpl, nil, t.TempDir(), zap.NewNop())

	tests := []struct {
		name string
		want string
	}{
		{"AB", "Invalid bucket name"},        // too short + uppercase
		{"a", "Invalid bucket name"},         // too short
		{"../etc", "Invalid bucket name"},    // path traversal
		{"My Bucket", "Invalid bucket name"}, // spaces + uppercase
	}

	for _, tt := range tests {
		form := url.Values{"name": {tt.name}}
		req := injectSession(httptest.NewRequest("POST", "/dashboard/buckets",
			strings.NewReader(form.Encode())))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "Invalid bucket name", "name=%q", tt.name)
	}
}

func TestHandleBucketObjects_NoDB(t *testing.T) {
	tmpl := testBucketObjectsTemplate(t)
	handler := HandleBucketObjects(tmpl, nil, zap.NewNop())

	// chi requires a route context to extract URL params.
	req := injectSession(httptest.NewRequest("GET", "/dashboard/buckets/my-bucket", nil))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "my-bucket")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "my-bucket")
	assert.Contains(t, body, `<span class="count">0</span>`)
	assert.Contains(t, body, `<span class="size">0 B</span>`)
}

func TestHandleBucketObjects_NoSession(t *testing.T) {
	tmpl := testBucketObjectsTemplate(t)
	handler := HandleBucketObjects(tmpl, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/buckets/my-bucket", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
}

func TestSessionData(t *testing.T) {
	sd := &dashauth.SessionData{
		UserID:   "u1",
		TenantID: "t1",
		Email:    "a@b.com",
		Role:     "admin",
	}
	data := sessionData(sd, "test")
	assert.Equal(t, "a@b.com", data["Email"])
	assert.Equal(t, "admin", data["Role"])
	assert.Equal(t, "test", data["Page"])
}

func TestBucketNameRegex(t *testing.T) {
	valid := []string{"my-bucket", "bucket123", "a.b.c", "abc"}
	invalid := []string{"AB", "a", "my bucket", "-bucket", "bucket-", "..bad"}

	for _, name := range valid {
		assert.True(t, bucketNameRe.MatchString(name), "expected valid: %s", name)
	}
	for _, name := range invalid {
		assert.False(t, bucketNameRe.MatchString(name), "expected invalid: %s", name)
	}
}

// Ensure relativeTime and formatBytes still work (shared with overview).
func TestBuckets_SharedHelpers(t *testing.T) {
	assert.Equal(t, "1 KB", formatBytes(1024))
	assert.Equal(t, "just now", relativeTime(time.Now()))
}
