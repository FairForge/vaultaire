package dashboard

import (
	"bytes"
	"html/template"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/api/landing"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/modes"
)

// Stage 1a Prompt 39 Part A: the web host answers like a website — the
// pixel 404 page a browser gets for a path nothing serves, and the two legal
// pages a launch reader types, dark behind legal_draft_pages (default OFF =
// that 404, global only). Every commitment on the pages renders as a marked
// DRAFT (the F-36 pattern of /legal/law-enforcement): refunds are owner item
// F-28, the DMCA agent F-37. The paragraphs that are not drafts carry
// data-fact and state only code facts: the egress allowance (prices.json),
// the deletion grace (internal/account), the abuse form and address, the
// mode names and what each holds (modes.json).

// FlagLegalDraftPages serves /legal/refunds and /legal/dmca.
const FlagLegalDraftPages = "legal_draft_pages"

// legalDraftPages maps each page's path to its template.
var legalDraftPages = map[string]string{
	"/legal/refunds": "templates/legal/refunds.html",
	"/legal/dmca":    "templates/legal/dmca.html",
}

func parseLegalPage(file string) *template.Template {
	base := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates, "templates/layouts/base.html"))
	return template.Must(base.ParseFS(Templates, file))
}

var web404Tmpl = parseLegalPage("templates/errors/404.html")

// WebNotFound is the web host's 404 page (the legal layout), never cached:
// a 404 must not outlive a page that starts to exist at an edge. HEAD gets
// the headers alone.
var WebNotFound http.HandlerFunc = middleware.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	_ = web404Tmpl.ExecuteTemplate(&buf, "base", map[string]any{"Page": "legal"})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf.Bytes())
	}
})).ServeHTTP

// LegalDraft serves the two pages.
type LegalDraft struct {
	flags *flags.Service
	pages map[string]*template.Template
}

// NewLegalDraft parses the pages; fl nil = always OFF.
func NewLegalDraft(fl *flags.Service) *LegalDraft {
	l := &LegalDraft{flags: fl, pages: map[string]*template.Template{}}
	for p, file := range legalDraftPages {
		l.pages[p] = parseLegalPage(file)
	}
	return l
}

func (l *LegalDraft) on() bool {
	return l.flags != nil && l.flags.Enabled(FlagLegalDraftPages, "")
}

// Mount registers GET and HEAD of both pages on r, answering WebNotFound
// while the flag is OFF.
func (l *LegalDraft) Mount(r chi.Router) {
	for p, tmpl := range l.pages {
		render := middleware.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = tmpl.ExecuteTemplate(w, "base", l.data())
		}))
		h := func(w http.ResponseWriter, r *http.Request) {
			if !l.on() {
				WebNotFound(w, r)
				return
			}
			render.ServeHTTP(w, r)
		}
		r.Get(p, h)
		r.Head(p, h)
	}
}

// LegalDraftMode is one mode as the DMCA page shows it.
type LegalDraftMode struct{ Name, ServerHolds string }

// data is the pages' template data: code facts only.
func (l *LegalDraft) data() map[string]any {
	e := landing.Get().Egress
	key := modes.MasterKeyLoaded()
	var ms []LegalDraftMode
	for _, m := range modes.All().Modes {
		ms = append(ms, LegalDraftMode{Name: m.Name, ServerHolds: m.ServerHolds.Render(key)})
	}
	return map[string]any{
		"Page":          "legal",
		"StdRatio":      e.StandardFreeRatio,
		"VaultRatio":    e.VaultRestoreFreeRatio,
		"PastAllowance": e.PastAllowance,
		"OneAllowance":  e.OneAllowance,
		"GraceDays":     int(account.GracePeriod.Hours() / 24),
		"Modes":         ms,
	}
}
