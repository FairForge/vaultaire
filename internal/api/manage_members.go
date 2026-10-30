package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/email"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// The members of a tenant on the management API (WP-TEAM-1 PR B, Stage 1a
// Prompt 52; docs/API.md § Members). Mounted inside /api/v1/manage
// (registerMemberRoutes), so every route has the acting member's role:
// listing is a read, inviting / changing a role / removing is
// member.manage (an owner's or an admin's), and the owner row is never
// removed or given another role. Accepting an invitation is any signed-in
// user's whose e-mail it was sent to.

// registerMemberRoutes adds the members routes to the /api/v1/manage group.
func (s *Server) registerMemberRoutes(r chi.Router) {
	r.Get("/members", s.handleMgmtListMembers)
	r.With(s.requireRole(auth.ActionMemberManage)).Post("/members", s.handleMgmtInviteMember)
	r.With(s.requireRole(auth.ActionMemberManage)).Patch("/members/{user}", s.handleMgmtSetMemberRole)
	// A member removes itself (leaves) whatever its role (RB10-1, WP-ACCT-1);
	// removing another is member.manage — the handler checks which.
	r.Delete("/members/{user}", s.handleMgmtRemoveMember)
	r.With(s.requireRole(auth.ActionMemberManage)).Delete("/invitations/{id}", s.handleMgmtRevokeInvitation)
	// POST /invitations/{token}/accept is mounted outside requireMember
	// (registerManagementRoutes): the invitee is a member of no tenant yet.
}

