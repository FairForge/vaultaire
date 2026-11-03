package api

// The family (WP-FAMILY-1, Stage 1a Prompt 56; docs/ACCOUNTS.md § A family;
// internal/auth/family.go). A family is a pool of personal tenants: the
// organizer invites people (a person, the owner, a fresh authenticator
// code), an invitee attaches their OWN account as a sponsored member, and
// the organizer's view is usage and caps — never a bucket, an object, a key
// or a request of a member. Dark behind the `family` flag (default OFF, per
// tenant: the organizer's).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/email"
	"github.com/FairForge/vaultaire/internal/usage"
)

// flagFamily turns the family on for an organizer (per tenant first).
const flagFamily = "family"

func (s *Server) registerFamilyRoutes(r chi.Router) {
	r.Get("/family", s.handleMgmtFamily)
	// A person's only: a management key or an agent never invites anyone
	// into a pool, nor attaches an account to one.
	// Every write is the owner's: the pool is paid for (auth.ActionBilling).
	r.With(s.requirePerson, s.requireRole(auth.ActionBilling)).Post("/family/invitations", s.handleMgmtFamilyInvite)
	r.With(s.requirePerson, s.requireRole(auth.ActionBilling)).Post("/family/join/{token}", s.handleMgmtFamilyJoin)
	// PR B: caps, removal, leaving — a person's, the owner's.
	r.With(s.requirePerson, s.requireRole(auth.ActionBilling)).Put("/family/members/{id}/cap", s.handleMgmtFamilyCap)
	r.With(s.requirePerson, s.requireRole(auth.ActionBilling)).Delete("/family/members/{id}", s.handleMgmtFamilyRemove)
	r.With(s.requirePerson, s.requireRole(auth.ActionBilling)).Post("/family/leave", s.handleMgmtFamilyLeave)
}

// familyOn reports whether tenantID's pool root has the family flag (a
// member reads it on its organizer).
func (s *Server) familyOn(ctx context.Context, tenantID string) bool {
	if s.db == nil || s.flags == nil || tenantID == "" {
		return false
	}
	root, err := usage.PoolRoot(ctx, s.db, tenantID)
	if err != nil {
		return false
	}
	return s.flags.Enabled(flagFamily, root)
}

func writeFamilyNotFound(w http.ResponseWriter) {
	writeManagementError(w, ErrTypeNotFound, "not_found", "no such route", "")
}

// writeFamilyError answers the family flow's refusals (false: not one).
func writeFamilyError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, auth.ErrFamilyFull):
		writeManagementError(w, ErrTypeConflict, "family_full", "a family is six people, the organizer included (pending invitations count)", "")
	case errors.Is(err, auth.ErrMemberPaysOwnPlan):
		writeManagementError(w, ErrTypeConflict, "member_pays_own_plan", "this account pays for its own plan: cancel it, then join the family", "")
	case errors.Is(err, auth.ErrCannotJoinFamily):
		writeManagementError(w, ErrTypeConflict, "cannot_join_family", err.Error(), "")
	case errors.Is(err, auth.ErrNotFamilyOrganizer):
		writeManagementError(w, ErrTypeConflict, "not_family_organizer", err.Error(), "")
	case errors.Is(err, auth.ErrAlreadyMember):
		writeManagementError(w, ErrTypeConflict, "already_in_family", "this address is already in the family", "email")
	case errors.Is(err, auth.ErrInvitationRateLimited):
		writeManagementError(w, ErrTypeRateLimit, "invitation_rate_limited", err.Error(), "")
	default:
		return writeInvitationError(w, err)
	}
	return true
}

