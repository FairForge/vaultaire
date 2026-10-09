package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Prompt 2a, PR 4: with a notification target and a webhook configured, a
// 1,000-key DeleteObjects used to start one goroutine per key for each —
// 2,000 concurrent POSTs and 1,000 webhook_deliveries inserts against the
// 50-connection pool. A batch now hands its per-key events to the
// process-wide delivery pool (event_delivery.go), in key order (start
// order); the events rows are still written per key as before. The test
// installs a 4-worker pool so its bounds stay tight.

// keyOf extracts the object key from either payload shape the target receives.
func keyOf(r *http.Request) string {
	body, _ := io.ReadAll(r.Body)
	switch r.URL.Path {
	case "/s3":
		var ev S3Event
		if json.Unmarshal(body, &ev) == nil && len(ev.Records) == 1 {
			return ev.Records[0].S3.Object.Key
		}
	case "/hook":
		var ev struct {
			Data struct {
				Key string `json:"key"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &ev) == nil {
			return ev.Data.Key
		}
	}
	return ""
}

func TestDeleteObjects_ABatchDeliversFromABoundedWorkerSetInKeyOrder(t *testing.T) {
	f := setupBatchFanoutFixture(t)
	webhookAllowPrivateTargets.Store(true)
	t.Cleanup(func() { webhookAllowPrivateTargets.Store(false) })
	const batchDeliveryWorkers = 4
	useDeliveryPool(t, newDeliveryPool(batchDeliveryWorkers, deliveryQueueSize))

	var mu sync.Mutex
	var inFlight, peakPosts int
	arrivals := map[string][]string{"/s3": nil, "/hook": nil}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peakPosts {
			peakPosts = inFlight
		}
		mu.Unlock()
		key := keyOf(r)
		mu.Lock()
		arrivals[r.URL.Path] = append(arrivals[r.URL.Path], key)
		inFlight--
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)
	_, err := f.db.Exec(`INSERT INTO bucket_notifications (tenant_id, bucket, event_filter, target_type, target_url)
		VALUES ($1, $2, 's3:ObjectRemoved:*', 'webhook', $3)`, f.tenantID, f.bucket, target.URL+"/s3")
	require.NoError(t, err)
	_, err = f.db.Exec(`INSERT INTO webhook_endpoints (id, tenant_id, url, event_filter, secret)
		VALUES ($1, $2, $3, '{object.deleted}', 'whsec_test')`, uuid.New().String(), f.tenantID, target.URL+"/hook")
	require.NoError(t, err)

	const n = 1000
	body := f.objects(n)
	f.log.reset()
	f.log.resetPeaks()
	f.deleteObjects(body)
	f.log.settled(t)

	// Everything still happens, once per key.
	mu.Lock()
	s3Keys, hookKeys := append([]string(nil), arrivals["/s3"]...), append([]string(nil), arrivals["/hook"]...)
	peak := peakPosts
	mu.Unlock()
	assert.Len(t, s3Keys, n, "s3:ObjectRemoved:Delete once per key")
	assert.Len(t, hookKeys, n, "object.deleted once per key")
	assert.Equal(t, n, f.log.count("INSERT INTO webhook_deliveries"))
	assert.Equal(t, n, f.log.count("INSERT INTO events"), "the events rows are written as before")
	var eventsRows int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM events WHERE tenant_id = $1 AND type = 'object.deleted'`, f.tenantID).Scan(&eventsRows))
	assert.Equal(t, n, eventsRows)

	// Bounded: never more than the worker set in flight, on the wire or in the pool.
	assert.LessOrEqual(t, peak, batchDeliveryWorkers, "concurrent POSTs")
	assert.LessOrEqual(t, f.log.peakOf("INSERT INTO webhook_deliveries"), batchDeliveryWorkers, "concurrent delivery inserts")
	assert.LessOrEqual(t, f.log.peakTotal(), batchDeleteConcurrency+batchDeliveryWorkers, "concurrent statements overall")

	// In key order: each key once, started in request order. Four workers
	// pull from one ordered queue (a notification job then a webhook job
	// per key), so arrivals
	// can lead or trail their position by a few workers' worth under
	// scheduling jitter (CI saw 8 with a bound of 8) — never the 60+ of the
	// per-key goroutines this replaced.
	for name, keys := range map[string][]string{"notifications": s3Keys, "webhooks": hookKeys} {
		seen := map[string]bool{}
		maxDrift := 0
		for pos, k := range keys {
			assert.False(t, seen[k], "%s: %s delivered twice", name, k)
			seen[k] = true
			i, convErr := strconv.Atoi(strings.TrimPrefix(k, "k/"))
			require.NoError(t, convErr, k)
			if d := abs(pos - (i - 1)); d > maxDrift {
				maxDrift = d
			}
		}
		assert.LessOrEqual(t, maxDrift, 4*batchDeliveryWorkers, "%s: dispatched in key order", name)
	}
	sorted := append([]string(nil), hookKeys...)
	sort.Strings(sorted)
	assert.Len(t, sorted, n)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
