package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/compliance"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/docs"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/rbac"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

type Server struct {
	config       *config.Config
	logger       *zap.Logger
	router       chi.Router
	httpServer   *http.Server
	db           *sql.DB
	events       chan Event
	engine       *engine.CoreEngine
	quotaManager QuotaManager
	rbacService  *RBACService
	auth         *auth.AuthService
	auditLogger  *auth.AuditLogger

	requestCount  int64
	testMode      bool
	errorCount    int64
	healthChecker *BackendHealthChecker
	startTime     time.Time
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

	// Initialize auth service and audit logger
	s.auth = auth.NewAuthService(nil)
	s.auditLogger = auth.NewAuditLogger()
	s.auth.SetAuditLogger(s.auditLogger)

	// Initialize RBAC
	s.rbacService = NewRBACService(logger)

	// Add ALL middleware BEFORE any routes
	s.router.Use(s.rbacService.InjectUserContext)
	s.router.Use(s.loggingMiddleware)

	// Set up routes
	s.setupRoutes()

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      s.router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	return s
}

func (s *Server) setupRoutes() {
	s.router.Get("/health", s.handleHealthEnhanced)
	s.router.Get("/health/live", s.handleLiveness)
	s.router.Get("/health/ready", s.handleReadiness)
	s.router.Get("/health/backends", s.handleBackendsHealth)
	s.router.Get("/ready", s.handleReadiness) // Keep old endpoint for compatibility
	s.router.Get("/metrics", s.handleMetrics)
	s.router.Get("/version", s.handleVersion)

	// Auth routes - using server's shared auth service
	s.logger.Info("Registering auth routes")
	s.router.Post("/auth/register", s.handleRegister)
	s.router.Post("/auth/login", s.handleLogin)
	s.router.Post("/auth/password-reset", s.handlePasswordReset)
	s.router.Post("/auth/password-reset/complete", s.handlePasswordResetComplete)

	// API Documentation routes
	s.router.Get("/docs", docs.SwaggerUIHandler())
	s.router.Get("/openapi.json", docs.OpenAPIJSONHandler())

	// IMPORTANT: Register ALL /api routes BEFORE the S3 catch-all
	// The order matters! More specific routes must come first

	// User API routes under /api/v1/user - MUST be before catch-all
	s.logger.Info("Registering user API routes")
	s.registerUserAPIRoutes()

	// Quota routes under /api/v1/quota - MUST be before catch-all
	s.logger.Info("Registering quota routes")
	s.registerQuotaRoutes()

	// Compliance routes under /api/compliance - MUST be before catch-all
	s.logger.Info("Registering compliance routes")
	s.registerComplianceRoutes()

	// Usage routes with RBAC under /api/v1/usage
	s.router.With(s.rbacService.RequirePermission("quota.read")).
		Get("/api/v1/usage/stats", s.handleGetUsageStats)
	s.router.With(s.rbacService.RequirePermission("quota.read")).
		Get("/api/v1/usage/alerts", s.handleGetUsageAlerts)
	s.router.With(s.rbacService.RequirePermission("storage.read")).
		Get("/api/v1/presigned", s.handleGetPresignedURL)

	// Quota management routes
	s.setupQuotaManagementRoutes()

	// Pattern routes if DB available
	s.setupPatternRoutes()

	// RBAC management endpoints under /api/rbac
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

	// S3 catch-all (MUST be ABSOLUTELY LAST)
	// This catches everything that didn't match above
	s.logger.Info("Registering S3 catch-all handler")
	s.router.HandleFunc("/*", s.handleS3Request)
}

