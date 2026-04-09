package middleware

import (
	"context"
	"net/http"
	"net/url"
)

type flashKey struct{}

// SetFlash sets a flash message that will be available on the next request.
// Category is typically "success" or "error".
func SetFlash(w http.ResponseWriter, category, message string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "flash",
		Value:    url.Values{category: {message}}.Encode(),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   true,
		MaxAge:   60, // 1 minute — only needs to survive a redirect
	})
}

// Flash is HTTP middleware that reads the flash cookie, injects it into
// context, and clears the cookie so the message is shown only once.
func Flash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("flash")
		if err == nil && c.Value != "" {
			vals, pErr := url.ParseQuery(c.Value)
			if pErr == nil && len(vals) > 0 {
				m := make(map[string]string, len(vals))
				for k, v := range vals {
					if len(v) > 0 {
						m[k] = v[0]
					}
				}
				ctx := context.WithValue(r.Context(), flashKey{}, m)
				r = r.WithContext(ctx)
			}

			// Clear the flash cookie.
			http.SetCookie(w, &http.Cookie{
				Name:     "flash",
				Value:    "",
				Path:     "/",
				HttpOnly: true,
				Secure:   true,
				SameSite: http.SameSiteLaxMode,
				MaxAge:   -1,
			})
		}

		next.ServeHTTP(w, r)
	})
}

// GetFlash returns a flash message for the given category, or "".
func GetFlash(ctx context.Context, category string) string {
	m, _ := ctx.Value(flashKey{}).(map[string]string)
	return m[category]
}

// GetFlashMap returns all flash messages from the context.
func GetFlashMap(ctx context.Context) map[string]string {
	m, _ := ctx.Value(flashKey{}).(map[string]string)
	if m == nil {
		return map[string]string{}
	}
	return m
}
