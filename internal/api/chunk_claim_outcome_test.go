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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/engine"
)

// stallMode is how unanswerChunkDriver fails a chunk PUT after taking its
// whole body.
type stallMode int32

const (
	stallOff      stallMode = iota
	stallDeadline           // the fixed-bucket driver's "no answer within …, twice" (put_deadline.go)
	stallSilent             // says nothing until the caller's context ends (the chunk bound, a cancel)
	stallAnswer             // answers 500: nothing committed
)

// unanswerChunkDriver is the fixture's primary whose chunk PUTs read the
// whole body and then fail as mode says.
type unanswerChunkDriver struct {
	*fixedBucketDriver
	mode      atomic.Int32
	attempted atomic.Int32
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
	d := &unanswerChunkDriver{fixedBucketDriver: f.fixed}
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

func TestChunkClaim_ACancelAfterTheBodyWentOutKeepsTheClaim(t *testing.T) {
	// Arrange: the request goes away while the backend holds the whole body.
	o := setupOutcomeFixture(t, stallSilent)
	o.server.chunkPutBound = 30 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for o.drv.attempted.Load() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()

	// Act
	require.NotEqual(t, http.StatusOK, o.put(ctx, t, "gone.bin"))

	// Assert
	found, state, _ := o.claim(t)
	require.True(t, found, "the body was sent: the backend may commit it, so the claim stays")
	assert.Equal(t, "pending", state)
}
