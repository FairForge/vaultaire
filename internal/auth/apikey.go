package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/usage"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

// APIKey represents an API key for S3 access
// ErrKeyNotFound / ErrKeyRevoked are the typed key-lifecycle errors.
var (
	ErrKeyNotFound = errors.New("API key not found")
	ErrKeyRevoked  = errors.New("API key already revoked")
	// ErrKeyLimitReached: the free tier allows usage.FreeTierLimits.MaxAPIKeys
	// active keys BEYOND the primary pair minted at signup. Enforced in
	// GenerateAPIKey so every entry point agrees (Review R12 / R11-16).
	ErrKeyLimitReached = errors.New("API key limit reached for this plan")
	// ErrPrimaryKeyRevoke: the primary pair is never revoked outright — an
	// account must keep one way in, and the export download link, the
	// presigned-URL route and the credentials page all mint from it.
	// Rotation kills a leaked primary and hands over its successor in the
	// same call (WP-R5-14).
	ErrPrimaryKeyRevoke = errors.New("the primary key cannot be revoked; rotate it instead")
)

type APIKey struct {
	ID       string `json:"id" db:"id"`
	UserID   string `json:"user_id" db:"user_id"`
	TenantID string `json:"tenant_id" db:"tenant_id"`
	Name     string `json:"name" db:"name"`
	Key      string `json:"key" db:"key_id"`
	// IsPrimary marks the account's primary pair: the one registration hands
	// out, kept by rotation, never revoked outright (WP-R5-14). The row's
	// is_primary column is the truth; tenants.access_key mirrors it.
	IsPrimary   bool              `json:"is_primary" db:"is_primary"`
	Secret      string            `json:"secret,omitempty"`
	Hash        string            `json:"-" db:"secret_hash"`
	Permissions []string          `json:"permissions" db:"permissions"`
	BucketScope []string          `json:"bucket_scope" db:"bucket_scope"`
	IPAllowlist []string          `json:"ip_allowlist" db:"ip_allowlist"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty" db:"expires_at"`
	LastUsed    *time.Time        `json:"last_used,omitempty" db:"last_used"`
	CreatedAt   time.Time         `json:"created_at" db:"created_at"`
	RevokedAt   *time.Time        `json:"revoked_at,omitempty" db:"revoked_at"`
	Metadata    map[string]string `json:"metadata" db:"metadata"`
	UsageCount  int64             `json:"usage_count" db:"usage_count"`
	LastIP      string            `json:"last_ip,omitempty" db:"last_ip"`
}

// GenerateAPIKey creates a new API key for a user.
// opts may be nil for full-access keys.
func (a *AuthService) GenerateAPIKey(ctx context.Context, userID, name string, opts *KeyCreateOptions) (*APIKey, error) {
	a.cacheMu.RLock()
	user, exists := a.userIndex[userID]
	a.cacheMu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("user not found")
	}
	// The scope is checked here, once, for every entry point (WP-R5-12):
	// unknown permissions, allowlist entries that are not an address or a
	// network, an expiry that has already passed.
	if err := opts.Validate(time.Now()); err != nil {
		return nil, err
	}
	if err := a.checkKeyCap(ctx, user); err != nil {
		return nil, err
	}

	accessKey, err := generateAccessKey()
	if err != nil {
		return nil, fmt.Errorf("generate access key: %w", err)
	}

	secretKey, hash, err := generateSecretKey()
	if err != nil {
		return nil, fmt.Errorf("generate secret key: %w", err)
	}

	apiKey := &APIKey{
		ID:          uuid.New().String(),
		UserID:      userID,
		TenantID:    user.TenantID,
		Name:        name,
		Key:         accessKey,
		Secret:      secretKey,
		Hash:        hash,
		Permissions: []string{"*"},
		CreatedAt:   time.Now(),
		Metadata:    make(map[string]string),
	}

	if opts != nil {
		if len(opts.Permissions) > 0 {
			apiKey.Permissions = opts.Permissions
		}
		apiKey.BucketScope = opts.BucketScope
		apiKey.IPAllowlist = opts.IPAllowlist
		apiKey.ExpiresAt = opts.ExpiresAt
	}

	// Persist FIRST, then publish to the in-memory maps: a key whose INSERT
	// failed must not exist anywhere (R5-05 — it used to authenticate until
	// the next restart).
	if err := a.persistAPIKey(ctx, a.dbExecer(), apiKey); err != nil {
		return nil, err
	}
	a.indexAPIKey(apiKey)

	a.record(ctx, audit.Entry{UserID: userID, TenantID: apiKey.TenantID, Action: "key.created", Resource: "key:" + apiKey.ID,
		Metadata: map[string]any{"name": apiKey.Name, "permissions": apiKey.Permissions, "bucket_scope": apiKey.BucketScope,
			"ip_allowlist": apiKey.IPAllowlist, "expires_at": apiKey.ExpiresAt}})
	return apiKey, nil
}

