// internal/auth/agent_grants.go
//
// The agent grant (WP-AGENT-1, Stage 1a Prompt 55 PR A; docs/ACCOUNTS.md
// § Agents; F-41; migration 122). An agent is never an account: it acts
// for a person, who stays accountable. A grant IS a management key
// (WP-KEY-1) — never a second credential kind — plus a row that says who
// it acts for (granted_by), which agent and OAuth client it is, what the
// consent approved, and the caps every S3 key it mints carries. Every grant
// comes with a TEST key on the account's sandbox (RP88-1), so the agent can
// rehearse before it touches live data. The key's expires_at is the
// grant's lifetime: 30 days by default, never longer; a renewal pushes it
// and widens nothing.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lib/pq"
)

// AgentGrantLifetime is a grant's default and longest life (F-41).
const AgentGrantLifetime = 30 * 24 * time.Hour

// AgentScopes is the default agent scope: the developer's writes on
// /api/v1/manage, which the approval gate narrows further (approvals).
var AgentScopes = []string{ScopeManageWrite}

// AgentS3Permissions is the ceiling of every S3 key an agent grant mints:
// every operation but what could lose or expose data, which the person
// keeps (F-41): deleting a bucket, turning its versioning off, making
// anything public, Object Lock and retention, and the governance bypass.
var AgentS3Permissions = func() []string {
	never := map[string]bool{OpDeleteBucket: true, OpPutBucketVersioning: true, OpPutBucketAcl: true, OpPutObjectAcl: true,
		OpPutObjectLockConfiguration: true, OpPutObjectRetention: true, OpPutObjectLegalHold: true}
	out := []string{}
	for _, op := range S3Operations {
		if !never[op] {
			out = append(out, op)
		}
	}
	return out
}()

// AgentGrant is one grant as the API answers it (never a secret).
type AgentGrant struct {
	ID                  string     `json:"id"`
	TenantID            string     `json:"tenant_id"`
	ManagementKeyID     string     `json:"management_key_id"`
	TestManagementKeyID string     `json:"test_management_key_id,omitempty"`
	AgentName           string     `json:"agent_name"`
	ClientID            string     `json:"client_id"`
	GrantedBy           string     `json:"granted_by,omitempty"`
	GrantedByEmail      string     `json:"granted_by_email,omitempty"`
	ApprovedScopes      []string   `json:"approved_scopes"`
	StorageLimit        *int64     `json:"storage_limit_bytes"`
	EgressLimit         *int64     `json:"egress_limit_bytes"`
	CreatedAt           time.Time  `json:"created_at"`
	RenewedAt           *time.Time `json:"renewed_at"`
	ExpiresAt           *time.Time `json:"expires_at"`
	RevokedAt           *time.Time `json:"revoked_at"`
	LastUsedAt          *time.Time `json:"last_used_at"`
}

// Live reports whether the grant's key still authenticates.
func (g *AgentGrant) Live(now time.Time) bool {
	return g.RevokedAt == nil && (g.ExpiresAt == nil || g.ExpiresAt.After(now))
}

// AgentGrantOptions is a grant's consent.
type AgentGrantOptions struct {
	AgentName    string
	ClientID     string
	GrantedBy    string // the member who consented (required)
	StorageLimit *int64 // every S3 key the grant mints carries these caps
	EgressLimit  *int64
	// Lifetime: 0 = AgentGrantLifetime; never longer.
	Lifetime time.Duration
}

var (
	// ErrAgentGrantNotFound: no grant of the tenant has that id.
	ErrAgentGrantNotFound = errors.New("agent grant not found")
	// ErrAgentGrantInvalid: a consent that does not validate.
	ErrAgentGrantInvalid = errors.New("invalid agent grant")
)

// AgentGrantSecrets are the two secrets of a new grant, shown once.
type AgentGrantSecrets struct {
	Live string // sk_live_…: the agent's key on the account
	Test string // sk_test_…: the same grant's key on the sandbox
}

