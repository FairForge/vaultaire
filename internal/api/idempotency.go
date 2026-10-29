package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// WP-R11-2 (R11-08, R10-23): the key is CLAIMED before the handler runs —
// one row per (tenant, key), inserted with a ready 409
// idempotency_in_progress answer and claimed = TRUE — so two requests with
// one key never both run; the request is bound by request_digest
// (sha256(method ‖ path ‖ body)), so the same key with another body is 422
// idempotency_key_reused instead of the first answer; and the answer stored
// for a replay holds no secret: every field idempotencySecretFields names is
// blanked at rest and the replay says "idempotent_replay": true. A 2xx
// answer replaces the claim; anything else (and a handler that panics)
// removes it, so the client retries with the same key. A claim older than
// idempotencyClaimTTL is a process that died: taken over. Rows written
// before migration 119 carry no hash and are judged on method and path, as
// before. The previous build reads a claim as a cached 409 "in progress" —
// the right answer — during a switch.

const (
	idempotencyClaimTTL = 10 * time.Minute
	// idempotencyMaxBody bounds the body hashed (the management routes'
	// limit is 1 MiB, server.go).
	idempotencyMaxBody = 8 << 20
)

// idempotencySecretFields are the JSON keys of every answer that carries a
// secret on a route behind this middleware: an API key's create and rotate
// ("secret"), an STS token ("secret_key"), a webhook's create and
// rotate-secret ("secret"), the user API's "secretAccessKey", and the
// generic shapes (secret_access_key, session_token, token). A cached answer
// keeps the key with its value blanked, at any depth.
var idempotencySecretFields = map[string]bool{
	"secret": true, "secret_key": true, "secretAccessKey": true, "secret_access_key": true,
	"session_token": true, "sessionToken": true, "token": true,
}

// idempotencyClaimBody is the claim row's stored answer.
var idempotencyClaimBody = []byte(`{"error":{"type":"conflict_error","code":"idempotency_in_progress","message":"a request with this idempotency key is in progress; retry when it has finished","param":"Idempotency-Key"}}`)

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

		// The request's identity: method, path and body.
		var body []byte
		if r.Body != nil {
			b, err := io.ReadAll(io.LimitReader(r.Body, idempotencyMaxBody+1))
			if err != nil {
				writeManagementError(w, ErrTypeInvalidRequest, "invalid_body", "the request body could not be read", "")
				return
			}
			if len(b) > idempotencyMaxBody {
				writeManagementError(w, ErrTypeInvalidRequest, "body_too_large", "the request body is too large for an idempotent request", "")
				return
			}
			body = b
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		hash := idempotencyRequestHash(r.Method, r.URL.Path, body)

		claimed, err := im.claim(r.Context(), tenantID, key, r.Method, r.URL.Path, hash)
		if err != nil {
			im.logger.Error("idempotency claim failed", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "the idempotency key could not be recorded", "")
			return
		}
		if !claimed {
			im.answerExisting(w, r, tenantID, key, hash)
			return
		}

		rw := &capturingResponseWriter{ResponseWriter: w, body: &bytes.Buffer{}}
		completed := false
		defer func() {
			if !completed {
				im.release(tenantID, key, hash)
			}
		}()
		next.ServeHTTP(rw, r)

		if rw.status >= 200 && rw.status < 300 {
			completed = im.complete(tenantID, key, hash, rw)
		}
	})
}

// idempotencyRequestHash is sha256(method ‖ "\n" ‖ path ‖ "\n" ‖ body), hex.
func idempotencyRequestHash(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method + "\n" + path + "\n"))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// claim inserts the claim row, or takes over an expired answer or a stale
// claim. false = another row holds the key.
func (im *idempotencyMiddleware) claim(ctx context.Context, tenantID, key, method, path, hash string) (bool, error) {
	var one int
	err := im.db.QueryRowContext(ctx, `
		INSERT INTO idempotency_cache (tenant_id, idempotency_key, method, path, response_status, response_headers, response_body, request_digest, claimed, created_at)
		VALUES ($1, $2, $3, $4, 409, '{"Content-Type":"application/json"}', $5, $6, TRUE, NOW())
		ON CONFLICT (tenant_id, idempotency_key) DO UPDATE SET
			method = EXCLUDED.method, path = EXCLUDED.path, response_status = EXCLUDED.response_status,
			response_headers = EXCLUDED.response_headers, response_body = EXCLUDED.response_body,
			request_digest = EXCLUDED.request_digest, claimed = TRUE, created_at = NOW()
		WHERE idempotency_cache.created_at < NOW() - $7::interval
		   OR (idempotency_cache.claimed AND idempotency_cache.created_at < NOW() - $8::interval)
		RETURNING 1`,
		tenantID, key, method, path, idempotencyClaimBody, hash,
		fmt.Sprintf("%d seconds", int(idempotencyTTL.Seconds())), fmt.Sprintf("%d seconds", int(idempotencyClaimTTL.Seconds()))).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim idempotency key: %w", err)
	}
	return true, nil
}

