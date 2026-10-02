package handlers

import (
	"context"
	"database/sql"
	"html/template"
	"net/http"
	"runtime"
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
