package dataplane

// The byte budget of a Go host's writes (RP74-6, Stage 1a Prompt 48): the
// Worker's ISOLATE_BYTES rule in Go. A codec write holds up to
// codecBatchBytes of chunk plaintext between cut and seal; nothing bounded
// how many such writes a host ran at once (GOMEMLIMIT was a node's only
// bound). A write takes its bytes from the host's Budget BEFORE it reads a
// byte of its body and gives them back when it ends.
//
// ByteBudget is a node's: FIFO (a large request is never starved by small
// ones behind it), a request larger than the bound admitted alone, a wait
// past Wait (2 s, the origin's chunk budget's patience) = ErrBusy — the
// client's 503 SlowDown + Retry-After 30, nothing stored. On host=slc the
// Budget is the origin's own chunk budget (CHUNK_PUT_BUDGET_BYTES, the same
// gauge — internal/api data_plane_slc.go).

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

// ErrBusy: the host's budget could not spare the write's bytes in time.
var ErrBusy = errors.New("data plane: the host cannot take another upload right now")

// BudgetWait is how long a write waits for its bytes before ErrBusy.
const BudgetWait = 2 * time.Second

// Budget bounds the bytes a host's writes hold. Acquire returns the
// release (idempotent), or ErrBusy.
type Budget interface {
	Acquire(ctx context.Context, n int64) (release func(), err error)
}

// ByteBudget is a FIFO byte semaphore.
type ByteBudget struct {
	limit int64
	// Wait is how long Acquire waits (BudgetWait when zero).
	Wait time.Duration

	mu      sync.Mutex
	inUse   int64
	waiters list.List // of *budgetWaiter, FIFO
}

type budgetWaiter struct {
	n     int64
	ready chan struct{}
}

// NewByteBudget is a budget of limit bytes.
func NewByteBudget(limit int64) *ByteBudget { return &ByteBudget{limit: limit} }

// Limit is the bound.
func (b *ByteBudget) Limit() int64 { return b.limit }

// InUse is what writes hold now.
func (b *ByteBudget) InUse() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inUse
}

// Waiting is how many writes wait in line.
func (b *ByteBudget) Waiting() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.waiters.Len()
}

// Acquire takes n bytes (the whole bound when n exceeds it: admitted
// alone), first come first served, within Wait.
func (b *ByteBudget) Acquire(ctx context.Context, n int64) (func(), error) {
	n = min(max(n, 0), b.limit)
	b.mu.Lock()
	if b.waiters.Len() == 0 && b.inUse+n <= b.limit {
		b.inUse += n
		b.mu.Unlock()
		return b.releaser(n), nil
	}
	w := &budgetWaiter{n: n, ready: make(chan struct{})}
	el := b.waiters.PushBack(w)
	b.mu.Unlock()

	wait := b.Wait
	if wait <= 0 {
		wait = BudgetWait
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-w.ready:
		return b.releaser(n), nil
	case <-t.C:
	case <-ctx.Done():
	}
	b.mu.Lock()
	select {
	case <-w.ready:
		// Granted while giving up: give it back.
		b.mu.Unlock()
		b.release(n)
		return nil, ErrBusy
	default:
	}
	b.waiters.Remove(el)
	b.grantLocked() // the one behind may fit now
	b.mu.Unlock()
	return nil, ErrBusy
}

func (b *ByteBudget) releaser(n int64) func() {
	var once sync.Once
	return func() { once.Do(func() { b.release(n) }) }
}

func (b *ByteBudget) release(n int64) {
	b.mu.Lock()
	b.inUse -= n
	b.grantLocked()
	b.mu.Unlock()
}

// grantLocked serves the line in order while the head fits.
func (b *ByteBudget) grantLocked() {
	for el := b.waiters.Front(); el != nil; el = b.waiters.Front() {
		w := el.Value.(*budgetWaiter)
		if b.inUse+w.n > b.limit {
			return
		}
		b.inUse += w.n
		b.waiters.Remove(el)
		close(w.ready)
	}
}
