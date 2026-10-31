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
// The tree (WP-ACCT-1, Stage 1a Prompt 53; docs/ACCOUNTS.md): a child hangs
// off its parent by an edge, tenants.child_kind — managed (the default) or
// app. An app may have managed children of its own: depth 2, the only depth
// beyond 1, made by the platform (parent_id = the app). Every tenant of the
// tree names the platform as its pool_root_id (migration 120). A sandbox,
// a managed child or an app's child never has children, and no child is
// ever made a platform. A sponsored edge is a person's own account a sponsor
// pays for (WP-FAMILY-1): never made here. A child has no primary pair:
// every key of a child is a scoped key the platform mints and may revoke
// (internal/api). A child may carry caps inside the pool: storage and
// egress bytes, 0 = the pool decides.

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
	// ErrBadChildKind: a child is made managed or app (a sponsored edge is a
	// family invitation's, never this call's).
	ErrBadChildKind = errors.New("kind must be managed or app")
	// ErrBadCap: a cap is a byte count, 0 or more.
	ErrBadCap = errors.New("a cap is 0 (the pool decides) or a positive number of bytes")
	// ErrBadResidency: a residency is eu, uk or us.
	ErrBadResidency = errors.New("residency must be eu, uk or us")
	// ErrResidencyWidens: a child names a residency other than its parent's
	// (a child narrows its parent's residency, never widens it).
	ErrResidencyWidens = errors.New("a child's residency must be its parent's")
)

// ValidResidency reports whether r is a residency a tenant may hold
// (tenants.residency, migration 120); "" is none (anywhere).
func ValidResidency(r string) bool { return r == "eu" || r == "uk" || r == "us" }

// ChildKind is the edge a child hangs off its parent by (tenants.child_kind).
type ChildKind string

// The edges (docs/ACCOUNTS.md).
const (
	ChildManaged   ChildKind = "managed"
	ChildApp       ChildKind = "app"
	ChildSponsored ChildKind = "sponsored"
)

// ChildCaps are a child's own caps inside its pool: 0 = the pool decides.
type ChildCaps struct {
	StorageBytes int64 `json:"storage_cap_bytes"`
	EgressBytes  int64 `json:"egress_cap_bytes"`
}

func (c ChildCaps) valid() bool { return c.StorageBytes >= 0 && c.EgressBytes >= 0 }

// ChildOptions are a child's edge, its parent below the platform (an app of
// the platform's, for depth 2) and its caps.
type ChildOptions struct {
	Kind     ChildKind // "" = managed
	ParentID string    // "" = the platform itself
	Caps     ChildCaps
	// Residency is where its bytes may be stored at rest (eu | uk | us;
	// "" = its parent's, or anywhere). A child inherits its parent's
	// residency and may not name another (WP-ACCT-1).
	Residency string
	// SelfServe makes an app child of a root that is no platform (WP-APP-1,
	// F-43): a verified, paying account, up to MaxSelfServeApps live apps.
	SelfServe bool
}

// MaxSelfServeApps bounds the live apps a self-serve builder holds (F-43).
var MaxSelfServeApps = 25

var (
	// ErrAppsNeedVerified: a self-serve app needs the builder's e-mail
	// verified.
	ErrAppsNeedVerified = errors.New("apps need a verified account")
	// ErrAppsNeedPaying: a self-serve app needs a paying account.
	ErrAppsNeedPaying = errors.New("apps need a paying account")
	// ErrAppLimit: the builder holds MaxSelfServeApps live apps.
	ErrAppLimit = errors.New("app limit reached")
)

// The predicate "tenant t is in the tree under the platform $n" (a child,
// or an app's child): the platform's reach over its tenants, at any depth
// the tree allows. A SPONSORED child is outside it: its sponsor never mints
// its keys, reads it or erases it (docs/ACCOUNTS.md). Constants (no SQL is built at run time), one per
// parameter position.
const (
	DescendantT1 = `(COALESCE(t.child_kind, 'managed') <> 'sponsored' AND (t.parent_tenant_id = $1 OR t.parent_tenant_id IN (SELECT a.id FROM tenants a WHERE a.parent_tenant_id = $1 AND a.child_kind = 'app')))`
	DescendantT2 = `(COALESCE(t.child_kind, 'managed') <> 'sponsored' AND (t.parent_tenant_id = $2 OR t.parent_tenant_id IN (SELECT a.id FROM tenants a WHERE a.parent_tenant_id = $2 AND a.child_kind = 'app')))`
	DescendantT3 = `(COALESCE(t.child_kind, 'managed') <> 'sponsored' AND (t.parent_tenant_id = $3 OR t.parent_tenant_id IN (SELECT a.id FROM tenants a WHERE a.parent_tenant_id = $3 AND a.child_kind = 'app')))`
)

// MaxChildLabel bounds a child's label and external id (runes).
const MaxChildLabel = 200

