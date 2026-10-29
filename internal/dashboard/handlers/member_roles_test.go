package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/auth"
	dashauth "github.com/FairForge/vaultaire/internal/dashboard/auth"
	"github.com/stretchr/testify/assert"
)

// WP-TEAM-1: the dashboard's write check is auth.Can on the session's
// member role.
func TestRequireAction_RefusesARoleThatMayNot(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, c := range []struct {
		role   auth.Role
		action auth.Action
		want   int
	}{
		{auth.RoleViewer, auth.ActionKeyWrite, http.StatusForbidden},
		{auth.RoleViewer, auth.ActionBucketWrite, http.StatusForbidden},
		{auth.RoleDeveloper, auth.ActionKeyWrite, http.StatusNoContent},
		{auth.RoleAdmin, auth.ActionAccountDelete, http.StatusForbidden},
		{auth.RoleAdmin, auth.ActionBilling, http.StatusForbidden},
		{auth.RoleOwner, auth.ActionAccountDelete, http.StatusNoContent},
	} {
		req := httptest.NewRequest(http.MethodPost, "/dashboard/x", nil)
		req = req.WithContext(dashauth.WithSession(req.Context(), &dashauth.SessionData{UserID: "u", TenantID: "t", MemberRole: string(c.role)}))
		w := httptest.NewRecorder()
		RequireAction(c.action)(ok).ServeHTTP(w, req)
		assert.Equal(t, c.want, w.Code, "%s %s", c.role, c.action)
	}
}
