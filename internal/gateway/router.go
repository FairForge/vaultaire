// internal/gateway/router.go
package gateway

import (
	"net/http"
)

// Router handles request routing and transformation
type Router struct {
	transforms []Transform
	backends   map[string]string
}

// Transform interface for request transformations
type Transform interface {
	Apply(r *http.Request) *http.Request
}

// HeaderTransform adds or removes headers
type HeaderTransform struct {
	Add    map[string]string
	Remove []string
}

// Apply implements Transform interface for HeaderTransform
func (ht HeaderTransform) Apply(r *http.Request) *http.Request {
	// Clone the request
	r2 := r.Clone(r.Context())

	// Remove headers
	for _, header := range ht.Remove {
		r2.Header.Del(header)
	}

	// Add headers
	for key, value := range ht.Add {
		r2.Header.Set(key, value)
	}

	return r2
}

// NewRouter creates a new router
func NewRouter() *Router {
	return &Router{
		transforms: make([]Transform, 0),
		backends:   make(map[string]string),
	}
}

// AddTransform adds a transformation to the pipeline
func (r *Router) AddTransform(t Transform) {
	r.transforms = append(r.transforms, t)
}

// Transform applies all transformations to a request
func (r *Router) Transform(req *http.Request) *http.Request {
	result := req
	for _, transform := range r.transforms {
		result = transform.Apply(result)
	}
	return result
}

// AddBackend registers a backend for a pattern
func (r *Router) AddBackend(pattern, backend string) {
	r.backends[pattern] = backend
}

// SelectBackend chooses the appropriate backend for a path
func (r *Router) SelectBackend(path string) string {
	for pattern, backend := range r.backends {
		if matchesPattern(pattern, path) {
			return backend
		}
	}
	return ""
}
