package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/FairForge/vaultaire/internal/audit"
)

// The second factor is ONE flow with two entry points — the password form
// (router.handleLogin) and the OAuth callbacks (HandleOAuthCallback). Review
// R5-06 found the OAuth side created the session directly, so a TOTP-enabled
// account signed in with Google/GitHub was never asked for the code. Both
// entry points now call BeginMFAChallenge; the cookie name, TTL and redirect
// live here once so they cannot drift again (the R4-22 lesson).

// MFAPendingCookie carries the pending-2FA token between the first factor
// and /login/verify-2fa.
const MFAPendingCookie = "mfa_pending"

// MFAPendingTTLSeconds is how long a pending challenge (and its cookie) lives.
const MFAPendingTTLSeconds = 300

// MFAChallenge is the identity the second step turns into a session once the
// code is right.
type MFAChallenge struct {
	UserID   string
	TenantID string
	Email    string
	Role     string
}

// MFAChallenger stores a pending challenge and returns its single-use token
// (dashboard.MFAPendingStore behind an adapter in router.go).
type MFAChallenger interface {
	Begin(c MFAChallenge) (token string, err error)
}

// MFAStatus is the slice of auth.AuthService the gate needs.
type MFAStatus interface {
	IsMFAEnabled(ctx context.Context, userID string) (bool, error)
}

// SetMFAPendingCookie writes the pending-2FA cookie.
func SetMFAPendingCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     MFAPendingCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   true,
		MaxAge:   MFAPendingTTLSeconds,
	})
}

// ClearMFAPendingCookie removes it.
func ClearMFAPendingCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     MFAPendingCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   true,
		MaxAge:   -1,
	})
}

// BeginMFAChallenge starts the second factor for user when MFA is enabled:
// it stores the challenge, sets the cookie and redirects to
// /login/verify-2fa, returning true. It returns false (nothing written) when
// MFA is off for the user, or when the challenger is nil (a deployment with
// no pending store — the caller creates the session as before).
//
// The first factor is recorded as auth.login_succeeded with mfa=pending so
// the audit trail shows a password (or OAuth identity) that was accepted but
// not yet a session.
func BeginMFAChallenge(w http.ResponseWriter, r *http.Request, status MFAStatus, ch MFAChallenger, c MFAChallenge) (bool, error) {
	if status == nil || ch == nil {
		return false, nil
	}
	// The MFA state is read from the database (auth.IsMFAEnabled); when it
	// cannot be read the sign-in fails closed — no session without knowing
	// whether a second factor is owed.
	enabled, err := status.IsMFAEnabled(r.Context(), c.UserID)
	if err != nil {
		return false, fmt.Errorf("read mfa state: %w", err)
	}
	if !enabled {
		return false, nil
	}
	token, err := ch.Begin(c)
	if err != nil {
		return false, err
	}
	SetMFAPendingCookie(w, token)
	http.Redirect(w, r, "/login/verify-2fa", http.StatusSeeOther)
	return true, nil
}

// LoginEvent is one dashboard sign-in outcome for the audit trail
// (pre-launch checklist item 2: `user_activities` had 0 rows in 30 days, so
// an account takeover was invisible). Actions:
//
//	auth.login_failed     bad password / unknown user / locked account
//	auth.login_succeeded  first factor accepted (Metadata.mfa = "pending" | "none")
//	auth.mfa_failed       wrong or replayed second-factor code
//	auth.mfa_succeeded    session created after the second factor
//	auth.login_locked     the account lockout engaged
//
// The client IP and user agent come from the request context
// (audit.WithRequest, stashed by the server's first middleware from
// internal/clientip — R1-01). Email is kept in Metadata because a failed
// login for an unknown address has no user id.
type LoginEvent struct {
	Action   string
	UserID   string
	TenantID string
	Email    string
	Via      string // "password" | "oauth:google" | "oauth:github" | "totp" | "backup_code"
	Reason   string // for failures: bad_password | unknown_user | locked | bad_code | replayed_code
	MFA      string // for auth.login_succeeded: "pending" | "none"
}

// RecordLoginEvent writes one audit_logs row for a sign-in outcome. Nil db is
// a no-op (tests, dev without PostgreSQL); a failed insert never fails the
// login.
func RecordLoginEvent(ctx context.Context, db *sql.DB, ev LoginEvent) {
	if db == nil || ev.Action == "" {
		return
	}
	meta := map[string]any{"email": ev.Email}
	if ev.Via != "" {
		meta["via"] = ev.Via
	}
	if ev.Reason != "" {
		meta["reason"] = ev.Reason
	}
	if ev.MFA != "" {
		meta["mfa"] = ev.MFA
	}
	e := audit.Entry{
		Actor:     ev.UserID,
		UserID:    ev.UserID,
		TenantID:  ev.TenantID,
		EventType: "auth",
		Action:    ev.Action,
		Metadata:  meta,
	}
	switch ev.Action {
	case "auth.login_failed", "auth.mfa_failed":
		e.Severity = "warning"
	case "auth.login_locked":
		e.Severity = "error"
	}
	audit.Record(ctx, db, e)
}

// ResolveRole reads users.role for the session snapshot (R5-22: the role is
// fixed at login; promote/demote needs a re-login). Defaults to "user".
func ResolveRole(ctx context.Context, db *sql.DB, userID string) string {
	role := "user"
	if db != nil {
		_ = db.QueryRowContext(ctx, `SELECT role FROM users WHERE id = $1`, userID).Scan(&role)
	}
	return role
}
