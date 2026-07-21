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
			"GET": item.Get, "PUT": item.Put, "POST": item.Post,
			"DELETE": item.Delete, "HEAD": item.Head,
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

// knownUndocumented is the recorded, reviewed backlog of API routes that
// predate the drift guard and are not yet in the OpenAPI spec (B3 —
// OpenAPI rebrand + expand — burns this down; note the spec's PathItem has
// no Patch field yet, so PATCH routes cannot be documented until it grows
// one). Do not add to it casually: new endpoints should ship documented.
// 100 routes as of 2026-07-20 — the "7 of ~40 ops documented" audit
// finding, now enumerated and guarded.
var knownUndocumented = map[string]bool{
	"DELETE /api/compliance/consent/{purpose}":               true,
	"DELETE /api/compliance/ropa/activities/{id}":            true,
	"DELETE /api/rbac/users/{userID}/roles":                  true,
	"DELETE /api/v1/admin/flags/{key}":                       true,
	"DELETE /api/v1/manage/account":                          true,
	"DELETE /api/v1/manage/buckets/{name}":                   true,
	"DELETE /api/v1/manage/keys/{id}":                        true,
	"DELETE /api/v1/user":                                    true,
	"DELETE /api/v1/user/apikeys/{keyId}":                    true,
	"DELETE /api/v1/webhooks/{id}":                           true,
	"GET /api/compliance/activities":                         true,
	"GET /api/compliance/breach":                             true,
	"GET /api/compliance/breach/stats":                       true,
	"GET /api/compliance/breach/{id}":                        true,
	"GET /api/compliance/consent":                            true,
	"GET /api/compliance/consent/history":                    true,
	"GET /api/compliance/consent/purposes":                   true,
	"GET /api/compliance/consent/{purpose}":                  true,
	"GET /api/compliance/export/{id}":                        true,
	"GET /api/compliance/inventory":                          true,
	"GET /api/compliance/privacy/purpose/{dataId}/{purpose}": true,
	"GET /api/compliance/ropa/activities":                    true,
	"GET /api/compliance/ropa/activities/{id}":               true,
	"GET /api/compliance/ropa/compliance/{id}":               true,
	"GET /api/compliance/ropa/report":                        true,
	"GET /api/compliance/ropa/stats":                         true,
	"GET /api/compliance/sar/{id}":                           true,
	"GET /api/rbac/audit":                                    true,
	"GET /api/rbac/permissions":                              true,
	"GET /api/rbac/roles":                                    true,
	"GET /api/rbac/users/{userID}/roles":                     true,
	"GET /api/v1/admin/breaches":                             true,
	"GET /api/v1/admin/flags":                                true,
	"GET /api/v1/events":                                     true,
	"GET /api/v1/manage/account/export/{id}":                 true,
	"GET /api/v1/manage/buckets":                             true,
	"GET /api/v1/manage/buckets/{name}":                      true,
	"GET /api/v1/manage/buckets/{name}/objects":              true,
	"GET /api/v1/manage/keys":                                true,
	"GET /api/v1/manage/usage":                               true,
	"GET /api/v1/presigned":                                  true,
	"GET /api/v1/quota":                                      true,
	"GET /api/v1/quota/history":                              true,
	"GET /api/v1/usage/alerts":                               true,
	"GET /api/v1/usage/stats":                                true,
	"GET /api/v1/user":                                       true,
	"GET /api/v1/user/activity":                              true,
	"GET /api/v1/user/apikeys":                               true,
	"GET /api/v1/user/apikeys/audit":                         true,
	"GET /api/v1/user/mfa/backup-codes":                      true,
	"GET /api/v1/user/preferences":                           true,
	"GET /api/v1/user/profile":                               true,
	"GET /api/v1/user/quota":                                 true,
	"GET /api/v1/user/usage":                                 true,
	"GET /api/v1/webhooks":                                   true,
	"GET /api/v1/webhooks/{id}/deliveries":                   true,
	"PATCH /api/compliance/breach/{id}":                      true,
	"PATCH /api/compliance/ropa/activities/{id}":             true,
	"PATCH /api/v1/admin/breach/{id}":                        true,
	"PATCH /api/v1/manage/buckets/{name}":                    true,
	"PATCH /api/v1/webhooks/{id}":                            true,
	"POST /api/compliance/breach":                            true,
	"POST /api/compliance/breach/{id}/notify":                true,
	"POST /api/compliance/consent":                           true,
	"POST /api/compliance/deletion":                          true,
	"POST /api/compliance/export":                            true,
	"POST /api/compliance/privacy/controls":                  true,
	"POST /api/compliance/privacy/minimize":                  true,
	"POST /api/compliance/privacy/pseudonymize":              true,
	"POST /api/compliance/ropa/activities":                   true,
	"POST /api/compliance/ropa/activities/{id}/review":       true,
	"POST /api/compliance/sar":                               true,
	"POST /api/rbac/users/{userID}/roles":                    true,
	"POST /api/v1/admin/breach":                              true,
	"POST /api/v1/admin/dedup-gc":                            true,
	"POST /api/v1/admin/quota-reconcile":                     true,
	"POST /api/v1/manage/account/cancel-deletion":            true,
	"POST /api/v1/manage/account/export":                     true,
	"POST /api/v1/manage/buckets":                            true,
	"POST /api/v1/manage/keys":                               true,
	"POST /api/v1/quota/upgrade":                             true,
	"POST /api/v1/sts/token":                                 true,
	"POST /api/v1/user/apikeys":                              true,
	"POST /api/v1/user/apikeys/{keyId}/expire":               true,
	"POST /api/v1/user/apikeys/{keyId}/rotate":               true,
	"POST /api/v1/user/mfa/disable":                          true,
	"POST /api/v1/user/mfa/enable":                           true,
	"POST /api/v1/webhooks":                                  true,
	"POST /api/v1/webhooks/{id}/test":                        true,
	"POST /api/waitlist":                                     true,
	"POST /auth/login":                                       true,
	"POST /auth/password-reset":                              true,
	"POST /auth/password-reset/complete":                     true,
	"POST /auth/register":                                    true,
	"PUT /api/v1/admin/flags/{key}":                          true,
	"PUT /api/v1/manage/buckets/{name}/residency":            true,
	"PUT /api/v1/manage/buckets/{name}/tier":                 true,
	"PUT /api/v1/user":                                       true,
	"PUT /api/v1/user/preferences":                           true,
	"PUT /api/v1/user/profile":                               true,
}