// ChildTenant is one child of a platform.
type ChildTenant struct {
	ID         string
	ParentID   string
	Label      string
	ExternalID string
	Kind       ChildKind
	Caps       ChildCaps
	Residency  string
}

// SetChildCaps sets a child's caps inside its pool (0 = the pool decides),
// for a tenant in the platform's tree, with its audit row. ErrTenantNotFound
// for any other tenant; ErrBadCap.
func SetChildCaps(ctx context.Context, db *sql.DB, platformID, childID string, storage, egress *int64) (ChildCaps, error) {
	var caps ChildCaps
	if (storage != nil && *storage < 0) || (egress != nil && *egress < 0) {
		return caps, ErrBadCap
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return caps, fmt.Errorf("set caps of %s: %w", childID, err)
	}
	defer func() { _ = tx.Rollback() }()
	err = tx.QueryRowContext(ctx, `
		UPDATE tenant_quotas q
		   SET storage_limit_bytes = COALESCE($3, q.storage_limit_bytes),
		       egress_cap_bytes = COALESCE($4, q.egress_cap_bytes), updated_at = NOW()
		  FROM tenants t
		 WHERE q.tenant_id = t.id AND t.id = $1 AND `+DescendantT2+`
		RETURNING q.storage_limit_bytes, q.egress_cap_bytes`, childID, platformID, storage, egress).Scan(&caps.StorageBytes, &caps.EgressBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return caps, ErrTenantNotFound
	}
	if err != nil {
		return caps, fmt.Errorf("set caps of %s: %w", childID, err)
	}
	if err := audit.RecordTx(ctx, tx, audit.Entry{TenantID: platformID, EventType: "platform", Action: "platform.tenant_caps_set",
		Resource: "tenant:" + childID, Metadata: map[string]any{"child": childID, "storage_cap_bytes": caps.StorageBytes,
			"egress_cap_bytes": caps.EgressBytes}}); err != nil {
		return caps, fmt.Errorf("set caps of %s: %w", childID, err)
	}
	return caps, tx.Commit()
}

// childEmail is a child's placeholder e-mail (tenants.email is unique and
// NOT NULL; a child has no user, so nothing ever mails it).
func childEmail(id string) string { return "platform+" + id + "@platform.invalid" }

