package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-DX-1 PR B: an event of a sandbox is recorded under — and delivered to
// the webhooks of — its PARENT, with "livemode": false in the body (Stripe's
// shape); a live event says true. A retry rebuilds the body from the row:
// the same bytes, the same mode.
func TestWebhook_LivemodeFalseFromASandboxTenant(t *testing.T) {
	hook := newScriptedHook(t, 500, 200)
	f := setupOutboxFixture(t, hook.srv.URL+"/hook")
	sandbox := "sandbox-lm-" + f.tenantID[len(f.tenantID)-8:]
	_, err := f.db.Exec(`INSERT INTO tenants (id, name, email, sandbox_of) VALUES ($1, 'test data', $2, $3)`, sandbox, "sandbox+"+sandbox+"@sandbox.invalid", f.tenantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM events WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM tenants WHERE id = $1`, sandbox)
	})

	emitEvent(context.Background(), f.db, zap.NewNop(), "object.deleted", sandbox, map[string]interface{}{"key": "test-k"})
	f.settle(t)
	_, first := hook.last()
	var body map[string]any
	require.NoError(t, json.Unmarshal(first, &body))
	assert.Equal(t, false, body["livemode"], "a sandbox's event")
	assert.Equal(t, f.tenantID, body["tenant_id"], "delivered as the parent's")
	var stored bool
	require.NoError(t, f.db.QueryRow(`SELECT livemode FROM events WHERE id = $1`, body["id"]).Scan(&stored))
	assert.False(t, stored)

	// The retry (the 500 above) sends the same body, livemode false.
	f.clock.advance(webhookBackoff[0] + time.Second)
	require.Equal(t, 1, f.retry(t).Submitted)
	f.settle(t)
	_, second := hook.last()
	assert.JSONEq(t, string(first), string(second))

	// A live event of the parent.
	emitEvent(context.Background(), f.db, zap.NewNop(), "object.deleted", f.tenantID, map[string]interface{}{"key": "live-k"})
	f.settle(t)
	_, live := hook.last()
	require.NoError(t, json.Unmarshal(live, &body))
	assert.Equal(t, true, body["livemode"])
}

// GET /api/v1/events?after=<id>&wait=<s> (WP-DX-1, `stored listen`): the
// events after one, oldest first, each with its livemode; nothing new is a
// long poll that answers when one arrives (or at wait); after=now answers
// the newest event's id as the cursor to start from.
func TestEvents_AfterIsAForwardLongPoll(t *testing.T) {
	hook := newScriptedHook(t, 200)
	f := setupOutboxFixture(t, hook.srv.URL+"/hook")
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM events WHERE tenant_id = $1`, f.tenantID) })
	s := &Server{logger: zap.NewNop(), db: f.db}
	list := func(q string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/events?"+q, nil)
		req = req.WithContext(context.WithValue(req.Context(), tenantIDKey, f.tenantID))
		w := httptest.NewRecorder()
		s.handleListEvents(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	emit := func(k string) {
		emitEvent(context.Background(), f.db, zap.NewNop(), "object.deleted", f.tenantID, map[string]interface{}{"key": k})
	}
	emit("before")
	code, start := list("after=now")
	require.Equal(t, http.StatusOK, code)
	cursor, _ := start["next_cursor"].(string)
	require.NotEmpty(t, cursor)
	assert.Empty(t, start["data"], "now: nothing old is replayed")

	emit("one")
	emit("two")
	_, got := list("after=" + cursor + "&wait=5") // answered past the settle horizon (RP80-3)
	items := got["data"].([]any)
	require.Len(t, items, 2)
	assert.Equal(t, "one", items[0].(map[string]any)["data"].(map[string]any)["key"], "oldest first")
	assert.Equal(t, true, items[0].(map[string]any)["livemode"])
	cursor = got["next_cursor"].(string)

	go func() {
		time.Sleep(700 * time.Millisecond)
		emit("late")
	}()
	began := time.Now()
	_, got = list("after=" + cursor + "&wait=5")
	assert.Less(t, time.Since(began), 4*time.Second, "answered when the event arrived")
	require.Len(t, got["data"].([]any), 1)

	began = time.Now()
	_, got = list("after=" + got["next_cursor"].(string) + "&wait=1")
	assert.GreaterOrEqual(t, time.Since(began), 900*time.Millisecond, "a long poll")
	assert.Empty(t, got["data"])
}

// RP80-3: the forward cursor never skips an event that commits after a
// later-stamped one — an event is answered only once it is older than the
// settle horizon, so a slower commit with an earlier created_at is still
// ahead of the cursor when it is read.
func TestEvents_AfterNeverSkipsALateCommit(t *testing.T) {
	hook := newScriptedHook(t, 200)
	f := setupOutboxFixture(t, hook.srv.URL+"/hook")
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM events WHERE tenant_id = $1`, f.tenantID) })
	s := &Server{logger: zap.NewNop(), db: f.db}
	list := func(q string) map[string]any {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/events?"+q, nil)
		req = req.WithContext(context.WithValue(req.Context(), tenantIDKey, f.tenantID))
		w := httptest.NewRecorder()
		s.handleListEvents(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	insert := func(id, key string, at string) {
		_, err := f.db.Exec(`INSERT INTO events (id, tenant_id, type, data, created_at) VALUES ($1, $2, 'object.deleted', $3, `+at+`)`,
			id, f.tenantID, `{"key":"`+key+`"}`)
		require.NoError(t, err)
	}
	c0, a, b := uuid.NewString(), uuid.NewString(), uuid.NewString()
	insert(c0, "cursor", "NOW() - interval '10 seconds'")
	// B commits first, stamped now.
	insert(b, "b", "NOW()")
	seen := map[string]bool{}
	got := list("after=" + c0)
	for _, it := range got["data"].([]any) {
		seen[it.(map[string]any)["id"].(string)] = true
	}
	cursor := got["next_cursor"].(string)
	// A commits after B with an EARLIER stamp (its transaction began first).
	insert(a, "a", "NOW() - interval '500 milliseconds'")
	for i := 0; i < 3 && !(seen[a] && seen[b]); i++ {
		got = list("after=" + cursor + "&wait=5")
		for _, it := range got["data"].([]any) {
			seen[it.(map[string]any)["id"].(string)] = true
		}
		cursor = got["next_cursor"].(string)
	}
	assert.True(t, seen[a], "the late commit is delivered")
	assert.True(t, seen[b])
}

// RP80-4: at most four long polls of one tenant wait at once; the fifth is
// 429 too_many_listeners with Retry-After.
func TestEvents_LongPollsBoundedPerTenant(t *testing.T) {
	hook := newScriptedHook(t, 200)
	f := setupOutboxFixture(t, hook.srv.URL+"/hook")
	s := &Server{logger: zap.NewNop(), db: f.db}
	poll := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/events?after=now&wait=3", nil)
		req = req.WithContext(context.WithValue(req.Context(), tenantIDKey, f.tenantID))
		w := httptest.NewRecorder()
		s.handleListEvents(w, req)
		return w
	}
	var wg sync.WaitGroup
	codes := make([]int, eventsMaxWaitsPerTenant)
	for i := range codes {
		wg.Add(1)
		go func(i int) { defer wg.Done(); codes[i] = poll().Code }(i)
	}
	time.Sleep(500 * time.Millisecond)
	w := poll()
	assert.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "too_many_listeners")
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
	wg.Wait()
	for _, c := range codes {
		assert.Equal(t, http.StatusOK, c)
	}
	assert.Equal(t, http.StatusOK, poll().Code, "a slot is free again")
}
