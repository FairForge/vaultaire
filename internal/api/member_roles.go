package api

import (
	"context"
	"net/http"

	"github.com/FairForge/vaultaire/internal/auth"
)

// The role check of the management surfaces (WP-TEAM-1, Stage 1a Prompt
// 52). requireJWT puts the acting member's role on the context; every write
// route of /api/v1/manage and /api/v1/platform is mounted behind
// requireRole with the action it performs, and auth.Can is the one rule
// (TestManagementRoutes_EveryWriteNamesItsAction walks the router).

// memberRole is the acting member's role ("" = no member of the tenant). A
// request that reached a handler without requireJWT (a test mounting the
// handler by hand) is the owner: the role default of an un-migrated flow.
func memberRole(ctx context.Context) auth.Role {
	if r, ok := ctx.Value(roleKey).(auth.Role); ok {
		return r
	}
	return auth.RoleOwner
}

// requireMember refuses (403 not_a_member) a JWT whose user is no member of
// its tenant: every route of /api/v1/manage and /api/v1/platform.
func (s *Server) requireMember(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if memberRole(r.Context()) == "" {
			writeManagementError(w, ErrTypePermission, "not_a_member", "this account is not a member of the token's tenant", "")
			return
		}
		next.ServeHTTP(w, r)
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
