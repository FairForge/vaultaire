package middleware

import "net/http"

// ContentSecurityPolicy is the policy of every dashboard, admin and public
// auth page. No external host appears in it: every script, style, font and
// image the pages use is served by this binary (htmx is vendored under
// /static/js; the 2FA QR code is a PNG the server renders — until WP-R12-8
// `script-src` allowed cdn.jsdelivr.net for a QR library loaded, without SRI,
// on the page that shows a new TOTP secret). `'unsafe-inline'` is still there
// for the layouts' small inline scripts and style attributes.
const ContentSecurityPolicy = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'"

// SecurityHeaders sets browser security headers on every dashboard and admin
// response. Applied only to dashboard routes — S3 API responses must not
// include CSP or X-Frame-Options as they would break XML-parsing S3 clients.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", ContentSecurityPolicy)
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}
