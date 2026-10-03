package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

// DeletionScheduledMessage is the flash the settings page shows once a
// deletion is scheduled (WP-R10-3): what the runner does on the date.
func DeletionScheduledMessage(at time.Time) string {
	return fmt.Sprintf("Account scheduled for deletion on %s. You can cancel any time before then; on that date your subscription is cancelled and your objects, keys and account records are erased. Backups age out within 7 days.",
		at.Format("January 2, 2006"))
}

// HandleRequestDeletion schedules the account's erasure through the one
// state machine (internal/account — the dashboard used to run its own SQL,
// R12-22). The request must be confirmed by the strongest factor the user
// has: the password when they have one; a TOTP code when the account is
// OAuth-only with 2FA on; otherwise the account e-mail re-typed
// (bcrypt against an empty hash always failed, so OAuth-only users could
// never delete — R12-22).
func HandleRequestDeletion(db *sql.DB, authSvc *auth.AuthService, accounts *account.Service, mfa *auth.MFAService, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		if db == nil {
			middleware.SetFlash(w, "error", "Database unavailable.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}
		if accounts == nil {
			accounts = account.NewService(db, logger)
		}

		var passwordHash, email string
		err := db.QueryRowContext(r.Context(),
			`SELECT password_hash, email FROM users WHERE id = $1`, sd.UserID).Scan(&passwordHash, &email)
		if err != nil {
			logger.Error("fetch user for deletion", zap.Error(err))
			middleware.SetFlash(w, "error", "Failed to verify your identity.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		if msg := confirmDeletionIdentity(r, authSvc, mfa, sd.UserID, passwordHash, email); msg != "" {
			middleware.SetFlash(w, "error", msg)
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		reason := account.CapReason(r.FormValue("reason"))
		scheduledAt, err := accounts.Schedule(r.Context(), sd.UserID, sd.TenantID, reason)
		if err != nil {
			logger.Error("schedule account deletion", zap.Error(err))
			middleware.SetFlash(w, "error", "Failed to schedule account deletion.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		audit.Record(r.Context(), db, audit.Entry{UserID: sd.UserID, TenantID: sd.TenantID, Action: "account.deletion_scheduled",
			Resource: "user:" + sd.UserID, Severity: "warning", Metadata: map[string]any{"scheduled_at": scheduledAt, "reason": reason, "via": "dashboard"}})
		middleware.SetFlash(w, "success", DeletionScheduledMessage(scheduledAt))
		http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
	}
}

// confirmDeletionIdentity checks the confirmation factor and returns the
// flash message to show when it fails ("" = confirmed).
func confirmDeletionIdentity(r *http.Request, authSvc *auth.AuthService, mfa *auth.MFAService, userID, passwordHash, email string) string {
	if passwordHash != "" {
		password := r.FormValue("password")
		if password == "" {
			return "Password is required to delete your account."
		}
		if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)); err != nil {
			return "Incorrect password."
		}
		return ""
	}
	// OAuth-only account: no password to check.
	mfaOn := false
	if authSvc != nil {
		mfaOn, _ = authSvc.IsMFAEnabled(r.Context(), userID)
	}
	if mfaOn && mfa != nil {
		code := strings.TrimSpace(r.FormValue("totp_code"))
		if code == "" {
			return "Enter the 6-digit code from your authenticator app to delete your account."
		}
		secret, err := authSvc.GetMFASecret(r.Context(), userID)
		if err != nil || !mfa.ValidateCode(secret, code) || !authSvc.ConsumeTOTPCode(userID, code) {
			return "Incorrect authenticator code."
		}
		return ""
	}
	typed := strings.ToLower(strings.TrimSpace(r.FormValue("confirm_email")))
	if typed == "" {
		return "Type your account e-mail address to confirm the deletion."
	}
	if typed != strings.ToLower(strings.TrimSpace(email)) {
		return "The e-mail address you typed does not match your account."
	}
	return ""
}

func HandleCancelDeletion(db *sql.DB, accounts *account.Service, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		if db == nil {
			middleware.SetFlash(w, "error", "Database unavailable.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}
		if accounts == nil {
			accounts = account.NewService(db, logger)
		}

		if err := accounts.Cancel(r.Context(), sd.UserID); err != nil {
			if errors.Is(err, account.ErrNoPendingDeletion) {
				middleware.SetFlash(w, "error", "No account deletion is scheduled.")
				http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
				return
			}
			logger.Error("cancel account deletion", zap.Error(err))
			middleware.SetFlash(w, "error", "Failed to cancel deletion.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		audit.Record(r.Context(), db, audit.Entry{UserID: sd.UserID, TenantID: sd.TenantID, Action: "account.deletion_cancelled",
			Resource: "user:" + sd.UserID, Metadata: map[string]any{"via": "dashboard"}})
		middleware.SetFlash(w, "success", "Account deletion has been cancelled.")
		http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
	}
}
