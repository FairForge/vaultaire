package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/billing"
	"github.com/FairForge/vaultaire/internal/compliance"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/dashboard"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/docs"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/rbac"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
)

// OAuth provider endpoints.
var (
	googleEndpoint = oauth2.Endpoint{
		AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL: "https://oauth2.googleapis.com/token",
	}
	githubEndpoint = oauth2.Endpoint{
		AuthURL:  "https://github.com/login/oauth/authorize",
		TokenURL: "https://github.com/login/oauth/access_token",
	}
)

type Server struct {
	config           *config.Config
	logger           *zap.Logger
	router           chi.Router
	httpServer       *http.Server
	db               *sql.DB
	events           chan Event
	engine           *engine.CoreEngine
	quotaManager     QuotaManager
	rbacService      *RBACService
	auth             *auth.AuthService
	auditLogger      *auth.AuditLogger
	stripe           *billing.StripeService
	webhookHandler   *billing.WebhookHandler
	requestCount     int64
	testMode         bool
	errorCount       int64
	healthChecker    *BackendHealthChecker
	sessionStore     dashauth.SessionStore
	bandwidthTracker *BandwidthTracker
	googleOAuth      *oauth2.Config
	githubOAuth      *oauth2.Config
	mfaService       *auth.MFAService
	mfaPendingStore  *dashboard.MFAPendingStore
	cdnRateLimiter   *RateLimiter
	startTime        time.Time
}

type QuotaManager interface {
	GetUsage(ctx context.Context, tenantID string) (used, limit int64, err error)
	CheckAndReserve(ctx context.Context, tenantID string, bytes int64) (bool, error)
	CreateTenant(ctx context.Context, tenantID, plan string, storageLimit int64) error
	UpdateQuota(ctx context.Context, tenantID string, newLimit int64) error
	ListQuotas(ctx context.Context) ([]map[string]interface{}, error)
	DeleteQuota(ctx context.Context, tenantID string) error
	GetTier(ctx context.Context, tenantID string) (string, error)
	UpdateTier(ctx context.Context, tenantID, newTier string) error
	GetUsageHistory(ctx context.Context, tenantID string, days int) ([]map[string]interface{}, error)
}

