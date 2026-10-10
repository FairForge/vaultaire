package parfetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parts builds n parts of size bytes each, part i filled with byte i.
func parts(n, size int) ([][]byte, []int64) {
	data := make([][]byte, n)
	sizes := make([]int64, n)
	for i := range data {
		data[i] = bytes.Repeat([]byte{byte(i)}, size)
		sizes[i] = int64(size)
	}
	return data, sizes
}

// drain reads every part in order through Next.
func drain(t *testing.T, s *Stream) ([][]byte, error) {
	t.Helper()
	var got [][]byte
	for {
		p, err := s.Next()
		if errors.Is(err, io.EOF) {
			return got, nil
		}
		if err != nil {
			return got, err
		}
		got = append(got, append([]byte(nil), p...))
	}
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// noLeak fails when the goroutine count does not return to the baseline.
func noLeak(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.GC()
		after := runtime.NumGoroutine()
		if after <= before {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			t.Fatalf("goroutines grew from %d to %d:\n%s", before, after, buf[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStream_DeliversInOrderUnderOutOfOrderCompletion(t *testing.T) {
	// Arrange: later parts finish first.
	data, sizes := parts(16, 1024)
	fetch := func(ctx context.Context, i int) ([]byte, error) {
		if err := sleepCtx(ctx, time.Duration(16-i)*3*time.Millisecond); err != nil {
			return nil, err
		}
		return data[i], nil
	}

	// Act
	s := Start(context.Background(), Config{Window: 1 << 20, MaxParts: 16}, sizes, fetch)
	defer s.Close()
	got, err := drain(t, s)

	// Assert
	require.NoError(t, err)
	require.Len(t, got, 16)
	for i := range got {
		assert.Equal(t, data[i], got[i], "part %d out of order", i)
	}
}

func TestStream_HedgeWinsAndCancelsTheLoser(t *testing.T) {
	// Arrange: the first attempt at part 2 hangs until cancelled; any
	// second attempt answers at once.
	data, sizes := parts(4, 512)
	var attempts [4]atomic.Int32
	loserCancelled := make(chan struct{})
	fetch := func(ctx context.Context, i int) ([]byte, error) {
		n := attempts[i].Add(1)
		if i == 2 && n == 1 {
			<-ctx.Done()
			close(loserCancelled)
			return nil, ctx.Err()
		}
		return data[i], nil
	}
	var hedged, won atomic.Int32
	cfg := Config{Window: 1 << 20, MaxParts: 4, HedgeAfter: 20 * time.Millisecond,
		Hooks: Hooks{Hedged: func(string) { hedged.Add(1) }, HedgeWon: func() { won.Add(1) }}}

	// Act
	s := Start(context.Background(), cfg, sizes, fetch)
	got, err := drain(t, s)
	s.Close()

	// Assert
	require.NoError(t, err)
	assert.Equal(t, data[2], got[2])
	select {
	case <-loserCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the slow attempt was never cancelled after the hedge won")
	}
	assert.Equal(t, int32(1), hedged.Load(), "exactly one hedge")
	assert.Equal(t, int32(1), won.Load(), "the hedge won")
}

func TestStream_NoHedgeWhenFast(t *testing.T) {
	data, sizes := parts(8, 256)
	var calls atomic.Int32
	fetch := func(_ context.Context, i int) ([]byte, error) {
		calls.Add(1)
		return data[i], nil
	}
	s := Start(context.Background(), Config{Window: 1 << 20, MaxParts: 8, HedgeAfter: time.Second}, sizes, fetch)
	_, err := drain(t, s)
	s.Close()
	require.NoError(t, err)
	assert.Equal(t, int32(8), calls.Load(), "a fast part is fetched once")
}

func TestStream_ErrorIsRetriedOnceThenSurfacesInOrder(t *testing.T) {
	// Arrange: part 3 fails on every attempt.
	data, sizes := parts(8, 128)
	var p3 atomic.Int32
	boom := errors.New("backend broke")
	fetch := func(_ context.Context, i int) ([]byte, error) {
		if i == 3 {
			p3.Add(1)
			return nil, boom
		}
		return data[i], nil
	}

	// Act
	s := Start(context.Background(), Config{Window: 1 << 20, MaxParts: 8, HedgeAfter: time.Second}, sizes, fetch)
	got, err := drain(t, s)
	s.Close()

	// Assert: parts 0..2 then the error — never a later part.
	require.ErrorIs(t, err, boom)
	require.Len(t, got, 3)
	assert.Equal(t, int32(2), p3.Load(), "a failed part gets one more attempt, no more")
}

func TestStream_ErrorRetrySucceeds(t *testing.T) {
	data, sizes := parts(4, 128)
	var p1 atomic.Int32
	fetch := func(_ context.Context, i int) ([]byte, error) {
		if i == 1 && p1.Add(1) == 1 {
			return nil, errors.New("transient")
		}
		return data[i], nil
	}
	s := Start(context.Background(), Config{Window: 1 << 20, MaxParts: 4, HedgeAfter: time.Second}, sizes, fetch)
	got, err := drain(t, s)
	s.Close()
	require.NoError(t, err)
	assert.Equal(t, data[1], got[1])
}

func TestStream_WindowBoundsHeldBytes(t *testing.T) {
	// Arrange: 32 parts of 1 KiB, a 4 KiB window, a slow consumer.
	data, sizes := parts(32, 1024)
	var held, maxHeld atomic.Int64
	var mu sync.Mutex
	fetch := func(ctx context.Context, i int) ([]byte, error) {
		_ = sleepCtx(ctx, time.Millisecond)
		return data[i], nil
	}
	cfg := Config{Window: 4096, MaxParts: 100, Hooks: Hooks{Held: func(d int64) {
		mu.Lock()
		defer mu.Unlock()
		if v := held.Add(d); v > maxHeld.Load() {
			maxHeld.Store(v)
		}
	}}}

	// Act
	s := Start(context.Background(), cfg, sizes, fetch)
	for {
		_, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		time.Sleep(2 * time.Millisecond)
	}
	s.Close()

	// Assert
	assert.LessOrEqual(t, maxHeld.Load(), int64(4096), "never more than the window held")
	assert.Equal(t, int64(0), held.Load(), "everything released")
}

func TestStream_MaxPartsBoundsConcurrency(t *testing.T) {
	data, sizes := parts(20, 16)
	var cur, peak atomic.Int32
	fetch := func(ctx context.Context, i int) ([]byte, error) {
		c := cur.Add(1)
		defer cur.Add(-1)
		for {
			p := peak.Load()
			if c <= p || peak.CompareAndSwap(p, c) {
				break
			}
		}
		_ = sleepCtx(ctx, 5*time.Millisecond)
		return data[i], nil
	}
	s := Start(context.Background(), Config{Window: 1 << 20, MaxParts: 3}, sizes, fetch)
	_, err := drain(t, s)
	s.Close()
	require.NoError(t, err)
	assert.LessOrEqual(t, peak.Load(), int32(3))
	assert.GreaterOrEqual(t, peak.Load(), int32(2), "parts overlap")
}

func TestStream_BudgetExhaustedDegradesToFreeParts(t *testing.T) {
	// Arrange: someone else holds the whole budget.
	b := NewBudget(1 << 20)
	require.True(t, b.TryAcquire(1<<20))
	data, sizes := parts(12, 1024)
	var cur, peak atomic.Int32
	fetch := func(ctx context.Context, i int) ([]byte, error) {
		c := cur.Add(1)
		defer cur.Add(-1)
		for {
			p := peak.Load()
			if c <= p || peak.CompareAndSwap(p, c) {
				break
			}
		}
		_ = sleepCtx(ctx, 3*time.Millisecond)
		return data[i], nil
	}

	// Act
	s := Start(context.Background(), Config{Window: 1 << 20, MaxParts: 8, FreeParts: 2, Budget: b}, sizes, fetch)
	got, err := drain(t, s)
	s.Close()

	// Assert: it still finishes, two at a time.
	require.NoError(t, err)
	assert.Len(t, got, 12)
	assert.LessOrEqual(t, peak.Load(), int32(2))
	assert.Equal(t, int64(1<<20), b.InUse(), "the stream gave back exactly what it took")
}

func TestStream_BudgetIsReturnedAfterClose(t *testing.T) {
	b := NewBudget(1 << 30)
	data, sizes := parts(64, 4096)
	fetch := func(ctx context.Context, i int) ([]byte, error) {
		if i > 5 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return data[i], nil
	}
	s := Start(context.Background(), Config{Window: 1 << 20, MaxParts: 16, FreeParts: 1, Budget: b}, sizes, fetch)
	_, err := s.Next()
	require.NoError(t, err)
	assert.Positive(t, b.InUse())
	s.Close()
	assert.Equal(t, int64(0), b.InUse())
}

func TestStream_CloseMidStreamLeaksNoGoroutines(t *testing.T) {
	// Arrange
	runtime.GC()
	before := runtime.NumGoroutine()
	data, sizes := parts(64, 1024)
	fetch := func(ctx context.Context, i int) ([]byte, error) {
		if i >= 2 {
			<-ctx.Done() // a backend that hangs: only cancellation ends it
			return nil, ctx.Err()
		}
		return data[i], nil
	}

	// Act: read one part, give up.
	s := Start(context.Background(), Config{Window: 1 << 20, MaxParts: 16, HedgeAfter: 5 * time.Millisecond}, sizes, fetch)
	_, err := s.Next()
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond) // let hedges start too
	s.Close()

	// Assert
	noLeak(t, before)
}

func TestStream_ParentContextCancelEndsNext(t *testing.T) {
	runtime.GC()
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	_, sizes := parts(4, 16)
	fetch := func(ctx context.Context, _ int) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	s := Start(ctx, Config{Window: 1 << 20, MaxParts: 4}, sizes, fetch)
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	_, err := s.Next()
	require.ErrorIs(t, err, context.Canceled)
	s.Close()
	noLeak(t, before)
}

func TestReader_ReadsWholeStreamAndSurfacesMidStreamError(t *testing.T) {
	data, sizes := parts(6, 1000)
	want := bytes.Join(data, nil)

	t.Run("whole", func(t *testing.T) {
		s := Start(context.Background(), Config{Window: 1 << 20, MaxParts: 4}, sizes,
			func(_ context.Context, i int) ([]byte, error) { return data[i], nil })
		r := NewReader(s, nil)
		got, err := io.ReadAll(r)
		require.NoError(t, err)
		require.NoError(t, r.Close())
		assert.Equal(t, want, got)
	})

	t.Run("broken", func(t *testing.T) {
		boom := fmt.Errorf("range 4: %w", io.ErrUnexpectedEOF)
		s := Start(context.Background(), Config{Window: 1 << 20, MaxParts: 4}, sizes,
			func(_ context.Context, i int) ([]byte, error) {
				if i == 4 {
					return nil, boom
				}
				return data[i], nil
			})
		r := NewReader(s, nil)
		got, err := io.ReadAll(r)
		require.ErrorIs(t, err, io.ErrUnexpectedEOF, "a broken stream is an error, never a clean EOF")
		assert.Equal(t, want[:4000], got, "everything before the broken part, nothing after")
		_ = r.Close()
	})

	t.Run("first part already taken", func(t *testing.T) {
		s := Start(context.Background(), Config{Window: 1 << 20, MaxParts: 4}, sizes,
			func(_ context.Context, i int) ([]byte, error) { return data[i], nil })
		first, err := s.Next()
		require.NoError(t, err)
		r := NewReader(s, first)
		got, err := io.ReadAll(r)
		require.NoError(t, err)
		_ = r.Close()
		assert.Equal(t, want, got)
	})
}

func TestBudget(t *testing.T) {
	b := NewBudget(100)
	assert.True(t, b.TryAcquire(60))
	assert.False(t, b.TryAcquire(41))
	assert.True(t, b.TryAcquire(40))
	assert.Equal(t, int64(0), b.Available())
	b.Release(100)
	assert.Equal(t, int64(100), b.Available())

	var nilBudget *Budget
	assert.True(t, nilBudget.TryAcquire(1<<40), "no budget = unlimited")
	nilBudget.Release(5)
}

func TestHedgeDelay_FollowsTheMedianNotTheTail(t *testing.T) {
	// Arrange: a stream whose parts mostly take 100 ms with a few at 3 s.
	s := &Stream{cfg: Config{HedgeAfter: 50 * time.Millisecond, HedgeFactor: 3}}
	for i := 0; i < 40; i++ {
		d := 100 * time.Millisecond
		if i%8 == 0 {
			d = 3 * time.Second
		}
		s.observe(d)
	}

	// Act + Assert: 3 × the median (100 ms), not 3 × a tail-inflated mean.
	assert.Equal(t, 300*time.Millisecond, s.hedgeDelay())

	// The base delay is the floor.
	s.cfg.HedgeAfter = time.Second
	assert.Equal(t, time.Second, s.hedgeDelay())

	// No observations yet: the base.
	fresh := &Stream{cfg: Config{HedgeAfter: 400 * time.Millisecond, HedgeFactor: 3}}
	assert.Equal(t, 400*time.Millisecond, fresh.hedgeDelay())
}