// handleMgmtFamily is GET /api/v1/manage/family: the organizer's members
// (label, storage used, cap), pending invitations and pool; a member's
// organizer. Usage and caps only.
func (s *Server) handleMgmtFamily(w http.ResponseWriter, r *http.Request) {
	tenantID := activeTenant(r)
	if !s.familyOn(r.Context(), tenantID) {
		writeFamilyNotFound(w)
		return
	}
	v, err := auth.Family(r.Context(), s.db, tenantID)
	if err != nil {
		s.logger.Error("family: read", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to read the family", "")
		return
	}
	out := map[string]any{"object": "family", "role": v.Role, "request_id": getRequestID(w)}
	if v.Role == "member" {
		out["organizer"] = v.Organizer
		writeJSON(w, http.StatusOK, out)
		return
	}
	members := make([]map[string]any, 0, len(v.Members))
	for _, m := range v.Members {
		members = append(members, map[string]any{"object": "family_member", "id": m.ID, "label": m.Label,
			"storage_used_bytes": m.StorageUsed, "storage_limit_bytes": m.StorageLimit, "joined_at": m.JoinedAt})
	}
	invitations := make([]map[string]any, 0, len(v.Invitations))
	for _, i := range v.Invitations {
		invitations = append(invitations, map[string]any{"object": "family_invitation", "id": i.ID, "email": i.Email, "expires_at": i.ExpiresAt})
	}
	out["members"], out["invitations"] = members, invitations
	out["pool_used_bytes"], out["pool_limit_bytes"], out["max_people"] = v.PoolUsed, v.PoolLimit, auth.MaxFamilyPeople
	writeJSON(w, http.StatusOK, out)
}

// handleMgmtFamilyInvite is POST /api/v1/manage/family/invitations
// {email, code}: the owner, with the current code of their authenticator
// app (a destructive-class act: it spends the pool), invites an address.
func (s *Server) handleMgmtFamilyInvite(w http.ResponseWriter, r *http.Request) {
	tenantID := activeTenant(r)
	userID, _ := r.Context().Value(userIDKey).(string)
	inviterEmail, _ := r.Context().Value(emailKey).(string)
	if !s.familyOn(r.Context(), tenantID) {
		writeFamilyNotFound(w)
		return
	}
	if role, _ := r.Context().Value(roleKey).(auth.Role); role != "" && role != auth.RoleOwner {
		writeManagementError(w, ErrTypePermission, "role_forbidden", "only the account's owner organizes its family", "")
		return
	}
	var req struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}
	if s.auth == nil || s.mfaService == nil {
		writeManagementError(w, ErrTypeAPI, "no_mfa", "two-factor sign-in is not available", "")
		return
	}
	if err := s.auth.CheckFreshTOTP(r.Context(), s.mfaService, userID, strings.TrimSpace(req.Code), "family_invite"); err != nil {
		if errors.Is(err, auth.ErrTOTPLocked) {
			w.Header().Set("Retry-After", "900")
			writeManagementError(w, ErrTypeRateLimit, "mfa_locked", "too many wrong authenticator codes: try again in 15 minutes", "code")
			return
		}
		writeManagementError(w, ErrTypePermission, "totp_required",
			"inviting someone into your family needs the current code of your authenticator app: {\"email\": …, \"code\": \"123456\"}", "code")
		return
	}
	inv, token, err := auth.InviteFamilyMember(r.Context(), s.db, tenantID, userID, req.Email, time.Now())
	if err != nil {
		if !writeFamilyError(w, err) {
			if strings.Contains(err.Error(), "invalid email") {
				writeManagementError(w, ErrTypeInvalidRequest, "invalid_email", "email must be an e-mail address", "email")
				return
			}
			s.logger.Error("family: invite", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to invite", "")
		}
		return
	}
	s.sendFamilyInvitation(r.Context(), tenantID, inviterEmail, inv, token)
	writeJSON(w, http.StatusCreated, map[string]any{"object": "family_invitation", "id": inv.ID, "email": inv.Email,
		"expires_at": inv.ExpiresAt, "request_id": getRequestID(w)})
}

// sendFamilyInvitation mails the link (a failure is a Warn: the organizer
// re-invites, which is the resend).
func (s *Server) sendFamilyInvitation(ctx context.Context, tenantID, inviterEmail string, inv auth.Invitation, token string) {
	var account string
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(name, ''), email) FROM tenants WHERE id = $1`, tenantID).Scan(&account)
	sender := s.emailSender
	if sender == nil {
		sender = email.NewSender(s.logger)
	}
	html, text, err := email.RenderFamilyInvitation(s.baseURL, token, inv.Email, inviterEmail)
	if err == nil {
		err = sender.Send(ctx, inv.Email, inviterEmail+" invites you to their family on Stored", html, text)
	}
	if err != nil {
		s.logger.Warn("family invitation: the e-mail was not sent", zap.String("invitation", inv.ID), zap.Error(err))
	}
}

// handleMgmtFamilyJoin is POST /api/v1/manage/family/join/{token}: the
// invited person attaches the account they act on — their own — to the
// family. The flag is the organizer's.
func (s *Server) handleMgmtFamilyJoin(w http.ResponseWriter, r *http.Request) {
	tenantID := activeTenant(r)
	userID, _ := r.Context().Value(userIDKey).(string)
	addr, _ := r.Context().Value(emailKey).(string)
	token := chi.URLParam(r, "token")
	inv, err := auth.FamilyInvitationByToken(r.Context(), s.db, token, time.Now())
	if err != nil {
		if !writeFamilyError(w, err) {
			s.logger.Error("family: read the invitation", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to read the invitation", "")
		}
		return
	}
	if !s.familyOn(r.Context(), inv.TenantID) {
		writeFamilyNotFound(w)
		return
	}
	if role, _ := r.Context().Value(roleKey).(auth.Role); role != "" && role != auth.RoleOwner {
		writeManagementError(w, ErrTypePermission, "role_forbidden", "join with your own account: you are not its owner", "")
		return
	}
	inv, err = auth.AcceptFamilyInvitation(r.Context(), s.db, token, userID, addr, tenantID, time.Now())
	if err != nil {
		if !writeFamilyError(w, err) {
			s.logger.Error("family: join", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to join the family", "")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "family_membership", "tenant_id": tenantID, "organizer_id": inv.TenantID,
		"request_id": getRequestID(w)})
}

// familyOwner answers whether the caller may act for the active tenant's
// family: the flag on, and the owner (false: answered).
func (s *Server) familyOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := activeTenant(r)
	if !s.familyOn(r.Context(), tenantID) {
		writeFamilyNotFound(w)
		return "", false
	}
	if role, _ := r.Context().Value(roleKey).(auth.Role); role != "" && role != auth.RoleOwner {
		writeManagementError(w, ErrTypePermission, "role_forbidden", "only the account's owner acts for its family", "")
		return "", false
	}
	return tenantID, true
}

// handleMgmtFamilyCap is PUT /api/v1/manage/family/members/{id}/cap
// {storage_limit_bytes}: a member's cap inside the pool (null or 0 = the
// pool decides; docs/ACCOUNTS.md "caps inside the pool").
func (s *Server) handleMgmtFamilyCap(w http.ResponseWriter, r *http.Request) {
	organizer, ok := s.familyOwner(w, r)
	if !ok {
		return
	}
	var req struct {
		StorageLimit *int64 `json:"storage_limit_bytes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}
	var limit int64
	if req.StorageLimit != nil {
		limit = *req.StorageLimit
	}
	if limit < 0 {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_cap", "a cap is a number of bytes, 0 or null for none", "storage_limit_bytes")
		return
	}
	member := chi.URLParam(r, "id")
	err := account.NewService(s.db, s.logger).SetFamilyMemberCap(r.Context(), organizer, member, limit)
	if errors.Is(err, account.ErrNotAFamilyMember) {
		writeManagementError(w, ErrTypeNotFound, "family_member_not_found", "no member of this family has that id", "id")
		return
	}
	if err != nil {
		s.logger.Error("family: cap", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to set the cap", "")
		return
	}
	audit.Record(r.Context(), s.db, audit.Entry{TenantID: organizer, Action: "family.member_capped", Resource: "tenant:" + member,
		Metadata: map[string]any{"storage_limit_bytes": limit}})
	var out *int64
	if limit > 0 {
		out = &limit
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "family_member", "id": member, "storage_limit_bytes": out, "request_id": getRequestID(w)})
}

