package api

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R9-5 (R2-17, R6-15, R9-10): a cache-hit GET touched
// object_head_cache.last_accessed with one UPDATE and one goroutine each.
// The touches are coalesced per key in memory and written by one
// UPDATE … FROM unnest(…) within accessTouchEvery (60 s) of the first, and
// a stopping process writes what it holds.
func TestLastAccessed_CoalescedPerKey(t *testing.T) {
	f := setupAdapterFixture(t)
	body := bytes.Repeat([]byte("t"), 1000)
	require.Equal(t, http.StatusOK, adapterPut(t, f, "hot.bin", body, nil).Code)
	lastAccessed := func(key string) sql.NullTime {
		var at sql.NullTime
		require.NoError(t, f.db.QueryRow(`SELECT last_accessed FROM object_head_cache WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`,
			f.tenantID, key).Scan(&at))
		return at
	}
	_, err := f.db.Exec(`UPDATE object_head_cache SET last_accessed = NOW() - INTERVAL '30 days' WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	before := lastAccessed("hot.bin")

	toucher := accessToucherFor(f.db, zap.NewNop())
	require.Same(t, toucher, f.adapter.touch, "every adapter of a database shares one")
	assert.Equal(t, 60*time.Second, toucher.every, "the value lands within 60 s")
	updates := toucher.updates.Load()

	for range 100 {
		req := httptest.NewRequest("GET", "/test-bucket/hot.bin", nil).WithContext(s3Ctx(context.Background(), f.tenant))
		w := httptest.NewRecorder()
		f.adapter.HandleGet(w, req, "test-bucket", "hot.bin")
		require.Equal(t, http.StatusOK, w.Code)
	}
	assert.Equal(t, updates, toucher.updates.Load(), "no UPDATE per GET")
	assert.Equal(t, 1, toucher.pendingFor(f.tenantID), "one pending touch for the key")
	assert.Equal(t, before, lastAccessed("hot.bin"), "not written yet")

	// A process stop writes what it holds (Server.flushTrackers).
	(&Server{db: f.db, logger: zap.NewNop()}).flushTrackers(context.Background())
	assert.Equal(t, updates+1, toucher.updates.Load(), "at most one UPDATE for the 100 GETs")
	after := lastAccessed("hot.bin")
	require.True(t, after.Valid)
	assert.WithinDuration(t, time.Now(), after.Time, time.Minute)
	assert.Zero(t, toucher.pendingFor(f.tenantID))

	t.Run("the timer writes within its interval", func(t *testing.T) {
		fast := newAccessToucher(f.db, zap.NewNop(), 50*time.Millisecond)
		_, err := f.db.Exec(`UPDATE object_head_cache SET last_accessed = NOW() - INTERVAL '30 days' WHERE tenant_id = $1`, f.tenantID)
		require.NoError(t, err)
		old := lastAccessed("hot.bin")
		for range 10 {
			fast.Touch(f.tenantID, "test-bucket", "hot.bin")
		}
		require.Eventually(t, func() bool { return lastAccessed("hot.bin").Time.After(old.Time.Add(24 * time.Hour)) },
			5*time.Second, 20*time.Millisecond)
		assert.Equal(t, int64(1), fast.updates.Load(), "one UPDATE for ten touches")
	})

	t.Run("a touch never moves the value back", func(t *testing.T) {
		toucher.mu.Lock()
		toucher.pending[touchKey{f.tenantID, "test-bucket", "hot.bin"}] = time.Now().Add(-48 * time.Hour)
		toucher.mu.Unlock()
		cur := lastAccessed("hot.bin")
		toucher.Flush(context.Background())
		assert.Equal(t, cur, lastAccessed("hot.bin"))
	})
}
