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
// not no-op.

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
		s.logger.Error("admin flag set failed",
			zap.String("flag", key), zap.Error(err))
		http.Error(w, "failed to set flag", http.StatusInternalServerError)
		return
	}

	s.writeResolvedFlag(w, key)
}

func (s *Server) handleAdminFlagUnset(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if !s.flags.Registered(key) {
		http.Error(w, "unknown flag: "+key, http.StatusBadRequest)
		return
	}

	tenantID := r.URL.Query().Get("tenant_id")
	if err := s.flags.Unset(r.Context(), key, tenantID); err != nil {
		s.logger.Error("admin flag unset failed",
			zap.String("flag", key), zap.Error(err))
		http.Error(w, "failed to unset flag", http.StatusInternalServerError)
		return
	}

	s.writeResolvedFlag(w, key)
}

// writeResolvedFlag responds with the flag's post-change resolved state so
// the caller sees exactly what is now live.
func (s *Server) writeResolvedFlag(w http.ResponseWriter, key string) {
	var out *flags.Flag
	for _, f := range s.flags.Resolved() {
		if f.Key == key {
			out = &f
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"flag": out}); err != nil {
		s.logger.Error("encode admin flag response", zap.Error(err))
	}
}
