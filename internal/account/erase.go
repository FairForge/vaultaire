package account

import (
	"context"
	"fmt"
	"time"
)

// Row erasure — stage c of the runner (WP-R10-3), the successor of
// api.AccountDeletionService.ExecuteDeletion's 23-table list plus the
// survivors R9 area 5 and R13 listed. The runner calls EraseRows only after
// the object walk found no head row left, so every backend blob is already
// gone; the transaction here removes what the database still knows.
//
// Ids are compared as text everywhere: prod tenant ids are `tenant-<hex>`
// text while a few scaffold tables typed the column UUID (dedup_statistics,
// the D-12 compliance tables); a bare `= $1` on those raises 22P02 and rolls
// the whole erase back.
//
// The lists below are the contract: TestErasedTablesCoverSchema walks
// information_schema.columns and fails when a table keyed by tenant_id /
// user_id / admin_user_id / granted_by is in neither Deleted nor Kept, so a
// new migration cannot add a table that survives an erasure unnoticed.

// KeyKind says which id a rule's $1 binds to.
type KeyKind int

const (
	// ByTenant binds tenants.id.
	ByTenant KeyKind = iota
	// ByUser binds users.id.
	ByUser
	// ByEmail binds the account e-mail.
	ByEmail
	// ByAccount binds all three: $1 = users.id, $2 = tenants.id ('' when the
	// user has no tenant row), $3 = the account e-mail.
	ByAccount
)

// Rule is one statement of the erasure, bound to one id.
type Rule struct {
	Table string
	// SQL is the full statement with $1 = the key. Statements are listed in
	// FK-safe order (children first); users and tenants go last.
	SQL string
	Key KeyKind
}

