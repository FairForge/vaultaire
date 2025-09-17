package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"go.uber.org/zap"
)

// Middleware is a function that wraps an HTTP handler
type Middleware func(http.Handler) http.Handler

// RateLimitMiddleware creates middleware that enforces rate limits
func RateLimitMiddleware(limiter *RateLimiter) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Extract tenant ID from header
			tenantID := r.Header.Get("X-Tenant-ID")
			if tenantID == "" {
				tenantID = "default" // Default tenant for requests without ID
			}

			// Set rate limit headers (always set, even on success)
			w.Header().Set("X-RateLimit-Limit", "100")
			w.Header().Set("X-RateLimit-Remaining", "99") // Simplified for now
			w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(time.Second).Unix()))

			// Check rate limit
			if !limiter.Allow(tenantID) {
				// Rate limit exceeded
				w.WriteHeader(http.StatusTooManyRequests)
				// Handle error to satisfy gosec
				_, _ = w.Write([]byte("Rate limit exceeded"))
				return
			}

			// Continue to next handler
			next.ServeHTTP(w, r)
		})
	}
}

// ExtractTenant extracts tenant from AWS signature
func ExtractTenant(db *sql.DB, logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			var tenantID string

			if strings.Contains(auth, "AWS4-HMAC-SHA256") {
				// Extract access key from signature
				parts := strings.Split(auth, "Credential=")
				if len(parts) > 1 {
					credParts := strings.Split(parts[1], "/")
					if len(credParts) > 0 {
						accessKey := credParts[0]
						logger.Debug("Extracting tenant", zap.String("access_key", accessKey))
						// Look up tenant
						err := db.QueryRow("SELECT id FROM tenants WHERE access_key = $1",
							accessKey).Scan(&tenantID)
						if err != nil {
							logger.Debug("tenant lookup failed",
								zap.String("access_key", accessKey))
							tenantID = "test-tenant"
						}
					}
				}
			}

			if tenantID == "" {
				tenantID = "test-tenant"
			}

			ctx := context.WithValue(r.Context(), common.TenantIDKey, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
