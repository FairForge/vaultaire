package handlers

import (
	"net/http"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/FairForge/vaultaire/internal/email"
	"go.uber.org/zap"
)

// HandleResendVerification handles POST /dashboard/settings/resend-verify.
// Generates a new verification token and sends the verification email.
func HandleResendVerification(authSvc *auth.AuthService, logger *zap.Logger, sender email.Sender, baseURL string) http.HandlerFunc {
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

		htmlBody, textBody, err := email.RenderVerification(baseURL, token, sd.Email)
		if err != nil {
			logger.Error("render verification email", zap.Error(err))
			middleware.SetFlash(w, "error", "Could not send verification email.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		if err := sender.Send(r.Context(), sd.Email, "Verify your email — stored.ge", htmlBody, textBody); err != nil {
			logger.Error("send verification email", zap.String("to", sd.Email), zap.Error(err))
		}

		middleware.SetFlash(w, "success", "Verification email sent. Check your inbox.")
		http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
	}
}
