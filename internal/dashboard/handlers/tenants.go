package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/FairForge/vaultaire/internal/audit"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/usage"
	"go.uber.org/zap"
)

// tierLimits maps plan names to storage limits in bytes.
var tierLimits = map[string]int64{
	"starter": 1099511627776,  // 1 TB
	"vault3":  3298534883328,  // 3 TB
	"vault9":  9895604649984,  // 9 TB
	"vault18": 19791209299968, // 18 TB
	"vault36": 39582418599936, // 36 TB
}

type tenantRow struct {
	ID              string
	Name            string
	Email           string
	Plan            string
	Status          string
	StatusClass     string
	StorageUsedFmt  string
	StorageLimitFmt string
	UsagePercent    int
	UsageBarClass   string
	House           string // "6 TB downstairs · 1 TB attic" for house tenants (Phase 4)
}

// houseSummary is the tenant's house in the site's words, "" without one.
func houseSummary(stdBytes, vaultBytes int64) string {
	var parts []string
	if stdBytes > 0 {
		parts = append(parts, fmt.Sprintf("%d TB downstairs", stdBytes/usage.TB))
	}
	if vaultBytes > 0 {
		parts = append(parts, fmt.Sprintf("%d TB attic", vaultBytes/usage.TB))
	}
	return strings.Join(parts, " · ")
}

