// Package parfetch reads one logical stream as many parts fetched in
// parallel and hands them back strictly in order: a bounded read-ahead window
// (bytes and parts), hedged fetches for the slow tail, and an optional
// process-wide byte budget shared by every stream.
//
// It is the engine of the two large-GET paths in internal/api: the chunked
// GET (one part = one content-defined chunk) and the parallel ranged GET of a
// whole object (one part = one byte range). Both used to be limited by the
// slowest backend round-trip in their window: the writer consumes in order,
// so one chunk at the p99 (≈ 1 s on iDrive, worst 2.4 s) stalled everything
// behind it. A hedge — a second request for a part that has not finished
// after HedgeAfter, first answer wins, the loser cancelled — is what turns
// that tail (download diagnosis 2026-10-09: 14–36 → 163–183 MB/s).
//
// Memory: a part holds its Size bytes from the moment its fetch starts until
// the caller asks for the next part (Next) — so in flight + fetched + the one
// part being written never exceed Window, except that a single part larger
// than Window still runs alone, and a hedge holds a second buffer for its
// part while it runs (at most MaxHedges per stream).
package parfetch

import (
	"context"
	"errors"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Budget is a byte budget shared by every stream of the process. A nil
// *Budget is unlimited.
type Budget struct {
	limit int64
	used  atomic.Int64
}

// NewBudget returns a budget of limit bytes.
func NewBudget(limit int64) *Budget { return &Budget{limit: limit} }

// TryAcquire takes n bytes if they are free; it never waits.
func (b *Budget) TryAcquire(n int64) bool {
	if b == nil {
		return true
	}
	for {
		u := b.used.Load()
		if u+n > b.limit {
			return false
		}
		if b.used.CompareAndSwap(u, u+n) {
			return true
		}
	}
}

// Release gives back n bytes taken with TryAcquire.
func (b *Budget) Release(n int64) {
	if b == nil {
		return
	}
	b.used.Add(-n)
}

// InUse is the bytes currently taken.
func (b *Budget) InUse() int64 {
	if b == nil {
		return 0
	}
	return b.used.Load()
}

// Available is the bytes free now.
func (b *Budget) Available() int64 {
	if b == nil {
		return 1<<63 - 1
	}
	return b.limit - b.used.Load()
}

// Limit is the budget's size.
func (b *Budget) Limit() int64 {
	if b == nil {
		return 1<<63 - 1
	}
	return b.limit
}

// Hooks observe a stream (metrics). Every field is optional.
type Hooks struct {
	// Held reports bytes taken (+) and given back (-) by the window.
	Held func(delta int64)
	// Hedged reports a second attempt at a part: "slow" (not finished
	// after the hedge delay) or "error" (the first attempt failed).
	Hedged func(reason string)
	// HedgeWon reports a part delivered by its second attempt.
	HedgeWon func()
}

// Config shapes one stream.
type Config struct {
	// Window bounds the bytes held: parts in flight, fetched and waiting,
	// and the part the caller is writing. <= 0 = unbounded by bytes.
	Window int64
	// MaxParts bounds the parts held at once. <= 0 = unbounded by count.
	MaxParts int
	// HedgeAfter is the base hedge delay; 0 disables hedging (and the
	// one retry of a failed part). The effective delay is
	// max(HedgeAfter, HedgeFactor × the median of the stream's recent part
	// times).
	HedgeAfter time.Duration
	// HedgeFactor scales the running part time (default 3).
	HedgeFactor float64
	// MaxHedges bounds the slow-part hedges running at once (default 8).
	MaxHedges int
	// Budget is the process-wide byte budget (nil = unlimited). The first
	// FreeParts parts held by a stream never need it, so a stream always
	// makes progress; parts beyond them do, and a stream whose budget
	// request fails waits for one of its own parts to be consumed.
	Budget *Budget
	// FreeParts is how many held parts need no budget (min 1).
	FreeParts int
	Hooks     Hooks
}

// FetchFunc returns part i's bytes. It must honour ctx: a hedge loser and a
// closed stream are ended by cancelling it.
type FetchFunc func(ctx context.Context, i int) ([]byte, error)

type result struct {
	data []byte
	err  error
}

// Stream is one ordered, windowed, hedged fetch of len(sizes) parts.
type Stream struct {
	ctx    context.Context
	cancel context.CancelFunc
	cfg    Config
	sizes  []int64
	fetch  FetchFunc

	results []chan result
	wg      sync.WaitGroup
	wake    chan struct{}
	hedges  atomic.Int32

	mu        sync.Mutex
	heldBytes int64
	heldParts int
	launched  []bool
	budgeted  []bool
	released  []bool
	recent    [hedgeSamples]time.Duration // the last part times (ring)
	nRecent   int

	cur       int
	err       error
	closeOnce sync.Once
}

// Start begins fetching. sizes are the bytes each part will hold (the
// window's accounting); the caller must Close the stream.
func Start(ctx context.Context, cfg Config, sizes []int64, fetch FetchFunc) *Stream {
	if cfg.FreeParts < 1 {
		cfg.FreeParts = 1
	}
	if cfg.HedgeFactor <= 0 {
		cfg.HedgeFactor = 3
	}
	if cfg.MaxHedges <= 0 {
		cfg.MaxHedges = 8
	}
	sctx, cancel := context.WithCancel(ctx)
	n := len(sizes)
	s := &Stream{
		ctx: sctx, cancel: cancel, cfg: cfg, sizes: sizes, fetch: fetch,
		results:  make([]chan result, n),
		wake:     make(chan struct{}, 1),
		launched: make([]bool, n),
		budgeted: make([]bool, n),
		released: make([]bool, n),
		cur:      -1,
	}
	for i := range s.results {
		s.results[i] = make(chan result, 1) // buffered: a closed stream never strands a part
	}
	s.wg.Add(1)
	go s.dispatch()
	return s
}

// budgetPoll is how often a stream blocked on the shared budget looks again
// when none of its own parts frees anything.
const budgetPoll = 10 * time.Millisecond

func (s *Stream) dispatch() {
	defer s.wg.Done()
	var poll *time.Timer
	defer func() {
		if poll != nil {
			poll.Stop()
		}
	}()
	for i := range s.sizes {
		for {
			ok, budgetBlocked := s.admit(i)
			if ok {
				s.wg.Add(1)
				go s.runPart(i)
				break
			}
			var pollC <-chan time.Time
			if budgetBlocked {
				if poll == nil {
					poll = time.NewTimer(budgetPoll)
				} else {
					poll.Reset(budgetPoll)
				}
				pollC = poll.C
			}
			select {
			case <-s.wake:
			case <-pollC:
			case <-s.ctx.Done():
				return
			}
		}
	}
}

// admit takes window (and, past the free parts, budget) room for part i.
func (s *Stream) admit(i int) (ok, budgetBlocked bool) {
	size := s.sizes[i]
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.heldParts > 0 {
		if s.cfg.Window > 0 && s.heldBytes+size > s.cfg.Window {
			return false, false
		}
		if s.cfg.MaxParts > 0 && s.heldParts >= s.cfg.MaxParts {
			return false, false
		}
	}
	if s.heldParts >= s.cfg.FreeParts && s.cfg.Budget != nil {
		if !s.cfg.Budget.TryAcquire(size) {
			return false, true
		}
		s.budgeted[i] = true
	}
	s.heldBytes += size
	s.heldParts++
	s.launched[i] = true
	if h := s.cfg.Hooks.Held; h != nil {
		h(size)
	}
	return true, false
}

// release gives part i's room back (once).
func (s *Stream) release(i int) {
	s.mu.Lock()
	if !s.launched[i] || s.released[i] {
		s.mu.Unlock()
		return
	}
	s.released[i] = true
	size := s.sizes[i]
	s.heldBytes -= size
	s.heldParts--
	if s.budgeted[i] {
		s.cfg.Budget.Release(size)
	}
	s.mu.Unlock()
	if h := s.cfg.Hooks.Held; h != nil {
		h(-size)
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// hedgeSamples is how many recent part times the hedge delay looks at.
const hedgeSamples = 32

// hedgeDelay is max(HedgeAfter, HedgeFactor × the median of the recent part
// times). The median, not the mean: the tail this hedges against would
// inflate a mean, push the delay out and stop hedging exactly the parts it
// is for.
func (s *Stream) hedgeDelay() time.Duration {
	s.mu.Lock()
	n := s.nRecent
	if n > hedgeSamples {
		n = hedgeSamples
	}
	samples := make([]time.Duration, n)
	copy(samples, s.recent[:n])
	s.mu.Unlock()
	d := s.cfg.HedgeAfter
	if n == 0 {
		return d
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	if m := time.Duration(float64(samples[n/2]) * s.cfg.HedgeFactor); m > d {
		d = m
	}
	return d
}

func (s *Stream) observe(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recent[s.nRecent%hedgeSamples] = d
	s.nRecent++
}

type attempt struct {
	data  []byte
	err   error
	hedge bool
}

func (s *Stream) runPart(i int) {
	defer s.wg.Done()
	pctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	ch := make(chan attempt, 2) // never more than two attempts: no send blocks
	start := time.Now()
	launch := func(hedge, slot bool) {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if slot {
				defer s.hedges.Add(-1)
			}
			data, err := s.fetch(pctx, i)
			ch <- attempt{data: data, err: err, hedge: hedge}
		}()
	}
	launch(false, false)

	hedging := s.cfg.HedgeAfter > 0
	var timerC <-chan time.Time
	if hedging {
		t := time.NewTimer(s.hedgeDelay())
		defer t.Stop()
		timerC = t.C
	}

	pending, hedged := 1, false
	var firstErr error
	for {
		select {
		case a := <-ch:
			pending--
			if a.err == nil {
				if a.hedge {
					if h := s.cfg.Hooks.HedgeWon; h != nil {
						h()
					}
				}
				s.observe(time.Since(start))
				cancel() // the other attempt, if any, is the loser
				s.results[i] <- result{data: a.data}
				return
			}
			if firstErr == nil {
				firstErr = a.err
			}
			if hedging && !hedged && pctx.Err() == nil {
				hedged = true
				pending++
				if h := s.cfg.Hooks.Hedged; h != nil {
					h("error")
				}
				launch(true, false)
				continue
			}
			if pending == 0 {
				s.results[i] <- result{err: firstErr}
				return
			}
		case <-timerC:
			timerC = nil
			if hedged || pending == 0 {
				continue
			}
			if s.hedges.Add(1) > int32(s.cfg.MaxHedges) {
				s.hedges.Add(-1)
				continue
			}
			hedged = true
			pending++
			if h := s.cfg.Hooks.Hedged; h != nil {
				h("slow")
			}
			launch(true, true)
		case <-s.ctx.Done():
			s.results[i] <- result{err: s.ctx.Err()}
			return
		}
	}
}

// Next returns the next part in order, io.EOF after the last. The bytes
// returned are the caller's until it calls Next again or Close: only then is
// the part's room in the window given back. An error is sticky.
func (s *Stream) Next() ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.cur >= 0 && s.cur < len(s.sizes) {
		s.release(s.cur)
	}
	s.cur++
	if s.cur >= len(s.sizes) {
		s.err = io.EOF
		return nil, io.EOF
	}
	select {
	case r := <-s.results[s.cur]:
		if r.err != nil {
			s.err = r.err
			return nil, r.err
		}
		return r.data, nil
	case <-s.ctx.Done():
		s.err = s.ctx.Err()
		return nil, s.err
	}
}

// Close cancels every fetch, waits for all of the stream's goroutines to
// exit and gives back everything it held. Safe to call more than once.
func (s *Stream) Close() {
	s.closeOnce.Do(func() {
		s.cancel()
		s.wg.Wait()
		for i := range s.sizes {
			s.release(i)
		}
	})
}

// Reader adapts a Stream to an io.ReadCloser: the parts' bytes in order, a
// part's failure as a read error (never a clean EOF).
type Reader struct {
	s   *Stream
	buf []byte
	err error
}

// NewReader reads s from its current position; first, when not nil, is a
// part the caller already took with Next and is served before the rest.
func NewReader(s *Stream, first []byte) *Reader {
	return &Reader{s: s, buf: first}
}

func (r *Reader) fill() bool {
	for len(r.buf) == 0 {
		if r.err != nil {
			return false
		}
		b, err := r.s.Next()
		if err != nil {
			r.err = err
			return false
		}
		r.buf = b
	}
	return true
}

// Read implements io.Reader.
func (r *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !r.fill() {
		return 0, r.err
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// WriteTo hands each part to w in one Write (io.Copy uses it).
func (r *Reader) WriteTo(w io.Writer) (int64, error) {
	var total int64
	for r.fill() {
		n, err := w.Write(r.buf)
		total += int64(n)
		r.buf = r.buf[n:]
		if err != nil {
			return total, err
		}
	}
	if errors.Is(r.err, io.EOF) {
		return total, nil
	}
	return total, r.err
}

// Err is the stream's failure, nil when it ended cleanly or is still open.
func (r *Reader) Err() error {
	if r.err == nil || errors.Is(r.err, io.EOF) {
		return nil
	}
	return r.err
}

// Done reports whether every part was read.
func (r *Reader) Done() bool { return errors.Is(r.err, io.EOF) && len(r.buf) == 0 }

// Close ends the stream.
func (r *Reader) Close() error {
	r.s.Close()
	return nil
}
