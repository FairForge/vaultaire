package auth

// Platform tenants (WP-PLAT-1, Stage 1a Prompt 51; plan § WP-STRIPE — the
// Connect primitive). A tenant with tenants.platform = true (granted by an
// admin) creates CHILD tenants by API: a tenants row whose parent_tenant_id
// names it (migration 117), with no users row and no e-mail of its own (a
// platform+<id>@platform.invalid placeholder — tenants.email is unique, as
// the sandbox's is), a label and an optional external id the platform's
// app owns (unique per parent), and a tenant_quotas row of limit 0: the
// pool decides (internal/usage — the child's bytes count against its
// parent's limit). A child is made in ONE transaction — the parent row
// FOR SHARE, the tenants row, the quota row, the audit row — so a child is
// never half-made and a revoked or suspended parent makes none.
//
// One level only: a child, or a sandbox, never has children (and is never
// made a platform). A child has no primary pair: every key of a child is a
// scoped key the platform mints and may revoke (internal/api).

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/lib/pq"

	"github.com/FairForge/vaultaire/internal/audit"
)

var (
	// ErrChildOfChild: a child (or a sandbox) never has children of its own
	// and is never a platform.
	ErrChildOfChild = errors.New("a child tenant cannot have children of its own")
	// ErrNotPlatform: the tenant is not a platform (tenants.platform).
	ErrNotPlatform = errors.New("the tenant is not a platform")
	// ErrExternalIDTaken: the parent already has a child with that external id.
	ErrExternalIDTaken = errors.New("external_id is already used by another child of this platform")
	// ErrTenantNotFound: no such tenant.
	ErrTenantNotFound = errors.New("tenant not found")
	// ErrBadChildLabel: the label is empty or too long.
	ErrBadChildLabel = errors.New("label must be 1-200 characters")
	// ErrBadExternalID: the external id is too long.
	ErrBadExternalID = errors.New("external_id must be at most 200 characters")
)

// MaxChildLabel bounds a child's label and external id (runes).
const MaxChildLabel = 200

// ChildTenant is one child of a platform.
type ChildTenant struct {
	ID         string
	ParentID   string
	Label      string
	ExternalID string
}

// childEmail is a child's placeholder e-mail (tenants.email is unique and
// NOT NULL; a child has no user, so nothing ever mails it).
func childEmail(id string) string { return "platform+" + id + "@platform.invalid" }