// HandleTenantList renders the admin tenant list page.
func HandleTenantList(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "admin-tenants")
		withCSRF(r.Context(), data)
		query := r.URL.Query().Get("q")
		data["SearchQuery"] = query

		if db != nil {
			data["Tenants"] = queryTenantList(r.Context(), db, query, logger)
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render tenant list", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

func queryTenantList(ctx context.Context, db *sql.DB, search string, logger *zap.Logger) []tenantRow {
	rows, err := db.QueryContext(ctx, `
		SELECT t.id, t.name, t.email, COALESCE(t.plan, 'starter'),
		       t.subscription_status, t.suspended_at,
		       COALESCE(q.storage_used_bytes, 0), COALESCE(q.storage_limit_bytes, 1099511627776),
		       COALESCE((SELECT storage_limit_bytes FROM tenant_floor_quotas WHERE tenant_id = t.id AND floor = 'standard'), 0),
		       COALESCE((SELECT storage_limit_bytes FROM tenant_floor_quotas WHERE tenant_id = t.id AND floor = 'vault'), 0)
		FROM tenants t
		LEFT JOIN tenant_quotas q ON q.tenant_id = t.id
		WHERE ($1 = '' OR t.name ILIKE '%' || $1 || '%' OR t.email ILIKE '%' || $1 || '%')
		ORDER BY t.created_at DESC
		LIMIT 100
	`, search)
	if err != nil {
		logger.Debug("admin: list tenants", zap.Error(err))
		return nil
	}
	defer func() { _ = rows.Close() }()

	var tenants []tenantRow
	for rows.Next() {
		var tr tenantRow
		var subStatus string
		var suspendedAt sql.NullTime
		var usedBytes, limitBytes, stdBytes, vaultBytes int64
		if err := rows.Scan(&tr.ID, &tr.Name, &tr.Email, &tr.Plan,
			&subStatus, &suspendedAt, &usedBytes, &limitBytes, &stdBytes, &vaultBytes); err != nil {
			continue
		}
		tr.House = houseSummary(stdBytes, vaultBytes)
		tr.StorageUsedFmt = formatBytes(usedBytes)
		tr.StorageLimitFmt = formatBytes(limitBytes)
		if limitBytes > 0 {
			tr.UsagePercent = int(usedBytes * 100 / limitBytes)
		}
		tr.UsageBarClass = usageBarClass(tr.UsagePercent)
		tr.Status, tr.StatusClass = tenantStatus(subStatus, suspendedAt.Valid)
		tenants = append(tenants, tr)
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	return tenants
}

func tenantStatus(subStatus string, isSuspended bool) (string, string) {
	if isSuspended {
		return "suspended", "danger"
	}
	switch subStatus {
	case "active":
		return "active", "success"
	case "past_due":
		return "past due", "warning"
	case "canceled":
		return "canceled", "default"
	default:
		return "free", "default"
	}
}

func usageBarClass(pct int) string {
	switch {
	case pct >= 90:
		return "danger"
	case pct >= 75:
		return "warning"
	default:
		return ""
	}
}

// HandleTenantDetail renders the admin tenant detail page.
func HandleTenantDetail(tmpl *template.Template, db *sql.DB, logger *zap.Logger, eg usage.EgressStatusReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		tenantID := chi.URLParam(r, "id")
		if tenantID == "" {
			http.NotFound(w, r)
			return
		}

		if db == nil {
			http.NotFound(w, r)
			return
		}

		data := sessionData(sd, "admin-tenants")
		withCSRF(r.Context(), data)
		if !loadTenantDetail(r.Context(), db, eg, tenantID, data, logger) {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render tenant detail", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

type tierOption struct {
	Value string
	Label string
}

var availableTiers = []tierOption{
	{"starter", "Starter (1 TB)"},
	{"vault3", "Vault3 (3 TB)"},
	{"vault9", "Vault9 (9 TB)"},
	{"vault18", "Vault18 (18 TB)"},
	{"vault36", "Vault36 (36 TB)"},
}

func loadTenantDetail(ctx context.Context, db *sql.DB, eg usage.EgressStatusReader, tenantID string, data map[string]any, logger *zap.Logger) bool {
	var name, email, plan, subStatus string
	var stripeCustomerID, stripeSubID sql.NullString
	var suspendedAt sql.NullTime
	var createdAt sql.NullTime

	err := db.QueryRowContext(ctx, `
		SELECT t.name, t.email, COALESCE(t.plan, 'starter'), t.subscription_status,
		       t.stripe_customer_id, t.stripe_subscription_id, t.suspended_at, t.created_at
		FROM tenants t WHERE t.id = $1
	`, tenantID).Scan(&name, &email, &plan, &subStatus,
		&stripeCustomerID, &stripeSubID, &suspendedAt, &createdAt)
	if err != nil {
		if err != sql.ErrNoRows {
			logger.Debug("admin: tenant detail", zap.Error(err))
		}
		return false
	}

	data["TenantID"] = tenantID
	data["TenantName"] = name
	data["TenantEmail"] = email
	data["Plan"] = plan
	data["IsSuspended"] = suspendedAt.Valid

	status, statusClass := tenantStatus(subStatus, suspendedAt.Valid)
	data["Status"] = status
	data["StatusClass"] = statusClass

	subLabel, subClass := subStatusDisplay(subStatus)
	data["SubStatusLabel"] = subLabel
	data["SubStatusClass"] = subClass

	if createdAt.Valid {
		data["CreatedFmt"] = createdAt.Time.Format("Jan 2, 2006")
	}

	data["AvailableTiers"] = availableTiers

	// Load quota data.
	var usedBytes, limitBytes int64
	var tier string
	err = db.QueryRowContext(ctx, `
		SELECT COALESCE(storage_used_bytes, 0), COALESCE(storage_limit_bytes, 1099511627776),
		       COALESCE(tier, 'starter')
		FROM tenant_quotas WHERE tenant_id = $1
	`, tenantID).Scan(&usedBytes, &limitBytes, &tier)
	if err != nil && err != sql.ErrNoRows {
		logger.Debug("admin: tenant quota", zap.Error(err))
	}
	data["StorageUsedFmt"] = formatBytes(usedBytes)
	data["StorageLimitFmt"] = formatBytes(limitBytes)
	data["StorageLimitGB"] = limitBytes / (1024 * 1024 * 1024)
	pct := 0
	if limitBytes > 0 {
		pct = int(usedBytes * 100 / limitBytes)
	}
	data["StoragePercent"] = pct
	data["StorageBarClass"] = usageBarClass(pct)
	data["Tier"] = tier

	// Bandwidth: current month totals + 30-day chart.
	ingress, egress, requests, _ := QueryMonthBandwidth(ctx, db, tenantID)
	data["IngressFmt"] = formatBytes(ingress)
	data["EgressFmt"] = formatBytes(egress)
	data["BandwidthTotalFmt"] = formatBytes(ingress + egress)
	data["RequestsCount"] = requests

	// Egress allowance: used / allowance / override / state, from the same
	// definition and counter the throttle uses (WP-R10-9).
	populateEgress(ctx, db, eg, tenantID, data)

	days, _ := QueryBandwidthDays(ctx, db, tenantID)
	if len(days) > 0 {
		data["ChartBars"] = BuildChartBars(days)
		data["HasChartData"] = true
		var maxVal int64
		for _, d := range days {
			total := d.Ingress + d.Egress
			if total > maxVal {
				maxVal = total
			}
		}
		data["ChartMaxLabel"] = formatBytes(maxVal)
	} else {
		data["ChartBars"] = nil
		data["HasChartData"] = false
		data["ChartMaxLabel"] = "0 B"
	}

	return true
}

func subStatusDisplay(s string) (string, string) {
	switch s {
	case "active":
		return "Active", "success"
	case "past_due":
		return "Past Due", "warning"
	case "canceled":
		return "Canceled", "default"
	default:
		return "None", "default"
	}
}

// HandleSuspendTenant sets suspended_at on a tenant and returns an htmx fragment.
func HandleSuspendTenant(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		tenantID := chi.URLParam(r, "id")
		if tenantID == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		if db == nil {
			http.Error(w, "Database not available", http.StatusInternalServerError)
			return
		}

		if _, err := db.ExecContext(r.Context(),
			`UPDATE tenants SET suspended_at = NOW() WHERE id = $1 AND suspended_at IS NULL`,
			tenantID); err != nil {
			logger.Error("suspend tenant", zap.String("tenant_id", tenantID), zap.Error(err))
			http.Error(w, "Failed to suspend tenant", http.StatusInternalServerError)
			return
		}

		audit.Record(r.Context(), db, audit.Entry{UserID: sd.UserID, TenantID: tenantID, EventType: "admin", Action: "admin.tenant_suspended", Resource: "tenant:" + tenantID, Severity: "warning"})
		logger.Info("tenant suspended", zap.String("tenant_id", tenantID), zap.String("by", sd.Email))

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<span class="badge badge-danger">suspended</span>`+
			` <button class="btn btn-sm btn-primary"`+
			` hx-post="/admin/tenants/%s/enable"`+
			` hx-target="#tenant-status"`+
			` hx-swap="innerHTML"`+
			` hx-confirm="Enable this tenant?">Enable</button>`, template.HTMLEscapeString(tenantID))
	}
}

// HandleEnableTenant clears suspended_at and returns an htmx fragment.
func HandleEnableTenant(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		tenantID := chi.URLParam(r, "id")
		if tenantID == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		if db == nil {
			http.Error(w, "Database not available", http.StatusInternalServerError)
			return
		}

		if _, err := db.ExecContext(r.Context(),
			`UPDATE tenants SET suspended_at = NULL WHERE id = $1`,
			tenantID); err != nil {
			logger.Error("enable tenant", zap.String("tenant_id", tenantID), zap.Error(err))
			http.Error(w, "Failed to enable tenant", http.StatusInternalServerError)
			return
		}

		audit.Record(r.Context(), db, audit.Entry{UserID: sd.UserID, TenantID: tenantID, EventType: "admin", Action: "admin.tenant_enabled", Resource: "tenant:" + tenantID})
		logger.Info("tenant enabled", zap.String("tenant_id", tenantID), zap.String("by", sd.Email))

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<span class="badge badge-success">active</span>`+
			` <button class="btn btn-sm btn-danger"`+
			` hx-post="/admin/tenants/%s/suspend"`+
			` hx-target="#tenant-status"`+
			` hx-swap="innerHTML"`+
			` hx-confirm="Suspend this tenant? They will lose API access.">Suspend</button>`, template.HTMLEscapeString(tenantID))
	}
}

// hasHouse reports whether the tenant bought a house: its quota lives in
// tenant_floor_quotas (per floor) and the total is their sum. An admin edit
// of the total or the legacy tier would desync the two ledgers and be
// overwritten by the next Stripe event (R10-21 / WP-R10-5), so those edits
// are refused for house tenants until the page edits floors.
func hasHouse(ctx context.Context, db *sql.DB, tenantID string) bool {
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenant_floor_quotas WHERE tenant_id = $1`, tenantID).Scan(&n)
	return n > 0
}

const houseEditRefused = `<div class="alert alert-error">This tenant bought a house: its quota is the sum of the floors in tenant_floor_quotas and is set by Stripe. Resize the house from the customer's billing page (or SetHouse) — editing the total here would desync the floors.</div>`

// HandleUpdateQuota updates a tenant's storage limit and returns an htmx fragment.
func HandleUpdateQuota(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		tenantID := chi.URLParam(r, "id")
		if tenantID == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		limitGB, err := strconv.ParseInt(r.FormValue("storage_limit"), 10, 64)
		if err != nil || limitGB <= 0 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `<div class="alert alert-error">Invalid storage limit. Enter a positive number in GB.</div>`)
			return
		}

		if db == nil {
			http.Error(w, "Database not available", http.StatusInternalServerError)
			return
		}

		if hasHouse(r.Context(), db, tenantID) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprint(w, houseEditRefused)
			return
		}

		limitBytes := limitGB * 1024 * 1024 * 1024
		if _, err := db.ExecContext(r.Context(),
			`UPDATE tenant_quotas SET storage_limit_bytes = $1, updated_at = NOW() WHERE tenant_id = $2`,
			limitBytes, tenantID); err != nil {
			logger.Error("update quota", zap.String("tenant_id", tenantID), zap.Error(err))
			http.Error(w, "Failed to update quota", http.StatusInternalServerError)
			return
		}

		audit.Record(r.Context(), db, audit.Entry{UserID: sd.UserID, TenantID: tenantID, EventType: "admin", Action: "admin.tenant_quota_set", Resource: "tenant:" + tenantID,
			Metadata: map[string]any{"storage_limit_bytes": limitBytes}})
		logger.Info("quota updated",
			zap.String("tenant_id", tenantID),
			zap.Int64("limit_gb", limitGB),
			zap.String("by", sd.Email))

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<div class="alert alert-success">Storage limit updated to %s.</div>`, formatBytes(limitBytes))
	}
}

// HandleUpdateBandwidthLimit sets or clears a tenant's egress-allowance
// override (tenant_quotas.bandwidth_limit_bytes). 0 clears it: the tenant is
// back on the plan's allowance (the house, or half the single quota). It is
// not "unlimited" — exempting a tenant from the throttle is a per-tenant
// egress_throttle flag row (/admin/flags).
func HandleUpdateBandwidthLimit(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		tenantID := chi.URLParam(r, "id")
		if tenantID == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		limitGB, err := strconv.ParseInt(r.FormValue("bandwidth_limit"), 10, 64)
		if err != nil || limitGB < 0 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `<div class="alert alert-error">Invalid egress allowance. Enter a non-negative number in GB (0 = the plan's allowance).</div>`)
			return
		}

		if db == nil {
			http.Error(w, "Database not available", http.StatusInternalServerError)
			return
		}

		var limitBytes *int64
		if limitGB > 0 {
			v := limitGB * 1024 * 1024 * 1024
			limitBytes = &v
		}

		if _, err := db.ExecContext(r.Context(),
			`UPDATE tenant_quotas SET bandwidth_limit_bytes = $1, updated_at = NOW() WHERE tenant_id = $2`,
			limitBytes, tenantID); err != nil {
			logger.Error("update egress allowance override", zap.String("tenant_id", tenantID), zap.Error(err))
			http.Error(w, "Failed to update egress allowance", http.StatusInternalServerError)
			return
		}

		audit.Record(r.Context(), db, audit.Entry{UserID: sd.UserID, TenantID: tenantID, EventType: "admin", Action: "admin.tenant_bandwidth_set", Resource: "tenant:" + tenantID,
			Metadata: map[string]any{"bandwidth_limit_bytes": limitBytes}})
		logger.Info("egress allowance override updated",
			zap.String("tenant_id", tenantID),
			zap.Int64("limit_gb", limitGB),
			zap.String("by", sd.Email))

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if limitGB == 0 {
			_, _ = fmt.Fprint(w, `<div class="alert alert-success">Override removed: this tenant is on its plan's egress allowance. The throttle picks it up within a minute.</div>`)
		} else {
			_, _ = fmt.Fprintf(w, `<div class="alert alert-success">Egress allowance override set to %s/month. The throttle picks it up within a minute.</div>`, formatBytes(*limitBytes))
		}
	}
}

