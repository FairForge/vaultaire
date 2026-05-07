package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"go.uber.org/zap"
)

const (
	idempotencyKeyHeader    = "Idempotency-Key"
	idempotencyReplayHeader = "Idempotency-Replayed"
	idempotencyKeyMaxLen    = 256
	idempotencyTTL          = 24 * time.Hour
)

type idempotencyMiddleware struct {
	db     *sql.DB
	logger *zap.Logger
}

func newIdempotencyMiddleware(db *sql.DB, logger *zap.Logger) *idempotencyMiddleware {
	return &idempotencyMiddleware{db: db, logger: logger}
}

func (im *idempotencyMiddleware) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		key := r.Header.Get(idempotencyKeyHeader)
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}

		if len(key) > idempotencyKeyMaxLen {
			writeManagementError(w, ErrTypeInvalidRequest, "idempotency_key_too_long",
				"idempotency key must be 256 characters or fewer", idempotencyKeyHeader)
			return
		}

		if im.db == nil {
			next.ServeHTTP(w, r)
			return
		}

		tenantID, _ := r.Context().Value(tenantIDKey).(string)
		if tenantID == "" {
			next.ServeHTTP(w, r)
			return
		}

		var cached cachedResponse
		err := im.db.QueryRowContext(r.Context(), `
			SELECT method, path, response_status, response_headers, response_body
			FROM idempotency_cache
			WHERE tenant_id = $1 AND idempotency_key = $2 AND created_at > $3
		`, tenantID, key, time.Now().Add(-idempotencyTTL)).Scan(
			&cached.Method, &cached.Path, &cached.Status, &cached.HeadersJSON, &cached.Body,
		)

		if err == nil {
			if cached.Method != r.Method || cached.Path != r.URL.Path {
				writeManagementError(w, ErrTypeConflict, "idempotency_key_reuse",
					"idempotency key already used with a different request", idempotencyKeyHeader)
				return
			}
			im.replayResponse(w, &cached)
			return
		}

		rw := &capturingResponseWriter{
			ResponseWriter: w,
			body:           &bytes.Buffer{},
		}

		next.ServeHTTP(rw, r)

		if rw.status >= 200 && rw.status < 300 {
			im.cacheResponse(r.Context(), tenantID, key, r.Method, r.URL.Path, rw)
		}
	})
}

type cachedResponse struct {
	Method      string
	Path        string
	Status      int
	HeadersJSON []byte
	Body        []byte
}

func (im *idempotencyMiddleware) replayResponse(w http.ResponseWriter, cached *cachedResponse) {
	var headers map[string]string
	if err := json.Unmarshal(cached.HeadersJSON, &headers); err == nil {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set(idempotencyReplayHeader, "true")
	w.WriteHeader(cached.Status)
	_, _ = w.Write(cached.Body)
}

func (im *idempotencyMiddleware) cacheResponse(_ context.Context, tenantID, key, method, path string, rw *capturingResponseWriter) {
	headers := make(map[string]string)
	for k, v := range rw.Header() {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}
	headersJSON, err := json.Marshal(headers)
	if err != nil {
		im.logger.Error("marshal idempotency headers", zap.Error(err))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = im.db.ExecContext(ctx, `
		INSERT INTO idempotency_cache (tenant_id, idempotency_key, method, path, response_status, response_headers, response_body)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, idempotency_key) DO NOTHING
	`, tenantID, key, method, path, rw.status, headersJSON, rw.body.Bytes())
	if err != nil {
		im.logger.Error("cache idempotency response", zap.Error(err))
	}
}

// StartCleanup launches a goroutine that deletes expired idempotency cache entries hourly.
func (im *idempotencyMiddleware) StartCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = im.db.ExecContext(ctx, `DELETE FROM idempotency_cache WHERE created_at < NOW() - INTERVAL '24 hours'`)
			}
		}
	}()
}

// capturingResponseWriter wraps http.ResponseWriter to capture the response for caching.
type capturingResponseWriter struct {
	http.ResponseWriter
	status      int
	body        *bytes.Buffer
	wroteHeader bool
}

func (w *capturingResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *capturingResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}
