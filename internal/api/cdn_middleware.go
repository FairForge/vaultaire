package api

import (
	"net/http"

	"github.com/FairForge/vaultaire/internal/hosts"
)

// CDNHostRouter routes requests on a CDN host (hosts.IsCDNHost: the
// CDN_PUBLIC_HOST host, cdn.<canonical>, cdn.<each alias>, and cdn.stored.ge /
// cdn.stored.cloud whatever the environment) to cdnHandler. All other hosts go
// to the fallback handler (normal router).
func CDNHostRouter(h hosts.Hosts, cdnHandler, fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.IsCDNHost(r.Host) {
			cdnHandler.ServeHTTP(w, r)
			return
		}
		fallback.ServeHTTP(w, r)
	})
}
