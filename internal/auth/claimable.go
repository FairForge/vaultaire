// internal/auth/claimable.go
//
// Claimable apps (WP-APP-1 PR D; docs/ACCOUNTS.md § Apps; F-42;
// migration 122): the integration path for AI app builders. A PARTNER
// (tenants.partner, an operator's grant) makes, with a management key of
// scope partner, a provisional tenant and a single-use claim link (only its
// sha256 kept). While provisional the tenant is a managed child of the
// partner — inside the partner's pool, 1 GiB, never Public, 7 days — so the
// partner is accountable for what its builder stores, and an unclaimed one
// is erased as any due child is. A person who claims it keeps it, its data
// and its keys, as an app under their own account: ClaimProvisional, ONE
// transaction.
package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/usage"
)

// ProvisionalLifetime and ProvisionalStorageBytes are a claimable's (F-42's
// recommendation).
const (
	ProvisionalLifetime     = 7 * 24 * time.Hour
	ProvisionalStorageBytes = 1 << 30
)

var (
	// ErrNotPartner: the tenant is no partner.
	ErrNotPartner = errors.New("not a partner")
	// ErrClaimGone: the link is unknown, used, expired, or its tenant is
	// gone (never said which).
	ErrClaimGone = errors.New("claim link gone")
	// ErrClaimNeedsVerified: a claimer's e-mail must be verified.
	ErrClaimNeedsVerified = errors.New("claim needs a verified account")
)

// SetPartner grants (or revokes) the partner scope of a root tenant.
func SetPartner(ctx context.Context, db *sql.DB, tenantID string, partner bool) error {
	res, err := db.ExecContext(ctx, `UPDATE tenants SET partner = $2
		 WHERE id = $1 AND parent_tenant_id IS NULL AND sandbox_of IS NULL`, tenantID, partner)
	if err != nil {
		return fmt.Errorf("set partner %s: %w", tenantID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists bool
		_ = db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, tenantID).Scan(&exists)
		if !exists {
			return ErrTenantNotFound
		}
		return ErrChildOfChild
	}
	return nil
}

// Claimable is a new provisional tenant and its secrets (shown once).
type Claimable struct {
	TenantID  string
	ClaimID   string
	Label     string
	ExpiresAt time.Time
	Token     string // the claim link's token
	Key       *ManagementKey
	KeySecret string // the builder's key on the provisional tenant
}

