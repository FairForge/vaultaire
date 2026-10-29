package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// idemFixture is the middleware over the test database, one tenant per test
// (its own rows only), with handlers that count their runs.
type idemFixture struct {
	t        *testing.T
	db       *sql.DB
	tenantID string
	router   chi.Router
	runs     atomic.Int64
	release  chan struct{} // /slow waits on it
	started  chan struct{} // /slow signals it is running
}

func newIdemFixture(t *testing.T) *idemFixture {
	t.Helper()
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	f := &idemFixture{t: t, db: db, tenantID: "idem-" + uuid.New().String()[:8],
		release: make(chan struct{}), started: make(chan struct{}, 4)}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM idempotency_cache WHERE tenant_id = $1`, f.tenantID) })

	im := newIdempotencyMiddleware(db, zap.NewNop())
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tenantIDKey, f.tenantID)))
		})
	})
	r.Use(im.Middleware)
	r.Post("/test", func(w http.ResponseWriter, r *http.Request) {
		f.runs.Add(1)
		writeJSON(w, http.StatusCreated, map[string]string{"status": "created"})
	})
	r.Get("/test", func(w http.ResponseWriter, r *http.Request) {
		f.runs.Add(1)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Delete("/test", func(w http.ResponseWriter, r *http.Request) {
		f.runs.Add(1)
		writeJSON(w, http.StatusOK, map[string]string{"deleted": "true"})
	})
	r.Post("/error", func(w http.ResponseWriter, r *http.Request) {
		f.runs.Add(1)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad"})
	})
	r.Post("/slow", func(w http.ResponseWriter, r *http.Request) {
		f.runs.Add(1)
		f.started <- struct{}{}
		<-f.release
		writeJSON(w, http.StatusCreated, map[string]string{"status": "created"})
	})
	// The shapes of the answers that carry a secret: a key create / rotate
	// (management, user API), an STS token, a webhook secret.
	r.Post("/keys", func(w http.ResponseWriter, r *http.Request) {
		f.runs.Add(1)
		writeJSON(w, http.StatusCreated, map[string]any{
			"object": "api_key", "id": "key_1", "key_id": "VKAAAA", "secret": "SK-live-secret",
			"nested": map[string]any{"secret_key": "sts-secret", "session_token": "tok", "secretAccessKey": "x"},
			"keys":   []any{map[string]any{"secret_access_key": "y", "token": "z", "name": "kept"}},
		})
	})
	f.router = r
	return f
}

func (f *idemFixture) do(method, path, key, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set(idempotencyKeyHeader, key)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

func idemErrCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		Error struct{ Code string } `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return resp.Error.Code
}

func TestIdempotency_FirstRequestAndReplay(t *testing.T) {
	f := newIdemFixture(t)
	w := f.do("POST", "/test", "key-1", `{"a":1}`)
	require.Equal(t, http.StatusCreated, w.Code)
	assert.Empty(t, w.Header().Get(idempotencyReplayHeader))

	w = f.do("POST", "/test", "key-1", `{"a":1}`)
	require.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "true", w.Header().Get(idempotencyReplayHeader))
	assert.JSONEq(t, `{"status":"created"}`, w.Body.String())
	assert.Equal(t, int64(1), f.runs.Load(), "the handler ran once")

	w = f.do("POST", "/test", "key-2", `{"a":1}`)
	require.Equal(t, http.StatusCreated, w.Code)
	assert.Empty(t, w.Header().Get(idempotencyReplayHeader), "another key is another request")
	assert.Equal(t, int64(2), f.runs.Load())
}

