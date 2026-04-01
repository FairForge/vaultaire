package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestRegister(t *testing.T) {
	// Setup test database
	db := setupTestDB(t)
	handler := NewAuthHandler(db, zap.NewNop())

	// Test registration
	reqBody := `{"email":"test@stored.ge","password":"Password123!","company":"Test Corp"}`
	req := httptest.NewRequest("POST", "/auth/register", bytes.NewBufferString(reqBody))
	w := httptest.NewRecorder()

	handler.Register(w, req)

	require.Equal(t, http.StatusOK, w.Code, "Response: %s", w.Body.String())

	var resp map[string]string
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)
	require.Contains(t, resp, "accessKeyId")
	require.Contains(t, resp, "secretAccessKey")
}

func TestLogin(t *testing.T) {
	// Test login after registration
	// Add test here
}
