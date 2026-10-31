// internal/auth/management_keys.go
//
// The management key (WP-KEY-1, Stage 1a Prompt 54 PR A; D-STRIPE-29;
// migration 121; docs/ACCOUNTS.md "Principal"): an opaque bearer key a
// SERVER presents to /api/v1/manage and /api/v1/platform instead of a
// person's JWT — a platform's backend, an app's backend, later an agent
// grant (WP-AGENT-1). It is the TENANT's, never a person's: a member's
// removal or erasure never revokes it (created_by is recorded, no foreign
// key). sk_live_… for an account, sk_test_… for its sandbox (minted with
// Test: the key is the sandbox's, RP88-1); an optional expiry (RP88-2);
// stored as the
// sha256 of the whole secret, which is shown once; looked up on every
// request under revoked_at IS NULL (no cache: a revoke is refused at the
// very next request, as a member's role is re-read). Its scopes are its
// authority (the api package derives a role from them); its S3 ceiling
// bounds what it MINTS (RP82-6: its own field, never a bucket scope of the
// key's — CapScope reads it through KeyScope).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/lib/pq"
)

// The management key's scopes.
const (
	// ScopeManageRead: the GETs of /api/v1/manage (a viewer's).
	ScopeManageRead = "manage:read"
	// ScopeManageWrite: the writes of /api/v1/manage a developer may do
	// (buckets, S3 keys, the sandbox) — never a member, the export, the
	// erasure or billing: those need a person.
	ScopeManageWrite = "manage:write"
	// ScopePlatform: /api/v1/platform of a platform tenant (its children).
	ScopePlatform = "platform"
	// ScopeApps: /api/v1/platform AS an app child — its own managed
	// children only (depth 2 stays the deepest). Only an app's key.
	ScopeApps = "apps"
)

// The two prefixes: the tenant's livemode at mint (a sandbox is test mode).
const (
	ManagementKeyLivePrefix = "sk_live_"
	ManagementKeyTestPrefix = "sk_test_"
)

// MaxLiveManagementKeys bounds a tenant's live management keys.
const MaxLiveManagementKeys = 25

// mintAfterCount runs between the live-key count and the insert (a test
// widens the window with it; RP88-3).
var mintAfterCount = func() {}

// managementKeyUseEvery coalesces last_used_at writes.
const managementKeyUseEvery = time.Minute

var (
	// ErrManagementKeyInvalid: no live key has that secret (unknown,
	// malformed or revoked — never said which).
	ErrManagementKeyInvalid = errors.New("invalid management key")
	// ErrManagementKeyNotFound: no live key of the tenant has that id.
	ErrManagementKeyNotFound = errors.New("management key not found")
	// ErrManagementKeyScope: a scope list that is empty, unknown, or not
	// available to the tenant (platform on a non-platform, apps on a
	// tenant that is no app).
	ErrManagementKeyScope = errors.New("invalid management key scopes")
	// ErrManagementKeyCeiling: an S3 ceiling that does not validate.
	ErrManagementKeyCeiling = errors.New("invalid S3 ceiling")
	// ErrManagementKeyExpiry: an expiry that is not in the future.
	ErrManagementKeyExpiry = errors.New("invalid management key expiry")
)

// S3Ceiling is the most a management key may mint (an S3 key, a child's
// key, an STS token): every member optional; empty = no bound on it.
type S3Ceiling struct {
	Permissions      []string `json:"permissions,omitempty"`
	BucketScope      []string `json:"bucket_scope,omitempty"`
	IPAllowlist      []string `json:"ip_allowlist,omitempty"`
	MaxExpirySeconds int64    `json:"max_expiry_seconds,omitempty"`
}

// Validate checks the ceiling and canonicalises its IP list.
func (c *S3Ceiling) Validate() error {
	if c == nil {
		return nil
	}
	if err := ValidatePermissions(c.Permissions); err != nil {
		return fmt.Errorf("%w: %w", ErrManagementKeyCeiling, err)
	}
	for _, b := range c.BucketScope {
		if strings.TrimSpace(b) == "" {
			return fmt.Errorf("%w: an empty bucket name", ErrManagementKeyCeiling)
		}
	}
	canon, err := ValidateIPAllowlist(c.IPAllowlist)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrManagementKeyCeiling, err)
	}
	c.IPAllowlist = canon
	if c.MaxExpirySeconds < 0 {
		return fmt.Errorf("%w: max_expiry_seconds is negative", ErrManagementKeyCeiling)
	}
	return nil
}

