package auth

// A platform's child's keys (WP-PLAT-1 PR C). A child has no primary pair:
// every key of a child is a scoped key its platform mints (the row's
// user_id is the platform's user who minted it, as a sandbox's test keys
// are their parent user's) and may revoke. A child's key is never wider
// than the credential that minted it (CapScope); it lives in the database
// only — never in the platform user's in-memory key list (LoadFromDB skips
// it, the mint never indexes it), and never counted in that user's plan
// cap. STS tokens of a child are bounded by one of its keys (they die with
// it, the one lookup's rule).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/FairForge/vaultaire/internal/audit"
)

// ErrScopeExceedsParent: a child's key asks more than the credential that
// mints it holds.
var ErrScopeExceedsParent = errors.New("the key's scope exceeds the calling credential's")

// maxChildKeys bounds the live keys of one child.
const maxChildKeys = 50

// CapScope bounds o by the calling credential's scope (nil = the account's
// own authority: no bound beyond Validate). What o leaves unset is the
// caller's — permissions, buckets, IP allowlist, expiry — so a child key
// is never wider by omission; what it sets must lie within the caller's.
// ErrScopeExceedsParent names the first excess.
func CapScope(caller *KeyScope, o *KeyCreateOptions) error {
	if caller == nil {
		return nil
	}
	if len(o.Permissions) == 0 {
		o.Permissions = append([]string(nil), caller.Permissions...)
	}
	for _, p := range o.Permissions {
		switch {
		case p == PermBypassGovernanceRetention:
			if !caller.CanBypassGovernanceRetention() {
				return fmt.Errorf("%w: %s", ErrScopeExceedsParent, p)
			}
		case p == "*":
			if !contains(caller.Permissions, "*") {
				return fmt.Errorf("%w: *", ErrScopeExceedsParent)
			}
		case !CheckPermission(caller.Permissions, p):
			return fmt.Errorf("%w: permission %s", ErrScopeExceedsParent, p)
		}
	}
	if len(caller.BucketScope) > 0 {
		if len(o.BucketScope) == 0 {
			o.BucketScope = append([]string(nil), caller.BucketScope...)
		}
		for _, b := range o.BucketScope {
			if !CheckBucketScope(caller.BucketScope, b) {
				return fmt.Errorf("%w: bucket %s", ErrScopeExceedsParent, b)
			}
		}
	}
	if len(caller.IPAllowlist) > 0 {
		if len(o.IPAllowlist) == 0 {
			o.IPAllowlist = append([]string(nil), caller.IPAllowlist...)
		}
		for _, e := range o.IPAllowlist {
			if !ipAllowedBy(e, caller.IPAllowlist) {
				return fmt.Errorf("%w: ip_allowlist %s", ErrScopeExceedsParent, e)
			}
		}
	}
	if caller.ExpiresAt != nil {
		if o.ExpiresAt == nil {
			t := *caller.ExpiresAt
			o.ExpiresAt = &t
		} else if o.ExpiresAt.After(*caller.ExpiresAt) {
			return fmt.Errorf("%w: expires_at after the caller's", ErrScopeExceedsParent)
		}
	}
	return nil
}

// ipAllowedBy reports whether entry lies inside one entry of allow (the
// STS intersection's own containment, sts.go).
func ipAllowedBy(entry string, allow []string) bool {
	for _, a := range allow {
		if ipEntryWithin(entry, a) {
			return true
		}
	}
	return false
}