func NewServer(cfg *config.Config, logger *zap.Logger, eng *engine.CoreEngine, qm QuotaManager, db *sql.DB) *Server {
	s := &Server{
		config:       cfg,
		logger:       logger,
		engine:       eng,
		quotaManager: qm,
		db:           db,
		router:       chi.NewRouter(),
		events:       make(chan Event, 1000),
		startTime:    time.Now(),
	}
	s.healthChecker = NewBackendHealthChecker()

	// Register the Quotaless backend so /health reports meaningful data.
	// Without this the backends map is empty and status is always "unknown".
	s.healthChecker.RegisterBackend("quotaless")

	// Pass s.db so registrations are persisted to PostgreSQL.
	// Previously NewAuthService(nil) left sqlDB nil, so CreateUserWithTenant
	// wrote only to in-memory maps and credentials vanished on restart.
	s.auth = auth.NewAuthService(nil, s.db)

	// Use JWT_SECRET from environment if available.
	s.auth.SetJWTSecret(os.Getenv("JWT_SECRET"))

	// Email verification HMAC secret — reuses JWT_SECRET if no separate key is set.
	verifySecret := os.Getenv("VERIFY_SECRET")
	if verifySecret == "" {
		verifySecret = os.Getenv("JWT_SECRET")
	}
	s.auth.SetVerifySecret(verifySecret)

	// Populate in-memory maps from PostgreSQL so that existing users
	// can authenticate immediately after a restart/deploy.
	if s.db != nil {
		if err := s.auth.LoadFromDB(context.Background()); err != nil {
			logger.Error("failed to load auth state from DB — existing users cannot log in",
				zap.Error(err))
		} else {
			logger.Info("auth state loaded from database")
		}
	}

	// Load MFA state from DB.
	if s.db != nil {
		if err := s.auth.LoadMFAFromDB(context.Background()); err != nil {
			logger.Error("failed to load MFA state from DB", zap.Error(err))
		}
	}

	// Backfill bucket registry and tenant slugs on startup.
	if s.db != nil {
		if err := auth.BackfillBuckets(context.Background(), s.db, logger); err != nil {
			logger.Error("failed to backfill buckets", zap.Error(err))
		}
		if err := auth.BackfillSlugs(context.Background(), s.db, logger); err != nil {
			logger.Error("failed to backfill slugs", zap.Error(err))
		}
	}

	s.cdnRateLimiter = NewRateLimiter()

	// MFA service for TOTP generation and validation.
	s.mfaService = auth.NewMFAService("stored.ge")
	s.mfaPendingStore = dashboard.NewMFAPendingStore()

	s.auditLogger = auth.NewAuditLogger()
	s.auth.SetAuditLogger(s.auditLogger)

	// Session store for the dashboard web UI.
	// Uses PostgreSQL when available, in-memory otherwise.
	if s.db != nil {
		s.sessionStore = dashauth.NewDBStore(s.db)
	} else {
		s.sessionStore = dashauth.NewMemoryStore()
	}

	// Bandwidth tracker — buffers S3 bandwidth events and flushes to DB.
	s.bandwidthTracker = NewBandwidthTracker(s.db)
	s.bandwidthTracker.SetLogger(logger)
	s.bandwidthTracker.StartFlusher(context.Background(), 5*time.Second)

	// Stripe billing service. Only active when STRIPE_SECRET_KEY is set.
	if stripeKey := os.Getenv("STRIPE_SECRET_KEY"); stripeKey != "" {
		s.stripe = billing.NewStripeService(stripeKey, s.db, logger)
		whSecret := os.Getenv("STRIPE_WEBHOOK_SECRET")
		s.webhookHandler = billing.NewWebhookHandler(whSecret, s.stripe, logger)

		// Register plans. Price IDs come from environment (set in Stripe Dashboard).
		// If not set, plans won't appear on the billing page but nothing breaks.
		registerStripePlans(s.stripe)

		logger.Info("stripe billing service initialized")
	}

	// OAuth providers. Only active when client ID+secret are set.
	baseURL := os.Getenv("VAULTAIRE_BASE_URL")
	if baseURL == "" {
		baseURL = fmt.Sprintf("http://localhost:%d", cfg.Server.Port)
	}
	if googleID := os.Getenv("GOOGLE_CLIENT_ID"); googleID != "" {
		s.googleOAuth = &oauth2.Config{
			ClientID:     googleID,
			ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
			RedirectURL:  baseURL + "/auth/google/callback",
			Scopes:       []string{"openid", "email", "profile"},
			Endpoint:     googleEndpoint,
		}
		logger.Info("google oauth initialized")
	}
	if githubID := os.Getenv("GITHUB_CLIENT_ID"); githubID != "" {
		s.githubOAuth = &oauth2.Config{
			ClientID:     githubID,
			ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
			RedirectURL:  baseURL + "/auth/github/callback",
			Scopes:       []string{"user:email"},
			Endpoint:     githubEndpoint,
		}
		logger.Info("github oauth initialized")
	}

	s.rbacService = NewRBACService(logger)

	s.router.Use(s.requestIDMiddleware)
	s.router.Use(s.rbacService.InjectUserContext)
	s.router.Use(s.loggingMiddleware)

	s.setupRoutes()

	// CDN host-based router: cdn.stored.ge → CDN handler, everything else → normal router.
	cdnRouter := chi.NewRouter()
	cdnRouter.Get("/{slug}/{bucket}/*", s.handleCDNRequest)
	cdnRouter.Head("/{slug}/{bucket}/*", s.handleCDNRequest)
	cdnRouter.Options("/{slug}/{bucket}/*", s.handleCDNRequest)

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      CDNHostRouter(cdnRouter, s.router),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	return s
}