// handleMgmtFamilyRemove is DELETE /api/v1/manage/family/members/{id}: the
// member detaches — its account, data, keys and sign-in its own.
func (s *Server) handleMgmtFamilyRemove(w http.ResponseWriter, r *http.Request) {
	organizer, ok := s.familyOwner(w, r)
	if !ok {
		return
	}
	member := chi.URLParam(r, "id")
	err := account.NewService(s.db, s.logger).RemoveFamilyMember(r.Context(), organizer, member, usage.FreeTierLimits.StorageBytes)
	if errors.Is(err, account.ErrNotAFamilyMember) {
		writeManagementError(w, ErrTypeNotFound, "family_member_not_found", "no member of this family has that id", "id")
		return
	}
	if err != nil {
		s.logger.Error("family: remove", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to remove the member", "")
		return
	}
	audit.Record(r.Context(), s.db, audit.Entry{TenantID: organizer, Action: "family.member_removed", Resource: "tenant:" + member})
	writeJSON(w, http.StatusOK, map[string]any{"object": "family_member", "id": member, "removed": true, "request_id": getRequestID(w)})
}

// handleMgmtFamilyLeave is POST /api/v1/manage/family/leave: the member's
// own account leaves its family, as a removal does.
func (s *Server) handleMgmtFamilyLeave(w http.ResponseWriter, r *http.Request) {
	member := activeTenant(r)
	if role, _ := r.Context().Value(roleKey).(auth.Role); role != "" && role != auth.RoleOwner {
		writeManagementError(w, ErrTypePermission, "role_forbidden", "only the account's owner takes it out of a family", "")
		return
	}
	organizer, err := account.NewService(s.db, s.logger).LeaveFamily(r.Context(), member, usage.FreeTierLimits.StorageBytes)
	if errors.Is(err, account.ErrNotAFamilyMember) {
		writeManagementError(w, ErrTypeConflict, "not_in_family", "this account is in no family", "")
		return
	}
	if err != nil {
		s.logger.Error("family: leave", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to leave the family", "")
		return
	}
	audit.Record(r.Context(), s.db, audit.Entry{TenantID: organizer, Action: "family.member_left", Resource: "tenant:" + member})
	writeJSON(w, http.StatusOK, map[string]any{"object": "family_membership", "tenant_id": member, "left": true, "request_id": getRequestID(w)})
}