// Deleted is every statement EraseRows runs, in order. A table appears once
// per key it is scoped by.
var Deleted = []Rule{
	// --- object index and manifests (the backend bytes are already gone) ---
	// WP-6/F5: release the tenant's GCI references set-based BEFORE its chunk
	// refs are deleted; rows that drop to zero are marked so dedup GC sweeps
	// the blobs after grace. Tenant-scoped (encrypted) chunks stay decryptable
	// until then because the key derives from the master key + tenant id.
	{"global_content_index", `WITH refs AS (
			SELECT dedup_scope, plaintext_hash, COUNT(*) AS cnt
			FROM tenant_chunk_refs
			WHERE tenant_id::text = $1
			GROUP BY dedup_scope, plaintext_hash
		)
		UPDATE global_content_index g
		SET ref_count = GREATEST(g.ref_count - refs.cnt, 0),
		    marked_for_deletion = CASE
		        WHEN GREATEST(g.ref_count - refs.cnt, 0) = 0 THEN TRUE
		        ELSE g.marked_for_deletion END,
		    marked_at = CASE
		        WHEN GREATEST(g.ref_count - refs.cnt, 0) = 0 AND NOT g.marked_for_deletion THEN NOW()
		        ELSE g.marked_at END
		FROM refs
		WHERE g.dedup_scope = refs.dedup_scope
		  AND g.plaintext_hash = refs.plaintext_hash`, ByTenant},
	{"tenant_chunk_refs", `DELETE FROM tenant_chunk_refs WHERE tenant_id::text = $1`, ByTenant},
	{"object_metadata", `DELETE FROM object_metadata WHERE tenant_id::text = $1`, ByTenant},
	{"object_head_cache", `DELETE FROM object_head_cache WHERE tenant_id = $1`, ByTenant},
	// The shards the rows name were erased on the leg before the sweep
	// (deletion_runner.go stage b1, WP-VAULT-1); a leg that could not be
	// reached deferred the tenant before this runs.
	{"vault_parity", `DELETE FROM vault_parity WHERE tenant_id = $1`, ByTenant},
	{"vault_parity_orphans", `DELETE FROM vault_parity_orphans WHERE tenant_id = $1`, ByTenant},
	// The pack store's member index (080, internal/packstore). The bytes stay
	// in their pack until pack_gc rewrites it: a pack whose rows no longer
	// cover its member_count is rewritten at the next run, whatever its live
	// ratio. packs rows carry no tenant id (infrastructure, kept).
	{"pack_members", `DELETE FROM pack_members WHERE tenant_id = $1`, ByTenant},
	{"object_versions", `DELETE FROM object_versions WHERE tenant_id = $1`, ByTenant},
	{"object_locks", `DELETE FROM object_locks WHERE tenant_id = $1`, ByTenant},
	{"object_locations", `DELETE FROM object_locations WHERE tenant_id = $1`, ByTenant},
	{"smart_demotions", `DELETE FROM smart_demotions WHERE tenant_id = $1`, ByTenant},
	{"multipart_uploads", `DELETE FROM multipart_uploads WHERE tenant_id = $1`, ByTenant}, // parts cascade
	{"artifacts", `DELETE FROM artifacts WHERE tenant_id = $1`, ByTenant},
	{"bucket_notifications", `DELETE FROM bucket_notifications WHERE tenant_id = $1`, ByTenant},
	{"buckets", `DELETE FROM buckets WHERE tenant_id = $1`, ByTenant},

	// --- events and webhooks: deliveries before endpoints before events ---
	{"webhook_deliveries", `DELETE FROM webhook_deliveries
		WHERE webhook_id IN (SELECT id FROM webhook_endpoints WHERE tenant_id = $1)
		   OR event_id IN (SELECT id FROM events WHERE tenant_id = $1)`, ByTenant},
	{"webhook_endpoints", `DELETE FROM webhook_endpoints WHERE tenant_id = $1`, ByTenant},
	{"events", `DELETE FROM events WHERE tenant_id = $1`, ByTenant},

	// --- logs and rollups (PII: source IP, user agent, referer, country) ---
	{"s3_access_log", `DELETE FROM s3_access_log WHERE tenant_id = $1`, ByTenant},
	{"cdn_access_log", `DELETE FROM cdn_access_log WHERE tenant_id = $1`, ByTenant},
	{"cdn_stats_daily", `DELETE FROM cdn_stats_daily WHERE tenant_id = $1`, ByTenant},
	{"access_patterns", `DELETE FROM access_patterns WHERE tenant_id = $1`, ByTenant},
	{"bandwidth_usage_daily", `DELETE FROM bandwidth_usage_daily WHERE tenant_id = $1`, ByTenant},
	{"bandwidth_alerts", `DELETE FROM bandwidth_alerts WHERE tenant_id = $1`, ByTenant},
	{"bandwidth_rollover", `DELETE FROM bandwidth_rollover WHERE tenant_id = $1`, ByTenant},
	{"tenant_cost_daily", `DELETE FROM tenant_cost_daily WHERE tenant_id = $1`, ByTenant},
	{"dedup_statistics", `DELETE FROM dedup_statistics WHERE tenant_id::text = $1`, ByTenant},
	{"idempotency_cache", `DELETE FROM idempotency_cache WHERE tenant_id = $1`, ByTenant},

	// --- quota ledgers (children of tenant_quotas first; they cascade too) ---
	{"quota_usage_events", `DELETE FROM quota_usage_events WHERE tenant_id = $1`, ByTenant},
	{"tenant_floor_quotas", `DELETE FROM tenant_floor_quotas WHERE tenant_id = $1`, ByTenant},
	{"grace_periods", `DELETE FROM grace_periods WHERE tenant_id = $1`, ByTenant},
	{"upgrade_suggestions", `DELETE FROM upgrade_suggestions WHERE tenant_id = $1`, ByTenant},
	{"upgrade_triggers", `DELETE FROM upgrade_triggers WHERE tenant_id = $1`, ByTenant},
	{"usage_daily_snapshots", `DELETE FROM usage_daily_snapshots WHERE tenant_id = $1`, ByTenant},
	{"usage_reports", `DELETE FROM usage_reports WHERE tenant_id = $1`, ByTenant},
	{"report_schedules", `DELETE FROM report_schedules WHERE tenant_id = $1`, ByTenant},
	{"billing_policies", `DELETE FROM billing_policies WHERE tenant_id = $1`, ByTenant},
	{"billing_credits", `DELETE FROM billing_credits WHERE tenant_id = $1`, ByTenant}, // no writer (R10-14); cascades from tenant_quotas anyway
	{"invoices", `DELETE FROM invoices WHERE tenant_id = $1`, ByTenant},               // no writer (R10-14); cascades from tenant_quotas anyway
	{"tenant_quotas", `DELETE FROM tenant_quotas WHERE tenant_id = $1`, ByTenant},
	{"subscriptions", `DELETE FROM subscriptions WHERE tenant_id = $1`, ByTenant}, // no writer (R10-14)

	// --- tenant configuration and credentials ---
	{"tiering_policies", `DELETE FROM tiering_policies WHERE tenant_id = $1`, ByTenant},
	{"retention_policies", `DELETE FROM retention_policies WHERE tenant_id = $1`, ByTenant},
	{"feature_flags", `DELETE FROM feature_flags WHERE tenant_id = $1`, ByTenant}, // per-tenant overrides; '*' is the global row
	{"tenant_encryption_keys", `DELETE FROM tenant_encryption_keys WHERE tenant_id = $1`, ByTenant},
	{"sts_tokens", `DELETE FROM sts_tokens WHERE tenant_id = $1`, ByTenant},
	{"admin_notifications", `DELETE FROM admin_notifications WHERE tenant_id = $1`, ByTenant},
	{"admin_notes", `DELETE FROM admin_notes WHERE tenant_id = $1`, ByTenant},
	{"change_history", `DELETE FROM change_history WHERE tenant_id = $1`, ByTenant},
	{"dashboard_sessions", `DELETE FROM dashboard_sessions WHERE tenant_id = $1`, ByTenant},

	// --- the user: credentials, identity, compliance scaffolding (D-12) ---
	{"api_keys", `DELETE FROM api_keys WHERE user_id::text = $1`, ByUser},
	{"dashboard_sessions", `DELETE FROM dashboard_sessions WHERE user_id::text = $1`, ByUser},
	{"user_mfa", `DELETE FROM user_mfa WHERE user_id = $1`, ByUser},
	{"mfa_audit_log", `DELETE FROM mfa_audit_log WHERE user_id = $1`, ByUser},
	{"oauth_accounts", `DELETE FROM oauth_accounts WHERE user_id::text = $1`, ByUser},
	{"user_activities", `DELETE FROM user_activities WHERE user_id = $1`, ByUser},
	{"user_consents", `DELETE FROM user_consents WHERE user_id::text = $1`, ByUser},
	{"consent_audit", `DELETE FROM consent_audit WHERE user_id::text = $1`, ByUser},
	// user_roles.granted_by → users has NO ACTION: a role this user granted
	// to someone else would block the users DELETE. The grant stays, the
	// granter is forgotten.
	{"user_roles", `UPDATE user_roles SET granted_by = NULL WHERE granted_by::text = $1`, ByUser},
	{"user_roles", `DELETE FROM user_roles WHERE user_id::text = $1`, ByUser},
	{"breach_affected_users", `DELETE FROM breach_affected_users WHERE user_id::text = $1`, ByUser},
	{"deletion_requests", `DELETE FROM deletion_requests WHERE user_id::text = $1`, ByUser},
	{"gdpr_deletion_requests", `DELETE FROM gdpr_deletion_requests WHERE user_id::text = $1`, ByUser},
	{"gdpr_subject_access_requests", `DELETE FROM gdpr_subject_access_requests WHERE user_id::text = $1`, ByUser},
	{"legal_holds", `DELETE FROM legal_holds WHERE user_id::text = $1`, ByUser},
	{"portability_downloads", `DELETE FROM portability_downloads WHERE user_id::text = $1`, ByUser},
	{"portability_requests", `DELETE FROM portability_requests WHERE user_id::text = $1`, ByUser},
	{"change_history", `DELETE FROM change_history WHERE user_id::text = $1`, ByUser},
	{"account_exports", `DELETE FROM account_exports WHERE user_id::text = $1`, ByUser},
	// admin_notes.admin_user_id → users has NO ACTION: notes this user
	// AUTHORED (as an admin) would block the DELETE and are their PII.
	{"admin_notes", `DELETE FROM admin_notes WHERE admin_user_id::text = $1`, ByUser},
	// The privacy policy: "your own entries are removed with your account"
	// (the audit trail otherwise lives for the life of the service). Rows
	// keyed by the tenant only (operator actions on the tenant) are kept and
	// scrubbed below. An OPERATOR's erasure is the other half (WP-R10-3d):
	// what they did to other accounts and to the service is not their own
	// entry — it is de-identified first, so the DELETE no longer matches it.
	{"audit_logs", operatorTrailScrub("audit_logs"), ByAccount},
	{"audit_logs", `DELETE FROM audit_logs WHERE user_id::text = $1 OR performed_by::text = $1`, ByUser},
	{"audit_logs_archive", operatorTrailScrub("audit_logs_archive"), ByAccount},
	{"audit_logs_archive", `DELETE FROM audit_logs_archive WHERE user_id::text = $1 OR performed_by::text = $1`, ByUser},
	// The waitlist row is the sign-up; with the account it is PII with no
	// purpose left.
	{"waitlist_signups", `DELETE FROM waitlist_signups WHERE email = $1`, ByEmail},

	// --- last: the account rows themselves ---
	{"users", `DELETE FROM users WHERE id::text = $1`, ByUser},
	{"tenants", `DELETE FROM tenants WHERE id = $1`, ByTenant},
}

