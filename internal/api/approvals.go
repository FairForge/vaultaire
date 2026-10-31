package api

// Approvals (WP-AGENT-1 PR B) — registered here so PR A's routes compile;
// the gate itself lands with PR B.

import "github.com/go-chi/chi/v5"

func (s *Server) registerApprovalRoutes(chi.Router) {}
