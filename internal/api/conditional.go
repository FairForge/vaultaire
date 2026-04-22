package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

func normalizeETag(etag string) string {
	etag = strings.TrimPrefix(etag, "W/")
	etag = strings.Trim(etag, `"`)
	return etag
}

func etagsMatch(a, b string) bool {
	na, nb := normalizeETag(a), normalizeETag(b)
	return na != "" && na == nb
}

func checkIfNoneMatch(r *http.Request, currentETag string) bool {
	inm := r.Header.Get("If-None-Match")
	if inm == "" {
		return false
	}
	if inm == "*" {
		return true
	}
	for _, candidate := range strings.Split(inm, ",") {
		if etagsMatch(strings.TrimSpace(candidate), currentETag) {
			return true
		}
	}
	return false
}

func checkIfModifiedSince(r *http.Request, lastModified time.Time) bool {
	ims := r.Header.Get("If-Modified-Since")
	if ims == "" {
		return false
	}
	t, err := http.ParseTime(ims)
	if err != nil {
		return false
	}
	return !lastModified.Truncate(time.Second).After(t.Truncate(time.Second))
}

func checkIfUnmodifiedSince(r *http.Request, lastModified time.Time) bool {
	ius := r.Header.Get("If-Unmodified-Since")
	if ius == "" {
		return false
	}
	t, err := http.ParseTime(ius)
	if err != nil {
		return false
	}
	return lastModified.Truncate(time.Second).After(t.Truncate(time.Second))
}

func checkIfMatch(r *http.Request, currentETag string) bool {
	im := r.Header.Get("If-Match")
	if im == "" {
		return false
	}
	if im == "*" {
		return false
	}
	for _, candidate := range strings.Split(im, ",") {
		if etagsMatch(strings.TrimSpace(candidate), currentETag) {
			return false
		}
	}
	return true
}

// evaluateConditionalGET checks conditional headers per RFC 9110 §13.2.2.
// Returns 304, 412, or 0 (proceed normally).
func evaluateConditionalGET(r *http.Request, etag string, lastModified time.Time) int {
	if checkIfUnmodifiedSince(r, lastModified) {
		return http.StatusPreconditionFailed
	}
	if checkIfNoneMatch(r, etag) {
		return http.StatusNotModified
	}
	if r.Header.Get("If-None-Match") == "" && checkIfModifiedSince(r, lastModified) {
		return http.StatusNotModified
	}
	return 0
}

func writeNotModified(w http.ResponseWriter, etag string, lastModified time.Time, cacheControl string) {
	if etag != "" {
		w.Header().Set("ETag", fmt.Sprintf(`"%s"`, normalizeETag(etag)))
	}
	if !lastModified.IsZero() {
		w.Header().Set("Last-Modified", lastModified.UTC().Format(http.TimeFormat))
	}
	if cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	w.WriteHeader(http.StatusNotModified)
}