// HandleChangeTier changes a tenant's plan and adjusts quota limits.
func HandleChangeTier(db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		tenantID := chi.URLParam(r, "id")
		if tenantID == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		newTier := r.FormValue("tier")
		limitBytes, ok := tierLimits[newTier]
		if !ok {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `<div class="alert alert-error">Invalid tier.</div>`)
			return
		}

		if db == nil {
			http.Error(w, "Database not available", http.StatusInternalServerError)
			return
		}

		if hasHouse(r.Context(), db, tenantID) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprint(w, houseEditRefused)
			return
		}

		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			logger.Error("begin tx for tier change", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		defer func() { _ = tx.Rollback() }()

		if _, err := tx.ExecContext(r.Context(),
			`UPDATE tenants SET plan = $1 WHERE id = $2`, newTier, tenantID); err != nil {
			logger.Error("update tenant plan", zap.Error(err))
			http.Error(w, "Failed to change tier", http.StatusInternalServerError)
			return
		}

		if _, err := tx.ExecContext(r.Context(),
			`UPDATE tenant_quotas SET tier = $1, storage_limit_bytes = $2, updated_at = NOW() WHERE tenant_id = $3`,
			newTier, limitBytes, tenantID); err != nil {
			logger.Error("update tenant quota tier", zap.Error(err))
			http.Error(w, "Failed to change tier", http.StatusInternalServerError)
			return
		}

		if err := tx.Commit(); err != nil {
			logger.Error("commit tier change", zap.Error(err))
			http.Error(w, "Failed to change tier", http.StatusInternalServerError)
			return
		}

		audit.Record(r.Context(), db, audit.Entry{UserID: sd.UserID, TenantID: tenantID, EventType: "admin", Action: "admin.tenant_tier_set", Resource: "tenant:" + tenantID,
			Metadata: map[string]any{"tier": newTier, "storage_limit_bytes": limitBytes}})
		logger.Info("tier changed",
			zap.String("tenant_id", tenantID),
			zap.String("tier", newTier),
			zap.String("by", sd.Email))

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<div class="alert alert-success">Tier changed to %s (%s limit).</div>`,
			template.HTMLEscapeString(newTier), formatBytes(limitBytes))
	}
}
