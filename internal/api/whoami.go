package api

// GET /api/v1/whoami — what a key is, told to the holder of the key
// (docs/STATUS.md queue item 3, 2026-10-06; plan 42.7 "the missing piece").
//
// An edge gateway (the Workers Cache gateway of 40.1(a), any app that holds a
// customer's S3 key and never a JWT) verifies a key today with one signed
// `bytes=0-0` GET against the origin and can only partition its cache per
// KEY. This call answers, for a SigV4-signed request: the tenant, the key id,
// whether it is the primary pair, a scoped key or a temporary STS token, its
// permissions, bucket scope, IP allowlist and expiry — and never a secret.
// Authentication is the S3 path's (header or presigned SigV4, the same
// LookupCredential, the same expiry / IP allowlist / suspension gates, the
// same failure metrics), the envelope is the management API's JSON.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

// registerWhoamiRoute mounts the call on the production router, before the
// S3 catch-all (server.go). GET only.
func (s *Server) registerWhoamiRoute() {
	s.router.Get("/api/v1/whoami", s.handleWhoami)
}

type whoamiResponse struct {
	Object      string   `json:"object"`
	TenantID    string   `json:"tenant_id"`
	KeyID       string   `json:"key_id"`
	KeyType     string   `json:"key_type"` // primary | scoped | sts
	KeyName     string   `json:"key_name"`
	Permissions []string `json:"permissions"`
	BucketScope []string `json:"bucket_scope"`
	IPAllowlist []string `json:"ip_allowlist"`
	ExpiresAt   *string  `json:"expires_at"`
	Temporary   bool     `json:"temporary"`
	RequestID   string   `json:"request_id"`
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	reqID := generateRequestID()
	w.Header().Set("Cache-Control", "no-store")

	var (
		tenantID string
		scope    *auth.KeyScope
		err      error
	)
	switch {
	case s.testMode:
		if t, tErr := tenant.FromContext(r.Context()); tErr == nil && t != nil {
			tenantID = t.ID
		} else {
			tenantID = "test"
		}
		scope = &auth.KeyScope{Permissions: []string{"*"}}
	case isPresignedRequest(r):
		tenantID, scope, err = s.verifyPresignedURL(r)
		if err != nil {
			reason, known := presignFailureReason(err)
			if known {
				known = s.accessKeyExists(r.Context(), auth.AccessKeyFromRequest(r))
			}
			recordAuthFailure(r, reason, known)
			writeManagementError(w, ErrTypeAuthentication, whoamiAuthCode(err), "the request is not signed with a valid key", reqID)
			return
		}
	default:
		a := auth.NewAuth(s.db, s.logger)
		a.MissingSignedHeader = s.missingSignedHeaderHint
		tenantID, scope, err = a.ValidateRequest(r)
		if err != nil {
			reason, known := authFailureReason(err)
			recordAuthFailure(r, reason, known)
			writeManagementError(w, ErrTypeAuthentication, whoamiAuthCode(err), "the request is not signed with a valid key", reqID)
			return
		}
	}

	if tenantID != "" && engine.IsReservedTenantID(tenantID) {
		writeManagementError(w, ErrTypePermission, "reserved_tenant", "the credential resolves to a reserved tenant id", reqID)
		return
	}
	if scope != nil {
		if auth.IsKeyExpired(scope.ExpiresAt) {
			recordAuthFailure(r, "expired", true)
			writeManagementError(w, ErrTypeAuthentication, "key_expired",
				"this access key expired at "+scope.ExpiresAt.UTC().Format(time.RFC3339), reqID)
			return
		}
		if !auth.CheckIPAllowlist(scope.IPAllowlist, extractClientIP(r)) {
			recordAuthFailure(r, "ip_denied", true)
			writeManagementError(w, ErrTypePermission, "ip_denied", "this key is restricted by IP address", reqID)
			return
		}
	}
	if s.db != nil && !s.testMode && isTenantSuspended(r.Context(), s.db, tenantID) {
		writeManagementError(w, ErrTypePermission, "account_suspended", "this account is suspended", reqID)
		return
	}

	keyID := auth.AccessKeyFromRequest(r)
	resp := whoamiResponse{
		Object:      "whoami",
		TenantID:    tenantID,
		KeyID:       keyID,
		KeyType:     "scoped",
		Permissions: []string{},
		BucketScope: []string{},
		IPAllowlist: []string{},
		RequestID:   reqID,
	}
	if scope != nil {
		if len(scope.Permissions) > 0 {
			resp.Permissions = append(resp.Permissions, scope.Permissions...)
		}
		if len(scope.BucketScope) > 0 {
			resp.BucketScope = append(resp.BucketScope, scope.BucketScope...)
		}
		if len(scope.IPAllowlist) > 0 {
			resp.IPAllowlist = append(resp.IPAllowlist, scope.IPAllowlist...)
		}
		if scope.ExpiresAt != nil {
			exp := scope.ExpiresAt.UTC().Format(time.RFC3339)
			resp.ExpiresAt = &exp
		}
		resp.Temporary = scope.Temporary
	}
	switch {
	case scope != nil && scope.Temporary:
		resp.KeyType = "sts"
	case s.db != nil && keyID != "":
		var name string
		var primary bool
		qErr := s.db.QueryRowContext(r.Context(),
			`SELECT COALESCE(name, ''), is_primary FROM api_keys WHERE key_id = $1 AND tenant_id = $2`,
			keyID, tenantID).Scan(&name, &primary)
		switch {
		case qErr == nil:
			resp.KeyName = name
			if primary {
				resp.KeyType = "primary"
			}
		case errors.Is(qErr, sql.ErrNoRows):
			// A credential the lookup accepted and the table does not name
			// under this tenant: report it as scoped, say nothing more.
		default:
			s.logger.Warn("whoami: key row lookup failed", zap.Error(qErr))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// whoamiAuthCode: the management-envelope code for an S3 auth failure.
func whoamiAuthCode(err error) string {
	switch {
	case errors.Is(err, auth.ErrAccessKeyRevoked):
		return "key_revoked"
	case errors.Is(err, auth.ErrSignatureMismatch):
		return "signature_mismatch"
	case errors.Is(err, auth.ErrRequestTimeSkewed):
		return "request_time_skewed"
	}
	switch err.Error() {
	case ErrExpiredPresignedRequest:
		return "presigned_url_expired"
	case ErrSignatureDoesNotMatch:
		return "signature_mismatch"
	case ErrRequestTimeTooSkewed:
		return "request_time_skewed"
	case ErrInvalidAccessKeyId:
		return "key_revoked"
	}
	if strings.Contains(err.Error(), "revoked") {
		return "key_revoked"
	}
	return "invalid_credentials"
}