// answerExisting answers a request whose key another row holds: in
// progress, another request, or the stored answer.
func (im *idempotencyMiddleware) answerExisting(w http.ResponseWriter, r *http.Request, tenantID, key, hash string) {
	var cached cachedResponse
	var storedHash sql.NullString
	var claimed bool
	err := im.db.QueryRowContext(r.Context(), `
		SELECT method, path, response_status, response_headers, response_body, request_digest, claimed
		FROM idempotency_cache
		WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, key).Scan(
		&cached.Method, &cached.Path, &cached.Status, &cached.HeadersJSON, &cached.Body, &storedHash, &claimed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Released between the claim and this read: the client retries.
		writeManagementError(w, ErrTypeConflict, "idempotency_in_progress",
			"a request with this idempotency key is in progress; retry when it has finished", idempotencyKeyHeader)
		return
	case err != nil:
		im.logger.Error("read idempotency key", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "the idempotency key could not be read", "")
		return
	}
	if storedHash.Valid {
		if storedHash.String != hash {
			writeJSON(w, http.StatusUnprocessableEntity, managementError{Error: managementErrorBody{
				Type: ErrTypeInvalidRequest, Code: "idempotency_key_reused",
				Message:   "this idempotency key was used with a different request (method, path or body)",
				Param:     idempotencyKeyHeader,
				RequestID: getRequestID(w),
			}})
			return
		}
		if claimed {
			writeManagementError(w, ErrTypeConflict, "idempotency_in_progress",
				"a request with this idempotency key is in progress; retry when it has finished", idempotencyKeyHeader)
			return
		}
	} else if cached.Method != r.Method || cached.Path != r.URL.Path {
		writeManagementError(w, ErrTypeConflict, "idempotency_key_reuse",
			"idempotency key already used with a different request", idempotencyKeyHeader)
		return
	}
	im.replayResponse(w, &cached)
}

// complete replaces the claim with the handler's answer, its secrets
// blanked. false = the claim could not be completed (it is released).
func (im *idempotencyMiddleware) complete(tenantID, key, hash string, rw *capturingResponseWriter) bool {
	headers := make(map[string]string)
	for k, v := range rw.Header() {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}
	delete(headers, "Content-Length")
	headersJSON, err := json.Marshal(headers)
	if err != nil {
		im.logger.Error("marshal idempotency headers", zap.Error(err))
		return false
	}
	stored := redactIdempotentBody(rw.body.Bytes())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := im.db.ExecContext(ctx, `
		UPDATE idempotency_cache
		SET response_status = $4, response_headers = $5, response_body = $6, claimed = FALSE, created_at = NOW()
		WHERE tenant_id = $1 AND idempotency_key = $2 AND request_digest = $3 AND claimed`,
		tenantID, key, hash, rw.status, headersJSON, stored)
	if err != nil {
		im.logger.Error("cache idempotency response", zap.Error(err))
		return false
	}
	if n, _ := res.RowsAffected(); n != 1 {
		im.logger.Warn("idempotency claim was taken over before the answer was cached", zap.String("tenant_id", tenantID))
	}
	return true
}

// release removes this request's claim (a refusal, a failure, a panic).
func (im *idempotencyMiddleware) release(tenantID, key, hash string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := im.db.ExecContext(ctx, `
		DELETE FROM idempotency_cache WHERE tenant_id = $1 AND idempotency_key = $2 AND request_digest = $3 AND claimed`,
		tenantID, key, hash); err != nil {
		im.logger.Error("release idempotency claim", zap.Error(err))
	}
}

// redactIdempotentBody blanks every secret field of a JSON answer, at any
// depth; an answer that had one also says "idempotent_replay": true. A body
// that is not a JSON object or array is stored as it is (no route behind
// this middleware answers a secret outside JSON).
func redactIdempotentBody(body []byte) []byte {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	if !redactSecrets(v) {
		return body
	}
	if obj, ok := v.(map[string]any); ok {
		obj["idempotent_replay"] = true
	}
	out, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"idempotent_replay":true}`)
	}
	return out
}

func redactSecrets(v any) bool {
	found := false
	switch t := v.(type) {
	case map[string]any:
		for k, inner := range t {
			if str, ok := inner.(string); ok && idempotencySecretFields[k] {
				if str != "" {
					t[k] = ""
					found = true
				}
				continue
			}
			if redactSecrets(inner) {
				found = true
			}
		}
	case []any:
		for _, inner := range t {
			if redactSecrets(inner) {
				found = true
			}
		}
	}
	return found
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

// cleanupExpired deletes idempotency cache entries past their 24 h life. It
// is the `idempotency_cleanup` job (hourly, jobs.go); the error used to be
// dropped.
func (im *idempotencyMiddleware) cleanupExpired(ctx context.Context) (int64, error) {
	res, err := im.db.ExecContext(ctx, `DELETE FROM idempotency_cache WHERE created_at < NOW() - INTERVAL '24 hours'`)
	if err != nil {
		return 0, fmt.Errorf("delete expired idempotency keys: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
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
