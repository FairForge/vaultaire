package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/webhooksig"
)

// fakeEventLog is GET /api/v1/events?after=&wait= over an in-memory log.
type fakeEventLog struct {
	mu     sync.Mutex
	events []map[string]any
	token  string
}

func (l *fakeEventLog) add(typ string, livemode bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.events) + 1
	l.events = append(l.events, map[string]any{"object": "event", "id": fmt.Sprintf("evt-%03d", n), "type": typ,
		"data": map[string]any{"n": n}, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "livemode": livemode})
}

func (l *fakeEventLog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v1/events" || r.Header.Get("Authorization") != "Bearer "+l.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	after := r.URL.Query().Get("after")
	l.mu.Lock()
	defer l.mu.Unlock()
	if after == "now" {
		after = ""
		if len(l.events) > 0 {
			after = l.events[len(l.events)-1]["id"].(string)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{}, "next_cursor": after})
		return
	}
	var out []any
	next := after
	for _, e := range l.events {
		if e["id"].(string) > after {
			out = append(out, e)
			next = e["id"].(string)
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": out, "next_cursor": next})
}

type receiver struct {
	mu     sync.Mutex
	bodies [][]byte
	sigs   []string
	ids    []string
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.bodies = append(rc.bodies, b)
	rc.sigs = append(rc.sigs, r.Header.Get("X-Webhook-Signature"))
	rc.ids = append(rc.ids, r.Header.Get("X-Event-ID"))
	rc.mu.Unlock()
}

// TestListen_ReplaysToLocalhost: `stored listen` posts each new event of
// the account's log to the local endpoint once, signed with the listen
// secret, in order; a restart resumes from its cursor file.
func TestListen_ReplaysToLocalhost(t *testing.T) {
	log := &fakeEventLog{token: "jwt-test"}
	log.add("object.created", true) // before the listener: never replayed
	api := httptest.NewServer(log)
	defer api.Close()
	rc := &receiver{}
	local := httptest.NewServer(rc)
	defer local.Close()
	cursor := filepath.Join(t.TempDir(), "cursor")
	newListener := func() *listener {
		return &listener{api: api.URL, token: "jwt-test", forward: strings.TrimPrefix(local.URL, "http://") + "/hook",
			cursorFile: cursor, secret: "whsec_local", client: api.Client(), out: io.Discard, wait: time.Second}
	}
	ctx := context.Background()

	l := newListener()
	n, err := l.once(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "the first poll starts at now")
	log.add("object.created", false)
	log.add("object.deleted", false)
	n, err = l.once(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	require.Equal(t, []string{"evt-002", "evt-003"}, rc.ids, "in order, once each")
	for i, b := range rc.bodies {
		require.NoError(t, webhooksig.Verify(rc.sigs[i], b, "whsec_local", time.Now()), "signed")
		var ev map[string]any
		require.NoError(t, json.Unmarshal(b, &ev))
		assert.Equal(t, false, ev["livemode"])
	}

	// A restart resumes from the cursor: nothing re-sent, the next one sent.
	l = newListener()
	n, err = l.once(ctx)
	require.NoError(t, err)
	assert.Zero(t, n)
	log.add("key.created", true)
	n, err = l.once(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{"evt-002", "evt-003", "evt-004"}, rc.ids)
}

// The forward target must be a local address: listen never relays an
// account's events to another host.
func TestListen_ForwardsOnlyToLocalhost(t *testing.T) {
	for _, bad := range []string{"example.com/hook", "10.0.0.5:3000/x", "http://evil.test/x"} {
		_, err := forwardURL(bad)
		assert.Error(t, err, bad)
	}
	for _, ok := range []string{"localhost:3000/hook", "127.0.0.1:8080/", "http://localhost:4242/webhooks", "[::1]:3000/x"} {
		u, err := forwardURL(ok)
		require.NoError(t, err, ok)
		assert.True(t, strings.HasPrefix(u, "http://"), u)
	}
}
