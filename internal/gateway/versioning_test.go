// internal/gateway/versioning_test.go
package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestVersioning(t *testing.T) {
	t.Run("extracts version from URL path", func(t *testing.T) {
		// Arrange
		vm := NewVersionManager()

		// Act & Assert
		assert.Equal(t, "v1", vm.ExtractVersion("/api/v1/users"))
		assert.Equal(t, "v2", vm.ExtractVersion("/api/v2/users"))
		assert.Equal(t, "", vm.ExtractVersion("/users"))
	})

	t.Run("routes to version-specific handler", func(t *testing.T) {
		// Arrange
		vm := NewVersionManager()
		vm.RegisterHandler("v1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("API v1"))
		}))
		vm.RegisterHandler("v2", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("API v2"))
		}))

		// Test v1
		req1 := httptest.NewRequest("GET", "/api/v1/test", nil)
		rec1 := httptest.NewRecorder()
		vm.ServeHTTP(rec1, req1)
		assert.Equal(t, "API v1", rec1.Body.String())

		// Test v2
		req2 := httptest.NewRequest("GET", "/api/v2/test", nil)
		rec2 := httptest.NewRecorder()
		vm.ServeHTTP(rec2, req2)
		assert.Equal(t, "API v2", rec2.Body.String())
	})
}