// operatorTrailScrub is the statement that keeps the operator audit trail when
// the operator's own account is erased (WP-R10-3d). Of the rows that name the
// user ($1) as subject or actor, a row is NOT the user's own entry when it is
//
//   - about another tenant (tenant_id set and not theirs — the admin dashboard
//     writes the admin as user_id AND performed_by with the subject tenant in
//     tenant_id: suspend, enable, quota, bandwidth, tier, the erasure trigger),
//   - an administrative action on the service (event_type 'admin': primary
//     swap, job triggers, reconcile, waitlist and audit exports), or
//   - an action with no tenant on a subject that is not the user (a global
//     flag flip, anything done to another user).
//
// Those rows stay, with everything that identifies the erased person removed:
// user_id / performed_by where they are the user, the client IP and user agent
// (the actor's — kept when another, surviving operator made the request), and
// any top-level metadata value equal to their id or e-mail (the primary-swap
// row carries "admin": <e-mail>). `actor_erased` is added because
// a NULL performed_by alone reads as "the system did it". Everything else that
// names the user — sign-ins, their own keys, their own account events — falls
// through to the DELETE that follows.
func operatorTrailScrub(table string) string {
	return `UPDATE ` + table + `
	   SET user_id      = CASE WHEN user_id::text = $1 THEN NULL ELSE user_id END,
	       performed_by = CASE WHEN performed_by::text = $1 THEN NULL ELSE performed_by END,
	       ip         = CASE WHEN performed_by IS NULL OR performed_by::text = $1 THEN NULL ELSE ip END,
	       user_agent = CASE WHEN performed_by IS NULL OR performed_by::text = $1 THEN NULL ELSE user_agent END,
	       metadata = (CASE WHEN jsonb_typeof(metadata) = 'object' THEN
	                       (SELECT COALESCE(jsonb_object_agg(m.k, m.v), '{}'::jsonb)
	                          FROM jsonb_each(metadata) AS m(k, v)
	                         WHERE jsonb_typeof(m.v) <> 'string'
	                            OR lower(m.v #>> '{}') NOT IN (lower($1), lower($3)))
	                   ELSE '{}'::jsonb END) || '{"actor_erased": true}'::jsonb
	 WHERE (user_id::text = $1 OR performed_by::text = $1)
	   AND ((tenant_id IS NOT NULL AND tenant_id <> $2)
	     OR event_type = 'admin'
	     OR (tenant_id IS NULL AND user_id::text IS DISTINCT FROM $1))`
}

