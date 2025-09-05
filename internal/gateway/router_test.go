// internal/gateway/router_test.go
package gateway

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRouter(t *testing.T) {
	t.Run("transforms request headers", func(t *testing.T) {
		// Arrange
		router := NewRouter()
		router.AddTransform(HeaderTransform{
			Add:    map[string]string{"X-Gateway": "vaultaire"},
			Remove: []string{"X-Internal"},
		})

		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("X-Internal", "secret")

		// Act
		transformed := router.Transform(req)

		// Assert
		assert.Equal(t, "vaultaire", transformed.Header.Get("X-Gateway"))
		assert.Empty(t, transformed.Header.Get("X-Internal"))
	})

	t.Run("routes to backend based on path", func(t *testing.T) {
		// Arrange
		router := NewRouter()
		router.AddBackend("/storage/*", "http://backend1")
		router.AddBackend("/api/*", "http://backend2")

		// Act
		backend1 := router.SelectBackend("/storage/file.txt")
		backend2 := router.SelectBackend("/api/users")

		// Assert
		assert.Equal(t, "http://backend1", backend1)
		assert.Equal(t, "http://backend2", backend2)
	})
}
