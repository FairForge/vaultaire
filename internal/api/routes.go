package api

import (
	"github.com/go-chi/chi/v5"
)

// registerQuotaRoutes registers quota management routes
func (s *Server) registerQuotaRoutes() {
	s.router.Route("/api/v1/quota", func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/", s.handleGetQuota)
		r.Post("/upgrade", s.handleUpgradeQuota)
		r.Get("/history", s.handleGetQuotaHistory)
	})
}
