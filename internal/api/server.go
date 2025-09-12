package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/docs"
	"github.com/FairForge/vaultaire/internal/engine"
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

	requestCount int64
	testMode     bool
	errorCount   int64
	startTime    time.Time
}

type QuotaManager interface {
	GetUsage(ctx context.Context, tenantID string) (used, limit int64, err error)
	CheckAndReserve(ctx context.Context, tenantID string, bytes int64) (bool, error)
	CreateTenant(ctx context.Context, tenantID, plan string, storageLimit int64) error
	UpdateQuota(ctx context.Context, tenantID string, newLimit int64) error
	ListQuotas(ctx context.Context) ([]map[string]interface{}, error)
	DeleteQuota(ctx context.Context, tenantID string) error
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

	// Add ALL middleware BEFORE any routes
	if db != nil {
		// 		s.router.Use(ExtractTenant(db, logger))
	}

	// Add logging middleware
	s.router.Use(s.loggingMiddleware)

	if db != nil {
	}

	// Now set up routes (no middleware should be added in setupRoutes)
	s.setupRoutes()

	// Add auth routes after main routes
	if db != nil {
		// 		authHandler := NewAuthHandler(db, logger)
		s.router.Post("/auth/register", authHandler.Register)
	}

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      s.router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	return s
}

func (s *Server) setupRoutes() {
	s.router.Get("/health", s.handleHealth)
	s.router.Get("/ready", s.handleReady)
	s.router.Get("/metrics", s.handleMetrics)
	s.router.Get("/version", s.handleVersion)

	// Add usage routes
	s.router.Get("/api/v1/usage/stats", s.handleGetUsageStats)
	s.router.Get("/api/v1/usage/alerts", s.handleGetUsageAlerts)
	s.router.Get("/api/v1/presigned", s.handleGetPresignedURL)
	// Add quota management routes
	s.setupQuotaManagementRoutes()

	// API Documentation routes
	s.router.Get("/docs", docs.SwaggerUIHandler())
	s.router.Get("/openapi.json", docs.OpenAPIJSONHandler())

	// S3 catch-all (MUST be last)
	s.router.HandleFunc("/*", s.handleS3Request)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	health := map[string]interface{}{
		"status":  "healthy",
		"version": "0.1.0",
		"uptime":  time.Since(s.startTime).Seconds(),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(health)
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ready := map[string]interface{}{
		"ready":     true,
		"memory_mb": getMemoryUsageMB(),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ready)
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
	s.logger.Info("Starting server", zap.Int("port", s.config.Server.Port))
	return s.httpServer.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func getMemoryUsageMB() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Alloc / 1024 / 1024
}

// GetRouter returns the chi router for adding routes
// GetRouter returns the chi router for adding routes

// GetRouter returns the chi router for adding routes
func (s *Server) GetRouter() chi.Router {
	return s.router
}
