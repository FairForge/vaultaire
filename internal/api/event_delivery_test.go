package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Post-merge review of 2a (Prompt 2a.2 G2, 2026-10-08): the batch fanout of
// #637 shared one 10 s context between a key's notification and its webhook
// (two hanging notification targets used it up: no webhook POST, no
// webhook_deliveries row), bounded its workers per batch only (a blackholed
// target and concurrent batches piled up without limit), and lost every
// pending delivery on a deploy without a log line.

// hangingTarget answers nothing until the test ends.
func hangingTarget(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	release := make(chan struct{})
	var inFlight atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inFlight.Add(1)
		defer inFlight.Add(-1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs first: lets Close return
	return srv, &inFlight
}

// useDeliveryPool installs p as the process-wide delivery pool for one test.
func useDeliveryPool(t *testing.T, p *deliveryPool) {
	t.Helper()
	prev := eventDeliveries
	eventDeliveries = p
	t.Cleanup(func() {
		p.drain(0)
		eventDeliveries = prev
	})
}

func (f *batchFanoutFixture) webhookRows(status, bodyLike string) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.db.QueryRow(`
		SELECT count(*) FROM webhook_deliveries d JOIN webhook_endpoints e ON e.id = d.webhook_id
		WHERE e.tenant_id = $1 AND d.status = $2 AND d.response_body LIKE $3`,
		f.tenantID, status, bodyLike).Scan(&n))
	return n
}

func (f *batchFanoutFixture) addNotificationTarget(url string) {
	f.t.Helper()
	_, err := f.db.Exec(`INSERT INTO bucket_notifications (tenant_id, bucket, event_filter, target_type, target_url)
		VALUES ($1, $2, 's3:ObjectRemoved:*', 'webhook', $3)`, f.tenantID, f.bucket, url)
	require.NoError(f.t, err)
}

func (f *batchFanoutFixture) addWebhook(url string) {
	f.t.Helper()
	_, err := f.db.Exec(`INSERT INTO webhook_endpoints (id, tenant_id, url, event_filter, secret)
		VALUES ($1, $2, $3, '{object.deleted}', 'whsec_test')`, uuid.New().String(), f.tenantID, url)
	require.NoError(f.t, err)
}

func TestDeleteObjects_HangingNotificationTargetsDoNotStarveTheWebhook(t *testing.T) {
	// Arrange: two notification targets that never answer, one webhook that does.
	// Before: both used up the key's one 10 s context (5 s client timeout each),
	// so the webhook got no POST and no webhook_deliveries row — 0 and 0.
	f := setupBatchFanoutFixture(t)
	webhookAllowPrivateTargets.Store(true)
	t.Cleanup(func() { webhookAllowPrivateTargets.Store(false) })
	useDeliveryPool(t, newDeliveryPool(16, 64)) // 4 running per tenant
	hang1, _ := hangingTarget(t)
	hang2, _ := hangingTarget(t)
	var hooks atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hooks.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)
	f.addNotificationTarget(hang1.URL + "/s3")
	f.addNotificationTarget(hang2.URL + "/s3")
	f.addWebhook(hook.URL + "/hook")

	// Act
	f.deleteObjects(f.objects(2))

	// Assert: the webhook is delivered and recorded for each key.
	require.Eventually(t, func() bool { return f.webhookRows("delivered", "%") == 2 }, 20*time.Second, 50*time.Millisecond)
	assert.Equal(t, int32(2), hooks.Load())
}