// CreateChildTenant makes a child of the platform parentTenantID in one
// transaction (the file comment); opts (at most one) names its edge, an app
// of the platform's to hang it under, and its caps. ErrNotPlatform,
// ErrChildOfChild, ErrTenantSuspended, ErrTenantNotFound,
// ErrExternalIDTaken, ErrBadChildLabel, ErrBadExternalID, ErrBadChildKind,
// ErrBadCap.
func CreateChildTenant(ctx context.Context, db *sql.DB, parentTenantID, label, externalID string, opts ...ChildOptions) (*ChildTenant, error) {
	var o ChildOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Kind == "" {
		o.Kind = ChildManaged
	}
	label = strings.TrimSpace(label)
	externalID = strings.TrimSpace(externalID)
	switch {
	case label == "" || utf8.RuneCountInString(label) > MaxChildLabel:
		return nil, ErrBadChildLabel
	case utf8.RuneCountInString(externalID) > MaxChildLabel:
		return nil, ErrBadExternalID
	case o.Kind != ChildManaged && o.Kind != ChildApp:
		return nil, ErrBadChildKind
	case !o.Caps.valid():
		return nil, ErrBadCap
	case o.Residency != "" && !ValidResidency(o.Residency):
		return nil, ErrBadResidency
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create child of %s: %w", parentTenantID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var platform, suspended, verified bool
	var sandboxOf, parentOf sql.NullString
	var tier string
	// A self-serve app's count runs under the builder's row FOR UPDATE: two
	// creates never both take the last place.
	lock := "FOR SHARE OF t"
	if o.SelfServe {
		lock = "FOR UPDATE OF t"
	}
	err = tx.QueryRowContext(ctx, `
		SELECT t.platform, t.suspended_at IS NOT NULL, t.sandbox_of, t.parent_tenant_id,
		       COALESCE((SELECT q.tier FROM tenant_quotas q WHERE q.tenant_id = t.id), 'free'),
		       EXISTS (SELECT 1 FROM users u WHERE lower(u.email) = lower(t.email) AND COALESCE(u.email_verified, FALSE))
		  FROM tenants t WHERE t.id = $1 `+lock, parentTenantID).
		Scan(&platform, &suspended, &sandboxOf, &parentOf, &tier, &verified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("create child of %s: read the parent: %w", parentTenantID, err)
	}
	switch {
	case sandboxOf.String != "" || parentOf.String != "":
		return nil, ErrChildOfChild
	case suspended:
		return nil, ErrTenantSuspended
	case o.SelfServe:
		// WP-APP-1 (F-43): no platform grant; a verified, paying root.
		if o.Kind != ChildApp || o.ParentID != "" {
			return nil, ErrBadChildKind
		}
		if !verified {
			return nil, ErrAppsNeedVerified
		}
		if tier == "" || tier == "free" {
			return nil, ErrAppsNeedPaying
		}
		var apps int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenants WHERE parent_tenant_id = $1 AND child_kind = 'app'
			 AND deletion_scheduled_at IS NULL`, parentTenantID).Scan(&apps); err != nil {
			return nil, fmt.Errorf("create child of %s: count the apps: %w", parentTenantID, err)
		}
		if apps >= MaxSelfServeApps {
			return nil, ErrAppLimit
		}
	case !platform && o.ParentID == "":
		return nil, ErrNotPlatform
		// A non-platform's depth 2 is its self-serve app's own end user (the
		// app's key on the platform routes): checked as an app of it below.
	}
	platformID, parent := parentTenantID, parentTenantID
	if o.ParentID != "" && o.ParentID != platformID {
		// Depth 2: under an app of this platform's, and only a managed child.
		var appOf sql.NullString
		var kind sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT parent_tenant_id, child_kind FROM tenants WHERE id = $1 FOR SHARE`, o.ParentID).Scan(&appOf, &kind)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && appOf.String != platformID) {
			return nil, ErrTenantNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("create child of %s: read the app: %w", o.ParentID, err)
		}
		if kind.String != string(ChildApp) || o.Kind != ChildManaged {
			return nil, ErrChildOfChild
		}
		parent = o.ParentID
	}
	// The residency: the parent's, which the child may name but not change
	// (every residency is one jurisdiction, so narrowing it is keeping it);
	// a parent with none lets the child choose one.
	var parentResidency sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT residency FROM tenants WHERE id = $1`, parent).Scan(&parentResidency); err != nil {
		return nil, fmt.Errorf("create child of %s: read the residency: %w", parent, err)
	}
	switch {
	case parentResidency.String == "":
	case o.Residency == "":
		o.Residency = parentResidency.String
	case o.Residency != parentResidency.String:
		return nil, ErrResidencyWidens
	}

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	c := &ChildTenant{ID: "tenant-" + hex.EncodeToString(b[:]), ParentID: parent, Label: label, ExternalID: externalID,
		Kind: o.Kind, Caps: o.Caps, Residency: o.Residency}
	ext := sql.NullString{String: externalID, Valid: externalID != ""}
	// pool_root_id is the platform's root (itself: a platform is a root).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenants (id, name, email, parent_tenant_id, external_id, label, child_kind, pool_root_id, residency)
		VALUES ($1, $2, $3, $4, $5, $6, $7, (SELECT p.pool_root_id FROM tenants p WHERE p.id = $8), $9)`,
		c.ID, label, childEmail(c.ID), parent, ext, label, string(o.Kind), platformID,
		sql.NullString{String: o.Residency, Valid: o.Residency != ""}); err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" && pqErr.Constraint == "idx_tenants_parent_external" {
			return nil, ErrExternalIDTaken
		}
		return nil, fmt.Errorf("create child of %s: the tenants row: %w", parentTenantID, err)
	}
	// Limit = the child's own storage cap (0: the pool decides — the
	// root's limit, the pool's counter), and its egress cap. The tier is
	// the platform's: a child is its customer, not a free account of its own.
	if _, err := tx.ExecContext(ctx, `INSERT INTO tenant_quotas (tenant_id, storage_limit_bytes, egress_cap_bytes, tier) VALUES ($1, $2, $3, $4)`,
		c.ID, o.Caps.StorageBytes, o.Caps.EgressBytes, tier); err != nil {
		return nil, fmt.Errorf("create child of %s: the quota row: %w", parentTenantID, err)
	}
	// A counting floor row (limit 0) per floor the parent bought (RP82-1):
	// the child's bytes on a floor count against the PARENT's floor limit
	// (usage.ReserveTx sums the pool's rows). A parent with no house gives
	// none; usage.SetHouse adds them when it buys one.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenant_floor_quotas (tenant_id, floor, storage_limit_bytes, storage_used_bytes)
		SELECT $1, floor, 0, 0 FROM tenant_floor_quotas WHERE tenant_id = $2`, c.ID, platformID); err != nil {
		return nil, fmt.Errorf("create child of %s: the floor rows: %w", parentTenantID, err)
	}
	if err := audit.RecordTx(ctx, tx, audit.Entry{TenantID: parentTenantID, EventType: "platform",
		Action: "platform.tenant_created", Resource: "tenant:" + c.ID,
		Metadata: map[string]any{"child": c.ID, "label": label, "external_id": externalID, "kind": string(o.Kind),
			"parent": parent, "storage_cap_bytes": o.Caps.StorageBytes, "egress_cap_bytes": o.Caps.EgressBytes,
			"residency": o.Residency}}); err != nil {
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
