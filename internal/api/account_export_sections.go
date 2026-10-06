package api

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
)

// The export's sections (WP-R10-3b). Every section is one or more SQL
// statements that select, by name, exactly the columns the customer gets —
// never a `SELECT *`, never a column that holds a secret. The schema-walk
// test (TestExportSectionsCoverSchema) holds the two contracts: every table
// keyed by tenant/user is a section here or in exportExcludedTables with its
// reason, and every column whose name says secret/hash/token/password/code
// is in exportNeverColumns and absent from the section SQL of its table.
//
// The render streams: a json.Encoder writes each section's value as it is
// produced; the big tables (objects, versions, locks, events, the audit
// trail) are paged by keyset so no page is larger than pageSize rows.

// exportSubject is who the export is about.
type exportSubject struct {
	UserID, TenantID, ExportID string
	Now                        time.Time
}

// exportSection is one section of the export document.
type exportSection struct {
	// Name is the JSON key.
	Name string
	// Tables are the tenant/user tables the section covers (the schema
	// walk's bookkeeping; a query may read other tables too).
	Tables []string
	// SQL lists every statement the section runs (prepared by the test).
	SQL []string
	// render writes the section's value.
	render func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, pageSize int) error
}

// exportExcludedTables are the tenant/user tables the export deliberately
// leaves out, with the reason.
var exportExcludedTables = map[string]string{
	// internal placement and dedup state: the customer-visible facts (size,
	// etag, class) are in `objects`; where the bytes sit is the service's
	"object_locations":   "internal placement ledger; the object's class is in `objects`",
	"object_metadata":    "chunk manifest of a deduplicated object; `objects` carries the object",
	"tenant_chunk_refs":  "chunk references of a deduplicated object; `objects` carries the object",
	"smart_demotions":    "Smart-tier placement ledger; the class the customer sees is in `objects`",
	"vault_parity":       "the parity copy's placement ledger (WP-VAULT-1): where the engine keeps a second copy of an object already in `objects`",
	"dedup_statistics":   "internal dedup statistics (no customer data)",
	"access_patterns":    "internal access heuristics (no writer in the product)",
	"tenant_cost_daily":  "the operator's cost-of-goods ledger, not the customer's data",
	"bandwidth_alerts":   "the allowance warning ladder; usage itself is in `bandwidth_usage`",
	"bandwidth_rollover": "internal allowance bookkeeping; usage itself is in `bandwidth_usage`",
	"quota_usage_events": "internal quota ledger (90-day retention); totals are in `quota`",
	// operator-side records about the tenant
	"admin_notes":         "the operator's own notes (not the customer's data; erased with the account)",
	"admin_notifications": "operator alerts about the tenant (not the customer's data)",
	"feature_flags":       "the operator's per-tenant switches",
	"abuse_reports":       "reports filed by third parties about content; the reporter's data, not the customer's",
	// logs with their own retention
	"s3_access_log":     "request log of the tenant's buckets (30-day retention); deliverable to the customer's own bucket via server access logging",
	"cdn_access_log":    "public-link visitor log (2-day retention): the visitors' data, not the customer's",
	"cdn_stats_daily":   "aggregate of public-link visitors, not the customer's personal data",
	"stripe_events":     "payment-provider event bodies (90-day retention), the processor's records",
	"idempotency_cache": "24-hour request cache",
	// credentials and key material
	"sts_tokens":             "temporary credentials (secret_key); expire within hours",
	"tenant_encryption_keys": "key material",
	// scaffolding with no writer in the product (D-12)
	"artifacts":                    "no writer (D-12 scaffolding)",
	"billing_charges":              "no writer (D-12 scaffolding)",
	"billing_credits":              "no writer (R10-14)",
	"billing_policies":             "no writer (D-12 scaffolding)",
	"invoices":                     "no writer (R10-14)",
	"subscriptions":                "no writer (R10-14)",
	"metered_usage_reports":        "no writer (D-12 scaffolding)",
	"grace_periods":                "no writer (D-12 scaffolding)",
	"upgrade_suggestions":          "no writer (D-12 scaffolding)",
	"upgrade_triggers":             "no writer (D-12 scaffolding)",
	"usage_daily_snapshots":        "no writer (D-12 scaffolding)",
	"usage_reports":                "no writer (D-12 scaffolding)",
	"report_schedules":             "no writer (D-12 scaffolding)",
	"retention_policies":           "no writer (D-12 scaffolding)",
	"tiering_policies":             "no writer (D-12 scaffolding)",
	"change_history":               "no writer (D-12 scaffolding)",
	"breach_affected_users":        "no writer (D-12 scaffolding)",
	"consent_audit":                "no writer (D-12 scaffolding)",
	"user_consents":                "no writer (D-12 scaffolding)",
	"deletion_requests":            "no writer (D-12 scaffolding)",
	"gdpr_deletion_requests":       "no writer (D-12 scaffolding)",
	"gdpr_subject_access_requests": "no writer (D-12 scaffolding)",
	"legal_holds":                  "no writer (D-12 scaffolding)",
	"portability_downloads":        "no writer (D-12 scaffolding)",
	"portability_requests":         "no writer (D-12 scaffolding)",
	"user_roles":                   "no writer (D-11 RBAC removed)",
}

