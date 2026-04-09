package handlers

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func testAdminSystemTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("admin").Parse(
		`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}System{{end}}` +
			`{{define "content"}}` +
			`<h1>System Health</h1>` +
			`<span class="goroutines">{{.Goroutines}}</span>` +
			`<span class="memory">{{.MemAllocFmt}}</span>` +
			`<span class="gc">{{.NumGC}}</span>` +
			`{{end}}`))
	return tmpl
}

func TestHandleAdminSystem_NoSession(t *testing.T) {
	handler := HandleAdminSystem(testAdminSystemTemplate(t), nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/admin/system", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleAdminSystem_AdminSession(t *testing.T) {
	handler := HandleAdminSystem(testAdminSystemTemplate(t), nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/admin/system", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "System Health")
	// Runtime stats should always have values.
	assert.NotContains(t, body, `<span class="goroutines">0</span>`)
	assert.NotContains(t, body, `<span class="memory">0 B</span>`)
}
