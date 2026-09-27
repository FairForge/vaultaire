package api

import (
	_ "embed"
	"net/http"
	"strconv"
)

// og.png is the social preview for the landing page (Open Graph / Twitter
// card): the pixel house at 1200x630. It is rendered from the landing page
// sources with `make og` (headless Chrome) and committed, so a scene change
// needs a re-render; the meta tags in landing.src.html point at /og.png.
//
//go:embed og.png
var ogImagePNG []byte

// handleOGImage serves the preview image. Registered before the S3 catch-all
// (landing.go pattern); long cache, the file only changes on deploy.
func (s *Server) handleOGImage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(ogImagePNG)))
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(ogImagePNG)
}
