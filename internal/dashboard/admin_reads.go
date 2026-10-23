package dashboard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/adminread"
)

// Admin reads in the transparency log (K4c-1, Stage 1a Prompt 33;
// internal/api/admin_reads.go is the API side and the recorder). Every GET
// page of the /admin group is classified here, by its chi pattern; the
// router registers them through adminGet, which panics on a page with no
// rule, and TestAdminReadRoutes_AllClassified walks the router. Every other
// method's admin route is in adminWriteRoutes (RP43-6).
//
// One parser (RP44-1, D-P34a-1): each resolver reads its subject with the
// handler's own function (handlers.AdminTenantID, handlers.ParseAuditFilters,
// handlers.AbuseReportID, handlers.SupportSearchSubject), so the tenant
// logged is the tenant the page serves. A subject that is no tenant is a
// 404 before the page runs (the recorder's adminread.ErrNoSuchTenant).

// AdminReadRecorder appends the admin_read entry of one read (the API
// server's RecordAdminRead): nil error = recorded, or not to be recorded
// (the translog_admin_reads flag off for the subject tenant).
type AdminReadRecorder interface {
	RecordAdminRead(ctx context.Context, actorID, scope, tenantID string) error
}

type adminReadRule struct {
	exempt  string
	resolve func(r *http.Request, db *sql.DB) (scope, tenantID string, err error)
}

func readScope(scope string) func(*http.Request, *sql.DB) (string, string, error) {
	return func(*http.Request, *sql.DB) (string, string, error) { return scope, "", nil }
}

func readTenantParam(scope string) func(*http.Request, *sql.DB) (string, string, error) {
	return func(r *http.Request, _ *sql.DB) (string, string, error) { return scope, handlers.AdminTenantID(r), nil }
}

// auditScope: /admin/audit (and its export) filtered to one tenant
// (?tenant=) or one user (?user=, logged against the user's tenant) is that
// tenant's audit trail (subject: tenant_audit viewed, tenant_audit_export
// exported — RP44-4); otherwise, or filtered to a tenant that no longer
// exists, the trail across tenants (an actor or IP filter is still a list).
// The filters are the page's own (handlers.ParseAuditFilters).
func auditScope(subject, list string) func(*http.Request, *sql.DB) (string, string, error) {
	return func(r *http.Request, db *sql.DB) (string, string, error) {
		f := handlers.ParseAuditFilters(r)
		if f.Tenant != "" {
			ok, err := adminread.TenantExists(r.Context(), db, f.Tenant)
			if err != nil {
				return "", "", err
			}
			if ok {
				return subject, f.Tenant, nil
			}
			return list, "", nil
		}
		if f.User != "" {
			t, err := adminread.TenantOfUser(r.Context(), db, f.User)
			if err != nil {
				return "", "", err
			}
			if t != "" {
				return subject, t, nil
			}
		}
		return list, "", nil
	}
}

// supportSearchScope: /admin/support?q= that finds exactly one tenant is
// that tenant's support case (RP43-4) — found by the page's own search;
// otherwise the tenant list.
func supportSearchScope(r *http.Request, db *sql.DB) (string, string, error) {
	if t := handlers.SupportSearchSubject(r.Context(), db, r, zap.NewNop()); t != "" {
		return translog.ScopeSupportCase, t, nil
	}
	return translog.ScopeTenantList, "", nil
}

