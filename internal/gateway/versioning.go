// internal/gateway/versioning.go
package gateway

import (
	"net/http"
	"regexp"
	"strings"
)

// VersionManager handles API versioning
type VersionManager struct {
	handlers       map[string]http.Handler
	defaultVersion string
}

// NewVersionManager creates a new version manager
func NewVersionManager() *VersionManager {
	return &VersionManager{
		handlers:       make(map[string]http.Handler),
		defaultVersion: "v1",
	}
}

// ExtractVersion extracts the API version from a path
func (vm *VersionManager) ExtractVersion(path string) string {
	// Pattern: /api/v{number}/...
	re := regexp.MustCompile(`/api/(v\d+)/`)
	matches := re.FindStringSubmatch(path)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// RegisterHandler registers a handler for a specific version
func (vm *VersionManager) RegisterHandler(version string, handler http.Handler) {
	vm.handlers[version] = handler
}

// ServeHTTP implements http.Handler interface
func (vm *VersionManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	version := vm.ExtractVersion(r.URL.Path)

	// If no version found, try default
	if version == "" {
		version = vm.defaultVersion
	}

	// Find handler for version
	if handler, exists := vm.handlers[version]; exists {
		// Strip version from path before passing to handler
		r.URL.Path = strings.Replace(r.URL.Path, "/api/"+version, "", 1)
		handler.ServeHTTP(w, r)
		return
	}

	// No handler found
	http.Error(w, "API version not supported", http.StatusNotFound)
}

// SetDefaultVersion sets the default API version
func (vm *VersionManager) SetDefaultVersion(version string) {
	vm.defaultVersion = version
}
