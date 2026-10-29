package api

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"
)

// WP-R9-5 (R2-17, R6-15, R9-10): object_head_cache.last_accessed — read by
// the smart_demotion job, which counts in days — was touched by every
// cache-hit GET with its own UPDATE and goroutine (and every edge read, and
// the CDN's once a day). The touches are kept in memory per key (the latest
// time wins) and written by ONE statement, UPDATE … FROM unnest(…), at most
// accessTouchEvery after the first pending one; a stopping process writes
// what it holds (Server.flushTrackers). A value is never moved back: the
// row keeps a later one. A crash loses at most a minute of touches — an
// object read in that minute looks idle a minute longer.

// accessTouchEvery is how long a touch may wait before it is written.
const accessTouchEvery = 60 * time.Second

// accessTouchMaxPending writes early when this many keys wait (a burst over
// many objects): the map stays bounded.
const accessTouchMaxPending = 20000

type touchKey struct{ tenant, bucket, key string }

// accessToucher coalesces the last_accessed touches of one database.
type accessToucher struct {
	db     *sql.DB
	logger *zap.Logger
	every  time.Duration

	mu      sync.Mutex
	pending map[touchKey]time.Time
	armed   bool // a flush is scheduled

	updates atomic.Int64 // statements written (tests)
}

// accessTouchers holds one toucher per database: the S3 adapter is built
// per request, and every adapter of a database shares it.
var accessTouchers sync.Map // *sql.DB → *accessToucher

// accessToucherFor is the toucher of db (nil for no database).
func accessToucherFor(db *sql.DB, logger *zap.Logger) *accessToucher {
	if db == nil {
		return nil
	}
	if v, ok := accessTouchers.Load(db); ok {
		return v.(*accessToucher)
	}
	v, _ := accessTouchers.LoadOrStore(db, newAccessToucher(db, logger, accessTouchEvery))
	return v.(*accessToucher)
}

func newAccessToucher(db *sql.DB, logger *zap.Logger, every time.Duration) *accessToucher {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &accessToucher{db: db, logger: logger, every: every, pending: map[touchKey]time.Time{}}
}

// Touch records a read of the key now. Nil-safe.
func (t *accessToucher) Touch(tenant, bucket, key string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.pending[touchKey{tenant, bucket, key}] = time.Now()
	n := len(t.pending)
	arm := !t.armed
	t.armed = true
	t.mu.Unlock()
	switch {
	case n >= accessTouchMaxPending:
		go t.Flush(context.Background()) // #nosec G118 -- the touches outlive the request that made them
	case arm:
		time.AfterFunc(t.every, func() { t.Flush(context.Background()) })
	}
}

// pendingFor counts the keys of tenant waiting to be written (tests).
func (t *accessToucher) pendingFor(tenant string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for k := range t.pending {
		if k.tenant == tenant {
			n++
		}
	}
	return n
}

// Flush writes every pending touch in one statement. A failure is a Warn:
// the touches are an approximation the demotion job reads in days.
func (t *accessToucher) Flush(ctx context.Context) {
	if t == nil {
		return
	}
	t.mu.Lock()
	batch := t.pending
	t.pending = map[touchKey]time.Time{}
	t.armed = false
	t.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	tenants := make([]string, 0, len(batch))
	buckets := make([]string, 0, len(batch))
	keys := make([]string, 0, len(batch))
	ats := make([]time.Time, 0, len(batch))
	for k, at := range batch {
		tenants, buckets, keys, ats = append(tenants, k.tenant), append(buckets, k.bucket), append(keys, k.key), append(ats, at.UTC())
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	t.updates.Add(1)
	if _, err := t.db.ExecContext(ctx, `
		UPDATE object_head_cache h SET last_accessed = u.at
		FROM unnest($1::text[], $2::text[], $3::text[], $4::timestamptz[]) AS u(tenant_id, bucket, object_key, at)
		WHERE h.tenant_id = u.tenant_id AND h.bucket = u.bucket AND h.object_key = u.object_key
		  AND (h.last_accessed IS NULL OR h.last_accessed < u.at)`,
		pq.Array(tenants), pq.Array(buckets), pq.Array(keys), pq.Array(ats)); err != nil {
		t.logger.Warn("last_accessed touches not written", zap.Int("keys", len(batch)), zap.Error(err))
	}
}
