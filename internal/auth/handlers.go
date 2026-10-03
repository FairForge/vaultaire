// Package auth holds the S3 SigV4 verifier, the account/key/session
// services and the STS minting logic.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"
)

// S3 signature constants
const (
	algorithm   = "AWS4-HMAC-SHA256"
	aws4Request = "aws4_request"
	serviceName = "s3"
	timeFormat  = "20060102T150405Z"
	dateFormat  = "20060102"
	maxTimeSkew = 15 * time.Minute
)

// Auth handles S3 authentication
type Auth struct {
	db     *sql.DB
	logger *zap.Logger
}

// NewAuth creates a new Auth handler
func NewAuth(db *sql.DB, logger *zap.Logger) *Auth {
	return &Auth{
		db:     db,
		logger: logger,
	}
}

// ValidateRequest validates an S3 request and returns the tenant ID and key scope.
// With SIGV4_ENFORCE active (the default), AWS4-HMAC-SHA256 requests must carry a
// valid signature; legacy formats that prove only possession of the access key ID
// (SigV2 "AWS ak:sig" and the bare AWSAccessKeyId query parameter) are rejected.
func (a *Auth) ValidateRequest(r *http.Request) (string, *KeyScope, error) {
	fullAccess := &KeyScope{Permissions: []string{"*"}}
	enforce := sigV4Enforced()

	// Extract Authorization header
	authHeader := r.Header.Get("Authorization")

	// If no auth header, check for access key in query (for presigned URLs)
	if authHeader == "" {
		if accessKey := r.URL.Query().Get("AWSAccessKeyId"); accessKey != "" {
			if enforce && a.db != nil {
				return "", nil, fmt.Errorf("%w: unsigned AWSAccessKeyId query authentication is not supported", ErrSignatureMismatch)
			}
			return a.validateAccessKey(r.Context(), accessKey)
		}
		// For testing without auth, allow but use test-tenant
		if a.db == nil {
			return "test-tenant", fullAccess, nil
		}
		return "", nil, fmt.Errorf("missing authorization")
	}

	// AWS Signature v4 format (used by AWS CLI/SDKs)
	if strings.HasPrefix(authHeader, algorithm) {
		params, err := parseSigV4AuthHeader(authHeader)
		if err != nil {
			a.logger.Debug("failed to parse auth header", zap.Error(err))
			return "", nil, err
		}
		if a.db == nil {
			a.logger.Warn("no database connection, using test-tenant")
			return "test-tenant", fullAccess, nil
		}
		cred, err := a.LookupCredential(r.Context(), params.AccessKey)
		if err != nil {
			return "", nil, err
		}
		if enforce {
			// A key whose plaintext secret was never stored (legacy rows with
			// only a bcrypt secret_hash) can never verify: fail closed, but
			// leave an actionable trail — the key must be regenerated.
			if cred.SecretKey == "" {
				a.logger.Warn("access key has no stored secret — cannot verify SigV4 signature; regenerate this API key",
					zap.String("access_key", params.AccessKey[:min(6, len(params.AccessKey))]+"..."),
					zap.String("tenant_id", cred.TenantID))
				return "", nil, fmt.Errorf("%w: key has no stored secret for signature verification; regenerate this API key", ErrSignatureMismatch)
			}
			if err := a.verifySigV4(r, params, cred.SecretKey); err != nil {
				a.logger.Debug("signature verification failed",
					zap.String("access_key", params.AccessKey[:min(6, len(params.AccessKey))]+"..."),
					zap.Error(err))
				return "", nil, err
			}
			// The signature proves the DECLARED payload hash is authentic;
			// wrapping the body makes the received bytes live up to it.
			if err := wrapPayloadVerification(r); err != nil {
				return "", nil, err
			}
		}
		return cred.TenantID, cred.Scope, nil
	}

	// Basic AWS format (SigV2-era clients) — key-existence only, so it is
	// disabled while signatures are enforced.
	if strings.HasPrefix(authHeader, "AWS ") {
		if enforce && a.db != nil {
			return "", nil, fmt.Errorf("%w: AWS signature version 2 is not supported", ErrSignatureMismatch)
		}
		parts := strings.SplitN(strings.TrimPrefix(authHeader, "AWS "), ":", 2)
		if len(parts) == 2 {
			return a.validateAccessKey(r.Context(), parts[0])
		}
	}

	return "", nil, fmt.Errorf("invalid authorization format")
}

