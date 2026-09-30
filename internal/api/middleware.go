package api

import (
	"net/http"
)

// requireAdmin gates a handler on users.role = 'admin', looked up per
// request for the JWT user (requireJWT must run first). Moved here from the
// deleted quota_management.go (Review R11 / WP-R10-7); the string-keyed
// "is_admin" context short-circuit that nothing set is gone.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdminRequest(r) {
			http.Error(w, "admin access required", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// requireAdminMiddleware is requireAdmin in chi's middleware shape, for
// r.Use on a whole route group.
func (s *Server) requireAdminMiddleware(next http.Handler) http.Handler {
	return s.requireAdmin(next.ServeHTTP)
}

func (s *Server) isAdminRequest(r *http.Request) bool {
	userID, _ := r.Context().Value(userIDKey).(string)
	if userID == "" || s.db == nil {
		return false
	}
	var role string
	if err := s.db.QueryRowContext(r.Context(),
		"SELECT COALESCE(role, '') FROM users WHERE id = $1", userID,
	).Scan(&role); err != nil {
		return false
	}
	return role == "admin"
}
