package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/dashboard/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestRecovery_PanicReturns500(t *testing.T) {
	// Arrange
	core, logs := observer.New(zap.ErrorLevel)
	logger := zap.New(core)

	handler := middleware.Recovery(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	}))

	req := httptest.NewRequest(http.MethodGet, "/dashboard/explode", nil)
	rec := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	body := rec.Body.String()
	assert.Contains(t, body, "500")
	assert.Contains(t, body, "Something went wrong")
	assert.NotContains(t, body, "goroutine", "stack trace must not leak to client")

	// Verify the panic was logged with stack trace.
	require.Equal(t, 1, logs.Len())
	entry := logs.All()[0]
	assert.Equal(t, "panic recovered", entry.Message)
}

func TestRecovery_NoPanic_Passthrough(t *testing.T) {
	// Arrange
	core, logs := observer.New(zap.ErrorLevel)
	logger := zap.New(core)

	handler := middleware.Recovery(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	rec := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", rec.Body.String())
	assert.Equal(t, 0, logs.Len(), "no errors should be logged on normal request")
}

func TestRecovery_PanicWithNonStringValue(t *testing.T) {
	// Arrange — panics with a non-string value (e.g. int).
	core, logs := observer.New(zap.ErrorLevel)
	logger := zap.New(core)

	handler := middleware.Recovery(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(42)
	}))

	req := httptest.NewRequest(http.MethodGet, "/dashboard/boom", nil)
	rec := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, 1, logs.Len())
	// Verify the body does not contain any Go source file paths.
	assert.False(t, strings.Contains(rec.Body.String(), ".go:"),
		"response must not contain Go source paths")
}
