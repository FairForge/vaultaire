package api

// Stage 1a Prompt 7, Part 0: a 1,000-key DeleteObjects against concurrent
// PUTs to the same tenant — env-gated, run by hand on two builds (WP-R10-8
// moved every release into its key's delete transaction).

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestPostMerge108_DeleteObjectsQuotaRowHold measures (env-gated, not CI) a
// 1,000-key DeleteObjects against concurrent PUTs to the same tenant: the
// batch's duration and the PUTs' p50/p99 during it, and the longest any
// PUT waited on the tenant's quota row. Run it with the same command on
// both builds: VAULTAIRE_MEASURE_BATCH=1 go test -run …QuotaRowHold -v.
func TestPostMerge108_DeleteObjectsQuotaRowHold(t *testing.T) {
	if os.Getenv("VAULTAIRE_MEASURE_BATCH") == "" {
		t.Skip("measurement: set VAULTAIRE_MEASURE_BATCH=1")
	}
	f := setupQuotaAccountingFixture(t, 1<<40)
	const keys = 1000
	var objs strings.Builder
	objs.WriteString("<Delete><Quiet>true</Quiet>")
	for i := 0; i < keys; i++ {
		require.Equal(t, http.StatusOK, f.put(t, fmt.Sprintf("d/%04d", i), testBytes(4096)))
		fmt.Fprintf(&objs, "<Object><Key>d/%04d</Key></Object>", i)
	}
	objs.WriteString("</Delete>")

	for round := 0; round < 3; round++ {
		var stop atomic.Bool
		var mu sync.Mutex
		var lat []time.Duration
		var wg sync.WaitGroup
		for w := 0; w < 8; w++ {
			wg.Go(func() {
				for i := 0; !stop.Load(); i++ {
					t0 := time.Now()
					code := f.put(t, fmt.Sprintf("p/%d-%d-%d", round, w, i), testBytes(4096))
					d := time.Since(t0)
					if code == http.StatusOK {
						mu.Lock()
						lat = append(lat, d)
						mu.Unlock()
					}
				}
			})
		}
		time.Sleep(200 * time.Millisecond)
		mu.Lock()
		lat = lat[:0]
		mu.Unlock()
		// The quota row's holders, sampled every 2 ms during the batch: how
		// often a backend waited on a lock held by a transaction that has
		// updated the tenant's tenant_quotas row, and the longest run of
		// consecutive samples it did (≈ the longest single wait).
		var samples, waiting, longest, run int
		sampling := make(chan struct{})
		sampled := make(chan struct{})
		go func() {
			defer close(sampled)
			for {
				select {
				case <-sampling:
					return
				case <-time.After(2 * time.Millisecond):
				}
				var n int
				_ = f.db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity a
					WHERE a.datname = current_database() AND a.wait_event_type = 'Lock'
					  AND a.query LIKE '%tenant_quotas%'`).Scan(&n)
				samples++
				if n > 0 {
					waiting++
					run++
					longest = max(longest, run)
				} else {
					run = 0
				}
			}
		}()
		t0 := time.Now()
		w := doS3Request(f.server, f.tenant, "POST", "/test-bucket?delete", strings.NewReader(objs.String()))
		batch := time.Since(t0)
		close(sampling)
		<-sampled
		stop.Store(true)
		wg.Wait()
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		mu.Lock()
		p50, p99, n := percentiles(lat)
		mu.Unlock()
		t.Logf("round %d: DeleteObjects(%d) %v; concurrent PUTs during it: n=%d p50=%v p99=%v; a quota-row statement waiting in %d of %d samples, longest run %d (×2 ms)",
			round, keys, batch.Round(time.Millisecond), n, p50, p99, waiting, samples, longest)
		// Re-seed the keys for the next round.
		for i := 0; i < keys; i++ {
			require.Equal(t, http.StatusOK, f.put(t, fmt.Sprintf("d/%04d", i), testBytes(4096)))
		}
	}
	_ = f.used(t)
}

func percentiles(d []time.Duration) (p50, p99 time.Duration, n int) {
	n = len(d)
	if n == 0 {
		return 0, 0, 0
	}
	s := append([]time.Duration(nil), d...)
	for i := 1; i < len(s); i++ { // insertion sort: n is small
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s[n/2], s[(n*99)/100], n
}
