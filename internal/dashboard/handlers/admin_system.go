package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"go.uber.org/zap"
)

// HandleAdminSystem renders the admin system health page with runtime
// and database connection pool stats.
func HandleAdminSystem(tmpl *template.Template, db *sql.DB, logger *zap.Logger) http.HandlerFunc {
	startTime := time.Now()

	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "admin-system")
		withCSRF(r.Context(), data)

		// Go runtime stats.
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		data["Goroutines"] = runtime.NumGoroutine()
		data["MemAllocFmt"] = formatBytes(int64(mem.Alloc))
		data["MemSysFmt"] = formatBytes(int64(mem.Sys))
		data["NumGC"] = mem.NumGC
		data["GoVersion"] = runtime.Version()
		data["Uptime"] = time.Since(startTime).Truncate(time.Second).String()

		// DB connection pool stats.
		if db != nil {
			stats := db.Stats()
			data["DBOpen"] = stats.OpenConnections
			data["DBInUse"] = stats.InUse
			data["DBIdle"] = stats.Idle
			data["DBMaxOpen"] = stats.MaxOpenConnections
			data["DBWaitCount"] = stats.WaitCount
			data["DBStatus"] = "connected"
			data["Jobs"] = loadJobRuns(r.Context(), db, logger)
			data["Routing"] = loadRoutingTruth(r.Context(), db, logger)
			data["Capacity"] = loadBackendCapacity(r.Context(), db, logger)
			data["DataPlaneHosts"] = loadDataPlaneHosts(r.Context(), db, logger)
		} else {
			data["DBStatus"] = "not connected"
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render admin system", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// DataPlaneHostRow is one data-plane host on the System page (WP-EDGE-3):
// the registry row's health, and Up = what vaultaire_data_plane_host_up
// says (a node: its health up AND a heartbeat within 90 s). TenantScoped
// says a host is a customer's own (BYO) — never which customer: the
// System page names none.
type DataPlaneHostRow struct {
	Host, Kind, Health, Version, LastSeen string
	Up, TenantScoped                      bool
	Weight                                int
}

// loadDataPlaneHosts reads the data_plane_hosts registry for the card.
func loadDataPlaneHosts(ctx context.Context, db *sql.DB, logger *zap.Logger) []DataPlaneHostRow {
	rows, err := db.QueryContext(ctx, `
		SELECT host, kind, health, COALESCE(version, ''), weight, tenant_id IS NOT NULL, last_seen,
		       health = 'up' AND (kind <> 'node' OR (last_seen IS NOT NULL AND last_seen > NOW() - interval '90 seconds'))
		FROM data_plane_hosts ORDER BY kind, host`)
	if err != nil {
		logger.Warn("admin system: read data_plane_hosts", zap.Error(err))
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []DataPlaneHostRow
	for rows.Next() {
		var row DataPlaneHostRow
		var seen sql.NullTime
		if err := rows.Scan(&row.Host, &row.Kind, &row.Health, &row.Version, &row.Weight, &row.TenantScoped, &seen, &row.Up); err != nil {
			logger.Warn("admin system: scan data_plane_hosts", zap.Error(err))
			return out
		}
		row.LastSeen = "—"
		if seen.Valid {
			row.LastSeen = time.Since(seen.Time).Truncate(time.Second).String() + " ago"
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		logger.Warn("admin system: read data_plane_hosts", zap.Error(err))
	}
	return out
}

// RoutingTruthRow is the routing-truth line of the System page (WP-R7-5):
// the routing_truth job's last run (its one-line summary from
// job_runs.result) and the head rows whose backend no driver is registered
// under — read from the tables, so the line is right after a restart.
type RoutingTruthRow struct {
	LastRun     string // relative time of the last run, or "never"
	Outcome     string
	Summary     string // the run's note
	UnknownRows int64  // object_head_cache rows on a backend the job found no driver for ("" included)
	Unknown     string // "onedrive 2613, (NULL) 62"
}

// loadRoutingTruth reads the routing_truth job row. The names no driver
// answers to come from the run's result (the dashboard has no engine to ask).
func loadRoutingTruth(ctx context.Context, db *sql.DB, logger *zap.Logger) *RoutingTruthRow {
	var started sql.NullTime
	var outcome, note sql.NullString
	var result []byte
	err := db.QueryRowContext(ctx, `SELECT last_started_at, last_outcome, last_error, result FROM job_runs WHERE job = 'routing_truth'`).
		Scan(&started, &outcome, &note, &result)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logger.Warn("admin system: read routing_truth", zap.Error(err))
		}
		return nil
	}
	row := &RoutingTruthRow{LastRun: "never", Outcome: outcome.String, Summary: note.String}
	if started.Valid {
		row.LastRun = relativeTime(started.Time)
	}
	var res struct {
		Unknown map[string]int64 `json:"unknown"`
	}
	if len(result) > 0 && json.Unmarshal(result, &res) == nil {
		names := make([]string, 0, len(res.Unknown))
		for name := range res.Unknown {
			names = append(names, name)
		}
		sort.Strings(names)
		var parts []string
		for _, name := range names {
			row.UnknownRows += res.Unknown[name]
			label := name
			if label == "" {
				label = "(NULL)"
			}
			parts = append(parts, fmt.Sprintf("%s %d", label, res.Unknown[name]))
		}
		row.Unknown = strings.Join(parts, ", ")
	}
	return row
}

// CapacityRow is one backend's line of the System page's Capacity card
// (WP-CAP-1): the plan row (what we bought, when it renews) beside the last
// backend_capacity run's sums (what our tables say it holds) and, where the
// vendor's API answers, the vendor's own numbers.
type CapacityRow struct {
	Backend   string
	HasPlan   bool
	PlanFmt   string // capacity_bytes, human
	StoredFmt string // our stored total, human ("—" before the first run)
	VendorFmt string // the vendor's own meter ("—" without a reseller view)
	// EgressFreeFmt: free egress left this month (multiple × stored −
	// vendor month-to-date), "" while the vendor does not meter egress.
	EgressFreeFmt string
	RenewsInDays  int
	PaymentOnFile bool
	OverageFmt    string // "$5.12/TB-mo", "" when the row records none
}

// loadBackendCapacity joins backend_plans with the last backend_capacity
// run in job_runs.result. Nil when neither exists yet (the card hides).
func loadBackendCapacity(ctx context.Context, db *sql.DB, logger *zap.Logger) []CapacityRow {
	type bytesByKind struct {
		Total int64 `json:"total"`
	}
	var res struct {
		Backends map[string]*bytesByKind `json:"backends"`
		Vendor   map[string]struct {
			UsedBytes        int64 `json:"used_bytes"`
			EgressMonthBytes int64 `json:"egress_month_bytes"`
		} `json:"vendor"`
	}
	var raw []byte
	err := db.QueryRowContext(ctx, `SELECT result FROM job_runs WHERE job = 'backend_capacity'`).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		logger.Warn("admin system: read backend_capacity", zap.Error(err))
	}
	hasRun := len(raw) > 0 && json.Unmarshal(raw, &res) == nil && res.Backends != nil
	rows, err := db.QueryContext(ctx, `
		SELECT backend, capacity_bytes, egress_free_multiple, overage_usd_per_tb_month, term_end, payment_on_file
		  FROM backend_plans ORDER BY backend`)
	if err != nil {
		logger.Warn("admin system: read backend_plans", zap.Error(err))
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []CapacityRow
	seen := map[string]bool{}
	for rows.Next() {
		var row CapacityRow
		var capacity int64
		var mult, overage sql.NullFloat64
		var termEnd time.Time
		if err := rows.Scan(&row.Backend, &capacity, &mult, &overage, &termEnd, &row.PaymentOnFile); err != nil {
			logger.Warn("admin system: scan backend_plans", zap.Error(err))
			return nil
		}
		row.HasPlan = true
		row.PlanFmt = formatBytes(capacity)
		row.RenewsInDays = int(time.Until(termEnd).Hours() / 24)
		if overage.Valid {
			row.OverageFmt = fmt.Sprintf("$%.2f/TB-mo", overage.Float64)
		}
		row.StoredFmt, row.VendorFmt = "—", "—"
		if hasRun {
			var stored int64
			if b := res.Backends[row.Backend]; b != nil {
				stored = b.Total
			}
			row.StoredFmt = formatBytes(stored)
			if v, ok := res.Vendor[row.Backend]; ok {
				row.VendorFmt = formatBytes(v.UsedBytes)
				if mult.Valid {
					left := int64(mult.Float64*float64(stored)) - v.EgressMonthBytes
					if left < 0 {
						left = 0
					}
					row.EgressFreeFmt = formatBytes(left)
				}
			}
		}
		seen[row.Backend] = true
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		logger.Warn("admin system: read backend_plans", zap.Error(err))
		return nil
	}
	// Backends the run saw that have no plan row (BackendPlanUnknown's).
	if hasRun {
		names := make([]string, 0, len(res.Backends))
		for n := range res.Backends {
			if !seen[n] {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			out = append(out, CapacityRow{Backend: n, StoredFmt: formatBytes(res.Backends[n].Total), PlanFmt: "—", VendorFmt: "—"})
		}
	}
	return out
}

// JobRunRow is one background job as the System page shows it: the job_runs
// row the scheduler writes (internal/api/jobs.go, WP-R13-3). The same data,
// with the schedule, is GET /api/v1/admin/jobs.
type JobRunRow struct {
	Job         string
	Outcome     string // ok | error | interrupted | running
	LastStarted string
	LastSuccess string
	Rows        int64
	Detail      string // the error of a failed run, or the note of an ok one
}

// loadJobRuns reads job_runs, most recently started first. An error is
// logged and shows as an empty table: the page is a status page.
func loadJobRuns(ctx context.Context, db *sql.DB, logger *zap.Logger) []JobRunRow {
	rows, err := db.QueryContext(ctx, `
		SELECT job, last_outcome, last_started_at, last_success_at, rows_affected, last_error
		  FROM job_runs
		 ORDER BY last_started_at DESC NULLS LAST, job`)
	if err != nil {
		logger.Warn("admin system: read job_runs", zap.Error(err))
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []JobRunRow
	for rows.Next() {
		var r JobRunRow
		var started, success sql.NullTime
		if err := rows.Scan(&r.Job, &r.Outcome, &started, &success, &r.Rows, &r.Detail); err != nil {
			logger.Warn("admin system: scan job_runs", zap.Error(err))
			return out
		}
		r.LastStarted, r.LastSuccess = "never", "never"
		if started.Valid {
			r.LastStarted = relativeTime(started.Time)
		}
		if success.Valid {
			r.LastSuccess = relativeTime(success.Time)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		logger.Warn("admin system: iterate job_runs", zap.Error(err))
	}
	return out
}
