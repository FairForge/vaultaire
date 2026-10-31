package api

// Agents as principals (WP-AGENT-1, Stage 1a Prompt 55; docs/ACCOUNTS.md
// § Agents; F-41; migration 122). An agent grant IS a management key with a
// person behind it (internal/auth/agent_grants.go): it authenticates through
// requireCaller like any management key, acts as a developer on
// /api/v1/manage (manage:write) — narrowed by the approval gate
// (approvals.go), never Private (line 8) — and the audit trail names it as
// agent:<grant> on behalf of the member who granted it. The grant itself is
// the person's: listed by any member, renewed and revoked by a person only.
// Dark behind the agent_grants flag (default OFF, per tenant): off, a
// grant's key authenticates nothing and the connect flow is 404.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
)

// flagAgentGrants opens agent grants for a tenant (WP-AGENT-1). Default OFF,
// per tenant first (tenant zero).
const flagAgentGrants = "agent_grants"

type agentGrantCtxKey struct{}

// agentOf is the agent grant's key that authenticated the request (nil = a
// person, or a server's key).
func agentOf(ctx context.Context) *auth.ManagementKey {
	k, _ := ctx.Value(agentGrantCtxKey{}).(*auth.ManagementKey)
	return k
}

func (s *Server) agentsEnabled(tenantID string) bool {
	return s.flags != nil && tenantID != "" && s.flags.Enabled(flagAgentGrants, tenantID)
}

// registerAgentRoutes are the grants' routes on /api/v1/manage (a member's).
func (s *Server) registerAgentRoutes(r chi.Router) {
	r.Get("/agents", s.handleMgmtListAgents)
	r.With(s.requirePerson, s.requireRole(auth.ActionManagementKey)).Post("/agents/{id}/renew", s.handleMgmtRenewAgent)
	r.With(s.requirePerson, s.requireRole(auth.ActionManagementKey)).Delete("/agents/{id}", s.handleMgmtRevokeAgent)
	s.registerApprovalRoutes(r)
}

func agentGrantJSON(g *auth.AgentGrant) map[string]any {
	return map[string]any{"object": "agent_grant", "id": g.ID, "agent_name": g.AgentName, "client_id": g.ClientID,
		"granted_by": g.GrantedBy, "granted_by_email": g.GrantedByEmail, "approved_scopes": g.ApprovedScopes,
		"storage_limit_bytes": g.StorageLimit, "egress_limit_bytes": g.EgressLimit, "management_key_id": g.ManagementKeyID,
		"test_management_key_id": g.TestManagementKeyID, "created_at": g.CreatedAt, "renewed_at": g.RenewedAt,
		"expires_at": g.ExpiresAt, "revoked_at": g.RevokedAt, "last_used_at": g.LastUsedAt}
}

func (s *Server) agentsOr404(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := activeTenant(r)
	if s.db == nil || !s.agentsEnabled(tenantID) {
		writeManagementError(w, ErrTypeNotFound, "not_found", "no such route", "")
		return "", false
	}
	return tenantID, true
}

// handleMgmtListAgents is GET /api/v1/manage/agents.
func (s *Server) handleMgmtListAgents(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.agentsOr404(w, r)
	if !ok {
		return
	}
	gs, err := auth.ListAgentGrants(r.Context(), s.db, tenantID)
	if err != nil {
		s.logger.Error("list agent grants", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list the agent grants", "")
		return
	}
	data := make([]map[string]any, 0, len(gs))
	for _, g := range gs {
		data = append(data, agentGrantJSON(g))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data, "request_id": getRequestID(w)})
}

func (s *Server) writeAgentError(w http.ResponseWriter, err error, what string) {
	if errors.Is(err, auth.ErrAgentGrantNotFound) {
		writeManagementError(w, ErrTypeNotFound, "agent_grant_not_found", "no agent grant of this account has that id", "id")
		return
	}
	s.logger.Error(what, zap.Error(err))
	writeManagementError(w, ErrTypeAPI, "internal_error", "failed to "+what, "")
}

// handleMgmtRenewAgent is POST /api/v1/manage/agents/{id}/renew: a fresh
// 30 days on both keys. It takes no body: a renewal never widens.
func (s *Server) handleMgmtRenewAgent(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.agentsOr404(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	g, err := auth.RenewAgentGrant(r.Context(), s.db, tenantID, id)
	if err != nil {
		s.writeAgentError(w, err, "renew the agent grant")
		return
	}
	audit.Record(r.Context(), s.db, audit.Entry{TenantID: tenantID, Action: "agent.grant_renewed", Resource: "agent:" + id,
		Metadata: map[string]any{"expires_at": g.ExpiresAt}})
	out := agentGrantJSON(g)
	out["request_id"] = getRequestID(w)
	writeJSON(w, http.StatusOK, out)
}

// handleMgmtRevokeAgent is DELETE /api/v1/manage/agents/{id}.
func (s *Server) handleMgmtRevokeAgent(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.agentsOr404(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	minted, err := auth.RevokeAgentGrant(r.Context(), s.db, tenantID, id)
	if err != nil {
		s.writeAgentError(w, err, "revoke the agent grant")
		return
	}
	for _, tk := range minted { // "<tenant>/<key id>": a sandbox's keys are the sandbox's
		t, k, _ := strings.Cut(tk, "/")
		if err := s.auth.RevokeAPIKey(r.Context(), t, k); err != nil && !errors.Is(err, auth.ErrKeyRevoked) {
			s.logger.Warn("revoke an agent's S3 key", zap.String("key_id", k), zap.Error(err))
		}
	}
	audit.Record(r.Context(), s.db, audit.Entry{TenantID: tenantID, Action: "agent.grant_revoked", Resource: "agent:" + id})
	writeJSON(w, http.StatusOK, map[string]any{"object": "agent_grant", "id": id, "revoked": true, "request_id": getRequestID(w)})
}

// markAgentKey ties an S3 key an agent minted to its grant: the grant's
// caps (the consent's — the agent may lower them later, never raise them,
// RP88-8), its expiry clamped to the grant's, and agent_grant_id, so the
// key is revoked with the grant and never reaches a Private bucket.
func (s *Server) markAgentKey(ctx context.Context, tenantID, keyID string) error {
	a := agentOf(ctx)
	if a == nil {
		return nil
	}
	storage, egress, err := auth.AgentGrantCaps(ctx, s.db, a.AgentGrantID)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE api_keys k SET agent_grant_id = $3::uuid,
		       storage_limit_bytes = COALESCE($4, k.storage_limit_bytes), egress_limit_bytes = COALESCE($5, k.egress_limit_bytes),
		       expires_at = CASE WHEN m.expires_at IS NULL THEN k.expires_at
		                         WHEN k.expires_at IS NULL OR k.expires_at > m.expires_at THEN m.expires_at ELSE k.expires_at END
		  FROM management_keys m
		 WHERE k.id = $1::uuid AND k.tenant_id = $2 AND m.id = $6::uuid`,
		keyID, tenantID, a.AgentGrantID, storage, egress, a.ID)
	return err
}
