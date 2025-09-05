// internal/gateway/gateway.go
package gateway

import (
	"net/http"
	"strings"
)

// Gateway handles API routing
type Gateway struct {
	routes   map[string]string
	handlers map[string]http.HandlerFunc
}

// NewGateway creates a new API gateway
func NewGateway() *Gateway {
	return &Gateway{
		routes:   make(map[string]string),
		handlers: make(map[string]http.HandlerFunc),
	}
}

// RegisterRoute registers a route pattern with a handler name
func (g *Gateway) RegisterRoute(pattern, handler string) {
	g.routes[pattern] = handler
}

// HandleFunc registers an HTTP handler function for a pattern
func (g *Gateway) HandleFunc(pattern string, handler http.HandlerFunc) {
	g.handlers[pattern] = handler
}

// ServeHTTP implements http.Handler interface
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Check for direct handler match
	for pattern, handler := range g.handlers {
		if matchesPattern(pattern, path) {
			handler(w, r)
			return
		}
	}

	// If no handler found, return 404
	http.NotFound(w, r)
}

// Route finds the appropriate handler name for a path
func (g *Gateway) Route(path string) string {
	for pattern, handler := range g.routes {
		if matchesPattern(pattern, path) {
			return handler
		}
	}
	return ""
}

// matchesPattern checks if a path matches a pattern
func matchesPattern(pattern, path string) bool {
	// Simple wildcard matching for now
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "/*")
		return strings.HasPrefix(path, prefix)
	}
	return pattern == path
}