func TestIdempotency_PassThrough(t *testing.T) {
	f := newIdemFixture(t)
	assert.Equal(t, http.StatusOK, f.do("GET", "/test", "k", "").Code, "GET is never cached")
	assert.Equal(t, http.StatusCreated, f.do("POST", "/test", "", "").Code, "no key")
	assert.Equal(t, http.StatusCreated, f.do("POST", "/test", "", "").Code)
	assert.Equal(t, int64(3), f.runs.Load())

	w := f.do("POST", "/test", strings.Repeat("x", 257), "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "idempotency_key_too_long", idemErrCode(t, w))
}

func TestIdempotency_ErrorNotCached(t *testing.T) {
	f := newIdemFixture(t)
	assert.Equal(t, http.StatusBadRequest, f.do("POST", "/error", "k", "").Code)
	assert.Equal(t, http.StatusBadRequest, f.do("POST", "/error", "k", "").Code)
	assert.Equal(t, int64(2), f.runs.Load(), "a refusal is not cached: the client retries with the same key")
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM idempotency_cache WHERE tenant_id = $1`, f.tenantID).Scan(&n))
	assert.Zero(t, n, "and its claim is gone")
}

func TestIdempotency_DeleteMethod(t *testing.T) {
	f := newIdemFixture(t)
	assert.Equal(t, http.StatusOK, f.do("DELETE", "/test", "del", "").Code)
	w := f.do("DELETE", "/test", "del", "")
	assert.Equal(t, "true", w.Header().Get(idempotencyReplayHeader))
	assert.Equal(t, int64(1), f.runs.Load())
}

// A row written before migration 119 has no request hash: method and path
// decide, as before.
func TestIdempotency_LegacyRowMethodMismatch(t *testing.T) {
	f := newIdemFixture(t)
	_, err := f.db.Exec(`INSERT INTO idempotency_cache (tenant_id, idempotency_key, method, path, response_status, response_headers, response_body)
		VALUES ($1, 'legacy', 'DELETE', '/test', 200, '{"Content-Type":"application/json"}', '{"deleted":"true"}')`, f.tenantID)
	require.NoError(t, err)
	w := f.do("POST", "/test", "legacy", `{}`)
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "idempotency_key_reuse", idemErrCode(t, w))
	w = f.do("DELETE", "/test", "legacy", "")
	assert.Equal(t, "true", w.Header().Get(idempotencyReplayHeader), "the same method and path replays")
	assert.Zero(t, f.runs.Load())
}

// WP-R11-2 (R11-08, R10-23).
func TestIdempotency_ClaimFirstAndBodyHash(t *testing.T) {
	t.Run("two concurrent requests with one key: one runs", func(t *testing.T) {
		f := newIdemFixture(t)
		first := make(chan *httptest.ResponseRecorder, 1)
		go func() { first <- f.do("POST", "/slow", "race", `{"n":1}`) }()
		select {
		case <-f.started:
		case <-time.After(5 * time.Second):
			t.Fatal("the first request never reached the handler")
		}

		w := f.do("POST", "/slow", "race", `{"n":1}`)
		assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Equal(t, "idempotency_in_progress", idemErrCode(t, w))

		close(f.release)
		require.Equal(t, http.StatusCreated, (<-first).Code)
		assert.Equal(t, int64(1), f.runs.Load(), "the handler ran once")

		w = f.do("POST", "/slow", "race", `{"n":1}`)
		assert.Equal(t, http.StatusCreated, w.Code, "after the first finished: its answer")
		assert.Equal(t, "true", w.Header().Get(idempotencyReplayHeader))
	})

	t.Run("many at once: exactly one runs", func(t *testing.T) {
		f := newIdemFixture(t)
		close(f.release)
		var wg sync.WaitGroup
		codes := make(chan int, 16)
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes <- f.do("POST", "/slow", "burst", `{}`).Code
			}()
		}
		wg.Wait()
		close(codes)
		for c := range codes {
			assert.Contains(t, []int{http.StatusCreated, http.StatusConflict}, c)
		}
		assert.Equal(t, int64(1), f.runs.Load())
	})

	t.Run("the same key with another body", func(t *testing.T) {
		f := newIdemFixture(t)
		require.Equal(t, http.StatusCreated, f.do("POST", "/test", "body", `{"amount":1}`).Code)
		w := f.do("POST", "/test", "body", `{"amount":1000}`)
		assert.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
		assert.Equal(t, "idempotency_key_reused", idemErrCode(t, w))
		w = f.do("DELETE", "/test", "body", "")
		assert.Equal(t, http.StatusUnprocessableEntity, w.Code, "another method is another request")
		assert.Equal(t, int64(1), f.runs.Load())
	})

	t.Run("a cached answer stores no secret", func(t *testing.T) {
		f := newIdemFixture(t)
		w := f.do("POST", "/keys", "mint", `{}`)
		require.Equal(t, http.StatusCreated, w.Code)
		assert.Contains(t, w.Body.String(), "SK-live-secret", "the first answer carries the secret")

		var stored []byte
		require.NoError(t, f.db.QueryRow(`SELECT response_body FROM idempotency_cache WHERE tenant_id = $1 AND idempotency_key = 'mint'`, f.tenantID).Scan(&stored))
		for _, s := range []string{"SK-live-secret", "sts-secret", `"tok"`, `"x"`, `"y"`, `"z"`} {
			assert.NotContains(t, string(stored), s, "never at rest")
		}

		w = f.do("POST", "/keys", "mint", `{}`)
		require.Equal(t, http.StatusCreated, w.Code, "the original status")
		var replay map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &replay))
		assert.Equal(t, true, replay["idempotent_replay"])
		assert.Equal(t, "", replay["secret"])
		assert.Equal(t, "VKAAAA", replay["key_id"], "the rest of the answer stays")
		nested := replay["nested"].(map[string]any)
		assert.Equal(t, "", nested["secret_key"])
		assert.Equal(t, "", nested["session_token"])
		assert.Equal(t, "", nested["secretAccessKey"])
		k := replay["keys"].([]any)[0].(map[string]any)
		assert.Equal(t, "", k["secret_access_key"])
		assert.Equal(t, "", k["token"])
		assert.Equal(t, "kept", k["name"])
		assert.False(t, bytes.Contains(w.Body.Bytes(), []byte("SK-live-secret")))
		assert.Equal(t, int64(1), f.runs.Load())
	})

	t.Run("a claim left by a process that died is taken over", func(t *testing.T) {
		f := newIdemFixture(t)
		_, err := f.db.Exec(`INSERT INTO idempotency_cache (tenant_id, idempotency_key, method, path, response_status, request_digest, claimed, created_at)
			VALUES ($1, 'stale', 'POST', '/test', 409, 'h', TRUE, NOW() - INTERVAL '1 hour')`, f.tenantID)
		require.NoError(t, err)
		w := f.do("POST", "/test", "stale", `{}`)
		assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		assert.Equal(t, int64(1), f.runs.Load())
	})

	t.Run("an expired answer is not replayed", func(t *testing.T) {
		f := newIdemFixture(t)
		require.Equal(t, http.StatusCreated, f.do("POST", "/test", "old", `{}`).Code)
		_, err := f.db.Exec(`UPDATE idempotency_cache SET created_at = NOW() - INTERVAL '25 hours' WHERE tenant_id = $1`, f.tenantID)
		require.NoError(t, err)
		w := f.do("POST", "/test", "old", `{}`)
		assert.Equal(t, http.StatusCreated, w.Code)
		assert.Empty(t, w.Header().Get(idempotencyReplayHeader))
		assert.Equal(t, int64(2), f.runs.Load())
	})
}
