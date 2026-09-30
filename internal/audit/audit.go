// Package audit writes the operator audit trail (`audit_logs`, migration 008).
//
// Until Review R11 nothing in the binary wrote this table — 0 rows in 30
// days of prod while keys were revoked, flags flipped and the primary
// swapped. Every admin / key / account mutation now records a row through
// Record, from the SERVICE layer where one exists (auth keys, MFA,
// passwords, feature flags) so the S3 path, the management API and the
// dashboard cannot drift apart (the R4-22 lesson), and from the handler
// where there is no shared service.
//
// Record never fails the caller: an audit insert that cannot be made is
// logged and dropped. The client IP and user agent come from the request
// context (`WithRequest`, stashed by the server's first middleware from
// `clientip.FromRequest` — rule R1-01), the acting user from `WithActor`
// (the JWT / dashboard session user) or, when absent, the subject.
package audit

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Entry is one audit row. Action is the only required field
// ("key.revoked", "flag.set", "account.deletion_scheduled", …); EventType
// defaults to the part before the first dot.
type Entry struct {
	// Actor is the user performing the action (performed_by). Defaults to
	// the context actor, then to UserID.
	Actor string
	// UserID is the subject the action is about (user_id).
	UserID   string
	TenantID string
	// EventType groups actions ("key", "flag", "account", "admin", "auth").
	EventType string
	Action    string
	Resource  string
	// Error marks the row result=failure / severity=error and stores the
	// message. Nil = success.
	Error    error
	Severity string
	Metadata map[string]any
}

type ctxKey int

const (
	requestKey ctxKey = iota
	actorKey
)

type requestInfo struct {
	ip, userAgent string
}

var logger = zap.NewNop()

// SetLogger sets the logger failed inserts are reported to.
func SetLogger(l *zap.Logger) {
	if l != nil {
		logger = l
	}
}

// WithRequest stashes the client IP and user agent for Record.
func WithRequest(ctx context.Context, ip, userAgent string) context.Context {
	return context.WithValue(ctx, requestKey, requestInfo{ip: ip, userAgent: userAgent})
}

// WithActor stashes the acting user id for Record.
func WithActor(ctx context.Context, userID string) context.Context {
	if userID == "" {
		return ctx
	}
	return context.WithValue(ctx, actorKey, userID)
}

// ActorFromContext returns the acting user id stashed by WithActor.
func ActorFromContext(ctx context.Context) string {
	a, _ := ctx.Value(actorKey).(string)
	return a
}

const insertTimeout = 3 * time.Second

// Record writes one row. Nil db is a no-op. The write is detached from the
// request's cancellation so a client that hangs up after the mutation
// still leaves a trail.
func Record(ctx context.Context, db *sql.DB, e Entry) {
	if db == nil || e.Action == "" {
		return
	}
	if e.EventType == "" {
		if i := strings.IndexByte(e.Action, '.'); i > 0 {
			e.EventType = e.Action[:i]
		} else {
			e.EventType = e.Action
		}
	}
	result, severity := "success", e.Severity
	var errMsg sql.NullString
	if e.Error != nil {
		result = "failure"
		errMsg = sql.NullString{String: e.Error.Error(), Valid: true}
		if severity == "" {
			severity = "error"
		}
	}
	if severity == "" {
		severity = "info"
	}
	actor := e.Actor
	if actor == "" {
		actor = ActorFromContext(ctx)
	}
	if actor == "" {
		actor = e.UserID
	}
	var ip, ua sql.NullString
	if ri, ok := ctx.Value(requestKey).(requestInfo); ok {
		if net.ParseIP(ri.ip) != nil {
			ip = sql.NullString{String: ri.ip, Valid: true}
		}
		if ri.userAgent != "" {
			ua = sql.NullString{String: truncate(ri.userAgent, 512), Valid: true}
		}
	}
	meta := []byte("{}")
	if len(e.Metadata) > 0 {
		if b, err := json.Marshal(e.Metadata); err == nil {
			meta = b
		}
	}

	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), insertTimeout)
	defer cancel()
	_, err := db.ExecContext(wctx, `
		INSERT INTO audit_logs (user_id, tenant_id, event_type, action, resource, result, severity,
		                        ip, user_agent, error_msg, metadata, performed_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::inet, $9, $10, $11, $12)`,
		nullUUID(e.UserID), nullString(e.TenantID), e.EventType, e.Action, nullString(e.Resource),
		result, severity, ip, ua, errMsg, meta, nullUUID(actor))
	if err != nil {
		logger.Error("audit row not written",
			zap.String("action", e.Action),
			zap.String("tenant_id", e.TenantID),
			zap.Error(err))
	}
}

