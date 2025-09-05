// internal/gateway/pipeline_test.go
package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPipeline(t *testing.T) {
	t.Run("executes middleware in order", func(t *testing.T) {
		// Arrange
		pipeline := NewPipeline()
		var order []string

		pipeline.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, "first")
				next.ServeHTTP(w, r)
			})
		})

		pipeline.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, "second")
				next.ServeHTTP(w, r)
			})
		})

		final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "handler")
			w.WriteHeader(http.StatusOK)
		})

		// Act
		req := httptest.NewRequest("GET", "/test", nil)
		rec := httptest.NewRecorder()
		pipeline.Then(final).ServeHTTP(rec, req)

		// Assert
		assert.Equal(t, []string{"first", "second", "handler"}, order)
	})
}
