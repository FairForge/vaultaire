package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// SessionRow is the display-oriented view of one active session, tailored
// for the settings page "Active Sessions" table.
type SessionRow struct {
	ID         string // Opaque token, used by the revoke form.
	IPAddress  string
	Device     string // Short label derived from the User-Agent.
	FullUA     string // Full User-Agent (shown in title tooltip).
	CreatedAgo string // e.g., "3 days ago".
	ActiveAgo  string // e.g., "5 minutes ago".
	IsCurrent  bool   // True if this row matches the request's cookie.
}

// buildSessionRows converts raw SessionInfo records to the display rows
// shown on the settings page.
func buildSessionRows(list []dashauth.SessionInfo, currentToken string) []SessionRow {
	out := make([]SessionRow, 0, len(list))
	now := time.Now()
	for _, s := range list {
		out = append(out, SessionRow{
			ID:         s.ID,
			IPAddress:  firstNonEmpty(s.IPAddress, "unknown"),
			Device:     describeDevice(s.UserAgent),
			FullUA:     s.UserAgent,
			CreatedAgo: relativeAgo(now, s.CreatedAt),
			ActiveAgo:  relativeAgo(now, s.LastActiveAt),
			IsCurrent:  s.ID == currentToken,
		})
	}
	return out
}

// HandleRevokeSession handles POST /dashboard/settings/sessions/{id}/revoke.
// The target session must belong to the current user — attempting to revoke
// another user's session silently no-ops (no information leak).
func HandleRevokeSession(sessions dashauth.SessionStore, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if sessions == nil {
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		target := chi.URLParam(r, "id")
		if target == "" {
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		// Ownership check: scan the user's sessions and confirm the target
		// belongs to them before deleting it. This is cheap (per-user small
		// list) and avoids a race where a user submits another user's token.
		list, err := sessions.ListByUserID(r.Context(), sd.UserID)
		if err != nil {
			logger.Error("list sessions for revoke", zap.Error(err))
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}
		owned := false
		for _, s := range list {
			if s.ID == target {
				owned = true
				break
			}
		}
		if !owned {
			middleware.SetFlash(w, "error", "Session not found.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		// Don't let a user revoke the session that issued the request.
		if c, cErr := r.Cookie(dashauth.SessionCookieName); cErr == nil && c.Value == target {
			middleware.SetFlash(w, "error", "Use Sign Out to end this session.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		if err := sessions.Delete(r.Context(), target); err != nil {
			logger.Error("delete session", zap.String("token", target), zap.Error(err))
			middleware.SetFlash(w, "error", "Failed to revoke session.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		middleware.SetFlash(w, "success", "Device signed out.")
		http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
	}
}

// HandleRevokeAllOtherSessions handles POST /dashboard/settings/sessions/revoke-all.
// It signs out every other device the current user has without logging them
// out of the device that issued the request.
func HandleRevokeAllOtherSessions(sessions dashauth.SessionStore, logger *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sd := dashauth.GetSession(r.Context())
		if sd == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if sessions == nil {
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		currentToken := ""
		if c, err := r.Cookie(dashauth.SessionCookieName); err == nil {
			currentToken = c.Value
		}

		if err := sessions.DeleteByUserIDExcept(r.Context(), sd.UserID, currentToken); err != nil {
			logger.Error("revoke all other sessions", zap.Error(err))
			middleware.SetFlash(w, "error", "Failed to sign out other devices.")
			http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
			return
		}

		middleware.SetFlash(w, "success", "Signed out of all other devices.")
		http.Redirect(w, r, "/dashboard/settings", http.StatusSeeOther)
	}
}

// describeDevice returns a short label like "Chrome on macOS" for a
// User-Agent string. It's best-effort — we avoid pulling in a full UA
// parser library for what is a cosmetic display string.
func describeDevice(ua string) string {
	if ua == "" {
		return "Unknown device"
	}
	browser := firstMatch(ua, []string{"Firefox", "Edg", "OPR", "Chrome", "Safari"})
	switch browser {
	case "Edg":
		browser = "Edge"
	case "OPR":
		browser = "Opera"
	}
	os := firstMatch(ua, []string{"Windows", "Android", "iPhone", "iPad", "Macintosh", "Mac OS", "Linux"})
	switch os {
	case "Macintosh", "Mac OS":
		os = "macOS"
	case "iPhone":
		os = "iOS"
	}
	switch {
	case browser != "" && os != "":
		return browser + " on " + os
	case browser != "":
		return browser
	case os != "":
		return os
	default:
		return "Unknown device"
	}
}

// firstMatch returns the first needle from the list that appears in s
// (case-insensitive), or "" if none match. Used by describeDevice.
func firstMatch(s string, needles []string) string {
	lower := strings.ToLower(s)
	for _, n := range needles {
		if strings.Contains(lower, strings.ToLower(n)) {
			return n
		}
	}
	return ""
}

// relativeAgo returns a short, human-readable duration like "2 minutes
// ago" or "3 days ago". Used for the CreatedAgo / ActiveAgo columns.
func relativeAgo(now, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return pluralize(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return pluralize(int(d/time.Hour), "hour")
	case d < 30*24*time.Hour:
		return pluralize(int(d/(24*time.Hour)), "day")
	default:
		return pluralize(int(d/(30*24*time.Hour)), "month")
	}
}

func pluralize(n int, unit string) string {
	if n <= 0 {
		n = 1
	}
	if n == 1 {
		return "1 " + unit + " ago"
	}
	return strconv.Itoa(n) + " " + unit + "s ago"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