// abuseCaseScope: one abuse report and the tenant it names
// (abuse_reports.tenant_id); a report that names none — or a tenant that
// no longer exists — is abuse_report.
func abuseCaseScope(r *http.Request, db *sql.DB) (string, string, error) {
	id, err := handlers.AbuseReportID(r)
	if err != nil {
		return translog.ScopeAbuseReport, "", nil // the handler answers its 400/404; nothing is read
	}
	if db == nil {
		return "", "", errors.New("admin read: no database to resolve the abuse report's tenant")
	}
	var tenant sql.NullString
	err = db.QueryRowContext(r.Context(), `SELECT tenant_id FROM abuse_reports WHERE id = $1`, id).Scan(&tenant)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return translog.ScopeAbuseReport, "", nil
	case err != nil:
		return "", "", fmt.Errorf("admin read: the abuse report's tenant: %w", err)
	case tenant.Valid && tenant.String != "":
		ok, err := adminread.TenantExists(r.Context(), db, tenant.String)
		if err != nil {
			return "", "", err
		}
		if ok {
			return translog.ScopeAbuseCase, tenant.String, nil
		}
	}
	return translog.ScopeAbuseReport, "", nil
}

// adminReadRoutes classifies every GET page of the /admin group (the
// inventory: docs/reviews/WP-P33.md § K4c-1).
var adminReadRoutes = map[string]adminReadRule{
	"/admin/":                    {resolve: readScope(translog.ScopeTenantList)}, // the ten newest users' e-mail and company
	"/admin/tenants":             {resolve: readScope(translog.ScopeTenantList)},
	"/admin/tenants/{id}":        {resolve: readTenantParam(translog.ScopeTenantDetail)},
	"/admin/system":              {resolve: readScope(translog.ScopeJobList)}, // job notes name tenants, buckets, keys
	"/admin/audit":               {resolve: auditScope(translog.ScopeTenantAudit, translog.ScopeAuditList)},
	"/admin/audit/export":        {resolve: auditScope(translog.ScopeTenantAuditCSV, translog.ScopeAuditExport)},
	"/admin/waitlist":            {resolve: readScope(translog.ScopeWaitlistList)},
	"/admin/waitlist/export":     {resolve: readScope(translog.ScopeWaitlistExport)},
	"/admin/stats":               {exempt: "cookieless site statistics and daily counts: no tenant, no e-mail"},
	"/admin/revenue":             {resolve: readScope(translog.ScopeBillingList)}, // the top customers' e-mail
	"/admin/costs":               {resolve: readScope(translog.ScopeBillingList)}, // margin per tenant, by e-mail
	"/admin/dedup":               {resolve: readScope(translog.ScopeUsageList)},   // dedup per tenant, by e-mail
	"/admin/notifications":       {resolve: readScope(translog.ScopeNoticeList)},
	"/admin/notifications/count": {exempt: "one count of unread notifications"},
	"/admin/abuse":               {resolve: readScope(translog.ScopeAbuseList)},
	"/admin/abuse/{id}":          {resolve: abuseCaseScope},
	"/admin/support":             {resolve: supportSearchScope}, // a search over tenants' e-mails, ids, keys
	"/admin/support/{id}":        {resolve: readTenantParam(translog.ScopeSupportCase)},
	"/admin/backends":            {exempt: "the engine's drivers and their health: no customer"},
	"/admin/flags":               {resolve: readScope(translog.ScopeFlagList)}, // the tenants a flag is set for
}

// adminReadWrapped records every page registered through adminGet (the
// router walk checks each admin GET page went through it).
var adminReadWrapped sync.Map

// AdminReadWrapped reports whether the admin page pattern was registered
// through adminGet — with its classification's gate.
func AdminReadWrapped(pattern string) bool {
	_, ok := adminReadWrapped.Load(pattern)
	return ok
}

