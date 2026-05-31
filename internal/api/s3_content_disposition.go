package api

import (
	"path"
	"strings"
)

// sanitizeContentDisposition guards against HTTP response header injection.
// The value may come from a user-controlled request header (PUT) or query
// parameter (?response-content-disposition), so any control character (CR, LF,
// NUL, DEL, …) means we drop the value entirely rather than emit a header that
// could split the response. Returns "" for empty or unsafe input.
func sanitizeContentDisposition(v string) string {
	if v == "" {
		return ""
	}
	for _, c := range v {
		if c < 0x20 || c == 0x7f {
			return ""
		}
	}
	return v
}

// isInlineRenderable reports whether a content type is safe to render inline in
// a browser. Unknown types default to attachment (download), which is the safe
// choice for a CDN serving arbitrary tenant content — it avoids inline
// rendering of untrusted HTML/SVG.
func isInlineRenderable(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return strings.HasPrefix(ct, "image/") ||
		strings.HasPrefix(ct, "video/") ||
		strings.HasPrefix(ct, "text/") ||
		ct == "application/pdf"
}

// attachmentDisposition builds `attachment; filename="<base>"` for an object
// key, stripping quotes, backslashes, and control characters from the filename.
func attachmentDisposition(key string) string {
	base := path.Base(key)
	base = strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, base)
	if base == "" || base == "." || base == "/" {
		return "attachment"
	}
	return `attachment; filename="` + base + `"`
}

// cdnContentDisposition computes the Content-Disposition the CDN should serve,
// in precedence order:
//  1. force-download flag on the bucket → attachment
//  2. a stored Content-Disposition on the object → use it
//  3. a browser-renderable content type → inline
//  4. otherwise → attachment (safe default)
func cdnContentDisposition(forceDownload bool, stored, contentType, key string) string {
	if forceDownload {
		return attachmentDisposition(key)
	}
	if s := sanitizeContentDisposition(stored); s != "" {
		return s
	}
	if isInlineRenderable(contentType) {
		return "inline"
	}
	return attachmentDisposition(key)
}
