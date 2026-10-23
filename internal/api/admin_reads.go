package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/adminread"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
)

// Admin reads in the transparency log (K4c-1, Stage 1a Prompt 33;
// docs/ARCHITECTURE_CONTRACT.md rule 7: "an admin read of customer metadata"
// writes to the log; docs/TRANSPARENCY.md § Admin reads).
//
//   - Every admin GET route is classified (apiAdminReads here, the dashboard's
//     adminReadRoutes): a read of ONE identified tenant's records, a list
//     across tenants that shows per-tenant identity, or exempt (an aggregate,
//     with its reason). TestAdminReadRoutes_AllClassified walks both routers.
//   - A logged read appends its admin_read entry BEFORE the handler reads
//     anything (fail closed): an append that fails — a frozen log (RP42-1),
//     the database — refuses the read (API 503, dashboard 503 page). There is
//     no break-glass.
//   - The operator stays in audit_logs (admin.read, with the entry's index —
//     the reference the log itself does not reveal); the entry names a role.
//   - Flag translog_admin_reads (default OFF), keyed on the SUBJECT tenant
//     (per tenant first; a list reads the global row). OFF = no entry and no
//     refusal: the behaviour before K4c-1. ON wins (RP44-2): a tenant's row
//     can only turn it on; its writes are log_policy entries (translog_flags.go).
//   - One parser (RP44-1, D-P34a-1): a resolver reads the subject with the
//     handler's own parse — chi.URLParam for a path parameter, adminAuditFilter
//     for the audit filters — so the tenant logged is the tenant served.
//   - A subject that is no tenant (RP44-1, D-P34a-2) is a 404 and the handler
//     never runs; the audit filter of a tenant that no longer exists is read
//     as the audit list (an erased tenant's account.erased row stays readable).
//   - Customers read outcomes: GET /api/v1/manage/account/transparency lists
//     the tenant's own entries, behind translog_customer_view (default OFF,
//     per tenant first).

const (
	flagTranslogAdminReads   = translog.FlagTranslogAdminReads
	flagTranslogCustomerView = translog.FlagTranslogCustomerView
)

// adminReadRule classifies one admin GET route: exempt (with its reason),
// or resolve gives the scope read and, for a subject scope, the tenant.
type adminReadRule struct {
	exempt  string
	resolve func(r *http.Request, db *sql.DB) (scope, tenantID string, err error)
}

func fixedScope(scope string) func(*http.Request, *sql.DB) (string, string, error) {
	return func(*http.Request, *sql.DB) (string, string, error) { return scope, "", nil }
}

// urlTenant: the path parameter, read by chi.URLParam exactly as the
// handler reads it.
func urlTenant(scope, param string) func(*http.Request, *sql.DB) (string, string, error) {
	return func(r *http.Request, _ *sql.DB) (string, string, error) { return scope, chi.URLParam(r, param), nil }
}

// apiAuditScope: GET /api/v1/admin/audit filtered to one tenant (tenant_id)
// or one user (user_id, logged against the user's tenant) is that tenant's
// audit trail; unfiltered — or filtered to a tenant that no longer exists —
// it is the trail across tenants. The filter is the handler's own
// (adminAuditFilter): the one parser.
func apiAuditScope(r *http.Request, db *sql.DB) (string, string, error) {
	f := adminAuditFilter(r)
	if f.TenantID != "" {
		ok, err := adminread.TenantExists(r.Context(), db, f.TenantID)
		if err != nil {
			return "", "", err
		}
		if ok {
			return translog.ScopeTenantAudit, f.TenantID, nil
		}
		return translog.ScopeAuditList, "", nil
	}
	if f.UserID != "" {
		t, err := adminread.TenantOfUser(r.Context(), db, f.UserID)
		if err != nil {
			return "", "", err
		}
		if t != "" {
			return translog.ScopeTenantAudit, t, nil
		}
	}
	return translog.ScopeAuditList, "", nil
}