// execer is what persistAPIKey writes through: the service's *sql.DB or the
// transaction a rotation runs in.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// persistAPIKey inserts the key row. Scope slices are normalised to empty
// (never NULL): bucket_scope and ip_allowlist are NOT NULL DEFAULT '{}', and
// pq.Array(nil) encodes SQL NULL — the "permissions-only key" case (R5-05).
// The row carries its tenant (WP-R5-9) and whether it is the primary pair.
func (a *AuthService) persistAPIKey(ctx context.Context, db execer, k *APIKey) error {
	if k.BucketScope == nil {
		k.BucketScope = []string{}
	}
	if k.IPAllowlist == nil {
		k.IPAllowlist = []string{}
	}
	if db == nil {
		return nil
	}
	secretHash, err := bcrypt.GenerateFromPassword([]byte(k.Secret), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash api key secret: %w", err)
	}
	permJSON, err := json.Marshal(k.Permissions)
	if err != nil {
		return fmt.Errorf("encode api key permissions: %w", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO api_keys (id, user_id, tenant_id, is_primary, name, key_id, secret_hash, secret_key,
		                      permissions, bucket_scope, ip_allowlist, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (key_id) DO NOTHING
	`, k.ID, k.UserID, k.TenantID, k.IsPrimary, k.Name, k.Key, string(secretHash), k.Secret,
		permJSON, pq.Array(k.BucketScope), pq.Array(k.IPAllowlist),
		k.ExpiresAt, k.CreatedAt)
	if err != nil {
		return fmt.Errorf("persist api key: %w", err)
	}
	return nil
}

// dbExecer returns the service database as an execer, or a nil interface
// when there is none (tests without a database).
func (a *AuthService) dbExecer() execer {
	if a.sqlDB == nil {
		return nil
	}
	return a.sqlDB
}

// indexAPIKey publishes a persisted key to the in-memory maps. Caller holds
// no lock.
func (a *AuthService) indexAPIKey(k *APIKey) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	a.indexAPIKeyLocked(k)
}

// indexAPIKeyLocked is indexAPIKey under cacheMu. A primary key also becomes
// the tenant's pair in memory (what ValidateS3Request hands back).
func (a *AuthService) indexAPIKeyLocked(k *APIKey) {
	a.apiKeys[k.Key] = k
	if tenant, ok := a.tenants[k.TenantID]; ok {
		a.keyIndex[k.Key] = tenant
		if k.IsPrimary && k.RevokedAt == nil {
			tenant.AccessKey = k.Key
			tenant.SecretKey = k.Secret
		}
	}
}

// markRevokedLocked records a revocation in memory: the listing copy keeps
// the key with revoked_at set, the hot index forgets it (a rotation or a
// revocation used to leave the old key authenticating from this map until
// the next restart — WP-R5-14).
func (a *AuthService) markRevokedLocked(k *APIKey, at time.Time) {
	k.RevokedAt = &at
	delete(a.keyIndex, k.Key)
}

// findOwnedKey returns the in-memory key with this id owned by userID.
func (a *AuthService) findOwnedKey(userID, keyID string) *APIKey {
	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()
	for _, key := range a.apiKeys {
		if key.ID == keyID && key.UserID == userID {
			return key
		}
	}
	return nil
}

// GetPrimaryAPIKey returns the tenant's live primary key (no secret), or
// ErrKeyNotFound when the tenant has none.
func (a *AuthService) GetPrimaryAPIKey(_ context.Context, tenantID string) (*APIKey, error) {
	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()
	for _, key := range a.apiKeys {
		if key.TenantID == tenantID && key.IsPrimary && key.RevokedAt == nil {
			cp := *key
			cp.Secret = ""
			return &cp, nil
		}
	}
	return nil, ErrKeyNotFound
}

// PrimaryPair reads the tenant's live primary pair from the database — the
// one row every surface that signs on the tenant's behalf (the export
// download link, the presigned-URL route) must use. ErrKeyNotFound when the
// tenant has no live primary.
func PrimaryPair(ctx context.Context, db *sql.DB, tenantID string) (accessKey, secretKey string, err error) {
	if db == nil {
		return "", "", errors.New("database not initialized")
	}
	err = db.QueryRowContext(ctx, `
		SELECT key_id, COALESCE(secret_key, '') FROM api_keys
		WHERE tenant_id = $1 AND is_primary AND revoked_at IS NULL`, tenantID).Scan(&accessKey, &secretKey)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrKeyNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("read primary pair of %s: %w", tenantID, err)
	}
	return accessKey, secretKey, nil
}

// liveSTSChildren counts the unexpired STS tokens minted from a key: the
// ones a revocation or rotation of that key kills (the credential lookup
// joins the parent, so nothing is deleted — the count is for the audit row).
func (a *AuthService) liveSTSChildren(ctx context.Context, parentKeyID string) int {
	if a.sqlDB == nil {
		return 0
	}
	var n int
	if err := a.sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sts_tokens WHERE parent_key_id = $1 AND expires_at > NOW()`, parentKeyID).Scan(&n); err != nil {
		return 0
	}
	return n
}

// GetOwnedAPIKey returns the caller's key with this id, or ErrKeyNotFound /
// ErrKeyRevoked / ErrKeyExpired. Used by STS to bound a token to a named
// parent key: an expired parent mints nothing.
func (a *AuthService) GetOwnedAPIKey(_ context.Context, userID, keyID string) (*APIKey, error) {
	key := a.findOwnedKey(userID, keyID)
	if key == nil {
		return nil, ErrKeyNotFound
	}
	if key.RevokedAt != nil {
		return nil, ErrKeyRevoked
	}
	if IsKeyExpired(key.ExpiresAt) {
		return nil, ErrKeyExpired
	}
	cp := *key
	cp.Secret = ""
	return &cp, nil
}

// persistRevocation stamps revoked_at on the row. The S3 auth path reads
// api_keys per request and filters on revoked_at IS NULL, so this — not the
// in-memory field — is what actually stops a key (R5-01).
func (a *AuthService) persistRevocation(ctx context.Context, userID, keyID string) error {
	if a.sqlDB == nil {
		return nil
	}
	_, err := a.sqlDB.ExecContext(ctx, `
		UPDATE api_keys SET revoked_at = NOW()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
	`, keyID, userID)
	if err != nil {
		return fmt.Errorf("persist api key revocation %s: %w", keyID, err)
	}
	return nil
}

// ValidateAPIKey checks if API key is valid
func (a *AuthService) ValidateAPIKey(ctx context.Context, key, secret string) (*User, error) {
	a.cacheMu.RLock()
	apiKey, exists := a.apiKeys[key]
	a.cacheMu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("invalid API key")
	}

	if apiKey.RevokedAt != nil {
		return nil, fmt.Errorf("API key has been revoked")
	}

	if IsKeyExpired(apiKey.ExpiresAt) {
		return nil, ErrKeyExpired
	}

	hash := sha256.Sum256([]byte(secret))
	if hex.EncodeToString(hash[:]) != apiKey.Hash {
		return nil, fmt.Errorf("invalid API secret")
	}

	a.cacheMu.RLock()
	user, exists := a.userIndex[apiKey.UserID]
	a.cacheMu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("user not found")
	}

	now := time.Now()
	apiKey.LastUsed = &now
	apiKey.UsageCount++

	return user, nil
}