// adminGet registers the admin GET page sub (its pattern inside /admin)
// with its classification: a logged page appends its entry BEFORE the
// handler runs and answers 503 when that fails — fail closed, no
// break-glass.
func adminGet(ar chi.Router, deps Deps, sub string, h http.HandlerFunc) {
	pattern := "/admin" + sub
	rule, ok := adminReadRoutes[pattern]
	if !ok {
		panic(fmt.Sprintf("admin page %s is not classified in adminReadRoutes (internal/dashboard/admin_reads.go)", pattern))
	}
	adminReadWrapped.Store(pattern, true)
	if rule.exempt != "" || deps.AdminReads == nil {
		ar.Get(sub, h)
		return
	}
	ar.Get(sub, func(w http.ResponseWriter, r *http.Request) {
		actor := ""
		if sd := dashauth.GetSession(r.Context()); sd != nil {
			actor = sd.UserID
		}
		scope, tenantID, err := rule.resolve(r, deps.DB)
		if err == nil {
			err = deps.AdminReads.RecordAdminRead(r.Context(), actor, scope, tenantID)
		}
		if errors.Is(err, adminread.ErrNoSuchTenant) {
			w.Header().Set("Cache-Control", "no-store")
			http.NotFound(w, r)
			return
		}
		if err != nil {
			if deps.Logger != nil {
				deps.Logger.Error("admin page refused: its transparency-log entry could not be appended", zap.Error(err),
					zap.String("page", pattern), zap.String("scope", scope))
			}
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "Service Unavailable: this read could not be recorded in the transparency log, so it is not shown (docs/TRANSPARENCY.md § Admin reads).",
				http.StatusServiceUnavailable)
			return
		}
		h(w, r)
	})
}

// adminWriteRoutes classifies every admin route of another method than GET,
// keyed "METHOD pattern" (RP43-6): each changes state and answers a
// redirect, a flash or a status — none answers customer metadata, so none
// is an admin read (a POST that did would go through the gate).
var adminWriteRoutes = map[string]string{
	"POST /admin/tenants/{id}/suspend":             "mutation: suspends the tenant; redirects",
	"POST /admin/tenants/{id}/enable":              "mutation: re-enables the tenant; redirects",
	"POST /admin/tenants/{id}/quota":               "mutation: sets the quota; redirects",
	"POST /admin/tenants/{id}/tier":                "mutation: sets the tier; redirects",
	"POST /admin/tenants/{id}/bandwidth-limit":     "mutation: sets the egress override; redirects",
	"POST /admin/tenants/{id}/reset-mfa":           "mutation: resets the user's MFA; redirects",
	"POST /admin/tenants/{id}/billing/payment":     "mutation: records a manual payment; redirects",
	"POST /admin/tenants/{id}/billing/entitlement": "mutation: adjusts the entitlement; redirects",
	"POST /admin/tenants/{id}/billing/refund":      "mutation: records a refund; redirects",
	"POST /admin/notifications/read-all":           "mutation: marks notifications read",
	"POST /admin/notifications/{id}/read":          "mutation: marks one notification read",
	"POST /admin/abuse/{id}/action":                "mutation: resolves or dismisses the report; redirects",
	"POST /admin/support/{id}/notes":               "mutation: adds a note; redirects",
	"POST /admin/backends/{name}/primary":          "mutation: swaps the primary; redirects",
	"POST /admin/backends/{name}/check":            "mutation: runs a health check; redirects",
	"POST /admin/flags/{key}/set":                  "mutation: writes a flag row (a translog_* row is a log_policy entry first); redirects",
	"POST /admin/flags/{key}/clear":                "mutation: removes a flag row (a translog_* row is a log_policy entry first); redirects",
}

// AdminWriteClassification is adminWriteRoutes, for the router walk.
func AdminWriteClassification() map[string]string {
	out := make(map[string]string, len(adminWriteRoutes))
	for k, v := range adminWriteRoutes {
		out[k] = v
	}
	return out
}

// AdminReadClassification is the classification of every admin page by its
// chi pattern — "log" or "exempt: <reason>" — for the router walk
// (internal/api TestAdminReadRoutes_AllClassified) and the inventory.
func AdminReadClassification() map[string]string {
	out := make(map[string]string, len(adminReadRoutes))
	for p, r := range adminReadRoutes {
		if r.exempt != "" {
			out[p] = "exempt: " + r.exempt
			continue
		}
		out[p] = "log"
	}
	return out
}