// Row is one audit_logs record as read back by List.
type Row struct {
	ID          string          `json:"id"`
	Timestamp   time.Time       `json:"timestamp"`
	UserID      string          `json:"user_id,omitempty"`
	PerformedBy string          `json:"performed_by,omitempty"`
	TenantID    string          `json:"tenant_id,omitempty"`
	EventType   string          `json:"event_type"`
	Action      string          `json:"action"`
	Resource    string          `json:"resource,omitempty"`
	Result      string          `json:"result"`
	Severity    string          `json:"severity"`
	IP          string          `json:"ip,omitempty"`
	UserAgent   string          `json:"user_agent,omitempty"`
	Error       string          `json:"error,omitempty"`
	Metadata    json.RawMessage `json:"metadata"`
}

// Filter narrows List. Limit is clamped to 1..100 (default 50).
type Filter struct {
	TenantID  string
	UserID    string // the subject (user_id)
	Actor     string // who acted (performed_by)
	IP        string // client address, exact match
	Action    string
	EventType string
	Cursor    string
	Limit     int
}

// Page is one page of List results; NextCursor is opaque.
type Page struct {
	Rows       []Row
	HasMore    bool
	NextCursor string
}

// ErrBadCursor is returned for a cursor List did not mint.
var ErrBadCursor = errors.New("invalid audit cursor")

// List reads audit rows newest first with a keyset (timestamp, id) cursor.
func List(ctx context.Context, db *sql.DB, f Filter) (*Page, error) {
	if db == nil {
		return &Page{Rows: []Row{}}, nil
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	where := []string{"TRUE"}
	args := []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.TenantID != "" {
		add("tenant_id = $%d", f.TenantID)
	}
	if f.UserID != "" {
		add("user_id::text = $%d", f.UserID)
	}
	if f.Actor != "" {
		add("performed_by::text = $%d", f.Actor)
	}
	if f.IP != "" {
		add("host(ip) = $%d", f.IP)
	}
	if f.Action != "" {
		add("action = $%d", f.Action)
	}
	if f.EventType != "" {
		add("event_type = $%d", f.EventType)
	}
	if f.Cursor != "" {
		ts, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, err
		}
		args = append(args, ts, id)
		where = append(where, fmt.Sprintf("(timestamp, id::text) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, limit+1)
	q := `SELECT id::text, timestamp, COALESCE(user_id::text,''), COALESCE(performed_by::text,''), COALESCE(tenant_id,''),
	             event_type, action, COALESCE(resource,''), result, COALESCE(severity,''), COALESCE(host(ip),''),
	             COALESCE(user_agent,''), COALESCE(error_msg,''), COALESCE(metadata, '{}'::jsonb)
	      FROM audit_logs WHERE ` + strings.Join(where, " AND ") +
		fmt.Sprintf(" ORDER BY timestamp DESC, id::text DESC LIMIT $%d", len(args))
	rows, err := db.QueryContext(ctx, q, args...) // #nosec G202 -- conditions are fixed literals with $N placeholders
	if err != nil {
		return nil, fmt.Errorf("list audit rows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	page := &Page{Rows: []Row{}}
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.Timestamp, &r.UserID, &r.PerformedBy, &r.TenantID, &r.EventType, &r.Action,
			&r.Resource, &r.Result, &r.Severity, &r.IP, &r.UserAgent, &r.Error, &r.Metadata); err != nil {
			return nil, fmt.Errorf("scan audit row: %w", err)
		}
		page.Rows = append(page.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit rows: %w", err)
	}
	if len(page.Rows) > limit {
		page.Rows = page.Rows[:limit]
		page.HasMore = true
		last := page.Rows[limit-1]
		page.NextCursor = encodeCursor(last.Timestamp, last.ID)
	}
	return page, nil
}

func encodeCursor(ts time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(ts.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeCursor(c string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", ErrBadCursor
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", ErrBadCursor
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", ErrBadCursor
	}
	if _, err := uuid.Parse(parts[1]); err != nil {
		return time.Time{}, "", ErrBadCursor
	}
	return ts, parts[1], nil
}

func nullUUID(s string) sql.NullString {
	if _, err := uuid.Parse(s); err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
