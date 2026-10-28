package dataplane

// RP74-6: a Go host's codec writes hold their chunk plaintext inside a
// byte budget — FIFO, a request larger than the bound admitted alone, a
// wait past the bound's patience the client's 503 SlowDown.

import (
	"bytes"
	"context"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestByteBudget_FIFOLargerThanTheBoundAloneAndTheWait(t *testing.T) {
	b := NewByteBudget(100)
	b.Wait = 200 * time.Millisecond
	rel1, err := b.Acquire(context.Background(), 60)
	require.NoError(t, err)
	assert.Equal(t, int64(60), b.InUse())

	// A second 60 cannot fit: it waits, then is busy.
	start := time.Now()
	_, err = b.Acquire(context.Background(), 60)
	assert.ErrorIs(t, err, ErrBusy)
	assert.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond)

	// FIFO: a 60 queued first is served before a later 10 that would fit.
	b.Wait = 5 * time.Second
	order := make(chan int, 2)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r, err := b.Acquire(context.Background(), 60)
		if err == nil {
			order <- 60
			r()
		}
	}()
	require.Eventually(t, func() bool { return b.Waiting() == 1 }, time.Second, time.Millisecond)
	wg.Add(1)
	go func() {
		defer wg.Done()
		r, err := b.Acquire(context.Background(), 10)
		if err == nil {
			order <- 10
			r()
		}
	}()
	// The 10 would fit beside the held 60, yet it queues behind the
	// waiting 60: first come, first served.
	require.Eventually(t, func() bool { return b.Waiting() == 2 }, time.Second, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, 2, b.Waiting(), "never served past the head of the line")
	assert.Equal(t, int64(60), b.InUse())
	rel1()
	wg.Wait()
	close(order)
	got := map[int]bool{}
	for v := range order {
		got[v] = true
	}
	assert.Equal(t, map[int]bool{60: true, 10: true}, got, "both served once the head fits")
	assert.Zero(t, b.InUse())

	// Larger than the bound: admitted alone (when nothing else holds).
	r, err := b.Acquire(context.Background(), 500)
	require.NoError(t, err)
	assert.Equal(t, int64(100), b.InUse(), "it takes the whole bound")
	r()
	r() // a second release is a no-op
	assert.Zero(t, b.InUse())

	// A cancelled context is the same refusal.
	ctx, cancel := context.WithCancel(context.Background())
	hold, _ := b.Acquire(context.Background(), 100)
	cancel()
	_, err = b.Acquire(ctx, 1)
	assert.ErrorIs(t, err, ErrBusy)
	hold()
}

// countingCP answers every chunk a hit (no seal, no PUT) and counts the
// codec writes inside their index call at once.
type countingCP struct {
	fakeCP
	in, max atomic.Int32
}

func (c *countingCP) Index(_ context.Context, r IndexRequest) (IndexResponse, error) {
	n := c.in.Add(1)
	for {
		m := c.max.Load()
		if n <= m || c.max.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(150 * time.Millisecond)
	c.in.Add(-1)
	out := IndexResponse{}
	for _, ch := range r.Chunks {
		out.Hits = append(out.Hits, ch.Hash)
	}
	return out, nil
}

// holdCounter wraps a Budget and records the most writes holding it at once.
type holdCounter struct {
	Budget
	in, max atomic.Int32
}

func (h *holdCounter) Acquire(ctx context.Context, n int64) (func(), error) {
	rel, err := h.Budget.Acquire(ctx, n)
	if err != nil {
		return nil, err
	}
	k := h.in.Add(1)
	for {
		m := h.max.Load()
		if k <= m || h.max.CompareAndSwap(m, k) {
			break
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			h.in.Add(-1)
			rel()
		})
	}, nil
}

func TestHost_TwoCodecWritesUnderOneBudgetSerialise(t *testing.T) {
	const size = 48 << 20
	body := make([]byte, size)
	rand.New(rand.NewSource(48)).Read(body)

	// Each write holds the budget from before its first byte to its commit
	// (seconds of chunking): two writes started together overlap there
	// unless the budget serialises them.
	run := func(budget Budget) int32 {
		hc := &holdCounter{Budget: budget}
		cp := &countingCP{}
		cp.admit = AdmitResponse{Edge: true, AdmitID: "A", Codec: &CodecGrant{}}
		h := &Host{CP: cp, Budget: hc}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				req := httptest.NewRequest(http.MethodPut, "https://s3.test/b/k", nil)
				res, err := h.Write(context.Background(), req, bytes.NewReader(body), size)
				assert.NoError(t, err)
				assert.Equal(t, http.StatusOK, res.Relay.Status)
			}()
		}
		close(start)
		wg.Wait()
		return hc.max.Load()
	}
	b := NewByteBudget(64 << 20)
	b.Wait = time.Minute
	assert.Equal(t, int32(1), run(b), "48 + 48 MiB > 64 MiB: one write at a time")
	assert.Zero(t, b.InUse(), "everything given back")
	assert.Equal(t, int32(2), run(NewByteBudget(1<<30)), "a budget with room: both at once")
}

func TestHost_ACodecWriteTheBudgetCannotTakeIsTheClients503(t *testing.T) {
	cp := &countingCP{}
	cp.admit = AdmitResponse{Edge: true, AdmitID: "A", Codec: &CodecGrant{}}
	b := NewByteBudget(64 << 20)
	b.Wait = 50 * time.Millisecond
	hold, err := b.Acquire(context.Background(), 64<<20)
	require.NoError(t, err)
	defer hold()
	h := &Host{CP: cp, Budget: b}
	req := httptest.NewRequest(http.MethodPut, "https://s3.test/b/k", nil)
	res, err := h.Write(context.Background(), req, bytes.NewReader(make([]byte, 1<<20)), 1<<20)
	require.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, res.Relay.Status)
	assert.Equal(t, "30", res.Relay.Headers["Retry-After"])
	require.NotNil(t, cp.commit)
	assert.Equal(t, OutcomeEdgeError, cp.commit.Outcome, "everything given back")
	assert.Zero(t, cp.max.Load(), "refused before the body was read: no index call")
}
