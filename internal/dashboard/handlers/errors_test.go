package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func TestHandleNotFound_Returns404HTML(t *testing.T) {
	// Arrange
	handler := handlers.HandleNotFound(zap.NewNop())
	req := httptest.NewRequest(http.MethodGet, "/dashboard/does-not-exist", nil)
	rec := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	body := rec.Body.String()
	assert.Contains(t, body, "404")
	assert.Contains(t, body, "Page not found")
	assert.Contains(t, body, "Back to Dashboard")
}