// exportNeverColumns: every column whose name says secret, hash, token,
// password or code, with why it is never exported (or why its name is a
// false positive). The schema walk fails on a column missing here.
var exportNeverColumns = map[string]string{
	"users.password_hash":                  "credential",
	"users.password_changed_at":            "false positive (a timestamp, WP-R5-10); the change itself is in the audit section",
	"users.email_verify_token":             "credential (single-use link)",
	"tenants.secret_key":                   "credential (the primary pair)",
	"api_keys.secret_hash":                 "credential",
	"api_keys.secret_key":                  "credential",
	"sts_tokens.secret_key":                "credential",
	"user_mfa.secret":                      "credential (TOTP seed)",
	"user_mfa.backup_codes":                "credential",
	"webhook_endpoints.secret":             "credential (webhook signing secret)",
	"global_content_index.ciphertext_hash": "dedup index, not customer data",
	"global_content_index.plaintext_hash":  "dedup index, not customer data",
	"tenant_chunk_refs.ciphertext_hash":    "dedup index, not customer data",
	"tenant_chunk_refs.plaintext_hash":     "dedup index, not customer data",
	"object_metadata.content_hash":         "dedup index, not customer data",
	"deletion_requests.proof_hash":         "D-12 scaffolding, no writer",
	"pseudonym_mappings.original_hash":     "D-12 scaffolding, no writer",
	"s3_access_log.error_code":             "false positive (an S3 error code); the table is excluded anyway",
	"s3_access_log.status_code":            "false positive (an HTTP status); the table is excluded anyway",
	"webhook_deliveries.response_code":     "false positive (an HTTP status); not a tenant table",
}

// --- the stream -------------------------------------------------------------

// jsonStream writes one JSON object field by field, arrays element by
// element, through a json.Encoder — the document is never held whole.
type jsonStream struct {
	w     *bufio.Writer
	enc   *json.Encoder
	err   error
	first bool
}

func newJSONStream(w io.Writer) *jsonStream {
	bw := bufio.NewWriterSize(w, 64<<10)
	enc := json.NewEncoder(bw)
	enc.SetIndent("", "  ")
	return &jsonStream{w: bw, enc: enc, first: true}
}

func (j *jsonStream) raw(s string) {
	if j.err == nil {
		_, j.err = j.w.WriteString(s)
	}
}

func (j *jsonStream) begin() { j.raw("{\n") }

func (j *jsonStream) key(name string) {
	if !j.first {
		j.raw(",\n")
	}
	j.first = false
	b, _ := json.Marshal(name)
	j.raw(string(b) + ": ")
}

// field writes one named value.
func (j *jsonStream) field(name string, v any) {
	j.key(name)
	if j.err == nil {
		j.err = j.enc.Encode(v)
	}
}