// apiAdminReads is the classification of every GET route of the
// /api/v1/admin and /api/compliance groups (both admin-only), by chi pattern.
var apiAdminReads = map[string]adminReadRule{
	"/api/v1/admin/tenants/{tenant_id}/payments":         {resolve: urlTenant(translog.ScopeTenantPayments, "tenant_id")},
	"/api/v1/admin/audit":                                {resolve: apiAuditScope},
	"/api/v1/admin/jobs":                                 {resolve: fixedScope(translog.ScopeJobList)},  // job notes and results name tenants, buckets, keys
	"/api/v1/admin/flags":                                {resolve: fixedScope(translog.ScopeFlagList)}, // the tenants a flag is set for
	"/api/v1/admin/routing-truth":                        {exempt: "counts of rows per (table, backend) and the job's counts: no tenant, bucket or key"},
	"/api/v1/admin/backends/{backend}/plan":              {exempt: "what we bought from a storage vendor: no customer in it"},
	"/api/v1/admin/breaches":                             {exempt: "the operator's breach register: no tenant or user column"},
	"/api/compliance/breach":                             {exempt: "the operator's breach register: no tenant or user column"},
	"/api/compliance/breach/{id}":                        {exempt: "one breach-register record: no tenant or user column"},
	"/api/compliance/breach/stats":                       {exempt: "breach-register counts"},
	"/api/compliance/sar/{id}":                           {exempt: "scaffolding (R11-01): answers a fixed pending status, reads nothing"},
	"/api/compliance/inventory":                          {exempt: "scaffolding (R11-01): reads a user context key requireJWT never sets — 401 for everyone"},
	"/api/compliance/activities":                         {exempt: "the operator's processing activities (ROPA): no customer"},
	"/api/compliance/consent":                            {exempt: "scaffolding (R11-01): reads a user context key requireJWT never sets — 401 for everyone"},
	"/api/compliance/consent/{purpose}":                  {exempt: "scaffolding (R11-01): reads a user context key requireJWT never sets — 401 for everyone"},
	"/api/compliance/consent/history":                    {exempt: "scaffolding (R11-01): reads a user context key requireJWT never sets — 401 for everyone"},
	"/api/compliance/consent/purposes":                   {exempt: "the list of consent purposes: no customer"},
	"/api/compliance/ropa/activities/{id}":               {exempt: "one processing activity (ROPA): no customer"},
	"/api/compliance/ropa/activities":                    {exempt: "the processing activities (ROPA): no customer"},
	"/api/compliance/ropa/report":                        {exempt: "the ROPA report: no customer"},
	"/api/compliance/ropa/compliance/{id}":               {exempt: "one activity's compliance check: no customer"},
	"/api/compliance/ropa/stats":                         {exempt: "ROPA counts"},
	"/api/compliance/privacy/purpose/{dataId}/{purpose}": {exempt: "scaffolding (R11-01): reads a user context key requireJWT never sets — 401 for everyone"},
	// The admin routes of another method that ANSWER customer data (RP43-6),
	// keyed "METHOD pattern"; every other one is in apiAdminWrites.
	"POST /api/v1/admin/routing-truth/resolve-null":      {resolve: fixedScope(translog.ScopeRoutingRows)}, // tenant, bucket, key of each NULL row
	"POST /api/v1/admin/chunk-orphans":                   {resolve: fixedScope(translog.ScopeChunkList)},   // blob keys under tenants' prefixes
	"POST /api/v1/admin/chunk-move":                      {resolve: fixedScope(translog.ScopeChunkList)},   // its errors name blob keys under tenants' prefixes
	"POST /api/v1/admin/tenants/{tenant_id}/payments":    {resolve: urlTenant(translog.ScopeTenantPayments, "tenant_id")},
	"POST /api/v1/admin/tenants/{tenant_id}/entitlement": {resolve: urlTenant(translog.ScopeTenantPayments, "tenant_id")},
	"POST /api/v1/admin/tenants/{tenant_id}/refunds":     {resolve: urlTenant(translog.ScopeTenantPayments, "tenant_id")},
	"POST /api/v1/admin/payments/stripe/stop-renewals":   {resolve: fixedScope(translog.ScopeBillingList)}, // the dry run lists the tenants
	"POST /api/v1/admin/payments/stripe/resume-renewals": {resolve: fixedScope(translog.ScopeBillingList)},
}

