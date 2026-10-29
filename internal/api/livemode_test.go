package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
	_, got := list("after=" + cursor)
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
