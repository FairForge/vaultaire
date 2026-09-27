package handlers

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// A registered driver that no probe has reported on is "not probed", never
// "unhealthy": the shipped template rendered the zero Healthy as unhealthy
// for every unprobed driver (Review R7 follow-up). Rendered against the real
// admin/backends.html so the badge under test is the shipped one.
func realBackendsTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("admin").Parse(`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	return template.Must(tmpl.ParseFiles("../templates/admin/backends.html"))
}

func TestHandleAdminBackends_UnprobedIsNotUnhealthy(t *testing.T) {
	eng := testEngine(t, "local", "idrive", "geyser")
	hc := &mockHealthChecker{states: map[string]*BackendState{
		"idrive": {Healthy: true, Score: 100, Latency: 20 * time.Millisecond, LastCheck: time.Now()},
		"geyser": {Healthy: false, Score: 0, LastCheck: time.Now(), LastError: "403 Forbidden"},
		// local: registered, no probe state
	}}
	handler := HandleAdminBackends(realBackendsTemplate(t), eng, hc, zap.NewNop())

	req := httptest.NewRequest("GET", "/admin/backends", nil).WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()

	assert.Equal(t, 1, countSubstring(body, ">not probed<"), "exactly one unprobed backend (local)")
	assert.Equal(t, 1, countSubstring(body, ">healthy<"), "idrive")
	assert.Equal(t, 1, countSubstring(body, ">unhealthy<"), "geyser — a real failing probe still reads unhealthy")
}

func countSubstring(s, sub string) int {
	n := 0
	for i := 0; ; {
		j := indexFrom(s, sub, i)
		if j < 0 {
			return n
		}
		n++
		i = j + len(sub)
	}
}

func indexFrom(s, sub string, from int) int {
	for i := from; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