// RotateAPIKey mints a replacement key with the old key's scope, persists
// it, and revokes the old key — in the database as well as in memory.
//
// One transaction: the old row is revoked (WHERE revoked_at IS NULL — a
// concurrent rotation of the same key sees zero rows and answers
// ErrKeyRevoked, so there are never two successors), the successor is
// inserted, and for the primary pair tenants.access_key/secret_key are
// rewritten to it (WP-R5-14; nothing used to write them, and every lookup
// read them first). The old key is dropped from the hot index in the same
// call. STS tokens minted from the old key die with it (the credential
// lookup joins the parent; WP-R5-5) and are counted on the audit row.
func (a *AuthService) RotateAPIKey(ctx context.Context, userID, keyID string) (*APIKey, error) {
	oldKey := a.findOwnedKey(userID, keyID)
	if oldKey == nil {
		return nil, ErrKeyNotFound
	}
	if oldKey.RevokedAt != nil {
		return nil, ErrKeyRevoked
	}

	accessKey, err := generateAccessKey()
	if err != nil {
		return nil, fmt.Errorf("generate access key: %w", err)
	}

	secretKey, hash, err := generateSecretKey()
	if err != nil {
		return nil, fmt.Errorf("generate secret key: %w", err)
	}

	newKey := &APIKey{
		ID:          uuid.New().String(),
		UserID:      oldKey.UserID,
		TenantID:    oldKey.TenantID,
		IsPrimary:   oldKey.IsPrimary,
		Name:        rotatedName(oldKey.Name),
		Key:         accessKey,
		Secret:      secretKey,
		Hash:        hash,
		Permissions: oldKey.Permissions,
		BucketScope: oldKey.BucketScope,
		IPAllowlist: oldKey.IPAllowlist,
		ExpiresAt:   oldKey.ExpiresAt,
		CreatedAt:   time.Now(),
		Metadata:    oldKey.Metadata,
	}

	now := time.Now()
	children := a.liveSTSChildren(ctx, oldKey.Key)
	if a.sqlDB != nil {
		if err := a.rotateRows(ctx, oldKey, newKey, now); err != nil {
			return nil, err
		}
	} else if err := a.persistAPIKey(ctx, nil, newKey); err != nil {
		return nil, err
	}

	a.cacheMu.Lock()
	if oldKey.RevokedAt == nil {
		a.markRevokedLocked(oldKey, now)
	}
	a.indexAPIKeyLocked(newKey)
	a.cacheMu.Unlock()

	a.record(ctx, audit.Entry{UserID: userID, TenantID: newKey.TenantID, Action: "key.rotated", Resource: "key:" + oldKey.ID,
		Metadata: map[string]any{"new_key_id": newKey.ID, "name": oldKey.Name, "primary": oldKey.IsPrimary,
			"sts_tokens_invalidated": children}})
	return newKey, nil
}

