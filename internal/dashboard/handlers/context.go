package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
)

// sessionData builds the base template data map from the session.
// Every dashboard handler calls this to populate Email, Role, UserID,
// TenantID, and Page for the base layout template.
func sessionData(sd *dashauth.SessionData, page string) map[string]any {
	return map[string]any{
		"Email":    sd.Email,
		"Role":     sd.Role,
		"UserID":   sd.UserID,
		"TenantID": sd.TenantID,
		"Page":     page,
	}
}

// withCSRF adds the CSRF token from the request context to the template data map.
func withCSRF(ctx context.Context, data map[string]any) {
	data["CSRFToken"] = middleware.Token(ctx)
}

// withFlash injects any flash messages from the context into the template data map.
func withFlash(ctx context.Context, data map[string]any) {
	for k, v := range middleware.GetFlashMap(ctx) {
		switch k {
		case "success":
			data["FlashSuccess"] = v
		case "error":
			data["FlashError"] = v
		}
	}
}

// populateEmailVerified checks the DB for the email_verified flag and sets
// ShowVerifyBanner in template data when the user hasn't verified yet.
func populateEmailVerified(ctx context.Context, db *sql.DB, userID string, data map[string]any) {
	if db == nil {
		return
	}
	var verified bool
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(email_verified, FALSE) FROM users WHERE id = $1`, userID).Scan(&verified)
	if err == nil && !verified {
		data["ShowVerifyBanner"] = true
	}
}

// formatBytes returns a human-readable size string (e.g. "1.5 GB").
func formatBytes(b int64) string {
	if b == 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	i := int(math.Log(float64(b)) / math.Log(1024))
	if i >= len(units) {
		i = len(units) - 1
	}
	val := float64(b) / math.Pow(1024, float64(i))
	if val == math.Trunc(val) {
		return fmt.Sprintf("%.0f %s", val, units[i])
	}
	return fmt.Sprintf("%.1f %s", val, units[i])
}

// relativeTime formats a timestamp as a human-readable "time ago" string.
func relativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		if days < 30 {
			return fmt.Sprintf("%d days ago", days)
		}
		return t.Format("Jan 2, 2006")
	}
}

// absInt64 returns the absolute value of an int64.
func absInt64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
