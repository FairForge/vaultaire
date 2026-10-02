package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

type STSToken struct {
	AccessKey   string    `json:"access_key"`
	SecretKey   string    `json:"secret_key"`
	TenantID    string    `json:"tenant_id"`
	ParentKeyID string    `json:"parent_key_id"`
	Permissions []string  `json:"permissions"`
	BucketScope []string  `json:"bucket_scope"`
	IPRestrict  []string  `json:"ip_restrict"`
	ExpiresAt   time.Time `json:"expires_at"`
	CreatedAt   time.Time `json:"created_at"`
}

type STSRequest struct {
	Permissions []string `json:"permissions"`
	BucketScope []string `json:"bucket_scope"`
	IPRestrict  []string `json:"ip_restrict"`
	TTL         int      `json:"ttl"`
	// ParentKeyID optionally names one of the caller's own API keys whose
	// scope bounds the token. Empty = the account's own (full) authority.
	// Review R11-03: the parent used to be a random key of the user, and a
	// fresh account's primary key has no permission list, so STS never
	// minted for it.
	ParentKeyID string `json:"parent_key_id,omitempty"`
}

const (
	stsDefaultTTL = 3600
	stsMaxTTL     = 43200
	stsMinTTL     = 1
)

// ErrSTSScope marks a request the caller can fix (scope does not overlap,
// unknown permission); anything else from GenerateSTSToken is internal.
var ErrSTSScope = errors.New("sts scope error")

func GenerateSTSToken(ctx context.Context, db *sql.DB, tenantID, parentKeyID string, parentScope *KeyScope, req STSRequest) (*STSToken, error) {
	perms := intersectPermissions(parentScope.Permissions, req.Permissions)
	// The GOVERNANCE bypass is never inherited: a token carries it only when
	// the request names it AND the parent may bypass (WP-R4-1). An unscoped
	// request copies the parent's list, and a parent of `*` hands the
	// requested list through — both would otherwise pass the privilege on
	// without anyone having asked for it.
	if !contains(req.Permissions, PermBypassGovernanceRetention) || !parentScope.CanBypassGovernanceRetention() {
		perms = without(perms, PermBypassGovernanceRetention)
	}
	if len(perms) == 0 {
		return nil, fmt.Errorf("%w: no permissions overlap between parent key and request", ErrSTSScope)
	}

	if err := ValidatePermissions(perms); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSTSScope, err)
	}

	buckets := intersectBucketScope(parentScope.BucketScope, req.BucketScope)
	if buckets == nil {
		// sts_tokens.bucket_scope / ip_restrict are NOT NULL: a nil slice
		// became NULL and the INSERT failed for every unscoped request
		// (Review R11-03, the R5-05 nil-array class on another entry point).
		buckets = []string{}
	}

	ipRestrict := narrowIPRestrict(parentScope.IPAllowlist, req.IPRestrict)
	if ipRestrict == nil {
		ipRestrict = []string{}
	}

	ttl := req.TTL
	if ttl <= 0 {
		ttl = stsDefaultTTL
	}
	if ttl > stsMaxTTL {
		ttl = stsMaxTTL
	}

	accessKey, err := generateSTSAccessKey()
	if err != nil {
		return nil, fmt.Errorf("generate STS access key: %w", err)
	}

	secretKey, err := generateSTSSecretKey()
	if err != nil {
		return nil, fmt.Errorf("generate STS secret key: %w", err)
	}

	now := time.Now()
	token := &STSToken{
		AccessKey:   accessKey,
		SecretKey:   secretKey,
		TenantID:    tenantID,
		ParentKeyID: parentKeyID,
		Permissions: perms,
		BucketScope: buckets,
		IPRestrict:  ipRestrict,
		ExpiresAt:   now.Add(time.Duration(ttl) * time.Second),
		CreatedAt:   now,
	}

	if db != nil {
		permJSON, _ := json.Marshal(token.Permissions)
		_, err = db.ExecContext(ctx, `
			INSERT INTO sts_tokens (access_key, secret_key, tenant_id, parent_key_id,
			                        permissions, bucket_scope, ip_restrict, expires_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`, token.AccessKey, token.SecretKey, token.TenantID, token.ParentKeyID,
			permJSON, pq.Array(token.BucketScope), pq.Array(token.IPRestrict),
			token.ExpiresAt, token.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("persist STS token: %w", err)
		}
	}

	return token, nil
}

// CleanupExpiredSTSTokens deletes expired temporary credentials and returns
// how many. The API server runs it hourly as the `sts_cleanup` job.
func CleanupExpiredSTSTokens(ctx context.Context, db *sql.DB) (int64, error) {
	result, err := db.ExecContext(ctx, `DELETE FROM sts_tokens WHERE expires_at < NOW()`)
	if err != nil {
		return 0, fmt.Errorf("delete expired STS tokens: %w", err)
	}
	n, _ := result.RowsAffected()
	return n, nil
}

func generateSTSAccessKey() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "ASIA" + hex.EncodeToString(b), nil
}

func generateSTSSecretKey() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(b)
	return secret, nil
}

func intersectPermissions(parent, requested []string) []string {
	if len(requested) == 0 {
		return parent
	}

	for _, p := range parent {
		if p == "*" {
			return requested
		}
	}

	parentSet := make(map[string]bool, len(parent))
	for _, p := range parent {
		parentSet[p] = true
	}

	var result []string
	for _, p := range requested {
		if parentSet[p] {
			result = append(result, p)
		}
	}
	return result
}

func intersectBucketScope(parent, requested []string) []string {
	if len(parent) == 0 && len(requested) == 0 {
		return nil
	}
	if len(parent) == 0 {
		return requested
	}
	if len(requested) == 0 {
		return parent
	}

	parentSet := make(map[string]bool, len(parent))
	for _, b := range parent {
		parentSet[b] = true
	}

	var result []string
	for _, b := range requested {
		if parentSet[b] {
			result = append(result, b)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func narrowIPRestrict(parentAllowlist, requested []string) []string {
	if len(parentAllowlist) == 0 {
		return requested
	}
	if len(requested) == 0 {
		return parentAllowlist
	}
	return requested
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// without returns list minus v, never aliasing the caller's slice.
func without(list []string, v string) []string {
	out := make([]string, 0, len(list))
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
