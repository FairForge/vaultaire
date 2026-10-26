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

	// algorithmV4A is SigV4A, the multi-region ECDSA scheme of the AWS SDKs
	// (RP58-1): a request that carries it carries S3 credentials — the S3
	// API's to refuse (this verifier does not support it), never a web page's.
	algorithmV4A = "AWS4-ECDSA-P256-SHA256"
)

// IsAWSAuthorization reports whether an Authorization header value is an
// AWS credential scheme — SigV4, SigV4A or SigV2 — by its scheme token (what
// precedes the first space; case-insensitive). It is the ONE predicate of
// "this header is S3 credentials" (Architecture Decision 8): the S3
// credential gate (internal/api signedS3Request) sends every such request
// to the S3 API before any route, and ValidateRequest attempts to parse a
// header only when it is one (wp_p41_predicate_agreement_test). A scheme
// with no space after it is no AWS header to either.
func IsAWSAuthorization(header string) bool {
	// No trimming: net/http has already trimmed a wire header's OWS, and
	// the verifier's HasPrefix sees the value as given — the two must read
	// the same bytes.
	scheme, _, ok := strings.Cut(header, " ")
	if !ok {
		return false
	}
	return strings.EqualFold(scheme, algorithm) || strings.EqualFold(scheme, algorithmV4A) || strings.EqualFold(scheme, "AWS")
}

// AttemptsAWSHeader reports whether ValidateRequest would attempt to parse
// this Authorization header as an AWS scheme (SigV4 or SigV2) — the
// verifier's side of the predicate-agreement invariant: every header it
// attempts must be one IsAWSAuthorization names (tests).
func AttemptsAWSHeader(header string) bool {
	return strings.HasPrefix(header, algorithm+" ") || strings.HasPrefix(header, "AWS ")
}

// Auth handles S3 authentication
type Auth struct {
	db     *sql.DB
	logger *zap.Logger
	// keys opens wrapped secrets (plan 43.7); nil = no master key, and a
	// wrapped row is then refused as ours (openStoredSecret).
	keys CredentialKeys

	// MissingSignedHeader supplies the value a client most plausibly signed
	// for a header that is in SignedHeaders but absent from the request —
	// the verifier's second proxy-rewrite variant after Accept-Encoding. The
	// one case today: R2 Sippy's multi-part pull signs `if-match: "<etag>"`
	// and sends no If-Match header, and the server knows the object's ETag.
	// The hint is a candidate for the signature check, never a bypass — the
	// signature must still verify with the secret over the request as
	// received plus that value. Nil = no hint. Set by the S3 layer.
	MissingSignedHeader func(r *http.Request, tenantID, name string) (string, bool)
}

// NewAuth creates a new Auth handler. keys is the tenant key store's
// credential seam (nil without a master key); the api builds every Auth in
// one place, Server.s3Auth (TestNewAuth_EveryCallSiteHasTheOpener).
func NewAuth(db *sql.DB, logger *zap.Logger, keys CredentialKeys) *Auth {
	return &Auth{
		db:     db,
		logger: logger,
		keys:   keys,
	}
}

// ValidateRequest authenticates an S3 request and returns the tenant ID and
// key scope. AWS Signature V4 is the only way in (WP-R5-4): the request must
// carry an AWS4-HMAC-SHA256 Authorization header whose signature verifies
// against the key's stored secret. The formats that proved only possession
// of the access key ID — SigV2 "AWS ak:sig" and the bare AWSAccessKeyId
// query parameter — answer ErrSignatureMismatch, and there is no switch that
// turns verification off (SIGV4_ENFORCE=false used to make one env typo
// into key-existence auth for every tenant).
//
// Without a database (local development, unit tests) every request is the
// "test-tenant" with full access.
func (a *Auth) ValidateRequest(r *http.Request) (string, *KeyScope, error) {
	// The live entrance of the one verifier (described.go, WP-EDGE-1 Part
	// A): the request itself is what verifySigV4 canonicalises — a
	// descriptor built from it would be the same bytes — and its body is
	// then bound to the hash the signature covered.
	tenantID, scope, err := a.validateDescribed(r, clientIPOf(r))
	if err != nil {
		return "", nil, err
	}
	if a.db == nil {
		return tenantID, scope, nil
	}
	// The signature proves the DECLARED payload hash is authentic; wrapping
	// the body makes the received bytes live up to it.
	if err := wrapPayloadVerification(r); err != nil {
		return "", nil, err
	}
	return tenantID, scope, nil
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

// ErrSTSTokenExpired: the presented id is an STS token past its expiry. The
// text is the one callers matched before it was typed ("expired STS token").
var ErrSTSTokenExpired = errors.New("expired STS token")

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
		wrapped                  []byte
		wrapVersion              sql.NullInt64
		permJSON                 []byte
		bucketScope, ipAllowlist pq.StringArray
		expiresAt, revokedAt     sql.NullTime
		isPrimary                bool
	)
	err := a.db.QueryRowContext(ctx, `
		SELECT COALESCE(ak.tenant_id, ''), COALESCE(ak.secret_key, ''), ak.secret_wrapped, ak.secret_key_version,
		       COALESCE(ak.permissions, '["*"]'::jsonb),
		       COALESCE(ak.bucket_scope, '{}'),
		       COALESCE(ak.ip_allowlist, '{}'),
		       ak.expires_at, ak.revoked_at, ak.is_primary
		FROM api_keys ak
		WHERE ak.key_id = $1
	`, accessKey).Scan(&tenantID, &secretKey, &wrapped, &wrapVersion, &permJSON, &bucketScope, &ipAllowlist, &expiresAt, &revokedAt, &isPrimary)
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
		secret, err := openStoredSecret(ctx, a.keys, a.logger, "api_keys", tenantID, accessKey, secretKey, wrapped, wrapVersion)
		if err != nil {
			return nil, err
		}
		a.logger.Debug("authenticated tenant (api key)",
			zap.String("tenant_id", tenantID),
			zap.Bool("primary", isPrimary),
			zap.Int("permissions", len(scope.Permissions)))
		return &Credential{tenantID, secret, scope}, nil
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
			SELECT s.tenant_id, COALESCE(s.secret_key, ''), s.secret_wrapped, s.secret_key_version,
			       s.permissions, s.bucket_scope, s.ip_restrict, s.expires_at,
			       ak.key_id IS NOT NULL, ak.revoked_at IS NOT NULL, ak.expires_at
			FROM sts_tokens s
			LEFT JOIN api_keys ak ON ak.key_id = s.parent_key_id
			WHERE s.access_key = $1
		`, accessKey).Scan(&tenantID, &secretKey, &wrapped, &wrapVersion, &stsPermJSON, &stsBucketScope, &stsIPRestrict, &stsExpiresAt,
			&parentFound, &parentRevoked, &parentExpiresAt)
		switch {
		case err == nil:
			if time.Now().After(stsExpiresAt) {
				a.logger.Debug("expired STS token", zap.String("tenant_id", tenantID))
				return nil, ErrSTSTokenExpired
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
			secret, err := openStoredSecret(ctx, a.keys, a.logger, "sts_tokens", tenantID, accessKey, secretKey, wrapped, wrapVersion)
			if err != nil {
				return nil, err
			}
			a.logger.Debug("authenticated tenant (STS token)", zap.String("tenant_id", tenantID))
			return &Credential{tenantID, secret, scope}, nil
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
