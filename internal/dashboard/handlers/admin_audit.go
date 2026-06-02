package handlers

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"go.uber.org/zap"
)

var auditEventTypes = []string{
	"bandwidth.alert",
	"bucket.created",
	"bucket.deleted",
	"key.created",
	"key.revoked",
	"object.created",
	"object.deleted",
	"object.downloaded",
	"sts.token_created",
	"webhook.test",
}

type auditEventRow struct {
	ID          string
	Type        string
	TenantID    string
	DataJSON    string
	DataSummary string
	CreatedFmt  string
	createdRaw  string
}

type auditFilters struct {
	Tenant string
	Type   string
	From   string
	To     string
	Cursor string
}

const auditPageSize = 50

func HandleAdminAudit(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "admin-audit")
		withCSRF(r.Context(), data)
		data["EventTypes"] = auditEventTypes
		data["Events"] = []auditEventRow{}
		data["HasMore"] = false

		f := parseAuditFilters(r)
		data["Filters"] = f
		data["ExportURL"] = "/admin/audit/export" + auditFilterQS(f)

		if db != nil {
			events, hasMore, nextCursor := queryAuditEvents(r, db, f, logger)
			data["Events"] = events
			data["HasMore"] = hasMore
			if hasMore {
				data["NextURL"] = "/admin/audit" + auditNextPageQS(f, nextCursor)
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render admin audit", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func HandleAdminAuditExport(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if dashauth.GetSession(r.Context()) == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition",
			`attachment; filename="audit-`+time.Now().Format("2006-01-02")+`.csv"`)

		cw := csv.NewWriter(w)
		defer cw.Flush()
		_ = cw.Write([]string{"id", "type", "tenant_id", "data", "created_at"})

		if db == nil {
			return
		}

		f := parseAuditFilters(r)
		where, args := auditWhere(f, false)
		q := "SELECT id, type, tenant_id, data, created_at FROM events" + where + " ORDER BY created_at DESC"

		rows, err := db.QueryContext(r.Context(), q, args...)
		if err != nil {
			logger.Error("audit export query", zap.Error(err))
			return
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var id, typ, tenantID string
			var data []byte
			var created time.Time
			if err := rows.Scan(&id, &typ, &tenantID, &data, &created); err != nil {
				logger.Error("audit export scan", zap.Error(err))
				continue
			}
			_ = cw.Write([]string{id, typ, tenantID, string(data), created.UTC().Format(time.RFC3339)})
		}
	}
}

func parseAuditFilters(r *http.Request) auditFilters {
	return auditFilters{
		Tenant: r.URL.Query().Get("tenant"),
		Type:   r.URL.Query().Get("type"),
		From:   r.URL.Query().Get("from"),
		To:     r.URL.Query().Get("to"),
		Cursor: r.URL.Query().Get("cursor"),
	}
}

func auditWhere(f auditFilters, withCursor bool) (string, []interface{}) {
	where := " WHERE 1=1"
	var args []interface{}
	idx := 1
	if f.Tenant != "" {
		where += fmt.Sprintf(" AND tenant_id = $%d", idx)
		args = append(args, f.Tenant)
		idx++
	}
	if f.Type != "" {
		where += fmt.Sprintf(" AND type = $%d", idx)
		args = append(args, f.Type)
		idx++
	}
	if f.From != "" {
		where += fmt.Sprintf(" AND created_at >= $%d::date", idx)
		args = append(args, f.From)
		idx++
	}
	if f.To != "" {
		where += fmt.Sprintf(" AND created_at < ($%d::date + INTERVAL '1 day')", idx)
		args = append(args, f.To)
		idx++
	}
	if withCursor && f.Cursor != "" {
		where += fmt.Sprintf(" AND created_at < $%d", idx)
		args = append(args, f.Cursor)
	}
	return where, args
}

func queryAuditEvents(r *http.Request, db *sql.DB, f auditFilters, logger *zap.Logger) ([]auditEventRow, bool, string) {
	where, args := auditWhere(f, true)
	q := "SELECT id, type, tenant_id, data, created_at FROM events" + where +
		fmt.Sprintf(" ORDER BY created_at DESC LIMIT %d", auditPageSize+1)

	rows, err := db.QueryContext(r.Context(), q, args...)
	if err != nil {
		logger.Error("audit events query", zap.Error(err))
		return nil, false, ""
	}
	defer func() { _ = rows.Close() }()

	var events []auditEventRow
	for rows.Next() {
		var id, typ, tenantID string
		var data []byte
		var created time.Time
		if err := rows.Scan(&id, &typ, &tenantID, &data, &created); err != nil {
			logger.Error("audit events scan", zap.Error(err))
			continue
		}
		dataStr := string(data)
		events = append(events, auditEventRow{
			ID:          id,
			Type:        typ,
			TenantID:    tenantID,
			DataJSON:    dataStr,
			DataSummary: auditSummarize(dataStr),
			CreatedFmt:  created.Format("Jan 2, 2006 15:04 MST"),
			createdRaw:  created.Format(time.RFC3339Nano),
		})
	}

	hasMore := len(events) > auditPageSize
	var nextCursor string
	if hasMore {
		events = events[:auditPageSize]
		nextCursor = events[auditPageSize-1].createdRaw
	}
	return events, hasMore, nextCursor
}

func auditSummarize(s string) string {
	if len(s) <= 80 {
		return s
	}
	return s[:80] + "…"
}

func auditFilterQS(f auditFilters) string {
	params := url.Values{}
	if f.Tenant != "" {
		params.Set("tenant", f.Tenant)
	}
	if f.Type != "" {
		params.Set("type", f.Type)
	}
	if f.From != "" {
		params.Set("from", f.From)
	}
	if f.To != "" {
		params.Set("to", f.To)
	}
	if qs := params.Encode(); qs != "" {
		return "?" + qs
	}
	return ""
}

func auditNextPageQS(f auditFilters, cursor string) string {
	params := url.Values{}
	if f.Tenant != "" {
		params.Set("tenant", f.Tenant)
	}
	if f.Type != "" {
		params.Set("type", f.Type)
	}
	if f.From != "" {
		params.Set("from", f.From)
	}
	if f.To != "" {
		params.Set("to", f.To)
	}
	params.Set("cursor", cursor)
	return "?" + params.Encode()
}
