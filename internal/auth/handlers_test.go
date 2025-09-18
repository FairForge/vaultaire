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

func TestAuthHandler_Register(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping database test")
	}

	db := setupTestDB(t)
	defer func() { _ = db.Close() }()

	logger := zap.NewNop()
	handler := NewAuthHandler(db, logger)

	reqBody := `{"email":"test@stored.ge","password":"Password123!","company":"Test Corp"}`
	req := httptest.NewRequest("POST", "/auth/register", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.Register(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]string
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)
	require.Contains(t, resp, "accessKeyId")
	require.Contains(t, resp, "secretAccessKey")
	require.Contains(t, resp, "endpoint")
}
