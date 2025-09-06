// internal/gateway/apikey_test.go
package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAPIKeyManager(t *testing.T) {
	t.Run("validates API keys", func(t *testing.T) {
		// Arrange
		mgr := NewAPIKeyManager()
		key := mgr.GenerateKey("tenant-1", []string{"read", "write"})

		// Act & Assert
		valid, tenant := mgr.ValidateKey(key)
		assert.True(t, valid)
		assert.Equal(t, "tenant-1", tenant)

		// Invalid key
		valid, _ = mgr.ValidateKey("invalid-key")
		assert.False(t, valid)
	})

	t.Run("enforces key permissions", func(t *testing.T) {
		// Arrange
		mgr := NewAPIKeyManager()
		readKey := mgr.GenerateKey("tenant-1", []string{"read"})
		writeKey := mgr.GenerateKey("tenant-1", []string{"read", "write"})

		// Act & Assert
		assert.True(t, mgr.HasPermission(readKey, "read"))
		assert.False(t, mgr.HasPermission(readKey, "write"))

		assert.True(t, mgr.HasPermission(writeKey, "read"))
		assert.True(t, mgr.HasPermission(writeKey, "write"))
	})

	t.Run("middleware validates keys from headers", func(t *testing.T) {
		// Arrange
		mgr := NewAPIKeyManager()
		key := mgr.GenerateKey("tenant-1", []string{"read"})

		handler := mgr.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant := r.Context().Value(ContextKeyTenant).(string)
			_, _ = w.Write([]byte("tenant:" + tenant))
		}))

		// Valid key
		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("X-API-Key", key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "tenant:tenant-1", rec.Body.String())

		// Missing key
		req2 := httptest.NewRequest("GET", "/test", nil)
		rec2 := httptest.NewRecorder()
		handler.ServeHTTP(rec2, req2)

		assert.Equal(t, http.StatusUnauthorized, rec2.Code)
	})
}
