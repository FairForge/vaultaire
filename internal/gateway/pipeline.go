// internal/gateway/pipeline.go
package gateway

import (
	"net/http"
)

// Middleware is a function that wraps an http.Handler
type Middleware func(http.Handler) http.Handler

// Pipeline manages middleware execution order
type Pipeline struct {
	middlewares []Middleware
}

// NewPipeline creates a new middleware pipeline
func NewPipeline() *Pipeline {
	return &Pipeline{
		middlewares: make([]Middleware, 0),
	}
}

// Use adds a middleware to the pipeline
func (p *Pipeline) Use(middleware Middleware) {
	p.middlewares = append(p.middlewares, middleware)
}

// Then wraps the final handler with all middlewares
func (p *Pipeline) Then(handler http.Handler) http.Handler {
	// Apply middleware in reverse order so they execute in the correct order
	for i := len(p.middlewares) - 1; i >= 0; i-- {
		handler = p.middlewares[i](handler)
	}
	return handler
}

// ServeHTTP allows Pipeline to be used as an http.Handler
func (p *Pipeline) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Default to 404 if no handler is set
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	p.Then(handler).ServeHTTP(w, r)
}