// backendCheck holds the config for a single backend health probe.
// TCP dial is used rather than HTTP because S3 backends have inconsistent
// behaviour on unauthenticated HTTP requests (EOF, 403, timeout, etc.).
// A successful TCP connection on the backend port is sufficient to confirm
// reachability — the same check works for any backend regardless of vendor.
type backendCheck struct {
	name    string // key in healthChecker, e.g. "quotaless"
	address string // host:port, e.g. "io.quotaless.cloud:8000"
}

// endpointToAddress parses an endpoint URL and returns host:port.
// If no port is present in the URL, the default for the scheme is used
// (443 for https, 80 for http).
func endpointToAddress(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse endpoint %q: %w", endpoint, err)
	}

	host := u.Hostname()
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(host, port), nil
}

// startHealthChecks runs background goroutines that TCP-dial each backend
// every 30 seconds and update the health checker accordingly.
// Goroutines stop when ctx is cancelled (i.e. on server shutdown).
//
// Adding a new backend: register it in NewServer with
// s.healthChecker.RegisterBackend("name"), then add a backendCheck entry
// here pointing at its endpoint env var.
func (s *Server) startHealthChecks(ctx context.Context) {
	quotalessEndpoint := os.Getenv("QUOTALESS_ENDPOINT")
	if quotalessEndpoint == "" {
		quotalessEndpoint = "https://io.quotaless.cloud:8000"
	}

	quotalessAddr, err := endpointToAddress(quotalessEndpoint)
	if err != nil {
		s.logger.Error("invalid QUOTALESS_ENDPOINT — health checks disabled",
			zap.Error(err))
		return
	}

	backends := []backendCheck{
		{name: "quotaless", address: quotalessAddr},
		// Add future backends here, e.g.:
		// {name: "lyve", address: "s3.us-east-1.lyvecloud.seagate.com:443"},
		// {name: "geyser", address: "s3.geyserdata.com:443"},
	}

	for _, b := range backends {
		b := b // capture for goroutine
		go s.runBackendHealthLoop(ctx, b)
	}
}

