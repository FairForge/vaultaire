package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/prometheus/client_golang/prometheus"
)

// Credential-attack signal (Review R11-10, pre-launch checklist item 3).
//
// Every rejected S3 authentication increments
// vaultaire_auth_failures_total{reason,key_known}. key_known="true" means
// the presented access key id EXISTS (tenant key, VLT_ key or STS token):
// that is the series a stuffing attempt against a real customer key moves,
// while scanner noise stays on key_known="false". For known keys a second
// series, vaultaire_auth_failures_by_key_total{key_hash}, is keyed by the
// first 8 hex of sha256(access key id) — bounded by the number of real
// keys, and the id itself is never a label or a log field. Unknown ids are
// deliberately NOT hashed (unbounded cardinality). Rules:
// deploy/monitoring/vaultaire-auth.yml.
var (
	authFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_auth_failures_total",
		Help: "Rejected S3 authentications by reason and whether the access key id exists.",
	}, []string{"reason", "key_known"})
	authFailuresByKey = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_auth_failures_by_key_total",
		Help: "Rejected S3 authentications against access keys that exist, by sha256(key id)[:8].",
	}, []string{"key_hash"})
)

// authFailureReason classifies a header-auth failure from auth.ValidateRequest.
func authFailureReason(err error) (reason string, keyKnown bool) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	switch {
	case errors.Is(err, auth.ErrUnknownAccessKey):
		return "unknown_access_key", false
	case errors.Is(err, auth.ErrSignatureMismatch):
		return "signature_mismatch", true
	case errors.Is(err, auth.ErrRequestTimeSkewed):
		return "time_skewed", true
	case errors.Is(err, auth.ErrInvalidContentSHA256):
		return "invalid_content_sha256", true
	case strings.Contains(msg, "missing authorization"):
		return "missing_authorization", false
	case strings.Contains(msg, "expired STS token"):
		return "expired", true
	case strings.Contains(msg, "auth lookup failed"), strings.Contains(msg, "database not initialized"):
		return "lookup_error", false
	default:
		return "malformed", false
	}
}

// presignFailureReason classifies a presigned-URL failure by its S3 code.
// mayBeKnown says the failure is one a real key can produce; whether the id
// actually exists is decided by accessKeyExists — verifyPresignedURL
// answers Expired / TooSkewed BEFORE its credential lookup, so the code
// alone cannot vouch for the key (post-merge R11-28: it used to, and every
// random X-Amz-Credential with a stale date minted a new key_hash series).
func presignFailureReason(code string) (reason string, mayBeKnown bool) {
	switch code {
	case ErrExpiredPresignedRequest:
		return "presign_expired", true
	case ErrSignatureDoesNotMatch:
		return "presign_signature_mismatch", true
	case ErrRequestTimeTooSkewed:
		return "presign_time_skewed", true
	case ErrAuthorizationQueryParametersError, ErrInvalidPresignExpires:
		return "presign_malformed", false
	default:
		return "presign_unknown_key", false
	}
}

// accessKeyExists reports whether some credential of ours has this access
// key id (tenant primary key, live VLT_ key or STS token). It is the gate
// before a per-key metric series: only ids that exist may be hashed into
// one, or the label set is attacker-controlled.
func (s *Server) accessKeyExists(ctx context.Context, accessKey string) bool {
	if s.db == nil || accessKey == "" {
		return false
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM tenants WHERE access_key = $1)
		    OR EXISTS(SELECT 1 FROM api_keys WHERE key_id = $1 AND revoked_at IS NULL)
		    OR EXISTS(SELECT 1 FROM sts_tokens WHERE access_key = $1)`, accessKey).Scan(&exists); err != nil {
		return false
	}
	return exists
}

// recordAuthFailure increments the counters for one rejected request.
func recordAuthFailure(r *http.Request, reason string, keyKnown bool) {
	known := "false"
	if keyKnown {
		known = "true"
	}
	authFailures.WithLabelValues(reason, known).Inc()
	if keyKnown {
		if ak := auth.AccessKeyFromRequest(r); ak != "" {
			authFailuresByKey.WithLabelValues(accessKeyHash(ak)).Inc()
		}
	}
}

// accessKeyHash is the bounded, non-reversible label for a real key id.
func accessKeyHash(accessKey string) string {
	sum := sha256.Sum256([]byte(accessKey))
	return hex.EncodeToString(sum[:4])
}