// Kept is every tenant/user-keyed table an erasure leaves rows in, with the
// scrub applied (empty SQL = kept verbatim) and why.
var Kept = []Rule{
	// The Stripe ledger: 90-day retention (R13), needed to answer a dispute;
	// the tenant reference is dropped, the event body is Stripe's record.
	{"stripe_events", `UPDATE stripe_events SET tenant_id = NULL WHERE tenant_id = $1`, ByTenant},
	// Operator actions keyed by the tenant only (suspend, quota, tier…):
	// the security record, with the client IP and user agent dropped. (The
	// rows an erased OPERATOR leaves behind are de-identified by
	// operatorTrailScrub in Deleted, before the DELETE of their own entries.)
	{"audit_logs", `UPDATE audit_logs SET ip = NULL, user_agent = NULL WHERE tenant_id = $1`, ByTenant},
	{"audit_logs_archive", `UPDATE audit_logs_archive SET ip = NULL, user_agent = NULL WHERE tenant_id = $1`, ByTenant},
	// Financial ledgers (no PII beyond the tenant id; a dispute needs them).
	{"billing_charges", "", ByTenant},
	{"metered_usage_reports", "", ByTenant},
	// Abuse reports name the reporter and the URL, not the tenant's PII;
	// the moderation record outlives the account.
	{"abuse_reports", "", ByTenant},
}

