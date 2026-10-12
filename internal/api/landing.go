package api

import (
	"bytes"
	_ "embed"
	"html/template"
	"net/http"
	"net/url"

	"github.com/FairForge/vaultaire/internal/hosts"
)

//go:embed landing.html
var landingTemplateSrc string

// The landing page has two variants driven by the signups feature flag: closed
// renders a waitlist capture, open renders working /register CTAs. Both are
// rendered once per server, at NewServer, with the server's hosts (the page
// prints the S3 endpoint and the canonical origin — internal/hosts,
// WP-DOMAIN-1); handleRoot just picks one per request, so flipping the flag
// converts the page without a deploy.

// landingData is what landing.src.html may print.
type landingData struct {
	SignupsOpen bool
	Web         string // canonical origin (og:url, the JSON API)
	S3          string // S3 endpoint
	S3Host      string // its bare host (DuckDB, Cyberduck)
	CDN         string // public-object URL prefix
	Domain      string // canonical domain (the house image caption)
	Support     string // support address
}

func mustRenderLanding(signupsOpen bool, h hosts.Hosts) []byte {
	tmpl := template.Must(template.New("landing").Parse(landingTemplateSrc))
	s3Host := h.S3
	if u, err := url.Parse(h.S3); err == nil && u.Host != "" {
		s3Host = u.Host
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, landingData{
		SignupsOpen: signupsOpen, Web: h.Canonical, S3: h.S3, S3Host: s3Host,
		CDN: h.CDNBase(), Domain: h.Domain(), Support: h.SupportEmail(),
	}); err != nil {
		panic("render landing page: " + err.Error())
	}
	return buf.Bytes()
}

// landingPages renders both variants for a host set: [closed, open].
func landingPages(h hosts.Hosts) [2][]byte {
	return [2][]byte{mustRenderLanding(false, h), mustRenderLanding(true, h)}
}

// landingPage is the server's rendered variant; a Server built without
// NewServer (unit tests) renders from the process's hosts on first use.
func (s *Server) landingPage(open bool) []byte {
	s.landingOnce.Do(func() {
		if s.landing[0] == nil {
			s.landing = landingPages(s.hostSet())
		}
	})
	if open {
		return s.landing[1]
	}
	return s.landing[0]
}

// handleRoot serves the public marketing landing page for browser visitors to "/".
//
// The only S3 operation on the root path is ListBuckets, which always carries
// SigV4 auth (an Authorization header) or presigned X-Amz-* query params — those
// requests are delegated to the S3 handler untouched. Only anonymous GET/HEAD "/"
// (which would otherwise return 403 AccessDenied) is served the landing page.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if isS3RootRequest(r) {
		s.handleS3Request(w, r)
		return
	}

	page := s.landingPage(s.flags != nil && s.flags.Enabled(flagSignups, ""))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Short TTL so a launch-day signups-flag flip propagates within a minute.
	w.Header().Set("Cache-Control", "public, max-age=60")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(page)
}

// isS3RootRequest reports whether a request to "/" is an authenticated S3 call
// (ListBuckets) rather than a browser visit. ListBuckets cannot be anonymous, so
// the presence of SigV4 auth — header or presigned query — is a reliable signal.
func isS3RootRequest(r *http.Request) bool {
	return r.Header.Get("Authorization") != "" ||
		r.URL.Query().Get("X-Amz-Algorithm") != ""
}
