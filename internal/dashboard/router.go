package dashboard

import (
	"database/sql"
	"html/template"
	"io/fs"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/billing"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
)

const sessionTTL = 24 * time.Hour

// Deps groups the dependencies the dashboard routes need.
type Deps struct {
	DB       *sql.DB
	Auth     *auth.AuthService
	Sessions dashauth.SessionStore
	Logger   *zap.Logger
	DataPath string                 // Local storage root for bucket creation.
	Stripe   *billing.StripeService // Nil when STRIPE_SECRET_KEY is not set.
	Google   *oauth2.Config         // Nil when GOOGLE_CLIENT_ID is not set.
	GitHub   *oauth2.Config         // Nil when GITHUB_CLIENT_ID is not set.
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
	r.Get("/login", renderAuthPage(baseTmpl, "login", deps))
	r.Post("/login", handleLogin(baseTmpl, deps))
	r.Get("/register", renderAuthPage(baseTmpl, "register", deps))
	r.Post("/register", handleRegister(baseTmpl, deps))
	r.Get("/logout", handleLogout(deps.Sessions))

	// --- OAuth login ---
	if deps.Google != nil {
		r.Get("/auth/google", handlers.HandleOAuthLogin(deps.Google, deps.Logger))
		r.Get("/auth/google/callback", handlers.HandleOAuthCallback(
			deps.Google, "google", handlers.FetchGoogleUser(deps.Google),
			deps.Auth, deps.Sessions, deps.DB, deps.Logger))
	}
	if deps.GitHub != nil {
		r.Get("/auth/github", handlers.HandleOAuthLogin(deps.GitHub, deps.Logger))
		r.Get("/auth/github/callback", handlers.HandleOAuthCallback(
			deps.GitHub, "github", handlers.FetchGithubUser(deps.GitHub),
			deps.Auth, deps.Sessions, deps.DB, deps.Logger))
	}

	// --- Customer dashboard (session required) ---
	r.Route("/dashboard", func(dr chi.Router) {
		dr.Use(dashauth.RequireSession(deps.Sessions))
		dr.Use(middleware.CSRF)

		// Overview: parse the real template from embedded FS.
		overviewTmpl := template.Must(baseTmpl.Clone())
		template.Must(overviewTmpl.ParseFS(Templates,
			"templates/customer/dashboard.html",
		))
		dr.Get("/", handlers.HandleOverview(overviewTmpl, deps.DB, deps.Logger))

		// Bucket browser.
		bucketsTmpl := template.Must(baseTmpl.Clone())
		template.Must(bucketsTmpl.ParseFS(Templates,
			"templates/customer/buckets.html",
		))
		bucketObjsTmpl := template.Must(baseTmpl.Clone())
		template.Must(bucketObjsTmpl.ParseFS(Templates,
			"templates/customer/bucket_objects.html",
		))
		dr.Get("/buckets", handlers.HandleBuckets(bucketsTmpl, deps.DB, deps.DataPath, deps.Logger))
		dr.Post("/buckets", handlers.HandleCreateBucket(bucketsTmpl, deps.DB, deps.DataPath, deps.Logger))
		dr.Get("/buckets/{name}", handlers.HandleBucketObjects(bucketObjsTmpl, deps.DB, deps.Logger))

		// API key management.
		apikeysTmpl := template.Must(baseTmpl.Clone())
		template.Must(apikeysTmpl.ParseFS(Templates,
			"templates/customer/apikeys.html",
		))
		dr.Get("/apikeys", handlers.HandleAPIKeys(apikeysTmpl, deps.Auth, deps.Logger))
		dr.Post("/apikeys", handlers.HandleGenerateKey(apikeysTmpl, deps.Auth, deps.Logger))
		dr.Post("/apikeys/{id}/revoke", handlers.HandleRevokeKey(deps.Auth, deps.Logger))

		// Usage page.
		usageTmpl := template.Must(baseTmpl.Clone())
		template.Must(usageTmpl.ParseFS(Templates,
			"templates/customer/usage.html",
		))
		dr.Get("/usage", handlers.HandleUsage(usageTmpl, deps.DB, deps.Logger))

		// Settings page.
		settingsTmpl := template.Must(baseTmpl.Clone())
		template.Must(settingsTmpl.ParseFS(Templates,
			"templates/customer/settings.html",
		))
		dr.Get("/settings", handlers.HandleSettings(settingsTmpl, deps.Auth, deps.DB, deps.Logger))
		dr.Post("/settings/profile", handlers.HandleUpdateProfile(settingsTmpl, deps.Auth, deps.DB, deps.Logger))
		dr.Post("/settings/password", handlers.HandleChangePassword(settingsTmpl, deps.Auth, deps.DB, deps.Logger))
		dr.Post("/settings/notifications", handlers.HandleUpdateNotifications(settingsTmpl, deps.Auth, deps.DB, deps.Logger))

		// Billing page.
		billingTmpl := template.Must(baseTmpl.Clone())
		template.Must(billingTmpl.ParseFS(Templates,
			"templates/customer/billing.html",
		))
		dr.Get("/billing", handlers.HandleBilling(billingTmpl, deps.Stripe, deps.DB, deps.Logger))
		dr.Post("/billing/upgrade", handlers.HandleUpgrade(deps.Stripe, deps.DB, deps.Logger))
		dr.Post("/billing/portal", handlers.HandleManageBilling(deps.Stripe, deps.Logger))
	})

	// --- Admin (session + admin role required) ---
	adminTmpl := template.Must(template.ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/dashboard.html",
	))
	tenantListTmpl := template.Must(template.ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/tenants.html",
	))
	tenantDetailTmpl := template.Must(template.ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/tenant_detail.html",
	))
	systemTmpl := template.Must(template.ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/system.html",
	))

	r.Route("/admin", func(ar chi.Router) {
		ar.Use(dashauth.RequireAdmin(deps.Sessions))
		ar.Use(middleware.CSRF)
		ar.Get("/", handlers.HandleAdminOverview(adminTmpl, deps.DB, deps.Logger))
		ar.Get("/tenants", handlers.HandleTenantList(tenantListTmpl, deps.DB, deps.Logger))
		ar.Get("/tenants/{id}", handlers.HandleTenantDetail(tenantDetailTmpl, deps.DB, deps.Logger))
		ar.Post("/tenants/{id}/suspend", handlers.HandleSuspendTenant(deps.DB, deps.Logger))
		ar.Post("/tenants/{id}/enable", handlers.HandleEnableTenant(deps.DB, deps.Logger))
		ar.Post("/tenants/{id}/quota", handlers.HandleUpdateQuota(deps.DB, deps.Logger))
		ar.Post("/tenants/{id}/tier", handlers.HandleChangeTier(deps.DB, deps.Logger))
		ar.Post("/tenants/{id}/bandwidth-limit", handlers.HandleUpdateBandwidthLimit(deps.DB, deps.Logger))
		ar.Get("/system", handlers.HandleAdminSystem(systemTmpl, deps.DB, deps.Logger))
	})
}

