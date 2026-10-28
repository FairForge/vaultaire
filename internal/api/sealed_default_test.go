package api

// WP-EDGE-4 PR C (Stage 1a Prompt 48): standard_sealed_default. Global
// only, default OFF, refused ON without a master key or while a tenant has
// no key version (409, the credential_plaintext_drop guard's shape). ON =
// every Standard-mode write is placed sealed: a sub-threshold body whole
// and SSE-S3 sealed, a chunked one chunk-encrypted, at the edge the codec
// for every tenant; a write that cannot be sealed is 503, never plaintext.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/flags"
)

func adminFlagSet(t *testing.T, s *Server, key string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/api/v1/admin/flags/"+key, strings.NewReader(body))
	rc := chi.NewRouteContext()
	rc.URLParams.Add("key", key)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rc))
	rr := httptest.NewRecorder()
	s.handleAdminFlagSet(rr, req)
	return rr
}

func TestStandardSealedDefault_RefusedWithoutKeyOrWithTenantsWithoutKeys(t *testing.T) {
	f := setupLifecycleFixture(t)
	ctx := context.Background()
	fl := flags.New(f.db, zap.NewNop())
	registerFlags(fl)
	f.srv.useFlags(fl)
	f.srv.credentialScope = f.tenant.ID
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM feature_flags WHERE flag_key = $1`, flagStandardSealedDefault) })
	globalRow := func() bool {
		var n int
		require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM feature_flags WHERE flag_key = $1`, flagStandardSealedDefault).Scan(&n))
		return n > 0
	}

	// No master key: 409 master_key_absent, nothing written.
	rr := adminFlagSet(t, f.srv, flagStandardSealedDefault, `{"enabled":true}`)
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "master_key_absent", body["error"])
	assert.False(t, globalRow())

	// A master key and a tenant without a key version: 409 naming the count.
	f.srv.tenantKeys = testTenantKeys(t, f.db, keyTestMasterA)
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM tenant_key_versions WHERE tenant_id = $1`, f.tenant.ID) })
	rr = adminFlagSet(t, f.srv, flagStandardSealedDefault, `{"enabled":true}`)
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "tenant_keys_missing", body["error"])
	assert.Equal(t, float64(1), body["count"])
	assert.False(t, globalRow())

	// A tenant row: 400 flag_global_only.
	rr = adminFlagSet(t, f.srv, flagStandardSealedDefault, fmt.Sprintf(`{"enabled":true,"tenant_id":%q}`, f.tenant.ID))
	assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.ErrorIs(t, fl.Set(ctx, flagStandardSealedDefault, f.tenant.ID, true, "test"), flags.ErrGlobalOnly)

	// Every tenant has a version: the guard passes (asked directly: a global
	// row on the shared database would reach every package's servers).
	_, err := f.srv.tenantKeys.Current(ctx, f.tenant.ID)
	require.NoError(t, err)
	assert.NoError(t, f.srv.guardStandardSealedDefault(ctx, flags.Change{Key: flagStandardSealedDefault, TenantID: flags.GlobalTenant, Enabled: true}))
	// Off and clear are never refused.
	f.srv.tenantKeys = nil
	assert.NoError(t, f.srv.guardStandardSealedDefault(ctx, flags.Change{Key: flagStandardSealedDefault, TenantID: flags.GlobalTenant, Enabled: false}))
	assert.NoError(t, f.srv.guardStandardSealedDefault(ctx, flags.Change{Key: flagStandardSealedDefault, TenantID: flags.GlobalTenant, Clear: true}))
}

// sealedDefaultFlags is an in-memory flags service with
// standard_sealed_default on (a global row on the shared database would
// reach every package's servers) and the edge flags the fixture needs.
func sealedDefaultFlags(on bool) *flags.Service {
	fl := flags.New(nil, zap.NewNop())
	registerFlags(fl)
	fl.Register(flagStandardSealedDefault, on)
	fl.Register(flagChunking, true)
	fl.Register(flagEdgeIngest, true)
	return fl
}

// plaintextOnBackend reports whether any blob the fake backend holds
// contains the first 64 bytes of body.
func (f *edgeFixture) plaintextOnBackend(body []byte) bool {
	f.drv.mu.Lock()
	defer f.drv.mu.Unlock()
	for _, b := range f.drv.objects {
		if bytes.Contains(b, body[:64]) {
			return true
		}
	}
	return false
}

func TestStandardSealedDefault_EveryStandardWriteIsSealed(t *testing.T) {
	f := setupCodecFixture(t)
	f.srv.flags = sealedDefaultFlags(true)
	f.srv.chunkingThreshold = 4 << 20 // 2 MiB is a whole object, 12 MiB chunked
	sizes := []int{2 << 20, 12 << 20}
	if !testing.Short() {
		sizes = append(sizes, 200<<20)
	}
	for i, n := range sizes {
		body := crypto.GoldenBody(uint64(4900+i), n)
		key := fmt.Sprintf("sealed-%d.bin", n)
		w := f.s3(http.MethodPut, "/"+f.bucket+"/"+key, body, nil)
		require.Equal(t, http.StatusOK, w.Code, "%d: %s", n, w.Body.String())
		var alg string
		require.NoError(t, f.db.QueryRow(`SELECT COALESCE(encryption_algorithm, '') FROM object_head_cache WHERE tenant_id = $1 AND object_key = $2`,
			f.tenantID, key).Scan(&alg))
		assert.NotEmpty(t, alg, "%d bytes: the row is sealed", n)
		assert.False(t, f.plaintextOnBackend(body), "%d bytes: no plaintext on the backend", n)
		g := f.originGet(key, nil)
		require.Equal(t, http.StatusOK, g.Code)
		assert.True(t, bytes.Equal(body, g.Body.Bytes()), "%d bytes read back", n)
	}

	// OFF: the same plain PUT stays as it was (unsealed, no key asked).
	f.srv.flags = sealedDefaultFlags(false)
	body := crypto.GoldenBody(4999, 2<<20)
	w := f.s3(http.MethodPut, "/"+f.bucket+"/plain.bin", body, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, f.plaintextOnBackend(body), "the flag off changes nothing")
}

// A write that cannot be sealed (no master key on this process while the
// flag is on) is 503, nothing stored — never plaintext.
func TestStandardSealedDefault_AWriteThatCannotBeSealedIs503(t *testing.T) {
	f := setupEdgeFixture(t)
	f.srv.flags = sealedDefaultFlags(true)
	body := testBytes(3 << 20)
	w := f.s3(http.MethodPut, "/"+f.bucket+"/nokey.bin", body, nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "30", w.Header().Get("Retry-After"))
	_, ok := f.head("nokey.bin")
	assert.False(t, ok)
	assert.False(t, f.plaintextOnBackend(body))
}

// At the edge the flag implies the codec for every tenant: an edge write
// of a tenant without edge_codec is admitted codec (sealed by the host).
func TestStandardSealedDefault_ImpliesTheEdgeCodec(t *testing.T) {
	f := setupCodecFixture(t)
	fl := sealedDefaultFlags(true)
	fl.Register(flagEdgeCodec, false)
	f.srv.flags = fl
	body := crypto.GoldenBody(4950, 6<<20)
	resp := f.admit(f.signed("/"+f.bucket+"/codec-implied.bin", nil, body, nil, ""))
	require.True(t, resp.Edge, "%+v", resp)
	assert.NotNil(t, resp.Codec, "the codec, though the tenant's edge_codec is off")
	assert.Empty(t, resp.URL, "never a plaintext body link")
}

// docs/BACKEND_ENCRYPTION.md says whether Standard is sealed by default:
// the cell must be the flag's default.
func TestBackendEncryptionDocMatchesFlagDefault(t *testing.T) {
	doc, err := os.ReadFile("../../docs/BACKEND_ENCRYPTION.md")
	require.NoError(t, err)
	m := regexp.MustCompile(`Sealed by default: \*\*(yes|no)\*\*`).FindSubmatch(doc)
	require.NotNil(t, m, "the doc states `Sealed by default: **yes|no**`")
	fl := flags.New(nil, zap.NewNop())
	registerFlags(fl)
	want := "no"
	if fl.Enabled(flagStandardSealedDefault, "") {
		want = "yes"
	}
	assert.Equal(t, want, string(m[1]), "the doc's cell is the flag's default")
}
