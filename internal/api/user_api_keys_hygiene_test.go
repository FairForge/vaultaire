package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/config"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R11-6 (R11-17, R11-22): the user API's key routes answer failures in
// the management envelope with the right status — 404 for an id that is
// not the caller's, 409 for a revoked key, 400 for a day count below 1 —
// and the management listing shows a revoked key as revoked.

type keyHygieneFixture struct {
	t    *testing.T
	s    *Server
	r    chi.Router
	user *auth.User
	key  *auth.APIKey
}

func newKeyHygieneFixture(t *testing.T) *keyHygieneFixture {
	t.Helper()
	authSvc := auth.NewAuthService(nil, nil)
	user, _, key, err := authSvc.CreateUserWithTenant(context.Background(), "keyhygiene-"+auth.GenerateID()[:8]+"@stored.ge", "Str0ngPassw0rd!", "x")
	require.NoError(t, err)
	s := &Server{logger: zap.NewNop(), auth: authSvc, config: &config.Config{Server: config.ServerConfig{Port: 8000}}}
	r := chi.NewRouter()
	r.Get("/apikeys", s.handleListUserAPIKeys)
	r.Post("/apikeys", s.handleCreateUserAPIKey)
	r.Post("/apikeys/{keyId}/rotate", s.handleRotateUserAPIKey)
	r.Delete("/apikeys/{keyId}", s.handleDeleteUserAPIKey)
	r.Post("/apikeys/{keyId}/expire", s.handleSetUserAPIKeyExpiration)
	r.Get("/keys", s.handleMgmtListKeys)
	return &keyHygieneFixture{t: t, s: s, r: r, user: user, key: key}
}

type envelope struct {
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Param   string `json:"param"`
	} `json:"error"`
}

func (f *keyHygieneFixture) do(method, path, body string) (*httptest.ResponseRecorder, envelope) {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx := context.WithValue(req.Context(), userIDKey, f.user.ID)
	ctx = context.WithValue(ctx, tenantIDKey, f.user.TenantID)
	rr := httptest.NewRecorder()
	f.r.ServeHTTP(rr, req.WithContext(ctx))
	var env envelope
	if rr.Code >= 400 {
		require.NoError(f.t, json.Unmarshal(rr.Body.Bytes(), &env), "every failure is the JSON envelope: %s", rr.Body.String())
		assert.Equal(f.t, "application/json", rr.Header().Get("Content-Type"))
	}
	return rr, env
}

func TestUserKeyAPI_UnknownKeyIs404InTheEnvelope(t *testing.T) {
	f := newKeyHygieneFixture(t)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/apikeys/not-a-key/rotate", ""},
		{"DELETE", "/apikeys/not-a-key", ""},
		{"POST", "/apikeys/not-a-key/expire", `{"days":30}`},
	} {
		rr, env := f.do(c.method, c.path, c.body)
		assert.Equal(t, http.StatusNotFound, rr.Code, "%s %s", c.method, c.path)
		assert.Equal(t, ErrTypeNotFound, env.Error.Type)
		assert.Equal(t, "key_not_found", env.Error.Code)
		assert.Equal(t, "keyId", env.Error.Param)
	}

	// Another user's key is not the caller's either.
	other, _, otherKey, err := f.s.auth.CreateUserWithTenant(context.Background(), "other-"+auth.GenerateID()[:8]+"@stored.ge", "Str0ngPassw0rd!", "y")
	require.NoError(t, err)
	_ = other
	rr, env := f.do("DELETE", "/apikeys/"+otherKey.ID, "")
	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.Equal(t, "key_not_found", env.Error.Code)
}

func TestUserKeyAPI_RevokedKeyIs409(t *testing.T) {
	f := newKeyHygieneFixture(t)
	k, err := f.s.auth.GenerateAPIKey(context.Background(), f.user.ID, "scoped", &auth.KeyCreateOptions{Permissions: []string{auth.OpGetObject}})
	require.NoError(t, err)

	rr, _ := f.do("DELETE", "/apikeys/"+k.ID, "")
	require.Equal(t, http.StatusNoContent, rr.Code)

	rr, env := f.do("DELETE", "/apikeys/"+k.ID, "")
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, ErrTypeConflict, env.Error.Type)
	assert.Equal(t, "key_revoked", env.Error.Code)

	rr, env = f.do("POST", "/apikeys/"+k.ID+"/rotate", "")
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "key_revoked", env.Error.Code)

	rr, env = f.do("POST", "/apikeys/"+k.ID+"/expire", `{"days":7}`)
	assert.Equal(t, http.StatusConflict, rr.Code, "a revoked key gets no new expiry")
	assert.Equal(t, "key_revoked", env.Error.Code)

	// The primary pair: 409 primary_key, rotate instead.
	rr, env = f.do("DELETE", "/apikeys/"+f.key.ID, "")
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "primary_key", env.Error.Code)
}

