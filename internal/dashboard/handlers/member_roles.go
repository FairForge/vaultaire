package handlers

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"go.uber.org/zap"
)

// The dashboard's role check (WP-TEAM-1, Stage 1a Prompt 52): the same
// rule as the management API's — the acting member's role on the session's
// active tenant is read from tenant_members on every request
// (auth.MemberRole) and every write names its action (auth.Can).

// ResolveMember puts the session user's role on its active tenant on the
// session (SessionData.MemberRole). A session whose user is no member of
// its tenant any more (removed meanwhile) is answered 403. Without a
// database (tests) the user owns its tenant.
func ResolveMember(db *sql.DB, logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sd := dashauth.GetSession(r.Context())
			if sd == nil {
				next.ServeHTTP(w, r)
				return
			}
			role := auth.RoleOwner
			if db != nil && sd.TenantID != "" {
				var err error
				role, err = auth.MemberRole(r.Context(), db, sd.TenantID, sd.UserID)
				switch {
				case errors.Is(err, auth.ErrNotMember):
					http.Error(w, "You are no longer a member of this account. Sign in again.", http.StatusForbidden)
					return
				case err != nil:
					logger.Error("dashboard: resolve member role", zap.String("tenant_id", sd.TenantID), zap.Error(err))
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
					return
				}
			}
			cp := *sd
			cp.MemberRole = string(role)
			next.ServeHTTP(w, r.WithContext(dashauth.WithSession(r.Context(), &cp)))
		})
	}
}

// RequireAction refuses (403) a member whose role may not do action.
func RequireAction(action auth.Action) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sd := dashauth.GetSession(r.Context())
			role := auth.RoleOwner
			if sd != nil && sd.MemberRole != "" {
				role = auth.Role(sd.MemberRole)
			}
			if !auth.Can(role, action) {
				http.Error(w, "Your role on this account ("+string(role)+") may not do this.", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
