package handlers

import (
	"context"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testBackendsTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("admin").Parse(
		`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Backends{{end}}` +
			`{{define "content"}}` +
			`<h1>Storage Backends</h1>` +
			`<span class="count">{{.BackendCount}}</span>` +
			`{{range .Backends}}` +
			`<span class="backend">{{.Name}}</span>` +
			`<span class="healthy">{{.Healthy}}</span>` +
			`<span class="primary">{{.IsPrimary}}</span>` +
			`<span class="circuit">{{.CircuitState}}</span>` +
			`<span class="class">{{.StorageClass}}</span>` +
			`{{end}}` +
			`{{end}}`))
	return tmpl
}

type mockHealthChecker struct {
	states map[string]*BackendState
}

func (m *mockHealthChecker) GetBackendStates() map[string]*BackendState {
	return m.states
}

func testEngine(t *testing.T, drivers ...string) *engine.CoreEngine {
	t.Helper()
	eng := engine.NewEngine(nil, zap.NewNop(), &engine.Config{
		DefaultBackend: drivers[0],
	})
	for _, name := range drivers {
		eng.AddDriver(name, &stubDriver{name: name})
	}
	return eng
}

type stubDriver struct {
	name     string
	checkErr error
}

func (d *stubDriver) Name() string { return d.name }
func (d *stubDriver) Get(_ context.Context, _, _ string) (io.ReadCloser, error) {
	return nil, nil
}
func (d *stubDriver) Put(_ context.Context, _, _ string, _ io.Reader, _ ...engine.PutOption) error {
	return nil
}
func (d *stubDriver) Delete(_ context.Context, _, _ string) error { return nil }
func (d *stubDriver) List(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}
func (d *stubDriver) Exists(_ context.Context, _, _ string) (bool, error) { return false, nil }
func (d *stubDriver) HealthCheck(_ context.Context) error                 { return d.checkErr }

func TestHandleAdminBackends_NoSession(t *testing.T) {
	eng := testEngine(t, "local")
	handler := HandleAdminBackends(testBackendsTemplate(t), eng, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/admin/backends", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleAdminBackends_RendersList(t *testing.T) {
	eng := testEngine(t, "local", "s3")
	hc := &mockHealthChecker{
		states: map[string]*BackendState{
			"local": {Healthy: true, Score: 95.0, Latency: 2 * time.Millisecond, LastCheck: time.Now()},
			"s3":    {Healthy: true, Score: 88.0, Latency: 15 * time.Millisecond, LastCheck: time.Now()},
		},
	}
	handler := HandleAdminBackends(testBackendsTemplate(t), eng, hc, zap.NewNop())

	req := httptest.NewRequest("GET", "/admin/backends", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "Storage Backends")
	assert.Contains(t, body, "local")
	assert.Contains(t, body, "s3")
	assert.Contains(t, body, `<span class="count">2</span>`)
}

func TestHandleAdminBackends_ShowsCircuitState(t *testing.T) {
	eng := testEngine(t, "local")
	hc := &mockHealthChecker{
		states: map[string]*BackendState{
			"local": {Healthy: true, Score: 100.0, LastCheck: time.Now()},
		},
	}
	handler := HandleAdminBackends(testBackendsTemplate(t), eng, hc, zap.NewNop())

	req := httptest.NewRequest("GET", "/admin/backends", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "closed")
}

func TestHandleSetPrimary(t *testing.T) {
	eng := testEngine(t, "local", "s3")
	handler := HandleSetPrimary(eng, zap.NewNop())

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "s3")
	req := httptest.NewRequest("POST", "/admin/backends/s3/primary", nil)
	req = req.WithContext(context.WithValue(adminCtx(t), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/admin/backends", w.Header().Get("Location"))
	assert.Equal(t, "s3", eng.GetPrimary())
}

func TestHandleForceHealthCheck(t *testing.T) {
	eng := testEngine(t, "local")
	handler := HandleForceHealthCheck(eng, zap.NewNop())

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "local")
	req := httptest.NewRequest("POST", "/admin/backends/local/check", nil)
	req = req.WithContext(context.WithValue(adminCtx(t), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/admin/backends", w.Header().Get("Location"))
}

func TestHandleAdminBackends_PrimaryFirst(t *testing.T) {
	eng := testEngine(t, "s3", "local")
	eng.SetPrimary("local")
	hc := &mockHealthChecker{
		states: map[string]*BackendState{
			"local": {Healthy: true, Score: 95.0, LastCheck: time.Now()},
			"s3":    {Healthy: true, Score: 88.0, LastCheck: time.Now()},
		},
	}

	handler := HandleAdminBackends(testBackendsTemplate(t), eng, hc, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/backends", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	require.Contains(t, body, `<span class="backend">local</span>`)
	require.Contains(t, body, `<span class="primary">true</span>`)
}

func TestHandleAdminBackends_NilHealthChecker(t *testing.T) {
	eng := testEngine(t, "local")
	handler := HandleAdminBackends(testBackendsTemplate(t), eng, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/admin/backends", nil)
	req = req.WithContext(adminCtx(t))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "local")
}
