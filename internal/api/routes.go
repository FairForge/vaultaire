package api

import (
	"github.com/go-chi/chi/v5"
)

// registerQuotaRoutes registers the quota read routes. The self-service
// POST /upgrade that changed a tenant's tier with no payment was removed in
// Review R10 (R10-09); the two GETs are dead-by-auth today (requireAuth sets
// no tenant) and move under /api/v1/user in WP-R10-7.
func (s *Server) registerQuotaRoutes() {
	s.router.Route("/api/v1/quota", func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/", s.handleGetQuota)
		r.Get("/history", s.handleGetQuotaHistory)
	})
}