// renderAuthPage renders a public auth page (login, register) with OAuth flags.
func renderAuthPage(base *template.Template, page string, deps Deps) http.HandlerFunc {
	tmpl := template.Must(base.Clone())
	template.Must(tmpl.Parse(pageContent(page)))

	return func(w http.ResponseWriter, r *http.Request) {
		data := map[string]any{
			"Page":      page,
			"HasGoogle": deps.Google != nil,
			"HasGithub": deps.GitHub != nil,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func handleLogin(baseTmpl *template.Template, deps Deps) http.HandlerFunc {
	errTmpl := template.Must(baseTmpl.Clone())
	template.Must(errTmpl.Parse(pageContent("login")))

	return func(w http.ResponseWriter, r *http.Request) {
		email := r.FormValue("email")
		password := r.FormValue("password")

		renderErr := func(msg string) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_ = errTmpl.ExecuteTemplate(w, "base", map[string]any{
				"Error":     msg,
				"Email":     email,
				"Page":      "login",
				"HasGoogle": deps.Google != nil,
				"HasGithub": deps.GitHub != nil,
			})
		}

		valid, err := deps.Auth.ValidatePassword(r.Context(), email, password)
		if err != nil || !valid {
			renderErr("Invalid email or password.")
			return
		}

		user, err := deps.Auth.GetUserByEmail(r.Context(), email)
		if err != nil {
			renderErr("Invalid email or password.")
			return
		}

		// Determine role — default to "user" if the DB column isn't loaded yet.
		role := "user"
		if deps.DB != nil {
			_ = deps.DB.QueryRowContext(r.Context(),
				`SELECT role FROM users WHERE id = $1`, user.ID).Scan(&role)
		}

		token, err := deps.Sessions.Create(r.Context(), dashauth.SessionData{
			UserID:   user.ID,
			TenantID: user.TenantID,
			Email:    user.Email,
			Role:     role,
		}, sessionTTL)
		if err != nil {
			deps.Logger.Error("create session", zap.Error(err))
			renderErr("Something went wrong. Please try again.")
			return
		}

		dashauth.SetSessionCookie(w, token, sessionTTL)
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}

func handleRegister(baseTmpl *template.Template, deps Deps) http.HandlerFunc {
	errTmpl := template.Must(baseTmpl.Clone())
	template.Must(errTmpl.Parse(pageContent("register")))

	return func(w http.ResponseWriter, r *http.Request) {
		email := r.FormValue("email")
		password := r.FormValue("password")
		company := r.FormValue("company")

		renderErr := func(msg string) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_ = errTmpl.ExecuteTemplate(w, "base", map[string]any{
				"Error":     msg,
				"Email":     email,
				"Company":   company,
				"Page":      "register",
				"HasGoogle": deps.Google != nil,
				"HasGithub": deps.GitHub != nil,
			})
		}

		if len(password) < 8 {
			renderErr("Password must be at least 8 characters.")
			return
		}

		user, _, _, err := deps.Auth.CreateUserWithTenant(r.Context(), email, password, company)
		if err != nil {
			if err.Error() == "user already exists" {
				renderErr("An account with that email already exists.")
			} else {
				deps.Logger.Error("registration failed", zap.Error(err))
				renderErr("Registration failed. Please try again.")
			}
			return
		}

		// Create Stripe customer for billing (non-blocking).
		if deps.Stripe != nil {
			if _, stripeErr := deps.Stripe.CreateCustomer(r.Context(), email, user.TenantID); stripeErr != nil {
				deps.Logger.Error("create stripe customer on registration",
					zap.String("tenant", user.TenantID), zap.Error(stripeErr))
			}
		}

		token, err := deps.Sessions.Create(r.Context(), dashauth.SessionData{
			UserID:   user.ID,
			TenantID: user.TenantID,
			Email:    user.Email,
			Role:     "user",
		}, sessionTTL)
		if err != nil {
			deps.Logger.Error("create session after register", zap.Error(err))
			renderErr("Account created but login failed. Please sign in.")
			return
		}

		dashauth.SetSessionCookie(w, token, sessionTTL)
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
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
			`{{if .Error}}<div class="alert alert-error">{{.Error}}</div>{{end}}` +
			`{{if or .HasGoogle .HasGithub}}` +
			`<div class="oauth-buttons">` +
			`{{if .HasGoogle}}<a href="/auth/google" class="btn btn-oauth btn-google">Sign in with Google</a>{{end}}` +
			`{{if .HasGithub}}<a href="/auth/github" class="btn btn-oauth btn-github">Sign in with GitHub</a>{{end}}` +
			`</div>` +
			`<div class="auth-divider"><span>or</span></div>` +
			`{{end}}` +
			`<form method="POST" action="/login">` +
			`<div class="form-group"><label>Email</label><input type="email" name="email" value="{{.Email}}" required></div>` +
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
			`{{if .Error}}<div class="alert alert-error">{{.Error}}</div>{{end}}` +
			`{{if or .HasGoogle .HasGithub}}` +
			`<div class="oauth-buttons">` +
			`{{if .HasGoogle}}<a href="/auth/google" class="btn btn-oauth btn-google">Sign up with Google</a>{{end}}` +
			`{{if .HasGithub}}<a href="/auth/github" class="btn btn-oauth btn-github">Sign up with GitHub</a>{{end}}` +
			`</div>` +
			`<div class="auth-divider"><span>or</span></div>` +
			`{{end}}` +
			`<form method="POST" action="/register">` +
			`<div class="form-group"><label>Email</label><input type="email" name="email" value="{{.Email}}" required></div>` +
			`<div class="form-group"><label>Password</label><input type="password" name="password" required minlength="8"></div>` +
			`<div class="form-group"><label>Company</label><input type="text" name="company" value="{{.Company}}"></div>` +
			`<button type="submit" class="btn btn-primary btn-block">Create Account</button>` +
			`</form>` +
			`<div class="auth-footer">Have an account? <a href="/login">Sign in</a></div>` +
			`</div></div>{{end}}`
	default:
		return `{{define "content"}}<p>Page not found.</p>{{end}}`
	}
}