// CreateClaimable makes a provisional tenant of partnerID with its claim
// link and a management key (manage:write) its builder stores with.
func CreateClaimable(ctx context.Context, db *sql.DB, partnerID, label, createdBy string) (*Claimable, error) {
	label = strings.TrimSpace(label)
	if label == "" || utf8.RuneCountInString(label) > MaxChildLabel {
		return nil, ErrBadChildLabel
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claimable of %s: %w", partnerID, err)
	}
	defer func() { _ = tx.Rollback() }()
	var partner, suspended bool
	var parent, sandbox sql.NullString
	var residency sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT partner, suspended_at IS NOT NULL, parent_tenant_id, sandbox_of, residency
		  FROM tenants WHERE id = $1 FOR SHARE`, partnerID).Scan(&partner, &suspended, &parent, &sandbox, &residency)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrTenantNotFound
	case err != nil:
		return nil, fmt.Errorf("claimable of %s: %w", partnerID, err)
	case !partner || parent.Valid || sandbox.Valid:
		return nil, ErrNotPartner
	case suspended:
		return nil, ErrTenantSuspended
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	c := &Claimable{TenantID: "tenant-" + hex.EncodeToString(b[:]), Label: label, ExpiresAt: time.Now().Add(ProvisionalLifetime).UTC(),
		Token: hex.EncodeToString(tok)}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenants (id, name, email, parent_tenant_id, label, child_kind, pool_root_id, residency, provisional_until, provisional_partner)
		VALUES ($1, $2::varchar, $3, $4::varchar, $2::varchar, 'managed', (SELECT p.pool_root_id FROM tenants p WHERE p.id = $4::varchar), $5, $6, $4::varchar)`,
		c.TenantID, label, childEmail(c.TenantID), partnerID, residency, c.ExpiresAt); err != nil {
		return nil, fmt.Errorf("claimable of %s: the tenant: %w", partnerID, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, tier) VALUES ($1, $2, 'free')`,
		c.TenantID, int64(ProvisionalStorageBytes)); err != nil {
		return nil, fmt.Errorf("claimable of %s: the quota: %w", partnerID, err)
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO claims (id, tenant_id, partner_tenant_id, token_hash, label, expires_at)
		VALUES ('clm_' || substr(md5(random()::text), 1, 20), $1, $2, $3, $4, $5) RETURNING id`,
		c.TenantID, partnerID, oauthSHA256Hex(c.Token), label, c.ExpiresAt).Scan(&c.ClaimID); err != nil {
		return nil, fmt.Errorf("claimable of %s: the claim: %w", partnerID, err)
	}
	expires := c.ExpiresAt
	c.Key, c.KeySecret, err = mintManagementKeyTx(ctx, tx, c.TenantID, ManagementKeyOptions{Name: "builder: " + label,
		Scopes: []string{ScopeManageWrite}, CreatedBy: createdBy, ExpiresAt: &expires})
	if err != nil {
		return nil, err
	}
	if err := audit.RecordTx(ctx, tx, audit.Entry{TenantID: partnerID, EventType: "partner", Action: "partner.claimable_created",
		Resource: "tenant:" + c.TenantID, Metadata: map[string]any{"claim_id": c.ClaimID, "label": label, "expires_at": c.ExpiresAt}}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("claimable of %s: %w", partnerID, err)
	}
	return c, nil
}

// ClaimTarget is what a link names, for the claim page.
type ClaimTarget struct {
	TenantID, Label, PartnerName string
	ExpiresAt                    time.Time
	UsedBytes                    int64
}

// LookupClaim reads a live link (ErrClaimGone otherwise).
func LookupClaim(ctx context.Context, db *sql.DB, token string) (*ClaimTarget, error) {
	var c ClaimTarget
	err := db.QueryRowContext(ctx, `
		SELECT c.tenant_id, c.label, COALESCE(p.name, ''), c.expires_at, COALESCE(q.storage_used_bytes, 0)
		  FROM claims c JOIN tenants t ON t.id = c.tenant_id
		  LEFT JOIN tenants p ON p.id = c.partner_tenant_id
		  LEFT JOIN tenant_quotas q ON q.tenant_id = c.tenant_id
		 WHERE c.token_hash = $1 AND c.claimed_at IS NULL AND c.expires_at > NOW()
		   AND t.provisional_until IS NOT NULL AND t.deletion_scheduled_at IS NULL`, oauthSHA256Hex(token)).
		Scan(&c.TenantID, &c.Label, &c.PartnerName, &c.ExpiresAt, &c.UsedBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrClaimGone
	}
	if err != nil {
		return nil, fmt.Errorf("read a claim: %w", err)
	}
	return &c, nil
}

