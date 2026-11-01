package api

// Claimable apps (WP-APP-1 PR D, Stage 1a Prompt 55; docs/ACCOUNTS.md
// § Apps; F-42; internal/auth/claimable.go). /api/v1/partner is a
// PARTNER's management key's only (scope partner, on a tenant an operator
// made a partner): every other caller — a person, any other key, no key —
// gets the unknown route's 404. A person claims a link on the dashboard
// (/dashboard/claim/{token}) or with POST /api/v1/manage/claims/{token}.
// The claimable_purge job (hourly) schedules every unclaimed provisional
// tenant past its 7 days for erasure now.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/hosts"
)

func (s *Server) registerPartnerRoutes() {
	rl, im := s.jsonAPIMiddleware()
	s.router.Route("/api/v1/partner", func(r chi.Router) {
		r.Use(s.requireCaller(surfaceManage))
		r.Use(s.requirePartnerKey)
		r.Use(s.logManagementRequest)
		r.Use(rl.Middleware)
		r.Use(im.Middleware)
		r.Post("/claimables", s.handlePartnerCreateClaimable)
		r.Get("/claimables", s.handlePartnerListClaimables)
	})
}

// requirePartnerKey answers 404 unless a partner's own key calls.
func (s *Server) requirePartnerKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := managementKeyOf(r.Context())
		var partner bool
		if k != nil && k.Has(auth.ScopePartner) && s.db != nil {
			_ = s.db.QueryRowContext(r.Context(), `SELECT partner FROM tenants WHERE id = $1`, k.TenantID).Scan(&partner)
		}
		if !partner {
			writeManagementError(w, ErrTypeNotFound, "not_found", "no such route", "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// claimURL is the link a partner hands its user.
func claimURL(token string) string { return hosts.Get().URL("/dashboard/claim/" + token) }

// handlePartnerCreateClaimable is POST /api/v1/partner/claimables {label}.
func (s *Server) handlePartnerCreateClaimable(w http.ResponseWriter, r *http.Request) {
	k := managementKeyOf(r.Context())
	var req struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}
	c, err := auth.CreateClaimable(r.Context(), s.db, k.TenantID, req.Label, k.CreatedBy)
	switch {
	case errors.Is(err, auth.ErrBadChildLabel):
		writeManagementError(w, ErrTypeInvalidRequest, "label_required", err.Error(), "label")
		return
	case errors.Is(err, auth.ErrNotPartner), errors.Is(err, auth.ErrTenantNotFound):
		writeManagementError(w, ErrTypeNotFound, "not_found", "no such route", "")
		return
	case errors.Is(err, auth.ErrTenantSuspended):
		writeManagementError(w, ErrTypePermission, "account_suspended", "the account is suspended", "")
		return
	case errors.Is(err, auth.ErrKeyLimitReached):
		writeManagementError(w, ErrTypeConflict, "key_limit_exceeded", err.Error(), "")
		return
	case err != nil:
		s.logger.Error("partner: create claimable", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to create the claimable", "")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"object": "claimable", "id": c.ClaimID, "tenant_id": c.TenantID, "label": c.Label,
		"claim_url": claimURL(c.Token), "expires_at": c.ExpiresAt, "storage_limit_bytes": int64(auth.ProvisionalStorageBytes),
		"management_key": managementKeyJSON(c.Key, c.KeySecret), "request_id": getRequestID(w)})
}

// handlePartnerListClaimables is GET /api/v1/partner/claimables (newest
// first, 100; never a token).
func (s *Server) handlePartnerListClaimables(w http.ResponseWriter, r *http.Request) {
	k := managementKeyOf(r.Context())
	rows, err := s.db.QueryContext(r.Context(), `SELECT id, tenant_id, label, created_at, expires_at, claimed_at
		  FROM claims WHERE partner_tenant_id = $1 ORDER BY created_at DESC, id LIMIT 100`, k.TenantID)
	if err != nil {
		s.logger.Error("partner: list claimables", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list the claimables", "")
		return
	}
	defer func() { _ = rows.Close() }()
	items := []interface{}{}
	for rows.Next() {
		var id, tenant, label string
		var created, expires time.Time
		var claimed *time.Time
		if err := rows.Scan(&id, &tenant, &label, &created, &expires, &claimed); err != nil {
			s.logger.Error("partner: list claimables", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list the claimables", "")
			return
		}
		state := "open"
		switch {
		case claimed != nil:
			state = "claimed"
		case time.Now().After(expires):
			state = "expired"
		}
		items = append(items, map[string]any{"object": "claimable", "id": id, "tenant_id": tenant, "label": label,
			"created_at": created, "expires_at": expires, "claimed_at": claimed, "state": state})
	}
	if err := rows.Err(); err != nil {
		s.logger.Error("partner: list claimables", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list the claimables", "")
		return
	}
	writeListResponse(w, items, false, "", len(items))
}

// handleMgmtClaim is POST /api/v1/manage/claims/{token}: a person keeps the
// claimable as an app of the account they act on.
func (s *Server) handleMgmtClaim(w http.ResponseWriter, r *http.Request) {
	tenantID := activeTenant(r)
	userID, _ := r.Context().Value(userIDKey).(string)
	if s.db == nil || tenantID == "" {
		writeManagementError(w, ErrTypeAPI, "no_database", "database unavailable", "")
		return
	}
	c, err := auth.ClaimProvisional(r.Context(), s.db, chi.URLParam(r, "token"), tenantID, userID)
	switch {
	case errors.Is(err, auth.ErrClaimGone):
		writeManagementError(w, ErrTypeGone, "claim_gone", "this claim link is used, expired or unknown", "token")
		return
	case errors.Is(err, auth.ErrClaimNeedsVerified):
		writeManagementError(w, ErrTypePermission, "claim_needs_verified_account", "verify your e-mail address first, then claim the app", "")
		return
	case errors.Is(err, auth.ErrChildOfChild):
		writeManagementError(w, ErrTypeInvalidRequest, "child_of_child", "claim it on your own account, not an app, a sandbox or a child account", "")
		return
	case err != nil:
		s.logger.Error("claim", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to claim the app", "")
		return
	}
	if s.flags != nil {
		_ = s.flags.Refresh(r.Context())
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "app", "id": c.TenantID, "label": c.Label, "kind": "app",
		"parent_id": tenantID, "storage_used_bytes": c.UsedBytes, "request_id": getRequestID(w)})
}

// purgeExpiredClaimables is the claimable_purge job's run.
func (s *Server) purgeExpiredClaimables(ctx context.Context) (int, error) {
	if s.db == nil {
		return 0, nil
	}
	return auth.ExpiredClaimables(ctx, s.db)
}

// handleAdminSetPartner is PUT /api/v1/admin/tenants/{tenant_id}/partner
// {"partner": bool}: an operator's grant, audited.
func (s *Server) handleAdminSetPartner(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenant_id")
	var req struct {
		Partner *bool `json:"partner"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Partner == nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", `the body must be {"partner": true|false}`, "partner")
		return
	}
	err := auth.SetPartner(r.Context(), s.db, tenantID, *req.Partner)
	switch {
	case errors.Is(err, auth.ErrTenantNotFound):
		writeManagementError(w, ErrTypeNotFound, "tenant_not_found", "no such tenant", "tenant_id")
		return
	case errors.Is(err, auth.ErrChildOfChild):
		writeManagementError(w, ErrTypeInvalidRequest, "child_of_child", "a child or a sandbox is never a partner", "tenant_id")
		return
	case err != nil:
		s.logger.Error("admin: set partner", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to set the partner flag", "")
		return
	}
	action := "admin.partner_granted"
	if !*req.Partner {
		action = "admin.partner_revoked"
	}
	audit.Record(r.Context(), s.db, audit.Entry{TenantID: tenantID, EventType: "admin", Action: action, Resource: "tenant:" + tenantID})
	writeJSON(w, http.StatusOK, map[string]any{"object": "tenant", "id": tenantID, "partner": *req.Partner, "request_id": getRequestID(w)})
}
