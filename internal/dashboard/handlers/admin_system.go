package handlers

import (
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
