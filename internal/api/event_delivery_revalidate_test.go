package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Prompt 2a.3 H2 — the post-merge review of #645. "Before" numbers are the
// same scenarios run against 7bd5c5c.

func TestEventDeliveries_ATenantWithNothingQueuedIsAcceptedWhenOthersFillThePool(t *testing.T) {
	// Arrange: the production sizes; four tenants each keep a full share
	// queued behind workers that never finish. Before: 4 × 2,048 = 8,192
	// filled the queue and a fifth tenant's single job was dropped
	// `overloaded` (dropped 1, accepted 0).
	pool := newDeliveryPool(deliveryWorkers, deliveryQueueSize)
	release := make(chan struct{})
	t.Cleanup(func() { close(release); pool.drain(time.Second) })
	block := func(tenant string) deliveryJob {
		return deliveryJob{tenant: tenant, kind: deliveryKindNotification, run: func(context.Context) { <-release }}
	}
	fill := func(tenant string, n int) {
		jobs := make([]deliveryJob, n)
		for i := range jobs {
			jobs[i] = block(tenant)
		}
		pool.submit(jobs...)
	}
	tenants := []string{"t-a-" + uuid.NewString(), "t-b-" + uuid.NewString(), "t-c-" + uuid.NewString(), "t-d-" + uuid.NewString()}
	for _, tn := range tenants {
		fill(tn, deliveryQueueSize/4+deliveryWorkers)
	}
	require.Eventually(t, func() bool { return pool.pending()-pool.queuedLen() == deliveryWorkers }, 5*time.Second, 10*time.Millisecond)
	for _, tn := range tenants {
		fill(tn, deliveryQueueSize/4) // top every share up again
	}
	dropped := func() float64 {
		return testutil.ToFloat64(deliveriesDropped.WithLabelValues(deliveryKindNotification, "overloaded"))
	}
	before, accepted := dropped(), pool.accepted.Load()

	// Act: a fifth tenant's one notification.
	pool.submit(block("t-e-" + uuid.NewString()))

	// Assert
	assert.Equal(t, before, dropped(), "the newcomer is never dropped")
	assert.Equal(t, accepted+1, pool.accepted.Load())
	assert.LessOrEqual(t, pool.queuedLen(), deliveryQueueSize, "the queue stays bounded")
}

