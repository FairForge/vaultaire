package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/translog/adminread"
)

// Stage 1a Prompt 34a, Part 0a.

// noTenantRecorder answers every subject read as the API's recorder does
// for a subject that is no tenant (RP44-1).
type noTenantRecorder struct{}

func (noTenantRecorder) RecordAdminRead(_ context.Context, _, scope, tenant string) error {
	return fmt.Errorf("admin read %s of %q: %w", scope, tenant, adminread.ErrNoSuchTenant)
}

// A subject that is no tenant: 404, and the page never runs.
func TestAdminRead_UnresolvedSubject_PageNeverRuns(t *testing.T) {
	for _, sub := range []string{"/tenants/{id}", "/support/{id}"} {
		ran := false
		r := chi.NewRouter()
		r.Route("/admin", func(ar chi.Router) {
			adminGet(ar, Deps{AdminReads: noTenantRecorder{}, Logger: zap.NewNop()}, sub, func(http.ResponseWriter, *http.Request) { ran = true })
		})
		w := httptest.NewRecorder()

		// Act
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin"+strings.ReplaceAll(sub, "{id}", "%20t-1"), nil))

		// Assert
		assert.Equal(t, http.StatusNotFound, w.Code, sub)
		assert.False(t, ran, "%s ran for a subject that is no tenant", sub)
		assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	}
}

// Every admin route of another method than GET is classified, with its
// reason (RP43-6) — with an engine and a flags service, so every one is
// mounted.
func TestAdminWriteRoutes_AllClassified(t *testing.T) {
	r, _ := adminRouter(t, &failingRecorder{})
	mounted := map[string]bool{}
	require.NoError(t, chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.ReplaceAll(route, "/*/", "/")
		if method != http.MethodGet && strings.HasPrefix(route, "/admin") {
			mounted[method+" "+route] = true
		}
		return nil
	}))
	for k := range mounted {
		assert.True(t, strings.HasPrefix(adminWriteRoutes[k], "mutation: "), "%s is mounted but not classified", k)
	}
	for k := range adminWriteRoutes {
		assert.True(t, mounted[k], "%s is classified but not mounted", k)
	}
	assert.NotEmpty(t, mounted)
}