// rotateRows is the database half of a rotation: revoke, insert the
// successor, rewrite the tenant's mirror — one transaction.
func (a *AuthService) rotateRows(ctx context.Context, oldKey, newKey *APIKey, at time.Time) error {
	tx, err := a.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin key rotation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE api_keys SET revoked_at = $3
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
	`, oldKey.ID, oldKey.UserID, at)
	if err != nil {
		return fmt.Errorf("revoke api key %s: %w", oldKey.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Another rotation (or a revocation) got there first.
		return ErrKeyRevoked
	}
	if err := a.persistAPIKey(ctx, tx, newKey); err != nil {
		return err
	}
	if newKey.IsPrimary {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tenants SET access_key = $1, secret_key = $2 WHERE id = $3`,
			newKey.Key, newKey.Secret, newKey.TenantID); err != nil {
			return fmt.Errorf("rewrite tenant pair of %s: %w", newKey.TenantID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit key rotation: %w", err)
	}
	return nil
}

// rotatedName marks a successor once, however often a key is rotated.
func rotatedName(name string) string {
	const suffix = " (rotated)"
	if strings.HasSuffix(name, suffix) {
		return name
	}
	return name + suffix
}

// RevokeAPIKey revokes an API key: the row gets revoked_at (the S3 auth
// path's source of truth), the hot index forgets it, the listing copy is
// marked. The primary pair is refused (ErrPrimaryKeyRevoke): rotate it.
func (a *AuthService) RevokeAPIKey(ctx context.Context, userID, keyID string) error {
	key := a.findOwnedKey(userID, keyID)
	if key == nil {
		return ErrKeyNotFound
	}
	if key.RevokedAt != nil {
		return ErrKeyRevoked
	}
	if key.IsPrimary {
		return ErrPrimaryKeyRevoke
	}
	children := a.liveSTSChildren(ctx, key.Key)
	if err := a.persistRevocation(ctx, userID, keyID); err != nil {
		return err
	}
	a.cacheMu.Lock()
	a.markRevokedLocked(key, time.Now())
	a.cacheMu.Unlock()
	a.record(ctx, audit.Entry{UserID: userID, TenantID: key.TenantID, Action: "key.revoked", Resource: "key:" + keyID,
		Metadata: map[string]any{"name": key.Name, "sts_tokens_invalidated": children}})
	return nil
}

