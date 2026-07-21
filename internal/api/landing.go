package api

import (
	"bytes"
	_ "embed"
	"html/template"
	"net/http"
)

//go:embed landing.html
var landingTemplateSrc string

// The landing page has two variants driven by the signups feature flag: closed
// renders a waitlist capture, open renders working /register CTAs. Both are
// rendered once at process start; handleRoot just picks one per request, so
// flipping the flag converts the page without a deploy.
var (
	landingClosed = mustRenderLanding(false)
	landingOpen   = mustRenderLanding(true)
)

func mustRenderLanding(signupsOpen bool) []byte {
	tmpl := template.Must(template.New("landing").Parse(landingTemplateSrc))
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct{ SignupsOpen bool }{signupsOpen}); err != nil {
		panic("render landing page: " + err.Error())
	}
	return buf.Bytes()
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

	page := landingClosed
	if s.flags != nil && s.flags.Enabled(flagSignups, "") {
		page = landingOpen
	}

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