// CreateChildTenant makes a child of the platform parentTenantID in one
// transaction (the file comment). ErrNotPlatform, ErrChildOfChild,
// ErrTenantSuspended, ErrTenantNotFound, ErrExternalIDTaken,
// ErrBadChildLabel, ErrBadExternalID.
func CreateChildTenant(ctx context.Context, db *sql.DB, parentTenantID, label, externalID string) (*ChildTenant, error) {
	label = strings.TrimSpace(label)
	externalID = strings.TrimSpace(externalID)
	if label == "" || utf8.RuneCountInString(label) > MaxChildLabel {
		return nil, ErrBadChildLabel
	}
	if utf8.RuneCountInString(externalID) > MaxChildLabel {
		return nil, ErrBadExternalID
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create child of %s: %w", parentTenantID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var platform, suspended bool
	var sandboxOf, parentOf sql.NullString
	var tier string
	err = tx.QueryRowContext(ctx, `
		SELECT t.platform, t.suspended_at IS NOT NULL, t.sandbox_of, t.parent_tenant_id,
		       COALESCE((SELECT q.tier FROM tenant_quotas q WHERE q.tenant_id = t.id), 'free')
		  FROM tenants t WHERE t.id = $1 FOR SHARE OF t`, parentTenantID).
		Scan(&platform, &suspended, &sandboxOf, &parentOf, &tier)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("create child of %s: read the parent: %w", parentTenantID, err)
	}
	switch {
	case sandboxOf.String != "" || parentOf.String != "":
		return nil, ErrChildOfChild
	case !platform:
		return nil, ErrNotPlatform
	case suspended:
		return nil, ErrTenantSuspended
	}

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	c := &ChildTenant{ID: "tenant-" + hex.EncodeToString(b[:]), ParentID: parentTenantID, Label: label, ExternalID: externalID}
	ext := sql.NullString{String: externalID, Valid: externalID != ""}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenants (id, name, email, parent_tenant_id, external_id, label)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		c.ID, label, childEmail(c.ID), parentTenantID, ext, label); err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" && pqErr.Constraint == "idx_tenants_parent_external" {
			return nil, ErrExternalIDTaken
		}
		return nil, fmt.Errorf("create child of %s: the tenants row: %w", parentTenantID, err)
	}
	// Limit 0: the pool decides (the parent's limit, the pool's sum). The
	// tier is the parent's: a child is the parent's customer, not a free
	// account of its own.
	if _, err := tx.ExecContext(ctx, `INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, tier) VALUES ($1, 0, $2)`, c.ID, tier); err != nil {
		return nil, fmt.Errorf("create child of %s: the quota row: %w", parentTenantID, err)
	}
	// A counting floor row (limit 0) per floor the parent bought (RP82-1):
	// the child's bytes on a floor count against the PARENT's floor limit
	// (usage.ReserveTx sums the pool's rows). A parent with no house gives
	// none; usage.SetHouse adds them when it buys one.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenant_floor_quotas (tenant_id, floor, storage_limit_bytes, storage_used_bytes)
		SELECT $1, floor, 0, 0 FROM tenant_floor_quotas WHERE tenant_id = $2`, c.ID, parentTenantID); err != nil {
		return nil, fmt.Errorf("create child of %s: the floor rows: %w", parentTenantID, err)
	}
	if err := audit.RecordTx(ctx, tx, audit.Entry{TenantID: parentTenantID, EventType: "platform",
		Action: "platform.tenant_created", Resource: "tenant:" + c.ID,
		Metadata: map[string]any{"child": c.ID, "label": label, "external_id": externalID}}); err != nil {
		return nil, fmt.Errorf("create child of %s: %w", parentTenantID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create child of %s: %w", parentTenantID, err)
	}
	return c, nil
}

// ParentOfChild is the platform of tenantID when it is a child, "" when it
// is not.
func ParentOfChild(ctx context.Context, db queryRower, tenantID string) (string, error) {
	var parent sql.NullString
	err := db.QueryRowContext(ctx, `SELECT parent_tenant_id FROM tenants WHERE id = $1`, tenantID).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("is %s a child: %w", tenantID, err)
	}
	return parent.String, nil
}

// IsPlatform reports whether tenantID is a platform.
func IsPlatform(ctx context.Context, db queryRower, tenantID string) (bool, error) {
	var platform bool
	err := db.QueryRowContext(ctx, `SELECT platform FROM tenants WHERE id = $1`, tenantID).Scan(&platform)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("is %s a platform: %w", tenantID, err)
	}
	return platform, nil
}

// SetPlatform grants (or revokes) the platform flag, in one transaction
// with its audit row (admin.platform_granted / admin.platform_revoked, the
// context's actor). ErrTenantNotFound; ErrChildOfChild for a child or a
// sandbox; a revoke while children remain is ErrPlatformHasChildren.
func SetPlatform(ctx context.Context, db *sql.DB, tenantID string, platform bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set platform %s: %w", tenantID, err)
	}
	defer func() { _ = tx.Rollback() }()
	var sandboxOf, parentOf sql.NullString
	var was bool
	err = tx.QueryRowContext(ctx, `SELECT sandbox_of, parent_tenant_id, platform FROM tenants WHERE id = $1 FOR UPDATE`, tenantID).
		Scan(&sandboxOf, &parentOf, &was)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTenantNotFound
	}
	if err != nil {
		return fmt.Errorf("set platform %s: %w", tenantID, err)
	}
	if sandboxOf.String != "" || parentOf.String != "" {
		return ErrChildOfChild
	}
	if !platform {
		var children int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenants WHERE parent_tenant_id = $1`, tenantID).Scan(&children); err != nil {
			return fmt.Errorf("set platform %s: count children: %w", tenantID, err)
		}
		if children > 0 {
			return fmt.Errorf("%w: %d", ErrPlatformHasChildren, children)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tenants SET platform = $2 WHERE id = $1`, tenantID, platform); err != nil {
		return fmt.Errorf("set platform %s: %w", tenantID, err)
	}
	action := "admin.platform_granted"
	if !platform {
		action = "admin.platform_revoked"
	}
	if err := audit.RecordTx(ctx, tx, audit.Entry{TenantID: tenantID, EventType: "admin", Action: action,
		Resource: "tenant:" + tenantID, Metadata: map[string]any{"platform": platform, "was": was}}); err != nil {
		return fmt.Errorf("set platform %s: %w", tenantID, err)
	}
	return tx.Commit()
}

// ErrPlatformHasChildren: the platform flag is not revoked while children
// remain (they would be a pool with no platform to manage it).
var ErrPlatformHasChildren = errors.New("the platform still has child tenants")