// runBackendHealthLoop probes a single backend on a 30-second ticker until
// ctx is cancelled.
func (s *Server) runBackendHealthLoop(ctx context.Context, b backendCheck) {
	check := func() {
		start := time.Now()

		// TCP dial confirms the host is reachable on the expected port.
		// This is backend-agnostic: works for Quotaless, Lyve Cloud, Geyser,
		// or any future S3-compatible provider without any HTTP-level quirks.
		conn, err := net.DialTimeout("tcp", b.address, 3*time.Second)
		latency := time.Since(start)

		if err != nil {
			s.logger.Warn("backend health check failed",
				zap.String("backend", b.name),
				zap.String("address", b.address),
				zap.Error(err),
				zap.Duration("latency", latency))
			s.healthChecker.UpdateHealth(b.name, false, latency, err)
			return
		}
		_ = conn.Close()

		s.healthChecker.UpdateHealth(b.name, true, latency, nil)
		s.logger.Debug("backend health check: TCP OK",
			zap.String("backend", b.name),
			zap.Duration("latency", latency))
	}

	// Run immediately so /health is accurate from the first request.
	check()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

func (s *Server) setupRoutes() {
	s.router.Get("/health", s.handleHealthEnhanced)
	s.router.Get("/health/live", s.handleLiveness)
	s.router.Get("/health/ready", s.handleReadiness)
	s.router.Get("/health/backends", s.handleBackendsHealth)
	s.router.Get("/ready", s.handleReadiness)
	s.router.Get("/metrics", s.handleMetrics)
	s.router.Get("/version", s.handleVersion)

	s.logger.Info("Registering auth routes")
	s.router.Post("/auth/register", s.handleRegister)
	s.router.Post("/auth/login", s.handleLogin)
	s.router.Post("/auth/password-reset", s.handlePasswordReset)
	s.router.Post("/auth/password-reset/complete", s.handlePasswordResetComplete)

	s.router.Get("/docs", docs.SwaggerUIHandler())
	s.router.Get("/openapi.json", docs.OpenAPIJSONHandler())

	s.logger.Info("Registering user API routes")
	s.registerUserAPIRoutes()

	s.logger.Info("Registering quota routes")
	s.registerQuotaRoutes()

	s.logger.Info("Registering compliance routes")
	s.registerComplianceRoutes()

	s.router.With(s.rbacService.RequirePermission("quota.read")).
		Get("/api/v1/usage/stats", s.handleGetUsageStats)
	s.router.With(s.rbacService.RequirePermission("quota.read")).
		Get("/api/v1/usage/alerts", s.handleGetUsageAlerts)
	s.router.With(s.rbacService.RequirePermission("storage.read")).
		Get("/api/v1/presigned", s.handleGetPresignedURL)

	s.setupQuotaManagementRoutes()
	s.setupPatternRoutes()

	handlers := rbac.NewRBACHandlers(s.rbacService.manager, s.rbacService.auditor)
	s.router.Route("/api/rbac", func(r chi.Router) {
		r.Get("/roles", handlers.HandleGetRoles)
		r.Get("/users/{userID}/roles", handlers.HandleGetUserRoles)
		r.With(s.rbacService.RequireRole(rbac.RoleAdmin)).
			Post("/users/{userID}/roles", handlers.HandleAssignRole)
		r.With(s.rbacService.RequireRole(rbac.RoleAdmin)).
			Delete("/users/{userID}/roles", handlers.HandleRevokeRole)
		r.Get("/permissions", handlers.HandleGetPermissions)
		r.Get("/audit", handlers.HandleGetAuditLogs)
	})

	// Stripe webhook endpoint. No auth middleware — Stripe verifies via signature.
	if s.webhookHandler != nil {
		s.logger.Info("Registering Stripe webhook route")
		s.router.Post("/webhook/stripe", s.webhookHandler.ServeHTTP)
	}

	// Public status page — no auth, renders HTML.
	s.router.Get("/status", s.handleStatusPage)

	// CDN path-based routes — before dashboard and S3 catch-all.
	s.logger.Info("Registering CDN routes")
	s.router.Get("/cdn/{slug}/{bucket}/*", s.handleCDNRequest)
	s.router.Head("/cdn/{slug}/{bucket}/*", s.handleCDNRequest)
	s.router.Options("/cdn/{slug}/{bucket}/*", s.handleCDNRequest)

	// Dashboard routes must be registered BEFORE the S3 catch-all so that
	// /login, /dashboard/*, /admin/*, and /static/* are matched first.
	s.logger.Info("Registering dashboard routes")
	dataPath := os.Getenv("DATA_PATH")
	if dataPath == "" {
		dataPath = "/tmp/vaultaire-data"
	}
	dashboard.RegisterRoutes(s.router, dashboard.Deps{
		DB:         s.db,
		Auth:       s.auth,
		MFA:        s.mfaService,
		MFAPending: s.mfaPendingStore,
		Sessions:   s.sessionStore,
		Logger:     s.logger,
		DataPath:   dataPath,
		Stripe:     s.stripe,
		Google:     s.googleOAuth,
		GitHub:     s.githubOAuth,
	})

	s.logger.Info("Registering management API routes")
	s.registerManagementRoutes()

	s.router.Get("/llms.txt", s.handleLlmsTxt)

	s.logger.Info("Registering S3 catch-all handler")
	s.router.HandleFunc("/*", s.handleS3Request)
}

func (s *Server) registerComplianceRoutes() {
	complianceHandler := compliance.NewAPIHandler(
		compliance.NewGDPRService(nil, s.logger),
		compliance.NewPortabilityService(nil, nil, s.logger),
		compliance.NewConsentService(nil, s.logger),
		compliance.NewBreachService(nil, s.logger),
		compliance.NewROPAService(nil, s.logger),
		compliance.NewPrivacyService(nil),
		s.logger,
	)

	s.router.Route("/api/compliance", func(r chi.Router) {
		r.Use(s.requireJWT)

		r.Post("/sar", complianceHandler.HandleCreateSAR)
		r.Get("/sar/{id}", complianceHandler.HandleGetSARStatus)
		r.Get("/inventory", complianceHandler.HandleGetDataInventory)
		r.Get("/activities", complianceHandler.HandleListProcessingActivities)

		r.Post("/deletion", complianceHandler.HandleCreateDeletionRequest)

		r.Post("/export", complianceHandler.HandleCreateExport)
		r.Get("/export/{id}", complianceHandler.HandleGetExport)

		r.Post("/consent", complianceHandler.HandleGrantConsent)
		r.Delete("/consent/{purpose}", complianceHandler.HandleWithdrawConsent)
		r.Get("/consent", complianceHandler.HandleGetConsentStatus)
		r.Get("/consent/{purpose}", complianceHandler.HandleCheckConsent)
		r.Get("/consent/history", complianceHandler.HandleGetConsentHistory)
		r.Get("/consent/purposes", complianceHandler.HandleListConsentPurposes)

		r.Post("/breach", complianceHandler.HandleReportBreach)
		r.Get("/breach/{id}", complianceHandler.HandleGetBreach)
		r.Get("/breach", complianceHandler.HandleListBreaches)
		r.Patch("/breach/{id}", complianceHandler.HandleUpdateBreach)
		r.Post("/breach/{id}/notify", complianceHandler.HandleNotifyBreach)
		r.Get("/breach/stats", complianceHandler.HandleGetBreachStats)

		r.Post("/ropa/activities", complianceHandler.HandleCreateActivity)
		r.Get("/ropa/activities/{id}", complianceHandler.HandleGetActivity)
		r.Get("/ropa/activities", complianceHandler.HandleListActivities)
		r.Patch("/ropa/activities/{id}", complianceHandler.HandleUpdateActivity)
		r.Delete("/ropa/activities/{id}", complianceHandler.HandleDeleteActivity)
		r.Post("/ropa/activities/{id}/review", complianceHandler.HandleReviewActivity)
		r.Get("/ropa/report", complianceHandler.HandleGetROPAReport)
		r.Get("/ropa/compliance/{id}", complianceHandler.HandleCheckCompliance)
		r.Get("/ropa/stats", complianceHandler.HandleGetROPAStats)

		r.Post("/privacy/controls", complianceHandler.HandleEnablePrivacyControl)
		r.Post("/privacy/minimize", complianceHandler.HandleMinimizeData)
		r.Get("/privacy/purpose/{dataId}/{purpose}", complianceHandler.HandleCheckPurpose)
		r.Post("/privacy/pseudonymize", complianceHandler.HandlePseudonymize)
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Company  string `json:"company"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	user, tenant, apiKey, err := s.auth.CreateUserWithTenant(
		r.Context(), req.Email, req.Password, req.Company)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Create Stripe customer for billing (non-blocking — registration
	// succeeds even if Stripe is unavailable).
	if s.stripe != nil {
		if _, stripeErr := s.stripe.CreateCustomer(r.Context(), req.Email, tenant.ID); stripeErr != nil {
			s.logger.Error("create stripe customer on registration",
				zap.String("tenant", tenant.ID), zap.Error(stripeErr))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{
		"accessKeyId":     apiKey.Key,
		"secretAccessKey": apiKey.Secret,
		"endpoint":        getPublicEndpoint(s.config.Server.Port),
	}); err != nil {
		s.logger.Error("failed to encode register response", zap.Error(err))
	}

	s.logger.Info("user registered",
		zap.String("email", req.Email),
		zap.String("user_id", user.ID),
		zap.String("tenant_id", tenant.ID))
}

// registerStripePlans loads plan definitions from environment variables.
// Each plan needs a STRIPE_PRICE_<ID> env var with the Stripe Price ID.
// If the env var is empty, the plan is skipped (won't appear on billing page).
func registerStripePlans(s *billing.StripeService) {
	plans := []struct {
		id, envVar, name, priceFmt string
		storageTB                  int64
	}{
		{"vault3", "STRIPE_PRICE_VAULT3", "Vault 3TB", "$2.99/mo", 3},
		{"vault9", "STRIPE_PRICE_VAULT9", "Vault 9TB", "$9/mo", 9},
		{"vault18", "STRIPE_PRICE_VAULT18", "Vault 18TB", "$18/mo", 18},
		{"vault36", "STRIPE_PRICE_VAULT36", "Vault 36TB", "$36/mo", 36},
		{"standard", "STRIPE_PRICE_STANDARD", "Standard", "$3.99/TB/mo", 0},
	}
	for _, p := range plans {
		priceID := os.Getenv(p.envVar)
		if priceID == "" {
			continue
		}
		s.RegisterPlan(billing.Plan{
			ID:        p.id,
			PriceID:   priceID,
			Name:      p.name,
			PriceFmt:  p.priceFmt,
			StorageTB: p.storageTB,
		})
	}
}

// getPublicEndpoint returns the public S3 endpoint from the VAULTAIRE_ENDPOINT
// environment variable. This is set in /opt/vaultaire/configs/.env on the
// production server. Falls back to localhost for local development.
func getPublicEndpoint(port int) string {
	if ep := os.Getenv("VAULTAIRE_ENDPOINT"); ep != "" {
		return ep
	}
	return fmt.Sprintf("http://localhost:%d", port)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	valid, err := s.auth.ValidatePassword(r.Context(), req.Email, req.Password)
	if err != nil || !valid {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	user, err := s.auth.GetUserByEmail(r.Context(), req.Email)
	if err != nil {
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}

	token, err := s.auth.GenerateJWT(user)
	if err != nil {
		http.Error(w, "Failed to generate token", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{
		"token":     token,
		"tenant_id": user.TenantID,
	}); err != nil {
		s.logger.Error("failed to encode login response", zap.Error(err))
	}
}

func (s *Server) handlePasswordReset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	token, err := s.auth.RequestPasswordReset(r.Context(), req.Email)
	if err != nil {
		http.Error(w, "Email not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{
		"message": "Reset token generated",
		"token":   token,
	}); err != nil {
		s.logger.Error("failed to encode password reset response", zap.Error(err))
	}
}

func (s *Server) handlePasswordResetComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token       string `json:"token"`
		NewPassword string `json:"new_password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if _, err := s.auth.CompletePasswordReset(r.Context(), req.Token, req.NewPassword); err != nil {
		http.Error(w, "Invalid or expired token", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{
		"message": "Password reset successful",
	}); err != nil {
		s.logger.Error("failed to encode password reset complete response", zap.Error(err))
	}
}

func (s *Server) SetAuthService(authService *auth.AuthService) {
	s.auth = authService
}

func (s *Server) SetAuditLogger(logger *auth.AuditLogger) {
	s.auditLogger = logger
}

func (s *Server) WrapWithRBACPermission(permission string, handler http.HandlerFunc) http.HandlerFunc {
	if s.rbacService == nil {
		return handler
	}
	return func(w http.ResponseWriter, r *http.Request) {
		userID := rbac.GetUserID(r.Context())
		if userID.String() == "00000000-0000-0000-0000-000000000000" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if !s.rbacService.manager.UserHasPermission(userID, permission) {
			http.Error(w, "Forbidden - insufficient permissions", http.StatusForbidden)
			s.rbacService.auditor.LogPermissionCheck(userID, permission, false)
			return
		}
		s.rbacService.auditor.LogPermissionCheck(userID, permission, true)
		handler(w, r)
	}
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	metrics := fmt.Sprintf("vaultaire_requests_total %d\nvaultaire_errors_total %d\n",
		atomic.LoadInt64(&s.requestCount),
		atomic.LoadInt64(&s.errorCount),
	)
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(metrics))
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"version": "0.1.0",
		"build":   "2025-08-12",
		"go":      runtime.Version(),
	})
}

