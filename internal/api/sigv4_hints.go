package api

import (
	"net/http"
	"strings"
)

// missingSignedHeaderHint is the S3 layer's answer to auth.Auth.MissingSignedHeader:
// the value a client most plausibly signed for a header that is in
// SignedHeaders but did not arrive.
//
// R2 Sippy's multi-part pull — every object above ~200 MiB that is read
// through a Sippy-backed R2 bucket — signs `if-match: "<etag>"` on each
// part GET and sends the request without the header (prod origin,
// 2026-10-05: five parallel part GETs answered SignatureDoesNotMatch; a
// Worker's own fetch delivers If-Match intact, so Sippy's client drops it).
// The ETag it signs is the one the first response carried — the head row's.
// Offering that value lets the signature verify; it never relaxes the
// check, because the signature must still be valid over the request plus
// exactly that value. Semantically the condition is then satisfied by
// construction (the object's ETag matches itself), so serving the object
// unconditionally is the right answer.
func (s *Server) missingSignedHeaderHint(r *http.Request, tenantID, name string) (string, bool) {
	if name != "if-match" || s.db == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return "", false
	}
	bucket, key, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !ok || bucket == "" || key == "" {
		return "", false
	}
	var etag string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT etag FROM object_head_cache WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`,
		tenantID, bucket, key).Scan(&etag)
	if err != nil || etag == "" {
		return "", false
	}
	return `"` + strings.Trim(etag, `"`) + `"`, true
}
