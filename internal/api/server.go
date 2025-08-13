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

	"github.com/gorilla/mux"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/config"
)

type Server struct {
	config     *config.Config
	logger     *zap.Logger
	router     *mux.Router
	httpServer *http.Server
	db         *sql.DB
	auth       *Auth
	events     chan Event

	requestCount int64
	errorCount   int64
	startTime    time.Time
}

func NewServer(cfg *config.Config, logger *zap.Logger, db *sql.DB) *Server {
	s := &Server{
		config:    cfg,
		logger:    logger,
		db:        db,
		auth:      NewAuth(db, logger),
		events:    make(chan Event, 1000),
		router:    mux.NewRouter(),
		startTime: time.Now(),
	}

	s.setupRoutes()

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      s.router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	return s
}

func (s *Server) setupRoutes() {
	s.router.HandleFunc("/health", s.handleHealth).Methods("GET")
	s.router.HandleFunc("/ready", s.handleReady).Methods("GET")
	s.router.HandleFunc("/metrics", s.handleMetrics).Methods("GET")
	s.router.HandleFunc("/version", s.handleVersion).Methods("GET")

	s.router.Use(s.loggingMiddleware)

	s.router.PathPrefix("/").HandlerFunc(s.handleS3Request)

}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	health := map[string]interface{}{
		"status":  "healthy",
		"version": "0.1.0",
		"uptime":  time.Since(s.startTime).Seconds(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(health)
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ready := map[string]interface{}{
		"ready":     true,
		"memory_mb": getMemoryUsageMB(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ready)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	metrics := fmt.Sprintf("vaultaire_requests_total %d\nvaultaire_errors_total %d\n",
		atomic.LoadInt64(&s.requestCount),
		atomic.LoadInt64(&s.errorCount),
	)

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(metrics))
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	version := map[string]string{
		"version": "0.1.0",
		"build":   "2025-08-12",
		"go":      runtime.Version(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(version)
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