// Credential is the result of an access-key lookup: the owning tenant, the
// secret used for signature verification, and the key's scope.
type Credential struct {
	TenantID  string
	SecretKey string
	Scope     *KeyScope
}

// ErrAccessKeyRevoked: the presented access key id exists and is dead — a
// revoked or rotated key, or an STS token whose parent key is (WP-R5-14 /
// WP-R5-5). The S3 layer answers InvalidAccessKeyId and counts it as a
// failure against a KNOWN key: a burst of these after a rotation is
// someone still holding the old pair.
var ErrAccessKeyRevoked = errors.New("access key revoked")

// validateAccessKey looks up the tenant ID and key scope by access key,
// without signature verification (legacy paths and SIGV4_ENFORCE=false).
func (a *Auth) validateAccessKey(ctx context.Context, accessKey string) (string, *KeyScope, error) {
	if a.db == nil {
		a.logger.Warn("no database connection, using test-tenant")
		return "test-tenant", &KeyScope{Permissions: []string{"*"}}, nil
	}
	cred, err := a.LookupCredential(ctx, accessKey)
	if err != nil {
		return "", nil, err
	}
	return cred.TenantID, cred.Scope, nil
}

// LookupCredential resolves an access key to its secret, tenant and scope.
// It is THE credential lookup — the S3 header-auth path, the presigned-URL
// verifier and STS all go through it (R5's invariant: one lookup, one
// revocation check).
//
//   - api_keys by key_id (the primary pair is an is_primary row like any
//     other since WP-R5-14; tenants.access_key is a mirror and is never
//     read here): a revoked row is ErrAccessKeyRevoked, a row with no
//     tenant is refused, expiry is reported in the scope and enforced by
//     the caller.
//   - sts_tokens for ASIA ids, joined to the parent key: a token whose
//     parent is revoked, expired or gone is ErrAccessKeyRevoked (WP-R5-5).
//   - anything else is ErrUnknownAccessKey.
func (a *Auth) LookupCredential(ctx context.Context, accessKey string) (*Credential, error) {
	if a.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	var (
		tenantID, secretKey      string
		permJSON                 []byte
		bucketScope, ipAllowlist pq.StringArray
		expiresAt, revokedAt     sql.NullTime
		isPrimary                bool
	)
	err := a.db.QueryRowContext(ctx, `
		SELECT COALESCE(ak.tenant_id, ''), COALESCE(ak.secret_key, ''),
		       COALESCE(ak.permissions, '["*"]'::jsonb),
		       COALESCE(ak.bucket_scope, '{}'),
		       COALESCE(ak.ip_allowlist, '{}'),
		       ak.expires_at, ak.revoked_at, ak.is_primary
		FROM api_keys ak
		WHERE ak.key_id = $1
	`, accessKey).Scan(&tenantID, &secretKey, &permJSON, &bucketScope, &ipAllowlist, &expiresAt, &revokedAt, &isPrimary)
	switch {
	case err == nil:
		if revokedAt.Valid {
			a.logger.Debug("revoked access key presented", zap.String("tenant_id", tenantID), zap.Bool("primary", isPrimary))
			return nil, ErrAccessKeyRevoked
		}
		if tenantID == "" {
			// A row the backfill could not attach to a tenant: it never
			// authenticates (and never as "default").
			a.logger.Warn("api key row has no tenant — refused")
			return nil, fmt.Errorf("%w: key row has no tenant", ErrUnknownAccessKey)
		}
		scope := &KeyScope{
			BucketScope: []string(bucketScope),
			IPAllowlist: []string(ipAllowlist),
		}
		// A permissions column that is not a JSON array of strings grants
		// NOTHING — it used to grant everything (R5-14).
		if jsonErr := json.Unmarshal(permJSON, &scope.Permissions); jsonErr != nil {
			// Log the DB-derived tenant, never the request-derived key (CodeQL
			// clear-text-logging taint from the Authorization header).
			a.logger.Warn("api key has unparsable permissions — treating as no permissions",
				zap.String("tenant_id", tenantID), zap.Error(jsonErr))
			scope.Permissions = nil
		}
		if expiresAt.Valid {
			scope.ExpiresAt = &expiresAt.Time
		}
		a.logger.Debug("authenticated tenant (api key)",
			zap.String("tenant_id", tenantID),
			zap.Bool("primary", isPrimary),
			zap.Int("permissions", len(scope.Permissions)))
		return &Credential{tenantID, secretKey, scope}, nil
	case !errors.Is(err, sql.ErrNoRows):
		a.logger.Error("database error during auth", zap.Error(err))
		return nil, fmt.Errorf("auth lookup failed: %w", err)
	}

	// STS temporary credential (ASIA prefix keys), bounded by its parent.
	if strings.HasPrefix(accessKey, "ASIA") {
		var (
			stsPermJSON                   []byte
			stsBucketScope, stsIPRestrict pq.StringArray
			stsExpiresAt                  time.Time
			parentFound, parentRevoked    bool
			parentExpiresAt               sql.NullTime
		)
		err = a.db.QueryRowContext(ctx, `
			SELECT s.tenant_id, COALESCE(s.secret_key, ''), s.permissions, s.bucket_scope, s.ip_restrict, s.expires_at,
			       ak.key_id IS NOT NULL, ak.revoked_at IS NOT NULL, ak.expires_at
			FROM sts_tokens s
			LEFT JOIN api_keys ak ON ak.key_id = s.parent_key_id
			WHERE s.access_key = $1
		`, accessKey).Scan(&tenantID, &secretKey, &stsPermJSON, &stsBucketScope, &stsIPRestrict, &stsExpiresAt,
			&parentFound, &parentRevoked, &parentExpiresAt)
		switch {
		case err == nil:
			if time.Now().After(stsExpiresAt) {
				a.logger.Debug("expired STS token", zap.String("tenant_id", tenantID))
				return nil, fmt.Errorf("expired STS token")
			}
			if !parentFound || parentRevoked || (parentExpiresAt.Valid && time.Now().After(parentExpiresAt.Time)) {
				a.logger.Debug("STS token of a dead parent key refused", zap.String("tenant_id", tenantID),
					zap.Bool("parent_found", parentFound), zap.Bool("parent_revoked", parentRevoked))
				return nil, fmt.Errorf("%w: the parent key of this token is revoked", ErrAccessKeyRevoked)
			}
			scope := &KeyScope{
				BucketScope: []string(stsBucketScope),
				IPAllowlist: []string(stsIPRestrict),
				ExpiresAt:   &stsExpiresAt,
				Temporary:   true,
			}
			if jsonErr := json.Unmarshal(stsPermJSON, &scope.Permissions); jsonErr != nil {
				a.logger.Warn("sts token has unparsable permissions — treating as no permissions",
					zap.String("tenant_id", tenantID), zap.Error(jsonErr))
				scope.Permissions = nil
			}
			a.logger.Debug("authenticated tenant (STS token)", zap.String("tenant_id", tenantID))
			return &Credential{tenantID, secretKey, scope}, nil
		case !errors.Is(err, sql.ErrNoRows):
			a.logger.Error("database error during STS auth", zap.Error(err))
			return nil, fmt.Errorf("auth lookup failed: %w", err)
		}
	}

	// Never log the request-derived key, even truncated (CodeQL
	// go/clear-text-logging on the Authorization header); the metric
	// carries the signal (vaultaire_auth_failures_total{reason="unknown_access_key"}).
	a.logger.Debug("invalid access key")
	return nil, ErrUnknownAccessKey
}

func (a *Auth) createStringToSign(amzDate, scope, canonicalRequest string) string {
	hash := sha256.Sum256([]byte(canonicalRequest))
	return strings.Join([]string{
		algorithm,
		amzDate,
		scope,
		hex.EncodeToString(hash[:]),
	}, "\n")
}

func (a *Auth) deriveSigningKey(secretKey, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte(aws4Request))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}
