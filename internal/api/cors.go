package api

import (
	"net/http"
	"strings"
)

const (
	cdnAllowMethods  = "GET, HEAD, OPTIONS"
	cdnExposeHeaders = "ETag, Content-Length, Content-Range, Content-Type, Last-Modified, Accept-Ranges"
	cdnMaxAge        = "86400"
)

func setCORSHeaders(w http.ResponseWriter, r *http.Request, allowedOrigins string) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}

	matched := matchOrigin(origin, allowedOrigins)
	if matched == "" {
		return
	}

	w.Header().Set("Access-Control-Allow-Origin", matched)
	w.Header().Set("Access-Control-Allow-Methods", cdnAllowMethods)
	w.Header().Set("Access-Control-Expose-Headers", cdnExposeHeaders)
	w.Header().Set("Access-Control-Max-Age", cdnMaxAge)
	if matched != "*" {
		w.Header().Add("Vary", "Origin")
	}
}

func handleCDNPreflight(w http.ResponseWriter, r *http.Request, allowedOrigins string) {
	setCORSHeaders(w, r, allowedOrigins)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

func matchOrigin(origin, allowedOrigins string) string {
	if allowedOrigins == "" || origin == "" {
		return ""
	}
	if allowedOrigins == "*" {
		return "*"
	}
	for _, allowed := range strings.Split(allowedOrigins, ",") {
		allowed = strings.TrimSpace(allowed)
		if allowed == "*" {
			return "*"
		}
		if strings.EqualFold(origin, allowed) {
			return origin
		}
	}
	return ""
}
