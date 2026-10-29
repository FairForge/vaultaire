package api

// WP-PLAT-1 PR C: a platform mints, lists and revokes scoped keys — and
// STS tokens — for its children. A child has no primary pair; a child's
// key never exceeds the scope of the credential that minted it.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/auth"
)

// makeChild creates a child through the API and returns its id.
func (f *platformFixture) makeChild(label string) string {
	f.t.Helper()
	code, out := f.call(http.MethodPost, "/api/v1/platform/tenants", f.jwt, map[string]any{"label": label})
	require.Equal(f.t, http.StatusCreated, code, out)
	return out["id"].(string)
}

// callScoped calls a platform handler as a caller whose credential carries
// scope (a scoped key's — the JWT's scope is the account's, unscoped).
func (f *platformFixture) callScoped(h http.HandlerFunc, method, path string, params map[string]string, scope *auth.KeyScope, body any) (int, map[string]any) {
	f.t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	ctx := context.WithValue(req.Context(), tenantIDKey, f.user.TenantID)
	ctx = context.WithValue(ctx, userIDKey, f.user.ID)
	if scope != nil {
		ctx = auth.WithKeyScope(ctx, scope)
	}
	rc := chi.NewRouteContext()
	for k, v := range params {
		rc.URLParams.Add(k, v)
	}
	req = req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rc))
	w := httptest.NewRecorder()
	h(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// The calling credential scoped to GetObject on bucket a: a child key
// asking PutObject, or bucket b, or a wider IP range, or a later expiry, is
// 400 scope_exceeds_parent; within it, 201 — and what it did not ask is
// the caller's (narrowest) scope.
func TestPlatform_ChildKeysNeverExceedTheParentsScope(t *testing.T) {
	f := setupPlatformFixture(t)
	f.grant()
	child := f.makeChild("scoped")
	exp := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	caller := &auth.KeyScope{Permissions: []string{"GetObject"}, BucketScope: []string{"a"}, IPAllowlist: []string{"10.0.0.0/8"}, ExpiresAt: &exp}
	p := map[string]string{"id": child}
	path := "/api/v1/platform/tenants/" + child + "/keys"
	for _, bad := range []map[string]any{
		{"name": "k", "permissions": []string{"PutObject"}},
		{"name": "k", "permissions": []string{"*"}},
		{"name": "k", "permissions": []string{"GetObject"}, "bucket_scope": []string{"b"}},
		{"name": "k", "permissions": []string{"GetObject"}, "ip_allowlist": []string{"192.168.1.1"}},
		{"name": "k", "permissions": []string{"GetObject"}, "ip_allowlist": []string{"10.0.0.0/7"}},
		{"name": "k", "permissions": []string{"GetObject"}, "expires_at": exp.Add(time.Hour)},
		{"name": "k", "permissions": []string{"GetObject", "BypassGovernanceRetention"}},
	} {
		code, out := f.callScoped(f.s.handlePlatformCreateKey, http.MethodPost, path, p, caller, bad)
		assert.Equal(t, http.StatusBadRequest, code, "%v → %v", bad, out)
		assert.Equal(t, "scope_exceeds_parent", errCode(out), "%v", bad)
	}
	code, out := f.callScoped(f.s.handlePlatformCreateKey, http.MethodPost, path, p, caller,
		map[string]any{"name": "ok", "ip_allowlist": []string{"10.1.2.3"}})
	require.Equal(t, http.StatusCreated, code, out)
	assert.Equal(t, []any{"GetObject"}, out["permissions"], "the caller's permissions when none asked")
	assert.Equal(t, []any{"a"}, out["bucket_scope"], "the caller's buckets when none asked")
	assert.NotEmpty(t, out["secret"])
	assert.Equal(t, false, out["is_primary"])
	exp2, _ := time.Parse(time.RFC3339, out["expires_at"].(string))
	assert.True(t, exp2.Equal(exp), "never outlives the caller's credential")

	// The key resolves to the CHILD, scoped as minted.
	cred, err := f.s.s3Auth().LookupCredential(context.Background(), out["key"].(string))
	require.NoError(t, err)
	assert.Equal(t, child, cred.TenantID)
	assert.Equal(t, []string{"GetObject"}, cred.Scope.Permissions)

	// The JWT (the account's own authority) mints any valid scope.
	code, out = f.call(http.MethodPost, path, f.jwt, map[string]any{"name": "full", "permissions": []string{"PutObject", "GetObject"}})
	require.Equal(t, http.StatusCreated, code, out)
	keyRowID := out["id"].(string)

	// Listed under the child, never among the platform user's own keys.
	code, out = f.call(http.MethodGet, path, f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.Len(t, out["data"].([]any), 2)
	for _, k := range out["data"].([]any) {
		assert.NotContains(t, k.(map[string]any), "secret")
	}
	own, err := f.s.auth.ListAPIKeys(context.Background(), f.user.ID)
	require.NoError(t, err)
	for _, k := range own {
		assert.NotEqual(t, child, k.TenantID, "a child's key is not the platform user's")
	}

	// Revoked by the platform: the key stops resolving.
	code, out = f.call(http.MethodDelete, path+"/"+keyRowID, f.jwt, nil)
	require.Equal(t, http.StatusOK, code, out)
	var revoked bool
	require.NoError(t, f.db.QueryRow(`SELECT revoked_at IS NOT NULL FROM api_keys WHERE id = $1`, keyRowID).Scan(&revoked))
	assert.True(t, revoked)
	code, _ = f.call(http.MethodDelete, path+"/"+keyRowID, f.jwt, nil)
	assert.Equal(t, http.StatusConflict, code, "already revoked")

	// Another platform cannot mint for this child.
	other := setupPlatformFixture(t)
	other.grant()
	code, _ = other.call(http.MethodPost, path, other.jwt, map[string]any{"name": "x"})
	assert.Equal(t, http.StatusNotFound, code)
}

// POST …/tenants/{id}/sts mints a temporary credential of the child,
// bounded by one of the child's keys (parent_key_id, required: a child has
// no primary) — the STS scope intersection reused.
func TestPlatform_ChildSTSBoundedByTheChildsKey(t *testing.T) {
	f := setupPlatformFixture(t)
	f.grant()
	child := f.makeChild("sts")
	path := "/api/v1/platform/tenants/" + child
	code, out := f.call(http.MethodPost, path+"/keys", f.jwt, map[string]any{"name": "ro", "permissions": []string{"GetObject"}})
	require.Equal(t, http.StatusCreated, code, out)
	parentKey := out["id"].(string)

	code, out = f.call(http.MethodPost, path+"/sts", f.jwt, map[string]any{"permissions": []string{"GetObject"}})
	assert.Equal(t, http.StatusBadRequest, code, "parent_key_id is required: %v", out)
	code, out = f.call(http.MethodPost, path+"/sts", f.jwt, map[string]any{"parent_key_id": parentKey, "permissions": []string{"PutObject"}})
	assert.Equal(t, http.StatusBadRequest, code, out)
	assert.Equal(t, "scope_error", errCode(out))
	code, out = f.call(http.MethodPost, path+"/sts", f.jwt, map[string]any{"parent_key_id": parentKey, "permissions": []string{"GetObject"}, "ttl": 600})
	require.Equal(t, http.StatusCreated, code, out)
	cred, err := f.s.s3Auth().LookupCredential(context.Background(), out["access_key"].(string))
	require.NoError(t, err)
	assert.Equal(t, child, cred.TenantID)
	assert.Equal(t, []string{"GetObject"}, cred.Scope.Permissions)
}
