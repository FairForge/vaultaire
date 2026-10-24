package dashboard

import (
	"html/template"
	"io/fs"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/modes"
	"github.com/FairForge/vaultaire/internal/produce"
)

// Plan 43.4 (Stage 1a Prompt 38 Part A): the law-enforcement policy and the
// "what we can produce" page, on the canonical origin and the trust host
// alike, behind legal_le_pages (default OFF = 404, read globally). The
// produce page's rows are internal/produce's (produce.json, every row a code
// fact under test); the policy's procedural sentences are counsel's and
// render as a marked DRAFT until then (F-36).

// legalLEPages maps each page's path to its template.
var legalLEPages = map[string]string{
	"/legal/law-enforcement":     "templates/legal/law-enforcement.html",
	"/legal/what-we-can-produce": "templates/legal/what-we-can-produce.html",
}

// LegalLEAssets are the static files the pages' layout loads (base.html's
// stylesheet and scripts, the stylesheet's fonts): what the trust host
// serves beside the pages, and nothing else of /static/
// (TestLegalLEAssets_AreWhatTheLayoutLoads).
var LegalLEAssets = []string{
	"/static/css/style.css",
	"/static/js/htmx.min.js",
	"/static/js/dashboard.js",
	"/static/fonts/montserrat.woff2",
	"/static/fonts/silk400.woff2",
	"/static/fonts/silk700.woff2",
}

// LegalLE serves the two pages (and, on the trust host, their assets).
type LegalLE struct {
	flags  *flags.Service
	pages  map[string]*template.Template
	static http.Handler
}

// NewLegalLE parses the pages; fl nil = always OFF.
func NewLegalLE(fl *flags.Service) *LegalLE {
	base := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates, "templates/layouts/base.html"))
	l := &LegalLE{flags: fl, pages: map[string]*template.Template{}}
	for p, file := range legalLEPages {
		t := template.Must(base.Clone())
		l.pages[p] = template.Must(t.ParseFS(Templates, file))
	}
	staticFS, _ := fs.Sub(Static, "static")
	l.static = http.StripPrefix("/static/", staticHandler(http.FS(staticFS)))
	return l
}

func (l *LegalLE) on() bool { return legalLEOn(l.flags) }

func legalLEOn(fl *flags.Service) bool {
	return fl != nil && fl.Enabled(produce.FlagLegalLEPages, "")
}

// LegalLENotFound is the pages' 404 on the canonical origin while the flag
// is OFF: plain, never cached (a 404 must not outlive the flip at an edge)
// — the same answer as the trust host's.
func LegalLENotFound(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("404 page not found\n"))
}

// Mount registers GET and HEAD of both pages on r — and, withAssets (the
// trust host), of LegalLEAssets — answering notFound while the flag is OFF.
func (l *LegalLE) Mount(r chi.Router, notFound http.HandlerFunc, withAssets bool) {
	for p := range legalLEPages {
		h := l.page(p, notFound)
		r.Get(p, h)
		r.Head(p, h)
	}
	if !withAssets {
		return
	}
	asset := func(w http.ResponseWriter, r *http.Request) {
		if !l.on() {
			notFound(w, r)
			return
		}
		l.static.ServeHTTP(w, r)
	}
	for _, p := range LegalLEAssets {
		r.Get(p, asset)
		r.Head(p, asset)
	}
}

func (l *LegalLE) page(path string, notFound http.HandlerFunc) http.HandlerFunc {
	tmpl := l.pages[path]
	render := middleware.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tmpl.ExecuteTemplate(w, "base", l.data())
	}))
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.on() {
			notFound(w, r)
			return
		}
		render.ServeHTTP(w, r)
	}
}

// LegalLEMode is one mode as the produce page shows it.
type LegalLEMode struct{ Name, ServerHolds, Customer string }

// LegalLEAccount is one export section as the produce page shows it.
type LegalLEAccount struct{ Section, Label, Fields string }

// LegalLERetention is one retention row as the produce page shows it.
type LegalLERetention struct {
	Table, What string
	Days        int
	Scrub       string // the blanked columns; "" = rows deleted
}

// data is the pages' template data: every fact from produce.json and
// modes.json, a flag-gated sentence only while its flag is ON globally.
func (l *LegalLE) data() map[string]any {
	f := produce.All()
	key := modes.MasterKeyLoaded()
	var ms []LegalLEMode
	for _, id := range f.Modes {
		m, _ := modes.Get(id)
		ms = append(ms, LegalLEMode{Name: m.Name, ServerHolds: m.ServerHolds.Render(key), Customer: m.Customer.Render(key)})
	}
	var acct []LegalLEAccount
	for _, a := range f.Account {
		acct = append(acct, LegalLEAccount{Section: a.Section, Label: a.Label, Fields: strings.Join(a.Fields, ", ")})
	}
	var ret []LegalLERetention
	for _, r := range f.Retention {
		ret = append(ret, LegalLERetention{Table: r.Table, What: r.What, Days: r.Days, Scrub: strings.Join(r.Scrub, ", ")})
	}
	gated := func(g produce.Gated) string {
		if l.flags != nil && l.flags.Enabled(g.RequiresFlag, "") {
			return g.Text
		}
		return ""
	}
	return map[string]any{
		"Page":        "legal",
		"Modes":       ms,
		"Account":     acct,
		"Retention":   ret,
		"ExportsDays": f.ExportsDays,
		"Kept":        f.Kept,
		"Backups":     f.Backups,
		"SSEC":        f.PrivateSSEC,
		"AdminReads":  gated(f.AdminReads),
		"Builds":      gated(f.Builds),
		"Caveat":      f.Caveat,
	}
}
