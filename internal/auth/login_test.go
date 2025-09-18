package auth

import (
	"bytes"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLogin_Success(t *testing.T) {
	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	handler := NewAuthHandler(db, nil)

	// First register a user
	regReq := `{"email":"test@stored.ge","password":"Test123!"}`
	req := httptest.NewRequest("POST", "/auth/register", bytes.NewBufferString(regReq))
	w := httptest.NewRecorder()
	handler.Register(w, req)

	// Now try to login
	loginReq := `{"email":"test@stored.ge","password":"Test123!"}`
	req = httptest.NewRequest("POST", "/auth/login", bytes.NewBufferString(loginReq))
	w = httptest.NewRecorder()

	handler.Login(w, req) // This will fail - Login doesn't exist yet

	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]string
	_ = json.NewDecoder(w.Body).Decode(&resp)
	require.Contains(t, resp, "token")
	require.NotEmpty(t, resp["token"])
}

func TestLogin_InvalidPassword(t *testing.T) {
	// Test wrong password returns 401
	// TODO: Implement
}

func TestLogin_UserNotFound(t *testing.T) {
	// Test non-existent user returns 401
	// TODO: Implement
}
