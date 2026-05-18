package handlers

import (
	"html/template"
	"net/http"
	"sort"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// HealthChecker provides backend health state without importing internal/api.
type HealthChecker interface {
	GetBackendStates() map[string]*BackendState
}

// BackendState mirrors the fields the dashboard needs from health state.
type BackendState struct {
	Healthy   bool
	Score     float64
	Latency   time.Duration
	LastCheck time.Time
	LastError string
}

// BackendInfo is the per-backend view model for the template.
type BackendInfo struct {
	Name         string
	IsPrimary    bool
	IsBackup     bool
	Healthy      bool
	Score        float64
	LatencyMs    int64
	LastCheck    string
	LastError    string
	CircuitState string
	StorageClass string
}

// HandleAdminBackends renders the backend health dashboard page.
func HandleAdminBackends(tmpl *template.Template, eng *engine.CoreEngine, hc HealthChecker, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		data := sessionData(sd, "admin-backends")
		withCSRF(r.Context(), data)

		primary := eng.GetPrimary()
		backup := eng.GetBackup()
		circuitStates := eng.GetFailoverStatus()

		states := make(map[string]*BackendState)
		if hc != nil {
			for k, v := range hc.GetBackendStates() {
				states[k] = &BackendState{
					Healthy:   v.Healthy,
					Score:     v.Score,
					Latency:   v.Latency,
					LastCheck: v.LastCheck,
					LastError: v.LastError,
				}
			}
		}

		names := eng.GetDriverNames()
		backends := make([]BackendInfo, 0, len(names))
		for _, name := range names {
			bi := BackendInfo{
				Name:         name,
				IsPrimary:    name == primary,
				IsBackup:     name == backup,
				CircuitState: circuitStates[name],
				StorageClass: engine.BackendToStorageClass(name),
			}
			if bi.CircuitState == "" {
				bi.CircuitState = "unknown"
			}
			if st, ok := states[name]; ok {
				bi.Healthy = st.Healthy
				bi.Score = st.Score
				bi.LatencyMs = st.Latency.Milliseconds()
				bi.LastCheck = relativeTime(st.LastCheck)
				bi.LastError = st.LastError
			}
			backends = append(backends, bi)
		}

		sort.SliceStable(backends, func(i, j int) bool {
			if backends[i].IsPrimary != backends[j].IsPrimary {
				return backends[i].IsPrimary
			}
			return false
		})

		data["Backends"] = backends
		data["BackendCount"] = len(backends)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "admin", data); err != nil {
			logger.Error("render admin backends", zap.Error(err))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
}

// HandleSetPrimary handles POST /admin/backends/{name}/primary.
func HandleSetPrimary(eng *engine.CoreEngine, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		name := chi.URLParam(r, "name")
		eng.SetPrimary(name)
		logger.Info("primary backend changed via admin dashboard",
			zap.String("backend", name),
			zap.String("admin", sd.Email))

		http.Redirect(w, r, "/admin/backends", http.StatusSeeOther)
	}
}

// HandleForceHealthCheck handles POST /admin/backends/{name}/check.
func HandleForceHealthCheck(eng *engine.CoreEngine, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		name := chi.URLParam(r, "name")
		if err := eng.CheckDriver(r.Context(), name); err != nil {
			logger.Warn("forced health check failed",
				zap.String("backend", name),
				zap.Error(err))
		} else {
			logger.Info("forced health check passed",
				zap.String("backend", name),
				zap.String("admin", sd.Email))
		}

		http.Redirect(w, r, "/admin/backends", http.StatusSeeOther)
	}
}
