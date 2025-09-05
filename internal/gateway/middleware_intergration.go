// internal/gateway/middleware_integration.go
package gateway

import (
	"net/http"

	"github.com/FairForge/vaultaire/internal/gateway/validation"
	"github.com/go-chi/chi/v5"
)

// SetupValidatedRoutes adds validation to your API routes
func SetupValidatedRoutes(r chi.Router) {
	validator := validation.NewRequestValidator()

	// Example: Validate artifact uploads
	r.Route("/v1/containers/{container}/artifacts", func(r chi.Router) {
		uploadRules := &validation.ValidationRules{
			ContentTypes: []string{"application/octet-stream"},
			MaxBodySize:  5 * 1024 * 1024 * 1024, // 5GB
			Headers: validation.HeaderRules{
				Required: []string{"X-Tenant-ID", "X-API-Key"},
			},
		}

		r.With(validation.ValidationMiddleware(validator, uploadRules)).
			Put("/{artifact}", handlePutArtifact)
	})

	// Example: Validate list operations
	r.Route("/v1/containers/{container}/artifacts", func(r chi.Router) {
		listRules := &validation.ValidationRules{
			Query: validation.QueryRules{
				Types: map[string]validation.ParamType{
					"limit":  validation.ParamTypeInt,
					"offset": validation.ParamTypeInt,
				},
				Ranges: map[string]validation.Range{
					"limit": {Min: 1, Max: 1000},
				},
			},
		}

		r.With(validation.ValidationMiddleware(validator, listRules)).
			Get("/", handleListArtifacts)
	})
}

// Placeholder handlers
func handlePutArtifact(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusCreated)
}

func handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
