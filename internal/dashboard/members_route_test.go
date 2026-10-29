package dashboard

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-TEAM-1 (Stage 1a Prompt 52, PR A): every write of the /dashboard
// group is mounted through dashboardAction, which names its action (the
// role check) or marks it personal. A write mounted around it would let a
// viewer change the account.
func TestDashboardRoutes_EveryWriteNamesItsAction(t *testing.T) {
	r, _, _ := setupTestRouter(t)
	writes := 0
	require.NoError(t, chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/dashboard/") || method == http.MethodGet || method == http.MethodHead {
			return nil
		}
		writes++
		key := method + " " + strings.TrimPrefix(route, "/dashboard")
		dashboardActedMu.Lock()
		acted := dashboardActed[key]
		dashboardActedMu.Unlock()
		assert.True(t, acted, "%s is mounted without dashboardAction", key)
		return nil
	}))
	assert.Greater(t, writes, 20, "the walk found the writes")
	for route := range dashboardWrites {
		assert.True(t, dashboardActed[route], "%s names an action no route uses", route)
	}
}