func TestUserKeyAPI_DaysMustBeAtLeastOne(t *testing.T) {
	f := newKeyHygieneFixture(t)
	for _, body := range []string{`{"days":0}`, `{"days":-3}`, `{}`} {
		rr, env := f.do("POST", "/apikeys/"+f.key.ID+"/expire", body)
		assert.Equal(t, http.StatusBadRequest, rr.Code, body)
		assert.Equal(t, "invalid_days", env.Error.Code)
		assert.Equal(t, "days", env.Error.Param)
	}
	rr, env := f.do("POST", "/apikeys/"+f.key.ID+"/expire", `{"days":`)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Equal(t, "invalid_json", env.Error.Code)

	// Create: expiry_days present and below 1 is refused; omitted = no expiry.
	for _, body := range []string{`{"name":"x","expiry_days":0}`, `{"name":"x","expiry_days":-1}`} {
		rr, env := f.do("POST", "/apikeys", body)
		assert.Equal(t, http.StatusBadRequest, rr.Code, body)
		assert.Equal(t, "invalid_expiry_days", env.Error.Code)
		assert.Equal(t, "expiry_days", env.Error.Param)
	}
	rr, _ = f.do("POST", "/apikeys", `{"name":"forever"}`)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var created map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &created))
	assert.Nil(t, created["expires_at"])

	// days ≥ 1 on a scoped key is accepted (the primary never takes an
	// expiry — TestUserKeyAPI_PrimaryCannotBeExpired).
	rr, _ = f.do("POST", "/apikeys/"+created["id"].(string)+"/expire", `{"days":1}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	exp, err := time.Parse(time.RFC3339, resp["expires_at"])
	require.NoError(t, err)
	assert.True(t, exp.After(time.Now().Add(23*time.Hour)))

	rr, env = f.do("POST", "/apikeys", `{"name":"x","permissions":["Bogus"]}`)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Equal(t, "invalid_permissions", env.Error.Code)
	rr, env = f.do("POST", "/apikeys", `not json`)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Equal(t, "invalid_json", env.Error.Code)
}

func TestKeyListings_ShowRevokedAndPrimary(t *testing.T) {
	f := newKeyHygieneFixture(t)
	k, err := f.s.auth.GenerateAPIKey(context.Background(), f.user.ID, "to-revoke", nil)
	require.NoError(t, err)
	require.NoError(t, f.s.auth.RevokeAPIKey(context.Background(), f.user.ID, k.ID))

	// Management listing: revoked_at and is_primary on every item.
	rr, _ := f.do("GET", "/keys", "")
	require.Equal(t, http.StatusOK, rr.Code)
	var list struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &list))
	byID := map[string]map[string]any{}
	for _, item := range list.Data {
		_, hasRevoked := item["revoked_at"]
		assert.True(t, hasRevoked, "revoked_at is present on every item (null for a live key)")
		byID[item["id"].(string)] = item
	}
	require.Contains(t, byID, k.ID)
	assert.NotNil(t, byID[k.ID]["revoked_at"], "a revoked key is shown revoked")
	assert.Equal(t, false, byID[k.ID]["is_primary"])
	require.Contains(t, byID, f.key.ID)
	assert.Nil(t, byID[f.key.ID]["revoked_at"])
	assert.Equal(t, true, byID[f.key.ID]["is_primary"])

	// User listing: still a plain array, revoked_at on the revoked row.
	rr, _ = f.do("GET", "/apikeys", "")
	require.Equal(t, http.StatusOK, rr.Code)
	var keys []map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &keys))
	var found bool
	for _, item := range keys {
		if item["id"] == k.ID {
			found = true
			assert.NotNil(t, item["revoked_at"])
		}
		assert.Nil(t, item["secret"], "no secret after creation")
	}
	assert.True(t, found)
}

func TestUserKeyAPI_ListIsAnArrayForAUserWithNoKeys(t *testing.T) {
	// ListAPIKeys returns a nil slice for an unknown user; the wire shape is
	// still an array, never `null`.
	s := &Server{logger: zap.NewNop(), auth: auth.NewAuthService(nil, nil), config: &config.Config{Server: config.ServerConfig{Port: 8000}}}
	req := httptest.NewRequest("GET", "/apikeys", nil)
	req = req.WithContext(context.WithValue(req.Context(), userIDKey, "nobody"))
	rr := httptest.NewRecorder()
	s.handleListUserAPIKeys(rr, req)
	assert.Equal(t, "[]\n", rr.Body.String())
}

// Post-merge review of #584: the primary pair could be expired through this
// route (409 primary_key now, the revoke shape), and an expired key could
// be rotated into a successor born expired (409 key_expired).
func TestUserKeyAPI_PrimaryCannotBeExpired(t *testing.T) {
	f := newKeyHygieneFixture(t)

	rr, env := f.do("POST", "/apikeys/"+f.key.ID+"/expire", `{"days":30}`)
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Equal(t, ErrTypeConflict, env.Error.Type)
	assert.Equal(t, "primary_key", env.Error.Code)
	assert.Equal(t, "keyId", env.Error.Param)
	assert.Contains(t, env.Error.Message, "rotate")

	// Rotating first changes nothing: the successor is the primary.
	rr, _ = f.do("POST", "/apikeys/"+f.key.ID+"/rotate", "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var rotated struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &rotated))
	rr, env = f.do("POST", "/apikeys/"+rotated.ID+"/expire", `{"days":30}`)
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Equal(t, "primary_key", env.Error.Code)

	keys, err := f.s.auth.ListAPIKeys(context.Background(), f.user.ID)
	require.NoError(t, err)
	for _, k := range keys {
		if k.IsPrimary {
			assert.Nil(t, k.ExpiresAt, "no primary row carries an expiry")
		}
	}
}

func TestUserKeyAPI_ExpiredKeyIsNotRotated(t *testing.T) {
	f := newKeyHygieneFixture(t)
	k, err := f.s.auth.GenerateAPIKey(context.Background(), f.user.ID, "scoped", &auth.KeyCreateOptions{Permissions: []string{auth.OpGetObject}})
	require.NoError(t, err)
	require.NoError(t, f.s.auth.SetAPIKeyExpiration(context.Background(), f.user.ID, k.ID, time.Now().Add(-time.Minute)))

	rr, env := f.do("POST", "/apikeys/"+k.ID+"/rotate", "")
	assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	assert.Equal(t, ErrTypeConflict, env.Error.Type)
	assert.Equal(t, "key_expired", env.Error.Code)
	assert.Equal(t, "keyId", env.Error.Param)
}

// Post-merge review of #584: an allowlist of 0.0.0.0/0 or ::/0 is refused
// with its own code on the management API (the one JSON route that takes
// an allowlist) — hidden among valid entries too.
func TestMgmtKeyAPI_UnrestrictedAllowlistIs400(t *testing.T) {
	f := newKeyHygieneFixture(t)
	f.r.Post("/keys", f.s.handleMgmtCreateKey)
	for _, body := range []string{
		`{"name":"open","ip_allowlist":["0.0.0.0/0"]}`,
		`{"name":"open","ip_allowlist":["::/0"]}`,
		`{"name":"open","ip_allowlist":["203.0.113.7","10.0.0.0/8","0.0.0.0/0"]}`,
	} {
		rr, env := f.do("POST", "/keys", body)
		assert.Equal(t, http.StatusBadRequest, rr.Code, body)
		assert.Equal(t, ErrTypeInvalidRequest, env.Error.Type)
		assert.Equal(t, "unrestricted_ip_allowlist", env.Error.Code, body)
		assert.Equal(t, "ip_allowlist", env.Error.Param)
		assert.Contains(t, env.Error.Message, "empty")
	}
	keys, err := f.s.auth.ListAPIKeys(context.Background(), f.user.ID)
	require.NoError(t, err)
	assert.Len(t, keys, 1, "no key was created")
}
