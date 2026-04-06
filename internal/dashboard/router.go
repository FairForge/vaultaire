package dashboard

import (
	"database/sql"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// Deps groups the dependencies the dashboard routes need.
type Deps struct {
	DB       *sql.DB
	Auth     *auth.AuthService
	Sessions dashauth.SessionStore
	Logger   *zap.Logger
}

// RegisterRoutes mounts the dashboard, auth, admin, and static-asset
// routes on the given router. It MUST be called before the S3 catch-all
// in server.go so that these paths are matched first.
func RegisterRoutes(r chi.Router, deps Deps) {
	// Serve embedded static assets (CSS, JS).
	staticFS, _ := fs.Sub(Static, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Parse shared layout templates.
	baseTmpl := template.Must(template.ParseFS(Templates,
		"templates/layouts/base.html",
	))

	// --- Public auth routes ---
	r.Get("/login", renderPage(baseTmpl, "login"))
	r.Get("/register", renderPage(baseTmpl, "register"))
	r.Get("/logout", handleLogout(deps.Sessions))

	// --- Customer dashboard (session required) ---
	r.Route("/dashboard", func(dr chi.Router) {
		dr.Use(dashauth.RequireSession(deps.Sessions))
		dr.Get("/", renderPage(baseTmpl, "dashboard"))
	})

	// --- Admin (session + admin role required) ---
	r.Route("/admin", func(ar chi.Router) {
		ar.Use(dashauth.RequireAdmin(deps.Sessions))
		ar.Get("/", renderPage(baseTmpl, "admin"))
	})
}

// renderPage returns a handler that executes the "base" layout with a
// page-specific content block. Until Phase 1 templates are built, each
// page renders a minimal placeholder inside the base layout.
func renderPage(base *template.Template, page string) http.HandlerFunc {
	// Clone the base layout and add a page-specific content block.
	tmpl := template.Must(base.Clone())
	template.Must(tmpl.Parse(pageContent(page)))

	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		data := map[string]any{}
		if sd != nil {
			data["Email"] = sd.Email
			data["Role"] = sd.Role
			data["UserID"] = sd.UserID
			data["TenantID"] = sd.TenantID
		}
		data["Page"] = page
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func handleLogout(store dashauth.SessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("vaultaire_session"); err == nil {
			_ = store.Delete(r.Context(), c.Value)
		}
		dashauth.ClearSessionCookie(w)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

// pageContent returns a small template snippet that defines the "content"
// block for each page. Phase 1 replaces these with real template files.
func pageContent(page string) string {
	switch page {
	case "login":
		return `{{define "title"}}Sign In — stored.ge{{end}}` +
			`{{define "nav"}}{{end}}` +
			`{{define "content"}}` +
			`<div class="auth-page"><div class="auth-card">` +
			`<div class="auth-brand">stored.ge</div>` +
			`<h1>Sign In</h1>` +
			`<form method="POST" action="/login">` +
			`<div class="form-group"><label>Email</label><input type="email" name="email" required></div>` +
			`<div class="form-group"><label>Password</label><input type="password" name="password" required></div>` +
			`<button type="submit" class="btn btn-primary btn-block">Sign In</button>` +
			`</form>` +
			`<div class="auth-footer">No account? <a href="/register">Create one</a></div>` +
			`</div></div>{{end}}`
	case "register":
		return `{{define "title"}}Register — stored.ge{{end}}` +
			`{{define "nav"}}{{end}}` +
			`{{define "content"}}` +
			`<div class="auth-page"><div class="auth-card">` +
			`<div class="auth-brand">stored.ge</div>` +
			`<h1>Create Account</h1>` +
			`<form method="POST" action="/register">` +
			`<div class="form-group"><label>Email</label><input type="email" name="email" required></div>` +
			`<div class="form-group"><label>Password</label><input type="password" name="password" required minlength="8"></div>` +
			`<div class="form-group"><label>Company</label><input type="text" name="company"></div>` +
			`<button type="submit" class="btn btn-primary btn-block">Create Account</button>` +
			`</form>` +
			`<div class="auth-footer">Have an account? <a href="/login">Sign in</a></div>` +
			`</div></div>{{end}}`
	case "dashboard":
		return `{{define "title"}}Dashboard — stored.ge{{end}}` +
			`{{define "content"}}` +
			`<h1>Dashboard</h1>` +
			`<p>Welcome, {{.Email}}. Your dashboard is coming in Phase 1.</p>` +
			`{{end}}`
	case "admin":
		return `{{define "title"}}Admin — stored.ge{{end}}` +
			`{{define "content"}}` +
			`<h1>Admin Panel</h1>` +
			`<p>Admin features coming in Phase 3.</p>` +
			`{{end}}`
	default:
		return `{{define "content"}}<p>Page not found.</p>{{end}}`
	}
}