// KeyScope is the ceiling as the calling credential's scope CapScope
// bounds a mint by (nil = no ceiling: the account's own authority). No
// permission list = every permission; the longest expiry is from now.
func (c *S3Ceiling) KeyScope(now time.Time) *KeyScope {
	if c == nil {
		return nil
	}
	ks := &KeyScope{Permissions: c.Permissions, BucketScope: c.BucketScope, IPAllowlist: c.IPAllowlist}
	if len(ks.Permissions) == 0 {
		ks.Permissions = []string{"*"}
	}
	if c.MaxExpirySeconds > 0 {
		at := now.Add(time.Duration(c.MaxExpirySeconds) * time.Second)
		ks.ExpiresAt = &at
	}
	return ks
}

// ManagementKey is one row (never the secret).
type ManagementKey struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"tenant_id"`
	Prefix     string     `json:"prefix"`
	Hint       string     `json:"hint"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	Ceiling    *S3Ceiling `json:"s3_ceiling"`
	CreatedBy  string     `json:"created_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at"`

	// Set by LookupManagementKey only:
	// Actor is the user the key's rows hang on (an S3 key's user_id): the
	// tenant's owner member, else its registering user, else (a child: an
	// app has no member) its parent's, then its grandparent's — the account holder,
	// never the member who minted the key (a member leaves; the key stays).
	Actor string `json:"-"`
	// TenantKind is the tenant's child_kind ("" for a root);
	// ParentTenantID its parent ("" for a root) — an app's platform.
	TenantKind     string `json:"-"`
	ParentTenantID string `json:"-"`
}

// Principal is the key's name in the request log, the audit trail and the
// idempotency key: mk:<id>.
func (k *ManagementKey) Principal() string { return "mk:" + k.ID }

// Has reports whether the key carries scope.
func (k *ManagementKey) Has(scope string) bool {
	for _, s := range k.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// ManagementKeyOptions is a mint request.
type ManagementKeyOptions struct {
	Name    string
	Scopes  []string
	Ceiling *S3Ceiling
	// CreatedBy is the person who minted it ("" = a key minted it: a
	// platform's key minting its app's).
	CreatedBy string
	// Test mints the key ON the tenant's sandbox (made on first use): the
	// one way to an sk_test_ key (RP88-1). A sandbox reset or purge deletes
	// data, never keys.
	Test bool
	// ExpiresAt is the key's end (nil = none); it must be in the future.
	ExpiresAt *time.Time
}

// IsManagementKeySecret reports whether a bearer token is shaped like a
// management key (the requireCaller branch); anything else is a JWT.
func IsManagementKeySecret(token string) bool {
	return strings.HasPrefix(token, ManagementKeyLivePrefix) || strings.HasPrefix(token, ManagementKeyTestPrefix)
}

func hashManagementKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// randomBase62 is 32 random bytes in base62 (43 characters, left-padded).
func randomBase62() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("management key: random: %w", err)
	}
	n := new(big.Int).SetBytes(b)
	base := big.NewInt(62)
	mod := new(big.Int)
	out := make([]byte, 0, 43)
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		out = append(out, base62Alphabet[mod.Int64()])
	}
	for len(out) < 43 {
		out = append(out, '0')
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out), nil
}

// validScopes checks the list is non-empty, known and without repeats.
func validScopes(scopes []string) error {
	if len(scopes) == 0 {
		return fmt.Errorf("%w: at least one of manage:read, manage:write, platform, apps", ErrManagementKeyScope)
	}
	seen := map[string]bool{}
	for _, s := range scopes {
		switch s {
		case ScopeManageRead, ScopeManageWrite, ScopePlatform, ScopeApps:
		default:
			return fmt.Errorf("%w: unknown scope %q", ErrManagementKeyScope, s)
		}
		if seen[s] {
			return fmt.Errorf("%w: %q twice", ErrManagementKeyScope, s)
		}
		seen[s] = true
	}
	return nil
}

