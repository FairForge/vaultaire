package api

import (
	"net/http"
	"strings"
)

// requestContentEncoding returns the Content-Encoding to persist for an
// uploaded object. The aws-chunked token is transport framing (stripped by
// awsChunkedReader before the payload reaches storage), never a property of
// the stored bytes, so it must not be echoed back on GET/HEAD — aws-cli v2
// sends "aws-chunked" (or "aws-chunked,gzip") on every streaming upload.
func requestContentEncoding(r *http.Request) string {
	raw := r.Header.Get("Content-Encoding")
	if raw == "" {
		return ""
	}
	var kept []string
	for _, part := range strings.Split(raw, ",") {
		token := strings.TrimSpace(part)
		if token == "" || strings.EqualFold(token, "aws-chunked") {
			continue
		}
		kept = append(kept, token)
	}
	return strings.Join(kept, ", ")
}

// cleanHeaderValue drops values carrying control characters — the same
// header-injection rule sanitizeContentDisposition applies.
func cleanHeaderValue(v string) string {
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] == 0x7f {
			return ""
		}
	}
	return v
}

// requestContentLanguage returns the Content-Language to persist for an
// uploaded object. Same store-and-echo contract as Content-Encoding.
func requestContentLanguage(r *http.Request) string {
	return cleanHeaderValue(r.Header.Get("Content-Language"))
}

// putEchoHeaders are the remaining PUT request headers S3 stores verbatim and
// echoes on GET/HEAD (Content-Encoding/-Language/-Disposition have their own
// capture above/elsewhere). Website redirect is stored-and-echoed metadata
// only — we serve no website endpoint, so no redirect behavior is implied.
type putEchoHeaders struct {
	CacheControl    string
	Expires         string
	WebsiteRedirect string
}

func requestEchoHeaders(r *http.Request) putEchoHeaders {
	return putEchoHeaders{
		CacheControl:    cleanHeaderValue(r.Header.Get("Cache-Control")),
		Expires:         cleanHeaderValue(r.Header.Get("Expires")),
		WebsiteRedirect: cleanHeaderValue(r.Header.Get("x-amz-website-redirect-location")),
	}
}

func setEchoHeaders(h http.Header, cacheControl, expires, websiteRedirect string) {
	if cacheControl != "" {
		h.Set("Cache-Control", cacheControl)
	}
	if expires != "" {
		h.Set("Expires", expires)
	}
	if websiteRedirect != "" {
		h.Set("x-amz-website-redirect-location", websiteRedirect)
	}
}
