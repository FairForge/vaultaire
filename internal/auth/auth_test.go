package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegister(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping database test in short mode")
	}
	// Setup test database
	db := setupTestDB(t)
	handler := NewAuthHandler(db, nil)

	// Test registration
	reqBody := `{"email":"test@stored.ge","password":"Password123!","company":"Test Corp"}`
	req := httptest.NewRequest("POST", "/auth/register", bytes.NewBufferString(reqBody))
	w := httptest.NewRecorder()

	handler.Register(w, req)

	require.Equal(t, http.StatusOK, w.Code)

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
