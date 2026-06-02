package api

import (
	"encoding/json"
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
	if email == "" {
		var body struct {
			Email string `json:"email"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body) == nil {
			email = strings.TrimSpace(body.Email)
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
		s.logger.Warn("waitlist signup with no database", zap.String("email", email))
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	// ON CONFLICT DO NOTHING: re-signing up with the same email is a no-op success.
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO waitlist_signups (email, source, ip_address, user_agent)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (email) DO NOTHING`,
		email, "landing", ip, r.UserAgent()); err != nil {
		s.logger.Error("waitlist insert", zap.String("email", email), zap.Error(err))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save"})
		return
	}

	s.logger.Info("waitlist signup", zap.String("email", email))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
