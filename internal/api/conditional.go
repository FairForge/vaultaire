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
	// If-Match is evaluated before everything else (RFC 9110 §13.2.2) —
	// it was never evaluated on GET/HEAD at all (R2-11 / R4-07).
	if checkIfMatch(r, etag) {
		return http.StatusPreconditionFailed
	}
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

// copySourceConditionHeaders maps CopyObject's x-amz-copy-source-if-*
// headers to the conditional headers they mirror.
var copySourceConditionHeaders = [][2]string{
	{"x-amz-copy-source-if-match", "If-Match"},
	{"x-amz-copy-source-if-none-match", "If-None-Match"},
	{"x-amz-copy-source-if-modified-since", "If-Modified-Since"},
	{"x-amz-copy-source-if-unmodified-since", "If-Unmodified-Since"},
}

// evaluateCopySourceConditions decides CopyObject's x-amz-copy-source-if-*
// conditions on the copy's source (its ETag and Last-Modified; R61-8 — no
// copy evaluated them). 412 when one fails, 0 to proceed. AWS's two pairs:
// an If-Match that holds wins over an If-Unmodified-Since that does not;
// an If-None-Match that fails (the ETag matches) is 412 whatever
// If-Modified-Since says, and one that holds wins over it.
func evaluateCopySourceConditions(r *http.Request, etag string, lastModified time.Time) int {
	h := http.Header{}
	for _, m := range copySourceConditionHeaders {
		if v := r.Header.Get(m[0]); v != "" {
			h.Set(m[1], v)
		}
	}
	if len(h) == 0 {
		return 0
	}
	cr := &http.Request{Header: h}
	if h.Get("If-Match") != "" {
		if checkIfMatch(cr, etag) {
			return http.StatusPreconditionFailed
		}
	} else if checkIfUnmodifiedSince(cr, lastModified) {
		return http.StatusPreconditionFailed
	}
	if h.Get("If-None-Match") != "" {
		if checkIfNoneMatch(cr, etag) {
			return http.StatusPreconditionFailed
		}
	} else if checkIfModifiedSince(cr, lastModified) {
		return http.StatusPreconditionFailed
	}
	return 0
}
