package api

import (
	_ "embed"
	"net/http"
)

//go:embed landing.html
var landingHTML []byte

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

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(landingHTML)
}

// isS3RootRequest reports whether a request to "/" is an authenticated S3 call
// (ListBuckets) rather than a browser visit. ListBuckets cannot be anonymous, so
// the presence of SigV4 auth — header or presigned query — is a reliable signal.
func isS3RootRequest(r *http.Request) bool {
	return r.Header.Get("Authorization") != "" ||
		r.URL.Query().Get("X-Amz-Algorithm") != ""
}
