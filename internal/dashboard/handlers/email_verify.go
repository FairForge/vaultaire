package handlers

import (
	"net/http"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"go.uber.org/zap"
)

// HandleResendVerification handles POST /dashboard/settings/resend-verify.
// Generates a new verification token (the actual email sending is deferred
// to when an email service is configured — for now the token is logged).
func HandleResendVerification(authSvc *auth.AuthService, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		if authSvc.IsEmailVerified(r.Context(), sd.UserID) {
			middleware.SetFlash(w, "success", "Your email is already verified.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		token, err := authSvc.GenerateEmailVerifyToken(r.Context(), sd.UserID)
		if err != nil {
			logger.Error("generate verify token", zap.Error(err))
			middleware.SetFlash(w, "error", "Could not generate verification link.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		// TODO: Send email with verification link when email service is configured.
		// For now, log the verification URL.
		logger.Info("email verification token generated",
			zap.String("user", sd.Email),
			zap.String("verify_url", "/verify?token="+token))

		middleware.SetFlash(w, "success", "Verification email sent. Check your inbox.")
		http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
	}
}
