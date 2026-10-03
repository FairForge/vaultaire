package dashboard

import (
	"database/sql"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/tenant"

	"github.com/FairForge/vaultaire/internal/api/landing"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/billing"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/FairForge/vaultaire/internal/email"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/usage"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
)

const sessionTTL = 24 * time.Hour

// Deps groups the dependencies the dashboard routes need.
type Deps struct {
	DB            *sql.DB
	Auth          *auth.AuthService
	MFA           *auth.MFAService            // TOTP secret generation / validation.
	MFAPending    *MFAPendingStore            // Short-lived store for 2FA login challenges.
	MFAEnrol      *handlers.MFAEnrolmentStore // Pending TOTP enrolments (WP-R12-8); nil = one is created here.
	Sessions      dashauth.SessionStore
	Logger        *zap.Logger
	DataPath      string                   // Local storage root (bucket list sizes in dev).
	CreateBucket  handlers.BucketCreator   // The API layer's bucket registry (nil = creation refused).
	Stripe        *billing.StripeService   // Nil when STRIPE_SECRET_KEY is not set.
	Google        *oauth2.Config           // Nil when GOOGLE_CLIENT_ID is not set.
	GitHub        *oauth2.Config           // Nil when GITHUB_CLIENT_ID is not set.
	StorageMode   string                   // e.g. "local", "s3", "quotaless", "geyser", "idrive"
	Email         email.Sender             // Email sender (LogSender if unconfigured).
	BaseURL       string                   // Base URL for email links (e.g. "https://stored.ge").
	Engine        *engine.CoreEngine       // Nil-safe; used by admin backends page.
	HealthChecker handlers.HealthChecker   // Nil-safe; backend health state provider.
	Flags         *flags.Service           // Nil-safe; admin feature-flags page (1.13).
	Quotas        billing.HouseQuotas      // Nil-safe; applies a resized house's floor quotas at once (Phase 1).
	Account       *account.Service         // The one account-deletion state machine (WP-R10-3); nil = built per request from DB.
	Exports       handlers.ExportService   // The one GDPR export service (WP-R10-3b); nil = the settings page says export is unavailable.
	Egress        usage.EgressStatusReader // The API server's live egress counter (WP-R10-9); nil = the recorded month.
	// CSRFKey keys the session-bound CSRF token (WP-R12-5): CSRFKeyFromSecret
	// of a secret that survives restarts (prod: JWT_SECRET). Nil = a random
	// key for this process — forms opened before a restart then fail once.
	CSRFKey []byte
}

// CSRFKeyFromSecret derives the CSRF key from a persistent server secret
// with its own label (middleware.DeriveCSRFKey): the secret itself is never
// the key.
func CSRFKeyFromSecret(secret string) []byte { return middleware.DeriveCSRFKey(secret) }