// ClaimProvisional moves the link's tenant under claimerID — a verified root
// — as an app child, with its data, in ONE transaction: the tenant
// re-parented (child_kind app, the claimer's pool root, no longer
// provisional), the pool counter moved (usage.MovePoolTx), its S3 keys
// handed to the claimer's user (they stay the tenant's), the link spent.
func ClaimProvisional(ctx context.Context, db *sql.DB, token, claimerID, userID string) (*ClaimTarget, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var c ClaimTarget
	var claimID, partnerID string
	err = tx.QueryRowContext(ctx, `
		SELECT c.id, c.tenant_id, c.label, c.partner_tenant_id, c.expires_at
		  FROM claims c JOIN tenants t ON t.id = c.tenant_id
		 WHERE c.token_hash = $1 AND c.claimed_at IS NULL AND c.expires_at > NOW()
		   AND t.provisional_until IS NOT NULL AND t.deletion_scheduled_at IS NULL
		   FOR UPDATE OF c, t`, oauthSHA256Hex(token)).Scan(&claimID, &c.TenantID, &c.Label, &partnerID, &c.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrClaimGone
	}
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	var verified, root bool
	var claimerRoot string
	err = tx.QueryRowContext(ctx, `
		SELECT t.parent_tenant_id IS NULL AND t.sandbox_of IS NULL AND t.provisional_until IS NULL, COALESCE(t.pool_root_id, t.id),
		       EXISTS (SELECT 1 FROM users u WHERE u.id::text = $2 AND COALESCE(u.email_verified, FALSE))
		  FROM tenants t WHERE t.id = $1 FOR SHARE`, claimerID, userID).Scan(&root, &claimerRoot, &verified)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrTenantNotFound
	case err != nil:
		return nil, fmt.Errorf("claim: the claimer: %w", err)
	case !root:
		return nil, ErrChildOfChild
	case !verified:
		return nil, ErrClaimNeedsVerified
	}
	var fromRoot string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(pool_root_id, $2) FROM tenants WHERE id = $1`, c.TenantID, partnerID).Scan(&fromRoot); err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	if c.UsedBytes, err = usage.MovePoolTx(ctx, tx, c.TenantID, fromRoot, claimerRoot); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tenants SET parent_tenant_id = $2, child_kind = 'app', pool_root_id = $3,
		       provisional_until = NULL, provisional_partner = NULL
		 WHERE id = $1`, c.TenantID, claimerID, claimerRoot); err != nil {
		return nil, fmt.Errorf("claim: re-parent: %w", err)
	}
	// Its keys stay the tenant's; the user their rows hang on is the
	// claimer now (the partner's person no longer stands behind them).
	if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET user_id = $2::uuid WHERE tenant_id = $1`, c.TenantID, userID); err != nil {
		return nil, fmt.Errorf("claim: the keys: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE claims SET claimed_at = NOW(), claimed_by = $2 WHERE id = $1`, claimID, claimerID); err != nil {
		return nil, fmt.Errorf("claim: spend the link: %w", err)
	}
	for _, e := range []audit.Entry{
		{TenantID: claimerID, EventType: "app", Action: "app.claimed", Resource: "tenant:" + c.TenantID,
			Metadata: map[string]any{"claim_id": claimID, "label": c.Label, "used_bytes": c.UsedBytes}},
		{TenantID: partnerID, EventType: "partner", Action: "partner.claimable_claimed", Resource: "tenant:" + c.TenantID,
			Metadata: map[string]any{"claim_id": claimID}},
	} {
		if err := audit.RecordTx(ctx, tx, e); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	return &c, nil
}

// ExpiredClaimables schedules every provisional tenant past its 7 days for
// erasure now (the deletion runner erases due children) and revokes its
// keys: unclaimed, it is purged. Returns how many.
func ExpiredClaimables(ctx context.Context, db *sql.DB) (int, error) {
	rows, err := db.QueryContext(ctx, `
		UPDATE tenants SET deletion_scheduled_at = NOW()
		 WHERE provisional_until IS NOT NULL AND provisional_until <= NOW() AND deletion_scheduled_at IS NULL
		   AND parent_tenant_id IS NOT NULL
		RETURNING id`)
	if err != nil {
		return 0, fmt.Errorf("expire claimables: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("expire claimables: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("expire claimables: %w", err)
	}
	_ = rows.Close()
	for _, id := range ids {
		if _, err := db.ExecContext(ctx, `UPDATE management_keys SET revoked_at = NOW() WHERE tenant_id = $1 AND revoked_at IS NULL`, id); err != nil {
			return len(ids), fmt.Errorf("expire claimable %s: the keys: %w", id, err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE api_keys SET revoked_at = NOW() WHERE tenant_id = $1 AND revoked_at IS NULL`, id); err != nil {
			return len(ids), fmt.Errorf("expire claimable %s: the keys: %w", id, err)
		}
	}
	return len(ids), nil
}