// MintManagementKey makes a key of tenantID (of its sandbox with Test) and
// returns it with its secret — the only time the secret exists outside the
// caller. platform needs a platform tenant; apps needs an app child
// (ErrManagementKeyScope) — a sandbox is neither. The live-key count runs
// under the key's tenant row FOR UPDATE, so two mints never both take the
// last place (RP88-3).
func MintManagementKey(ctx context.Context, db *sql.DB, tenantID string, o ManagementKeyOptions) (*ManagementKey, string, error) {
	if err := validScopes(o.Scopes); err != nil {
		return nil, "", err
	}
	if err := o.Ceiling.Validate(); err != nil {
		return nil, "", err
	}
	if o.ExpiresAt != nil && !o.ExpiresAt.After(time.Now()) {
		return nil, "", fmt.Errorf("%w: expires_at must be in the future", ErrManagementKeyExpiry)
	}
	name := strings.TrimSpace(o.Name)
	if len([]rune(name)) > 200 {
		return nil, "", fmt.Errorf("%w: the name is longer than 200 characters", ErrManagementKeyScope)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", fmt.Errorf("mint a management key of %s: %w", tenantID, err)
	}
	defer func() { _ = tx.Rollback() }()
	if o.Test {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, tenantID).Scan(&exists); err != nil {
			return nil, "", fmt.Errorf("mint a management key of %s: %w", tenantID, err)
		}
		if !exists {
			return nil, "", ErrTenantNotFound
		}
		sandbox, err := ensureSandboxTx(ctx, tx, tenantID)
		if err != nil {
			return nil, "", fmt.Errorf("mint a test management key of %s: %w", tenantID, err)
		}
		tenantID = sandbox
	}
	var platform bool
	var sandboxOf, kind sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT platform, sandbox_of, child_kind FROM tenants WHERE id = $1 FOR UPDATE`, tenantID).
		Scan(&platform, &sandboxOf, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrTenantNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("mint a management key of %s: %w", tenantID, err)
	}
	for _, s := range o.Scopes {
		switch {
		case s == ScopePlatform && !platform:
			return nil, "", fmt.Errorf("%w: platform is a platform account's scope", ErrManagementKeyScope)
		case s == ScopeApps && kind.String != string(ChildApp):
			return nil, "", fmt.Errorf("%w: apps is an app's scope (minted by its platform)", ErrManagementKeyScope)
		}
	}
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM management_keys WHERE tenant_id = $1 AND revoked_at IS NULL`, tenantID).Scan(&live); err != nil {
		return nil, "", fmt.Errorf("mint a management key of %s: %w", tenantID, err)
	}
	mintAfterCount()
	if live >= MaxLiveManagementKeys {
		return nil, "", ErrKeyLimitReached
	}
	prefix := ManagementKeyLivePrefix
	if sandboxOf.Valid {
		prefix = ManagementKeyTestPrefix
	}
	body, err := randomBase62()
	if err != nil {
		return nil, "", err
	}
	secret := prefix + body
	var ceiling []byte
	if o.Ceiling != nil {
		if ceiling, err = json.Marshal(o.Ceiling); err != nil {
			return nil, "", fmt.Errorf("mint a management key of %s: %w", tenantID, err)
		}
	}
	var createdBy any
	if o.CreatedBy != "" {
		createdBy = o.CreatedBy
	}
	k := &ManagementKey{TenantID: tenantID, Prefix: prefix, Hint: body[:4], Name: name,
		Scopes: append([]string(nil), o.Scopes...), Ceiling: o.Ceiling, CreatedBy: o.CreatedBy}
	var expires sql.NullTime
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO management_keys (tenant_id, prefix, key_hash, hint, name, scopes, s3_ceiling, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id::text, created_at, expires_at`,
		tenantID, prefix, hashManagementKey(secret), k.Hint, name, pq.Array(k.Scopes), nullJSON(ceiling), createdBy, o.ExpiresAt).
		Scan(&k.ID, &k.CreatedAt, &expires); err != nil {
		return nil, "", fmt.Errorf("mint a management key of %s: %w", tenantID, err)
	}
	if expires.Valid {
		k.ExpiresAt = &expires.Time
	}
	if err := tx.Commit(); err != nil {
		return nil, "", fmt.Errorf("mint a management key of %s: %w", tenantID, err)
	}
	return k, secret, nil
}

func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

const managementKeyColumns = `k.id::text, k.tenant_id, k.prefix, k.hint, k.name, k.scopes, k.s3_ceiling,
	COALESCE(k.created_by::text, ''), k.created_at, k.last_used_at, k.revoked_at, k.expires_at`

func scanManagementKey(sc interface{ Scan(...any) error }, extra ...any) (*ManagementKey, error) {
	var k ManagementKey
	var ceiling []byte
	var used, revoked, expires sql.NullTime
	dest := append([]any{&k.ID, &k.TenantID, &k.Prefix, &k.Hint, &k.Name, pq.Array(&k.Scopes), &ceiling,
		&k.CreatedBy, &k.CreatedAt, &used, &revoked, &expires}, extra...)
	if err := sc.Scan(dest...); err != nil {
		return nil, err
	}
	if len(ceiling) > 0 {
		k.Ceiling = &S3Ceiling{}
		if err := json.Unmarshal(ceiling, k.Ceiling); err != nil {
			return nil, fmt.Errorf("the S3 ceiling of management key %s: %w", k.ID, err)
		}
	}
	if used.Valid {
		k.LastUsedAt = &used.Time
	}
	if revoked.Valid {
		k.RevokedAt = &revoked.Time
	}
	if expires.Valid {
		k.ExpiresAt = &expires.Time
	}
	return &k, nil
}

// LookupManagementKey finds the live key whose secret this is, records its
// use (at most once a minute) and resolves the user its rows hang on. A
// revoked or expired key is ErrManagementKeyInvalid, as is anything else.
func LookupManagementKey(ctx context.Context, db *sql.DB, secret string) (*ManagementKey, error) {
	if !IsManagementKeySecret(secret) {
		return nil, ErrManagementKeyInvalid
	}
	var actor, kind, parent sql.NullString
	k, err := scanManagementKey(db.QueryRowContext(ctx, `
		SELECT `+managementKeyColumns+`,
		       COALESCE(
		         (SELECT m.user_id::text FROM tenant_members m WHERE m.tenant_id = t.id AND m.role = 'owner'),
		         (SELECT u.id::text FROM users u WHERE lower(u.email) = lower(t.email) ORDER BY u.created_at LIMIT 1),
		         -- a child (an app) has no member: its parent's owner, then the
		         -- grandparent's (an app's platform; depth 2 is the deepest)
		         (SELECT m.user_id::text FROM tenant_members m WHERE m.tenant_id = t.parent_tenant_id AND m.role = 'owner'),
		         (SELECT u.id::text FROM tenants p JOIN users u ON lower(u.email) = lower(p.email) WHERE p.id = t.parent_tenant_id ORDER BY u.created_at LIMIT 1),
		         (SELECT m.user_id::text FROM tenants p JOIN tenant_members m ON m.tenant_id = p.parent_tenant_id AND m.role = 'owner' WHERE p.id = t.parent_tenant_id)),
		       t.child_kind, t.parent_tenant_id
		  FROM management_keys k JOIN tenants t ON t.id = k.tenant_id
		 WHERE k.key_hash = $1 AND k.revoked_at IS NULL
		   AND (k.expires_at IS NULL OR k.expires_at > NOW())`, hashManagementKey(secret)), &actor, &kind, &parent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrManagementKeyInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("look up a management key: %w", err)
	}
	k.Actor, k.TenantKind, k.ParentTenantID = actor.String, kind.String, parent.String
	now := time.Now()
	if k.LastUsedAt == nil || now.Sub(*k.LastUsedAt) >= managementKeyUseEvery {
		if err := db.QueryRowContext(ctx, `
			UPDATE management_keys SET last_used_at = NOW()
			 WHERE id = $1::uuid AND (last_used_at IS NULL OR last_used_at < NOW() - $2::interval)
			RETURNING last_used_at`, k.ID, managementKeyUseEvery.String()).Scan(&now); err == nil {
			k.LastUsedAt = &now
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("record a management key's use: %w", err)
		}
	}
	return k, nil
}

// ownOrSandbox matches a key of the tenant ($1) or of its sandbox: the
// tenant lists and revokes its test keys (RP88-1).
const ownOrSandbox = `(k.tenant_id = $1 OR k.tenant_id IN (SELECT s.id FROM tenants s WHERE s.sandbox_of = $1))`

// ListManagementKeys lists the tenant's keys and its sandbox's, live first,
// newest first.
func ListManagementKeys(ctx context.Context, db *sql.DB, tenantID string) ([]*ManagementKey, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+managementKeyColumns+` FROM management_keys k
		 WHERE `+ownOrSandbox+` ORDER BY (k.revoked_at IS NULL) DESC, k.created_at DESC, k.id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list the management keys of %s: %w", tenantID, err)
	}
	defer func() { _ = rows.Close() }()
	out := []*ManagementKey{}
	for rows.Next() {
		k, err := scanManagementKey(rows)
		if err != nil {
			return nil, fmt.Errorf("list the management keys of %s: %w", tenantID, err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeManagementKey revokes the tenant's (or its sandbox's) live key id.
func RevokeManagementKey(ctx context.Context, db *sql.DB, tenantID, id string) error {
	res, err := db.ExecContext(ctx, `UPDATE management_keys k SET revoked_at = NOW()
		 WHERE k.id::text = $2 AND `+ownOrSandbox+` AND k.revoked_at IS NULL`, tenantID, id)
	if err != nil {
		return fmt.Errorf("revoke management key %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrManagementKeyNotFound
	}
	return nil
}
