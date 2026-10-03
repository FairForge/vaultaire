package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/docs"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestOpenAPIDriftGuard — B4 Tier-0 docs drift guard (5.15.6).
//
// The OpenAPI spec (internal/docs/openapi.go) covered 7 of ~40 operations
// and nothing noticed, because nothing tied the spec to the router. This
// test walks the ACTUAL registered chi routes and fails the build when:
//
//  1. an API route (management /api/v1/*, auth, or S3 sub-resource) exists
//     in code but is neither in the spec nor in the explicit
//     knownUndocumented list below, or
//  2. a knownUndocumented entry gets documented (stale allowlist), or
//  3. the spec documents a path that no longer maps to any registered route.
//
// The allowlist IS today's documented debt (B3 burns it down): shrinking it
// is progress, growing it is a conscious reviewed choice in this file —
// silent drift is the only thing that breaks the build.
func TestOpenAPIDriftGuard(t *testing.T) {
	// Full production router: NewServer with nil DB is the supported
	// degraded dev path and registers every route.
	s := NewServer(
		&config.Config{Server: config.ServerConfig{Port: 8000}},
		zap.NewNop(),
		engine.NewEngine(nil, zap.NewNop(), nil),
		nil,
		nil,
	)

	registered := map[string]bool{} // "METHOD /pattern"
	require.NoError(t, chi.Walk(s.GetRouter(),
		func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			route = strings.TrimSuffix(route, "/")
			if route == "" {
				route = "/"
			}
			registered[method+" "+route] = true
			return nil
		}))

	spec := docs.GenerateOpenAPISpec()
	specOps := map[string]bool{} // "METHOD /path"
	for path, item := range spec.Paths {
		for method, op := range map[string]any{
			"GET": item.Get, "PUT": item.Put, "POST": item.Post, "PATCH": item.Patch,
			"DELETE": item.Delete, "HEAD": item.Head, "OPTIONS": item.Options,
		} {
			if op != nil && !isNilPtr(op) {
				specOps[method+" "+path] = true
			}
		}
	}

	// --- Direction 1: every API route is documented or explicitly listed.
	var missing []string
	for key := range registered {
		if !isGuardedRoute(key) {
			continue
		}
		if specOps[key] || knownUndocumented[key] {
			continue
		}
		missing = append(missing, key)
	}
	sort.Strings(missing)
	assert.Empty(t, missing,
		"routes exist in code but not in the OpenAPI spec — document them in internal/docs/openapi.go or consciously add them to knownUndocumented in this file:\n%s",
		strings.Join(missing, "\n"))

	// --- Direction 2: the allowlist may not go stale.
	var stale []string
	for key := range knownUndocumented {
		if specOps[key] {
			stale = append(stale, key+" (now documented — remove from allowlist)")
		}
		if !registered[key] {
			stale = append(stale, key+" (route no longer exists — remove from allowlist)")
		}
	}
	sort.Strings(stale)
	assert.Empty(t, stale, "knownUndocumented entries out of date:\n%s", strings.Join(stale, "\n"))

	// --- Direction 3: every documented path maps to a live route. S3
	// object/bucket paths are served by the catch-all, which chi reports
	// as /* — accept that mapping.
	catchAll := registered["GET /*"] || registered["PUT /*"]
	var ghost []string
	for key := range specOps {
		if specOps[key] && !registered[key] {
			if isS3SpecPath(key) && catchAll {
				continue
			}
			ghost = append(ghost, key)
		}
	}
	sort.Strings(ghost)
	assert.Empty(t, ghost,
		"the OpenAPI spec documents paths that no registered route serves:\n%s",
		strings.Join(ghost, "\n"))
}

// isGuardedRoute selects the routes the drift guard covers: the JSON API
// surface (/api/* — management, user, admin, rbac, compliance) plus the
// auth endpoints. Dashboard HTML pages, static assets, health probes, and
// the S3 catch-all (documented via /{bucket} spec paths, walked in
// direction 3) are out of scope.
func isGuardedRoute(key string) bool {
	route := key[strings.Index(key, " ")+1:]
	return strings.HasPrefix(route, "/api/") || strings.HasPrefix(route, "/auth/")
}

// isS3SpecPath reports whether a spec path is part of the S3 wire protocol
// (served by the catch-all rather than a dedicated chi route).
func isS3SpecPath(key string) bool {
	route := key[strings.Index(key, " ")+1:]
	return route == "/" || strings.HasPrefix(route, "/{bucket}")
}

func isNilPtr(v any) bool {
	return fmt.Sprintf("%v", v) == "<nil>"
}

// knownUndocumented is the reviewed backlog of API routes that exist in
// code but are deliberately absent from the OpenAPI spec. Since Review R14
// (B3, OpenAPI expand) every JSON route under /api/v1, /auth and
// /api/waitlist is documented in internal/docs/openapi.go; what remains is
// exactly the /api/compliance/* group — admin-only GDPR scaffolding
// (SAR, consent, ROPA, breach, privacy controls) whose user-scoped
// handlers are dead-by-auth and whose breach/ROPA handlers duplicate the
// documented /api/v1/admin/breach* routes. It is slated for deletion under
// decision D-12 / WP-R11-5 and is not published until then. Do not add to
// this list casually: new endpoints ship documented.
var knownUndocumented = map[string]bool{
	"DELETE /api/compliance/consent/{purpose}":               true,
	"DELETE /api/compliance/ropa/activities/{id}":            true,
	"GET /api/compliance/activities":                         true,
	"GET /api/compliance/breach":                             true,
	"GET /api/compliance/breach/stats":                       true,
	"GET /api/compliance/breach/{id}":                        true,
	"GET /api/compliance/consent":                            true,
	"GET /api/compliance/consent/history":                    true,
	"GET /api/compliance/consent/purposes":                   true,
	"GET /api/compliance/consent/{purpose}":                  true,
	"GET /api/compliance/inventory":                          true,
	"GET /api/compliance/privacy/purpose/{dataId}/{purpose}": true,
	"GET /api/compliance/ropa/activities":                    true,
	"GET /api/compliance/ropa/activities/{id}":               true,
	"GET /api/compliance/ropa/compliance/{id}":               true,
	"GET /api/compliance/ropa/report":                        true,
	"GET /api/compliance/ropa/stats":                         true,
	"GET /api/compliance/sar/{id}":                           true,
	"PATCH /api/compliance/breach/{id}":                      true,
	"PATCH /api/compliance/ropa/activities/{id}":             true,
	"POST /api/compliance/breach":                            true,
	"POST /api/compliance/breach/{id}/notify":                true,
	"POST /api/compliance/consent":                           true,
	"POST /api/compliance/deletion":                          true,
	"POST /api/compliance/privacy/controls":                  true,
	"POST /api/compliance/privacy/minimize":                  true,
	"POST /api/compliance/privacy/pseudonymize":              true,
	"POST /api/compliance/ropa/activities":                   true,
	"POST /api/compliance/ropa/activities/{id}/review":       true,
	"POST /api/compliance/sar":                               true,
}