// Untouched are tables with no tenant/user column the walk must ignore, or
// keyed by neither: job_runs (the runner's own schedule) and
// global_content_index (dedup_scope, handled by the release CTE above).

// EraseResult is what EraseRows removed.
type EraseResult struct {
	Rows  map[string]int64 `json:"rows"`
	Total int64            `json:"total"`
}

// EraseRows removes every row of the account in one transaction: it locks
// the users row, refuses with ErrNotPending when a cancel won the race (the
// schedule is cleared or in the future), runs Deleted in order, then the
// Kept scrubs. Nothing is committed on any error.
func (s *Service) EraseRows(ctx context.Context, userID, tenantID, email string, now time.Time) (EraseResult, error) {
	res := EraseResult{Rows: map[string]int64{}}
	if s.db == nil {
		return res, ErrNoDatabase
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("begin erase: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	due, err := lockedStillDue(ctx, tx, userID, now)
	if err != nil {
		return res, err
	}
	if !due {
		return res, ErrNotPending
	}
	// The runner's walk must have emptied the index: a head row here means
	// bytes on a backend nobody will delete once the row is gone (a PUT
	// that landed after the walk, a backend that refused). Refuse; the next
	// run walks again.
	if tenantID != "" {
		var remaining int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM object_head_cache WHERE tenant_id = $1`, tenantID).Scan(&remaining); err != nil {
			return res, fmt.Errorf("count head rows: %w", err)
		}
		if remaining > 0 {
			return res, fmt.Errorf("%w: %d object_head_cache rows", ErrObjectsRemain, remaining)
		}
	}

	for _, r := range Deleted {
		args, ok := ruleArgs(r.Key, userID, tenantID, email)
		if !ok {
			continue
		}
		out, err := tx.ExecContext(ctx, r.SQL, args...)
		if err != nil {
			return res, fmt.Errorf("erase %s: %w", r.Table, err)
		}
		n, _ := out.RowsAffected()
		res.Rows[r.Table] += n
		res.Total += n
	}
	for _, r := range Kept {
		if r.SQL == "" {
			continue
		}
		args, ok := ruleArgs(r.Key, userID, tenantID, email)
		if !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, r.SQL, args...); err != nil {
			return res, fmt.Errorf("scrub %s: %w", r.Table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("commit erase: %w", err)
	}
	return res, nil
}

// ruleArgs picks the ids a rule binds; a rule whose id is unknown (a user with
// no tenant row, no e-mail) is skipped rather than run with an empty id. ByAccount
// needs only the user: with no tenant of their own every tenant-keyed row
// they acted on is someone else's, and an empty e-mail matches no metadata.
func ruleArgs(k KeyKind, userID, tenantID, email string) ([]any, bool) {
	switch k {
	case ByUser:
		return []any{userID}, userID != ""
	case ByTenant:
		return []any{tenantID}, tenantID != ""
	case ByAccount:
		return []any{userID, tenantID, email}, userID != ""
	default:
		return []any{email}, email != ""
	}
}