// array writes a named array whose elements fn emits one at a time.
func (j *jsonStream) array(name string, fn func(emit func(v any)) error) error {
	j.key(name)
	j.raw("[")
	n := 0
	emit := func(v any) {
		if n > 0 {
			j.raw(",")
		}
		n++
		if j.err == nil {
			j.err = j.enc.Encode(v)
		}
	}
	if err := fn(emit); err != nil {
		return err
	}
	j.raw("]")
	return j.err
}

func (j *jsonStream) end() error {
	j.raw("\n}\n")
	if j.err != nil {
		return j.err
	}
	return j.w.Flush()
}

// --- row helpers ------------------------------------------------------------

// scanRow reads the current row into a map keyed by the column aliases: text
// as string, timestamps as time.Time, json/jsonb as decoded JSON, arrays as
// their text form decoded to []string, NULL as nil.
func scanRow(rows *sql.Rows) (map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(cols))
	for i, c := range cols {
		v := vals[i]
		if b, ok := v.([]byte); ok {
			switch types[i].DatabaseTypeName() {
			case "JSONB", "JSON":
				var decoded any
				if json.Unmarshal(b, &decoded) == nil {
					v = decoded
				} else {
					v = string(b)
				}
			default:
				s := string(b)
				if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") && strings.HasPrefix(types[i].DatabaseTypeName(), "_") {
					v = pgTextArray(s)
				} else {
					v = s
				}
			}
		}
		out[c] = v
	}
	return out, nil
}

// pgTextArray decodes a one-dimensional PostgreSQL text array literal.
func pgTextArray(s string) []string {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
	if s == "" {
		return []string{}
	}
	var out []string
	var cur strings.Builder
	quoted, escaped := false, false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			quoted = !quoted
		case r == ',' && !quoted:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	out = append(out, cur.String())
	return out
}

// streamQuery emits every row of one query (transform may edit a row; nil
// keeps it).
func streamQuery(ctx context.Context, db *sql.DB, emit func(any), transform func(map[string]any), q string, args ...any) error {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		row, err := scanRow(rows)
		if err != nil {
			return err
		}
		if transform != nil {
			transform(row)
		}
		emit(row)
	}
	return rows.Err()
}

// streamPaged emits every row of a keyset-paged query. q takes the
// section's fixed args, then the cursor values, then the page size; next
// returns the cursor of a row.
func streamPaged(ctx context.Context, db *sql.DB, emit func(any), transform func(map[string]any), q string, fixed []any, cursor []any, pageSize int, next func(map[string]any) []any) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		args := append(append(append([]any{}, fixed...), cursor...), pageSize)
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		n := 0
		for rows.Next() {
			row, err := scanRow(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			cursor = next(row)
			if transform != nil {
				transform(row)
			}
			emit(row)
			n++
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if n < pageSize {
			return nil
		}
	}
}

func oneRow(ctx context.Context, db *sql.DB, q string, args ...any) (map[string]any, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return map[string]any{}, rows.Err()
	}
	return scanRow(rows)
}

// --- the sections -----------------------------------------------------------

