package api

import (
	"net"
	"net/http"
)

// CDNHostRouter routes requests with Host cdn.stored.ge or cdn.stored.cloud
// to cdnHandler. All other hosts go to the fallback handler (normal router).
func CDNHostRouter(cdnHandler, fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		switch host {
		case "cdn.stored.ge", "cdn.stored.cloud":
			cdnHandler.ServeHTTP(w, r)
		default:
			fallback.ServeHTTP(w, r)
		}
	})
}
