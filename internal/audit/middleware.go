package audit

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ContextKey types for extracting values from context
type contextKey string

const (
	// ContextKeyUserID is the context key for user ID
	ContextKeyUserID contextKey = "user_id"
	// ContextKeyTenantID is the context key for tenant ID
	ContextKeyTenantID contextKey = "tenant_id"
)

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (rw *responseWriter) WriteHeader(code int) {
	if !rw.written {
		rw.statusCode = code
		rw.written = true
		rw.ResponseWriter.WriteHeader(code)
	}
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.written {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.ResponseWriter.Write(b)
}

// AuditMiddleware creates middleware that logs all HTTP requests
func AuditMiddleware(auditor *AuditService, logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap response writer to capture status
			wrapped := &responseWriter{
				ResponseWriter: w,
				statusCode:     http.StatusOK,
			}

			// Call next handler
			next.ServeHTTP(wrapped, r)

			// Log audit event asynchronously
			go func() {
				duration := time.Since(start)

				event := buildAuditEvent(r, wrapped.statusCode, duration)

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				if err := auditor.LogEvent(ctx, event); err != nil {
					logger.Error("failed to log audit event",
						zap.Error(err),
						zap.String("path", r.URL.Path),
					)
				}
			}()
		})
	}
}

// buildAuditEvent constructs an audit event from the HTTP request
func buildAuditEvent(r *http.Request, statusCode int, duration time.Duration) *AuditEvent {
	event := &AuditEvent{
		Timestamp: time.Now(),
		Action:    r.Method,
		Resource:  r.URL.Path,
		Duration:  duration,
		IP:        extractIP(r),
		UserAgent: r.UserAgent(),
		Metadata: map[string]string{
			"method":      r.Method,
			"path":        r.URL.Path,
			"status_code": http.StatusText(statusCode),
		},
	}

	// Extract user ID from context
	if userID, ok := r.Context().Value(ContextKeyUserID).(uuid.UUID); ok {
		event.UserID = userID
	}

	// Extract tenant ID from context
	if tenantID, ok := r.Context().Value(ContextKeyTenantID).(string); ok {
		event.TenantID = tenantID
	}

	// Determine event type based on path
	event.EventType = determineEventType(r.Method, r.URL.Path)

	// Determine result based on status code
	event.Result = determineResult(statusCode)
	event.Severity = determineSeverity(statusCode)

	// Add query parameters if present
	if len(r.URL.RawQuery) > 0 {
		event.Metadata["query"] = r.URL.RawQuery
	}

	return event
}

// extractIP gets the client IP from the request
func extractIP(r *http.Request) string {
	// Check X-Forwarded-For header
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded != "" {
		// Take the first IP in the chain
		parts := strings.Split(forwarded, ",")
		return strings.TrimSpace(parts[0])
	}

	// Check X-Real-IP header
	realIP := r.Header.Get("X-Real-IP")
	if realIP != "" {
		return realIP
	}

	// Fall back to RemoteAddr
	ip := r.RemoteAddr
	// Remove port if present
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}
	return ip
}

// determineEventType maps HTTP operations to audit event types
func determineEventType(method, path string) EventType {
	// Simple path-based classification
	switch {
	case strings.Contains(path, "/auth/login"):
		return EventTypeLogin
	case strings.Contains(path, "/auth/logout"):
		return EventTypeLogout
	case strings.Contains(path, "/api-keys"):
		switch method {
		case "POST":
			return EventTypeAPIKeyCreated
		default:
			return EventTypeAPIKeyUsed
		}
	case strings.Contains(path, "/buckets"):
		switch method {
		case "POST":
			return EventTypeBucketCreate
		case "DELETE":
			return EventTypeBucketDelete
		default:
			return EventTypeBucketList
		}
	case strings.HasSuffix(path, "/") || method == "GET":
		return EventTypeFileList
	case method == "PUT" || method == "POST":
		return EventTypeFileUpload
	case method == "DELETE":
		return EventTypeFileDelete
	case method == "GET":
		return EventTypeFileDownload
	default:
		return EventType("http.request")
	}
}

// determineResult maps HTTP status codes to audit results
func determineResult(statusCode int) Result {
	switch {
	case statusCode >= 200 && statusCode < 300:
		return ResultSuccess
	case statusCode == 401 || statusCode == 403:
		return ResultDenied
	default:
		return ResultFailure
	}
}

// determineSeverity maps HTTP status codes to severity levels
func determineSeverity(statusCode int) Severity {
	switch {
	case statusCode >= 200 && statusCode < 300:
		return SeverityInfo
	case statusCode >= 400 && statusCode < 500:
		return SeverityWarning
	case statusCode >= 500:
		return SeverityError
	default:
		return SeverityInfo
	}
}