func (s *Server) handleStatusPage(w http.ResponseWriter, r *http.Request) {
	status, healthy, total := s.healthChecker.GetOverallStatus()
	uptime := time.Since(s.startTime).Truncate(time.Second)

	statusLabel := "All Systems Operational"
	statusClass := "operational"
	if status != "healthy" {
		statusLabel = "Degraded"
		statusClass = "degraded"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, statusPageHTML, statusClass, statusLabel, "0.1.0", uptime, healthy, total)
}

const statusPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Status — stored.ge</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:#0f172a;color:#e2e8f0;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0}
.card{background:#1e293b;border-radius:12px;padding:2.5rem;max-width:480px;width:100%%;text-align:center}
h1{font-size:1.5rem;margin:0 0 1.5rem;color:#e2e8f0}
.brand{font-size:1.1rem;color:#94a3b8;margin-bottom:1rem}
.status{font-size:1.25rem;font-weight:700;padding:0.5rem 1rem;border-radius:8px;display:inline-block;margin-bottom:1.5rem}
.operational{background:#065f46;color:#6ee7b7}
.degraded{background:#78350f;color:#fcd34d}
.detail{color:#94a3b8;font-size:0.9rem;margin:0.4rem 0}
a{color:#60a5fa;text-decoration:none}
a:hover{text-decoration:underline}
</style>
</head>
<body>
<div class="card">
<div class="brand">stored.ge</div>
<h1>System Status</h1>
<div class="status %s">%s</div>
<p class="detail">Version: %s</p>
<p class="detail">Uptime: %s</p>
<p class="detail">Backends: %d / %d healthy</p>
<p class="detail" style="margin-top:1.5rem"><a href="/health?details=true">JSON health endpoint</a></p>
</div>
</body>
</html>`

func (s *Server) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", uuid.New().String())
		w.Header().Set("Server", "stored.ge")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&s.requestCount, 1)
		start := time.Now()
		next.ServeHTTP(w, r)
		s.logger.Info("request",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.String("request_id", w.Header().Get("X-Request-Id")),
			zap.Duration("latency", time.Since(start)),
		)
	})
}

// Start begins serving requests and launches the background health check loop.
// The health check goroutine is tied to a context that is cancelled when
// Shutdown is called, so it exits cleanly without a goroutine leak.
func (s *Server) Start() error {
	ctx, cancel := context.WithCancel(context.Background())

	// Store cancel so Shutdown can stop the health goroutines.
	s.httpServer.RegisterOnShutdown(cancel)

	go s.startHealthChecks(ctx)

	// Clean up expired dashboard sessions hourly.
	if ds, ok := s.sessionStore.(*dashauth.DBStore); ok {
		ds.StartCleanup(ctx)
	}

	s.logger.Info("Starting server with RBAC and API Key Management",
		zap.Int("port", s.config.Server.Port))
	return s.httpServer.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) GetRouter() chi.Router {
	return s.router
}

func (s *Server) requireJWT(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if token == "" {
			http.Error(w, "Unauthorized - missing token", http.StatusUnauthorized)
			return
		}
		token = strings.TrimPrefix(token, "Bearer ")
		token = strings.TrimSpace(token)

		claims, err := s.auth.ValidateJWT(token)
		if err != nil {
			http.Error(w, "Invalid token: "+err.Error(), http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), userIDKey, claims.UserID)
		ctx = context.WithValue(ctx, emailKey, claims.Email)
		ctx = context.WithValue(ctx, tenantIDKey, claims.TenantID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
