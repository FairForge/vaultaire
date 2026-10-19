package api

// RP13-1 (WP-P12 post-merge review): a failed chunk PUT deleted its claim at
// once (dropChunkClaim), so the next writer claimed and stored the chunk
// within seconds — outside decision 8's takeover argument. A backend that
// got the whole body and never answered (RP8-11) can commit it late, over
// the next writer's bytes, whose row describes the next writer's encoding.
// A PUT that ended WITHOUT AN ANSWER now keeps its claim for the takeover
// window; one the backend ANSWERED with an error (nothing committed) drops
// it as before.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
)

// stallMode is how unanswerChunkDriver fails a chunk PUT after taking its
// whole body.
type stallMode int32

const (
	stallOff            stallMode = iota
	stallDeadline                 // the fixed-bucket driver's "no answer within …, twice" (put_deadline.go)
	stallSilent                   // says nothing until the caller's context ends (the chunk bound, a cancel)
	stallAnswer                   // answers 500: nothing committed
	stallGateThenOK               // takes the body, waits for the gate, then stores it (a slow backend that answers)
	stallGateThenAnswer           // takes the body, waits for the gate, then answers 500
)

// unanswerChunkDriver is the fixture's primary whose chunk PUTs read the
// whole body and then fail as mode says.
type unanswerChunkDriver struct {
	*fixedBucketDriver
	mode      atomic.Int32
	attempted atomic.Int32
	gate      chan struct{} // the gated modes answer once it is closed
}

func (d *unanswerChunkDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	if container == chunkContainer {
		switch stallMode(d.mode.Load()) {
		case stallDeadline:
			_, _ = io.Copy(io.Discard, data)
			d.attempted.Add(1)
			return fmt.Errorf("PutObject %s: no answer within 10s, twice (after one retry on a fresh connection): %w", artifact, context.DeadlineExceeded)
		case stallSilent:
			_, _ = io.Copy(io.Discard, data)
			d.attempted.Add(1)
			<-ctx.Done()
			return fmt.Errorf("operation error S3: PutObject, https response error StatusCode: 0, canceled, %w", ctx.Err())
		case stallAnswer:
			_, _ = io.Copy(io.Discard, data)
			d.attempted.Add(1)
			return backendAnswer{status: http.StatusInternalServerError, msg: "api error InternalError: boom"}
		case stallGateThenOK, stallGateThenAnswer:
			buf, _ := io.ReadAll(data)
			d.attempted.Add(1)
			select {
			case <-d.gate:
			case <-ctx.Done():
				return fmt.Errorf("operation error S3: PutObject, https response error StatusCode: 0, canceled, %w", ctx.Err())
			}
			if stallMode(d.mode.Load()) == stallGateThenAnswer {
				return backendAnswer{status: http.StatusInternalServerError, msg: "api error InternalError: boom"}
			}
			return d.fixedBucketDriver.Put(ctx, container, artifact, bytes.NewReader(buf), opts...)
		}
	}
	return d.fixedBucketDriver.Put(ctx, container, artifact, data, opts...)
}

type outcomeFixture struct {
	*mpChunkFixture
	drv  *unanswerChunkDriver
	body []byte
	hash string
}

