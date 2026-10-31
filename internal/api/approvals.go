package api

// Approvals (WP-AGENT-1 PR B; docs/ACCOUNTS.md § Agents; F-41). An agent
// grant's key that reaches an act outside the default agent scope gets 403
// approval_required with an approval link; the person decides it on the
// dashboard or here (a person only; the role the act itself needs; a fresh
// TOTP code for a destructive act); an approval names ONE action on ONE
// resource and is used ONCE within 24 h. A server's management key and a
// person are never asked: the gate is the agent's.
//
// And the agent never reaches a Private bucket (contract line 8): a list
// leaves it out, every bucket route answers 403 private_bucket, and an S3
// key the agent minted is refused by the S3 path the same way.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/hosts"
	"github.com/FairForge/vaultaire/internal/modes"
)

func (s *Server) registerApprovalRoutes(r chi.Router) {
	r.Get("/approvals", s.handleMgmtListApprovals)
	// A viewer decides nothing; the act's own role is checked per approval.
	r.With(s.requirePerson, s.requireRole(auth.ActionBucketWrite)).Post("/approvals/{id}/approve", s.handleMgmtDecideApproval("approved"))
	r.With(s.requirePerson, s.requireRole(auth.ActionBucketWrite)).Post("/approvals/{id}/deny", s.handleMgmtDecideApproval("denied"))
}

// approvalTenant is where a grant's approvals live: the account, also for
// its sandbox test key (the person decides them on the account).
func approvalTenant(a *auth.ManagementKey) string {
	if a.SandboxOf != "" {
		return a.SandboxOf
	}
	return a.TenantID
}

// approvalResource names the resource on the grant's tenant: a sandbox's
// is prefixed, so an approval on the account never spends on the sandbox.
func approvalResource(a *auth.ManagementKey, resource string) string {
	if a.SandboxOf != "" {
		return "sandbox/" + resource
	}
	return resource
}

// askApproval spends an approval of action on resource when the request is
// an agent's (true = go on), else answers 403 approval_required naming the
// pending approval (false).
func (s *Server) askApproval(w http.ResponseWriter, r *http.Request, action, resource string) bool {
	a := agentOf(r.Context())
	if a == nil {
		return true
	}
	if s.db == nil {
		writeManagementError(w, ErrTypeAPI, "no_database", "approvals need the database", "")
		return false
	}
	res := approvalResource(a, resource)
	id, ok, err := auth.ConsumeApproval(r.Context(), s.db, a.AgentGrantID, action, res)
	if err != nil {
		s.logger.Error("spend an approval", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "the approval could not be read", "")
		return false
	}
	if ok {
		audit.Record(r.Context(), s.db, audit.Entry{TenantID: a.TenantID, Action: "agent.approval_used", Resource: res,
			Metadata: map[string]any{"approval_id": id, "action": action}})
		return true
	}
	ap, err := auth.RequestApproval(r.Context(), s.db, approvalTenant(a), a.AgentGrantID, action, res)
	if err != nil {
		s.logger.Error("request an approval", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "the approval could not be asked for", "")
		return false
	}
	url := hosts.Get().URL("/dashboard/agents?approval=" + ap.ID)
	writeJSON(w, http.StatusForbidden, managementError{Error: managementErrorBody{
		Type: ErrTypePermission, Code: "approval_required",
		Message: "an agent needs the account holder's approval to " + auth.ApprovalActions[action].Says +
			": show them approval_url; once they approve, the same request succeeds once (within 24 hours)",
		RequestID: getRequestID(w), ApprovalID: ap.ID, ApprovalURL: url,
	}})
	noteS3Error(w, "approval_required", getRequestID(w))
	return false
}

// requireApproval gates a route by an approval of action on the resource
// resource(r) names.
func (s *Server) requireApproval(action string, resource func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s.askApproval(w, r, action, resource(r)) {
				next.ServeHTTP(w, r)
			}
		})
	}
}

func bucketResource(r *http.Request) string { return "bucket:" + chi.URLParam(r, "name") }

// agentKeyRevoke lets an agent revoke an S3 key its grant minted; any
// other key asks for approval.
func (s *Server) agentKeyRevoke(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := agentOf(r.Context())
		if a == nil || s.db == nil {
			next.ServeHTTP(w, r)
			return
		}
		id := chi.URLParam(r, "id")
		var own bool
		err := s.db.QueryRowContext(r.Context(), `SELECT agent_grant_id IS NOT DISTINCT FROM $2::uuid FROM api_keys
			 WHERE id::text = $1 AND tenant_id = $3`, id, a.AgentGrantID, activeTenant(r)).Scan(&own)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.logger.Error("agent key revoke: read the key", zap.Error(err))
		}
		if own || s.askApproval(w, r, "key.revoke", "key:"+id) {
			next.ServeHTTP(w, r)
		}
	})
}

// bucketIsPrivate reports whether tenantID's bucket is in Private mode.
func (s *Server) bucketIsPrivate(ctx context.Context, tenantID, bucket string) bool {
	if s.db == nil || bucket == "" {
		return false
	}
	var mode string
	if err := s.db.QueryRowContext(ctx, `SELECT mode FROM buckets WHERE tenant_id = $1 AND name = $2`, tenantID, bucket).Scan(&mode); err != nil {
		return false
	}
	return mode == modes.Private
}

