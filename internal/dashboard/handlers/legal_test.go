package handlers_test

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const baseLayout = `{{define "base"}}<!DOCTYPE html><html><body>{{block "content" .}}{{end}}</body></html>{{end}}`

func legalTmpl(t *testing.T, content string) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("base").Parse(baseLayout))
	template.Must(tmpl.Parse(content))
	return tmpl
}

func TestHandleLegalPage_ReturnsHTML(t *testing.T) {
	// Arrange
	tmpl := legalTmpl(t, `{{define "title"}}Test — stored.ge{{end}}{{define "content"}}<h1>Test Page</h1>{{end}}`)
	handler := handlers.HandleLegalPage(tmpl)
	req := httptest.NewRequest(http.MethodGet, "/legal/test", nil)
	rec := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, rec.Body.String(), "<h1>Test Page</h1>")
}

func TestLegalPrivacy_ContainsGDPRSections(t *testing.T) {
	// Arrange
	tmpl := legalTmpl(t, `{{define "content"}}`+
		`<h2 id="data-subject-rights">Data Subject Rights</h2>`+
		`<h2 id="lawful-basis">Lawful Basis for Processing</h2>`+
		`<h2 id="retention">Retention Periods</h2>`+
		`{{end}}`)
	handler := handlers.HandleLegalPage(tmpl)
	req := httptest.NewRequest(http.MethodGet, "/legal/privacy", nil)
	rec := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Data Subject Rights")
	assert.Contains(t, body, "Lawful Basis")
	assert.Contains(t, body, "Retention")
}

func TestLegalTerms_ContainsSLA(t *testing.T) {
	// Arrange
	tmpl := legalTmpl(t, `{{define "content"}}`+
		`<h2 id="service-level">Service Level Agreement</h2>`+
		`<h2 id="liability">Limitation of Liability</h2>`+
		`{{end}}`)
	handler := handlers.HandleLegalPage(tmpl)
	req := httptest.NewRequest(http.MethodGet, "/legal/terms", nil)
	rec := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Service Level")
	assert.Contains(t, body, "Limitation of Liability")
}

func TestLegalDPA_ContainsArticle28(t *testing.T) {
	// Arrange
	tmpl := legalTmpl(t, `{{define "content"}}`+
		`<p>Article 28 of the GDPR</p>`+
		`<h2 id="sub-processors">Sub-processors</h2>`+
		`{{end}}`)
	handler := handlers.HandleLegalPage(tmpl)
	req := httptest.NewRequest(http.MethodGet, "/legal/dpa", nil)
	rec := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Article 28")
	assert.Contains(t, body, "Sub-processors")
}

func TestLegalCookies_NoTracking(t *testing.T) {
	// Arrange
	tmpl := legalTmpl(t, `{{define "content"}}`+
		`<h2 id="no-tracking">No Tracking Cookies</h2>`+
		`<p>stored.ge does <strong>not</strong> use analytics or advertising cookies.</p>`+
		`{{end}}`)
	handler := handlers.HandleLegalPage(tmpl)
	req := httptest.NewRequest(http.MethodGet, "/legal/cookies", nil)
	rec := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "No Tracking Cookies")
	assert.Contains(t, body, "not")
}

func TestLegalAUP_ContainsProhibited(t *testing.T) {
	// Arrange
	tmpl := legalTmpl(t, `{{define "content"}}`+
		`<h2 id="prohibited-content">Prohibited Content</h2>`+
		`<h2 id="enforcement">Enforcement</h2>`+
		`{{end}}`)
	handler := handlers.HandleLegalPage(tmpl)
	req := httptest.NewRequest(http.MethodGet, "/legal/aup", nil)
	rec := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Prohibited Content")
	assert.Contains(t, body, "Enforcement")
}