const (
	sqlExportUser = `
		SELECT id, email, company, role, status, email_verified, created_at, updated_at,
		       deletion_scheduled_at, deletion_reason,
		       signup_referrer, signup_utm_source, signup_utm_medium, signup_utm_campaign
		  FROM users WHERE id::text = $1`
	sqlExportMFA       = `SELECT enabled FROM user_mfa WHERE user_id::text = $1`
	sqlExportMFAEvents = `SELECT action, success, ip_address AS ip, user_agent, created_at FROM mfa_audit_log WHERE user_id::text = $1 ORDER BY created_at`
	sqlExportTenant    = `
		SELECT id, name, email,
		       (SELECT key_id FROM api_keys WHERE tenant_id = tenants.id AND is_primary AND revoked_at IS NULL) AS primary_access_key_id,
		       plan, subscription_status, created_at, suspended_at,
		       slug, house_period, deletion_stripe_cancelled_at
		  FROM tenants WHERE id = $1`
	sqlExportQuotaTotals = `
		SELECT storage_limit_bytes, storage_used_bytes, bandwidth_limit_bytes, bandwidth_used_bytes, tier, pin_hot_bytes, updated_at
		  FROM tenant_quotas WHERE tenant_id = $1`
	sqlExportQuotaFloors = `SELECT floor, storage_limit_bytes, storage_used_bytes, updated_at FROM tenant_floor_quotas WHERE tenant_id = $1 ORDER BY floor`
	sqlExportHouse       = `SELECT intent_std_tb AS standard_tb, intent_vault_tb AS vault_tb, intent_room AS room, house_period AS period FROM tenants WHERE id = $1`
	sqlExportBuckets     = `
		SELECT name, visibility, region, data_residency, versioning_status AS versioning, object_lock_enabled,
		       default_retention_mode, default_retention_days, mfa_delete_enabled, sse_enabled,
		       cors_origins, cors_rules, cache_max_age_secs, bandwidth_budget_bytes, cdn_force_download, tier_preference,
		       logging_enabled, logging_target_bucket, logging_prefix,
		       inventory_enabled, inventory_schedule, inventory_target_bucket, inventory_prefix, inventory_format,
		       metadata, created_at, updated_at
		  FROM buckets WHERE tenant_id = $1 AND name NOT LIKE '\_%' ORDER BY name COLLATE "C"`
	sqlExportNotifications = `SELECT bucket, event_filter, target_type, target_url, enabled, created_at FROM bucket_notifications WHERE tenant_id = $1 ORDER BY bucket COLLATE "C", id`
	sqlExportObjects       = `
		SELECT bucket, object_key AS key, size_bytes AS size, etag, content_type, floor, backend_name AS backend,
		       updated_at AS last_modified, created_at, last_accessed, metadata, tags,
		       content_disposition, content_encoding, content_language, cache_control, http_expires, website_redirect_location,
		       encryption_algorithm AS server_side_encryption, is_chunked AS deduplicated
		  FROM object_head_cache
		 WHERE tenant_id = $1 AND bucket NOT LIKE '\_%'
		   AND (bucket COLLATE "C", object_key COLLATE "C") > ($2::text COLLATE "C", $3::text COLLATE "C")
		 ORDER BY bucket COLLATE "C", object_key COLLATE "C"
		 LIMIT $4`
	sqlExportVersions = `
		SELECT bucket, object_key AS key, version_id, size_bytes AS size, etag, content_type, is_latest, is_delete_marker, created_at
		  FROM object_versions
		 WHERE tenant_id = $1
		   AND (bucket COLLATE "C", object_key COLLATE "C", version_id COLLATE "C") > ($2::text COLLATE "C", $3::text COLLATE "C", $4::text COLLATE "C")
		 ORDER BY bucket COLLATE "C", object_key COLLATE "C", version_id COLLATE "C"
		 LIMIT $5`
	sqlExportLocks = `
		SELECT bucket, object_key AS key, retention_mode, retain_until_date, legal_hold, created_at, updated_at
		  FROM object_locks
		 WHERE tenant_id = $1
		   AND (bucket COLLATE "C", object_key COLLATE "C") > ($2::text COLLATE "C", $3::text COLLATE "C")
		 ORDER BY bucket COLLATE "C", object_key COLLATE "C"
		 LIMIT $4`
	sqlExportMultipart = `
		SELECT upload_id, bucket, object_key AS key, status, content_type, storage_class, metadata, created_at
		  FROM multipart_uploads WHERE tenant_id = $1 ORDER BY created_at, upload_id`
	sqlExportAPIKeys = `
		SELECT id, key_id, is_primary, name, permissions, bucket_scope, ip_allowlist, expires_at, revoked_at, last_used, created_at
		  FROM api_keys WHERE user_id::text = $1 ORDER BY created_at, id`
	sqlExportBandwidth = `
		SELECT date, ingress_bytes, egress_bytes, requests_count AS requests
		  FROM bandwidth_usage_daily WHERE tenant_id = $1 AND date >= CURRENT_DATE - INTERVAL '90 days' ORDER BY date`
	sqlExportEvents = `
		SELECT id, type, data, created_at
		  FROM events
		 WHERE tenant_id = $1 AND ($2::timestamptz IS NULL OR (created_at, id) < ($2::timestamptz, $3::text))
		 ORDER BY created_at DESC, id DESC
		 LIMIT $4`
	sqlExportWebhooks = `SELECT id, url, event_filter AS events, enabled, created_at, updated_at FROM webhook_endpoints WHERE tenant_id = $1 ORDER BY created_at, id`
	sqlExportOAuth    = `SELECT provider, created_at AS linked_at FROM oauth_accounts WHERE user_id::text = $1 ORDER BY created_at`
	sqlExportSessions = `
		SELECT created_at, last_active_at, expires_at, ip_address AS ip, user_agent
		  FROM dashboard_sessions WHERE user_id::text = $1 ORDER BY created_at`
	sqlExportSignup = `
		SELECT created_at, source, referrer, utm_source, utm_medium, utm_campaign, plan_std_tb, plan_vault_tb, room, country, ip_address AS ip, user_agent
		  FROM waitlist_signups WHERE email = $1 ORDER BY created_at`
	sqlExportExports = `
		SELECT id, status, format, file_size_bytes, attempts, created_at, completed_at, expires_at
		  FROM account_exports WHERE user_id::text = $1 ORDER BY created_at, id`
	sqlExportAudit = `
		SELECT "timestamp" AS at, event_type, action, resource, result, severity, ip, user_agent, metadata, id::text AS _id
		  FROM audit_logs
		 WHERE (user_id::text = $1 OR performed_by::text = $1)
		   AND ($2::timestamptz IS NULL OR ("timestamp", id::text) < ($2::timestamptz, $3::text))
		 ORDER BY "timestamp" DESC, id::text DESC
		 LIMIT $4`
	sqlExportAuditArchive = `
		SELECT "timestamp" AS at, event_type, action, resource, result, severity, ip, user_agent, metadata, id::text AS _id
		  FROM audit_logs_archive
		 WHERE (user_id::text = $1 OR performed_by::text = $1)
		   AND ($2::timestamptz IS NULL OR ("timestamp", id::text) < ($2::timestamptz, $3::text))
		 ORDER BY "timestamp" DESC, id::text DESC
		 LIMIT $4`
	sqlExportActivities = `
		SELECT "timestamp" AS at, action, resource, ip, user_agent, metadata
		  FROM user_activities WHERE user_id::text = $1 ORDER BY "timestamp", id`
)

