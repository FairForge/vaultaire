package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/FairForge/vaultaire/internal/api/landing"
	"github.com/FairForge/vaultaire/internal/clientip"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// waitlistRL throttles the public, unauthenticated waitlist endpoint per client
// IP (10 signups/hour) so it can't be scripted to flood the table. Cloudflare
// provides edge protection on top of this.
var waitlistRL = newWaitlistLimiter(10, time.Hour)

// handleWaitlistSignup captures a pre-launch waitlist email from the landing page.
// Public and unauthenticated. Accepts form-encoded (email=...) or JSON ({"email"}).
func (s *Server) handleWaitlistSignup(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.FormValue("email"))
	// The house the visitor built on the landing page, if any (TB per floor
	// + the share-link room): stored beside the email so launch-day demand
	// is known per tier. Optional; junk is clamped/dropped, never rejected.
	intent := landing.ParseHouseIntent(r.FormValue("std_tb"), r.FormValue("vault_tb"), r.FormValue("room"))
	// Where the visitor came from (checklist item 7): page.js sends the
	// document.referrer host and the utm_* labels of the landing URL; the
	// Referer header is the fallback for a form posted without JS.
	referrer := r.FormValue("referrer")
	if referrer == "" {
		referrer = r.Referer()
	}
	attr := landing.ParseAttribution(referrer, r.FormValue("utm_source"), r.FormValue("utm_medium"), r.FormValue("utm_campaign"))
	if email == "" {
		var body struct {
			Email       string          `json:"email"`
			StdTB       json.RawMessage `json:"std_tb"`
			VaultTB     json.RawMessage `json:"vault_tb"`
			Room        string          `json:"room"`
			Referrer    string          `json:"referrer"`
			UTMSource   string          `json:"utm_source"`
			UTMMedium   string          `json:"utm_medium"`
			UTMCampaign string          `json:"utm_campaign"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body) == nil {
			email = strings.TrimSpace(body.Email)
			intent = landing.ParseHouseIntent(rawNumber(body.StdTB), rawNumber(body.VaultTB), body.Room)
			if body.Referrer == "" {
				body.Referrer = r.Referer()
			}
			attr = landing.ParseAttribution(body.Referrer, body.UTMSource, body.UTMMedium, body.UTMCampaign)
		}
	}

	addr, err := mail.ParseAddress(email)
	if err != nil || len(email) > 320 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid email"})
		return
	}
	email = strings.ToLower(addr.Address)

	ip := extractClientIP(r)
	if !waitlistRL.allow(ip, time.Now().Unix()) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many requests"})
		return
	}

	// Degrade gracefully without a DB (dev/local): don't fail the visitor's submit.
	if s.db == nil {
		s.logger.Warn("waitlist signup with no database", zap.String("email_hash", emailHash(email)))
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	// Re-signing up with the same email is a no-op success, except that a
	// newly built house replaces the old one (the latest plan is the truth).
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO waitlist_signups (email, source, ip_address, user_agent, plan_std_tb, plan_vault_tb, room,
		                              referrer, utm_source, utm_medium, utm_campaign, country)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (email) DO UPDATE
		   SET plan_std_tb = EXCLUDED.plan_std_tb, plan_vault_tb = EXCLUDED.plan_vault_tb, room = EXCLUDED.room
		 WHERE EXCLUDED.plan_std_tb + EXCLUDED.plan_vault_tb > 0`,
		email, attr.Source("landing"), ip, r.UserAgent(), intent.StdTB, intent.VaultTB, intent.Room,
		attr.Referrer, attr.UTMSource, attr.UTMMedium, attr.UTMCampaign, clientip.Country(r)); err != nil {
		s.logger.Error("waitlist insert", zap.String("email_hash", emailHash(email)), zap.Error(err))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save"})
		return
	}

	s.logger.Info("waitlist signup", zap.String("email_hash", emailHash(email)), zap.String("source", attr.Source("landing")),
		zap.Int("std_tb", intent.StdTB), zap.Int("vault_tb", intent.VaultTB))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// rawNumber renders a JSON number or string field as the text ParseHouseIntent
// reads ("6", "6.0", "\"6\"" all become 6; anything else becomes empty).
func rawNumber(raw json.RawMessage) string {
	return strings.Trim(strings.TrimSpace(string(raw)), `"`)
}

// waitlistLimiter is a small per-IP sliding-window rate limiter.
type waitlistLimiter struct {
	mu         sync.Mutex
	hits       map[string][]int64
	limit      int
	windowSecs int64
}

func newWaitlistLimiter(limit int, window time.Duration) *waitlistLimiter {
	return &waitlistLimiter{
		hits:       make(map[string][]int64),
		limit:      limit,
		windowSecs: int64(window.Seconds()),
	}
}

// allow reports whether an IP may submit at time `now` (unix seconds), recording
// the hit when allowed. `now` is injected for testability.
func (l *waitlistLimiter) allow(ip string, now int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Bound memory under abuse: reset if the map grows large (mirrors the
	// management rate limiter's eviction).
	if len(l.hits) > 10000 {
		l.hits = make(map[string][]int64)
	}

	cutoff := now - l.windowSecs
	kept := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t > cutoff {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.hits[ip] = kept
		return false
	}
	l.hits[ip] = append(kept, now)
	return true
}

// emailHash is what the log carries instead of the address (Review R13-22 /
// R14-16): sha256 truncated to 8 hex — enough to correlate one visitor's
// lines, not enough to be a second copy of the PII the retention job expires.
func emailHash(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(sum[:4])
}
