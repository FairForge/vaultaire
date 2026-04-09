package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

type csrfKey struct{}

// CSRF implements the double-submit cookie pattern. On every request it
// ensures a csrf_token cookie exists and stores the token in the request
// context. On state-changing methods (POST, PUT, PATCH, DELETE) it
// validates that the submitted token (form field or header) matches the
// cookie.
func CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read or generate token.
		token := ""
		if c, err := r.Cookie("csrf_token"); err == nil && c.Value != "" {
			token = c.Value
		} else {
			token = generateToken()
			http.SetCookie(w, &http.Cookie{
				Name:     "csrf_token",
				Value:    token,
				Path:     "/",
				HttpOnly: true,
				Secure:   true,
				SameSite: http.SameSiteLaxMode,
			})
		}

		// Inject token into context for handlers/templates.
		ctx := context.WithValue(r.Context(), csrfKey{}, token)
		r = r.WithContext(ctx)

		// Safe methods pass through without validation.
		if r.Method == "GET" || r.Method == "HEAD" || r.Method == "OPTIONS" {
			next.ServeHTTP(w, r)
			return
		}

		// Validate: submitted token must match cookie.
		submitted := r.Header.Get("X-CSRF-Token")
		if submitted == "" {
			submitted = r.FormValue("csrf_token")
		}

		if submitted == "" || submitted != token {
			http.Error(w, "CSRF token invalid", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Token returns the CSRF token from the request context.
func Token(ctx context.Context) string {
	if v, ok := ctx.Value(csrfKey{}).(string); ok {
		return v
	}
	return ""
}

func generateToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("csrf: failed to generate random token")
	}
	return hex.EncodeToString(b)
}
