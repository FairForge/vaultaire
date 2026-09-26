package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// R5-13: POST /api/v1/user/keys used to echo the requested permissions in the
// response while the stored key stayed ["*"], and never validated them.
func TestHandleCreateUserAPIKey_PersistsRequestedScope(t *testing.T) {
	authSvc := auth.NewAuthService(nil, nil)
	user, _, _, err := authSvc.CreateUserWithTenant(context.Background(), "userapi@stored.ge", "Str0ngPassw0rd!", "x")
	require.NoError(t, err)
	s := &Server{logger: zap.NewNop(), auth: authSvc, config: &config.Config{Server: config.ServerConfig{Port: 8000}}}

	body := `{"name":"ro","permissions":["GetObject"],"expiry_days":7}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/keys", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), userIDKey, user.ID))
	w := httptest.NewRecorder()
	s.handleCreateUserAPIKey(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var resp struct {
		ID          string   `json:"id"`
		Permissions []string `json:"permissions"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, []string{"GetObject"}, resp.Permissions)

	keys, err := authSvc.ListAPIKeys(context.Background(), user.ID)
	require.NoError(t, err)
	for _, k := range keys {
		if k.ID == resp.ID {
			assert.Equal(t, []string{"GetObject"}, k.Permissions, "the stored key must carry the requested scope")
			assert.NotNil(t, k.ExpiresAt)
			return
		}
	}
	t.Fatal("created key not found")
}

func TestHandleCreateUserAPIKey_RejectsUnknownPermission(t *testing.T) {
	authSvc := auth.NewAuthService(nil, nil)
	user, _, _, err := authSvc.CreateUserWithTenant(context.Background(), "userapi2@stored.ge", "Str0ngPassw0rd!", "x")
	require.NoError(t, err)
	s := &Server{logger: zap.NewNop(), auth: authSvc, config: &config.Config{Server: config.ServerConfig{Port: 8000}}}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/keys", strings.NewReader(`{"name":"x","permissions":["Bogus"]}`))
	req = req.WithContext(context.WithValue(req.Context(), userIDKey, user.ID))
	w := httptest.NewRecorder()
	s.handleCreateUserAPIKey(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