func (s *Server) handleMgmtListMembers(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	members, err := auth.ListMembers(r.Context(), s.db, tenantID)
	if err != nil {
		s.logger.Error("list members", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list members", "")
		return
	}
	invs, err := auth.ListInvitations(r.Context(), s.db, tenantID)
	if err != nil {
		s.logger.Error("list invitations", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list invitations", "")
		return
	}
	now := time.Now()
	items := make([]interface{}, 0, len(members))
	for _, m := range members {
		items = append(items, map[string]any{"object": "member", "user_id": m.UserID, "email": m.Email,
			"role": m.Role, "invited_by": m.InvitedBy, "accepted_at": m.AcceptedAt})
	}
	pending := make([]map[string]any, 0, len(invs))
	for _, inv := range invs {
		pending = append(pending, map[string]any{"object": "invitation", "id": inv.ID, "email": inv.Email,
			"role": inv.Role, "invited_by": inv.InvitedBy, "expires_at": inv.ExpiresAt, "expired": !now.Before(inv.ExpiresAt)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": items, "invitations": pending,
		"has_more": false, "total_count": len(items)})
}

// inviteMember records an invitation and sends its link. The e-mail is
// sent after the row is written; a send that fails is logged (the
// invitation can be revoked and sent again).
func (s *Server) inviteMember(ctx context.Context, tenantID, inviterID, inviterEmail, address string, role auth.Role) (auth.Invitation, error) {
	inv, token, err := auth.CreateInvitation(ctx, s.db, tenantID, inviterID, address, role, time.Now())
	if err != nil {
		return inv, err
	}
	var account string
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(name, ''), email) FROM tenants WHERE id = $1`, tenantID).Scan(&account)
	sender := s.emailSender
	if sender == nil {
		sender = email.NewSender(s.logger)
	}
	html, text, err := email.RenderInvitation(s.baseURL, token, inv.Email, account, inviterEmail, string(inv.Role))
	if err == nil {
		err = sender.Send(ctx, inv.Email, "You are invited to "+account+" on Stored", html, text)
	}
	if err != nil {
		s.logger.Warn("member invitation: the e-mail was not sent", zap.String("invitation", inv.ID), zap.Error(err))
	}
	return inv, nil
}

func (s *Server) handleMgmtInviteMember(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	userID, _ := r.Context().Value(userIDKey).(string)
	inviterEmail, _ := r.Context().Value(emailKey).(string)
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}
	role, ok := auth.ParseRole(req.Role)
	if !ok || role == auth.RoleOwner {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_role", "role is admin, developer or viewer", "role")
		return
	}
	inv, err := s.inviteMember(r.Context(), tenantID, userID, inviterEmail, req.Email, role)
	switch {
	case errors.Is(err, auth.ErrAlreadyMember):
		writeManagementError(w, ErrTypeConflict, "already_a_member", "that address is already a member", "email")
		return
	case errors.Is(err, auth.ErrInvitationRateLimited):
		writeManagementError(w, ErrTypeRateLimit, "invitation_rate_limited",
			"too many invitations: an account holds at most 20 pending and sends at most 50 a day — revoke one, or try tomorrow", "")
		return
	case err != nil && err.Error() == "invalid email address":
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_email", "email must be an e-mail address", "email")
		return
	case err != nil:
		s.logger.Error("invite member", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to invite", "")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"object": "invitation", "id": inv.ID, "email": inv.Email,
		"role": inv.Role, "expires_at": inv.ExpiresAt, "created_at": inv.CreatedAt})
}

// memberChangeError answers the refusals of a role change or a removal.
func memberChangeError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, auth.ErrNotMember):
		writeManagementError(w, ErrTypeNotFound, "member_not_found", "no such member", "user")
	case errors.Is(err, auth.ErrOwnerMember):
		writeManagementError(w, ErrTypeConflict, "owner_is_permanent", "the owner cannot be removed or given another role", "user")
	case errors.Is(err, auth.ErrInviteRole):
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_role", "role is admin, developer or viewer", "role")
	default:
		return false
	}
	return true
}

func (s *Server) handleMgmtSetMemberRole(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	target := chi.URLParam(r, "user")
	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return
	}
	role, _ := auth.ParseRole(req.Role)
	if err := auth.SetMemberRole(r.Context(), s.db, tenantID, target, role); err != nil {
		if !memberChangeError(w, err) {
			s.logger.Error("set member role", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to change the role", "")
		}
		return
	}
	audit.Record(r.Context(), s.db, audit.Entry{UserID: target, TenantID: tenantID, Action: "member.role_changed",
		Resource: "user:" + target, Metadata: map[string]any{"role": string(role)}})
	writeJSON(w, http.StatusOK, map[string]any{"object": "member", "user_id": target, "role": role})
}

// handleMgmtRemoveMember removes a member. The answer names the keys the
// member made (keys_made: they are the tenant's and stay live); a body
// {"revoke_keys": true} revokes them in the same transaction (RP84-4).
func (s *Server) handleMgmtRemoveMember(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	target := chi.URLParam(r, "user")
	self, _ := r.Context().Value(userIDKey).(string)
	leaving := target == self && self != ""
	if role := memberRole(r.Context()); !leaving && !auth.Can(role, auth.ActionMemberManage) {
		writeManagementError(w, ErrTypePermission, "role_forbidden",
			"a member with the role "+string(role)+" may not do this ("+string(auth.ActionMemberManage)+"); a member may remove itself", "")
		return
	}
	var req struct {
		RevokeKeys bool `json:"revoke_keys"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
			return
		}
	}
	res, err := auth.RemoveMember(r.Context(), s.db, tenantID, target, req.RevokeKeys)
	if err != nil {
		if !memberChangeError(w, err) {
			s.logger.Error("remove member", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to remove the member", "")
		}
		return
	}
	if res.KeysRevoked && s.auth != nil {
		s.auth.ForgetRevokedKeys(tenantID, res.KeysMade)
	}
	action := "member.removed"
	if leaving {
		action = "member.left"
	}
	audit.Record(r.Context(), s.db, audit.Entry{UserID: target, TenantID: tenantID, Action: action,
		Resource: "user:" + target, Severity: "warning",
		Metadata: map[string]any{"keys_made": len(res.KeysMade), "keys_revoked": res.KeysRevoked}})
	writeJSON(w, http.StatusOK, map[string]any{"object": "member", "user_id": target, "deleted": true,
		"keys_made": res.KeysMade, "keys_revoked": res.KeysRevoked})
}

func (s *Server) handleMgmtRevokeInvitation(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	id := chi.URLParam(r, "id")
	if err := auth.RevokeInvitation(r.Context(), s.db, tenantID, id); err != nil {
		if errors.Is(err, auth.ErrInvitationNotFound) {
			writeManagementError(w, ErrTypeNotFound, "invitation_not_found", "no such pending invitation", "id")
			return
		}
		s.logger.Error("revoke invitation", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to revoke the invitation", "")
		return
	}
	audit.Record(r.Context(), s.db, audit.Entry{TenantID: tenantID, Action: "member.invitation_revoked", Resource: "invitation:" + id})
	writeJSON(w, http.StatusOK, map[string]any{"object": "invitation", "id": id, "deleted": true})
}

// writeInvitationError answers the refusals of an accept.
func writeInvitationError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, auth.ErrInvitationNotFound):
		writeManagementError(w, ErrTypeNotFound, "invitation_not_found", "no such invitation", "token")
	case errors.Is(err, auth.ErrInvitationExpired):
		writeManagementError(w, ErrTypeGone, "invitation_expired", "the invitation has expired: ask for a new one", "token")
	case errors.Is(err, auth.ErrInvitationUsed):
		writeManagementError(w, ErrTypeGone, "invitation_used", "the invitation has already been used", "token")
	case errors.Is(err, auth.ErrInvitationEmailMismatch):
		writeManagementError(w, ErrTypePermission, "invitation_other_address", "the invitation was sent to another address: sign in as that address", "token")
	default:
		return false
	}
	return true
}

func (s *Server) handleMgmtAcceptInvitation(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value(userIDKey).(string)
	addr, _ := r.Context().Value(emailKey).(string)
	inv, err := auth.AcceptInvitation(r.Context(), s.db, chi.URLParam(r, "token"), userID, addr, time.Now())
	if err != nil {
		if !writeInvitationError(w, err) {
			s.logger.Error("accept invitation", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to accept the invitation", "")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "member", "tenant_id": inv.TenantID, "user_id": userID, "role": inv.Role})
}