// RegisterRoutes mounts the dashboard, auth, admin, and static-asset
// routes on the given router. It MUST be called before the S3 catch-all
// in server.go so that these paths are matched first.
func RegisterRoutes(r chi.Router, deps Deps) {
	// Serve embedded static assets (CSS, JS, fonts).
	staticFS, _ := fs.Sub(Static, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", staticHandler(http.FS(staticFS))))

	// Parse shared layout templates.
	baseTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/base.html",
	))

	// One CSRF middleware for both session chains (/dashboard and /admin):
	// the token is an HMAC over the session id, checked together with
	// Sec-Fetch-Site and Origin against the dashboard's own origins.
	csrfKeyBytes := deps.CSRFKey
	if len(csrfKeyBytes) == 0 {
		csrfKeyBytes = middleware.DeriveCSRFKey("")
		if deps.Logger != nil {
			deps.Logger.Warn("dashboard: no CSRF key configured (JWT_SECRET unset?) — using a random key; forms opened before a restart will be refused once")
		}
	}
	csrf := middleware.NewCSRF(csrfKeyBytes, deps.BaseURL)

	// Rate limiter: 5 attempts/min per IP for login and 2FA. The account
	// lockout is the per-ACCOUNT complement (10 failures in 15 min → locked
	// 15 min), so a distributed guess against one mailbox is stopped too.
	loginRL := middleware.NewLoginRateLimiter(5, 5)
	lockout := middleware.NewAccountLockout(10, 15*time.Minute, 15*time.Minute)
	// Registration: 10/min per IP (it used to be unlimited — mass account
	// creation minted a key pair and four DB rows per request).
	registerRL := middleware.NewLoginRateLimiter(10, 10)
	// The pending-2FA store behind the interface both login entry points use.
	var mfaGate handlers.MFAChallenger
	if deps.MFAPending != nil {
		mfaGate = mfaChallenger{deps.MFAPending}
	}

	// Public pages get the same browser security headers as the dashboard
	// (the login, register, reset and abuse forms used to ship without a CSP).
	r.Group(func(pub chi.Router) {
		pub.Use(middleware.SecurityHeaders)

		// --- Public auth routes ---
		pub.Get("/login", renderAuthPage(baseTmpl, "login", deps))
		pub.Post("/login", loginRL.Limit(handleLogin(baseTmpl, deps, lockout, mfaGate)).ServeHTTP)
		// When signups are closed, the register page redirects to the homepage
		// (waitlist). The POST is still gated server-side at CreateUserWithTenant.
		pub.Get("/register", func(w http.ResponseWriter, r *http.Request) {
			if deps.Auth != nil && !deps.Auth.SignupsEnabled() {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			renderAuthPage(baseTmpl, "register", deps)(w, r)
		})
		pub.Post("/register", registerRL.Limit(handleRegister(baseTmpl, deps)).ServeHTTP)
		pub.Get("/logout", handleLogout(deps.Sessions))

		// --- Email verification (public) ---
		pub.Get("/verify", handleVerifyEmail(baseTmpl, deps))

		// --- Password reset (public) ---
		resetRL := middleware.NewLoginRateLimiter(5, 5)
		pub.Get("/forgot-password", renderAuthPage(baseTmpl, "forgot-password", deps))
		pub.Post("/forgot-password", resetRL.Limit(handleForgotPassword(baseTmpl, deps)).ServeHTTP)
		pub.Get("/reset-password", handleResetPasswordForm(baseTmpl, deps))
		pub.Post("/reset-password", resetRL.Limit(handleResetPassword(baseTmpl, deps)).ServeHTTP)

		// --- 2FA verification (public, used during login) ---
		pub.Get("/login/verify-2fa", renderAuthPage(baseTmpl, "verify-2fa", deps))
		pub.Post("/login/verify-2fa", loginRL.Limit(handleVerify2FA(baseTmpl, deps, lockout)).ServeHTTP)

		// --- OAuth login ---
		// New OAuth signups get the same reveal-once credentials page as the
		// web register form (B2) — without it the minted secret was discarded.
		// Existing accounts with TOTP go through the same second factor as the
		// password form (R5-06) via mfaGate.
		oauthCreds := signupCredsRenderer(baseTmpl, deps)
		if deps.Google != nil {
			pub.Get("/auth/google", handlers.HandleOAuthLogin(deps.Google, deps.Logger))
			pub.Get("/auth/google/callback", handlers.HandleOAuthCallback(
				deps.Google, "google", handlers.FetchGoogleUser(deps.Google),
				deps.Auth, deps.Sessions, deps.DB, mfaGate, deps.Logger, oauthCreds))
		}
		if deps.GitHub != nil {
			pub.Get("/auth/github", handlers.HandleOAuthLogin(deps.GitHub, deps.Logger))
			pub.Get("/auth/github/callback", handlers.HandleOAuthCallback(
				deps.GitHub, "github", handlers.FetchGithubUser(deps.GitHub),
				deps.Auth, deps.Sessions, deps.DB, mfaGate, deps.Logger, oauthCreds))
		}

		// --- Legal pages (public) ---
		legalPages := map[string]string{
			"privacy":  "templates/legal/privacy.html",
			"terms":    "templates/legal/terms.html",
			"dpa":      "templates/legal/dpa.html",
			"cookies":  "templates/legal/cookies.html",
			"aup":      "templates/legal/aup.html",
			"baa":      "templates/legal/baa.html",
			"gdpr":     "templates/legal/gdpr.html",
			"data-act": "templates/legal/data-act.html",
		}
		for slug, tmplPath := range legalPages {
			pageTmpl := template.Must(baseTmpl.Clone())
			template.Must(pageTmpl.ParseFS(Templates, tmplPath))
			pub.Get("/legal/"+slug, handlers.HandleLegalPage(pageTmpl))
		}
		pub.Get("/compliance/gdpr", http.RedirectHandler("/legal/gdpr", http.StatusMovedPermanently).ServeHTTP)
		pub.Get("/compliance/data-act", http.RedirectHandler("/legal/data-act", http.StatusMovedPermanently).ServeHTTP)

		// --- Public abuse report form ---
		abusePubTmpl := template.Must(baseTmpl.Clone())
		template.Must(abusePubTmpl.ParseFS(Templates, "templates/public/abuse.html"))
		pub.Get("/abuse", handlers.HandleAbuseForm(abusePubTmpl, deps.Logger))
		abuseRL := middleware.NewLoginRateLimiter(5, 5)
		pub.Post("/abuse", abuseRL.Limit(handlers.HandleAbuseSubmit(abusePubTmpl, deps.DB, deps.Logger)).ServeHTTP)
	})

	// --- Customer dashboard (session required) ---
	r.Route("/dashboard", func(dr chi.Router) {
		dr.Use(middleware.Recovery(deps.Logger))
		dr.Use(middleware.SecurityHeaders)
		dr.Use(dashauth.RequireSession(deps.Sessions))
		dr.Use(csrf)
		dr.Use(middleware.Flash)
		dr.NotFound(handlers.HandleNotFound(deps.Logger))

		// Overview: parse the real template from embedded FS.
		overviewTmpl := template.Must(baseTmpl.Clone())
		template.Must(overviewTmpl.ParseFS(Templates,
			"templates/customer/dashboard.html",
			"templates/generated/house.html", // the house: sprites, room, CSS (make landing)
		))
		dr.Get("/", handlers.HandleOverview(overviewTmpl, deps.DB, deps.Logger, deps.StorageMode, deps.Flags, deps.Egress))

		// Bucket browser.
		bucketsTmpl := template.Must(baseTmpl.Clone())
		template.Must(bucketsTmpl.ParseFS(Templates,
			"templates/customer/buckets.html",
		))
		bucketObjsTmpl := template.Must(baseTmpl.Clone())
		template.Must(bucketObjsTmpl.ParseFS(Templates,
			"templates/customer/bucket_objects.html",
		))
		bucketSettingsTmpl := template.Must(baseTmpl.Clone())
		template.Must(bucketSettingsTmpl.ParseFS(Templates,
			"templates/customer/bucket_settings.html",
		))
		dr.Get("/buckets", handlers.HandleBuckets(bucketsTmpl, deps.DB, deps.DataPath, deps.Logger))
		dr.Post("/buckets", handlers.HandleCreateBucket(bucketsTmpl, deps.DB, deps.CreateBucket, deps.Logger))
		// A system bucket (tenant.ExportsBucket, WP-R10-3b) has no page: 404
		// before any handler, as the S3 API answers NoSuchBucket.
		dr.With(noSystemBucket).Get("/buckets/{name}", handlers.HandleBucketObjects(bucketObjsTmpl, deps.DB, deps.Logger))
		dr.With(noSystemBucket).Post("/buckets/{name}/restore", handlers.HandleRestoreObject(deps.Engine, deps.DB, deps.Logger))
		dr.With(noSystemBucket).Get("/buckets/{name}/restore-status", handlers.HandleObjectRestoreStatus(deps.Engine, deps.DB, deps.Logger))
		dr.With(noSystemBucket).Get("/buckets/{name}/settings", handlers.HandleBucketSettings(bucketSettingsTmpl, deps.DB, deps.Logger))
		dr.With(noSystemBucket).Post("/buckets/{name}/settings", handlers.HandleUpdateBucketSettings(bucketSettingsTmpl, deps.DB, deps.Logger))

		// Bucket CDN analytics.
		analyticsTmpl := template.Must(baseTmpl.Clone())
		template.Must(analyticsTmpl.ParseFS(Templates,
			"templates/customer/bucket_analytics.html",
		))
		dr.With(noSystemBucket).Get("/buckets/{name}/analytics", handlers.HandleBucketAnalytics(analyticsTmpl, deps.DB, deps.Logger))

		// API key management.
		apikeysTmpl := template.Must(baseTmpl.Clone())
		template.Must(apikeysTmpl.ParseFS(Templates,
			"templates/customer/apikeys.html",
		))
		dr.Get("/apikeys", handlers.HandleAPIKeys(apikeysTmpl, deps.Auth, deps.Logger))
		dr.Post("/apikeys", handlers.HandleGenerateKey(apikeysTmpl, deps.Auth, deps.DB, deps.Logger))
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
		dr.Get("/settings", handlers.HandleSettings(settingsTmpl, deps.Auth, deps.DB, deps.Sessions, deps.Exports, deps.Logger))
		dr.Post("/settings/profile", handlers.HandleUpdateProfile(settingsTmpl, deps.Auth, deps.DB, deps.Logger))
		dr.Post("/settings/password", handlers.HandleChangePassword(settingsTmpl, deps.Auth, deps.DB, deps.Sessions, deps.Logger))
		dr.Post("/settings/notifications", handlers.HandleUpdateNotifications(settingsTmpl, deps.Auth, deps.DB, deps.Logger))

		// Active sessions / sign out other devices (Phase 5.8).
		dr.Post("/settings/sessions/revoke-all", handlers.HandleRevokeAllOtherSessions(deps.Sessions, deps.Logger))
		dr.Post("/settings/sessions/{id}/revoke", handlers.HandleRevokeSession(deps.Sessions, deps.Logger))

		// 2FA settings.
		mfaSetupTmpl := template.Must(baseTmpl.Clone())
		template.Must(mfaSetupTmpl.ParseFS(Templates,
			"templates/customer/mfa_setup.html",
		))
		// Enrolment (WP-R12-8): the pending TOTP secret stays in this
		// process, keyed by the session; the QR code is a PNG the server
		// renders; the enable POST carries the 6-digit code only and answers
		// with the backup codes, once, on the setup template.
		mfaEnrol := deps.MFAEnrol
		if mfaEnrol == nil {
			mfaEnrol = handlers.NewMFAEnrolmentStore()
		}
		dr.Get("/settings/mfa", handlers.HandleMFASetup(mfaSetupTmpl, deps.Auth, deps.MFA, mfaEnrol, deps.Logger))
		dr.Get("/settings/mfa/qr.png", handlers.HandleMFAQR(mfaEnrol, deps.Logger))
		dr.Post("/settings/mfa/enable", handlers.HandleMFAEnable(mfaSetupTmpl, deps.Auth, deps.MFA, mfaEnrol, deps.Logger))
		dr.Post("/settings/mfa/disable", handlers.HandleMFADisable(settingsTmpl, deps.Auth, deps.Sessions, deps.Logger))

		// GDPR: data export (WP-R10-3b: requested here, rendered by the
		// account_export job, downloaded through a presigned URL) + account
		// deletion.
		dr.Post("/settings/export", handlers.HandleExportData(deps.Exports, deps.DB, deps.Logger))
		dr.Get("/settings/export/download", handlers.HandleExportDownload(deps.Exports, deps.Logger))
		dr.Post("/settings/delete-account", handlers.HandleRequestDeletion(deps.DB, deps.Auth, deps.Account, deps.MFA, deps.Logger))
		dr.Post("/settings/cancel-deletion", handlers.HandleCancelDeletion(deps.DB, deps.Account, deps.Logger))

		// Email verification resend.
		dr.Post("/settings/resend-verify", handlers.HandleResendVerification(deps.Auth, deps.Logger, deps.Email, deps.BaseURL))

		// Onboarding dismiss.
		dr.Post("/onboarding/dismiss", handlers.HandleDismissOnboarding(deps.Logger))

		// Billing page.
		billingTmpl := template.Must(baseTmpl.Clone())
		template.Must(billingTmpl.ParseFS(Templates,
			"templates/customer/billing.html",
		))
		// A nil *StripeService must stay a nil interface (the handlers
		// nil-check it), so convert only when configured.
		var billingSvc handlers.BillingService
		if deps.Stripe != nil {
			billingSvc = deps.Stripe
		}
		dr.Get("/billing", handlers.HandleBilling(billingTmpl, billingSvc, deps.DB, deps.Flags, deps.Logger))
		dr.Post("/billing/upgrade", handlers.HandleUpgrade(deps.Stripe, deps.DB, deps.BaseURL, deps.Logger))
		dr.Post("/billing/house", handlers.HandleHouseCheckout(billingSvc, deps.DB, deps.Quotas, deps.Flags, deps.BaseURL, deps.Logger))
		dr.Post("/billing/portal", handlers.HandleManageBilling(deps.Stripe, deps.BaseURL, deps.Logger))

		// Compliance dashboard.
		complianceTmpl := template.Must(baseTmpl.Clone())
		template.Must(complianceTmpl.ParseFS(Templates,
			"templates/customer/compliance.html",
		))
		dr.Get("/compliance", handlers.HandleCompliance(complianceTmpl, deps.DB, deps.Logger))
		dr.Get("/compliance/export", handlers.HandleComplianceExport(deps.DB, deps.Logger))
	})

	// --- Admin (session + admin role required) ---
	adminTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/dashboard.html",
	))
	tenantListTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/tenants.html",
	))
	tenantDetailTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/tenant_detail.html",
	))
	systemTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/system.html",
	))
	backendsTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/backends.html",
	))
	statsTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/stats.html",
	))
	waitlistTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/waitlist.html",
	))
	auditTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/audit.html",
	))
	revenueTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/revenue.html",
	))
	costsTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/costs.html",
	))
	dedupTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/dedup.html",
	))
	supportTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/support.html",
	))
	customerDetailTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/support_detail.html",
	))
	notificationsTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/notifications.html",
	))
	adminAbuseTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/abuse.html",
	))
	abuseDetailTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/abuse_detail.html",
	))
	flagsTmpl := template.Must(template.New("").Funcs(handlers.TemplateFuncs()).ParseFS(Templates,
		"templates/layouts/admin.html",
		"templates/admin/flags.html",
	))

	r.Route("/admin", func(ar chi.Router) {
		ar.Use(middleware.Recovery(deps.Logger))
		ar.Use(middleware.SecurityHeaders)
		ar.Use(dashauth.RequireAdmin(deps.Sessions))
		ar.Use(csrf)
		ar.Use(middleware.RequireAdminMFA(deps.Auth))
		ar.Use(middleware.Flash)
		ar.NotFound(handlers.HandleNotFound(deps.Logger))
		ar.Get("/", handlers.HandleAdminOverview(adminTmpl, deps.DB, deps.Logger))
		ar.Get("/tenants", handlers.HandleTenantList(tenantListTmpl, deps.DB, deps.Logger))
		ar.Get("/tenants/{id}", handlers.HandleTenantDetail(tenantDetailTmpl, deps.DB, deps.Logger, deps.Egress))
		ar.Post("/tenants/{id}/suspend", handlers.HandleSuspendTenant(deps.DB, deps.Logger))
		ar.Post("/tenants/{id}/enable", handlers.HandleEnableTenant(deps.DB, deps.Logger))
		ar.Post("/tenants/{id}/quota", handlers.HandleUpdateQuota(deps.DB, deps.Logger))
		ar.Post("/tenants/{id}/tier", handlers.HandleChangeTier(deps.DB, deps.Logger))
		ar.Post("/tenants/{id}/bandwidth-limit", handlers.HandleUpdateBandwidthLimit(deps.DB, deps.Logger))
		ar.Post("/tenants/{id}/reset-mfa", handlers.HandleAdminResetMFA(deps.Auth, deps.Sessions, deps.Logger))
		ar.Get("/system", handlers.HandleAdminSystem(systemTmpl, deps.DB, deps.Logger))
		ar.Get("/audit", handlers.HandleAdminAudit(auditTmpl, deps.DB, deps.Logger))
		ar.Get("/audit/export", handlers.HandleAdminAuditExport(deps.DB, deps.Logger))
		ar.Get("/waitlist", handlers.HandleAdminWaitlist(waitlistTmpl, deps.DB, deps.Logger))
		ar.Get("/waitlist/export", handlers.HandleAdminWaitlistExport(deps.DB, deps.Logger))
		ar.Get("/stats", handlers.HandleAdminStats(statsTmpl, deps.DB, deps.Logger))
		ar.Get("/revenue", handlers.HandleAdminRevenue(revenueTmpl, deps.DB, deps.Logger))
		ar.Get("/costs", handlers.HandleAdminCosts(costsTmpl, deps.DB, deps.Logger))
		ar.Get("/dedup", handlers.HandleAdminDedup(dedupTmpl, deps.DB, deps.Logger))
		ar.Get("/notifications", handlers.HandleAdminNotifications(notificationsTmpl, deps.DB, deps.Logger))
		ar.Post("/notifications/read-all", handlers.HandleMarkAllRead(deps.DB, deps.Logger))
		ar.Post("/notifications/{id}/read", handlers.HandleMarkRead(deps.DB, deps.Logger))
		ar.Get("/notifications/count", handlers.HandleNotifCount(deps.DB, deps.Logger))
		ar.Get("/abuse", handlers.HandleAdminAbuse(adminAbuseTmpl, deps.DB, deps.Logger))
		ar.Get("/abuse/{id}", handlers.HandleAdminAbuseDetail(abuseDetailTmpl, deps.DB, deps.Logger))
		ar.Post("/abuse/{id}/action", handlers.HandleAbuseAction(deps.DB, deps.Logger))
		ar.Get("/support", handlers.HandleAdminSupport(supportTmpl, deps.DB, deps.Logger))
		ar.Get("/support/{id}", handlers.HandleCustomerDetail(customerDetailTmpl, deps.DB, deps.Logger, deps.Egress))
		ar.Post("/support/{id}/notes", handlers.HandleAddNote(deps.DB, deps.Logger))
		if deps.Engine != nil {
			ar.Get("/backends", handlers.HandleAdminBackends(backendsTmpl, deps.Engine, deps.HealthChecker, deps.Logger))
			ar.Post("/backends/{name}/primary", handlers.HandleSetPrimary(deps.Engine, deps.DB, deps.Logger))
			ar.Post("/backends/{name}/check", handlers.HandleForceHealthCheck(deps.Engine, deps.Logger))
		}
		if deps.Flags != nil {
			ar.Get("/flags", handlers.HandleAdminFlags(flagsTmpl, deps.Flags, deps.Logger))
			ar.Post("/flags/{key}/set", handlers.HandleAdminFlagSet(deps.Flags, deps.Logger))
			ar.Post("/flags/{key}/clear", handlers.HandleAdminFlagClear(deps.Flags, deps.Logger))
		}
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
		// The house built on the landing page arrives in the query string
		// (its receipt links here); show it and carry it through the form.
		q := r.URL.Query()
		if h := landing.ParseHouseIntent(q.Get("std_tb"), q.Get("vault_tb"), q.Get("room")); !h.Empty() {
			data["Intent"] = h
		}
		// Where the visitor came from (checklist item 7): page.js appends the
		// landing URL's utm_* and the referring host (`ref`) to /register
		// links; the Referer header is the fallback. Carried as hidden fields.
		if page == "register" {
			ref := q.Get("ref")
			if ref == "" {
				ref = r.Referer()
			}
			if a := landing.ParseAttribution(ref, q.Get("utm_source"), q.Get("utm_medium"), q.Get("utm_campaign")); !a.Empty() {
				data["Attribution"] = a
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// mfaChallenger adapts MFAPendingStore to handlers.MFAChallenger.
type mfaChallenger struct{ store *MFAPendingStore }

func (m mfaChallenger) Begin(c handlers.MFAChallenge) (string, error) {
	return m.store.Create(MFAPending{UserID: c.UserID, TenantID: c.TenantID, Email: c.Email, Role: c.Role})
}

// staticHandler serves the embedded static tree without directory listings
// (http.FileServer indexed /static/, /static/js/ … — Review R12) and with a
// long immutable cache for versioned URLs: every template references assets
// through {{asset}} (?v=<hash of the tree>), so a changed file is a new URL.
func staticHandler(fsys http.FileSystem) http.Handler {
	files := http.FileServer(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		files.ServeHTTP(w, r)
	})
}

func handleLogin(baseTmpl *template.Template, deps Deps, lockout *middleware.AccountLockout, mfaGate handlers.MFAChallenger) http.HandlerFunc {
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
		// Every outcome is one audit_logs row (checklist item 2); the message
		// to the browser stays the same for every failure (no enumeration).
		fail := func(reason string, user *auth.User) {
			LoginFailures.WithLabelValues(reason).Inc()
			ev := handlers.LoginEvent{Action: "auth.login_failed", Email: email, Via: "password", Reason: reason}
			if user != nil {
				ev.UserID, ev.TenantID = user.ID, user.TenantID
			}
			handlers.RecordLoginEvent(r.Context(), deps.DB, ev)
			if reason != "locked" && lockout != nil && lockout.Fail(email) {
				LoginLockouts.Inc()
				ev.Action, ev.Reason = "auth.login_locked", ""
				handlers.RecordLoginEvent(r.Context(), deps.DB, ev)
				deps.Logger.Warn("dashboard account locked after repeated failures", zap.String("email", email))
			}
			renderErr("Invalid email or password.")
		}

		// The user is looked up first so failures can be attributed; the
		// answer to the browser does not depend on whether it exists.
		user, uErr := deps.Auth.GetUserByEmail(r.Context(), email)
		if uErr != nil {
			user = nil
		}
		if lockout != nil {
			if locked, _ := lockout.Locked(email); locked {
				fail("locked", user)
				return
			}
		}
		valid, err := deps.Auth.ValidatePassword(r.Context(), email, password)
		if err != nil || !valid || user == nil {
			if user == nil {
				fail("unknown_user", nil)
			} else {
				fail("bad_password", user)
			}
			return
		}
		if lockout != nil {
			lockout.Reset(email)
		}

		role := handlers.ResolveRole(r.Context(), deps.DB, user.ID)

		// If MFA is enabled, redirect to the 2FA verification page (the same
		// challenge the OAuth callbacks start — R5-06).
		handled, mErr := handlers.BeginMFAChallenge(w, r, deps.Auth, mfaGate, handlers.MFAChallenge{
			UserID: user.ID, TenantID: user.TenantID, Email: user.Email, Role: role,
		})
		if mErr != nil {
			deps.Logger.Error("create mfa pending", zap.Error(mErr))
			renderErr("Something went wrong. Please try again.")
			return
		}
		if handled {
			handlers.RecordLoginEvent(r.Context(), deps.DB, handlers.LoginEvent{Action: "auth.login_succeeded",
				UserID: user.ID, TenantID: user.TenantID, Email: user.Email, Via: "password", MFA: "pending"})
			return
		}

		token, err := deps.Sessions.Create(r.Context(), dashauth.SessionData{
			UserID:    user.ID,
			TenantID:  user.TenantID,
			Email:     user.Email,
			Role:      role,
			IPAddress: middleware.ClientIP(r),
			UserAgent: dashauth.TruncateUserAgent(r.UserAgent()),
		}, sessionTTL)
		if err != nil {
			deps.Logger.Error("create session", zap.Error(err))
			renderErr("Something went wrong. Please try again.")
			return
		}

		dashauth.SetSessionCookie(w, token, sessionTTL)
		handlers.RecordLoginEvent(r.Context(), deps.DB, handlers.LoginEvent{Action: "auth.login_succeeded",
			UserID: user.ID, TenantID: user.TenantID, Email: user.Email, Via: "password", MFA: "none"})
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	}
}

// signupCredsRenderer builds the reveal-once S3 credentials renderer (B2),
// shared by the web register handler and the OAuth signup callbacks. The
// secret exists only in the rendered response — never persisted, never
// shown again.
func signupCredsRenderer(baseTmpl *template.Template, deps Deps) func(w http.ResponseWriter, accessKey, secret string) {
	credsTmpl := template.Must(baseTmpl.Clone())
	template.Must(credsTmpl.Parse(pageContent("credentials")))

	return func(w http.ResponseWriter, accessKey, secret string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := credsTmpl.ExecuteTemplate(w, "base", map[string]any{
			"Page":      "credentials",
			"AccessKey": accessKey,
			"SecretKey": secret,
			"Endpoint":  deps.BaseURL,
		}); err != nil {
			deps.Logger.Error("render signup credentials", zap.Error(err))
		}
	}
}

func handleRegister(baseTmpl *template.Template, deps Deps) http.HandlerFunc {
	errTmpl := template.Must(baseTmpl.Clone())
	template.Must(errTmpl.Parse(pageContent("register")))
	renderCreds := signupCredsRenderer(baseTmpl, deps)

	return func(w http.ResponseWriter, r *http.Request) {
		email := r.FormValue("email")
		password := r.FormValue("password")
		company := r.FormValue("company")
		intent := landing.ParseHouseIntent(r.FormValue("std_tb"), r.FormValue("vault_tb"), r.FormValue("room"))
		attr := landing.ParseAttribution(r.FormValue("referrer"), r.FormValue("utm_source"), r.FormValue("utm_medium"), r.FormValue("utm_campaign"))

		renderErr := func(msg string) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			data := map[string]any{
				"Error":     msg,
				"Email":     email,
				"Company":   company,
				"Page":      "register",
				"HasGoogle": deps.Google != nil,
				"HasGithub": deps.GitHub != nil,
			}
			if !intent.Empty() {
				data["Intent"] = intent
			}
			if !attr.Empty() {
				data["Attribution"] = attr
			}
			_ = errTmpl.ExecuteTemplate(w, "base", data)
		}

		if len(password) < 8 {
			renderErr("Password must be at least 8 characters.")
			return
		}

		user, _, apiKey, err := deps.Auth.CreateUserWithTenant(r.Context(), email, password, company)
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrSignupsDisabled):
				renderErr("Signups are closed for now — join the waitlist on our homepage.")
			case err.Error() == "user already exists":
				renderErr("An account with that email already exists.")
			default:
				deps.Logger.Error("registration failed", zap.Error(err))
				renderErr("Registration failed. Please try again.")
			}
			return
		}

		// Remember the house on the tenant: the billing page proposes it as
		// the plan. A hint only (nothing is billed from it); failure is logged.
		if !intent.Empty() && deps.DB != nil {
			if _, ierr := deps.DB.ExecContext(r.Context(),
				`UPDATE tenants SET intent_std_tb = $1, intent_vault_tb = $2, intent_room = $3 WHERE id = $4`,
				intent.StdTB, intent.VaultTB, intent.Room, user.TenantID); ierr != nil {
				deps.Logger.Error("store house intent on tenant", zap.String("tenant", user.TenantID), zap.Error(ierr))
			}
		}

		// Sign-up attribution on the user row (checklist item 7). A hint for
		// marketing; failure is logged, never shown.
		if !attr.Empty() && deps.DB != nil {
			if _, aerr := deps.DB.ExecContext(r.Context(),
				`UPDATE users SET signup_referrer = $1, signup_utm_source = $2, signup_utm_medium = $3, signup_utm_campaign = $4 WHERE id = $5`,
				attr.Referrer, attr.UTMSource, attr.UTMMedium, attr.UTMCampaign, user.ID); aerr != nil {
				deps.Logger.Error("store signup attribution", zap.String("user", user.ID), zap.Error(aerr))
			}
		}

		// Create Stripe customer for billing (non-blocking).
		if deps.Stripe != nil {
			if _, stripeErr := deps.Stripe.CreateCustomer(r.Context(), email, user.TenantID); stripeErr != nil {
				deps.Logger.Error("create stripe customer on registration",
					zap.String("tenant", user.TenantID), zap.Error(stripeErr))
			}
		}

		token, err := deps.Sessions.Create(r.Context(), dashauth.SessionData{
			UserID:    user.ID,
			TenantID:  user.TenantID,
			Email:     user.Email,
			Role:      "user",
			IPAddress: middleware.ClientIP(r),
			UserAgent: dashauth.TruncateUserAgent(r.UserAgent()),
		}, sessionTTL)
		if err != nil {
			deps.Logger.Error("create session after register", zap.Error(err))
			renderErr("Account created but login failed. Please sign in.")
			return
		}

		dashauth.SetSessionCookie(w, token, sessionTTL)

		// B2 (5.15.6): render the minted S3 credentials ONCE instead of
		// redirecting. The secret is never shown again anywhere — the old
		// redirect discarded it, leaving web signups with no usable keys.
		renderCreds(w, apiKey.Key, apiKey.Secret)
	}
}

