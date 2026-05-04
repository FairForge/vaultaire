package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newVersionTestServer() *Server {
	return &Server{
		logger: zap.NewNop(),
	}
}

func TestVersionHeader(t *testing.T) {
	s := newVersionTestServer()

	handler := s.versionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	got := rec.Header().Get("X-Vaultaire-Version")
	require.NotEmpty(t, got, "X-Vaultaire-Version header must be present")
}

func TestVersionHeaderValue(t *testing.T) {
	s := newVersionTestServer()

	handler := s.versionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, APIVersion, rec.Header().Get("X-Vaultaire-Version"))
}

func TestVersionHeaderClientEcho(t *testing.T) {
	s := newVersionTestServer()

	handler := s.versionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/health", nil)
	req.Header.Set("X-Vaultaire-Version", "2025-01-01")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, APIVersion, rec.Header().Get("X-Vaultaire-Version"))
}
