package auth

// Test mode (WP-DX-1, Stage 1a Prompt 50; plan § WP-STRIPE): a key with the
// prefix VLT_TEST_ belongs to the tenant's SANDBOX — a tenants row whose
// sandbox_of names the parent (migration 116) — so LookupCredential, the
// one lookup, resolves it to the sandbox with no branch of its own. The
// sandbox is made at the first test key, in the key's transaction (the
// parent user's row under FOR SHARE, the tenants row, the api_keys row,
// the tenant_quotas row: the four-table rule, the users row being the
// parent's — a sandbox has no user of its own, its keys are the user's).
// What a sandbox may do is the API layer's rule (placement: the `local`
// driver only, never billed, never metered).

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FairForge/vaultaire/internal/audit"
)

// TestKeyPrefix is a test-mode access key's prefix.
const TestKeyPrefix = "VLT_TEST_"

// maxTestKeys bounds the live test keys of one sandbox.
const maxTestKeys = 10

// ErrSandboxOfSandbox: a sandbox has no sandbox of its own.
var ErrSandboxOfSandbox = errors.New("a test-mode tenant has no sandbox of its own")

// ErrTenantSuspended: the tenant is suspended — no test key is minted for
// it (RP80-2: a sandbox is suspended with its parent).
var ErrTenantSuspended = errors.New("the account is suspended")

// IsTestKey reports whether an access key id is a test-mode key.
func IsTestKey(accessKey string) bool { return strings.HasPrefix(accessKey, TestKeyPrefix) }

// queryRower is *sql.DB or *sql.Tx.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SandboxOf is the sandbox of parent ("" when it has none).
func SandboxOf(ctx context.Context, db queryRower, parent string) (string, error) {
	var id string
	err := db.QueryRowContext(ctx, `SELECT id FROM tenants WHERE sandbox_of = $1`, parent).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("the sandbox of %s: %w", parent, err)
	}
	return id, nil
}

// ParentOfSandbox is the parent of tenantID when it is a sandbox, "" when it
// is a live tenant.
func ParentOfSandbox(ctx context.Context, db queryRower, tenantID string) (string, error) {
	var parent sql.NullString
	err := db.QueryRowContext(ctx, `SELECT sandbox_of FROM tenants WHERE id = $1`, tenantID).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("is %s a sandbox: %w", tenantID, err)
	}
	return parent.String, nil
}

// GenerateTestKey mints a VLT_TEST_ key on the user's tenant's sandbox,
// creating the sandbox in the same transaction when it does not exist.
func (a *AuthService) GenerateTestKey(ctx context.Context, userID, name string, opts *KeyCreateOptions) (*APIKey, error) {
	return a.GenerateTestKeyForTenant(ctx, "", userID, name, opts)
}

// GenerateTestKeyForTenant is GenerateTestKey on tenantID's sandbox, for a
// member acting on its active tenant (WP-TEAM-1); "" = the user's own.
func (a *AuthService) GenerateTestKeyForTenant(ctx context.Context, tenantID, userID, name string, opts *KeyCreateOptions) (*APIKey, error) {
	if a.sqlDB == nil {
		return nil, errors.New("generate test key: no database")
	}
	cached, err := a.userByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("generate test key: %w", err)
	}
	if cached == nil {
		return nil, fmt.Errorf("user not found")
	}
	a.cacheMu.RLock()
	parent := cached.TenantID
	a.cacheMu.RUnlock()
	if tenantID != "" {
		parent = tenantID
	}
	if parent == "" {
		return nil, errors.New("generate test key: the user has no tenant")
	}
	if err := opts.Validate(time.Now()); err != nil {
		return nil, err
	}

	tx, err := a.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("generate test key: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM users WHERE id = $1 FOR SHARE`, userID); err != nil {
		return nil, fmt.Errorf("generate test key: lock the user: %w", err)
	}
	var sandboxOf sql.NullString
	var suspended bool
	if err := tx.QueryRowContext(ctx, `SELECT sandbox_of, suspended_at IS NOT NULL FROM tenants WHERE id = $1`, parent).
		Scan(&sandboxOf, &suspended); err != nil {
		return nil, fmt.Errorf("generate test key: read the tenant: %w", err)
	}
	if sandboxOf.String != "" {
		return nil, ErrSandboxOfSandbox
	}
	if suspended {
		return nil, ErrTenantSuspended
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenants (id, name, email, sandbox_of)
		VALUES ($1, 'test data', $2, $3)
		ON CONFLICT (sandbox_of) WHERE sandbox_of IS NOT NULL DO NOTHING`,
		"sandbox-"+hex.EncodeToString(b[:]), "sandbox+"+parent+"@sandbox.invalid", parent); err != nil {
		return nil, fmt.Errorf("generate test key: the sandbox: %w", err)
	}
	sandbox, err := SandboxOf(ctx, tx, parent)
	if err != nil {
		return nil, err
	}
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE tenant_id = $1 AND revoked_at IS NULL`, sandbox).Scan(&live); err != nil {
		return nil, fmt.Errorf("generate test key: count: %w", err)
	}
	if live >= maxTestKeys {
		return nil, fmt.Errorf("%w: %d test keys", ErrKeyLimitReached, maxTestKeys)
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
		ID: uuid.New().String(), UserID: userID, TenantID: sandbox, Name: name,
		Key: TestKeyPrefix + strings.TrimPrefix(accessKey, "VLT_"), Secret: secretKey, Hash: hash,
		Permissions: []string{"*"}, CreatedAt: time.Now(), Metadata: map[string]string{"mode": "test"},
	}
	if opts != nil {
		if len(opts.Permissions) > 0 {
			k.Permissions = opts.Permissions
		}
		k.BucketScope, k.IPAllowlist, k.ExpiresAt = opts.BucketScope, opts.IPAllowlist, opts.ExpiresAt
	}
	if _, err := a.persistAPIKey(ctx, tx, k); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tenant_quotas (tenant_id) VALUES ($1) ON CONFLICT (tenant_id) DO NOTHING`, sandbox); err != nil {
		return nil, fmt.Errorf("generate test key: the sandbox's quota row: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("generate test key: %w", err)
	}
	a.indexAPIKey(k)
	a.record(ctx, audit.Entry{UserID: userID, TenantID: parent, Action: "key.created", Resource: "key:" + k.ID,
		Metadata: map[string]any{"name": k.Name, "mode": "test", "sandbox": sandbox, "permissions": k.Permissions}})
	return k, nil
}