func handleVerifyEmail(baseTmpl *template.Template, deps Deps) http.HandlerFunc {
	tmpl := template.Must(baseTmpl.Clone())
	template.Must(tmpl.Parse(pageContent("verify-result")))

	return func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		data := map[string]any{"Page": "verify-result"}

		if token == "" {
			data["Error"] = "Missing verification token."
		} else if err := deps.Auth.VerifyEmail(r.Context(), token); err != nil {
			deps.Logger.Warn("email verify failed", zap.Error(err))
			data["Error"] = "Invalid or expired verification link."
		} else {
			data["Success"] = "Your email has been verified. You can now use all features."
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tmpl.ExecuteTemplate(w, "base", data)
	}
}

func handleVerify2FA(baseTmpl *template.Template, deps Deps, lockout *middleware.AccountLockout) http.HandlerFunc {
	errTmpl := template.Must(baseTmpl.Clone())
	template.Must(errTmpl.Parse(pageContent("verify-2fa")))

	return func(w http.ResponseWriter, r *http.Request) {
		renderErr := func(msg string) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_ = errTmpl.ExecuteTemplate(w, "base", map[string]any{
				"Error": msg,
				"Page":  "verify-2fa",
			})
		}

		// Read the pending token from the cookie.
		cookie, err := r.Cookie(handlers.MFAPendingCookie)
		if err != nil || deps.MFAPending == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		// Peek first to keep the token alive for retries.
		pending := deps.MFAPending.Peek(cookie.Value)
		if pending == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		// An account locked meanwhile (by password guesses on the other
		// path) does not get to finish a challenge that predates the lock.
		if lockout != nil {
			if locked, _ := lockout.Locked(pending.Email); locked {
				deps.MFAPending.Get(cookie.Value)
				handlers.ClearMFAPendingCookie(w)
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
		}

		code := r.FormValue("totp_code")
		if code == "" {
			renderErr("Please enter your authentication code.")
			return
		}

		fail := func(reason string) {
			LoginFailures.WithLabelValues(reason).Inc()
			handlers.RecordLoginEvent(r.Context(), deps.DB, handlers.LoginEvent{Action: "auth.mfa_failed",
				UserID: pending.UserID, TenantID: pending.TenantID, Email: pending.Email, Reason: reason})
			if lockout != nil && lockout.Fail(pending.Email) {
				// Guessing the second factor counts like guessing the first:
				// the challenge is consumed and the account is locked.
				LoginLockouts.Inc()
				handlers.RecordLoginEvent(r.Context(), deps.DB, handlers.LoginEvent{Action: "auth.login_locked",
					UserID: pending.UserID, TenantID: pending.TenantID, Email: pending.Email})
				deps.MFAPending.Get(cookie.Value)
				handlers.ClearMFAPendingCookie(w)
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			renderErr("Invalid code. Please try again.")
		}

		// Try TOTP code first.
		secret, sErr := deps.Auth.GetMFASecret(r.Context(), pending.UserID)
		if sErr != nil {
			renderErr("2FA configuration error. Please contact support.")
			return
		}

		via := "totp"
		valid := deps.MFA.ValidateCode(secret, code)
		if valid && !deps.Auth.ConsumeTOTPCode(pending.UserID, code) {
			// RFC 6238 §5.2: a code is accepted once (R5-15c, proven live).
			fail("replayed_code")
			return
		}

		// If TOTP fails, try as backup code.
		if !valid {
			if ok, _ := deps.Auth.ValidateBackupCode(r.Context(), pending.UserID, code); ok {
				valid = true
				via = "backup_code"
			}
		}

		if !valid {
			fail("bad_code")
			return
		}

		// Consume the pending token and clear the cookie.
		deps.MFAPending.Get(cookie.Value)
		handlers.ClearMFAPendingCookie(w)
		if lockout != nil {
			lockout.Reset(pending.Email)
		}

		// Create the real session. The MFA pending token is single-use
		// (consumed above) so this is the first real session for this
		// login — i.e., 2FA verification implicitly rotates the token.
		token, cErr := deps.Sessions.Create(r.Context(), dashauth.SessionData{
			UserID:    pending.UserID,
			TenantID:  pending.TenantID,
			Email:     pending.Email,
			Role:      pending.Role,
			IPAddress: middleware.ClientIP(r),
			UserAgent: dashauth.TruncateUserAgent(r.UserAgent()),
		}, sessionTTL)
		if cErr != nil {
			deps.Logger.Error("create session after 2fa", zap.Error(cErr))
			renderErr("Something went wrong. Please try again.")
			return
		}

		dashauth.SetSessionCookie(w, token, sessionTTL)
		handlers.RecordLoginEvent(r.Context(), deps.DB, handlers.LoginEvent{Action: "auth.mfa_succeeded",
			UserID: pending.UserID, TenantID: pending.TenantID, Email: pending.Email, Via: via})
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

// handleForgotPassword handles POST /forgot-password. It always returns the
// same success message regardless of whether the email exists, to prevent
// user enumeration.
func handleForgotPassword(baseTmpl *template.Template, deps Deps) http.HandlerFunc {
	tmpl := template.Must(baseTmpl.Clone())
	template.Must(tmpl.Parse(pageContent("forgot-password")))

	return func(w http.ResponseWriter, r *http.Request) {
		addr := r.FormValue("email")

		const successMsg = "If that email is registered, you'll receive a reset link shortly."

		token, err := deps.Auth.RequestPasswordReset(r.Context(), addr)
		if err == nil {
			htmlBody, textBody, renderErr := email.RenderPasswordReset(deps.BaseURL, token, addr)
			if renderErr != nil {
				deps.Logger.Error("render password reset email", zap.Error(renderErr))
			} else if sendErr := deps.Email.Send(r.Context(), addr, "Reset your password — Stored", htmlBody, textBody); sendErr != nil {
				deps.Logger.Error("send password reset email", zap.String("to", addr), zap.Error(sendErr))
			}
		} else if !errors.Is(err, auth.ErrResetRateLimited) {
			deps.Logger.Debug("password reset requested for unknown email",
				zap.String("email", addr), zap.Error(err))
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tmpl.ExecuteTemplate(w, "base", map[string]any{
			"Page":    "forgot-password",
			"Success": successMsg,
		})
	}
}

// handleResetPasswordForm handles GET /reset-password?token=... and renders
// the new-password form. The token is passed through as a hidden field on
// the POST.
func handleResetPasswordForm(baseTmpl *template.Template, _ Deps) http.HandlerFunc {
	tmpl := template.Must(baseTmpl.Clone())
	template.Must(tmpl.Parse(pageContent("reset-password")))

	return func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		data := map[string]any{
			"Page":  "reset-password",
			"Token": token,
		}
		if token == "" {
			data["Error"] = "Missing reset token."
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tmpl.ExecuteTemplate(w, "base", data)
	}
}

// handleResetPassword handles POST /reset-password. It validates the token
// and the new password fields, updates the password, and invalidates all
// existing sessions for the user (forcing re-login on every device).
func handleResetPassword(baseTmpl *template.Template, deps Deps) http.HandlerFunc {
	tmpl := template.Must(baseTmpl.Clone())
	template.Must(tmpl.Parse(pageContent("reset-password")))

	return func(w http.ResponseWriter, r *http.Request) {
		token := r.FormValue("token")
		newPass := r.FormValue("new_password")
		confirm := r.FormValue("confirm_password")

		renderErr := func(msg string) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_ = tmpl.ExecuteTemplate(w, "base", map[string]any{
				"Page":  "reset-password",
				"Token": token,
				"Error": msg,
			})
		}

		if token == "" {
			renderErr("Missing reset token.")
			return
		}
		if len(newPass) < 8 {
			renderErr("Password must be at least 8 characters.")
			return
		}
		if newPass != confirm {
			renderErr("Passwords do not match.")
			return
		}

		userID, err := deps.Auth.CompletePasswordReset(r.Context(), token, newPass)
		if err != nil {
			deps.Logger.Warn("password reset failed", zap.Error(err))
			renderErr("Invalid or expired reset link. Please request a new one.")
			return
		}

		// Invalidate all existing sessions for this user (force re-login).
		if deps.Sessions != nil {
			if dErr := deps.Sessions.DeleteByUserID(r.Context(), userID); dErr != nil {
				deps.Logger.Error("invalidate sessions on password reset",
					zap.String("user", userID), zap.Error(dErr))
			}
		}

		middleware.SetFlash(w, "success", "Password reset successful. Please sign in with your new password.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

// pageContent returns a small template snippet that defines the "content"
// block for each page. Phase 1 replaces these with real template files.
func pageContent(page string) string {
	switch page {
	case "login":
		return `{{define "title"}}Sign In — Stored{{end}}` +
			`{{define "nav"}}{{end}}` +
			`{{define "content"}}` +
			`<div class="auth-page"><div class="auth-card">` +
			`<div class="auth-brand">Stored</div>` +
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
		return `{{define "title"}}Register — Stored{{end}}` +
			`{{define "nav"}}{{end}}` +
			`{{define "content"}}` +
			`<div class="auth-page"><div class="auth-card">` +
			`<div class="auth-brand">Stored</div>` +
			`<h1>Create Account</h1>` +
			`{{if .Intent}}<div class="alert alert-info house-intent"><strong>Your house:</strong> {{.Intent.StdTB}} TB downstairs, {{.Intent.VaultTB}} TB in the attic &middot; <strong>{{.Intent.Monthly}}/mo</strong> billed yearly. It will be waiting on your billing page.</div>{{end}}` +
			`{{if .Error}}<div class="alert alert-error">{{.Error}}</div>{{end}}` +
			`{{if or .HasGoogle .HasGithub}}` +
			`<div class="oauth-buttons">` +
			`{{if .HasGoogle}}<a href="/auth/google" class="btn btn-oauth btn-google">Sign up with Google</a>{{end}}` +
			`{{if .HasGithub}}<a href="/auth/github" class="btn btn-oauth btn-github">Sign up with GitHub</a>{{end}}` +
			`</div>` +
			`<div class="auth-divider"><span>or</span></div>` +
			`{{end}}` +
			`<form method="POST" action="/register">` +
			`{{if .Intent}}<input type="hidden" name="std_tb" value="{{.Intent.StdTB}}"><input type="hidden" name="vault_tb" value="{{.Intent.VaultTB}}"><input type="hidden" name="room" value="{{.Intent.Room}}">{{end}}` +
			`{{if .Attribution}}<input type="hidden" name="referrer" value="{{.Attribution.Referrer}}"><input type="hidden" name="utm_source" value="{{.Attribution.UTMSource}}"><input type="hidden" name="utm_medium" value="{{.Attribution.UTMMedium}}"><input type="hidden" name="utm_campaign" value="{{.Attribution.UTMCampaign}}">{{end}}` +
			`<div class="form-group"><label>Email</label><input type="email" name="email" value="{{.Email}}" required></div>` +
			`<div class="form-group"><label>Password</label><input type="password" name="password" required minlength="8"></div>` +
			`<div class="form-group"><label>Company</label><input type="text" name="company" value="{{.Company}}"></div>` +
			`<button type="submit" class="btn btn-primary btn-block">Create Account</button>` +
			`</form>` +
			`<div class="auth-footer">Have an account? <a href="/login">Sign in</a></div>` +
			`</div></div>{{end}}`
	case "credentials":
		// B2: post-signup reveal-once S3 credentials. The secret exists only
		// in this response — refreshing or navigating away loses it for good
		// (a replacement key can be minted at /dashboard/apikeys).
		return `{{define "title"}}Your S3 Credentials — Stored{{end}}` +
			`{{define "nav"}}{{end}}` +
			`{{define "content"}}` +
			`<div class="auth-page"><div class="auth-card">` +
			`<div class="auth-brand">Stored</div>` +
			`<h1>Your S3 Credentials</h1>` +
			`<div class="alert alert-error"><strong>Save these now.</strong> ` +
			`Your secret key is shown only once — it cannot be recovered. ` +
			`If you lose it, generate a new key from the dashboard.</div>` +
			`<div class="form-group"><label>Access Key ID</label>` +
			`<input type="text" id="cred-access" value="{{.AccessKey}}" readonly onclick="this.select()">` +
			`<button type="button" class="btn btn-block" onclick="navigator.clipboard.writeText(document.getElementById('cred-access').value);this.textContent='Copied ✓'">Copy access key</button></div>` +
			`<div class="form-group"><label>Secret Access Key</label>` +
			`<input type="text" id="cred-secret" value="{{.SecretKey}}" readonly onclick="this.select()">` +
			`<button type="button" class="btn btn-block" onclick="navigator.clipboard.writeText(document.getElementById('cred-secret').value);this.textContent='Copied ✓'">Copy secret key</button></div>` +
			`<div class="form-group"><label>Endpoint</label>` +
			`<input type="text" id="cred-endpoint" value="{{.Endpoint}}" readonly onclick="this.select()"></div>` +
			`<a href="/dashboard" class="btn btn-primary btn-block">I saved my keys — go to dashboard</a>` +
			`</div></div>{{end}}`
	case "verify-2fa":
		return `{{define "title"}}Verify 2FA — Stored{{end}}` +
			`{{define "nav"}}{{end}}` +
			`{{define "content"}}` +
			`<div class="auth-page"><div class="auth-card">` +
			`<div class="auth-brand">Stored</div>` +
			`<h1>Two-Factor Authentication</h1>` +
			`<p class="auth-subtitle">Enter the 6-digit code from your authenticator app, or a backup code.</p>` +
			`{{if .Error}}<div class="alert alert-error">{{.Error}}</div>{{end}}` +
			`<form method="POST" action="/login/verify-2fa">` +
			`<div class="form-group"><label>Authentication Code</label>` +
			`<input type="text" name="totp_code" placeholder="000000" maxlength="8" autocomplete="one-time-code" inputmode="numeric" autofocus required></div>` +
			`<button type="submit" class="btn btn-primary btn-block">Verify</button>` +
			`</form>` +
			`<div class="auth-footer"><a href="/login">Back to sign in</a></div>` +
			`</div></div>{{end}}`
	case "verify-result":
		return `{{define "title"}}Email Verification — Stored{{end}}` +
			`{{define "nav"}}{{end}}` +
			`{{define "content"}}` +
			`<div class="auth-page"><div class="auth-card">` +
			`<div class="auth-brand">Stored</div>` +
			`<h1>Email Verification</h1>` +
			`{{if .Success}}<div class="alert alert-success">{{.Success}}</div>` +
			`<div class="auth-footer"><a href="/login">Sign in</a></div>` +
			`{{else if .Error}}<div class="alert alert-error">{{.Error}}</div>` +
			`<div class="auth-footer"><a href="/login">Back to sign in</a></div>` +
			`{{end}}` +
			`</div></div>{{end}}`
	case "forgot-password":
		return `{{define "title"}}Forgot Password — Stored{{end}}` +
			`{{define "nav"}}{{end}}` +
			`{{define "content"}}` +
			`<div class="auth-page"><div class="auth-card">` +
			`<div class="auth-brand">Stored</div>` +
			`<h1>Reset Password</h1>` +
			`<p class="auth-subtitle">Enter your email and we'll send you a link to reset your password.</p>` +
			`{{if .Error}}<div class="alert alert-error">{{.Error}}</div>{{end}}` +
			`{{if .Success}}<div class="alert alert-success">{{.Success}}</div>{{end}}` +
			`<form method="POST" action="/forgot-password">` +
			`<div class="form-group"><label>Email</label><input type="email" name="email" value="{{.Email}}" required autofocus></div>` +
			`<button type="submit" class="btn btn-primary btn-block">Send Reset Link</button>` +
			`</form>` +
			`<div class="auth-footer"><a href="/login">Back to sign in</a></div>` +
			`</div></div>{{end}}`
	case "reset-password":
		return `{{define "title"}}Reset Password — Stored{{end}}` +
			`{{define "nav"}}{{end}}` +
			`{{define "content"}}` +
			`<div class="auth-page"><div class="auth-card">` +
			`<div class="auth-brand">Stored</div>` +
			`<h1>Choose a New Password</h1>` +
			`{{if .Error}}<div class="alert alert-error">{{.Error}}</div>{{end}}` +
			`<form method="POST" action="/reset-password">` +
			`<input type="hidden" name="token" value="{{.Token}}">` +
			`<div class="form-group"><label>New Password</label><input type="password" name="new_password" required minlength="8" autofocus></div>` +
			`<div class="form-group"><label>Confirm New Password</label><input type="password" name="confirm_password" required minlength="8"></div>` +
			`<button type="submit" class="btn btn-primary btn-block">Reset Password</button>` +
			`</form>` +
			`<div class="auth-footer"><a href="/login">Back to sign in</a></div>` +
			`</div></div>{{end}}`
	default:
		return `{{define "content"}}<p>Page not found.</p>{{end}}`
	}
}

// noSystemBucket answers 404 for a bucket page of a system bucket (its name
// starts with '_' — tenant.IsSystemBucket): the export bucket is the
// service's, hidden from the bucket list and never a page.
func noSystemBucket(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tenant.IsSystemBucket(chi.URLParam(r, "name")) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