// registerComplianceRoutes sets up all GDPR compliance endpoints
func (s *Server) registerComplianceRoutes() {
	// Initialize compliance handler with mock implementations
	// In production, these would use real database implementations
	complianceHandler := compliance.NewAPIHandler(
		compliance.NewGDPRService(nil, s.logger),
		compliance.NewPortabilityService(nil, nil, s.logger),
		compliance.NewConsentService(nil, s.logger),
		compliance.NewBreachService(nil, s.logger),
		compliance.NewROPAService(nil, s.logger),
		compliance.NewPrivacyService(nil), // Added privacy service
		s.logger,
	)

	s.router.Route("/api/compliance", func(r chi.Router) {
		// All compliance routes require authentication
		r.Use(s.requireJWT)

		// GDPR Subject Access Requests (Article 15)
		r.Post("/sar", complianceHandler.HandleCreateSAR)
		r.Get("/sar/{id}", complianceHandler.HandleGetSARStatus)
		r.Get("/inventory", complianceHandler.HandleGetDataInventory)
		r.Get("/activities", complianceHandler.HandleListProcessingActivities)

		// Right to Erasure (Article 17)
		r.Post("/deletion", complianceHandler.HandleCreateDeletionRequest)

		// Data Portability (Article 20)
		r.Post("/export", complianceHandler.HandleCreateExport)
		r.Get("/export/{id}", complianceHandler.HandleGetExport)

		// Consent Management (Articles 7 & 8)
		r.Post("/consent", complianceHandler.HandleGrantConsent)
		r.Delete("/consent/{purpose}", complianceHandler.HandleWithdrawConsent)
		r.Get("/consent", complianceHandler.HandleGetConsentStatus)
		r.Get("/consent/{purpose}", complianceHandler.HandleCheckConsent)
		r.Get("/consent/history", complianceHandler.HandleGetConsentHistory)
		r.Get("/consent/purposes", complianceHandler.HandleListConsentPurposes)

		// Breach Notification (Articles 33 & 34)
		r.Post("/breach", complianceHandler.HandleReportBreach)
		r.Get("/breach/{id}", complianceHandler.HandleGetBreach)
		r.Get("/breach", complianceHandler.HandleListBreaches)
		r.Patch("/breach/{id}", complianceHandler.HandleUpdateBreach)
		r.Post("/breach/{id}/notify", complianceHandler.HandleNotifyBreach)
		r.Get("/breach/stats", complianceHandler.HandleGetBreachStats)

		// Records of Processing Activities - ROPA (Article 30)
		r.Post("/ropa/activities", complianceHandler.HandleCreateActivity)
		r.Get("/ropa/activities/{id}", complianceHandler.HandleGetActivity)
		r.Get("/ropa/activities", complianceHandler.HandleListActivities)
		r.Patch("/ropa/activities/{id}", complianceHandler.HandleUpdateActivity)
		r.Delete("/ropa/activities/{id}", complianceHandler.HandleDeleteActivity)
		r.Post("/ropa/activities/{id}/review", complianceHandler.HandleReviewActivity)
		r.Get("/ropa/report", complianceHandler.HandleGetROPAReport)
		r.Get("/ropa/compliance/{id}", complianceHandler.HandleCheckCompliance)
		r.Get("/ropa/stats", complianceHandler.HandleGetROPAStats)

		// Privacy Controls (Article 25) - Added routes
		r.Post("/privacy/controls", complianceHandler.HandleEnablePrivacyControl)
		r.Post("/privacy/minimize", complianceHandler.HandleMinimizeData)
		r.Get("/privacy/purpose/{dataId}/{purpose}", complianceHandler.HandleCheckPurpose)
		r.Post("/privacy/pseudonymize", complianceHandler.HandlePseudonymize)
	})
}

// Auth handlers using the server's shared auth service

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

	// Use server's auth service
	user, tenant, apiKey, err := s.auth.CreateUserWithTenant(
		r.Context(), req.Email, req.Password, req.Company)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Return credentials
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{
		"accessKeyId":     apiKey.Key,
		"secretAccessKey": apiKey.Secret,
		"endpoint":        fmt.Sprintf("http://localhost:%d", s.config.Server.Port),
	}); err != nil {
		s.logger.Error("failed to encode register response", zap.Error(err))
	}

	s.logger.Info("user registered",
		zap.String("email", req.Email),
		zap.String("user_id", user.ID),
		zap.String("tenant_id", tenant.ID))
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

	// Use server's auth service
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

	response := map[string]string{
		"token":     token,
		"tenant_id": user.TenantID,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
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

	// In production, email the token. For now, return it
	response := map[string]string{
		"message": "Reset token generated",
		"token":   token, // Don't do this in production!
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
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

	err := s.auth.CompletePasswordReset(r.Context(), req.Token, req.NewPassword)
	if err != nil {
		http.Error(w, "Invalid or expired token", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"message": "Password reset successful"}); err != nil {
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
	version := map[string]string{
		"version": "0.1.0",
		"build":   "2025-08-12",
		"go":      runtime.Version(),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(version)
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&s.requestCount, 1)
		start := time.Now()

		next.ServeHTTP(w, r)

		s.logger.Info("request",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.Duration("latency", time.Since(start)),
		)
	})
}

func (s *Server) Start() error {
	s.logger.Info("Starting server with RBAC and API Key Management", zap.Int("port", s.config.Server.Port))
	return s.httpServer.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) GetRouter() chi.Router {
	return s.router
}

// requireJWT is middleware to check JWT authentication for API routes
func (s *Server) requireJWT(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if token == "" {
			http.Error(w, "Unauthorized - missing token", http.StatusUnauthorized)
			return
		}

		// Remove "Bearer " prefix if present
		token = strings.TrimPrefix(token, "Bearer ")
		token = strings.TrimSpace(token)

		claims, err := s.auth.ValidateJWT(token)
		if err != nil {
			http.Error(w, "Invalid token: "+err.Error(), http.StatusUnauthorized)
			return
		}

		// Use typed context keys
		ctx := context.WithValue(r.Context(), userIDKey, claims.UserID)
		ctx = context.WithValue(ctx, emailKey, claims.Email)
		ctx = context.WithValue(ctx, tenantIDKey, claims.TenantID)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
