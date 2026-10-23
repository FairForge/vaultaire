package api

import (
	"encoding/json"
	"net/http"

	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// Admin feature-flag API (1.13 live-iteration kit), mounted under
// /api/v1/admin (requireJWT + requireAdmin):
//
//	GET    /flags                  — resolved list: defaults + global rows + overrides
//	PUT    /flags/{key}            — body {"enabled": bool, "tenant_id"?: string}
//	DELETE /flags/{key}?tenant_id= — remove a row (revert to global/default)
//
// updated_by comes from the JWT — never the request body. Only keys
// registered in code are settable: a typo'd kill-switch must fail loudly,
// not no-op. A translog_* row write is a log entry first (translog_flags.go):
// 409 flag_not_exemptible (a per-tenant OFF under a global ON), 400
// flag_global_only, 404 tenant_not_found, 503 log_frozen /
// flag_change_not_logged — nothing written.

func (s *Server) handleAdminFlagsList(w http.ResponseWriter, r *http.Request) {
	_ = r
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"flags": s.flags.Resolved(),
	}); err != nil {
		s.logger.Error("encode admin flags list", zap.Error(err))
	}
}

func (s *Server) handleAdminFlagSet(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if !s.flags.Registered(key) {
		http.Error(w, "unknown flag: "+key, http.StatusBadRequest)
		return
	}

	var req struct {
		Enabled  *bool  `json:"enabled"`
		TenantID string `json:"tenant_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		http.Error(w, `invalid body: expected {"enabled": bool, "tenant_id"?: string}`, http.StatusBadRequest)
		return
	}

	updatedBy, _ := r.Context().Value(emailKey).(string)
	if err := s.flags.Set(r.Context(), key, req.TenantID, *req.Enabled, updatedBy); err != nil {
		if flagWriteError(w, err) {
			s.logger.Warn("admin flag set refused", zap.String("flag", key), zap.Error(err))
			return
		}
		s.logger.Error("admin flag set failed",
			zap.String("flag", key), zap.Error(err))
		http.Error(w, "failed to set flag", http.StatusInternalServerError)
		return
	}

	s.writeResolvedFlag(w, key, req.TenantID)
}

func (s *Server) handleAdminFlagUnset(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if !s.flags.Registered(key) {
		http.Error(w, "unknown flag: "+key, http.StatusBadRequest)
		return
	}

	tenantID := r.URL.Query().Get("tenant_id")
	if err := s.flags.Unset(r.Context(), key, tenantID); err != nil {
		if flagWriteError(w, err) {
			s.logger.Warn("admin flag unset refused", zap.String("flag", key), zap.Error(err))
			return
		}
		s.logger.Error("admin flag unset failed",
			zap.String("flag", key), zap.Error(err))
		http.Error(w, "failed to unset flag", http.StatusInternalServerError)
		return
	}

	s.writeResolvedFlag(w, key, tenantID)
}

// writeResolvedFlag responds with the flag's post-change state — its default,
// global row and effective global state, and the ONE tenant row written
// (none for the global row) — so the caller sees what is now live. Never the
// other tenants' rows (Stage 1a Prompt 34a, RP43-6): those are the flag_list
// read of GET /flags, and a write that answered them would read them past
// the log; answering what it wrote keeps a flag write a mutation, so a
// kill-switch can still be flipped while the log is frozen.
func (s *Server) writeResolvedFlag(w http.ResponseWriter, key, tenantID string) {
	var out *flags.Flag
	for _, f := range s.flags.Resolved() {
		if f.Key == key {
			var mine []flags.Override
			for _, o := range f.Overrides {
				if tenantID != "" && o.TenantID == tenantID {
					mine = append(mine, o)
				}
			}
			f.Overrides = mine
			out = &f
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"flag": out}); err != nil {
		s.logger.Error("encode admin flag response", zap.Error(err))
	}
}
