// Package requestlog reads the developers' request log (WP-LOG-1, Stage 1a
// Prompt 49; migration 115): one tenant's S3 requests of the last 7 days,
// newest first, keyset paged, the error code's suggestion joined. The API
// (GET /api/v1/manage/requests) and the dashboard's Developers page read
// through Read; the ONE writer is the access-log tracker's second sink
// (internal/api/request_log.go).
package requestlog

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Row is one request as the API and the page show it.
type Row struct {
	ID         int64     `json:"id"`
	LoggedAt   time.Time `json:"logged_at"`
	KeyID      string    `json:"key_id"`
	Op         string    `json:"op"`
	Bucket     string    `json:"bucket"`
	Key        string    `json:"key"`
	Status     int       `json:"status"`
	ErrorCode  string    `json:"error_code,omitempty"`
	Suggestion string    `json:"suggestion,omitempty"`
	RequestID  string    `json:"request_id"`
	BytesIn    int64     `json:"bytes_in"`
	BytesOut   int64     `json:"bytes_out"`
	Host       string    `json:"host"`
}

// Query filters a tenant's request log.
type Query struct {
	TenantID  string
	KeyID     string    // "" = every key
	RequestID string    // "" = every request
	Since     time.Time // zero = the whole 7 days
	// StatusMin..StatusMax: the status filter (0, 0 = any).
	StatusMin, StatusMax int
	// After: the keyset cursor (the last row of the previous page).
	After *Cursor
	Limit int
}

// Cursor is a page's last row: (logged_at, id), newest first.
type Cursor struct {
	At time.Time
	ID int64
}

// String is the cursor as the API and the page hand it out.
func (c Cursor) String() string {
	return strconv.FormatInt(c.At.UnixNano(), 10) + "_" + strconv.FormatInt(c.ID, 10)
}

// ParseCursor reads a cursor String gave ("" = the first page).
func ParseCursor(s string) (*Cursor, error) {
	if s == "" {
		return nil, nil
	}
	at, id, ok := strings.Cut(s, "_")
	ns, err1 := strconv.ParseInt(at, 10, 64)
	n, err2 := strconv.ParseInt(id, 10, 64)
	if !ok || err1 != nil || err2 != nil {
		return nil, fmt.Errorf("cursor %q is not one this API gave", s)
	}
	return &Cursor{At: time.Unix(0, ns), ID: n}, nil
}

// ParseStatus reads a status filter: an exact code (403), a class
// (4xx, 5xx) or "error" (every status from 400).
func ParseStatus(s string) (lo, hi int, err error) {
	switch s = strings.ToLower(strings.TrimSpace(s)); {
	case s == "":
		return 0, 0, nil
	case s == "error":
		return 400, 599, nil
	case len(s) == 3 && strings.HasSuffix(s, "xx") && s[0] >= '1' && s[0] <= '5':
		c := int(s[0]-'0') * 100
		return c, c + 99, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 100 || n > 599 {
		return 0, 0, fmt.Errorf("status %q: give a code (403), a class (4xx) or error", s)
	}
	return n, n, nil
}

// Read reads one page of a tenant's requests, newest first, the
// suggestion joined by error code; next is the cursor of the following page
// ("" = this is the last).
func Read(ctx context.Context, db *sql.DB, q Query) ([]Row, string, error) {
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var afterAt time.Time
	var afterID int64
	if q.After != nil {
		afterAt, afterID = q.After.At, q.After.ID
	}
	rows, err := db.QueryContext(ctx, `
		SELECT l.id, l.logged_at, l.key_id, l.op, l.bucket, l.object_key, l.status, l.error_code,
		       COALESCE(s.suggestion, ''), l.request_id, l.bytes_in, l.bytes_out, l.host
		  FROM request_log l
		  LEFT JOIN request_suggestions s ON s.error_code = l.error_code AND l.error_code <> ''
		 WHERE l.tenant_id = $1
		   AND ($2 = '' OR l.key_id = $2)
		   AND ($3 = '' OR l.request_id = $3)
		   AND l.logged_at >= $4
		   AND ($5 = 0 OR l.status BETWEEN $5 AND $6)
		   AND (NOT $7 OR (l.logged_at, l.id) < ($8, $9))
		 ORDER BY l.logged_at DESC, l.id DESC
		 LIMIT $10`,
		q.TenantID, q.KeyID, q.RequestID, q.Since, q.StatusMin, q.StatusMax, q.After != nil, afterAt, afterID, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("read the request log: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.LoggedAt, &r.KeyID, &r.Op, &r.Bucket, &r.Key, &r.Status, &r.ErrorCode,
			&r.Suggestion, &r.RequestID, &r.BytesIn, &r.BytesOut, &r.Host); err != nil {
			return nil, "", fmt.Errorf("read the request log: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("read the request log: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[limit-1]
		next = Cursor{At: last.LoggedAt, ID: last.ID}.String()
	}
	return out, next, nil
}