// SetAPIKeyExpiration sets expiration for an API key, persisted so the S3
// auth path enforces it.
func (a *AuthService) SetAPIKeyExpiration(ctx context.Context, userID, keyID string, expiresAt time.Time) error {
	key := a.findOwnedKey(userID, keyID)
	if key == nil {
		return ErrKeyNotFound
	}
	if a.sqlDB != nil {
		if _, err := a.sqlDB.ExecContext(ctx,
			`UPDATE api_keys SET expires_at = $1 WHERE id = $2 AND user_id = $3`,
			expiresAt, keyID, userID); err != nil {
			return fmt.Errorf("persist api key expiration %s: %w", keyID, err)
		}
	}
	key.ExpiresAt = &expiresAt
	a.record(ctx, audit.Entry{UserID: userID, TenantID: key.TenantID, Action: "key.expiry_set", Resource: "key:" + keyID,
		Metadata: map[string]any{"expires_at": expiresAt}})
	return nil
}

// ListAPIKeys lists all API keys for a user
func (a *AuthService) ListAPIKeys(ctx context.Context, userID string) ([]*APIKey, error) {
	a.cacheMu.RLock()
	defer a.cacheMu.RUnlock()
	var keys []*APIKey
	for _, key := range a.apiKeys {
		if key.UserID == userID {
			keyCopy := *key
			keyCopy.Secret = ""
			keys = append(keys, &keyCopy)
		}
	}
	return keys, nil
}

// Helper functions
func generateAccessKey() (string, error) {
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	encoded = strings.ToUpper(encoded)

	if len(encoded) < 20 {
		encoded = encoded + strings.Repeat("0", 20-len(encoded))
	}

	return fmt.Sprintf("VLT_%s", encoded[:20]), nil
}

func generateSecretKey() (string, string, error) {
	b := make([]byte, 30)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}

	secret := hex.EncodeToString(b)
	if len(secret) > 40 {
		secret = secret[:40]
	}

	hash := sha256.Sum256([]byte(secret))
	hashStr := hex.EncodeToString(hash[:])

	return secret, hashStr, nil
}

// checkKeyCap enforces the free-tier key cap: active (non-revoked) keys
// other than the tenant's primary pair (the is_primary row) must stay below
// usage.FreeTierLimits.MaxAPIKeys. Paid tiers are not capped here. Without
// a database (tests, dev) there is no tier and no cap.
func (a *AuthService) checkKeyCap(ctx context.Context, user *User) error {
	if a.sqlDB == nil {
		return nil
	}
	var tier string
	err := a.sqlDB.QueryRowContext(ctx,
		`SELECT COALESCE(tier, '') FROM tenant_quotas WHERE tenant_id = $1`, user.TenantID).Scan(&tier)
	if err != nil || !usage.IsFreeTier(tier) {
		return nil // no quota row → not free-tier capped; a DB error must not block key creation
	}
	var active int
	if err := a.sqlDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM api_keys ak
		WHERE ak.user_id = $1 AND ak.revoked_at IS NULL AND NOT ak.is_primary`,
		user.ID).Scan(&active); err != nil {
		return fmt.Errorf("count api keys: %w", err)
	}
	if active >= usage.FreeTierLimits.MaxAPIKeys {
		return ErrKeyLimitReached
	}
	return nil
}
