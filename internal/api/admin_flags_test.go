package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const testAdminEmail = "flags-admin@test.local"

// adminFlagsFixture mounts the production flag routes behind requireAdmin
// with a header-selectable authenticated user, against the real database.
type adminFlagsFixture struct {
	server  *Server
	svc     *flags.Service
	db      *sql.DB
	key     string
	adminID string
	userID  string
}

func setupAdminFlagsFixture(t *testing.T) *adminFlagsFixture {
	t.Helper()
	db := openFlagsTestDB(t)

	// Two real users so requireAdmin's role query runs for real.
	adminID := uuid.New().String()
	userID := uuid.New().String()
	for id, role := range map[string]string{adminID: "admin", userID: "user"} {
		_, err := db.Exec(`
			INSERT INTO users (id, email, password_hash, role)
			VALUES ($1, $2, 'x', $3)`,
			id, fmt.Sprintf("flags-%s-%d@test.local", role, time.Now().UnixNano()), role)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM users WHERE id IN ($1, $2)", adminID, userID)
	})

	key := fmt.Sprintf("test-adminapi-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM feature_flags WHERE flag_key = $1", key)
	})

	svc := flags.New(db, zap.NewNop())
	svc.Register(key, false)
	require.NoError(t, svc.Refresh(context.Background()))

	s := &Server{
		logger: zap.NewNop(),
		router: chi.NewRouter(),
		db:     db,
		flags:  svc,
		config: &config.Config{Server: config.ServerConfig{Port: 8000}},
	}

	s.router.Route("/api/v1/admin", func(r chi.Router) {
		// Stand-in for requireJWT: the caller picks the user via header.
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx := context.WithValue(req.Context(), userIDKey, req.Header.Get("X-Test-User"))
				ctx = context.WithValue(ctx, emailKey, testAdminEmail)
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		r.Get("/flags", s.requireAdmin(s.handleAdminFlagsList))
		r.Put("/flags/{key}", s.requireAdmin(s.handleAdminFlagSet))
		r.Delete("/flags/{key}", s.requireAdmin(s.handleAdminFlagUnset))
	})

	return &adminFlagsFixture{server: s, svc: svc, db: db, key: key, adminID: adminID, userID: userID}
}

func (f *adminFlagsFixture) do(t *testing.T, asUser, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", asUser)
	rec := httptest.NewRecorder()
	f.server.router.ServeHTTP(rec, req)
	return rec
}

func TestAdminFlags_NonAdminForbidden(t *testing.T) {
	f := setupAdminFlagsFixture(t)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/v1/admin/flags"},
		{"PUT", "/api/v1/admin/flags/" + f.key},
		{"DELETE", "/api/v1/admin/flags/" + f.key},
	} {
		rec := f.do(t, f.userID, tc.method, tc.path, map[string]any{"enabled": true})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s must be admin-only", tc.method, tc.path)
	}
}

func TestAdminFlags_ListResolved(t *testing.T) {
	f := setupAdminFlagsFixture(t)

	rec := f.do(t, f.adminID, "GET", "/api/v1/admin/flags", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")

	var resp struct {
		Flags []flags.Flag `json:"flags"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	var found bool
	for _, fl := range resp.Flags {
		if fl.Key == f.key {
			found = true
			assert.False(t, fl.Default)
			assert.True(t, fl.Registered)
		}
	}
	assert.True(t, found, "resolved list must include the registered flag")
}

func TestAdminFlags_GlobalSetAndUnset(t *testing.T) {
	f := setupAdminFlagsFixture(t)
	ctx := context.Background()

	// Set the global row on.
	rec := f.do(t, f.adminID, "PUT", "/api/v1/admin/flags/"+f.key, map[string]any{"enabled": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, f.svc.Enabled(f.key, "any-tenant"), "flip must be visible immediately")

	// updated_by is taken from the JWT, not the request body.
	var updatedBy string
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT COALESCE(updated_by, '') FROM feature_flags
		WHERE flag_key = $1 AND tenant_id = '*'`, f.key).Scan(&updatedBy))
	assert.Equal(t, testAdminEmail, updatedBy)

	// Unset reverts to the registered default (false).
	rec = f.do(t, f.adminID, "DELETE", "/api/v1/admin/flags/"+f.key, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, f.svc.Enabled(f.key, "any-tenant"))
}

func TestAdminFlags_TenantOverrideCRUD(t *testing.T) {
	f := setupAdminFlagsFixture(t)

	// Global on…
	rec := f.do(t, f.adminID, "PUT", "/api/v1/admin/flags/"+f.key, map[string]any{"enabled": true})
	require.Equal(t, http.StatusOK, rec.Code)

	// …tenant override off.
	rec = f.do(t, f.adminID, "PUT", "/api/v1/admin/flags/"+f.key,
		map[string]any{"enabled": false, "tenant_id": "tenant-crud"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, f.svc.Enabled(f.key, "tenant-crud"))
	assert.True(t, f.svc.Enabled(f.key, "tenant-other"))

	// The list shows the override.
	rec = f.do(t, f.adminID, "GET", "/api/v1/admin/flags", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Flags []flags.Flag `json:"flags"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	for _, fl := range resp.Flags {
		if fl.Key == f.key {
			require.Len(t, fl.Overrides, 1)
			assert.Equal(t, "tenant-crud", fl.Overrides[0].TenantID)
			assert.False(t, fl.Overrides[0].Enabled)
		}
	}

	// DELETE the override → tenant follows the global row again.
	rec = f.do(t, f.adminID, "DELETE",
		"/api/v1/admin/flags/"+f.key+"?tenant_id=tenant-crud", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, f.svc.Enabled(f.key, "tenant-crud"))
}

func TestAdminFlags_Validation(t *testing.T) {
	f := setupAdminFlagsFixture(t)

	// Unknown (unregistered) key: a typo must fail loudly, not no-op.
	rec := f.do(t, f.adminID, "PUT", "/api/v1/admin/flags/definitely-not-a-flag",
		map[string]any{"enabled": true})
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Malformed body.
	req := httptest.NewRequest("PUT", "/api/v1/admin/flags/"+f.key, bytes.NewReader([]byte("{nope")))
	req.Header.Set("X-Test-User", f.adminID)
	rec = httptest.NewRecorder()
	f.server.router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