// apiAdminWrites classifies every other admin route of another method than
// GET (RP43-6): it changes state and answers a status, a count, or what the
// caller sent — no customer metadata. A route that answered some would be
// in apiAdminReads, through the gate. TestAdminRoutes_EveryMethodClassified
// walks the router.
var apiAdminWrites = map[string]string{
	"POST /api/v1/admin/jobs/{job}/run":                            "mutation: starts a job; 202 or 409",
	"POST /api/v1/admin/dedup-gc":                                  "mutation: starts the job; 202 or 409",
	"POST /api/v1/admin/smart-demotion":                            "mutation: starts the job; 202 or 409",
	"POST /api/v1/admin/retention":                                 "mutation: starts the job; 202 or 409",
	"POST /api/v1/admin/account-deletion":                          "mutation: starts the job; 202 or 409",
	"POST /api/v1/admin/quota-reconcile":                           "mutation: recounts usage; answers a count (or the tenant id sent)",
	"POST /api/v1/admin/tenants/{tenant_id}/encryption-key/rotate": "mutation: rotates the tenant key; answers the new version number",
	"POST /api/v1/admin/translog/ceremony":                         "mutation: appends a ceremony entry; answers its index",
	"POST /api/v1/admin/breach":                                    "mutation: the operator's breach register; no customer",
	"PATCH /api/v1/admin/breach/{id}":                              "mutation: the operator's breach register; no customer",
	"PUT /api/v1/admin/backends/{backend}/plan":                    "mutation: the vendor plan row; no customer",
	"PUT /api/v1/admin/flags/{key}":                                "mutation: writes a flag row (a translog_* row is a log_policy entry first); answers the flag and the one row written, never the others",
	"DELETE /api/v1/admin/flags/{key}":                             "mutation: removes a flag row (a translog_* row is a log_policy entry first); answers the flag, never the other tenants' rows",
	"POST /api/compliance/breach":                                  "mutation: the operator's breach register; no customer",
	"PATCH /api/compliance/breach/{id}":                            "mutation: the operator's breach register; no customer",
	"POST /api/compliance/breach/{id}/notify":                      "mutation: the operator's breach register; no customer",
	"POST /api/compliance/ropa/activities":                         "mutation: a processing activity (ROPA); no customer",
	"PATCH /api/compliance/ropa/activities/{id}":                   "mutation: a processing activity (ROPA); no customer",
	"DELETE /api/compliance/ropa/activities/{id}":                  "mutation: a processing activity (ROPA); no customer",
	"POST /api/compliance/ropa/activities/{id}/review":             "mutation: a processing activity (ROPA); no customer",
	"POST /api/compliance/sar":                                     "mutation: scaffolding (R11-01) — answers the request it was sent",
	"POST /api/compliance/deletion":                                "mutation: scaffolding (R11-01) — answers the request it was sent",
	"POST /api/compliance/consent":                                 "mutation: scaffolding (R11-01) — reads a user context key requireJWT never sets",
	"DELETE /api/compliance/consent/{purpose}":                     "mutation: scaffolding (R11-01) — reads a user context key requireJWT never sets",
	"POST /api/compliance/privacy/controls":                        "mutation: scaffolding (R11-01) — answers what it was sent",
	"POST /api/compliance/privacy/minimize":                        "mutation: scaffolding (R11-01) — answers the data it was sent, minimized",
	"POST /api/compliance/privacy/pseudonymize":                    "mutation: scaffolding (R11-01) — answers the data it was sent, pseudonymized",
}