// MintAgentGrant makes a grant of tenantID: its live key, its sandbox test
// key and its row, in ONE transaction.
func MintAgentGrant(ctx context.Context, db *sql.DB, tenantID string, o AgentGrantOptions) (*AgentGrant, AgentGrantSecrets, error) {
	if o.GrantedBy == "" {
		return nil, AgentGrantSecrets{}, fmt.Errorf("%w: a grant names the member it acts for", ErrAgentGrantInvalid)
	}
	if o.Lifetime <= 0 || o.Lifetime > AgentGrantLifetime {
		o.Lifetime = AgentGrantLifetime
	}
	for _, v := range []*int64{o.StorageLimit, o.EgressLimit} {
		if v != nil && *v < 0 {
			return nil, AgentGrantSecrets{}, fmt.Errorf("%w: a cap is negative", ErrAgentGrantInvalid)
		}
	}
	name := strings.TrimSpace(o.AgentName)
	if name == "" {
		name = "agent"
	}
	if len([]rune(name)) > 100 {
		return nil, AgentGrantSecrets{}, fmt.Errorf("%w: the agent's name is longer than 100 characters", ErrAgentGrantInvalid)
	}
	expires := time.Now().Add(o.Lifetime).UTC()
	keyOpts := ManagementKeyOptions{Name: "agent: " + name, Scopes: AgentScopes, CreatedBy: o.GrantedBy, ExpiresAt: &expires,
		Ceiling: &S3Ceiling{Permissions: AgentS3Permissions}}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, AgentGrantSecrets{}, fmt.Errorf("grant an agent on %s: %w", tenantID, err)
	}
	defer func() { _ = tx.Rollback() }()
	live, liveSecret, err := mintManagementKeyTx(ctx, tx, tenantID, keyOpts)
	if err != nil {
		return nil, AgentGrantSecrets{}, err
	}
	keyOpts.Test = true
	test, testSecret, err := mintManagementKeyTx(ctx, tx, tenantID, keyOpts)
	if err != nil {
		return nil, AgentGrantSecrets{}, err
	}
	g := &AgentGrant{TenantID: tenantID, ManagementKeyID: live.ID, TestManagementKeyID: test.ID, AgentName: name,
		ClientID: o.ClientID, GrantedBy: o.GrantedBy, ApprovedScopes: slices.Clone(AgentScopes),
		StorageLimit: o.StorageLimit, EgressLimit: o.EgressLimit, ExpiresAt: live.ExpiresAt}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO agent_grants (tenant_id, management_key_id, test_management_key_id, agent_name, client_id, granted_by,
		                          approved_scopes, storage_limit_bytes, egress_limit_bytes)
		VALUES ($1, $2::uuid, $3::uuid, $4, $5, $6::uuid, $7, $8, $9)
		RETURNING id::text, created_at`,
		tenantID, live.ID, test.ID, name, o.ClientID, o.GrantedBy, pq.Array(g.ApprovedScopes), o.StorageLimit, o.EgressLimit).
		Scan(&g.ID, &g.CreatedAt); err != nil {
		return nil, AgentGrantSecrets{}, fmt.Errorf("grant an agent on %s: %w", tenantID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, AgentGrantSecrets{}, fmt.Errorf("grant an agent on %s: %w", tenantID, err)
	}
	return g, AgentGrantSecrets{Live: liveSecret, Test: testSecret}, nil
}

const agentGrantColumns = `g.id::text, g.tenant_id, g.management_key_id::text, COALESCE(g.test_management_key_id::text, ''),
	g.agent_name, g.client_id, COALESCE(g.granted_by::text, ''), COALESCE(u.email, ''), g.approved_scopes,
	g.storage_limit_bytes, g.egress_limit_bytes, g.created_at, g.renewed_at, k.expires_at, k.revoked_at, k.last_used_at`

const agentGrantFrom = ` FROM agent_grants g JOIN management_keys k ON k.id = g.management_key_id
	LEFT JOIN users u ON u.id = g.granted_by`

func scanAgentGrant(sc interface{ Scan(...any) error }) (*AgentGrant, error) {
	var g AgentGrant
	var storage, egress sql.NullInt64
	var renewed, expires, revoked, used sql.NullTime
	if err := sc.Scan(&g.ID, &g.TenantID, &g.ManagementKeyID, &g.TestManagementKeyID, &g.AgentName, &g.ClientID, &g.GrantedBy,
		&g.GrantedByEmail, pq.Array(&g.ApprovedScopes), &storage, &egress, &g.CreatedAt, &renewed, &expires, &revoked, &used); err != nil {
		return nil, err
	}
	if storage.Valid {
		g.StorageLimit = &storage.Int64
	}
	if egress.Valid {
		g.EgressLimit = &egress.Int64
	}
	for _, p := range []struct {
		src sql.NullTime
		dst **time.Time
	}{{renewed, &g.RenewedAt}, {expires, &g.ExpiresAt}, {revoked, &g.RevokedAt}, {used, &g.LastUsedAt}} {
		if p.src.Valid {
			t := p.src.Time
			*p.dst = &t
		}
	}
	return &g, nil
}

// ListAgentGrants lists the tenant's grants, live first, newest first.
func ListAgentGrants(ctx context.Context, db *sql.DB, tenantID string) ([]*AgentGrant, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+agentGrantColumns+agentGrantFrom+`
		 WHERE g.tenant_id = $1 ORDER BY (k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at > NOW())) DESC, g.created_at DESC, g.id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list the agent grants of %s: %w", tenantID, err)
	}
	defer func() { _ = rows.Close() }()
	out := []*AgentGrant{}
	for rows.Next() {
		g, err := scanAgentGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("list the agent grants of %s: %w", tenantID, err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetAgentGrant reads the tenant's grant id.
func GetAgentGrant(ctx context.Context, db *sql.DB, tenantID, id string) (*AgentGrant, error) {
	if !isUUID(id) {
		return nil, ErrAgentGrantNotFound
	}
	g, err := scanAgentGrant(db.QueryRowContext(ctx, `SELECT `+agentGrantColumns+agentGrantFrom+`
		 WHERE g.tenant_id = $1 AND g.id = $2::uuid`, tenantID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAgentGrantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read agent grant %s: %w", id, err)
	}
	return g, nil
}

// RenewAgentGrant gives the tenant's live (or lapsed, never revoked) grant
// a fresh lifetime from now — the default, never longer — on both its keys.
// Nothing else changes: its scopes, ceiling and caps are the consent's
// (a renewal never widens).
func RenewAgentGrant(ctx context.Context, db *sql.DB, tenantID, id string) (*AgentGrant, error) {
	if !isUUID(id) {
		return nil, ErrAgentGrantNotFound
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("renew agent grant %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	var live, test sql.NullString
	err = tx.QueryRowContext(ctx, `
		UPDATE agent_grants g SET renewed_at = NOW()
		  FROM management_keys k
		 WHERE g.tenant_id = $1 AND g.id = $2::uuid AND k.id = g.management_key_id AND k.revoked_at IS NULL
		RETURNING g.management_key_id::text, g.test_management_key_id::text`, tenantID, id).Scan(&live, &test)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAgentGrantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("renew agent grant %s: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE management_keys SET expires_at = NOW() + $2::interval
		 WHERE id = ANY($1::uuid[]) AND revoked_at IS NULL`, pq.Array([]string{live.String, test.String}), AgentGrantLifetime.String()); err != nil {
		return nil, fmt.Errorf("renew agent grant %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("renew agent grant %s: %w", id, err)
	}
	return GetAgentGrant(ctx, db, tenantID, id)
}

// RevokeAgentGrant revokes the tenant's grant: both its keys, at once. It
// answers the live S3 keys the grant minted, for the caller to revoke
// through AuthService.RevokeAPIKey (the hot index, their STS tokens, the
// audit row) — they die with the grant, as a token dies with its parent.
func RevokeAgentGrant(ctx context.Context, db *sql.DB, tenantID, id string) ([]string, error) {
	if !isUUID(id) {
		return nil, ErrAgentGrantNotFound
	}
	res, err := db.ExecContext(ctx, `UPDATE management_keys k SET revoked_at = NOW()
		  FROM agent_grants g
		 WHERE g.tenant_id = $1 AND g.id = $2::uuid AND k.revoked_at IS NULL
		   AND (k.id = g.management_key_id OR k.id = g.test_management_key_id)`, tenantID, id)
	if err != nil {
		return nil, fmt.Errorf("revoke agent grant %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := GetAgentGrant(ctx, db, tenantID, id); err != nil {
			return nil, err
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT id::text, tenant_id FROM api_keys
		 WHERE agent_grant_id = $1::uuid AND revoked_at IS NULL AND NOT is_primary ORDER BY created_at`, id)
	if err != nil {
		return nil, fmt.Errorf("the keys of agent grant %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var k, t string
		if err := rows.Scan(&k, &t); err != nil {
			return nil, fmt.Errorf("the keys of agent grant %s: %w", id, err)
		}
		keys = append(keys, t+"/"+k)
	}
	return keys, rows.Err()
}

// AgentGrantCaps are the caps an agent grant's minted S3 keys carry (nil,
// nil = none: the grant has none, or id is no grant).
func AgentGrantCaps(ctx context.Context, db *sql.DB, grantID string) (storage, egress *int64, err error) {
	if !isUUID(grantID) {
		return nil, nil, nil
	}
	var s, e sql.NullInt64
	err = db.QueryRowContext(ctx, `SELECT storage_limit_bytes, egress_limit_bytes FROM agent_grants WHERE id = $1::uuid`, grantID).Scan(&s, &e)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read the caps of agent grant %s: %w", grantID, err)
	}
	if s.Valid {
		storage = &s.Int64
	}
	if e.Valid {
		egress = &e.Int64
	}
	return storage, egress, nil
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}
