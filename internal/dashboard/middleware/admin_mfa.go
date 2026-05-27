package middleware

import (
	"context"
	"net/http"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
)

// MFAChecker is the subset of auth.AuthService needed by RequireAdminMFA.
type MFAChecker interface {
	IsMFAEnabled(ctx context.Context, userID string) (bool, error)
}

// RequireAdminMFA redirects admin users to the MFA setup page if they have
// not enabled two-factor authentication. This is a SOC 2 CC6.1 control.
// When authSvc is nil (tests without an auth service), the check is skipped.
func RequireAdminMFA(authSvc *auth.AuthService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if authSvc == nil {
				next.ServeHTTP(w, r)
				return
			}

			sd := dashauth.GetSession(r.Context())
			if sd == nil {
				next.ServeHTTP(w, r)
				return
			}

			enabled, _ := authSvc.IsMFAEnabled(r.Context(), sd.UserID)
			if !enabled {
				SetFlash(w, "error", "Administrators must enable two-factor authentication.")
				http.Redirect(w, r, "/dashboard/settings/mfa", http.StatusSeeOther)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