// agentNeverPrivate refuses an agent's request on a Private bucket's route
// (contract line 8): 403 private_bucket, before the handler.
func (s *Server) agentNeverPrivate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if agentOf(r.Context()) != nil && s.bucketIsPrivate(r.Context(), activeTenant(r), chi.URLParam(r, "name")) {
			writePrivateBucketRefusal(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writePrivateBucketRefusal(w http.ResponseWriter) {
	writeManagementError(w, ErrTypePermission, "private_bucket",
		"an agent never reaches a Private bucket: only its owner's own devices read it", "name")
}

// approvalJSON is an approval as the API answers it.
func approvalJSON(a *auth.Approval) map[string]any {
	return map[string]any{"object": "approval", "id": a.ID, "grant_id": a.GrantID, "agent_name": a.AgentName, "action": a.Action,
		"resource": a.Resource, "says": a.Says, "destructive": a.Destructive, "requested_at": a.RequestedAt, "expires_at": a.ExpiresAt,
		"decided_at": a.DecidedAt, "decided_by": a.DecidedBy, "decision": a.Decision, "used_at": a.UsedAt}
}

// handleMgmtListApprovals is GET /api/v1/manage/approvals (?pending=1).
func (s *Server) handleMgmtListApprovals(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.agentsOr404(w, r)
	if !ok {
		return
	}
	list, err := auth.ListApprovals(r.Context(), s.db, tenantID, r.URL.Query().Get("pending") != "")
	if err != nil {
		s.logger.Error("list approvals", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list the approvals", "")
		return
	}
	data := make([]map[string]any, 0, len(list))
	for _, a := range list {
		data = append(data, approvalJSON(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data, "request_id": getRequestID(w)})
}

// handleMgmtDecideApproval is POST /api/v1/manage/approvals/{id}/approve
// ({"code": "<TOTP>"} for a destructive act) and …/deny.
func (s *Server) handleMgmtDecideApproval(decision string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID, ok := s.agentsOr404(w, r)
		if !ok {
			return
		}
		userID, _ := r.Context().Value(userIDKey).(string)
		id := chi.URLParam(r, "id")
		ap, err := auth.GetApproval(r.Context(), s.db, tenantID, id)
		if errors.Is(err, auth.ErrApprovalNotFound) {
			writeManagementError(w, ErrTypeNotFound, "approval_not_found", "no approval of this account has that id", "id")
			return
		}
		if err != nil {
			s.logger.Error("read an approval", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to read the approval", "")
			return
		}
		act := auth.ApprovalActions[ap.Action]
		if ap.DecidedAt != nil { // before any code is spent on it
			writeManagementError(w, ErrTypeConflict, "approval_decided", "the approval was decided already", "id")
			return
		}
		if decision == "approved" {
			role, _ := r.Context().Value(roleKey).(auth.Role)
			if role == "" {
				role = auth.RoleOwner
			}
			if act.Role != "" && !auth.Can(role, act.Role) {
				writeManagementError(w, ErrTypePermission, "role_forbidden", "your role may not "+act.Says+" yourself, so it may not approve an agent doing it", "")
				return
			}
			if act.Destructive && !s.freshTOTP(r, userID) {
				writeManagementError(w, ErrTypePermission, "totp_required",
					"approving a destructive act needs the current code of your authenticator app: {\"code\": \"123456\"}", "code")
				return
			}
		}
		ap, err = auth.DecideApproval(r.Context(), s.db, tenantID, id, userID, decision)
		switch {
		case errors.Is(err, auth.ErrApprovalDecided):
			writeManagementError(w, ErrTypeConflict, "approval_decided", "the approval was decided already", "id")
			return
		case errors.Is(err, auth.ErrApprovalExpired):
			writeManagementError(w, ErrTypeConflict, "approval_expired", "the approval's 24 hours have passed: the agent asks again", "id")
			return
		case err != nil:
			s.logger.Error("decide an approval", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to decide the approval", "")
			return
		}
		audit.Record(r.Context(), s.db, audit.Entry{TenantID: tenantID, Action: "agent.approval_" + decision,
			Resource: ap.Resource, Metadata: map[string]any{"approval_id": ap.ID, "action": ap.Action, "grant_id": ap.GrantID}})
		out := approvalJSON(ap)
		out["request_id"] = getRequestID(w)
		writeJSON(w, http.StatusOK, out)
	}
}

// freshTOTP reports whether the body's "code" is the person's current
// TOTP code, accepted once.
func (s *Server) freshTOTP(r *http.Request, userID string) bool {
	var body struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<10)).Decode(&body)
	code := strings.TrimSpace(body.Code)
	if code == "" || s.auth == nil || s.mfaService == nil || userID == "" {
		return false
	}
	secret, err := s.auth.GetMFASecret(r.Context(), userID)
	return err == nil && secret != "" && s.mfaService.ValidateCode(secret, code) && s.auth.ConsumeTOTPCode(userID, code)
}

// agentS3PrivateRefused: the request's bucket, or its copy source's, is
// Private (the S3 path of an agent-minted key).
func (s *Server) agentS3PrivateRefused(r *http.Request, tenantID string, req *S3Request) bool {
	if s.bucketIsPrivate(r.Context(), tenantID, req.Bucket) {
		return true
	}
	if src := r.Header.Get("x-amz-copy-source"); src != "" {
		src = strings.TrimPrefix(src, "/")
		if i := strings.IndexByte(src, '/'); i > 0 {
			return s.bucketIsPrivate(r.Context(), tenantID, src[:i])
		}
	}
	return false
}