func setupOutcomeFixture(t *testing.T, mode stallMode) *outcomeFixture {
	t.Helper()
	f := setupMPChunkFixture(t, false)
	d := &unanswerChunkDriver{fixedBucketDriver: f.fixed, gate: make(chan struct{})}
	d.mode.Store(int32(mode))
	require.NoError(t, f.server.engine.AddDriver("fixed", d))
	f.server.chunkPutBound = 300 * time.Millisecond
	f.server.chunkPendingTakeover = 1500 * time.Millisecond
	body := randomBody(t, 512<<10) // one chunk (under the 1 MiB minimum)
	sum := sha256.Sum256(body)
	o := &outcomeFixture{mpChunkFixture: f, drv: d, body: body, hash: hex.EncodeToString(sum[:])}
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`, crypto.GlobalDedupScope, o.hash)
	})
	return o
}

func (o *outcomeFixture) put(ctx context.Context, t *testing.T, key string) int {
	t.Helper()
	r := httptest.NewRequest("PUT", "/test-bucket/"+key, bytes.NewReader(o.body))
	r.ContentLength = int64(len(o.body))
	r = r.WithContext(s3Ctx(ctx, o.tenant))
	w := httptest.NewRecorder()
	o.server.handleS3Request(w, r)
	return w.Code
}

// claim reads the chunk's row: its state and pending_since (zero when none).
func (o *outcomeFixture) claim(t *testing.T) (found bool, state string, since time.Time) {
	t.Helper()
	var ps *time.Time
	err := o.db.QueryRow(`SELECT state, pending_since FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`,
		crypto.GlobalDedupScope, o.hash).Scan(&state, &ps)
	if err != nil {
		return false, "", time.Time{}
	}
	if ps != nil {
		since = *ps
	}
	return true, state, since
}

// refCount reads the chunk's row's ref_count (-1 when there is no row).
func (o *outcomeFixture) refCount(t *testing.T) int {
	t.Helper()
	n := -1
	_ = o.db.QueryRow(`SELECT ref_count FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`,
		crypto.GlobalDedupScope, o.hash).Scan(&n)
	return n
}

// cutAfterTheBodyWentOut cancels ctx once the backend has taken the body.
func (o *outcomeFixture) cutAfterTheBodyWentOut(cancel context.CancelFunc) {
	go func() {
		for o.drv.attempted.Load() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
}

func TestChunkClaim_APutWithNoAnswerKeepsItsClaimForTheWindow(t *testing.T) {
	for _, mode := range []struct {
		name string
		m    stallMode
	}{{"deadline", stallDeadline}, {"bound", stallSilent}} {
		t.Run(mode.name, func(t *testing.T) {
			// Arrange: the backend takes the body and never answers.
			o := setupOutcomeFixture(t, mode.m)

			// Act: the first writer fails.
			require.NotEqual(t, http.StatusOK, o.put(context.Background(), t, "first.bin"))

			// Assert: its claim stays, with its pending_since.
			found, state, since := o.claim(t)
			require.True(t, found, "a PUT with no answer may still land: its claim must stay")
			assert.Equal(t, "pending", state)
			assert.False(t, since.IsZero())

			// Act: a second writer of the chunk, the backend healthy again.
			o.drv.mode.Store(int32(stallOff))
			start := time.Now()
			require.Equal(t, http.StatusOK, o.put(context.Background(), t, "second.bin"))

			// Assert: it waited out the window from the claim, then took it
			// over and stored.
			assert.GreaterOrEqual(t, time.Since(since), o.server.chunkPendingTakeover-100*time.Millisecond)
			assert.GreaterOrEqual(t, time.Since(start), 500*time.Millisecond, "the second writer waited")
			_, state, _ = o.claim(t)
			assert.Equal(t, "ready", state)
			assert.Equal(t, o.body, (&pendingFixture{mpChunkFixture: o.mpChunkFixture}).getObj(t, "second.bin"))
		})
	}
}

func TestChunkClaim_APutTheBackendAnsweredWithAnErrorDropsItsClaimAtOnce(t *testing.T) {
	// Arrange
	o := setupOutcomeFixture(t, stallAnswer)

	// Act
	require.NotEqual(t, http.StatusOK, o.put(context.Background(), t, "first.bin"))

	// Assert: nothing committed — the claim is gone, the next writer stores
	// at once.
	found, _, _ := o.claim(t)
	assert.False(t, found, "an error answer committed nothing: the claim is dropped")
	o.drv.mode.Store(int32(stallOff))
	start := time.Now()
	require.Equal(t, http.StatusOK, o.put(context.Background(), t, "second.bin"))
	assert.Less(t, time.Since(start), o.server.chunkPendingTakeover, "no window waited")
}

func TestChunkClaim_ACancelAfterTheBodyWentOutLetsThePutFinishDetachedAndTheChunkIsReady(t *testing.T) {
	// Arrange: the request goes away while the backend holds the whole
	// body; the backend then stores it. (Until RP14-1 the body having gone
	// out kept the claim for the window: the PUT was cancelled with the
	// request and its outcome never known.)
	o := setupOutcomeFixture(t, stallGateThenOK)
	o.server.chunkPutBound = 30 * time.Second
	o.server.chunkPendingTakeover = 60 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	o.cutAfterTheBodyWentOut(cancel)
	ready0 := promtest.ToFloat64(chunkPutDetached.WithLabelValues("ready"))

	// Act: the handler answers at once — the PUT is not waited on …
	start := time.Now()
	require.NotEqual(t, http.StatusOK, o.put(ctx, t, "gone.bin"))
	assert.Less(t, time.Since(start), 5*time.Second, "the handler does not wait for the detached PUT")
	found, state, _ := o.claim(t)
	require.True(t, found, "the claim is kept while the PUT runs")
	assert.Equal(t, "pending", state)
	assert.Equal(t, 1, o.server.chunkPuts().count(), "the detached PUT is accounted")

	// … and the PUT finishes detached: the chunk is ready, at ref 0.
	close(o.drv.gate)
	waitFor(t, "the detached PUT to make the chunk ready", func() bool { _, st, _ := o.claim(t); return st == "ready" })
	assert.Equal(t, 0, o.refCount(t), "the aborted write took no reference")
	assert.Equal(t, ready0+1, promtest.ToFloat64(chunkPutDetached.WithLabelValues("ready")))
	waitFor(t, "the registry to empty", func() bool { return o.server.chunkPuts().count() == 0 })

	// A retry of the body dedups onto it at once, never the window.
	start = time.Now()
	require.Equal(t, http.StatusOK, o.put(context.Background(), t, "gone.bin"))
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, 1, o.refCount(t), "the retry's reference, and only it")
	assert.Equal(t, o.body, (&pendingFixture{mpChunkFixture: o.mpChunkFixture}).getObj(t, "gone.bin"))
}
