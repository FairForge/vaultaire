package handlers

import (
	"html/template"
	"net/http"

	"github.com/FairForge/vaultaire/internal/modes"
)

// HandleLegalPage renders a public legal document page.
// No session or database access required.
func HandleLegalPage(tmpl *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tmpl.ExecuteTemplate(w, "base", map[string]any{"Page": "legal", "Modes": privacyModes()})
	}
}

// PrivacyMode is one row of the privacy page's per-mode table.
type PrivacyMode struct {
	Name, ServerHolds, Customer, How string
}

// privacyModes is the privacy page's per-mode table, GENERATED from
// modes.json (keystone K3, contract line 3) — only while modes_copy is on
// GLOBALLY (the owner flips it after reading: a legal page), else nil and
// the page shows no table. A sentence true only with the master key
// ("keys we hold") renders its pre-key sentence while none is loaded.
func privacyModes() []PrivacyMode {
	if !modesCopy("") {
		return nil
	}
	key := modes.MasterKeyLoaded()
	var out []PrivacyMode
	for _, m := range modes.All().Modes {
		out = append(out, PrivacyMode{Name: m.Name, ServerHolds: m.ServerHolds.Render(key),
			Customer: m.Customer.Render(key), How: m.How.Render(key)})
	}
	return out
}