// emailOf reads the account e-mail (the waitlist row is keyed by it).
func emailOf(ctx context.Context, db *sql.DB, userID string) string {
	var email string
	_ = db.QueryRowContext(ctx, `SELECT email FROM users WHERE id::text = $1`, userID).Scan(&email)
	return email
}

var exportSections = []exportSection{
	{Name: "user", Tables: []string{"users"}, SQL: []string{sqlExportUser},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, _ int) error {
			row, err := oneRow(ctx, db, sqlExportUser, sub.UserID)
			if err != nil {
				return err
			}
			j.field("user", row)
			return nil
		}},
	{Name: "security", Tables: []string{"user_mfa", "mfa_audit_log"}, SQL: []string{sqlExportMFA, sqlExportMFAEvents},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, _ int) error {
			mfa, err := oneRow(ctx, db, sqlExportMFA, sub.UserID)
			if err != nil {
				return err
			}
			enabled, _ := mfa["enabled"].(bool)
			var events []any
			if err := streamQuery(ctx, db, func(v any) { events = append(events, v) }, nil, sqlExportMFAEvents, sub.UserID); err != nil {
				return err
			}
			if events == nil {
				events = []any{}
			}
			j.field("security", map[string]any{"mfa_enabled": enabled, "mfa_events": events})
			return nil
		}},
	{Name: "tenant", Tables: []string{"tenants"}, SQL: []string{sqlExportTenant},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, _ int) error {
			row, err := oneRow(ctx, db, sqlExportTenant, sub.TenantID)
			if err != nil {
				return err
			}
			j.field("tenant", row)
			return nil
		}},
	{Name: "quota", Tables: []string{"tenant_quotas", "tenant_floor_quotas"}, SQL: []string{sqlExportQuotaTotals, sqlExportQuotaFloors, sqlExportHouse},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, _ int) error {
			totals, err := oneRow(ctx, db, sqlExportQuotaTotals, sub.TenantID)
			if err != nil {
				return err
			}
			floors := []any{}
			if err := streamQuery(ctx, db, func(v any) { floors = append(floors, v) }, nil, sqlExportQuotaFloors, sub.TenantID); err != nil {
				return err
			}
			house, err := oneRow(ctx, db, sqlExportHouse, sub.TenantID)
			if err != nil {
				return err
			}
			j.field("quota", map[string]any{"totals": totals, "floors": floors, "house": house})
			return nil
		}},
	{Name: "buckets", Tables: []string{"buckets"}, SQL: []string{sqlExportBuckets},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, _ int) error {
			return j.array("buckets", func(emit func(any)) error {
				return streamQuery(ctx, db, emit, nil, sqlExportBuckets, sub.TenantID)
			})
		}},
	{Name: "bucket_notifications", Tables: []string{"bucket_notifications"}, SQL: []string{sqlExportNotifications},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, _ int) error {
			return j.array("bucket_notifications", func(emit func(any)) error {
				return streamQuery(ctx, db, emit, nil, sqlExportNotifications, sub.TenantID)
			})
		}},
	{Name: "objects", Tables: []string{"object_head_cache"}, SQL: []string{sqlExportObjects},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, pageSize int) error {
			return j.array("objects", func(emit func(any)) error {
				return streamPaged(ctx, db, emit, customerObjectView, sqlExportObjects, []any{sub.TenantID}, []any{"", ""}, pageSize,
					func(r map[string]any) []any { return []any{r["bucket"], r["key"]} })
			})
		}},
	{Name: "object_versions", Tables: []string{"object_versions"}, SQL: []string{sqlExportVersions},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, pageSize int) error {
			return j.array("object_versions", func(emit func(any)) error {
				return streamPaged(ctx, db, emit, nil, sqlExportVersions, []any{sub.TenantID}, []any{"", "", ""}, pageSize,
					func(r map[string]any) []any { return []any{r["bucket"], r["key"], r["version_id"]} })
			})
		}},
	{Name: "object_locks", Tables: []string{"object_locks"}, SQL: []string{sqlExportLocks},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, pageSize int) error {
			return j.array("object_locks", func(emit func(any)) error {
				return streamPaged(ctx, db, emit, nil, sqlExportLocks, []any{sub.TenantID}, []any{"", ""}, pageSize,
					func(r map[string]any) []any { return []any{r["bucket"], r["key"]} })
			})
		}},
	{Name: "multipart_uploads", Tables: []string{"multipart_uploads"}, SQL: []string{sqlExportMultipart},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, _ int) error {
			return j.array("multipart_uploads", func(emit func(any)) error {
				return streamQuery(ctx, db, emit, nil, sqlExportMultipart, sub.TenantID)
			})
		}},
	{Name: "api_keys", Tables: []string{"api_keys"}, SQL: []string{sqlExportAPIKeys},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, _ int) error {
			return j.array("api_keys", func(emit func(any)) error {
				return streamQuery(ctx, db, emit, nil, sqlExportAPIKeys, sub.UserID)
			})
		}},
	{Name: "bandwidth_usage", Tables: []string{"bandwidth_usage_daily"}, SQL: []string{sqlExportBandwidth},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, _ int) error {
			return j.array("bandwidth_usage", func(emit func(any)) error {
				return streamQuery(ctx, db, emit, func(r map[string]any) {
					if d, ok := r["date"].(time.Time); ok {
						r["date"] = d.Format("2006-01-02")
					}
				}, sqlExportBandwidth, sub.TenantID)
			})
		}},
	{Name: "events", Tables: []string{"events"}, SQL: []string{sqlExportEvents},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, pageSize int) error {
			return j.array("events", func(emit func(any)) error {
				return streamPaged(ctx, db, emit, nil, sqlExportEvents, []any{sub.TenantID}, []any{nil, nil}, pageSize,
					func(r map[string]any) []any { return []any{r["created_at"], r["id"]} })
			})
		}},
	{Name: "webhooks", Tables: []string{"webhook_endpoints"}, SQL: []string{sqlExportWebhooks},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, _ int) error {
			return j.array("webhooks", func(emit func(any)) error {
				return streamQuery(ctx, db, emit, nil, sqlExportWebhooks, sub.TenantID)
			})
		}},
	{Name: "oauth_accounts", Tables: []string{"oauth_accounts"}, SQL: []string{sqlExportOAuth},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, _ int) error {
			return j.array("oauth_accounts", func(emit func(any)) error {
				return streamQuery(ctx, db, emit, nil, sqlExportOAuth, sub.UserID)
			})
		}},
	{Name: "sessions", Tables: []string{"dashboard_sessions"}, SQL: []string{sqlExportSessions},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, _ int) error {
			return j.array("sessions", func(emit func(any)) error {
				return streamQuery(ctx, db, emit, nil, sqlExportSessions, sub.UserID)
			})
		}},
	{Name: "signup", Tables: nil, SQL: []string{sqlExportSignup}, // waitlist_signups is keyed by e-mail (not in the walk)
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, _ int) error {
			return j.array("signup", func(emit func(any)) error {
				return streamQuery(ctx, db, emit, nil, sqlExportSignup, emailOf(ctx, db, sub.UserID))
			})
		}},
	{Name: "exports", Tables: []string{"account_exports"}, SQL: []string{sqlExportExports},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, _ int) error {
			return j.array("exports", func(emit func(any)) error {
				return streamQuery(ctx, db, emit, nil, sqlExportExports, sub.UserID)
			})
		}},
	{Name: "audit_trail", Tables: []string{"audit_logs", "audit_logs_archive", "user_activities"}, SQL: []string{sqlExportAudit, sqlExportAuditArchive, sqlExportActivities},
		render: func(ctx context.Context, db *sql.DB, j *jsonStream, sub exportSubject, pageSize int) error {
			return j.array("audit_trail", func(emit func(any)) error {
				cursor := func(r map[string]any) []any { return []any{r["at"], r["_id"]} }
				for _, q := range []string{sqlExportAudit, sqlExportAuditArchive} {
					// The id is the cursor's tiebreak; it is not part of the export.
					if err := streamPaged(ctx, db, emit, func(r map[string]any) { delete(r, "_id") }, q, []any{sub.UserID}, []any{nil, nil}, pageSize, cursor); err != nil {
						return err
					}
				}
				return streamQuery(ctx, db, emit, nil, sqlExportActivities, sub.UserID)
			})
		}},
}

// customerObjectView turns a head row into what the customer sees: the
// class from the floor and the backend (engine.CustomerStorageClass,
// WP-R13-1), never the backend itself.
func customerObjectView(r map[string]any) {
	floor, _ := r["floor"].(string)
	backend, _ := r["backend"].(string)
	r["storage_class"] = engine.CustomerStorageClass(floor, backend)
	delete(r, "backend")
}

// renderExport writes the whole export to w.
func renderExport(ctx context.Context, db *sql.DB, w io.Writer, sub exportSubject, pageSize int) error {
	if pageSize <= 0 {
		pageSize = exportDefaultPageSize
	}
	j := newJSONStream(w)
	j.begin()
	j.field("format", "stored-account-export/1")
	j.field("export_id", sub.ExportID)
	j.field("exported_at", sub.Now.UTC())
	j.field("user_id", sub.UserID)
	j.field("tenant_id", sub.TenantID)
	for _, s := range exportSections {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.render(ctx, db, j, sub, pageSize); err != nil {
			return fmt.Errorf("export section %s: %w", s.Name, err)
		}
		if j.err != nil {
			return fmt.Errorf("export section %s: write: %w", s.Name, j.err)
		}
	}
	return j.end()
}
