package api

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/config"
	"github.com/FairForge/vaultaire/internal/docs"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestOpenAPIRouteInventory prints every registered chi route next to its
// OpenAPI status (Review R14, item 1). Run with -v to read the inventory:
//
//	go test ./internal/api/ -run TestOpenAPIRouteInventory -v
//
// It never fails on its own — TestOpenAPIDriftGuard is the gate; this is the
// human-readable diff the reviews paste.
func TestOpenAPIRouteInventory(t *testing.T) {
	s := NewServer(&config.Config{Server: config.ServerConfig{Port: 8000}}, zap.NewNop(),
		engine.NewEngine(nil, zap.NewNop(), nil), nil, nil)

	var routes []string
	require.NoError(t, chi.Walk(s.GetRouter(),
		func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			route = strings.TrimSuffix(route, "/")
			if route == "" {
				route = "/"
			}
			routes = append(routes, method+" "+route)
			return nil
		}))
	sort.Strings(routes)

	spec := docs.GenerateOpenAPISpec()
	specOps := map[string]bool{}
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

	documented, allowlisted, undocumented, outOfScope := 0, 0, 0, 0
	for _, r := range routes {
		state := "out-of-scope"
		switch {
		case specOps[r]:
			state = "documented"
			documented++
		case !isGuardedRoute(r):
			outOfScope++
		case knownUndocumented[r]:
			state = "ALLOWLISTED"
			allowlisted++
		default:
			state = "UNDOCUMENTED"
			undocumented++
		}
		t.Logf("%-14s %s", state, r)
	}
	var ghosts []string
	for op := range specOps {
		found := false
		for _, r := range routes {
			if r == op {
				found = true
				break
			}
		}
		if !found && !isS3SpecPath(op) {
			ghosts = append(ghosts, op)
		}
	}
	sort.Strings(ghosts)
	for _, g := range ghosts {
		t.Logf("%-14s %s", "GHOST-IN-SPEC", g)
	}
	t.Logf("routes=%d documented=%d allowlisted=%d undocumented=%d out-of-scope=%d ghosts=%d",
		len(routes), documented, allowlisted, undocumented, outOfScope, len(ghosts))
}
