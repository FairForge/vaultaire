package api

// WP-EDGE-3 step 6: the data-plane host registry's admin routes — GET, PUT
// and DELETE /api/v1/admin/data-plane-hosts/{host} (admin JWT, audited
// data_plane.host_set / data_plane.host_deleted). A node's token is minted
// on its first PUT, stored as its sha256 and answered ONCE.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupHostRegistryFixture(t *testing.T) (*planFixture, string) {
	t.Helper()
	f := setupPlanFixture(t)
	f.server.router = chi.NewRouter()
	f.server.router.Route("/api/v1/admin", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx := context.WithValue(req.Context(), userIDKey, req.Header.Get("X-Test-User"))
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		f.server.registerAdminDataPlaneHostRoutes(r)
	})
	host := "node-" + uuid.New().String()[:8] + ".lab.test"
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM data_plane_hosts WHERE host = $1`, host)
		_, _ = f.db.Exec(`DELETE FROM audit_logs WHERE action LIKE 'data_plane.%' AND resource = $1`, "data_plane_host:"+host)
	})
	return f, host
}

func TestDataPlaneHosts_PutGetDeleteAndTheTokenShownOnce(t *testing.T) {
	f, host := setupHostRegistryFixture(t)
	path := "/api/v1/admin/data-plane-hosts/" + host

	w, out := f.call(t, f.adminID, http.MethodPut, path, map[string]any{"kind": "node", "address": "198.51.100.7", "weight": 1, "health": "up"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	token, _ := out["token"].(string)
	require.Len(t, token, 64, "a 32-byte token, hex, answered once")
	var hash string
	require.NoError(t, f.db.QueryRow(`SELECT token_hash FROM data_plane_hosts WHERE host = $1`, host).Scan(&hash))
	sum := sha256.Sum256([]byte(token))
	assert.Equal(t, hex.EncodeToString(sum[:]), hash, "only its sha256 is stored")
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action = 'data_plane.host_set' AND resource = $1`, "data_plane_host:"+host).Scan(&n))
	assert.Equal(t, 1, n, "audited")

	// A second PUT changes the row and never answers the token again.
	w, out = f.call(t, f.adminID, http.MethodPut, path, map[string]any{"kind": "node", "address": "198.51.100.8", "weight": 2, "health": "draining"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, has := out["token"]
	assert.False(t, has, "the token is shown once")
	require.NoError(t, f.db.QueryRow(`SELECT token_hash FROM data_plane_hosts WHERE host = $1`, host).Scan(&hash))
	assert.Equal(t, hex.EncodeToString(sum[:]), hash, "the token stands")

	// GET answers the row, never the token or its hash.
	w, out = f.call(t, f.adminID, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "198.51.100.8", out["address"])
	assert.Equal(t, "draining", out["health"])
	assert.NotContains(t, w.Body.String(), hash)
	assert.NotContains(t, w.Body.String(), "token")

	// DELETE removes it, audited.
	w, _ = f.call(t, f.adminID, http.MethodDelete, path, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM data_plane_hosts WHERE host = $1`, host).Scan(&n))
	assert.Zero(t, n)
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action = 'data_plane.host_deleted' AND resource = $1`, "data_plane_host:"+host).Scan(&n))
	assert.Equal(t, 1, n)
	w, _ = f.call(t, f.adminID, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDataPlaneHosts_Validation(t *testing.T) {
	f, host := setupHostRegistryFixture(t)
	path := "/api/v1/admin/data-plane-hosts/" + host
	for name, body := range map[string]map[string]any{
		"bad kind":            {"kind": "proxy", "address": "198.51.100.7"},
		"node without an IP":  {"kind": "node"},
		"address not an IP":   {"kind": "node", "address": "node.example"},
		"bad health":          {"kind": "node", "address": "198.51.100.7", "health": "sick"},
		"negative weight":     {"kind": "node", "address": "198.51.100.7", "weight": -1},
		"edge with a tenant":  {"kind": "edge", "tenant_id": "t-x"},
		"slc with an address": {"kind": "slc", "address": "198.51.100.7"},
	} {
		w, _ := f.call(t, f.adminID, http.MethodPut, path, body)
		assert.Equal(t, http.StatusBadRequest, w.Code, "%s: %s", name, w.Body.String())
	}
	// RP74-7: nothing reads a weight on the Worker's or this box's row yet.
	for _, kind := range []string{"edge", "slc"} {
		w, _ := f.call(t, f.adminID, http.MethodPut, path, map[string]any{"kind": kind, "weight": 1})
		assert.Equal(t, http.StatusBadRequest, w.Code, kind)
		assert.Contains(t, w.Body.String(), "weight_unused", kind)
	}
	// A tenant-scoped host names a tenant that exists.
	w, _ := f.call(t, f.adminID, http.MethodPut, path, map[string]any{"kind": "node", "address": "198.51.100.7", "tenant_id": "no-such-tenant-" + uuid.New().String()[:6]})
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	// A user who is not an admin: 403.
	w, _ = f.call(t, f.userID, http.MethodPut, path, map[string]any{"kind": "node", "address": "198.51.100.7"})
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// D-EDGE-34: the GET of a tenant-scoped (BYO) host's row is an admin read
// of that tenant (its entry appended before the handler runs); a platform
// host's row names no customer — no entry.
func TestDataPlaneHosts_GetOfATenantScopedHostIsAnAdminRead(t *testing.T) {
	f, host := setupHostRegistryFixture(t)
	tenantID := "dph-" + uuid.New().String()[:8]
	_, err := f.db.Exec(`INSERT INTO tenants (id, name, email) VALUES ($1, 'DPH', $2)`, tenantID, tenantID+"@test.local")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM tenants WHERE id = $1`, tenantID) })
	type read struct{ scope, tenant string }
	var reads []read
	f.server.adminReadHook = func(_ context.Context, _, scope, tenant string) error {
		reads = append(reads, read{scope, tenant})
		return nil
	}
	path := "/api/v1/admin/data-plane-hosts/" + host
	w, _ := f.call(t, f.adminID, http.MethodPut, path, map[string]any{"kind": "node", "address": "198.51.100.9"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w, _ = f.call(t, f.adminID, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, reads, "a platform host's row names no customer")

	w, _ = f.call(t, f.adminID, http.MethodPut, path, map[string]any{"kind": "node", "address": "198.51.100.9", "tenant_id": tenantID})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w, _ = f.call(t, f.adminID, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []read{{"tenant_detail", tenantID}}, reads, "the tenant's record, read")
}
