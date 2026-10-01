package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/clientip"
	"github.com/FairForge/vaultaire/internal/sitestats"
)

// Cookieless statistics for the public site (internal/sitestats): every
// rendered marketing page is counted by siteStatsMiddleware, and the page's
// beacon posts named clicks to /api/ping. Both feed one in-memory collector
// that flushes daily aggregates to site_stats_daily. Nothing here sets a
// cookie, stores an identifier or calls a third party; the privacy policy's
// "site statistics" paragraph describes exactly this.

// pingRL bounds the public beacon per client IP so it cannot be scripted
// into a write load (a page fires at most a couple of dozen pings).
var pingRL = newWaitlistLimiter(240, time.Hour)

// siteStatsMiddleware counts a public page view once the handler has
// answered: GET, 200, text/html, on a page sitestats.PagePath knows, from a
// user agent that is not a bot. Authenticated S3 ListBuckets on "/" is the
// API, not a visit.
func (s *Server) siteStatsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.siteStats == nil || r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		name, ok := sitestats.PagePath(r.URL.Path)
		if !ok || (name == "/" && isS3RootRequest(r)) {
			next.ServeHTTP(w, r)
			return
		}
		cw := &countingResponseWriter{ResponseWriter: w}
		next.ServeHTTP(cw, r)
		if cw.statusCode != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
			return
		}
		ua := r.UserAgent()
		if sitestats.IsBot(ua) {
			return
		}
		q := r.URL.Query()
		s.siteStats.Record(sitestats.Hit{
			Kind: sitestats.KindView, Name: name, Referrer: r.Referer(),
			UTMSource: q.Get("utm_source"), UTMMedium: q.Get("utm_medium"), UTMCampaign: q.Get("utm_campaign"),
			Country: clientip.Country(r), Device: sitestats.DeviceClass(ua),
		})
		s.siteStats.RecordVisitor(clientip.FromRequest(r), ua)
	})
}

// handlePing is the page's beacon: POST /api/ping with a small JSON body
// naming one event from sitestats' closed list. Answers 204 whether or not
// the event was counted (unknown names and bots are ignored, not rejected,
// so the page never sees an error), 400 for a body that is not JSON, 429
// past the per-IP limit. Public, no auth, no cookie.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !pingRL.allow(extractClientIP(r), time.Now().Unix()) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many requests"})
		return
	}
	var body struct {
		Event       string `json:"event"`
		Referrer    string `json:"referrer"`
		UTMSource   string `json:"utm_source"`
		UTMMedium   string `json:"utm_medium"`
		UTMCampaign string `json:"utm_campaign"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	ua := r.UserAgent()
	if s.siteStats != nil && sitestats.EventAllowed(body.Event) && !sitestats.IsBot(ua) {
		s.siteStats.Record(sitestats.Hit{
			Kind: sitestats.KindEvent, Name: body.Event, Referrer: body.Referrer,
			UTMSource: body.UTMSource, UTMMedium: body.UTMMedium, UTMCampaign: body.UTMCampaign,
			Country: clientip.Country(r), Device: sitestats.DeviceClass(ua),
		})
	}
	w.WriteHeader(http.StatusNoContent)
}