// GenerateChildKey mints a scoped key of childID, a child of parentID, for
// the platform user actorUserID, in one transaction (the child row FOR
// SHARE — a child being erased mints nothing — the live-key cap, the row,
// the audit row platform.key_created). opts must already be capped
// (CapScope). ErrTenantNotFound when childID is not parentID's child.
func (a *AuthService) GenerateChildKey(ctx context.Context, actorUserID, parentID, childID, name string, opts *KeyCreateOptions) (*APIKey, error) {
	if a.sqlDB == nil {
		return nil, errors.New("generate child key: no database")
	}
	if opts == nil {
		opts = &KeyCreateOptions{}
	}
	if err := opts.Validate(time.Now()); err != nil {
		return nil, err
	}
	tx, err := a.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("generate child key: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The child, locked against its erasure while the key is written. A
	// child scheduled for erasure may still be given keys: the 30-day grace
	// blocks nothing (D-16), so nothing here reads the schedule (RP82-5).
	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM tenants WHERE id = $1 AND parent_tenant_id = $2 FOR SHARE`,
		childID, parentID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("generate child key: read the child: %w", err)
	}
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE tenant_id = $1 AND revoked_at IS NULL`, childID).Scan(&live); err != nil {
		return nil, fmt.Errorf("generate child key: count: %w", err)
	}
	if live >= maxChildKeys {
		return nil, fmt.Errorf("%w: %d keys per child", ErrKeyLimitReached, maxChildKeys)
	}
	accessKey, err := generateAccessKey()
	if err != nil {
		return nil, fmt.Errorf("generate access key: %w", err)
	}
	secretKey, hash, err := generateSecretKey()
	if err != nil {
		return nil, fmt.Errorf("generate secret key: %w", err)
	}
	k := &APIKey{
		ID: uuid.New().String(), UserID: actorUserID, TenantID: childID, Name: name,
		Key: accessKey, Secret: secretKey, Hash: hash, Permissions: []string{"*"},
		BucketScope: opts.BucketScope, IPAllowlist: opts.IPAllowlist, ExpiresAt: opts.ExpiresAt,
		CreatedAt: time.Now(), Metadata: map[string]string{"child_of": parentID},
	}
	if len(opts.Permissions) > 0 {
		k.Permissions = opts.Permissions
	}
	if _, err := a.persistAPIKey(ctx, tx, k); err != nil {
		return nil, err
	}
	if err := audit.RecordTx(ctx, tx, audit.Entry{TenantID: parentID, EventType: "platform", Action: "platform.key_created",
		Resource: "key:" + k.ID, Metadata: map[string]any{"child": childID, "name": name, "permissions": k.Permissions,
			"bucket_scope": k.BucketScope, "ip_allowlist": k.IPAllowlist, "expires_at": k.ExpiresAt}}); err != nil {
		return nil, fmt.Errorf("generate child key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("generate child key: %w", err)
	}
	return k, nil
}

// ListChildKeys is childID's keys (never a secret), the newest first.
func ListChildKeys(ctx context.Context, db *sql.DB, childID string) ([]*APIKey, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, user_id, tenant_id, name, key_id, COALESCE(permissions, '["*"]'::jsonb), COALESCE(bucket_scope, '{}'),
		       COALESCE(ip_allowlist, '{}'), expires_at, created_at, revoked_at, last_used
		  FROM api_keys WHERE tenant_id = $1 ORDER BY created_at DESC, id`, childID)
	if err != nil {
		return nil, fmt.Errorf("list the keys of %s: %w", childID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []*APIKey
	for rows.Next() {
		var k APIKey
		var perm []byte
		var buckets, ips pq.StringArray
		var exp, rev, used sql.NullTime
		if err := rows.Scan(&k.ID, &k.UserID, &k.TenantID, &k.Name, &k.Key, &perm, &buckets, &ips, &exp, &k.CreatedAt, &rev, &used); err != nil {
			return nil, fmt.Errorf("list the keys of %s: %w", childID, err)
		}
		if json.Unmarshal(perm, &k.Permissions) != nil {
			k.Permissions = nil
		}
		k.BucketScope, k.IPAllowlist = buckets, ips
		if exp.Valid {
			k.ExpiresAt = &exp.Time
		}
		if rev.Valid {
			k.RevokedAt = &rev.Time
		}
		if used.Valid {
			k.LastUsed = &used.Time
		}
		out = append(out, &k)
	}
	return out, rows.Err()
}

// RevokeChildKey revokes the key keyRowID of childID, a child of parentID
// (its STS tokens die with it: the lookup joins the parent key), audited
// platform.key_revoked. ErrKeyNotFound, ErrKeyRevoked.
func (a *AuthService) RevokeChildKey(ctx context.Context, parentID, childID, keyRowID string) error {
	if a.sqlDB == nil {
		return errors.New("revoke child key: no database")
	}
	if _, err := uuid.Parse(keyRowID); err != nil {
		return ErrKeyNotFound
	}
	var keyID string
	var revoked bool
	err := a.sqlDB.QueryRowContext(ctx, `
		SELECT ak.key_id, ak.revoked_at IS NOT NULL FROM api_keys ak JOIN tenants t ON t.id = ak.tenant_id
		 WHERE ak.id = $1 AND ak.tenant_id = $2 AND t.parent_tenant_id = $3`, keyRowID, childID, parentID).Scan(&keyID, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrKeyNotFound
	}
	if err != nil {
		return fmt.Errorf("revoke child key: %w", err)
	}
	if revoked {
		return ErrKeyRevoked
	}
	if _, err := a.sqlDB.ExecContext(ctx, `UPDATE api_keys SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL`, keyRowID); err != nil {
		return fmt.Errorf("revoke child key: %w", err)
	}
	a.cacheMu.Lock()
	delete(a.keyIndex, keyID)
	a.cacheMu.Unlock()
	a.record(ctx, audit.Entry{TenantID: parentID, EventType: "platform", Action: "platform.key_revoked", Resource: "key:" + keyRowID,
		Metadata: map[string]any{"child": childID}})
	return nil
}

// ChildKeyScope is the scope of childID's live key keyRowID (the STS
// parent): ErrKeyNotFound, ErrKeyRevoked, ErrKeyExpired.
func ChildKeyScope(ctx context.Context, db *sql.DB, childID, keyRowID string) (accessKey string, scope *KeyScope, err error) {
	if _, perr := uuid.Parse(keyRowID); perr != nil {
		return "", nil, ErrKeyNotFound
	}
	var perm []byte
	var buckets, ips pq.StringArray
	var exp, rev sql.NullTime
	err = db.QueryRowContext(ctx, `
		SELECT key_id, COALESCE(permissions, '["*"]'::jsonb), COALESCE(bucket_scope, '{}'), COALESCE(ip_allowlist, '{}'), expires_at, revoked_at
		  FROM api_keys WHERE id = $1 AND tenant_id = $2`, keyRowID, childID).Scan(&accessKey, &perm, &buckets, &ips, &exp, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrKeyNotFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("child key scope: %w", err)
	}
	if rev.Valid {
		return "", nil, ErrKeyRevoked
	}
	scope = &KeyScope{BucketScope: buckets, IPAllowlist: ips}
	if json.Unmarshal(perm, &scope.Permissions) != nil {
		scope.Permissions = nil
	}
	if exp.Valid {
		if !exp.Time.After(time.Now()) {
			return "", nil, ErrKeyExpired
		}
		scope.ExpiresAt = &exp.Time
	}
	return accessKey, scope, nil
}