func TestEventDeliveries_AreBoundedProcessWideAcrossConcurrentBatches(t *testing.T) {
	// Arrange: a blackholed notification target and webhook, 20 batches of 50
	// keys at once. Before: 4 workers PER BATCH = 80 POSTs hanging at once,
	// plus a feeder goroutine per batch, and no limit on how many batches pile
	// up. Now one process-wide pool (here 8 workers, a 40-job queue): at most
	// 8 POSTs in flight (and 2 for one tenant); past the tenant's share of
	// the queue a key's webhook is recorded as failed `dropped: overloaded`
	// instead of waiting in a goroutine.
	f := setupBatchFanoutFixture(t)
	webhookAllowPrivateTargets.Store(true)
	t.Cleanup(func() { webhookAllowPrivateTargets.Store(false) })
	const workers, queue = 8, 40
	useDeliveryPool(t, newDeliveryPool(workers, queue))
	hole, inFlight := hangingTarget(t)
	f.addNotificationTarget(hole.URL + "/s3")
	f.addWebhook(hole.URL + "/hook")

	const batches, keys = 20, 50
	_, err := f.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor, is_chunked, content_type)
		SELECT $1, $2, 'b' || b || '/' || i, 1, 'etag', 'local', 'standard', false, 'application/octet-stream'
		FROM generate_series(1, $3) AS b, generate_series(1, $4) AS i`, f.tenantID, f.bucket, batches, keys)
	require.NoError(t, err)

	// Act
	var wg sync.WaitGroup
	for b := 1; b <= batches; b++ {
		wg.Add(1)
		go func(b int) {
			defer wg.Done()
			body := "<Delete>"
			for i := 1; i <= keys; i++ {
				body += fmt.Sprintf("<Object><Key>b%d/%d</Key></Object>", b, i)
			}
			f.deleteObjects(body + "</Delete>")
		}(b)
	}
	wg.Wait()
	time.Sleep(300 * time.Millisecond)

	// Assert: the wire is bounded by the pool, and everything past the queue
	// has its failure row at once (none of it waits in a goroutine).
	assert.LessOrEqual(t, int(inFlight.Load()), workers/4, "POSTs in flight for one tenant")
	dropped := f.webhookRows("failed", "dropped: overloaded")
	// 2,000 jobs (a notification + a webhook per key), at most workers +
	// queue of them accepted: every webhook job beyond that is a row.
	assert.GreaterOrEqual(t, dropped, batches*keys-(workers+queue))
	assert.LessOrEqual(t, eventDeliveries.pending(), workers+queue)
}

func TestEventDeliveries_ShutdownDrainsThenRecordsTheRestAsFailed(t *testing.T) {
	// Arrange: a blackholed webhook, 2 running for the tenant, 8 keys queued behind them.
	// Before: the pending deliveries vanished with the process — no row, no
	// log line; GET /api/v1/events showed the events with no delivery.
	f := setupBatchFanoutFixture(t)
	webhookAllowPrivateTargets.Store(true)
	t.Cleanup(func() { webhookAllowPrivateTargets.Store(false) })
	pool := newDeliveryPool(8, 64) // 2 running per tenant
	useDeliveryPool(t, pool)
	core, logs := observer.New(zap.WarnLevel)
	pool.logger = zap.New(core)
	hole, inFlight := hangingTarget(t)
	f.addWebhook(hole.URL + "/hook")
	f.deleteObjects(f.objects(10))
	require.Eventually(t, func() bool { return inFlight.Load() == 2 }, 5*time.Second, 10*time.Millisecond)

	// Act: the stop sequence gives the deliveries 200 ms.
	start := time.Now()
	n := pool.drain(200 * time.Millisecond)
	took := time.Since(start)

	// Assert: every delivery has a row — the 8 queued as dropped at shutdown,
	// the 2 in flight cut and recorded as failed — and the count is logged.
	assert.Less(t, took, 8*time.Second)
	assert.Equal(t, 8, n, "queued deliveries dropped at the bound")
	assert.Equal(t, 8, f.webhookRows("failed", "dropped: shutdown"))
	require.Eventually(t, func() bool { return f.webhookRows("failed", "%") == 10 }, 5*time.Second, 20*time.Millisecond)
	entries := logs.FilterMessage("event deliveries dropped at shutdown").All()
	require.Len(t, entries, 1)
	assert.EqualValues(t, 8, entries[0].ContextMap()["dropped"])

	// After the drain nothing new starts: a late delivery is a row, not a goroutine.
	f.deleteObjects(f.objects(1))
	assert.Equal(t, 9, f.webhookRows("failed", "dropped: shutdown"))
}

func TestEventDeliveries_OneTenantsBlackholeDoesNotDropAnotherTenantsWebhook(t *testing.T) {
	// Arrange: tenant A has a blackholed webhook and runs 10 concurrent
	// 50-key batches; tenant B has a working webhook and does one PUT. Pool:
	// 8 workers, a 64-job queue (per tenant: 2 running, 16 queued). Before:
	// A's jobs held every worker and filled the shared queue, so B's
	// delivery was dropped `overloaded` (or waited behind A's 10 s POSTs).
	a := setupBatchFanoutFixture(t)
	b := setupBatchFanoutFixture(t)
	webhookAllowPrivateTargets.Store(true)
	t.Cleanup(func() { webhookAllowPrivateTargets.Store(false) })
	useDeliveryPool(t, newDeliveryPool(8, 64))
	hole, _ := hangingTarget(t)
	a.addWebhook(hole.URL + "/hook")
	var bHooks atomic.Int32
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHooks.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(ok.Close)
	_, err := b.db.Exec(`INSERT INTO webhook_endpoints (id, tenant_id, url, event_filter, secret)
		VALUES ($1, $2, $3, '{object.created}', 'whsec_test')`, uuid.New().String(), b.tenantID, ok.URL+"/hook")
	require.NoError(t, err)
	_, err = a.db.Exec(`
		INSERT INTO object_head_cache (tenant_id, bucket, object_key, size_bytes, etag, backend_name, floor, is_chunked, content_type)
		SELECT $1, $2, 'b' || bb || '/' || i, 1, 'etag', 'local', 'standard', false, 'application/octet-stream'
		FROM generate_series(1, 10) AS bb, generate_series(1, 50) AS i`, a.tenantID, a.bucket)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for bb := 1; bb <= 10; bb++ {
		wg.Add(1)
		go func(bb int) {
			defer wg.Done()
			body := "<Delete>"
			for i := 1; i <= 50; i++ {
				body += fmt.Sprintf("<Object><Key>b%d/%d</Key></Object>", bb, i)
			}
			a.deleteObjects(body + "</Delete>")
		}(bb)
	}
	wg.Wait()

	// Act: B's PUT event while A's backlog is in the pool.
	start := time.Now()
	emitEvent(context.Background(), b.db, zap.NewNop(), "object.created", b.tenantID, map[string]interface{}{"bucket": b.bucket, "key": "x"})

	// Assert: B's webhook is delivered and recorded promptly, never dropped.
	require.Eventually(t, func() bool { return b.webhookRows("delivered", "%") == 1 }, 3*time.Second, 10*time.Millisecond,
		"B delivered=%d overloaded=%d", b.webhookRows("delivered", "%"), b.webhookRows("failed", "dropped: overloaded"))
	t.Logf("tenant B delivered after %v", time.Since(start))
	assert.Equal(t, 0, b.webhookRows("failed", "dropped: overloaded"))
	assert.Greater(t, a.webhookRows("failed", "dropped: overloaded"), 0, "A's own overflow is A's rows")
}

func TestEventDeliveries_ATenantWithNoTargetsSubmitsNothingAndQueriesNothing(t *testing.T) {
	// Arrange: a tenant with no webhook and no bucket notification. Before:
	// every GET (object.downloaded) and PUT submitted a pool job that read
	// webhook_endpoints / bucket_notifications — one query per request, and
	// a slot in the shared queue.
	f := setupBatchFanoutFixture(t)
	pool := newDeliveryPool(4, 64)
	useDeliveryPool(t, pool)
	notify := NewNotificationDispatcher(f.db, zap.NewNop())
	get := func() {
		emitEvent(context.Background(), f.db, zap.NewNop(), "object.downloaded", f.tenantID, map[string]interface{}{"bucket": f.bucket, "key": "k"})
		notify.Fire(f.tenantID, f.bucket, "s3:ObjectCreated:Put", "k", 1, "etag")
	}
	get() // the first request of the TTL reads the lists once

	// Act
	f.log.reset()
	for i := 0; i < 10; i++ {
		get()
	}
	f.log.settled(t)

	// Assert: only the tenant's event log rows; no target read, no job.
	assert.Equal(t, 0, f.log.count("webhook_endpoints"))
	assert.Equal(t, 0, f.log.count("bucket_notifications"))
	assert.Equal(t, 10, f.log.count("INSERT INTO events"), "the events rows are written as before")
	assert.Equal(t, 10, f.log.total(), "no other statement")
	assert.EqualValues(t, 0, pool.accepted.Load(), "nothing submitted")
}
