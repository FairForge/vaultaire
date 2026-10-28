package main

// WP-EDGE-3 step 4: with VAULTAIRE_ROLE=node the binary is a data node —
// no database, no engine, no jobs, and only the data-plane routes.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/node"
)

func TestNodeRole_TheRouterIsTheDataPlaneOnly(t *testing.T) {
	t.Setenv("VAULTAIRE_ROLE", "node")
	t.Setenv("NODE_CONTROL_PLANE_URL", "http://127.0.0.1:1")
	t.Setenv("NODE_TOKEN", strings.Repeat("x", 32))
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	t.Setenv("NODE_TOKEN_FILE", "")
	t.Setenv("NODE_BIND", "127.0.0.1")
	assert.True(t, isNodeRole(os.Getenv))

	n, err := node.Build(os.Getenv, os.ReadFile, "test-sha", zap.NewNop())
	require.NoError(t, err)
	var routes []string
	require.NoError(t, chi.Walk(n.Router(), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, method+" "+route)
		return nil
	}))
	sort.Strings(routes)
	assert.Equal(t, []string{"GET /health", "GET /{bucket}/*", "PUT /{bucket}/*"}, routes,
		"PutObject/UploadPart, GetObject, /health — nothing else (/metrics is on NODE_METRICS_ADDR, RP74-10)")

	for _, p := range []string{"/api/v1/admin/flags", "/api/v1/edge/admit", "/api/v1/manage/buckets", "/dashboard", "/dashboard/billing", "/translog/checkpoint", "/translog/tile/0/000"} {
		for _, signed := range []bool{false, true} {
			r := httptest.NewRequest(http.MethodGet, p, nil)
			if signed {
				r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIA/20261027/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00")
			}
			w := httptest.NewRecorder()
			n.Router().ServeHTTP(w, r)
			assert.Equal(t, http.StatusNotFound, w.Code, "%s (signed %v) is not the node's", p, signed)
		}
	}
	w := httptest.NewRecorder()
	n.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	assert.Equal(t, http.StatusOK, w.Code)

	// The origin's landing page is not the node's; a SIGNED "/" is S3's
	// ListBuckets — handed to the origin untouched (unreachable here: the
	// node's SlowDown).
	w = httptest.NewRecorder()
	n.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusNotFound, w.Code)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIA/20261027/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00")
	w = httptest.NewRecorder()
	n.Router().ServeHTTP(w, r)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "proxied to the origin")
	assert.Contains(t, w.Body.String(), "SlowDown")
}

// The node refuses to boot without its control plane or with a token that
// is no token; what the role ignores (a database, a storage mode) is named.
func TestNodeRole_Config(t *testing.T) {
	env := map[string]string{"NODE_CONTROL_PLANE_URL": "https://stored.ge", "NODE_TOKEN": strings.Repeat("t", 32),
		"STORAGE_MODE": "idrive", "DB_HOST": "db.internal", "NODE_BIND": "127.0.0.1"}
	get := func(k string) string { return env[k] }
	cfg, ignored, err := node.ConfigFromEnv(get, os.ReadFile)
	require.NoError(t, err)
	assert.Equal(t, "https://stored.ge", cfg.ControlPlaneURL)
	assert.Equal(t, "https://stored.ge", cfg.OriginURL, "pass-throughs go to the control plane's origin by default")
	assert.ElementsMatch(t, []string{"STORAGE_MODE", "DB_HOST"}, ignored)

	env["NODE_TOKEN"] = "short"
	_, _, err = node.ConfigFromEnv(get, os.ReadFile)
	assert.Error(t, err)
	env["NODE_TOKEN"] = strings.Repeat("t", 32)
	delete(env, "NODE_CONTROL_PLANE_URL")
	_, _, err = node.ConfigFromEnv(get, os.ReadFile)
	assert.Error(t, err)
	env["NODE_CONTROL_PLANE_URL"] = "ftp://x"
	_, _, err = node.ConfigFromEnv(get, os.ReadFile)
	assert.Error(t, err)
}
