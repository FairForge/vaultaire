// internal/gateway/gateway_test.go
package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGateway(t *testing.T) {
	t.Run("routes requests to correct backend", func(t *testing.T) {
		// Arrange
		gw := NewGateway()
		gw.RegisterRoute("/api/v1/*", "v1-handler")
		gw.RegisterRoute("/api/v2/*", "v2-handler")

		// Act
		handler := gw.Route("/api/v1/users")

		// Assert
		assert.Equal(t, "v1-handler", handler)
	})

	t.Run("supports exact matches", func(t *testing.T) {
		// Arrange
		gw := NewGateway()
		gw.RegisterRoute("/health", "health-handler")
		gw.RegisterRoute("/metrics", "metrics-handler")

		// Act & Assert
		assert.Equal(t, "health-handler", gw.Route("/health"))
		assert.Equal(t, "metrics-handler", gw.Route("/metrics"))
		assert.Equal(t, "", gw.Route("/unknown"))
	})

	t.Run("handles HTTP requests", func(t *testing.T) {
		// Arrange
		gw := NewGateway()
		gw.HandleFunc("/api/v1/*", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v1 response"))
		})

		// Act
		req := httptest.NewRequest("GET", "/api/v1/test", nil)
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)

		// Assert
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "v1 response", rec.Body.String())
	})
}
