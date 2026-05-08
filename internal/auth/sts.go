package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"
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
}

const (
	stsDefaultTTL = 3600
	stsMaxTTL     = 43200
	stsMinTTL     = 1
)

func GenerateSTSToken(ctx context.Context, db *sql.DB, tenantID, parentKeyID string, parentScope *KeyScope, req STSRequest) (*STSToken, error) {
	perms := intersectPermissions(parentScope.Permissions, req.Permissions)
	if len(perms) == 0 {
		return nil, fmt.Errorf("no permissions overlap between parent key and request")
	}

	if err := ValidatePermissions(perms); err != nil {
		return nil, fmt.Errorf("validate permissions: %w", err)
	}

	buckets := intersectBucketScope(parentScope.BucketScope, req.BucketScope)

	ipRestrict := narrowIPRestrict(parentScope.IPAllowlist, req.IPRestrict)

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

func StartSTSCleanup(ctx context.Context, db *sql.DB, logger *zap.Logger) {
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				result, err := db.ExecContext(ctx, `DELETE FROM sts_tokens WHERE expires_at < NOW()`)
				if err != nil {
					logger.Error("sts token cleanup", zap.Error(err))
				} else if n, _ := result.RowsAffected(); n > 0 {
					logger.Info("cleaned expired STS tokens", zap.Int64("count", n))
				}
			}
		}
	}()
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
