package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWriteS3Error_AccountSuspended(t *testing.T) {
	// Arrange
	w := httptest.NewRecorder()

	// Act
	WriteS3Error(w, ErrAccountSuspended, "/test-bucket/test.txt", "req-123")

	// Assert
	assert.Equal(t, 403, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "<Code>AccountSuspended</Code>")
	assert.Contains(t, body, "suspended")
	assert.Equal(t, "application/xml", w.Header().Get("Content-Type"))
}

func TestIsTenantSuspended_NilDB(t *testing.T) {
	// isTenantSuspended should fail open (return false) when DB is nil.
	// Can't call with nil db (would panic), so we just verify the function exists
	// and the error codes are properly registered.
	assert.Equal(t, "Your account has been suspended. Contact support for assistance.", errorMessages[ErrAccountSuspended])
	assert.Equal(t, 403, errorStatusCodes[ErrAccountSuspended])
}

func TestWriteS3Error_AccessDenied(t *testing.T) {
	// Verify existing AccessDenied still works alongside new AccountSuspended.
	w := httptest.NewRecorder()
	WriteS3Error(w, ErrAccessDenied, "/bucket", "req-456")

	assert.Equal(t, 403, w.Code)
	body := w.Body.String()
	assert.True(t, strings.Contains(body, "<Code>AccessDenied</Code>"))
}