// adminReadWrapped records every pattern registered through adminRead:
// TestAdminReadRoutes_AllClassified checks each admin GET route went
// through it (the wrapper is on the route, not only in the map).
var adminReadWrapped sync.Map

// adminRead wraps the handler of the admin GET route pattern (its full chi
// pattern; "POST <pattern>" for a POST that answers customer data) with its
// classification: a logged route appends its entry first and refuses the
// read when that fails. A pattern with no rule panics at registration — a
// new admin read cannot ship unclassified.
func (s *Server) adminRead(pattern string, h http.HandlerFunc) http.HandlerFunc {
	rule, ok := apiAdminReads[pattern]
	if !ok {
		panic(fmt.Sprintf("admin read %s is not classified in apiAdminReads (internal/api/admin_reads.go)", pattern))
	}
	adminReadWrapped.Store(pattern, true)
	if rule.exempt != "" {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		actor, _ := r.Context().Value(userIDKey).(string)
		scope, tenantID, err := rule.resolve(r, s.db)
		if err == nil {
			record := s.RecordAdminRead
			if s.adminReadHook != nil {
				record = s.adminReadHook
			}
			err = record(r.Context(), actor, scope, tenantID)
		}
		if errors.Is(err, adminread.ErrNoSuchTenant) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		if err != nil {
			code := "admin_read_not_logged"
			if errors.Is(err, logstore.ErrLogFrozen) {
				code = "log_frozen"
			}
			s.log().Error("admin read refused: its transparency-log entry could not be appended", zap.Error(err),
				zap.String("route", pattern), zap.String("scope", scope))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": code})
			return
		}
		h(w, r)
	}
}

// RecordAdminRead appends the admin_read entry of one read when
// translog_admin_reads is on for the subject tenant (the global row for a
// list) — the dashboard's recorder too. A subject scope naming no tenant
// that exists is adminread.ErrNoSuchTenant, whatever the flag (RP44-1): the
// gates answer 404 and the handler never runs — never "nothing to record"
// while it reads (D-P33-15 withdrawn). The audit row admin.read names the
// operator and the entry's index; the entry names neither.
func (s *Server) RecordAdminRead(ctx context.Context, actorID, scope, tenantID string) error {
	if translog.ScopeHasSubject(scope) {
		exists, err := adminread.TenantExists(ctx, s.db, tenantID)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("admin read %s of %q: %w", scope, tenantID, adminread.ErrNoSuchTenant)
		}
	}
	if s.flags == nil || !s.flags.Enabled(flagTranslogAdminReads, tenantID) {
		return nil
	}
	if s.adminReads == nil {
		return errors.New("admin read: the transparency log is not configured")
	}
	idx, appended, err := s.adminReads.Record(ctx, actorID, scope, tenantID)
	if err != nil {
		return err
	}
	if appended {
		audit.Record(ctx, s.db, audit.Entry{UserID: actorID, TenantID: tenantID, EventType: "admin", Action: "admin.read",
			Metadata: map[string]any{"scope": scope, "translog_index": idx}})
	}
	return nil
}

// handleMgmtAccountTransparency is GET /api/v1/manage/account/transparency:
// the tenant's own admin-read entries — index, scope, time and the first
// signed checkpoint whose tree includes each (null = not signed yet) —
// found by recomputing each entry's commitment under the tenant's salt.
// Dark behind translog_customer_view (OFF = 404).
func (s *Server) handleMgmtAccountTransparency(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" || s.flags == nil || !s.flags.Enabled(flagTranslogCustomerView, tenantID) {
		writeManagementError(w, ErrTypeNotFound, "not_found", "not found", "")
		return
	}
	if s.adminReads == nil {
		writeManagementError(w, ErrTypeAPI, "transparency_unavailable", "the transparency log is not available on this deployment", "")
		return
	}
	found, err := s.adminReads.Find(r.Context(), tenantID)
	if err != nil {
		s.log().Error("account transparency: entries not read", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "transparency_unavailable", "the transparency log cannot be read right now", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "origin": translog.Origin, "data": found})
}
