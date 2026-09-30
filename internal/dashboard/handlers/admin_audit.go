package handlers

import (
	"database/sql"
	"encoding/csv"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"go.uber.org/zap"
)

// The admin audit page reads the operator audit trail — `audit_logs`, written
// by internal/audit since Review R11 (keys, passwords, MFA, registration,
// flags, admin tenant/backend actions, exports, and since R12 every dashboard
// sign-in outcome). Until R12 this page read the `events` table (object.created
// & co.), so an operator looking for "who revoked that key" saw uploads
// (WP-R11-10). Filters: tenant, actor (performed_by), subject user, client IP,
// event type, action; keyset cursor from audit.List.

// auditEventTypes are the event_type values audit.Record produces (the part
// of the action before the dot).
var auditEventTypes = []string{"account", "admin", "auth", "flag", "key", "mfa", "sts", "webhook"}

type auditRowView struct {
	Time        string
	EventType   string
	Action      string
	Actor       string
	Subject     string
	TenantID    string
	Resource    string
	Result      string
	ResultClass string
	Severity    string
	IP          string
	UserAgent   string
	Error       string
	MetaJSON    string
	MetaSummary string
}

type auditFilters struct {
	Tenant string
	Actor  string
	User   string
	IP     string
	Type   string
	Action string
	Cursor string
}

const auditPageSize = 50

func parseAuditFilters(r *http.Request) auditFilters {
	q := r.URL.Query()
	return auditFilters{
		Tenant: strings.TrimSpace(q.Get("tenant")),
		Actor:  strings.TrimSpace(q.Get("actor")),
		User:   strings.TrimSpace(q.Get("user")),
		IP:     strings.TrimSpace(q.Get("ip")),
		Type:   strings.TrimSpace(q.Get("type")),
		Action: strings.TrimSpace(q.Get("action")),
		Cursor: q.Get("cursor"),
	}
}

func (f auditFilters) toAudit(limit int) audit.Filter {
	return audit.Filter{TenantID: f.Tenant, Actor: f.Actor, UserID: f.User, IP: f.IP,
		EventType: f.Type, Action: f.Action, Cursor: f.Cursor, Limit: limit}
}

// queryString renders the filters (without the cursor unless given) for links.
func (f auditFilters) queryString(cursor string) string {
	params := url.Values{}
	set := func(k, v string) {
		if v != "" {
			params.Set(k, v)
		}
	}
	set("tenant", f.Tenant)
	set("actor", f.Actor)
	set("user", f.User)
	set("ip", f.IP)
	set("type", f.Type)
	set("action", f.Action)
	set("cursor", cursor)
	if qs := params.Encode(); qs != "" {
		return "?" + qs
	}
	return ""
}

func auditView(row audit.Row) auditRowView {
	v := auditRowView{
		Time:      row.Timestamp.UTC().Format("Jan 2, 2006 15:04:05 UTC"),
		EventType: row.EventType,
		Action:    row.Action,
		Actor:     row.PerformedBy,
		Subject:   row.UserID,
		TenantID:  row.TenantID,
		Resource:  row.Resource,
		Result:    row.Result,
		Severity:  row.Severity,
		IP:        row.IP,
		UserAgent: row.UserAgent,
		Error:     row.Error,
		MetaJSON:  string(row.Metadata),
	}
	switch {
	case row.Result == "failure", row.Severity == "error":
		v.ResultClass = "danger"
	case row.Severity == "warning":
		v.ResultClass = "warning"
	default:
		v.ResultClass = "success"
	}
	v.MetaSummary = auditSummarize(v.MetaJSON)
	return v
}

func auditSummarize(s string) string {
	if s == "{}" {
		return ""
	}
	if len(s) <= 80 {
		return s
	}
	return s[:80] + "…"
}

// HandleAdminAudit renders GET /admin/audit.
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
		data["Events"] = []auditRowView{}
		data["HasMore"] = false

		f := parseAuditFilters(r)
		data["Filters"] = f
		data["ExportURL"] = "/admin/audit/export" + f.queryString("")

		if db != nil {
			page, err := audit.List(r.Context(), db, f.toAudit(auditPageSize))
			if err != nil {
				logger.Error("audit page query", zap.Error(err))
				data["QueryError"] = "Could not read the audit trail."
			} else {
				rows := make([]auditRowView, 0, len(page.Rows))
				for _, row := range page.Rows {
					rows = append(rows, auditView(row))
				}
				data["Events"] = rows
				data["HasMore"] = page.HasMore
				if page.HasMore {
					data["NextURL"] = "/admin/audit" + f.queryString(page.NextCursor)
				}
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render admin audit", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// auditExportMaxPages bounds one CSV download (pages × 100 rows).
const auditExportMaxPages = 200

// HandleAdminAuditExport streams GET /admin/audit/export as CSV with the same
// filters. Cells are formula-escaped (the metadata carries user-supplied
// e-mails and key names); the export itself is audited — it is a bulk read
// of client IPs and account identifiers.
func HandleAdminAuditExport(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		f := parseAuditFilters(r)
		audit.Record(r.Context(), db, audit.Entry{UserID: sd.UserID, EventType: "admin", Action: "admin.audit_exported",
			Metadata: map[string]any{"tenant": f.Tenant, "actor": f.Actor, "user": f.User, "ip": f.IP, "type": f.Type, "action": f.Action}})

		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition",
			`attachment; filename="audit-`+time.Now().Format("2006-01-02")+`.csv"`)

		cw := csv.NewWriter(w)
		defer cw.Flush()
		_ = cw.Write([]string{"timestamp", "event_type", "action", "result", "severity", "performed_by", "user_id",
			"tenant_id", "resource", "ip", "user_agent", "error", "metadata"})

		if db == nil {
			return
		}
		af := f.toAudit(100)
		af.Cursor = ""
		for i := 0; i < auditExportMaxPages; i++ {
			page, err := audit.List(r.Context(), db, af)
			if err != nil {
				logger.Error("audit export query", zap.Error(err))
				return
			}
			for _, row := range page.Rows {
				_ = cw.Write(csvSafeRow(row.Timestamp.UTC().Format(time.RFC3339), row.EventType, row.Action, row.Result,
					row.Severity, row.PerformedBy, row.UserID, row.TenantID, row.Resource, row.IP, row.UserAgent,
					row.Error, string(row.Metadata)))
			}
			if !page.HasMore {
				return
			}
			af.Cursor = page.NextCursor
		}
	}
}
