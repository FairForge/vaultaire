package handlers

import (
	"html/template"
	"net/http"
)

// HandleLegalPage renders a public legal document page.
// No session or database access required.
func HandleLegalPage(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tmpl.ExecuteTemplate(w, "base", map[string]any{"Page": "legal"})
	}
}
