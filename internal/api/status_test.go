package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestStatusPage_Healthy(t *testing.T) {
	// Arrange
	s := newTestServerWithHealthChecker(t)
	s.healthChecker.RegisterBackend("quotaless")
	s.healthChecker.UpdateHealth("quotaless", true, 5*time.Millisecond, nil)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()

	// Act
	s.handleStatusPage(rec, req)

	// Assert
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	body := rec.Body.String()
	assert.Contains(t, body, "All Systems Operational")
	assert.Contains(t, body, "stored.ge")
	assert.Contains(t, body, "0.1.0")
	assert.Contains(t, body, "1 / 1 healthy")
}

func TestStatusPage_Degraded(t *testing.T) {
	// Arrange
	s := newTestServerWithHealthChecker(t)
	s.healthChecker.RegisterBackend("quotaless")
	s.healthChecker.UpdateHealth("quotaless", false, 0, assert.AnError)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()

	// Act
	s.handleStatusPage(rec, req)

	// Assert
	body := rec.Body.String()
	assert.Contains(t, body, "Degraded")
}

func TestRequestIDMiddleware_SetsHeaders(t *testing.T) {
	// Arrange
	s := newTestServerWithHealthChecker(t)
	handler := s.requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	rec := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	assert.NotEmpty(t, rec.Header().Get("X-Request-Id"))
	assert.Equal(t, "stored.ge", rec.Header().Get("Server"))

	// Verify the request ID looks like a UUID (36 chars with dashes).
	rid := rec.Header().Get("X-Request-Id")
	assert.Len(t, rid, 36)
}
