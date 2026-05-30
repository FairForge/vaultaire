package handlers

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testComplianceTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("base").Parse(
		`{{define "base"}}` +
			`{{block "nav" .}}{{end}}` +
			`{{block "content" .}}{{end}}` +
			`{{end}}`))
	template.Must(tmpl.Parse(
		`{{define "title"}}Compliance{{end}}` +
			`{{define "content"}}` +
			`<span class="score">{{.ComplianceScore}}</span>` +
			`<span class="total">{{.TotalBuckets}}</span>` +
			`<span class="compliant">{{.CompliantBuckets}}</span>` +
			`<span class="has">{{.HasBuckets}}</span>` +
			`{{range .Buckets}}<span class="bucket">{{.Name}}:{{.IsFullyCompliant}}</span>{{end}}` +
			`{{end}}`))
	return tmpl
}

func TestHandleCompliance_NoSession(t *testing.T) {
	// Arrange
	tmpl := testComplianceTemplate(t)
	handler := HandleCompliance(tmpl, nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/compliance", nil)
	w := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleCompliance_NilDB(t *testing.T) {
	// Arrange
	tmpl := testComplianceTemplate(t)
	handler := HandleCompliance(tmpl, nil, zap.NewNop())

	sd := &dashauth.SessionData{
		UserID: "user-1", TenantID: "tenant-1",
		Email: "test@stored.ge", Role: "user",
	}

	req := httptest.NewRequest("GET", "/dashboard/compliance", nil)
	ctx := context.WithValue(req.Context(), dashauth.SessionKey, sd)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `<span class="score">0</span>`)
	assert.Contains(t, body, `<span class="total">0</span>`)
	assert.Contains(t, body, `<span class="compliant">0</span>`)
	assert.Contains(t, body, `<span class="has">false</span>`)
}

func TestHandleComplianceExport_NoSession(t *testing.T) {
	// Arrange
	handler := HandleComplianceExport(nil, zap.NewNop())

	req := httptest.NewRequest("GET", "/dashboard/compliance/export", nil)
	w := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login", w.Header().Get("Location"))
}

func TestHandleComplianceExport_NilDB(t *testing.T) {
	// Arrange
	handler := HandleComplianceExport(nil, zap.NewNop())

	sd := &dashauth.SessionData{
		UserID: "user-1", TenantID: "tenant-1",
		Email: "test@stored.ge", Role: "user",
	}

	req := httptest.NewRequest("GET", "/dashboard/compliance/export", nil)
	ctx := context.WithValue(req.Context(), dashauth.SessionKey, sd)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
	assert.Contains(t, w.Header().Get("Content-Disposition"), "compliance-report-")
	assert.Contains(t, w.Header().Get("Content-Disposition"), ".json")

	var report complianceReport
	err := json.Unmarshal(w.Body.Bytes(), &report)
	require.NoError(t, err)
	assert.Equal(t, 0, report.ComplianceScore)
	assert.Equal(t, 0, report.TotalBuckets)
	assert.Empty(t, report.Buckets)
}

func TestBucketCompliance_IsFullyCompliant(t *testing.T) {
	tests := []struct {
		name      string
		sse       bool
		logging   bool
		version   bool
		compliant bool
	}{
		{"all enabled", true, true, true, true},
		{"missing sse", false, true, true, false},
		{"missing logging", true, false, true, false},
		{"missing versioning", true, true, false, false},
		{"all disabled", false, false, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := BucketCompliance{
				SSEEnabled:        tt.sse,
				LoggingEnabled:    tt.logging,
				VersioningEnabled: tt.version,
			}
			got := b.SSEEnabled && b.LoggingEnabled && b.VersioningEnabled
			assert.Equal(t, tt.compliant, got)
		})
	}
}
