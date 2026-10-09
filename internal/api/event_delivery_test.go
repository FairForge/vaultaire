package api

import (
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
	useDeliveryPool(t, newDeliveryPool(4, 64))
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
	// 8 POSTs in flight; past the queue a key's webhook is recorded as
	// failed `dropped: overloaded` instead of waiting in a goroutine.
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
	assert.LessOrEqual(t, int(inFlight.Load()), workers, "POSTs in flight process-wide")
	dropped := f.webhookRows("failed", "dropped: overloaded")
	// 2,000 jobs (a notification + a webhook per key), at most workers +
	// queue of them accepted: every webhook job beyond that is a row.
	assert.GreaterOrEqual(t, dropped, batches*keys-(workers+queue))
	assert.LessOrEqual(t, eventDeliveries.pending(), workers+queue)
}

func TestEventDeliveries_ShutdownDrainsThenRecordsTheRestAsFailed(t *testing.T) {
	// Arrange: a blackholed webhook, 2 workers, 10 keys queued behind them.
	// Before: the pending deliveries vanished with the process — no row, no
	// log line; GET /api/v1/events showed the events with no delivery.
	f := setupBatchFanoutFixture(t)
	webhookAllowPrivateTargets.Store(true)
	t.Cleanup(func() { webhookAllowPrivateTargets.Store(false) })
	pool := newDeliveryPool(2, 64)
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
