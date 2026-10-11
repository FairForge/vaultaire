package parfetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Prompt 2b.5 F3.2 (from the 2b.4 review's red team). A first attempt that
// ERRORS fast gets its one retry; when that retry hung, the timer path was
// inert and the ordered stream waited on it until the caller gave up
// (before: delivered 1/3 in 1.50 s, 2 attempts, context deadline exceeded).
func TestStream_ErrorThenHungRetryIsHedged(t *testing.T) {
	data, sizes := parts(3, 64)
	var calls [3]atomic.Int32
	fetch := func(ctx context.Context, i int) ([]byte, error) {
		n := calls[i].Add(1)
		if i == 1 && n == 1 {
			return nil, errors.New("connection reset")
		}
		if i == 1 && n == 2 {
			<-ctx.Done() // the retry hangs (vendor accepted, never answers)
			return nil, ctx.Err()
		}
		return data[i], nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	s := Start(ctx, Config{HedgeAfter: 50 * time.Millisecond, MaxHedges: 8}, sizes, fetch)
	defer s.Close()
	start := time.Now()
	got, err := drain(t, s)
	took := time.Since(start)
	t.Logf("delivered %d/3 in %s, attempts at part 1: %d, err=%v", len(got), took, calls[1].Load(), err)
	assert.NoError(t, err, "stream stalled on the hung retry")
	assert.Less(t, took, 500*time.Millisecond)
	assert.Equal(t, int32(3), calls[1].Load(), "the first, its retry, one hedge of the hung retry")
}

// The mirror image: the slow-hedge errors fast while the ORIGINAL hangs —
// nothing was launched again and the stream waited on the hung original.
func TestStream_HungOriginalWhoseHedgeFailedIsHedgedAgain(t *testing.T) {
	data, sizes := parts(3, 64)
	var calls [3]atomic.Int32
	fetch := func(ctx context.Context, i int) ([]byte, error) {
		n := calls[i].Add(1)
		if i == 1 && n == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if i == 1 && n == 2 {
			return nil, errors.New("503 slow down")
		}
		return data[i], nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	s := Start(ctx, Config{HedgeAfter: 50 * time.Millisecond, MaxHedges: 8}, sizes, fetch)
	defer s.Close()
	start := time.Now()
	got, err := drain(t, s)
	took := time.Since(start)
	t.Logf("delivered %d/3 in %s, attempts at part 1: %d, err=%v", len(got), took, calls[1].Load(), err)
	assert.NoError(t, err, "stream stalled on the hung original")
	assert.Less(t, took, 500*time.Millisecond)
	assert.Equal(t, int32(3), calls[1].Load())
}

// With the re-arms, MaxHedges still bounds CONCURRENT slot hedges per
// stream, and a part whose hedge succeeds gets no other.
func TestStream_ReArmKeepsMaxHedgesAndOneHedgePerPart(t *testing.T) {
	const n, maxH = 40, 3
	data, sizes := parts(n, 128)
	var calls [n]atomic.Int32
	var inflightHedges, peak atomic.Int32
	fetch := func(ctx context.Context, i int) ([]byte, error) {
		if calls[i].Add(1) == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		cur := inflightHedges.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		defer inflightHedges.Add(-1)
		if err := sleepCtx(ctx, 30*time.Millisecond); err != nil {
			return nil, err
		}
		return data[i], nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := Start(ctx, Config{HedgeAfter: 20 * time.Millisecond, MaxHedges: maxH, MaxParts: 16}, sizes, fetch)
	defer s.Close()
	got, err := drain(t, s)
	require.NoError(t, err)
	assert.Equal(t, data, got)
	assert.LessOrEqual(t, peak.Load(), int32(maxH), "concurrent slot hedges")
	for i := range calls {
		assert.LessOrEqual(t, calls[i].Load(), int32(2), "part %d attempts", i)
	}
}

// Randomized sizes, latencies, failures, hangs: byte-exact output, no
// goroutine left after Close; a parent-ctx cancel leaks nothing either.
func TestStream_HedgingRandomizedByteExactNoLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	for seed := int64(1); seed <= 12; seed++ {
		rng := rand.New(rand.NewSource(seed))
		n := 8 + rng.Intn(40)
		data := make([][]byte, n)
		sizes := make([]int64, n)
		h := sha256.New()
		for i := range data {
			sz := 1 + rng.Intn(4096)
			data[i] = make([]byte, sz)
			rng.Read(data[i])
			sizes[i] = int64(sz)
			h.Write(data[i])
		}
		want := h.Sum(nil)
		var mu sync.Mutex
		pr := rand.New(rand.NewSource(seed * 7919))
		attempts := make([]atomic.Int32, n)
		fetch := func(ctx context.Context, i int) ([]byte, error) {
			mu.Lock()
			roll := pr.Float64()
			lat := time.Duration(pr.Intn(20)) * time.Millisecond
			mu.Unlock()
			a := attempts[i].Add(1)
			switch {
			case roll < 0.15 && a == 1: // first attempt hangs
				<-ctx.Done()
				return nil, ctx.Err()
			case roll < 0.30 && a == 1: // first attempt errors
				return nil, errors.New("boom")
			}
			if err := sleepCtx(ctx, lat); err != nil {
				return nil, err
			}
			return data[i], nil
		}
		cancelParent := seed%4 == 0
		ctx, cancel := context.WithCancel(context.Background())
		s := Start(ctx, Config{Window: 8192, MaxParts: 6, HedgeAfter: 15 * time.Millisecond, MaxHedges: 2, FreeParts: 2, Budget: NewBudget(1 << 20)}, sizes, fetch)
		if cancelParent {
			// cancel midway: read a few parts, then pull the plug
			for k := 0; k < 3; k++ {
				if _, err := s.Next(); err != nil {
					break
				}
			}
			cancel()
			var err error
			for k := 0; k < n+1 && err == nil; k++ { // already-buffered results may still come
				_, err = s.Next()
			}
			assert.Error(t, err, "seed %d: cancelled parent must surface", seed)
			s.Close()
			cancel()
			continue
		}
		got, err := drain(t, s)
		s.Close()
		cancel()
		require.NoError(t, err, "seed %d", seed)
		gh := sha256.New()
		for _, p := range got {
			gh.Write(p)
		}
		assert.True(t, bytes.Equal(want, gh.Sum(nil)), "seed %d: bytes differ", seed)
	}
	noLeak(t, before)
}

// A re-armed timer never fires into a finished part: Close right after the
// slots fill; every goroutine (timers included) is gone.
func TestStream_CloseWhileReArmedLeaksNothing(t *testing.T) {
	before := runtime.NumGoroutine()
	for round := 0; round < 20; round++ {
		_, sizes := parts(12, 64)
		fetch := func(ctx context.Context, i int) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		s := Start(context.Background(), Config{HedgeAfter: 5 * time.Millisecond, MaxHedges: 2}, sizes, fetch)
		time.Sleep(25 * time.Millisecond) // timers fired, slots full, re-arms pending
		s.Close()
		_, err := s.Next()
		assert.Error(t, err)
	}
	noLeak(t, before)
}

// Prompt 2b.5 F3.3: the re-arm delay was hedgeDelay() = max(HedgeAfter, 3 ×
// median part time), and a part's time included its wait for a hedge slot.
// Under a brownout (every first attempt hung) the wait fed the median, the
// median the next wait (before: 20 ms → 772 ms → 1.65 s → … 5.06 s, 14.7 s
// in total for 0.4 s of work). Re-arms now wait the fixed HedgeAfter and
// only an attempt's own fetch time is observed.
func TestStream_ReArmDelayDoesNotFeedOnItself(t *testing.T) {
	const n, maxH = 40, 3
	data, sizes := parts(n, 128)
	var calls [n]atomic.Int32
	fetch := func(ctx context.Context, i int) ([]byte, error) {
		if calls[i].Add(1) == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if err := sleepCtx(ctx, 30*time.Millisecond); err != nil {
			return nil, err
		}
		return data[i], nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := Start(ctx, Config{HedgeAfter: 20 * time.Millisecond, MaxHedges: maxH, MaxParts: 16}, sizes, fetch)
	defer s.Close()
	start := time.Now()
	var last time.Duration
	for i := 0; ; i++ {
		_, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("part %d: %v after %s (delay now %s)", i, err, time.Since(start), s.hedgeDelay())
		}
		el := time.Since(start)
		if i%5 == 0 || el-last > 500*time.Millisecond {
			t.Logf("part %2d at %8s  hedgeDelay=%s", i, el.Round(time.Millisecond), s.hedgeDelay().Round(time.Millisecond))
		}
		last = el
	}
	total := time.Since(start)
	t.Logf("total %s for %d parts whose hedges each take 30 ms (3 at a time ⇒ ~0.4 s ideal)", total.Round(time.Millisecond), n)
	assert.Less(t, total, 2*time.Second)
	assert.Less(t, s.hedgeDelay(), 200*time.Millisecond, "the median is the hedges' own 30 ms, not their waits")
}
