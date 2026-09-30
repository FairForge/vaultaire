// Package auth holds the S3 SigV4 verifier, the account/key/session
// services and the STS minting logic.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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
			return a.validateAccessKey(accessKey)
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
		cred, err := a.lookupCredential(params.AccessKey)
		if err != nil {
			return "", nil, err
		}
		if enforce {
			// A key whose plaintext secret was never stored (legacy rows with
			// only a bcrypt secret_hash) can never verify: fail closed, but
			// leave an actionable trail — the key must be regenerated.
			if cred.secretKey == "" {
				a.logger.Warn("access key has no stored secret — cannot verify SigV4 signature; regenerate this API key",
					zap.String("access_key", params.AccessKey[:min(6, len(params.AccessKey))]+"..."),
					zap.String("tenant_id", cred.tenantID))
				return "", nil, fmt.Errorf("%w: key has no stored secret for signature verification; regenerate this API key", ErrSignatureMismatch)
			}
			if err := a.verifySigV4(r, params, cred.secretKey); err != nil {
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
		return cred.tenantID, cred.scope, nil
	}

	// Basic AWS format (SigV2-era clients) — key-existence only, so it is
	// disabled while signatures are enforced.
	if strings.HasPrefix(authHeader, "AWS ") {
		if enforce && a.db != nil {
			return "", nil, fmt.Errorf("%w: AWS signature version 2 is not supported", ErrSignatureMismatch)
		}
		parts := strings.SplitN(strings.TrimPrefix(authHeader, "AWS "), ":", 2)
		if len(parts) == 2 {
			return a.validateAccessKey(parts[0])
		}
	}

	return "", nil, fmt.Errorf("invalid authorization format")
}

// credential is the result of an access-key lookup: the owning tenant, the
// secret used for signature verification, and the key's scope.
type credential struct {
	tenantID  string
	secretKey string
	scope     *KeyScope
}

// validateAccessKey looks up the tenant ID and key scope by access key,
// without signature verification (legacy paths and SIGV4_ENFORCE=false).
func (a *Auth) validateAccessKey(accessKey string) (string, *KeyScope, error) {
	if a.db == nil {
		a.logger.Warn("no database connection, using test-tenant")
		return "test-tenant", &KeyScope{Permissions: []string{"*"}}, nil
	}
	cred, err := a.lookupCredential(accessKey)
	if err != nil {
		return "", nil, err
	}
	return cred.tenantID, cred.scope, nil
}

// lookupCredential resolves an access key to its secret, tenant and scope.
// Checks the tenants table first (primary keys, full access), then falls
// back to api_keys for scoped VLT_ keys, then sts_tokens for ASIA keys.
func (a *Auth) lookupCredential(accessKey string) (*credential, error) {
	if a.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	// Try primary tenant key first (full access).
	var tenantID, secretKey string
	// COALESCE: a NULL secret_key must resolve to the empty-secret fail-closed
	// path ("regenerate this API key"), not a scan error that hard-locks the
	// tenant out even under SIGV4_ENFORCE=false.
	err := a.db.QueryRow(`SELECT id, COALESCE(secret_key, '') FROM tenants WHERE access_key = $1`, accessKey).
		Scan(&tenantID, &secretKey)
	if err == nil {
		a.logger.Debug("authenticated tenant (primary key)",
			zap.String("tenant_id", tenantID),
			zap.String("access_key", accessKey[:min(6, len(accessKey))]+"..."))
		return &credential{tenantID, secretKey, &KeyScope{Permissions: []string{"*"}}}, nil
	}
	if err != sql.ErrNoRows {
		a.logger.Error("database error during auth", zap.Error(err))
		return nil, fmt.Errorf("auth lookup failed: %w", err)
	}

	// Try scoped API key (VLT_ prefix keys from api_keys table).
	var permJSON []byte
	var bucketScope, ipAllowlist pq.StringArray
	var expiresAt sql.NullTime
	err = a.db.QueryRow(`
		SELECT t.id, COALESCE(ak.secret_key, ''),
		       COALESCE(ak.permissions, '["*"]'::jsonb),
		       COALESCE(ak.bucket_scope, '{}'),
		       COALESCE(ak.ip_allowlist, '{}'),
		       ak.expires_at
		FROM api_keys ak
		JOIN users u ON u.id = ak.user_id
		JOIN tenants t ON t.email = u.email
		WHERE ak.key_id = $1 AND ak.revoked_at IS NULL
	`, accessKey).Scan(&tenantID, &secretKey, &permJSON, &bucketScope, &ipAllowlist, &expiresAt)
	if err == nil {
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

		a.logger.Debug("authenticated tenant (scoped key)",
			zap.String("tenant_id", tenantID),
			zap.String("access_key", accessKey[:min(6, len(accessKey))]+"..."),
			zap.Int("permissions", len(scope.Permissions)))
		return &credential{tenantID, secretKey, scope}, nil
	}
	if err != sql.ErrNoRows {
		a.logger.Error("database error during scoped key auth", zap.Error(err))
		return nil, fmt.Errorf("auth lookup failed: %w", err)
	}

	// Try STS temporary credential (ASIA prefix keys).
	if len(accessKey) >= 4 && accessKey[:4] == "ASIA" {
		var stsPermJSON []byte
		var stsBucketScope, stsIPRestrict pq.StringArray
		var stsExpiresAt time.Time
		err = a.db.QueryRow(`
			SELECT tenant_id, COALESCE(secret_key, ''), permissions, bucket_scope, ip_restrict, expires_at
			FROM sts_tokens WHERE access_key = $1
		`, accessKey).Scan(&tenantID, &secretKey, &stsPermJSON, &stsBucketScope, &stsIPRestrict, &stsExpiresAt)
		if err == nil {
			if time.Now().After(stsExpiresAt) {
				a.logger.Debug("expired STS token", zap.String("access_key", accessKey[:min(6, len(accessKey))]+"..."))
				return nil, fmt.Errorf("expired STS token")
			}
			scope := &KeyScope{
				BucketScope: []string(stsBucketScope),
				IPAllowlist: []string(stsIPRestrict),
				ExpiresAt:   &stsExpiresAt,
			}
			if jsonErr := json.Unmarshal(stsPermJSON, &scope.Permissions); jsonErr != nil {
				a.logger.Warn("sts token has unparsable permissions — treating as no permissions",
					zap.String("tenant_id", tenantID), zap.Error(jsonErr))
				scope.Permissions = nil
			}
			a.logger.Debug("authenticated tenant (STS token)",
				zap.String("tenant_id", tenantID),
				zap.String("access_key", accessKey[:min(6, len(accessKey))]+"..."))
			return &credential{tenantID, secretKey, scope}, nil
		}
		if err != sql.ErrNoRows {
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