// webhookRowsFor counts one endpoint's delivery rows.
func webhookRowsFor(t *testing.T, db *sql.DB, hook, status, bodyLike string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM webhook_deliveries WHERE webhook_id = $1 AND status = $2 AND response_body LIKE $3`,
		hook, status, bodyLike).Scan(&n))
	return n
}

func (f *batchFanoutFixture) webhook(url, filter string) string {
	f.t.Helper()
	id := uuid.New().String()
	_, err := f.db.Exec(`INSERT INTO webhook_endpoints (id, tenant_id, url, event_filter, secret) VALUES ($1, $2, $3, ARRAY[$4], 'whsec_test')`,
		id, f.tenantID, url, filter)
	require.NoError(f.t, err)
	return id
}

func (f *batchFanoutFixture) event() string {
	f.t.Helper()
	id := uuid.New().String()
	_, err := f.db.Exec(`INSERT INTO events (id, type, tenant_id, data) VALUES ($1, 'object.deleted', $2, '{}')`, id, f.tenantID)
	require.NoError(f.t, err)
	return id
}

func TestEventDeliveries_ADroppedRowNamingAGoneWebhookOrEventCostsNoOtherRow(t *testing.T) {
	// Arrange: one batch of dropped rows across tenants — A's are valid, one
	// names a webhook deleted meanwhile, one an event the retention removed.
	// Before: webhook_deliveries_webhook_id_fkey failed the statement and 0
	// rows landed (at the drain: every other tenant's `dropped: shutdown`).
	a := setupBatchFanoutFixture(t)
	b := setupBatchFanoutFixture(t)
	hookA := a.webhook("https://a.example/hook", "object.deleted")
	evA := a.event()
	gone := b.webhook("https://b.example/hook", "object.deleted")
	evB := b.event()
	_, err := b.db.Exec(`DELETE FROM webhook_endpoints WHERE id = $1`, gone)
	require.NoError(t, err)
	rows := []droppedDelivery{
		{webhookID: hookA, eventID: evA},
		{webhookID: gone, eventID: evB},
		{webhookID: hookA, eventID: uuid.NewString()}, // the event is gone
		{webhookID: hookA, eventID: evA},
	}

	// Act
	filtered, err := insertDroppedDeliveries(context.Background(), a.db, rows, deliveryDroppedShutdown)

	// Assert
	require.NoError(t, err)
	assert.Equal(t, 2, filtered)
	assert.Equal(t, 2, webhookRowsFor(t, a.db, hookA, "failed", deliveryDroppedShutdown))
}

// heldPool is a one-worker pool whose worker is held by a job of the
// tenant until release is closed: what is queued behind it waits.
func heldPool(t *testing.T, tenant string) (*deliveryPool, chan struct{}) {
	t.Helper()
	pool := newDeliveryPool(1, 64)
	useDeliveryPool(t, pool)
	release := make(chan struct{})
	pool.submit(deliveryJob{tenant: tenant, kind: deliveryKindNotification, run: func(context.Context) { <-release }})
	require.Eventually(t, func() bool { return pool.queuedLen() == 0 && pool.pending() == 1 }, 5*time.Second, 5*time.Millisecond)
	return pool, release
}

func countingHook(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestEventDeliveries_AWebhookDeletedWhileItsJobIsQueuedGetsNoPOST(t *testing.T) {
	// Before: the job POSTed with the URL and secret it captured at submit
	// (1 POST to a webhook the customer had deleted; up to ~40 min late
	// behind a blackholed target on prod).
	f := setupBatchFanoutFixture(t)
	webhookAllowPrivateTargets.Store(true)
	t.Cleanup(func() { webhookAllowPrivateTargets.Store(false) })
	srv, posts := countingHook(t)
	hook := f.webhook(srv.URL+"/hook", "object.deleted")
	pool, release := heldPool(t, f.tenantID)
	emitEvent(context.Background(), f.db, zap.NewNop(), "object.deleted", f.tenantID, map[string]interface{}{"key": "k"})
	require.Equal(t, 1, pool.queuedLen())
	removed := testutil.ToFloat64(deliveriesDropped.WithLabelValues(deliveryKindWebhook, "removed"))

	// Act: the customer deletes the webhook (the route's two steps), then the job runs.
	_, err := f.db.Exec(`DELETE FROM webhook_endpoints WHERE id = $1`, hook)
	require.NoError(t, err)
	forgetWebhookEndpoints(f.tenantID)
	close(release)
	require.Eventually(t, func() bool { return pool.pending() == 0 }, 5*time.Second, 5*time.Millisecond)

	// Assert
	assert.Equal(t, int32(0), posts.Load())
	assert.Equal(t, removed+1, testutil.ToFloat64(deliveriesDropped.WithLabelValues(deliveryKindWebhook, "removed")))
}

func TestEventDeliveries_AWebhookDisabledWhileItsJobIsQueuedIsRecordedSkipped(t *testing.T) {
	// Before: 1 POST, recorded `delivered`.
	f := setupBatchFanoutFixture(t)
	webhookAllowPrivateTargets.Store(true)
	t.Cleanup(func() { webhookAllowPrivateTargets.Store(false) })
	srv, posts := countingHook(t)
	hook := f.webhook(srv.URL+"/hook", "object.deleted")
	pool, release := heldPool(t, f.tenantID)
	emitEvent(context.Background(), f.db, zap.NewNop(), "object.deleted", f.tenantID, map[string]interface{}{"key": "k"})

	_, err := f.db.Exec(`UPDATE webhook_endpoints SET enabled = FALSE WHERE id = $1`, hook)
	require.NoError(t, err)
	forgetWebhookEndpoints(f.tenantID)
	close(release)
	require.Eventually(t, func() bool { return pool.pending() == 0 }, 5*time.Second, 5*time.Millisecond)

	assert.Equal(t, int32(0), posts.Load())
	assert.Equal(t, 1, webhookRowsFor(t, f.db, hook, "failed", "skipped: endpoint removed"))
}

func TestEventDeliveries_AWebhookRepointedWhileItsJobIsQueuedPostsToTheNewURL(t *testing.T) {
	// Before: the POST went to the old URL (old 1, new 0).
	f := setupBatchFanoutFixture(t)
	webhookAllowPrivateTargets.Store(true)
	t.Cleanup(func() { webhookAllowPrivateTargets.Store(false) })
	oldSrv, oldPosts := countingHook(t)
	newSrv, newPosts := countingHook(t)
	hook := f.webhook(oldSrv.URL+"/hook", "object.deleted")
	pool, release := heldPool(t, f.tenantID)
	emitEvent(context.Background(), f.db, zap.NewNop(), "object.deleted", f.tenantID, map[string]interface{}{"key": "k"})

	_, err := f.db.Exec(`UPDATE webhook_endpoints SET url = $2 WHERE id = $1`, hook, newSrv.URL+"/hook")
	require.NoError(t, err)
	forgetWebhookEndpoints(f.tenantID)
	close(release)
	require.Eventually(t, func() bool { return pool.pending() == 0 }, 5*time.Second, 5*time.Millisecond)

	assert.Equal(t, int32(0), oldPosts.Load())
	assert.Equal(t, int32(1), newPosts.Load())
}

func TestEventTargets_ALoadThatRacesAnInvalidationIsNotCached(t *testing.T) {
	// A load reads the rows, the webhook CRUD invalidates, then the load
	// stores what it read. Before: the stale list was served for 15 s.
	tenant := "t-race-" + uuid.NewString()
	gen := webhookGeneration(tenant)
	forgetWebhookEndpoints(tenant) // the CRUD lands while the load is in flight

	rememberWebhookEndpoints(tenant, gen, []webhookEndpoint{{id: "stale"}}, nil)

	deliveryTargets.mu.Lock()
	_, cached := deliveryTargets.webhooks[tenant]
	deliveryTargets.mu.Unlock()
	assert.False(t, cached)
}

func TestEventTargets_AFailedLookupIsCachedBriefly(t *testing.T) {
	// A lookup that fails (here: a tenant id the server refuses as text) is
	// answered from the cache for deliveryTargetsFailureTTL. Before: every
	// write ran the failing lookup again — 3 writes, 3 + 3 queries (on prod
	// each one can take the request path's 2 s cap against a slow database).
	f := setupBatchFanoutFixture(t)
	bad := "t-\xff-" + uuid.NewString()
	notify := NewNotificationDispatcher(f.db, zap.NewNop())
	f.log.reset()

	for i := 0; i < 3; i++ {
		_, _, err := tenantWebhookEndpoints(context.Background(), f.db, zap.NewNop(), bad)
		assert.Error(t, err)
		_, err = notify.bucketNotifyTargets(context.Background(), bad, "b")
		assert.Error(t, err)
	}

	assert.Equal(t, 1, f.log.count("webhook_endpoints"))
	assert.Equal(t, 1, f.log.count("bucket_notifications"))
}

func TestShutdown_DeliveryDrainUsesTheConfiguredLongOpBound(t *testing.T) {
	// LONG_OP_DRAIN_BOUND=1s: the delivery drain gets what is left of THAT
	// budget. Before: the constant 15 min — the drain waited its full 30 s.
	pool := newDeliveryPool(1, 8)
	useDeliveryPool(t, pool)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	pool.submit(deliveryJob{tenant: "t", kind: deliveryKindNotification, run: func(ctx context.Context) {
		select {
		case <-release:
		case <-ctx.Done():
		}
	}})
	s := &Server{logger: zap.NewNop()}

	start := time.Now()
	s.drainDeliveries(start, time.Second)

	assert.Less(t, time.Since(start), 5*time.Second)
}

// queuedLen is how many jobs wait in the pool (all tenants).
func (p *deliveryPool) queuedLen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.queued
}
