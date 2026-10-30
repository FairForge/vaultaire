package api

import (
	"context"
	"net/http"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
)

// The role check of the management surfaces (WP-TEAM-1, Stage 1a Prompt
// 52). requireJWT puts the acting member's role on the context; every write
// route of /api/v1/manage and /api/v1/platform is mounted behind
// requireRole with the action it performs, and auth.Can is the one rule
// (TestManagementRoutes_EveryWriteNamesItsAction walks the router).

// memberRole is the acting member's role ("" = no member of the tenant). A
// request that reached a handler without requireJWT has no role (RP84-2:
// the owner default was the API's half of a tenant-less sign-in acting as
// an owner); a test mounting a handler by hand sets the role it means.
func memberRole(ctx context.Context) auth.Role {
	r, _ := ctx.Value(roleKey).(auth.Role)
	return r
}

// activeTenant is the tenant the request acts on: the JWT's (WP-TEAM-1 —
// the one the token was minted for, among the user's).
func activeTenant(r *http.Request) string {
	t, _ := r.Context().Value(tenantIDKey).(string)
	return t
}

// requireMember refuses (403 not_a_member) a JWT whose user is no member of
// its tenant: every route of the tenant surfaces (/api/v1/manage,
// /api/v1/platform, /api/v1/user, /api/v1/webhooks, /api/v1/events,
// /api/v1/sts). It also names the active tenant for the audit trail
// (audit.WithTenant): a row a handler writes with no tenant of its own is
// the tenant the member acted on — never on the operator routes, whose
// rows are about other tenants.
func (s *Server) requireMember(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if memberRole(r.Context()) == "" {
			writeManagementError(w, ErrTypePermission, "not_a_member", "this account is not a member of the token's tenant", "")
			return
		}
		next.ServeHTTP(w, r.WithContext(audit.WithTenant(r.Context(), activeTenant(r))))
	})
}

// requireRole refuses (403 role_forbidden) a member whose role may not do
// action.
func (s *Server) requireRole(action auth.Action) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if role := memberRole(r.Context()); !auth.Can(role, action) {
				writeManagementError(w, ErrTypePermission, "role_forbidden",
					"a member with the role "+string(role)+" may not do this ("+string(action)+")", "")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
